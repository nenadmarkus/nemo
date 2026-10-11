package terminal

// editor.go: a multi-line line editor — rune buffer with embedded '\n',
// cursor, in-memory history. Key mutates; Render is pure. Cursor math is
// in display cells (wide runes count 2, combining marks 0), so CJK and
// emoji behave. Tab completion goes through Completer; the candidate list
// stays in state for the app to render as a ghost line.

import (
	"strconv"
	"unicode"
	"unicode/utf8"
)

const (
	histCap      = 500 // history entries kept in memory
	editorWindow = 12  // max editor lines shown (windowed around the cursor)
)

// Editor is one editable prompt.
type Editor struct {
	buf       []rune
	cur       int // rune index
	history   []string
	hIdx      int // len(history) = live draft
	draft     string
	paste     bool
	pending   []string // completion candidates (suffixes) for the ghost line
	pendTok   string   // the token the candidates were computed for
	completer Completer
}

// NewEditor builds an editor around a completer (nil disables Tab).
func NewEditor(c Completer) *Editor { return &Editor{completer: c} }

// Insert inserts s at the cursor; tabs become spaces, control runes other
// than newline are dropped.
func (e *Editor) Insert(s string) {
	for _, r := range s {
		switch {
		case r == '\t':
			r = ' '
		case r != '\n' && r < ' ':
			continue
		}
		e.insertRune(r)
	}
}

func (e *Editor) insertRune(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cur+1:], e.buf[e.cur:])
	e.buf[e.cur] = r
	e.cur++
}

// Text returns the whole buffer.
func (e *Editor) Text() string { return string(e.buf) }

