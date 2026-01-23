# generic-go-mcp

A reusable Go framework for building Model Context Protocol (MCP) servers with support for both stdio and HTTP/SSE transports.

## Build Commands

### Standard Go Build
```bash
go build -o go-mcp ./cmd/go-mcp
```

### Cross-Compilation
For static binaries without CGO dependencies:
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o go-mcp ./cmd/go-mcp
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
RUN CGO_ENABLED=0 go build -o go-mcp ./cmd/go-mcp

# Runtime stage
FROM alpine:latest
COPY --from=builder /build/go-mcp /usr/local/bin/
ENTRYPOINT ["go-mcp"]
```

Build for multiple platforms:
```bash
docker buildx build --platform linux/amd64,linux/arm64 -t go-mcp:latest .
```

## Architecture Overview

The framework follows a layered architecture pattern:

### Transport Layer (`internal/transport/`)
Abstracts communication mechanisms (stdio vs HTTP/SSE) behind a common interface. Allows MCP servers to run in different environments without protocol-specific code changes.

**Key Interface:**
```go
type Transport interface {
    Read() ([]byte, error)
    Write([]byte) error
    Close() error
}
```

### MCP Protocol Layer (`internal/mcp/`)
Handles JSON-RPC 2.0 message parsing, validation, and routing. Manages tool definitions and their registration.

**Responsibilities:**
- JSON-RPC 2.0 request/response handling
- Tool definition schema
- Method routing
- Error handling per MCP specification

### Auth Layer (`internal/auth/`)
Implements authentication and authorization for HTTP/SSE mode.

**Components:**
- GitHub OAuth 2.0 Authorization Code flow
- Token persistence (BoltDB)
- Session management
- Authentication middleware

### Server Layer (`internal/server/`)
HTTP server implementation using Chi router.

**Features:**
- RESTful endpoint routing
- SSE endpoint for MCP protocol
- OAuth callback handling
- Middleware chain (auth, logging, CORS)

### Config Layer (`internal/config/`)
Flexible configuration loading supporting multiple sources:

**Sources (in priority order):**
1. Mounted secrets (Kubernetes/Docker)
2. Environment variables
3. YAML configuration files
4. Defaults

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
├── cmd/
│   └── go-mcp/           # Main entry point
├── internal/
│   ├── transport/        # Transport abstraction
│   ├── mcp/              # MCP protocol implementation
│   ├── auth/             # OAuth and authentication
│   ├── server/           # HTTP server (Chi)
│   └── config/           # Configuration loading
└── CLAUDE.md             # This file
```

## Development Guidelines

1. **Transport Independence:** Tools should not depend on transport implementation details
2. **Error Handling:** Follow JSON-RPC 2.0 error codes and MCP error conventions
3. **Security:** Never log tokens or sensitive data; use secure token storage
4. **Testing:** Write tests for both stdio and HTTP/SSE transports
5. **Configuration:** Support all config sources (files, env vars, secrets)
