package terminal

// render.go: ANSI helpers and the owned-tail painter. The screen is split
// into finalized transcript lines (printed and forgotten — scrollback
// belongs to the terminal) and an owned tail that is repainted with
// clear-line / cursor-up on each tick. Every visible line is hard-wrapped
// to width-1 so a line can never spill and auto-scroll the screen.

import (
	"fmt"
	"io"
	"strings"
)

// Painter renders the owned tail of the screen.
type Painter struct {
	w     io.Writer
	owned int // lines painted last frame
}

// NewPainter wraps a writer (stdout).
func NewPainter(w io.Writer) *Painter { return &Painter{w: w} }

// clearSeq erases the owned lines and parks the cursor at the start of
// the top one.
func clearSeq(owned int) string {
	var b strings.Builder
	b.WriteString("\r\x1b[2K")
	for i := 1; i < owned; i++ {
		b.WriteString("\x1b[1A\x1b[2K")
	}
	return b.String()
}

// Finalize flushes lines into scrollback, below any owned region.
func (p *Painter) Finalize(lines []string) {
	var b strings.Builder
	b.WriteString(clearSeq(p.owned))
	p.owned = 0
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	io.WriteString(p.w, b.String())
}

// Repaint redraws the owned region; the cursor is left hidden at the end
// of the last line (see Cursor).
func (p *Painter) Repaint(lines []string) {
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	b.WriteString(clearSeq(p.owned))
	p.owned = len(lines)
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l)
	}
	io.WriteString(p.w, b.String())
}

// ClearScreen erases the whole screen, homes the cursor, and restarts
// the owned region at the top line (Ctrl-L: transcript gone, prompt
// stays).
func (p *Painter) ClearScreen() {
	io.WriteString(p.w, "\x1b[H\x1b[2J")
	p.owned = 0
}

// Cursor shows the cursor at row/col (0-based; col in display cells)
// within the owned region. Must follow a Repaint.
func (p *Painter) Cursor(row, col int) {
	var b strings.Builder
	b.WriteString("\r")
	if up := p.owned - 1 - row; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	if col > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", col)
	}
	b.WriteString("\x1b[?25h")
	io.WriteString(p.w, b.String())
}

// WrapStyled hard-wraps s — which may contain ANSI escape sequences — so
// every output line occupies at most width cells. Escapes are zero-width;
// an SGR sequence active at a wrap point is re-emitted at the start of the
// next line, and a reset clears the carry.
func WrapStyled(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	var line, sgr []byte
	cells := 0
	for rest := s; rest != ""; {
		if rest[0] == 0x1b {
			seq := escSeq(rest)
			line = append(line, seq...)
			if len(seq) > 0 && seq[len(seq)-1] == 'm' {
				if string(seq) == "\x1b[0m" {
					sgr = sgr[:0]
				} else {
					sgr = append(sgr, seq...)
				}
			}
			rest = rest[len(seq):]
			continue
		}
		cluster, w, r := Next(rest)
		if w > 0 && cells+w > width {
			out = append(out, string(line))
			line = append(line[:0], sgr...)
			cells = 0
		}
		line = append(line, cluster...)
		cells += w
		rest = r
	}
	return append(out, string(line))
}

// escSeq returns the complete escape sequence at the head of s (s must
// begin with ESC); an unterminated sequence consumes the rest of s.
func escSeq(s string) string {
	if len(s) == 1 {
		return s
	}
	switch s[1] {
	case '[':
		i := 2
		for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
			i++
		}
		if i < len(s) {
			i++
		}
		return s[:i]
	case 'P', 'X', '^', '_': // string sequences: ST- or BEL-terminated
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return s[:i+1]
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return s[:i+2]
			}
		}
		return s
	}
	return s[:2]
}
