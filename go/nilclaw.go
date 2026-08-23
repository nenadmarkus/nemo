package main

import (
	"bufio"
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
			finishReason = fr
			if onSystem != nil {
				onSystem("finish_reason: " + fr)
			}
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
		if onSystem != nil {
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
func handleToolCall(ctx context.Context, i int, tci any) (callID string, out string, callErr error) {
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

	tool, ok := toolRegistry[name]
	if !ok {
		return callID, "unknown tool", nil
	}

	result, err := tool.Handler(ctx, args)
	if err != nil {
		return callID, err.Error(), err
	}
	return callID, result, nil
}

func InvokeIntelligence(
	ctx context.Context,
	username, message, url, model, apiKey string,
	onReasoning, onContent, onSystem func(string),
	onTool func(callID, name, args string, ok bool, detail string),
) error {

	var messages []map[string]any

	addMessage := func(msg map[string]any) {
		messages = append(messages, msg)
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
			return err
		}

		reqBody := map[string]any{
			"model":       model,
			"messages":    messages,
			"tools":       tools,
			"tool_choice": "auto",
			"provider": map[string]any{
				"order":           []string{"DeepSeek"},
				"allow_fallbacks": true,
			},
		}

		// Stream the assistant reply through the caller's callbacks.
		res, err := llmCallStream(ctx, url, apiKey, reqBody, onReasoning, onContent, onSystem)
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
			callID, toolResult, callErr := handleToolCall(ctx, n, tci)

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

	//out, err := InvokeIntelligence(ctx, "alice", "How old is Josipa Lisac?", url, model, apiKey)
	err := InvokeIntelligence(ctx, "alice", "What will the weather be tomorrow around Krapina, Croatia?", url, model, apiKey,
		onReasoning, onContent, onSystem, onTool)
	//out, err := InvokeIntelligence(ctx, "alice", "What time is it in Croatia?", url, model, apiKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return
	}

	fmt.Println()
}
