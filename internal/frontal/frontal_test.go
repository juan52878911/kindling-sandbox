package frontal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling-sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

const (
	tokenAlice = "tok-alice-1234567890"
	tokenBob   = "tok-bob-0987654321"
)

// montar levanta un frontal sobre los daemons falsos dados y devuelve el
// servidor de pruebas y el frontal.
func montar(t *testing.T, daemons map[string]*falso, tenants ...Tenant) (*httptest.Server, *Servidor) {
	t.Helper()
	endpoints := map[string]string{}
	for n, f := range daemons {
		endpoints[n] = f.endpoint
	}
	if tenants == nil {
		tenants = []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "bob", Token: tokenBob}}
	}
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(endpoints), Tenants: tenants, Log: testLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

func testLog(t *testing.T) *log.Logger { return log.New(&logT{t}, "", 0) }

type logT struct{ t *testing.T }

func (l *logT) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// pide hace una petición con token y devuelve la respuesta.
func pide(t *testing.T, srv *httptest.Server, token, method, path string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		bs, _ := json.Marshal(b)
		rd = bytes.NewReader(bs)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodifica[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

func esperaCodigo(t *testing.T, resp *http.Response, code int) {
	t.Helper()
	if resp.StatusCode != code {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, code, b)
	}
}

func crea(t *testing.T, srv *httptest.Server, token string, pet any) Sandbox {
	t.Helper()
	resp := pide(t, srv, token, "POST", "/v1/sandboxes", pet)
	esperaCodigo(t, resp, 201)
	return decodifica[Sandbox](t, resp)
}

// ---- tests

func TestHealthSinTokenYElRestoConToken(t *testing.T) {
	srv, _ := montar(t, map[string]*falso{"a": nuevoFalso(t)})
	esperaCodigo(t, pide(t, srv, "", "GET", "/v1/health", nil), 200)
	esperaCodigo(t, pide(t, srv, "", "GET", "/v1/hosts", nil), 401)
	esperaCodigo(t, pide(t, srv, "", "GET", "/v1/sandboxes", nil), 401)
	esperaCodigo(t, pide(t, srv, "mal", "GET", "/v1/sandboxes", nil), 401)
	resp := pide(t, srv, tokenAlice, "GET", "/v1/hosts", nil)
	esperaCodigo(t, resp, 200)
	ests := decodifica[[]hosts.Estado](t, resp)
	if len(ests) != 1 || !ests[0].Vivo || ests[0].Nombre != "a" {
		t.Fatalf("hosts: %+v", ests)
	}
}

func TestNuevoSinTenantsSeNiega(t *testing.T) {
	if _, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil)}); err == nil {
		t.Fatal("expected an error without tenants")
	}
	if _, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Tenants: []Tenant{{Nombre: "x", Token: ""}}}); err == nil {
		t.Fatal("expected an error with an empty token")
	}
	if _, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Tenants: []Tenant{{Nombre: "a/b", Token: "t"}}}); err == nil {
		t.Fatal("expected an error with a slash in the tenant name")
	}
}

func TestCrearConImagen(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})

	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Labels: map[string]string{"proyecto": "x"}})
	if sb.Host != "a" || !strings.HasPrefix(sb.ID, "a/") || sb.State != api.StateRunning {
		t.Fatalf("sandbox: %+v", sb)
	}
	if sb.Egress != "none" || sb.OnTTL != api.OnTTLFreeze || sb.TTLSeconds != api.SandboxDefaultTTL || sb.ExpiresAt == nil {
		t.Fatalf("defaults: %+v", sb)
	}
	if sb.Labels["proyecto"] != "x" || sb.Labels[LabelTenant] != "" {
		t.Fatalf("labels: %+v", sb.Labels)
	}
	_, ref, _ := partirID(sb.ID)
	mc := f.maquina(ref)
	if mc == nil || mc.Labels[api.LabelKind] != api.KindSandbox || mc.Labels[LabelTenant] != "alice" || mc.Image != "base" {
		t.Fatalf("machine in the daemon: %+v", mc)
	}

	// Imagen que no hay en ningún host.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "nope"}), 404)
	// Ni plantilla ni imagen, sin imagen por defecto.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", `{}`), 400)
	// Etiqueta reservada.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Labels: map[string]string{LabelTenant: "bob"}}), 400)
	// Cuerpo con campos desconocidos.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", `{"image":"base","foo":1}`), 400)
	// on_ttl y egress inválidos.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", OnTTL: "explode"}), 400)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", Egress: "lan"}), 400)
}

