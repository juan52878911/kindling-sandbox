package frontal

// GET /v1/metrics: lo mismo que internal/daemon/metrics.go del núcleo pero
// visto desde el frontal, así que las cifras son las de ESTE proceso (todos
// los hosts, todos los tenants) y no las de un solo daemon.
//
// Escrito a mano, como el del núcleo: son unas pocas líneas de Fprintf y no
// justifican una dependencia externa en un proyecto que hoy no tiene ninguna.
//
// Lo que se puede preguntar a los daemons (sandboxes vivos, precalentadas,
// memoria de los hosts) se pregunta en el momento del scrape: es la misma
// fuente de verdad que /v1/sandboxes y /v1/hosts, y así un reinicio del
// frontal no pierde ninguna cifra. Lo que NO se puede sacar preguntando —
// cuántas creaciones ha habido y de qué tipo, cuánto tardaron, cuántas shells
// hay abiertas ahora— se acumula aparte, en `metricas` y `contadorTenant`.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// Resultados posibles de un intento de creación, para la etiqueta "result" de
// kling_sandbox_creations_total.
const (
	resultadoOK       = "ok"
	resultadoCuota    = "quota"
	resultadoSinSitio = "no_room"
	resultadoError    = "error"
)

// resultados enumera los cuatro en un orden fijo: así el scrape siempre ve las
// cuatro series, aunque alguna esté a cero.
var resultados = []string{resultadoOK, resultadoCuota, resultadoSinSitio, resultadoError}

// resultadoDe clasifica el error final de crear() para la métrica. Se llama
// solo cuando la petición pasó la validación (normalizar) y llegó a pedir la
// cuota: antes de eso no hay intento de creación que contar, solo una
// petición mal formada.
func resultadoDe(err error) string {
	if errors.Is(err, hosts.ErrSinHosts) || errors.Is(err, hosts.ErrSinSitio) {
		return resultadoSinSitio
	}
	return resultadoError
}

// metricas acumula los contadores que /v1/metrics no puede reconstruir
// preguntando a los daemons. Vive fuera del ritmo de una petición: se escribe
// desde handleCrear y se lee de una vez al atender /v1/metrics.
type metricas struct {
	mu sync.Mutex

	creaciones map[string]int64 // resultado -> cuenta

	// duracion es la latencia de las creaciones que llegaron a intentarlo en un
	// daemon (no las rechazadas por cuota, que no tocan la red): suma y cuenta,
	// que es lo que un histograma de un solo bucket sería de todos modos, y no
	// obliga a decidir de antemano dónde poner los cortes.
	duracionSumaSeg float64
	duracionCuenta  int64
}

// registrar apunta un intento de creación. dur es la duración a contar; 0
// significa "no aplica" (por ejemplo, un rechazo por cuota que ni llegó a
// pedirle nada a un daemon).
func (m *metricas) registrar(resultado string, dur time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.creaciones == nil {
		m.creaciones = map[string]int64{}
	}
	m.creaciones[resultado]++
	if dur > 0 {
		m.duracionSumaSeg += dur.Seconds()
		m.duracionCuenta++
	}
}

// foto copia lo acumulado hasta ahora, para escribirlo sin tener el candado
// tomado mientras se escribe la respuesta HTTP.
func (m *metricas) foto() (creaciones map[string]int64, sumaSeg float64, cuenta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	creaciones = make(map[string]int64, len(m.creaciones))
	for k, v := range m.creaciones {
		creaciones[k] = v
	}
	return creaciones, m.duracionSumaSeg, m.duracionCuenta
}

// contadorTenant cuenta "cuántos X tiene abiertos cada tenant ahora mismo".
// Hoy solo lo usan las sesiones de shell, pero no es nada específico de ellas.
type contadorTenant struct {
	mu sync.Mutex
	m  map[string]int64
}

// abrir cuenta una apertura más si no se pasa de max (0 = sin tope), y dice si
// se dejó abrir. No abrir no descuenta: quien llama no debe cerrar algo que
// nunca llegó a contar como abierto.
func (c *contadorTenant) abrir(tenant string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if max > 0 && c.m[tenant] >= int64(max) {
		return false
	}
	if c.m == nil {
		c.m = map[string]int64{}
	}
	c.m[tenant]++
	return true
}

// cerrar descuenta una apertura. Nunca baja de cero: un cierre de más (por un
// error de programación en quien llama) no debe dejar la cuota negativa, que
// se traduciría en "sin tope" para ese tenant.
func (c *contadorTenant) cerrar(tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[tenant] > 0 {
		c.m[tenant]--
	}
}

