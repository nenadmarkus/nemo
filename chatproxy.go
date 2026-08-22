package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	modelName     = "~deepseek/deepseek-v4-flash-latest" //"cohere/north-mini-code:free"

	// Per-user rate limit: 1 request every 5s, burst of 3.
	rateRefill     = 5 * time.Second
	rateBurst      = 3
	rateGCInterval = 10 * time.Minute
	rateGCAfter    = 30 * time.Minute

	// Cap on request body size to prevent memory exhaustion (DoS).
	maxBodyBytes = 1 << 20 // 1 MiB
)

// userLimiter is a simple token-bucket limiter for one user (token).
type userLimiter struct {
	mu       sync.Mutex
	tokens   float64   // current available tokens
	last     time.Time // last refill time
	lastSeen time.Time // for GC
}

var (
	limitersMu sync.Mutex
	limiters   = make(map[string]*userLimiter)
)

// httpClient is used for upstream requests. The Timeout caps the full
// exchange (including streaming body read) so a slow upstream can't block
// forever. Client-disconnect cancellation is handled via r.Context().
var httpClient = &http.Client{Timeout: 60 * time.Second}

func getLimiter(token string) *userLimiter {
	now := time.Now()

	limitersMu.Lock()
	defer limitersMu.Unlock()

	if l, ok := limiters[token]; ok {
		l.lastSeen = now
		return l
	}

	l := &userLimiter{
		tokens:   float64(rateBurst),
		last:     now,
		lastSeen: now,
	}
	limiters[token] = l
	return l
}

// allow reports whether a request may proceed. If not, it returns the
// duration the caller should wait before retrying.
func (l *userLimiter) allow() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	// Refill tokens based on elapsed time since last call.
	l.tokens += now.Sub(l.last).Seconds() / rateRefill.Seconds()
	if l.tokens > float64(rateBurst) {
		l.tokens = float64(rateBurst)
	}
	l.last = now

	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}

	// Not enough tokens; compute how long until one is available.
	needed := 1 - l.tokens
	return false, time.Duration(needed * float64(rateRefill))
}

// gcLimiters removes limiters that haven't been seen in rateGCAfter.
func gcLimiters() {
	cutoff := time.Now().Add(-rateGCAfter)

	limitersMu.Lock()
	defer limitersMu.Unlock()

	for token, l := range limiters {
		if l.lastSeen.Before(cutoff) {
			delete(limiters, token)
		}
	}
}

func main() {
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		log.Fatal("missing OPENROUTER_API_KEY")
	}

	http.HandleFunc("/v1/chat/completions", chatHandler)
	http.HandleFunc("/", chatUIHandler)

	go func() {
		ticker := time.NewTicker(rateGCInterval)
		defer ticker.Stop()
		for range ticker.C {
			gcLimiters()
		}
	}()

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

	// --- rate limit ---
	if ok, wait := getLimiter(token).allow(); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// --- parse JSON request (with body size cap) ---
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Force the model, regardless of what the client sent.
	payload["model"] = modelName

	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode request", http.StatusInternalServerError)
		return
	}

	// --- upstream request ---
	// Inherit the client's context so the upstream request is cancelled
	// when the downstream client disconnects.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openRouterURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
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

	// --- SSE response headers ---
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

		if _, err := w.Write([]byte("data: " + data + "\n\n")); err != nil {
			return
		}
		flusher.Flush()
	}
}

func chatUIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(chatHTML))
}

const chatHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>nullclaw chat</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #0f1117;
    color: #e4e4e7;
    height: 100vh;
    display: flex;
    flex-direction: column;
  }
  header {
    background: #181a23;
    padding: 14px 20px;
    border-bottom: 1px solid #27272a;
    font-size: 18px;
    font-weight: 600;
    color: #a1a1aa;
  }
  header span { color: #6366f1; }
  #chat {
    flex: 1;
    overflow-y: auto;
    padding: 20px;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }
  .msg {
    max-width: 75%;
    padding: 12px 16px;
    border-radius: 12px;
    line-height: 1.5;
    white-space: pre-wrap;
    word-wrap: break-word;
  }
  .user {
    align-self: flex-end;
    background: #4f46e5;
    color: #fff;
    border-bottom-right-radius: 4px;
  }
  .assistant {
    align-self: flex-start;
    background: #27272a;
    color: #e4e4e7;
    border-bottom-left-radius: 4px;
  }
  .assistant.empty { color: #71717a; font-style: italic; }
  .error {
    align-self: center;
    background: #7f1d1d;
    color: #fecaca;
    font-size: 14px;
    border-radius: 8px;
  }
  #input-bar {
    background: #181a23;
    border-top: 1px solid #27272a;
    padding: 14px 20px;
    display: flex;
    gap: 10px;
  }
  #input {
    flex: 1;
    background: #0f1117;
    border: 1px solid #27272a;
    border-radius: 8px;
    padding: 12px 14px;
    color: #e4e4e7;
    font-size: 15px;
    outline: none;
    resize: none;
    max-height: 120px;
    font-family: inherit;
  }
  #input:focus { border-color: #4f46e5; }
  #send {
    background: #4f46e5;
    color: #fff;
    border: none;
    border-radius: 8px;
    padding: 0 22px;
    font-size: 15px;
    font-weight: 600;
    cursor: pointer;
    transition: background 0.15s;
  }
  #send:hover { background: #4338ca; }
  #send:disabled { background: #3f3f46; cursor: not-allowed; }
</style>
</head>
<body>
<header>Once Sport <span>Helpbot</span></header>
<div id="chat"></div>
<div id="input-bar">
  <textarea id="input" rows="1" placeholder="Type a message... (Enter to send, Shift+Enter for newline)"></textarea>
  <button id="send">Send</button>
</div>
<script>
(function() {
  var chat = document.getElementById("chat");
  var input = document.getElementById("input");
  var sendBtn = document.getElementById("send");
  var messages = [];
  var sending = false;

  function scrollDown() {
    chat.scrollTop = chat.scrollHeight;
  }

  function addMsg(role, text) {
    var div = document.createElement("div");
    div.className = "msg " + role;
    div.textContent = text;
    chat.appendChild(div);
    scrollDown();
    return div;
  }

  function addError(text) {
    var div = document.createElement("div");
    div.className = "msg error";
    div.textContent = text;
    chat.appendChild(div);
    scrollDown();
  }

  function setSending(v) {
    sending = v;
    sendBtn.disabled = v;
    input.disabled = v;
    if (!v) input.focus();
  }

  async function send() {
    if (sending) return;
    var text = input.value.trim();
    if (!text) return;

    addMsg("user", text);
    messages.push({ role: "user", content: text });
    input.value = "";
    input.style.height = "auto";

    var bubble = addMsg("assistant", "");
    bubble.classList.add("empty");
    bubble.textContent = "thinking...";
    setSending(true);

    try {
      var resp = await fetch("/v1/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Authorization": "Bearer demo-user-token"
        },
        body: JSON.stringify({ messages: messages, stream: true })
      });

      if (!resp.ok) {
        var errText = await resp.text();
        addError("Error " + resp.status + ": " + errText);
        chat.removeChild(bubble);
        setSending(false);
        return;
      }

      var reader = resp.body.getReader();
      var decoder = new TextDecoder();
      var buffer = "";
      var full = "";

      while (true) {
        var chunk = await reader.read();
        if (chunk.done) break;
        buffer += decoder.decode(chunk.value, { stream: true });

        var lines = buffer.split("\n");
        buffer = lines.pop();

        for (var i = 0; i < lines.length; i++) {
          var line = lines[i].trim();
          if (line === "" || line.indexOf("data: ") !== 0) continue;
          var data = line.slice(6).trim();
          if (data === "[DONE]") continue;

          try {
            var parsed = JSON.parse(data);
            if (parsed.choices && parsed.choices.length > 0) {
              var delta = parsed.choices[0].delta;
              if (delta && delta.content) {
                if (bubble.classList.contains("empty")) {
                  bubble.classList.remove("empty");
                  bubble.textContent = "";
                }
                full += delta.content;
                bubble.textContent = full;
                scrollDown();
              }
            }
          } catch (e) {
            // ignore unparseable lines
          }
        }
      }

      if (full) {
        messages.push({ role: "assistant", content: full });
      } else {
        bubble.classList.remove("empty");
        bubble.textContent = "(no response)";
      }
    } catch (e) {
      addError("Network error: " + e.message);
      if (chat.contains(bubble)) chat.removeChild(bubble);
    } finally {
      setSending(false);
    }
  }

  sendBtn.addEventListener("click", send);

  input.addEventListener("keydown", function(e) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });

  input.addEventListener("input", function() {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 120) + "px";
  });

  input.focus();
})();
</script>
</body>
</html>`
