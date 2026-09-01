package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

/*
stolen from the picoclaw repo
*/
var (
	// Pre-compiled regexes for HTML text extraction
	reScript     = regexp.MustCompile(`<script[\s\S]*?</script>`)
	reStyle      = regexp.MustCompile(`<style[\s\S]*?</style>`)
	reTags       = regexp.MustCompile(`<[^>]+>`)
	reWhitespace = regexp.MustCompile(`[^\S\n]+`)
	reBlankLines = regexp.MustCompile(`\n{3,}`)
	// DuckDuckGo result extraction
	reDDGLink    = regexp.MustCompile(`<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>([\s\S]*?)</a>`)
	reDDGSnippet = regexp.MustCompile(`<a class="result__snippet[^"]*".*?>([\s\S]*?)</a>`)
)

func extractText(htmlContent string) string {
	result := reScript.ReplaceAllLiteralString(htmlContent, "")
	result = reStyle.ReplaceAllLiteralString(result, "")
	result = reTags.ReplaceAllLiteralString(result, "")

	result = strings.TrimSpace(result)

	result = reWhitespace.ReplaceAllString(result, " ")
	result = reBlankLines.ReplaceAllString(result, "\n\n")

	lines := strings.Split(result, "\n")
	var cleanLines []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleanLines = append(cleanLines, line)
		}
	}

	return strings.Join(cleanLines, "\n")
}

// Timeouts and size limits for outbound requests.
const (
	// llmTimeout caps one LLM round trip.
	llmTimeout = 32 * time.Second
	// llmStreamTimeout caps one streaming LLM round trip. Streaming needs more
	// headroom than a one-shot call: thinking models can emit nothing for a
	// while before the first token, and [DONE] only arrives at the very end.
	llmStreamTimeout = 5 * time.Minute
	// fetchTimeout caps one tool fetch.
	fetchTimeout = 15 * time.Second
	// runTimeout bounds the entire run, so a wedged run cannot hang forever.
	runTimeout = 10 * time.Minute

	// maxLLMResponseBytes caps one LLM response body.
	maxLLMResponseBytes int64 = 1 << 20
	// maxToolResponseBytes is the hard cap (and default) for tool fetch
	// bodies; model-supplied max_bytes can only lower it.
	maxToolResponseBytes int64 = 2 << 20
	// maxToolResultBytes caps a tool result fed back into the conversation.
	maxToolResultBytes = 64 << 10

	// maxReadLines and maxReadBytes cap read output so a huge file cannot
	// flood the context; maxReadBytes leaves room for the continuation
	// footer under maxToolResultBytes. (Explicit types: inside a const
	// block an omitted type would inherit int64 from maxToolResultBytes.)
	maxReadLines int   = 2000
	maxReadBytes int64 = 48 << 10

	// maxRedirects bounds redirect chains.
	maxRedirects = 10

	// maxRetries bounds transient LLM call attempts (network errors, 429/5xx).
	maxRetries = 3
	// maxBackoff caps the sleep between LLM retries.
	maxBackoff = 5 * time.Second
)

/*
tool defs
*/

// systemPrompt is nemo's standing instruction to the model.
const systemPrompt = "You are an expert coding assistant operating inside `nemo`, a coding agent harness. " +
	"You help users by reading files, executing commands, editing code, and writing new files.\n\n" +
	"Guidelines:\n" +
	"* be minimal and brief\n" +
	"* show file paths clearly when working with files\n" +
	"* be mindful with destructive and irreversible actions\n" +
	"* ask the user to clarify intent if there is uncertainty"

// groundedPrompt appends workspace facts (cwd, platform, today's date) to
// the base instructions so the model knows where and when it operates.
// Computed once per process; a multi-hour session may see a stale date.
func groundedPrompt(base string) string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = fmt.Sprintf("(unknown: %v)", err)
	}
	return base +
		"\n\nCurrent working directory: " + cwd +
		"\nPlatform: " + runtime.GOOS + "/" + runtime.GOARCH +
		"\nDate: " + time.Now().Format("Monday, January 2, 2006")
}

// Tool is a function the model may call; ctx carries the run's deadline
// and cancellation.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Handler     func(ctx context.Context, args map[string]any) (string, error)
}

