package deathtranscript

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is a hand-rolled s3API for exercising the aws-sdk-go-v2 adapter
// without touching AWS. It returns objects in (optionally) paginated pages and
// can inject errors.
type fakeS3 struct {
	// pages is the sequence of key-batches ListObjectsV2 returns; a nil
	// NextContinuationToken on the last page ends pagination.
	pages   [][]string
	listErr error

	objects map[string][]byte
	getErr  error
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// The continuation token, when present, is the index of the page to return.
	idx := 0
	if in.ContinuationToken != nil {
		// token is "1", "2", … — the paginator echoes back what we set.
		idx = int((*in.ContinuationToken)[0] - '0')
	}
	page := f.pages[idx]
	contents := make([]s3types.Object, 0, len(page))
	for _, k := range page {
		contents = append(contents, s3types.Object{Key: aws.String(k)})
	}
	out := &s3.ListObjectsV2Output{Contents: contents}
	if idx+1 < len(f.pages) {
		next := string(rune('0' + idx + 1))
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(next)
	}
	return out, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	body, ok := f.objects[*in.Key]
	if !ok {
		return nil, errors.New("NoSuchKey")
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func TestS3Store_List_Paginated(t *testing.T) {
	fake := &fakeS3{pages: [][]string{
		{"coop/sessions/gd-x/1.jsonl", "coop/sessions/gd-x/2.jsonl"},
		{"coop/sessions/gd-x/3.jsonl"},
	}}
	store := &s3Store{client: fake, bucket: "b"}

	keys, err := store.List(context.Background(), "coop/sessions/gd-x/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("List returned %d keys across pages, want 3: %v", len(keys), keys)
	}
}

func TestS3Store_List_Error(t *testing.T) {
	store := &s3Store{client: &fakeS3{listErr: errors.New("access denied")}, bucket: "b"}
	if _, err := store.List(context.Background(), "p/"); err == nil {
		t.Fatal("expected list error to propagate")
	}
}

func TestS3Store_Get(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{"p/1.jsonl": []byte("hello")}}
	store := &s3Store{client: fake, bucket: "b"}

	data, err := store.Get(context.Background(), "p/1.jsonl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("Get returned %q, want hello", data)
	}
}

func TestS3Store_Get_Error(t *testing.T) {
	store := &s3Store{client: &fakeS3{getErr: errors.New("throttled")}, bucket: "b"}
	if _, err := store.Get(context.Background(), "p/1.jsonl"); err == nil {
		t.Fatal("expected get error to propagate")
	}
}

func TestS3Store_Get_ReadCap(t *testing.T) {
	// A body at exactly the cap is read fully; one byte over is truncated to
	// cap+1 by the LimitReader (the parser then rejects it as oversize).
	fake := &fakeS3{objects: map[string][]byte{
		"p/big.jsonl": bytes.Repeat([]byte("a"), maxTranscriptBytes+100),
	}}
	store := &s3Store{client: fake, bucket: "b"}
	data, err := store.Get(context.Background(), "p/big.jsonl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(data) != maxTranscriptBytes+1 {
		t.Fatalf("read %d bytes, want cap+1 (%d)", len(data), maxTranscriptBytes+1)
	}
}

func TestNewS3Store(t *testing.T) {
	// NewS3Store must accept a real *s3.Client and satisfy ObjectStore.
	var _ ObjectStore = NewS3Store(&s3.Client{}, "bucket")
}

func TestBucketAndPrefixFromEnv(t *testing.T) {
	t.Setenv(EnvBucket, "  my-bucket  ")
	if got := BucketFromEnv(); got != "my-bucket" {
		t.Fatalf("BucketFromEnv = %q, want trimmed my-bucket", got)
	}

	t.Setenv(EnvPrefix, "")
	if got := PrefixFromEnv(); got != DefaultPrefix {
		t.Fatalf("PrefixFromEnv = %q, want default %q", got, DefaultPrefix)
	}
	t.Setenv(EnvPrefix, "custom/prefix")
	if got := PrefixFromEnv(); got != "custom/prefix" {
		t.Fatalf("PrefixFromEnv = %q, want custom/prefix", got)
	}
}

// Guard: the adapter's paginator uses the client's own token echo. Confirm a
// single-page result (no truncation) terminates cleanly.
func TestS3Store_List_SinglePage(t *testing.T) {
	fake := &fakeS3{pages: [][]string{{"only.jsonl"}}}
	store := &s3Store{client: fake, bucket: "b"}
	keys, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != "only.jsonl" {
		t.Fatalf("keys = %v, want [only.jsonl]", keys)
	}
}
