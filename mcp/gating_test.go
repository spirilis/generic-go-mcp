package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/transport"
)

// rolesKey carries the caller's roles in tests, standing in for auth.AuthService.RolesFromContext.
type rolesKey struct{}

func withRoles(roles ...string) context.Context {
	return context.WithValue(context.Background(), rolesKey{}, roles)
}

func rolesFromTestContext(ctx context.Context) []string {
	roles, _ := ctx.Value(rolesKey{}).([]string)
	return roles
}

func okTool(name string) (Tool, ToolFunction) {
	return Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *ToolRequest) (Result, error) {
			return &ToolCallResult{Content: []Content{Text("ran " + name)}}, nil
		}
}

// newGatedServer registers one ungated tool and three gated ones. withSecret=false leaves the
// "secret" tool out entirely, which is the baseline a hidden tool's answers must equal.
func newGatedServer(t *testing.T, rolesFn func(context.Context) []string, withSecret bool) (*Server, *ToolRegistry) {
	t.Helper()
	registry := NewToolRegistry()
	registry.Register(okTool("public"))
	if withSecret {
		tool, fn := okTool("secret")
		registry.RegisterWithOptions(tool, fn, RequireRoles("admin"))
	}
	tool, fn := okTool("shared")
	registry.RegisterWithOptions(tool, fn, RequireRoles("admin", "reader"))
	tool, fn = okTool("nobody")
	registry.RegisterWithOptions(tool, fn, RequireRoles())
	srv := NewServer(registry, NewResourceRegistry(), &ServerConfig{
		Name:             "test-server",
		Version:          "0.0.0",
		RolesFromContext: rolesFn,
	})
	return srv, registry
}

func callCtx(t *testing.T, ctx context.Context, srv *Server, id int, method string, params map[string]interface{}) []byte {
	t.Helper()
	w := transport.NewBufferedResponseWriter()
	srv.HandleMessage(ctx, buildRequest(t, id, method, params), w)
	if w.Message() == nil {
		t.Fatalf("%s wrote no response", method)
	}
	return w.Message()
}

func listNames(t *testing.T, ctx context.Context, srv *Server) ([]string, string) {
	t.Helper()
	var names []string
	cursor, scope := "", ""
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("tools/list did not terminate")
		}
		params := map[string]interface{}{"_meta": validMeta()}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var env rpcResponseEnvelope
		if err := json.Unmarshal(callCtx(t, ctx, srv, 1, "tools/list", params), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error != nil {
			t.Fatalf("tools/list: %+v", env.Error)
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
			CacheScope string `json:"cacheScope"`
		}
		if err := json.Unmarshal(env.Result, &res); err != nil {
			t.Fatal(err)
		}
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		scope = res.CacheScope
		if res.NextCursor == "" {
			return names, scope
		}
		cursor = res.NextCursor
	}
}

func TestToolsListHidesGatedTools(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  []string
	}{
		{"no roles", nil, []string{"public"}},
		{"unrelated role", []string{"writer"}, []string{"public"}},
		{"reader", []string{"reader"}, []string{"public", "shared"}},
		{"admin", []string{"admin"}, []string{"public", "secret", "shared"}},
		{"both", []string{"reader", "admin"}, []string{"public", "secret", "shared"}},
	}
	srv, _ := newGatedServer(t, rolesFromTestContext, true)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, scope := listNames(t, withRoles(tt.roles...), srv)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tools = %v, want %v", got, tt.want)
			}
			if scope != CacheScopePrivate {
				t.Errorf("cacheScope = %q, want private while tools are gated", scope)
			}
		})
	}
}

// TestRolesFromContextReturningNilHidesGatedTools: a configured RolesFromContext that finds no
// principal (nil roles) must fail closed, not be read as "gating off".
func TestRolesFromContextReturningNilHidesGatedTools(t *testing.T) {
	srv, _ := newGatedServer(t, func(context.Context) []string { return nil }, true)
	got, _ := listNames(t, withRoles("admin"), srv)
	if !reflect.DeepEqual(got, []string{"public"}) {
		t.Errorf("tools = %v, want only the ungated one", got)
	}
}

