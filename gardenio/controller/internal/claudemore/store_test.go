package claudemore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is an in-memory s3API for store tests — mirrors the fake-client pattern
// used elsewhere in the controller (cmd/beads-viewer image proxy).
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[aws.ToString(in.Key)] = b
	f.puts++
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

// ListObjectsV2 returns, in a single (non-paginated) page, every stored key that
// has the requested prefix. The in-memory store is small, so one page is
// sufficient to exercise the enumeration path; the real S3 client paginates and
// the paginator loop in S3CassetteStore.List handles both.
func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := aws.ToString(in.Prefix)
	var contents []s3types.Object
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			contents = append(contents, s3types.Object{Key: aws.String(k)})
		}
	}
	return &s3.ListObjectsV2Output{Contents: contents}, nil
}

func newTestCassette(t *testing.T, prompt string) Cassette {
	t.Helper()
	req, model, hash, err := CanonicalizeRequest(RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"` + prompt + `"}]}`),
	})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return NewCassette(req, model, hash, CassetteResponse{StatusCode: 200, Body: []byte("resp-" + prompt)}, 1700000000000)
}

func TestS3CassetteStore_PutGetRoundTrip(t *testing.T) {
	fake := newFakeS3()
	store, err := NewS3CassetteStore(fake, "bkt", "coop/sessions")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	c := newTestCassette(t, "hello")
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.Get(context.Background(), c.Model, c.RequestHash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RequestHash != c.RequestHash || string(got.Response.Body) != "resp-hello" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

// TestS3CassetteStore_PutIdempotent is the A3 store-level property: re-recording
// the same request writes the SAME key with byte-identical content.
func TestS3CassetteStore_PutIdempotent(t *testing.T) {
	fake := newFakeS3()
	store, _ := NewS3CassetteStore(fake, "bkt", "")
	c := newTestCassette(t, "hello")
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put1: %v", err)
	}
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put2: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.objects) != 1 {
		t.Errorf("idempotent re-record created %d objects, want 1: %v keys", len(fake.objects), keysOf(fake.objects))
	}
}

func TestS3CassetteStore_GetMissingIsNotFound(t *testing.T) {
	fake := newFakeS3()
	store, _ := NewS3CassetteStore(fake, "bkt", "")
	_, err := store.Get(context.Background(), "m", "nope")
	if !errors.Is(err, ErrCassetteNotFound) {
		t.Errorf("missing cassette: got %v want ErrCassetteNotFound", err)
	}
}

// TestS3CassetteStore_KeyLayout locks the on-disk key shape so REPLAY (a
// separate reader) and versioning-by-model stay stable.
func TestS3CassetteStore_KeyLayout(t *testing.T) {
	fake := newFakeS3()
	store, _ := NewS3CassetteStore(fake, "bkt", "coop/sessions")
	c := newTestCassette(t, "hi")
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put: %v", err)
	}
	var key string
	for k := range fake.objects {
		key = k
	}
	wantPrefix := "coop/sessions/cassettes/claude-sonnet-5/"
	if !strings.HasPrefix(key, wantPrefix) || !strings.HasSuffix(key, ".json") {
		t.Errorf("key %q does not match %s<hash>.json", key, wantPrefix)
	}
}

func TestS3CassetteStore_UnversionedModel(t *testing.T) {
	fake := newFakeS3()
	store, _ := NewS3CassetteStore(fake, "bkt", "")
	req, model, hash, _ := CanonicalizeRequest(RequestIdentity{
		Method: "POST", Path: "/v1/messages", Body: []byte(`{"messages":[]}`), // no model
	})
	c := NewCassette(req, model, hash, CassetteResponse{StatusCode: 200}, 1)
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put: %v", err)
	}
	for k := range fake.objects {
		if !strings.Contains(k, "/cassettes/_unversioned/") {
			t.Errorf("empty model should tag _unversioned, got key %q", k)
		}
	}
}

func TestS3CassetteStore_SanitizesModelSegment(t *testing.T) {
	fake := newFakeS3()
	store, _ := NewS3CassetteStore(fake, "bkt", "p")
	req, _, hash, _ := CanonicalizeRequest(RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"anthropic/claude-x:beta","messages":[]}`),
	})
	c := NewCassette(req, "anthropic/claude-x:beta", hash, CassetteResponse{StatusCode: 200}, 1)
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put: %v", err)
	}
	for k := range fake.objects {
		if strings.Contains(k, "cassettes/anthropic/claude-x:beta") {
			t.Errorf("unsanitized model created nested/odd key: %q", k)
		}
		if !strings.Contains(k, "cassettes/anthropic_claude-x_beta/") {
			t.Errorf("model not sanitized to a single safe segment: %q", k)
		}
	}
}

func TestS3CassetteStore_RejectsBadConstruction(t *testing.T) {
	if _, err := NewS3CassetteStore(nil, "bkt", "p"); err == nil {
		t.Error("nil client should error")
	}
	if _, err := NewS3CassetteStore(newFakeS3(), "  ", "p"); err == nil {
		t.Error("empty bucket should error")
	}
}

func TestS3CassetteStore_PutRejectsEmptyHash(t *testing.T) {
	store, _ := NewS3CassetteStore(newFakeS3(), "bkt", "p")
	c := NewCassette(CassetteRequest{}, "m", "", CassetteResponse{StatusCode: 200}, 1)
	if err := store.Put(context.Background(), c); err == nil {
		t.Error("empty hash should be refused")
	}
}

// TestS3CassetteStore_StoredContentIsValidCassette proves the persisted bytes are
// a well-formed cassette (a REPLAY reader can parse them).
func TestS3CassetteStore_StoredContentIsValidCassette(t *testing.T) {
	fake := newFakeS3()
	store, _ := NewS3CassetteStore(fake, "bkt", "p")
	c := newTestCassette(t, "hi")
	if err := store.Put(context.Background(), c); err != nil {
		t.Fatalf("put: %v", err)
	}
	for _, raw := range fake.objects {
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("stored bytes are not valid JSON: %v", err)
		}
		if _, err := UnmarshalCassette(raw); err != nil {
			t.Fatalf("stored bytes are not a valid cassette: %v", err)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
