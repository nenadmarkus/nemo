// Command nemo-wasm is the nemo agent library compiled to WebAssembly
// (GOOS=js GOARCH=wasm) for the browser: the same streaming tool-use loop
// as the terminal client, with tools supplied from JavaScript at runtime.
//
// Tools are registered from JS (async handlers supported via Promises) and
// every Run callback is forwarded across the bridge as one JSON event.
// LLM traffic goes through Go's net/http, which maps onto the browser
// fetch() API — so the wasm module talks to any OpenAI-compatible
// endpoint directly from the page, subject to the endpoint's CORS policy.
//
// Build (see build.sh):
//
//	GOOS=js GOARCH=wasm go build -o nemo.wasm .
//
// and load wasm_exec.js + nemo.wasm next to index.html.
//
// JS surface, exported under globalThis.nemo by main():
//
//	nemo.registerTool(name, desc, paramsJSON, mutating, fn)
//	    fn(argsJSON: string) -> string | Promise<string> | {result} | {error}
//	nemo.unregisterTool(name)
//	nemo.setEvents({reasoning, content, system, toolStart, toolEnd,
//	                display, done})     each optional, receives one object
//	nemo.startRun(configJSON, prompt) -> run id (exchange runs async)
//	nemo.send(runID, configJSON, message) -> run id (follow-up turn)
//	nemo.cancel(runID)
//	nemo.sessionMessages(runID)       -> JSON string of the history
//	nemo.usage(runID)                 -> JSON string of token/cost usage
//
// configJSON accepts both {"endpoint": "<full chat-completions URL>"} and
// the terminal client's schema {"base_url": "<API root>"} (…/chat/completions
// is appended automatically), so one config works in both front ends.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"nemo"
)

// ---------------------------------------------------------------------------
// Outbound HTTP through the browser fetch() API.
// ---------------------------------------------------------------------------
//
// net/http on js/wasm uses fetch() (roundtrip_js.go) unless it detects
// Node.js (process.argv0 starting with "node"), where it switches to a
// test-only fake network; that would silently break real HTTP under the
// Node-based smoke test. Scrubbing process.argv0 keeps every runtime on
// the fetch() path; Node 18+ ships fetch and AbortController globals, so
// the browser code path runs there too. Browsers are unaffected (they
// have no process object at all).

func init() {
	if p := js.Global().Get("process"); p.Type() == js.TypeObject {
		p.Set("argv0", "nemo-wasm")
	}
}

// ---------------------------------------------------------------------------
// Event bridge: Go -> JS.
// ---------------------------------------------------------------------------

// eventNames are the sink keys setEvents accepts; every callback receives
// one plain JS object (decoded from one JSON string per call).
var eventNames = []string{"reasoning", "content", "system", "toolStart", "toolEnd", "display", "done"}

var (
	eventsMu sync.RWMutex
	events   = map[string]js.Value{}
)

