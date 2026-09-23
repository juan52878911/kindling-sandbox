package operator

// El bucle del operador: list + watch contra Kubernetes, reconciliando cada
// Sandbox contra el frontal. Sin client-go no hay workqueue ni informer, así
// que esto es la versión de mano: una caché en memoria de los objetos vistos,
// un candado que serializa toda reconciliación (evita que un watch y un
// resync se pisen creando dos veces el mismo sandbox) y un resync periódico
// que hace de red de seguridad para lo que un evento no repite por sí solo
// (el estado del frontal cambia sin que Kubernetes se entere).

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"strconv"
	"sync"
	"time"
)

// DefaultResync es cada cuánto se refresca el estado de cada sandbox desde el
// frontal y se reintenta lo que hubiera fallado (crear, borrar, renovar).
const DefaultResync = 30 * time.Second

// backoffBase es el primer plazo de espera al reconectar tras un list o un
// watch fallidos. Variable (no const) para que los tests de este mismo
// paquete puedan encogerlo y no esperar segundos de verdad por cada
// reconexión simulada.
var backoffBase = time.Second

// Controller reconcilia los Sandbox del clúster contra el frontal.
type Controller struct {
	Kube    *KubeClient
	Frontal *FrontalClient
	Log     *log.Logger
	// Resync es el periodo del resync; 0 usa DefaultResync.
	Resync time.Duration

	// reconcileMu serializa TODA reconciliación (venga de un evento de watch
	// o del resync): es lo único que impide crear dos veces el mismo sandbox
	// si ambos caminos lo procesan casi a la vez.
	reconcileMu sync.Mutex

	mu    sync.Mutex
	cache map[string]*Sandbox
}

// NewController construye el controlador. kube y frontal ya vienen
// configurados (ver main.go): el operador no sabe leer flags ni entorno.
func NewController(kube *KubeClient, frontal *FrontalClient, logger *log.Logger) *Controller {
	if logger == nil {
		logger = log.Default()
	}
	return &Controller{Kube: kube, Frontal: frontal, Log: logger, cache: map[string]*Sandbox{}}
}

func (c *Controller) resync() time.Duration {
	if c.Resync > 0 {
		return c.Resync
	}
	return DefaultResync
}

