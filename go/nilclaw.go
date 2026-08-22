package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
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

/*
outbound hardening: timeouts, size caps and an SSRF/redirect policy
applied to every request this program makes (both the LLM calls and
the model-driven fetch tool). See httpClient, guardedDialContext and
checkRedirect below.
*/
const (
	// llmTimeout caps a single chat-completions round trip.
	llmTimeout = 32 * time.Second
	// fetchTimeout caps a single tool fetch.
	fetchTimeout = 15 * time.Second
	// dialTimeout caps TCP connection establishment.
	dialTimeout = 10 * time.Second
	// runTimeout bounds the whole agentic run (worst case: 32 iterations
	// of LLM call plus tool fetches), so a wedged run cannot hang forever.
	runTimeout = 10 * time.Minute

	// maxLLMResponseBytes caps a single LLM response body.
	maxLLMResponseBytes int64 = 1 << 20
	// maxToolResponseBytes is the hard cap for tool fetch bodies; it is
	// also the default, and a model-supplied max_bytes can only ask for
	// less, never more.
	maxToolResponseBytes int64 = 2 << 20
	// maxToolResultBytes caps a tool result as it is fed back into the
	// conversation, so repeated fetches cannot blow up context and cost.
	maxToolResultBytes = 64 << 10

	// maxRedirects bounds redirect chains (like the default client does).
	maxRedirects = 10
)

/*
tool defs
*/

// Tool handlers receive the run context so a Ctrl-C, the overall run
// deadline, or the per-request timeout cancels in-flight fetches.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	Handler     func(ctx context.Context, args map[string]interface{}) (string, error)
}

var toolRegistry = map[string]Tool{
	"fetch": {
		Name:        "fetch",
		Description: "make an HTTP request to a URL and return the response body or extracted readable content",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type": "string",
				},
				"method": map[string]interface{}{
					"type":    "string",
					"default": "GET",
				},
				"headers": map[string]interface{}{
					"type": "object",
					"additionalProperties": map[string]interface{}{
						"type": "string",
					},
				},
				"body": map[string]interface{}{
					"type": "string",
				},
				"max_bytes": map[string]interface{}{
					"type":        "integer",
					"description": "maximum response size in bytes (default 2097152)",
				},
				"readable": map[string]interface{}{
					"type":        "boolean",
					"description": "light postprocessing to extract readable content by removing script and style tags, etc.",
				},
			},
			"required": []string{"url"},
		},
		Handler: func(ctx context.Context, args map[string]interface{}) (string, error) {

			urlStr, ok := args["url"].(string)
			if !ok || urlStr == "" {
				return "", fmt.Errorf("url is required")
			}

			// Reject non-http(s) URLs up front (file:, gopher:, ...). The
			// address itself is enforced at dial time by guardedDialContext,
			// which also covers every redirect hop.
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

			if h, ok := args["headers"].(map[string]interface{}); ok {
				for k, v := range h {
					if vs, ok := v.(string); ok {
						req.Header.Set(k, vs)
					}
				}
			}

			// NOTE: there is deliberately no credential-injection mechanism
			// yet (the old Serper X-API-KEY hack lived here). The plan is to
			// drive it from env/JSON config later; until then fetches carry
			// no credentials beyond what the model itself puts in "headers".

			res, err := httpClient.Do(req)
			if err != nil {
				return "", err
			}
			defer res.Body.Close()

			// max_bytes is clamped to the hard cap: a hallucinated huge
			// value cannot turn this tool into a memory-exhaustion vector.
			// Smaller values are honored; absent/invalid falls back to the
			// cap (the old 2 MiB default).
			maxBytes := maxToolResponseBytes
			if mb, ok := args["max_bytes"].(float64); ok && mb > 0 && mb < float64(maxToolResponseBytes) {
				maxBytes = int64(mb)
			}

			data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes))
			if err != nil {
				return "", err
			}

			readable := false
			if r, ok := args["readable"].(bool); ok {
				readable = r
			}

			if readable {
				return extractText(string(data)), nil
			}

			return string(data), nil
		},
	},
}

