package claudemore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// enumerate.go adds cassette ENUMERATION on top of the (Put/Get) stores. The
// LIVE record path and the REPLAY serve path each key a SINGLE cassette by its
// (model, hash); neither needs to walk the store. The beads-viewer /cassettes
// surface does: it must visit every recorded cassette. Listing is deliberately
// a SEPARATE interface from CassetteStore rather than a third method on it —
// folding List into CassetteStore would force every existing fake store (the
// recorder/replay test doubles) to grow a method it does not use. Both concrete
// stores implement CassetteLister; a caller that only records/replays keeps
// depending on the narrow CassetteStore.

// CassetteRef identifies one stored cassette by the (model, hash) pair that keys
// it — exactly the two arguments CassetteStore.Get takes. List returns refs (not
// full cassettes) so a scan can page over a large store cheaply and Get each
// cassette body only when it is about to replay it.
type CassetteRef struct {
	Model string
	Hash  string
}

// CassetteLister enumerates the (model, hash) refs of every cassette in a store.
// It is the read-side companion to CassetteStore for callers that must walk the
// whole store (drift detection, re-record tooling) rather than address one
// cassette. Implementations must be safe for concurrent use with Put/Get.
type CassetteLister interface {
	// List returns a ref for every cassette currently in the store. Order is
	// unspecified. A store with no cassettes returns an empty slice and nil
	// error (an empty store is not an error — a fresh CI namespace has none).
	List(ctx context.Context) ([]CassetteRef, error)
}

// refFromKey parses a "(model)/(hash)" pair out of a cassette storage key that
// ends in "cassettes/<model>/<hash>.json". It returns ok=false for any key that
// does not match that layout (a stray object under the prefix, a directory
// marker) so enumeration skips foreign keys rather than manufacturing a bogus
// ref. The model segment is returned as-STORED (already sanitized at Put time);
// Get re-sanitizes deterministically, so a round-trip is stable.
func refFromKey(key string) (CassetteRef, bool) {
	// Normalize to forward slashes so the same parser handles S3 keys and
	// filepath-joined FS paths on any OS.
	key = filepath.ToSlash(key)
	const marker = "cassettes/"
	i := strings.LastIndex(key, marker)
	if i < 0 {
		return CassetteRef{}, false
	}
	rest := key[i+len(marker):] // "<model>/<hash>.json"
	if !strings.HasSuffix(rest, ".json") {
		return CassetteRef{}, false
	}
	rest = strings.TrimSuffix(rest, ".json")
	// Exactly one separator: <model>/<hash>. A model segment is a single
	// sanitized path element (sanitizeModelSegment guarantees no '/'), so any
	// other shape is not a cassette we wrote.
	model, hash, ok := strings.Cut(rest, "/")
	if !ok || model == "" || hash == "" || strings.Contains(hash, "/") {
		return CassetteRef{}, false
	}
	return CassetteRef{Model: model, Hash: hash}, true
}

// List enumerates cassette refs in the S3 store by paginating ListObjectsV2 over
// the "<prefix>/cassettes/" key space and parsing each key. Foreign objects
// under the prefix are skipped. Mirrors the paginator pattern used by the
// deathtranscript/scif stores elsewhere in the controller.
func (s *S3CassetteStore) List(ctx context.Context) ([]CassetteRef, error) {
	listPrefix := s.prefix + "/cassettes/"
	var refs []CassetteRef
	p := s3.NewListObjectsV2Paginator(s.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(listPrefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("claudemore: list cassettes s3://%s/%s: %w", s.bucket, listPrefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			if ref, ok := refFromKey(*obj.Key); ok {
				refs = append(refs, ref)
			}
		}
	}
	return refs, nil
}

// List enumerates cassette refs in the filesystem store by walking the
// "<dir>/cassettes/" subtree. A store that has recorded nothing (no cassettes/
// dir yet) returns an empty slice, not an error — mirroring the S3 store's
// behavior on an empty prefix. The walk root is derived the same way
// cassettePath builds keys, so FS and S3 enumeration agree on layout.
func (s *FSCassetteStore) List(_ context.Context) ([]CassetteRef, error) {
	root := filepath.Join(s.dir, "cassettes")
	var refs []CassetteRef
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// The cassettes/ root not existing yet just means "no cassettes".
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		// Parse relative to s.dir so refFromKey sees the same
		// "cassettes/<model>/<hash>.json" tail the S3 keys carry.
		rel, relErr := filepath.Rel(s.dir, path)
		if relErr != nil {
			rel = path
		}
		if ref, ok := refFromKey(rel); ok {
			refs = append(refs, ref)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claudemore: walk cassette dir %s: %w", root, err)
	}
	return refs, nil
}
