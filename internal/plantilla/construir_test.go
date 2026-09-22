package plantilla

// Los tests corren contra un daemon FALSO: un httptest sobre un socket Unix que
// sirve las rutas que usa Construir. Tiene que ser Unix y no http:// porque el
// cliente de kindling no habla TCP a propósito (pkg/transport: un daemon de
// microVMs equivale a root en su host), así que api.NewClient solo entiende
// unix:// y ssh://.

import (
	"encoding/json"
	"errors"
	"fmt"
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

type anotacionEscrita struct {
	Snapshot string
	Clave    string
	Valor    json.RawMessage
}

// daemonFalso es kindling reducido a lo que Construir le pide, y con memoria de
// lo que se le pidió.
type daemonFalso struct {
	mu        sync.Mutex
	corridas  []api.RunRequest
	execs     []api.ExecRequest
	commits   []api.CommitRequest
	borradas  []string
	anotadas  []anotacionEscrita
	snapshots map[string]*api.Snapshot

	// salida decide qué contesta el exec número n (n empieza en 1, y el 1 es
	// siempre el sondeo con el que Construir espera al agente). nil = todo bien.
	salida func(n int, r api.ExecRequest) []api.ExecEvent
}

func (d *daemonFalso) mux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		var req api.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fallo(w, http.StatusBadRequest, err.Error())
			return
		}
		d.mu.Lock()
		d.corridas = append(d.corridas, req)
		d.mu.Unlock()
		escribir(w, http.StatusCreated, api.Machine{
			ID: "m-1", Name: "build-1", Image: req.Image, State: api.StateRunning,
			AllowExec: req.AllowExec, IP: "10.0.0.2",
		})
	})

	mux.HandleFunc("POST /machines/{ref}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req api.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fallo(w, http.StatusBadRequest, err.Error())
			return
		}
		d.mu.Lock()
		d.execs = append(d.execs, req)
		n := len(d.execs)
		salida := d.salida
		d.mu.Unlock()

		eventos := []api.ExecEvent{{Exit: intPtr(0), DurationMS: 7}}
		if salida != nil {
			if ev := salida(n, req); ev != nil {
				eventos = ev
			}
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, ev := range eventos {
			if err := enc.Encode(ev); err != nil {
				return
			}
		}
	})

	mux.HandleFunc("POST /machines/{ref}/commit", func(w http.ResponseWriter, r *http.Request) {
		var req api.CommitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fallo(w, http.StatusBadRequest, err.Error())
			return
		}
		d.mu.Lock()
		d.commits = append(d.commits, req)
		sn := &api.Snapshot{Name: req.Name, Image: "toolchain", CreatedAt: time.Now().UTC(), AllowExec: true}
		if d.snapshots == nil {
			d.snapshots = map[string]*api.Snapshot{}
		}
		d.snapshots[req.Name] = sn
		d.mu.Unlock()
		escribir(w, http.StatusOK, sn)
	})

	mux.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.borradas = append(d.borradas, r.PathValue("ref"))
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		sn := d.snapshots[r.PathValue("name")]
		d.mu.Unlock()
		if sn == nil {
			fallo(w, http.StatusNotFound, "no such snapshot")
			return
		}
		escribir(w, http.StatusOK, sn)
	})

	mux.HandleFunc("PUT /snapshots/{name}/annotations/{key}", func(w http.ResponseWriter, r *http.Request) {
		var valor json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&valor); err != nil {
			fallo(w, http.StatusBadRequest, err.Error())
			return
		}
		nombre, clave := r.PathValue("name"), r.PathValue("key")
		d.mu.Lock()
		d.anotadas = append(d.anotadas, anotacionEscrita{Snapshot: nombre, Clave: clave, Valor: valor})
		sn := d.snapshots[nombre]
		if sn == nil {
			fallo(w, http.StatusNotFound, "no such snapshot")
			d.mu.Unlock()
			return
		}
		if sn.Annotations == nil {
			sn.Annotations = map[string]json.RawMessage{}
		}
		sn.Annotations[clave] = valor
		d.mu.Unlock()
		escribir(w, http.StatusOK, sn)
	})

	return mux
}