func TestCrearConPlantillaSoloDondeExiste(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	b.snapshots = []string{"sbx-node"}
	// a tiene más hueco: sin la plantilla, sería el elegido.
	a.libreMiB, b.libreMiB = 8192, 1024
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b})

	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node", TTLSeconds: 120})
	if sb.Host != "b" || sb.Template != "node" || sb.TTLSeconds != 120 {
		t.Fatalf("sandbox: %+v", sb)
	}
	_, ref, _ := partirID(sb.ID)
	mc := b.maquina(ref)
	if mc == nil || mc.From != "sbx-node" || mc.Labels[LabelTemplate] != "node" || mc.Labels[LabelTenant] != "alice" {
		t.Fatalf("machine in the daemon: %+v", mc)
	}

	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "python"}), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "node", Image: "base"}), 400)
}

func TestCrearReclamaPrecalentada(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	viejo := time.Now().Add(-time.Hour)
	pre := f.sembrar(&api.Machine{From: "sbx-node", TTLSeconds: 600, TTLAt: &viejo,
		Labels: map[string]string{LabelTemplate: "node"}})
	srv, _ := montar(t, map[string]*falso{"a": f})

	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node", TTLSeconds: 300, Labels: map[string]string{"k": "v"}})
	if sb.ID != "a/"+pre.ID {
		t.Fatalf("expected the prewarmed %s, got %s", pre.ID, sb.ID)
	}
	mc := f.maquina(pre.ID)
	if mc.Labels[LabelTenant] != "alice" || mc.Labels["k"] != "v" || mc.TTLSeconds != 300 {
		t.Fatalf("claimed machine: %+v", mc)
	}
	if mc.TTLAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("the TTL clock was not renewed: %v", mc.TTLAt)
	}
	if f.creadas != 0 {
		t.Fatalf("a claim should not create: %d created", f.creadas)
	}

	// Ya no hay precalentadas: la siguiente se crea del snapshot.
	sb2 := crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	if sb2.ID == sb.ID || f.creadas != 1 {
		t.Fatalf("second sandbox: %+v (created %d)", sb2, f.creadas)
	}

	// Una petición que pide red no se conforma con una precalentada sin red.
	f.sembrar(&api.Machine{From: "sbx-node", Labels: map[string]string{LabelTemplate: "node"}})
	sb3 := crea(t, srv, tokenAlice, CrearPeticion{Template: "node", Egress: "internet"})
	if f.creadas != 2 || sb3.Egress != "internet" {
		t.Fatalf("with egress: %+v (created %d)", sb3, f.creadas)
	}
}

func TestReintentaEnOtroHostCuandoNoCabe(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	a.libreMiB, b.libreMiB = 8192, 1024          // a es el preferido...
	a.crearCodigo = api.StatusInsufficientMemory // ...pero dice que no cabe.
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b})

	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	if sb.Host != "b" {
		t.Fatalf("expected host b, got %s", sb.Host)
	}
	// Tope de máquinas: también se reintenta.
	a.crearCodigo = api.StatusMachineLimit
	if sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"}); sb.Host != "b" {
		t.Fatalf("expected host b, got %s", sb.Host)
	}
	// Ningún host: 503.
	b.crearCodigo = api.StatusInsufficientMemory
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 503)
	// Un error cualquiera no se reintenta: se devuelve tal cual.
	a.crearCodigo = 400
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 400)
}

