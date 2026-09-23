package frontal

// Autocuración de plantillas invalidadas por un reinicio del host.
//
// Un snapshot de kindling lleva grabada la frecuencia del TSC del host en el
// que se hizo, y un reinicio la cambia: TODOS los dorados de ese host dejan de
// restaurar a la vez, sin que un byte de sus ficheros cambie. api.EsFalloTSC
// reconoce ese fallo concreto (ver pkg/api/tsc.go del núcleo), y
// plantilla.Construir es, a propósito, también el camino de recuperación:
// reconstruir pisa el dorado roto con uno nuevo (replace=true).
//
// La receta para reconstruir no hace falta pedírsela a nadie: viaja colgada
// del propio snapshot roto, en la anotación plantilla.AnotacionReceta que dejó
// la construcción original (cmd/kling-sandbox/template.go hace lo mismo a
// mano, con `sbx template rebuild`). Aquí se dispara sola, en segundo plano,
// la primera vez que crear un sandbox de esa plantilla en ese host tropieza
// con el fallo.

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling-sandbox/internal/plantilla"
)

// retryAfterReconstruccion es lo que se ofrece en la cabecera Retry-After del
// 503: no es una promesa exacta de cuánto va a tardar —depende de la receta—,
// es solo para que un cliente no reintente antes de que tenga sentido.
const retryAfterReconstruccion = "60"

// reconstrucciones vigila qué pares (host, plantilla) se están reconstruyendo
// AHORA MISMO, para no lanzar dos veces el mismo trabajo: Construir tarda
// minutos, y una ráfaga de peticiones contra la misma plantilla rota lanzaría
// una reconstrucción por petición si no hubiera nada que lo impidiera.
type reconstrucciones struct {
	mu      sync.Mutex
	enCurso map[string]bool
}

func claveReconstruccion(host, plantilla string) string { return host + "\x00" + plantilla }

// empezar dice si (host, plantilla) NO se estaba reconstruyendo ya, y si es
// así, lo marca como que empieza ahora. Un "no" no es un error: solo dice que
// ya hay una en marcha y esta petición no tiene que lanzar otra.
func (r *reconstrucciones) empezar(host, tpl string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := claveReconstruccion(host, tpl)
	if r.enCurso[k] {
		return false
	}
	if r.enCurso == nil {
		r.enCurso = map[string]bool{}
	}
	r.enCurso[k] = true
	return true
}

// enMarcha dice si (host, plantilla) se está reconstruyendo ahora mismo. Solo
// lo usan los tests: el camino normal no necesita preguntarlo, solo empezar y
// dejar que termine.
func (r *reconstrucciones) enMarcha(host, tpl string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enCurso[claveReconstruccion(host, tpl)]
}

func (r *reconstrucciones) terminar(host, tpl string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.enCurso, claveReconstruccion(host, tpl))
}

// repararEnSegundoPlano lanza, si no hay ya una en marcha, la reconstrucción
// de nombrePlantilla en h a partir de la receta grabada en su propio snapshot.
//
// No bloquea: quien llama (crear, tras un fallo de TSC) ya tiene que contestar
// esta petición con un 503, no esperar minutos a que termine una construcción.
func (s *Servidor) repararEnSegundoPlano(h *hosts.Host, nombrePlantilla string) {
	if !s.reconstruir.empezar(h.Nombre, nombrePlantilla) {
		return
	}
	go func() {
		defer s.reconstruir.terminar(h.Nombre, nombrePlantilla)

		// Contexto propio: la petición HTTP que disparó esto ya habrá terminado
		// (con su 503) mucho antes de que una reconstrucción, que tarda minutos,
		// acabe. MargenTTL de la máquina de preparación es de 10 minutos; 15 le
		// deja margen sin quedarse colgado para siempre si algo se atasca.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		snap := plantilla.SnapshotDe(nombrePlantilla)
		sn, err := h.Cliente.Snapshot(ctx, snap)
		if err != nil {
			s.log.Printf("frontal: rebuild %s/%s: can't read the broken snapshot: %v", h.Nombre, nombrePlantilla, err)
			return
		}
		var rec struct {
			Plantilla plantilla.Plantilla `json:"template"`
		}
		raw, ok := sn.Annotations[plantilla.AnotacionReceta]
		if !ok || json.Unmarshal(raw, &rec) != nil || rec.Plantilla.Nombre == "" {
			s.log.Printf("frontal: rebuild %s/%s: no readable recipe on the snapshot, can't self-heal", h.Nombre, nombrePlantilla)
			return
		}

		s.log.Printf("frontal: rebuild %s/%s: starting (a host restart likely invalidated its snapshot)", h.Nombre, nombrePlantilla)
		inicio := time.Now()
		if _, err := plantilla.Construir(ctx, h.Cliente, rec.Plantilla); err != nil {
			s.log.Printf("frontal: rebuild %s/%s: failed after %s: %v", h.Nombre, nombrePlantilla, time.Since(inicio).Round(time.Second), err)
			return
		}
		s.log.Printf("frontal: rebuild %s/%s: done in %s", h.Nombre, nombrePlantilla, time.Since(inicio).Round(time.Second))
	}()
}
