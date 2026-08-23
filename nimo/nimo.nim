## A single-shot agent loop: talks to an OpenAI-compatible chat completions
## endpoint (OpenRouter by default), exposes one `fetch` tool to the model
## backed by a plain httpclient request.
##
## Cancellation (Ctrl-C/SIGTERM) and the run deadline are enforced by checks
## between requests plus per-request socket timeouts, not by aborting an
## in-flight request mid-flight.
##
## Build & run:  nim c -r nimo.nim   (needs OPENROUTER_API_KEY set)

import std/[httpclient, json, net, os, re, streams, strutils, tables, times,
            uri]

when not defined(windows):
  from std/posix import signal, SIGINT, SIGTERM

# Timeouts and size limits for outbound requests. ---------------------------

const
  llmTimeoutMs* = 32_000    ## llmTimeout caps one LLM round trip.
  fetchTimeoutMs* = 15_000  ## fetchTimeout caps one tool fetch.
  runTimeoutMs* = 600_000   ## runTimeout bounds the entire run, so a wedged
                            ## run cannot hang forever.

  maxLLMResponseBytes* = 1 shl 20  ## Caps one LLM response body.
  maxToolResponseBytes* = 2 shl 20 ## Hard cap (and default) for tool fetch
                                   ## bodies; model-supplied max_bytes can
                                   ## only lower it.
  maxToolResultBytes* = 64 shl 10  ## Caps a tool result fed back into the
                                   ## conversation.

  chromeUserAgent* = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " &
    "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

# Run context: deadline + cancellation (context.WithTimeout + signals). ------

type
  ContextCancelledError* = object of CatchableError
    ## The run was cancelled from a signal handler (context.Canceled).

  RunContext* = object
    ## A poor man's context.Context: the run's deadline plus a cancellation
    ## flag shared with the SIGINT/SIGTERM handlers (signal.NotifyContext +
    ## context.WithTimeout in the original).
    deadline*: Time

var runCancelled = false

when not defined(windows):
  proc onSignal(sig: cint) {.noconv.} =
    runCancelled = true

proc installSignalHandlers*() =
  ## Ctrl-C/SIGTERM cancels the run.
  when defined(windows):
    proc onCtrlC() {.noconv.} =
      runCancelled = true
    setControlCHook(onCtrlC)
  else:
    signal(SIGINT, onSignal)
    signal(SIGTERM, onSignal)

proc ctxErr*(ctx: RunContext): ref CatchableError =
  ## ctx.Err(): nil means keep going.
  if runCancelled:
    return newException(ContextCancelledError, "context canceled")
  if not (getTime() <= ctx.deadline):
    return newException(TimeoutError, "context deadline exceeded")
  nil

proc remainingMs*(ctx: RunContext): int =
  let ms = (ctx.deadline - getTime()).inMilliseconds
  if ms < 0: 0 else: ms.int

# stolen from the picoclaw repo ------------------------------------------------
# (pre-compiled regexes for HTML text extraction; the DDG ones are unused
# here, exactly as in the Go original)

let
  reScript* = re"<script[\s\S]*?</script>"
  reStyle* = re"<style[\s\S]*?</style>"
  reTags* = re"<[^>]+>"
  reWhitespace* = re"[^\S\n]+"
  reBlankLines* = re"\n{3,}"
  # DuckDuckGo result extraction
  reDDGLink* = re"""<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>([\s\S]*?)</a>"""
  reDDGSnippet* = re"""<a class="result__snippet[^"]*".*?>([\s\S]*?)</a>"""

proc extractText*(htmlContent: string): string =
  result = htmlContent.replace(reScript, "")
  result = result.replace(reStyle, "")
  result = result.replace(reTags, "")

  result = result.strip

  result = result.replace(reWhitespace, " ")
  result = result.replace(reBlankLines, "\n\n")

  var cleanLines: seq[string] = @[]
  for line in result.split("\n"):
    let l = line.strip
    if l != "":
      cleanLines.add(l)

  result = cleanLines.join("\n")

# Small helpers. --------------------------------------------------------------

proc truncateUtf8*(s: string, limit: int, marker: string): string =
  ## Cuts s to at most limit bytes without splitting a multi-byte rune,
  ## appending marker when something was cut.
  if s.len <= limit:
    return s
  var cut = limit
  while cut > 0 and (ord(s[cut]) and 0xC0) == 0x80:
    dec cut
  s.substr(0, cut - 1) & marker

proc readUpTo*(input: Stream, limit: int): string =
  ## Reads up to limit bytes, looping past short reads (io.LimitReader).
  const chunk = 64 * 1024
  result = newStringOfCap(min(limit, chunk))
  while result.len < limit:
    let piece = input.readStr(min(chunk, limit - result.len))
    if piece.len == 0:
      break
    result.add(piece)

