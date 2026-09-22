package plantilla

// DE RECETA A SNAPSHOT DORADO.
//
// Construir arranca una microVM de preparación, ejecuta dentro los pasos de la
// receta, la congela como snapshot y la borra. Todo con el API público de
// kindling (pkg/api): nada de esto vive en el daemon, y por eso una plantilla
// puede cambiar sin tocar el núcleo.
//
// Lo caro se paga UNA vez aquí para que crear un sandbox cueste un thaw: el
// snapshot es memoria ya instalada, no un rootfs que haya que volver a poblar.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// EgressPorDefecto es la red de la máquina de PREPARACIÓN cuando la plantilla
// no dice otra cosa: preparar es instalar, y sin salida no se instala nada.
//
// No es la red de los sandboxes que nacen del snapshot: esa se decide al
// crearlos y por defecto es ninguna.
const EgressPorDefecto = "internet"

// KindConstruccion marca la máquina de preparación en la etiqueta api.LabelKind.
// Es un valor distinto de api.KindSandbox a propósito: quien liste sandboxes no
// debe encontrarse máquinas de construir, y quien vea una huérfana debe saber de
// un vistazo de dónde salió.
const KindConstruccion = "template-build"

// EtiquetaPlantilla dice de qué plantilla es la máquina (y, heredada, el
// snapshot que salga de ella).
const EtiquetaPlantilla = "template"

// MargenTTL es lo que se le da a la máquina de preparación por encima de la
// suma de los plazos de sus pasos.
//
// El TTL es una red de seguridad, no el plazo real: si este proceso muere de
// golpe, el defer que borra la máquina no llega a correr y esa microVM retendría
// su RAM hasta que alguien la viera. El margen cubre lo que no son pasos —
// arrancar en frío, esperar al agente, congelar el dorado—.
const MargenTTL = 10 * time.Minute

// Topes de la salida que se incluye en el error de un paso fallido. Un paso
// puede escupir megabytes; lo que ayuda a entender por qué falló son las
// últimas líneas, y volcar el resto solo entierra el mensaje.
const (
	maxLineasError = 20
	maxBytesError  = 4 << 10
)

// reNombre acota el nombre de una plantilla. Es componente de un nombre de
// snapshot (SnapshotDe) y de una ruta en el daemon, así que se queda en el
// subconjunto que kindling acepta sin discusión.
var reNombre = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ErrorPaso es un paso de preparación que terminó con código distinto de cero.
//
// Es un tipo y no un fmt.Errorf porque quien construye en lote necesita el
// código y el comando por separado para decidir qué hacer, no una cadena que
// tendría que volver a parsear.
type ErrorPaso struct {
	Plantilla string
	Indice    int // empezando en 1, como se cuentan los pasos al leer la receta
	Cmd       []string
	Codigo    int
	Stdout    string
	Stderr    string
	TimedOut  bool
	Truncated bool
}

func (e *ErrorPaso) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "template %q: step %d (%s) exited with code %d",
		e.Plantilla, e.Indice, comandoLegible(e.Cmd), e.Codigo)
	if e.TimedOut {
		b.WriteString(" after running out of time")
	}
	if e.Truncated {
		b.WriteString(" (output was truncated)")
	}
	if e.Stderr != "" {
		fmt.Fprintf(&b, "\n--- stderr (last lines) ---\n%s", e.Stderr)
	}
	if e.Stdout != "" {
		fmt.Fprintf(&b, "\n--- stdout (last lines) ---\n%s", e.Stdout)
	}
	return b.String()
}