func escribir(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fallo(w http.ResponseWriter, code int, msg string) {
	escribir(w, code, api.Error{Message: msg})
}

func intPtr(i int) *int { return &i }

// arrancarConHandler sirve h en un socket Unix y devuelve un cliente de
// kindling apuntado a él.
func arrancarConHandler(t *testing.T, h http.Handler) *api.Client {
	t.Helper()
	// El socket va en un temporal corto y no en t.TempDir(): sun_path son 104
	// bytes en macOS y la ruta de t.TempDir(), que lleva dentro el nombre del
	// test, se acerca al límite.
	dir, err := os.MkdirTemp("", "kls")
	if err != nil {
		t.Fatalf("no se pudo crear el temporal: %v", err)
	}
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("no se pudo escuchar en %s: %v", sock, err)
	}
	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		_ = os.RemoveAll(dir)
	})
	return api.NewClient("unix://" + sock)
}

// arrancar levanta el daemon falso y devuelve un cliente apuntado a él.
func arrancar(t *testing.T, d *daemonFalso) *api.Client {
	t.Helper()
	return arrancarConHandler(t, d.mux())
}

func plantillaDemo() Plantilla {
	return Plantilla{
		Nombre: "python-datos",
		Imagen: "toolchain",
		MemMiB: 512,
		VCPUs:  2,
		Pasos: []Paso{
			{Cmd: []string{"pip", "install", "pandas"}, Timeout: 600},
			{Cmd: []string{"sh", "-c", "echo listo > /opt/marca"}, Dir: "/opt", Env: []string{"LC_ALL=C"}},
		},
	}
}

func TestConstruirDejaSnapshotConRecetaYNoDejaMaquina(t *testing.T) {
	d := &daemonFalso{}
	c := arrancar(t, d)
	p := plantillaDemo()

	est, err := Construir(t.Context(), c, p)
	if err != nil {
		t.Fatalf("Construir: %v", err)
	}

	if len(d.corridas) != 1 {
		t.Fatalf("máquinas creadas = %d, esperaba 1", len(d.corridas))
	}
	run := d.corridas[0]
	if !run.AllowExec {
		t.Error("la máquina de preparación se creó sin AllowExec: no habría pasos que ejecutar")
	}
	if run.Egress != EgressPorDefecto {
		t.Errorf("egress = %q, esperaba %q: preparar es instalar", run.Egress, EgressPorDefecto)
	}
	if run.Image != "toolchain" || run.MemMiB != 512 || run.VCPUs != 2 {
		t.Errorf("la máquina no heredó la plantilla: %+v", run)
	}
	if run.Labels[api.LabelKind] != KindConstruccion || run.Labels[EtiquetaPlantilla] != p.Nombre {
		t.Errorf("etiquetas = %v, esperaba que dijeran que es una máquina de preparación", run.Labels)
	}
	if run.OnTTL != api.OnTTLRemove || run.TTLSeconds <= 0 {
		t.Errorf("sin red de seguridad: on_ttl=%q ttl=%d", run.OnTTL, run.TTLSeconds)
	}

	// El primer exec es la espera al agente; después, los pasos en orden.
	if len(d.execs) != 3 {
		t.Fatalf("execs = %d, esperaba 3 (sondeo + 2 pasos): %+v", len(d.execs), d.execs)
	}
	if got := strings.Join(d.execs[1].Cmd, " "); got != "pip install pandas" {
		t.Errorf("primer paso = %q", got)
	}
	if got := strings.Join(d.execs[2].Cmd, " "); got != "sh -c echo listo > /opt/marca" {
		t.Errorf("segundo paso = %q", got)
	}
	if d.execs[2].Dir != "/opt" || len(d.execs[2].Env) != 1 {
		t.Errorf("el paso perdió su dir o su entorno: %+v", d.execs[2])
	}
	if d.execs[1].TimeoutSeconds != 600 {
		t.Errorf("timeout del paso = %d, esperaba 600", d.execs[1].TimeoutSeconds)
	}

	if len(d.commits) != 1 || d.commits[0].Name != SnapshotDe(p.Nombre) || !d.commits[0].Replace {
		t.Fatalf("commits = %+v, esperaba uno a %q con replace", d.commits, SnapshotDe(p.Nombre))
	}

	if len(d.anotadas) != 1 {
		t.Fatalf("anotaciones = %d, esperaba 1", len(d.anotadas))
	}
	an := d.anotadas[0]
	if an.Snapshot != SnapshotDe(p.Nombre) || an.Clave != AnotacionReceta {
		t.Errorf("anotación en %s/%s, esperaba %s/%s", an.Snapshot, an.Clave, SnapshotDe(p.Nombre), AnotacionReceta)
	}
	var rec Receta
	if err := json.Unmarshal(an.Valor, &rec); err != nil {
		t.Fatalf("la receta guardada no se puede leer: %v", err)
	}
	if rec.Hash != HashReceta(p) {
		t.Errorf("hash guardado = %q, esperaba %q", rec.Hash, HashReceta(p))
	}
	if rec.Plantilla.Nombre != p.Nombre || len(rec.Plantilla.Pasos) != 2 {
		t.Errorf("la receta guardada no lleva la plantilla entera: %+v", rec.Plantilla)
	}
	if rec.Hecho.IsZero() {
		t.Error("la receta guardada no lleva fecha")
	}

	if len(d.borradas) != 1 || d.borradas[0] != "m-1" {
		t.Errorf("máquinas borradas = %v, esperaba [m-1]: una huérfana retiene RAM", d.borradas)
	}

	if est.Snapshot != SnapshotDe(p.Nombre) || est.Receta != HashReceta(p) || est.Hecho.IsZero() {
		t.Errorf("estado devuelto = %+v", est)
	}
	if est.Host == "" {
		t.Error("el estado no dice en qué host quedó el snapshot")
	}
}

