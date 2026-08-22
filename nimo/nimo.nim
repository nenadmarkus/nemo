## A single-shot agent loop: talks to an OpenAI-compatible chat completions
## endpoint (OpenRouter by default), exposes one `fetch` tool to the model,
## and guards every outbound request against SSRF.
##
## Deliberate divergences from the Go original, forced by Nim's stdlib:
##   * Nim's httpclient has no dial hook, so instead of resolving + validating
##     + dialing a validated IP ourselves, every hop (the initial URL and each
##     redirect, since redirects are followed manually) is resolved and
##     validated immediately before the request is issued. The DNS-rebinding
##     TOCTOU window is therefore slightly wider than in the Go version, which
##     pins the connection to a validated IP.
##   * Cancellation (Ctrl-C/SIGTERM) and the run deadline are enforced by
##     checks between requests plus per-request socket timeouts, not by
##     aborting an in-flight request mid-flight.
##
## Build & run:  nim c -r nimo/nimo.nim   (needs OPENROUTER_API_KEY set)

import std/[httpclient, json, nativesockets, net, os, re, sequtils,
            streams, strutils, tables, times, uri]

when not defined(windows):
  from std/posix import signal, SIGINT, SIGTERM

# Timeouts and size limits for outbound requests. ---------------------------

const
  llmTimeoutMs* = 32_000    ## llmTimeout caps one LLM round trip.
  fetchTimeoutMs* = 15_000  ## fetchTimeout caps one tool fetch.
  dialTimeoutMs* = 10_000   ## dialTimeout caps TCP connection establishment;
                            ## Nim's client folds it into the per-request
                            ## timeout (see guardedRequest).
  runTimeoutMs* = 600_000   ## runTimeout bounds the entire run, so a wedged
                            ## run cannot hang forever.

  maxLLMResponseBytes* = 1 shl 20  ## Caps one LLM response body.
  maxToolResponseBytes* = 2 shl 20 ## Hard cap (and default) for tool fetch
                                   ## bodies; model-supplied max_bytes can
                                   ## only lower it.
  maxToolResultBytes* = 64 shl 10  ## Caps a tool result fed back into the
                                   ## conversation.

  maxRedirects* = 10        ## maxRedirects bounds redirect chains.

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

# Outbound request policy: SSRF guard, redirect rules, shared clients. -------

template isZeroRange(a: array[16, uint8], first, last: int): bool =
  (block:
    var allZero = true
    for i in first .. last:
      if a[i] != 0'u8:
        allZero = false
        break
    allZero)

