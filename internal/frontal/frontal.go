// Package frontal es el servidor HTTP que se pone DELANTE de uno o varios
// daemons de kindling para ofrecer sandboxes a agentes de código.
//
// Existe porque el daemon de kindling escucha solo en un socket Unix y equivale
// a root en su host: no admite autenticación ni puede exponerse a la red. Igual
// que el gateway de MCP, este frontal es la única pieza que habla con la red, y
// lo que hace es autenticar, contabilizar y traducir: cada petición se
// convierte en una o varias llamadas al daemon por su cliente oficial.
//
// LO QUE ESTO NO ES. Los "tenants" de este paquete NO son aislamiento entre
// clientes. Todos los sandboxes de todos los tenants corren en los mismos
// daemons, en los mismos hosts, con el mismo kernel de host y la misma red de
// invitados. Lo que se garantiza es REPARTO Y CONTABILIDAD: un tenant no ve, no
// toca y no puede pasarse de su cuota de sandboxes, y cada máquina lleva
// etiquetado quién la creó. Si dos clientes no deben compartir un host —porque
// uno pudiera atacar al otro a través del hipervisor, del daemon o del
// scheduler— la respuesta no está aquí: son hosts separados, cada uno con su
// frontal o con su propio conjunto de tokens.
//
// El frontal no guarda NADA en disco. Todo lo que sabe de un sandbox lo saca de
// las etiquetas de la máquina en el daemon (kind=sandbox, tenant=..., template=...)
// y de sus relojes (TTLAt, FrozenAt). Así, reiniciar el frontal no pierde
// nada, dos frontales sobre los mismos hosts ven lo mismo, y `kling sandbox ls`
// en el host enseña exactamente la misma verdad. El store del daemon
// (PutStore/GetStore) no hace falta hoy: no hay ningún dato que no quepa en una
// etiqueta.
package frontal

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// Etiquetas con las que el frontal marca sus máquinas en el daemon. Son el
// único "estado" del frontal, y por tanto el contrato con el paquete plantilla
// (que crea las instancias precalentadas) y con quien mire las máquinas a mano.
const (
	// LabelTenant es quién creó el sandbox. Una máquina kind=sandbox con
	// plantilla y SIN esta etiqueta es una instancia precalentada, libre para el
	// primero que la reclame (ver reclamarPrecalentada).
	LabelTenant = "tenant"
	// LabelTemplate es la plantilla de la que nació el sandbox.
	LabelTemplate = "template"
)

// Tenant es un token con nombre y sus límites.
type Tenant struct {
	Nombre string
	Token  string
	// MaxSandboxes es cuántos sandboxes vivos puede tener a la vez (0 = sin
	// tope). MaxPorPlantilla acota lo mismo por plantilla.
	MaxSandboxes    int
	MaxPorPlantilla int
	// MaxShells es cuántas sesiones de shell interactivas puede tener abiertas
	// a la vez (0 = sin tope). Cuenta aparte de MaxSandboxes: una shell no es un
	// sandbox nuevo, pero sí un proceso del daemon y una conexión que este
	// frontal mantiene en pie, y ambos son un recurso que un tenant puede acaparar.
	MaxShells int
}

// Opciones configura el frontal.
type Opciones struct {
	Hosts   *hosts.Registro
	Tenants []Tenant

	// ImagenPorDefecto se usa cuando la petición no trae ni template ni image.
	// Vacía: la petición tiene que decir una de las dos.
	ImagenPorDefecto string
	// TTLPorDefecto en segundos; 0 = api.SandboxDefaultTTL.
	TTLPorDefecto int

	// Abandono es cuánto puede llevar un sandbox sin usarse antes de que la
	// limpieza lo borre. Es el segundo plazo, distinto del TTL: el TTL lo
	// duerme (on_ttl=freeze), este lo destruye. 0 = 24 h.
	Abandono time.Duration
	// CadaLimpieza es el periodo de la vuelta de limpieza. 0 = 5 min.
	CadaLimpieza time.Duration

	Log *log.Logger
}

// Servidor es el frontal. Implementa http.Handler.
type Servidor struct {
	reg      *hosts.Registro
	tenants  []Tenant
	imagen   string
	ttl      int
	abandono time.Duration
	cada     time.Duration
	log      *log.Logger
	mux      *http.ServeMux

	// mu protege enVuelo y serializa la reclamación de precalentadas. Las
	// cuotas se cuentan contra lo que el daemon tiene vivo, y una creación tarda
	// segundos en aparecer ahí: sin contar las que están en camino, N peticiones
	// simultáneas pasarían todas la cuota.
	mu      sync.Mutex
	enVuelo map[string]int

	// shells cuenta las sesiones de shell abiertas por tenant AHORA MISMO. Sirve
	// dos propósitos con el mismo número: la cuota de Tenant.MaxShells y la
	// métrica kling_sandbox_shell_sessions de /v1/metrics.
	shells contadorTenant
	// metricas acumula lo que /v1/metrics no puede sacar preguntando a los
	// daemons: cuántas creaciones ha habido y de qué tipo, y cuánto tardaron.
	metricas metricas
	// reconstruir vigila qué (host, plantilla) se están reconstruyendo ahora
	// mismo tras un fallo de TSC, para no lanzar la misma reconstrucción dos
	// veces (ver reconstruir.go).
	reconstruir reconstrucciones
}

