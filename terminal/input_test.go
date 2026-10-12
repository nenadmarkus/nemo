package terminal

import "testing"

// TestDecodeKeys runs the decoder over byte sequences (optionally split
// across feeds, as across read boundaries) and checks the keys emitted.
func TestDecodeKeys(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []Key
	}{
		{"rune", []string{"a"}, []Key{{Name: KeyRune, R: 'a'}}},
		{"enter", []string{"\r"}, []Key{{Name: KeyEnter}}},
		{"backspace", []string{"\x7f"}, []Key{{Name: KeyBackspace}}},
		{"ctrl-c", []string{"\x03"}, []Key{{Name: KeyRune, R: 'c', Ctrl: true}}},
		{"arrow up", []string{"\x1b[A"}, []Key{{Name: KeyUp}}},
		{"arrow split across reads", []string{"\x1b", "[A"}, []Key{{Name: KeyUp}}},
		{"ss3 right", []string{"\x1bOC"}, []Key{{Name: KeyRight}}},
		{"ctrl right", []string{"\x1b[1;5C"}, []Key{{Name: KeyRight, Ctrl: true}}},
		{"alt right", []string{"\x1b[1;3C"}, []Key{{Name: KeyRight, Alt: true}}},
		{"alt left", []string{"\x1b[1;3D"}, []Key{{Name: KeyLeft, Alt: true}}},
		{"home", []string{"\x1b[H"}, []Key{{Name: KeyHome}}},
		{"delete", []string{"\x1b[3~"}, []Key{{Name: KeyDelete}}},
		{"paste markers", []string{"\x1b[200~x\x1b[201~"}, []Key{
			{Name: KeyPasteStart}, {Name: KeyRune, R: 'x'}, {Name: KeyPasteEnd}}},
		{"alt enter", []string{"\x1b\r"}, []Key{{Name: KeyEnter, Alt: true}}},
		{"alt letter", []string{"\x1bf"}, []Key{{Name: KeyRune, R: 'f', Alt: true}}},
		{"shift enter csi-u", []string{"\x1b[13;2u"}, []Key{{Name: KeyShiftEnter, Shift: true}}},
		{"utf8 split across reads", []string{"\xe4\xb8", "\xad"}, []Key{{Name: KeyRune, R: '中'}}},
		{"lone esc flushed by timeout", []string{"\x1b"}, []Key{{Name: KeyEsc}}},
		{"unknown csi dropped", []string{"\x1b[99z"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []Key
			d := &keyDecoder{}
			for _, chunk := range tt.in {
				d.feed([]byte(chunk), func(k Key) { got = append(got, k) })
			}
			d.flush(func(k Key) { got = append(got, k) })
			if len(got) != len(tt.want) {
				t.Fatalf("got %d keys %v, want %d", len(got), got, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("key %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