// cuenta dice cuántas aperturas tiene contadas un tenant ahora mismo. Solo
// para mensajes: la decisión de admitir o no la toma abrir(), bajo el mismo
// candado, para que no haya una ventana entre "leer la cuenta" y "aplicarla".
func (c *contadorTenant) cuenta(tenant string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[tenant]
}

// foto copia el estado actual, por tenant.
func (c *contadorTenant) foto() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// estadosConocidos son los estados que se emiten aunque estén a cero, para que
// un scrape no vea "desaparecer" una serie cuando el contador cae a cero
// (mismo motivo que internal/daemon/metrics.go del núcleo).
var estadosConocidos = []api.State{api.StateRunning, api.StateWarm, api.StateCreated, api.StateStopped, api.StateFailed}

func (s *Servidor) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	porHost, _ := s.enCadaHost(r.Context(), func(ctx context.Context, h *hosts.Host) ([]*api.Machine, error) {
		return h.Cliente.Sandboxes(ctx)
	})
	porTenantEstado := map[[2]string]int64{}
	precalentadas := map[string]int64{}
	for _, ms := range porHost {
		for _, mc := range ms {
			if mc.Labels[api.LabelKind] != api.KindSandbox {
				continue
			}
			tenant := mc.Labels[LabelTenant]
			if tenant == "" {
				// Sin tenant: es una instancia precalentada del fondo, no un
				// sandbox de nadie.
				if tpl := mc.Labels[LabelTemplate]; tpl != "" {
					precalentadas[tpl]++
				}
				continue
			}
			porTenantEstado[[2]string{tenant, string(mc.State)}]++
		}
	}

	fmt.Fprintln(w, "# HELP kling_sandbox_machines Live sandboxes by tenant and state.")
	fmt.Fprintln(w, "# TYPE kling_sandbox_machines gauge")
	for _, t := range s.tenants {
		for _, st := range estadosConocidos {
			fmt.Fprintf(w, "kling_sandbox_machines{tenant=%q,state=%q} %d\n",
				t.Nombre, st, porTenantEstado[[2]string{t.Nombre, string(st)}])
		}
	}

	fmt.Fprintln(w, "# HELP kling_sandbox_creations_total Sandbox creation attempts, by result.")
	fmt.Fprintln(w, "# TYPE kling_sandbox_creations_total counter")
	creaciones, sumaSeg, cuenta := s.metricas.foto()
	for _, res := range resultados {
		fmt.Fprintf(w, "kling_sandbox_creations_total{result=%q} %d\n", res, creaciones[res])
	}

	fmt.Fprintln(w, "# HELP kling_sandbox_create_duration_seconds Time a creation took once it reached a daemon (claims and restores).")
	fmt.Fprintln(w, "# TYPE kling_sandbox_create_duration_seconds summary")
	fmt.Fprintf(w, "kling_sandbox_create_duration_seconds_sum %f\n", sumaSeg)
	fmt.Fprintf(w, "kling_sandbox_create_duration_seconds_count %d\n", cuenta)

	fmt.Fprintln(w, "# HELP kling_sandbox_prewarmed Unclaimed prewarmed instances, by template.")
	fmt.Fprintln(w, "# TYPE kling_sandbox_prewarmed gauge")
	plantillas := make([]string, 0, len(precalentadas))
	for p := range precalentadas {
		plantillas = append(plantillas, p)
	}
	sort.Strings(plantillas)
	for _, p := range plantillas {
		fmt.Fprintf(w, "kling_sandbox_prewarmed{template=%q} %d\n", p, precalentadas[p])
	}

	fmt.Fprintln(w, "# HELP kling_sandbox_host_up Whether a kindling host behind the gateway answered the last check.")
	fmt.Fprintln(w, "# TYPE kling_sandbox_host_up gauge")
	fmt.Fprintln(w, "# HELP kling_sandbox_host_available_mib Memory available on the host, in MiB.")
	fmt.Fprintln(w, "# TYPE kling_sandbox_host_available_mib gauge")
	for _, e := range s.reg.Estados(r.Context()) {
		vivo := 0
		if e.Vivo {
			vivo = 1
		}
		fmt.Fprintf(w, "kling_sandbox_host_up{host=%q} %d\n", e.Nombre, vivo)
		fmt.Fprintf(w, "kling_sandbox_host_available_mib{host=%q} %d\n", e.Nombre, e.DisponibleMB)
	}

	fmt.Fprintln(w, "# HELP kling_sandbox_shell_sessions Open interactive shell sessions, by tenant.")
	fmt.Fprintln(w, "# TYPE kling_sandbox_shell_sessions gauge")
	abiertas := s.shells.foto()
	for _, t := range s.tenants {
		fmt.Fprintf(w, "kling_sandbox_shell_sessions{tenant=%q} %d\n", t.Nombre, abiertas[t.Nombre])
	}
}
