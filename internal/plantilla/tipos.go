// Package plantilla define las recetas de sandbox y cómo se convierten en un
// snapshot dorado de kindling.
//
// Una plantilla es una RECETA, no una imagen: dice de qué imagen partir, qué
// instalar y con qué red hacerlo, y de ahí sale un snapshot del que nacen los
// sandboxes en milisegundos. Tiene que ser una receta y no un artefacto porque
// los snapshots de kindling están atados a su host —un reinicio los invalida—,
// así que hay que poder reconstruirlos en cualquier momento y en cualquier host
// sin que nadie recuerde a mano qué se hizo.
package plantilla

import "time"

// Plantilla es la receta.
type Plantilla struct {
	Nombre string `json:"name"`
	// Imagen de kindling de la que se parte. Tiene que llevar agente de
	// invitado: la construye `kling images toolchain` o el constructor "base".
	Imagen string `json:"image"`
	VCPUs  int    `json:"vcpus,omitempty"`
	MemMiB int    `json:"mem_mib,omitempty"`
	CPUPct int    `json:"cpu_pct,omitempty"`

	// EgressBuild es la red durante la preparación: casi siempre "internet",
	// porque preparar es instalar. No es la red de los sandboxes que salgan de
	// aquí, que se decide al crearlos y por defecto es ninguna.
	EgressBuild string   `json:"build_egress,omitempty"`
	AllowBuild  []string `json:"build_allow_domains,omitempty"`

	// Pasos son los comandos de preparación, en orden.
	Pasos []Paso `json:"steps,omitempty"`

	// Volumes se montan durante la preparación y quedan grabados en el snapshot.
	Volumes []Volumen `json:"volumes,omitempty"`

	// Pool son las instancias precalentadas que se mantienen listas para esta
	// plantilla. 0 = ninguna; crear entonces cuesta el thaw (~300 ms).
	Pool int `json:"pool,omitempty"`
}

// Paso es un comando de preparación.
type Paso struct {
	Cmd     []string `json:"cmd"`
	Dir     string   `json:"dir,omitempty"`
	Env     []string `json:"env,omitempty"`
	Timeout int      `json:"timeout_seconds,omitempty"`
}

// Volumen es un volumen montado en la preparación.
type Volumen struct {
	Nombre   string `json:"name"`
	Mount    string `json:"mount,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Estado es lo que se sabe de una plantilla en un host concreto.
type Estado struct {
	Host      string    `json:"host"`
	Snapshot  string    `json:"snapshot,omitempty"`
	Hecho     time.Time `json:"built_at,omitempty"`
	Receta    string    `json:"recipe_sha256,omitempty"`
	Instancia int       `json:"instances,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// SnapshotDe es cómo se llama el snapshot de una plantilla. El prefijo evita
// pisar los snapshots de servicios MCP o los que alguien tenga a mano.
func SnapshotDe(nombre string) string { return "sbx-" + nombre }

// AnotacionReceta es la anotación del snapshot donde se guarda la receta con la
// que se construyó. Sirve para saber si hay que reconstruir y para poder
// hacerlo sin que nadie recuerde nada.
const AnotacionReceta = "sandbox.template"