// walkTree walks root (a file or directory), calling fn for each file.
// Unreadable entries are skipped.
func walkTree(root string, fn func(path string)) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		fn(root)
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() {
			fn(p)
		}
		return nil
	})
}

// defaultTools is the tool set the root agent runs with; Agent.Run itself
// is tool-agnostic and takes its tools via the Agent.
var defaultTools = []Tool{
	{
		Name:        "fetch",
		Description: "make an HTTP request to a URL and return the response body or extracted readable content",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type": "string",
				},
				"method": map[string]any{
					"type":    "string",
					"default": "GET",
				},
				"headers": map[string]any{
					"type": "object",
					"additionalProperties": map[string]any{
						"type": "string",
					},
				},
				"body": map[string]any{
					"type": "string",
				},
				"max_bytes": map[string]any{
					"type":        "integer",
					"description": "maximum response size in bytes (default 2097152)",
				},
				"readable": map[string]any{
					"type":        "boolean",
					"description": "light postprocessing to extract readable content by removing script and style tags, etc.",
				},
			},
			"required": []string{"url"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {

			urlStr, ok := args["url"].(string)
			if !ok || urlStr == "" {
				return "", fmt.Errorf("url is required")
			}

			// http/https only.
			u, err := url.Parse(urlStr)
			if err != nil {
				return "", fmt.Errorf("invalid url: %w", err)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return "", fmt.Errorf("scheme %q not allowed (http/https only)", u.Scheme)
			}

			method := "GET"
			if m, ok := args["method"].(string); ok && m != "" {
				method = strings.ToUpper(m)
			}

			var body io.Reader
			if b, ok := args["body"].(string); ok && b != "" {
				body = strings.NewReader(b)
			}

			reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(reqCtx, method, urlStr, body)
			if err != nil {
				return "", err
			}

			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

			if h, ok := args["headers"].(map[string]any); ok {
				for k, v := range h {
					if vs, ok := v.(string); ok {
						req.Header.Set(k, vs)
					}
				}
			}

			res, err := httpClient.Do(req)
			if err != nil {
				return "", err
			}
			defer res.Body.Close()

			// Clamp the model-supplied cap; only values below the hard cap
			// are honored.
			maxBytes := maxToolResponseBytes
			if mb, ok := args["max_bytes"].(float64); ok && mb > 0 && mb < float64(maxToolResponseBytes) {
				maxBytes = int64(mb)
			}

			data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
			if err != nil {
				return "", err
			}

			readable := false
			if r, ok := args["readable"].(bool); ok {
				readable = r
			}

			out := string(data)
			if int64(len(data)) > maxBytes {
				out = truncateUTF8(out, int(maxBytes), "\n...[truncated]")
			}
			if readable {
				out = extractText(out)
			}

			return out, nil
		},
	},

	{
		Name:        "read",
		Description: "read a text file; lines are prefixed with 1-indexed line numbers like \"123| text\" " +
			"(strip that prefix when quoting file text elsewhere, e.g. in edit's old_text); " +
			"offset is the 1-indexed first line, limit caps the line count",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string"},
				"offset": map[string]any{"type": "integer", "description": "1-indexed line to start from"},
				"limit":  map[string]any{"type": "integer", "description": "maximum number of lines to return"},
			},
			"required": []string{"path"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			path, _ := args["path"].(string)
			if path == "" {
				return "", fmt.Errorf("path is required")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if bytes.IndexByte(data, 0) >= 0 {
				return "", fmt.Errorf("%s: binary file", path)
			}
			if len(data) == 0 {
				return fmt.Sprintf("%s: empty file", path), nil
			}
			lines := strings.Split(string(data), "\n")
			total := len(lines)

			start := 0
			if o, ok := args["offset"].(float64); ok && int(o) > 1 {
				start = int(o) - 1
			}
			if start >= total {
				return "", fmt.Errorf("offset %d beyond end of file (%d lines)", start+1, total)
			}
			limit := total - start
			if l, ok := args["limit"].(float64); ok && int(l) > 0 && int(l) < limit {
				limit = int(l)
			}
			if limit > maxReadLines {
				limit = maxReadLines
			}

			// Numbered output, byte-capped below maxToolResultBytes so the
			// continuation footer survives intact. At least one line is
			// always emitted; a pathologically long single line falls
			// through to Run's hard truncation.
			width := len(strconv.Itoa(total))
			var b strings.Builder
			fmt.Fprintf(&b, "%s: %d lines, %d bytes", path, total, len(data))
			if start > 0 {
				fmt.Fprintf(&b, " (from line %d)", start+1)
			}
			b.WriteByte('\n')

			last := start // 0-indexed; will hold the last line written
			for i := start; i < start+limit && i < total; i++ {
				entry := fmt.Sprintf("%*d| %s\n", width, i+1, lines[i])
				if i > start && b.Len()+len(entry) > int(maxReadBytes) {
					break
				}
				b.WriteString(entry)
				last = i
			}
			if last+1 < total {
				fmt.Fprintf(&b, "...[showing lines %d-%d of %d; use offset=%d to continue]\n",
					start+1, last+1, total, last+2)
			}
			return b.String(), nil
		},
	},

	{
		Name:        "write",
		Description: "write content to a file, creating or overwriting it (makes parent directories)",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
		},
	},

	{
		Name:        "edit",
		Description: "replace text in a file; old_text must occur exactly once",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":     map[string]any{"type": "string"},
				"old_text": map[string]any{"type": "string"},
				"new_text": map[string]any{"type": "string"},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			path, _ := args["path"].(string)
			oldText, _ := args["old_text"].(string)
			newText, _ := args["new_text"].(string)
			if path == "" || oldText == "" {
				return "", fmt.Errorf("path and old_text are required")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			n := strings.Count(string(data), oldText)
			if n == 0 {
				return "", fmt.Errorf("old_text not found in %s", path)
			}
			if n > 1 {
				return "", fmt.Errorf("old_text matches %d locations in %s; make it unique", n, path)
			}
			out := strings.Replace(string(data), oldText, newText, 1)
			if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
				return "", err
			}
			return "ok", nil
		},
	},

	{
		Name:        "grep",
		Description: "search file contents for a regex; returns matching lines as path:line: text",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string", "description": "file or directory (default .)"},
				"ignore_case": map[string]any{"type": "boolean"},
			},
			"required": []string{"pattern"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			pattern, _ := args["pattern"].(string)
			if pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			if ic, ok := args["ignore_case"].(bool); ok && ic {
				pattern = "(?i)" + pattern
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return "", fmt.Errorf("invalid pattern: %w", err)
			}
			root := "."
			if p, ok := args["path"].(string); ok && p != "" {
				root = p
			}
			var out []string
			err = walkTree(root, func(path string) {
				if len(out) >= 100 {
					return
				}
				f, err := os.Open(path)
				if err != nil {
					return
				}
				defer f.Close()
				sc := bufio.NewScanner(f)
				for i := 1; sc.Scan(); i++ {
					if re.MatchString(sc.Text()) {
						out = append(out, fmt.Sprintf("%s:%d: %s", path, i, truncateUTF8(sc.Text(), 500, " ...")))
						if len(out) >= 100 {
							out = append(out, "...[truncated at 100 matches]")
							break
						}
					}
				}
			})
			if err != nil {
				return "", err
			}
			if len(out) == 0 {
				return "no matches", nil
			}
			return strings.Join(out, "\n"), nil
		},
	},

	{
		Name:        "find",
		Description: "find files whose name or path matches a glob pattern (e.g. *.go, **/*_test.go)",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
				"path":    map[string]any{"type": "string", "description": "directory to search (default .)"},
			},
			"required": []string{"pattern"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			pattern, _ := args["pattern"].(string)
			if pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			root := "."
			if p, ok := args["path"].(string); ok && p != "" {
				root = p
			}
			trim := strings.TrimPrefix(pattern, "**/")
			match := func(pat, name string) bool {
				ok, err := filepath.Match(pat, name)
				return err == nil && ok
			}
			var out []string
			err := walkTree(root, func(p string) {
				if len(out) >= 1000 {
					return
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return
				}
				rel = filepath.ToSlash(rel)
				base := filepath.Base(p)
				if match(pattern, base) || match(pattern, rel) ||
					match(trim, base) || match(trim, rel) {
					out = append(out, rel)
				}
			})
			if err != nil {
				return "", err
			}
			if len(out) == 0 {
				return "no matches", nil
			}
			if len(out) >= 1000 {
				out = append(out, "...[truncated at 1000 results]")
			}
			return strings.Join(out, "\n"), nil
		},
	},

	{
		Name:        "ls",
		Description: "list directory entries (directories get a trailing /)",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "directory (default .)"},
			},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			root := "."
			if p, ok := args["path"].(string); ok && p != "" {
				root = p
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				return "", err
			}
			var out []string
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				out = append(out, name)
				if len(out) >= 500 {
					out = append(out, "...[truncated at 500 entries]")
					break
				}
			}
			return strings.Join(out, "\n"), nil
		},
	},
}

