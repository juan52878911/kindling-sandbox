package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// daemonFalso levanta un daemon de mentira en un socket Unix: es lo único que el
// cliente de kindling sabe marcar, porque el daemon de verdad nunca escucha en
// un puerto.
func daemonFalso(t *testing.T, libreMiB int64, vivo bool) string {
	t.Helper()
	// Directorio corto: sun_path son 104 bytes en macOS y t.TempDir() se pasa.
	dir, err := os.MkdirTemp("", "kh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		if !vivo {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(api.Info{Version: "0.7.0", Machines: 1})
	})
	mux.HandleFunc("GET /procstats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ProcStats{AvailableMiB: libreMiB})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix aquí: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return "unix://" + sock
}

// El host con más hueco va primero: es toda la política de colocación que hay, y
// conviene que sea esa y no el orden del fichero de configuración.
func TestCandidatosOrdenaPorHuecoYDejaFueraAlQueNoContesta(t *testing.T) {
	reg := Nuevo(map[string]string{
		"pequeño": daemonFalso(t, 512, true),
		"grande":  daemonFalso(t, 8192, true),
		"caido":   daemonFalso(t, 4096, false),
	})
	cands := reg.Candidatos(context.Background(), nil)
	if len(cands) != 2 {
		t.Fatalf("candidatos = %d, quería 2 (el caído no cuenta)", len(cands))
	}
	if cands[0].Nombre != "grande" || cands[1].Nombre != "pequeño" {
		t.Fatalf("orden %s, %s: quería el de más hueco primero", cands[0].Nombre, cands[1].Nombre)
	}
}

// El filtro es lo que impide mandar un sandbox a un host donde su plantilla no
// está construida.
func TestCandidatosRespetaElFiltro(t *testing.T) {
	reg := Nuevo(map[string]string{
		"a": daemonFalso(t, 1024, true),
		"b": daemonFalso(t, 2048, true),
	})
	cands := reg.Candidatos(context.Background(), func(h *Host) bool { return h.Nombre == "a" })
	if len(cands) != 1 || cands[0].Nombre != "a" {
		t.Fatalf("candidatos = %v", cands)
	}
}

// Lo que justifica tener varios hosts: que el "no cabe" de uno se resuelva en
// otro sin que quien pide se entere.
func TestIntentarReintentaCuandoNoCabeYNoCuandoEsOtraCosa(t *testing.T) {
	reg := Nuevo(map[string]string{
		"lleno": daemonFalso(t, 8192, true), // más hueco: se prueba primero
		"libre": daemonFalso(t, 1024, true),
	})

	// No cabe en el primero: tiene que acabar en el segundo.
	visitados := []string{}
	got, h, err := Intentar(context.Background(), reg, nil, func(ctx context.Context, h *Host) (string, error) {
		visitados = append(visitados, h.Nombre)
		if h.Nombre == "lleno" {
			return "", &api.StatusError{Code: api.StatusInsufficientMemory, Message: "doesn't fit"}
		}
		return "hecho en " + h.Nombre, nil
	})
	if err != nil || got != "hecho en libre" || h.Nombre != "libre" {
		t.Fatalf("got %q, host %v, err %v (visitados %v)", got, h, err, visitados)
	}

	// El tope de máquinas también se reintenta: otro host sí puede aceptarlo.
	_, _, err = Intentar(context.Background(), reg, nil, func(ctx context.Context, h *Host) (string, error) {
		return "", &api.StatusError{Code: api.StatusMachineLimit, Message: "machine limit reached: 256 of 256"}
	})
	if !errors.Is(err, ErrSinSitio) {
		t.Fatalf("con todos llenos: %v, quería ErrSinSitio", err)
	}

	// Un error cualquiera NO se reintenta: repetirlo en otro host solo
	// multiplica el mismo fallo, y el mensaje que llega es el bueno.
	intentos := 0
	_, _, err = Intentar(context.Background(), reg, nil, func(ctx context.Context, h *Host) (string, error) {
		intentos++
		return "", errors.New("la imagen no existe")
	})
	if intentos != 1 || err == nil {
		t.Fatalf("intentos = %d, err = %v: un error normal no se reintenta", intentos, err)
	}
}

// Sin hosts configurados no hay ambigüedad posible: se dice, no se devuelve una
// lista vacía que el que llama interpretará como quiera.
func TestIntentarSinHosts(t *testing.T) {
	if _, _, err := Intentar(context.Background(), Nuevo(nil), nil, func(ctx context.Context, h *Host) (int, error) {
		return 0, nil
	}); !errors.Is(err, ErrSinHosts) {
		t.Fatalf("%v, quería ErrSinHosts", err)
	}
}
