// Tests for the command-level git machinery: .gitignore parsing, the
// gitFilter handed to the library's search tools, and those tools'
// behavior under it. (All of this lived in the library's tests before
// the git logic moved from nemo.go into the command.)
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nemo"
)

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

// mustWrite writes content to path, failing the test on error.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustMkdir creates dir (and parents), failing the test on error.
func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// callTool invokes a Tool handler with args.
func callTool(t *testing.T, tool nemo.Tool, args map[string]any) (string, error) {
	t.Helper()
	return tool.Handler(context.Background(), args)
}

// ---------------------------------------------------------------------------
// parseGitignore / gitignoreRules.ignored.
// ---------------------------------------------------------------------------

func TestParseGitignore(t *testing.T) {
	data := "# comment\n\n" +
		"*.log\n" +
		"!keep.log\n" +
		"build/\n" +
		"/docs\n" +
		"data?\n"
	g := parseGitignore(data)

	tests := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"x.log", false, true},
		{"dir/x.log", false, true},      // unanchored globs match at any depth
		{"keep.log", false, false},      // ! negation, last match wins
		{"build", true, true},           // trailing slash: dirs only
		{"build", false, false},         // ... so a file named build survives
		{"a/build", true, true},         // unanchored dir pattern matches nested
		{"docs", true, true},            // anchored
		{"docs/file.txt", false, false}, // anchored patterns cover only the exact path (simplified parser)
		{"data1", false, true},          // ? matches one char
		{"data12", false, false},
	}
	for _, tt := range tests {
		if got := g.ignored(tt.rel, tt.isDir); got != tt.want {
			t.Errorf("ignored(%q, isDir=%v) = %v, want %v", tt.rel, tt.isDir, got, tt.want)
		}
	}

	// Comment-only input parses to nil rules, which ignore nothing.
	if g := parseGitignore("# only comments\n\n"); g != nil {
		t.Errorf("parseGitignore of comment-only input = %v, want nil", g)
	}
}

// ---------------------------------------------------------------------------
// gitFilter.
// ---------------------------------------------------------------------------

func TestGitFilter(t *testing.T) {
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, ".gitignore"), "*.log\n!keep.log\nsub/\n")
	ff := gitFilter(tmp)

	tests := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"a.go", false, true},
		{"dir/a.go", false, true},
		{"x.log", false, false},     // gitignored
		{"dir/x.log", false, false}, // unanchored glob matches at any depth
		{"keep.log", false, true},   // ! negation
		{"sub", true, false},        // trailing slash: dir ignored...
		{"sub/c.go", false, true},   // ...its subtree is pruned by the walk, not the rule
		{".git", true, false},        // .git always skipped...
		{"nested/.git", true, false}, // ...wherever it appears
	}
	for _, tt := range tests {
		if got := ff(tt.rel, tt.isDir); got != tt.want {
			t.Errorf("gitFilter(%q, isDir=%v) = %v, want %v", tt.rel, tt.isDir, got, tt.want)
		}
	}

	// No .gitignore at the root: everything but .git is admitted.
	ff = gitFilter(t.TempDir())
	if !ff("anything.log", false) || ff(".git", true) {
		t.Errorf("gitFilter without .gitignore: want everything but .git admitted")
	}
}

// TestGitFilterTools pins the git behavior end to end through the
// library's search tools: .git subtrees and gitignored paths are not
// found by find and not searched by grep.
func TestGitFilterTools(t *testing.T) {
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, ".gitignore"), "*.log\n")
	mustWrite(t, filepath.Join(tmp, "x.go"), "package x")
	mustMkdir(t, filepath.Join(tmp, "sub"))
	mustWrite(t, filepath.Join(tmp, "sub", "y.go"), "package y")
	mustWrite(t, filepath.Join(tmp, "z.log"), "package ignored")
	mustMkdir(t, filepath.Join(tmp, ".git"))
	mustWrite(t, filepath.Join(tmp, ".git", "g.go"), "package git")

	find := nemo.NewFindTool(gitFilter)
	grep := nemo.NewGrepTool(gitFilter)

	out, err := callTool(t, find, map[string]any{"pattern": "*.go", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	// Walk order is lexical: .git and .gitignore come first, then sub/,
	// then the top-level files; z.log is gitignored, .git/g.go pruned.
	if want := "sub/y.go\nx.go"; out != want {
		t.Errorf("find *.go = %q, want %q", out, want)
	}

	out, err = callTool(t, find, map[string]any{"pattern": "*.log", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	if out != "no matches" {
		t.Errorf("find *.log = %q, want no matches", out)
	}

	out, err = callTool(t, grep, map[string]any{"pattern": "package", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, "sub", "y.go") + ":1: package y\n" +
		filepath.Join(tmp, "x.go") + ":1: package x"
	if out != want {
		t.Errorf("grep package = %q, want %q", out, want)
	}
}
