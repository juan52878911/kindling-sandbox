// Comando kindling-operator: un operador fino de Kubernetes para sandboxes de
// kindling-sandbox. Ver internal/operator para el bucle y docs/kubernetes.md
// para qué es y qué NO es.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/juan52878911/kindling-sandbox/internal/operator"
)

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	// -kube-url y -kube-token son SOLO para hablar con el API de Kubernetes
	// desde fuera del clúster (pruebas, un `kubectl proxy` local). Dentro del
	// clúster no hacen falta: el kubeconfig lo pone el propio Kubernetes
	// (KUBERNETES_SERVICE_HOST/PORT, el token y el ca.crt de la cuenta de
	// servicio), y pasar ese token por flag lo dejaría en `ps` y en argv.
	kubeURL := flag.String("kube-url", "", "Kubernetes API server URL (only outside a cluster; e.g. behind `kubectl proxy`)")
	kubeToken := flag.String("kube-token", "", "Kubernetes bearer token (only outside a cluster)")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	logger.Printf("kindling-operator %s", Version)

	kubeCfg, err := kubeConfig(*kubeURL, *kubeToken)
	if err != nil {
		logger.Fatalf("kindling-operator: %v", err)
	}

	// La URL y el token del frontal SIEMPRE van por entorno, nunca por flag:
	// son el secreto del tenant y el destino del plano de datos, no
	// parámetros de arranque de este proceso.
	frontalURL := os.Getenv("KLING_SANDBOX_URL")
	frontalToken := os.Getenv("KLING_SANDBOX_TOKEN")
	if frontalURL == "" || frontalToken == "" {
		logger.Fatal("kindling-operator: KLING_SANDBOX_URL and KLING_SANDBOX_TOKEN must be set")
	}

	kube := operator.NewKubeClient(kubeCfg)
	frontal := operator.NewFrontalClient(frontalURL, frontalToken)
	ctrl := operator.NewController(kube, frontal, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Printf("kindling-operator: watching %s/%s %s against %s", operator.Group, operator.Version, operator.Resource, frontalURL)
	if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Fatalf("kindling-operator: %v", err)
	}
}

// kubeConfig decide entre dentro y fuera del clúster. Las flags mandan si se
// dan las dos; si no, se intenta la configuración de dentro del clúster.
func kubeConfig(url, token string) (*operator.KubeConfig, error) {
	if url != "" {
		return &operator.KubeConfig{BaseURL: url, Token: token}, nil
	}
	cfg, err := operator.InClusterKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("no -kube-url given and not running in a cluster: %w", err)
	}
	return cfg, nil
}
