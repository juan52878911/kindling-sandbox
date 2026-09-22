//go:build linux

package main

// Terminal local en Linux, solo con la biblioteca estándar.
//
// Es una copia del mismo fichero de kling (cmd/kling/term_linux.go), y lo es a
// propósito: son 90 líneas de ioctl que no cambian nunca, y compartirlas
// exigiría exportarlas desde el núcleo como API pública para siempre. Copiarlas
// cuesta menos que mantener esa promesa.
//
// No se usa golang.org/x/term ni se ejecuta `stty`: kling no tiene
// dependencias externas y no quiere depender de un binario que puede faltar en
// la imagen desde la que se llama. Son tres ioctl: leer termios, escribirlo y
// leer el tamaño de la ventana. Este fichero y term_darwin.go son casi iguales
// pero no comparten código a propósito: los campos de Termios son uint32 aquí y
// uint64 en darwin, y los nombres de los ioctl cambian (TCGETS/TCSETS frente a
// TIOCGETA/TIOCSETA).

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
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		return t, e
	}
	return t, nil
}

func termiosSet(fd uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

// isTerminal dice si fd es una terminal: la prueba es que acepte TCGETS.
func isTerminal(fd uintptr) bool {
	_, err := termiosGet(fd)
	return err == nil
}

// makeRaw pone la terminal en modo crudo y devuelve cómo dejarla como estaba.
//
// Lo importante es ISIG apagado: Ctrl-C tiene que viajar como el byte 0x03 y
// ser el PTY de dentro quien mate el proceso remoto, no el kernel local quien
// mate a kling. ICANON y ECHO apagados para que cada tecla se mande al momento
// y sea el otro lado quien la pinte; OPOST apagado para que la salida remota
// llegue tal cual (el PTY remoto ya hizo su \n → \r\n). VMIN=1/VTIME=0 es "un
// byte en cuanto llegue".
//
// restore puede llamarse varias veces y desde cualquier goroutine: se llama
// desde un defer, desde el recover de un pánico y desde el manejador de
// señales, y solo la primera vez hace algo.
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
