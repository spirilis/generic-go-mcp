package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/transport"
)

type rpcErrorBody struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rpcResponseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcErrorBody   `json:"error"`
}

func newTestServer(t *testing.T) (*Server, *ToolRegistry, *ResourceRegistry) {
	t.Helper()
	registry := NewToolRegistry()
	registry.Register(Tool{
		Name:        "echo",
		Description: "echoes input",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}, func(ctx context.Context, req *ToolRequest) (Result, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = req.BindArguments(&args)
		return &ToolCallResult{Content: []Content{Text(args.Text)}}, nil
	})
	registry.Register(Tool{
		Name:        "fail",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(ctx context.Context, req *ToolRequest) (Result, error) {
		return ErrorResultf("intentional failure"), nil
	})
	registry.Register(Tool{
		Name:        "needs_input",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(ctx context.Context, req *ToolRequest) (Result, error) {
		if v, ok := req.ElicitResponse("x"); ok && v.Accepted() {
			return &ToolCallResult{Content: []Content{Text("done")}}, nil
		}
		return req.NeedInput(InputRequests{
			"x": NewElicitRequest("form", "give x", json.RawMessage(`{"type":"object"}`)),
		})
	})

	resources := NewResourceRegistry()
	resources.Register(Resource{URI: "test:///hello", Name: "hello", MimeType: "text/plain"},
		func(ctx context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "hello world"}, nil
		})

	srv := NewServer(registry, resources, &ServerConfig{Name: "test-server", Version: "0.0.0"})
	return srv, registry, resources
}

func metaObject(protocolVersion string, capabilities map[string]interface{}) map[string]interface{} {
	m := map[string]interface{}{}
	if protocolVersion != "" {
		m[metaKeyProtocolVersion] = protocolVersion
	}
	if capabilities != nil {
		m[metaKeyClientCapabilities] = capabilities
	}
	return m
}

func buildRequest(t *testing.T, id int, method string, params map[string]interface{}) []byte {
	t.Helper()
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return b
}

func call(t *testing.T, srv *Server, id int, method string, params map[string]interface{}) rpcResponseEnvelope {
	t.Helper()
	w := transport.NewBufferedResponseWriter()
	srv.HandleMessage(context.Background(), buildRequest(t, id, method, params), w)
	msg := w.Message()
	if msg == nil {
		t.Fatalf("HandleMessage for %s did not write a response", method)
	}
	var env rpcResponseEnvelope
	if err := json.Unmarshal(msg, &env); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, msg)
	}
	return env
}

func validMeta() map[string]interface{} {
	return metaObject(ProtocolVersion, map[string]interface{}{})
}

