package compat

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

// Config configures an Overlay.
type Config struct {
	// SessionTTL bounds how long an idle legacy session survives without any request
	// touching it. Zero (the default) uses a 30-minute TTL.
	SessionTTL time.Duration

	// ServerInfo is reported as serverInfo in the legacy initialize result whenever the
	// inner handler's server/discover response doesn't carry one under
	// _meta."io.modelcontextprotocol/serverInfo" — every mcp.Server response does, so this
	// is a fallback for a hand-rolled inner transport.MessageHandler, not the common case.
	ServerInfo mcp.Implementation
}

// Overlay is a legacy-compatibility layer: it implements transport.MessageHandler,
// wrapping an inner handler (typically an *mcp.Server) that implements the modern
// (2026-07-28) protocol. A modern request passes straight through to inner untouched (see
// Claims); a legacy (2025-11-25 and earlier) request is answered locally (initialize,
// ping, notifications/initialized) or translated into the modern wire shape, forwarded to
// inner, and downgraded back on the way out (tools/list, tools/call, resources/list,
// resources/read). Everything else a legacy client sends gets -32601, same as it would
// from a capability the handshake never declared.
type Overlay struct {
	inner    transport.MessageHandler
	sessions *sessionStore
	cfg      Config
}

// syntheticRequestID is the JSON-RPC id used on every request this overlay synthesizes
// against inner. It is never seen by a real client: the overlay always substitutes the
// legacy client's own request id back in before writing the (downgraded) response.
var syntheticRequestID = json.RawMessage("1")

// New wraps inner with a legacy-compatibility overlay. See the Overlay doc comment.
func New(inner transport.MessageHandler, cfg Config) *Overlay {
	o := &Overlay{inner: inner, cfg: cfg}
	o.sessions = newSessionStore(inner, cfg.SessionTTL)
	return o
}

// LegacySessions returns o's session store as a transport.LegacySessions, for
// HTTPTransportConfig.LegacySessions. Returns a genuinely nil interface (not a non-nil
// interface holding a nil pointer) when o itself is nil, so a caller that forgot to
// construct an Overlay gets the same "compatibility off" behavior as never wiring one up.
func (o *Overlay) LegacySessions() transport.LegacySessions {
	if o == nil || o.sessions == nil {
		return nil
	}
	return o.sessions
}

// HandleMessage implements transport.MessageHandler.
func (o *Overlay) HandleMessage(ctx context.Context, data []byte, w transport.ResponseWriter) {
	var req transport.JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		// Let inner produce the standard parse-error response — it does the exact same
		// unmarshal-and-report-ParseError work; no need to duplicate it here.
		o.inner.HandleMessage(ctx, data, w)
		return
	}

	if !Claims(req.Method, req.Params) {
		// Modern request: zero interference, straight through to the native handler.
		o.inner.HandleMessage(ctx, data, w)
		return
	}

	if req.IsNotification() {
		if req.Method == "notifications/initialized" {
			// Required after a successful legacy handshake; carries no useful payload
			// and, being a notification, expects no response. Just extends the
			// session's idle TTL.
			o.sessions.touch(o.sessionIDFromContext(ctx))
		}
		// No other legacy client-to-server notification is acted on.
		return
	}

	switch req.Method {
	case "initialize":
		o.handleInitialize(ctx, req, data, w)
	case "ping":
		// Mandatory in these revisions: answer with an empty result.
		w.WriteMessage(transport.NewSuccessResponse(req.ID, struct{}{}))
	case "tools/list", "tools/call", "resources/list", "resources/read",
		"resources/templates/list", "completion/complete",
		"skills/list", "skills/get", "resources/directory/read":
		o.forward(ctx, req, w)
	default:
		// Everything else — including resources/subscribe and resources/unsubscribe,
		// consistent with this overlay always declaring resources.subscribe: false — is
		// the correct -32601 answer for a capability the handshake never declared. Do
		// not stub a removed/unimplemented method to return {} because some client
		// calls it unconditionally.
		w.WriteMessage(transport.NewErrorResponse(req.ID, &transport.RPCError{
			Code:    transport.MethodNotFound,
			Message: "Method not found",
		}))
	}
}

// legacyInitializeParams is the params shape of a legacy "initialize" request.
type legacyInitializeParams struct {
	ProtocolVersion string                  `json:"protocolVersion"`
	Capabilities    *mcp.ClientCapabilities `json:"capabilities"`
	ClientInfo      *mcp.Implementation     `json:"clientInfo"`
}

// legacyInitializeResult is the wire shape of a legacy "initialize" result.
type legacyInitializeResult struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    mcp.ServerCapabilities `json:"capabilities"`
	ServerInfo      mcp.Implementation     `json:"serverInfo"`
	Instructions    string                 `json:"instructions,omitempty"`
}