// buildToolSpecs renders tools as OpenAI-style function specs, preserving
// the caller's order.
func buildToolSpecs(tools []Tool) []map[string]any {
	specs := make([]map[string]any, 0, len(tools))

	for _, t := range tools {
		specs = append(specs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}

	return specs
}

// Outbound request policy: redirect rules and shared client.

// checkRedirect applies the outbound policy to every redirect hop:
// http/https only, bounded chain length. (Go itself strips Authorization
// on cross-host redirects.)
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("blocked redirect to scheme %q", req.URL.Scheme)
	}
	return nil
}

// httpClient is shared by all outbound traffic (LLM calls and fetches).
var httpClient = &http.Client{
	CheckRedirect: checkRedirect,
}

// Small helpers.

// readCapped reads up to limit bytes, erroring if the body is larger
// (rather than truncating it into invalid JSON).
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d byte limit", limit)
	}
	return data, nil
}

// truncateUTF8 cuts s to at most limit bytes without splitting a multi-byte
// rune, appending marker when something was cut.
func truncateUTF8(s string, limit int, marker string) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// extractUpstreamError pulls a human-readable message out of an OpenAI-style
// error body: {"error": "msg"} or {"error": {"message": "msg", ...}}.
func extractUpstreamError(result map[string]any) string {
	switch e := result["error"].(type) {
	case string:
		return e
	case map[string]any:
		if msg, _ := e["message"].(string); msg != "" {
			return msg
		}
	}
	return ""
}

