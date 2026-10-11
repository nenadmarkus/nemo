package terminal

// input.go: byte stream -> Key events. CSI/SS3 escape sequences, bracketed
// paste markers, Alt prefixes, and UTF-8 are decoded incrementally across
// read boundaries; a lone or truncated ESC is delivered after a short
// timeout (terminals send whole sequences in one write). Kitty / CSI-u
// keyboard protocols are not negotiated; xterm-style CSI-u sequences that
// arrive anyway are decoded.

import (
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// KeyName enumerates the special keys we decode; printable input arrives
// as KeyRune with R set.
type KeyName int

const (
	KeyRune KeyName = iota
	KeyEnter
	KeyShiftEnter
	KeyTab
	KeyBackspace
	KeyDelete
	KeyEsc
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
	KeyInsert
	KeyPasteStart
	KeyPasteEnd
	KeyUnknown
)

// Key is one decoded keypress; Ctrl/Alt/Shift carry the modifiers where a
// terminal reports them.
type Key struct {
	Name  KeyName
	R     rune
	Ctrl  bool
	Alt   bool
	Shift bool
}

// seqTimeout is how long a partial escape sequence waits for more bytes
// before being flushed as best-effort input.
const seqTimeout = 50 * time.Millisecond

// ReadKeys decodes r into keys delivered on out. It returns immediately;
// decoding stops when done closes or r fails (EOF).
func ReadKeys(r io.Reader, out chan<- Key, done <-chan struct{}) {
	chunks := make(chan []byte, 8)
	go func() {
		defer close(chunks)
		buf := make([]byte, 256)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				c := make([]byte, n)
				copy(c, buf[:n])
				chunks <- c
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		dec := &keyDecoder{}
		emit := func(k Key) {
			select {
			case out <- k:
			case <-done:
			}
		}
		var timer <-chan time.Time
		for {
			select {
			case <-done:
				return
			case b, ok := <-chunks:
				if !ok {
					dec.flush(emit)
					return
				}
				timer = nil
				if wait := dec.feed(b, emit); wait > 0 {
					timer = time.After(wait)
				}
			case <-timer:
				timer = nil
				dec.flush(emit)
			}
		}
	}()
}

// keyDecoder holds bytes of a sequence that is still incomplete.
type keyDecoder struct {
	pending []byte
}

// feed processes b (after any pending bytes), emitting complete keys.
// When the input ends mid-sequence it returns the timeout after which
// flush should resolve the remainder.
func (d *keyDecoder) feed(b []byte, emit func(Key)) time.Duration {
	if len(d.pending) > 0 {
		b = append(d.pending, b...)
		d.pending = nil
	}
	for len(b) > 0 {
		if b[0] == 0x1b {
			n, k, complete := decodeEscape(b)
			if !complete {
				d.pending = b
				return seqTimeout
			}
			b = b[n:]
			if k.Name != KeyUnknown {
				emit(k)
			}
			continue
		}
		if b[0] < 0x80 {
			if k, ok := plainKey(b[0]); ok {
				emit(k)
			}
			b = b[1:]
			continue
		}
		if !utf8.FullRune(b) {
			d.pending = b
			return seqTimeout
		}
		r, sz := utf8.DecodeRune(b)
		if r == utf8.RuneError && sz <= 1 {
			b = b[1:] // stray continuation byte: drop
			continue
		}
		emit(Key{Name: KeyRune, R: r})
		b = b[sz:]
	}
	return 0
}

// flush resolves a pending partial sequence: a lone or truncated ESC emits
// KeyEsc and its remainder decodes as plain input; a partial UTF-8 rune is
// dropped.
func (d *keyDecoder) flush(emit func(Key)) {
	b := d.pending
	d.pending = nil
	for len(b) > 0 {
		if b[0] == 0x1b {
			emit(Key{Name: KeyEsc})
			b = b[1:]
			continue
		}
		if b[0] >= 0x80 {
			n := 1
			for n < len(b) && b[n]&0xc0 == 0x80 {
				n++
			}
			b = b[n:]
			continue
		}
		if k, ok := plainKey(b[0]); ok {
			emit(k)
		}
		b = b[1:]
	}
}

// plainKey maps one ASCII byte to a key.
func plainKey(c byte) (Key, bool) {
	switch {
	case c == '\r' || c == '\n':
		return Key{Name: KeyEnter}, true
	case c == '\t':
		return Key{Name: KeyTab}, true
	case c == 0x7f:
		return Key{Name: KeyBackspace}, true
	case c == 0:
		return Key{Name: KeyRune, R: ' ', Ctrl: true}, true // Ctrl+Space
	case c >= 0x01 && c <= 0x1a:
		return Key{Name: KeyRune, R: rune('a' + c - 1), Ctrl: true}, true
	case c >= 0x20 && c < 0x7f:
		return Key{Name: KeyRune, R: rune(c)}, true
	default: // 0x1c..0x1f
		return Key{}, false
	}
}

// decodeEscape decodes one escape sequence at the head of b. It returns
// the bytes consumed, the key, and whether the sequence was complete.
func decodeEscape(b []byte) (int, Key, bool) {
	if len(b) < 2 {
		return 0, Key{}, false
	}
	switch b[1] {
	case '[': // CSI
		i := 2
		for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f { // parameters
			i++
		}
		for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f { // intermediates
			i++
		}
		if i >= len(b) {
			return 0, Key{}, false
		}
		return i + 1, csiKey(string(b[2:i]), b[i]), true
	case 'O': // SS3
		if len(b) < 3 {
			return 0, Key{}, false
		}
		return 3, ss3Key(b[2]), true
	case '\r', '\n':
		return 2, Key{Name: KeyEnter, Alt: true}, true
	case 0x1b:
		return 1, Key{Name: KeyEsc}, true // ESC ESC: deliver first, restate
	}
	if b[1] >= 0x80 { // Alt + non-ASCII rune
		if !utf8.FullRune(b[1:]) {
			return 0, Key{}, false
		}
		r, sz := utf8.DecodeRune(b[1:])
		return 1 + sz, Key{Name: KeyRune, R: r, Alt: true}, true
	}
	if k, ok := plainKey(b[1]); ok {
		k.Alt = true
		return 2, k, true
	}
	return 1, Key{Name: KeyEsc}, true
}

// csiKey interprets a CSI sequence's parameter string and final byte.
func csiKey(params string, final byte) Key {
	ps := strings.Split(params, ";")
	num := make([]int, len(ps))
	for i, p := range ps {
		n, _ := strconv.Atoi(p)
		num[i] = n
	}
	mod := 0
	if len(num) >= 2 && num[1] >= 2 {
		mod = num[1] - 1 // xterm modifier encoding: 1=shift 2=alt 4=ctrl
	}
	k := Key{Shift: mod&1 != 0, Alt: mod&2 != 0, Ctrl: mod&4 != 0}
	switch final {
	case 'A':
		k.Name = KeyUp
	case 'B':
		k.Name = KeyDown
	case 'C':
		k.Name = KeyRight
	case 'D':
		k.Name = KeyLeft
	case 'H':
		k.Name = KeyHome
	case 'F':
		k.Name = KeyEnd
	case '~':
		switch num[0] {
		case 200:
			k.Name = KeyPasteStart
		case 201:
			k.Name = KeyPasteEnd
		case 1, 7:
			k.Name = KeyHome
		case 2:
			k.Name = KeyInsert
		case 3:
			k.Name = KeyDelete
		case 4, 8:
			k.Name = KeyEnd
		case 5:
			k.Name = KeyPgUp
		case 6:
			k.Name = KeyPgDn
		default:
			k.Name = KeyUnknown
		}
	case 'u': // CSI-u (xterm modifyOtherKeys / kitty when enabled upstream)
		switch num[0] {
		case 13:
			if k.Shift {
				k.Name = KeyShiftEnter
			} else {
				k.Name = KeyEnter
			}
		case 9:
			k.Name = KeyTab
		case 127:
			k.Name = KeyBackspace
		default:
			k.Name = KeyUnknown
		}
	default:
		k.Name = KeyUnknown
	}
	return k
}

// ss3Key interprets an SS3 (ESC O x) sequence.
func ss3Key(c byte) Key {
	switch c {
	case 'A':
		return Key{Name: KeyUp}
	case 'B':
		return Key{Name: KeyDown}
	case 'C':
		return Key{Name: KeyRight}
	case 'D':
		return Key{Name: KeyLeft}
	case 'H':
		return Key{Name: KeyHome}
	case 'F':
		return Key{Name: KeyEnd}
	}
	return Key{Name: KeyUnknown}
}
