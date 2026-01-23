# Logging Implementation

This document describes the logging implementation added to the generic-go-mcp server.

## Overview

The server now includes configurable structured logging using Go's standard `log/slog` package, with support for three logging levels (info, debug, trace) and two output formats (text, json).

## Configuration

Add a `logging` section to your YAML config file:

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"
    port: 8080

logging:
  level: "debug"   # "info" (default), "debug", or "trace"
  format: "text"   # "text" (default) or "json"
```

## Logging Levels

### info (Default)
Minimal output for production use:
- Server startup/shutdown messages
- Critical errors
- Configuration information

Example:
```
time=2026-01-23T09:25:10.807-05:00 level=INFO msg="Starting MCP server in HTTP mode" host=0.0.0.0 port=8080
time=2026-01-23T09:25:10.808-05:00 level=INFO msg="HTTP server listening" addr=0.0.0.0:8080 transport="Streamable HTTP"
```

### debug
Detailed operational logging for development/troubleshooting:
- All info-level logs
- HTTP access logs (method, path, status, duration, size)
- Session creation/deletion
- Authentication events (success/failure with reasons)
- JSON-RPC method calls

Example:
```
time=2026-01-23T09:22:57.807-05:00 level=DEBUG msg="Session created" session_id=ac1357d5-019f-4a9e-96ee-795f30a503b4
time=2026-01-23T09:22:57.808-05:00 level=DEBUG msg="JSON-RPC request" method=initialize id=1
time=2026-01-23T09:22:57.808-05:00 level=DEBUG msg="HTTP request completed" method=POST path=/mcp status=200 size=151 duration_ms=0 remote_addr=[::1]:46466
```

### trace
Extremely verbose logging for deep debugging:
- All debug-level logs
- Full HTTP headers (with sensitive values redacted)
- Complete request/response bodies
- JSON-RPC parameters and responses

Example:
```
time=2026-01-23T09:22:57.807-05:00 level=TRACE msg="HTTP request received" method=POST path=/mcp remote_addr=[::1]:46466 headers="map[Accept:*/* Authorization:[REDACTED] Content-Length:106 Content-Type:application/json User-Agent:curl/8.5.0]"
time=2026-01-23T09:22:57.807-05:00 level=TRACE msg="HTTP POST request body" body="{\"jsonrpc\":\"2.0\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{}},\"id\":1}"
time=2026-01-23T09:22:57.808-05:00 level=TRACE msg="JSON-RPC params" method=initialize params="{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{}}"
time=2026-01-23T09:22:57.808-05:00 level=TRACE msg="JSON-RPC response" method=initialize result="{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"generic-go-mcp\",\"version\":\"0.1.0\"}}"
```

## Output Formats

### text (Default)
Human-readable format optimized for terminal viewing:
```
time=2026-01-23T09:22:57.808-05:00 level=DEBUG msg="Session created" session_id=ac1357d5-019f-4a9e-96ee-795f30a503b4
```

### json
Machine-readable JSON format for log aggregation tools:
```json
{"time":"2026-01-23T09:25:10.807-05:00","level":"INFO","msg":"Starting MCP server in HTTP mode","host":"0.0.0.0","port":8080}
```

## Security

The logging implementation includes automatic sanitization of sensitive data:
- **Authorization** headers are redacted as `[REDACTED]`
- **Cookie** and **Set-Cookie** headers are redacted
- **X-Api-Key** and **X-Auth-Token** headers are redacted

This ensures that secrets never appear in logs, even at trace level.

## Implementation Details

### Files Modified

1. **internal/config/config.go**
   - Added `LoggingConfig` struct with `Level` and `Format` fields
   - Added defaults (level="info", format="text")

2. **internal/logging/logging.go** (new file)
   - Custom `LevelTrace` constant for trace logging
   - `Initialize()` function to configure slog
   - Helper functions: `Trace()`, `Debug()`, `Info()`, `Warn()`, `Error()`
   - `IsTraceEnabled()`, `IsDebugEnabled()` for conditional logging
   - `SanitizeHeaders()` for redacting sensitive values

3. **internal/transport/http.go**
   - `responseRecorder` wrapper to capture status/size/body
   - Request timing and logging in `handleMCP()`
   - Session lifecycle logging in `handlePost()`, `handleDelete()`
   - SSE connection logging in `handleGet()`

4. **internal/auth/middleware.go**
   - Authentication attempt logging (success/failure with reasons)
   - User information logging on successful auth

5. **internal/mcp/server.go**
   - JSON-RPC method call logging
   - Full parameter and response logging at trace level

6. **cmd/go-mcp/main.go**
   - Early logger initialization after config load
   - Replaced fmt.Fprintf calls with logging functions

## Testing

Example config files are provided:
- `config-logging-info.yaml` - Minimal logging
- `config-logging-debug.yaml` - Development logging
- `config-logging-trace.yaml` - Full diagnostic logging
- `config-logging-json.yaml` - JSON format output

Test the logging:
```bash
# Build the server
go build -o go-mcp ./cmd/go-mcp

# Test info level (minimal output)
./go-mcp -config config-logging-info.yaml

# Test debug level (access logs)
./go-mcp -config config-logging-debug.yaml

# Test trace level (full HTTP dumps)
./go-mcp -config config-logging-trace.yaml

# Test JSON format
./go-mcp -config config-logging-json.yaml
```

## Usage with Claude Code

When debugging connection issues with Claude Code, use debug or trace level logging:

```yaml
logging:
  level: "trace"  # Captures all HTTP traffic details
  format: "text"
```

Run the server and redirect stderr to a file:
```bash
./go-mcp -config config.yaml 2>mcp-debug.log
```

The log file will contain complete details of all HTTP requests, headers, bodies, and JSON-RPC messages for troubleshooting.
