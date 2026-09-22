package frontal

// Passthrough de exec y ficheros. El frontal no interpreta lo que corre
// dentro: comprueba que el sandbox es del tenant y deja pasar el flujo del
// daemon tal cual, con los mismos topes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// handleExec sirve POST /v1/sandboxes/{id}/exec: NDJSON de api.ExecEvent según
// llega del daemon, o con ?wait=1 un api.ExecResult al terminar.
func (s *Servidor) handleExec(w http.ResponseWriter, r *http.Request, id string) {
	var req api.ExecRequest
	// El stdin viaja en base64 (4/3 del tamaño) y con su sobre JSON: mismo
	// tope que aplica el daemon.
	if r.ContentLength > 2*api.ExecMaxStdin+64<<10 {
		fail(w, http.StatusRequestEntityTooLarge, errors.New("exec body is too large"))
		return
	}
	b, err := api.LeerCuerpo(r.Body, 2*api.ExecMaxStdin+64<<10)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := json.Unmarshal(b, &req); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	// Se valida aquí para contestar 400 antes de molestar al daemon, y para
	// no fiarnos de que el daemon lo haga.
	if _, _, err := req.Limits(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	t := tenantDe(r)
	h, mc, err := s.buscar(r.Context(), t, id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}

	if r.URL.Query().Get("wait") == "1" {
		res, err := h.Cliente.Exec(r.Context(), mc.ID, req, nil)
		if err != nil {
			fail(w, codigoExec(err), err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	// Streaming de verdad: cada evento se escribe y se vacía al momento. Las
	// cabeceras se escriben al PRIMER evento, no antes: si el daemon rechaza
	// la petición (404, 409, 504...) todavía se puede contestar con un código.
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	empezado := false
	emitir := func(ev api.ExecEvent) error {
		if !empezado {
			empezado = true
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
		}
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	res, err := h.Cliente.Exec(r.Context(), mc.ID, req, func(stream string, data []byte) error {
		return emitir(api.ExecEvent{Stream: stream, Data: data})
	})
	switch {
	case err != nil && !empezado:
		fail(w, codigoExec(err), err)
	case err != nil:
		// Ya se contestó 200: solo queda decirlo en el flujo, como hace el daemon.
		_ = emitir(api.ExecEvent{Error: err.Error()})
	default:
		code := res.ExitCode
		_ = emitir(api.ExecEvent{Exit: &code, DurationMS: res.DurationMS, Truncated: res.Truncated, TimedOut: res.TimedOut})
	}
}

// codigoExec: un ExecError es que el flujo se cortó (502); un StatusError trae
// el código del daemon; lo demás es que no se pudo hablar con el host.
func codigoExec(err error) int {
	var ee *api.ExecError
	if errors.As(err, &ee) {
		return http.StatusBadGateway
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return codigoDeHost(err)
}

// handleFiles sirve GET|PUT|DELETE /v1/sandboxes/{id}/files?path=...
func (s *Servidor) handleFiles(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		fail(w, http.StatusBadRequest, errors.New("missing path"))
		return
	}
	// Los topes de subida se comprueban ANTES de resolver el sandbox: no hay
	// por qué gastar una llamada al daemon en algo que se va a rechazar.
	var modo uint64
	if r.Method == http.MethodPut {
		if r.ContentLength > api.FileMaxUpload {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("upload is %d bytes; the limit is %d", r.ContentLength, api.FileMaxUpload))
			return
		}
		if m := q.Get("mode"); m != "" {
			var err error
			if modo, err = strconv.ParseUint(m, 8, 32); err != nil {
				fail(w, http.StatusBadRequest, fmt.Errorf("invalid mode %q: octal like 0644", m))
				return
			}
		}
		// Subir 64 MiB por una red lenta puede pasar del plazo de lectura del
		// servidor, pensado para JSON.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Minute))
	}

	h, mc, err := s.buscar(r.Context(), tenantDe(r), id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	cl := h.Cliente
	switch r.Method {
	case http.MethodGet:
		if q.Get("stat") == "1" {
			st, err := cl.StatFile(r.Context(), mc.ID, path)
			if err != nil {
				fail(w, http.StatusBadGateway, err)
				return
			}
			writeJSON(w, http.StatusOK, st)
			return
		}
		rc, err := cl.ReadFile(r.Context(), mc.ID, path)
		if err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		// Tope también aquí: el daemon ya lo aplica, pero el frontal no debe
		// depender de que sea así.
		_, _ = io.Copy(w, io.LimitReader(rc, api.FileMaxDownload))

	case http.MethodPut:
		// No se trunca en silencio: si el cuerpo pasa del tope sin haber
		// anunciado Content-Length, el lector falla y la subida falla con él.
		st, err := cl.WriteFile(r.Context(), mc.ID, path, uint32(modo), q.Get("mkdir") == "1",
			&lectorAcotado{r: r.Body, quedan: api.FileMaxUpload})
		if err != nil {
			var ea *errAcotado
			if errors.As(err, &ea) {
				fail(w, http.StatusRequestEntityTooLarge, err)
				return
			}
			fail(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, st)

	case http.MethodDelete:
		if err := cl.RemoveFile(r.Context(), mc.ID, path); err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// lectorAcotado lee hasta un máximo y FALLA si hay más, en vez de cortar.
type lectorAcotado struct {
	r      io.Reader
	quedan int64
}

type errAcotado struct{ max int64 }

func (e *errAcotado) Error() string {
	return fmt.Sprintf("upload exceeds the %d-byte limit", e.max)
}

func (l *lectorAcotado) Read(p []byte) (int, error) {
	if l.quedan < 0 {
		return 0, &errAcotado{api.FileMaxUpload}
	}
	if int64(len(p)) > l.quedan+1 {
		p = p[:l.quedan+1]
	}
	n, err := l.r.Read(p)
	l.quedan -= int64(n)
	if l.quedan < 0 {
		return n - 1, &errAcotado{api.FileMaxUpload}
	}
	return n, err
}
