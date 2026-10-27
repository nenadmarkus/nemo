# nemo in the browser (wasm)

The nemo agent library compiled to WebAssembly (`GOOS=js GOARCH=wasm`):
the same streaming tool-use loop as the terminal client, running entirely
in the page. There is no server-side component — the wasm module talks to
the LLM endpoint directly via `fetch`, and tools are ordinary JavaScript
functions registered at runtime.

| file | role |
|---|---|
| `main.go` | the wasm module: installs `globalThis.nemo` (event bridge, tool bridge, run management) |
| `nemo.js` | browser glue: `Nemo.load()` → promise/event front end over the raw API |
| `index.html` | demo chat UI with three page-provided tools |
| `build.sh` | builds `nemo.wasm`, copies Go's `wasm_exec.js` next to it |
| `smoke.mjs` | end-to-end test under Node: fake OpenAI endpoint, real module, 27 checks |

## Build & run

```sh
cd wasm
./build.sh                                  # nemo.wasm + wasm_exec.js (gitignored)
python3 -m http.server 8080                 # any static file server
# open http://localhost:8080/wasm/
```

The browser needs an OpenAI-compatible endpoint that sends CORS headers
allowing your origin (OpenRouter does). The API key lives in the browser:
serve over HTTPS and prefer a spend-limited key.

## The bridge

```js
const nemo = await Nemo.load();              // instantiate (idempotent)

nemo.registerTool({                          // tools run in your page
  name: "clock",
  description: "current time in the browser, ISO-8601",
  parameters: { type: "object", properties: {} },
  run: async (args) => new Date().toISOString(),   // sync or async
});
nemo.unregisterTool("clock");

const run = nemo.run(config, "prompt");      // config mirrors cmd/nemo's
run.onReasoning(ev => ...);                  // ev.delta
run.onContent(ev => ...);                    // ev.delta
run.onToolStart(ev => ...);                  // ev.id, ev.name, ev.args
run.onToolEnd(ev => ...);                    // ev.id/name/args + result|error, ev.ok
run.onDisplay(ev => ...);                    // ev.text (diff previews)
await run.done;                              // {run, error, usage}
await run.send("follow-up");                 // same conversation, new done
run.cancel();                                // abort mid-stream
run.history(); run.usage();                  // session introspection
```

`config` accepts both schemas — `{"endpoint": "<full chat-completions URL>"}` or the terminal client's `{"base_url": "<API root>"}` (…/chat/completions is appended for you) — plus `api_key`, `model`, `provider?`, `temperature?`, `max_tokens?`, `system_prompt?`, `max_tool_iterations?`, and `tools?`, being an array of registered tool names (default: all of them). Every event carries its `run` id, so several conversations can share one set of sinks. A tool's `run` may throw; the error becomes that call's tool result and the model sees it.

## Events, from the module itself

Every callback the library offers crosses the bridge as one JSON event
(each sink receives one object): `reasoning{run,delta}`,
`content{run,delta}`, `system{run,message}`,
`toolStart{run,id,name,args}`, `toolEnd{run,id,name,args,ok,result|error}`,
`display{run,text}`, `done{run,error,usage}`.

## How it fits together

- **net/http on wasm maps onto `fetch`**: streaming SSE reads through the
  response body's stream reader. Cancellation (`run.cancel()`) aborts the
  in-flight request through the context, mid-stream.
- **Blocked goroutines park, the event loop keeps spinning.** A JS tool
  returning a Promise suspends its handler goroutine until the promise
  settles; other events keep flowing. Runs start on fresh goroutines, so
  several conversations proceed concurrently.
- **The keep-alive is a rescheduled timer, not `select{}`** — a parked
  select trips wasm's deadlock detector; a pending timer does not.
- **Node detection is defeated on purpose** (`process.argv0` scrub in
  `main.go`): Go's http transport switches to a fake network under Node,
  which would break the smoke test's real sockets. Browsers have no
  `process` object and are unaffected.

## Test

```sh
cd wasm && ./build.sh && node smoke.mjs
```

The smoke test spins up a fake OpenAI-compatible SSE endpoint and drives
the real compiled module through it: streamed content, sync and async
tools, failing/unknown/malformed tool calls, multi-turn `send()`,
`cancel()` mid-stream, usage accounting, and the `nemo.js` wrapper.
