package nemo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
// TruncateUTF8.
// ---------------------------------------------------------------------------

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		s, marker string
		limit     int
		want      string
	}{
		{"hello", "!", 10, "hello"}, // under limit: untouched
		{"hello", "!", 5, "hello"},  // exactly at limit: untouched
		{"hello", "…", 3, "hel…"},   // ASCII cut
		{"héllo", "!", 2, "h!"},     // cut inside é: back up to rune start
		{"日本語", "!", 6, "日本!"},      // cut lands exactly between runes
	}
	for _, tt := range tests {
		if got := TruncateUTF8(tt.s, tt.limit, tt.marker); got != tt.want {
			t.Errorf("TruncateUTF8(%q, %d, %q) = %q, want %q",
				tt.s, tt.limit, tt.marker, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// walkTree.
// ---------------------------------------------------------------------------

func TestWalkTree(t *testing.T) {
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, "a.go"), "package a")
	mustWrite(t, filepath.Join(tmp, "b.log"), "data")
	mustWrite(t, filepath.Join(tmp, "keep.txt"), "kept")
	mustMkdir(t, filepath.Join(tmp, ".git"))
	mustWrite(t, filepath.Join(tmp, ".git", "config"), "[core]")
	mustMkdir(t, filepath.Join(tmp, "sub"))
	mustWrite(t, filepath.Join(tmp, "sub", "c.go"), "package c")

	var names []string
	collect := func(p string) {
		rel, err := filepath.Rel(tmp, p)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, filepath.ToSlash(rel))
	}

	// A nil filter walks every file, including those inside .git
	// (directories themselves are never yielded, only descended).
	err := walkTree(tmp, nil, collect)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	want := []string{".git/config", "a.go", "b.log", "keep.txt", "sub/c.go"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("walkTree (nil filter) collected %v, want %v", names, want)
	}

	// A custom filter sees root-relative paths: rejecting the .git
	// directory prunes its whole subtree, rejecting *.log skips plain
	// files. (The nemo command's gitFilter is exactly this shape; it
	// lives in cmd/nemo with its own tests.)
	names = nil
	if err := walkTree(tmp, func(rel string, isDir bool) bool {
		return !(isDir && filepath.Base(rel) == ".git") && !strings.HasSuffix(rel, ".log")
	}, collect); err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	want = []string{"a.go", "keep.txt", "sub/c.go"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("walkTree (custom filter) collected %v, want %v", names, want)
	}

	// A file root yields just that file; the filter never sees it.
	names = nil
	if err := walkTree(filepath.Join(tmp, "a.go"), nil, func(p string) { names = append(names, p) }); err != nil {
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
		"prompt_tokens":             float64(10),
		"completion_tokens":         float64(5),
		"prompt_tokens_details":     map[string]any{"cached_tokens": float64(3)},
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(2)},
		"cost":                      float64(0.5),
	})
	if got, want := u.String(), "10 in / 5 out (3 cached) (2 reasoning) $0.50000"; got != want {
		t.Errorf("usage.String() = %q, want %q", got, want)
	}

	if u := parseUsage(nil); u != (Usage{}) {
		t.Errorf("parseUsage(nil) = %+v, want zero value", u)
	}
	if got, want := parseUsage(nil).String(), "0 in / 0 out"; got != want {
		t.Errorf("zero usage.String() = %q, want %q", got, want)
	}

	// add accumulates every field.
	a := Usage{PromptTokens: 1, CompletionTokens: 2, CachedTokens: 3, ReasoningTokens: 4, Cost: 5}
	a.add(Usage{PromptTokens: 10, CompletionTokens: 20, CachedTokens: 30, ReasoningTokens: 40, Cost: 50})
	want := Usage{PromptTokens: 11, CompletionTokens: 22, CachedTokens: 33, ReasoningTokens: 44, Cost: 55}
	if a != want {
		t.Errorf("add = %+v, want %+v", a, want)
	}

	// Delta subtracts a snapshot, including cost.
	d := Usage{PromptTokens: 20, CompletionTokens: 10, CachedTokens: 6, ReasoningTokens: 4, Cost: 1.0}.
		Delta(Usage{PromptTokens: 15, CompletionTokens: 8, CachedTokens: 5, ReasoningTokens: 3, Cost: 0.75})
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
		name string
		body map[string]any
		want string
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
// processToolCalls (parallel tool-call batches).
// ---------------------------------------------------------------------------

// batchCall builds a tool_calls entry shaped like the ones the model
// sends (arguments as a JSON string, as after SSE assembly).
func batchCall(id, name, args string) map[string]any {
	return map[string]any{
		"id":       id,
		"function": map[string]any{"name": name, "arguments": args},
	}
}

func TestProcessToolCallsParallel(t *testing.T) {
	// Two read-only handlers that each block until both have started:
	// only concurrent execution gets past the second receive (a
	// sequential loop would hang on the first handler's <-release).
	started := make(chan string, 2)
	release := make(chan struct{})
	reg := map[string]Tool{
		"r": {Name: "r", Handler: func(ctx context.Context, args map[string]any) (string, error) {
			started <- args["x"].(string)
			<-release
			return "done " + args["x"].(string), nil
		}},
	}
	calls := []any{
		batchCall("a", "r", `{"x":"a"}`),
		batchCall("b", "r", `{"x":"b"}`),
	}

	type report struct {
		id, name, args, detail string
		ok                     bool
	}
	var got []report
	var starts []report
	var messages []map[string]any
	done := make(chan struct{})
	go func() {
		processToolCalls(context.Background(), reg, calls,
			func(callID, name, args string) {
				starts = append(starts, report{id: callID, name: name, args: args})
			},
			func(callID, name, args string, ok bool, detail string) {
				got = append(got, report{callID, name, args, detail, ok})
			},
			func(msg map[string]any) { messages = append(messages, msg) })
		close(done)
	}()

	// Both handlers must reach their start point without any release.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("tool calls did not run concurrently")
		}
	}
	close(release)
	<-done

	// Reported in call order (completion order is arbitrary here), with
	// each call's id, name, args, and output.
	want := []report{
		{"a", "r", `{"x":"a"}`, "done a", true},
		{"b", "r", `{"x":"b"}`, "done b", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("onToolEnd reports = %v, want %v", got, want)
	}
	// Start callbacks fired for every call, in issue order, before any
	// result came back (handlers were still parked on <-release).
	wantStarts := []report{
		{"a", "r", `{"x":"a"}`, "", false},
		{"b", "r", `{"x":"b"}`, "", false},
	}
	if !reflect.DeepEqual(starts, wantStarts) {
		t.Errorf("onToolStart reports = %v, want %v", starts, wantStarts)
	}
	// Tool messages appended in call order, ids paired in sequence.
	if len(messages) != 2 ||
		messages[0]["tool_call_id"] != "a" || messages[1]["tool_call_id"] != "b" ||
		messages[0]["content"] != "done a" || messages[1]["content"] != "done b" {
		t.Errorf("tool messages = %v, want a then b in call order", messages)
	}
}

