package pool

import (
	"encoding/json"
	"testing"

	"github.com/juan52878911/kindling-sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// El fondo lee lo que quiere cada plantilla de la anotación del snapshot, que es
// donde la dejó quien la construyó: así no hay una segunda lista de plantillas
// que mantener en sincronía.
func TestDeseoDe(t *testing.T) {
	receta, _ := json.Marshal(map[string]any{
		"template": plantilla.Plantilla{Nombre: "ts", Imagen: "toolchain", Pool: 3},
	})

	casos := []struct {
		nombre string
		snap   *api.Snapshot
		quiere int
		cual   string
	}{
		{"con receta y pool", &api.Snapshot{Name: "sbx-ts",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}, 3, "ts"},
		{"snapshot ajeno", &api.Snapshot{Name: "notas",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}, 0, ""},
		{"sin anotación", &api.Snapshot{Name: "sbx-otra"}, 0, ""},
		{"anotación ilegible", &api.Snapshot{Name: "sbx-rota",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: json.RawMessage("{")}}, 0, ""},
	}
	for _, c := range casos {
		cual, quiere := DeseoDe(c.snap)
		if quiere != c.quiere || (c.quiere > 0 && cual != c.cual) {
			t.Errorf("%s: (%q, %d), quería (%q, %d)", c.nombre, cual, quiere, c.cual, c.quiere)
		}
	}
}

// Una plantilla sin pool no fabrica nada: el fondo es opt-in, porque cuesta RAM
// en reposo.
func TestSinPoolNoSeFabricaNada(t *testing.T) {
	receta, _ := json.Marshal(map[string]any{
		"template": plantilla.Plantilla{Nombre: "sin", Imagen: "toolchain"},
	})
	if _, quiere := DeseoDe(&api.Snapshot{Name: "sbx-sin",
		Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}); quiere != 0 {
		t.Fatalf("quiere %d, want 0", quiere)
	}
}
