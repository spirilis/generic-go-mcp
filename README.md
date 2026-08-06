# generic-go-mcp

A reusable Go framework for building [Model Context Protocol](https://spec.modelcontextprotocol.io/) (MCP) servers with support for both stdio and Streaming HTTP transports.

## Overview

**generic-go-mcp** is a library that abstracts away the complexity of implementing MCP servers. It handles JSON-RPC 2.0 message parsing, transport layer management, authentication, and configuration—allowing you to focus on building powerful tools for Claude and other MCP clients.

### What is MCP?

The Model Context Protocol enables AI assistants like Claude to interact with external tools and data sources. This library makes it easy to create custom MCP servers that expose your own functionality to AI models.

### Protocol version

This library implements MCP protocol version **2026-07-28**, which made the protocol stateless: there
is no `initialize` handshake and no session — every request carries its own protocol version and
capabilities, and long-running server-to-client interactions (elicitation, sampling) travel as
[Multi Round-Trip Requests](GOLANG-MCP-CONVERT-TO-2026-07-28.md#4-worked-example-one-client-session-start-to-finish)
rather than server-initiated JSON-RPC requests. See
[GOLANG-MCP-CONVERT-TO-2026-07-28.md](GOLANG-MCP-CONVERT-TO-2026-07-28.md) for the full design
rationale and a worked example.

## Features

- **Dual Transport Support** - Run in stdio mode (for desktop integration) or Streaming HTTP mode (for web services)
- **OAuth Authentication** - Built-in GitHub OAuth 2.0 support with PKCE for HTTP mode
- **Flexible Configuration** - Load from YAML files, environment variables, or mounted secrets
- **Structured Logging** - Multi-level logging (trace/debug/info/warn/error) with JSON and text formats
- **Simple Tool API** - Register tools with JSON schema definitions and type-safe handlers
- **Production Ready** - BoltDB token storage, session management, graceful shutdown
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
    logging.Initialize(cfg.Logging)

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

## Installation

```bash
go get github.com/spirilis/generic-go-mcp
```

## Project Structure

```
generic-go-mcp/
├── config/               # Configuration loading (YAML, env vars, secrets)
├── logging/              # Structured logging with multiple levels
├── auth/                 # OAuth 2.0 authentication (GitHub)
├── transport/            # Transport abstractions (stdio, Streaming HTTP)
├── mcp/                  # MCP protocol implementation (JSON-RPC 2.0)
├── examples/
│   ├── go-mcp/           # Complete example server application
│   └── tools/            # Reference tool implementations (date, fortune)
├── CLAUDE-new-project-harness.md  # Comprehensive getting started guide
├── CLAUDE.md             # Architecture and design patterns
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

The library supports flexible configuration from multiple sources (in priority order):

1. YAML configuration files - Specified via `-config` flag
2. Defaults - Fallback values

### Example Configuration (stdio mode)

```yaml
server:
  mode: "stdio"

logging:
  level: "info"
  format: "text"
```

### Example Configuration (HTTP mode with auth)

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"
    port: 8080

auth:
  enabled: true
  github:
    client_id: "your-github-oauth-app-id"
    client_secret: "your-github-oauth-secret"
    redirect_url: "http://localhost:8080/auth/callback"
  db_path: "./auth.db"

logging:
  level: "info"
  format: "json"
```

See [examples/](examples/) for more configuration samples.

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

To build and run the example:

```bash
go build -o go-mcp ./examples/go-mcp
./go-mcp -config config.yaml
```

## Key Concepts

### Transport Layer
Abstracts communication mechanisms behind a common interface:
- **StdioTransport** - Reads from stdin, writes to stdout (for Claude Code, desktop apps)
- **HTTPTransport** - HTTP streaming (for web services, remote access)

### Tool Registry
Simple API for registering and listing tools (invocation is handled internally by the server, which
also enforces Multi Round-Trip Requests and `_meta` validation before your function runs):

```go
registry := mcp.NewToolRegistry()
registry.Register(toolDefinition, toolFunction)
registry.List()          // Returns all registered tools, in a stable order
registry.HasTools()      // Whether any tool is registered
```

Registering (or, via `mcp.NewResourceRegistry()`, unregistering) at runtime after the server has
started automatically emits a `notifications/tools/list_changed` (or `resources/list_changed`) to
any client with an open `subscriptions/listen` stream.

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