func TestNoRolesFromContextServesEveryTool(t *testing.T) {
	srv, _ := newGatedServer(t, nil, true)
	got, scope := listNames(t, context.Background(), srv)
	if want := []string{"public", "secret", "shared", "nobody"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	if scope != CacheScopePublic {
		t.Errorf("cacheScope = %q, want the default", scope)
	}
	env := call(t, srv, 2, "tools/call", map[string]interface{}{"_meta": validMeta(), "name": "nobody"})
	if env.Error != nil {
		t.Errorf("tools/call without gating: %+v", env.Error)
	}
}

func TestToolsListCacheScope(t *testing.T) {
	tests := []struct {
		name    string
		rolesFn func(context.Context) []string
		gated   bool
		want    string
	}{
		{"gated tool, gating on", rolesFromTestContext, true, CacheScopePrivate},
		{"no gated tool, gating on", rolesFromTestContext, false, CacheScopePublic},
		{"gated tool, gating off", nil, true, CacheScopePublic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewToolRegistry()
			tool, fn := okTool("t")
			if tt.gated {
				registry.RegisterWithOptions(tool, fn, RequireRoles("admin"))
			} else {
				registry.Register(tool, fn)
			}
			srv := NewServer(registry, NewResourceRegistry(), &ServerConfig{RolesFromContext: tt.rolesFn})
			if _, scope := listNames(t, withRoles("admin"), srv); scope != tt.want {
				t.Errorf("cacheScope = %q, want %q", scope, tt.want)
			}
		})
	}
}

// TestHiddenToolCallMatchesUnknownTool compares whole responses: a caller without the role must
// get byte-for-byte what a server that never registered the tool answers, whatever else the
// request carries (arguments, a forged requestState).
func TestHiddenToolCallMatchesUnknownTool(t *testing.T) {
	gated, _ := newGatedServer(t, rolesFromTestContext, true)
	absent, _ := newGatedServer(t, rolesFromTestContext, false)

	tests := []struct {
		name   string
		roles  []string
		params map[string]interface{}
	}{
		{"no roles", nil, map[string]interface{}{"name": "secret"}},
		{"wrong role", []string{"reader"}, map[string]interface{}{"name": "secret"}},
		{"with arguments", []string{"reader"}, map[string]interface{}{"name": "secret", "arguments": map[string]interface{}{"x": 1}}},
		{"forged requestState", []string{"reader"}, map[string]interface{}{"name": "secret", "requestState": "bogus"}},
		{"RequireRoles() with no roles", []string{"admin"}, map[string]interface{}{"name": "nobody", "requestState": "bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := map[string]interface{}{"_meta": validMeta()}
			for k, v := range tt.params {
				params[k] = v
			}
			ctx := withRoles(tt.roles...)
			got := callCtx(t, ctx, gated, 7, "tools/call", params)

			want := callCtx(t, ctx, absent, 7, "tools/call", params)
			if tt.params["name"] == "nobody" {
				// "nobody" is registered on both servers; compare against a name neither has.
				params["name"] = "nobody-at-all"
				want = []byte(strings.Replace(string(callCtx(t, ctx, absent, 7, "tools/call", params)), "nobody-at-all", "nobody", 1))
			}
			if string(got) != string(want) {
				t.Errorf("hidden tool answered\n  %s\nunknown tool answers\n  %s", got, want)
			}
			if !strings.Contains(string(got), `"code":-32602`) {
				t.Errorf("want -32602, got %s", got)
			}
		})
	}
}

func TestGatedToolCallableWithRole(t *testing.T) {
	srv, _ := newGatedServer(t, rolesFromTestContext, true)
	for _, tc := range []struct{ tool, role string }{{"secret", "admin"}, {"shared", "reader"}, {"shared", "admin"}} {
		var env rpcResponseEnvelope
		raw := callCtx(t, withRoles(tc.role), srv, 1, "tools/call", map[string]interface{}{"_meta": validMeta(), "name": tc.tool})
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if env.Error != nil || !strings.Contains(string(env.Result), "ran "+tc.tool) {
			t.Errorf("%s as %s: %s", tc.tool, tc.role, raw)
		}
	}
}

// TestGatedPagination interleaves hidden tools across several pages: the visible ones must all
// come back, in order, with nothing hidden and no short page in between.
func TestGatedPagination(t *testing.T) {
	registry := NewToolRegistry()
	var want []string
	for i := 0; i < 3*defaultPageSize; i++ {
		tool, fn := okTool(fmt.Sprintf("t%03d", i))
		if i%3 == 0 {
			registry.RegisterWithOptions(tool, fn, RequireRoles("admin"))
			continue
		}
		registry.Register(tool, fn)
		want = append(want, tool.Name)
	}
	srv := NewServer(registry, NewResourceRegistry(), &ServerConfig{RolesFromContext: rolesFromTestContext})

	params := map[string]interface{}{"_meta": validMeta()}
	var first struct {
		Tools []Tool `json:"tools"`
	}
	var env rpcResponseEnvelope
	if err := json.Unmarshal(callCtx(t, withRoles(), srv, 1, "tools/list", params), &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Result, &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Tools) != defaultPageSize {
		t.Errorf("first page has %d tools, want a full page of %d", len(first.Tools), defaultPageSize)
	}

	got, _ := listNames(t, withRoles(), srv)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged %d tools, want %d (first mismatch hides or skips a tool)", len(got), len(want))
	}
}