func TestDiscoverResultEnvelope(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "server/discover", map[string]interface{}{"_meta": validMeta()})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result struct {
		ResultType        string                 `json:"resultType"`
		SupportedVersions []string               `json:"supportedVersions"`
		TTLMs             int64                  `json:"ttlMs"`
		CacheScope        string                 `json:"cacheScope"`
		Meta              map[string]interface{} `json:"_meta"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.ResultType != ResultTypeComplete {
		t.Errorf("resultType = %q, want %q", result.ResultType, ResultTypeComplete)
	}
	if len(result.SupportedVersions) != 1 || result.SupportedVersions[0] != ProtocolVersion {
		t.Errorf("supportedVersions = %v, want [%q]", result.SupportedVersions, ProtocolVersion)
	}
	if result.TTLMs < 0 {
		t.Errorf("ttlMs = %d, want >= 0", result.TTLMs)
	}
	if result.CacheScope != CacheScopePublic && result.CacheScope != CacheScopePrivate {
		t.Errorf("cacheScope = %q, want public or private", result.CacheScope)
	}
	if _, ok := result.Meta[metaKeyServerInfo]; !ok {
		t.Errorf("_meta missing %s", metaKeyServerInfo)
	}
}

func TestToolsListCacheableAndOrdered(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "tools/list", map[string]interface{}{"_meta": validMeta()})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result ToolsListResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.ResultType != ResultTypeComplete {
		t.Errorf("resultType = %q", result.ResultType)
	}
	if result.TTLMs < 0 {
		t.Errorf("ttlMs = %d, want >= 0", result.TTLMs)
	}
	want := []string{"echo", "fail", "needs_input"}
	if len(result.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(result.Tools), len(want))
	}
	for i, name := range want {
		if result.Tools[i].Name != name {
			t.Errorf("tools[%d].Name = %q, want %q (registration order must be stable)", i, result.Tools[i].Name, name)
		}
	}
}

func TestMissingProtocolVersion(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "tools/list", map[string]interface{}{"_meta": metaObject("", map[string]interface{}{})})
	if env.Error == nil {
		t.Fatal("expected an error for missing protocolVersion")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", env.Error.Code, transport.InvalidParams)
	}
}

func TestMissingClientCapabilitiesField(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "tools/list", map[string]interface{}{"_meta": map[string]interface{}{metaKeyProtocolVersion: ProtocolVersion}})
	if env.Error == nil {
		t.Fatal("expected an error for missing clientCapabilities")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", env.Error.Code, transport.InvalidParams)
	}
}

func TestUnsupportedProtocolVersion(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "tools/list", map[string]interface{}{"_meta": metaObject("1999-01-01", map[string]interface{}{})})
	if env.Error == nil {
		t.Fatal("expected UnsupportedProtocolVersionError")
	}
	if env.Error.Code != transport.UnsupportedProtocolVersion {
		t.Errorf("code = %d, want %d (UnsupportedProtocolVersion)", env.Error.Code, transport.UnsupportedProtocolVersion)
	}
	var data struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}
	if err := json.Unmarshal(env.Error.Data, &data); err != nil {
		t.Fatalf("unmarshal error data: %v", err)
	}
	if len(data.Supported) != 1 || data.Supported[0] != ProtocolVersion {
		t.Errorf("data.supported = %v, want [%q]", data.Supported, ProtocolVersion)
	}
	if data.Requested != "1999-01-01" {
		t.Errorf("data.requested = %q, want %q", data.Requested, "1999-01-01")
	}
}

func TestInitializeReturnsLegacyDiagnostic(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// A legacy "initialize" request has no _meta at all.
	env := call(t, srv, 1, "initialize", map[string]interface{}{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "legacy-client", "version": "1.0"},
	})
	if env.Error == nil {
		t.Fatal("expected an error for initialize")
	}
	if env.Error.Code != transport.MethodNotFound {
		t.Errorf("code = %d, want %d (MethodNotFound)", env.Error.Code, transport.MethodNotFound)
	}
	var data struct {
		Supported []string `json:"supported"`
	}
	if err := json.Unmarshal(env.Error.Data, &data); err != nil {
		t.Fatalf("unmarshal error data: %v", err)
	}
	if len(data.Supported) != 1 || data.Supported[0] != ProtocolVersion {
		t.Errorf("data.supported = %v, want [%q]", data.Supported, ProtocolVersion)
	}
}

func TestUnknownToolIsInvalidParams(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "tools/call", map[string]interface{}{
		"_meta": validMeta(), "name": "does-not-exist", "arguments": map[string]interface{}{},
	})
	if env.Error == nil {
		t.Fatal("expected an error for unknown tool")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams) — -32002 must never be emitted", env.Error.Code, transport.InvalidParams)
	}
}

func TestUnknownResourceIsInvalidParams(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "resources/read", map[string]interface{}{
		"_meta": validMeta(), "uri": "test:///does-not-exist",
	})
	if env.Error == nil {
		t.Fatal("expected an error for unknown resource")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams) — -32002 must never be emitted", env.Error.Code, transport.InvalidParams)
	}
}

func TestResourcesReadRoundTrip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "resources/read", map[string]interface{}{"_meta": validMeta(), "uri": "test:///hello"})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result ResourcesReadResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Contents) != 1 || result.Contents[0].Text != "hello world" {
		t.Errorf("contents = %+v, want text %q", result.Contents, "hello world")
	}
}

func TestFailingToolReportsIsError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "tools/call", map[string]interface{}{
		"_meta": validMeta(), "name": "fail", "arguments": map[string]interface{}{},
	})
	if env.Error != nil {
		t.Fatalf("tool execution errors must not be JSON-RPC errors, got: %+v", env.Error)
	}
	var result ToolCallResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !result.IsError {
		t.Error("isError = false, want true")
	}
}

func TestEchoToolRoundTrip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := call(t, srv, 1, "tools/call", map[string]interface{}{
		"_meta": validMeta(), "name": "echo", "arguments": map[string]interface{}{"text": "hi there"},
	})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result ToolCallResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hi there" {
		t.Errorf("content = %+v, want text %q", result.Content, "hi there")
	}
}

func TestMRTRRoundTrip(t *testing.T) {
	srv, _, _ := newTestServer(t)

	capsWithElicitation := map[string]interface{}{"elicitation": map[string]interface{}{}}

	// First attempt: no capability declared at all -> -32021.
	env := call(t, srv, 1, "tools/call", map[string]interface{}{
		"_meta": metaObject(ProtocolVersion, map[string]interface{}{}), "name": "needs_input", "arguments": map[string]interface{}{},
	})
	if env.Error == nil || env.Error.Code != transport.MissingRequiredClientCapability {
		t.Fatalf("expected MissingRequiredClientCapability, got %+v", env.Error)
	}

	// Second attempt: elicitation declared -> input_required.
	env = call(t, srv, 2, "tools/call", map[string]interface{}{
		"_meta": metaObject(ProtocolVersion, capsWithElicitation), "name": "needs_input", "arguments": map[string]interface{}{},
	})
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var interim struct {
		ResultType    string          `json:"resultType"`
		InputRequests json.RawMessage `json:"inputRequests"`
		RequestState  string          `json:"requestState"`
	}
	if err := json.Unmarshal(env.Result, &interim); err != nil {
		t.Fatalf("unmarshal interim result: %v", err)
	}
	if interim.ResultType != ResultTypeInputRequired {
		t.Fatalf("resultType = %q, want %q", interim.ResultType, ResultTypeInputRequired)
	}
	if interim.RequestState == "" {
		t.Fatal("requestState must be set on an input_required result")
	}

	// Retry with inputResponses + requestState -> complete.
	env = call(t, srv, 3, "tools/call", map[string]interface{}{
		"_meta": metaObject(ProtocolVersion, capsWithElicitation),
		"name":  "needs_input", "arguments": map[string]interface{}{},
		"inputResponses": map[string]interface{}{
			"x": map[string]interface{}{"action": "accept", "content": map[string]interface{}{}},
		},
		"requestState": interim.RequestState,
	})
	if env.Error != nil {
		t.Fatalf("unexpected error on retry: %+v", env.Error)
	}
	var final ToolCallResult
	if err := json.Unmarshal(env.Result, &final); err != nil {
		t.Fatalf("unmarshal final result: %v", err)
	}
	if final.ResultType != ResultTypeComplete {
		t.Errorf("resultType = %q, want %q", final.ResultType, ResultTypeComplete)
	}
	if len(final.Content) != 1 || final.Content[0].Text != "done" {
		t.Errorf("content = %+v, want text %q", final.Content, "done")
	}

	// Tampered requestState must be rejected.
	env = call(t, srv, 4, "tools/call", map[string]interface{}{
		"_meta": metaObject(ProtocolVersion, capsWithElicitation),
		"name":  "needs_input", "arguments": map[string]interface{}{},
		"inputResponses": map[string]interface{}{
			"x": map[string]interface{}{"action": "accept", "content": map[string]interface{}{}},
		},
		"requestState": interim.RequestState + "tampered",
	})
	if env.Error == nil {
		t.Fatal("expected tampered requestState to be rejected")
	}
	if env.Error.Code != transport.InvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", env.Error.Code, transport.InvalidParams)
	}
}

func TestPaginationRoundTrip(t *testing.T) {
	items := make([]int, 205)
	for i := range items {
		items[i] = i
	}

	var cursor string
	var seen []int
	for {
		page, next, err := paginate(items, cursor, 50)
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}
		seen = append(seen, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != len(items) {
		t.Fatalf("got %d items across all pages, want %d", len(seen), len(items))
	}
	for i, v := range seen {
		if v != items[i] {
			t.Fatalf("seen[%d] = %d, want %d", i, v, items[i])
		}
	}

	if _, _, err := paginate(items, "not-a-valid-cursor!!", 50); err == nil {
		t.Fatal("expected an error for an invalid cursor")
	}
}

func TestSubscriptionsListenNotifiesOnToolsListChanged(t *testing.T) {
	srv, registry, _ := newTestServer(t)

	w := transport.NewBufferedResponseWriter()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := buildRequest(t, 1, "subscriptions/listen", map[string]interface{}{
			"_meta":         validMeta(),
			"notifications": map[string]interface{}{"toolsListChanged": true},
		})
		srv.HandleMessage(ctx, req, w)
	}()

	// Wait for the acknowledgment before mutating the registry, so the
	// list_changed notification is guaranteed to be observable rather than racing the
	// subscribe() call inside handleSubscriptionsListen.
	deadline := time.Now().Add(2 * time.Second)
	for len(w.Notifications()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for subscriptions/listen acknowledgment")
		}
		time.Sleep(5 * time.Millisecond)
	}

	registry.Register(Tool{Name: "late", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *ToolRequest) (Result, error) {
			return &ToolCallResult{Content: []Content{Text("late")}}, nil
		})

	deadline = time.Now().Add(2 * time.Second)
	for len(w.Notifications()) < 2 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	notifications := w.Notifications()
	if len(notifications) == 0 || notifications[0].Method != "notifications/subscriptions/acknowledged" {
		t.Fatalf("first message on the subscription must be the acknowledgment, got %+v", notifications)
	}

	foundAck := false
	foundChanged := false
	for i, n := range notifications {
		params, ok := n.Params.(map[string]interface{})
		if !ok {
			t.Fatalf("notification %d (%s): params is %T, want map[string]interface{}", i, n.Method, n.Params)
		}
		meta, ok := params["_meta"].(map[string]interface{})
		if !ok {
			t.Fatalf("notification %d (%s): missing _meta", i, n.Method)
		}
		subID, err := json.Marshal(meta[metaKeySubscriptionID])
		if err != nil || string(subID) != "1" {
			t.Errorf("notification %d (%s): _meta.subscriptionId = %s, want 1 (the listen request's JSON-RPC id)", i, n.Method, subID)
		}
		switch n.Method {
		case "notifications/subscriptions/acknowledged":
			foundAck = true
		case "notifications/tools/list_changed":
			foundChanged = true
		}
	}
	if !foundAck {
		t.Error("did not observe notifications/subscriptions/acknowledged")
	}
	if !foundChanged {
		t.Error("did not observe notifications/tools/list_changed after a runtime Register()")
	}
}

// observeDuringMutation opens a subscriptions/listen stream with the given notification
// filter, waits for its acknowledgment (so a subsequent registry mutation cannot race the
// subscribe), runs mutate, then gives the broker a bounded window to deliver one
// notification. It returns every notification written after the acknowledgment.
func observeDuringMutation(t *testing.T, srv *Server, filter map[string]interface{}, mutate func()) []transport.BufferedNotification {
	t.Helper()
	return observeDuringMutationN(t, srv, filter, 1, mutate)
}

// observeDuringMutationN is observeDuringMutation with an explicit count: it keeps waiting
// until want notifications have arrived past the acknowledgment, or the window expires.
// Callers expecting a mutation to fire two notifications must use this — otherwise the wait
// returns as soon as the first lands and the second races the stream teardown.
func observeDuringMutationN(t *testing.T, srv *Server, filter map[string]interface{}, want int, mutate func()) []transport.BufferedNotification {
	t.Helper()

	w := transport.NewBufferedResponseWriter()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := buildRequest(t, 1, "subscriptions/listen", map[string]interface{}{
			"_meta":         validMeta(),
			"notifications": filter,
		})
		srv.HandleMessage(ctx, req, w)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(w.Notifications()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("timed out waiting for subscriptions/listen acknowledgment")
		}
		time.Sleep(5 * time.Millisecond)
	}

	mutate()

	// Bounded wait for the post-mutation notifications. Callers asserting that nothing
	// arrives still pay this window, which is what makes that assertion meaningful.
	deadline = time.Now().Add(500 * time.Millisecond)
	for len(w.Notifications()) < want+1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	all := w.Notifications()
	if len(all) == 0 {
		return nil
	}
	return all[1:] // drop the acknowledgment
}

func TestUnregisterRemovesToolAndNotifies(t *testing.T) {
	srv, registry, _ := newTestServer(t)

	before := len(registry.List())
	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{"toolsListChanged": true},
		func() {
			if !registry.Unregister("fail") {
				t.Error("Unregister(\"fail\") = false, want true for a registered tool")
			}
		})

	if got := len(registry.List()); got != before-1 {
		t.Errorf("len(List()) = %d, want %d after unregistering one tool", got, before-1)
	}
	if _, ok := registry.Get("fail"); ok {
		t.Error("Get(\"fail\") still reports the tool after Unregister")
	}

	env := call(t, srv, 2, "tools/list", map[string]interface{}{"_meta": validMeta()})
	var listed struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(env.Result, &listed); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "fail" {
			t.Error("tools/list still advertises an unregistered tool")
		}
	}

	// Calling the removed tool is reported exactly like a name that never existed.
	env = call(t, srv, 3, "tools/call", map[string]interface{}{
		"_meta": validMeta(), "name": "fail", "arguments": map[string]interface{}{},
	})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Errorf("tools/call on an unregistered tool: error = %+v, want code %d", env.Error, transport.InvalidParams)
	}

	if !hasNotification(notifications, "notifications/tools/list_changed") {
		t.Errorf("did not observe notifications/tools/list_changed after Unregister, got %+v", notifications)
	}
}

func TestUnregisterRemovesResourceAndNotifies(t *testing.T) {
	srv, _, resources := newTestServer(t)

	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{"resourcesListChanged": true},
		func() {
			if !resources.Unregister("test:///hello") {
				t.Error("Unregister of a registered resource = false, want true")
			}
		})

	if resources.HasResources() {
		t.Error("HasResources() = true after unregistering the only resource")
	}

	env := call(t, srv, 2, "resources/read", map[string]interface{}{
		"_meta": validMeta(), "uri": "test:///hello",
	})
	if env.Error == nil || env.Error.Code != transport.InvalidParams {
		t.Errorf("resources/read on an unregistered URI: error = %+v, want code %d", env.Error, transport.InvalidParams)
	}

	// capabilities() derives from HasResources(), so dropping the last resource must drop
	// the advertised capability too.
	env = call(t, srv, 3, "server/discover", map[string]interface{}{"_meta": validMeta()})
	var discovered struct {
		Capabilities ServerCapabilities `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &discovered); err != nil {
		t.Fatalf("unmarshal server/discover: %v", err)
	}
	if discovered.Capabilities.Resources != nil {
		t.Error("server/discover still advertises a resources capability with no resources registered")
	}

	if !hasNotification(notifications, "notifications/resources/list_changed") {
		t.Errorf("did not observe notifications/resources/list_changed after Unregister, got %+v", notifications)
	}
}