func TestConstruirAbortaEnUnPasoQueFallaYBorraLaMaquinaIgual(t *testing.T) {
	d := &daemonFalso{
		salida: func(n int, r api.ExecRequest) []api.ExecEvent {
			if n != 2 { // el sondeo y lo que venga después, bien
				return nil
			}
			return []api.ExecEvent{
				{Stream: "stdout", Data: []byte("Collecting pandas\n")},
				{Stream: "stderr", Data: []byte("ERROR: no matching distribution\n")},
				{Exit: intPtr(3), DurationMS: 120},
			}
		},
	}
	c := arrancar(t, d)
	p := plantillaDemo()

	_, err := Construir(t.Context(), c, p)
	if err == nil {
		t.Fatal("Construir no falló con un paso que sale con código 3")
	}
	var ep *ErrorPaso
	if !errors.As(err, &ep) {
		t.Fatalf("error = %T (%v), esperaba *ErrorPaso", err, err)
	}
	if ep.Codigo != 3 || ep.Indice != 1 {
		t.Errorf("paso %d con código %d, esperaba el 1 con 3", ep.Indice, ep.Codigo)
	}
	msg := err.Error()
	for _, quiero := range []string{"pip install pandas", "code 3", "no matching distribution"} {
		if !strings.Contains(msg, quiero) {
			t.Errorf("el error no dice %q:\n%s", quiero, msg)
		}
	}

	if len(d.commits) != 0 {
		t.Errorf("se hizo commit de una preparación que falló: %+v", d.commits)
	}
	if len(d.execs) != 2 {
		t.Errorf("execs = %d, esperaba que se parara en el paso que falló", len(d.execs))
	}
	if len(d.borradas) != 1 {
		t.Errorf("máquinas borradas = %v, esperaba que se borrara también al fallar", d.borradas)
	}
}

func TestConstruirAgregaElNDJSONDeLaExecEnStreaming(t *testing.T) {
	// El daemon manda la salida a trozos, partidos por donde le venga: un mismo
	// renglón puede llegar en dos tramas. Lo que se enseña en el error tiene que
	// ser el texto entero, y recortado por el final.
	var eventos []api.ExecEvent
	eventos = append(eventos,
		api.ExecEvent{Stream: "stdout", Data: []byte("insta")},
		api.ExecEvent{Stream: "stdout", Data: []byte("lando paquetes\n")},
	)
	for i := 1; i <= 50; i++ {
		eventos = append(eventos, api.ExecEvent{Stream: "stderr", Data: []byte(fmt.Sprintf("aviso %d\n", i))})
	}
	eventos = append(eventos, api.ExecEvent{Exit: intPtr(1), DurationMS: 3, Truncated: true})

	d := &daemonFalso{salida: func(n int, r api.ExecRequest) []api.ExecEvent {
		if n != 2 {
			return nil
		}
		return eventos
	}}
	c := arrancar(t, d)

	_, err := Construir(t.Context(), c, plantillaDemo())
	var ep *ErrorPaso
	if !errors.As(err, &ep) {
		t.Fatalf("error = %T (%v), esperaba *ErrorPaso", err, err)
	}
	if ep.Stdout != "instalando paquetes" {
		t.Errorf("stdout agregado = %q, esperaba las dos tramas unidas", ep.Stdout)
	}
	if !strings.Contains(ep.Stderr, "aviso 50") {
		t.Errorf("el error no conserva el final de stderr:\n%s", ep.Stderr)
	}
	if strings.Contains(ep.Stderr, "aviso 1\n") {
		t.Errorf("el error vuelca stderr entero en vez de las últimas líneas:\n%s", ep.Stderr)
	}
	if !ep.Truncated {
		t.Error("se perdió la marca de salida recortada")
	}
	if !strings.Contains(err.Error(), "output was truncated") {
		t.Errorf("el mensaje no avisa de que la salida venía recortada:\n%s", err)
	}
}

