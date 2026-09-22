//go:build !linux && !darwin

package main

// Plataformas sin soporte de terminal cruda. kling tiene que compilar en todas
// partes (el resto de comandos hablan HTTP y no tocan la tty), así que estas
// funciones existen pero `kling shell` se niega antes de intentar nada.

import (
	"errors"
	"os"
)

var errNoTerm = errors.New("interactive shell is not supported on this platform")

func termUnsupported() error                  { return errNoTerm }
func notifyResize(chan<- os.Signal)           {}
func isTerminal(uintptr) bool                 { return false }
func makeRaw(uintptr) (func(), error)         { return nil, errNoTerm }
func winsize(uintptr) (uint16, uint16, error) { return 0, 0, errNoTerm }