// Run bloquea hasta que ctx se cancele. list, luego watch desde el
// resourceVersion de la lista; si el watch se corta, reconecta desde el
// último resourceVersion visto; si el API server contesta 410 (el
// resourceVersion ya no está en su histórico), relista desde cero.
func (c *Controller) Run(ctx context.Context) error {
	go c.resyncLoop(ctx)
	backoff := backoffBase
	for ctx.Err() == nil {
		rv, err := c.relist(ctx)
		if err != nil {
			c.Log.Printf("operator: list: %v", err)
			if !sleepCtx(ctx, backoff) {
				break
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = backoffBase
		c.watchUntilGone(ctx, rv)
	}
	return ctx.Err()
}

// relist trae todos los Sandbox, reconcilia cada uno (por si el operador
// estuvo parado y algo cambió mientras tanto) y devuelve el resourceVersion
// desde el que arrancar el watch.
func (c *Controller) relist(ctx context.Context) (string, error) {
	list, err := c.Kube.List(ctx)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for i := range list.Items {
		sb := &list.Items[i]
		c.remember(sb)
		seen[sb.Key()] = true
	}
	for key := range seen {
		if latest := c.lookup(key); latest != nil {
			c.reconcile(ctx, latest)
		}
	}
	// Lo que estaba en caché y ya no aparece en la lista se borró mientras el
	// operador no miraba (o perdió su último finalizer): fuera de la caché.
	c.mu.Lock()
	for k := range c.cache {
		if !seen[k] {
			delete(c.cache, k)
		}
	}
	c.mu.Unlock()
	return list.Metadata.ResourceVersion, nil
}

// watchUntilGone mantiene el watch abierto, reconectando con backoff cuando
// se corta, hasta que ctx se cancele o el server pida un relist (410).
func (c *Controller) watchUntilGone(ctx context.Context, rv string) {
	backoff := backoffBase
	for ctx.Err() == nil {
		abierto := time.Now()
		err := c.Kube.Watch(ctx, rv, func(kind string, sb *Sandbox) {
			if sb.Metadata.ResourceVersion != "" {
				rv = sb.Metadata.ResourceVersion
			}
			if kind == "DELETED" {
				c.forget(sb.Key())
				return
			}
			if kind == "BOOKMARK" {
				return
			}
			c.remember(sb)
			// Se reconcilia la copia más nueva que la caché conoce, no el sb
			// tal cual llegó: remember puede haberlo descartado por
			// anticuado (ver su comentario), y reconciliar el sb crudo de
			// todos modos volvería a procesar un paso ya superado.
			if latest := c.lookup(sb.Key()); latest != nil {
				c.reconcile(ctx, latest)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errGone) {
			return // Run relista.
		}
		if err != nil {
			c.Log.Printf("operator: watch: %v", err)
		}
		// Un watch que aguantó abierto un buen rato y se cerró es lo normal (el
		// API server los corta por tiempo): no cuenta como racha de fallos, y
		// arrastrar el backoff de fallos anteriores solo retrasaría el
		// siguiente sin motivo.
		if time.Since(abierto) > time.Minute {
			backoff = backoffBase
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// resyncLoop reconcilia periódicamente todo lo que hay en caché: es lo que
// mantiene status.state al día (el frontal cambia sin avisar a Kubernetes) y
// reintenta lo que una reconciliación anterior dejó a medias.
func (c *Controller) resyncLoop(ctx context.Context) {
	t := time.NewTicker(c.resync())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, sb := range c.snapshot() {
				c.reconcile(ctx, sb)
			}
		}
	}
}

// remember guarda sb en la caché, PERO solo si es al menos tan nuevo como lo
// que ya había (por resourceVersion). Sin esto, un evento de watch que
// reparte el eco de un escrito propio anterior (más lento en volver que los
// pasos siguientes de la misma reconciliación) podría hacer retroceder la
// caché a un estado ya superado — y, en concreto, a un "finalizer puesto,
// aún sin crear" después de que la creación ya hubiera terminado, disparando
// una segunda creación en el frontal.
func (c *Controller) remember(sb *Sandbox) {
	cp := *sb
	key := cp.Key()
	c.mu.Lock()
	if old, ok := c.cache[key]; ok && rvOf(old) > rvOf(&cp) {
		c.mu.Unlock()
		return
	}
	c.cache[key] = &cp
	c.mu.Unlock()
}

// rvOf interpreta el resourceVersion como el entero que es en todo
// Kubernetes real (un contador de etcd); un valor vacío o no numérico (solo
// puede pasar en pruebas que construyen el objeto a mano) cuenta como el más
// antiguo posible, nunca como el más nuevo.
func rvOf(sb *Sandbox) int64 {
	n, _ := strconv.ParseInt(sb.Metadata.ResourceVersion, 10, 64)
	return n
}

func (c *Controller) lookup(key string) *Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cache[key]
}

func (c *Controller) forget(key string) {
	c.mu.Lock()
	delete(c.cache, key)
	c.mu.Unlock()
}

func (c *Controller) snapshot() []*Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Sandbox, 0, len(c.cache))
	for _, sb := range c.cache {
		out = append(out, sb)
	}
	return out
}

// ---- reconciliación

// reconcile decide y aplica UN paso para un Sandbox. Nunca muta sb (puede ser
// el mismo puntero que vive en la caché mientras otra goroutine lo lee): todo
// cambio se hace sobre una copia local que, si algo se escribió de verdad en
// Kubernetes, se guarda de vuelta en la caché con remember. reconcileMu
// asegura que dos llamadas (una de watch, una de resync) no se crucen.
func (c *Controller) reconcile(ctx context.Context, sb *Sandbox) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()

	cur := *sb
	ns, name := cur.Metadata.Namespace, cur.Metadata.Name
	changed := false

	if cur.Metadata.DeletionTimestamp != nil {
		c.reconcileDeleting(ctx, &cur)
		return
	}

	if !cur.Metadata.HasFinalizer() {
		fin := append(append([]string{}, cur.Metadata.Finalizers...), CleanupFinalizer)
		updated, err := c.Kube.PatchFinalizers(ctx, ns, name, fin)
		if err != nil {
			c.Log.Printf("operator: %s/%s: adding finalizer: %v", ns, name, err)
			return
		}
		cur.Metadata.Finalizers = fin
		// El resourceVersion de la respuesta, no el de antes del patch: si
		// no se actualiza aquí, un watch que le devuelva a esta misma
		// reconciliación su PROPIO escrito (Kubernetes hace eco de lo que el
		// operador cambia porque está suscrito a ello) parecería tan nuevo
		// como esta copia local aunque sea un paso ya superado, y remember
		// no lo rechazaría — reabriendo la puerta a crear el sandbox dos
		// veces (ver el comentario de mergePatch en kube.go).
		cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
		changed = true
		c.remember(&cur)
	}

	if cur.Status.ID == "" {
		out, err := c.Frontal.Create(ctx, cur.Spec)
		if err != nil {
			c.Log.Printf("operator: %s/%s: creating sandbox: %v", ns, name, err)
			c.setMessage(ctx, &cur, err.Error())
			return
		}
		cur.Status = SandboxStatus{
			ID: out.ID, Host: out.Host, State: string(out.State),
			ExpiresAt: out.ExpiresAt, ObservedGeneration: cur.Metadata.Generation,
		}
		if updated, err := c.Kube.PatchStatus(ctx, ns, name, cur.Status); err != nil {
			c.Log.Printf("operator: %s/%s: writing status after create: %v", ns, name, err)
			// El sandbox YA nace en el frontal aunque esto falle: no se
			// reintenta el create (dejaría un sandbox huérfano más), el
			// próximo resync reintentará solo escribir el status con el
			// id que ahora mismo solo conoce esta goroutine... y se
			// perdería si el proceso muere aquí. Es el hueco que un CRD
			// sin más estado que Kubernetes no puede cerrar del todo;
			// se acepta porque el resync de 30s deja el status como
			// mucho ese tiempo desfasado, nunca duplica la creación
			// (status.id ya no está vacío en esta copia local).
		} else {
			cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
		}
		c.remember(&cur)
		c.Log.Printf("operator: %s/%s: created sandbox %s", ns, name, out.ID)
		return
	}

	// Ya existe: idempotente por construcción (el `if` de arriba ya lo cortó).
	// Si spec cambió (hoy, solo ttlSeconds es mutable) se renueva.
	if cur.Status.ObservedGeneration != cur.Metadata.Generation {
		out, err := c.Frontal.Renew(ctx, cur.Status.ID, cur.Spec.TTLSeconds)
		if err != nil {
			c.Log.Printf("operator: %s/%s: renewing: %v", ns, name, err)
			c.setMessage(ctx, &cur, err.Error())
			return
		}
		cur.Status.State = string(out.State)
		cur.Status.ExpiresAt = out.ExpiresAt
		cur.Status.ObservedGeneration = cur.Metadata.Generation
		cur.Status.Message = ""
		changed = true
	}

	// Refresco periódico: lo que el frontal sabe manda. Si desapareció (lo
	// borró la limpieza por abandono, o alguien a mano), se marca "gone" sin
	// volver a crearlo: status.id ya no está vacío, y no se recrea nada solo
	// porque el frontal lo olvidó.
	out, err := c.Frontal.Get(ctx, cur.Status.ID)
	switch {
	case err == nil:
		if cur.Status.State != string(out.State) || !sameTime(cur.Status.ExpiresAt, out.ExpiresAt) || cur.Status.Message != "" {
			cur.Status.State = string(out.State)
			cur.Status.ExpiresAt = out.ExpiresAt
			cur.Status.Host = out.Host
			cur.Status.Message = ""
			changed = true
		}
	case IsFrontalNotFound(err):
		if cur.Status.State != StateGone {
			cur.Status.State = StateGone
			changed = true
		}
	default:
		c.Log.Printf("operator: %s/%s: refreshing status: %v", ns, name, err)
	}

	if changed {
		updated, err := c.Kube.PatchStatus(ctx, ns, name, cur.Status)
		if err != nil {
			c.Log.Printf("operator: %s/%s: writing status: %v", ns, name, err)
			return
		}
		cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
		c.remember(&cur)
	}
}

// reconcileDeleting maneja un Sandbox con deletionTimestamp: borra en el
// frontal y quita el finalizer. Si el borrado falla, se deja el finalizer
// puesto a propósito — el próximo resync (o el próximo evento de watch)
// vuelve a intentarlo, y así nunca se pierde un sandbox huérfano por un fallo
// pasajero del frontal.
func (c *Controller) reconcileDeleting(ctx context.Context, cur *Sandbox) {
	ns, name := cur.Metadata.Namespace, cur.Metadata.Name
	if !cur.Metadata.HasFinalizer() {
		return // nada que limpiar, Kubernetes ya puede borrarlo.
	}
	if cur.Status.ID != "" {
		if err := c.Frontal.Delete(ctx, cur.Status.ID); err != nil {
			c.Log.Printf("operator: %s/%s: deleting sandbox %s: %v", ns, name, cur.Status.ID, err)
			return
		}
	}
	fin := removeFinalizer(cur.Metadata.Finalizers)
	updated, err := c.Kube.PatchFinalizers(ctx, ns, name, fin)
	if err != nil {
		c.Log.Printf("operator: %s/%s: removing finalizer: %v", ns, name, err)
		return
	}
	cur.Metadata.Finalizers = fin
	cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
	c.remember(cur)
	c.Log.Printf("operator: %s/%s: cleaned up sandbox %s", ns, name, cur.Status.ID)
}

func (c *Controller) setMessage(ctx context.Context, cur *Sandbox, msg string) {
	if cur.Status.Message == msg {
		return
	}
	cur.Status.Message = msg
	updated, err := c.Kube.PatchStatus(ctx, cur.Metadata.Namespace, cur.Metadata.Name, SandboxStatus{Message: msg})
	if err != nil {
		c.Log.Printf("operator: %s/%s: writing error message: %v", cur.Metadata.Namespace, cur.Metadata.Name, err)
		return
	}
	cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
	c.remember(cur)
}

func removeFinalizer(finalizers []string) []string {
	out := make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if f != CleanupFinalizer {
			out = append(out, f)
		}
	}
	return out
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// ---- utilidades

// sleepCtx espera d o hasta que ctx se cancele; devuelve false si fue el
// contexto quien lo cortó.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextBackoff dobla el plazo con un poco de aleatoriedad (evita que, si el
// API server se recupera, todos los operadores del clúster reconecten en el
// mismo instante) y lo tapa en 30 s.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(d)/4 + 1))
	return d + jitter
}
