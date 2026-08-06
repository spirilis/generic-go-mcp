# generic-go-mcp

A reusable Go framework for building [Model Context Protocol](https://spec.modelcontextprotocol.io/) (MCP) servers over stdio, UNIX domain sockets, or Streamable HTTP.

## Overview

**generic-go-mcp** is a library that abstracts away the complexity of implementing MCP servers. It handles JSON-RPC 2.0 message parsing, transport layer management, authentication, and configuration—allowing you to focus on building powerful tools for Claude and other MCP clients.

### What is MCP?

The Model Context Protocol enables AI assistants like Claude to interact with external tools and data sources. This library makes it easy to create custom MCP servers that expose your own functionality to AI models.

## ⚠️ Hard requirement: the client MUST implement MCP protocol version 2026-07-28

This library implements MCP protocol version **2026-07-28 and nothing else**. This is a hard
cutover, not a preference: `mcp.SupportedVersions` is a single-element list, there is no dual-stack
mode, no compatibility shim, and no negotiation down to an earlier revision. A client that speaks
2025-11-25 or earlier **cannot talk to a server built on this library at all** — not in degraded
form, not for `tools/list`, not for anything.

If you control only the server, this is the constraint you are accepting. If your client is an
older MCP host, you need a different library or a translating proxy in front of this one.

### What a conforming client must do

**1. Never send `initialize`.** The 2026-07-28 revision removed the handshake. An `initialize`
request is answered with JSON-RPC `-32601` (Method not found) carrying a diagnostic that names the
supported versions — it never yields a session:

```json
{"jsonrpc":"2.0","id":1,"error":{
  "code":-32601,
  "message":"Method not found: \"initialize\" is not implemented. This server speaks MCP protocol version 2026-07-28, which has no initialize handshake — every request carries its own protocol version and capabilities.",
  "data":{"supported":["2026-07-28"]}}}
```

That message exists purely so a legacy-only client has something intelligible to show its user.

**2. Put `_meta` on every single request.** There is no session to remember who you are, so each
request re-states it. Two fields are **mandatory on every request** (`server/discover`,
`tools/list`, `tools/call`, `resources/list`, `resources/read`, `subscriptions/listen`):

| `params._meta` key | Required | Notes |
|---|---|---|
| `io.modelcontextprotocol/protocolVersion` | **yes** | Must be exactly `"2026-07-28"` |
| `io.modelcontextprotocol/clientCapabilities` | **yes** | May be `{}`, but the key must be present |
| `io.modelcontextprotocol/clientInfo` | no | `{name, version}`; display/logging only |
| `io.modelcontextprotocol/logLevel` | no | Per-request log level hint |
| `progressToken` | no | Enables progress notifications for this call |

Omitting either mandatory field is a malformed request: `-32602`, `missing required _meta field
"io.modelcontextprotocol/protocolVersion"`. This is the single most common first-contact failure.

```json
{
  "jsonrpc": "2.0", "id": 1, "method": "tools/call",
  "params": {
    "name": "current_time",
    "arguments": {},
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  }
}
```

**3. Declare capabilities per request, honestly.** The server may only ask for what *this* request
declared. A tool that needs mid-call user input returns `-32021` (missing required client
capability) if `clientCapabilities.elicitation` was absent from the request that triggered it.

**4. Discover via `server/discover`, not a handshake.** It returns `supportedVersions`,
`capabilities`, optional `instructions`, and — like every result from this server — the server's
identity at `result._meta["io.modelcontextprotocol/serverInfo"]`. That is everything `initialize`
used to carry, and `server/discover` is itself an ordinary request that requires `_meta`.

**5. Handle Multi Round-Trip Requests.** Servers can no longer originate JSON-RPC requests, so
elicitation/sampling-style interactions come back as a result with `resultType: "input_required"`,
an `inputRequests` map, and an opaque signed `requestState`. The client answers by re-issuing the
same call (new JSON-RPC `id`, same `name`/`arguments`) with `inputResponses` and the
`requestState` echoed back **byte-for-byte**. See `examples/tools/confirm.go`.

**6. Over Streamable HTTP, additionally:**
- `POST /mcp` only. `GET` and `DELETE` (the old SSE stream and session teardown) return **405**.
- No `Mcp-Session-Id` — sessions do not exist.
- Send `MCP-Protocol-Version` and `Mcp-Method` headers on every request, plus `Mcp-Name` for
  `tools/call`, `resources/read`, and `prompts/get`. Each is validated against the JSON body;
  a missing or mismatched header is **HTTP 400** with JSON-RPC `-32020` (header mismatch).
- Browser-issued requests must carry an allowed `Origin` (defaults to localhost only; see
  `HTTPConfig.AllowedOrigins`), or the request is rejected with **403**.
- A response is a plain JSON object unless the handler emits a notification, at which point the
  same response upgrades to `text/event-stream` for that request only.

### Version-specific error codes

| Code | Meaning |
|---|---|
| `-32020` | Streamable HTTP header missing or disagreeing with the body |
| `-32021` | Request needs a client capability that was not declared |
| `-32022` | `protocolVersion` is not one this server implements (`data.supported` lists what is) |
| `-32601` on `initialize` | Legacy handshake attempted against a stateless server |

### Not implemented in this revision

Roots, Sampling, and MCP's own Logging utility are deprecated upstream in 2026-07-28 and are not
implemented here. Prompts are not implemented yet (the HTTP layer already reserves `prompts/get`
for the `Mcp-Name` header rule, and a `subscriptions/listen` filter may set `promptsListChanged`,
but nothing will ever fire it).

The `subscriptions/listen` **graceful-closure response** is also unimplemented. The spec SHOULDs
that a server-initiated teardown reply to the still-open listen request with an empty
`{"resultType":"complete"}` before closing; this library always treats termination as the
abrupt-disconnect case and writes no final response. A client must not wait for one.

See [GOLANG-MCP-CONVERT-TO-2026-07-28.md](GOLANG-MCP-CONVERT-TO-2026-07-28.md) for the full design
rationale, the hard-cutover decisions, and a worked wire-to-Go-types example.

## Features

- **Three Transports** - stdio (desktop integration), UNIX domain socket (local IPC), and
  Streamable HTTP (web services), behind one `Transport` interface
- **OAuth Authentication** - Built-in GitHub OAuth 2.0 support with PKCE for HTTP mode
- **YAML Configuration** - File-based config with defaults; OAuth credentials may instead be read
  from mounted secret files (Docker/Kubernetes)
- **Structured Logging** - Multi-level logging (trace/debug/info/warn/error) with JSON and text formats
- **Simple Tool API** - Register tools with JSON schema definitions and type-safe handlers
- **Live Change Notifications** - Runtime-mutable tool and resource catalogs; clients subscribe once
  via `subscriptions/listen` and receive `list_changed` and per-resource `updated` notifications
- **Production Ready** - BoltDB token storage, HMAC-signed MRTR request state, graceful shutdown
- **Dependency-Free Core** - `mcp` and `transport` import nothing beyond the Go standard library
  (and each other); `auth` (BoltDB) and `config` (YAML) are opt-in, pulled in only if you import them

## Using this library from another project

```bash
go get github.com/spirilis/generic-go-mcp@v0.2.0
```

If you're using an AI coding agent to build on top of this library, point it at
[`.claude/skills/generic-go-mcp-consumption/SKILL.md`](.claude/skills/generic-go-mcp-consumption/SKILL.md)
in this repo — it covers tool/resource authoring, transport and auth setup, and the Multi
Round-Trip Requests pattern, with every example checked against the real API.

## Quick Start

```go
package main

import (
    "context"
    "encoding/json"
    "time"

    "github.com/spirilis/generic-go-mcp/config"
    "github.com/spirilis/generic-go-mcp/logging"
    "github.com/spirilis/generic-go-mcp/mcp"
    "github.com/spirilis/generic-go-mcp/transport"
)

func main() {
    // Load configuration
    cfg, _ := config.Load("config.yaml")
    logging.Initialize(cfg.Logging.Level, cfg.Logging.Format)

    // Create a tool registry
    registry := mcp.NewToolRegistry()

    // Define a simple tool
    timeTool := mcp.Tool{
        Name:        "current_time",
        Description: "Returns the current UTC time",
        InputSchema: json.RawMessage(`{"type": "object", "additionalProperties": false}`),
    }

    // Register the tool with its implementation. ToolFunction takes a context (for
    // cancellation) and a *ToolRequest (arguments, parsed _meta, and — for tools that
    // need mid-call user input — Multi Round-Trip Requests helpers).
    registry.Register(timeTool, func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
        return &mcp.ToolCallResult{
            Content: []mcp.Content{mcp.Text(time.Now().UTC().String())},
        }, nil
    })

    // A resource registry is required even if you register no resources.
    resources := mcp.NewResourceRegistry()

    // Create the MCP server
    server := mcp.NewServer(registry, resources, &mcp.ServerConfig{
        Name:    "my-mcp-server",
        Version: "1.0.0",
    })

    // Start the stdio transport
    trans := transport.NewStdioTransport()
    trans.Start(server)
}
```

Then talk to it — note the mandatory `_meta`, without which every request is rejected:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{
  "io.modelcontextprotocol/protocolVersion":"2026-07-28",
  "io.modelcontextprotocol/clientCapabilities":{}}}}' | ./my-mcp-server
