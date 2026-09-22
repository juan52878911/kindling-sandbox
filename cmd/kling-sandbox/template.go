package main

// `kling sbx template`: las recetas.
//
// Construir una plantilla es trabajo del host, no del frontal: se hace con el
// cliente del daemon directamente, para poder prepararla antes de que nadie
// pida un sandbox y para poder rehacerla cuando un reinicio del host invalide
// sus snapshots.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling-sandbox/internal/hosts"
	"github.com/juan52878911/kindling-sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

func cmdTemplate(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kling sbx template <apply|ls|rebuild|rm> [...]")
	}
	switch args[0] {
	case "apply":
		return tplApply(args[1:])
	case "ls", "list":
		return tplLs(args[1:])
	case "rebuild":
		return tplRebuild(args[1:])
	case "rm", "remove":
		return tplRm(args[1:])
	}
	return fmt.Errorf("unknown subcommand %q: use apply, ls, rebuild or rm", args[0])
}

// registro abre el inventario de hosts de la configuración.
func registro() (*hosts.Registro, error) {
	a, err := leerAjustes()
	if err != nil {
		return nil, err
	}
	return hosts.Nuevo(a.Hosts), nil
}

// leerPlantilla lee la receta de un fichero o de la entrada estándar.
func leerPlantilla(ruta string) (plantilla.Plantilla, error) {
	var p plantilla.Plantilla
	var b []byte
	var err error
	if ruta == "-" || ruta == "" {
		b, err = os.ReadFile("/dev/stdin")
	} else {
		b, err = os.ReadFile(ruta)
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s: %w", ruta, err)
	}
	return p, nil
}

func tplApply(args []string) error {
	fs := flag.NewFlagSet("sbx template apply", flag.ExitOnError)
	fichero := fs.String("f", "", "JSON file with the recipe ('-' = stdin)")
	host := fs.String("host", "", "only this host (default: all of them)")
	forzar := fs.Bool("force", false, "rebuild even if the recipe did not change")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fichero == "" {
		return errors.New("usage: kling sbx template apply -f tpl.json [-host NAME] [-force]")
	}
	p, err := leerPlantilla(*fichero)
	if err != nil {
		return err
	}
	reg, err := registro()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()

	// En TODOS los hosts, porque un snapshot no viaja: cada daemon necesita el
	// suyo o no podrá servir sandboxes de esta plantilla.
	var fallos int
	for _, h := range reg.Todos() {
		if *host != "" && h.Nombre != *host {
			continue
		}
		inicio := time.Now()
		var est *plantilla.Estado
		var hecho bool
		if *forzar {
			est, err = plantilla.Construir(ctx, h.Cliente, p)
			hecho = err == nil
		} else {
			est, hecho, err = plantilla.Reconciliar(ctx, h.Cliente, p)
		}
		switch {
		case err != nil:
			fmt.Printf("  %-12s ✗ %v\n", h.Nombre, err)
			fallos++
		case hecho:
			fmt.Printf("  %-12s ✓ built %s in %s\n", h.Nombre, est.Snapshot, time.Since(inicio).Round(time.Second))
		default:
			fmt.Printf("  %-12s · already up to date (%s)\n", h.Nombre, est.Snapshot)
		}
	}
	if fallos > 0 {
		return fmt.Errorf("%d host(s) could not build the template", fallos)
	}
	fmt.Printf("\nSandboxes from it:  kling sbx new -template %s\n", p.Nombre)
	return nil
}

func tplLs(args []string) error {
	reg, err := registro()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "TEMPLATE\tHOST\tSNAPSHOT\tBUILT\tINSTANCES")
	for _, h := range reg.Todos() {
		snaps, err := h.Cliente.Snapshots(ctx)
		if err != nil {
			fmt.Fprintf(tw, "—\t%s\t✗ %v\t\t\n", h.Nombre, err)
			continue
		}
		for _, s := range snaps {
			if !strings.HasPrefix(s.Name, "sbx-") {
				continue
			}
			hecho := "—"
			if r, err := leerReceta(ctx, h.Cliente, s.Name); err == nil && !r.IsZero() {
				hecho = r.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", strings.TrimPrefix(s.Name, "sbx-"), h.Nombre, s.Name, hecho, s.Instances)
		}
	}
	return tw.Flush()
}

// leerReceta saca la fecha de construcción de la anotación del snapshot.
func leerReceta(ctx context.Context, c *api.Client, snap string) (time.Time, error) {
	s, err := c.Snapshot(ctx, snap)
	if err != nil {
		return time.Time{}, err
	}
	raw, ok := s.Annotations[plantilla.AnotacionReceta]
	if !ok {
		return time.Time{}, nil
	}
	var r struct {
		Hecho time.Time `json:"built_at"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return time.Time{}, err
	}
	return r.Hecho, nil
}

func tplRebuild(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kling sbx template rebuild <name> [-host NAME]")
	}
	// Reconstruir es aplicar la receta que quedó grabada en el snapshot: por eso
	// se guarda entera y no solo su hash.
	fs := flag.NewFlagSet("sbx template rebuild", flag.ExitOnError)
	host := fs.String("host", "", "only this host")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	nombre := args[0]
	reg, err := registro()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()

	var p plantilla.Plantilla
	var encontrada bool
	for _, h := range reg.Todos() {
		s, err := h.Cliente.Snapshot(ctx, plantilla.SnapshotDe(nombre))
		if err != nil {
			continue
		}
		raw, ok := s.Annotations[plantilla.AnotacionReceta]
		if !ok {
			continue
		}
		var r struct {
			Plantilla plantilla.Plantilla `json:"template"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Plantilla.Nombre != "" {
			p, encontrada = r.Plantilla, true
			break
		}
	}
	if !encontrada {
		return fmt.Errorf("no recipe for %q on any host: apply it again with -f", nombre)
	}
	var fallos int
	for _, h := range reg.Todos() {
		if *host != "" && h.Nombre != *host {
			continue
		}
		inicio := time.Now()
		est, err := plantilla.Construir(ctx, h.Cliente, p)
		if err != nil {
			fmt.Printf("  %-12s ✗ %v\n", h.Nombre, err)
			fallos++
			continue
		}
		fmt.Printf("  %-12s ✓ rebuilt %s in %s\n", h.Nombre, est.Snapshot, time.Since(inicio).Round(time.Second))
	}
	if fallos > 0 {
		return fmt.Errorf("%d host(s) could not rebuild it", fallos)
	}
	return nil
}

func tplRm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kling sbx template rm <name>")
	}
	reg, err := registro()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()
	snap := plantilla.SnapshotDe(args[0])
	var fallos int
	for _, h := range reg.Todos() {
		if err := h.Cliente.RemoveSnapshot(ctx, snap); err != nil {
			if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "doesn't exist") {
				continue
			}
			fmt.Printf("  %-12s ✗ %v\n", h.Nombre, err)
			fallos++
			continue
		}
		fmt.Printf("  %-12s ✓ removed %s\n", h.Nombre, snap)
	}
	if fallos > 0 {
		return fmt.Errorf("%d host(s) could not remove it", fallos)
	}
	return nil
}
