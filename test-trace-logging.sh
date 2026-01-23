#!/bin/bash

# Start server in background
./go-mcp -config config-logging-trace.yaml &
PID=$!

# Wait for server to be ready
sleep 3

# Make test request
echo "Making test request..."
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-token-123" \
  -d '{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}},"id":1}'

echo ""
echo "Waiting for logs..."
sleep 2

# Gracefully stop server
kill -TERM $PID 2>/dev/null
wait $PID 2>/dev/null || true

echo "Test complete"
