# Streamable HTTP Transport Guide

## Overview

The Streamable HTTP transport (MCP protocol version 2026-07-28) lets an MCP server run as a network
service. As of this protocol revision, MCP is **stateless**: there is no session, no handshake, and no
standalone SSE channel. The server exposes exactly one endpoint — `POST /mcp` — and every request is
self-contained.

* Each JSON-RPC request is its own HTTP POST.
* The server answers with either a single JSON object, or (only if the handler emits at least one
  notification — progress, or a change notification on a `subscriptions/listen` stream) a
  request-scoped `text/event-stream`.
* There is no `Mcp-Session-Id`, no `GET`/`DELETE` on `/mcp`, and no persistent connection to manage.
  `GET` and `DELETE` return `405 Method Not Allowed`.

See [GOLANG-MCP-CONVERT-TO-2026-07-28.md](GOLANG-MCP-CONVERT-TO-2026-07-28.md) for the full protocol
background and a worked example spanning discovery, a plain tool call, Multi Round-Trip Requests, and
subscriptions.

## Configuration

Create a configuration file (e.g., `config-http.yaml`):

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"        # Listen on all interfaces (default)
    port: 8080              # Port number (default: 8080)
    allowed_origins: []     # Optional Origin allow-list; see "Origin Validation" below
```

## Starting the Server

```bash
./go-mcp -config config-http.yaml
```

```
time=... level=INFO msg="Starting MCP server in HTTP mode" host=0.0.0.0 port=8080
time=... level=INFO msg="HTTP server listening" addr=0.0.0.0:8080 transport="Streamable HTTP"
```

## Required Headers

Every POST to `/mcp` (other than a legacy `initialize` request — see below) **must** carry:

| Header | Value | Required for |
| --- | --- | --- |
| `MCP-Protocol-Version` | Must match `params._meta["io.modelcontextprotocol/protocolVersion"]` in the body | every request |
| `Mcp-Method` | Must match the body's `method` | every request |
| `Mcp-Name` | Must match `params.name` or `params.uri` | `tools/call`, `resources/read`, `prompts/get` |

A missing or mismatched header is rejected with `400 Bad Request` and a JSON-RPC error, code `-32020`
(`HeaderMismatch`) — the request never reaches the tool/resource handler. If `Mcp-Name`'s value isn't
plain ASCII (or happens to collide with the encoding's own sentinel pattern), encode it with the
base64 sentinel format: `Mcp-Name: =?base64?<base64>?=`.

Every request body must also carry, inside `params._meta`:

```json
{
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientCapabilities": {}
}
```

`io.modelcontextprotocol/clientInfo` is optional but recommended. A request missing a required `_meta`
field is rejected with JSON-RPC error `-32602` (`Invalid params`).

## Example Session

No handshake — call whatever you need directly. `server/discover` is the closest analogue to the old
`initialize`, useful for learning the server's capabilities up front, but it's optional.

```bash
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: server/discover" \
  -d '{
    "jsonrpc": "2.0", "id": 1, "method": "server/discover",
    "params": {"_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }}
  }'
```

```bash
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: tools/call" \
  -H "Mcp-Name: date" \
  -d '{
    "jsonrpc": "2.0", "id": 2, "method": "tools/call",
    "params": {
      "name": "date", "arguments": {"timezone": "America/New_York"},
      "_meta": {
        "io.modelcontextprotocol/protocolVersion": "2026-07-28",
        "io.modelcontextprotocol/clientCapabilities": {}
      }
    }
  }'
