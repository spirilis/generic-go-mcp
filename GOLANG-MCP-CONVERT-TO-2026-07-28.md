# Converting generic-go-mcp to MCP 2026-07-28

Status: implemented on `main`. This document was written before the code changes it describes and
is kept up to date as the reference for anyone embedding this library — every code sample below is
the real, shipped implementation, not illustrative pseudocode.

## 1. Why this is a v2, not a patch

Every previous MCP revision (2024-11-05 through 2025-11-25) was connection-scoped: a client opens
a channel, calls `initialize`, gets a negotiated session, and everything after that trusts state
established during the handshake. `generic-go-mcp` implements that model today — literally: it
hardcodes protocol version `"2024-11-05"` at `mcp/handlers.go:59`, tracks `Server.initialized`
(set, never read), and the HTTP transport mints an `Mcp-Session-Id` and threads it through every
request.

**2026-07-28 deletes that model.** MCP is now a stateless protocol: every request is
self-contained, carrying its own protocol version, capabilities, and identity in
`params._meta`. There is no handshake, no session, and — critically for a Go framework whose
whole job is mediating the wire — no server-initiated JSON-RPC request. Sampling, elicitation,
and roots, which used to arrive as the server pushing a request down an open channel, now travel
as an `InputRequiredResult` on the *response* to the request that needed them (Multi Round-Trip
Requests, MRTR). Change notifications move off a general-purpose SSE channel and onto a single
purpose-built `subscriptions/listen` long-lived request.

None of the five methods this server routes today (`initialize`, `tools/list`, `tools/call`,
`resources/list`, `resources/read`) can be migrated by patching field names. The session lifecycle,
the transport's job, the shape of a `MessageHandler`, and the shape of a `ToolFunction` all change.
That is why this is a rewrite of `mcp/` and `transport/`, not a version bump.

## 2. Hard-cutover decisions

These are binding for the implementation. There is no dual-era support and no compatibility shim.

1. **`2026-07-28` is the only supported protocol version.** `mcp.SupportedVersions = []string{"2026-07-28"}`.
   A request whose `_meta["io.modelcontextprotocol/protocolVersion"]` differs gets
   `UnsupportedProtocolVersionError` (`-32022`) listing that one-element `supported` list. There is
   no fallback negotiation to an earlier version.
2. **`initialize` and `notifications/initialized` are not implemented as protocol.** `initialize`
   is routed to exactly one behavior: return a JSON-RPC error (`-32601`, "Method not found") whose
   `data.supported = ["2026-07-28"]`. This is the spec's own guidance for a modern-only server —
   "name the protocol versions it supports in any error it returns to an `initialize` request...
   this message may be the only diagnostic \[a legacy client] can surface to users." Nothing else
   about the legacy handshake — no `Capabilities` struct shaped for `initialize`, no
   `ClientInfo`/`ServerInfo` types reused from it — survives in the new code.
3. **Sessions do not exist.** No `Mcp-Session-Id`, no `SessionManager`, no `Session` struct, no
   HTTP `GET` or `DELETE` on `/mcp`, no `Last-Event-ID`, no `event: endpoint` SSE frame. `GET` and
   `DELETE` on the MCP endpoint return `405 Method Not Allowed`. A stray `Mcp-Session-Id` header is
   ignored, not validated, not echoed.
4. **Removed methods are not routed at all**, not stubbed: `ping`, `logging/setLevel`,
   `resources/subscribe`, `resources/unsubscribe`, `notifications/roots/list_changed`. Falling
   through to `-32601` for these is correct — they no longer exist.
5. **Roots, Sampling, and MCP's own Logging utility are not implemented.** Upstream deprecated all
   three in this revision with a 12-month offramp and explicit migration guidance (tool parameters
   instead of Roots, direct provider API calls instead of Sampling, stderr/OpenTelemetry instead of
   Logging). Elicitation is supported, but only ever as an MRTR `inputRequests` entry — never as a
   bare server-initiated request, because that mechanism no longer exists on the wire.
6. **The public Go API breaks.** `transport.MessageHandler.HandleMessage` and
   `mcp.ToolFunction` both change signature (§4, §7). Anyone embedding this library rebuilds
   against the new API; there is no adapter shimming the old signatures, because the old
   signatures cannot express `_meta`, cancellation, or a stream of notifications regardless.
