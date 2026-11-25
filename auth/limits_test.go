package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func clientCount(t *testing.T, svc *AuthService) int {
	t.Helper()
	cs, err := svc.storage.ListClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(cs)
}

// /register is open to anyone: an oversized body or field must be refused and nothing kept.
func TestRegistrationIsBounded(t *testing.T) {
	svc, _ := newTestAuthService(t)
	before := clientCount(t, svc)
	for name, body := range map[string]string{
		"huge body":      `{"redirect_uris":["https://a.example/cb"],"client_name":"` + strings.Repeat("A", 200_000) + `"}`,
		"long name":      `{"redirect_uris":["https://a.example/cb"],"client_name":"` + strings.Repeat("A", maxClientNameLen+1) + `"}`,
		"many redirects": `{"redirect_uris":[` + strings.TrimSuffix(strings.Repeat(`"https://a.example/cb",`, maxRedirectURIs+1), ",") + `]}`,
		"long redirect":  `{"redirect_uris":["https://a.example/` + strings.Repeat("p", maxURILen) + `"]}`,
	} {
		rec := registerJSON(t, svc, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if got := clientCount(t, svc); got != before {
		t.Fatalf("refused registrations were stored: %d -> %d", before, got)
	}
	if rec := registerJSON(t, svc, `{"redirect_uris":["https://a.example/cb"],"client_name":"ok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("an ordinary registration must still work: %d %s", rec.Code, rec.Body)
	}
}

// /authorize stores the request until the callback: an oversized one must not be stored.
func TestAuthorizeIsBounded(t *testing.T) {
	svc, client := newTestAuthService(t)
	q := url.Values{"client_id": {client.ClientID}, "redirect_uri": {client.RedirectURIs[0]},
		"response_type": {"code"}, "code_challenge": {"abc"}, "state": {strings.Repeat("s", maxStateLen+1)}}
	if rec := authorizeGET(t, svc, q); rec.Code != http.StatusBadRequest {
		t.Fatalf("long state: status %d, want 400", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader("state="+strings.Repeat("s", 200_000)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	svc.handleAuthorize(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized form: status %d, want 400", rec.Code)
	}
	q.Set("state", "ok")
	if rec := authorizeGET(t, svc, q); rec.Code != http.StatusFound {
		t.Fatalf("an ordinary request must still redirect: %d", rec.Code)
	}
}

// Sweep drops what has expired and clients nobody used, and keeps everything else.
func TestSweep(t *testing.T) {
	svc, static := newTestAuthService(t)
	svc.stopSweeper() // the test drives Sweep itself
	ctx := context.Background()
	st := svc.storage.(*BoltStorage)
	now := time.Now()
	old := now.Add(-48 * time.Hour)

	put := func(id string, created time.Time) {
		if err := st.StoreClient(ctx, &RegisteredClient{ClientID: id, RedirectURIs: []string{"https://x/cb"}, CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
	}
	put("unused-old", old)
	put("unused-new", now)
	put("consented-old", old)
	put("has-refresh-old", old)
	put("pending-old", old)
	_ = st.StoreConsent(ctx, "user-1", "consented-old")
	_ = st.StoreRefreshToken(ctx, &RefreshToken{Token: "rt-live", ClientID: "has-refresh-old", ExpiresAt: now.Add(time.Hour)})
	_ = st.StoreRefreshToken(ctx, &RefreshToken{Token: "rt-dead", ClientID: "unused-old", ExpiresAt: now.Add(-time.Hour)})
	_ = st.StoreAccessToken(ctx, &AccessToken{Token: "at-dead", ExpiresAt: now.Add(-time.Hour)})
	_ = st.StoreAuthRequest(ctx, &PendingAuthRequest{ID: "req-dead", ExpiresAt: now.Add(-time.Minute)})
	_ = st.StoreAuthRequest(ctx, &PendingAuthRequest{ID: "req-live", ClientID: "pending-old", ExpiresAt: now.Add(time.Minute)})

	n, err := st.Sweep(ctx, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // rt-dead, at-dead, req-dead, unused-old
		t.Errorf("removed %d records, want 4", n)
	}
	for id, want := range map[string]bool{"unused-old": false, "unused-new": true, "consented-old": true,
		"has-refresh-old": true, "pending-old": true, static.ClientID: true} {
		c, _ := st.GetClient(ctx, id)
		if (c != nil) != want {
			t.Errorf("client %s present=%v, want %v", id, c != nil, want)
		}
	}
	if r, _ := st.GetAuthRequest(ctx, "req-dead"); r != nil {
		t.Error("expired auth request survived")
	}
	if r, _ := st.GetAuthRequest(ctx, "req-live"); r == nil {
		t.Error("live auth request was removed")
	}
	if r, _ := st.GetRefreshToken(ctx, "rt-live"); r == nil {
		t.Error("live refresh token was removed")
	}
}
