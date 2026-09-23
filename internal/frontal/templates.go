package frontal

// GET /v1/templates: qué plantillas hay listas para pedir y en qué hosts,
// para que un cliente (u operador) sepa qué -template puede pedir sin tener
// acceso directo a los daemons de kindling.
//
// De solo lectura y sin nada propio: lee los snapshots "sbx-*" de cada host y
// la receta de su anotación, la misma que escribe plantilla.Construir. Un
// snapshot sin esa anotación (de antes de que existiera, o corrupto) sigue
// apareciendo, solo que sin receta ni fecha.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling-sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// TemplateHost es lo que hay de una plantilla en un host concreto.
type TemplateHost struct {
	Host      string    `json:"host"`
	Snapshot  string    `json:"snapshot"`
	BuiltAt   time.Time `json:"built_at,omitempty"`
	Recipe    string    `json:"recipe_sha256,omitempty"`
	Instances int       `json:"instances,omitempty"`
}

// TemplateInfo es una plantilla vista desde el frontal: su nombre y en qué
// hosts está construida.
type TemplateInfo struct {
	Name  string         `json:"name"`
	Hosts []TemplateHost `json:"hosts"`
}

func (s *Servidor) handleTemplates(w http.ResponseWriter, r *http.Request) {
	todos := s.reg.Todos()
	porHost := make([][]*api.Snapshot, len(todos))
	var wg sync.WaitGroup
	for i, h := range todos {
		wg.Add(1)
		go func(i int, h *hosts.Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			snaps, err := h.Cliente.Snapshots(ctx)
			if err != nil {
				s.log.Printf("frontal: templates: %s: %v", h.Nombre, err)
				return
			}
			porHost[i] = snaps
		}(i, h)
	}
	wg.Wait()

	porNombre := map[string]*TemplateInfo{}
	var orden []string
	for i, h := range todos {
		for _, sn := range porHost[i] {
			if !strings.HasPrefix(sn.Name, "sbx-") {
				continue
			}
			nombre := strings.TrimPrefix(sn.Name, "sbx-")
			ti, ok := porNombre[nombre]
			if !ok {
				ti = &TemplateInfo{Name: nombre}
				porNombre[nombre] = ti
				orden = append(orden, nombre)
			}
			th := TemplateHost{Host: h.Nombre, Snapshot: sn.Name, Instances: sn.Instances}
			var rec plantilla.Receta
			if ok, err := sn.Annotation(plantilla.AnotacionReceta, &rec); ok && err == nil {
				th.BuiltAt, th.Recipe = rec.Hecho, rec.Hash
			}
			ti.Hosts = append(ti.Hosts, th)
		}
	}

	sort.Strings(orden)
	out := make([]TemplateInfo, 0, len(orden))
	for _, n := range orden {
		ti := porNombre[n]
		sort.Slice(ti.Hosts, func(i, j int) bool { return ti.Hosts[i].Host < ti.Hosts[j].Host })
		out = append(out, *ti)
	}
	writeJSON(w, http.StatusOK, out)
}