7. **`-32002` (resource not found) is retired.** Unknown tool and unknown resource both return
   `-32602 Invalid params`, per the changelog's alignment with JSON-RPC error semantics.

## 3. Architecture

```
                        ┌─────────────────────────────────────────┐
                        │              transport/                  │
                        │                                           │
  stdio  ──────────────▶│  stream.go  (shared newline-JSON framing) │
  unix socket ─────────▶│  stdio.go / unix.go  (thin wrappers)      │
  Streamable HTTP ─────▶│  http.go  (POST-only, header validation)  │
                        │                                           │
                        │  MessageHandler.HandleMessage(            │
                        │      ctx, data []byte, w ResponseWriter)  │
                        └───────────────────┬───────────────────────┘
                                            │  raw JSON-RPC request
                                            ▼
                        ┌─────────────────────────────────────────┐
                        │                  mcp/                    │
                        │                                           │
                        │  1. parse envelope (server.go)            │
                        │  2. parse + validate _meta (meta.go)      │
                        │       → -32602 / -32022                  │
                        │  3. route by method (server.go)           │
                        │       server/discover                     │
                        │       tools/list · tools/call             │
                        │       resources/list · resources/read     │
                        │       subscriptions/listen                │
                        │       initialize → legacy diagnostic       │
                        │       (prompts/*, resources/templates/list │
                        │        not yet implemented — see below)   │
                        │  4. handler runs against a registry        │
                        │       ToolRegistry / ResourceRegistry      │
                        │       may return InputRequiredResult       │
                        │       (mrtr.go) or stream via Broker        │
                        │       (subscriptions.go)                   │
                        │  5. stamp result envelope (result.go)      │
                        │       resultType, serverInfo, ttlMs,       │
                        │       cacheScope                           │
                        └───────────────────┬───────────────────────┘
                                            │  ResponseWriter.WriteMessage /
                                            │  WriteNotification
                                            ▼
                                  back through transport to client
```

`ResponseWriter` is the seam that makes streaming and MRTR possible: a handler that never calls
`WriteNotification` produces a plain `application/json` response; a handler that emits progress or
holds a `subscriptions/listen` stream open drives the transport into
`text/event-stream` (HTTP) or interleaved lines (stdio/unix) transparently.

## 4. Worked example: one client session, start to finish

All request examples below omit no required field — this is exactly what a conforming client must
send and exactly what this server must produce.

### (a) Discovery — no handshake, the client just asks

```jsonc
// → POST /mcp
// MCP-Protocol-Version: 2026-07-28
// Mcp-Method: server/discover
{
  "jsonrpc": "2.0", "id": 1, "method": "server/discover",
  "params": {
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientInfo": {"name": "ExampleClient", "version": "1.0.0"},
      "io.modelcontextprotocol/clientCapabilities": {"elicitation": {}}
    }
  }
}
```

```jsonc
// ← 200 application/json
{
  "jsonrpc": "2.0", "id": 1,
  "result": {
    "resultType": "complete",
    "supportedVersions": ["2026-07-28"],
    "capabilities": {"tools": {"listChanged": true}, "resources": {"listChanged": true}},
    "instructions": "This server provides date/time and fortune-cookie utilities.",
    "ttlMs": 3600000, "cacheScope": "public",
    "_meta": {"io.modelcontextprotocol/serverInfo": {"name": "go-mcp-example", "version": "0.2.0"}}
  }
}
```

```go
// mcp/discover.go
type DiscoverResult struct {
    CacheableResult                      // embeds BaseResult{ResultType,Meta} + TTLMs + CacheScope
    SupportedVersions []string           `json:"supportedVersions"`
    Capabilities      ServerCapabilities `json:"capabilities"`
    Instructions      string             `json:"instructions,omitempty"`
}

func (s *Server) handleDiscover(ctx context.Context, meta *RequestMeta) (Result, *transport.RPCError) {
    return &DiscoverResult{
        CacheableResult:   NewCacheableResult(s.listTTLMs, s.cacheScope()), // default 300000ms, "public"
        SupportedVersions: SupportedVersions,
        Capabilities:      s.capabilities(),
        Instructions:      s.config.Instructions,
    }, nil
}
```

