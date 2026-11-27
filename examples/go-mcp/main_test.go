package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/auth"
	"github.com/spirilis/generic-go-mcp/config"
)

// TestOAuthExampleConfigStarts keeps config-oauth-example.yaml honest: it must load, pass every
// startup check, and grant only roles this example declares. NewAuthService makes no network
// calls (OIDC discovery is lazy), so the placeholder IdP is fine here.
func TestOAuthExampleConfigStarts(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config-oauth-example.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Auth.Storage.DBPath = filepath.Join(t.TempDir(), "oauth.db")

	res, err := cfg.Auth.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.IdentityMaxAge != 168*time.Hour {
		t.Errorf("identityMaxAge = %v, want 168h", res.IdentityMaxAge)
	}
	for _, name := range []string{"github", "corp"} {
		if _, ok := res.Provider(name); !ok {
			t.Errorf("provider %q missing", name)
		}
	}

	svc, err := auth.NewAuthService(cfg.Auth, exampleAuthOptions...)
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	defer svc.Close()

	// Without the declared roles the same config must refuse to start: proof the rules name
	// roles and that the check is live.
	cfg.Auth.Storage.DBPath = filepath.Join(t.TempDir(), "oauth2.db")
	if svc2, err := auth.NewAuthService(cfg.Auth); err == nil {
		svc2.Close()
		t.Error("example config started without the app's roles declared")
	}
}
