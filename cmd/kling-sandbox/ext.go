package main

import (
	"io"
	"strings"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// extension declara lo que kindling-sandbox añade a `kling`.
//
// El comando es `sbx` y no `sandbox` a propósito: `kling sandbox` es del núcleo
// —crea una microVM de usar y tirar contra el daemon local— y los comandos del
// núcleo ganan siempre. Esto es otra cosa: habla con un FRONTAL, que a su vez
// reparte entre varios daemons, con plantillas, inquilinos y cuotas.
func extension() *plugin.Builtin {
	return &plugin.Builtin{
		Manifest: plugin.Manifest{
			ManifestVersion: plugin.ManifestVersion,
			Name:            "sandbox",
			Version:         strings.TrimPrefix(Version, "v"),
			MinKling:        "0.7.0",
			Summary:         "sandboxes for code agents: templates, tenants and several hosts",
			Commands: []plugin.Command{{
				Name:    "sbx",
				Group:   "SANDBOX FLEET",
				Summary: "sandboxes served by a gateway (templates, tenants, several hosts)",
				Usage: "  sbx gateway [-listen :8090]                      serves the sandbox API\n" +
					"  sbx template apply -f tpl.json | ls | rebuild N  recipes that become golden snapshots\n" +
					"  sbx new -template T [-ttl 10m] | ls | rm ID      sandboxes through the gateway\n" +
					"  sbx exec ID -- cmd... | shell ID | cp ...        work inside one\n" +
					"  sbx hosts                                        which daemons are behind the gateway",
				Subcommands: []string{"gateway", "template", "new", "ls", "rm", "renew", "exec", "shell", "cp", "hosts"},
			}},
			Config: []plugin.ConfigKey{
				{Key: "listen", Type: "string", Help: "address the gateway listens on (default :8090)"},
				{Key: "url", Type: "string", Help: "gateway the client commands talk to (default http://127.0.0.1:8090)"},
				{Key: "token", Type: "secret", Help: "token the client commands send"},
				{Key: "hosts", Type: "string", Help: "kindling daemons behind the gateway: name=endpoint, comma separated"},
				{Key: "idle", Type: "string", Help: "how long a sandbox may sit unused before it is frozen (default 5m)"},
				{Key: "reap", Type: "string", Help: "how long a frozen sandbox is kept before it is destroyed (default 24h)"},
			},
			Units: []string{"kling-sandbox.service"},
			Hooks: []string{plugin.HookStatus},
		},
		Commands: map[string]func([]string) error{
			"sbx": cmdSbx,
		},
		Hooks: map[string]func([]string, io.Writer) error{
			plugin.HookStatus: hookStatus,
		},
	}
}
