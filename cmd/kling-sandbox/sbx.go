package main

// `kling sbx`: el comando único de esta extensión.
//
// Tiene dos mitades que no se parecen: `gateway` levanta el frontal (un proceso
// que escucha y reparte), y el resto son comandos de cliente que hablan CON ese
// frontal por HTTP. Viven juntos porque son el mismo producto y se despliegan
// juntos, y porque separar el binario obligaría a instalar dos cosas para usar
// una.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

func cmdSbx(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kling sbx <gateway|template|new|ls|rm|renew|exec|cp|hosts> [...]")
	}
	switch args[0] {
	case "gateway":
		return cmdGateway(args[1:])
	case "template", "templates":
		return cmdTemplate(args[1:])
	case "new", "create":
		return sbxNew(args[1:])
	case "ls", "list":
		return sbxLs(args[1:])
	case "rm", "remove":
		return sbxRm(args[1:])
	case "renew":
		return sbxRenew(args[1:])
	case "exec":
		return sbxExec(args[1:])
	case "cp":
		return sbxCp(args[1:])
	case "shell":
		// Termina con el código de la shell remota, sin el "error:" de siempre:
		// un 1 de grep no es un fallo de kling.
		code, err := sbxShell(args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			if code == 0 {
				code = 1
			}
		}
		os.Exit(code)
	case "hosts":
		return sbxHosts(args[1:])
	case "-h", "--help", "help":
		fmt.Println(ayuda)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q: use gateway, template, new, ls, rm, renew, exec, cp or hosts", args[0])
}

const ayuda = `kling sbx — sandboxes for code agents, served by a gateway

  sbx gateway [-listen :8090]                  serves the sandbox API
  sbx template apply -f tpl.json               builds the recipe into a golden snapshot
  sbx template ls | rebuild <name>             what is built, and rebuild it
  sbx new -template T [-ttl 10m] [-q]          a sandbox from that template
  sbx ls | rm <id>... | renew <id> [-ttl D]    the ones you own
  sbx exec <id> [--] <cmd> [args...]           runs a command inside, streaming
  sbx shell <id>                               an interactive terminal inside
  sbx cp <local|-> <id>:<path> | <id>:<path> <local|->
  sbx hosts                                    the daemons behind the gateway

The client commands talk to the gateway, not to a daemon:
  kling config set sandbox.url http://gateway:8090
  kling config set sandbox.token <token>`

// cliente es el cliente HTTP del frontal. Plazo largo: al otro lado puede haber
// un arranque en frío de varios segundos.
type cliente struct {
	url   string
	token string
	http  *http.Client
}

func nuevoCliente() (*cliente, error) {
	a, err := leerAjustes()
	if err != nil {
		return nil, err
	}
	if v := os.Getenv("KLING_SANDBOX_URL"); v != "" {
		a.URL = v
	}
	if v := os.Getenv("KLING_SANDBOX_TOKEN"); v != "" {
		a.Token = v
	}
	if a.Token == "" {
		return nil, errors.New("no gateway token: kling config set sandbox.token <token> (or $KLING_SANDBOX_TOKEN)")
	}
	return &cliente{url: strings.TrimSuffix(a.URL, "/"), token: a.Token,
		http: &http.Client{Timeout: 0}}, nil
}

