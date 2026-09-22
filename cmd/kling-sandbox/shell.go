package main

// `kling sbx shell <id>`: una terminal dentro de un sandbox, a través del
// gateway.
//
// Es el mismo protocolo que `kling shell` (tramas sobre una conexión que cambia
// de protocolo, ver pkg/api/shell.go de kindling), con dos diferencias: al otro
// lado hay un frontal y no un daemon, así que la petición lleva token, y el
// sandbox se identifica como host/máquina.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

func sbxShell(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: kling sbx shell <id> [--] [cmd [args...]]")
	}
	id, cmd := args[0], args[1:]
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if err := termUnsupported(); err != nil {
		return 1, err
	}
	if !isTerminal(os.Stdin.Fd()) || !isTerminal(os.Stdout.Fd()) {
		return 2, errors.New("kling sbx shell needs a terminal on stdin and stdout; to run a command use kling sbx exec")
	}
	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm"
	}
	rows, cols, _ := winsize(os.Stdout.Fd())

	c, err := nuevoCliente()
	if err != nil {
		return 1, err
	}
	cuerpo, _ := json.Marshal(api.ShellRequest{Cmd: cmd, Term: term, Rows: rows, Cols: cols})

	// Sin contexto con señales: tras el 101 la conexión deja de ser del
	// http.Client y cancelar el contexto no la cierra, así que lo único que
	// conseguiría un contexto cancelable aquí es matar la petición ANTES de
	// negociar. De colgar por señal se encarga la goroutine de más abajo.
	req, err := http.NewRequest(http.MethodPost, c.url+"/v1/sandboxes/"+id+"/shell", bytes.NewReader(cuerpo))
	if err != nil {
		return 1, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", api.ShellProto)

	resp, err := c.http.Do(req)
	if err != nil {
		return 1, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return 1, errors.New(e.Message)
		}
		return 1, errors.New(strings.TrimSpace(string(b)))
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return 1, errors.New("the gateway did not hand over the connection")
	}
	defer conn.Close()

	restaurar, err := makeRaw(os.Stdin.Fd())
	if err != nil {
		return 1, fmt.Errorf("raw mode: %w", err)
	}
	defer func() {
		restaurar()
		if r := recover(); r != nil {
			panic(r)
		}
	}()

	hecho := make(chan struct{})
	defer close(hecho)
	var mu sync.Mutex
	mandar := func(t byte, p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return api.WriteFrame(conn, t, p)
	}

	// Señales de fuera: restaurar la terminal y colgar. Ctrl-C no llega por aquí
	// (ISIG está apagado), viaja como byte hasta el pseudoterminal remoto.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			restaurar()
			conn.Close()
		case <-hecho:
		}
	}()

	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-winch:
				if r, c, err := winsize(os.Stdout.Fd()); err == nil {
					_ = mandar(api.ShellResize, api.ResizePayload(r, c))
				}
			case <-hecho:
				return
			}
		}
	}()

	// No se espera a esta goroutine: un Read sobre la terminal no se desbloquea.
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if mandar(api.ShellData, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		t, p, err := api.ReadFrame(br)
		if err != nil {
			return 1, fmt.Errorf("connection to %s closed", id)
		}
		switch t {
		case api.ShellData:
			if _, err := os.Stdout.Write(p); err != nil {
				return 1, err
			}
		case api.ShellExit:
			code, err := api.ParseExit(p)
			if err != nil {
				return 1, err
			}
			return int(code), nil
		case api.ShellError:
			return 1, errors.New(string(p))
		case api.ShellPing:
		}
	}
}