// emit forwards one event payload to its JS sink as JSON. Handlers run on
// arbitrary goroutines (streaming callbacks, parallel tools); a JS-side
// throw must not take the module down, so panics are recovered.
func emitEvent(event string, payload any) {
	eventsMu.RLock()
	fn, ok := events[event]
	eventsMu.RUnlock()
	if !ok || fn.Type() != js.TypeFunction {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	defer func() { _ = recover() }() // a throwing sink must not kill the run
	fn.Invoke(string(data))
}

// setEvents(eventsObj) installs any subset of the sinks; see eventNames.
func setEventsJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		obj := args[0]
		if obj.Type() != js.TypeObject {
			return fmt.Sprintf("setEvents: expected an object, got %s", obj.Type())
		}
		eventsMu.Lock()
		defer eventsMu.Unlock()
		for _, name := range eventNames {
			if v := obj.Get(name); v.Type() == js.TypeFunction {
				events[name] = v
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Tool bridge: JS -> Go.
// ---------------------------------------------------------------------------

var (
	toolsMu sync.RWMutex
	jsTools []nemo.Tool
)

// registerTool(name, description, paramsJSON, mutating, fn) adds a tool
// whose handler is the JS function fn. fn receives the call arguments as
// a JSON string and returns the result as a string, a Promise resolving
// to one, or an object {result: "..."} / {error: "..."}.
func registerToolJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		name := jsString(args, 0)
		desc := jsString(args, 1)
		paramsJSON := jsString(args, 2)
		mutating := jsArg(args, 3).Truthy()
		fn := jsArg(args, 4)

		if name == "" {
			return "registerTool: name is required"
		}
		if fn.Type() != js.TypeFunction {
			return fmt.Sprintf("registerTool %s: handler must be a function", name)
		}

		var params map[string]any
		if paramsJSON != "" {
			if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
				return fmt.Sprintf("registerTool %s: bad params JSON: %v", name, err)
			}
		}

		handler := js.ValueOf(fn) // own reference, independent of registry state
		toolsMu.Lock()
		jsTools = append(jsTools, nemo.Tool{
			Name:        name,
			Description: desc,
			Parameters:  params,
			Mutating:    mutating,
			Handler: func(ctx context.Context, callArgs map[string]any) (string, error) {
				argsJSON, err := json.Marshal(callArgs)
				if err != nil {
					return "", err
				}
				return callJS(ctx, handler, string(argsJSON))
			},
		})
		toolsMu.Unlock()
		return nil
	})
}

// unregisterTool(name) drops a previously registered tool; unknown names
// are ignored. Safe while a run is active (running calls keep their own
// handler references).
func unregisterToolJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		name := jsString(args, 0)
		toolsMu.Lock()
		defer toolsMu.Unlock()
		for i, t := range jsTools {
			if t.Name == name {
				jsTools = append(jsTools[:i], jsTools[i+1:]...)
				break
			}
		}
		return nil
	})
}