func (c *cliente) peticion(ctx context.Context, metodo, ruta string, cuerpo any) (*http.Request, error) {
	var r io.Reader
	if cuerpo != nil {
		b, err := json.Marshal(cuerpo)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, metodo, c.url+ruta, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if cuerpo != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// llamar hace una petición JSON y decodifica la respuesta en out.
func (c *cliente) llamar(ctx context.Context, metodo, ruta string, cuerpo, out any) error {
	req, err := c.peticion(ctx, metodo, ruta, cuerpo)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
			if e.Message == "" {
				e.Message = resp.Status
			}
		}
		return errors.New(e.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// sandbox es lo que devuelve el frontal. Se declara aquí y no se importa de
// internal/frontal a propósito: el cliente habla con un frontal que puede ser de
// otra versión, y atarlo al tipo de dentro invitaría a romperlo sin querer.
type sandbox struct {
	ID        string     `json:"id"`
	Host      string     `json:"host"`
	Template  string     `json:"template,omitempty"`
	Image     string     `json:"image,omitempty"`
	State     string     `json:"state"`
	Egress    string     `json:"egress,omitempty"`
	OnTTL     string     `json:"on_ttl"`
	TTL       int        `json:"ttl_seconds"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func ctxSenales() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func sbxNew(args []string) error {
	fs := flag.NewFlagSet("sbx new", flag.ExitOnError)
	tpl := fs.String("template", "", "template the sandbox is born from")
	img := fs.String("image", "", "image, when there is no template")
	ttl := fs.Duration("ttl", 0, "idle time before it is frozen (or destroyed with -on-ttl remove)")
	onTTL := fs.String("on-ttl", "", "freeze (default) or remove")
	egress := fs.String("egress", "", "none (default) | internet | allowlist")
	allow := fs.String("allow", "", "domains allowed with -egress allowlist")
	mem := fs.Int("mem", 0, "memory in MiB")
	quiet := fs.Bool("q", false, "print only the id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	cuerpo := map[string]any{}
	if *tpl != "" {
		cuerpo["template"] = *tpl
	}
	if *img != "" {
		cuerpo["image"] = *img
	}
	if *ttl > 0 {
		cuerpo["ttl_seconds"] = int(ttl.Seconds())
	}
	if *onTTL != "" {
		cuerpo["on_ttl"] = *onTTL
	}
	if *egress != "" {
		cuerpo["egress"] = *egress
	}
	if *allow != "" {
		cuerpo["allow_domains"] = strings.Split(*allow, ",")
	}
	if *mem > 0 {
		cuerpo["mem_mib"] = *mem
	}

	ctx, stop := ctxSenales()
	defer stop()
	inicio := time.Now()
	var sb sandbox
	if err := c.llamar(ctx, http.MethodPost, "/v1/sandboxes", cuerpo, &sb); err != nil {
		return err
	}
	if *quiet {
		fmt.Println(sb.ID)
		return nil
	}
	fmt.Printf("%s  ready in %s on %s (%s)\n", sb.ID, time.Since(inicio).Round(time.Millisecond), sb.Host, fuente(sb))
	fmt.Printf("\n  kling sbx exec %s -- uname -a\n  kling sbx rm %s\n", sb.ID, sb.ID)
	return nil
}

func fuente(sb sandbox) string {
	if sb.Template != "" {
		return "template " + sb.Template
	}
	return "image " + sb.Image
}

func sbxLs(args []string) error {
	fs := flag.NewFlagSet("sbx ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()
	var lista []sandbox
	if err := c.llamar(ctx, http.MethodGet, "/v1/sandboxes", nil, &lista); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(lista)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tSOURCE\tSTATE\tEGRESS\tON TTL\tIN")
	for _, sb := range lista {
		queda := "—"
		if sb.ExpiresAt != nil {
			d := time.Until(*sb.ExpiresAt).Round(time.Second)
			if d < 0 {
				d = 0
			}
			queda = d.String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", sb.ID, fuente(sb), sb.State, sb.Egress, sb.OnTTL, queda)
	}
	return tw.Flush()
}

func sbxRm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kling sbx rm <id>...")
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()
	var fallos int
	for _, id := range args {
		if err := c.llamar(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			fallos++
			continue
		}
		fmt.Println(id)
	}
	if fallos > 0 {
		return fmt.Errorf("%d sandbox(es) not removed", fallos)
	}
	return nil
}

func sbxRenew(args []string) error {
	fs := flag.NewFlagSet("sbx renew", flag.ExitOnError)
	ttl := fs.Duration("ttl", 0, "new idle time from now")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kling sbx renew <id> [-ttl 30m]")
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()
	cuerpo := map[string]any{}
	if *ttl > 0 {
		cuerpo["ttl_seconds"] = int(ttl.Seconds())
	}
	var sb sandbox
	if err := c.llamar(ctx, http.MethodPost, "/v1/sandboxes/"+fs.Arg(0)+"/renew", cuerpo, &sb); err != nil {
		return err
	}
	if sb.ExpiresAt != nil {
		fmt.Printf("%s  %s at %s\n", sb.ID, map[bool]string{true: "freezes", false: "expires"}[sb.OnTTL == "freeze"],
			sb.ExpiresAt.Local().Format("15:04:05"))
	}
	return nil
}

func sbxHosts(args []string) error {
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()
	var lista []struct {
		Nombre       string `json:"name"`
		Endpoint     string `json:"endpoint"`
		Vivo         bool   `json:"alive"`
		Version      string `json:"version"`
		Maquinas     int    `json:"machines"`
		DisponibleMB int64  `json:"available_mib"`
		Error        string `json:"error"`
	}
	if err := c.llamar(ctx, http.MethodGet, "/v1/hosts", nil, &lista); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENDPOINT\tSTATE\tVERSION\tMACHINES\tAVAILABLE")
	for _, h := range lista {
		estado := "✓ alive"
		if !h.Vivo {
			estado = "✗ " + h.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d MiB\n", h.Nombre, h.Endpoint, estado, h.Version, h.Maquinas, h.DisponibleMB)
	}
	return tw.Flush()
}

// sbxExec manda un comando y escribe la salida según llega. Termina con el
// código del comando remoto, igual que `kling exec`.
func sbxExec(args []string) error {
	fs := flag.NewFlagSet("sbx exec", flag.ExitOnError)
	timeout := fs.Duration("timeout", 0, "kill the command after this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("usage: kling sbx exec <id> [--] <cmd> [args...]")
	}
	id, cmd := rest[0], rest[1:]
	if cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return errors.New("missing command")
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	cuerpo := map[string]any{"cmd": cmd}
	if *timeout > 0 {
		cuerpo["timeout_seconds"] = int(timeout.Seconds())
	}

	ctx, stop := ctxSenales()
	defer stop()
	req, err := c.peticion(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/exec", cuerpo)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return errors.New(e.Message)
		}
		return errors.New(strings.TrimSpace(string(b)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		linea := bytes.TrimSpace(sc.Bytes())
		if len(linea) == 0 {
			continue
		}
		var ev struct {
			Stream string `json:"stream"`
			Data   []byte `json:"data"`
			Exit   *int   `json:"exit"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(linea, &ev); err != nil {
			return fmt.Errorf("unreadable event from the gateway: %w", err)
		}
		switch {
		case ev.Error != "":
			return errors.New(ev.Error)
		case ev.Exit != nil:
			if *ev.Exit != 0 {
				// El código del comando remoto se propaga como código de salida
				// de kling, igual que en `kling exec`.
				os.Exit(*ev.Exit)
			}
			return nil
		case ev.Stream == "stderr":
			os.Stderr.Write(ev.Data)
		default:
			os.Stdout.Write(ev.Data)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the exec stream ended without an exit code")
}

// sbxCp sube o baja un fichero. Un lado es <id>:<ruta>.
func sbxCp(args []string) error {
	fs := flag.NewFlagSet("sbx cp", flag.ExitOnError)
	mode := fs.String("mode", "", "permissions inside the sandbox (octal)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: kling sbx cp <local|-> <id>:<path>   |   kling sbx cp <id>:<path> <local|->")
	}
	origen, destino := fs.Arg(0), fs.Arg(1)
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	ctx, stop := ctxSenales()
	defer stop()

	if id, ruta, ok := partirRemoto(destino); ok {
		var r io.Reader = os.Stdin
		if origen != "-" {
			f, err := os.Open(origen)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		q := "?path=" + ruta
		if *mode != "" {
			q += "&mode=" + *mode
		}
		req, err := c.peticion(ctx, http.MethodPut, "/v1/sandboxes/"+id+"/files"+q, nil)
		if err != nil {
			return err
		}
		req.Body = io.NopCloser(r)
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return errors.New(strings.TrimSpace(string(b)))
		}
		return nil
	}

	id, ruta, ok := partirRemoto(origen)
	if !ok {
		return errors.New("exactly one side has to be <id>:<path>")
	}
	req, err := c.peticion(ctx, http.MethodGet, "/v1/sandboxes/"+id+"/files?path="+ruta, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return errors.New(strings.TrimSpace(string(b)))
	}
	var w io.Writer = os.Stdout
	if destino != "-" {
		f, err := os.Create(destino)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// partirRemoto separa "id:/ruta". El id del frontal lleva una barra
// (host/máquina), así que se corta por el ÚLTIMO ':' antes de la ruta.
func partirRemoto(s string) (id, ruta string, ok bool) {
	if s == "-" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") {
		return "", "", false
	}
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