// handleInitialize answers the legacy handshake: negotiates a protocol version, mints a
// session (or claims stdio's implicit one), and derives capabilities/instructions/identity
// from the inner server's own server/discover rather than duplicating that logic. data is
// the exact bytes HandleMessage received, forwarded unchanged in the one case this
// shortcuts to the inner handler directly.
func (o *Overlay) handleInitialize(ctx context.Context, req transport.JSONRPCRequest, data []byte, w transport.ResponseWriter) {
	var p legacyInitializeParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}

	if p.ProtocolVersion == mcp.ProtocolVersion {
		// A client naming exactly the modern version at the legacy handshake method is a
		// contradiction — that revision has no handshake. Let the inner server answer
		// with its own -32601 diagnostic (already correctly worded and pointing at
		// server/discover) rather than duplicate it here, and mint no session: never
		// quietly downgrade a modern client into a legacy one.
		o.inner.HandleMessage(ctx, data, w)
		return
	}

	negotiated := NegotiateVersion(p.ProtocolVersion)
	caps := p.Capabilities
	if caps == nil {
		caps = &mcp.ClientCapabilities{}
	}

	sess := &legacySession{
		protocolVersion: negotiated,
		clientInfo:      p.ClientInfo,
		capabilities:    caps,
		createdAt:       time.Now(),
		done:            make(chan struct{}),
	}
	if hs, ok := w.(transport.HeaderSetter); ok {
		// A transport with response headers (Streamable HTTP): mint a real session id
		// and hand it back so the client can carry it on every later request.
		sess.id = newSessionID()
		o.sessions.create(sess)
		hs.SetResponseHeader(transport.SessionIDHeader, sess.id)
	} else {
		// stdio/UNIX: the connection is the process, so there is exactly one session,
		// under the reserved empty-string key. Re-handshaking replaces it — a
		// legitimate reset, not an error.
		sess.id = stdioSessionKey
		o.sessions.create(sess)
		o.startImplicitListener(sess, w)
	}

	discoverBody := buildModernRequest(syntheticRequestID, "server/discover", json.RawMessage(`{}`), caps, p.ClientInfo)
	bw := transport.NewBufferedResponseWriter()
	o.inner.HandleMessage(ctx, discoverBody, bw)

	result := o.buildInitializeResult(negotiated, bw.Message())
	w.WriteMessage(transport.NewSuccessResponse(req.ID, result))
}

// startImplicitListener starts the background goroutine that bridges the stdio/UNIX
// implicit session's list_changed notifications to the connection's shared writer. Unlike
// the HTTP GET stream (driven per-request by the Streamable HTTP binding), stdio has no
// separate request to hold this open: it must run for as long as the session lives, driven
// by the session's own Done channel rather than the initiating "initialize" request's
// context — that context is cancelled the moment handleInitialize returns, long before
// this goroutine's job is done.
//
// If the underlying connection ends without ever creating a replacement session (e.g. the
// client process died without a clean shutdown), this goroutine outlives it until the
// server process itself exits. That is consistent with this overlay's "no janitor
// goroutine, sweep opportunistically on create" design, and harmless: a dead process
// cannot leak beyond its own lifetime, and a live one still sweeps this session (and stops
// this goroutine) once it goes idle past its TTL.
func (o *Overlay) startImplicitListener(sess *legacySession, w transport.ResponseWriter) {
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-sess.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		_ = o.sessions.StreamNotifications(ctx, sess.id, w)
	}()
}

// buildInitializeResult derives a legacy initialize result from the inner server's raw
// server/discover response.
func (o *Overlay) buildInitializeResult(negotiated string, discoverResp []byte) legacyInitializeResult {
	var env struct {
		Result struct {
			Capabilities mcp.ServerCapabilities     `json:"capabilities"`
			Instructions string                     `json:"instructions,omitempty"`
			Meta         map[string]json.RawMessage `json:"_meta,omitempty"`
		} `json:"result"`
	}
	_ = json.Unmarshal(discoverResp, &env)

	caps := env.Result.Capabilities
	if caps.Resources != nil {
		// The native server never sets Subscribe, but say so explicitly rather than
		// rely on that: promising a capability this overlay does not serve
		// (resources/subscribe and resources/unsubscribe are both -32601 here) invites
		// a legacy client to wait forever for a notification that will never arrive.
		caps.Resources.Subscribe = false
	}

	var serverInfo mcp.Implementation
	if raw, ok := env.Result.Meta["io.modelcontextprotocol/serverInfo"]; ok {
		_ = json.Unmarshal(raw, &serverInfo)
	}
	if serverInfo.Name == "" {
		serverInfo = o.cfg.ServerInfo
	}

	return legacyInitializeResult{
		ProtocolVersion: negotiated,
		Capabilities:    caps,
		ServerInfo:      serverInfo,
		Instructions:    env.Result.Instructions,
	}
}

