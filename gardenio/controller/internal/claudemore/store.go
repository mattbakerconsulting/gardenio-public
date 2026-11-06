package claudemore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// CassetteStore persists and retrieves cassettes. The LIVE record path only
// needs Put; the Phase-2 REPLAY path (ka-0000001159) needs Get. Both are
// declared here so the store is a complete seam and REPLAY can build on it
// without touching the proxy. Implementations must be safe for concurrent use.
type CassetteStore interface {
	// Put stores the cassette keyed by its RequestHash (and model tag, for
	// versioning-by-model). It must be idempotent: re-recording the same request
	// overwrites the same key with byte-identical content (A3).
	Put(ctx context.Context, c Cassette) error
	// Get returns the cassette for the given model + request hash, or
	// ErrCassetteNotFound if absent (the REPLAY fail_on_miss signal, R2 §5).
	Get(ctx context.Context, model, hash string) (Cassette, error)
}

// ErrCassetteNotFound is returned by Get when no cassette matches. REPLAY's
// fail_on_miss turns this into a loud CI failure rather than a silent skip.
var ErrCassetteNotFound = fmt.Errorf("claudemore: cassette not found")

// s3API is the subset of *s3.Client the store uses, declared as an interface so
// tests inject an in-memory fake — mirrors the s3ImageGetter/s3Uploader pattern
// already used across the controller (cmd/beads-viewer, cmd/gb). ListObjectsV2
// is here for the drift-scan enumeration path (enumerate.go); the record/replay
// paths use only Put/Get.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// S3CassetteStore is the S3-backed CassetteStore. Layout:
//
//	<prefix>/cassettes/<model>/<hash>.json
//
// Keying by model first makes versioning-by-model-tag (R2 §5 staleness
// mitigation) a prefix listing, and a re-record of the same request overwrites
// the same object (idempotent Put). An empty model tags under `_unversioned` so
// a request whose body omitted `model` still stores deterministically.
type S3CassetteStore struct {
	s3     s3API
	bucket string
	prefix string
}

// NewS3CassetteStore builds a store over an existing S3 client. bucket must be
// non-empty; prefix defaults to the coop session prefix convention when blank so
// cassettes live alongside the other coop S3 artifacts.
func NewS3CassetteStore(client s3API, bucket, prefix string) (*S3CassetteStore, error) {
	if client == nil {
		return nil, fmt.Errorf("claudemore: nil S3 client")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("claudemore: empty S3 bucket")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = "coop/sessions"
	}
	return &S3CassetteStore{s3: client, bucket: bucket, prefix: prefix}, nil
}

// cassetteKey builds the deterministic S3 key for a (model, hash) pair. model is
// sanitized to a single safe path segment so an alias like `anthropic/claude-x`
// or one carrying odd characters cannot escape the cassettes/ subtree or create
// unintended nesting.
func (s *S3CassetteStore) cassetteKey(model, hash string) string {
	return fmt.Sprintf("%s/cassettes/%s/%s.json", s.prefix, sanitizeModelSegment(model), hash)
}

// sanitizeModelSegment collapses a model tag into one filesystem/S3-safe path
// segment: anything outside [A-Za-z0-9._-] becomes '_'. An empty tag becomes
// `_unversioned` so the key is always well-formed.
func sanitizeModelSegment(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "_unversioned"
	}
	var b strings.Builder
	b.Grow(len(model))
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Put stores the cassette. It is idempotent — the key is derived purely from the
// stable (model, hash), so a re-record overwrites in place.
func (s *S3CassetteStore) Put(ctx context.Context, c Cassette) error {
	if c.RequestHash == "" {
		return fmt.Errorf("claudemore: refusing to store cassette with empty request hash")
	}
	body, err := c.Marshal()
	if err != nil {
		return fmt.Errorf("claudemore: marshal cassette: %w", err)
	}
	key := s.cassetteKey(c.Model, c.RequestHash)
	_, err = s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("claudemore: put cassette %s: %w", key, err)
	}
	return nil
}

// Get fetches and decodes the cassette for a (model, hash) pair. A missing
// object is normalized to ErrCassetteNotFound.
func (s *S3CassetteStore) Get(ctx context.Context, model, hash string) (Cassette, error) {
	key := s.cassetteKey(model, hash)
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return Cassette{}, ErrCassetteNotFound
		}
		return Cassette{}, fmt.Errorf("claudemore: get cassette %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(out.Body); err != nil {
		return Cassette{}, fmt.Errorf("claudemore: read cassette %s: %w", key, err)
	}
	return UnmarshalCassette(buf.Bytes())
}

// isNotFound reports whether an S3 GetObject error means the object is absent.
// S3 surfaces this as either the typed NoSuchKey/NotFound exceptions or, for
// some endpoints (and MinIO), a bare "NoSuchKey"/"NotFound" APIError code —
// match both, mirroring the beads-viewer image proxy's mapping.
func isNotFound(err error) bool {
	var noKey *s3types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}
