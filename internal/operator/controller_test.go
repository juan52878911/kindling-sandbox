package operator

// Tests sin clúster: un API de Kubernetes falso con httptest (list, watch en
// streaming con ADDED/MODIFIED/DELETED, PATCH de status y de finalizers) y un
// frontal falso. No hace falta ni un Kubernetes ni un daemon de kindling de
// verdad: el operador solo habla HTTP con los dos, y ambos se sustituyen aquí
// por sus formas mínimas.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/frontal"
	"github.com/juan52878911/kindling/pkg/api"
)

// ---- API de Kubernetes falso ----

type wireEvent struct {
	typ string
	obj Sandbox
}

type fakeK8s struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	objs   map[string]*Sandbox
	rv     int
	subs   map[int]chan wireEvent
	subSeq int
	// triggerGone hace que la PRÓXIMA petición de watch conteste 410, sin
	// importar el resourceVersion que pida: así se simula que el histórico
	// del API server se compactó y ese resourceVersion ya no vale.
	triggerGone bool
	// dropWatches hace que las próximas N conexiones de watch se cierren en
	// cuanto se abren, sin mandar ni un evento: una reconexión de red, no un
	// 410.
	dropWatches int
}

func newFakeK8s(t *testing.T) *fakeK8s {
	t.Helper()
	f := &fakeK8s{t: t, objs: map[string]*Sandbox{}, subs: map[int]chan wireEvent{}}
	mux := http.NewServeMux()
	base := "/apis/" + Group + "/" + Version + "/"
	mux.HandleFunc(base+Resource, f.handleCollection)
	mux.HandleFunc(base+"namespaces/", f.handleItem)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeK8s) client() *KubeClient {
	return NewKubeClient(&KubeConfig{BaseURL: f.srv.URL})
}

// seed mete un objeto directamente en el almacén, como si ya existiera antes
// de que el operador arrancara. No dispara ningún evento de watch.
func (f *fakeK8s) seed(sb Sandbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rv++
	sb.Metadata.ResourceVersion = strconv.Itoa(f.rv)
	f.objs[sb.Key()] = &sb
}

func (f *fakeK8s) get(key string) (Sandbox, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[key]
	if !ok {
		return Sandbox{}, false
	}
	return *o, true
}

func (f *fakeK8s) waitForWatcher(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.subs)
		f.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a watch subscriber")
}

func (f *fakeK8s) broadcast(typ string, obj Sandbox) {
	f.mu.Lock()
	chans := make([]chan wireEvent, 0, len(f.subs))
	for _, ch := range f.subs {
		chans = append(chans, ch)
	}
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- wireEvent{typ: typ, obj: obj}:
		default:
		}
	}
}

func (f *fakeK8s) handleCollection(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "1" {
		f.handleWatch(w, r)
		return
	}
	f.mu.Lock()
	items := make([]Sandbox, 0, len(f.objs))
	for _, o := range f.objs {
		items = append(items, *o)
	}
	rv := f.rv
	f.mu.Unlock()
	writeTestJSON(w, 200, map[string]any{
		"metadata": map[string]any{"resourceVersion": strconv.Itoa(rv)},
		"items":    items,
	})
}