// sleepCtx sleeps for d, returning early with the context's error if the run
// is cancelled meanwhile.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// llmCall sends a JSON payload to the LLM endpoint and returns the status and
// body. Transient failures (network/read errors, 429 and 5xx) are retried up
// to maxRetries times with capped exponential backoff, honoring Retry-After
// when present.
func llmCall(ctx context.Context, url, apiKey string, payload []byte) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		var resp *http.Response

		// Per-attempt deadline within the run context.
		reqCtx, cancel := context.WithTimeout(ctx, llmTimeout)

		req, err := http.NewRequestWithContext(
			reqCtx,
			http.MethodPost,
			url,
			bytes.NewReader(payload),
		)
		if err != nil {
			cancel()
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		var respBody []byte
		resp, err = httpClient.Do(req)
		if err == nil {
			var readErr error
			respBody, readErr = readCapped(resp.Body, maxLLMResponseBytes)
			resp.Body.Close() // close immediately after reading, saves memory
			cancel()

			if readErr != nil {
				err = readErr
			} else if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return resp.StatusCode, respBody, nil
			} else {
				err = fmt.Errorf("upstream %s: %s", resp.Status,
					truncateUTF8(strings.TrimSpace(string(respBody)), 1024, " ...[truncated]"))
			}
		} else {
			cancel()
		}

		if attempt >= maxRetries || ctx.Err() != nil {
			return 0, nil, err
		}

		delay := time.Second << attempt
		if resp != nil {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
					delay = time.Duration(secs) * time.Second
				}
			}
		}
		if delay > maxBackoff {
			delay = maxBackoff
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return 0, nil, err
		}
	}
}

// sseResult carries the assembled assistant message plus how the stream ended:
// whether any token streamed and the last finish_reason seen.
type sseResult struct {
	assistant    map[string]any
	streamed     bool
	finishReason string
}

