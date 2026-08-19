package mcp

import "encoding/json"

// ToolsCapability, ResourcesCapability, PromptsCapability, and ServerCapabilities mirror
// the server-side capability objects returned from server/discover.

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// CompletionsCapability declares that the server can answer completion/complete. It has no
// fields — the capability's presence is the whole signal. This server declares it only when
// an embedder has attached a CompletionProvider to at least one resource template.
type CompletionsCapability struct{}

// ServerCapabilities describes what this server supports. Tools/Resources/Prompts are
// only present when the corresponding registry actually has entries to serve — an empty
// registry declares no capability for it, rather than an always-present empty object.
type ServerCapabilities struct {
	Tools       *ToolsCapability           `json:"tools,omitempty"`
	Resources   *ResourcesCapability       `json:"resources,omitempty"`
	Prompts     *PromptsCapability         `json:"prompts,omitempty"`
	Completions *CompletionsCapability     `json:"completions,omitempty"`
	Extensions  map[string]json.RawMessage `json:"extensions,omitempty"`
}