proc readCapped*(input: Stream, limit: int): string =
  ## Reads up to limit bytes, erroring if the body is larger (rather than
  ## truncating it into invalid JSON).
  let data = readUpTo(input, limit + 1)
  if data.len > limit:
    raise newException(ValueError, "response exceeds " & $limit & " byte limit")
  data

proc extractUpstreamError*(node: JsonNode): string =
  ## Pulls a human-readable message out of an OpenAI-style error body:
  ## {"error": "msg"} or {"error": {"message": "msg", ...}}.
  let e = node{"error"}
  if e == nil or e.kind == JNull:
    return ""
  if e.kind == JString:
    return e.getStr
  if e.kind == JObject:
    let m = e{"message"}
    if m != nil and m.kind == JString and m.getStr != "":
      return m.getStr
  ""

proc jstr*(node: JsonNode, key: string): string =
  ## Kind-checked string field access; "" when missing or not a string.
  let v = node{key}
  if v != nil and v.kind == JString: v.getStr else: ""

# Outbound requests: one shared client. --------------------------------------

var client: HttpClient

proc ensureHttpClient*() =
  ## One shared client for all outbound traffic (LLM calls and fetches
  ## alike), with TLS certificates verified.
  if client != nil:
    return
  client = newHttpClient(userAgent = chromeUserAgent,
                         sslContext = newContext(verifyMode = CVerifyPeer))

proc httpRequest*(ctx: RunContext, meth, urlStr, body: string,
                  headers: seq[tuple[name, value: string]],
                  perCallTimeout: Duration): Response =
  ## Makes one HTTP request: http/https only, with a per-request deadline of
  ## min(perCallTimeout, remaining run budget).
  let u = parseUri(urlStr)
  if u.scheme != "http" and u.scheme != "https":
    raise newException(ValueError,
      "scheme \"" & u.scheme & "\" not allowed (http/https only)")
  let cerr = ctxErr(ctx)
  if cerr != nil:
    raise cerr

  client.timeout = min(perCallTimeout.inMilliseconds.int, remainingMs(ctx))

  let httpHeaders = newHttpHeaders()
  for h in headers:
    httpHeaders[h.name] = h.value

  let methodEnum = parseEnum[HttpMethod](meth, HttpGet)
  result = client.request(urlStr, methodEnum, body, httpHeaders)

# tool defs -------------------------------------------------------------------

type
  Tool* = object
    ## A function the model may call; ctx carries the run's deadline and
    ## cancellation.
    name*: string
    description*: string
    parameters*: JsonNode
    handler*: proc (ctx: RunContext, args: JsonNode): string

let fetchParameters* = %*{
  "type": "object",
  "properties": {
    "url": {"type": "string"},
    "method": {"type": "string", "default": "GET"},
    "headers": {
      "type": "object",
      "additionalProperties": {"type": "string"}
    },
    "body": {"type": "string"},
    "max_bytes": {
      "type": "integer",
      "description": "maximum response size in bytes (default 2097152)"
    },
    "readable": {
      "type": "boolean",
      "description": "light postprocessing to extract readable content by " &
                     "removing script and style tags, etc."
    }
  },
  "required": ["url"]
}

proc fetchHandler*(ctx: RunContext, args: JsonNode): string =
  let urlNode = args{"url"}
  if urlNode == nil or urlNode.kind != JString or urlNode.getStr == "":
    raise newException(ValueError, "url is required")
  let urlStr = urlNode.getStr

  var meth = "GET"
  let m = args{"method"}
  if m != nil and m.kind == JString and m.getStr != "":
    meth = m.getStr.toUpperAscii

  var body = ""
  let b = args{"body"}
  if b != nil and b.kind == JString and b.getStr != "":
    body = b.getStr

  var headers: seq[tuple[name, value: string]] = @[("User-Agent", chromeUserAgent)]
  let h = args{"headers"}
  if h != nil and h.kind == JObject:
    for k, v in h.pairs:
      if v.kind == JString:
        headers.add((k, v.getStr))

  # Clamp the model-supplied cap; only values below the hard cap are honored.
  var maxBytes = maxToolResponseBytes
  let mb = args{"max_bytes"}
  if mb != nil and mb.kind in {JInt, JFloat}:
    let n = if mb.kind == JInt: mb.getInt else: int(mb.getFloat)
    if n > 0 and n < maxToolResponseBytes:
      maxBytes = n

  let resp = httpRequest(ctx, meth, urlStr, body, headers,
                         initDuration(milliseconds = fetchTimeoutMs))
  let data = readUpTo(resp.bodyStream, maxBytes)

  let r = args{"readable"}
  if r != nil and r.kind == JBool and r.getBool:
    return extractText(data)
  data

let toolRegistry* = {
  "fetch": Tool(
    name: "fetch",
    description: "make an HTTP request to a URL and return the response " &
                 "body or extracted readable content",
    parameters: fetchParameters,
    handler: fetchHandler),
}.toTable

