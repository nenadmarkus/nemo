//go:build linux || darwin

// term_unix: raw-mode tty control via stdlib ioctl — no dependencies.
package terminal

import (
	"os"
	"syscall"
	"unsafe"
)

// Terminal owns the tty: raw mode, hidden cursor, bracketed paste.
type Terminal struct {
	f        *os.File
	orig     syscall.Termios
	restored bool
}

type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// Probe reports whether f is a terminal: the termios ioctl succeeds only
// on real ttys (unlike a ModeCharDevice stat check, which /dev/null too).
func Probe(f *os.File) bool {
	var t syscall.Termios
	return ioctl(f.Fd(), reqReadTermios, unsafe.Pointer(&t)) == nil
}

// Open puts stdin in raw mode and prepares the screen (cursor hidden,
// bracketed paste on). Restore must be called when done; it is idempotent.
func Open() (*Terminal, error) {
	f := os.Stdin
	var t syscall.Termios
	if err := ioctl(f.Fd(), reqReadTermios, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	orig := t
	// cfmakeraw equivalent.
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(f.Fd(), reqWriteTermios, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	os.Stdout.WriteString("\x1b[?25l\x1b[?2004h")
	return &Terminal{f: f, orig: orig}, nil
}

// Restore undoes Open; safe to call more than once and from a panic
// unwinder (the deferred call covers both).
func (t *Terminal) Restore() {
	if t == nil || t.restored {
		return
	}
	t.restored = true
	ioctl(t.f.Fd(), reqWriteTermios, unsafe.Pointer(&t.orig))
	os.Stdout.WriteString("\x1b[?2004l\x1b[?25h\x1b[0m")
}

// Size returns the terminal dimensions, 80x24 when unqueryable.
func (t *Terminal) Size() (int, int) {
	var ws winsize
	if ioctl(t.f.Fd(), uintptr(syscall.TIOCGWINSZ), unsafe.Pointer(&ws)) != nil || ws.Col == 0 {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}
