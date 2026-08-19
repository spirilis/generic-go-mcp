package compat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/examples/tools"
	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

// stdioLikeWriter deliberately implements only transport.ResponseWriter, not
// transport.HeaderSetter, even though the *transport.BufferedResponseWriter it wraps does
// (BufferedResponseWriter satisfies HeaderSetter so tests can observe an HTTP-like minted
// session). Wrapping without embedding is what hides that promoted method, letting tests
// exercise the stdio/UNIX code path — no header layer at all — without standing up a real
// stdio connection.
type stdioLikeWriter struct {
	inner *transport.BufferedResponseWriter
}

func newStdioLikeWriter() *stdioLikeWriter {
	return &stdioLikeWriter{inner: transport.NewBufferedResponseWriter()}
}

func (w *stdioLikeWriter) WriteNotification(method string, params interface{}) error {
	return w.inner.WriteNotification(method, params)
}

func (w *stdioLikeWriter) WriteMessage(data []byte) error {
	return w.inner.WriteMessage(data)
}

var _ transport.ResponseWriter = (*stdioLikeWriter)(nil)

// newTestOverlay builds an Overlay over a real *mcp.Server carrying two tools (date,
// confirm_delete — the MRTR reference tool) and one resource, with AdvertisedVersions
// widened to include every legacy revision this overlay serves.
func newTestOverlay(t *testing.T, cfg Config) (*Overlay, *mcp.ToolRegistry, *mcp.ResourceRegistry) {
	t.Helper()

	registry := mcp.NewToolRegistry()
	registry.Register(tools.GetDateToolDefinition(), tools.DateTool)
	registry.Register(tools.GetConfirmToolDefinition(), tools.ConfirmTool)

	resourceRegistry := mcp.NewResourceRegistry()
	resourceRegistry.Register(mcp.Resource{
		URI:      "test:///hello",
		Name:     "hello",
		MimeType: "text/plain",
	}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
		return mcp.ResourceContentResult{Text: "hello"}, nil
	})

	server := mcp.NewServer(registry, resourceRegistry, &mcp.ServerConfig{
		Name:               "test-server",
		Version:            "0.0.1",
		AdvertisedVersions: append([]string{mcp.ProtocolVersion}, LegacyVersions...),
	})

	return New(server, cfg), registry, resourceRegistry
}

func mustMarshal(t *testing.T, id json.RawMessage, method string, params interface{}) []byte {
	t.Helper()
	p, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := transport.JSONRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: p}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return out
}

func legacyInitParams(protocolVersion string, caps map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"protocolVersion": protocolVersion,
		"capabilities":    caps,
		"clientInfo":      map[string]string{"name": "test-client", "version": "0.0.1"},
	}
}

func modernMetaParams(fields map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"_meta": map[string]interface{}{
			"io.modelcontextprotocol/protocolVersion":    mcp.ProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]interface{}{},
		},
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func decodeResult(t *testing.T, data []byte, v interface{}) {
	t.Helper()
	var env struct {
		Result json.RawMessage     `json:"result"`
		Error  *transport.RPCError `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, data)
	}
	if env.Error != nil {
		t.Fatalf("unexpected error response: %+v", env.Error)
	}
	if v != nil {
		if err := json.Unmarshal(env.Result, v); err != nil {
			t.Fatalf("decode result: %v", err)
		}
	}
}

func decodeError(t *testing.T, data []byte) *transport.RPCError {
	t.Helper()
	var env struct {
		Error *transport.RPCError `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, data)
	}
	return env.Error
}

func TestLegacyHandshakeVersionNegotiation(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		want      string
	}{
		{"known version echoed", "2025-11-25", "2025-11-25"},
		{"older known version echoed", "2024-11-05", "2024-11-05"},
		{"unknown version falls back to newest", "2099-01-01", LegacyVersions[0]},
		{"missing version falls back to newest", "", LegacyVersions[0]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, _, _ := newTestOverlay(t, Config{})
			bw := transport.NewBufferedResponseWriter()
			body := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams(tc.requested, map[string]interface{}{}))
			o.HandleMessage(context.Background(), body, bw)

			var result legacyInitializeResult
			decodeResult(t, bw.Message(), &result)
			if result.ProtocolVersion != tc.want {
				t.Errorf("protocolVersion = %q, want %q", result.ProtocolVersion, tc.want)
			}
		})
	}
}