func buildToolSpecs() []map[string]interface{} {
	var tools []map[string]interface{}

	for _, t := range toolRegistry {
		tools = append(tools, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}

	return tools
}

/*
outbound request policy (SSRF guard, redirect policy, shared client)
*/

var guardedDialer = &net.Dialer{
	Timeout:   dialTimeout,
	KeepAlive: 30 * time.Second,
}

// guardedDialContext is the SSRF gate for every outbound connection: the
// hostname from the URL is resolved here, every resolved address is checked,
// and the connection is then made to a validated IP directly (never
// re-resolved), so neither redirects nor DNS rebinding can smuggle a request
// to loopback, private, or link-local addresses (cloud metadata endpoints,
// local admin panels, internal services). Note: requests routed through an
// explicit HTTP(S)_PROXY are governed by that proxy - only the proxy's own
// address is dialed by us.
func guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	dialAddrs := make([]string, 0, len(ips))
	for _, ipa := range ips {
		if ipa.Zone != "" || !isPublicIP(ipa.IP) {
			return nil, fmt.Errorf("blocked non-public address %s for %q", ipa.IP, host)
		}
		dialAddrs = append(dialAddrs, net.JoinHostPort(ipa.IP.String(), port))
	}
	if len(dialAddrs) == 0 {
		return nil, fmt.Errorf("host %q did not resolve to any address", host)
	}

	// Dial exactly the addresses we validated (TLS keeps using the URL
	// hostname for SNI and certificate verification).
	var lastErr error
	for _, da := range dialAddrs {
		conn, err := guardedDialer.DialContext(ctx, network, da)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// isPublicIP reports whether ip is a globally routable address. Everything
// else - loopback, RFC1918 private, link-local (including cloud metadata at
// 169.254.169.254), multicast, unspecified, broadcast and carrier-grade NAT
// space - is refused.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.Equal(net.IPv4bcast) {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false // 100.64.0.0/10 carrier-grade NAT
	}
	return true
}

// checkRedirect re-applies the outbound policy on every redirect hop:
// http/https only and a bounded chain length. (Go itself already strips
// the Authorization header on cross-host redirects. There is no injected
// credential to protect yet - see the note in the fetch handler - and when
// config-driven injection arrives, the simple rule for such requests is
// to not follow redirects at all, e.g. by sending them with
// CheckRedirect: func(...) error { return http.ErrUseLastResponse }.)
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("blocked redirect to scheme %q", req.URL.Scheme)
	}
	return nil
}

// httpClient is the single shared client for all outbound traffic (tool
// fetches and LLM calls): connections are pooled, and every request -
// initial or redirected - goes through guardedDialContext and
// checkRedirect. Per-request deadlines come from the caller's context.
var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           guardedDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	CheckRedirect: checkRedirect,
}

/*
	small helpers
*/

// readCapped reads at most limit+1 bytes and reports an error when the body
// is larger, so an oversized response fails loudly instead of being silently
// truncated into invalid JSON.
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
func extractUpstreamError(result map[string]interface{}) string {
	switch e := result["error"].(type) {
	case string:
		return e
	case map[string]interface{}:
		if msg, _ := e["message"].(string); msg != "" {
			return msg
		}
	}
	return ""
}

