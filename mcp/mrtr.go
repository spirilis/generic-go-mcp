package mcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// requestStateTTL bounds how long a signed requestState blob remains valid. Per the
// spec's security guidance, servers SHOULD include a short expiry inside the
// integrity-protected payload.
const requestStateTTL = 5 * time.Minute

// InputRequests is a map of server-assigned identifiers to server-to-client requests
// (elicitation/create, sampling/createMessage, or roots/list), carried in an
// InputRequiredResult. Build entries with NewElicitRequest et al. rather than
// constructing the raw JSON by hand.
type InputRequests map[string]json.RawMessage

// InputResponses is the client's answers to a previous InputRequests, keyed by the same
// identifiers, carried back on the retry of the original request.
type InputResponses map[string]json.RawMessage

// ElicitRequestParams is the params object of an elicitation/create MRTR input request.
type ElicitRequestParams struct {
	Mode            string          `json:"mode,omitempty"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
}

type elicitRequest struct {
	Method string              `json:"method"`
	Params ElicitRequestParams `json:"params"`
}

// NewElicitRequest builds an InputRequests entry asking the client to elicit information
// from the user via mode ("form" is the common case), with schema describing the shape
// of the expected answer.
func NewElicitRequest(mode, message string, schema json.RawMessage) json.RawMessage {
	raw, _ := json.Marshal(elicitRequest{
		Method: "elicitation/create",
		Params: ElicitRequestParams{Mode: mode, Message: message, RequestedSchema: schema},
	})
	return raw
}

// ElicitResult is the client's response to an elicitation/create input request.
type ElicitResult struct {
	Action  string          `json:"action"` // "accept" | "decline" | "cancel"
	Content json.RawMessage `json:"content,omitempty"`
}

// Accepted reports whether the user accepted the elicitation (as opposed to declining or
// cancelling it).
func (e *ElicitResult) Accepted() bool {
	return e != nil && e.Action == "accept"
}

// InputRequiredResult is returned instead of a normal result when the server needs more
// information from the client before it can complete the request (Multi Round-Trip
// Requests, MRTR). The client is expected to fulfill inputRequests and retry the
// identical request (new JSON-RPC id) with inputResponses and requestState attached.
type InputRequiredResult struct {
	BaseResult
	InputRequests InputRequests `json:"inputRequests,omitempty"`
	RequestState  string        `json:"requestState,omitempty"`
}

func newInputRequiredResult(reqs InputRequests, state string) *InputRequiredResult {
	r := &InputRequiredResult{InputRequests: reqs, RequestState: state}
	r.BaseResult.ResultType = ResultTypeInputRequired
	return r
}

// requestStatePayload is the signed content of a requestState blob: the caller identity
// it was issued to, an expiry, and a digest binding it to the exact tool call it was
// issued for, so it cannot be replayed against a different call or a different caller.
type requestStatePayload struct {
	Principal string `json:"p"`
	Expiry    int64  `json:"exp"`
	Digest    string `json:"d"`
}

func digestMethodArgs(name string, arguments json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(arguments)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// signRequestState signs an opaque requestState blob for a call to tool name with
// arguments, bound to principal and expiring after requestStateTTL.
func (s *Server) signRequestState(principal, name string, arguments json.RawMessage) string {
	payload := requestStatePayload{
		Principal: principal,
		Expiry:    time.Now().Add(requestStateTTL).Unix(),
		Digest:    digestMethodArgs(name, arguments),
	}
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, s.config.RequestStateKey)
	mac.Write(body)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// verifyRequestState checks a requestState blob echoed back on a retry: signature,
// expiry, calling principal, and that it was issued for this exact tool call.
// requestState is attacker-controlled (it passes through the client), so every check
// here matters — a tampered, expired, cross-principal, or cross-request blob is
// rejected rather than trusted.
func (s *Server) verifyRequestState(state, principal, name string, arguments json.RawMessage) error {
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("malformed requestState")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("malformed requestState")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("malformed requestState")
	}
	mac := hmac.New(sha256.New, s.config.RequestStateKey)
	mac.Write(body)
	expected := mac.Sum(nil)
	if !hmac.Equal(sig, expected) {
		return fmt.Errorf("requestState signature invalid")
	}
	var payload requestStatePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("malformed requestState")
	}
	if time.Now().Unix() > payload.Expiry {
		return fmt.Errorf("requestState expired")
	}
	if payload.Principal != principal {
		return fmt.Errorf("requestState principal mismatch")
	}
	if payload.Digest != digestMethodArgs(name, arguments) {
		return fmt.Errorf("requestState does not match the originating request")
	}
	return nil
}

// missingCapabilities scans reqs for any server-to-client method the caller's declared
// ClientCapabilities does not cover, returning the missing capability names (sorted, for
// deterministic error output). A server MUST NOT send an inputRequests entry the client
// hasn't declared support for.
func missingCapabilitiesFor(reqs InputRequests, caps *ClientCapabilities) []string {
	needed := map[string]bool{}
	for _, raw := range reqs {
		var probe struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue
		}
		switch probe.Method {
		case "elicitation/create":
			if !caps.HasElicitation() {
				needed["elicitation"] = true
			}
		case "sampling/createMessage":
			if !caps.HasSampling() {
				needed["sampling"] = true
			}
		case "roots/list":
			if !caps.HasRoots() {
				needed["roots"] = true
			}
		}
	}
	if len(needed) == 0 {
		return nil
	}
	out := make([]string, 0, len(needed))
	for k := range needed {
		out = append(out, k)
	}
	// Small, fixed universe of capability names — sort without importing "sort" for one
	// three-element case.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