Note there is no `initialize` anywhere in this flow — `server/discover` is optional for the client
to call at all (it may just send `tools/call` directly and handle
`UnsupportedProtocolVersionError` if guessed wrong), but servers **MUST** implement it.

### (b) Per-request validation — the chain every method runs through first

```go
// mcp/meta.go
type RequestMeta struct {
    ProtocolVersion    string
    ClientInfo         *Implementation
    ClientCapabilities *ClientCapabilities
    LogLevel           string
    ProgressToken      json.RawMessage
}

func ParseRequestMeta(params json.RawMessage) (*RequestMeta, *transport.RPCError) {
    // ... unmarshal params._meta, checking each required key is present ...
    if !ok || pv == "" {
        return nil, invalidParamsErr("missing required _meta field %q", metaKeyProtocolVersion)
    }
    if !present {
        return nil, invalidParamsErr("missing required _meta field %q", metaKeyClientCapabilities)
    }
    if !isSupportedVersion(pv) {
        return rm, unsupportedProtocolVersionErr(pv) // rm is still returned for logging/inspection
    }
    return rm, nil
}
```

`transport.HTTPStatusForRPCError` turns the resulting `*RPCError` into a status code without the
transport re-deriving the mapping: `-32602/-32020/-32021/-32022 → 400`, `-32601 → 404`, everything
else `→ 200` (JSON-RPC errors inside a 200 are still valid per base protocol; only the
*transport-validation* failures get non-200 per the Streamable HTTP binding).

### (c) A plain tool call — unstructured + structured content together

```go
// examples/tools/date.go — ToolFunction's real signature returns the mcp.Result
// interface, not a concrete type, because a tool can also return *InputRequiredResult
// (see (d) below); ToolCallResult and InputRequiredResult both implement it.
func DateTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
    var args DateArguments
    if err := req.BindArguments(&args); err != nil {
        return mcp.ErrorResultf("invalid arguments: %v", err), nil // isError:true, not a protocol error
    }
    timezone := args.Timezone
    if timezone == "" {
        timezone = "UTC"
    }
    loc, err := time.LoadLocation(timezone)
    if err != nil {
        return mcp.ErrorResultf("invalid timezone: %v", err), nil
    }
    now := time.Now().In(loc)
    return &mcp.ToolCallResult{
        Content: []mcp.Content{mcp.Text(now.Format("2006-01-02 15:04:05 MST"))},
        StructuredContent: map[string]interface{}{
            "iso8601":  now.Format(time.RFC3339),
            "timezone": loc.String(),
        },
    }, nil
}

func GetDateToolDefinition() mcp.Tool {
    return mcp.Tool{
        Name:         "date",
        Title:        "Current Date/Time",
        Description:  "Returns the current date and time in the specified timezone",
        InputSchema:  dateInputSchema,  // {"type":"object","properties":{"timezone":{"type":"string",...}},"required":["timezone"]}
        OutputSchema: dateOutputSchema, // {"type":"object","properties":{"iso8601":{...},"timezone":{...}},"required":[...]}
        Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
    }
}
```

```jsonc
// ← id:2 result
{
  "resultType": "complete",
  "content": [{"type": "text", "text": "2026-07-30 14:03:11 PDT"}],
  "structuredContent": {"iso8601": "2026-07-30T14:03:11-07:00", "timezone": "America/Los_Angeles"},
  "isError": false
}
```

### (d) MRTR — a tool that needs user confirmation mid-call

```go
// examples/tools/confirm.go — the reference MRTR tool
func ConfirmTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
    var args ConfirmArguments
    if err := req.BindArguments(&args); err != nil {
        return mcp.ErrorResultf("invalid arguments: %v", err), nil
    }

    // An explicit check here reports a tool execution error (isError:true) with a
    // message the model can act on. This is redundant with the enforcement NeedInput
    // does internally (see below) — it exists to fail with a clearer message before ever
    // constructing an elicitation request.
    if !req.ClientCapabilities.HasElicitation() {
        return mcp.ErrorResultf("this tool requires elicitation support, which the client did not declare"), nil
    }

    answer, asked := req.ElicitResponse("confirm")
    if !asked {
        // First pass: ask. NeedInput signs requestState for us, binding it to this
        // tool name + these exact arguments; it also re-checks every inputRequests
        // entry against req.ClientCapabilities on its own, returning a
        // *MissingCapabilityError (-32021) if something needed isn't declared.
        return req.NeedInput(mcp.InputRequests{
            "confirm": mcp.NewElicitRequest("form", fmt.Sprintf("Delete %d record(s)?", args.Count), confirmSchema),
        })
    }
    if !answer.Accepted() {
        return mcp.ErrorResultf("cancelled by user"), nil
    }
    return &mcp.ToolCallResult{Content: []mcp.Content{mcp.Text(fmt.Sprintf("Deleted %d record(s)", args.Count))}}, nil
}
```