```

Both return a single JSON object immediately — no SSE connection to open first, no session ID to
capture.

Run `./test-http.sh` for a scripted version of this session, including the header-validation and
legacy-`initialize`-diagnostic error cases.

## When the Response Is an SSE Stream

The server responds with `Content-Type: text/event-stream` only when the request needs to carry
notifications before its final result:

* A tool call that reports `notifications/progress` while it runs.
* `subscriptions/listen`, which never sends a single result at all — its response stream stays open
  and delivers `notifications/tools/list_changed`, `notifications/resources/list_changed`, and
  `notifications/resources/updated` (whichever the request opted into) until the client disconnects.

The two `list_changed` notifications are emitted automatically whenever a registry is mutated at
runtime. `notifications/resources/updated` — opted into by naming URIs in the request's
`notifications.resourceSubscriptions` array, and carrying `{"uri": "..."}` — cannot be automatic: a
resource's content is produced on demand by server code that the library cannot inspect. The server
announces it explicitly with `ResourceRegistry.NotifyUpdated(uri)`. URIs are matched exactly, so a
client hears only about the URIs it actually named. Re-registering an existing URI also fires it,
since the replacement may have swapped the content function.

Every event is a `data: <JSON-RPC message>` line. There is no event `id` and no `Last-Event-ID`
resumability — this protocol revision does not support resuming a broken stream; re-issue the request
instead. Long-lived streams periodically emit a `:` keep-alive comment line, which conforming clients
must ignore.

## Notifications (Client → Server)

A JSON-RPC message with no `id` is a notification. The server responds `202 Accepted` with an empty
body. In practice this protocol revision defines no client-to-server notification carried over
Streamable HTTP — request cancellation, in particular, is signaled by closing the request's own
response stream, not by sending `notifications/cancelled` (that notification is stdio/UNIX-only).

## Origin Validation

Per the transport's DNS-rebinding protection, the server validates the `Origin` header on every
request that includes one (requests without an `Origin` — i.e. not from a browser — are always
allowed):

* If `http.HTTPConfig.allowed_origins` is unset, only `http(s)://localhost` and `http(s)://127.0.0.1`
  are accepted.
* Set `allowed_origins: ["https://your-app.example.com"]` to allow specific origins, or `["*"]` to
  allow any (e.g. behind a trusted reverse proxy that already restricts access).

A disallowed `Origin` gets `403 Forbidden`.

## Error Handling

| Condition | HTTP status | JSON-RPC error code |
| --- | --- | --- |
| Disallowed `Origin` | 403 | — |
| `GET` or `DELETE` on `/mcp` | 405 | `-32601` |
| Missing/mismatched required header | 400 | `-32020` (`HeaderMismatch`) |
| Missing/invalid `_meta` field | 400 | `-32602` (`InvalidParams`) |
| Unsupported protocol version | 400 | `-32022` (`UnsupportedProtocolVersion`), `data.supported` lists what this server accepts |
| Missing client capability needed for MRTR | 400 | `-32021` (`MissingRequiredClientCapability`) |
| Legacy `initialize` request | 404 | `-32601`, `data.supported` names the versions this server speaks |
| Unknown method | 404 | `-32601` |
| Unknown tool/resource name | 200 | `-32602` (never the retired `-32002`) |
| Tool execution failure | 200 | not a JSON-RPC error — `isError: true` in the result, so the model can see and self-correct |

## Testing

```bash
chmod +x test-http.sh
./test-http.sh
```

This exercises `server/discover`, `tools/list`, `tools/call`, a request missing required headers
(expect `400`/`-32020`), and a legacy `initialize` request (expect `404`/`-32601`).

## Differences from stdio/UNIX Transport

| Feature | stdio / UNIX | Streamable HTTP |
| --- | --- | --- |
| Framing | Newline-delimited JSON-RPC over a shared stream | One HTTP POST per JSON-RPC message |
| Response delivery | Same shared stream, demultiplexed by JSON-RPC id / `subscriptionId` | Per-request: a JSON object, or a request-scoped SSE stream |
| Multiple clients | One connection at a time (UNIX) / one process (stdio) | Any number of concurrent HTTP requests |
| Network access | Local only | Network accessible |
| Authentication | Environment/process-level | `Authorization: Bearer` (OAuth), validated per request |
| Cancellation | `notifications/cancelled` naming the request id | Closing the request's response stream |
