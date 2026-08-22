package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
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
	tool defs
*/

type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	Handler     func(map[string]interface{}) (string, error)
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
		Handler: func(args map[string]interface{}) (string, error) {

			urlStr, ok := args["url"].(string)
			if !ok || urlStr == "" {
				return "", fmt.Errorf("url is required")
			}

			method := "GET"
			if m, ok := args["method"].(string); ok && m != "" {
				method = strings.ToUpper(m)
			}

			var body io.Reader
			if b, ok := args["body"].(string); ok && b != "" {
				body = strings.NewReader(b)
			}

			req, err := http.NewRequest(method, urlStr, body)
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

			// Apply token injection
			hvPattern := []struct {
				regex *regexp.Regexp
				name  string
				value string
			}{
				{regexp.MustCompile(`^https://google\.serper\.dev/search\?q=`), "X-API-KEY", os.Getenv("SERPER_API_KEY")},
			}
			for _, pattern := range hvPattern {
				if pattern.regex.MatchString(urlStr) {
					fmt.Println("insert token")
					req.Header.Set(pattern.name, pattern.value)
					break
				}
			}

			client := &http.Client{Timeout: 15 * time.Second}

			res, err := client.Do(req)
			if err != nil {
				return "", err
			}
			defer res.Body.Close()

			maxBytes := int64(2 * 1024 * 1024)
			if mb, ok := args["max_bytes"].(float64); ok && mb > 0 {
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

func InvokeIntelligence(username, message, url, model, apiKey string) (string, error) {

	client := &http.Client{Timeout: 32 * time.Second}

	var messages []map[string]interface{}

	addMessage := func(msg map[string]interface{}) {
		messages = append(messages, msg)
		b, _ := json.MarshalIndent(msg, "", "  ")
		fmt.Println(string(b))
	}

	addMessage(map[string]interface{}{
		"role": "system",
		"content": fmt.Sprintf(
			"You are an assistant. Current time: %s. Use fetch from https://google.serper.dev/search?q=<query> and https://html.duckduckgo.com/html/?q=<query> for web search.",
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
		// construct the request
		reqBody := map[string]interface{}{
			"model":       model,
			"messages":    messages,
			"tools":       tools,
			"tool_choice": "auto",
		}

		j, _ := json.Marshal(reqBody)

		// do the request
		req, err := http.NewRequest(
			"POST",
			url,
			bytes.NewBuffer(j),
		)
		if err != nil {
			return "", err
		}

		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close() // close immediately after reading, saves memory
		if err != nil {
			return "", err
		}

		// parse request body (JSON)
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			return "", err
		}

		// extract revelant data (message)
		choices, ok := result["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			return "", fmt.Errorf("invalid or empty choices in response")
		}
		msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})

		addMessage(msg)

		// are there tool calls?
		toolCalls, hasTools := msg["tool_calls"]
		if !hasTools {
			if content, ok := msg["content"].(string); ok {
				// no tool calls, final answer
				return content, nil
			}

			return "", fmt.Errorf("no tool calls and failed to parse msg['content']")
		}

		// process tool calls
		for _, tc := range toolCalls.([]interface{}) {

			call := tc.(map[string]interface{})
			fn := call["function"].(map[string]interface{})
			name := fn["name"].(string)

			argStr := fn["arguments"].(string)
			args := map[string]interface{}{}
			if err := json.Unmarshal([]byte(argStr), &args); err != nil {
				return "", err
			}

			tool, ok := toolRegistry[name]

			var result string
			if ok {
				out, err := tool.Handler(args)
				if err != nil {
					result = err.Error()
				} else {
					result = out
				}
			} else {
				result = "unknown tool"
			}

			addMessage(map[string]interface{}{
				"role":         "tool",
				"tool_call_id": call["id"],
				"content":      result,
			})
		}
	}

	return "", fmt.Errorf("max iterations reached")
}

func main() {

	url := "https://openrouter.ai/api/v1/chat/completions"

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("error: OPENROUTER_API_KEY not set")
		return
	}

	model := "deepseek/deepseek-v4-flash-0731"

	//out, err := InvokeIntelligence("alice", "How old is Josipa Lisac?", url, model, apiKey)
	out, err := InvokeIntelligence("alice", "What will the weather be tomorrow around Krapina, Croatia?", url, model, apiKey)
	//out, err := InvokeIntelligence("alice", "What time is it in Croatia?", url, model, apiKey)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println(out)
}