proc isPublicIP*(ip: IpAddress): bool =
  ## Reports whether ip is globally routable; loopback, private, link-local,
  ## multicast, unspecified, broadcast and CGNAT space are not.
  if ip.family == IpAddressFamily.IPv4:
    let b = ip.address_v4
    if b[0] == 127: return false                                  # loopback
    if b[0] == 10: return false                                   # 10/8
    if b[0] == 172 and (b[1] and 0xF0) == 16: return false        # 172.16/12
    if b[0] == 192 and b[1] == 168: return false                  # 192.168/16
    if b[0] == 169 and b[1] == 254: return false                  # link-local
    if b[0] >= 224 and b[0] <= 239: return false                  # multicast
    if b == [0'u8, 0, 0, 0]: return false                         # unspecified
    if b == [255'u8, 255, 255, 255]: return false                 # broadcast
    if b[0] == 100 and b[1] >= 64 and b[1] <= 127: return false   # CGNAT 100.64/10
    result = true
  else:
    let b = ip.address_v6
    # IPv4-mapped ::ffff:0:0/96 addresses follow the IPv4 rules.
    if b.isZeroRange(0, 9) and b[10] == 0xFF'u8 and b[11] == 0xFF'u8:
      return isPublicIP(IpAddress(family: IpAddressFamily.IPv4,
                                  address_v4: [b[12], b[13], b[14], b[15]]))
    if b[0] == 0xFF'u8: return false                              # ff00::/8 multicast
    if (b[0] and 0xFE'u8) == 0xFC'u8: return false                # fc00::/7 private
    if b[0] == 0xFE'u8 and (b[1] and 0xC0'u8) == 0x80'u8: return false # fe80::/10
    if b.isZeroRange(0, 14) and b[15] == 1'u8: return false       # ::1 loopback
    if b.isZeroRange(0, 15): return false                         # :: unspecified
    # Slightly stricter than the Go original: the deprecated
    # IPv4-compatible ::a.b.c.d space is refused outright too.
    if b.isZeroRange(0, 11): return false
    result = true

proc lookupIPs*(host: string): seq[IpAddress] =
  ## net.DefaultResolver.LookupIPAddr equivalent: every address the resolver
  ## returns for host (A and AAAA alike).
  var aiList = getAddrInfo(host, Port(0), AF_UNSPEC)
  defer: freeAddrInfo(aiList)
  var it = aiList
  while it != nil:
    result.add(parseIpAddress(getAddrString(it.ai_addr)))
    it = it.ai_next

proc assertPublicHost*(host: string) =
  ## The SSRF gate for every outbound connection: resolve the host and
  ## refuse non-public addresses. Validation runs right before each request,
  ## covering redirect hops too, because redirects are followed manually
  ## below. (See the module doc for the dial-time divergence from Go.)
  if host == "":
    raise newException(ValueError, "host is required")
  let h = host.strip(chars = {'[', ']'}) # bracketed IPv6 literals
  if isIpAddress(h):
    let ip = parseIpAddress(h)
    if not isPublicIP(ip):
      raise newException(ValueError,
        "blocked non-public address " & $ip & " for \"" & h & "\"")
  else:
    let ips = lookupIPs(h)
    if ips.len == 0:
      raise newException(IOError,
        "host \"" & h & "\" did not resolve to any address")
    for ip in ips:
      if not isPublicIP(ip):
        raise newException(ValueError,
          "blocked non-public address " & $ip & " for \"" & h & "\"")

proc proxyFromEnv*(): Proxy =
  ## http.ProxyFromEnvironment equivalent: HTTPS_PROXY/https_proxy wins over
  ## HTTP_PROXY/http_proxy; malformed values are ignored.
  for key in ["HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"]:
    let v = getEnv(key)
    if v != "":
      try:
        return newProxy(v)
      except CatchableError:
        return nil
  nil

proc noProxyMatches*(host: string): bool =
  ## Simplified NO_PROXY matching: "*" or comma-separated exact/suffix
  ## domain entries.
  let list = getEnv("NO_PROXY") & "," & getEnv("no_proxy")
  if list == ",": return false
  let h = host.strip(chars = {'[', ']'}).toLowerAscii
  for entry0 in list.split(','):
    var entry = entry0.strip.toLowerAscii
    if entry == "": continue
    if entry == "*": return true
    if entry[0] == '.': entry = entry[1 ..^ 1]
    if entry != "" and (h == entry or h.endsWith("." & entry)):
      return true
  false

var
  directClient: HttpClient
  proxyClient: HttpClient
  envProxy: Proxy = nil

proc ensureHttpClients*() =
  ## A shared pair of clients for all outbound traffic (LLM calls and
  ## fetches alike); every request, initial or redirected, passes through the
  ## host guard and the redirect policy below. TLS certificates are verified.
  if directClient != nil:
    return
  let sslCtx = newContext(verifyMode = CVerifyPeer)
  directClient = newHttpClient(userAgent = chromeUserAgent, maxRedirects = 0,
                               sslContext = sslCtx)
  envProxy = proxyFromEnv()
  if envProxy != nil:
    proxyClient = newHttpClient(userAgent = chromeUserAgent, maxRedirects = 0,
                                sslContext = sslCtx, proxy = envProxy)

proc pickClient*(targetHost: string): tuple[client: HttpClient, dialHost: string] =
  ## Behind an explicit HTTP(S)_PROXY only the proxy itself is dialed, so
  ## only the proxy's address gets validated.
  if envProxy != nil and proxyClient != nil and not noProxyMatches(targetHost):
    result = (proxyClient, envProxy.url.hostname)
  else:
    result = (directClient, targetHost)

proc hostKey*(u: Uri): string =
  let h = u.hostname.strip(chars = {'[', ']'}).toLowerAscii
  let p = if u.port == "": (if u.scheme == "https": "443" else: "80")
          else: u.port
  h & ":" & p

proc resolveReference(base: Uri, reference: string): Uri =
  ## RFC 3986 §5-style resolution for redirect Location values (covers
  ## absolute URLs, network-path, absolute-path, relative-path, and
  ## query/fragment-only references).
  if reference == "":
    return base
  let r = parseUri(reference)
  if r.scheme != "":
    return r
  if reference[0] == '#':
    result = base
    result.anchor = r.anchor
    return
  if reference[0] == '?':
    result = base
    result.query = r.query
    result.anchor = r.anchor
    return
  if r.hostname != "":
    result = r
    result.scheme = base.scheme
    return
  result = base
  if reference[0] == '/':
    result.path = r.path
  else:
    let idx = base.path.rfind('/')
    if idx < 0:
      result.path = "/" & r.path
    else:
      result.path = base.path[0 .. idx] & r.path
  result.query = r.query
  result.anchor = r.anchor

proc guardedRequest*(ctx: RunContext, meth0, urlStr, body0: string,
                     headers0: seq[tuple[name, value: string]],
                     perCallTimeout: Duration): Response =
  ## Makes one policy-checked HTTP request: http/https only, SSRF-guarded
  ## host resolution (or a guarded proxy host when HTTP(S)_PROXY is in
  ## effect), a bounded redirect chain (Go: guardedDialContext +
  ## checkRedirect), and a per-request deadline of
  ## min(perCallTimeout, remaining run budget).
  var current = parseUri(urlStr)
  var redirectsFollowed = 0
  let originHostKey = hostKey(current)
  var meth = meth0
  var body = body0
  var headers = headers0

  while true:
    let cerr = ctxErr(ctx)
    if cerr != nil:
      raise cerr

    if current.scheme != "http" and current.scheme != "https":
      if redirectsFollowed == 0:
        raise newException(ValueError,
          "scheme \"" & current.scheme & "\" not allowed (http/https only)")
      raise newException(ValueError,
        "blocked redirect to scheme \"" & current.scheme & "\"")

    let picked = pickClient(current.hostname)
    doAssert picked.client != nil, "call ensureHttpClients() first"
    assertPublicHost(picked.dialHost)

    let ms = min(perCallTimeout.inMilliseconds.int, remainingMs(ctx))
    if ms <= 0:
      raise newException(TimeoutError, "context deadline exceeded")
    picked.client.timeout = ms

    let httpHeaders = newHttpHeaders()
    for h in headers:
      httpHeaders[h.name] = h.value

    let resp = picked.client.request($current, meth, body, httpHeaders)

    if resp.code in {Http301, Http302, Http303, Http307, Http308}:
      let location = resp.headers.getOrDefault("location")
      if location != "":
        # checkRedirect: bounded chain length; scheme and host of the next
        # hop are re-validated at the top of the loop.
        if redirectsFollowed >= maxRedirects:
          raise newException(ValueError,
            "stopped after " & $maxRedirects & " redirects")
        inc redirectsFollowed
        current = resolveReference(current, location)
        if hostKey(current) != originHostKey:
          # Go's client strips Authorization on cross-host redirects.
          headers = headers.filterIt(it.name.toLowerAscii != "authorization")
        if resp.code in {Http301, Http302, Http303} and meth != "GET" and meth != "HEAD":
          meth = "GET"
          body = ""
        continue

    return resp

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

  # http/https only; the address itself is enforced per request by
  # guardedRequest, which covers redirect hops too.
  let u = parseUri(urlStr)
  if u.scheme != "http" and u.scheme != "https":
    raise newException(ValueError,
      "scheme \"" & u.scheme & "\" not allowed (http/https only)")

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

  let resp = guardedRequest(ctx, meth, urlStr, body, headers,
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
  ensureHttpClients()

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

    let resp = guardedRequest(ctx, "POST", url, $reqBody, @[
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
  ensureHttpClients()

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