// jsString returns args[i] as a string when it is one, "" otherwise (a
// missing or non-string argument must not panic the bridge).
func jsString(args []js.Value, i int) string {
	if v := jsArg(args, i); v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// jsArg returns args[i], or js.Undefined() when absent — Go's js.Value
// indexing panics on out-of-range, a missing argument must not.
func jsArg(args []js.Value, i int) js.Value {
	if i < len(args) {
		return args[i]
	}
	return js.Undefined()
}

// callJS invokes a JS tool handler and waits — without spinning the event
// loop — for a synchronous return value or a Promise to settle. The
// blocked goroutine parks; the runtime stays alive because the promise
// callbacks hold references into the wasm instance. Returns
// context.Canceled promptly when the run is cancelled.
func callJS(ctx context.Context, fn js.Value, argsJSON string) (string, error) {
	type outcome struct {
		val string
		err error
	}
	res := make(chan outcome, 1)

	// settle maps one settled value to an outcome. Recovers panics from
	// malformed shapes (Get panics on non-objects) instead of letting a
	// misbehaving handler take the module down.
	settle := func(v js.Value, isErr bool) {
		defer func() {
			if r := recover(); r != nil {
				res <- outcome{err: fmt.Errorf("tool handler panicked: %v", r)}
			}
		}()
		if isErr {
			res <- outcome{err: errors.New(errMessage(v))}
			return
		}
		if v.Type() == js.TypeObject || v.Type() == js.TypeFunction {
			if e := v.Get("error"); e.Type() == js.TypeString {
				res <- outcome{err: errors.New(e.String())}
				return
			}
			if r := v.Get("result"); r.Type() == js.TypeString {
				res <- outcome{val: r.String()}
				return
			}
		}
		res <- outcome{val: fmt.Sprintf("%v", v)} // String() of non-strings is "<type>" junk; JSON-shape objects above are the contract
	}

	then := js.FuncOf(func(this js.Value, a []js.Value) any {
		settle(a[0], false)
		return nil
	})
	catch := js.FuncOf(func(this js.Value, a []js.Value) any {
		settle(a[0], true)
		return nil
	})
	defer then.Release()
	defer catch.Release()

	ret := func() (v js.Value) {
		defer func() { _ = recover() }()
		return fn.Invoke(argsJSON)
	}()

	if ret.InstanceOf(js.Global().Get("Promise")) {
		ret.Call("then", then).Call("catch", catch)
	} else {
		settle(ret, false)
	}

	select {
	case o := <-res:
		return o.val, o.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// errMessage extracts a readable message from a rejected value: Error
// objects (.message), strings, anything else via String().
func errMessage(v js.Value) string {
	if v.Type() == js.TypeObject || v.Type() == js.TypeFunction {
		if m := v.Get("message"); m.Type() == js.TypeString {
			return m.String()
		}
	}
	return v.String()
}

// ---------------------------------------------------------------------------
// Runs: config + prompt in, events out.
// ---------------------------------------------------------------------------

// runConfig mirrors nemo.Agent's knobs plus run shaping. Both key
// schemas are accepted: `endpoint` (a full chat-completions URL) and the
// TUI's `base_url` (an API root like "https://api.openai.com/v1"), so a
// config that works in the terminal client works here unchanged.
type runConfig struct {
	Endpoint     string          `json:"endpoint"`
	BaseURL      string          `json:"base_url"`
	APIKey       string          `json:"api_key"`
	Model        string          `json:"model"`
	Provider     map[string]any  `json:"provider"`
	Temperature  *float64        `json:"temperature"`
	MaxTokens    int             `json:"max_tokens"`
	SystemPrompt string          `json:"system_prompt"`
	MaxIter      int             `json:"max_tool_iterations"`
	Tools        json.RawMessage `json:"tools"` // "" = all JS-registered tools; otherwise a JSON array of names
}

// normalize folds base_url into endpoint, appending /chat/completions
// when the base is an API root rather than the full URL. Without an
// endpoint at all, Go's http client would POST to the page's own origin
// — a static server answering 501 — so an empty value fails fast with a
// message naming the field instead.
func (c *runConfig) normalize() error {
	if c.Endpoint == "" {
		c.Endpoint = c.BaseURL
	}
	c.Endpoint = strings.TrimRight(c.Endpoint, "/")
	switch {
	case c.Endpoint == "":
		return errors.New(`config needs "endpoint" (chat-completions URL) or "base_url" (API root, e.g. https://api.openai.com/v1)`)
	case strings.HasSuffix(c.Endpoint, "/chat/completions"):
		// already the full URL
	case strings.HasSuffix(c.Endpoint, "/completions"):
		// base like .../v1/completions — leave as-is, provider-specific
	default:
		c.Endpoint += "/chat/completions"
	}
	return nil
}

// run is one conversation: a session plus its cancellation. gen counts
// the turns started on it: a turn retires itself from emitting when the
// id advances (send() while busy supersedes the in-flight exchange), so
// a slow retiring turn cannot bleed events into the new one.
type run struct {
	mu      sync.Mutex
	session *nemo.Session
	cancel  context.CancelFunc
	gen     int // turn generation owning event emission
}

var (
	runMu  sync.Mutex
	runCtr int
	runMap = map[int]*run{}
)

// resolveTools picks the run's tool set: all JS-registered tools by
// default, or the named subset when config.tools is an array of names.
// Unknown names are dropped; the agent reports "unknown tool" if the
// model calls one anyway.
func resolveTools(cfgTools json.RawMessage) []nemo.Tool {
	toolsMu.RLock()
	defer toolsMu.RUnlock()

	if len(cfgTools) > 0 {
		var names []string
		if err := json.Unmarshal(cfgTools, &names); err == nil {
			want := make(map[string]bool, len(names))
			for _, n := range names {
				want[n] = true
			}
			var out []nemo.Tool
			for _, t := range jsTools {
				if want[t.Name] {
					out = append(out, t)
				}
			}
			return out
		}
	}
	return append([]nemo.Tool(nil), jsTools...)
}

// newRun registers a run and starts its goroutine.
func newRun(cfg runConfig, tools []nemo.Tool, prompt string) int {
	runMu.Lock()
	runCtr++
	id := runCtr
	ctx, cancel := context.WithCancel(context.Background())
	runMap[id] = &run{cancel: cancel, session: nemo.NewSession()}
	r := runMap[id]
	runMu.Unlock()

	go driveRun(id, ctx, r, cfg, tools, prompt)
	return id
}

// driveRun executes one exchange, forwarding every callback across the
// bridge as a JSON event and finishing with a done event. All events
// carry the run id so a UI can multiplex several runs. Emission stops
// the moment the run's generation advances (a send() on the same id
// superseded this turn), so a slow retiring turn emits nothing that
// would collide with the successor's events.
func driveRun(id int, ctx context.Context, r *run, cfg runConfig, tools []nemo.Tool, prompt string) {
	// genSnapshot is this turn's generation, captured before the loop.
	r.mu.Lock()
	genSnapshot := r.gen
	r.mu.Unlock()

	// A panic must not kill the module; report it as this turn's done
	// event — unless the turn was already superseded, in which case
	// emitting would collide with the successor.
	defer func() {
		if rec := recover(); rec != nil {
			r.mu.Lock()
			alive := r.gen == genSnapshot
			r.mu.Unlock()
			if alive {
				emitEvent("done", map[string]any{"run": id, "error": fmt.Sprintf("panic: %v", rec)})
			}
		}
	}()

	ag := &nemo.Agent{
		Endpoint:          cfg.Endpoint,
		APIKey:            cfg.APIKey,
		Model:             cfg.Model,
		Provider:          cfg.Provider,
		Temperature:       cfg.Temperature,
		MaxTokens:         cfg.MaxTokens,
		MaxToolIterations: cfg.MaxIter,
		SystemPrompt:      nemo.ResolveSystemPrompt(cfg.SystemPrompt),
		Tools:             tools,
	}

	// alive reports whether this turn still owns the run's events.
	alive := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.gen == genSnapshot
	}

	// emit forwards one event unless the turn was superseded.
	emit := func(event string, payload map[string]any) {
		if !alive() {
			return
		}
		payload["run"] = id
		emitEvent(event, payload)
	}

	// Diff previews from write/edit ride along as display events.
	ctx = nemo.WithDisplay(ctx, func(s string) { emit("display", map[string]any{"text": s}) })

	err := ag.Run(ctx, r.session, prompt,
		func(s string) { emit("reasoning", map[string]any{"delta": s}) },
		func(s string) { emit("content", map[string]any{"delta": s}) },
		func(s string) { emit("system", map[string]any{"message": s}) },
		func(callID, name, a string) {
			emit("toolStart", map[string]any{"id": callID, "name": name, "args": json.RawMessage(orObject(a))})
		},
		func(callID, name, a string, ok bool, detail string) {
			evt := map[string]any{"id": callID, "name": name,
				"args": json.RawMessage(orObject(a)), "ok": ok}
			if ok {
				evt["result"] = detail
			} else {
				evt["error"] = detail
			}
			emit("toolEnd", evt)
		},
	)

	emit("done", map[string]any{"error": errString(err), "usage": r.session.Usage.String()})
}

// orObject passes s through as JSON, falling back to "{}" when empty or
// malformed, so an event's args field always decodes.
func orObject(s string) string {
	if s == "" {
		return "{}"
	}
	if !json.Valid([]byte(s)) {
		b, _ := json.Marshal(s)
		return string(b)
	}
	return s
}

// send(runID, configJSON, message) continues a conversation on its
// existing session. A still-running exchange is superseded: its context
// is cancelled and its events stop (generation advanced), and the new
// turn owns the session from here on. Unknown ids fail with a done event.
func sendJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		cfgJSON, message := jsString(args, 1), jsString(args, 2)
		id := jsArg(args, 0).Int()

		var cfg runConfig
		if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
			emitEvent("done", map[string]any{"run": id, "error": fmt.Sprintf("bad config: %v", err)})
			return -1
		}
		if err := cfg.normalize(); err != nil {
			emitEvent("done", map[string]any{"run": id, "error": err.Error()})
			return -1
		}

		runMu.Lock()
		r, ok := runMap[id]
		if !ok {
			runMu.Unlock()
			emitEvent("done", map[string]any{"run": id, "error": fmt.Sprintf("no such run: %d", id)})
			return -1
		}
		r.mu.Lock()
		r.gen++ // supersede the in-flight turn, if any
		r.cancel()
		r.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		runMu.Unlock()

		go driveRun(id, ctx, r, cfg, resolveTools(cfg.Tools), message)
		return id
	})
}

