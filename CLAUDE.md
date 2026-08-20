# generic-go-mcp

A reusable Go framework for building Model Context Protocol (MCP) servers with support for both stdio and Streamable HTTP transports.

This library implements MCP protocol version **2026-07-28**. That revision made MCP stateless: there is
no `initialize` handshake and no `Mcp-Session-Id` — every request carries its own protocol version,
capabilities, and identity in `params._meta`, and a legacy client's `initialize` request gets a
diagnostic error naming the versions this server supports rather than a session. See
[GOLANG-MCP-CONVERT-TO-2026-07-28.md](GOLANG-MCP-CONVERT-TO-2026-07-28.md) for the full design
rationale, the hard-cutover decisions this library makes, and a worked wire-to-Go-types example.
Roots, Sampling, and MCP's own Logging utility are deprecated upstream in this revision and are not
implemented here; Prompts are not yet implemented (see that document's "Out of scope" section).
Parameterized resources **are** implemented: RFC 6570 resource templates via
`resources/templates/list`, matched back to a concrete URI on `resources/read`, plus an opt-in
`completion/complete` provider for narrowing a template variable — see "Resource Templates" below.
The `io.modelcontextprotocol/skills` extension (SEP-2640) is implemented as an explicitly
**experimental, opt-in** surface: `skills/list`, `skills/get`, and `resources/directory/read`, off
entirely unless `ServerConfig.Skills` is set — see "Skills" below. An optional `compat` package (off
by default) can additionally serve clients still on 2025-11-25 or earlier alongside this native
2026-07-28 support — see [LEGACY-COMPAT.md](LEGACY-COMPAT.md).

## Build Commands

### Building the Example Application

```bash
go build -o go-mcp ./examples/go-mcp
```

### Cross-Compilation
For static binaries without CGO dependencies:
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o go-mcp ./examples/go-mcp
```

### Multi-Platform Docker Builds
Following multi-platform build practices, leverage Go's cross-compilation:
```dockerfile
# Build stage - use native platform
FROM golang:1.21 AS builder
WORKDIR /build
COPY . .
# Cross-compile for target platform
ARG TARGETPLATFORM
RUN CGO_ENABLED=0 go build -o go-mcp ./examples/go-mcp

# Runtime stage
FROM alpine:latest
COPY --from=builder /build/go-mcp /usr/local/bin/
ENTRYPOINT ["go-mcp"]
```

Build for multiple platforms:
```bash
docker buildx build --platform linux/amd64,linux/arm64 -t go-mcp:latest .
```

## Using as a Library

This framework is designed to be used as a library for building custom MCP servers:

```go
package main

import (
    "github.com/spirilis/generic-go-mcp/mcp"
    "github.com/spirilis/generic-go-mcp/transport"
)

func main() {
    // Create tool registry and register your tools
    registry := mcp.NewToolRegistry()
    registry.Register(myToolDef, myToolFunc)

    // Create MCP server with custom name/version
    server := mcp.NewServer(registry, &mcp.ServerConfig{
        Name:    "my-mcp-server",
        Version: "1.0.0",
    })

    // Create and start transport, then wait for the client to close stdin
    trans := transport.NewStdioTransport()
    trans.Start(server)
    <-trans.Done()
    trans.Stop()
}
```

## Architecture Overview

The framework follows a layered architecture pattern:

### Transport Layer (`transport/`)
Abstracts communication mechanisms (stdio, UNIX domain sockets, Streamable HTTP) behind a common
interface. Allows MCP servers to run in different environments without protocol-specific code changes.
Because the protocol is stateless, a transport may have several requests in flight concurrently on
the same connection (e.g. a long-lived `subscriptions/listen` alongside an ordinary `tools/call`), so
`HandleMessage` takes a `context.Context` (cancelled when the request should stop) and a
`ResponseWriter` (through which notifications and the final response are written), rather than
returning a single buffered response.

`Start` is non-blocking; `Stop` initiates shutdown, and what it waits for is each transport's own
business. A transport whose run can end on its own — currently only `StdioTransport`, whose input
reaching EOF is a real ending — additionally implements the optional `DoneNotifier` interface, so
transport-agnostic startup code can wait on it with a type assertion (the same optional-interface
pattern as `HeaderSetter`). `HTTPTransport` and `UnixTransport` deliberately do not: they end only
when `Stop` is called.

**Key Interfaces:**
```go
type Transport interface {
    Start(handler MessageHandler) error
    Stop() error
}

type DoneNotifier interface {
    Done() <-chan struct{} // closed when the transport's run has ended
}

type MessageHandler interface {
    HandleMessage(ctx context.Context, data []byte, w ResponseWriter)
}

type ResponseWriter interface {
    WriteNotification(method string, params interface{}) error
    WriteMessage(data []byte) error
}
```

### MCP Protocol Layer (`mcp/`)
Handles JSON-RPC 2.0 message parsing, validation, and routing. Manages tool definitions and their registration.

**Responsibilities:**
- JSON-RPC 2.0 request/response handling
- Tool definition schema
- Method routing
- Error handling per MCP specification
- Server name/version configuration
- Resource templates: RFC 6570 compilation and URI-to-variable matching (`mcp/templates.go`), which
  is hand-rolled rather than taken from a library so `mcp` stays pure standard library
- Optional completion provider plumbing (`mcp/completion.go`)

### Legacy Compatibility Overlay (`compat/`)
Optional: serves MCP protocol revisions 2025-11-25 and earlier (the connection-scoped, `initialize`
handshake era) alongside this library's native 2026-07-28 support, for the duration of upstream's
migration window. Off by default; wraps an `*mcp.Server` (or any `transport.MessageHandler`) rather
than modifying it, so a server that never opts in is byte-for-byte the modern-only server described
everywhere else in this file. See [LEGACY-COMPAT.md](LEGACY-COMPAT.md).

**Responsibilities:**
- Per-request era detection (`compat.Claims` / `transport.DeclaresModernProtocol`)
- Legacy session lifecycle (handshake, TTL, teardown) — the modern stack has none
- Translating a legacy request into the modern wire shape and forwarding it to the inner handler
  unchanged, then downgrading the result back (stripping `resultType`/`ttlMs`/`cacheScope`/`_meta`).
  Resource templates, `completion/complete`, and the skills extension's three methods
  (`skills/list`, `skills/get`, `resources/directory/read`) are all era-neutral and share an
  identical params object across eras, so they are forwarded like any other method. Downgrading does
  not descend into nested values, so a skill's frontmatter and per-file digests survive untouched,
  and `capabilities.extensions` reaches the legacy handshake because `mcp.ServerCapabilities` has a
  typed field for it
- Refusing MRTR (`input_required`) results visibly to legacy clients, which have no way to answer one

### Auth Layer (`auth/`)
Implements authentication and authorization for HTTP/SSE mode.

**Components:**
- GitHub OAuth 2.0 Authorization Code flow
- Token persistence (BoltDB)
- Session management
- Authentication middleware

### Config Layer (`config/`)
Flexible configuration loading supporting multiple sources:

**Sources (in priority order):**
1. Mounted secrets (Kubernetes/Docker)
2. Environment variables
3. YAML configuration files
4. Defaults

### Logging Layer (`logging/`)
Structured logging with multiple levels and formats.

**Features:**
- Trace, Debug, Info, Warn, Error levels
- JSON and text output formats
- Header sanitization for security

## Key Patterns

### Transport Interface Pattern
Enables MCP servers to support stdio, UNIX domain sockets, and Streamable HTTP without duplicating
protocol logic. Implementations:
- `StdioTransport` / `UnixTransport` - newline-delimited JSON-RPC over stdin/stdout or a UNIX socket,
  sharing a common framing (`transport/stream.go`) that dispatches each message concurrently.
  `StdioTransport` serves arbitrary streams via `NewStdioTransportWithStreams(in, out)` (nil means
  the process default), which is what makes the stdio path testable with `io.Pipe`; its `Done()`
  closes when the input reaches EOF and `Err()` then says whether that was clean. Its `Stop()`
  cancels in-flight requests and returns immediately — it does not wait for the read loop, which is
  parked in a blocking read nothing portable can interrupt, so `Stop(); <-Done()` is the spelling
  for "wait for it to unwind"
- `HTTPTransport` - a single POST-only `/mcp` endpoint; responds with a plain JSON object, or
  upgrades to a request-scoped `text/event-stream` the moment a handler emits a notification
  (progress, or a `subscriptions/listen` change notification)

`transport` has no dependency on `auth`: `HTTPTransportConfig.AuthService` is typed as the small
`transport.AuthProvider` interface (`RegisterRoutes`, `RegisterAdminRoutes`, `Middleware`,
`UserFromContext`), not `*auth.AuthService`. `auth.AuthService` satisfies it (see the compile-time
assertion in `auth/middleware.go`), so passing one just works, but `transport` — and therefore
`mcp`, which depends on `transport` — never pulls in `auth`'s BoltDB dependency. A consumer
importing only `mcp` + `transport` (stdio, or unauthenticated HTTP) therefore gets a dependency
graph of `transport` (pure standard library) plus one library: `gopkg.in/yaml.v3`, which `mcp`
imports for `SkillRegistry.LoadFS` to parse `SKILL.md` frontmatter. It was already a direct module
dependency by way of `config`, so nothing new enters `go.mod`, but `mcp` is no longer stdlib-only.
When wiring this up, only assign `AuthService` when auth is actually enabled: an
unconditionally-assigned nil `*auth.AuthService` is a typed nil that reads as non-nil through the
interface (`NewHTTPTransport` also guards against this defensively, but callers should get it
right rather than lean on that).

### JSON-RPC 2.0 Protocol Handling
All MCP messages follow JSON-RPC 2.0 specification. Every request also carries its protocol version,
capabilities, and (optionally) identity in `params._meta` — there is no separate handshake:
```json
{
  "jsonrpc": "2.0",
  "method": "tools/call",
  "params": {
    "name": "my_tool",
    "arguments": {},
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  },
  "id": 1
}
```

### OAuth 2.0 Authorization Code Flow
For HTTP/SSE mode, implements GitHub OAuth:
1. Redirect to GitHub authorization URL
2. Handle callback with authorization code
3. Exchange code for access token
4. Store token securely in BoltDB
5. Use token for API authentication

### BoltDB for Token/Session Storage
Embedded key-value store for persisting:
- OAuth access tokens
- Refresh tokens
- Session data
- User preferences

### Context-Based Authentication Middleware
HTTP requests carry authentication context:
```go
func AuthMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Extract and validate token
        // Store user info in context
        ctx := context.WithValue(r.Context(), "user", user)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

## Reference Tools

The framework includes reference tool implementations demonstrating best practices. For brevity, the
`_meta` field required on every real request (see above) is omitted from the example bodies below.

### `date(timezone)`
Returns the current date/time for a specified timezone.

**Parameters:**
- `timezone` (string) - IANA timezone (e.g., "America/New_York")

**Example:**
```json
{
  "method": "tools/call",
  "params": {
    "name": "date",
    "arguments": {
      "timezone": "Asia/Tokyo"
    }
  }
}
```

### `fortune()`
Executes the local `fortune` CLI command and returns output.

**Parameters:** None

**Example:**
```json
{
  "method": "tools/call",
  "params": {
    "name": "fortune",
    "arguments": {}
  }
}
```

**Note:** Demonstrates safe CLI execution patterns with proper error handling and output capture.

### `confirm_delete(count)`
Reference implementation of **Multi Round-Trip Requests (MRTR)** — the pattern that replaced
server-initiated requests (sampling, elicitation, roots) in protocol version 2026-07-28. Servers can
no longer send their own JSON-RPC requests to the client; instead they return `resultType:
"input_required"` and the client retries the same call with the answer attached.

**First call** (no confirmation yet) returns an `input_required` result:
```json
{
  "resultType": "input_required",
  "inputRequests": {
    "confirm": {
      "method": "elicitation/create",
      "params": {"mode": "form", "message": "Delete 12 record(s)?", "requestedSchema": {"...": "..."}}
    }
  },
  "requestState": "<HMAC-signed opaque blob>"
}
```

**Retry** (new JSON-RPC `id`, same tool name/arguments, plus the client's answer and the echoed
`requestState`) completes the call:
```json
{
  "method": "tools/call",
  "params": {
    "name": "confirm_delete",
    "arguments": {"count": 12},
    "inputResponses": {"confirm": {"action": "accept", "content": {"confirm": true}}},
    "requestState": "<the exact blob from the previous response>"
  }
}
```

See `mcp.ToolRequest.NeedInput` / `ElicitResponse` (`mcp/tools.go`, `mcp/mrtr.go`) and
`examples/tools/confirm.go` for the Go side of this exchange.

### Resource Templates

`examples/go-mcp/main.go`'s UNIX-socket mode registers `mcp+unix:///env/{name}` as a resource
*template*: the environment is an unbounded keyspace, so the server advertises the shape through
`resources/templates/list` and the client expands it locally, then reads a concrete
`mcp+unix:///env/PATH`. It also attaches a `mcp.CompletionFunc` over `os.Environ()`, which is what
makes that server declare the `completions` capability at all.

Only two RFC 6570 forms are supported, and anything else is a `RegisterTemplate` error naming the
offending expression:

| Form | Matches |
|---|---|
| `{var}` | one path segment, never `/` |
| `{+var}` | one or more characters including `/`; **final expression only** |

Key behaviors: a concrete `Resource` always beats a template; templates match in registration order
(first match wins, so register the specific one first); `req.Vars` arrives percent-decoded;
`contents[0].uri` is always the concrete URI, never the template; template registration fires the
ordinary `notifications/resources/list_changed`; and a registry holding only templates still
advertises the `resources` capability. A handler returning `mcp.ErrResourceNotFound` (or wrapping it)
produces `-32602 "Unknown resource"` rather than `-32603` — absent is not a server fault, and empty
content is never the right way to say "not found".

See `mcp/templates.go`, `mcp/completion.go`, and the template API on `ResourceRegistry` in
`mcp/resources.go`.

### Skills (EXPERIMENTAL — SEP-2640)

`examples/go-mcp/main.go`'s UNIX-socket mode embeds `examples/go-mcp/skills/` and hands it to
`SkillRegistry.LoadFS`, which is what makes that server declare the
`io.modelcontextprotocol/skills` extension and answer `skills/list`, `skills/get`, and (because it
also sets `SkillsDirectoryRead`) `resources/directory/read`.

**This tracks an unmerged proposal.** SEP-2640 is Extensions Track, open, written here against head
`641d1eb` (2026-08-20). Nothing about skills is in the ratified 2026-07-28 spec and no public client
consumes it yet. Every exported symbol in `mcp/skills.go` carries an `EXPERIMENTAL:` doc comment
naming the SEP and that commit, so drift is visible in `go doc`. The single most important design
property is that `ServerConfig.Skills == nil` means *nothing changes*: no capability key, no method
answering anything but `-32601`.

Two rules from the SEP that the code must never soften:

1. **`skill://` is not privileged.** A server MAY serve skills under any scheme native to its domain,
   so nothing inspects or requires a scheme.
2. **Skill-ness is established only by `skills/list`/`skills/get`.** The library must never
   synthesize a skill entry by scanning the resource registry for a URI shape.

Key behaviors: a skill's files ARE resources, registered through the same `ResourceRegistry`, so
`resources/read` serves them and registration fires the ordinary
`notifications/resources/list_changed` — once per skill, not once per file, via the unexported
`ResourceRegistry.mutateBatch`. Digests are derived from the bytes, never supplied by a caller, so
the published `sha256:` and what `resources/read` returns cannot drift. The last path segment of a
skill's URI must equal its frontmatter `name`. Nested skills publish flat and share files with the
enclosing skill, which is why `SkillRegistry` refcounts file URIs. `skills/get` falls back to an
optional `SkillResolver` for unenumerable catalogs (that resolver owns its own digests and must be
paired with a `ResourceTemplate` to keep the files readable). `resources/directory/read` derives
direct children lexically from registered resource URIs rather than bookkeeping directories, so it
is scheme-agnostic and cannot go stale; a directory with no registered descendants and one that
never existed both answer `-32602`. `skills/get`'s cache envelope is this library's choice — the SEP
leaves it open. SEP-2640 defines no skills notification and none is invented here, which does leave a
real gap: a client that cached `skills/list` cannot learn a skill's frontmatter changed while its
file set stayed identical.

See `mcp/skills.go` (types, registry, loader, handlers), `DirectoryChildren` and `mutateBatch` in
`mcp/resources.go`, and the `Skills` / `SkillsDirectoryRead` fields on `ServerConfig`.

## Project Structure

```
generic-go-mcp/
├── config/               # PUBLIC: Configuration types and loading
├── logging/              # PUBLIC: Structured logging
├── auth/                 # PUBLIC: OAuth authentication (HTTP mode)
├── transport/            # PUBLIC: Transport abstractions (stdio, HTTP/SSE)
├── mcp/                  # PUBLIC: MCP protocol implementation
├── compat/               # PUBLIC: Optional legacy (2025-11-25 and earlier) compatibility overlay
├── examples/             # Example implementations
│   ├── go-mcp/           # Example MCP server application
│   │   └── skills/       # Embedded demonstration skill (SEP-2640, UNIX-socket mode)
│   └── tools/            # Reference tool implementations (date, fortune, confirm_delete/MRTR)
├── CLAUDE.md             # This file
└── go.mod                # Go module definition
```

All packages under the root are public and importable by third-party code, enabling you to build custom MCP servers using this framework as a library.

## Development Guidelines

1. **Transport Independence:** Tools should not depend on transport implementation details
2. **Error Handling:** Follow JSON-RPC 2.0 error codes and MCP error conventions
3. **Security:** Never log tokens or sensitive data; use secure token storage
4. **Testing:** Write tests for both stdio and HTTP/SSE transports
5. **Configuration:** Support all config sources (files, env vars, secrets)
