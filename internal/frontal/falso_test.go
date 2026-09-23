package frontal

// Daemon falso para los tests: implementa, sobre un socket Unix, la parte del
// API de kindling que usa el frontal. Sin KVM, sin microVMs: máquinas en un
// mapa y respuestas guionizadas.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

type falso struct {
	t   *testing.T
	srv *httptest.Server
	// endpoint es la ruta del socket Unix.
	endpoint string

	mu        sync.Mutex
	maquinas  map[string]*api.Machine
	snapshots []string
	// snapAnot son las anotaciones de cada snapshot (por nombre), donde vive la
	// receta que plantilla.Construir cuelga de AnotacionReceta.
	snapAnot map[string]map[string]json.RawMessage
	imagenes []string
	ficheros map[string][]byte
	libreMiB int64

	// crearCodigo hace fallar POST /sandboxes con ese estado (507, 409...).
	crearCodigo int
	// crearErrTSC hace que restaurar DESDE UN SNAPSHOT (req.From != "") falle
	// con el mensaje real que un host reiniciado deja ver al restaurar, y que
	// api.EsFalloTSC reconoce. Un commit (lo que hace plantilla.Construir al
	// terminar) lo apaga: es justo lo que arregla el fallo simulado.
	crearErrTSC bool
	// execLibera: el exec falso manda la primera salida, espera a que se cierre
	// este canal y entonces manda el exit. Es lo que permite comprobar que el
	// frontal no acumula el flujo.
	execLibera chan struct{}
	ultimoExec api.ExecRequest
	// shellIlegal: la shell falsa manda al cliente una trama que no le toca.
	shellIlegal bool
	// creadas cuenta los POST /sandboxes aceptados.
	creadas int
}

