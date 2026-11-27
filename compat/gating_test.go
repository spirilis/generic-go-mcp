package compat

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

// rolesKey carries the caller's roles in tests, standing in for auth.AuthService.RolesFromContext.
type rolesKey struct{}

func withRoles(ctx context.Context, roles ...string) context.Context {
	return context.WithValue(ctx, rolesKey{}, roles)
}

// newGatedOverlay builds an overlay over a server with one ungated tool and, when withSecret,
// one tool gated to "admin". The overlay adds no gating of its own: these tests prove that the
// legacy path reaches the inner server's gate with the request's context intact.
func newGatedOverlay(t *testing.T, withSecret bool) (*Overlay, *mcp.ToolRegistry) {
	t.Helper()
	registry := mcp.NewToolRegistry()
	tool := func(name string) (mcp.Tool, mcp.ToolFunction) {
		return mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
				return &mcp.ToolCallResult{Content: []mcp.Content{mcp.Text("ran " + name)}}, nil
			}
	}
	registry.Register(tool("public"))
	if withSecret {
		def, fn := tool("secret")
		registry.RegisterWithOptions(def, fn, mcp.RequireRoles("admin"))
	}
	server := mcp.NewServer(registry, mcp.NewResourceRegistry(), &mcp.ServerConfig{
		Name:               "test-server",
		Version:            "0.0.1",
		AdvertisedVersions: append([]string{mcp.ProtocolVersion}, LegacyVersions...),
		RolesFromContext: func(ctx context.Context) []string {
			roles, _ := ctx.Value(rolesKey{}).([]string)
			return roles
		},
	})
	return New(server, Config{}), registry
}

func TestLegacyToolsListHidesGatedTools(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  []string
	}{
		{"no roles", nil, []string{"public"}},
		{"wrong role", []string{"reader"}, []string{"public"}},
		{"admin", []string{"admin"}, []string{"public", "secret"}},
	}
	o, _ := newGatedOverlay(t, true)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := withRoles(legacySessionContext(t, o), tt.roles...)
			bw := transport.NewBufferedResponseWriter()
			o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("2"), "tools/list", map[string]interface{}{}), bw)
			var res struct {
				Tools []mcp.Tool `json:"tools"`
			}
			decodeResult(t, bw.Message(), &res)
			var got []string
			for _, tool := range res.Tools {
				got = append(got, tool.Name)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("legacy tools/list = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLegacyHiddenToolCallMatchesUnknownTool compares whole legacy responses against an overlay
// whose server never registered the tool.
func TestLegacyHiddenToolCallMatchesUnknownTool(t *testing.T) {
	gated, _ := newGatedOverlay(t, true)
	absent, _ := newGatedOverlay(t, false)

	answer := func(o *Overlay, roles ...string) string {
		ctx := withRoles(legacySessionContext(t, o), roles...)
		bw := transport.NewBufferedResponseWriter()
		o.HandleMessage(ctx, mustMarshal(t, json.RawMessage("9"), "tools/call", map[string]interface{}{
			"name": "secret", "arguments": map[string]interface{}{},
		}), bw)
		return string(bw.Message())
	}

	for _, roles := range [][]string{nil, {"reader"}} {
		got, want := answer(gated, roles...), answer(absent, roles...)
		if got != want {
			t.Errorf("roles %v: hidden tool answered\n  %s\nunknown tool answers\n  %s", roles, got, want)
		}
		if !strings.Contains(got, `"code":-32602`) {
			t.Errorf("roles %v: want -32602, got %s", roles, got)
		}
	}
	if got := answer(gated, "admin"); !strings.Contains(got, "ran secret") {
		t.Errorf("admin over the legacy path: %s", got)
	}
}

// TestLegacyListChangedCarriesNoToolNames: the legacy notification stream relays the inner
// server's list_changed, which must not name the gated tool that triggered it.
func TestLegacyListChangedCarriesNoToolNames(t *testing.T) {
	o, registry := newGatedOverlay(t, false)
	sessCtx := legacySessionContext(t, o)
	id := transport.RequestInfoFromContext(sessCtx).SessionID

	w := transport.NewBufferedResponseWriter()
	ctx, cancel := context.WithCancel(withRoles(context.Background()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = o.LegacySessions().StreamNotifications(ctx, id, w)
	}()

	// The legacy stream drops the subscription ack, so there is nothing to wait on before
	// registering; keep registering until a notification comes through.
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; len(w.Notifications()) == 0; i++ {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("no list_changed on the legacy stream")
		}
		def := mcp.Tool{Name: "top-secret-tool", InputSchema: json.RawMessage(`{"type":"object"}`)}
		registry.RegisterWithOptions(def, func(context.Context, *mcp.ToolRequest) (mcp.Result, error) { return nil, nil },
			mcp.RequireRoles("admin"))
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	raw, _ := json.Marshal(w.Notifications())
	if strings.Contains(string(raw), "top-secret-tool") {
		t.Errorf("legacy stream leaks the gated tool's name: %s", raw)
	}
	if w.Notifications()[0].Method != "notifications/tools/list_changed" {
		t.Errorf("first notification = %s", raw)
	}
}