func (f *fakeK8s) handleWatch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.triggerGone {
		f.triggerGone = false
		f.mu.Unlock()
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(status{Kind: "Status", Status: "Failure", Message: "too old resource version", Reason: "Gone", Code: http.StatusGone})
		return
	}
	if f.dropWatches > 0 {
		f.dropWatches--
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK) // conexión que se corta sin mandar nada
		return
	}
	id := f.subSeq
	f.subSeq++
	ch := make(chan wireEvent, 32)
	f.subs[id] = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.subs, id)
		f.mu.Unlock()
	}()

	fl, ok := w.(http.Flusher)
	if !ok {
		f.t.Fatal("ResponseWriter is not a Flusher")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if err := enc.Encode(map[string]any{"type": ev.typ, "object": ev.obj}); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (f *fakeK8s) handleItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/apis/"+Group+"/"+Version+"/namespaces/")
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[1] != Resource {
		http.NotFound(w, r)
		return
	}
	ns, name := parts[0], parts[2]
	isStatus := len(parts) == 4 && parts[3] == "status"
	key := ns + "/" + name

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	obj, ok := f.objs[key]
	if !ok {
		f.mu.Unlock()
		writeTestJSON(w, http.StatusNotFound, status{Kind: "Status", Message: "no such sandbox", Code: http.StatusNotFound})
		return
	}
	cur := *obj
	if isStatus {
		var patch struct {
			Status SandboxStatus `json:"status"`
		}
		// El merge patch (RFC 7386) solo toca las claves presentes en el
		// JSON; decodificar sobre cur.Status ya puesto respeta eso porque
		// encoding/json no toca los campos ausentes del cuerpo.
		patch.Status = cur.Status
		if err := json.Unmarshal(body, &patch); err != nil {
			f.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cur.Status = patch.Status
	} else {
		var patch struct {
			Metadata struct {
				Finalizers []string `json:"finalizers"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &patch); err != nil {
			f.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cur.Metadata.Finalizers = patch.Metadata.Finalizers
	}
	f.rv++
	cur.Metadata.ResourceVersion = strconv.Itoa(f.rv)

	deleted := cur.Metadata.DeletionTimestamp != nil && !cur.Metadata.HasFinalizer()
	if deleted {
		delete(f.objs, key)
	} else {
		saved := cur
		f.objs[key] = &saved
	}
	f.mu.Unlock()

	evType := "MODIFIED"
	if deleted {
		evType = "DELETED"
	}
	f.broadcast(evType, cur)
	writeTestJSON(w, http.StatusOK, cur)
}

func writeTestJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- frontal falso ----

type fakeFrontal struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	sandboxes map[string]*frontal.Sandbox
	seq       int
	creates   int
	// down simula el frontal caído: get y renew contestan 503 mientras esté a
	// true, para probar que status.message se pone y se limpia después.
	down bool
}

func (f *fakeFrontal) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

func (f *fakeFrontal) isDown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down
}

func newFakeFrontal(t *testing.T) *fakeFrontal {
	t.Helper()
	f := &fakeFrontal{t: t, sandboxes: map[string]*frontal.Sandbox{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", f.create)
	mux.HandleFunc("GET /v1/sandboxes/{id}", f.get)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", f.del)
	mux.HandleFunc("POST /v1/sandboxes/{id}/renew", f.renew)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFrontal) client() *FrontalClient {
	return NewFrontalClient(f.srv.URL, "t")
}

func (f *fakeFrontal) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func (f *fakeFrontal) create(w http.ResponseWriter, r *http.Request) {
	var pet frontal.CrearPeticion
	_ = json.NewDecoder(r.Body).Decode(&pet)
	f.mu.Lock()
	f.seq++
	id := fmt.Sprintf("sbx-%d", f.seq)
	f.creates++
	ttl := pet.TTLSeconds
	if ttl == 0 {
		ttl = api.SandboxDefaultTTL
	}
	exp := time.Now().Add(time.Duration(ttl) * time.Second)
	sb := &frontal.Sandbox{
		ID: id, Host: "host1", Template: pet.Template, Image: pet.Image,
		State: api.StateRunning, TTLSeconds: ttl, ExpiresAt: &exp,
	}
	f.sandboxes[id] = sb
	f.mu.Unlock()
	writeTestJSON(w, http.StatusCreated, sb)
}

func (f *fakeFrontal) get(w http.ResponseWriter, r *http.Request) {
	if f.isDown() {
		writeTestJSON(w, http.StatusServiceUnavailable, api.Error{Message: "frontal down"})
		return
	}
	id := r.PathValue("id")
	f.mu.Lock()
	sb, ok := f.sandboxes[id]
	f.mu.Unlock()
	if !ok {
		writeTestJSON(w, http.StatusNotFound, api.Error{Message: "no sandbox " + id})
		return
	}
	writeTestJSON(w, http.StatusOK, sb)
}

func (f *fakeFrontal) del(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	_, ok := f.sandboxes[id]
	delete(f.sandboxes, id)
	f.mu.Unlock()
	if !ok {
		writeTestJSON(w, http.StatusNotFound, api.Error{Message: "no sandbox " + id})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeFrontal) renew(w http.ResponseWriter, r *http.Request) {
	if f.isDown() {
		writeTestJSON(w, http.StatusServiceUnavailable, api.Error{Message: "frontal down"})
		return
	}
	id := r.PathValue("id")
	var pet frontal.RenovarPeticion
	_ = json.NewDecoder(r.Body).Decode(&pet)
	f.mu.Lock()
	sb, ok := f.sandboxes[id]
	if ok {
		sb.TTLSeconds = pet.TTLSeconds
		exp := time.Now().Add(time.Duration(pet.TTLSeconds) * time.Second)
		sb.ExpiresAt = &exp
	}
	f.mu.Unlock()
	if !ok {
		writeTestJSON(w, http.StatusNotFound, api.Error{Message: "no sandbox " + id})
		return
	}
	writeTestJSON(w, http.StatusOK, sb)
}

// ---- helpers de test ----

func newTestSandbox(ns, name string) Sandbox {
	return Sandbox{
		Metadata: ObjectMeta{Name: name, Namespace: ns, Generation: 1},
		Spec:     SandboxSpec{Template: "node", TTLSeconds: 300, OnTTL: api.OnTTLFreeze, Egress: "none"},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testContext da un contexto que se cancela solo al terminar el test:
// evita que la goroutine del controlador (ctrl.Run) sobreviva al test que la
// lanzó.
func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, _ := testContext(t)
	return ctx
}

func shrinkBackoff(t *testing.T) {
	t.Helper()
	old := backoffBase
	backoffBase = 10 * time.Millisecond
	t.Cleanup(func() { backoffBase = old })
}

// ---- tests ----

func TestReconcile_CreatesFinalizerAndStatus(t *testing.T) {
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	k8s.seed(newTestSandbox("default", "one"))

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	waitFor(t, "status.id to be set", func() bool {
		sb, ok := k8s.get("default/one")
		return ok && sb.Status.ID != ""
	})

	sb, _ := k8s.get("default/one")
	if !sb.Metadata.HasFinalizer() {
		t.Fatalf("expected finalizer %q, got %v", CleanupFinalizer, sb.Metadata.Finalizers)
	}
	if sb.Status.Host != "host1" || sb.Status.State != string(api.StateRunning) {
		t.Fatalf("unexpected status: %+v", sb.Status)
	}
	if got := fr.createdCount(); got != 1 {
		t.Fatalf("expected 1 create against the frontal, got %d", got)
	}
}

func TestReconcile_NoDuplicateCreation(t *testing.T) {
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	k8s.seed(newTestSandbox("default", "two"))

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctrl.Resync = 20 * time.Millisecond // resync agresivo a propósito
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	waitFor(t, "status.id to be set", func() bool {
		sb, ok := k8s.get("default/two")
		return ok && sb.Status.ID != ""
	})
	// Deja correr varias vueltas de resync de más: si algo duplicara la
	// creación, aquí se vería.
	time.Sleep(200 * time.Millisecond)

	if got := fr.createdCount(); got != 1 {
		t.Fatalf("expected exactly 1 create against the frontal, got %d", got)
	}
}

func TestReconcile_DeleteCallsFrontalAndRemovesFinalizer(t *testing.T) {
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)

	// Un sandbox ya creado, con dueño en el frontal, al que le acaban de
	// pedir borrarse (deletionTimestamp puesto, como haría `kubectl delete`).
	created, err := fr.client().Create(testCtx(t), SandboxSpec{Template: "node", TTLSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sb := newTestSandbox("default", "three")
	sb.Metadata.Finalizers = []string{CleanupFinalizer}
	sb.Metadata.DeletionTimestamp = &now
	sb.Status = SandboxStatus{ID: created.ID, Host: created.Host, State: string(created.State)}
	k8s.seed(sb)

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	waitFor(t, "the object to be gone (finalizer removed)", func() bool {
		_, ok := k8s.get("default/three")
		return !ok
	})
	if _, err := fr.client().Get(testCtx(t), created.ID); err == nil || !IsFrontalNotFound(err) {
		t.Fatalf("expected the sandbox to be gone from the frontal, got: %v", err)
	}
}

func TestReconcile_DeleteMissingInFrontalStillRemovesFinalizer(t *testing.T) {
	// El frontal ya no lo tiene (lo borró la limpieza por abandono): un 404
	// al borrar cuenta como hecho, y el finalizer se quita igualmente.
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)

	now := time.Now()
	sb := newTestSandbox("default", "four")
	sb.Metadata.Finalizers = []string{CleanupFinalizer}
	sb.Metadata.DeletionTimestamp = &now
	sb.Status = SandboxStatus{ID: "does-not-exist", Host: "host1"}
	k8s.seed(sb)

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	waitFor(t, "the object to be gone despite the frontal 404", func() bool {
		_, ok := k8s.get("default/four")
		return !ok
	})
}

func TestWatch_ReconnectsAfterDrop(t *testing.T) {
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	k8s.dropWatches = 2 // las dos primeras conexiones se cortan sin avisar

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	k8s.waitForWatcher(t) // hasta que sobrevive una conexión de verdad
	k8s.seed(newTestSandbox("default", "five"))
	k8s.broadcast("ADDED", func() Sandbox { sb, _ := k8s.get("default/five"); return sb }())

	waitFor(t, "reconciliation after reconnecting the watch", func() bool {
		sb, ok := k8s.get("default/five")
		return ok && sb.Status.ID != ""
	})
}

func TestWatch_RelistsOn410(t *testing.T) {
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)
	k8s.triggerGone = true // el primer watch después del list arranca con un 410

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	// El 410 fuerza un relist; solo después de relistar aparece un watcher
	// de verdad. Si el operador se quedara reintentando el watch con el
	// mismo resourceVersion en vez de relistar, esto no ocurriría nunca (el
	// falso k8s solo manda un 410 una vez).
	k8s.waitForWatcher(t)

	k8s.seed(newTestSandbox("default", "six"))
	k8s.broadcast("ADDED", func() Sandbox { sb, _ := k8s.get("default/six"); return sb }())

	waitFor(t, "reconciliation after relisting past the 410", func() bool {
		sb, ok := k8s.get("default/six")
		return ok && sb.Status.ID != ""
	})
}

func TestReconcile_TTLChangeRenews(t *testing.T) {
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)

	created, err := fr.client().Create(testCtx(t), SandboxSpec{Template: "node", TTLSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	sb := newTestSandbox("default", "seven")
	sb.Metadata.Finalizers = []string{CleanupFinalizer}
	sb.Metadata.Generation = 2
	sb.Spec.TTLSeconds = 900
	sb.Status = SandboxStatus{ID: created.ID, Host: created.Host, State: string(created.State), ObservedGeneration: 1}
	k8s.seed(sb)

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	waitFor(t, "observedGeneration to catch up after a renew", func() bool {
		got, ok := k8s.get("default/seven")
		return ok && got.Status.ObservedGeneration == 2
	})
	got, _ := fr.client().Get(testCtx(t), created.ID)
	if got.TTLSeconds != 900 {
		t.Fatalf("expected the frontal to have been renewed to 900s, got %d", got.TTLSeconds)
	}
}

// TestReconcile_MessageClearsAfterFrontalRecovers cubre un bug visto contra
// un k3s real: status.message se ponía con el error del frontal caído, pero
// nunca se limpiaba al recuperarse, porque un SandboxStatus{Message: ""} con
// `omitempty` no manda la clave "message" en el merge patch, y el servidor
// se queda con lo que ya tenía. La caché en memoria del operador SÍ se veía
// bien (cur.Status.Message = "" en cuanto la reconciliación de éxito
// terminaba), así que hacía falta mirar el objeto tal y como lo sirve el
// falso API de Kubernetes, no solo lo que el operador cree que escribió.
func TestReconcile_MessageClearsAfterFrontalRecovers(t *testing.T) {
	shrinkBackoff(t)
	k8s := newFakeK8s(t)
	fr := newFakeFrontal(t)

	created, err := fr.client().Create(testCtx(t), SandboxSpec{Template: "node", TTLSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	sb := newTestSandbox("default", "eight")
	sb.Metadata.Finalizers = []string{CleanupFinalizer}
	sb.Metadata.Generation = 2
	sb.Spec.TTLSeconds = 900
	sb.Status = SandboxStatus{ID: created.ID, Host: created.Host, State: string(created.State), ObservedGeneration: 1}
	k8s.seed(sb)

	fr.setDown(true)

	ctrl := NewController(k8s.client(), fr.client(), nil)
	ctrl.Resync = 20 * time.Millisecond // resync agresivo: sin esperar 30s de verdad
	ctx, cancel := testContext(t)
	defer cancel()
	go ctrl.Run(ctx)

	waitFor(t, "status.message to be set while the frontal is down", func() bool {
		got, ok := k8s.get("default/eight")
		return ok && hasMessage(got.Status.Message)
	})

	fr.setDown(false)

	waitFor(t, "status.message to clear once the frontal recovers", func() bool {
		got, ok := k8s.get("default/eight")
		return ok && got.Status.ObservedGeneration == 2 && !hasMessage(got.Status.Message)
	})
}
