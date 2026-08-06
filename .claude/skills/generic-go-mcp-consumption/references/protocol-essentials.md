# Protocol essentials for debugging a client

This server implements exactly one protocol revision — `mcp.ProtocolVersion = "2026-07-28"`
(`mcp.SupportedVersions` is a single-element list; there is no dual-era negotiation, no fallback to
an earlier revision). If a client was written against, or auto-negotiates, an earlier MCP revision,
it will fail here in one of a few predictable ways. This page is the lookup table for those
failures.

## "It sent `initialize` and got an error"

Expected. `initialize` doesn't exist in this revision — the method itself is unrecognized. The
server still answers with a deliberately informative `-32601 Method not found` naming what it does
support, rather than a bare 404-shaped error, since that message may be the only diagnostic a
legacy-only client can surface to its user:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32601,
    "message": "Method not found: \"initialize\" is not implemented. This server speaks MCP protocol version 2026-07-28, which has no initialize handshake — every request carries its own protocol version and capabilities.",
    "data": {"supported": ["2026-07-28"]}
  }
}
```

Fix: the client needs to skip the handshake entirely and start sending `params._meta` on every
request instead (see below).

## "Missing required `_meta` field" (`-32602 Invalid params`)

Every non-`initialize` request must carry, inside `params._meta`:

- `"io.modelcontextprotocol/protocolVersion"` — a string, must be `"2026-07-28"`
- `"io.modelcontextprotocol/clientCapabilities"` — an object, may be `{}`

Both are required on **every single request**, not just a one-time handshake. There's no session to
remember them across calls.

## "Unsupported protocol version" (`-32022`)

`protocolVersion` was present but not `"2026-07-28"`. The error's `data.supported` field lists what
this server does accept (currently always just the one version).

## "Missing required client capability" (`-32021`)

A tool tried to send an MRTR `inputRequests` entry (e.g. `elicitation/create`) that the caller's
`clientCapabilities` on *this specific request* didn't declare. Capabilities are per-request, not
sticky from an earlier call — if a client declared `elicitation: {}` on call 1 but omits it on call
2's retry, call 2 will fail this check even though call 1 worked.

## `-32020 HeaderMismatch` (HTTP transport only)

Streamable HTTP requires `MCP-Protocol-Version` and `Mcp-Method` headers on every POST (plus
`Mcp-Name` for `tools/call`/`resources/read`/`prompts/get`), each matching the corresponding value
in the JSON-RPC body. This is **in addition to** the body-level `_meta` check above — a request can
pass `_meta` validation and still fail here if the headers don't match. See
`transports-and-auth.md` for the exact header list. Not applicable to stdio/UNIX, which have no
header layer.

## Error code reference

| Code | Meaning | Where |
|---|---|---|
| `-32700` | Parse error (malformed JSON) | any transport |
| `-32600` | Invalid Request | any transport |
| `-32601` | Method not found (includes the `initialize` diagnostic above) | any transport |
| `-32602` | Invalid params (bad/missing arguments, missing `_meta` fields, bad cursor) | any transport |
| `-32603` | Internal error (an unexpected Go error from a tool/handler) | any transport |
| `-32020` | `HeaderMismatch` — Streamable HTTP header/body mismatch or missing header | HTTP only |
| `-32021` | `MissingRequiredClientCapability` — MRTR inputRequest needs an undeclared capability | any transport |
| `-32022` | `UnsupportedProtocolVersion` | any transport |

## Exercising a running server by hand

Minimal working request against the HTTP transport (adjust host/port; on stdio/UNIX, drop the
headers and write the JSON body as one line to the socket/stdin instead):

```bash
curl -s -X POST http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'MCP-Protocol-Version: 2026-07-28' \
  -H 'Mcp-Method: server/discover' \
  -d '{
    "jsonrpc": "2.0",
    "method": "server/discover",
    "params": {
      "_meta": {
        "io.modelcontextprotocol/protocolVersion": "2026-07-28",
        "io.modelcontextprotocol/clientCapabilities": {}
      }
    },
    "id": 1
  }'
```

`server/discover` is the right first call to test against any new server — it needs no `Mcp-Name`
header (unlike `tools/call`) and returns the server's identity, capabilities, and supported
versions, i.e. everything a client used to learn from `initialize`, without any handshake:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "resultType": "complete",
    "ttlMs": 300000,
    "cacheScope": "public",
    "supportedVersions": ["2026-07-28"],
    "capabilities": {"tools": {"listChanged": true}},
    "_meta": {"io.modelcontextprotocol/serverInfo": {"name": "...", "version": "..."}}
  }
}
```

For `tools/call`, add `-H 'Mcp-Name: <tool-name>'` and put `"name"`/`"arguments"` in `params`. See
`test-http.sh` in this repo for a fuller worked script (auth, multiple tools, error cases).