```

## Project Structure

```
generic-go-mcp/
├── config/               # Configuration loading (YAML, env vars, secrets)
├── logging/              # Structured logging with multiple levels
├── auth/                 # OAuth 2.0 authentication (GitHub)
├── transport/            # Transport abstractions (stdio, UNIX socket, Streamable HTTP)
├── mcp/                  # MCP protocol implementation (JSON-RPC 2.0)
├── examples/
│   ├── go-mcp/           # Complete example server application
│   └── tools/            # Reference tools (date, fortune, confirm_delete/MRTR)
├── CLAUDE-new-project-harness.md  # Comprehensive getting started guide
├── CLAUDE.md             # Architecture and design patterns
├── GOLANG-MCP-CONVERT-TO-2026-07-28.md  # Protocol revision design & rationale
├── HTTP-TRANSPORT.md     # HTTP transport documentation
└── LOGGING.md            # Logging system documentation
```

All packages are public and designed to be imported by your projects.

## Getting Started

For a comprehensive guide on building your own MCP server, see **[CLAUDE-new-project-harness.md](CLAUDE-new-project-harness.md)**.

This guide covers:
- Setting up a new project with the library
- Creating tools with and without arguments
- Main application template with stdio/HTTP mode switching
- Configuration examples (stdio, HTTP, auth-enabled)
- Build commands including cross-compilation and Docker
- Testing with Claude Code and HTTP clients
- Debugging tips and common issues

## Configuration

`config.Load(path)` reads a YAML file and applies defaults (`mode: stdio`, HTTP `0.0.0.0:8080`,
UNIX file mode `0660`, logging `info`/`text`). The example application layers CLI flags on top of
the loaded file; the `config` package itself does not read environment variables.

### Example Configuration (stdio mode)

```yaml
server:
  mode: "stdio"

