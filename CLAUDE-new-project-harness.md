# Building Your MCP Server with generic-go-mcp

This guide walks you through creating your own Model Context Protocol (MCP) server using the generic-go-mcp library. Follow these steps to build a custom server with your own tools.

## Project Setup

### 1. Initialize Your Go Module

```bash
mkdir my-mcp-server
cd my-mcp-server
go mod init github.com/yourusername/my-mcp-server
```

### 2. Add the Library Dependency

```bash
go get github.com/spirilis/generic-go-mcp
```

### 3. Recommended Project Structure

```
my-mcp-server/
├── main.go              # Application entry point
├── tools/               # Your custom tool implementations
│   ├── mytool.go
│   └── anothertool.go
├── config.yaml          # Configuration file
├── go.mod
└── go.sum
```

## Tool Development Guide

### Understanding the Tool Architecture

Every MCP tool consists of two components:

1. **Tool Definition** (`mcp.Tool`) - Describes the tool's name, description, and input schema
2. **Tool Function** (`mcp.ToolFunction`) - The actual implementation

### Tool Definition Structure

```go
type Tool struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    InputSchema json.RawMessage `json:"inputSchema"`
}
```

### Tool Function Signature

```go
type ToolFunction func(arguments json.RawMessage) (interface{}, error)
```

### Return Types

Your tool function must return a `mcp.ToolCallResult`:

```go
type ToolCallResult struct {
    Content []ToolContent `json:"content"`
    IsError bool          `json:"isError,omitempty"`
}

type ToolContent struct {
    Type string `json:"type"`  // Usually "text"
    Text string `json:"text"`  // Your result content
}
```

### Pattern: Tool WITH Arguments

Here's a complete example of a tool that accepts arguments:

```go
package tools

import (
    "encoding/json"
    "fmt"
    "time"

    "github.com/spirilis/generic-go-mcp/mcp"
)

// Define your argument structure
type DateArguments struct {
    Timezone string `json:"timezone"`
}

// Implement the tool function
func DateTool(arguments json.RawMessage) (interface{}, error) {
    // 1. Parse the JSON arguments
    var args DateArguments
    if err := json.Unmarshal(arguments, &args); err != nil {
        return nil, fmt.Errorf("invalid arguments: %w", err)
    }

    // 2. Validate inputs
    timezone := args.Timezone
    if timezone == "" {
        timezone = "UTC"
    }

    // 3. Execute your logic
    loc, err := time.LoadLocation(timezone)
    if err != nil {
        return nil, fmt.Errorf("invalid timezone: %w", err)
    }

    now := time.Now().In(loc)
    formatted := now.Format("2006-01-02 15:04:05 MST")

    // 4. Return the result
    return mcp.ToolCallResult{
        Content: []mcp.ToolContent{
            {
                Type: "text",
                Text: formatted,
            },
        },
    }, nil
}

// Create the tool definition
func GetDateToolDefinition() mcp.Tool {
    // Define JSON schema for inputs
    schema := json.RawMessage(`{
        "type": "object",
        "properties": {
            "timezone": {
                "type": "string",
                "description": "IANA timezone name (e.g., 'America/New_York', 'Europe/London', 'Asia/Tokyo')"
            }
        },
        "required": ["timezone"]
    }`)

    return mcp.Tool{
        Name:        "date",
        Description: "Returns the current date and time in the specified timezone",
        InputSchema: schema,
    }
}
```

### Pattern: Tool WITHOUT Arguments

Here's a simpler tool that doesn't require arguments:

```go
package tools

import (
    "encoding/json"
    "fmt"
    "os/exec"
    "strings"

    "github.com/spirilis/generic-go-mcp/mcp"
)

// Tool function (arguments parameter is still required but can be ignored)
func FortuneTool(arguments json.RawMessage) (interface{}, error) {
    // Execute your logic directly
    cmd := exec.Command("fortune")
    output, err := cmd.Output()
    if err != nil {
        return nil, fmt.Errorf("failed to execute fortune: %w", err)
    }

    // Return the result
    return mcp.ToolCallResult{
        Content: []mcp.ToolContent{
            {
                Type: "text",
                Text: strings.TrimSpace(string(output)),
            },
        },
    }, nil
}

// Tool definition with empty schema
func GetFortuneToolDefinition() mcp.Tool {
    schema := json.RawMessage(`{
        "type": "object",
        "properties": {}
    }`)

    return mcp.Tool{
        Name:        "fortune",
        Description: "Returns a random fortune from the fortune command",
        InputSchema: schema,
    }
}
```

