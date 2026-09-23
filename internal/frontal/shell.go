package frontal

// Relé de la shell interactiva (protocolo kling-shell/1, ver pkg/api/shell.go).
//
// El frontal hace con el daemon lo que el daemon hace con el invitado: recibe
// un Upgrade, abre otro contra el daemon con api.Client.Shell y se queda en
// medio pasando tramas. En medio y no de paso: cada trama se decodifica, se
// comprueba que su tipo tiene sentido en esa dirección y se vuelve a escribir.
// Del cliente solo entran data, resize y signal; del daemon solo salen data,
// exit y error —y ping, que el protocolo define como "del daemon al cliente"
// para detectar conexiones muertas y que el CLI ya entiende—. Cualquier otra
// cosa del daemon corta la sesión con un error explícito: si el daemon (o lo
// que se haga pasar por él) manda un resize hacia fuera, algo va mal y no se
// le da un canal hacia el terminal de quien llama.
//
// Un cliente que manda lo que no debe no tumba la sesión: se ignora, igual
// que hace el daemon. La asimetría es a propósito: quien está detrás del
// frontal es un agente que se equivoca; lo que viene del otro lado es lo que
// el sandbox produjo, y se considera hostil.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// maxCuerpoShell acota la petición de apertura: cmd, env y tamaño inicial.
const maxCuerpoShell = 64 << 10

func (s *Servidor) handleShell(w http.ResponseWriter, r *http.Request, id string) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), api.ShellProto) {
		fail(w, http.StatusUpgradeRequired, fmt.Errorf("this endpoint speaks %s; ask for it with Upgrade", api.ShellProto))
		return
	}
	var req api.ShellRequest
	b, err := api.LeerCuerpo(r.Body, maxCuerpoShell)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &req); err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
			return
		}
	}
	t := tenantDe(r)
	h, mc, err := s.buscar(r.Context(), t, id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}

	// Cuota de shells abiertas. Se comprueba DESPUÉS de resolver el sandbox
	// (así un id ajeno sigue dando 404 y no delata la cuota de otro) y ANTES de
	// tocar el daemon: no hay por qué abrir una sesión que se va a rechazar.
	if !s.shells.abrir(t.Nombre, t.MaxShells) {
		fail(w, http.StatusTooManyRequests, fmt.Errorf(
			"tenant %s already has %d shell session(s) open; the limit is %d", t.Nombre, s.shells.cuenta(t.Nombre), t.MaxShells))
		return
	}
	defer s.shells.cerrar(t.Nombre)

	// Primero el daemon y después el secuestro: mientras la respuesta siga
	// siendo HTTP normal, un fallo se cuenta con un código y un mensaje. Tras
	// el 101 ya no hay manera de decir nada que el cliente entienda como error.
	daemon, err := h.Cliente.Shell(r.Context(), mc.ID, req)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, api.ErrShellUnsupported) {
			code = http.StatusNotImplemented
		}
		fail(w, code, fmt.Errorf("%s: %w", h.Nombre, err))
		return
	}
	// Shell avisa: cancelar el contexto NO cierra la conexión. Se cierra aquí
	// en todos los caminos.
	defer daemon.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusInternalServerError, errors.New("this server cannot take over the connection"))
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})

	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
		api.ShellProto + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}
	relayShell(conn, brw.Reader, daemon)
}

// relayShell pasa tramas entre el cliente y el daemon hasta que uno cuelga o
// el daemon manda algo que no le toca. Se lee al cliente por el lector del
// secuestro porque puede tener bytes ya leídos; se le escribe directo a la
// conexión, bajo un candado, porque escriben dos goroutines.
func relayShell(cliente net.Conn, lector *bufio.Reader, daemon *api.ShellConn) {
	var mu sync.Mutex
	alCliente := func(t byte, p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return api.WriteFrame(cliente, t, p)
	}

	fin := make(chan struct{})
	var once sync.Once
	cerrar := func() { once.Do(func() { close(fin) }) }

	// Daemon → cliente.
	go func() {
		defer cerrar()
		for {
			t, p, err := daemon.ReadFrame()
			if err != nil {
				_ = alCliente(api.ShellError, []byte("the daemon stopped answering"))
				return
			}
			switch t {
			case api.ShellData, api.ShellPing:
				if err := alCliente(t, p); err != nil {
					return
				}
			case api.ShellExit, api.ShellError:
				_ = alCliente(t, p)
				return
			default:
				_ = alCliente(api.ShellError, []byte(fmt.Sprintf("the daemon sent a frame of type %d, which it must not", t)))
				return
			}
		}
	}()

	// Cliente → daemon.
	go func() {
		defer cerrar()
		for {
			t, p, err := api.ReadFrame(lector)
			if err != nil {
				return
			}
			switch t {
			case api.ShellData, api.ShellResize, api.ShellSignal:
				if err := daemon.WriteFrame(t, p); err != nil {
					return
				}
			default:
				// Una trama que no viene a cuento no tumba la sesión: se ignora.
			}
		}
	}()

	<-fin
	// Cerrar los dos extremos es lo que desbloquea a la goroutine que siga
	// leyendo; sin esto, un cliente que no cuelga dejaría la sesión del daemon
	// abierta para siempre.
	_ = daemon.Close()
	_ = cliente.Close()
}