logging:
  level: "info"
  format: "text"
```

### Example Configuration (UNIX socket mode)

```yaml
server:
  mode: "unix"
  unix:
    socket_path: /tmp/go-mcp.sock
    name: go-mcp-example-endpoint
    file_mode: 0660
```

### Example Configuration (HTTP mode with auth)

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"
    port: 8080
    # Origin allow-list for DNS-rebinding protection.
    # Omit for localhost-only; use ["*"] behind a trusted reverse proxy.
    allowed_origins: ["https://app.example.com"]

auth:
  enabled: true
  issuer: "https://mcp.example.com"   # must match the server's public URL
  github:
    clientId: "your-github-oauth-app-id"
    clientSecret: "your-github-oauth-secret"
    # Or read from mounted secret files instead:
    # clientIdFile: "/run/secrets/github_client_id"
    # clientSecretFile: "/run/secrets/github_client_secret"
  storage:
    dbPath: "/var/lib/go-mcp/oauth.db"
  allowlist:
    users: ["my-github-user"]
    orgs: ["my-company"]
    teams:
      - org: "my-company"
        team: "platform-team"   # team slug, not display name

logging:
  level: "info"
  format: "json"
```

Note the key names: they are camelCase under `auth:` and snake_case under `server:`, matching the
struct tags in `config/config.go`. There is no `redirect_url` key: the callback URL you register
with GitHub is always `<issuer>/callback`. See
[config-oauth-example.yaml](config-oauth-example.yaml) for the fully annotated version, including
pre-registered static clients.

