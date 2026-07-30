#!/bin/bash

# Test Streamable HTTP Transport (MCP protocol version 2026-07-28)
#
# This protocol revision is stateless: there is no initialize handshake and no
# Mcp-Session-Id. Every POST to /mcp carries its own MCP-Protocol-Version, Mcp-Method,
# and (for tools/call, resources/read, prompts/get) Mcp-Name headers, matching the
# corresponding fields in the JSON-RPC body.

set -e

echo "Starting Streamable HTTP Transport Test..."
echo

echo "Starting server..."
./go-mcp -config config-http.yaml &
SERVER_PID=$!
sleep 2
trap 'kill $SERVER_PID 2>/dev/null' EXIT

META='"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"test-http.sh","version":"1.0"},"io.modelcontextprotocol/clientCapabilities":{}}'

# Test 1: server/discover — the replacement for the old initialize handshake. No
# session is created or returned; the response is a single JSON object.
echo "Test 1: server/discover"
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: server/discover" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"server/discover\",\"params\":{$META}}" \
  -w "\nHTTP Status: %{http_code}\n"
echo

# Test 2: tools/list
echo "Test 2: tools/list"
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: tools/list" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\",\"params\":{$META}}" \
  -w "\nHTTP Status: %{http_code}\n"
echo

# Test 3: tools/call for the date tool. Mcp-Name must match params.name.
echo "Test 3: tools/call (date tool)"
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: tools/call" \
  -H "Mcp-Name: date" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"date\",\"arguments\":{\"timezone\":\"America/New_York\"},$META}}" \
  -w "\nHTTP Status: %{http_code}\n"
echo

# Test 4: a request missing the required headers must be rejected with 400 and a
# HeaderMismatch (-32020) JSON-RPC error — there is no fallback to session-based auth.
echo "Test 4: tools/list without required headers (expect 400 / -32020)"
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/list\",\"params\":{$META}}" \
  -w "\nHTTP Status: %{http_code}\n"
echo

# Test 5: a legacy "initialize" request gets a diagnostic naming the versions this
# server supports, rather than a session.
echo "Test 5: legacy initialize (expect 404 / -32601 diagnostic)"
curl -s -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}' \
  -w "\nHTTP Status: %{http_code}\n"
echo

echo "Test complete!"
