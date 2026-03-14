package main

import (
	"bytes"
	"time"
	"strings"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/go-shiori/go-readability"
)

const maxIterations = 32

type ToolFunc func(map[string]interface{}) (string, error)

type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	Handler     ToolFunc
}

var toolRegistry = map[string]Tool{
	"search": {
		Name: "search",
		Description: "find web pages on the world wide web (url + short description)",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type": "string",
				},
			},
			"required": []string{"query"},
		},
		Handler: func(args map[string]interface{}) (string, error) {
			// get access
			apiKey := os.Getenv("SERPER_API_KEY")
			if apiKey == "" {
				return "", fmt.Errorf("no search API key")
			}

			// do the query
			query, ok := args["query"].(string)
			if !ok || query == "" {
				return "", fmt.Errorf("query is required")
			}

			fullUrl := fmt.Sprintf("https://google.serper.dev/search?q=%s&apiKey=%s", url.QueryEscape(query), apiKey)
			method := "GET"

			client := &http.Client {}
			req, err := http.NewRequest(method, fullUrl, nil)
			if err != nil {
				return "", err
			}

			res, err := client.Do(req)
			if err != nil {
				return "", err
			}
			defer res.Body.Close()

			body, err := io.ReadAll(res.Body)
			if err != nil {
				return "", err
			}

			// we're done
			return string(body), nil
		},
	},
	"fetch": {
		Name: "fetch",
		Description: "make an HTTP request to a URL and return the response body or extracted readable content",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type": "string",
				},
				"method": map[string]interface{}{
					"type": "string",
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
					"type": "integer",
					"description": "maximum response size in bytes (default 2097152)",
				},
				"readable": map[string]interface{}{
					"type": "boolean",
					"description": "postprocess to extract main readable content",
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

			req.Header.Set("User-Agent", "Mozilla/5.0")

			if h, ok := args["headers"].(map[string]interface{}); ok {
				for k, v := range h {
					if vs, ok := v.(string); ok {
						req.Header.Set(k, vs)
					}
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

				u, err := url.Parse(urlStr)
				if err != nil {
					return "", err
				}
				article, err := readability.FromReader(
					bytes.NewReader(data),
					u,
				)
				if err != nil {
					return "", err
				}

				out := struct {
					Title string `json:"title"`
					Text  string `json:"text"`
					URL   string `json:"url"`
				}{
					Title: article.Title,
					Text:  article.TextContent,
					URL:   urlStr,
				}

				b, _ := json.Marshal(out)
				return string(b), nil
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
		"content": "You are an assistant.",
	})
	addMessage(map[string]interface{}{
		"role": "user",
		"content": fmt.Sprintf("%s: %s", username, message),
	})

	tools := buildToolSpecs()

	for i := 0; i < maxIterations; i++ {
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
		if err != nil { return "", err }

		req.Header.Set("Authorization", "Bearer " + apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil { return "", err }

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close() // close immediately after reading, saves memory
		if err != nil { return "", err }

		// parse request body (JSON)
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil { return "", err }

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
			if err := json.Unmarshal([]byte(argStr), &args); err != nil { return "", err }

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

	model:= "openrouter/hunter-alpha"

	//out, err := InvokeIntelligence("alice", "How old is Josipa Lisac?", url, model, apiKey)
	out, err := InvokeIntelligence("alice", "What will the weather be tomorrow around Krapina, Croatia?", url, model, apiKey)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println(out)
}