// startRun(configJSON, prompt) starts a conversation on a fresh session
// and returns its id; the exchange reports through the event sinks and
// ends with a done event. Invalid config fails fast: the id is -1 and
// the reason arrives as a done event.
func startRunJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		return turn(jsString(args, 0), jsString(args, 1))
	})
}

// turn validates the config and starts a new conversation.
func turn(cfgJSON, prompt string) int {
	var cfg runConfig
	if cfgJSON != "" {
		if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
			emitEvent("done", map[string]any{"run": -1, "error": fmt.Sprintf("bad config: %v", err)})
			return -1
		}
	}
	if err := cfg.normalize(); err != nil {
		emitEvent("done", map[string]any{"run": -1, "error": err.Error()})
		return -1
	}
	return newRun(cfg, resolveTools(cfg.Tools), prompt)
}

// cancelRun(id) cancels a running exchange; its goroutine then finishes
// with a done event carrying the cancellation error.
func cancelRunJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		if id := optionalInt(args); id >= 0 {
			runMu.Lock()
			r, ok := runMap[id]
			runMu.Unlock()
			if ok {
				r.cancel()
			}
		}
		return nil
	})
}

// sessionMessages(id) returns the run's history as a JSON string, "" when
// the id is unknown.
func sessionMessagesJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		runMu.Lock()
		r, ok := runMap[optionalInt(args)]
		runMu.Unlock()
		if !ok {
			return ""
		}
		b, err := json.Marshal(r.session.Messages)
		if err != nil {
			return err.Error()
		}
		return string(b)
	})
}