// nuevoFalso arranca un daemon falso en un socket Unix corto (macOS limita
// la ruta de un socket a 104 bytes y t.TempDir() puede pasarse).
func nuevoFalso(t *testing.T) *falso {
	t.Helper()
	dir, err := os.MkdirTemp("", "fr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	f := &falso{t: t, endpoint: sock, maquinas: map[string]*api.Machine{},
		ficheros: map[string][]byte{}, libreMiB: 4096, imagenes: []string{"base"}}
	f.srv = httptest.NewUnstartedServer(f.rutas())
	f.srv.Listener = ln
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *falso) rutas() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := len(f.maquinas)
		f.mu.Unlock()
		writeJSON(w, 200, api.Info{Version: "fake", Machines: n})
	})
	m.HandleFunc("GET /procstats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.ProcStats{AvailableMiB: f.libreMiB})
	})
	m.HandleFunc("GET /snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		out := []*api.Snapshot{}
		for _, n := range f.snapshots {
			out = append(out, &api.Snapshot{Name: n, AllowExec: true, Annotations: f.snapAnot[n]})
		}
		f.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("GET /snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		nombre := r.PathValue("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		hay := false
		for _, s := range f.snapshots {
			hay = hay || s == nombre
		}
		if !hay {
			fail(w, 404, fmt.Errorf("no snapshot %q", nombre))
			return
		}
		writeJSON(w, 200, &api.Snapshot{Name: nombre, AllowExec: true, Annotations: f.snapAnot[nombre]})
	})
	m.HandleFunc("PUT /snapshots/{name}/annotations/{key}", func(w http.ResponseWriter, r *http.Request) {
		var valor json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&valor); err != nil {
			fail(w, 400, err)
			return
		}
		nombre, clave := r.PathValue("name"), r.PathValue("key")
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.snapAnot == nil {
			f.snapAnot = map[string]map[string]json.RawMessage{}
		}
		if f.snapAnot[nombre] == nil {
			f.snapAnot[nombre] = map[string]json.RawMessage{}
		}
		f.snapAnot[nombre][clave] = valor
		writeJSON(w, 200, &api.Snapshot{Name: nombre, AllowExec: true, Annotations: f.snapAnot[nombre]})
	})
	// POST /machines y POST /machines/{ref}/commit: lo que plantilla.Construir
	// necesita para rehacer un dorado (Run, un exec que ya sirve /machines/{ref}/exec,
	// y Commit).
	m.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		var req api.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, 400, err)
			return
		}
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		id := "build-" + hex.EncodeToString(b)
		mc := &api.Machine{ID: id, Name: id, Image: req.Image, State: api.StateRunning,
			AllowExec: req.AllowExec, IP: "10.0.0.3"}
		f.mu.Lock()
		f.maquinas[mc.ID] = mc
		f.mu.Unlock()
		c := *mc
		writeJSON(w, 201, &c)
	})
	m.HandleFunc("POST /machines/{ref}/commit", func(w http.ResponseWriter, r *http.Request) {
		var req api.CommitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, 400, err)
			return
		}
		f.mu.Lock()
		ya := false
		for _, s := range f.snapshots {
			ya = ya || s == req.Name
		}
		if !ya {
			f.snapshots = append(f.snapshots, req.Name)
		}
		if f.snapAnot == nil {
			f.snapAnot = map[string]map[string]json.RawMessage{}
		}
		// Un replace es un dorado nuevo, aunque conserve el nombre: se limpia lo
		// que hubiera hasta que Construir cuelgue la receta de nuevo.
		f.snapAnot[req.Name] = map[string]json.RawMessage{}
		// Esto es justo lo que arregla el fallo de TSC simulado.
		f.crearErrTSC = false
		f.mu.Unlock()
		writeJSON(w, 200, &api.Snapshot{Name: req.Name, AllowExec: true, CreatedAt: time.Now().UTC()})
	})
	m.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		out := []api.Image{}
		for _, n := range f.imagenes {
			out = append(out, api.Image{Name: n})
		}
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /sandboxes", f.crear)
	m.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		out := []*api.Machine{}
		for _, mc := range f.maquinas {
			if mc.Labels[api.LabelKind] == api.KindSandbox {
				c := *mc
				out = append(out, &c)
			}
		}
		f.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /sandboxes/{ref}/renew", func(w http.ResponseWriter, r *http.Request) {
		var req api.RenewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		mc, ok := f.maquinas[r.PathValue("ref")]
		if !ok {
			fail(w, 404, fmt.Errorf("no sandbox"))
			return
		}
		ahora := time.Now()
		mc.TTLAt, mc.TTLSeconds = &ahora, req.TTLSeconds
		c := *mc
		writeJSON(w, 200, &c)
	})
	m.HandleFunc("DELETE /sandboxes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.maquinas[r.PathValue("ref")]; !ok {
			fail(w, 404, fmt.Errorf("no sandbox"))
			return
		}
		delete(f.maquinas, r.PathValue("ref"))
		w.WriteHeader(204)
	})
	m.HandleFunc("PUT /machines/{ref}/labels", func(w http.ResponseWriter, r *http.Request) {
		var labels map[string]string
		_ = json.NewDecoder(r.Body).Decode(&labels)
		f.mu.Lock()
		defer f.mu.Unlock()
		mc, ok := f.maquinas[r.PathValue("ref")]
		if !ok {
			fail(w, 404, fmt.Errorf("no machine"))
			return
		}
		mc.Labels = api.MergeLabels(mc.Labels, labels)
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /machines/{ref}/exec", f.exec)
	m.HandleFunc("POST /machines/{ref}/shell", f.shell)
	m.HandleFunc("/machines/{ref}/files", f.files)
	return m
}

func (f *falso) existe(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.maquinas[ref]
	return ok
}

func (f *falso) crear(w http.ResponseWriter, r *http.Request) {
	var req api.SandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	f.mu.Lock()
	tsc := f.crearErrTSC
	f.mu.Unlock()
	if req.From != "" && tsc {
		// El mensaje real que deja ver explainRestoreErr del núcleo cuando la
		// causa es un reinicio del host: api.EsFalloTSC lo reconoce por el
		// token "TSC", y va en un 500 porque el daemon no tiene un código propio
		// para esto (es la traducción, no un StatusError).
		fail(w, 500, fmt.Errorf("Could not set TSC scaling within the snapshot: Invalid argument (os error 22)"))
		return
	}
	if f.crearCodigo != 0 {
		msg := "no room"
		if f.crearCodigo == api.StatusMachineLimit {
			msg = "machine limit reached"
		}
		fail(w, f.crearCodigo, fmt.Errorf("%s", msg))
		return
	}
	if req.From != "" {
		ok := false
		for _, s := range f.snapshots {
			ok = ok || s == req.From
		}
		if !ok {
			fail(w, 404, fmt.Errorf("no snapshot %q", req.From))
			return
		}
	}
	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	labels[api.LabelKind] = api.KindSandbox
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	ahora := time.Now()
	mc := &api.Machine{
		ID: hex.EncodeToString(b), Name: "sbx-" + hex.EncodeToString(b[:2]), Image: req.Image, From: req.From,
		State: api.StateRunning, VCPUs: req.VCPUs, MemMiB: req.MemMiB, Egress: req.Egress,
		AllowDomains: req.AllowDomains, TTLSeconds: req.TTLSeconds, OnTTL: req.OnTTL,
		AllowExec: true, CreatedAt: ahora, StartedAt: &ahora, TTLAt: &ahora, Labels: labels, IP: "10.0.0.2",
	}
	if mc.OnTTL == api.OnTTLFreeze {
		mc.OnTTL = ""
	}
	f.mu.Lock()
	f.maquinas[mc.ID] = mc
	f.creadas++
	f.mu.Unlock()
	c := *mc
	writeJSON(w, 201, &c)
}

// sembrar mete una máquina a mano, como si la hubiera creado otro.
func (f *falso) sembrar(mc *api.Machine) *api.Machine {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mc.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		mc.ID = hex.EncodeToString(b)
	}
	if mc.State == "" {
		mc.State = api.StateRunning
	}
	if mc.Labels == nil {
		mc.Labels = map[string]string{}
	}
	mc.Labels[api.LabelKind] = api.KindSandbox
	if mc.CreatedAt.IsZero() {
		mc.CreatedAt = time.Now()
	}
	f.maquinas[mc.ID] = mc
	return mc
}