### Error Handling Patterns

**Return errors for invalid inputs or execution failures:**

```go
func MyTool(arguments json.RawMessage) (interface{}, error) {
    var args MyArguments
    if err := json.Unmarshal(arguments, &args); err != nil {
        return nil, fmt.Errorf("invalid arguments: %w", err)
    }

    // Validate business logic
    if args.Value < 0 {
        return nil, fmt.Errorf("value must be non-negative")
    }

    // Handle external errors
    result, err := someExternalCall()
    if err != nil {
        return nil, fmt.Errorf("external call failed: %w", err)
    }

    return mcp.ToolCallResult{
        Content: []mcp.ToolContent{
            {Type: "text", Text: result},
        },
    }, nil
}
```

## Main Application Template

Create your `main.go` with this template:

```go
package main

import (
    "flag"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    "github.com/spirilis/generic-go-mcp/config"
    "github.com/spirilis/generic-go-mcp/logging"
    "github.com/spirilis/generic-go-mcp/mcp"
    "github.com/spirilis/generic-go-mcp/transport"

    // Import your tools package
    "github.com/yourusername/my-mcp-server/tools"
)

func main() {
    // 1. Parse command-line flags
    configPath := flag.String("config", "config.yaml", "Path to configuration file")
    flag.Parse()

    // 2. Load configuration
    cfg, err := config.Load(*configPath)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
        os.Exit(1)
    }

    // 3. Initialize logger early (so all subsequent code can use logging)
    logging.Initialize(cfg.Logging)

    // 4. Create tool registry and register your tools
    registry := mcp.NewToolRegistry()
    registry.Register(tools.GetDateToolDefinition(), tools.DateTool)
    registry.Register(tools.GetFortuneToolDefinition(), tools.FortuneTool)
    // Add more tools as needed
    // registry.Register(tools.GetMyToolDefinition(), tools.MyTool)

    // 5. Create MCP server with custom name and version
    server := mcp.NewServer(registry, &mcp.ServerConfig{
        Name:    "my-mcp-server",
        Version: "1.0.0",
    })

    // 6. Create and start transport based on config
    var trans transport.Transport
    switch cfg.Server.Mode {
    case "stdio":
        trans = transport.NewStdioTransport()
        logging.Info("Starting MCP server in stdio mode")
    case "http":
        trans = transport.NewHTTPTransport(transport.HTTPTransportConfig{
            Host: cfg.Server.HTTP.Host,
            Port: cfg.Server.HTTP.Port,
            // AuthService can be added here if needed
        })
        logging.Info("Starting MCP server in HTTP mode",
            "host", cfg.Server.HTTP.Host,
            "port", cfg.Server.HTTP.Port)
    default:
        logging.Error("Unknown transport mode", "mode", cfg.Server.Mode)
        os.Exit(1)
    }

    // 7. Start the transport
    if err := trans.Start(server); err != nil {
        logging.Error("Error starting transport", "error", err)
        os.Exit(1)
    }

    // 8. Wait for interrupt signal (Ctrl+C)
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
    <-sigCh

    logging.Info("Shutting down gracefully")

    // 9. Graceful shutdown
    if err := trans.Stop(); err != nil {
        logging.Error("Error stopping transport", "error", err)
        os.Exit(1)
    }

    logging.Info("Shutdown complete")
}
```

## Configuration Reference

### Basic Configuration (stdio mode)

Create `config.yaml`:

```yaml
server:
  mode: "stdio"

logging:
  level: "info"      # Options: trace, debug, info, warn, error
  format: "text"     # Options: text, json
```

### HTTP Mode Configuration

Create `config-http.yaml`:

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"
    port: 8080

logging:
  level: "info"
  format: "text"
```

### Advanced Configuration with Auth

For HTTP mode with GitHub OAuth authentication:

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"
    port: 8080

auth:
  enabled: true
  github:
    client_id: "your-github-oauth-app-client-id"
    client_secret: "your-github-oauth-app-client-secret"
    redirect_url: "http://localhost:8080/auth/callback"
  db_path: "./auth.db"  # BoltDB file for storing tokens

logging:
  level: "debug"
  format: "json"
```

**Note:** To enable auth in your main.go, add the auth initialization code:

```go
import "github.com/spirilis/generic-go-mcp/auth"

// In main(), after loading config:
var authService *auth.AuthService
if cfg.Auth != nil && cfg.Auth.Enabled {
    var err error
    authService, err = auth.NewAuthService(cfg.Auth)
    if err != nil {
        logging.Error("Error initializing auth", "error", err)
        os.Exit(1)
    }
    defer authService.Close()
}

// Pass authService to HTTP transport:
trans = transport.NewHTTPTransport(transport.HTTPTransportConfig{
    Host:        cfg.Server.HTTP.Host,
    Port:        cfg.Server.HTTP.Port,
    AuthService: authService,
})
```

### Configuration Sources

The library supports multiple configuration sources (in priority order):

1. **Mounted secrets** (Kubernetes/Docker) - `/run/secrets/`
2. **Environment variables** - `MCP_SERVER_MODE`, `MCP_LOGGING_LEVEL`, etc.
3. **YAML files** - Specified with `-config` flag
4. **Defaults** - Fallback values

## Build Commands

### Standard Build

```bash
go build -o my-mcp-server
```

### Cross-Compilation for Linux

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o my-mcp-server
```

### Cross-Compilation for macOS (Apple Silicon)

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o my-mcp-server
```

### Cross-Compilation for Windows

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o my-mcp-server.exe
```

### Docker Build (Multi-Platform)

Create a `Dockerfile`:

```dockerfile
# Build stage - use native platform
FROM golang:1.21 AS builder
WORKDIR /build
COPY . .

# Cross-compile for target platform
ARG TARGETPLATFORM
RUN CGO_ENABLED=0 go build -o my-mcp-server .

# Runtime stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /build/my-mcp-server /usr/local/bin/
COPY config.yaml /etc/my-mcp-server/config.yaml
ENTRYPOINT ["my-mcp-server"]
CMD ["-config", "/etc/my-mcp-server/config.yaml"]
```

Build for multiple platforms:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t my-mcp-server:latest .
```

## Testing Your MCP Server

### Testing with Claude Code (stdio mode)

1. **Build your server:**
   ```bash
   go build -o my-mcp-server
   ```

2. **Configure Claude Code to use your server:**

   Add to your Claude Code MCP settings (`~/.claude/mcp_settings.json`):

   ```json
   {
     "mcpServers": {
       "my-mcp-server": {
         "command": "/absolute/path/to/my-mcp-server",
         "args": ["-config", "/absolute/path/to/config.yaml"]
       }
     }
   }
   ```

3. **Restart Claude Code** and your tools will be available.

4. **Test your tools** by asking Claude to use them:
   ```
   What's the current time in Tokyo?
   ```

### Testing HTTP Mode

1. **Start your server:**
   ```bash
   ./my-mcp-server -config config-http.yaml
   ```

2. **Test the endpoints:**

   ```bash
   # Check server info
   curl http://localhost:8080/

   # List available tools
   curl -X POST http://localhost:8080/sse \
     -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","method":"tools/list","id":1}'

   # Call a tool
   curl -X POST http://localhost:8080/sse \
     -H "Content-Type: application/json" \
     -d '{
       "jsonrpc":"2.0",
       "method":"tools/call",
       "params":{"name":"date","arguments":{"timezone":"America/New_York"}},
       "id":2
     }'
   ```

3. **For SSE streaming**, use a tool that supports SSE or connect via browser.

### Debugging Tips

**Enable debug logging:**
```yaml
logging:
  level: "debug"
  format: "text"
```

**Enable trace logging** (shows full request/response payloads):
```yaml
logging:
  level: "trace"
  format: "text"
```

**Common issues:**
- **Tool not found:** Ensure your tool is registered in `main.go`
- **Invalid arguments:** Check your InputSchema matches your argument struct
- **Import errors:** Verify your import paths match your Go module name
- **Config not loading:** Check the config file path is absolute or relative to execution directory

## Next Steps

1. **Add more tools** - Create new tool files in your `tools/` package
2. **Customize configuration** - Add your own config fields for tool behavior
3. **Deploy** - Use Docker, systemd, or Kubernetes to deploy your server
4. **Add authentication** - Enable GitHub OAuth for HTTP mode
5. **Monitor** - Use structured logging (JSON format) with log aggregation tools

## Reference

- **MCP Specification:** https://spec.modelcontextprotocol.io/
- **generic-go-mcp Repository:** https://github.com/spirilis/generic-go-mcp
- **Example Tools:** See `examples/tools/` in the library repository

---

**Happy building!** You now have everything needed to create custom MCP servers using the generic-go-mcp library.