// nombreValido acota nombres de tenant y de host: van en etiquetas y en ids
// visibles ("host/máquina"), así que ni barras ni espacios ni vacíos.
var nombreValido = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Nuevo construye el frontal. Falla si no hay ningún tenant con token: este
// servidor se expone a la red y sin token sería el daemon a pelo.
func Nuevo(o Opciones) (*Servidor, error) {
	if o.Hosts == nil {
		return nil, errors.New("no hosts registry")
	}
	for _, h := range o.Hosts.Todos() {
		if !nombreValido.MatchString(h.Nombre) {
			return nil, fmt.Errorf("host name %q is not valid (letters, digits, '.', '_', '-')", h.Nombre)
		}
	}
	vistos := map[string]bool{}
	var tenants []Tenant
	for _, t := range o.Tenants {
		if !nombreValido.MatchString(t.Nombre) {
			return nil, fmt.Errorf("tenant name %q is not valid (letters, digits, '.', '_', '-')", t.Nombre)
		}
		if vistos[t.Nombre] {
			return nil, fmt.Errorf("tenant %q is configured twice", t.Nombre)
		}
		vistos[t.Nombre] = true
		// Un tenant sin token no protege nada: mejor negarse a arrancar que
		// registrar una entrada que aceptaría la cadena vacía.
		if t.Token == "" {
			return nil, fmt.Errorf("tenant %q has an empty token", t.Nombre)
		}
		if t.MaxSandboxes < 0 || t.MaxPorPlantilla < 0 || t.MaxShells < 0 {
			return nil, fmt.Errorf("tenant %q: limits can't be negative", t.Nombre)
		}
		tenants = append(tenants, t)
	}
	if len(tenants) == 0 {
		return nil, errors.New("no tenants configured: the frontal refuses to serve without authentication")
	}
	s := &Servidor{
		reg: o.Hosts, tenants: tenants, imagen: o.ImagenPorDefecto,
		ttl: o.TTLPorDefecto, abandono: o.Abandono, cada: o.CadaLimpieza,
		log: o.Log, enVuelo: map[string]int{},
	}
	if s.ttl == 0 {
		s.ttl = api.SandboxDefaultTTL
	}
	if s.ttl < 1 || s.ttl > api.SandboxMaxTTL {
		return nil, fmt.Errorf("default ttl must be between 1 and %d seconds", api.SandboxMaxTTL)
	}
	if s.abandono == 0 {
		s.abandono = 24 * time.Hour
	}
	if s.cada == 0 {
		s.cada = 5 * time.Minute
	}
	if s.log == nil {
		s.log = log.Default()
	}
	s.rutas()
	return s, nil
}

func (s *Servidor) rutas() {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/health", s.handleHealth)
	m.Handle("GET /v1/hosts", s.auth(http.HandlerFunc(s.handleHosts)))
	// Cualquier token de tenant vale para /v1/metrics y /v1/templates: no dan
	// nada de OTROS tenants (métricas agregadas, catálogo de plantillas), y
	// exigir un token distinto solo obligaría a repartir un segundo secreto sin
	// ganar nada a cambio.
	m.Handle("GET /v1/metrics", s.auth(http.HandlerFunc(s.handleMetrics)))
	m.Handle("GET /v1/templates", s.auth(http.HandlerFunc(s.handleTemplates)))
	m.Handle("POST /v1/sandboxes", s.auth(http.HandlerFunc(s.handleCrear)))
	m.Handle("GET /v1/sandboxes", s.auth(http.HandlerFunc(s.handleListar)))
	// Las rutas por sandbox se despachan a mano: el id lleva una barra dentro
	// ("host/máquina") y el ServeMux no deja que un comodín la contenga.
	m.Handle("/v1/sandboxes/", s.auth(http.HandlerFunc(s.handleSandbox)))
	s.mux = m
}

// ServeHTTP despacha.
func (s *Servidor) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ---- autenticación

type claveTenant struct{}

// auth exige un Bearer que coincida con el token de algún tenant y deja el
// tenant en el contexto. Todos los tokens se comparan siempre, sin cortar en el
// primer acierto, para que el tiempo de respuesta no delate cuál coincidió.
// ConstantTimeCompare ya devuelve 0 cuando difieren las longitudes.
func (s *Servidor) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			authFail(w)
			return
		}
		var t *Tenant
		for i := range s.tenants {
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.tenants[i].Token)) == 1 {
				t = &s.tenants[i]
			}
		}
		if t == nil {
			authFail(w)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claveTenant{}, t)))
	})
}

func bearer(h string) (string, bool) {
	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(p):])
	return tok, tok != ""
}

func authFail(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="kindling-sandbox"`)
	fail(w, http.StatusUnauthorized, errors.New("missing Authorization header: Bearer <token>, or the token is invalid"))
}

// tenantDe devuelve el tenant que auth dejó en el contexto. Nunca es nil detrás
// de auth; si lo fuera es un error de programación, no una petición anónima.
func tenantDe(r *http.Request) *Tenant {
	t, _ := r.Context().Value(claveTenant{}).(*Tenant)
	return t
}

// ---- rutas sueltas

func (s *Servidor) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hosts": len(s.reg.Todos())})
}

func (s *Servidor) handleHosts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reg.Estados(r.Context()))
}

// ---- respuestas

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail responde un error con su código. Si el error viene del daemon con
// código propio, ese manda: un 507 o un 409 del daemon significan algo concreto
// para quien llama y aplastarlos con un 502 perdería justo eso.
func fail(w http.ResponseWriter, code int, err error) {
	var se *api.StatusError
	if errors.As(err, &se) && se.Code != 0 {
		code = se.Code
	}
	writeJSON(w, code, api.Error{Message: err.Error()})
}

// codigoDeHost traduce el error de hosts.Intentar a un estado HTTP.
func codigoDeHost(err error) int {
	switch {
	case errors.Is(err, hosts.ErrSinHosts), errors.Is(err, hosts.ErrSinSitio):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	}
	var se *api.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return http.StatusBadGateway
}
