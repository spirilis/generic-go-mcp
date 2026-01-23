# generic-go-mcp

A reusable Go framework for building Model Context Protocol (MCP) servers with support for both stdio and HTTP/SSE transports.

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
Abstracts communication mechanisms (stdio vs HTTP/SSE) behind a common interface. Allows MCP servers to run in different environments without protocol-specific code changes.

**Key Interface:**
```go
type Transport interface {
    Start(handler MessageHandler) error
    Stop() error
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
Enables MCP servers to support both stdio and HTTP/SSE without duplicating protocol logic. Implementations:
- `StdioTransport` - reads from stdin, writes to stdout
- `SSETransport` - handles HTTP SSE streaming

### JSON-RPC 2.0 Protocol Handling
All MCP messages follow JSON-RPC 2.0 specification:
```json
{
  "jsonrpc": "2.0",
  "method": "tools/call",
  "params": {...},
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

The framework includes two reference tool implementations demonstrating best practices:

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
│   └── tools/            # Reference tool implementations (date, fortune)
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