// forward translates a legacy tools/list, tools/call, resources/list, resources/read,
// resources/templates/list, completion/complete, skills/list, skills/get, or
// resources/directory/read request into the modern shape — all of these share an identical
// params object between eras, so only _meta needs adding — using the calling session's
// declared capabilities, sends it to the inner handler, and writes the downgraded result
// back to w under the legacy client's own request id.
//
// Resource templates, completion, and the skills extension are all era-neutral, so
// forwarding them is all that is needed: the inner server's answer is already the shape a
// legacy client expects once downgradeResult has stripped the modern envelope fields, and
// because that stripping does not descend into nested values, a skill's frontmatter and its
// per-file digests survive untouched. A server with no completion provider (or no skill
// registry) answers with -32601 through the inner handler, which passes straight through —
// the same "unsupported" signal a legacy client would probe for.
func (o *Overlay) forward(ctx context.Context, req transport.JSONRPCRequest, w transport.ResponseWriter) {
	sess, ok := o.sessions.get(o.sessionIDFromContext(ctx))

	var caps *mcp.ClientCapabilities
	var clientInfo *mcp.Implementation
	if ok {
		caps = sess.capabilities
		clientInfo = sess.clientInfo
	}
	if caps == nil {
		// A call with no live session — the handshake was skipped, or the session id
		// was dropped or has expired — is still served, with no capabilities declared:
		// the safe reading of "nothing was negotiated," not a refusal. The one case
		// that genuinely matters, an id naming a session that no longer exists, is
		// caught in the Streamable HTTP binding (transport/http.go), not here.
		caps = &mcp.ClientCapabilities{}
	}

	body := buildModernRequest(syntheticRequestID, req.Method, req.Params, caps, clientInfo)
	bw := transport.NewBufferedResponseWriter()
	o.inner.HandleMessage(ctx, body, bw)

	resp := bw.Message()
	if resp == nil {
		// None of these four methods are long-lived on the inner server; it always
		// calls WriteMessage. Getting here would mean the inner handler is broken.
		w.WriteMessage(transport.NewErrorResponse(req.ID, &transport.RPCError{
			Code:    transport.InternalError,
			Message: "no response from inner handler",
		}))
		return
	}
	w.WriteMessage(downgradeResponse(resp, req.ID))
}

// sessionIDFromContext reports which legacy session a request belongs to: the
// Mcp-Session-Id header on Streamable HTTP (via transport.RequestInfoFromContext), or
// stdio/UNIX's one reserved implicit-session key when there is no header layer at all.
// Since a single server process runs exactly one transport, these two cases never collide.
func (o *Overlay) sessionIDFromContext(ctx context.Context) string {
	if info := transport.RequestInfoFromContext(ctx); info != nil && info.SessionID != "" {
		return info.SessionID
	}
	return stdioSessionKey
}

// capabilitiesToMeta re-encodes caps as a plain map, field by field, rather than letting
// encoding/json marshal the *mcp.ClientCapabilities struct directly. This matters: a
// legacy client typically declares a capability as an empty object (e.g.
// {"elicitation": {}}) meaning only "I support this," and mcp.ClientCapabilities.
// HasElicitation() correctly reads that as caps.Elicitation being a non-nil (if
// zero-length) map. But Go's encoding/json omitempty treats ANY zero-length map as
// "empty" regardless of nilness — so marshaling the struct directly would silently drop
// that declared-but-empty field, and the inner server would see no elicitation capability
// at all. Building the map by hand, keyed on nil-ness rather than length, preserves it.
func capabilitiesToMeta(caps *mcp.ClientCapabilities) map[string]interface{} {
	out := map[string]interface{}{}
	if caps == nil {
		return out
	}
	if caps.Roots != nil {
		out["roots"] = caps.Roots
	}
	if caps.Sampling != nil {
		out["sampling"] = caps.Sampling
	}
	if caps.Elicitation != nil {
		out["elicitation"] = caps.Elicitation
	}
	if caps.Extensions != nil {
		out["extensions"] = caps.Extensions
	}
	return out
}

// buildMeta constructs the params._meta object every request into the inner modern
// handler must carry.
func buildMeta(protocolVersion string, caps *mcp.ClientCapabilities, clientInfo *mcp.Implementation) map[string]interface{} {
	meta := map[string]interface{}{
		"io.modelcontextprotocol/protocolVersion":    protocolVersion,
		"io.modelcontextprotocol/clientCapabilities": capabilitiesToMeta(caps),
	}
	if clientInfo != nil {
		meta["io.modelcontextprotocol/clientInfo"] = clientInfo
	}
	return meta
}

// mergeMeta returns params (a JSON object, or empty/nil treated as {}) with its _meta
// field set to meta, overwriting any _meta the caller already supplied.
func mergeMeta(params json.RawMessage, meta map[string]interface{}) json.RawMessage {
	var fields map[string]json.RawMessage
	if len(params) > 0 {
		_ = json.Unmarshal(params, &fields)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	metaRaw, _ := json.Marshal(meta)
	fields["_meta"] = metaRaw
	out, _ := json.Marshal(fields)
	return out
}

// buildModernRequest marshals a synthetic modern (2026-07-28) JSON-RPC request for method,
// with params plus the given capabilities/clientInfo merged into params._meta.
func buildModernRequest(id json.RawMessage, method string, params json.RawMessage, caps *mcp.ClientCapabilities, clientInfo *mcp.Implementation) []byte {
	meta := buildMeta(mcp.ProtocolVersion, caps, clientInfo)
	req := transport.JSONRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: mergeMeta(params, meta)}
	out, _ := json.Marshal(req)
	return out
}

var _ transport.MessageHandler = (*Overlay)(nil)