```jsonc
// ← id:2 result — first attempt
{
  "resultType": "input_required",
  "inputRequests": {"confirm": {"method": "elicitation/create", "params": {
    "mode": "form", "message": "Delete 12 records?",
    "requestedSchema": {"type": "object", "properties": {"confirm": {"type": "boolean"}}, "required": ["confirm"]}
  }}},
  "requestState": "<hmac-signed opaque blob>"
}
```

```jsonc
// → id:3 — retry, same method + arguments, plus:
{
  "inputResponses": {"confirm": {"action": "accept", "content": {"confirm": true}}},
  "requestState": "<echoed back verbatim>"
}
```

`requestState` rules the implementation enforces (`mcp/mrtr.go`):
- Signed with an HMAC key configured on the `Server` (`ServerConfig.RequestStateKey`); the payload
  embeds the authenticated principal (or a constant for unauthenticated servers), a short expiry,
  and a digest of the originating method + arguments.
- A retry whose `requestState` fails verification, has expired, or names a different
  method/argument digest is rejected with `-32602`, *not* silently accepted.
- Results produced from an MRTR retry (request carries `inputResponses` or `requestState`) are
  **never cached** — `ToolCallResult` doesn't embed `CacheableResult` at all (tool calls aren't in
  the cacheable-operations list to begin with; this is called out because it's easy to get wrong
  by analogy with `resources/read`).

### (e) Subscriptions — the one long-lived request

```jsonc
// → id:4 subscriptions/listen
{"jsonrpc": "2.0", "id": 4, "method": "subscriptions/listen",
 "params": {"_meta": {...}, "notifications": {"toolsListChanged": true}}}
```

```jsonc
// ← first message on the now-open stream
{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged",
 "params": {"_meta": {"io.modelcontextprotocol/subscriptionId": 4}, "notifications": {"toolsListChanged": true}}}

// ← later, when a tool is registered/removed at runtime
{"jsonrpc": "2.0", "method": "notifications/tools/list_changed",
 "params": {"_meta": {"io.modelcontextprotocol/subscriptionId": 4}}}
```

```go
// mcp/subscriptions.go — id is the subscriptions/listen request's own JSON-RPC id,
// which becomes the subscriptionId every message on this stream is tagged with.
func (s *Server) handleSubscriptionsListen(ctx context.Context, id json.RawMessage, params json.RawMessage, w transport.ResponseWriter) {
    var p subscriptionsListenParams
    json.Unmarshal(params, &p)

    sub := s.broker.subscribe(p.Notifications)   // ToolRegistry/ResourceRegistry now carry a mutex
    defer s.broker.unsubscribe(sub)              // and call broker.notify*ListChanged() on mutation

    ack := withSubscriptionMeta(map[string]interface{}{"notifications": p.Notifications}, id)
    w.WriteNotification("notifications/subscriptions/acknowledged", ack)

    for {
        select {
        case <-ctx.Done(): // client closed the stream (HTTP) or sent notifications/cancelled (stdio)
            return
        case n := <-sub.ch:
            w.WriteNotification(n.method, withSubscriptionMeta(n.params, id))
        }
    }
}
```

(This round's `Broker` broadcasts to every subscriber whose filter matches, without tracking a
reduced "honored" subset in the acknowledgment — the ack currently echoes back the filter it was
given as-is.)

On the HTTP binding this is one open response stream per `subscriptions/listen` call — the
transport switches to `text/event-stream` on the first `WriteNotification` and emits a `:\r\n`
keep-alive comment on an interval so intermediaries don't time it out. On stdio/unix, every message
shares the single line-oriented channel, which is exactly why `subscriptionId` in `_meta` is
mandatory: it's the only way to demultiplex several concurrent `subscriptions/listen` calls (and
ordinary request/response traffic) sharing one stream.

