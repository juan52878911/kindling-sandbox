// kling-sandbox es la extensión de kling que sirve sandboxes de agentes de
// código a través de un frontal: plantillas, inquilinos con cuota y varios hosts
// de kindling detrás.
//
// No se teclea directamente: `kling sbx gateway`, `kling sbx new`… los encuentra
// kling y le pasa el control (ver docs/extensions.md de kindling). También
// funciona a mano, con la misma forma: `kling-sandbox sbx ls`.
package main

import "github.com/juan52878911/kindling/pkg/plugin"

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	ext := extension()
	plugin.Main(ext.Manifest, ext.Commands, ext.Hooks)
}
