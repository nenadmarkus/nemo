package main

import (
	"bytes"
	"time"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	"count_r": {
		Name:        "count_r",
		Description: "count r characters in a word",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"word": map[string]interface{}{
					"type": "string",
				},
			},
			"required": []string{"word"},
		},
		Handler: func(args map[string]interface{}) (string, error) {
			word := args["word"].(string)
			count := 0
			for _, c := range word {
				if c == 'r' || c == 'R' {
					count++
				}
			}
			return fmt.Sprintf("%d", count), nil
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

func InvokeIntelligence(username, message, url, apiKey string) (string, error) {

	client := &http.Client{Timeout: 10 * time.Second}

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
			"model":       "nvidia/nemotron-3-super-120b-a12b:free",
			"messages":    messages,
			"tools":       tools,
			"tool_choice": "auto",
		}

		j, _ := json.Marshal(reqBody)

		// do the request
		req, _ := http.NewRequest(
			"POST",
			url,
			bytes.NewBuffer(j),
		)

		req.Header.Set("Authorization", "Bearer " + apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil { return "", err }
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
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

	out, err := InvokeIntelligence("alice", "How many r's are in strawberry?", url, apiKey)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println(out)
}
