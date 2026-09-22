package frontal

// Ciclo de vida de los sandboxes: crear, listar, ver, renovar, borrar y la
// limpieza de los abandonados.
//
// PROPIEDAD. Cada sandbox lleva la etiqueta tenant=<nombre> del token que lo
// creó, y toda ruta /v1/sandboxes/{id} pasa por buscar(), que solo devuelve la
// máquina si esa etiqueta coincide con el tenant que llama. Si no coincide —o
// no existe— la respuesta es un 404 idéntico: confirmar que un id existe pero
// es de otro (un 403) regalaría información sobre lo ajeno.
//
// El daemon resuelve referencias por id, por NOMBRE y por PREFIJO de 4 o más
// caracteres. Por eso el frontal nunca le pasa lo que escribió el cliente:
// busca la máquina en la lista del host, comprueba el id EXACTO y la etiqueta,
// y solo entonces habla con el daemon usando el id completo.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling-sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// maxCuerpo acota el JSON de creación y renovación. Un sandbox se describe en
// unos cientos de bytes; 64 KiB deja sitio a etiquetas y dominios sin que
// nadie pueda hacernos leer sin fin.
const maxCuerpo = 64 << 10

// maxEtiquetas es cuántas etiquetas propias admite una petición. Van al
// meta.json de la máquina en el daemon.
const maxEtiquetas = 32

// Sandbox es lo que ve el cliente. El id es "host/máquina": lleva el host
// porque los ids del daemon solo son únicos dentro de él, y la barra hace que
// nadie pueda confundirlo con un id del daemon ni pasárselo a `kling` tal cual.
type Sandbox struct {
	ID       string    `json:"id"`
	Host     string    `json:"host"`
	Template string    `json:"template,omitempty"`
	Image    string    `json:"image,omitempty"`
	State    api.State `json:"state"`

	VCPUs        int      `json:"vcpus"`
	MemMiB       int      `json:"mem_mib"`
	Egress       string   `json:"egress,omitempty"`
	AllowDomains []string `json:"allow_domains,omitempty"`
	OnTTL        string   `json:"on_ttl"`
	TTLSeconds   int      `json:"ttl_seconds"`

	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt es el reloj del TTL del daemon: con on_ttl=freeze cada exec lo
	// pone a ahora, así que es "la última vez que se usó"; con on_ttl=remove
	// solo lo mueve renew. ExpiresAt es ese reloj más el TTL.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`

	// Labels son las etiquetas del cliente, sin las reservadas.
	Labels map[string]string `json:"labels,omitempty"`
}