### (f) Cancellation

- **HTTP**: the client closes the request's response stream. `handleMCP` wires `r.Context()`
  straight into the `context.Context` passed to `HandleMessage`; there is nothing else to signal.
- **stdio/unix**: the client sends `notifications/cancelled` naming the JSON-RPC `id`. The stream
  binding keeps a `map[string]context.CancelFunc` of in-flight requests and cancels the matching
  one; no further messages are written for that id afterward.

## 5. Migration notes for embedders

- `transport.MessageHandler` gains a `context.Context` parameter and a `ResponseWriter` output
  parameter instead of a `[]byte` return. If you wrote a custom transport, it now owns delivering
  notifications and the final response through that interface instead of returning a single blob.
- `mcp.ToolFunction` becomes `func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error)` —
  `mcp.Result` rather than a concrete type, because a tool can return either a `*mcp.ToolCallResult`
  or (via `req.NeedInput(...)`) an MRTR `*mcp.InputRequiredResult`; both implement it.
  `req.BindArguments(&v)` replaces manually unmarshalling `json.RawMessage`. A tool execution
  failure the model should see and can self-correct from is `mcp.ErrorResultf(...)` returned
  alongside a `nil` error; a non-nil Go `error` is now reserved for protocol-level failures and
  becomes `-32603`.
- `mcp.ToolContent{Type, Text}` is gone; use the constructors in `mcp/content.go`
  (`mcp.Text`, `mcp.Image`, `mcp.Audio`, `mcp.ResourceLinkContent`, `mcp.EmbeddedResourceContent`).
- `mcp.ResourceFunction` becomes `func(ctx context.Context) (mcp.ResourceContentResult, error)`,
  adding binary (`Blob`) support alongside `Text`.
- If you call `mcp.NewServer(...).HandleMessage(data)` directly (bypassing a `transport.Transport`),
  switch to `HandleMessage(ctx, data, w)` with a `w` that at minimum implements `WriteMessage` — use
  `transport.NewBufferedResponseWriter()` for synchronous callers that don't need streaming; its
  `.Message()` and `.Notifications()` give you back exactly what would have gone over the wire.
- If you were relying on `Mcp-Session-Id` to correlate state across calls, mint your own opaque
  handle from a tool and have the model pass it back as an argument (§"Stateful Tools" in the
  spec) — there is no protocol-level session to lean on anymore.
- Prompts (`prompts/list`/`prompts/get`), `resources/templates/list`, completion, the Tasks/Apps
  extensions, and MRTR support on `resources/read`/`prompts/get` (only `tools/call` is wired up this
  round) remain out of scope; see §6 below.

## 6. Out of scope for this round

Deliberately not implemented, to keep this migration bounded:

- **Prompts** (`prompts/list`, `prompts/get`) — was never implemented in this library even before
  2026-07-28; still absent.
- **`resources/templates/list`** and argument completion (`completion/complete`).
- **MRTR on `resources/read`/`prompts/get`** — the spec permits `InputRequiredResult` on all three of
  `tools/call`, `resources/read`, and `prompts/get`; only `tools/call` exercises it here.
  `ToolRequest.NeedInput`/`ElicitResponse` are tools-only today.
  `resources/read` always returns a complete result.
- **The `subscriptions/listen` graceful-closure response** — a server-initiated teardown SHOULD
  reply to the still-open listen request with an empty `{"resultType":"complete", ...}` before
  closing the stream. This implementation always treats termination as the abrupt-disconnect case
  (no final message), since there is currently no separate "server is shutting down" signal plumbed
  into `handleSubscriptionsListen`'s `ctx`.
- **Extensions framework** (`ServerCapabilities.Extensions`/`ClientCapabilities.Extensions` exist as
  passthrough fields, but no extension — Tasks, MCP Apps, EMA — is implemented).
- **Authorization hardening** items from the changelog's "Minor changes" (RFC 9207 `iss` validation,
  `application_type` on Dynamic Client Registration, Client ID Metadata Documents replacing DCR) —
  the existing `auth/` package (RFC 9728 protected-resource metadata, GitHub OAuth, PKCE) is
  untouched by this migration.

Worth a follow-up round.