func TestProcessToolCallsMutatingSerialized(t *testing.T) {
	// Two read-modify-write appends to one file in a single batch must
	// both land: mutating handlers execute in issue order, never
	// concurrently (a lost update would leave "a" or "b" missing).
	p := filepath.Join(t.TempDir(), "log.txt")
	mustWrite(t, p, "")
	reg := map[string]Tool{
		"append": {Name: "append", Mutating: true, Handler: func(ctx context.Context, args map[string]any) (string, error) {
			data, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			// Sleep inside the read-modify-write window so concurrent
			// execution would reliably lose one update.
			time.Sleep(25 * time.Millisecond)
			if err := os.WriteFile(p, []byte(string(data)+args["ch"].(string)), 0o644); err != nil {
				return "", err
			}
			return "ok", nil
		}},
	}
	calls := []any{
		batchCall("w1", "append", `{"ch":"a"}`),
		batchCall("w2", "append", `{"ch":"b"}`),
	}

	var messages []map[string]any
	processToolCalls(context.Background(), reg, calls, nil, nil,
		func(msg map[string]any) { messages = append(messages, msg) })

	data, err := os.ReadFile(p)
	if err != nil || string(data) != "ab" {
		t.Errorf("file = %q (%v), want %q: both appends must land", data, err, "ab")
	}
	if len(messages) != 2 || messages[0]["tool_call_id"] != "w1" || messages[1]["tool_call_id"] != "w2" {
		t.Errorf("tool messages = %v, want w1 then w2 in issue order", messages)
	}
}

func TestProcessToolCallsDisplayReplay(t *testing.T) {
	// Diff previews emitted by parallel handlers are buffered per call
	// and replayed in call order: the slow first call's preview must be
	// shown before the fast second call's even though the second call
	// finishes first (a shared emitter would show them out of order).
	reg := map[string]Tool{
		"slow": {Name: "slow", Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if d := displayFrom(ctx); d != nil {
				d("preview-1")
			}
			time.Sleep(30 * time.Millisecond)
			return "slow done", nil
		}},
		"fast": {Name: "fast", Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if d := displayFrom(ctx); d != nil {
				d("preview-2")
			}
			return "fast done", nil
		}},
	}
	var shown []string
	ctx := WithDisplay(context.Background(), func(s string) { shown = append(shown, s) })
	calls := []any{
		batchCall("c1", "slow", "{}"),
		batchCall("c2", "fast", "{}"),
	}
	var order []string
	processToolCalls(ctx, reg, calls, nil,
		func(callID, name, args string, ok bool, detail string) { order = append(order, callID) },
		func(msg map[string]any) {})

	if want := []string{"preview-1", "preview-2"}; !reflect.DeepEqual(shown, want) {
		t.Errorf("display order = %v, want %v", shown, want)
	}
	if want := []string{"c1", "c2"}; !reflect.DeepEqual(order, want) {
		t.Errorf("onToolEnd order = %v, want %v", order, want)
	}
}

func TestProcessToolCallsImageAttachment(t *testing.T) {
	// A call whose handler attaches an image URL gets content parts
	// (text + image_url) in its tool message, with the URL verbatim — the
	// transport (data: URL or hosted link) already ran in the handler; a
	// text-only call keeps plain string content.
	reg := map[string]Tool{
		"selfie": {Name: "selfie", Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if attach := attachFrom(ctx); attach != nil {
				attach("https://example.com/shot.png")
			}
			return "attached 1 image", nil
		}},
		"plain": {Name: "plain", Handler: func(ctx context.Context, args map[string]any) (string, error) {
			return "text only", nil
		}},
	}
	var msgs []map[string]any
	processToolCalls(context.Background(), reg, []any{
		batchCall("c1", "selfie", "{}"),
		batchCall("c2", "plain", "{}"),
	},
		nil,
		func(callID, name, args string, ok bool, detail string) {},
		func(msg map[string]any) { msgs = append(msgs, msg) })

	if len(msgs) != 2 {
		t.Fatalf("got %d tool messages, want 2", len(msgs))
	}

	// Image call: content is [text part, image_url part] with the URL verbatim.
	m := msgs[0]
	if m["role"] != "tool" || m["tool_call_id"] != "c1" {
		t.Errorf("message role/id = %v/%v, want tool/c1", m["role"], m["tool_call_id"])
	}
	parts, ok := m["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want 2 parts", m["content"])
	}
	text, _ := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "attached 1 image" {
		t.Errorf("text part = %v, want attached 1 image", text)
	}
	ip, _ := parts[1].(map[string]any)
	if ip["type"] != "image_url" {
		t.Fatalf("second part type = %v, want image_url", ip["type"])
	}
	iu, _ := ip["image_url"].(map[string]any)
	if iu["url"] != "https://example.com/shot.png" {
		t.Errorf("image url = %v, want https://example.com/shot.png", iu["url"])
	}
	// The parts must survive the wire format.
	if _, err := json.Marshal(m); err != nil {
		t.Errorf("marshal tool message with parts: %v", err)
	}

	// No-image call: content stays a plain string (regression guard).
	if s, ok := msgs[1]["content"].(string); !ok || s != "text only" {
		t.Errorf("plain content = %#v, want %q", msgs[1]["content"], "text only")
	}
}