func TestReRegisterWithoutRequireRolesUngates(t *testing.T) {
	srv, registry := newGatedServer(t, rolesFromTestContext, true)
	tool, fn := okTool("secret")
	registry.Register(tool, fn)
	got, _ := listNames(t, withRoles(), srv)
	if want := []string{"public", "secret"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}

	registry.Unregister("shared")
	registry.Unregister("nobody")
	if registry.HasGatedTools() {
		t.Error("HasGatedTools after every gated tool was replaced or removed")
	}
}

// TestCallRechecksGate covers a Register landing between the handler's lookup and the call: the
// function that runs must be one the caller may run.
func TestCallRechecksGate(t *testing.T) {
	srv, registry := newGatedServer(t, rolesFromTestContext, true)
	tool, fn := okTool("public")
	registry.RegisterWithOptions(tool, fn, RequireRoles("admin"))

	_, err := registry.call(context.Background(), &ToolRequest{Name: "public"}, srv.toolGate(withRoles("reader")))
	if !errors.Is(err, errToolNotFound) {
		t.Fatalf("call after the tool became gated: err = %v, want errToolNotFound", err)
	}
}

// TestListChangedCarriesNoToolNames: registering a gated tool notifies every tools listener,
// including callers who cannot see it. The notification must say only that the list changed.
func TestListChangedCarriesNoToolNames(t *testing.T) {
	srv, registry := newGatedServer(t, rolesFromTestContext, true)

	w := transport.NewBufferedResponseWriter()
	ctx, cancel := context.WithCancel(withRoles())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleMessage(ctx, buildRequest(t, 1, "subscriptions/listen", map[string]interface{}{
			"_meta":         validMeta(),
			"notifications": map[string]interface{}{"toolsListChanged": true},
		}), w)
	}()
	waitForNotifications(t, w, 1)

	tool, fn := okTool("top-secret-tool")
	registry.RegisterWithOptions(tool, fn, RequireRoles("admin"))
	waitForNotifications(t, w, 2)
	cancel()
	<-done

	params := notificationParams(t, w.Notifications(), "notifications/tools/list_changed")
	for k := range params {
		if k != "_meta" {
			t.Errorf("list_changed carries %q", k)
		}
	}
	raw, _ := json.Marshal(w.Notifications())
	if strings.Contains(string(raw), "top-secret-tool") {
		t.Errorf("listen stream leaks the gated tool's name: %s", raw)
	}
}

func waitForNotifications(t *testing.T, w *transport.BufferedResponseWriter, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(w.Notifications()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d notifications, have %d", n, len(w.Notifications()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHiddenToolSkipsHeaderValidation: x-mcp-header checks run only after the gate. A hidden
// tool whose header binding the request violates must still answer as an unknown tool, not with
// the header-mismatch error that would confirm it exists.
func TestHiddenToolSkipsHeaderValidation(t *testing.T) {
	newServer := func(withTool bool) *Server {
		registry := NewToolRegistry()
		if withTool {
			registry.RegisterWithOptions(Tool{
				Name:        "region_admin",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`),
			}, func(ctx context.Context, req *ToolRequest) (Result, error) {
				return &ToolCallResult{Content: []Content{Text("ran")}}, nil
			}, RequireRoles("admin"))
		}
		return NewServer(registry, NewResourceRegistry(), &ServerConfig{RolesFromContext: rolesFromTestContext})
	}
	params := map[string]interface{}{
		"_meta":     validMeta(),
		"name":      "region_admin",
		"arguments": map[string]interface{}{"region": "us-east"},
	}
	// Headers attached but the bound Mcp-Param header missing: a header-mismatch error for a
	// caller allowed to see the tool.
	ctx := transport.WithRequestHeaders(withRoles("reader"), transport.RequestHeaders{Values: map[string]string{}})

	got := callCtx(t, ctx, newServer(true), 3, "tools/call", params)
	want := callCtx(t, ctx, newServer(false), 3, "tools/call", params)
	if string(got) != string(want) {
		t.Errorf("hidden tool answered\n  %s\nunknown tool answers\n  %s", got, want)
	}

	admin := transport.WithRequestHeaders(withRoles("admin"), transport.RequestHeaders{Values: map[string]string{}})
	if raw := callCtx(t, admin, newServer(true), 3, "tools/call", params); string(raw) == string(want) {
		t.Errorf("the admin got the unknown-tool error too, so this test does not exercise header validation: %s", raw)
	}
}