func InvokeIntelligence(ctx context.Context, username, message, url, model, apiKey string) (string, error) {

	var messages []map[string]interface{}

	addMessage := func(msg map[string]interface{}) {
		messages = append(messages, msg)
		b, _ := json.MarshalIndent(msg, "", "  ")
		fmt.Println(string(b))
	}

	addMessage(map[string]interface{}{
		"role": "system",
		"content": fmt.Sprintf(
			"You are an assistant. Current time: %s. Use fetch from https://html.duckduckgo.com/html/?q=<query> for web search.",
			time.Now().Format(time.RFC3339),
		),
	})
	addMessage(map[string]interface{}{
		"role":    "user",
		"content": fmt.Sprintf("%s: %s", username, message),
	})

	tools := buildToolSpecs()

	const maxToolIterations = 32
	for i := 0; i < maxToolIterations; i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		// construct the request
		reqBody := map[string]interface{}{
			"model":       model,
			"messages":    messages,
			"tools":       tools,
			"tool_choice": "auto",
		}

		j, err := json.Marshal(reqBody)
		if err != nil {
			return "", err
		}

		// per-call deadline nested inside the run context, so a single
		// hung call cannot stall the run and Ctrl-C always wins
		reqCtx, cancel := context.WithTimeout(ctx, llmTimeout)

		// do the request
		req, err := http.NewRequestWithContext(
			reqCtx,
			http.MethodPost,
			url,
			bytes.NewReader(j),
		)
		if err != nil {
			cancel()
			return "", err
		}

		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			cancel()
			return "", err
		}

		body, err := readCapped(resp.Body, maxLLMResponseBytes)
		resp.Body.Close() // close immediately after reading, saves memory
		cancel()
		if err != nil {
			return "", err
		}

		// surface upstream failures with their status and body instead of
		// the old useless "invalid or empty choices"
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("upstream %s: %s", resp.Status,
				truncateUTF8(strings.TrimSpace(string(body)), 1024, " ...[truncated]"))
		}

		// parse request body (JSON)
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			return "", fmt.Errorf("invalid JSON from upstream: %w", err)
		}
		if msg := extractUpstreamError(result); msg != "" {
			return "", fmt.Errorf("upstream error: %s", msg)
		}

		// extract relevant data (message) - checked assertions only: a
		// malformed or unexpected shape must error, never panic
		choices, ok := result["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			return "", fmt.Errorf("invalid or empty choices in response")
		}
		first, ok := choices[0].(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("malformed choice entry in response")
		}
		msg, ok := first["message"].(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("response choice has no message object")
		}

		addMessage(msg)

		// are there tool calls? (a nil/absent/mistyped value simply means
		// "final answer")
		toolCalls, ok := msg["tool_calls"].([]interface{})
		if !ok || len(toolCalls) == 0 {
			if content, ok := msg["content"].(string); ok {
				// no tool calls, final answer
				return content, nil
			}

			return "", fmt.Errorf("no tool calls and failed to parse msg['content']")
		}

		// process tool calls
		for _, tci := range toolCalls {

			call, _ := tci.(map[string]interface{})
			callID, _ := call["id"].(string)
			fn, _ := call["function"].(map[string]interface{})
			name, _ := fn["name"].(string)
			argStr, _ := fn["arguments"].(string)

			args := map[string]interface{}{}
			var toolResult string
			if err := json.Unmarshal([]byte(argStr), &args); err != nil {
				// feed the parse failure back to the model instead of dying
				toolResult = fmt.Sprintf("invalid tool arguments: %v", err)
			} else if tool, ok := toolRegistry[name]; ok {
				out, err := tool.Handler(ctx, args)
				if err != nil {
					toolResult = err.Error()
				} else {
					toolResult = out
				}
			} else {
				toolResult = "unknown tool"
			}

			// never let a tool result flood the conversation/context
			toolResult = truncateUTF8(toolResult, maxToolResultBytes, "\n[...truncated...]")

			addMessage(map[string]interface{}{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      toolResult,
			})
		}
	}

	return "", fmt.Errorf("max iterations reached")
}

func main() {

	// Ctrl-C / SIGTERM cancels the whole run; runTimeout bounds it even if
	// no signal arrives.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	url := "https://openrouter.ai/api/v1/chat/completions"

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("error: OPENROUTER_API_KEY not set")
		return
	}

	model := "deepseek/deepseek-v4-flash-0731"

	//out, err := InvokeIntelligence(ctx, "alice", "How old is Josipa Lisac?", url, model, apiKey)
	out, err := InvokeIntelligence(ctx, "alice", "What will the weather be tomorrow around Krapina, Croatia?", url, model, apiKey)
	//out, err := InvokeIntelligence(ctx, "alice", "What time is it in Croatia?", url, model, apiKey)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println(out)
}