func TestUnregisterUnknownKeyIsNoOp(t *testing.T) {
	srv, registry, resources := newTestServer(t)

	toolsBefore := len(registry.List())
	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{"toolsListChanged": true, "resourcesListChanged": true},
		func() {
			if registry.Unregister("never-registered") {
				t.Error("ToolRegistry.Unregister of an absent name = true, want false")
			}
			if resources.Unregister("test:///nope") {
				t.Error("ResourceRegistry.Unregister of an absent URI = true, want false")
			}
		})

	if got := len(registry.List()); got != toolsBefore {
		t.Errorf("len(List()) = %d, want %d — an absent-key Unregister must not mutate", got, toolsBefore)
	}
	if len(notifications) != 0 {
		t.Errorf("absent-key Unregister fired %d notification(s), want none: %+v", len(notifications), notifications)
	}
}

func TestReRegisterReplacesAndMovesToEnd(t *testing.T) {
	srv, registry, resources := newTestServer(t)

	before := len(registry.List())

	// "echo" is registered first in newTestServer; re-registering must replace it in
	// place of appending a second entry, and move it to the end of the list.
	registry.Register(Tool{
		Name:        "echo",
		Description: "replacement",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(ctx context.Context, req *ToolRequest) (Result, error) {
		return &ToolCallResult{Content: []Content{Text("replaced")}}, nil
	})

	tools := registry.List()
	if len(tools) != before {
		t.Errorf("len(List()) = %d, want %d — re-registering must not duplicate", len(tools), before)
	}
	if tools[len(tools)-1].Name != "echo" {
		t.Errorf("last tool = %q, want \"echo\" moved to the end on re-registration", tools[len(tools)-1].Name)
	}
	seen := 0
	for _, tool := range tools {
		if tool.Name == "echo" {
			seen++
			if tool.Description != "replacement" {
				t.Errorf("echo.Description = %q, want the replacement definition", tool.Description)
			}
		}
	}
	if seen != 1 {
		t.Errorf("found %d entries named \"echo\", want exactly 1", seen)
	}

	// The replacement function must be the one that runs.
	env := call(t, srv, 2, "tools/call", map[string]interface{}{
		"_meta": validMeta(), "name": "echo", "arguments": map[string]interface{}{},
	})
	if env.Error != nil {
		t.Fatalf("tools/call on the replaced tool: unexpected error %+v", env.Error)
	}
	var result ToolCallResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal tools/call result: %v", err)
	}
	if len(result.Content) == 0 || result.Content[0].Text != "replaced" {
		t.Errorf("tools/call ran the old function; content = %+v, want \"replaced\"", result.Content)
	}

	// Same rule for resources, including the function swap observed through resources/read.
	resources.Register(Resource{URI: "test:///hello", Name: "hello", MimeType: "text/plain"},
		func(ctx context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "replaced body"}, nil
		})
	if got := len(resources.List()); got != 1 {
		t.Errorf("len(resources.List()) = %d, want 1 — re-registering a URI must not duplicate", got)
	}
	env = call(t, srv, 3, "resources/read", map[string]interface{}{
		"_meta": validMeta(), "uri": "test:///hello",
	})
	if env.Error != nil {
		t.Fatalf("resources/read after re-registration: unexpected error %+v", env.Error)
	}
	var read ResourcesReadResult
	if err := json.Unmarshal(env.Result, &read); err != nil {
		t.Fatalf("unmarshal resources/read: %v", err)
	}
	if len(read.Contents) == 0 || read.Contents[0].Text != "replaced body" {
		t.Errorf("resources/read returned the old function's content: %+v", read.Contents)
	}
}

