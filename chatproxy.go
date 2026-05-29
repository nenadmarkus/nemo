package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

func main() {
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		log.Fatal("missing OPENROUTER_API_KEY")
	}

	http.HandleFunc("/v1/chat/completions", chatHandler)

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func resolveAPIKey(userToken string) (string, bool) {
	// TODO: replace with DB/Redis lookup
	if userToken == "demo-user-token" {
		return os.Getenv("OPENROUTER_API_KEY"), true
	}
	return "", false
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	// --- auth ---
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	apiKey, ok := resolveAPIKey(token)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// --- read request body ---
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// --- upstream request ---
	req, err := http.NewRequest(http.MethodPost, openRouterURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// --- check upstream status ---
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		log.Printf("upstream error HTTP %d: %s", resp.StatusCode, string(errBody))
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// --- SSE headers ---
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if data == "[DONE]" {
			return
		}

		// forward immediately
		if _, err := w.Write([]byte("data: " + data + "\n\n")); err != nil {
			return
		}
		flusher.Flush()
	}
}