func TestUnTenantNoVeNiTocaLoAjeno(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	_, ref, _ := partirID(sb.ID)

	// Alice lo ve; Bob no.
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID, nil), 200)
	esperaCodigo(t, pide(t, srv, tokenBob, "GET", "/v1/sandboxes/"+sb.ID, nil), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "DELETE", "/v1/sandboxes/"+sb.ID, nil), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes/"+sb.ID+"/renew", `{"ttl_seconds":60}`), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes/"+sb.ID+"/exec", api.ExecRequest{Cmd: []string{"id"}}), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "GET", "/v1/sandboxes/"+sb.ID+"/files?path=/etc/passwd", nil), 404)
	if _, resp := abrirShell(t, srv, tokenBob, sb.ID); resp.StatusCode != 404 {
		t.Fatalf("shell as bob: %d", resp.StatusCode)
	}
	if f.maquina(ref) == nil {
		t.Fatal("bob removed alice's sandbox")
	}

	// Ni por el id a pelo del daemon, ni por prefijo, ni por nombre.
	for _, id := range []string{ref, "a/" + ref[:6], "a/" + f.maquina(ref).Name, "b/" + ref} {
		esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+id, nil), 404)
	}

	// Las listas están separadas.
	crea(t, srv, tokenBob, CrearPeticion{Image: "base"})
	if l := decodifica[[]Sandbox](t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes", nil)); len(l) != 1 || l[0].ID != sb.ID {
		t.Fatalf("alice's list: %+v", l)
	}
	if l := decodifica[[]Sandbox](t, pide(t, srv, tokenBob, "GET", "/v1/sandboxes", nil)); len(l) != 1 || l[0].ID == sb.ID {
		t.Fatalf("bob's list: %+v", l)
	}

	// Alice sí puede renovar y borrar el suyo.
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/renew", `{"ttl_seconds":42}`)
	esperaCodigo(t, resp, 200)
	if r := decodifica[Sandbox](t, resp); r.TTLSeconds != 42 {
		t.Fatalf("renew: %+v", r)
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "DELETE", "/v1/sandboxes/"+sb.ID, nil), 204)
	if f.maquina(ref) != nil {
		t.Fatal("the sandbox is still there")
	}
}

func TestLimitesRechazanCon429(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node", "sbx-py"}
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 3, MaxPorPlantilla: 1},
		Tenant{Nombre: "bob", Token: tokenBob, MaxSandboxes: 1})

	crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "node"}), 429)
	crea(t, srv, tokenAlice, CrearPeticion{Template: "py"})
	crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 429)

	// La cuota de Alice no es la de Bob.
	sb := crea(t, srv, tokenBob, CrearPeticion{Image: "base"})
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 429)
	// Borrar libera.
	esperaCodigo(t, pide(t, srv, tokenBob, "DELETE", "/v1/sandboxes/"+sb.ID, nil), 204)
	crea(t, srv, tokenBob, CrearPeticion{Image: "base"})
}

func TestExecEnStreamingYConWait(t *testing.T) {
	f := nuevoFalso(t)
	f.execLibera = make(chan struct{})
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})

	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/exec", api.ExecRequest{Cmd: []string{"echo", "hola"}})
	esperaCodigo(t, resp, 200)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type %q", ct)
	}
	// La primera línea tiene que llegar ANTES de que el daemon termine: si el
	// frontal acumulase, esto se quedaría esperando.
	sc := bufio.NewScanner(resp.Body)
	linea := make(chan string, 1)
	go func() {
		if sc.Scan() {
			linea <- sc.Text()
		}
		close(linea)
	}()
	select {
	case l := <-linea:
		var ev api.ExecEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev.Stream != "stdout" || string(ev.Data) != "hola\n" {
			t.Fatalf("first event: %s (%v)", l, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event did not arrive while the command was still running: the frontal is buffering")
	}
	close(f.execLibera)
	var ev api.ExecEvent
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Exit == nil || *ev.Exit != 3 || ev.DurationMS != 7 {
		t.Fatalf("exit event: %s", sc.Text())
	}
	if sc.Scan() {
		t.Fatalf("unexpected trailing line: %s", sc.Text())
	}
	if f.ultimoExec.Cmd[0] != "echo" {
		t.Fatalf("the daemon saw %v", f.ultimoExec.Cmd)
	}

	// ?wait=1 agrega.
	f.execLibera = nil
	resp = pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/exec?wait=1", api.ExecRequest{Cmd: []string{"true"}})
	esperaCodigo(t, resp, 200)
	res := decodifica[api.ExecResult](t, resp)
	if res.ExitCode != 3 || string(res.Stdout) != "hola\n" {
		t.Fatalf("wait result: %+v", res)
	}

	// Sin cmd: 400 antes de tocar el daemon.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/exec", `{}`), 400)
}

func TestFilesPassthrough(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	base := "/v1/sandboxes/" + sb.ID + "/files?path=/w/a.txt"

	resp := pide(t, srv, tokenAlice, "PUT", base+"&mode=0755&mkdir=1", []byte("contenido"))
	esperaCodigo(t, resp, 200)
	if st := decodifica[api.FileStat](t, resp); st.Size != 9 || st.Mode != "0755" {
		t.Fatalf("stat after put: %+v", st)
	}
	resp = pide(t, srv, tokenAlice, "GET", base, nil)
	esperaCodigo(t, resp, 200)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "contenido" {
		t.Fatalf("got %q", b)
	}
	resp = pide(t, srv, tokenAlice, "GET", base+"&stat=1", nil)
	esperaCodigo(t, resp, 200)
	if st := decodifica[api.FileStat](t, resp); st.Size != 9 {
		t.Fatalf("stat: %+v", st)
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "DELETE", base, nil), 204)
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", base, nil), 404)
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/files", nil), 400)

	// Una subida que anuncia más del tope se rechaza sin leerla.
	req, _ := http.NewRequest("PUT", srv.URL+base, strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	req.ContentLength = api.FileMaxUpload + 1
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		esperaCodigo(t, resp, 413)
	}
}

