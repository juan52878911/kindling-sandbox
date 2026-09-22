package main

// Configuración de la extensión.
//
// Vive donde la del resto de kindling (config.json, sección extensions.sandbox),
// para que se toque con `kling config set sandbox.<clave> <valor>` y salga en
// `kling config show` como todo lo demás.

import (
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/config"
)

const ext = "sandbox"

type ajustes struct {
	Listen string
	URL    string
	Token  string
	Hosts  map[string]string
	Idle   time.Duration
	Reap   time.Duration
}

// leerAjustes saca la configuración con sus valores por defecto.
func leerAjustes() (ajustes, error) {
	cfg, err := config.Load()
	if err != nil {
		return ajustes{}, err
	}
	a := ajustes{
		Listen: cfg.ExtensionValue(ext, "listen", "string"),
		URL:    cfg.ExtensionValue(ext, "url", "string"),
		Token:  cfg.ExtensionValue(ext, "token", "secret"),
		Idle:   5 * time.Minute,
		Reap:   24 * time.Hour,
	}
	if a.Listen == "" {
		a.Listen = ":8090"
	}
	if a.URL == "" {
		a.URL = "http://127.0.0.1:8090"
	}
	if a.Hosts, err = parseHosts(cfg, cfg.ExtensionValue(ext, "hosts", "string")); err != nil {
		return a, err
	}
	if v := cfg.ExtensionValue(ext, "idle", "string"); v != "" {
		if a.Idle, err = time.ParseDuration(v); err != nil {
			return a, fmt.Errorf("sandbox.idle: %w", err)
		}
	}
	if v := cfg.ExtensionValue(ext, "reap", "string"); v != "" {
		if a.Reap, err = time.ParseDuration(v); err != nil {
			return a, fmt.Errorf("sandbox.reap: %w", err)
		}
	}
	return a, nil
}

// parseHosts lee "lab=ssh://juan@lab,otro=unix:///run/kling.sock".
//
// Sin hosts configurados se usa el daemon local, que es lo que espera quien
// prueba esto en su máquina: un frontal que no sirve para nada hasta editar un
// fichero sería un mal primer contacto.
func parseHosts(cfg *config.Config, v string) (map[string]string, error) {
	out := map[string]string{}
	for _, trozo := range strings.Split(v, ",") {
		trozo = strings.TrimSpace(trozo)
		if trozo == "" {
			continue
		}
		nombre, endpoint, ok := strings.Cut(trozo, "=")
		if !ok || nombre == "" || endpoint == "" {
			return nil, fmt.Errorf("sandbox.hosts: %q is not name=endpoint", trozo)
		}
		out[strings.TrimSpace(nombre)] = strings.TrimSpace(endpoint)
	}
	if len(out) == 0 {
		out["local"] = cfg.Host("")
	}
	return out, nil
}
