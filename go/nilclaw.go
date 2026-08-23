package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
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

// Tool is a function the model may call; ctx carries the run's deadline
// and cancellation.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Handler     func(ctx context.Context, args map[string]any) (string, error)
}

var toolRegistry = map[string]Tool{
	"fetch": {
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
}

func buildToolSpecs() []map[string]any {
	var tools []map[string]any

	for _, t := range toolRegistry {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}

	return tools
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

// handleToolCall runs one tool call for the model, returning the call ID
// (synthesized when missing) and the result text. Malformed entries are
// reported back to the model instead of being forwarded as an empty message.
func handleToolCall(ctx context.Context, i int, tci any) (string, string) {
	call, ok := tci.(map[string]any)
	if !ok {
		if raw, err := json.Marshal(tci); err == nil {
			return "", fmt.Sprintf("invalid tool call: %s", truncateUTF8(string(raw), 512, " ..."))
		}
		return "", "invalid tool call: expected an object with id and function"
	}

	callID, _ := call["id"].(string)
	if callID == "" {
		callID = fmt.Sprintf("call_%d", i)
	}

	fn, ok := call["function"].(map[string]any)
	if !ok {
		return callID, "invalid tool call: function missing or not an object"
	}

	name, _ := fn["name"].(string)
	argStr, _ := fn["arguments"].(string)

	args := map[string]any{}
	if err := json.Unmarshal([]byte(argStr), &args); err != nil {
		// Report bad arguments back to the model.
		return callID, fmt.Sprintf("invalid tool arguments: %v", err)
	}

	tool, ok := toolRegistry[name]
	if !ok {
		return callID, "unknown tool"
	}

	out, err := tool.Handler(ctx, args)
	if err != nil {
		return callID, err.Error()
	}
	return callID, out
}

func InvokeIntelligence(ctx context.Context, username, message, url, model, apiKey string) (string, error) {

	var messages []map[string]any

	addMessage := func(msg map[string]any) {
		messages = append(messages, msg)
		b, _ := json.MarshalIndent(msg, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
	}

	addMessage(map[string]any{
		"role": "system",
		"content": fmt.Sprintf(
			"You are an assistant. Current time: %s. Use fetch from https://html.duckduckgo.com/html/?q=<query> for web search.",
			time.Now().Format(time.RFC3339),
		),
	})
	addMessage(map[string]any{
		"role":    "user",
		"content": fmt.Sprintf("%s: %s", username, message),
	})

	tools := buildToolSpecs()

	const maxToolIterations = 32
	for range maxToolIterations {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		reqBody := map[string]any{
			"model":       model,
			"messages":    messages,
			"tools":       tools,
			"tool_choice": "auto",
		}

		j, err := json.Marshal(reqBody)
		if err != nil {
			return "", err
		}

		status, body, err := llmCall(ctx, url, apiKey, j)
		if err != nil {
			return "", err
		}

		// Surface upstream failures with their status and body.
		if status != http.StatusOK {
			return "", fmt.Errorf("upstream %d: %s", status,
				truncateUTF8(strings.TrimSpace(string(body)), 1024, " ...[truncated]"))
		}

		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			return "", fmt.Errorf("invalid JSON from upstream: %w", err)
		}
		if msg := extractUpstreamError(result); msg != "" {
			return "", fmt.Errorf("upstream error: %s", msg)
		}

		// Malformed responses error out rather than panic.
		choices, ok := result["choices"].([]any)
		if !ok || len(choices) == 0 {
			return "", fmt.Errorf("invalid or empty choices in response")
		}
		first, ok := choices[0].(map[string]any)
		if !ok {
			return "", fmt.Errorf("malformed choice entry in response")
		}
		msg, ok := first["message"].(map[string]any)
		if !ok {
			return "", fmt.Errorf("response choice has no message object")
		}

		addMessage(msg)

		// No (or malformed) tool_calls: the message content is the final answer.
		toolCalls, ok := msg["tool_calls"].([]any)
		if !ok || len(toolCalls) == 0 {
			if content, ok := msg["content"].(string); ok {
				return content, nil
			}

			return "", fmt.Errorf("no tool calls and failed to parse msg['content']")
		}

		// process tool calls
		for n, tci := range toolCalls {
			callID, toolResult := handleToolCall(ctx, n, tci)
			toolResult = truncateUTF8(toolResult, maxToolResultBytes, "\n[...truncated...]")

			addMessage(map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      toolResult,
			})
		}
	}

	return "", fmt.Errorf("max iterations reached")
}

func main() {

	// Ctrl-C/SIGTERM cancels the run; runTimeout bounds it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	url := "https://openrouter.ai/api/v1/chat/completions"

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: OPENROUTER_API_KEY not set")
		return
	}

	model := "deepseek/deepseek-v4-flash-0731"

	//out, err := InvokeIntelligence(ctx, "alice", "How old is Josipa Lisac?", url, model, apiKey)
	out, err := InvokeIntelligence(ctx, "alice", "What will the weather be tomorrow around Krapina, Croatia?", url, model, apiKey)
	//out, err := InvokeIntelligence(ctx, "alice", "What time is it in Croatia?", url, model, apiKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return
	}

	fmt.Println(out)
}
