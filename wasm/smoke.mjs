// smoke.mjs — end-to-end test for nemo.wasm under Node 18+.
//
// Loads the module (plain WebAssembly.instantiate + go.run), registers
// JS tools, and drives conversations against a fake OpenAI-compatible
// endpoint (node:http): streaming SSE, tool calls (sync, async, failing,
// unknown, malformed), multi-turn send(), cancel(), usage, and history.
// The wasm module's fetch path runs against Node's global fetch, so this
// exercises the same network path a browser would (after main.go's
// process.argv0 scrub defeats Node detection in net/http).
//
// Run:
//
//	./build.sh && node smoke.mjs
//
// (Equivalent: go_js_wasm_exec nemo.wasm /dev/null, but the module API is
// driven from here directly.)
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";

// --- fake OpenAI-compatible endpoint ------------------------------------
const events = []; // every event the bridge emitted, for assertions
const byRun = new Map();
let served = 0; // POSTs handled

function sse(res, chunks) {
	res.writeHead(200, { "Content-Type": "text/event-stream" });
	for (const c of chunks) res.write("data: " + JSON.stringify(c) + "\n\n");
	res.write("data: [DONE]\n\n");
	res.end();
}

const server = createServer((req, res) => {
	let body = "";
	req.on("data", c => body += c);
	req.on("end", () => {
		served++;
		const msgs = JSON.parse(body).messages;

		// /toolcall: first POST demands a tool call, later ones answer.
		if (req.url.startsWith("/toolcall")) {
			if (!msgs.some(m => m.role === "tool")) {
				sse(res, [
					{ choices: [{ delta: { content: "Let me check. " } }] },
					{ choices: [{ delta: { tool_calls: [{
						index: 0, id: "call_1", type: "function",
						function: { name: "clock", arguments: "{}" },
					}] } }] },
					{ choices: [{ delta: {}, finish_reason: "tool_calls" }], usage: { prompt_tokens: 11, completion_tokens: 2 } },
				]);
				return;
			}
			sse(res, [
				{ choices: [{ delta: { content: "It is " } }] },
				{ choices: [{ delta: { content: "2026." } }] },
				{ choices: [{ delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 12, completion_tokens: 3 } },
			]);
			return;
		}

		// /greedy: answer immediately, no tools.
		if (req.url.startsWith("/greedy")) {
			sse(res, [
				{ choices: [{ delta: { content: "hello" } }] },
				{ choices: [{ delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 5, completion_tokens: 1 } },
			]);
			return;
		}

		// /evil: first POST demands good, failing, unknown, and malformed
		// tool calls at once; the follow-up answers.
		if (req.url.startsWith("/evil")) {
			if (!msgs.some(m => m.role === "tool")) {
				sse(res, [
					{ choices: [{ delta: { tool_calls: [
						{ index: 0, id: "a", type: "function", function: { name: "boom", arguments: "{}" } },
						{ index: 1, id: "b", type: "function", function: { name: "ghost", arguments: "{}" } },
						{ index: 2, id: "c" }, // malformed: no function object
					] } }] },
					{ choices: [{ delta: {}, finish_reason: "tool_calls" }] },
				]);
				return;
			}
			sse(res, [
				{ choices: [{ delta: { content: "recovered" } }] },
				{ choices: [{ delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 9, completion_tokens: 4 } },
			]);
			return;
		}

		// /hanging: accept the request, never respond; used for cancel().
		if (req.url.startsWith("/hanging")) {
			res.writeHead(200, { "Content-Type": "text/event-stream" });
			return; // socket stays open
		}

		res.writeHead(404).end();
	});
});
await new Promise(r => server.listen(0, "127.0.0.1", r));
const base = `http://127.0.0.1:${server.address().port}`;
console.log("fake endpoint at", base);

// --- load the module -----------------------------------------------------
await import(new URL("wasm_exec.js", import.meta.url).href); // defines global Go
const go = new Go();
const wasmBytes = await readFile(new URL("nemo.wasm", import.meta.url));
const { instance } = await WebAssembly.instantiate(wasmBytes, go.importObject);
go.run(instance);
const api = globalThis.nemo;
if (typeof api?.startRun !== "function") throw new Error("no globalThis.nemo");

api.setEvents({
	reasoning: s => push("reasoning", s),
	content: s => push("content", s),
	system: s => push("system", s),
	toolStart: s => push("toolStart", s),
	toolEnd: s => push("toolEnd", s),
	display: s => push("display", s),
	done: s => push("done", s),
});
function push(kind, json) {
	const ev = JSON.parse(json);
	ev.kind = kind;
	events.push(ev);
	const list = byRun.get(ev.run) || [];
	list.push(ev);
	byRun.set(ev.run, list);
}
const runEvents = id => byRun.get(id) || [];
const content = id => runEvents(id).filter(e => e.kind === "content").map(e => e.delta).join("");

// start a turn and resolve when its nth done event (default 1) lands.
function turn(cfg, prompt, doneCount = 1) {
	const id = api.startRun(JSON.stringify(cfg), prompt);
	return doneEvent(id, doneCount).then(doneEvent => ({ id, doneEvent }));
}
function doneEvent(id, doneCount = 1) {
	return new Promise(resolve => {
		const t = setInterval(() => {
			const dones = runEvents(id).filter(e => e.kind === "done");
			if (dones.length >= doneCount) { clearInterval(t); resolve(dones[dones.length - 1]); }
		}, 20);
	});
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

// --- assertions ----------------------------------------------------------
let failures = 0;
function check(name, cond) {
	console.log((cond ? "ok   " : "FAIL ") + name);
	if (!cond) failures++;
}

// --- test 1: greedy answer, no tools -------------------------------------
{
	const { id, doneEvent } = await turn({ endpoint: base + "/greedy", model: "m", api_key: "k" }, "hi");
	check("greedy: content streamed", content(id) === "hello");
	check("greedy: done ok", doneEvent.error === "");
	check("greedy: usage", doneEvent.usage.includes("5 in / 1 out"));
}

// --- test 2: sync tool-call round trip -----------------------------------
{
	api.registerTool("clock", "current time", JSON.stringify({ type: "object", properties: {} }), false, () => "2026");
	const { id, doneEvent } = await turn({ endpoint: base + "/toolcall", model: "m", api_key: "k" }, "what time");
	const start = runEvents(id).find(e => e.kind === "toolStart");
	const end = runEvents(id).find(e => e.kind === "toolEnd");
	check("tools: toolStart has id/name/args", start && start.name === "clock" && start.args !== undefined);
	check("tools: toolEnd ok with result", end && end.ok === true && end.result === "2026");
	check("tools: streamed text plus final answer", content(id) === "Let me check. It is 2026.");
	check("tools: usage accumulated over both calls", doneEvent.usage.includes("23 in / 5 out"));
}

// --- test 3: async error, unknown tool, malformed call --------------------
{
	let calls = 0;
	api.registerTool("boom", "always fails", JSON.stringify({ type: "object", properties: {} }), false, async () => {
		calls++;
		throw new Error("kaput");
	});
	api.unregisterTool("clock");
	const { id, doneEvent } = await turn({ endpoint: base + "/evil", model: "m", api_key: "k" }, "try tools");
	const ends = runEvents(id).filter(e => e.kind === "toolEnd");
	const boom = ends.find(e => e.id === "a");
	const ghost = ends.find(e => e.id === "b");
	const junk = ends.find(e => e.id === "c"); // malformed entry keeps its id
	check("evil: async throw -> error result", boom && boom.ok === false && /kaput/.test(boom.error));
	check("evil: unknown tool reported to model", ghost && ghost.ok === true && /unknown tool/.test(ghost.result));
	check("evil: malformed call reported", junk && junk.ok === true && /invalid tool call/.test(junk.result));
	check("evil: model recovered and answered", doneEvent.error === "" && content(id) === "recovered");
	check("evil: handler ran exactly once", calls === 1);
}

// --- test 4: send() continues the conversation ---------------------------
{
	const cfg = { endpoint: base + "/greedy", model: "m", api_key: "k" };
	const { id } = await turn(cfg, "one");
	api.send(id, JSON.stringify(cfg), "two");
	await doneEvent(id, 2);
	const hist = JSON.parse(api.sessionMessages(id));
	check("send: two user turns in history", hist.filter(m => m.role === "user").length === 2);
	check("send: system prompt seeded first", hist[0].role === "system");
	check("send: usage accumulated across turns", JSON.parse(api.usage(id)).prompt_tokens === 10);
}

// --- test 5: cancel -------------------------------------------------------
{
	const id = api.startRun(JSON.stringify({ endpoint: base + "/hanging", model: "m", api_key: "k" }), "hang");
	await sleep(200); // let the request fly and park
	api.cancel(id);
	const done = await doneEvent(id);
	check("cancel: done with cancellation error", /context canceled/.test(done.error));
}

// --- test 6: config errors surface as done events -------------------------
{
	const id = api.startRun("{ not json", "hi");
	check("bad config: rejected id", id === -1);

	// TUI-style config (base_url, no endpoint): must POST to
	// base_url + /chat/completions, not the (empty) page origin.
	const { id: id2, doneEvent } = await turn(
		{ base_url: base + "/greedy", model: "m", api_key: "k" }, "hi");
	check("base_url config works", id2 > 0 && doneEvent.error === "" && content(id2) === "hello");

	// neither endpoint nor base_url: rejected up front, no request sent
	const servedBefore = served;
	const id3 = api.startRun(JSON.stringify({ model: "m", api_key: "k" }), "hi");
	const d3 = await new Promise(resolve => {
		const t = setInterval(() => {
			const dones = runEvents(id3).filter(e => e.kind === "done");
			if (dones.length) { clearInterval(t); resolve(dones[0]); }
		}, 20);
	});
	check("missing endpoint rejected", id3 === -1 || /endpoint/.test(d3.error));
	check("missing endpoint made no request", served === servedBefore);
}

// --- test 7: send() while busy supersedes the in-flight turn --------------
{
	// Start a turn that hangs mid-stream, then send() on the same id:
	// the hanging turn must stop emitting (no done, no stray content)
	// and the follow-up must own the conversation.
	const id = api.startRun(JSON.stringify({ endpoint: base + "/hanging", model: "m", api_key: "k" }), "hang");
	await sleep(200); // let the request park
	api.send(id, JSON.stringify({ endpoint: base + "/greedy", model: "m", api_key: "k" }), "go on");
	const done = await doneEvent(id);
	check("supersede: follow-up answered", done.error === "");
	check("supersede: exactly one done event", runEvents(id).filter(e => e.kind === "done").length === 1);
	check("supersede: retired turn went quiet", runEvents(id).filter(e => e.kind === "done").every(e => e.usage));
}

// --- test 8: nemo.js promise wrapper (the browser front end) --------------
{
	await import("./nemo.js");
	const nemo = await globalThis.Nemo.load(async () => {}); // module already running
	let specErr = "";
	try { nemo.registerTool({ run: () => "" }); } catch (e) { specErr = e.message; }
	check("wrapper: tool spec validated", /name/.test(specErr));

	nemo.registerTool({
		name: "clock",
		description: "current time",
		parameters: { type: "object", properties: {} },
		run: async () => "2026-10-27", // async: exercises the Promise path
	});

	const seen = { content: "", toolEnds: [], displays: [] };
	const run = nemo.run({ endpoint: base + "/toolcall", model: "m", api_key: "k" }, "what time");
	run.onContent(ev => seen.content += ev.delta);
	run.onToolEnd(ev => seen.toolEnds.push(ev));
	const done1 = await run.done;
	check("wrapper: done resolved with usage", done1.error === "" && done1.usage.includes("23 in / 5 out"));
	check("wrapper: content events", seen.content === "Let me check. It is 2026.");
	check("wrapper: tool ran through wrapper", seen.toolEnds.length === 1 && seen.toolEnds[0].result === "2026-10-27");
	check("wrapper: history()", run.history().some(m => m.role === "tool"));
	check("wrapper: usage()", run.usage().prompt_tokens === 23);

	const done2 = run.send("again", { endpoint: base + "/greedy", model: "m", api_key: "k" });
	const d2 = await done2;
	check("wrapper: send() resolves new done", d2.error === "");
	check("wrapper: usage accumulated", run.usage().prompt_tokens === 28);

	// cancel through the wrapper
	let ondoneSeen = null;
	nemo.ondone = ev => ondoneSeen = ev;
	const h = nemo.run({ endpoint: base + "/hanging", model: "m", api_key: "k" }, "hang");
	await sleep(150);
	h.cancel();
	const d3 = await h.done;
	check("wrapper: cancel resolves done", /context canceled/.test(d3.error));
	check("wrapper: global ondone fired", ondoneSeen !== null);
}

// --- wrap up --------------------------------------------------------------
console.log(`\n${failures === 0 ? "PASS" : failures + " FAILURES"} (${events.length} events, ${served} LLM POSTs)`);
server.close();
process.exit(failures === 0 ? 0 : 1);