func TestReadSSEStreamToolCallType(t *testing.T) {
	// Assembled tool_calls entries must carry type "function" even when
	// the streamed fragments omit it. Permissive backends (OpenAI,
	// DeepSeek) stream tool-call deltas without a type and accept the
	// echo-back without one; strict ones (Zhipu GLM) reject the next
	// request with 400 code 1214 "Tool type cannot be empty" unless every
	// echoed entry has a type. A missing type defaults to "function"; an
	// explicit one is preserved.
	stream := strings.Join([]string{
		// Call 0: fragments with no type at all (OpenAI/DeepSeek style).
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		// Call 1: explicit type, split across fragments.
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","type":"function","function":{"name":"grep","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"\n",
	}, "\n")

	res, err := readSSEStream(strings.NewReader(stream), nil, nil, nil)
	if err != nil {
		t.Fatalf("readSSEStream: %v", err)
	}
	calls, ok := res.assistant["tool_calls"].([]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("tool_calls = %#v, want 2 entries", res.assistant["tool_calls"])
	}
	for i, want := range []string{"function", "function"} {
		call, _ := calls[i].(map[string]any)
		if call["type"] != want {
			t.Errorf("call %d type = %#v, want %q", i, call["type"], want)
		}
		if _, err := json.Marshal(call); err != nil {
			t.Errorf("call %d marshal: %v", i, err)
		}
	}
	// Arguments still accumulate across fragments.
	fn, _ := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"path":"x"}` {
		t.Errorf("call 0 arguments = %#v, want concatenated JSON args", fn["arguments"])
	}
	if res.finishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", res.finishReason)
	}
}

func TestNormalizeToolCallTypes(t *testing.T) {
	// Sessions written before the stream fix (or by other frontends) can
	// hold assistant tool_calls entries without a type; buildRequestBody
	// patches them to "function" at request time — copy-on-write, never
	// mutating the session it was handed.
	typed := map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id": "c1", "type": "custom_kind",
			"function": map[string]any{"name": "grep", "arguments": "{}"},
		}},
	}
	typeless := map[string]any{
		"role": "assistant",
		"tool_calls": []any{
			map[string]any{
				"id":       "c0",
				"function": map[string]any{"name": "read", "arguments": "{}"},
			},
			typed["tool_calls"].([]any)[0], // shared entry, already typed
		},
	}
	user := map[string]any{"role": "user", "content": "hi"}
	session := []map[string]any{typeless, typed, user}

	out := normalizeToolCallTypes(session)

	// The caller's session must be untouched.
	calls, _ := session[0]["tool_calls"].([]any)
	if _, has := calls[0].(map[string]any)["type"]; has {
		t.Error("normalizeToolCallTypes mutated the input session")
	}
	if !reflect.DeepEqual(session, []map[string]any{typeless, typed, user}) {
		t.Error("normalizeToolCallTypes replaced input messages")
	}

	// Patched output: typeless becomes function, explicit type survives.
	patched, _ := out[0]["tool_calls"].([]any)
	if patched[0].(map[string]any)["type"] != "function" {
		t.Errorf("patched type = %#v, want function", patched[0].(map[string]any)["type"])
	}
	if patched[1].(map[string]any)["type"] != "custom_kind" {
		t.Errorf("explicit type clobbered: %#v", patched[1].(map[string]any)["type"])
	}
	kept, _ := out[1]["tool_calls"].([]any)
	if kept[0].(map[string]any)["type"] != "custom_kind" {
		t.Errorf("already-typed message was rewritten: %#v", out[1])
	}
	// The shared typed entry is the same object in input and output (no
	// needless cloning), while the patched slice is new. Interface ==
	// panics on map dynamic types, so compare by runtime pointer.
	if reflect.ValueOf(patched[1]).Pointer() != reflect.ValueOf(typed["tool_calls"].([]any)[0]).Pointer() {
		t.Error("typed entry was cloned unnecessarily")
	}

	// All-typed histories come back unchanged (same slice).
	clean := []map[string]any{typed, user}
	if got := normalizeToolCallTypes(clean); !reflect.DeepEqual(got, clean) {
		t.Errorf("clean history rewritten: %#v", got)
	}
}

// ---------------------------------------------------------------------------
// Tool handlers (against temp directories).
// ---------------------------------------------------------------------------

func toolByName(t *testing.T, name string) Tool {
	t.Helper()
	for _, tt := range DefaultTools(nil, nil) {
		if tt.Name == name {
			return tt
		}
	}
	t.Fatalf("tool %q not found in DefaultTools", name)
	return Tool{}
}

func TestWriteTool(t *testing.T) {
	tool := toolByName(t, "write")
	p := filepath.Join(t.TempDir(), "nested", "deep", "f.txt") // parents must be created

	var displays []string
	ctx := WithDisplay(context.Background(), func(s string) { displays = append(displays, s) })

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

// ---------------------------------------------------------------------------
// Image attachments (read on image files, multimodal tool results).
// ---------------------------------------------------------------------------

// pngBytes renders a small real PNG so tests exercise true signatures.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSniffImage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"png", pngBytes(t), "image/png"},
		{"jpeg magic, no extension", append([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, make([]byte, 32)...), "image/jpeg"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...), "image/webp"},
		{"bmp", append([]byte("BM\x00\x00\x00\x00"), make([]byte, 16)...), "image/bmp"},
		{"text is not an image", []byte("hello world"), ""},
		{"svg is excluded", []byte("<?xml version=\"1.0\"?><svg xmlns=\"http://www.w3.org/2000/svg\"/>"), ""},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		if got := SniffImage(tt.data); got != tt.want {
			t.Errorf("%s: SniffImage = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestDefaultImageURL(t *testing.T) {
	dir := t.TempDir()
	img := pngBytes(t)

	// A real PNG round-trips into a decodable data: URL.
	p := filepath.Join(dir, "shot.png")
	mustWrite(t, p, string(img))
	url, err := DefaultImageURL(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("url = %q, want %q prefix", url, prefix)
	}
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix)); err != nil || !bytes.Equal(dec, img) {
		t.Errorf("decoded payload does not match file bytes (err %v)", err)
	}

	// Unsupported content (text named .png) errors instead of encoding garbage.
	txt := filepath.Join(dir, "fake.png")
	mustWrite(t, txt, "one\ntwo")
	if _, err := DefaultImageURL(context.Background(), txt); err == nil || !strings.Contains(err.Error(), "not a supported image") {
		t.Errorf("text file err = %v, want unsupported-image error", err)
	}

	// The size cap applies.
	big := append(append([]byte{}, img...), bytes.Repeat([]byte{0}, int(maxImageBytes))...)
	huge := filepath.Join(dir, "huge.png")
	mustWrite(t, huge, string(big))
	if _, err := DefaultImageURL(context.Background(), huge); err == nil || !strings.Contains(err.Error(), "capped at") {
		t.Errorf("oversized err = %v, want cap error", err)
	}
}

func TestReadImageTool(t *testing.T) {
	img := pngBytes(t)
	p := filepath.Join(t.TempDir(), "shot.png")
	mustWrite(t, p, string(img))

	// A read tool with a wired transport: the URL it returns is attached
	// verbatim; the result text stays a one-line summary (no URL or
	// base64 in it).
	var got []string
	ctx := WithAttachments(context.Background(), func(u string) {
		got = append(got, u)
	})
	upload := func(ctx context.Context, path string) (string, error) {
		return "https://example.com/" + filepath.Base(path), nil
	}
	tool := NewReadTool(upload)
	out, err := tool.Handler(ctx, map[string]any{"path": p})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s: image/png image, %d bytes (image attached)", p, len(img))
	if out != want {
		t.Errorf("read (image) = %q, want %q", out, want)
	}
	if want := []string{"https://example.com/shot.png"}; !reflect.DeepEqual(got, want) {
		t.Errorf("attachments = %v, want %v", got, want)
	}

	// nil transport (attachments disabled): the summary degrades
	// gracefully instead of failing.
	out, err = NewReadTool(nil).Handler(ctx, map[string]any{"path": p})
	if err != nil {
		t.Fatal(err)
	}
	want = fmt.Sprintf("%s: image/png image, %d bytes (image attachments not supported here)", p, len(img))
	if out != want {
		t.Errorf("read (image, nil transport) = %q, want %q", out, want)
	}

	// A transport but no attachment collector (handler invoked outside a
	// run): the same graceful degradation.
	out, err = tool.Handler(context.Background(), map[string]any{"path": p})
	if err != nil {
		t.Fatal(err)
	}
	if out != want {
		t.Errorf("read (image, no collector) = %q, want %q", out, want)
	}

	// A failing transport surfaces its error to the model and attaches nothing.
	failTool := NewReadTool(func(ctx context.Context, path string) (string, error) {
		return "", fmt.Errorf("s3 put failed")
	})
	failCtx := WithAttachments(context.Background(), func(u string) {
		t.Error("collector called despite transport failure")
	})
	if _, err := failTool.Handler(failCtx, map[string]any{"path": p}); err == nil || !strings.Contains(err.Error(), "image transport failed") {
		t.Errorf("transport failure err = %v, want image transport failed", err)
	}

	// A text file with an image extension stays readable as text.
	fake := filepath.Join(filepath.Dir(p), "fake.png")
	mustWrite(t, fake, "one\ntwo")
	textCtx := WithAttachments(context.Background(), func(u string) {})
	out, err = NewReadTool(nil).Handler(textCtx, map[string]any{"path": fake})
	if err != nil {
		t.Fatal(err)
	}
	if want := fake + ": 2 lines, 7 bytes\n1| one\n2| two\n"; out != want {
		t.Errorf("read (fake.png) = %q, want %q", out, want)
	}
}

func TestReadImageTooLarge(t *testing.T) {
	tool := toolByName(t, "read")
	big := append(append([]byte{}, pngBytes(t)...), bytes.Repeat([]byte{0}, int(maxImageBytes))...)
	p := filepath.Join(t.TempDir(), "huge.png")
	mustWrite(t, p, string(big))
	_, err := tool.Handler(context.Background(), map[string]any{"path": p})
	if err == nil || !strings.Contains(err.Error(), "capped at") {
		t.Errorf("oversized image err = %v, want cap error", err)
	}
}

func TestEditTool(t *testing.T) {
	tool := toolByName(t, "edit")
	p := filepath.Join(t.TempDir(), "f.txt")
	mustWrite(t, p, "alpha\nbeta\nalpha\n")

	var displays []string
	ctx := WithDisplay(context.Background(), func(s string) { displays = append(displays, s) })

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
	mustWrite(t, filepath.Join(tmp, "x.go"), "package x")
	mustMkdir(t, filepath.Join(tmp, "sub"))
	mustWrite(t, filepath.Join(tmp, "sub", "y.go"), "package y")

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

	// No .log files exist anywhere in the tree.
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
	mustWrite(t, filepath.Join(tmp, "a.txt"), "hello world\nHELLO again\nbye\n")
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

// ---------------------------------------------------------------------------
// System-prompt resolution.
// ---------------------------------------------------------------------------

// TestResolveSystemPrompt pins the fallback for callers that make the
// standing instructions configurable: unset or blank means nemo's
// default, anything else passes through, and both come out grounded
// with workspace facts.
func TestResolveSystemPrompt(t *testing.T) {
	def := ResolveSystemPrompt("")
	if !strings.HasPrefix(def, DefaultSystemPrompt) {
		t.Errorf("empty base should use DefaultSystemPrompt, got %.80q...", def)
	}
	if blank := ResolveSystemPrompt(" \n\t"); blank != def {
		t.Errorf("blank base should match empty base, got %.80q...", blank)
	}

	custom := ResolveSystemPrompt("You are terse.")
	if !strings.HasPrefix(custom, "You are terse.") {
		t.Errorf("custom base should pass through, got %.80q...", custom)
	}
	if strings.Contains(custom, DefaultSystemPrompt) {
		t.Error("custom base should replace, not append, DefaultSystemPrompt")
	}

	for name, p := range map[string]string{"default": def, "custom": custom} {
		if !strings.Contains(p, "\n\nCurrent working directory: ") ||
			!strings.Contains(p, "\nPlatform: ") || !strings.Contains(p, "\nDate: ") {
			t.Errorf("%s prompt lacks workspace grounding: %.120q...", name, p)
		}
	}
}

// ---------------------------------------------------------------------------
// buildRequestBody (Agent request assembly).
// ---------------------------------------------------------------------------

// TestBuildRequestBody pins that optional request knobs ride along only
// when set, so a zero-config agent produces the same payload as before
// temperature/max_tokens existed.
func TestBuildRequestBody(t *testing.T) {
	temp := 0.7
	zero := 0.0
	msgs := []map[string]any{{"role": "user", "content": "hi"}}
	specs := []map[string]any{{"type": "function"}}

	// Minimal agent: base fields present, optional fields omitted.
	body := buildRequestBody(&Agent{Model: "m"}, msgs, specs)
	for _, k := range []string{"model", "messages", "tools", "tool_choice"} {
		if _, ok := body[k]; !ok {
			t.Errorf("minimal agent: %s missing from request body", k)
		}
	}
	for _, k := range []string{"provider", "temperature", "max_tokens"} {
		if _, ok := body[k]; ok {
			t.Errorf("minimal agent: %s should be omitted", k)
		}
	}
	if body["model"] != "m" || body["tool_choice"] != "auto" {
		t.Errorf("minimal agent: got %#v", body)
	}

	// Everything set: values pass through verbatim.
	body = buildRequestBody(&Agent{
		Model:       "m",
		Provider:    map[string]any{"order": []string{"DeepSeek"}},
		Temperature: &temp,
		MaxTokens:   4096,
	}, msgs, specs)
	if _, ok := body["provider"]; !ok {
		t.Error("provider set: missing from request body")
	}
	if got, ok := body["temperature"].(float64); !ok || got != temp {
		t.Errorf("temperature set: got %#v, want %v", body["temperature"], temp)
	}
	if got, ok := body["max_tokens"].(int); !ok || got != 4096 {
		t.Errorf("max_tokens set: got %#v, want 4096", body["max_tokens"])
	}

	// A zero temperature is an explicit request (nil is the unset case);
	// zero max_tokens still means "unset", not a request for no tokens.
	body = buildRequestBody(&Agent{Model: "m", Temperature: &zero, MaxTokens: 0}, msgs, specs)
	if got, ok := body["temperature"].(float64); !ok || got != 0 {
		t.Errorf("temperature 0 should still be sent, got %#v", body["temperature"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Error("max_tokens = 0 should be omitted")
	}
}

// ---------------------------------------------------------------------------
// Optional network spike: image parts in tool messages (#15).
// ---------------------------------------------------------------------------

// TestSpikeToolImageTransport checks that the configured endpoint accepts
// image_url content parts inside a tool message (the #15 wire format) and
// that the model actually sees the image. It costs one real LLM call, so
// it is skipped unless opted in:
//
//	NEMO_SPIKE=1 OPENROUTER_API_KEY=... go test -run TestSpikeToolImageTransport -v
//
// NEMO_SPIKE_MODEL overrides the model (default: a vision-capable one —
// the default DeepSeek flash model is text-only and will fail).
func TestSpikeToolImageTransport(t *testing.T) {
	if os.Getenv("NEMO_SPIKE") != "1" {
		t.Skip("set NEMO_SPIKE=1 (and OPENROUTER_API_KEY) to run the image-transport spike")
	}
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	model := os.Getenv("NEMO_SPIKE_MODEL")
	if model == "" {
		model = "google/gemini-2.5-flash"
	}
	imgURL, err := DefaultImageURL(context.Background(), "table.png")
	if err != nil {
		t.Skipf("table.png not usable: %v", err)
	}

	// A history that looks like a read call on the image just happened.
	s := NewSession()
	s.Messages = []map[string]any{
		{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id":   "c1",
				"type": "function",
				"function": map[string]any{
					"name":      "read",
					"arguments": `{"path":"table.png"}`,
				},
			}},
		},
		toolMessage("c1", "table.png: image attached", []string{imgURL}),
	}

	ag := &Agent{
		Name:         "spike",
		Endpoint:     "https://openrouter.ai/api/v1/chat/completions",
		APIKey:       apiKey,
		Model:        model,
		Tools:        DefaultTools(DefaultImageURL, nil),
		SystemPrompt: DefaultSystemPrompt,
	}
	var reply strings.Builder
	err = ag.Run(context.Background(), s,
		"What does the image returned by the read tool show? Answer in one sentence.",
		nil,
		func(c string) { reply.WriteString(c) },
		func(sys string) { t.Log("[system] " + sys) },
		nil,
		nil)
	if err != nil {
		t.Fatalf("endpoint rejected image-in-tool-message for %s: %v", model, err)
	}
	t.Logf("%s reply: %s", model, reply.String())
	if reply.Len() == 0 {
		t.Fatal("empty reply")
	}
}

// ---------------------------------------------------------------------------
// Subagent tool: argument validation the agent contract, child
// construction, conversation isolation, recursion prevention, execution
// limits (timeout, iteration cap), error shaping, and the parent receiving
// the subagent's final answer through one tool call.
//
// The endpoint is a fake OpenAI-compatible streaming server running a
// canned script: each request pops the next scripted assistant reply and
// streams it as SSE, exactly what llmCallStream consumes. Requests are
// recorded so tests can assert what the parent and the child each sent.
// ---------------------------------------------------------------------------

// scriptedReply is one canned assistant reply.
type scriptedReply struct {
	content string        // plain content reply ("" = none)
	tool    string        // tool-call reply: function name...
	toolArg string        // ...and its raw JSON arguments
	status  int           // non-zero: respond with this HTTP status instead
	hang    time.Duration // non-zero: sleep before responding
}

// scriptServer is an httptest server speaking just enough of the
// OpenAI-compatible streaming protocol: one scripted reply per request
// (the last entry repeats when the script runs out), every request body
// recorded for later assertions.
type scriptServer struct {
	mu       sync.Mutex
	script   []scriptedReply
	requests []map[string]any
	srv      *httptest.Server
}

// newScriptServer starts a server on /chat/completions running script.
func newScriptServer(t *testing.T, script ...scriptedReply) *scriptServer {
	t.Helper()
	s := &scriptServer{script: script}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			t.Errorf("script server: bad request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, req)
		i := len(s.requests) - 1
		if i >= len(s.script) {
			i = len(s.script) - 1
		}
		reply := s.script[i]
		s.mu.Unlock()

		if reply.hang > 0 {
			// Abort the hang when the client goes away so server cleanup
			// (Close waits for handlers) is not blocked for the full
			// sleep.
			select {
			case <-time.After(reply.hang):
			case <-r.Context().Done():
				return
			}
		}
		if reply.status != 0 {
			// Retry-After: 0 keeps llmCallStream's backoff from making
			// failure tests slow.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(reply.status)
			fmt.Fprintln(w, `{"error":{"message":"scripted failure"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")

		write := func(v any) {
			b, err := json.Marshal(v)
			if err != nil {
				t.Errorf("script server: marshal chunk: %v", err)
				return
			}
			fmt.Fprintf(w, "data: %s\n", b)
		}
		switch {
		case reply.tool != "":
			write(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "c1", "type": "function",
					"function": map[string]any{"name": reply.tool, "arguments": reply.toolArg},
				}}},
			}}})
		default:
			write(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": reply.content},
			}}})
		}
		write(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "stop",
		}}})
		fmt.Fprint(w, "data: [DONE]\n")
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// endpoint returns the chat-completions URL.
func (s *scriptServer) endpoint() string { return s.srv.URL + "/chat/completions" }

