# generic-go-mcp

A reusable Go framework for building [Model Context Protocol](https://spec.modelcontextprotocol.io/) (MCP) servers with support for both stdio and HTTP/SSE transports.

## Overview

**generic-go-mcp** is a production-ready library that abstracts away the complexity of implementing MCP servers. It handles JSON-RPC 2.0 message parsing, transport layer management, authentication, and configuration—allowing you to focus on building powerful tools for Claude and other MCP clients.

### What is MCP?

The Model Context Protocol enables AI assistants like Claude to interact with external tools and data sources. This library makes it easy to create custom MCP servers that expose your own functionality to AI models.

## Features

- **Dual Transport Support** - Run in stdio mode (for desktop integration) or HTTP/SSE mode (for web services)
- **OAuth Authentication** - Built-in GitHub OAuth 2.0 support with PKCE for HTTP mode
- **Flexible Configuration** - Load from YAML files, environment variables, or mounted secrets
- **Structured Logging** - Multi-level logging (trace/debug/info/warn/error) with JSON and text formats
- **Simple Tool API** - Register tools with JSON schema definitions and type-safe handlers
- **Production Ready** - BoltDB token storage, session management, graceful shutdown

## Quick Start

```go
package main

import (
    "encoding/json"
    "github.com/spirilis/generic-go-mcp/config"
    "github.com/spirilis/generic-go-mcp/logging"
    "github.com/spirilis/generic-go-mcp/mcp"
    "github.com/spirilis/generic-go-mcp/transport"
    "time"
)

func main() {
    // Load configuration
    cfg, _ := config.Load("config.yaml")
    logging.Initialize(cfg.Logging)

    // Create tool registry
    registry := mcp.NewToolRegistry()

    // Define a simple tool
    timeTool := mcp.Tool{
        Name:        "current_time",
        Description: "Returns the current UTC time",
        InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
    }

    // Register tool with implementation
    registry.Register(timeTool, func(args json.RawMessage) (interface{}, error) {
        return mcp.ToolCallResult{
            Content: []mcp.ToolContent{
                {Type: "text", Text: time.Now().UTC().String()},
            },
        }, nil
    })

    // Create MCP server
    server := mcp.NewServer(registry, &mcp.ServerConfig{
        Name:    "my-mcp-server",
        Version: "1.0.0",
    })

    // Start stdio transport
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
├── transport/            # Transport abstractions (stdio, HTTP/SSE)
├── mcp/                  # MCP protocol implementation (JSON-RPC 2.0)
├── examples/
│   ├── go-mcp/           # Complete example server application
│   └── tools/            # Reference tool implementations (date, fortune)
├── CLAUDE-new-project-harness.md  # Comprehensive getting started guide
├── CLAUDE.md             # Architecture and design patterns
├── HTTP-TRANSPORT.md     # HTTP/SSE transport documentation
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

1. Mounted secrets (Kubernetes/Docker) - `/run/secrets/`
2. Environment variables - `MCP_SERVER_MODE`, `MCP_LOGGING_LEVEL`, etc.
3. YAML configuration files - Specified via `-config` flag
4. Defaults - Fallback values

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
- **[HTTP-TRANSPORT.md](HTTP-TRANSPORT.md)** - HTTP/SSE transport details
- **[LOGGING.md](LOGGING.md)** - Logging system documentation
- **[MCP Specification](https://spec.modelcontextprotocol.io/)** - Official Model Context Protocol specification

## Examples

The [examples/](examples/) directory contains:

- **go-mcp/** - A complete MCP server demonstrating stdio/HTTP mode, auth integration, and graceful shutdown
- **tools/date.go** - Example tool with arguments (timezone parameter)
- **tools/fortune.go** - Example tool without arguments (executes fortune command)

To build and run the example:

```bash
go build -o go-mcp ./examples/go-mcp
./go-mcp -config config.yaml
```

## Key Concepts

### Transport Layer
Abstracts communication mechanisms behind a common interface:
- **StdioTransport** - Reads from stdin, writes to stdout (for Claude Code, desktop apps)
- **HTTPTransport** - HTTP/SSE streaming (for web services, remote access)

### Tool Registry
Simple API for registering and invoking tools:

```go
registry := mcp.NewToolRegistry()
registry.Register(toolDefinition, toolFunction)
registry.List()  // Returns all registered tools
registry.Call(name, arguments)  // Invokes a tool
```

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
3. Add tests for both stdio and HTTP/SSE transports
4. Document new features in the appropriate .md files

## Acknowledgments

Built following the [Model Context Protocol specification](https://spec.modelcontextprotocol.io/) by Anthropic.
