package terminal

import "testing"

type fakeCompleter struct{ cands []string }

func (f fakeCompleter) Complete(line string, cursor int) []string { return f.cands }

func TestEditorBasics(t *testing.T) {
	e := NewEditor(nil)
	e.Insert("hello")
	e.Key(Key{Name: KeyLeft})
	e.Key(Key{Name: KeyBackspace})
	if got := e.Text(); got != "helo" {
		t.Fatalf("text = %q, want %q", got, "helo")
	}
	if !e.Key(Key{Name: KeyEnter}) {
		t.Fatal("enter should submit a non-empty buffer")
	}
	e.Reset()
	if !e.Empty() || e.Key(Key{Name: KeyEnter}) {
		t.Fatal("empty buffer must not submit")
	}
}

func TestEditorMultiline(t *testing.T) {
	e := NewEditor(nil)
	e.Insert("a")
	e.Key(Key{Name: KeyShiftEnter})
	e.Insert("b")
	if got := e.Text(); got != "a\nb" {
		t.Fatalf("text = %q, want %q", got, "a\nb")
	}
	e.Key(Key{Name: KeyUp}) // visual line up, not history
	lines, row, col := e.Render(80)
	if len(lines) != 2 || lines[0] != "› a" || lines[1] != "  b" {
		t.Fatalf("lines = %q", lines)
	}
	if row != 0 || col != 3 {
		t.Fatalf("cursor = %d,%d, want 0,3", row, col)
	}
}

func TestEditorHistoryDraft(t *testing.T) {
	e := NewEditor(nil)
	e.Add("one")
	e.Add("two")
	e.Insert("draft")
	e.Key(Key{Name: KeyUp}) // first row -> history up
	if got := e.Text(); got != "two" {
		t.Fatalf("after up = %q, want %q", got, "two")
	}
	e.Key(Key{Name: KeyUp})
	if got := e.Text(); got != "one" {
		t.Fatalf("after up up = %q, want %q", got, "one")
	}
	e.Key(Key{Name: KeyDown})
	e.Key(Key{Name: KeyDown}) // past the end: draft restored
	if got := e.Text(); got != "draft" {
		t.Fatalf("after down down = %q, want %q", got, "draft")
	}
}

func TestEditorWideCursor(t *testing.T) {
	e := NewEditor(nil)
	e.Insert("世界")
	_, row, col := e.Render(80)
	if row != 0 || col != 6 { // prompt 2 + 4 cells
		t.Fatalf("cursor = %d,%d, want 0,6", row, col)
	}
}

func TestEditorCompletion(t *testing.T) {
	e := NewEditor(fakeCompleter{[]string{"c/", "cc/"}})
	e.Insert("sr")
	e.Key(Key{Name: KeyTab}) // multiple: insert lcp "c", keep ghost list
	if got := e.Text(); got != "src" {
		t.Fatalf("text = %q, want %q", got, "src")
	}
	if len(e.Pending()) != 2 {
		t.Fatalf("pending = %v", e.Pending())
	}
	// The ghost shows full continuations, not raw suffixes.
	if got := e.Pending(); got[0] != "src/" || got[1] != "srcc/" {
		t.Fatalf("pending = %v, want [src/ srcc/]", got)
	}
	e.Key(Key{Name: KeyRune, R: 'x'}) // any other key clears the ghost
	if len(e.Pending()) != 0 {
		t.Fatal("pending should clear on non-tab key")
	}

	e2 := NewEditor(fakeCompleter{[]string{"c/"}})
	e2.Insert("sr")
	e2.Key(Key{Name: KeyTab}) // single: insert it
	if got := e2.Text(); got != "src/" {
		t.Fatalf("text = %q, want %q", got, "src/")
	}
}

func TestEditorWrapWidth(t *testing.T) {
	e := NewEditor(nil)
	e.Insert("0123456789012345678901234567890123456789")
	lines, _, _ := e.Render(30)
	for _, l := range lines {
		if w := String(l); w > 29 {
			t.Errorf("line %q occupies %d cells, want <= 29", l, w)
		}
	}
}