func TestNotifyUpdatedDeliversResourceUpdated(t *testing.T) {
	srv, _, resources := newTestServer(t)

	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{"resourceSubscriptions": []string{"test:///hello"}},
		func() {
			if !resources.NotifyUpdated("test:///hello") {
				t.Error("NotifyUpdated on a registered URI = false, want true")
			}
		})

	if len(notifications) != 1 {
		t.Fatalf("NotifyUpdated delivered %d notification(s), want exactly 1: %+v", len(notifications), notifications)
	}

	// This is the first broadcast to carry params, so assert that the URI survives
	// withSubscriptionMeta's merge alongside the _meta every subscription message needs.
	params := notificationParams(t, notifications, "notifications/resources/updated")
	if got, _ := params["uri"].(string); got != "test:///hello" {
		t.Errorf("params.uri = %q, want \"test:///hello\"", got)
	}
	meta, ok := params["_meta"].(map[string]interface{})
	if !ok {
		t.Fatalf("notifications/resources/updated: missing _meta, params = %+v", params)
	}
	subID, err := json.Marshal(meta[metaKeySubscriptionID])
	if err != nil || string(subID) != "1" {
		t.Errorf("_meta.subscriptionId = %s, want 1 (the listen request's JSON-RPC id)", subID)
	}
}

