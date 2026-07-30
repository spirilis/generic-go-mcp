#!/bin/bash

# Start server in background
./go-mcp -config config-logging-trace.yaml &
PID=$!

# Wait for server to be ready
sleep 3

# Make test request. MCP protocol version 2026-07-28 is stateless — no initialize
# handshake — so this calls server/discover with the required headers and per-request
# _meta instead.
echo "Making test request..."
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-token-123" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: server/discover" \
  -d '{"jsonrpc":"2.0","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}},"id":1}'

echo ""
echo "Waiting for logs..."
sleep 2

# Gracefully stop server
kill -TERM $PID 2>/dev/null
wait $PID 2>/dev/null || true

echo "Test complete"