proc buildToolSpecs*(): JsonNode =
  result = newJArray()
  for t in toolRegistry.values:
    result.add(%*{
      "type": "function",
      "function": {
        "name": t.name,
        "description": t.description,
        "parameters": t.parameters
      }
    })

# The agent loop. --------------------------------------------------------------

proc invokeIntelligence*(ctx: RunContext, username, message, url, model,
                         apiKey: string): string =
  ensureHttpClient()

  let messages = newJArray()

  proc addMessage(msg: JsonNode) =
    messages.add(msg)
    echo msg.pretty

  addMessage(%*{
    "role": "system",
    "content": "You are an assistant. Current time: " &
      now().format("yyyy-MM-dd'T'HH:mm:sszzz") &
      ". Use fetch from https://html.duckduckgo.com/html/?q=<query> for web search."
  })
  addMessage(%*{"role": "user", "content": username & ": " & message})

  let tools = buildToolSpecs()

  const maxToolIterations = 32
  for _ in 0 ..< maxToolIterations:
    let cerr = ctxErr(ctx)
    if cerr != nil:
      raise cerr

    let reqBody = %*{
      "model": model,
      "messages": messages,
      "tools": tools,
      "tool_choice": "auto"
    }

    let resp = httpRequest(ctx, "POST", url, $reqBody, @[
      ("Authorization", "Bearer " & apiKey),
      ("Content-Type", "application/json"),
    ], initDuration(milliseconds = llmTimeoutMs))

    let body = readCapped(resp.bodyStream, maxLLMResponseBytes)

    # Surface upstream failures with their status and body.
    if int(resp.code) != 200:
      raise newException(ValueError, "upstream " & resp.status & ": " &
        truncateUtf8(body.strip, 1024, " ...[truncated]"))

    let parsed =
      try:
        parseJson(body)
      except CatchableError as je:
        let e = newException(ValueError, "invalid JSON from upstream: " & je.msg)
        e.parent = je
        raise e

    let upstreamMsg = extractUpstreamError(parsed)
    if upstreamMsg != "":
      raise newException(ValueError, "upstream error: " & upstreamMsg)

    # Malformed responses error out rather than crash.
    let choices = parsed{"choices"}
    if choices == nil or choices.kind != JArray or choices.len == 0:
      raise newException(ValueError, "invalid or empty choices in response")
    let first = choices[0]
    if first.kind != JObject:
      raise newException(ValueError, "malformed choice entry in response")
    let msg = first{"message"}
    if msg == nil or msg.kind != JObject:
      raise newException(ValueError, "response choice has no message object")

    addMessage(msg)

    # No (or malformed) tool_calls: the message content is the final answer.
    let toolCalls = msg{"tool_calls"}
    if toolCalls == nil or toolCalls.kind != JArray or toolCalls.len == 0:
      let content = msg{"content"}
      if content != nil and content.kind == JString:
        return content.getStr
      raise newException(ValueError,
        "no tool calls and failed to parse msg['content']")

    # process tool calls
    for tci in toolCalls:
      let call = tci
      let callID = jstr(call, "id")
      let fn = call{"function"}
      let name = if fn != nil: jstr(fn, "name") else: ""
      let argStr = if fn != nil: jstr(fn, "arguments") else: ""

      var toolResult: string
      try:
        let args = parseJson(argStr)
        if args.kind != JObject:
          raise newException(ValueError, "expected a JSON object")
        if toolRegistry.hasKey(name):
          try:
            toolResult = toolRegistry[name].handler(ctx, args)
          except CatchableError as te:
            toolResult = te.msg
        else:
          toolResult = "unknown tool"
      except CatchableError as pe:
        # Report bad arguments back to the model.
        toolResult = "invalid tool arguments: " & pe.msg

      toolResult = truncateUtf8(toolResult, maxToolResultBytes, "\n[...truncated...]")

      addMessage(%*{
        "role": "tool",
        "tool_call_id": callID,
        "content": toolResult
      })

  raise newException(ValueError, "max iterations reached")

proc main() =
  let apiKey = getEnv("OPENROUTER_API_KEY")
  if apiKey == "":
    echo "error: OPENROUTER_API_KEY not set"
    return

  # Ctrl-C/SIGTERM cancels the run; runTimeout bounds it.
  installSignalHandlers()
  ensureHttpClient()

  let url = "https://openrouter.ai/api/v1/chat/completions"
  let model = "deepseek/deepseek-v4-flash-0731"

  let ctx = RunContext(deadline: getTime() + milliseconds(runTimeoutMs))

  try:
    #let answer = invokeIntelligence(ctx, "alice", "How old is Josipa Lisac?", url, model, apiKey)
    let answer = invokeIntelligence(ctx, "alice", "What will the weather be tomorrow around Krapina, Croatia?", url, model, apiKey)
    #let answer = invokeIntelligence(ctx, "alice", "What time is it in Croatia?", url, model, apiKey)
    echo answer
  except CatchableError as e:
    echo "error: ", e.msg

when isMainModule:
  main()
