package tui

import (
	"regexp"
	"strings"
	"testing"

	"nemo/terminal"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

func visWidth(s string) int { return terminal.String(ansi.ReplaceAllString(s, "")) }

// TestMarkdownZeroTheme pins md-lite output with styling disabled: the
// structure (headers, code fences, bullets, quotes, links) must survive
// and every line must respect the wrap width.
func TestMarkdownZeroTheme(t *testing.T) {
	src := "# Title\n\nsome `code` and **bold** here\n\n- a\n- b\n\n" +
		"```go\nx := 1\n```\n\n> quote me\n\nsee [docs](https://example.com) ok\n"
	got := renderMarkdown(src, 40, theme{})
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"Title",
		"some code and bold here",
		"• a",
		"  x := 1",
		"▎ quote me",
		"see docs ok",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("output missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "#") || strings.Contains(joined, "`") ||
		strings.Contains(joined, "https://") {
		t.Errorf("markers leaked:\n%s", joined)
	}
	for _, l := range got {
		if w := visWidth(l); w > 40 {
			t.Errorf("line %q occupies %d cells, want <= 40", l, w)
		}
	}
}

func TestMarkdownWrapLongParagraph(t *testing.T) {
	got := renderMarkdown(strings.Repeat("word ", 30), 20, theme{})
	if len(got) < 7 {
		t.Fatalf("expected wrapping, got %d lines: %q", len(got), got)
	}
	for _, l := range got {
		if w := visWidth(l); w > 20 {
			t.Errorf("line %q occupies %d cells, want <= 20", l, w)
		}
	}
}

func TestRenderDiff(t *testing.T) {
	src := "[diff: main.go] (1 -> 2 lines)\n ctx\n-gone\n+new\n"
	got := renderDiff(src, 60, theme{})
	if len(got) != 4 {
		t.Fatalf("got %d lines: %q", len(got), got)
	}
	for i, want := range []string{"[diff: main.go] (1 -> 2 lines)", " ctx", "-gone", "+new"} {
		if ansistrip(got[i]) != want {
			t.Errorf("line %d = %q, want %q", i, ansistrip(got[i]), want)
		}
	}
}

func TestTailLines(t *testing.T) {
	got := tailLines("1\n2\n3\n4\n5", 3)
	want := []string{"… +2 lines above", "3", "4", "5"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func ansistrip(s string) string { return ansi.ReplaceAllString(s, "") }