// Construir convierte una receta en un snapshot dorado reutilizable.
//
// Deja el snapshot con la receta colgada como anotación, y no deja máquina: la
// de preparación se borra siempre, también cuando algo falla.
func Construir(ctx context.Context, c *api.Client, p Plantilla) (*Estado, error) {
	if err := validar(p); err != nil {
		return nil, fmt.Errorf("invalid template: %w", err)
	}
	nombreSnap := SnapshotDe(p.Nombre)

	mc, err := c.Run(ctx, api.RunRequest{
		Image:  p.Imagen,
		VCPUs:  p.VCPUs,
		MemMiB: p.MemMiB,
		CPUPct: p.CPUPct,
		Egress: egresoDe(p),
		// AllowDomains solo lo mira el daemon con egress "allowlist"; pasarlo
		// siempre no tiene coste y evita una rama aquí.
		AllowDomains: p.AllowBuild,
		// AllowExec se decide al ARRANCAR y se congela con la memoria: sin él no
		// habría pasos que ejecutar, y además el snapshot que salga de aquí no
		// podría dar sandboxes con ejecución.
		AllowExec: true,
		Volumes:   adjuntos(p.Volumes),
		// Con on_ttl=remove el TTL es vida MÁXIMA y el uso no lo reinicia: por
		// eso se calcula sobre los plazos de los pasos y no sobre un número fijo.
		TTLSeconds: int(ttlPreparacion(p) / time.Second),
		OnTTL:      api.OnTTLRemove,
		Labels: map[string]string{
			api.LabelKind:     KindConstruccion,
			EtiquetaPlantilla: p.Nombre,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("template %q: can't start the build machine from image %q: %w", p.Nombre, p.Imagen, err)
	}

	// Borrar SIEMPRE la máquina de preparación. Una huérfana retiene su RAM y su
	// overlay, y en un host con densidad alta eso es el hueco de varios sandboxes.
	defer func() {
		// Contexto propio: si el de la construcción ya venció —un paso que se
		// pasó de plazo, quien llamaba que se fue—, borrar con él fallaría justo
		// cuando más falta hace.
		ctxBorrar, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := c.Remove(ctxBorrar, mc.ID); err != nil && !api.IsNotFound(err) {
			// No se convierte en el error devuelto: si la construcción fue bien,
			// el snapshot ya existe y es válido, y tirarlo por no poder borrar
			// una máquina sería cambiar un problema pequeño por uno grande.
			// Queda dicho, con el id, para poder borrarla a mano.
			log.Printf("plantilla %s: couldn't remove build machine %s: %v", p.Nombre, mc.ID, err)
		}
	}()

	// Esperar al agente del invitado sin sondear a mano: el daemon ya espera por
	// nosotros en cada exec (waitAgent), así que un primer exec trivial ES la
	// espera. `kling run` vuelve cuando arranca el VMM, no cuando el invitado
	// está listo, y un arranque en frío tarda segundos.
	//
	// Se usa /bin/true, con ruta absoluta, para no depender del PATH del agente.
	if _, err := c.Exec(ctx, mc.ID, api.ExecRequest{
		Cmd:            []string{"/bin/true"},
		TimeoutSeconds: 60,
	}, nil); err != nil {
		return nil, fmt.Errorf("template %q: the build machine never became usable: %w", p.Nombre, err)
	}

	for i, paso := range p.Pasos {
		// onOutput nil: la salida se acumula en el resultado, que es lo que hace
		// falta para explicar un fallo. Un paso que va bien no interesa a nadie.
		res, err := c.Exec(ctx, mc.ID, api.ExecRequest{
			Cmd:            paso.Cmd,
			Dir:            paso.Dir,
			Env:            paso.Env,
			TimeoutSeconds: paso.Timeout,
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("template %q: step %d (%s) couldn't run: %w",
				p.Nombre, i+1, comandoLegible(paso.Cmd), err)
		}
		if res.ExitCode != 0 {
			return nil, &ErrorPaso{
				Plantilla: p.Nombre,
				Indice:    i + 1,
				Cmd:       paso.Cmd,
				Codigo:    res.ExitCode,
				Stdout:    ultimasLineas(res.Stdout),
				Stderr:    ultimasLineas(res.Stderr),
				TimedOut:  res.TimedOut,
				Truncated: res.Truncated,
			}
		}
	}

	// replace=true: rehacer un dorado es rutina, no excepción. Los snapshots
	// quedan atados al TSC del host y un reinicio los invalida todos, así que
	// negarse a pisar el anterior dejaría la plantilla sin salida.
	sn, err := c.Commit(ctx, mc.ID, nombreSnap, true)
	if err != nil {
		return nil, fmt.Errorf("template %q: can't commit snapshot %q: %w", p.Nombre, nombreSnap, err)
	}

	hecho := sn.CreatedAt
	if hecho.IsZero() {
		hecho = time.Now().UTC()
	}
	rec := Receta{Plantilla: p, Hash: HashReceta(p), Hecho: hecho}

	// La anotación es parte de la construcción, no un extra: un snapshot sin
	// receta es opaco —no se sabe si sirve ni cómo rehacerlo—, así que si esto
	// falla la construcción ha fallado, aunque el snapshot exista.
	if _, err := c.SetAnnotation(ctx, nombreSnap, AnotacionReceta, rec); err != nil {
		return nil, fmt.Errorf("template %q: snapshot %q was built but its recipe couldn't be saved: %w",
			p.Nombre, nombreSnap, err)
	}

	return &Estado{
		Host:      c.Endpoint(),
		Snapshot:  nombreSnap,
		Hecho:     hecho,
		Receta:    rec.Hash,
		Instancia: sn.Instances,
	}, nil
}

// Reconciliar deja el host con el snapshot que pide la plantilla y dice si hubo
// que construirlo.
//
// Se construye cuando el snapshot no está, cuando lleva otra receta y cuando su
// receta no se puede leer: en los tres casos lo que hay no es lo que se pide, y
// lo único que se sabe hacer es rehacerlo.
//
// LIMITACIÓN CONOCIDA: que el snapshot exista y cuadre no garantiza que
// RESTAURE. Un snapshot de Firecracker graba la frecuencia del TSC del host, que
// se vuelve a medir en cada arranque, así que un reinicio los invalida todos a
// la vez sin tocar un byte de sus ficheros. El API no lo expone —GET
// /snapshots/{name} devuelve un snapshot sano— y solo se ve al restaurar, en el
// error, que api.EsFalloTSC reconoce. Por eso quien instancie debe tratar ese
// fallo como "hay que reconstruir" y volver a llamar aquí: Construir pisa el
// dorado (replace), de modo que el camino de recuperación es este mismo.
func Reconciliar(ctx context.Context, c *api.Client, p Plantilla) (*Estado, bool, error) {
	if err := validar(p); err != nil {
		return nil, false, fmt.Errorf("invalid template: %w", err)
	}
	nombreSnap := SnapshotDe(p.Nombre)
	hash := HashReceta(p)

	sn, err := c.Snapshot(ctx, nombreSnap)
	switch {
	case err == nil:
		var rec Receta
		if ok, errAnot := sn.Annotation(AnotacionReceta, &rec); ok && errAnot == nil && rec.Hash == hash {
			hecho := rec.Hecho
			if hecho.IsZero() {
				hecho = sn.CreatedAt
			}
			return &Estado{
				Host:      c.Endpoint(),
				Snapshot:  nombreSnap,
				Hecho:     hecho,
				Receta:    rec.Hash,
				Instancia: sn.Instances,
			}, false, nil
		}
		// Sin anotación, ilegible o de otra receta: se reconstruye.
	case api.IsNotFound(err):
		// Nunca se construyó aquí, o el host perdió sus dorados: caso normal.
	default:
		// Un daemon que no contesta no es "falta el snapshot": construir ahora
		// sería tapar una avería con minutos de trabajo que también fallarán.
		return nil, false, fmt.Errorf("template %q: can't check snapshot %q: %w", p.Nombre, nombreSnap, err)
	}

	est, err := Construir(ctx, c, p)
	if err != nil {
		return nil, false, err
	}
	return est, true, nil
}

// validar rechaza lo que el daemon rechazaría más tarde y más caro: mejor un
// error antes de arrancar una microVM que a los tres minutos de instalar.
func validar(p Plantilla) error {
	if !reNombre.MatchString(p.Nombre) {
		return fmt.Errorf("name %q is not valid: use lowercase letters, digits, - and _, "+
			"starting with a letter or a digit, up to 64 characters", p.Nombre)
	}
	if strings.TrimSpace(p.Imagen) == "" {
		return errors.New("image is required: a template says which kindling image it starts from")
	}
	switch p.EgressBuild {
	case "", "none", "internet", "allowlist":
	default:
		return fmt.Errorf("build_egress %q is not valid: use \"none\", \"internet\" or \"allowlist\"", p.EgressBuild)
	}
	if p.EgressBuild == "allowlist" && len(p.AllowBuild) == 0 {
		return errors.New("build_egress \"allowlist\" needs at least one domain in build_allow_domains")
	}
	if p.VCPUs < 0 || p.MemMiB < 0 || p.CPUPct < 0 || p.Pool < 0 {
		return errors.New("vcpus, mem_mib, cpu_pct and pool can't be negative")
	}
	maxPlazo := int(api.ExecMaxTimeout / time.Second)
	for i, paso := range p.Pasos {
		if len(paso.Cmd) == 0 || strings.TrimSpace(paso.Cmd[0]) == "" {
			return fmt.Errorf("step %d has no command: cmd is argv, e.g. [\"sh\", \"-c\", \"apt-get install -y git\"]", i+1)
		}
		if paso.Timeout < 0 {
			return fmt.Errorf("step %d has a negative timeout_seconds", i+1)
		}
		if paso.Timeout > maxPlazo {
			return fmt.Errorf("step %d asks for a timeout of %ds; the daemon's maximum is %ds", i+1, paso.Timeout, maxPlazo)
		}
		for _, e := range paso.Env {
			if !strings.Contains(e, "=") {
				return fmt.Errorf("step %d has an invalid env entry %q: use KEY=value", i+1, e)
			}
		}
	}
	for i, v := range p.Volumes {
		if strings.TrimSpace(v.Nombre) == "" {
			return fmt.Errorf("volume %d has no name", i+1)
		}
	}
	return nil
}

// egresoDe es la red efectiva de la preparación.
func egresoDe(p Plantilla) string {
	if p.EgressBuild == "" {
		return EgressPorDefecto
	}
	return p.EgressBuild
}

// ttlPreparacion es la vida máxima que se le da a la máquina de preparación: lo
// que como mucho pueden tardar sus pasos, más el margen de lo que no son pasos.
func ttlPreparacion(p Plantilla) time.Duration {
	total := time.Duration(0)
	for _, paso := range p.Pasos {
		if paso.Timeout > 0 {
			total += time.Duration(paso.Timeout) * time.Second
			continue
		}
		// Sin plazo propio manda el del daemon, que es el que de verdad va a
		// cortar el paso.
		total += api.ExecDefaultTimeout
	}
	return total + MargenTTL
}

func adjuntos(vs []Volumen) []api.VolumeAttachment {
	if len(vs) == 0 {
		return nil
	}
	out := make([]api.VolumeAttachment, 0, len(vs))
	for _, v := range vs {
		out = append(out, api.VolumeAttachment{Name: v.Nombre, Mount: v.Mount, ReadOnly: v.ReadOnly})
	}
	return out
}

// comandoLegible escribe un argv para que se pueda leer y copiar: se entrecomilla
// lo que lleve espacios, que es justo el ["sh", "-c", "..."] de la mayoría de los
// pasos.
func comandoLegible(cmd []string) string {
	partes := make([]string, 0, len(cmd))
	for _, a := range cmd {
		if strings.ContainsAny(a, " \t\n\"") {
			partes = append(partes, fmt.Sprintf("%q", a))
			continue
		}
		partes = append(partes, a)
	}
	return strings.Join(partes, " ")
}

// ultimasLineas recorta la salida a algo que se pueda leer en un error. Se
// conserva el FINAL: el mensaje que explica el fallo está al final, no al
// principio, y delante suele haber miles de líneas de progreso.
func ultimasLineas(b []byte) string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return ""
	}
	lineas := strings.Split(s, "\n")
	recortado := false
	if len(lineas) > maxLineasError {
		lineas = lineas[len(lineas)-maxLineasError:]
		recortado = true
	}
	s = strings.Join(lineas, "\n")
	if len(s) > maxBytesError {
		s = s[len(s)-maxBytesError:]
		recortado = true
	}
	if recortado {
		return "[...]\n" + s
	}
	return s
}