func TestLegacyHandshakeResultEnvelope(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{ServerInfo: mcp.Implementation{Name: "fallback", Version: "9.9.9"}})
	bw := transport.NewBufferedResponseWriter()
	body := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{}))
	o.HandleMessage(context.Background(), body, bw)

	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(bw.Message(), &env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(env.Result, &fields); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	for _, forbidden := range []string{"resultType", "ttlMs", "cacheScope"} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("legacy initialize result unexpectedly carries %q", forbidden)
		}
	}

	var result legacyInitializeResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode into legacyInitializeResult: %v", err)
	}
	if result.ServerInfo.Name != "test-server" {
		t.Errorf("serverInfo.name = %q, want %q (derived from inner server/discover, not the Config.ServerInfo fallback)",
			result.ServerInfo.Name, "test-server")
	}
}

func TestInitializeAtModernVersionRejected(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	bw := transport.NewBufferedResponseWriter()
	body := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams(mcp.ProtocolVersion, map[string]interface{}{}))
	o.HandleMessage(context.Background(), body, bw)

	rerr := decodeError(t, bw.Message())
	if rerr == nil {
		t.Fatal("expected an error response")
	}
	if rerr.Code != transport.MethodNotFound {
		t.Errorf("code = %d, want %d", rerr.Code, transport.MethodNotFound)
	}
	if sid := bw.ResponseHeaders()[transport.SessionIDHeader]; sid != "" {
		t.Errorf("expected no session minted, got %s=%q", transport.SessionIDHeader, sid)
	}
}

func TestStdioImplicitSession(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	w := newStdioLikeWriter()
	body := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{}))
	o.HandleMessage(context.Background(), body, w)

	decodeResult(t, w.inner.Message(), nil)

	if !o.sessions.Known(stdioSessionKey) {
		t.Fatal("expected the implicit stdio session to be known under the reserved key")
	}
}

func TestSessionCapabilitiesVisibleMidCall(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	bw := transport.NewBufferedResponseWriter()
	body := mustMarshal(t, json.RawMessage("1"), "initialize",
		legacyInitParams("2025-11-25", map[string]interface{}{"elicitation": map[string]interface{}{}}))
	o.HandleMessage(context.Background(), body, bw)
	sessionID := bw.ResponseHeaders()[transport.SessionIDHeader]
	if sessionID == "" {
		t.Fatal("expected a minted session id")
	}

	ctx := transport.WithRequestInfo(context.Background(), &transport.RequestInfo{SessionID: sessionID})
	callBW := transport.NewBufferedResponseWriter()
	callBody := mustMarshal(t, json.RawMessage("2"), "tools/call", map[string]interface{}{
		"name":      "confirm_delete",
		"arguments": map[string]interface{}{"count": 3},
	})
	o.HandleMessage(ctx, callBody, callBW)

	var result legacyToolCallResult
	decodeResult(t, callBW.Message(), &result)
	if !result.IsError {
		t.Fatal("expected isError:true (the MRTR refusal), got a normal result")
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, mcp.ProtocolVersion) {
		t.Errorf("expected the MRTR refusal to name %s, got %+v", mcp.ProtocolVersion, result.Content)
	}
	if raw := string(callBW.Message()); strings.Contains(raw, "inputRequests") || strings.Contains(raw, "requestState") {
		t.Errorf("legacy response leaked MRTR fields: %s", raw)
	}
}

func TestConfirmDeleteWithoutElicitationCapability(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	bw := transport.NewBufferedResponseWriter()
	body := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{}))
	o.HandleMessage(context.Background(), body, bw)
	sessionID := bw.ResponseHeaders()[transport.SessionIDHeader]

	ctx := transport.WithRequestInfo(context.Background(), &transport.RequestInfo{SessionID: sessionID})
	callBW := transport.NewBufferedResponseWriter()
	callBody := mustMarshal(t, json.RawMessage("2"), "tools/call", map[string]interface{}{
		"name":      "confirm_delete",
		"arguments": map[string]interface{}{"count": 3},
	})
	o.HandleMessage(ctx, callBody, callBW)

	var result legacyToolCallResult
	decodeResult(t, callBW.Message(), &result)
	if !result.IsError {
		t.Fatal("expected isError:true")
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "elicitation") {
		t.Errorf("expected the missing-elicitation-capability error, got %+v", result.Content)
	}
}

