package claudemore

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// refKey renders a ref as "model/hash" so a []CassetteRef can be compared as a
// sorted set independent of enumeration order.
func refKey(r CassetteRef) string { return r.Model + "/" + r.Hash }

func sortedRefKeys(refs []CassetteRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, refKey(r))
	}
	sort.Strings(out)
	return out
}

func TestRefFromKey(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		want  CassetteRef
		wantK bool
	}{
		{"s3 key", "coop/sessions/cassettes/claude-sonnet-5/abc123.json", CassetteRef{"claude-sonnet-5", "abc123"}, true},
		{"fs relative", "cassettes/claude-opus-4-8-oauth/deadbeef.json", CassetteRef{"claude-opus-4-8-oauth", "deadbeef"}, true},
		{"unversioned", "p/cassettes/_unversioned/h1.json", CassetteRef{"_unversioned", "h1"}, true},
		{"no cassettes marker", "coop/sessions/other/x.json", CassetteRef{}, false},
		{"not json", "cassettes/model/hash.txt", CassetteRef{}, false},
		{"missing hash", "cassettes/model.json", CassetteRef{}, false},
		{"empty model", "cassettes//hash.json", CassetteRef{}, false},
		{"extra nesting", "cassettes/model/sub/hash.json", CassetteRef{}, false},
		{"dir marker", "coop/sessions/cassettes/", CassetteRef{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := refFromKey(tc.key)
			if ok != tc.wantK {
				t.Fatalf("ok: got %v want %v (ref %+v)", ok, tc.wantK, got)
			}
			if ok && got != tc.want {
				t.Errorf("ref: got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestFSCassetteStore_List(t *testing.T) {
	store, err := NewFSCassetteStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	ctx := context.Background()

	// Empty store: no cassettes/ dir yet → empty, no error.
	refs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List(empty): %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("List(empty): got %d refs, want 0", len(refs))
	}

	want := []Cassette{
		newTestCassette(t, "one"),
		newTestCassette(t, "two"),
		newTestCassette(t, "three"),
	}
	for _, c := range want {
		if err := store.Put(ctx, c); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	refs, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantKeys := make([]CassetteRef, len(want))
	for i, c := range want {
		wantKeys[i] = CassetteRef{Model: c.Model, Hash: c.RequestHash}
	}
	if got, exp := sortedRefKeys(refs), sortedRefKeys(wantKeys); !equalStrings(got, exp) {
		t.Errorf("List refs: got %v want %v", got, exp)
	}

	// Every listed ref must resolve via Get — proving List refs are addressable.
	for _, ref := range refs {
		if _, err := store.Get(ctx, ref.Model, ref.Hash); err != nil {
			t.Errorf("Get(%+v) after List: %v", ref, err)
		}
	}
}

// TestFSCassetteStore_ListIgnoresForeignFiles proves the walk skips stray
// non-cassette files that share the cassettes/ subtree.
func TestFSCassetteStore_ListIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFSCassetteStore(dir)
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	ctx := context.Background()
	c := newTestCassette(t, "real")
	if err := store.Put(ctx, c); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Drop a stray .txt and a stray .json that is not under a model dir.
	foreignDir := filepath.Join(dir, "cassettes", c.Model)
	if err := os.WriteFile(filepath.Join(foreignDir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cassettes", "loose.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	refs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Hash != c.RequestHash {
		t.Errorf("List should return only the one real cassette, got %+v", refs)
	}
}

func TestS3CassetteStore_List(t *testing.T) {
	fake := newFakeS3()
	store, err := NewS3CassetteStore(fake, "bkt", "coop/sessions")
	if err != nil {
		t.Fatalf("NewS3CassetteStore: %v", err)
	}
	ctx := context.Background()

	// Empty store.
	refs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List(empty): %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("List(empty): got %d, want 0", len(refs))
	}

	want := []Cassette{
		newTestCassette(t, "alpha"),
		newTestCassette(t, "beta"),
	}
	for _, c := range want {
		if err := store.Put(ctx, c); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// A foreign object under the same prefix but outside cassettes/ must be
	// ignored by List.
	fake.objects["coop/sessions/other/stray.json"] = []byte("{}")

	refs, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantKeys := make([]CassetteRef, len(want))
	for i, c := range want {
		wantKeys[i] = CassetteRef{Model: c.Model, Hash: c.RequestHash}
	}
	if got, exp := sortedRefKeys(refs), sortedRefKeys(wantKeys); !equalStrings(got, exp) {
		t.Errorf("List refs: got %v want %v", got, exp)
	}
}

// TestListKeyLayoutAgrees proves FS and S3 enumeration return the SAME refs for
// the same recorded cassettes — the load-bearing invariant that a drift scan
// against either store sees identical (model, hash) coverage.
func TestListKeyLayoutAgrees(t *testing.T) {
	ctx := context.Background()
	fs, err := NewFSCassetteStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	s3store, err := NewS3CassetteStore(newFakeS3(), "bkt", "coop/sessions")
	if err != nil {
		t.Fatalf("NewS3CassetteStore: %v", err)
	}
	for _, prompt := range []string{"x", "y", "z"} {
		c := newTestCassette(t, prompt)
		if err := fs.Put(ctx, c); err != nil {
			t.Fatalf("fs Put: %v", err)
		}
		if err := s3store.Put(ctx, c); err != nil {
			t.Fatalf("s3 Put: %v", err)
		}
	}
	fsRefs, err := fs.List(ctx)
	if err != nil {
		t.Fatalf("fs List: %v", err)
	}
	s3Refs, err := s3store.List(ctx)
	if err != nil {
		t.Fatalf("s3 List: %v", err)
	}
	if got, exp := sortedRefKeys(fsRefs), sortedRefKeys(s3Refs); !equalStrings(got, exp) {
		t.Errorf("FS vs S3 enumeration diverged: fs=%v s3=%v", got, exp)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
