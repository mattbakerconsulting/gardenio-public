package claudemore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFSCassetteStore_PutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFSCassetteStore(dir)
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	c := newTestCassette(t, "hello fs")

	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background(), c.Model, c.RequestHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RequestHash != c.RequestHash || got.Model != c.Model {
		t.Fatalf("round-trip mismatch: got (%s,%s) want (%s,%s)", got.Model, got.RequestHash, c.Model, c.RequestHash)
	}
	if string(got.Response.Body) != string(c.Response.Body) {
		t.Fatalf("response body mismatch: got %q want %q", got.Response.Body, c.Response.Body)
	}
}

func TestFSCassetteStore_GetMissIsNotFound(t *testing.T) {
	store, err := NewFSCassetteStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	_, err = store.Get(context.Background(), "claude-sonnet-5", "deadbeef")
	if !errors.Is(err, ErrCassetteNotFound) {
		t.Fatalf("miss: got err %v, want ErrCassetteNotFound", err)
	}
}

func TestFSCassetteStore_PutIdempotentOverwrite(t *testing.T) {
	store, err := NewFSCassetteStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	c := newTestCassette(t, "idem")
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	// Re-record the same (model, hash) with a different body — must overwrite in
	// place at the same path (A3 idempotency), not create a second file.
	c.Response.Body = []byte("second-record")
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	got, err := store.Get(context.Background(), c.Model, c.RequestHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Response.Body) != "second-record" {
		t.Fatalf("overwrite: got body %q, want second-record", got.Response.Body)
	}
	// Exactly one cassette file on disk for this (model, hash).
	matches, _ := filepath.Glob(filepath.Join(store.cassettePath(c.Model, c.RequestHash)))
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 cassette file, found %d", len(matches))
	}
}

func TestFSCassetteStore_EmptyHashRejected(t *testing.T) {
	store, err := NewFSCassetteStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	c := newTestCassette(t, "x")
	c.RequestHash = ""
	if err := store.Put(context.Background(), c); err == nil {
		t.Fatal("Put with empty hash: want error, got nil")
	}
}

func TestNewFSCassetteStore_EmptyDirRejected(t *testing.T) {
	if _, err := NewFSCassetteStore("   "); err == nil {
		t.Fatal("NewFSCassetteStore(blank): want error, got nil")
	}
}

func TestNewFSCassetteStore_CreatesDir(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "a", "b", "cassettes-root")
	if _, err := NewFSCassetteStore(nested); err != nil {
		t.Fatalf("NewFSCassetteStore(nested): %v", err)
	}
	if fi, err := os.Stat(nested); err != nil || !fi.IsDir() {
		t.Fatalf("expected %s to be created as a dir (err=%v)", nested, err)
	}
}

// TestFSCassetteStore_KeyLayoutMatchesS3 pins the load-bearing invariant: the FS
// store and the S3 store derive the SAME relative key for a (model, hash), so a
// cassette recorded through one is found through the other. If either store's
// layout drifts, REPLAY silently misses.
func TestFSCassetteStore_KeyLayoutMatchesS3(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFSCassetteStore(dir)
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	s3store, err := NewS3CassetteStore(newFakeS3(), "bucket", "coop/sessions")
	if err != nil {
		t.Fatalf("NewS3CassetteStore: %v", err)
	}
	cases := []struct{ model, hash string }{
		{"claude-opus-4-8-oauth", "abc123"},
		{"anthropic/claude-haiku-4-5", "def456"}, // slash must sanitize identically
		{"", "nomodel"},                          // empty model → _unversioned in both
	}
	for _, tc := range cases {
		// Both stores must yield the SAME key relative to their root:
		// "cassettes/<model>/<hash>.json". FS root is dir; S3 root is the prefix.
		fsRel := stripPrefix(fs.cassettePath(tc.model, tc.hash), dir+string(filepath.Separator))
		s3Rel := stripPrefix(s3store.cassetteKey(tc.model, tc.hash), "coop/sessions/")
		if fsRel != s3Rel {
			t.Errorf("key layout drift for (%q,%q): fs=%q s3=%q", tc.model, tc.hash, fsRel, s3Rel)
		}
	}
}

func TestFSCassetteStore_ConcurrentPutGet(t *testing.T) {
	store, err := NewFSCassetteStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			c := newTestCassette(t, "concurrent")
			_ = store.Put(context.Background(), c)
			_, _ = store.Get(context.Background(), c.Model, c.RequestHash)
		})
	}
	wg.Wait()
	c := newTestCassette(t, "concurrent")
	if _, err := store.Get(context.Background(), c.Model, c.RequestHash); err != nil {
		t.Fatalf("final Get after concurrent access: %v", err)
	}
}

func stripPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}
