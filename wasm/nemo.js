// nemo.js — browser glue for nemo.wasm.
//
// The wasm module (wasm/main.go) exports a low-level API on
// globalThis.nemo; this file wraps it in a promise/event front end:
//
//   const nemo = await Nemo.load();             // instantiate (idempotent)
//   nemo.registerTool({                         // tools run in the page
//     name: "get_time",
//     description: "current time in the browser",
//     parameters: {type: "object", properties: {}},
//     run: async () => new Date().toISOString(),
//   });
//   const run = nemo.run(config, "prompt");     // starts a conversation
//   run.onContent(d => ...); run.onReasoning(d => ...);
//   await run.done;                             // resolves with the done event
//   await run.send("follow-up");                // same conversation
//   run.cancel();                               // abort mid-stream
//
// config = {endpoint, api_key, model, provider?, temperature?, max_tokens?,
// system_prompt?, max_tool_iterations?, tools? (array of tool names)}.
//
// The browser talks to the LLM endpoint directly (the wasm module uses
// fetch), so the endpoint must send CORS headers allowing your origin.
// API keys stay in the browser: serve over HTTPS and prefer a
// spend-limited key.
(function (global) {
	"use strict";

	let api = null;

	// load(instantiate?) returns the wasm-backed API, instantiating the
	// module on the first call; instantiate defaults to a loader that
	// fetches ./wasm_exec.js and ./nemo.wasm relative to this file.
	async function load(instantiate) {
		if (api) return api;
		await (instantiate || defaultInstantiate)();
		if (!global.nemo || typeof global.nemo.startRun !== "function") {
			throw new Error("nemo.wasm did not install globalThis.nemo");
		}
		api = new NemoApi(global.nemo);
		return api;
	}

	async function defaultInstantiate() {
		await loadScript(scriptDir() + "wasm_exec.js");
		// ArrayBuffer + instantiate (not instantiateStreaming): works
		// regardless of the server's wasm Content-Type.
		const bytes = await (await fetch(scriptDir() + "nemo.wasm")).arrayBuffer();
		const go = new global.Go();
		const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
		go.run(instance); // no await: wasm main parks forever by design
	}

	// scriptDir is this file's directory, with trailing slash.
	function scriptDir() {
		const el = document.currentScript;
		return el && el.src ? el.src.replace(/[^/]*$/, "") : "";
	}

	function loadScript(src) {
		return new Promise((resolve, reject) => {
			const s = document.createElement("script");
			s.src = src;
			s.onload = resolve;
			s.onerror = () => reject(new Error("failed to load " + src));
			document.head.appendChild(s);
		});
	}

	global.Nemo = { load };

	// NemoApi wraps one instantiated wasm module.
	class NemoApi {
		constructor(raw) {
			this.raw = raw;
			this._runs = new Map();
			this.ondone = null; // optional: every done event, all runs
			raw.setEvents({
				reasoning: ev => this._dispatch("reasoning", JSON.parse(ev)),
				content: ev => this._dispatch("content", JSON.parse(ev)),
				system: ev => this._dispatch("system", JSON.parse(ev)),
				toolStart: ev => this._dispatch("toolStart", JSON.parse(ev)),
				toolEnd: ev => this._dispatch("toolEnd", JSON.parse(ev)),
				display: ev => this._dispatch("display", JSON.parse(ev)),
				done: ev => this._finish(JSON.parse(ev)),
			});
		}

		// registerTool(spec) adds a tool. spec = {name, description,
		// parameters (JSON-schema object, optional), mutating (bool),
		// run(args) -> string | {result} | {error}, possibly async}.
		registerTool(spec) {
			if (!spec || typeof spec.name !== "string" || typeof spec.run !== "function") {
				throw new Error("tool spec needs name and run(args)");
			}
			this.raw.registerTool(
				spec.name,
				spec.description || "",
				JSON.stringify(spec.parameters || { type: "object", properties: {} }),
				!!spec.mutating,
				argsJSON => Promise.resolve()
					.then(() => spec.run(JSON.parse(argsJSON)))
					.then(
						out => (out && typeof out === "object" && !Array.isArray(out))
							? out
							: { result: String(out) },
						err => ({ error: errMessage(err) })
					)
			);
			return this;
		}

		unregisterTool(name) {
			this.raw.unregisterTool(name);
		}

		// run(config, prompt) starts a conversation and returns its handle.
		run(config, prompt) {
			const cfg = config || {};
			const id = this.raw.startRun(JSON.stringify(cfg), prompt || "");
			if (id < 0) throw new Error("startRun rejected the config (invalid JSON)");
			const handle = new RunHandle(this, id, cfg);
			this._runs.set(id, handle);
			return handle;
		}

		_dispatch(kind, ev) {
			const run = this._runs.get(ev.run);
			if (run) run._emit(kind, ev);
		}

		_finish(ev) {
			const run = this._runs.get(ev.run);
			if (run) run._done(ev);
			if (this.ondone) this.ondone(ev);
		}
	}

	// RunHandle is one conversation.
	class RunHandle {
		constructor(api, id, config) {
			this.id = id;
			this.api = api;
			this._config = config;
			this._listeners = {};
			this.done = new Promise(resolve => { this._resolveDone = resolve; });
		}

		// on(kind, fn) subscribes; kind = reasoning | content | system |
		// toolStart | toolEnd | display. Returns this for chaining.
		on(kind, fn) {
			(this._listeners[kind] || (this._listeners[kind] = [])).push(fn);
			return this;
		}
		onReasoning(fn) { return this.on("reasoning", fn); }
		onContent(fn) { return this.on("content", fn); }
		onSystem(fn) { return this.on("system", fn); }
		onToolStart(fn) { return this.on("toolStart", fn); }
		onToolEnd(fn) { return this.on("toolEnd", fn); }
		onDisplay(fn) { return this.on("display", fn); }

		// send(message, config?) appends a follow-up turn on the same
		// conversation. config defaults to the one this run started with.
		// Returns the new done promise.
		send(message, config) {
			const cfg = config || this._config;
			this._config = cfg;
			this.api.raw.send(this.id, JSON.stringify(cfg), message);
			this.done = new Promise(resolve => { this._resolveDone = resolve; });
			return this.done;
		}

		// cancel aborts the running turn; done then resolves with the
		// cancellation error.
		cancel() { this.api.raw.cancel(this.id); }

		// history() returns the session's messages.
		history() { return JSON.parse(this.api.raw.sessionMessages(this.id) || "[]"); }

		// usage() returns the session's cumulative token/cost accounting.
		usage() { return JSON.parse(this.api.raw.usage(this.id) || "{}"); }

		_emit(kind, ev) {
			const l = this._listeners[kind];
			if (l) for (const fn of l) fn(ev);
		}
		_done(ev) { this._resolveDone(ev); }
	}

	function errMessage(err) {
		if (err && typeof err === "object" && typeof err.message === "string") return err.message;
		return String(err);
	}
})(globalThis);