// usage(id) returns the run's cumulative token/cost usage as JSON.
func usageJS() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		runMu.Lock()
		r, ok := runMap[optionalInt(args)]
		runMu.Unlock()
		if !ok {
			return ""
		}
		u := r.session.Usage
		b, err := json.Marshal(map[string]any{
			"prompt_tokens":     u.PromptTokens,
			"completion_tokens": u.CompletionTokens,
			"cached_tokens":     u.CachedTokens,
			"reasoning_tokens":  u.ReasoningTokens,
			"cost":              u.Cost,
		})
		if err != nil {
			return err.Error()
		}
		return string(b)
	})
}

// optionalInt reads args[0] as a number, -1 when absent.
func optionalInt(args []js.Value) int {
	if len(args) == 0 || args[0].Type() != js.TypeNumber {
		return -1
	}
	return args[0].Int()
}

// errString renders nil as "" so done events carry a clean field.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// main installs the nemo API on globalThis.nemo and keeps the instance
// alive. A returned main exits the whole wasm module, and select{} trips
// the deadlock detector on wasm; an ever-rescheduled timer does not (it
// counts as a pending timer, keeping the runtime event loop busy — the
// standard pattern for library-style wasm builds).
func main() {
	n := js.Global().Get("nemo")
	if n.IsUndefined() {
		n = js.Global().Get("Object").New()
		js.Global().Set("nemo", n)
	}
	n.Set("setEvents", setEventsJS())
	n.Set("registerTool", registerToolJS())
	n.Set("unregisterTool", unregisterToolJS())
	n.Set("startRun", startRunJS())
	n.Set("send", sendJS())
	n.Set("cancel", cancelRunJS())
	n.Set("sessionMessages", sessionMessagesJS())
	n.Set("usage", usageJS())

	for {
		time.Sleep(time.Hour)
	}
}
