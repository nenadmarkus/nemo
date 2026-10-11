package terminal

import (
	"regexp"
	"testing"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

func TestWrapStyledPlain(t *testing.T) {
	got := WrapStyled("abcdefgh", 3)
	want := []string{"abc", "def", "gh"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestWrapStyledWide(t *testing.T) {
	// Wide runes are never split; a 2-cell rune does not start when only
	// one cell remains.
	got := WrapStyled("世界世界", 3)
	want := []string{"世", "界", "世", "界"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestWrapStyledSGRCarry(t *testing.T) {
	got := WrapStyled("\x1b[31mabcdefgh", 3)
	want := []string{"\x1b[31mabc", "\x1b[31mdef", "\x1b[31mgh"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestWrapStyledWidthDiscipline(t *testing.T) {
	for _, s := range []string{
		"plain text that is quite long indeed yes",
		"\x1b[38;5;75mstyled \x1b[1mbold\x1b[0m and more text to wrap around",
		"日本語のテキストも折り返されるべきです、はい",
	} {
		for w := 4; w < 20; w++ {
			for _, l := range WrapStyled(s, w) {
				if vis := String(ansi.ReplaceAllString(l, "")); vis > w {
					t.Errorf("width %d: line %q occupies %d cells", w, l, vis)
				}
			}
		}
	}
}

func TestWidthSanity(t *testing.T) {
	if String("ab") != 2 || String("世") != 2 || String("á") != 1 {
		t.Fatal("basic widths broken")
	}
	if String("👨‍👩‍👧") != 2 { // ZWJ family: one cluster, emoji presentation
		t.Fatalf("zwj family = %d cells, want 2", String("👨‍👩‍👧"))
	}
	if Truncate("世界世界", 5, "…") != "世界…" {
		t.Fatalf("truncate = %q", Truncate("世界世界", 5, "…"))
	}
}