func TestNoSessionServedWithNoCapabilities(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	ctx := transport.WithRequestInfo(context.Background(), &transport.RequestInfo{SessionID: "does-not-exist"})
	bw := transport.NewBufferedResponseWriter()
	body := mustMarshal(t, json.RawMessage("1"), "tools/list", map[string]interface{}{})
	o.HandleMessage(ctx, body, bw)

	var result struct {
		Tools []mcp.Tool `json:"tools"`
	}
	decodeResult(t, bw.Message(), &result)
	if len(result.Tools) == 0 {
		t.Fatal("expected tools/list to still succeed against an unknown session, not be refused")
	}
}

func TestSessionTTLExpiry(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{SessionTTL: 10 * time.Millisecond})
	bw := transport.NewBufferedResponseWriter()
	body := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{}))
	o.HandleMessage(context.Background(), body, bw)
	sessionID := bw.ResponseHeaders()[transport.SessionIDHeader]

	ls := o.LegacySessions()
	if !ls.Known(sessionID) {
		t.Fatal("expected a freshly minted session to be known")
	}

	time.Sleep(30 * time.Millisecond)
	if ls.Known(sessionID) {
		t.Error("expected the session to have expired past its idle TTL")
	}
	if ls.Terminate(sessionID) {
		t.Error("expected Terminate on an already-expired session to report false")
	}
}

func TestTerminateIdempotent(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	bw := transport.NewBufferedResponseWriter()
	body := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{}))
	o.HandleMessage(context.Background(), body, bw)
	sessionID := bw.ResponseHeaders()[transport.SessionIDHeader]

	ls := o.LegacySessions()
	if !ls.Terminate(sessionID) {
		t.Fatal("expected the first Terminate to succeed")
	}
	if ls.Terminate(sessionID) {
		t.Error("expected a second Terminate on the same id to report false")
	}
}

func TestReHandshakeReplacesSession(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	w := newStdioLikeWriter()

	body1 := mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{}))
	o.HandleMessage(context.Background(), body1, w)

	o.sessions.mu.Lock()
	first := o.sessions.byID[stdioSessionKey]
	o.sessions.mu.Unlock()
	if first == nil {
		t.Fatal("expected an implicit session after the first handshake")
	}

	body2 := mustMarshal(t, json.RawMessage("2"), "initialize", legacyInitParams("2024-11-05", map[string]interface{}{}))
	o.HandleMessage(context.Background(), body2, w)

	select {
	case <-first.done:
	default:
		t.Error("expected the replaced session's done channel to be closed")
	}

	o.sessions.mu.Lock()
	count := len(o.sessions.byID)
	second := o.sessions.byID[stdioSessionKey]
	o.sessions.mu.Unlock()
	if count != 1 {
		t.Errorf("expected exactly one session after re-handshake, found %d", count)
	}
	if second == first {
		t.Error("expected re-handshake to install a new session, not reuse the old one")
	}
}

func TestModernAndLegacyToolsListParity(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})

	modernBW := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("1"), "tools/list", modernMetaParams(nil)), modernBW)

	var modernEnv struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(modernBW.Message(), &modernEnv); err != nil {
		t.Fatalf("decode modern response: %v", err)
	}
	var modernFields map[string]json.RawMessage
	if err := json.Unmarshal(modernEnv.Result, &modernFields); err != nil {
		t.Fatalf("decode modern result: %v", err)
	}
	for _, want := range []string{"resultType", "ttlMs", "cacheScope"} {
		if _, ok := modernFields[want]; !ok {
			t.Errorf("modern tools/list result missing %q", want)
		}
	}

	legacyBW := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("2"), "tools/list", map[string]interface{}{}), legacyBW)

	var legacyEnv struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(legacyBW.Message(), &legacyEnv); err != nil {
		t.Fatalf("decode legacy response: %v", err)
	}
	var legacyFields map[string]json.RawMessage
	if err := json.Unmarshal(legacyEnv.Result, &legacyFields); err != nil {
		t.Fatalf("decode legacy result: %v", err)
	}
	for _, forbidden := range []string{"resultType", "ttlMs", "cacheScope"} {
		if _, ok := legacyFields[forbidden]; ok {
			t.Errorf("legacy tools/list result unexpectedly carries %q", forbidden)
		}
	}

	var modernList, legacyList struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(modernEnv.Result, &modernList); err != nil {
		t.Fatalf("decode modern tools: %v", err)
	}
	if err := json.Unmarshal(legacyEnv.Result, &legacyList); err != nil {
		t.Fatalf("decode legacy tools: %v", err)
	}
	if len(modernList.Tools) != len(legacyList.Tools) || len(modernList.Tools) == 0 {
		t.Fatalf("modern has %d tools, legacy has %d", len(modernList.Tools), len(legacyList.Tools))
	}
	for i := range modernList.Tools {
		if modernList.Tools[i].Name != legacyList.Tools[i].Name {
			t.Errorf("tool order mismatch at %d: modern=%q legacy=%q", i, modernList.Tools[i].Name, legacyList.Tools[i].Name)
		}
	}
}