func TestShellRelay(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})

	// Sin Upgrade: 426.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/shell", `{}`), 426)

	c, _ := abrirShell(t, srv, tokenAlice, sb.ID)
	if c == nil {
		t.Fatal("no upgrade")
	}
	if err := api.WriteFrame(c.conn, api.ShellData, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	if tp, p := c.leer(t); tp != api.ShellData || string(p) != "eco:ls\n" {
		t.Fatalf("frame %d %q", tp, p)
	}
	// resize pasa hacia dentro.
	_ = api.WriteFrame(c.conn, api.ShellResize, api.ResizePayload(40, 120))
	if tp, p := c.leer(t); tp != api.ShellData || string(p) != "resized" {
		t.Fatalf("frame %d %q", tp, p)
	}
	// Una trama que el cliente no debe mandar (exit) se ignora, no corta.
	_ = api.WriteFrame(c.conn, api.ShellExit, api.ExitPayload(0))
	_ = api.WriteFrame(c.conn, api.ShellData, []byte("x"))
	if tp, p := c.leer(t); tp != api.ShellData || string(p) != "eco:x" {
		t.Fatalf("frame %d %q", tp, p)
	}
	// signal → exit, y después el frontal cuelga.
	_ = api.WriteFrame(c.conn, api.ShellSignal, []byte{2})
	tp, p := c.leer(t)
	code, _ := api.ParseExit(p)
	if tp != api.ShellExit || code != 130 {
		t.Fatalf("frame %d %v", tp, p)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := api.ReadFrame(c.br); err == nil {
		t.Fatal("the connection should be closed after exit")
	}
}

func TestShellRelayCortaSiElDaemonMandaLoQueNoDebe(t *testing.T) {
	f := nuevoFalso(t)
	f.shellIlegal = true
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})

	c, _ := abrirShell(t, srv, tokenAlice, sb.ID)
	if c == nil {
		t.Fatal("no upgrade")
	}
	tp, p := c.leer(t)
	if tp != api.ShellError || !strings.Contains(string(p), "type 1") {
		t.Fatalf("expected an error frame about type 1, got %d %q", tp, p)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := api.ReadFrame(c.br); err == nil {
		t.Fatal("the connection should be closed after the illegal frame")
	}
}

func TestLimpiarBorraLosAbandonadosYRespetaElPool(t *testing.T) {
	f := nuevoFalso(t)
	hace := func(d time.Duration) *time.Time { x := time.Now().Add(-d); return &x }
	abandonado := f.sembrar(&api.Machine{State: api.StateWarm, TTLSeconds: 600, TTLAt: hace(30 * time.Hour),
		Labels: map[string]string{LabelTenant: "alice"}})
	reciente := f.sembrar(&api.Machine{State: api.StateWarm, TTLSeconds: 600, TTLAt: hace(2 * time.Hour),
		Labels: map[string]string{LabelTenant: "alice"}})
	precalentada := f.sembrar(&api.Machine{State: api.StateRunning, TTLSeconds: 600, TTLAt: hace(30 * time.Hour),
		Labels: map[string]string{LabelTemplate: "node"}})
	_, s := montar(t, map[string]*falso{"a": f})

	s.Limpiar(context.Background())
	if f.maquina(abandonado.ID) != nil {
		t.Fatal("the abandoned sandbox is still there")
	}
	if f.maquina(reciente.ID) == nil {
		t.Fatal("the recent sandbox was removed")
	}
	if f.maquina(precalentada.ID) == nil {
		t.Fatal("the prewarmed instance (no tenant) was removed: it belongs to the template pool")
	}
}