// readSSEStream reads an OpenAI-style SSE response, calling onReasoning and
// onContent for streamed deltas and assembling tool_call fragments across
// chunks. A clean EOF or [DONE] ends the stream normally; anything else (a
// dropped connection, a timeout) is returned as an error so callers can see
// what went wrong instead of silently succeeding with a partial reply. Chunks
// that carry neither tokens nor tool calls are tolerated (providers emit a
// final empty-choices usage chunk before [DONE]).
func readSSEStream(r io.Reader, onReasoning, onContent, onSystem func(string)) (sseResult, error) {
	assistant := map[string]any{"role": "assistant"}
	var content []string
	// []any (of map[string]any entries) so the value survives a round trip
	// through map[string]any and can be asserted back as []any by callers.
	var toolCalls []any
	streamed := false
	finishReason := ""

	addToolCall := func(tc any) {
		tcm, ok := tc.(map[string]any)
		if !ok {
			return
		}
		idx := 0
		if i, ok := tcm["index"].(float64); ok {
			idx = int(i)
		}
		for len(toolCalls) <= idx {
			toolCalls = append(toolCalls, map[string]any{})
		}
		call, _ := toolCalls[idx].(map[string]any)
		if id, ok := tcm["id"].(string); ok && id != "" {
			call["id"] = id
		}
		if fn, ok := tcm["function"].(map[string]any); ok {
			fnObj, _ := call["function"].(map[string]any)
			if fnObj == nil {
				fnObj = map[string]any{}
			}
			if name, ok := fn["name"].(string); ok && name != "" {
				fnObj["name"] = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				prev, _ := fnObj["arguments"].(string)
				fnObj["arguments"] = prev + args
			}
			call["function"] = fnObj
		}
		toolCalls[idx] = call
	}

	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return sseResult{}, fmt.Errorf("stream read error: %v", err)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if data == "[DONE]" {
			break
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			if onSystem != nil {
				onSystem(fmt.Sprintf("skipped malformed stream chunk: %s",
					truncateUTF8(data, 256, " ...")))
			}
			continue
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			// Empty-choices chunk (e.g. the final usage chunk): nothing to do.
			continue
		}
		first, ok := choices[0].(map[string]any)
		if !ok {
			continue
		}

		if fr, ok := first["finish_reason"].(string); ok && fr != "" {
			// Some upstreams repeat finish_reason on several trailing
			// chunks (last delta + final empty chunk); announce it once.
			if finishReason == "" && onSystem != nil {
				onSystem("finish_reason: " + fr)
			}
			finishReason = fr
		}

		// Standard streaming shape is delta.tool_calls / delta.content; some
		// providers emit a full message object instead (message.tool_calls),
		// so accept both.
		delta, hasDelta := first["delta"].(map[string]any)
		msgObj, hasMsg := first["message"].(map[string]any)
		handled := false

		if hasDelta {
			reasoning := ""
			if r, ok := delta["reasoning"].(string); ok {
				reasoning = r
			} else if r, ok := delta["reasoning_content"].(string); ok {
				reasoning = r
			}
			if reasoning != "" {
				handled = true
				streamed = true
				if onReasoning != nil {
					onReasoning(reasoning)
				}
			}
			if c, ok := delta["content"].(string); ok && c != "" {
				handled = true
				streamed = true
				if onContent != nil {
					onContent(c)
				}
				content = append(content, c)
			}
			if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
				handled = true
				streamed = true
				for _, tc := range calls {
					addToolCall(tc)
				}
			}
			if !handled {
				var unknown []string
				for k := range delta {
					switch k {
					case "role", "content", "reasoning", "reasoning_content", "tool_calls":
					default:
						unknown = append(unknown, k)
					}
				}
				if len(unknown) > 0 {
					if onSystem != nil {
						onSystem("unhandled delta keys: " + strings.Join(unknown, ","))
					}
				}
			}
		}
		if hasMsg {
			if calls, ok := msgObj["tool_calls"].([]any); ok && len(calls) > 0 {
				handled = true
				streamed = true
				for _, tc := range calls {
					addToolCall(tc)
				}
			}
		}
	}

	if len(content) > 0 {
		assistant["content"] = strings.Join(content, "")
	}
	if len(toolCalls) > 0 {
		assistant["tool_calls"] = toolCalls
	}
	return sseResult{assistant: assistant, streamed: streamed, finishReason: finishReason}, nil
}

