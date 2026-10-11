//go:build !linux && !darwin

// term_other: TUI support needs the termios ioctls; elsewhere Probe
// reports false and the plain REPL is used instead.
package terminal

import (
	"errors"
	"os"
)

// Terminal is a stub on platforms without termios support.
type Terminal struct{}

// Probe always reports false here.
func Probe(f *os.File) bool { return false }

// Open is unavailable on this platform.
func Open() (*Terminal, error) { return nil, errors.New("terminal support unavailable on this platform") }

// Restore is a no-op.
func (t *Terminal) Restore() {}

// Size returns a default.
func (t *Terminal) Size() (int, int) { return 80, 24 }