func TestIsErrorInBothEras(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	args := map[string]interface{}{"timezone": "Not/A/Real/Zone"}

	modernBW := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("1"), "tools/call", modernMetaParams(map[string]interface{}{
			"name": "date", "arguments": args,
		})), modernBW)
	var modernResult struct {
		IsError bool `json:"isError"`
	}
	decodeResult(t, modernBW.Message(), &modernResult)
	if !modernResult.IsError {
		t.Error("expected modern isError:true for a bad timezone")
	}

	legacyBW := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("2"), "tools/call", map[string]interface{}{
			"name": "date", "arguments": args,
		}), legacyBW)
	var legacyResult legacyToolCallResult
	decodeResult(t, legacyBW.Message(), &legacyResult)
	if !legacyResult.IsError {
		t.Error("expected legacy isError:true for a bad timezone")
	}
}

func TestAdvertisedVersionsConsistency(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	want := append([]string{mcp.ProtocolVersion}, LegacyVersions...)

	discoverBW := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("1"), "server/discover", modernMetaParams(nil)), discoverBW)
	var discoverResult struct {
		SupportedVersions []string `json:"supportedVersions"`
	}
	decodeResult(t, discoverBW.Message(), &discoverResult)
	if !slices.Equal(discoverResult.SupportedVersions, want) {
		t.Errorf("server/discover supportedVersions = %v, want %v", discoverResult.SupportedVersions, want)
	}

	badBW := transport.NewBufferedResponseWriter()
	badParams := map[string]interface{}{
		"_meta": map[string]interface{}{
			"io.modelcontextprotocol/protocolVersion":    "9999-01-01",
			"io.modelcontextprotocol/clientCapabilities": map[string]interface{}{},
		},
	}
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("2"), "tools/list", badParams), badBW)
	rerr := decodeError(t, badBW.Message())
	if rerr == nil || rerr.Code != transport.UnsupportedProtocolVersion {
		t.Fatalf("expected -32022, got %+v", rerr)
	}
	var data struct {
		Supported []string `json:"supported"`
	}
	dataBytes, err := json.Marshal(rerr.Data)
	if err != nil {
		t.Fatalf("marshal error data: %v", err)
	}
	if err := json.Unmarshal(dataBytes, &data); err != nil {
		t.Fatalf("decode error data: %v", err)
	}
	if !slices.Equal(data.Supported, want) {
		t.Errorf("-32022 data.supported = %v, want %v", data.Supported, want)
	}
}

func TestResourcesReadAndSubscribeRefusal(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})

	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{})), bw)
	var initResult legacyInitializeResult
	decodeResult(t, bw.Message(), &initResult)
	if initResult.Capabilities.Resources == nil {
		t.Fatal("expected a resources capability, since the test registry has one resource")
	}
	if initResult.Capabilities.Resources.Subscribe {
		t.Error("expected resources.subscribe to be forced false")
	}

	sessionID := bw.ResponseHeaders()[transport.SessionIDHeader]
	ctx := transport.WithRequestInfo(context.Background(), &transport.RequestInfo{SessionID: sessionID})

	readBW := transport.NewBufferedResponseWriter()
	o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("2"), "resources/read", map[string]interface{}{"uri": "test:///hello"}), readBW)
	var readResult struct {
		Contents []mcp.ResourceContent `json:"contents"`
	}
	decodeResult(t, readBW.Message(), &readResult)
	if len(readResult.Contents) != 1 || readResult.Contents[0].Text != "hello" {
		t.Errorf("unexpected resources/read result: %+v", readResult)
	}

	subBW := transport.NewBufferedResponseWriter()
	o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("3"), "resources/subscribe", map[string]interface{}{"uri": "test:///hello"}), subBW)
	rerr := decodeError(t, subBW.Message())
	if rerr == nil || rerr.Code != transport.MethodNotFound {
		t.Errorf("expected -32601 for resources/subscribe, got %+v", rerr)
	}
}