// llmCallStream posts a streaming chat request (stream: true) and feeds
// reasoning and content tokens to the callbacks as they arrive. It returns the
// assembled assistant message plus stream-end metadata. Connect errors and
// 429/5xx statuses are retried with capped exponential backoff; a mid-stream
// failure that already delivered tokens is surfaced instead.
func llmCallStream(
	ctx context.Context,
	url, apiKey string,
	reqBody map[string]any,
	onReasoning, onContent, onSystem func(string),
) (sseResult, error) {

	reqBody["stream"] = true
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return sseResult{}, err
	}

	for attempt := 0; ; attempt++ {
		var resp *http.Response

		reqCtx, cancel := context.WithTimeout(ctx, llmStreamTimeout)

		req, err := http.NewRequestWithContext(
			reqCtx,
			http.MethodPost,
			url,
			bytes.NewReader(payload),
		)
		if err != nil {
			cancel()
			return sseResult{}, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err = httpClient.Do(req)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				var respBody []byte
				respBody, err = readCapped(resp.Body, maxLLMResponseBytes)
				resp.Body.Close()
				cancel()
				if err == nil {
					err = fmt.Errorf("upstream %s: %s", resp.Status,
						truncateUTF8(strings.TrimSpace(string(respBody)), 1024, " ...[truncated]"))
				}
				if onSystem != nil {
					onSystem(fmt.Sprintf("upstream %s: %s", resp.Status,
						truncateUTF8(strings.TrimSpace(string(respBody)), 1024, " ...[truncated]")))
				}
			} else {
				res, readErr := readSSEStream(resp.Body, onReasoning, onContent, onSystem)
				resp.Body.Close()
				cancel()

				if readErr == nil {
					// Finished cleanly, or broke off after tokens already
					// streamed (nothing to retry: the caller saw partial text).
					return res, nil
				}
				if res.streamed {
					return sseResult{}, readErr
				}
				// Stream died before delivering anything; treat as transient.
				err = readErr
			}
		} else {
			cancel()
		}

		if attempt >= maxRetries || ctx.Err() != nil {
			return sseResult{}, err
		}

		delay := time.Second << attempt
		if resp != nil {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
					delay = time.Duration(secs) * time.Second
				}
			}
		}
		if delay > maxBackoff {
			delay = maxBackoff
		}
		if onSystem != nil && ctx.Err() == nil {
			onSystem(fmt.Sprintf("attempt %d/%d failed: %v; retrying in %.0fs",
				attempt+1, maxRetries, err, delay.Seconds()))
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return sseResult{}, err
		}
	}
}

// handleToolCall runs one tool call for the model, returning the call ID
// (synthesized when missing), the result text, and an error when the call
// itself failed. Malformed entries are reported back to the model instead of
// being forwarded as an empty message.
func handleToolCall(ctx context.Context, registry map[string]Tool, i int, tci any) (callID string, out string, callErr error) {
	call, ok := tci.(map[string]any)
	if !ok {
		if raw, err := json.Marshal(tci); err == nil {
			return "", fmt.Sprintf("invalid tool call: %s", truncateUTF8(string(raw), 512, " ...")), nil
		}
		return "", "invalid tool call: expected an object with id and function", nil
	}

	callID, _ = call["id"].(string)
	if callID == "" {
		callID = fmt.Sprintf("call_%d", i)
	}

	fn, ok := call["function"].(map[string]any)
	if !ok {
		return callID, "invalid tool call: function missing or not an object", nil
	}

	name, _ := fn["name"].(string)
	argStr, _ := fn["arguments"].(string)

	args := map[string]any{}
	if err := json.Unmarshal([]byte(argStr), &args); err != nil {
		// Report bad arguments back to the model.
		return callID, fmt.Sprintf("invalid tool arguments: %v", err), nil
	}

	tool, ok := registry[name]
	if !ok {
		return callID, "unknown tool", nil
	}

	result, err := tool.Handler(ctx, args)
	if err != nil {
		return callID, err.Error(), err
	}
	return callID, result, nil
}

// session is one ongoing conversation: the running user/assistant/tool
// history, so context persists across turns.
type session struct {
	messages []map[string]any
}

func newSession() *session {
	return &session{}
}

