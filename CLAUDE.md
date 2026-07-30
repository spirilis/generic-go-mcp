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

    // Create and start transport
    trans := transport.NewStdioTransport()
    trans.Start(server)
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

**Key Interfaces:**
```go
type Transport interface {
    Start(handler MessageHandler) error
    Stop() error
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
  sharing a common framing (`transport/stream.go`) that dispatches each message concurrently
- `HTTPTransport` - a single POST-only `/mcp` endpoint; responds with a plain JSON object, or
  upgrades to a request-scoped `text/event-stream` the moment a handler emits a notification
  (progress, or a `subscriptions/listen` change notification)

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

## Project Structure

```
generic-go-mcp/
├── config/               # PUBLIC: Configuration types and loading
├── logging/              # PUBLIC: Structured logging
├── auth/                 # PUBLIC: OAuth authentication (HTTP mode)
├── transport/            # PUBLIC: Transport abstractions (stdio, HTTP/SSE)
├── mcp/                  # PUBLIC: MCP protocol implementation
├── examples/             # Example implementations
│   ├── go-mcp/           # Example MCP server application
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
