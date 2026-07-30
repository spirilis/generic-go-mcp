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

// ServerCapabilities describes what this server supports. Tools/Resources/Prompts are
// only present when the corresponding registry actually has entries to serve — an empty
// registry declares no capability for it, rather than an always-present empty object.
type ServerCapabilities struct {
	Tools      *ToolsCapability           `json:"tools,omitempty"`
	Resources  *ResourcesCapability       `json:"resources,omitempty"`
	Prompts    *PromptsCapability         `json:"prompts,omitempty"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}