func TestLegacyPing(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(), mustMarshal(t, json.RawMessage("1"), "ping", map[string]interface{}{}), bw)

	var env struct {
		Result json.RawMessage     `json:"result"`
		Error  *transport.RPCError `json:"error"`
	}
	if err := json.Unmarshal(bw.Message(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if string(env.Result) != "{}" {
		t.Errorf("ping result = %s, want {}", env.Result)
	}
}

func TestUnknownLegacyMethodRejected(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(), mustMarshal(t, json.RawMessage("1"), "logging/setLevel", map[string]interface{}{}), bw)

	rerr := decodeError(t, bw.Message())
	if rerr == nil || rerr.Code != transport.MethodNotFound {
		t.Errorf("expected -32601, got %+v", rerr)
	}
}

func TestNotificationsInitializedTouchesSession(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{SessionTTL: 20 * time.Millisecond})
	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{})), bw)
	sessionID := bw.ResponseHeaders()[transport.SessionIDHeader]

	time.Sleep(12 * time.Millisecond)
	notifBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	ctx := transport.WithRequestInfo(context.Background(), &transport.RequestInfo{SessionID: sessionID})
	o.HandleMessage(ctx, notifBody, transport.NewBufferedResponseWriter())

	time.Sleep(12 * time.Millisecond)
	if !o.LegacySessions().Known(sessionID) {
		t.Error("expected notifications/initialized to have extended the session's idle TTL")
	}
}

// --- resource templates and completion over the legacy overlay ---

// registerTestTemplate adds a template family to an overlay's resource registry, plus a
// completion provider for its one variable.
func registerTestTemplate(t *testing.T, resources *mcp.ResourceRegistry) {
	t.Helper()
	if err := resources.RegisterTemplate(mcp.ResourceTemplate{
		URITemplate: "test:///env/{name}",
		Name:        "Environment variable",
		Description: "One environment variable by name",
		MimeType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		return mcp.ResourceContentResult{Text: "value-of-" + req.Vars["name"]}, nil
	}); err != nil {
		t.Fatalf("register template: %v", err)
	}
	if err := resources.SetTemplateCompleter("test:///env/{name}", mcp.CompletionFunc(
		func(ctx context.Context, req *mcp.CompletionRequest) (mcp.CompletionResult, error) {
			return mcp.CompletionResult{Values: []string{"HOME", "PATH"}}, nil
		})); err != nil {
		t.Fatalf("SetTemplateCompleter: %v", err)
	}
}

// legacySessionContext runs the legacy initialize handshake and returns a context carrying
// the resulting session id, the way every legacy request after the handshake arrives.
func legacySessionContext(t *testing.T, o *Overlay) context.Context {
	t.Helper()
	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{})), bw)
	var initResult legacyInitializeResult
	decodeResult(t, bw.Message(), &initResult)
	return transport.WithRequestInfo(context.Background(),
		&transport.RequestInfo{SessionID: bw.ResponseHeaders()[transport.SessionIDHeader]})
}

// assertDowngraded fails if a legacy result still carries any 2026-07-28 envelope field.
func assertDowngraded(t *testing.T, data []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	decodeResult(t, data, &fields)
	for _, k := range []string{"resultType", "ttlMs", "cacheScope", "_meta"} {
		if _, present := fields[k]; present {
			t.Errorf("legacy result still carries modern envelope field %q: %s", k, data)
		}
	}
}

func TestLegacyResourcesTemplatesList(t *testing.T) {
	o, _, resources := newTestOverlay(t, Config{})
	registerTestTemplate(t, resources)
	ctx := legacySessionContext(t, o)

	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("2"), "resources/templates/list", map[string]interface{}{}), bw)

	var result struct {
		ResourceTemplates []mcp.ResourceTemplate `json:"resourceTemplates"`
	}
	decodeResult(t, bw.Message(), &result)
	if len(result.ResourceTemplates) != 1 {
		t.Fatalf("got %d templates, want 1", len(result.ResourceTemplates))
	}
	if result.ResourceTemplates[0].URITemplate != "test:///env/{name}" {
		t.Errorf("uriTemplate = %q, want test:///env/{name}", result.ResourceTemplates[0].URITemplate)
	}
	assertDowngraded(t, bw.Message())
}

