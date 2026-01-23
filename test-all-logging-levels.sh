#!/bin/bash

echo "=== Testing Logging Levels ==="
echo ""

# Function to make a test request
make_request() {
    sleep 2
    curl -s -X POST http://localhost:8080/mcp \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer test-secret-token" \
      -d '{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}},"id":1}' \
      > /dev/null
    sleep 1
}

# Test INFO level
echo "1. INFO Level (minimal output):"
echo "---"
./go-mcp -config config-logging-info.yaml 2>&1 &
PID=$!
make_request
kill -TERM $PID 2>/dev/null
wait $PID 2>/dev/null || true
echo ""

# Test DEBUG level
echo "2. DEBUG Level (access logs):"
echo "---"
./go-mcp -config config-logging-debug.yaml 2>&1 &
PID=$!
make_request
kill -TERM $PID 2>/dev/null
wait $PID 2>/dev/null || true
echo ""

# Test TRACE level
echo "3. TRACE Level (full HTTP details with redaction):"
echo "---"
./go-mcp -config config-logging-trace.yaml 2>&1 &
PID=$!
make_request
kill -TERM $PID 2>/dev/null
wait $PID 2>/dev/null || true
echo ""

echo "=== Test Complete ==="
