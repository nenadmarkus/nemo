//go:build linux

package terminal

import "syscall"

const (
	reqReadTermios  = syscall.TCGETS
	reqWriteTermios = syscall.TCSETS
)