// Agent is the static configuration of one assistant loop: where to call,
// with which model, tools and system prompt, and how long the leash is.
// Sessions hold the dynamic part (history); Run drives an exchange.
type Agent struct {
	Name              string         // label for logging, e.g. "root"
	Endpoint          string         // chat-completions URL
	APIKey            string
	Model             string
	Tools             []Tool
	Provider          map[string]any // nil = omit from the request
	MaxToolIterations int            // 0 = default
	SystemPrompt      string
}

// Run appends the user's message to the session, streams the assistant reply
// through the callbacks while running the tool-use loop, and leaves the full
// exchange in the history. It returns nil once the assistant answers with
// plain content. On an empty session the agent's system prompt is seeded, so
// the first agent to run on a session defines its standing instructions.
func (ag *Agent) Run(
	ctx context.Context,
	s *session,
	message string,
	onReasoning, onContent, onSystem func(string),
	onTool func(callID, name, args string, ok bool, detail string),
) error {

	addMessage := func(msg map[string]any) {
		s.messages = append(s.messages, msg)
	}

	if len(s.messages) == 0 && ag.SystemPrompt != "" {
		addMessage(map[string]any{"role": "system", "content": ag.SystemPrompt})
	}
	addMessage(map[string]any{
		"role":    "user",
		"content": message,
	})

	// Index the tools by name for dispatch; the specs sent upstream keep
	// the caller's order.
	registry := make(map[string]Tool, len(ag.Tools))
	for _, t := range ag.Tools {
		registry[t.Name] = t
	}
	specs := buildToolSpecs(ag.Tools)

	maxIter := ag.MaxToolIterations
	if maxIter <= 0 {
		const defaultMaxToolIterations = 32
		maxIter = defaultMaxToolIterations
	}
	for range maxIter {
		if err := ctx.Err(); err != nil {
			return err
		}

		reqBody := map[string]any{
			"model":       ag.Model,
			"messages":    s.messages,
			"tools":       specs,
			"tool_choice": "auto",
		}
		if ag.Provider != nil {
			reqBody["provider"] = ag.Provider
		}

		// Stream the assistant reply through the caller's callbacks.
		res, err := llmCallStream(ctx, ag.Endpoint, ag.APIKey, reqBody, onReasoning, onContent, onSystem)
		if err != nil {
			return err
		}
		msg := res.assistant

		addMessage(msg)

		// No (or malformed) tool_calls: the streamed content is the answer.
		toolCalls, _ := msg["tool_calls"].([]any)
		if len(toolCalls) == 0 {
			if msg["content"] == nil {
				if onSystem != nil {
					onSystem(fmt.Sprintf("stream ended with no content and no tool calls (finish_reason: %q)",
						res.finishReason))
				}
				return fmt.Errorf("empty assistant reply (no content, no tool calls; finish_reason: %q)",
					res.finishReason)
			}
			return nil
		}

		// process tool calls
		for n, tci := range toolCalls {
			callID, toolResult, callErr := handleToolCall(ctx, registry, n, tci)

			if onTool != nil {
				name := ""
				argStr := ""
				if tciMap, ok := tci.(map[string]any); ok {
					if fn, ok := tciMap["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
						argStr, _ = fn["arguments"].(string)
					}
				}
				onTool(callID, name, argStr, callErr == nil, toolResult)
			}

			toolResult = truncateUTF8(toolResult, maxToolResultBytes, "\n[...truncated...]")

			addMessage(map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      toolResult,
			})
		}
	}

	return fmt.Errorf("max iterations reached")
}

