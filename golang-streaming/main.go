package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type requestBody struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	Reasoning struct {
		Enabled bool `json:"enabled"`
	} `json:"reasoning"`
	Stream bool `json:"stream"`
}

type delta struct {
	Content string `json:"content"`
}

type choice struct {
	Delta delta `json:"delta"`
}

type streamResponse struct {
	Choices []choice `json:"choices"`
}

func main() {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENROUTER_API_KEY not set")
		return
	}

	req := requestBody{
		Model: "nvidia/nemotron-3-super-120b-a12b:free",
		Messages: []message{
			{Role: "system", Content: "You are an assistant."},
			{Role: "user", Content: "How many r's are in the word strawberry?"},
		},
		Stream: true,
	}
	req.Reasoning.Enabled = false

	body, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error marshaling request:", err)
		return
	}

	httpReq, err := http.NewRequest("POST", openRouterURL, strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error creating request:", err)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "HTTP %d: %s\n", resp.StatusCode, string(errBody))
		return
	}

	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if data == "[DONE]" {
			break
		}

		var sr streamResponse
		if err := json.Unmarshal([]byte(data), &sr); err != nil {
			continue
		}

		if len(sr.Choices) > 0 && sr.Choices[0].Delta.Content != "" {
			fmt.Print(sr.Choices[0].Delta.Content)
		}
	}

	fmt.Println()
}