package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func boltOf(t *testing.T, svc *AuthService) *BoltStorage {
	t.Helper()
	s, ok := svc.storage.(*BoltStorage)
	if !ok {
		t.Fatalf("storage is %T", svc.storage)
	}
	return s
}

// floodAuthRequests stores n already-expired pending authorization requests, as an anonymous
// caller hammering /authorize would.
func floodAuthRequests(t *testing.T, s *BoltStorage, n int) {
	t.Helper()
	pad := strings.Repeat("s", maxStateLen)
	for i := 0; i < n; i++ {
		err := s.StoreAuthRequest(context.Background(), &PendingAuthRequest{
			ID: fmt.Sprintf("req-%06d", i), ClientID: "client-1", State: pad,
			CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The reason Compact exists: sweeping a flood empties the buckets but leaves the file as big
// as the flood made it. Compacting gives the space back and keeps every live record.
func TestCompactStoreShrinksFileAndKeepsLiveData(t *testing.T) {
	svc, client := newTestAuthService(t)
	s := boltOf(t, svc)
	ctx := context.Background()
	rt, err := svc.tokenService.GenerateRefreshToken("user-1", client.ClientID, "mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	floodAuthRequests(t, s, 500)

	if _, err := s.Sweep(ctx, time.Now(), unusedClientMaxAge); err != nil {
		t.Fatal(err)
	}
	swept, _ := os.Stat(s.path)

	res, err := svc.CompactStore(ctx)
	if err != nil {
		t.Fatalf("CompactStore: %v", err)
	}
	after, _ := os.Stat(s.path)
	if res.BytesBefore != swept.Size() || res.BytesAfter != after.Size() {
		t.Errorf("result %+v does not match the file sizes %d -> %d", res, swept.Size(), after.Size())
	}
	if after.Size() >= swept.Size()/4 {
		t.Errorf("file did not shrink: %d -> %d bytes", swept.Size(), after.Size())
	}
	if _, err := os.Stat(s.path + ".compact"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
	if fi, _ := os.Stat(s.path); fi.Mode().Perm() != 0600 {
		t.Errorf("store mode = %v, want 0600", fi.Mode().Perm())
	}

	// Live data survived, and the store still takes writes.
	if c, err := s.GetClient(ctx, client.ClientID); err != nil || c == nil {
		t.Fatalf("client lost: %v %v", c, err)
	}
	if got, err := s.GetRefreshToken(ctx, rt.Token); err != nil || got == nil || got.UserID != "user-1" {
		t.Fatalf("refresh token lost: %+v %v", got, err)
	}
	if err := s.StoreConsent(ctx, "user-1", client.ClientID); err != nil {
		t.Fatalf("write after compact: %v", err)
	}
	if ok, _ := s.HasConsent(ctx, "user-1", client.ClientID); !ok {
		t.Fatal("write after compact was not kept")
	}
}

// CompactStore sweeps first, so one call is enough after a flood.
func TestCompactStoreSweepsFirst(t *testing.T) {
	svc, _ := newTestAuthService(t)
	floodAuthRequests(t, boltOf(t, svc), 50)
	res, err := svc.CompactStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The service's own background sweep may have taken some of them already.
	if res.Swept > 50 {
		t.Errorf("Swept = %d, want at most 50", res.Swept)
	}
	for _, id := range []string{"req-000000", "req-000049"} {
		if r, _ := svc.storage.GetAuthRequest(context.Background(), id); r != nil {
			t.Errorf("expired request %s survived CompactStore", id)
		}
	}
}

// Between the copy and the swap a read must still be answered (from the old file) and a write
// must wait until the compaction is over, then land in the new file.
func TestCompactBlocksWritesNotReads(t *testing.T) {
	svc, client := newTestAuthService(t)
	s := boltOf(t, svc)
	ctx := context.Background()

	var wrote atomic.Bool
	writeDone := make(chan error, 1)
	s.compactHook = func() {
		if c, err := s.GetClient(ctx, client.ClientID); err != nil || c == nil {
			t.Errorf("read during compaction: %v %v", c, err)
		}
		go func() {
			err := s.StoreConsent(ctx, "user-late", client.ClientID)
			wrote.Store(true)
			writeDone <- err
		}()
		time.Sleep(100 * time.Millisecond)
		if wrote.Load() {
			t.Error("a write completed while the compaction was running")
		}
	}
	if _, err := s.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write after compaction: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting write never ran")
	}
	if ok, _ := s.HasConsent(ctx, "user-late", client.ClientID); !ok {
		t.Fatal("the write that waited is missing from the compacted store")
	}
}

// Requests keep working, and lose nothing, while compactions run back to back.
func TestCompactUnderLoad(t *testing.T) {
	svc, client := newTestAuthService(t)
	s := boltOf(t, svc)
	ctx := context.Background()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var written atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := s.StoreConsent(ctx, fmt.Sprintf("u%d-%d", w, i), client.ClientID); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				written.Add(1)
				if c, err := s.GetClient(ctx, client.ClientID); err != nil || c == nil {
					t.Errorf("read: %v %v", c, err)
					return
				}
			}
		}(w)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.Compact(ctx); err != nil {
			t.Fatalf("Compact %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	n := 0
	for w := 0; w < 4; w++ {
		for i := 0; ; i++ {
			ok, err := s.HasConsent(ctx, fmt.Sprintf("u%d-%d", w, i), client.ClientID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			n++
		}
	}
	if int64(n) != written.Load() {
		t.Errorf("%d writes acknowledged, %d found after compaction", written.Load(), n)
	}
}

// A leftover temporary file from a crashed run is replaced, never adopted; a cancelled run
// changes nothing.
func TestCompactLeftoverAndCancel(t *testing.T) {
	svc, client := newTestAuthService(t)
	s := boltOf(t, svc)
	if err := os.WriteFile(s.path+".compact", []byte("garbage from a crashed run"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(context.Background()); err != nil {
		t.Fatalf("Compact with a leftover file: %v", err)
	}
	if c, _ := s.GetClient(context.Background(), client.ClientID); c == nil {
		t.Fatal("client lost")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Compact(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Compact: %v", err)
	}
	if c, _ := s.GetClient(context.Background(), client.ClientID); c == nil {
		t.Fatal("client lost after a cancelled run")
	}
}

// A run that cannot even create its temporary file leaves the store working.
func TestCompactFailureLeavesStoreUsable(t *testing.T) {
	svc, client := newTestAuthService(t)
	s := boltOf(t, svc)
	ctx := context.Background()
	// A non-empty directory in the temporary file's place can be neither removed nor opened.
	if err := os.MkdirAll(s.path+".compact/x", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx); err == nil {
		t.Fatal("Compact succeeded although its temporary file could not be created")
	}
	if c, err := s.GetClient(ctx, client.ClientID); err != nil || c == nil {
		t.Fatalf("read after a failed run: %v %v", c, err)
	}
	if err := s.StoreConsent(ctx, "user-1", client.ClientID); err != nil {
		t.Fatalf("write after a failed run: %v", err)
	}
}

// failingStore refuses to store tokens, as a full disk would.
type failingStore struct{ Storage }

func (failingStore) StoreAccessToken(context.Context, *AccessToken) error {
	return errors.New("disk full")
}

// A refresh that cannot store its new tokens must answer with an error and leave the old
// refresh token usable. It used to delete the token and then crash.
func TestRefreshGrantSurvivesStoreFailure(t *testing.T) {
	svc, client := newTestAuthService(t)
	testUser(t, svc) // the refresh grant re-checks the user against the access rules
	rt, err := svc.tokenService.GenerateRefreshToken("user-1", client.ClientID, "mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	good := svc.tokenService
	svc.tokenService = NewTokenService(svc.config.Issuer, failingStore{svc.storage})

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt.Token},
		"client_id": {client.ClientID}, "client_secret": {"s3cret"}}
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		svc.handleToken(rec, req)
		return rec
	}
	if rec := post(); rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "server_error") {
		t.Fatalf("failed refresh answered %d %s", rec.Code, rec.Body)
	}
	svc.tokenService = good
	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("the refresh token was burned by the failed attempt: %d %s", rec.Code, rec.Body)
	}
}
