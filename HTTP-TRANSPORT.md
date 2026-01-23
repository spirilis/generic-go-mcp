# HTTP/SSE Transport Guide

## Overview

The HTTP/SSE transport enables MCP servers to communicate via HTTP with Server-Sent Events (SSE) for response streaming. This allows the server to run as a web service accessible over the network.

## Configuration

Create a configuration file (e.g., `config-http.yaml`):

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"  # Listen on all interfaces (default)
    port: 8080        # Port number (default: 8080)
```

## Starting the Server

```bash
./go-mcp -config config-http.yaml
```

The server will start and listen on the configured host and port:
```
Starting HTTP server on 0.0.0.0:8080
```

## Client Communication Flow

### 1. Establish SSE Connection

Open a Server-Sent Events connection to receive responses:

```bash
curl -N http://localhost:8080/sse
```

The server immediately sends an `endpoint` event containing your session ID:

```
event: endpoint
data: {"uri":"/message","sessionId":"444f4924-2dbf-4f03-a405-42576147ea10"}
```

**Important**: Save the `sessionId` value - you'll need it for sending requests.

### 2. Send JSON-RPC Requests

Send JSON-RPC 2.0 requests to the `/message` endpoint with your session ID:

```bash
curl -X POST http://localhost:8080/message \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: YOUR_SESSION_ID" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/list",
    "params": {}
  }'
```

The endpoint returns `202 Accepted` immediately.

### 3. Receive Responses via SSE

Responses appear as `message` events in your SSE stream:

```
event: message
data: {"jsonrpc":"2.0","id":1,"result":{"tools":[...]}}
```

## Example Session

### Terminal 1: Open SSE Connection
```bash
curl -N http://localhost:8080/sse
# Note the sessionId from the endpoint event
```

### Terminal 2: Send Requests

List available tools:
```bash
curl -X POST http://localhost:8080/message \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: YOUR_SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

Call the date tool:
```bash
curl -X POST http://localhost:8080/message \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: YOUR_SESSION_ID" \
  -d '{
    "jsonrpc": "2.0",
    "id": 2,
    "method": "tools/call",
    "params": {
      "name": "date",
      "arguments": {
        "timezone": "America/New_York"
      }
    }
  }'
```

Watch Terminal 1 for the responses in the SSE stream.

## API Reference

### GET /sse

Establishes a Server-Sent Events connection for receiving responses.

**Response Headers:**
- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `Connection: keep-alive`

**Events:**
- `endpoint`: Initial event containing session info
  ```
  event: endpoint
  data: {"uri":"/message","sessionId":"<uuid>"}
  ```

- `message`: JSON-RPC response events
  ```
  event: message
  data: <JSON-RPC response>
  ```

### POST /message

Sends a JSON-RPC 2.0 request to the server.

**Headers:**
- `Content-Type: application/json` (required)
- `Mcp-Session-Id: <session-id>` (required)

**Body:** JSON-RPC 2.0 request

**Response:** `202 Accepted`

The actual response is delivered asynchronously via the SSE connection.

## Error Handling

- Missing `Mcp-Session-Id` header: `400 Bad Request`
- Invalid session ID: `400 Bad Request`
- Invalid HTTP method: `405 Method Not Allowed`

## Keep-Alive

The server sends periodic ping comments (`: ping\n\n`) every 30 seconds to keep the SSE connection alive.

## Testing

Use the provided test script:

```bash
chmod +x test-http.sh
./test-http.sh
```

This automated test verifies:
1. SSE connection establishment
2. Session ID generation
3. Request/response flow
4. Tool invocation

## Security Notes

This initial implementation has **no authentication**. For production use:

1. Add authentication middleware (OAuth, API keys, etc.)
2. Use HTTPS/TLS encryption
3. Implement rate limiting
4. Validate and sanitize inputs
5. Configure CORS appropriately for your use case

## Differences from stdio Transport

| Feature | stdio | HTTP/SSE |
|---------|-------|----------|
| Communication | stdin/stdout | HTTP endpoints |
| Response delivery | Synchronous | Asynchronous (SSE) |
| Multiple clients | Single process | Multiple sessions |
| Network access | Local only | Network accessible |
| Authentication | Process-level | Application-level |