// Empty reports whether the buffer holds only whitespace.
func (e *Editor) Empty() bool {
	for _, r := range e.buf {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// Reset clears the buffer after a submit and drops the ghost list.
func (e *Editor) Reset() {
	e.buf = nil
	e.cur = 0
	e.pending = nil
	e.hIdx = len(e.history)
}

// Add records a submitted line in history (consecutive dedupe, capped).
func (e *Editor) Add(line string) {
	if n := len(e.history); n == 0 || e.history[n-1] != line {
		e.history = append(e.history, line)
		if len(e.history) > histCap {
			e.history = e.history[len(e.history)-histCap:]
		}
	}
	e.hIdx = len(e.history)
}

// Pending returns the current completion candidates for the ghost
// line, reconstructed to full continuations (typed token + suffix) so
// they read like real names, shell-style.
func (e *Editor) Pending() []string {
	if len(e.pending) == 0 {
		return nil
	}
	out := make([]string, len(e.pending))
	for i, c := range e.pending {
		out[i] = e.pendTok + c
	}
	return out
}

// Key applies one keypress, returning true when the buffer should be
// submitted (the caller reads Text, then calls Reset).
func (e *Editor) Key(k Key) bool {
	if k.Name != KeyTab {
		e.pending = nil
	}
	if e.paste {
		switch k.Name {
		case KeyEnter, KeyShiftEnter:
			e.insertRune('\n')
		case KeyTab:
			e.insertRune(' ')
		case KeyRune:
			e.Insert(string(k.R))
		case KeyBackspace:
			e.backspace()
		case KeyPasteEnd:
			e.paste = false
		}
		return false
	}
	switch k.Name {
	case KeyEnter:
		if k.Alt {
			e.insertRune('\n')
			return false
		}
		return len(e.buf) > 0
	case KeyShiftEnter:
		e.insertRune('\n')
	case KeyTab:
		e.complete()
	case KeyBackspace:
		e.backspace()
	case KeyDelete:
		if e.cur < len(e.buf) {
			e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
		}
	case KeyLeft:
		if k.Ctrl {
			e.wordBack()
		} else if e.cur > 0 {
			e.cur--
		}
	case KeyRight:
		if k.Ctrl {
			e.wordFwd()
		} else if e.cur < len(e.buf) {
			e.cur++
		}
	case KeyUp:
		e.up()
	case KeyDown:
		e.down()
	case KeyHome:
		e.cur = e.lineStart()
	case KeyEnd:
		e.cur = e.lineEnd()
	case KeyPasteStart:
		e.paste = true
	case KeyPasteEnd:
		e.paste = false
	case KeyRune:
		if k.Ctrl {
			switch k.R {
			case 'a':
				e.cur = e.lineStart()
			case 'e':
				e.cur = e.lineEnd()
			case 'h':
				e.backspace()
			case 'k':
				le := e.lineEnd()
				e.buf = append(e.buf[:e.cur], e.buf[le:]...)
			case 'u':
				ls := e.lineStart()
				e.buf = append(e.buf[:ls], e.buf[e.cur:]...)
				e.cur = ls
			case 'w':
				prev := e.cur
				e.wordBack()
				e.buf = append(e.buf[:e.cur], e.buf[prev:]...)
			case 'd':
				if e.cur < len(e.buf) {
					e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
				}
			}
		} else {
			e.insertRune(k.R)
		}
	}
	return false
}

// Render lays the editor out for a width-wide screen: "› " prompts the
// first line, continuations indent two spaces, long lines wrap. It
// returns the display lines and the cursor position (row within lines,
// column in cells).
func (e *Editor) Render(width int) (lines []string, curRow, curCol int) {
	if width < 8 {
		width = 8
	}
	const pfxW = 2
	wrapW := width - pfxW - 1 // never touch the last column
	if wrapW < 4 {
		wrapW = 4
	}
	curLogical, curOff := e.pos()
	for li, l := range splitRunes(e.buf) {
		segs, cseg, ccell := wrapTrack(string(l), wrapW, curOff, li == curLogical)
		for _, s := range segs {
			p := "  "
			if len(lines) == 0 {
				p = "› "
			}
			lines = append(lines, p+s)
		}
		if li == curLogical {
			curRow, curCol = len(lines)-len(segs)+cseg, pfxW+ccell
		}
	}
	// Window to a bounded number of lines around the cursor.
	if n := len(lines); n > editorWindow {
		start := curRow - editorWindow + 2
		if start < 0 {
			start = 0
		}
		if start > n-editorWindow {
			start = n - editorWindow
		}
		lines = append(lines[:0:0], lines[start:start+editorWindow]...)
		if start > 0 {
			lines = append([]string{"… +" + strconv.Itoa(start) + " lines above"}, lines...)
			curRow -= start - 1
		}
	}
	if curRow < 0 {
		curRow = 0
	}
	return lines, curRow, curCol
}

// wrapTrack wraps text to width cells per line, tracking where rune index
// cur lands (segment index and cell column) when track is set.
func wrapTrack(text string, width, cur int, track bool) (segs []string, seg, cell int) {
	var line []byte
	cells, runes := 0, 0
	for rest := text; rest != ""; {
		c, w, r := Next(rest)
		if w > 0 && cells+w > width {
			segs = append(segs, string(line))
			line = line[:0]
			cells = 0
		}
		if track && runes >= cur {
			seg, cell = len(segs), cells
			track = false
		}
		line = append(line, c...)
		cells += w
		runes += utf8.RuneCountInString(c)
		rest = r
	}
	if track { // cursor at the very end
		seg, cell = len(segs), cells
	}
	return append(segs, string(line)), seg, cell
}

// complete runs Tab completion for the token left of the cursor: one
// candidate inserts it, several insert the longest common prefix and keep
// the list for the ghost line.
func (e *Editor) complete() {
	if e.completer == nil {
		return
	}
	tokStart := e.cur
	for tokStart > 0 {
		if r := e.buf[tokStart-1]; r == ' ' || r == '\t' || r == '\n' {
			break
		}
		tokStart--
	}
	cands := e.completer.Complete(string(e.buf), e.cur)
	switch {
	case len(cands) == 0:
		e.pending = nil
	case len(cands) == 1:
		e.Insert(cands[0])
		e.pending = nil
	default:
		e.pendTok = string(e.buf[tokStart:e.cur])
		if lcp := commonPrefix(cands); lcp != "" {
			e.Insert(lcp)
		}
		e.pending = cands
	}
}

func commonPrefix(cands []string) string {
	if len(cands) == 0 {
		return ""
	}
	p := cands[0]
	for _, c := range cands[1:] {
		for !stringsHasPrefix(c, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

func stringsHasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// pos returns the cursor's logical row and rune offset within that row.
func (e *Editor) pos() (row, col int) {
	for i := 0; i < e.cur; i++ {
		if e.buf[i] == '\n' {
			row++
			col = 0
		} else {
			col++
		}
	}
	return
}

func (e *Editor) lineStart() int {
	i := e.cur
	for i > 0 && e.buf[i-1] != '\n' {
		i--
	}
	return i
}

func (e *Editor) lineEnd() int {
	i := e.cur
	for i < len(e.buf) && e.buf[i] != '\n' {
		i++
	}
	return i
}

func (e *Editor) backspace() {
	if e.cur > 0 {
		e.cur--
		e.buf = append(e.buf[:e.cur], e.buf[e.cur+1:]...)
	}
}

func (e *Editor) wordBack() {
	for e.cur > 0 && unicode.IsSpace(e.buf[e.cur-1]) {
		e.cur--
	}
	for e.cur > 0 && !unicode.IsSpace(e.buf[e.cur-1]) {
		e.cur--
	}
}

func (e *Editor) wordFwd() {
	for e.cur < len(e.buf) && unicode.IsSpace(e.buf[e.cur]) {
		e.cur++
	}
	for e.cur < len(e.buf) && !unicode.IsSpace(e.buf[e.cur]) {
		e.cur++
	}
}

// up moves to the previous visual line; from the first line it walks
// history instead (saving the live draft first, like shells).
func (e *Editor) up() {
	row, col := e.pos()
	if row == 0 {
		e.histUp()
		return
	}
	ls := e.lineStart()
	prevStart := 0
	// The newline ending the previous line sits at ls-1; scan past it.
	for i := ls - 2; i >= 0; i-- {
		if e.buf[i] == '\n' {
			prevStart = i + 1
			break
		}
	}
	if cur := prevStart + col; cur < ls {
		e.cur = cur
	} else {
		e.cur = ls - 1
	}
}

// down moves to the next visual line; from the last line it walks history
// forward, restoring the draft at the end.
func (e *Editor) down() {
	_, col := e.pos()
	le := e.lineEnd()
	if le >= len(e.buf) {
		e.histDown()
		return
	}
	nextStart := le + 1
	nextEnd := len(e.buf)
	for i := nextStart; i < len(e.buf); i++ {
		if e.buf[i] == '\n' {
			nextEnd = i
			break
		}
	}
	if cur := nextStart + col; cur < nextEnd {
		e.cur = cur
	} else {
		e.cur = nextEnd
	}
}

func (e *Editor) histUp() {
	if e.hIdx > 0 {
		if e.hIdx >= len(e.history) {
			e.draft = string(e.buf)
		}
		e.hIdx--
		e.buf = []rune(e.history[e.hIdx])
		e.cur = len(e.buf)
	}
}

func (e *Editor) histDown() {
	if e.hIdx < len(e.history) {
		e.hIdx++
		if e.hIdx == len(e.history) {
			e.buf = []rune(e.draft)
		} else {
			e.buf = []rune(e.history[e.hIdx])
		}
		e.cur = len(e.buf)
	}
}

func splitRunes(buf []rune) [][]rune {
	var out [][]rune
	start := 0
	for i, r := range buf {
		if r == '\n' {
			out = append(out, buf[start:i])
			start = i + 1
		}
	}
	return append(out, buf[start:])
}
