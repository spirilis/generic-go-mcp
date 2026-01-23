#!/bin/bash

# Test HTTP/SSE Transport
echo "Starting HTTP/SSE Transport Test..."
echo

# Start the server in the background
echo "Starting server..."
./go-mcp -config config-http.yaml &
SERVER_PID=$!
sleep 2

# Test 1: Open SSE connection and capture session ID
echo "Test 1: Opening SSE connection..."
rm -f /tmp/sse-output.txt
curl -N http://localhost:8080/sse > /tmp/sse-output.txt 2>&1 &
SSE_PID=$!
sleep 2

# Extract session ID from SSE output
SESSION_ID=$(grep -oP '"sessionId":"\K[^"]+' /tmp/sse-output.txt | head -1)
echo "Received Session ID: $SESSION_ID"
echo

if [ -z "$SESSION_ID" ]; then
    echo "ERROR: Failed to get session ID"
    kill $SSE_PID $SERVER_PID 2>/dev/null
    exit 1
fi

# Test 2: Send tools/list request
echo "Test 2: Sending tools/list request..."
curl -X POST http://localhost:8080/message \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' \
  -w "\nHTTP Status: %{http_code}\n"
echo

# Wait for response in SSE stream
sleep 2
echo "SSE Stream Output:"
cat /tmp/sse-output.txt
echo

# Test 3: Send tools/call request for date tool
echo "Test 3: Sending tools/call request for date tool..."
curl -X POST http://localhost:8080/message \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"date","arguments":{"timezone":"America/New_York"}}}' \
  -w "\nHTTP Status: %{http_code}\n"
echo

# Wait for response
sleep 2

# Cleanup
echo "Cleaning up..."
kill $SSE_PID 2>/dev/null
kill $SERVER_PID 2>/dev/null
sleep 1

echo "Test complete!"