func TestReconciliarNoConstruyeCuandoLaRecetaCuadra(t *testing.T) {
	p := plantillaDemo()
	hecho := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	d := &daemonFalso{snapshots: map[string]*api.Snapshot{
		SnapshotDe(p.Nombre): conReceta(t, p, HashReceta(p), hecho),
	}}
	c := arrancar(t, d)

	est, construido, err := Reconciliar(t.Context(), c, p)
	if err != nil {
		t.Fatalf("Reconciliar: %v", err)
	}
	if construido {
		t.Error("reconstruyó un snapshot que ya era el que pedía la receta")
	}
	if len(d.corridas) != 0 {
		t.Errorf("arrancó %d máquinas sin necesitarlo", len(d.corridas))
	}
	if est.Receta != HashReceta(p) || !est.Hecho.Equal(hecho) {
		t.Errorf("estado = %+v, esperaba el del snapshot que ya estaba", est)
	}
}

func TestReconciliarConstruyeCuandoLaRecetaCambiaFaltaOEsIlegible(t *testing.T) {
	p := plantillaDemo()
	otra := p
	otra.Pasos = append(append([]Paso(nil), p.Pasos...), Paso{Cmd: []string{"pip", "install", "polars"}})

	casos := []struct {
		nombre string
		snaps  map[string]*api.Snapshot
	}{
		{"no existe", nil},
		{"otra receta", map[string]*api.Snapshot{SnapshotDe(p.Nombre): conReceta(t, otra, HashReceta(otra), time.Now())}},
		{"sin anotación", map[string]*api.Snapshot{SnapshotDe(p.Nombre): {Name: SnapshotDe(p.Nombre)}}},
		{"anotación ilegible", map[string]*api.Snapshot{SnapshotDe(p.Nombre): {
			Name:        SnapshotDe(p.Nombre),
			Annotations: map[string]json.RawMessage{AnotacionReceta: json.RawMessage(`"esto no es una receta"`)},
		}}},
	}
	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			d := &daemonFalso{snapshots: caso.snaps}
			c := arrancar(t, d)

			est, construido, err := Reconciliar(t.Context(), c, p)
			if err != nil {
				t.Fatalf("Reconciliar: %v", err)
			}
			if !construido {
				t.Fatal("no construyó cuando lo que había no era lo que se pedía")
			}
			if len(d.corridas) != 1 || len(d.commits) != 1 {
				t.Errorf("corridas=%d commits=%d, esperaba una construcción entera", len(d.corridas), len(d.commits))
			}
			if est.Receta != HashReceta(p) {
				t.Errorf("estado con receta %q, esperaba %q", est.Receta, HashReceta(p))
			}
		})
	}
}

func TestReconciliarNoConstruyeCuandoElDaemonNoContesta(t *testing.T) {
	// Un 500 no es "falta el snapshot": construir ahora sería tapar una avería
	// con minutos de trabajo que también van a fallar.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		fallo(w, http.StatusInternalServerError, "disk is on fire")
	})
	mux.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		t.Error("arrancó una máquina pese al error del daemon")
		fallo(w, http.StatusInternalServerError, "no debería llegar aquí")
	})

	c := arrancarConHandler(t, mux)
	if _, _, err := Reconciliar(t.Context(), c, plantillaDemo()); err == nil {
		t.Fatal("Reconciliar tragó un error del daemon y siguió adelante")
	}
}