func TestMetricasExponePorTenantYResultado(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice},
		Tenant{Nombre: "bob", Token: tokenBob, MaxSandboxes: 1})

	// Sin token, como el resto de rutas protegidas.
	esperaCodigo(t, pide(t, srv, "", "GET", "/v1/metrics", nil), 401)

	crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})                                                     // ok
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "python"}), 404) // sin plantilla en ningún host: error
	crea(t, srv, tokenBob, CrearPeticion{Image: "base"})                                                       // ok
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes", CrearPeticion{Image: "base"}), 429)        // cuota de bob

	resp := pide(t, srv, tokenBob, "GET", "/v1/metrics", nil)
	esperaCodigo(t, resp, 200)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	texto := string(b)
	for _, quiero := range []string{
		`kling_sandbox_machines{tenant="alice",state="running"} 1`,
		`kling_sandbox_machines{tenant="bob",state="running"} 1`,
		`kling_sandbox_creations_total{result="ok"} 2`,
		`kling_sandbox_creations_total{result="quota"} 1`,
		`kling_sandbox_creations_total{result="error"} 1`,
		`kling_sandbox_host_up{host="a"} 1`,
		`kling_sandbox_shell_sessions{tenant="alice"} 0`,
		"kling_sandbox_create_duration_seconds_sum ",
		"kling_sandbox_create_duration_seconds_count ",
	} {
		if !strings.Contains(texto, quiero) {
			t.Errorf("metrics missing %q, got:\n%s", quiero, texto)
		}
	}
}

func TestCuotaDeShellsRechazaConCuatrocientosVeintinueveYLaMetricaLoVe(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f}, Tenant{Nombre: "alice", Token: tokenAlice, MaxShells: 1})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})

	c1, resp1 := abrirShell(t, srv, tokenAlice, sb.ID)
	if c1 == nil {
		t.Fatalf("no upgrade: %d", resp1.StatusCode)
	}

	// Segunda shell del mismo tenant, con la primera todavía abierta: 429.
	if _, resp2 := abrirShell(t, srv, tokenAlice, sb.ID); resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("segunda shell = %d, quería 429", resp2.StatusCode)
	}

	// La métrica ve la sesión abierta.
	metricasResp := pide(t, srv, tokenAlice, "GET", "/v1/metrics", nil)
	b, _ := io.ReadAll(metricasResp.Body)
	if !strings.Contains(string(b), `kling_sandbox_shell_sessions{tenant="alice"} 1`) {
		t.Fatalf("la métrica no ve la sesión abierta:\n%s", b)
	}

	// Al cerrarla, el cupo se libera (el frontal lo nota cuando el relé, en su
	// propia goroutine de servidor, se entera de que la conexión murió).
	c1.conn.Close()
	limite := time.Now().Add(3 * time.Second)
	for time.Now().Before(limite) {
		resp := pide(t, srv, tokenAlice, "GET", "/v1/metrics", nil)
		b, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(b), `kling_sandbox_shell_sessions{tenant="alice"} 0`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, resp3 := abrirShell(t, srv, tokenAlice, sb.ID); resp3.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("tras cerrar la primera, una nueva shell debía admitirse: %d", resp3.StatusCode)
	}
}

