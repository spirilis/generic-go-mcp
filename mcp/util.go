package mcp

import "encoding/json"

// stringFromRaw unmarshals raw as a JSON string, returning ok=false if raw is empty or
// not a valid JSON string. Used when peeking at optional fields inside a larger raw
// object (e.g. a single key of a params._meta map) without requiring the whole object to
// decode into a concrete struct up front.
func stringFromRaw(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