// CrearPeticion es el cuerpo de POST /v1/sandboxes.
type CrearPeticion struct {
	Template     string            `json:"template,omitempty"`
	Image        string            `json:"image,omitempty"`
	TTLSeconds   int               `json:"ttl_seconds,omitempty"`
	OnTTL        string            `json:"on_ttl,omitempty"`
	Egress       string            `json:"egress,omitempty"`
	AllowDomains []string          `json:"allow_domains,omitempty"`
	MemMiB       int               `json:"mem_mib,omitempty"`
	VCPUs        int               `json:"vcpus,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// RenovarPeticion es el cuerpo de POST /v1/sandboxes/{id}/renew.
type RenovarPeticion struct {
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// idDe compone el id visible; partirID lo deshace. Un id sin barra no es
// nuestro, y se trata como inexistente.
func idDe(h *hosts.Host, mc *api.Machine) string { return h.Nombre + "/" + mc.ID }

func partirID(id string) (host, ref string, ok bool) {
	i := strings.IndexByte(id, '/')
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// vista convierte la máquina del daemon en lo que se enseña.
func vista(h *hosts.Host, mc *api.Machine) Sandbox {
	sb := Sandbox{
		ID: idDe(h, mc), Host: h.Nombre,
		Template: mc.Labels[LabelTemplate], Image: mc.Image, State: mc.State,
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB, Egress: mc.Egress, AllowDomains: mc.AllowDomains,
		OnTTL: mc.OnTTL, TTLSeconds: mc.TTLSeconds, CreatedAt: mc.CreatedAt,
	}
	if sb.OnTTL == "" {
		sb.OnTTL = api.OnTTLFreeze
	}
	if desde := relojTTL(mc); !desde.IsZero() {
		d := desde
		sb.LastUsedAt = &d
		if mc.TTLSeconds > 0 {
			e := desde.Add(time.Duration(mc.TTLSeconds) * time.Second)
			sb.ExpiresAt = &e
		}
	}
	for k, v := range mc.Labels {
		if k == api.LabelKind || k == LabelTenant || k == LabelTemplate {
			continue
		}
		if sb.Labels == nil {
			sb.Labels = map[string]string{}
		}
		sb.Labels[k] = v
	}
	return sb
}

// relojTTL es desde cuándo cuenta el TTL, con los mismos respaldos que usa el
// daemon para máquinas anteriores a TTLAt.
func relojTTL(mc *api.Machine) time.Time {
	switch {
	case mc.TTLAt != nil:
		return *mc.TTLAt
	case mc.StartedAt != nil:
		return *mc.StartedAt
	}
	return mc.CreatedAt
}

// esSandboxDe dice si la máquina es un sandbox de este tenant.
func esSandboxDe(mc *api.Machine, t *Tenant) bool {
	return mc.Labels[api.LabelKind] == api.KindSandbox && mc.Labels[LabelTenant] == t.Nombre
}

// ---- consultas a los hosts

// enCadaHost ejecuta f contra todos los hosts a la vez y devuelve lo que cada
// uno contestó, en el orden estable del registro. Los que fallan quedan a nil
// y se apuntan en errs; el que llama decide si un host caído es fatal.
func (s *Servidor) enCadaHost(ctx context.Context, f func(context.Context, *hosts.Host) ([]*api.Machine, error)) (map[*hosts.Host][]*api.Machine, []error) {
	todos := s.reg.Todos()
	out := make([]([]*api.Machine), len(todos))
	errs := make([]error, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out[i], errs[i] = f(ctx, h)
			if errs[i] != nil {
				errs[i] = fmt.Errorf("%s: %w", h.Nombre, errs[i])
			}
		}(i, h)
	}
	wg.Wait()
	res := make(map[*hosts.Host][]*api.Machine, len(todos))
	var fallos []error
	for i, h := range todos {
		if errs[i] != nil {
			fallos = append(fallos, errs[i])
			continue
		}
		res[h] = out[i]
	}
	return res, fallos
}

// sandboxesDe lista los sandboxes del tenant en todos los hosts.
func (s *Servidor) sandboxesDe(ctx context.Context, t *Tenant) ([]Sandbox, []error) {
	porHost, errs := s.enCadaHost(ctx, func(ctx context.Context, h *hosts.Host) ([]*api.Machine, error) {
		return h.Cliente.Sandboxes(ctx)
	})
	var out []Sandbox
	for h, ms := range porHost {
		for _, mc := range ms {
			if esSandboxDe(mc, t) {
				out = append(out, vista(h, mc))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs
}

// errNoSandbox es la negativa única para "no existe", "es de otro", "el host
// no existe" y "el id no tiene nuestra forma": distinguirlas es lo que no se
// quiere.
type errNoSandbox struct{ id string }

func (e *errNoSandbox) Error() string { return fmt.Sprintf("no sandbox %q", e.id) }

func noSandbox(id string) error { return &errNoSandbox{id} }

// buscar resuelve un id a su host y su máquina, SOLO si es del tenant.
func (s *Servidor) buscar(ctx context.Context, t *Tenant, id string) (*hosts.Host, *api.Machine, error) {
	nombre, ref, ok := partirID(id)
	if !ok {
		return nil, nil, noSandbox(id)
	}
	h, ok := s.reg.Host(nombre)
	if !ok {
		return nil, nil, noSandbox(id)
	}
	ms, err := h.Cliente.Sandboxes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", h.Nombre, err)
	}
	for _, mc := range ms {
		if mc.ID == ref && esSandboxDe(mc, t) {
			return h, mc, nil
		}
	}
	return nil, nil, noSandbox(id)
}

// ---- rutas

func (s *Servidor) handleListar(w http.ResponseWriter, r *http.Request) {
	t := tenantDe(r)
	out, errs := s.sandboxesDe(r.Context(), t)
	if out == nil {
		out = []Sandbox{}
	}
	// Un host caído no esconde los demás: se devuelve lo que hay y se avisa.
	w.Header().Set("Content-Type", "application/json")
	if len(errs) > 0 {
		w.Header().Set("X-Kindling-Partial", strings.Join(mensajes(errs), "; "))
	}
	writeJSON(w, http.StatusOK, out)
}

func mensajes(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Error()
	}
	return out
}

// handleSandbox despacha /v1/sandboxes/{host}/{ref}[/acción].
func (s *Servidor) handleSandbox(w http.ResponseWriter, r *http.Request) {
	resto := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
	partes := strings.Split(resto, "/")
	if len(partes) < 2 || partes[0] == "" || partes[1] == "" {
		fail(w, http.StatusNotFound, noSandbox(resto))
		return
	}
	id := partes[0] + "/" + partes[1]
	accion := ""
	if len(partes) == 3 {
		accion = partes[2]
	} else if len(partes) > 3 {
		http.NotFound(w, r)
		return
	}
	switch {
	case accion == "" && r.Method == http.MethodGet:
		s.handleVer(w, r, id)
	case accion == "" && r.Method == http.MethodDelete:
		s.handleBorrar(w, r, id)
	case accion == "renew" && r.Method == http.MethodPost:
		s.handleRenovar(w, r, id)
	case accion == "exec" && r.Method == http.MethodPost:
		s.handleExec(w, r, id)
	case accion == "shell" && r.Method == http.MethodPost:
		s.handleShell(w, r, id)
	case accion == "files" && (r.Method == http.MethodGet || r.Method == http.MethodPut || r.Method == http.MethodDelete):
		s.handleFiles(w, r, id)
	case accion == "" || accion == "renew" || accion == "exec" || accion == "shell" || accion == "files":
		fail(w, http.StatusMethodNotAllowed, fmt.Errorf("%s is not allowed on this route", r.Method))
	default:
		http.NotFound(w, r)
	}
}

func (s *Servidor) handleVer(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	writeJSON(w, http.StatusOK, vista(h, mc))
}

func (s *Servidor) handleBorrar(w http.ResponseWriter, r *http.Request, id string) {
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	if err := h.Cliente.RemoveSandbox(r.Context(), mc.ID); err != nil {
		fail(w, http.StatusBadGateway, fmt.Errorf("%s: %w", h.Nombre, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Servidor) handleRenovar(w http.ResponseWriter, r *http.Request, id string) {
	var pet RenovarPeticion
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ttl := pet.TTLSeconds
	if ttl == 0 {
		ttl = s.ttl
	}
	if ttl < 1 || ttl > api.SandboxMaxTTL {
		fail(w, http.StatusBadRequest, fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL))
		return
	}
	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	out, err := h.Cliente.RenewSandbox(r.Context(), mc.ID, ttl)
	if err != nil {
		fail(w, http.StatusBadGateway, fmt.Errorf("%s: %w", h.Nombre, err))
		return
	}
	writeJSON(w, http.StatusOK, vista(h, out))
}

// codigoBuscar: un host que no contesta es un 502, no un 404, porque el
// sandbox puede existir y el cliente no debe borrarlo de su lista por eso.
func codigoBuscar(err error) int {
	var ns *errNoSandbox
	if errors.As(err, &ns) {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

// leerJSON decodifica el cuerpo con tope. Un cuerpo vacío vale (es "todo por
// defecto"); uno que no sea JSON o pase del tope, no.
func leerJSON(r *http.Request, v any) error {
	if r.ContentLength > maxCuerpo {
		return fmt.Errorf("body is %d bytes; the limit is %d", r.ContentLength, maxCuerpo)
	}
	b, err := api.LeerCuerpo(r.Body, maxCuerpo)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	if dec.More() {
		return errors.New("invalid body: trailing data")
	}
	return nil
}

// ---- creación

func (s *Servidor) handleCrear(w http.ResponseWriter, r *http.Request) {
	t := tenantDe(r)
	var pet CrearPeticion
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.normalizar(&pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	// Cuota. Se cuenta lo vivo en los daemons más lo que este proceso tiene en
	// camino, y se reserva el hueco ANTES de crear para que dos peticiones a la
	// vez no lo compartan.
	if err := s.reservar(r.Context(), t, pet.Template); err != nil {
		fail(w, http.StatusTooManyRequests, err)
		return
	}
	defer s.liberar(t, pet.Template)

	h, mc, err := s.crear(r.Context(), t, pet)
	if err != nil {
		fail(w, codigoCrear(err), err)
		return
	}
	sb := vista(h, mc)
	w.Header().Set("Location", "/v1/sandboxes/"+sb.ID)
	writeJSON(w, http.StatusCreated, sb)
}

// normalizar aplica los valores por defecto y rechaza lo que no vale. Por
// defecto sin red y durmiendo al vencer: lo que corre dentro lo escribió un
// agente, y dormir en vez de destruir es lo que permite retomar el trabajo en
// milisegundos sin pagar RAM entre tanto.
func (s *Servidor) normalizar(p *CrearPeticion) error {
	if p.Template != "" && p.Image != "" {
		return errors.New("pass template or image, not both")
	}
	if p.Template != "" && !nombreValido.MatchString(p.Template) {
		return fmt.Errorf("template name %q is not valid", p.Template)
	}
	if p.Template == "" && p.Image == "" {
		if s.imagen == "" {
			return errors.New("missing template (or image)")
		}
		p.Image = s.imagen
	}
	if p.TTLSeconds == 0 {
		p.TTLSeconds = s.ttl
	}
	if p.TTLSeconds < 1 || p.TTLSeconds > api.SandboxMaxTTL {
		return fmt.Errorf("ttl_seconds must be between 1 and %d", api.SandboxMaxTTL)
	}
	switch p.OnTTL {
	case "":
		p.OnTTL = api.OnTTLFreeze
	case api.OnTTLFreeze, api.OnTTLRemove:
	default:
		return fmt.Errorf("invalid on_ttl %q: use %q or %q", p.OnTTL, api.OnTTLFreeze, api.OnTTLRemove)
	}
	switch p.Egress {
	case "":
		p.Egress = "none"
	case "none", "internet", "allowlist":
	default:
		return fmt.Errorf("invalid egress %q: use none, internet or allowlist", p.Egress)
	}
	if p.Egress != "allowlist" && len(p.AllowDomains) > 0 {
		return errors.New("allow_domains only makes sense with egress \"allowlist\"")
	}
	if p.Egress == "allowlist" && len(p.AllowDomains) == 0 {
		return errors.New("egress \"allowlist\" needs allow_domains")
	}
	if p.MemMiB < 0 || p.VCPUs < 0 {
		return errors.New("mem_mib and vcpus can't be negative")
	}
	if len(p.Labels) > maxEtiquetas {
		return fmt.Errorf("too many labels (%d); the limit is %d", len(p.Labels), maxEtiquetas)
	}
	for k := range p.Labels {
		// Las reservadas las pone el frontal: dejar que el cliente las mande
		// sería dejarle firmar como otro tenant.
		if k == api.LabelKind || k == LabelTenant || k == LabelTemplate {
			return fmt.Errorf("label %q is reserved", k)
		}
		if !api.KeyPattern.MatchString(k) {
			return fmt.Errorf("label %q is not valid (lowercase letters, digits, '.', '_', '-')", k)
		}
	}
	return nil
}

// clave de contabilidad por plantilla.
func clavePlantilla(t *Tenant, plantilla string) string { return t.Nombre + "\x00" + plantilla }

// reservar comprueba la cuota y apunta la creación en vuelo.
func (s *Servidor) reservar(ctx context.Context, t *Tenant, plantilla string) error {
	if t.MaxSandboxes == 0 && (t.MaxPorPlantilla == 0 || plantilla == "") {
		s.mu.Lock()
		s.enVuelo[t.Nombre]++
		s.enVuelo[clavePlantilla(t, plantilla)]++
		s.mu.Unlock()
		return nil
	}
	vivos, _ := s.sandboxesDe(ctx, t)
	total, dePlantilla := len(vivos), 0
	for _, sb := range vivos {
		if plantilla != "" && sb.Template == plantilla {
			dePlantilla++
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.MaxSandboxes > 0 && total+s.enVuelo[t.Nombre] >= t.MaxSandboxes {
		return fmt.Errorf("tenant %s already has %d sandboxes; the limit is %d", t.Nombre, total, t.MaxSandboxes)
	}
	if plantilla != "" && t.MaxPorPlantilla > 0 && dePlantilla+s.enVuelo[clavePlantilla(t, plantilla)] >= t.MaxPorPlantilla {
		return fmt.Errorf("tenant %s already has %d sandboxes of template %s; the limit is %d", t.Nombre, dePlantilla, plantilla, t.MaxPorPlantilla)
	}
	s.enVuelo[t.Nombre]++
	s.enVuelo[clavePlantilla(t, plantilla)]++
	return nil
}

func (s *Servidor) liberar(t *Tenant, plantilla string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enVuelo[t.Nombre]--
	s.enVuelo[clavePlantilla(t, plantilla)]--
}

// errSinPlantilla es que la plantilla no está construida en ningún host.
type errSinPlantilla struct{ nombre string }

func (e *errSinPlantilla) Error() string {
	return fmt.Sprintf("template %q is not built on any reachable host", e.nombre)
}

func codigoCrear(err error) int {
	var sp *errSinPlantilla
	if errors.As(err, &sp) {
		return http.StatusNotFound
	}
	return codigoDeHost(err)
}

// crear decide dónde y cómo nace el sandbox.
//
// Con plantilla, el sandbox nace del snapshot de la plantilla y solo en hosts
// que lo tengan (los snapshots no viajan entre hosts). Si la petición no pide
// nada distinto de lo que una precalentada ya es, se reclama una; si no, se
// restaura una nueva del snapshot. Todo pasa por hosts.Intentar para que "no
// cabe" en un host se reintente en el siguiente.
func (s *Servidor) crear(ctx context.Context, t *Tenant, p CrearPeticion) (*hosts.Host, *api.Machine, error) {
	etiquetas := map[string]string{LabelTenant: t.Nombre}
	for k, v := range p.Labels {
		etiquetas[k] = v
	}
	req := api.SandboxRequest{
		VCPUs: p.VCPUs, MemMiB: p.MemMiB, TTLSeconds: p.TTLSeconds, OnTTL: p.OnTTL,
		Egress: p.Egress, AllowDomains: p.AllowDomains, Labels: etiquetas,
	}

	var sirve func(*hosts.Host) bool
	if p.Template == "" {
		req.Image = p.Image
		tienen := s.hostsCon(ctx, func(ctx context.Context, h *hosts.Host) (bool, error) {
			imgs, err := h.Cliente.Images(ctx)
			if err != nil {
				return false, err
			}
			for _, im := range imgs {
				if im.Name == p.Image {
					return true, nil
				}
			}
			return false, nil
		})
		if len(tienen) == 0 {
			return nil, nil, &api.StatusError{Code: http.StatusNotFound,
				Message: fmt.Sprintf("image %q is not on any reachable host", p.Image)}
		}
		sirve = func(h *hosts.Host) bool { return tienen[h.Nombre] }
	} else {
		snap := plantilla.SnapshotDe(p.Template)
		req.From = snap
		etiquetas[LabelTemplate] = p.Template
		tienen := s.hostsCon(ctx, func(ctx context.Context, h *hosts.Host) (bool, error) {
			snaps, err := h.Cliente.Snapshots(ctx)
			if err != nil {
				return false, err
			}
			for _, sn := range snaps {
				if sn.Name == snap {
					return true, nil
				}
			}
			return false, nil
		})
		if len(tienen) == 0 {
			return nil, nil, &errSinPlantilla{p.Template}
		}
		sirve = func(h *hosts.Host) bool { return tienen[h.Nombre] }

		if puedeReclamar(p) {
			if h, mc := s.reclamarPrecalentada(ctx, t, p, etiquetas, sirve); mc != nil {
				return h, mc, nil
			}
		}
	}

	mc, h, err := hosts.Intentar(ctx, s.reg, sirve, func(ctx context.Context, h *hosts.Host) (*api.Machine, error) {
		return h.Cliente.CreateSandbox(ctx, req)
	})
	if err != nil {
		return nil, nil, err
	}
	return h, mc, nil
}

// hostsCon devuelve los nombres de los hosts para los que f dice que sí.
func (s *Servidor) hostsCon(ctx context.Context, f func(context.Context, *hosts.Host) (bool, error)) map[string]bool {
	todos := s.reg.Todos()
	res := make([]bool, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			ok, err := f(ctx, h)
			if err != nil {
				s.log.Printf("frontal: %s: %v", h.Nombre, err)
				return
			}
			res[i] = ok
		}(i, h)
	}
	wg.Wait()
	out := map[string]bool{}
	for i, h := range todos {
		if res[i] {
			out[h.Nombre] = true
		}
	}
	return out
}

// puedeReclamar dice si la petición se conforma con lo que una precalentada
// ya es. Red, on_ttl, memoria y vCPUs se fijan al ARRANCAR y una máquina viva
// no los cambia; lo único que se puede ajustar después es el TTL (renew) y
// las etiquetas.
func puedeReclamar(p CrearPeticion) bool {
	return p.Egress == "none" && p.OnTTL == api.OnTTLFreeze && p.MemMiB == 0 && p.VCPUs == 0
}

// reclamarPrecalentada busca, en los hosts que sirven y por orden de hueco,
// una instancia de la plantilla sin dueño y la hace del tenant.
//
// La reclamación es etiquetar (tenant=...) y renovar (el TTL pedido, contando
// desde ahora). Va bajo s.mu porque entre listar y etiquetar otra petición de
// este mismo proceso podría elegir la misma máquina. Dos FRONTALES distintos
// sobre el mismo daemon sí podrían chocar: SetLabels es un merge y el último
// gana; por eso tras etiquetar se relee y, si el dueño no somos nosotros, se
// deja estar y se crea una nueva.
func (s *Servidor) reclamarPrecalentada(ctx context.Context, t *Tenant, p CrearPeticion, etiquetas map[string]string, sirve func(*hosts.Host) bool) (*hosts.Host, *api.Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.reg.Candidatos(ctx, sirve) {
		ms, err := h.Cliente.Sandboxes(ctx)
		if err != nil {
			continue
		}
		for _, mc := range ms {
			if !esPrecalentadaDe(mc, p.Template) {
				continue
			}
			if err := h.Cliente.SetLabels(ctx, mc.ID, etiquetas); err != nil {
				s.log.Printf("frontal: claiming %s/%s: %v", h.Nombre, mc.ID, err)
				continue
			}
			out, err := h.Cliente.RenewSandbox(ctx, mc.ID, p.TTLSeconds)
			if err != nil || !esSandboxDe(out, t) {
				continue
			}
			return h, out
		}
	}
	return nil, nil
}

// esPrecalentadaDe es el contrato con el pool del paquete plantilla: una
// máquina kind=sandbox, de esta plantilla, sin tenant, viva o dormida, sin red
// y que duerme al vencer.
func esPrecalentadaDe(mc *api.Machine, template string) bool {
	if mc.Labels[api.LabelKind] != api.KindSandbox || mc.Labels[LabelTemplate] != template || mc.Labels[LabelTenant] != "" {
		return false
	}
	if mc.State != api.StateRunning && mc.State != api.StateWarm {
		return false
	}
	if mc.Egress != "" && mc.Egress != "none" {
		return false
	}
	return mc.OnTTL != api.OnTTLRemove
}

// ---- limpieza

// Limpiar hace una vuelta: borra los sandboxes con dueño que llevan más de
// Abandono sin usarse. Es el segundo plazo: el TTL del daemon los duerme, y
// dormidos no cuestan RAM pero sí disco y una entrada en el tope de máquinas;
// esto es lo que evita que se acumulen para siempre.
//
// Solo toca sandboxes con tenant: las precalentadas son del pool de
// plantilla, y las máquinas sin kind=sandbox ni se listan.
func (s *Servidor) Limpiar(ctx context.Context) {
	porHost, errs := s.enCadaHost(ctx, func(ctx context.Context, h *hosts.Host) ([]*api.Machine, error) {
		return h.Cliente.Sandboxes(ctx)
	})
	for _, err := range errs {
		s.log.Printf("frontal: sweep: %v", err)
	}
	for h, ms := range porHost {
		for _, mc := range ms {
			if mc.Labels[api.LabelKind] != api.KindSandbox || mc.Labels[LabelTenant] == "" {
				continue
			}
			desde := relojTTL(mc)
			if desde.IsZero() || time.Since(desde) < s.abandono {
				continue
			}
			if err := h.Cliente.RemoveSandbox(ctx, mc.ID); err != nil {
				s.log.Printf("frontal: sweep: removing %s/%s: %v", h.Nombre, mc.ID, err)
				continue
			}
			s.log.Printf("frontal: removed %s/%s (tenant %s): unused for %s", h.Nombre, mc.ID, mc.Labels[LabelTenant], time.Since(desde).Round(time.Minute))
		}
	}
}

// Vigilar repite Limpiar cada CadaLimpieza hasta que el contexto se cancele.
func (s *Servidor) Vigilar(ctx context.Context) {
	go func() {
		tick := time.NewTicker(s.cada)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.Limpiar(ctx)
			}
		}
	}()
}