// reqCount returns how many requests were recorded.
func (s *scriptServer) reqCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// request returns a recorded request body.
func (s *scriptServer) request(t *testing.T, i int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.requests) {
		t.Fatalf("request %d not recorded (%d total)", i, len(s.requests))
	}
	return s.requests[i]
}

// reqMessages returns a recorded request's messages.
func reqMessages(t *testing.T, req map[string]any) []map[string]any {
	t.Helper()
	raw, ok := req["messages"].([]any)
	if !ok {
		t.Fatalf("request has no messages: %v", req["messages"])
	}
	msgs := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		msg, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message is not an object: %#v", m)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// reqToolNames returns the function names in a request's tool specs.
func reqToolNames(t *testing.T, req map[string]any) []string {
	t.Helper()
	raw, ok := req["tools"].([]any)
	if !ok {
		t.Fatalf("request has no tools: %v", req["tools"])
	}
	var names []string
	for _, s := range raw {
		spec, _ := s.(map[string]any)
		fn, _ := spec["function"].(map[string]any)
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

// asSubagentError asserts err is a *SubagentError with the given stage
// and returns it.
func asSubagentError(t *testing.T, err error, stage string) *SubagentError {
	t.Helper()
	var se *SubagentError
	if !errors.As(err, &se) {
		t.Fatalf("error %v (%T) is not a *SubagentError", err, err)
	}
	if se.Stage != stage {
		t.Errorf("stage = %q, want %q", se.Stage, stage)
	}
	return se
}

// newNoteTool returns a tool that records each invocation's text; the
// subagent gets it in its registry, so tests can observe it actually
// using its tools inside its own exchange.
func newNoteTool(t *testing.T) (Tool, *[]string) {
	t.Helper()
	var calls []string
	return Tool{
		Name:        "note",
		Description: "record text",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			calls = append(calls, args["text"].(string))
			return "noted", nil
		},
	}, &calls
}

// ---------------------------------------------------------------------------
// Argument validation.
// ---------------------------------------------------------------------------

func TestSubagentArgumentValidation(t *testing.T) {
	srv := newScriptServer(t, scriptedReply{content: "unused"})
	tool := NewSubagentTool(&Agent{Endpoint: srv.endpoint(), Model: "m", APIKey: "k"})
	ctx := context.Background()

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing task", map[string]any{}, "task is required"},
		{"empty task", map[string]any{"task": "  "}, "task is required"},
		{"non-string task", map[string]any{"task": float64(42)}, "task is required"},
	}
	for _, tt := range tests {
		_, err := tool.Handler(ctx, tt.args)
		if err == nil || err.Error() != tt.want {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
	if srv.reqCount() != 0 {
		t.Errorf("validation failures must not reach the endpoint; got %d requests", srv.reqCount())
	}
}

// ---------------------------------------------------------------------------
// The agent is used as-is.
// ---------------------------------------------------------------------------

func TestSubagentUsesAgentAsIs(t *testing.T) {
	// Whatever the caller constructed — name, model, prompt, leash,
	// tools — is sent verbatim; the tool adds and strips nothing.
	srv := newScriptServer(t, scriptedReply{content: "ok"})
	temp := 0.3
	note, _ := newNoteTool(t)
	tool := NewSubagentTool(&Agent{
		Name:              "researcher",
		Endpoint:          srv.endpoint(),
		APIKey:            "k",
		Model:             "child-model",
		Provider:          map[string]any{"order": []string{"DeepSeek"}},
		Temperature:       &temp,
		MaxTokens:         128,
		MaxToolIterations: 7,
		SystemPrompt:      "You are a researcher.",
		Tools:             []Tool{note},
	})
	if _, err := tool.Handler(context.Background(), map[string]any{"task": "t"}); err != nil {
		t.Fatal(err)
	}

	req := srv.request(t, 0)
	if req["model"] != "child-model" {
		t.Errorf("child model = %v, want child-model (as constructed)", req["model"])
	}
	if got, ok := req["temperature"].(float64); !ok || got != temp {
		t.Errorf("child temperature = %#v, want %v", req["temperature"], temp)
	}
	if got, ok := req["max_tokens"].(float64); !ok || got != 128 {
		t.Errorf("child max_tokens = %#v, want 128", req["max_tokens"])
	}
	if _, ok := req["provider"]; !ok {
		t.Error("child request lacks the agent's provider hints")
	}
	msgs := reqMessages(t, req)
	sys, _ := msgs[0]["content"].(string)
	if sys != "You are a researcher." {
		t.Errorf("child system prompt = %q, want it verbatim (no addendum, no fallback)", sys)
	}
	if want := []string{"note"}; !reflect.DeepEqual(reqToolNames(t, req), want) {
		t.Errorf("child tools = %v, want %v (as constructed)", reqToolNames(t, req), want)
	}
}

func TestSubagentRecursiveRegistryCall(t *testing.T) {
	// The tool does not strip itself from the agent's registry — that is
	// the caller's wiring decision — but a recursive call is still
	// stopped by the context marker, before any model call.
	srv := newScriptServer(t, scriptedReply{content: "unused"})
	note, _ := newNoteTool(t)
	// A mis-wired agent: its registry carries the delegation tool.
	inner := &Agent{Endpoint: srv.endpoint(), APIKey: "k", Model: "m", Tools: []Tool{note}}
	inner.Tools = append(inner.Tools, NewSubagentTool(inner))
	tool := NewSubagentTool(inner)

	ctx := context.WithValue(context.Background(), subagentContextKey{}, true)
	_, err := tool.Handler(ctx, map[string]any{"task": "nested"})
	se := asSubagentError(t, err, "init")
	if !strings.Contains(se.Err.Error(), "recursive") {
		t.Errorf("init error = %v, want recursive-delegation message", se.Err)
	}
	if srv.reqCount() != 0 {
		t.Errorf("recursion refusal must not reach the endpoint; got %d requests", srv.reqCount())
	}
}

func TestSubagentReusable(t *testing.T) {
	// The tool can be used for repeated delegations: each one runs on a
	// fresh session and reuses the agent's static configuration
	// untouched.
	srv := newScriptServer(t, scriptedReply{content: "ok"})
	ag := &Agent{Endpoint: srv.endpoint(), APIKey: "k", Model: "m", SystemPrompt: "Standing."}
	tool := NewSubagentTool(ag)
	for i := 0; i < 2; i++ {
		out, err := tool.Handler(context.Background(), map[string]any{"task": "t"})
		if err != nil || out != "ok" {
			t.Fatalf("delegation %d = (%q, %v), want (ok, nil)", i, out, err)
		}
	}
	if got := srv.reqCount(); got != 2 {
		t.Fatalf("%d requests, want 2 (one per delegation, fresh history each)", got)
	}
	sys, _ := reqMessages(t, srv.request(t, 1))[0]["content"].(string)
	if sys != "Standing." {
		t.Errorf("second delegation prompt = %q, want the agent's prompt unchanged", sys)
	}
}

// ---------------------------------------------------------------------------
// Successful delegation.
// ---------------------------------------------------------------------------

func TestSubagentSuccess(t *testing.T) {
	note, noted := newNoteTool(t)
	// Child script: one tool call, then the final answer.
	srv := newScriptServer(t,
		scriptedReply{tool: "note", toolArg: `{"text":"working on it"}`},
		scriptedReply{content: "The answer is 42."},
	)
	tool := NewSubagentTool(&Agent{
		Name:         "researcher",
		Endpoint:     srv.endpoint(),
		APIKey:       "k",
		Model:        "m",
		SystemPrompt: "You are a researcher.",
		Tools:        []Tool{note},
	})

	out, err := tool.Handler(context.Background(), map[string]any{
		"task":    "do the thing",
		"context": "the widget lives in widget.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "The answer is 42." {
		t.Errorf("result = %q, want the subagent's final reply", out)
	}

	// The subagent really ran its own tool loop.
	if want := []string{"working on it"}; !reflect.DeepEqual(*noted, want) {
		t.Errorf("subagent tool calls = %v, want %v", *noted, want)
	}
	if got := srv.reqCount(); got != 2 {
		t.Fatalf("child made %d requests, want 2", got)
	}

	// First child request: fresh history. The agent carries a system
	// prompt here, so it is [system, user]; the task and context are in
	// the opening message.
	first := reqMessages(t, srv.request(t, 0))
	if len(first) != 2 || first[0]["role"] != "system" || first[1]["role"] != "user" {
		t.Fatalf("child history = %v, want [system, user]", first)
	}
	user, _ := first[1]["content"].(string)
	if !strings.Contains(user, "do the thing") || !strings.Contains(user, "the widget lives in widget.go") {
		t.Errorf("child opening message = %q, want task and context", user)
	}

	// Second child request: the tool result is in its history.
	second := reqMessages(t, srv.request(t, 1))
	toolMsgs := 0
	for _, m := range second {
		if m["role"] == "tool" {
			toolMsgs++
			if m["content"] != "noted" {
				t.Errorf("tool message content = %v, want noted", m["content"])
			}
		}
	}
	if toolMsgs != 1 {
		t.Errorf("second child request carries %d tool messages, want 1", toolMsgs)
	}
}

func TestSubagentContextOptional(t *testing.T) {
	srv := newScriptServer(t, scriptedReply{content: "done"})
	tool := NewSubagentTool(&Agent{Endpoint: srv.endpoint(), APIKey: "k", Model: "m", SystemPrompt: "S."})

	out, err := tool.Handler(context.Background(), map[string]any{"task": "just this"})
	if err != nil || out != "done" {
		t.Fatalf("handler = (%q, %v), want (done, nil)", out, err)
	}
	user, _ := reqMessages(t, srv.request(t, 0))[1]["content"].(string)
	if strings.Contains(user, "Context:") {
		t.Errorf("opening message mentions context although none was given: %q", user)
	}
	if !strings.Contains(user, "just this") {
		t.Errorf("opening message lacks the task: %q", user)
	}
}

// ---------------------------------------------------------------------------
// Conversation isolation.
// ---------------------------------------------------------------------------

func TestSubagentIsolatedConversation(t *testing.T) {
	srv := newScriptServer(t,
		scriptedReply{tool: "subagent", toolArg: `{"task":"inner task"}`}, // parent delegates
		scriptedReply{tool: "note", toolArg: `{"text":"working on it"}`},  // child calls its tool
		scriptedReply{content: "inner answer"},                            // child answers
		scriptedReply{content: "outer answer"},                            // parent answers
	)
	note, noted := newNoteTool(t)
	ag := &Agent{
		Endpoint: srv.endpoint(),
		APIKey:   "k",
		Model:    "m",
		Tools:    []Tool{note},
		SystemPrompt: "Parent standing instructions that the child must not " +
			"receive: it has its own agent.",
	}
	// The subagent is a separately constructed agent: the parent's model
	// and workspace tools, but never the parent agent or its session.
	sub := &Agent{
		Endpoint:     srv.endpoint(),
		APIKey:       "k",
		Model:        "m",
		Tools:        []Tool{note},
		SystemPrompt: ag.SystemPrompt,
	}
	ag.Tools = append(ag.Tools, NewSubagentTool(sub))

	parent := NewSession()
	parent.Messages = []map[string]any{{"role": "user", "content": "earlier"}}

	err := ag.Run(context.Background(), parent, "delegate this",
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *noted == nil {
		t.Fatal("subagent never ran its tool")
	}

	// Request order (global across both agents): 0 parent turn (tool
	// call), 1-2 child (tool call, answer), 3 parent follow-up.
	if got := srv.reqCount(); got != 4 {
		t.Fatalf("%d requests, want 4 (parent, child, child, parent)", got)
	}

	// The parent's history holds exactly its own exchange: the earlier
	// message, this turn's user message (no system seeding — the session
	// was not empty), the subagent tool call, its result, and the final
	// reply. None of the child's messages.
	roles := make([]string, 0, len(parent.Messages))
	for _, m := range parent.Messages {
		roles = append(roles, fmt.Sprint(m["role"]))
	}
	want := []string{"user", "user", "assistant", "tool", "assistant"}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("parent roles = %v, want %v", roles, want)
	}

	// The delegation result in the parent's history is the child's final
	// answer — and the child's intermediate exchange (its note call,
	// its reasoning) appears nowhere in the parent's history.
	var toolContent string
	for _, m := range parent.Messages {
		if m["role"] == "tool" {
			toolContent, _ = m["content"].(string)
		}
		if strings.Contains(fmt.Sprint(m["content"]), "working on it") {
			t.Errorf("child's intermediate tool exchange leaked into parent history: %v", m)
		}
	}
	if toolContent != "inner answer" {
		t.Errorf("parent tool message = %q, want the child's final answer", toolContent)
	}

	// The child's request started from an empty history with its own
	// agent's system prompt: system + user only.
	child := reqMessages(t, srv.request(t, 1))
	if len(child) != 2 {
		t.Fatalf("child history has %d messages, want a fresh [system, user]", len(child))
	}
	sys, _ := child[0]["content"].(string)
	if sys != ag.SystemPrompt {
		t.Errorf("child system prompt = %q, want the subagent agent's own prompt verbatim", sys)
	}
	// No message from the parent's session rides along.
	for _, m := range child {
		if c, _ := m["content"].(string); c == "earlier" || c == "delegate this" {
			t.Errorf("parent conversation leaked into the child: %v", m)
		}
	}

	// The parent's follow-up request carried the tool result but not the
	// child's exchange.
	third := reqMessages(t, srv.request(t, 3))
	if len(third) != 4 {
		t.Errorf("parent follow-up has %d messages, want 4 (its own exchange only)", len(third))
	}
}

// ---------------------------------------------------------------------------
// Execution limits.
// ---------------------------------------------------------------------------

func TestSubagentTimeout(t *testing.T) {
	// The endpoint hangs; the delegation's own window (or a shorter
	// parent deadline, as here) must surface as a timeout error.
	srv := newScriptServer(t, scriptedReply{hang: 2 * time.Second})
	tool := NewSubagentTool(&Agent{Endpoint: srv.endpoint(), APIKey: "k", Model: "m"})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tool.Handler(ctx, map[string]any{"task": "slow"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hanging subagent should fail, not block the caller")
	}
	if elapsed > time.Second {
		t.Errorf("handler took %v; cancellation should propagate promptly", elapsed)
	}
	asSubagentError(t, err, "timeout")
}

func TestSubagentMaxIterations(t *testing.T) {
	// The child never stops calling tools; its configured leash (the
	// caller's wiring decision) must end the delegation with an
	// actionable error the parent can match.
	srv := newScriptServer(t, scriptedReply{tool: "note", toolArg: `{"text":"again"}`})
	note, _ := newNoteTool(t)
	tool := NewSubagentTool(&Agent{
		Endpoint:          srv.endpoint(),
		APIKey:            "k",
		Model:             "m",
		Tools:             []Tool{note},
		MaxToolIterations: 3,
	})

	_, err := tool.Handler(context.Background(), map[string]any{"task": "loop forever"})
	if err == nil {
		t.Fatal("runaway subagent should hit its configured cap")
	}
	se := asSubagentError(t, err, "run")
	if !errors.Is(se.Err, ErrMaxIterations) {
		t.Errorf("run error = %v, want ErrMaxIterations (errors.Is)", se.Err)
	}
	if got := srv.reqCount(); got != 3 {
		t.Errorf("child made %d requests, want 3 (configured leash)", got)
	}
}

// ---------------------------------------------------------------------------
// Failure reporting.
// ---------------------------------------------------------------------------

func TestSubagentInitFailures(t *testing.T) {
	srv := newScriptServer(t, scriptedReply{content: "unused"})
	args := map[string]any{"task": "t"}
	ctx := context.Background()

	// Nil agent (miswired constructor).
	_, err := NewSubagentTool(nil).Handler(ctx, args)
	if se := asSubagentError(t, err, "init"); !strings.Contains(se.Err.Error(), "nil") {
		t.Errorf("nil-agent failure = %v, want nil-agent message", se.Err)
	}

	// Missing endpoint.
	_, err = NewSubagentTool(&Agent{Model: "m"}).Handler(ctx, args)
	if se := asSubagentError(t, err, "init"); !strings.Contains(se.Err.Error(), "endpoint") {
		t.Errorf("endpoint failure = %v, want endpoint message", se.Err)
	}

	// Missing model.
	_, err = NewSubagentTool(&Agent{Endpoint: srv.endpoint()}).Handler(ctx, args)
	if se := asSubagentError(t, err, "init"); !strings.Contains(se.Err.Error(), "model") {
		t.Errorf("model failure = %v, want model message", se.Err)
	}

	if srv.reqCount() != 0 {
		t.Errorf("init failures must not reach the endpoint; got %d requests", srv.reqCount())
	}
}

func TestSubagentUpstreamFailure(t *testing.T) {
	srv := newScriptServer(t, scriptedReply{status: http.StatusBadRequest})
	tool := NewSubagentTool(&Agent{Endpoint: srv.endpoint(), APIKey: "k", Model: "m"})

	_, err := tool.Handler(context.Background(), map[string]any{"task": "t"})
	if err == nil {
		t.Fatal("upstream failure should fail the delegation")
	}
	se := asSubagentError(t, err, "run")
	if !strings.Contains(se.Err.Error(), "scripted failure") {
		t.Errorf("run error = %v, want the upstream message carried through", se.Err)
	}
}

func TestSubagentEmptyFinalAnswer(t *testing.T) {
	// The child finishes "successfully" with only whitespace to show: an
	// empty answer is a failure the parent can act on, not an empty
	// success.
	srv := newScriptServer(t, scriptedReply{content: "   "})
	tool := NewSubagentTool(&Agent{Endpoint: srv.endpoint(), APIKey: "k", Model: "m"})

	out, err := tool.Handler(context.Background(), map[string]any{"task": "t"})
	if err == nil || out != "" {
		t.Fatalf("whitespace answer = (%q, %v), want an error", out, err)
	}
	se := asSubagentError(t, err, "run")
	if !strings.Contains(se.Err.Error(), "empty final answer") {
		t.Errorf("run error = %v, want empty-final-answer message", se.Err)
	}
}

// ---------------------------------------------------------------------------
// End to end: the parent delegates through one tool call and uses the
// result in its own final reply.
// ---------------------------------------------------------------------------

func TestSubagentParentRoundTrip(t *testing.T) {
	// Chronological script across both agents:
	//   1. parent request -> call subagent with a task
	//   2. child request  -> final answer "3 files"
	//   3. parent request -> final reply built on that answer
	srv := newScriptServer(t,
		scriptedReply{tool: "subagent", toolArg: `{"task":"count the files","context":"in /tmp"}`},
		scriptedReply{content: "3 files"},
		scriptedReply{content: "The subagent counted 3 files."},
	)
	note, _ := newNoteTool(t)
	ag := &Agent{
		Name:     "root",
		Endpoint: srv.endpoint(),
		APIKey:   "k",
		Model:    "m",
		Tools:    []Tool{note},
	}
	// A separately constructed subagent agent whose registry simply has
	// no delegation tool — recursion stays impossible by construction.
	sub := &Agent{
		Endpoint: srv.endpoint(),
		APIKey:   "k",
		Model:    "m",
		Tools:    []Tool{note},
	}
	ag.Tools = append(ag.Tools, NewSubagentTool(sub))

	s := NewSession()
	var reply strings.Builder
	err := ag.Run(context.Background(), s, "how many files are in /tmp?",
		nil,
		func(c string) { reply.WriteString(c) },
		nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reply.String(), "The subagent counted 3 files."; got != want {
		t.Errorf("parent reply = %q, want %q", got, want)
	}

	// Three requests total: parent, child, parent.
	if got := srv.reqCount(); got != 3 {
		t.Fatalf("%d requests, want 3 (parent, child, parent)", got)
	}

	// The parent advertises the delegation tool; the child must not.
	if names := reqToolNames(t, srv.request(t, 0)); !contains(names, "subagent") {
		t.Errorf("parent tools = %v, want the delegation tool present", names)
	}
	if names := reqToolNames(t, srv.request(t, 1)); contains(names, "subagent") {
		t.Errorf("child tools = %v, delegation tool must be absent", names)
	}

	// The delegation arguments the model sent reached the child intact
	// (the subagent carries no system prompt, so its history opens with
	// the user message).
	childUser, _ := reqMessages(t, srv.request(t, 1))[0]["content"].(string)
	if !strings.Contains(childUser, "count the files") || !strings.Contains(childUser, "in /tmp") {
		t.Errorf("child task message = %q, want task and context passed through", childUser)
	}

	// The parent's follow-up carried the child's answer as the tool
	// result, which is how the model "used" it in its final reply.
	var toolResult string
	for _, m := range reqMessages(t, srv.request(t, 2)) {
		if m["role"] == "tool" {
			toolResult, _ = m["content"].(string)
		}
	}
	if toolResult != "3 files" {
		t.Errorf("parent tool result = %q, want the child's final answer", toolResult)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
