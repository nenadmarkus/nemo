//go:build darwin

package terminal

import "syscall"

const (
	reqReadTermios  = syscall.TIOCGETA
	reqWriteTermios = syscall.TIOCSETA
)
