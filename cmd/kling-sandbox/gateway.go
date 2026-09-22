package main

// `kling sbx gateway`: el proceso que escucha.
//
// Es lo único de esta extensión que corre como servicio. Habla con los daemons
// de kindling por su socket (o por SSH) y expone hacia la red una API con token,
// inquilinos y cuotas. El daemon sigue sin escuchar en ningún puerto, que es la
// barrera que de verdad protege el host.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/frontal"
	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling-sandbox/internal/pool"
)

func cmdGateway(args []string) error {
	fs := flag.NewFlagSet("sbx gateway", flag.ExitOnError)
	listen := fs.String("listen", "", "address to listen on (default sandbox.listen, or :8090)")
	imagen := fs.String("image", "", "image used when a request brings neither template nor image")
	abandono := fs.Duration("abandon", 0, "how long a sandbox may sit unused before it is destroyed (default 24h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := leerAjustes()
	if err != nil {
		return err
	}
	if *listen != "" {
		a.Listen = *listen
	}
	if *abandono > 0 {
		a.Reap = *abandono
	}

	tenants, err := leerTenants()
	if err != nil {
		return err
	}
	reg := hosts.Nuevo(a.Hosts)

	srv, err := frontal.Nuevo(frontal.Opciones{
		Hosts:            reg,
		Tenants:          tenants,
		ImagenPorDefecto: *imagen,
		Abandono:         a.Reap,
		Log:              log.New(os.Stderr, "", log.LstdFlags),
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.Vigilar(ctx)
	// El fondo de precalentadas: lo que hace que crear un sandbox de una
	// plantilla popular cueste una llamada HTTP en vez de un thaw.
	pool.Nuevo(reg, 30*time.Second, log.New(os.Stderr, "", log.LstdFlags)).Vigilar(ctx)

	ln, err := net.Listen("tcp", a.Listen)
	if err != nil {
		return err
	}
	// Sin WriteTimeout: por aquí pasan sesiones de shell y ejecuciones largas, y
	// un plazo global las cortaría a media faena. Lo que sí se acota es la
	// espera a las CABECERAS, que es lo que separa "está trabajando" de "no hay
	// nadie".
	hsrv := &http.Server{Handler: srv, ReadHeaderTimeout: 20 * time.Second}

	log.Printf("sandbox gateway on %s · %d tenant(s) · hosts: %s", ln.Addr(), len(tenants), strings.Join(nombres(a.Hosts), ", "))
	for _, e := range reg.Estados(ctx) {
		if e.Vivo {
			log.Printf("  %s: kindling %s, %d machine(s), %d MiB available", e.Nombre, e.Version, e.Maquinas, e.DisponibleMB)
		} else {
			log.Printf("  %s: not answering (%s)", e.Nombre, e.Error)
		}
	}

	errc := make(chan error, 1)
	go func() { errc <- hsrv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("shutting down")
		// Plazo corto: las sesiones de shell son conexiones secuestradas y
		// Shutdown no las espera; lo que se cuida aquí son las peticiones
		// normales a medio contestar.
		cerrar, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return hsrv.Shutdown(cerrar)
	}
}

func nombres(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// leerTenants saca los inquilinos del entorno.
//
// Del entorno y no de la configuración porque son SECRETOS y el gateway corre
// como servicio: /proc/<pid>/environ lo lee su dueño y root, mientras que un
// config.json legible se copia sin querer. Es el mismo trato que da el gateway
// de kindling-mcp a su token.
//
//	KLING_SANDBOX_TOKEN=xxx                      un inquilino "default", sin tope
//	KLING_SANDBOX_TENANTS=ana:tok1:10,bob:tok2:5 nombre:token:máximo de sandboxes
func leerTenants() ([]frontal.Tenant, error) {
	var out []frontal.Tenant
	if v := os.Getenv("KLING_SANDBOX_TOKEN"); v != "" {
		out = append(out, frontal.Tenant{Nombre: "default", Token: v})
	}
	for _, t := range strings.Split(os.Getenv("KLING_SANDBOX_TENANTS"), ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		partes := strings.Split(t, ":")
		if len(partes) < 2 || partes[0] == "" || partes[1] == "" {
			return nil, fmt.Errorf("KLING_SANDBOX_TENANTS: %q is not name:token[:max]", t)
		}
		te := frontal.Tenant{Nombre: partes[0], Token: partes[1]}
		if len(partes) > 2 && partes[2] != "" {
			n, err := strconv.Atoi(partes[2])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("KLING_SANDBOX_TENANTS: %q: max has to be a number", t)
			}
			te.MaxSandboxes = n
		}
		out = append(out, te)
	}
	if len(out) == 0 {
		return nil, errors.New("no tenants: set KLING_SANDBOX_TOKEN (or KLING_SANDBOX_TENANTS) before serving")
	}
	return out, nil
}

// hookStatus añade a `kling status` lo que sabe esta extensión.
func hookStatus(args []string, w io.Writer) error {
	a, err := leerAjustes()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(a.URL, "/")+"/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		fmt.Fprintf(w, "sandboxes:   ✗ no gateway at %s\n", a.URL)
		fmt.Fprintf(w, "             start one with:  kling sbx gateway\n")
		return nil
	}
	defer resp.Body.Close()
	fmt.Fprintf(w, "sandboxes:   %s\n", a.URL)
	if resp.StatusCode == http.StatusOK {
		fmt.Fprintf(w, "  gateway:   ✓ alive\n")
	} else {
		fmt.Fprintf(w, "  gateway:   ✗ answered %s\n", resp.Status)
	}
	return nil
}