func TestValidarRechazaLoQueNoSePuedeConstruir(t *testing.T) {
	casos := []struct {
		nombre string
		p      Plantilla
		dice   string
	}{
		{"sin nombre", Plantilla{Imagen: "toolchain"}, "name"},
		{"nombre con mayúsculas", Plantilla{Nombre: "Python", Imagen: "toolchain"}, "name"},
		{"nombre que empieza por guion", Plantilla{Nombre: "-py", Imagen: "toolchain"}, "name"},
		{"sin imagen", Plantilla{Nombre: "py"}, "image"},
		{"egreso inventado", Plantilla{Nombre: "py", Imagen: "t", EgressBuild: "vpn"}, "build_egress"},
		{"allowlist vacía", Plantilla{Nombre: "py", Imagen: "t", EgressBuild: "allowlist"}, "build_allow_domains"},
		{"paso sin comando", Plantilla{Nombre: "py", Imagen: "t", Pasos: []Paso{{}}}, "no command"},
		{"plazo imposible", Plantilla{Nombre: "py", Imagen: "t", Pasos: []Paso{{Cmd: []string{"sh"}, Timeout: 99999}}}, "maximum"},
		{"entorno sin =", Plantilla{Nombre: "py", Imagen: "t", Pasos: []Paso{{Cmd: []string{"sh"}, Env: []string{"HOME"}}}}, "env"},
		{"volumen sin nombre", Plantilla{Nombre: "py", Imagen: "t", Volumes: []Volumen{{Mount: "/data"}}}, "volume"},
	}
	d := &daemonFalso{}
	c := arrancar(t, d)
	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			_, err := Construir(t.Context(), c, caso.p)
			if err == nil {
				t.Fatal("se aceptó una plantilla que no se puede construir")
			}
			if !strings.Contains(err.Error(), caso.dice) {
				t.Errorf("el error no menciona %q: %v", caso.dice, err)
			}
		})
	}
	if len(d.corridas) != 0 {
		t.Errorf("se arrancaron %d máquinas antes de validar", len(d.corridas))
	}
}

func TestHashRecetaSoloCambiaConLoQueCambiaElSnapshot(t *testing.T) {
	p := plantillaDemo()
	p.EgressBuild = ""
	p.AllowBuild = []string{"pypi.org", "files.pythonhosted.org"}

	base := HashReceta(p)
	if base != HashReceta(p) {
		t.Fatal("el hash no es estable entre dos llamadas iguales")
	}

	iguales := map[string]func(Plantilla) Plantilla{
		"el egreso por defecto escrito a mano": func(q Plantilla) Plantilla {
			q.EgressBuild = EgressPorDefecto
			return q
		},
		"los dominios en otro orden": func(q Plantilla) Plantilla {
			q.AllowBuild = []string{"files.pythonhosted.org", "pypi.org"}
			return q
		},
		"otro tamaño de pool": func(q Plantilla) Plantilla {
			q.Pool = 4
			return q
		},
	}
	for nombre, cambio := range iguales {
		if got := HashReceta(cambio(p)); got != base {
			t.Errorf("%s cambió el hash (%s != %s): obligaría a reconstruir por nada", nombre, got, base)
		}
	}

	distintos := map[string]func(Plantilla) Plantilla{
		"otra imagen": func(q Plantilla) Plantilla { q.Imagen = "otra"; return q },
		"un paso más": func(q Plantilla) Plantilla {
			q.Pasos = append(append([]Paso(nil), q.Pasos...), Paso{Cmd: []string{"echo", "hola"}})
			return q
		},
		"los pasos al revés": func(q Plantilla) Plantilla {
			q.Pasos = []Paso{q.Pasos[1], q.Pasos[0]}
			return q
		},
		"otro dominio": func(q Plantilla) Plantilla {
			q.AllowBuild = append(append([]string(nil), q.AllowBuild...), "example.com")
			return q
		},
		"más memoria": func(q Plantilla) Plantilla { q.MemMiB = 1024; return q },
	}
	for nombre, cambio := range distintos {
		if HashReceta(cambio(p)) == base {
			t.Errorf("%s no cambió el hash: se daría por bueno un snapshot que ya no corresponde", nombre)
		}
	}
}

// conReceta fabrica un snapshot que ya lleva su anotación, como el que dejaría
// una construcción anterior.
func conReceta(t *testing.T, p Plantilla, hash string, hecho time.Time) *api.Snapshot {
	t.Helper()
	b, err := json.Marshal(Receta{Plantilla: p, Hash: hash, Hecho: hecho})
	if err != nil {
		t.Fatalf("no se pudo preparar la anotación: %v", err)
	}
	return &api.Snapshot{
		Name:        SnapshotDe(p.Nombre),
		CreatedAt:   hecho,
		AllowExec:   true,
		Annotations: map[string]json.RawMessage{AnotacionReceta: b},
	}
}
