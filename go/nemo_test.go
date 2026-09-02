package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
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

// callTool invokes a Tool handler with args, so tests read like the JSON
// the model would have sent (numbers as float64, as after unmarshal).
func callTool(t *testing.T, tool Tool, args map[string]any) (string, error) {
	t.Helper()
	return tool.Handler(context.Background(), args)
}

// ---------------------------------------------------------------------------
// containsCwd (delete guard).
// ---------------------------------------------------------------------------

// TestContainsCwd pins the polarity of the delete guard: the working
// directory and its ancestors are dangerous (true), paths inside or
// outside the working tree are deletable (false).
func TestContainsCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dotAbs, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(cwd)

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"cwd itself", cwd, true},
		{"cwd via dot", dotAbs, true},
		{"parent of cwd", parent, true},
		{"filesystem root", string(filepath.Separator), true},
		{"child of cwd", filepath.Join(cwd, "build"), false},
		{"nested child of cwd", filepath.Join(cwd, "a", "b"), false},
		{"sibling branch", filepath.Join(parent, "nemo-test-elsewhere"), false},
		{"other subtree", filepath.Join(os.TempDir(), "nemo-test"), false},
	}
	for _, tt := range tests {
		if got := containsCwd(tt.dir); got != tt.want {
			t.Errorf("%s: containsCwd(%q) = %v, want %v", tt.name, tt.dir, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// extractText (fetch "readable" postprocessing).
// ---------------------------------------------------------------------------

func TestExtractText(t *testing.T) {
	html := "<html><head><style>body{color:red}</style>" +
		"<script>var x=1;</script></head>\n" +
		"<body>\n" +
		"<p>Hello   <b>world</b></p>\n" +
		"<p>Second   paragraph</p>\n" +
		"</body></html>"
	want := "Hello world\nSecond paragraph"
	if got := extractText(html); got != want {
		t.Errorf("extractText = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// truncateUTF8.
// ---------------------------------------------------------------------------

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		s, marker string
		limit     int
		want      string
	}{
		{"hello", "!", 10, "hello"},  // under limit: untouched
		{"hello", "!", 5, "hello"},   // exactly at limit: untouched
		{"hello", "…", 3, "hel…"},    // ASCII cut
		{"héllo", "!", 2, "h!"},      // cut inside é: back up to rune start
		{"日本語", "!", 6, "日本!"},    // cut lands exactly between runes
	}
	for _, tt := range tests {
		if got := truncateUTF8(tt.s, tt.limit, tt.marker); got != tt.want {
			t.Errorf("truncateUTF8(%q, %d, %q) = %q, want %q",
				tt.s, tt.limit, tt.marker, got, tt.want)
		}
	}
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
		{"dir/x.log", false, true},  // unanchored globs match at any depth
		{"keep.log", false, false},  // ! negation, last match wins
		{"build", true, true},       // trailing slash: dirs only
		{"build", false, false},     // ... so a file named build survives
		{"a/build", true, true},     // unanchored dir pattern matches nested
		{"docs", true, true},        // anchored
		{"docs/file.txt", false, false}, // anchored patterns cover only the exact path (simplified parser)
		{"data1", false, true},      // ? matches one char
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
// walkTree.
// ---------------------------------------------------------------------------

func TestWalkTree(t *testing.T) {
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, ".gitignore"), "*.log\nsub/\n")
	mustWrite(t, filepath.Join(tmp, "a.go"), "package a")
	mustWrite(t, filepath.Join(tmp, "b.log"), "ignored")
	mustWrite(t, filepath.Join(tmp, "keep.txt"), "kept")
	mustMkdir(t, filepath.Join(tmp, ".git"))
	mustWrite(t, filepath.Join(tmp, ".git", "config"), "[core]")
	mustMkdir(t, filepath.Join(tmp, "sub"))
	mustWrite(t, filepath.Join(tmp, "sub", "c.go"), "package c")

	var names []string
	err := walkTree(tmp, func(p string) {
		rel, err := filepath.Rel(tmp, p)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, filepath.ToSlash(rel))
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)

	// .git is always skipped, b.log and sub/ are gitignored; the root
	// .gitignore itself is walked like any other file.
	want := []string{".gitignore", "a.go", "keep.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("walkTree collected %v, want %v", names, want)
	}

	// A file root yields just that file.
	names = nil
	if err := walkTree(filepath.Join(tmp, "a.go"), func(p string) { names = append(names, p) }); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != filepath.Join(tmp, "a.go") {
		t.Errorf("walkTree of a file = %v, want [%s]", names, filepath.Join(tmp, "a.go"))
	}
}

// ---------------------------------------------------------------------------
// Diff engine: lcsOps and diffLines.
// ---------------------------------------------------------------------------

func renderOps(ops []diffOp) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = fmt.Sprintf("%c%s", op.mark, op.line)
	}
	return out
}

func TestLcsOps(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want []string
	}{
		{"substitution", []string{"x", "y", "z"}, []string{"x", "w", "z"},
			[]string{" x", "-y", "+w", " z"}},
		{"identical", []string{"a", "b"}, []string{"a", "b"}, []string{" a", " b"}},
		{"pure insertion", nil, []string{"p", "q"}, []string{"+p", "+q"}},
		{"pure deletion", []string{"p", "q"}, nil, []string{"-p", "-q"}},
	}
	for _, tt := range tests {
		if got := renderOps(lcsOps(tt.a, tt.b)); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: lcsOps = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDiffLines(t *testing.T) {
	// Small edit: exact rendering with 3-line context on each side.
	got := diffLines("p", []string{"a", "b", "c", "d", "e"}, []string{"a", "b", "X", "d", "e"}, false)
	want := "[diff: p] (5 -> 5 lines)\n  a\n  b\n- c\n+ X\n  d\n  e\n"
	if got != want {
		t.Errorf("diffLines = %q, want %q", got, want)
	}

	// New file: header plus all-added body.
	got = diffLines("p", nil, []string{"a", "b"}, true)
	want = "[diff: p] (new file, 2 lines)\n+ a\n+ b\n"
	if got != want {
		t.Errorf("diffLines (new file) = %q, want %q", got, want)
	}

	// Identical content.
	got = diffLines("p", []string{"a"}, []string{"a"}, false)
	want = "[diff: p] (1 -> 1 lines)\n  (unchanged)\n"
	if got != want {
		t.Errorf("diffLines (unchanged) = %q, want %q", got, want)
	}

	// Both caps: middle over 300 lines collapses to markers, and a body
	// over 80 rendered lines is truncated.
	big := func(s string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	got = diffLines("p", big("a", 400), big("b", 400), false)
	for _, want := range []string{"(400 -> 400 lines)", "[...400 lines removed...]", "[...400 lines added...]"} {
		if !strings.Contains(got, want) {
			t.Errorf("diffLines (collapsed middle) missing %q in:\n%s", want, got)
		}
	}
	got = diffLines("p", big("a", 100), big("b", 100), false)
	if !strings.Contains(got, "[...diff truncated...]") {
		t.Errorf("diffLines (long body) missing truncation marker in:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// usage accounting.
// ---------------------------------------------------------------------------

func TestUsage(t *testing.T) {
	u := parseUsage(map[string]any{
		"prompt_tokens":               float64(10),
		"completion_tokens":           float64(5),
		"prompt_tokens_details":       map[string]any{"cached_tokens": float64(3)},
		"completion_tokens_details":   map[string]any{"reasoning_tokens": float64(2)},
		"cost":                        float64(0.5),
	})
	if got, want := u.String(), "10 in / 5 out (3 cached) (2 reasoning) $0.50000"; got != want {
		t.Errorf("usage.String() = %q, want %q", got, want)
	}

	if u := parseUsage(nil); u != (usage{}) {
		t.Errorf("parseUsage(nil) = %+v, want zero value", u)
	}
	if got, want := parseUsage(nil).String(), "0 in / 0 out"; got != want {
		t.Errorf("zero usage.String() = %q, want %q", got, want)
	}

	// add accumulates every field.
	a := usage{promptTokens: 1, completionTokens: 2, cachedTokens: 3, reasoningTokens: 4, cost: 5}
	a.add(usage{promptTokens: 10, completionTokens: 20, cachedTokens: 30, reasoningTokens: 40, cost: 50})
	want := usage{promptTokens: 11, completionTokens: 22, cachedTokens: 33, reasoningTokens: 44, cost: 55}
	if a != want {
		t.Errorf("add = %+v, want %+v", a, want)
	}

	// delta subtracts a snapshot, including cost.
	d := usage{promptTokens: 20, completionTokens: 10, cachedTokens: 6, reasoningTokens: 4, cost: 1.0}.
		delta(usage{promptTokens: 15, completionTokens: 8, cachedTokens: 5, reasoningTokens: 3, cost: 0.75})
	if got, want := d.String(), "5 in / 2 out (1 cached) (1 reasoning) $0.25000"; got != want {
		t.Errorf("delta.String() = %q, want %q", got, want)
	}
}

func TestNumAs(t *testing.T) {
	m := map[string]any{"i": float64(7), "s": "not a number"}
	if got := numAs(m, "i"); got != 7 {
		t.Errorf("numAs(i) = %d, want 7", got)
	}
	if got := numAs(m, "s"); got != 0 { // non-numeric value reads as zero
		t.Errorf("numAs(s) = %d, want 0", got)
	}
	if got := numAs(m, "missing"); got != 0 {
		t.Errorf("numAs(missing) = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// extractUpstreamError.
// ---------------------------------------------------------------------------

func TestExtractUpstreamError(t *testing.T) {
	tests := []struct {
		name  string
		body  map[string]any
		want  string
	}{
		{"string error", map[string]any{"error": "boom"}, "boom"},
		{"object error", map[string]any{"error": map[string]any{"message": "quota exceeded"}}, "quota exceeded"},
		{"object without message", map[string]any{"error": map[string]any{"code": float64(401)}}, ""},
		{"no error field", map[string]any{"ok": true}, ""},
	}
	for _, tt := range tests {
		if got := extractUpstreamError(tt.body); got != tt.want {
			t.Errorf("%s: extractUpstreamError = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// buildToolSpecs.
// ---------------------------------------------------------------------------

func TestBuildToolSpecs(t *testing.T) {
	tools := []Tool{
		{Name: "b", Description: "second", Parameters: map[string]any{"type": "object"}},
		{Name: "a", Description: "first", Parameters: map[string]any{"type": "object"}},
	}
	specs := buildToolSpecs(tools)
	if len(specs) != len(tools) {
		t.Fatalf("got %d specs, want %d", len(specs), len(tools))
	}
	// Order preserved, shape correct, parameters passed through.
	for i, want := range []string{"b", "a"} {
		spec := specs[i]
		if spec["type"] != "function" {
			t.Errorf("specs[%d][type] = %v, want function", i, spec["type"])
		}
		fn, ok := spec["function"].(map[string]any)
		if !ok {
			t.Fatalf("specs[%d][function] is not an object", i)
		}
		if fn["name"] != want {
			t.Errorf("specs[%d] name = %v, want %v (order must be preserved)", i, fn["name"], want)
		}
		if fn["description"] != tools[i].Description {
			t.Errorf("specs[%d] description = %v, want %v", i, fn["description"], tools[i].Description)
		}
		if !reflect.DeepEqual(fn["parameters"], tools[i].Parameters) {
			t.Errorf("specs[%d] parameters = %v, want %v", i, fn["parameters"], tools[i].Parameters)
		}
	}
}

// ---------------------------------------------------------------------------
// handleToolCall dispatch.
// ---------------------------------------------------------------------------

func TestHandleToolCall(t *testing.T) {
	reg := map[string]Tool{
		"echo": {Name: "echo", Handler: func(ctx context.Context, args map[string]any) (string, error) {
			return args["x"].(string), nil
		}},
	}
	ctx := context.Background()

	// Well-formed call: id, result, no error.
	id, out, err := handleToolCall(ctx, reg, 0, map[string]any{
		"id":       "call_9",
		"function": map[string]any{"name": "echo", "arguments": `{"x":"hi"}`},
	})
	if err != nil || id != "call_9" || out != "hi" {
		t.Errorf("valid call = (%q, %q, %v), want (call_9, hi, nil)", id, out, err)
	}

	// Missing id is synthesized from the call index.
	id, out, err = handleToolCall(ctx, reg, 3, map[string]any{
		"function": map[string]any{"name": "echo", "arguments": `{"x":"hi"}`},
	})
	if err != nil || id != "call_3" || out != "hi" {
		t.Errorf("no-id call = (%q, %q, %v), want (call_3, hi, nil)", id, out, err)
	}

	// Unknown tool: reported to the model, not an error.
	_, out, err = handleToolCall(ctx, reg, 0, map[string]any{
		"function": map[string]any{"name": "missing", "arguments": "{}"},
	})
	if err != nil || out != "unknown tool" {
		t.Errorf("unknown tool = (%q, %v), want (unknown tool, nil)", out, err)
	}

	// Malformed arguments JSON: reported, not an error.
	_, out, err = handleToolCall(ctx, reg, 0, map[string]any{
		"function": map[string]any{"name": "echo", "arguments": "{"},
	})
	if err != nil || !strings.HasPrefix(out, "invalid tool arguments") {
		t.Errorf("bad args = (%q, %v), want (invalid tool arguments..., nil)", out, err)
	}

	// Non-object tool call entry.
	_, out, err = handleToolCall(ctx, reg, 0, "junk")
	if err != nil || out != `invalid tool call: "junk"` {
		t.Errorf("non-object call = (%q, %v), want (invalid tool call: \"junk\", nil)", out, err)
	}

	// Function field not an object.
	_, out, err = handleToolCall(ctx, reg, 0, map[string]any{
		"id":       "z",
		"function": "nope",
	})
	if err != nil || out != "invalid tool call: function missing or not an object" {
		t.Errorf("non-object function = (%q, %v), want (invalid tool call: function missing..., nil)", out, err)
	}
}

// ---------------------------------------------------------------------------
// Tool handlers (against temp directories).
// ---------------------------------------------------------------------------

func toolByName(t *testing.T, name string) Tool {
	t.Helper()
	for _, tt := range defaultTools {
		if tt.Name == name {
			return tt
		}
	}
	t.Fatalf("tool %q not found in defaultTools", name)
	return Tool{}
}

func TestWriteTool(t *testing.T) {
	tool := toolByName(t, "write")
	p := filepath.Join(t.TempDir(), "nested", "deep", "f.txt") // parents must be created

	var displays []string
	ctx := withDisplay(context.Background(), func(s string) { displays = append(displays, s) })

	out, err := tool.Handler(ctx, map[string]any{"path": p, "content": "hello\nworld"})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("wrote 11 bytes to %s", p); out != want {
		t.Errorf("write = %q, want %q", out, want)
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != "hello\nworld" {
		t.Errorf("file content = (%q, %v), want hello\\nworld", data, err)
	}
	if len(displays) != 1 || !strings.Contains(displays[0], "(new file, 2 lines)") {
		t.Errorf("first display = %v, want new-file header", displays)
	}

	// Overwrite: diff against previous content, file updated.
	out, err = tool.Handler(ctx, map[string]any{"path": p, "content": "hello\nworld!"})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("wrote 12 bytes to %s", p); out != want {
		t.Errorf("overwrite = %q, want %q", out, want)
	}
	if len(displays) != 2 ||
		!strings.Contains(displays[1], "(2 -> 2 lines)") ||
		!strings.Contains(displays[1], "- world\n") ||
		!strings.Contains(displays[1], "+ world!\n") {
		t.Errorf("overwrite display = %v, want diff with - world / + world!", displays)
	}

	// Missing content is rejected.
	if _, err := callTool(t, tool, map[string]any{"path": p}); err == nil {
		t.Error("write without content should fail")
	}
}

func TestReadTool(t *testing.T) {
	tool := toolByName(t, "read")
	p := filepath.Join(t.TempDir(), "f.txt")
	mustWrite(t, p, "one\ntwo\nthree")

	out, err := callTool(t, tool, map[string]any{"path": p})
	if err != nil {
		t.Fatal(err)
	}
	if want := p + ": 3 lines, 13 bytes\n1| one\n2| two\n3| three\n"; out != want {
		t.Errorf("read = %q, want %q", out, want)
	}

	// offset/limit and the continuation footer.
	out, err = callTool(t, tool, map[string]any{"path": p, "offset": float64(2), "limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	want := p + ": 3 lines, 13 bytes (from line 2)\n2| two\n" +
		"...[showing lines 2-2 of 3; use offset=3 to continue]\n"
	if out != want {
		t.Errorf("read (offset) = %q, want %q", out, want)
	}

	// Empty file.
	empty := filepath.Join(filepath.Dir(p), "empty.txt")
	mustWrite(t, empty, "")
	out, err = callTool(t, tool, map[string]any{"path": empty})
	if err != nil {
		t.Fatal(err)
	}
	if want := empty + ": empty file"; out != want {
		t.Errorf("read (empty) = %q, want %q", out, want)
	}

	// Binary file.
	bin := filepath.Join(filepath.Dir(p), "bin.dat")
	mustWrite(t, bin, "a\x00b")
	if _, err := callTool(t, tool, map[string]any{"path": bin}); err == nil || err.Error() != bin+": binary file" {
		t.Errorf("read (binary) err = %v, want %q", err, bin+": binary file")
	}

	// Offset past EOF.
	_, err = callTool(t, tool, map[string]any{"path": p, "offset": float64(4)})
	if err == nil || err.Error() != "offset 4 beyond end of file (3 lines)" {
		t.Errorf("read (past eof) err = %v, want offset error", err)
	}
}

func TestEditTool(t *testing.T) {
	tool := toolByName(t, "edit")
	p := filepath.Join(t.TempDir(), "f.txt")
	mustWrite(t, p, "alpha\nbeta\nalpha\n")

	var displays []string
	ctx := withDisplay(context.Background(), func(s string) { displays = append(displays, s) })

	out, err := tool.Handler(ctx, map[string]any{"path": p, "old_text": "beta", "new_text": "BETA"})
	if err != nil || out != "ok" {
		t.Fatalf("edit = (%q, %v), want (ok, nil)", out, err)
	}
	if data, _ := os.ReadFile(p); string(data) != "alpha\nBETA\nalpha\n" {
		t.Errorf("edited content = %q, want alpha\\nBETA\\nalpha\\n", data)
	}
	if len(displays) != 1 || !strings.Contains(displays[0], "- beta\n") || !strings.Contains(displays[0], "+ BETA\n") {
		t.Errorf("display = %v, want diff with - beta / + BETA", displays)
	}

	if _, err := callTool(t, tool, map[string]any{"path": p, "old_text": "gamma", "new_text": "x"}); err == nil ||
		err.Error() != fmt.Sprintf("old_text not found in %s", p) {
		t.Errorf("not-found err = %v, want old_text not found", err)
	}
	if _, err := callTool(t, tool, map[string]any{"path": p, "old_text": "alpha", "new_text": "x"}); err == nil ||
		err.Error() != fmt.Sprintf("old_text matches 2 locations in %s; make it unique", p) {
		t.Errorf("multi-match err = %v, want matches 2 locations", err)
	}
}

func TestDeleteTool(t *testing.T) {
	tool := toolByName(t, "delete")
	tmp := t.TempDir()

	// Plain file.
	f := filepath.Join(tmp, "f.txt")
	mustWrite(t, f, "data")
	out, err := callTool(t, tool, map[string]any{"path": f})
	if err != nil || out != "deleted "+f {
		t.Fatalf("delete file = (%q, %v), want (deleted %s, nil)", out, err, f)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Errorf("file still exists after delete: %v", err)
	}

	// Symlinks are removed as links, never followed.
	target := filepath.Join(tmp, "target.txt")
	mustWrite(t, target, "keep me")
	dirTarget := filepath.Join(tmp, "targetdir")
	mustMkdir(t, dirTarget)
	mustWrite(t, filepath.Join(dirTarget, "inner.txt"), "keep me too")
	fileLink := filepath.Join(tmp, "filelink")
	dirLink := filepath.Join(tmp, "dirlink")
	if err := os.Symlink(target, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dirTarget, dirLink); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{fileLink, dirLink} {
		if _, err := callTool(t, tool, map[string]any{"path": link}); err != nil {
			t.Fatalf("delete symlink %s: %v", link, err)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("file symlink target removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirTarget, "inner.txt")); err != nil {
		t.Errorf("dir symlink target removed: %v", err)
	}

	// Directory without recursive is refused.
	d := filepath.Join(tmp, "d")
	mustMkdir(t, d)
	mustWrite(t, filepath.Join(d, "x.txt"), "x")
	_, err = callTool(t, tool, map[string]any{"path": d})
	if err == nil || !strings.Contains(err.Error(), "recursive=true") {
		t.Errorf("dir without recursive err = %v, want recursive hint", err)
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatal(err)
	}

	// Recursive delete removes contents.
	out, err = callTool(t, tool, map[string]any{"path": d, "recursive": true})
	if err != nil || out != "deleted directory "+d {
		t.Fatalf("recursive delete = (%q, %v), want (deleted directory %s, nil)", out, err, d)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Errorf("directory still exists after recursive delete: %v", err)
	}

	// The cwd guard refuses ".", the cwd itself, and ancestors — without
	// touching anything.
	cwd, _ := os.Getwd()
	for _, dangerous := range []string{".", cwd, filepath.Dir(cwd), string(filepath.Separator)} {
		_, err := callTool(t, tool, map[string]any{"path": dangerous, "recursive": true})
		if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
			t.Errorf("delete %q err = %v, want refusal", dangerous, err)
		}
	}
	if _, err := os.Stat(cwd); err != nil {
		t.Fatal(err) // sanity: the working tree is still there
	}
}

func TestLsTool(t *testing.T) {
	tool := toolByName(t, "ls")
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, "a.txt"), "a")
	mustWrite(t, filepath.Join(tmp, ".hidden"), "h")
	mustMkdir(t, filepath.Join(tmp, "sub"))

	out, err := callTool(t, tool, map[string]any{"path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	// Dotfiles included, directories get a trailing slash, sorted.
	if want := ".hidden\na.txt\nsub/"; out != want {
		t.Errorf("ls = %q, want %q", out, want)
	}
}

func TestFindTool(t *testing.T) {
	tool := toolByName(t, "find")
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, ".gitignore"), "*.log\n")
	mustWrite(t, filepath.Join(tmp, "x.go"), "package x")
	mustMkdir(t, filepath.Join(tmp, "sub"))
	mustWrite(t, filepath.Join(tmp, "sub", "y.go"), "package y")
	mustWrite(t, filepath.Join(tmp, "z.log"), "ignored")

	out, err := callTool(t, tool, map[string]any{"pattern": "*.go", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sub/y.go\nx.go"; out != want {
		t.Errorf("find *.go = %q, want %q", out, want)
	}

	// **/ prefix is trimmed and matches the same files.
	out, err = callTool(t, tool, map[string]any{"pattern": "**/*.go", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sub/y.go\nx.go"; out != want {
		t.Errorf("find **/*.go = %q, want %q", out, want)
	}

	// Gitignored files are not found.
	out, err = callTool(t, tool, map[string]any{"pattern": "*.log", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	if out != "no matches" {
		t.Errorf("find *.log = %q, want no matches", out)
	}
}

func TestGrepTool(t *testing.T) {
	tool := toolByName(t, "grep")
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, ".gitignore"), "*.log\n")
	mustWrite(t, filepath.Join(tmp, "a.txt"), "hello world\nHELLO again\nbye\n")
	mustWrite(t, filepath.Join(tmp, "b.log"), "hello in ignored file")
	mustWrite(t, filepath.Join(tmp, "bin.dat"), "\x00hello") // binary: skipped

	a := filepath.Join(tmp, "a.txt")

	out, err := callTool(t, tool, map[string]any{"pattern": "hello", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	if want := a + ":1: hello world"; out != want {
		t.Errorf("grep = %q, want %q", out, want)
	}

	out, err = callTool(t, tool, map[string]any{"pattern": "hello", "path": tmp, "ignore_case": true})
	if err != nil {
		t.Fatal(err)
	}
	want := a + ":1: hello world\n" + a + ":2: HELLO again"
	if out != want {
		t.Errorf("grep (ignore_case) = %q, want %q", out, want)
	}

	out, err = callTool(t, tool, map[string]any{"pattern": "zzz", "path": tmp})
	if err != nil {
		t.Fatal(err)
	}
	if out != "no matches" {
		t.Errorf("grep (no match) = %q, want no matches", out)
	}
}