func main() {

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: OPENROUTER_API_KEY not set")
		return
	}

	root := &Agent{
		Name:         "root",
		Endpoint:     "https://openrouter.ai/api/v1/chat/completions",
		APIKey:       apiKey,
		Model:        "deepseek/deepseek-v4-flash-0731",
		Tools:        defaultTools,
		Provider:     map[string]any{"order": []string{"DeepSeek"}, "allow_fallbacks": true},
		SystemPrompt: groundedPrompt(systemPrompt),
	}

	// Stream callbacks: render reasoning/output/tool/system blocks, each
	// starting on a fresh line with its own header so phases are unmistakable.
	const (
		blockNone      = 0
		blockReasoning = 1
		blockOutput    = 2
		blockTool      = 3
		blockSystem    = 4
	)
	currentBlock := blockNone
	printedAny := false
	lineOpen := false

	// stdout primitives; lineOpen tracks whether the last stdout write ended
	// with a newline.
	emitLine := func(s string) {
		fmt.Fprintln(os.Stdout, s)
		lineOpen = true
	}
	closeLine := func() {
		if !lineOpen {
			emitLine("")
		}
	}

	// beginStdout starts a reasoning/output/tool block on stdout.
	beginStdout := func(kind int, header string) {
		if currentBlock == kind {
			return
		}
		closeLine()
		if printedAny {
			emitLine("")
		}
		emitLine(header)
		printedAny = true
		currentBlock = kind
	}

	onReasoning := func(s string) {
		beginStdout(blockReasoning, "[reasoning]")
		fmt.Fprint(os.Stdout, s)
		lineOpen = false
	}

	onContent := func(s string) {
		beginStdout(blockOutput, "[output]")
		fmt.Fprint(os.Stdout, s)
		lineOpen = false
	}

	onSystem := func(s string) {
		if currentBlock != blockSystem {
			closeLine()
			if printedAny {
				fmt.Fprintln(os.Stderr, "")
			}
			printedAny = true
			currentBlock = blockSystem
		}
		oneLine := strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
		fmt.Fprintf(os.Stderr, "[system] %s\n", truncateUTF8(oneLine, 1024, " ..."))
	}

	onTool := func(callID, name, args string, ok bool, detail string) {
		label := name
		if callID != "" {
			label = callID + " " + name
		}
		beginStdout(blockTool, "[tool: "+label+"]")
		argOneLine := strings.ReplaceAll(args, "\n", " ")
		emitLine(truncateUTF8(argOneLine, 128, " ..."))
		if ok {
			emitLine("ok:")
			emitLine(truncateUTF8(detail, 128, " ..."))
		} else {
			emitLine("error:")
			emitLine(truncateUTF8(detail, 512, " ..."))
		}
		// Each tool call is its own block; the next phase starts fresh.
		currentBlock = blockNone
	}

	s := newSession()

	fmt.Println("nemo: turn-based coding assistant (Ctrl+C aborts a turn; exit or Ctrl-D to quit)")

	// Signals: Ctrl-C/SIGTERM during a run cancels just that turn; at the
	// prompt it discards the pending line (the tty does that anyway) and a
	// second consecutive press exits. Default signal disposition is never
	// relied on, so a stray SIGINT cannot kill the session.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// stdin is read from a goroutine so the prompt can select on input
	// and signals at once.
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	idleInterrupts := 0
	for {
		fmt.Print("\n> ")
		var input string
		select {
		case line, ok := <-lines:
			if !ok { // EOF (Ctrl-D)
				fmt.Println()
				return
			}
			input = strings.TrimSpace(line)
		case <-sigCh:
			idleInterrupts++
			if idleInterrupts >= 2 {
				fmt.Println("\nbye")
				return
			}
			fmt.Fprintln(os.Stderr, "\n(to quit: Ctrl+D or \"exit\"; during a run, Ctrl+C aborts the turn)")
			continue
		}
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			return
		}
		idleInterrupts = 0

		// The turn runs on its own context: runTimeout bounds it, and the
		// select below can cancel it on a signal without touching the
		// session or the process.
		turnCtx, cancel := context.WithTimeout(context.Background(), runTimeout)
		before := len(s.messages)

		runDone := make(chan error, 1)
		go func() {
			runDone <- root.Run(turnCtx, s, input,
				onReasoning, onContent, onSystem, onTool)
		}()

		var err error
		interrupted := false
		select {
		case err = <-runDone:
		case <-sigCh:
			cancel()
			interrupted = true
			err = <-runDone // wait for the run to unwind
			// A second press while unwinding means "really quit".
			select {
			case <-sigCh:
				fmt.Println("\nbye")
				return
			default:
			}
		}
		cancel()

		if err != nil {
			// Drop a partial exchange (e.g. tool_calls without their
			// results) so the history stays valid for the next turn.
			s.messages = s.messages[:before]
			if interrupted {
				fmt.Fprintln(os.Stderr, "\n[interrupted; turn dropped]")
			} else {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			continue
		}
		if interrupted {
			// Race: the turn completed before the cancel took effect.
			fmt.Fprintln(os.Stderr, "\n[turn completed before interrupt; kept]")
		}

		fmt.Println()
	}
}