// Templates are matched on the ordinary resources/read path, which the overlay already
// forwards — so a legacy client reads a template member with no extra machinery.
func TestLegacyResourcesReadThroughTemplate(t *testing.T) {
	o, _, resources := newTestOverlay(t, Config{})
	registerTestTemplate(t, resources)
	ctx := legacySessionContext(t, o)

	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("2"), "resources/read",
		map[string]interface{}{"uri": "test:///env/HOME"}), bw)

	var result struct {
		Contents []mcp.ResourceContent `json:"contents"`
	}
	decodeResult(t, bw.Message(), &result)
	if len(result.Contents) != 1 || result.Contents[0].Text != "value-of-HOME" {
		t.Fatalf("unexpected resources/read result: %+v", result)
	}
	if result.Contents[0].URI != "test:///env/HOME" {
		t.Errorf("uri = %q, want the concrete request URI", result.Contents[0].URI)
	}
	assertDowngraded(t, bw.Message())
}

// The modern -32602 reaches the legacy client unchanged. Earlier revisions used -32002 for
// this, but that code is retired library-wide and the overlay deliberately does not
// translate error codes. See LEGACY-COMPAT.md's known limitations.
func TestLegacyUnknownResourceKeepsModernErrorCode(t *testing.T) {
	o, _, resources := newTestOverlay(t, Config{})
	registerTestTemplate(t, resources)
	ctx := legacySessionContext(t, o)

	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("2"), "resources/read",
		map[string]interface{}{"uri": "test:///nothing/at/all"}), bw)

	rerr := decodeError(t, bw.Message())
	if rerr == nil || rerr.Code != transport.InvalidParams {
		t.Errorf("got %+v, want -32602", rerr)
	}
}

func TestLegacyCompletionComplete(t *testing.T) {
	o, _, resources := newTestOverlay(t, Config{})
	registerTestTemplate(t, resources)
	ctx := legacySessionContext(t, o)

	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("2"), "completion/complete", map[string]interface{}{
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": ""},
	}), bw)

	var result struct {
		Completion struct {
			Values []string `json:"values"`
		} `json:"completion"`
	}
	decodeResult(t, bw.Message(), &result)
	if !slices.Equal(result.Completion.Values, []string{"HOME", "PATH"}) {
		t.Errorf("values = %v, want [HOME PATH]", result.Completion.Values)
	}
	assertDowngraded(t, bw.Message())
}

// With no provider registered anywhere, the inner server's -32601 passes straight through —
// the same "unsupported" signal a legacy client probes for.
func TestLegacyCompletionUnsupportedWithoutProvider(t *testing.T) {
	o, _, _ := newTestOverlay(t, Config{})
	ctx := legacySessionContext(t, o)

	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("2"), "completion/complete", map[string]interface{}{
		"ref":      map[string]interface{}{"type": "ref/resource", "uri": "test:///env/{name}"},
		"argument": map[string]interface{}{"name": "name", "value": ""},
	}), bw)

	rerr := decodeError(t, bw.Message())
	if rerr == nil || rerr.Code != transport.MethodNotFound {
		t.Errorf("got %+v, want -32601", rerr)
	}
}

// A registry holding only templates must still declare resources in the legacy handshake,
// or a legacy client concludes the server serves no resources and never asks.
func TestLegacyHandshakeDeclaresResourcesForTemplatesOnlyRegistry(t *testing.T) {
	registry := mcp.NewToolRegistry()
	resources := mcp.NewResourceRegistry()
	if err := resources.RegisterTemplate(mcp.ResourceTemplate{URITemplate: "test:///{a}", Name: "x"},
		func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
			return mcp.ResourceContentResult{Text: "x"}, nil
		}); err != nil {
		t.Fatalf("register template: %v", err)
	}
	server := mcp.NewServer(registry, resources, &mcp.ServerConfig{
		Name:               "test-server",
		Version:            "0.0.1",
		AdvertisedVersions: append([]string{mcp.ProtocolVersion}, LegacyVersions...),
	})
	o := New(server, Config{})

	bw := transport.NewBufferedResponseWriter()
	o.HandleMessage(context.Background(),
		mustMarshal(t, json.RawMessage("1"), "initialize", legacyInitParams("2025-11-25", map[string]interface{}{})), bw)

	var initResult legacyInitializeResult
	decodeResult(t, bw.Message(), &initResult)
	if initResult.Capabilities.Resources == nil {
		t.Error("a templates-only registry did not declare the resources capability to a legacy client")
	}
}
