package operator

// Cliente mínimo del API de Kubernetes: lo justo que el operador necesita
// (list, watch, y parchear el objeto y su subrecurso status), hablado a mano
// por HTTP. No es un cliente general: no sabe de otros recursos ni de otras
// versiones, y así se queda sin arrastrar client-go.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	saTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCACert    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// KubeConfig es lo que hace falta para hablar con el API de Kubernetes.
type KubeConfig struct {
	// BaseURL sin barra final, p.ej. "https://10.0.0.1:443" o
	// "http://127.0.0.1:8001" (un `kubectl proxy` para pruebas fuera del
	// clúster).
	BaseURL string
	// Token del bearer. Puede ir vacío contra un kubectl proxy sin auth.
	Token string
	// TLSClientConfig es opcional: solo hace falta para el CA del clúster.
	TLSClientConfig *tls.Config
}

// InClusterKubeConfig lee la configuración estándar que Kubernetes inyecta en
// todo Pod: host y puerto por entorno, token y CA por fichero. Devuelve error
// si alguna de las dos cosas no está: sin eso no hay manera honesta de hablar
// con el API server, y un token de env o un flag no valen aquí (los secretos
// no viajan por flags ni por argv).
func InClusterKubeConfig() (*KubeConfig, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT are not set")
	}
	tok, err := os.ReadFile(saTokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading service account token: %w", err)
	}
	ca, err := os.ReadFile(saCACert)
	if err != nil {
		return nil, fmt.Errorf("reading service account ca.crt: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("ca.crt does not contain a valid PEM certificate")
	}
	base := "https://" + host
	if strings.Contains(host, ":") { // IPv6
		base = "https://[" + host + "]"
	}
	base += ":" + port
	return &KubeConfig{
		BaseURL:         base,
		Token:           strings.TrimSpace(string(tok)),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}, nil
}

// KubeClient habla con el API de Kubernetes para el recurso Sandbox.
type KubeClient struct {
	base  string
	token string
	http  *http.Client
	// watchHTTP es igual que http pero sin Timeout: un watch es una petición
	// larga a propósito, y un cliente con Timeout la cortaría a mitad.
	watchHTTP *http.Client
}

// NewKubeClient construye el cliente a partir de una configuración ya resuelta
// (dentro del clúster o por flags, ver InClusterKubeConfig y las flags
// -kube-url/-kube-token de main).
func NewKubeClient(c *KubeConfig) *KubeClient {
	transport := &http.Transport{TLSClientConfig: c.TLSClientConfig}
	return &KubeClient{
		base:      strings.TrimSuffix(c.BaseURL, "/"),
		token:     c.Token,
		http:      &http.Client{Transport: transport, Timeout: 30 * time.Second},
		watchHTTP: &http.Client{Transport: transport},
	}
}

func (k *KubeClient) sandboxesURL() string {
	return fmt.Sprintf("%s/apis/%s/%s/%s", k.base, Group, Version, Resource)
}

func (k *KubeClient) itemURL(namespace, name string) string {
	return fmt.Sprintf("%s/apis/%s/%s/namespaces/%s/%s/%s", k.base, Group, Version, namespace, Resource, name)
}

func (k *KubeClient) newRequest(ctx context.Context, method, url, contentType string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// apiError envuelve el Status que devuelve el API de Kubernetes en un error de
// Go, con el código HTTP a mano para que el que llama decida (un 404 en un
// GET no es lo mismo que un 409 en un PATCH).
type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("kubernetes api: %s (%d)", e.Message, e.Code) }

// errGone señala un 410: el resourceVersion desde el que se pidió el watch ya
// no está en el histórico del API server (compactado). No es un fallo de red,
// es la señal de "vuelve a listar desde cero".
var errGone = errors.New("resourceVersion is too old (410 Gone): a relist is needed")

func readAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var st status
	msg := string(b)
	if json.Unmarshal(b, &st) == nil && st.Message != "" {
		msg = st.Message
	}
	return &apiError{Code: resp.StatusCode, Message: msg}
}

