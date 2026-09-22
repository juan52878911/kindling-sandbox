//go:build darwin

package main

// Copia de cmd/kling/term_darwin.go de kindling; ver term_linux.go.
//
// Terminal local en macOS, solo con la biblioteca estándar.
//
// Gemelo de term_linux.go; ver allí el porqué de cada flag. No comparten código
// porque los campos de Termios son uint64 en darwin y uint32 en linux, y los
// ioctl se llaman TIOCGETA/TIOCSETA en vez de TCGETS/TCSETS.

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"
)

// termUnsupported dice si esta plataforma puede tener una shell interactiva.
func termUnsupported() error { return nil }

// notifyResize entrega en ch los cambios de tamaño de la terminal.
func notifyResize(ch chan<- os.Signal) { signal.Notify(ch, syscall.SIGWINCH) }

func termiosGet(fd uintptr) (syscall.Termios, error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&t))); e != 0 {
		return t, e
	}
	return t, nil
}

func termiosSet(fd uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

// isTerminal dice si fd es una terminal: la prueba es que acepte TIOCGETA.
func isTerminal(fd uintptr) bool {
	_, err := termiosGet(fd)
	return err == nil
}

// makeRaw pone la terminal en modo crudo y devuelve cómo dejarla como estaba.
// ISIG apagado para que Ctrl-C viaje como 0x03 al PTY remoto; el resto, en
// term_linux.go. restore es idempotente y seguro desde cualquier goroutine.
func makeRaw(fd uintptr) (restore func(), err error) {
	old, err := termiosGet(fd)
	if err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := termiosSet(fd, &raw); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = termiosSet(fd, &old) }) }, nil
}

// winsize lee el tamaño de la terminal. syscall no trae la struct, así que se
// declara aquí con la forma de la de la libc (cuatro uint16).
func winsize(fd uintptr) (rows, cols uint16, err error) {
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0, 0, e
	}
	if ws.Row == 0 || ws.Col == 0 {
		return 0, 0, errors.New("the terminal reports no size")
	}
	return ws.Row, ws.Col, nil
}