func TestNotifyUpdatedIgnoresUnwatchedURI(t *testing.T) {
	srv, _, resources := newTestServer(t)

	resources.Register(Resource{URI: "test:///other", Name: "other"},
		func(ctx context.Context) (ResourceContentResult, error) {
			return ResourceContentResult{Text: "other"}, nil
		})

	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{"resourceSubscriptions": []string{"test:///other"}},
		func() {
			resources.NotifyUpdated("test:///hello")
		})

	if len(notifications) != 0 {
		t.Errorf("a subscriber watching a different URI received %d notification(s), want none: %+v",
			len(notifications), notifications)
	}
}

// TestNotifyUpdatedIsIndependentOfListChanged pins the two resource filters as separate
// opt-ins: resourcesListChanged is about the catalog, resourceSubscriptions about content.
func TestNotifyUpdatedIsIndependentOfListChanged(t *testing.T) {
	srv, _, resources := newTestServer(t)

	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{"resourcesListChanged": true},
		func() {
			resources.NotifyUpdated("test:///hello")
		})

	if len(notifications) != 0 {
		t.Errorf("resourcesListChanged alone received %d notification(s) from NotifyUpdated, want none: %+v",
			len(notifications), notifications)
	}
}

func TestNotifyUpdatedUnknownURIIsNoOp(t *testing.T) {
	srv, _, resources := newTestServer(t)

	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{"resourceSubscriptions": []string{"test:///nope"}},
		func() {
			if resources.NotifyUpdated("test:///nope") {
				t.Error("NotifyUpdated on an unregistered URI = true, want false")
			}
		})

	if len(notifications) != 0 {
		t.Errorf("NotifyUpdated on an unregistered URI fired %d notification(s), want none: %+v",
			len(notifications), notifications)
	}
}

