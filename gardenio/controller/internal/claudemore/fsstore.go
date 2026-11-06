package claudemore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FSCassetteStore is a filesystem-backed CassetteStore. It mirrors
// S3CassetteStore's key layout exactly —
//
//	<dir>/cassettes/<model>/<hash>.json
//
// — so the record path (Put) and the REPLAY serve path (Get) agree on where a
// cassette lives, independent of whether the backing store is S3 or a local
// directory. The two share sanitizeModelSegment, so a request recorded against
// one store resolves against the other for the same (model, hash).
//
// Why a filesystem store exists alongside the S3 one:
//
//   - Gasbox / BYO (ka-0000005379): the in-box single-EC2 product has ZERO
//     external cloud deps — no S3. A local-dir cassette store lets claudemore
//     run REPLAY there without MinIO/S3 wiring.
//   - The Phase-2 hooks e2e (ka-0000004452) drives the REAL `claude` binary
//     against REPLAY hermetically in CI, where there is no S3/MinIO. It records
//     cassettes to a temp dir against a deterministic stub upstream, then
//     replays them from that same dir in one run ($0, no drift, no network).
//
// The store is safe for concurrent use: Put writes to a temp file and renames
// it into place (atomic on POSIX), so a concurrent Get never observes a
// half-written cassette.
type FSCassetteStore struct {
	dir string
}

// NewFSCassetteStore builds a filesystem store rooted at dir. dir must be
// non-empty; it is created (MkdirAll) so a caller can point at a fresh temp
// directory without pre-creating it.
func NewFSCassetteStore(dir string) (*FSCassetteStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("claudemore: empty cassette directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("claudemore: create cassette dir %s: %w", dir, err)
	}
	return &FSCassetteStore{dir: dir}, nil
}

// cassettePath builds the deterministic on-disk path for a (model, hash) pair.
// It reuses sanitizeModelSegment so the model segment can never escape the
// cassettes/ subtree (path traversal via a crafted model tag). filepath.Base on
// the hash is a belt-and-suspenders guard: hashes are hex, but a caller-supplied
// value must never introduce a path separator.
func (s *FSCassetteStore) cassettePath(model, hash string) string {
	return filepath.Join(s.dir, "cassettes", sanitizeModelSegment(model), filepath.Base(hash)+".json")
}

// Put stores the cassette, keyed by its (model, RequestHash). It is idempotent:
// the path is derived purely from the stable key, so a re-record overwrites in
// place with byte-identical content (A3). The write is atomic (temp file +
// rename) so a concurrent Get never sees a partial cassette.
func (s *FSCassetteStore) Put(_ context.Context, c Cassette) error {
	if c.RequestHash == "" {
		return fmt.Errorf("claudemore: refusing to store cassette with empty request hash")
	}
	body, err := c.Marshal()
	if err != nil {
		return fmt.Errorf("claudemore: marshal cassette: %w", err)
	}
	path := s.cassettePath(c.Model, c.RequestHash)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("claudemore: create cassette subdir for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cassette-*.tmp")
	if err != nil {
		return fmt.Errorf("claudemore: create temp cassette for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("claudemore: write temp cassette %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("claudemore: close temp cassette %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("claudemore: rename cassette into place %s: %w", path, err)
	}
	return nil
}

// Get fetches and decodes the cassette for a (model, hash) pair. A missing file
// is normalized to ErrCassetteNotFound — the REPLAY fail_on_miss signal — so the
// filesystem and S3 stores present an identical not-found contract.
func (s *FSCassetteStore) Get(_ context.Context, model, hash string) (Cassette, error) {
	path := s.cassettePath(model, hash)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Cassette{}, ErrCassetteNotFound
		}
		return Cassette{}, fmt.Errorf("claudemore: read cassette %s: %w", path, err)
	}
	return UnmarshalCassette(body)
}