func (f *falso) maquina(id string) *api.Machine {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mc, ok := f.maquinas[id]; ok {
		c := *mc
		return &c
	}
	return nil
}

func (f *falso) exec(w http.ResponseWriter, r *http.Request) {
	if !f.existe(r.PathValue("ref")) {
		fail(w, 404, fmt.Errorf("no machine"))
		return
	}
	var req api.ExecRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.ultimoExec = req
	libera := f.execLibera
	f.mu.Unlock()

	salida := []byte("hola\n")
	if r.URL.Query().Get("wait") == "1" {
		if libera != nil {
			<-libera
		}
		writeJSON(w, 200, api.ExecResult{ExitCode: 3, Stdout: salida, DurationMS: 7})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	enc := json.NewEncoder(w)
	fl := w.(http.Flusher)
	_ = enc.Encode(api.ExecEvent{Stream: "stdout", Data: salida})
	fl.Flush()
	if libera != nil {
		select {
		case <-libera:
		case <-r.Context().Done():
			return
		}
	}
	code := 3
	_ = enc.Encode(api.ExecEvent{Exit: &code, DurationMS: 7})
	fl.Flush()
}

func (f *falso) files(w http.ResponseWriter, r *http.Request) {
	if !f.existe(r.PathValue("ref")) {
		fail(w, 404, fmt.Errorf("no machine"))
		return
	}
	q := r.URL.Query()
	clave := r.PathValue("ref") + ":" + q.Get("path")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		b, ok := f.ficheros[clave]
		if !ok {
			fail(w, 404, fmt.Errorf("no such file"))
			return
		}
		if q.Get("stat") == "1" {
			writeJSON(w, 200, api.FileStat{Path: q.Get("path"), Size: int64(len(b)), Mode: "0644"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
	case http.MethodPut:
		b, err := io.ReadAll(io.LimitReader(r.Body, api.FileMaxUpload+1))
		if err != nil {
			fail(w, 400, err)
			return
		}
		if int64(len(b)) > api.FileMaxUpload {
			fail(w, 413, fmt.Errorf("too big"))
			return
		}
		f.ficheros[clave] = b
		writeJSON(w, 200, api.FileStat{Path: q.Get("path"), Size: int64(len(b)), Mode: q.Get("mode")})
	case http.MethodDelete:
		delete(f.ficheros, clave)
		w.WriteHeader(204)
	}
}

// shell hace de invitado: devuelve como salida lo que le mandan, contesta a
// una señal con un exit, y si shellIlegal manda antes un resize hacia fuera.
func (f *falso) shell(w http.ResponseWriter, r *http.Request) {
	if !f.existe(r.PathValue("ref")) {
		fail(w, 404, fmt.Errorf("no machine"))
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), api.ShellProto) {
		fail(w, 426, fmt.Errorf("upgrade required"))
		return
	}
	// Como el daemon real: el cuerpo se consume ANTES de secuestrar, o sus
	// bytes se leerían como tramas.
	var req api.ShellRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + api.ShellProto + "\r\nConnection: Upgrade\r\n\r\n")
	_ = brw.Flush()
	if f.shellIlegal {
		_ = api.WriteFrame(conn, api.ShellResize, api.ResizePayload(50, 200))
		// Se queda esperando a que el frontal cuelgue.
		_, _, _ = api.ReadFrame(brw.Reader)
		return
	}
	for {
		t, p, err := api.ReadFrame(brw.Reader)
		if err != nil {
			return
		}
		switch t {
		case api.ShellData:
			_ = api.WriteFrame(conn, api.ShellData, append([]byte("eco:"), p...))
		case api.ShellResize:
			_ = api.WriteFrame(conn, api.ShellData, []byte("resized"))
		case api.ShellSignal:
			_ = api.WriteFrame(conn, api.ShellExit, api.ExitPayload(130))
			return
		}
	}
}

// ---- cliente de pruebas

// conexionShell es una sesión kling-shell/1 abierta a mano contra el frontal.
type conexionShell struct {
	conn net.Conn
	br   *bufio.Reader
}

func abrirShell(t *testing.T, srv *httptest.Server, token, id string) (*conexionShell, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sandboxes/"+id+"/shell", strings.NewReader(`{"term":"xterm"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", api.ShellProto)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, resp
	}
	c := &conexionShell{conn: conn, br: br}
	t.Cleanup(func() { conn.Close() })
	return c, resp
}

func (c *conexionShell) leer(t *testing.T) (byte, []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	tp, p, err := api.ReadFrame(c.br)
	if err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	return tp, p
}