func TestListaPlantillasYEnQueHostsEstan(t *testing.T) {
	a, b := nuevoFalso(t), nuevoFalso(t)
	a.snapshots = []string{"sbx-node"}
	b.snapshots = []string{"sbx-node", "sbx-py"}
	rec, _ := json.Marshal(plantilla.Receta{
		Plantilla: plantilla.Plantilla{Nombre: "node", Imagen: "toolchain"},
		Hash:      "deadbeef",
		Hecho:     time.Now().UTC().Truncate(time.Second),
	})
	a.snapAnot = map[string]map[string]json.RawMessage{"sbx-node": {plantilla.AnotacionReceta: rec}}
	srv, _ := montar(t, map[string]*falso{"a": a, "b": b})

	esperaCodigo(t, pide(t, srv, "", "GET", "/v1/templates", nil), 401)

	resp := pide(t, srv, tokenAlice, "GET", "/v1/templates", nil)
	esperaCodigo(t, resp, 200)
	lista := decodifica[[]TemplateInfo](t, resp)
	if len(lista) != 2 || lista[0].Name != "node" || lista[1].Name != "py" {
		t.Fatalf("plantillas = %+v", lista)
	}
	node := lista[0]
	if len(node.Hosts) != 2 || node.Hosts[0].Host != "a" || node.Hosts[1].Host != "b" {
		t.Fatalf("hosts de node = %+v", node.Hosts)
	}
	if node.Hosts[0].Recipe != "deadbeef" || node.Hosts[0].BuiltAt.IsZero() {
		t.Fatalf("host a de node debía traer su receta: %+v", node.Hosts[0])
	}
	if node.Hosts[1].Recipe != "" {
		t.Fatalf("host b de node no tiene anotación, no debía traer receta: %+v", node.Hosts[1])
	}
	py := lista[1]
	if len(py.Hosts) != 1 || py.Hosts[0].Host != "b" {
		t.Fatalf("hosts de py = %+v", py.Hosts)
	}
}

// El corazón de la tarea 3: un reinicio del host invalida su dorado, y crear un
// sandbox de esa plantilla en ese host falla con el mensaje que api.EsFalloTSC
// reconoce. El frontal tiene que contestar 503 con Retry-After y reconstruir
// solo, en segundo plano, con la receta que colgaba del snapshot roto.
func TestPlantillaInvalidaPorTSCSeReconstruyeSolaYLuegoSirve(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	p := plantilla.Plantilla{Nombre: "node", Imagen: "toolchain"}
	rec, _ := json.Marshal(plantilla.Receta{Plantilla: p, Hash: plantilla.HashReceta(p), Hecho: time.Now()})
	f.snapAnot = map[string]map[string]json.RawMessage{"sbx-node": {plantilla.AnotacionReceta: rec}}
	f.crearErrTSC = true
	srv, _ := montar(t, map[string]*falso{"a": f})

	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "node"})
	esperaCodigo(t, resp, http.StatusServiceUnavailable)
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("esperaba una cabecera Retry-After")
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "rebuild") {
		t.Fatalf("el mensaje no dice que se está reconstruyendo: %s", b)
	}

	// La reconstrucción corre en segundo plano: se espera a que termine (el
	// falso apaga crearErrTSC en cuanto ve el commit de plantilla.Construir).
	limite := time.Now().Add(5 * time.Second)
	for time.Now().Before(limite) {
		f.mu.Lock()
		sigue := f.crearErrTSC
		f.mu.Unlock()
		if !sigue {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.crearErrTSC {
		t.Fatal("la reconstrucción en segundo plano no terminó a tiempo")
	}

	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	if sb.Template != "node" {
		t.Fatalf("sandbox tras la reconstrucción: %+v", sb)
	}
}

// Mientras un host tiene su dorado roto, otro host sano con la misma plantilla
// sigue sirviendo: hosts.Intentar reintenta el fallo de TSC en otro candidato
// antes de rendirse, igual que "no cabe".
func TestOtroHostSanoSigueSirviendoLaPlantillaRota(t *testing.T) {
	roto, sano := nuevoFalso(t), nuevoFalso(t)
	roto.snapshots, sano.snapshots = []string{"sbx-node"}, []string{"sbx-node"}
	roto.libreMiB, sano.libreMiB = 8192, 1024 // roto tiene más hueco: se probaría primero
	roto.crearErrTSC = true
	srv, _ := montar(t, map[string]*falso{"roto": roto, "sano": sano})

	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	if sb.Host != "sano" {
		t.Fatalf("host = %s, quería sano (el otro tiene el dorado invalidado)", sb.Host)
	}
}

func TestListaSigueConUnHostCaido(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f, "muerto": {endpoint: "/nonexistent/kling.sock"}})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	resp := pide(t, srv, tokenAlice, "GET", "/v1/sandboxes", nil)
	esperaCodigo(t, resp, 200)
	if resp.Header.Get("X-Kindling-Partial") == "" {
		t.Fatal("expected a partial-results warning")
	}
	if l := decodifica[[]Sandbox](t, resp); len(l) != 1 || l[0].ID != sb.ID {
		t.Fatalf("list: %+v", l)
	}
}