// TestReRegisterResourceFiresListChangedAndUpdated pins the deliberate over-notification on
// replacement: Register swaps both the metadata and the ResourceFunction, and the registry
// cannot tell which changed, so a watcher of that URI hears about it.
func TestReRegisterResourceFiresListChangedAndUpdated(t *testing.T) {
	srv, _, resources := newTestServer(t)

	notifications := observeDuringMutationN(t, srv,
		map[string]interface{}{
			"resourcesListChanged":  true,
			"resourceSubscriptions": []string{"test:///hello"},
		}, 2,
		func() {
			resources.Register(Resource{URI: "test:///hello", Name: "hello", MimeType: "text/plain"},
				func(ctx context.Context) (ResourceContentResult, error) {
					return ResourceContentResult{Text: "replaced body"}, nil
				})
		})

	if !hasNotification(notifications, "notifications/resources/list_changed") {
		t.Errorf("re-registering a URI did not fire list_changed, got %+v", notifications)
	}
	if !hasNotification(notifications, "notifications/resources/updated") {
		t.Errorf("re-registering a URI did not fire resources/updated, got %+v", notifications)
	}
	if params := notificationParams(t, notifications, "notifications/resources/updated"); params["uri"] != "test:///hello" {
		t.Errorf("params.uri = %v, want \"test:///hello\"", params["uri"])
	}
}