## Documentation

- **[CLAUDE-new-project-harness.md](CLAUDE-new-project-harness.md)** - Complete guide to building MCP servers with this library
- **[CLAUDE.md](CLAUDE.md)** - Architecture overview and design patterns
- **[HTTP-TRANSPORT.md](HTTP-TRANSPORT.md)** - HTTP transport details
- **[LOGGING.md](LOGGING.md)** - Logging system documentation
- **[MCP Specification](https://spec.modelcontextprotocol.io/)** - Official Model Context Protocol specification

## Examples

The [examples/](examples/) directory contains:

- **go-mcp/** - A complete MCP server demonstrating stdio/HTTP/UNIX-socket mode, auth integration, and graceful shutdown
- **tools/date.go** - Example tool with arguments, an `outputSchema`, and `structuredContent`
- **tools/fortune.go** - Example tool without arguments (executes fortune command)
- **tools/confirm.go** - Reference implementation of Multi Round-Trip Requests (elicitation)

Sample configs live at the repo root: [config.yaml](config.yaml) (stdio),
[config-unix.yaml](config-unix.yaml), [config-http.yaml](config-http.yaml), and
[config-oauth-example.yaml](config-oauth-example.yaml).

To build and run the example:

```bash
go build -o go-mcp ./examples/go-mcp
./go-mcp -config config.yaml
```

## Key Concepts

### Transport Layer
Abstracts communication mechanisms behind a common interface:
- **StdioTransport** - Reads from stdin, writes to stdout (for Claude Code, desktop apps)
- **UnixTransport** - Newline-delimited JSON-RPC over a UNIX domain socket (local IPC)
- **HTTPTransport** - POST-only `/mcp` Streamable HTTP endpoint (web services, remote access)

Because the protocol is stateless, a transport may have several requests in flight concurrently on
one connection, so `HandleMessage` takes a `context.Context` and a `ResponseWriter` rather than
returning a single buffered response.

`transport` does not import `auth`: `HTTPTransportConfig.AuthService` is the small
`transport.AuthProvider` interface, so a server using only `mcp` + `transport` stays pure-stdlib.
Assign it only when auth is actually enabled — an unconditionally-assigned nil `*auth.AuthService`
is a typed nil that reads as non-nil through the interface.

### Tool and Resource Registries
Simple API for registering and listing tools (invocation is handled internally by the server, which
also enforces `_meta` validation, `x-mcp-header` checks, and Multi Round-Trip Request state
verification before your function runs):

```go
registry := mcp.NewToolRegistry()
registry.Register(toolDefinition, toolFunction)
registry.Unregister("my_tool") // bool: whether a tool by that name was there to remove
registry.List()                // All registered tools, in registration order (stable across calls)
registry.Get("my_tool")        // (Tool, bool) lookup by name
registry.HasTools()            // Whether any tool is registered
```

`mcp.NewResourceRegistry()` mirrors this for resources (`Register`, `Unregister` keyed by URI,
`List`, `Get`, `Read`, `HasResources`) and is a required argument to `mcp.NewServer` even when you
register no resources. It adds one method with no tool equivalent:

```go
resources.NotifyUpdated("config://server-name") // announce a content change; bool = URI is registered
```

See [Change Notifications](#change-notifications) below for what that delivers and why it can't be
automatic.

Both registries are safe for concurrent use and can be mutated at runtime after the server has
started. Every mutation that actually changes the catalog emits a
`notifications/tools/list_changed` (or `resources/list_changed`) to any client with an open
`subscriptions/listen` stream; `Unregister` on a name or URI that isn't present returns `false`
and notifies nobody.

Three behaviors worth knowing:

- **Re-registering an existing name/URI replaces the entry and moves it to the end of the list**
  rather than adding a second one, so the list can never disagree with the function map.
- **Re-registering a resource URI also fires `resources/updated`** for it, on top of
  `list_changed`. A replacement swaps both the metadata and the `ResourceFunction`, and Go cannot
  compare func values, so the registry can't tell a description edit from a content swap —
  announcing it is the safe side of that. Registering a *new* URI fires `list_changed` only.
- **Removing the last tool or resource withdraws that capability** from `server/discover`, since
  capabilities are derived from what is actually registered.

Because list cursors are opaque offsets, unregistering between a client's page fetches can shift
later entries and cause one to be skipped — that is what `list_changed` is for; a client that sees
it should restart pagination.

### Change Notifications

One long-lived request, `subscriptions/listen`, is the **entire** server→client notification
channel. The 2026-07-28 revision deleted `resources/subscribe`/`resources/unsubscribe` along with
the server's ability to originate JSON-RPC requests at all, and folded everything they did into this
one call. A client opens it once and leaves it open.

(Not to be confused with `notifications/progress`, which rides a tool call's *own* request stream
while that call runs. The subscription stream carries only catalog and resource-content changes.)

A client subscribes by naming what it wants in the `notifications` filter:

```json
{
  "jsonrpc": "2.0", "id": 4, "method": "subscriptions/listen",
  "params": {
    "notifications": {
      "toolsListChanged": true,
      "resourcesListChanged": true,
      "resourceSubscriptions": ["config://server-name", "file:///var/db/status.json"]
    },
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  }
}
```

Every field is optional; omitting one simply means "don't send me those." The filters are
independent — a client that sets only `resourcesListChanged` hears nothing about resource *content*,
and vice versa.

| Filter field | Delivers | Triggered by |
|---|---|---|
| `toolsListChanged` | `notifications/tools/list_changed` | automatic, on any `ToolRegistry` mutation |
| `resourcesListChanged` | `notifications/resources/list_changed` | automatic, on any `ResourceRegistry` mutation |
| `resourceSubscriptions: [uri…]` | `notifications/resources/updated`, params `{"uri": …}` | **your code**, via `resources.NotifyUpdated(uri)` |
| `promptsListChanged` | — | accepted, never fires: prompts are not implemented |

That split is the thing to understand. The two `list_changed` flags need no code from you —
`mcp.NewServer` wires each registry's internal change hook to the server's broker, so ordinary
`Register`/`Unregister` calls emit them. `resourceSubscriptions` **cannot** work that way: a
`ResourceFunction` is called on demand and returns whatever it likes, so the library never sees the
underlying data and has no way to notice it changed. Only your code knows.

```go
// Watch something and announce it. NotifyUpdated returns false for a URI that
// isn't registered — a no-op that notifies nobody, so typos surface instead of
// silently broadcasting.
go func() {
    for range fileChanged {
        resources.NotifyUpdated("file:///var/db/status.json")
    }
}()
```

URIs are matched **exactly**. The spec permits a server to announce a sub-resource of a URI the
client subscribed to; a `ResourceRegistry` is a flat map with no URI hierarchy to derive one from,
so a client hears about exactly the URIs it named.

#### What the stream looks like

The first message is always an acknowledgment echoing the filter the server actually registered:

```json
{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{
  "notifications":{"toolsListChanged":true,"resourcesListChanged":true,
                   "resourceSubscriptions":["config://server-name","file:///var/db/status.json"]},
  "_meta":{"io.modelcontextprotocol/subscriptionId":4}}}
```

Then notifications, until the request ends:

```json
{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{
  "uri":"config://server-name",
  "_meta":{"io.modelcontextprotocol/subscriptionId":4}}}
```

Every message on the stream carries `_meta["io.modelcontextprotocol/subscriptionId"]`, set to the
listen request's own JSON-RPC `id`. That is mandatory, not decorative: on stdio and UNIX sockets all
requests share one channel, so a client running several concurrent subscriptions needs it to tell
them apart.

#### Delivery guarantees

**Notifications are hints, not an event log.** Each subscriber gets a 16-deep buffer, and once it is
full the broker drops rather than blocks — a registry mutation, or a `NotifyUpdated` call on a hot
path, must never stall behind a slow client. A burst can therefore reach a client as fewer
notifications than you sent, or as none at all.

Design both sides around that:

- Treat every notification as *"something may have changed, go re-read"* — never as a count of
  changes, and never as a stream you can replay state from.
- There is no `Last-Event-ID` resumability in this revision. A client that reconnects gets no
  backfill; it should re-`list` and re-`read` what it cares about.
- Because `ReadTTLMs` defaults to `0` (always refetch — see below), `resources/updated` is not
  load-bearing for cache invalidation. It is a *push* signal that lets a client re-read promptly
  instead of polling.

#### Lifecycle and cancellation

The request stays open until the client ends it. How depends on the transport:

| Transport | Client ends the subscription by | Notes |
|---|---|---|
| Streamable HTTP | closing the response stream | A listen request always becomes `text/event-stream`, since its acknowledgment is itself a notification. A `:` keep-alive comment every 15s holds it open through idle proxies. |
| stdio / UNIX socket | sending `notifications/cancelled` with `params.requestId` set to the listen request's `id` | Matched on the raw JSON id, so `4` and `"4"` are different requests. The connection multiplexes, so the subscription never blocks other in-flight requests. |

Either way the server writes **no final JSON-RPC response** for the listen request — see
"[Not implemented in this revision](#not-implemented-in-this-revision)" above. See
[HTTP-TRANSPORT.md](HTTP-TRANSPORT.md) for the SSE framing details.

### Result Caching Hints
Every result carries a `resultType`, and list/read results carry `ttlMs` and `cacheScope` hints.
Defaults are 5 minutes for catalogs and 0 (always refetch) for resource reads; override via
`ServerConfig.ListTTLMs`, `ReadTTLMs`, and `DefaultCacheScope` (set `"private"` for per-user
catalogs behind auth).

### JSON-RPC 2.0 Protocol
All MCP communication follows JSON-RPC 2.0 specification with automatic message parsing, validation, and error handling.

## Building

### Standard Build
```bash
go build -o my-mcp-server
```

### Cross-Compilation
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o my-mcp-server
```

### Docker Multi-Platform Build
```bash
docker buildx build --platform linux/amd64,linux/arm64 -t my-mcp-server:latest .
```

See [CLAUDE-new-project-harness.md](CLAUDE-new-project-harness.md) for complete build instructions and Dockerfile examples.

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! This library is designed to be general-purpose and not specialized to any particular use case. When contributing:

1. Keep the core library transport-agnostic
2. Follow JSON-RPC 2.0 and MCP specification conventions
3. Add tests for both stdio and HTTP transports
4. Document new features in the appropriate .md files

## Acknowledgments

Built following the [Model Context Protocol specification](https://spec.modelcontextprotocol.io/) by Anthropic.
