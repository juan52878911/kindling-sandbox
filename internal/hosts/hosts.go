// Package hosts es el inventario de daemons de kindling que hay detrás del
// frontal, y la decisión de en cuál cae cada sandbox.
//
// kindling es de un host: su daemon no sabe que existen otros y sus snapshots no
// viajan (están atados al TSC de su máquina). Todo lo que sea "varios hosts"
// vive aquí, fuera del núcleo, que es donde puede vivir sin mentir: el frontal
// mantiene N clientes, elige uno por hueco libre y reintenta en otro cuando el
// elegido dice que no cabe.
package hosts

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Host es un daemon de kindling.
type Host struct {
	Nombre   string
	Endpoint string
	Cliente  *api.Client
}

// Estado es lo que se sabe de un host ahora mismo.
type Estado struct {
	Nombre       string `json:"name"`
	Endpoint     string `json:"endpoint"`
	Vivo         bool   `json:"alive"`
	Version      string `json:"version,omitempty"`
	Maquinas     int    `json:"machines,omitempty"`
	DisponibleMB int64  `json:"available_mib,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ErrSinHosts es que no hay ninguno configurado.
var ErrSinHosts = errors.New("no kindling hosts configured")

// ErrSinSitio es que ninguno pudo aceptar el trabajo.
var ErrSinSitio = errors.New("no host had room")

// Registro es el conjunto de hosts, con su último estado conocido.
type Registro struct {
	mu     sync.RWMutex
	hosts  []*Host
	estado map[string]Estado
	// cache evita preguntar la memoria de cada host en cada creación: con
	// ráfagas de sandboxes eso serían miles de llamadas por minuto y la cifra
	// apenas se mueve entre una y otra.
	visto map[string]time.Time
}

// TTLEstado es cuánto vale una lectura de capacidad antes de repetirla.
const TTLEstado = 3 * time.Second

// Nuevo construye el registro a partir de endpoints con nombre.
func Nuevo(endpoints map[string]string) *Registro {
	r := &Registro{estado: map[string]Estado{}, visto: map[string]time.Time{}}
	nombres := make([]string, 0, len(endpoints))
	for n := range endpoints {
		nombres = append(nombres, n)
	}
	sort.Strings(nombres)
	for _, n := range nombres {
		r.hosts = append(r.hosts, &Host{Nombre: n, Endpoint: endpoints[n], Cliente: api.NewClient(endpoints[n])})
	}
	return r
}

// Todos devuelve los hosts en orden estable.
func (r *Registro) Todos() []*Host {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*Host(nil), r.hosts...)
}

// Host devuelve uno por nombre.
func (r *Registro) Host(nombre string) (*Host, bool) {
	for _, h := range r.Todos() {
		if h.Nombre == nombre {
			return h, true
		}
	}
	return nil, false
}

// Estados consulta todos los hosts y devuelve lo que contestan.
func (r *Registro) Estados(ctx context.Context) []Estado {
	hosts := r.Todos()
	out := make([]Estado, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h *Host) {
			defer wg.Done()
			out[i] = r.mirar(ctx, h, true)
		}(i, h)
	}
	wg.Wait()
	return out
}

// mirar pregunta a un host por su estado, con caché corta.
func (r *Registro) mirar(ctx context.Context, h *Host, forzar bool) Estado {
	r.mu.RLock()
	visto, hay := r.visto[h.Nombre]
	previo := r.estado[h.Nombre]
	r.mu.RUnlock()
	if !forzar && hay && time.Since(visto) < TTLEstado {
		return previo
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := Estado{Nombre: h.Nombre, Endpoint: h.Endpoint}
	info, err := h.Cliente.Info(ctx)
	if err != nil {
		e.Error = err.Error()
	} else {
		e.Vivo, e.Version, e.Maquinas = true, info.Version, info.Machines
		if ps, err := h.Cliente.ProcStats(ctx); err == nil {
			e.DisponibleMB = ps.AvailableMiB
		}
	}
	r.mu.Lock()
	r.estado[h.Nombre] = e
	r.visto[h.Nombre] = time.Now()
	r.mu.Unlock()
	return e
}

// Candidatos ordena los hosts por hueco libre, de más a menos, dejando fuera los
// que no contestan. sonValidos filtra los que no sirven para este trabajo (por
// ejemplo, los que no tienen la plantilla construida).
func (r *Registro) Candidatos(ctx context.Context, sirve func(*Host) bool) []*Host {
	type par struct {
		h     *Host
		libre int64
	}
	var pares []par
	for _, h := range r.Todos() {
		if sirve != nil && !sirve(h) {
			continue
		}
		e := r.mirar(ctx, h, false)
		if !e.Vivo {
			continue
		}
		pares = append(pares, par{h, e.DisponibleMB})
	}
	sort.SliceStable(pares, func(i, j int) bool { return pares[i].libre > pares[j].libre })
	out := make([]*Host, len(pares))
	for i, p := range pares {
		out[i] = p.h
	}
	return out
}

// Intentar prueba trabajo en los hosts que sirvan, en orden de hueco, y se queda
// con el primero que acepte.
//
// Reintenta cuando el host dice que no cabe (507) o que llegó a su tope de
// máquinas: son las dos negativas que OTRO host puede atender. Un error
// cualquiera no se reintenta, porque repetirlo en otro sitio solo multiplica el
// mismo fallo.
func Intentar[T any](ctx context.Context, r *Registro, sirve func(*Host) bool, trabajo func(context.Context, *Host) (T, error)) (T, *Host, error) {
	var cero T
	cands := r.Candidatos(ctx, sirve)
	if len(cands) == 0 {
		if len(r.Todos()) == 0 {
			return cero, nil, ErrSinHosts
		}
		return cero, nil, fmt.Errorf("%w: no host is reachable and ready", ErrSinSitio)
	}
	var ultimo error
	for _, h := range cands {
		out, err := trabajo(ctx, h)
		if err == nil {
			return out, h, nil
		}
		ultimo = fmt.Errorf("%s: %w", h.Nombre, err)
		if !api.IsInsufficientMemory(err) && !api.IsMachineLimit(err) {
			return cero, h, ultimo
		}
	}
	return cero, nil, fmt.Errorf("%w: %v", ErrSinSitio, ultimo)
}