// TestFreshResourceRegistrationDoesNotFireUpdated is the other half of the rule: a URI that
// was not there before is a catalog addition, which list_changed already covers.
func TestFreshResourceRegistrationDoesNotFireUpdated(t *testing.T) {
	srv, _, resources := newTestServer(t)

	notifications := observeDuringMutation(t, srv,
		map[string]interface{}{
			"resourcesListChanged":  true,
			"resourceSubscriptions": []string{"test:///fresh"},
		},
		func() {
			resources.Register(Resource{URI: "test:///fresh", Name: "fresh"},
				func(ctx context.Context) (ResourceContentResult, error) {
					return ResourceContentResult{Text: "fresh"}, nil
				})
		})

	if !hasNotification(notifications, "notifications/resources/list_changed") {
		t.Errorf("registering a new resource did not fire list_changed, got %+v", notifications)
	}
	if hasNotification(notifications, "notifications/resources/updated") {
		t.Errorf("a first-time registration fired resources/updated, want list_changed only: %+v", notifications)
	}
}

// TestCallAfterConcurrentUnregister covers the window Unregister opened: handleToolsCall
// looks the tool up, then calls it, and a removal landing in between used to hand call a
// nil function. It must report a missing tool rather than panic.
func TestCallAfterConcurrentUnregister(t *testing.T) {
	_, registry, _ := newTestServer(t)
	registry.Unregister("echo")

	_, err := registry.call(context.Background(), &ToolRequest{Name: "echo"})
	if !errors.Is(err, errToolNotFound) {
		t.Fatalf("call on an unregistered tool: err = %v, want errToolNotFound", err)
	}
}

// notificationParams returns the params of the first notification with the given method,
// failing the test if it is absent or not a params object.
func notificationParams(t *testing.T, notifications []transport.BufferedNotification, method string) map[string]interface{} {
	t.Helper()
	for _, n := range notifications {
		if n.Method != method {
			continue
		}
		params, ok := n.Params.(map[string]interface{})
		if !ok {
			t.Fatalf("%s: params is %T, want map[string]interface{}", method, n.Params)
		}
		return params
	}
	t.Fatalf("did not observe %s, got %+v", method, notifications)
	return nil
}

func hasNotification(notifications []transport.BufferedNotification, method string) bool {
	for _, n := range notifications {
		if n.Method == method {
			return true
		}
	}
	return false
}
