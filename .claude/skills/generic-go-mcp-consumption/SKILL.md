---
name: generic-go-mcp-consumption
description: |
  Build an MCP (Model Context Protocol) server in Go on top of the
  github.com/spirilis/generic-go-mcp library — protocol version 2026-07-28,
  stateless (no initialize handshake, no session). Covers: registering tools
  and resources, choosing a transport (stdio / UNIX socket / Streamable
  HTTP), the AuthProvider interface for optional GitHub OAuth on HTTP,
  Multi Round-Trip Requests (MRTR) for elicitation-style confirm-before-act
  tools, and the biggest first-contact gotcha — every client request must
  carry params._meta with a protocol version and capabilities, or it fails.
  Use this skill when: starting a new Go MCP server from scratch, adding a
  tool or resource to an existing generic-go-mcp server, picking a
  transport, wiring OAuth, debugging "missing required _meta field" or
  "missing required header" errors, or a client that can't get past its
  first request against this server.
---

# Building an MCP server on generic-go-mcp

This is a from-scratch Go implementation of MCP — no upstream MCP SDK underneath. It targets
protocol **2026-07-28** only, which is a hard break from every earlier revision: **there is no
`initialize` handshake and no session.** Every request is self-contained and carries its own
protocol version, capabilities, and (optionally) identity in `params._meta`. If you only remember
one thing from this file, remember that — it's the failure mode you'll hit first.

> This skill lives inside the generic-go-mcp repo (`.claude/skills/generic-go-mcp-consumption/`),
> so it's versioned with the library but does **not** auto-load in other projects. Copy or symlink
> this directory into `~/.claude/skills/` if you want it available globally.

## Get the module

```bash
go get github.com/spirilis/generic-go-mcp@v0.2.0
```

Import weight depends on what you use:
- `mcp` + `transport` alone (stdio or unauthenticated HTTP) → **pure standard library**, nothing else.
- `auth` → adds `go.etcd.io/bbolt` (token storage) and `golang.org/x/sys`.
- `config` → adds `gopkg.in/yaml.v3`. You don't need it; it's an optional convenience for YAML-file
  config. A CLI tool with flags, or a hardcoded config struct, works just as well.

## Minimal stdio server

This compiles as-is against `v0.2.0`. It's the shape every server starts from: a `ToolRegistry`, a
`ResourceRegistry` (even if empty — `NewServer` requires both), wrapped in a `Server`, driven by a
`Transport`.

```go
package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

func main() {
	registry := mcp.NewToolRegistry()
	registry.Register(echoToolDef(), echoTool)

	resources := mcp.NewResourceRegistry() // fine to leave empty

	server := mcp.NewServer(registry, resources, &mcp.ServerConfig{
		Name:    "my-mcp-server",
		Version: "0.1.0",
	})

	trans := transport.NewStdioTransport()
	if err := trans.Start(server); err != nil {
		os.Exit(1)
	}
	// trans.Start launches the read loop in a goroutine and returns immediately;
	// block on your own shutdown signal, then call trans.Stop().
	select {} // replace with a real signal.Notify + <-sigCh in a real server
}

func echoTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
	var args struct{ Text string `json:"text"` }
	if err := req.BindArguments(&args); err != nil {
		return mcp.ErrorResultf("invalid arguments: %v", err), nil
	}
	return &mcp.ToolCallResult{Content: []mcp.Content{mcp.Text(args.Text)}}, nil
}

func echoToolDef() mcp.Tool {
	return mcp.Tool{
		Name:        "echo",
		Description: "Echoes the given text back",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}
}
```

For a complete real example (three tools, all three transports, optional OAuth, config file/flag
handling), read `examples/go-mcp/main.go` in this repo — it's the actual reference implementation,
not a toy.

## The first-contact gotcha: `params._meta`

A client that has never talked to a 2026-07-28 server, or a hand-rolled test client, will send
something like:

```json
{"jsonrpc":"2.0","method":"tools/list","params":{},"id":1}
```

This server rejects it: **every request** (except a legacy `initialize`, which gets a diagnostic
error naming the supported versions, not a session) must include:

```json
{
  "jsonrpc": "2.0",
  "method": "tools/list",
  "params": {
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  },
  "id": 1
}
```

`clientCapabilities` must be present (an empty object `{}` is fine if the client declares nothing).
Missing either field is a `-32602 Invalid params` error; a `protocolVersion` this server doesn't
recognize is `-32022`. See `references/protocol-essentials.md` for the full error-code table and
curl recipes for exercising a running server by hand.

## Where to go next

| Task | Read |
|---|---|
| Define a tool's schema, handler signature, content/error conventions | `references/tool-authoring.md` |
| Choose stdio vs UNIX socket vs Streamable HTTP; wire optional GitHub OAuth | `references/transports-and-auth.md` |
| Build a confirm-before-act tool (delete, send, pay) or a readable resource | `references/mrtr-and-resources.md` |
| Debug a client that can't complete its first request; header/error reference | `references/protocol-essentials.md` |

## Accuracy note for future edits to this skill

Every snippet here was checked against the real source (`mcp/`, `transport/`, `auth/`,
`examples/`) as of `v0.2.0`, not written from memory of "what an MCP library usually looks like."
If you extend this skill, do the same — read the actual `.go` files before writing new examples.