// List trae todos los Sandbox de todos los namespaces (list cluster-wide: el
// ClusterRole del operador solo puede darse a nivel de clúster porque vigila
// namespaces que no conoce de antemano) y el resourceVersion desde el que
// seguir con Watch.
func (k *KubeClient) List(ctx context.Context) (*SandboxList, error) {
	req, err := k.newRequest(ctx, http.MethodGet, k.sandboxesURL(), "", nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	var list SandboxList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding sandbox list: %w", err)
	}
	return &list, nil
}

// watchHandler procesa un evento ya decodificado. Devuelve el resourceVersion
// más reciente visto, para que la reconexión retome desde ahí.
type watchHandler func(kind string, sb *Sandbox)

// Watch abre un watch desde resourceVersion y llama a handle por cada evento
// ADDED/MODIFIED/DELETED/BOOKMARK hasta que el contexto se cancele, la
// conexión se corte (error de red: quien llama decide si reconectar) o el
// server mande un 410 (errGone: hace falta relistar, no reconectar con el
// mismo resourceVersion).
func (k *KubeClient) Watch(ctx context.Context, resourceVersion string, handle watchHandler) error {
	url := fmt.Sprintf("%s?watch=1&allowWatchBookmarks=true&resourceVersion=%s", k.sandboxesURL(), resourceVersion)
	req, err := k.newRequest(ctx, http.MethodGet, url, "", nil)
	if err != nil {
		return err
	}
	resp, err := k.watchHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return errGone
	}
	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev watchEvent
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil // el server cerró limpio: quien llama reconecta.
			}
			return err
		}
		if ev.Type == "ERROR" {
			var st status
			if json.Unmarshal(ev.Object, &st) == nil && st.Code == http.StatusGone {
				return errGone
			}
			return fmt.Errorf("watch error event: %s", string(ev.Object))
		}
		var sb Sandbox
		if err := json.Unmarshal(ev.Object, &sb); err != nil {
			return fmt.Errorf("decoding watch object: %w", err)
		}
		handle(ev.Type, &sb)
	}
}

// mergePatch manda un JSON Merge Patch (RFC 7386, application/merge-patch+json)
// contra url y devuelve el objeto resultante, tal y como lo ve el API server
// justo después de aplicarlo. Un merge patch le viene bien a este operador
// porque solo toca las claves que se nombran: escribir status.state no pisa
// status.id, y así no hace falta releer el objeto entero antes de escribir.
//
// Devolver el objeto (y no solo un error) importa por su resourceVersion: si
// el que llama se queda con el de ANTES del patch, un watch que le devuelva
// su propio escrito (algo que Kubernetes hace de verdad: el operador está
// suscrito a lo que él mismo cambia) parecería "más nuevo" que la copia local
// aunque sea un eco de un paso ya superado, y podría hacer retroceder una
// reconciliación que ya había avanzado — por ejemplo, reprocesar el estado
// "finalizer puesto, aún sin crear" después de que la creación ya terminara,
// y crear el sandbox dos veces.
func (k *KubeClient) mergePatch(ctx context.Context, url string, patch any) (*Sandbox, error) {
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	req, err := k.newRequest(ctx, http.MethodPatch, url, "application/merge-patch+json", body)
	if err != nil {
		return nil, err
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	var sb Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
		return nil, fmt.Errorf("decoding patch response: %w", err)
	}
	return &sb, nil
}

// PatchStatus actualiza (mezcla) status.* a través del subrecurso /status.
// Pasar una SandboxStatus a medio rellenar es intencional: los campos en cero
// no se mandan (omitempty) y el merge patch los deja como estaban.
func (k *KubeClient) PatchStatus(ctx context.Context, namespace, name string, st SandboxStatus) (*Sandbox, error) {
	url := k.itemURL(namespace, name) + "/status"
	return k.mergePatch(ctx, url, map[string]any{"status": st})
}

// PatchFinalizers reemplaza metadata.finalizers por completo: un merge patch
// sustituye arrays enteros, no los fusiona elemento a elemento, así que quien
// llama tiene que mandar la lista final ya calculada (ver removeFinalizer en
// controller.go).
func (k *KubeClient) PatchFinalizers(ctx context.Context, namespace, name string, finalizers []string) (*Sandbox, error) {
	if finalizers == nil {
		finalizers = []string{}
	}
	url := k.itemURL(namespace, name)
	return k.mergePatch(ctx, url, map[string]any{"metadata": map[string]any{"finalizers": finalizers}})
}
