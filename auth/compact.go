package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// compactTxMaxSize bounds each write transaction of the copy, so a large store is not held in
// memory at once.
const compactTxMaxSize = 16 << 20

// CompactResult reports what a compaction did.
type CompactResult struct {
	// Swept is how many expired records were removed first (AuthService.CompactStore only).
	Swept int
	// BytesBefore and BytesAfter are the store file's size.
	BytesBefore, BytesAfter int64
	Duration                time.Duration
}

// Compact rewrites the store file with only its live data and puts the new file in the old
// one's place, giving back the space that deleted records leave behind: bbolt reuses freed
// pages but never shrinks its file, so a burst of records (anyone can cause one through
// /register and /authorize) would otherwise occupy the disk for good.
//
// Writes wait for the whole run. Reads carry on against the old file while the copy is made
// and wait only for the swap: close, rename, reopen. The rename is atomic, so a crash at any
// point leaves either the complete old file or the complete new one at the store's path.
//
// The copy is written beside the store as "<path>.compact", so the volume needs free space
// for the live data; with none, Compact fails and the store is left as it was. If the new
// file cannot be opened after the rename, the store is left closed, every later call fails,
// and the process should restart.
//
// Compact does not remove expired records itself; run Sweep first, or use
// AuthService.CompactStore, which does both.
func (s *BoltStorage) Compact(ctx context.Context) (CompactResult, error) {
	start := time.Now()
	var res CompactResult

	s.writeGate.Lock()
	defer s.writeGate.Unlock()
	if err := ctx.Err(); err != nil {
		return res, err
	}

	tmp := s.path + ".compact"
	_ = os.Remove(tmp) // left by a run that crashed; never the live store
	dst, err := openBolt(tmp)
	if err != nil {
		return res, fmt.Errorf("compact: create %s: %w", tmp, err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()

	// No write can start (writeGate), so this copy is the whole store.
	s.dbMu.RLock()
	if fi, err := os.Stat(s.path); err == nil {
		res.BytesBefore = fi.Size()
	}
	err = bolt.Compact(dst, s.db, compactTxMaxSize)
	s.dbMu.RUnlock()
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return res, fmt.Errorf("compact: copy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if s.compactHook != nil {
		s.compactHook()
	}

	// The swap: reads wait from here.
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if err := s.db.Close(); err != nil {
		// The old file is untouched; go back to it.
		return res, errors.Join(fmt.Errorf("compact: close store: %w", err), s.reopenLocked())
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return res, errors.Join(fmt.Errorf("compact: replace store: %w", err), s.reopenLocked())
	}
	renamed = true
	syncDir(filepath.Dir(s.path))
	if err := s.reopenLocked(); err != nil {
		return res, err
	}
	if fi, err := os.Stat(s.path); err == nil {
		res.BytesAfter = fi.Size()
	}
	res.Duration = time.Since(start)
	return res, nil
}

// reopenLocked opens the file at the store's path. The caller holds dbMu exclusively.
func (s *BoltStorage) reopenLocked() error {
	db, err := openBolt(s.path)
	if err != nil {
		return fmt.Errorf("compact: reopen store (it stays closed; restart the process): %w", err)
	}
	s.db = db
	return nil
}

// syncDir makes a rename in dir durable. Best effort: not every platform can sync a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// compacter is implemented by stores whose file can be rewritten in place.
type compacter interface {
	Compact(ctx context.Context) (CompactResult, error)
}

// ErrCompactUnsupported is returned by CompactStore when the store has no file to compact.
var ErrCompactUnsupported = errors.New("auth store does not support compaction")

// CompactStore removes expired records and then rewrites the store file so that it holds only
// live data (see BoltStorage.Compact for what blocks, and for how failures leave the store).
// It is safe to call while the service is handling requests, and from several goroutines:
// runs queue up.
//
// The service never calls this on its own. An embedder that exposes the OAuth endpoints
// should run it on a timer (hourly is plenty) or on an operator's signal such as SIGUSR1.
func (svc *AuthService) CompactStore(ctx context.Context) (CompactResult, error) {
	c, ok := svc.storage.(compacter)
	if !ok {
		return CompactResult{}, ErrCompactUnsupported
	}
	swept := 0
	if sw, ok := svc.storage.(sweeper); ok {
		n, err := sw.Sweep(ctx, time.Now(), unusedClientMaxAge)
		if err != nil {
			return CompactResult{}, fmt.Errorf("sweep before compact: %w", err)
		}
		swept = n
	}
	res, err := c.Compact(ctx)
	res.Swept = swept
	return res, err
}
