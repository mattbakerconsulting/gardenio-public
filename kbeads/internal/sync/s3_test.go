package sync

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is a minimal in-memory S3-compatible HTTP server that supports
// path-style PutObject and GetObject. It records every PUT for assertions.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte         // path => body
	headers map[string]http.Header    // path => request headers (last PUT)
	puts    []putRecord
}

type putRecord struct {
	Path        string
	ContentType string
	Body        []byte
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		objects: make(map[string][]byte),
		headers: make(map[string]http.Header),
	}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		f.mu.Lock()
		f.objects[r.URL.Path] = body
		f.headers[r.URL.Path] = r.Header.Clone()
		f.puts = append(f.puts, putRecord{
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			Body:        append([]byte(nil), body...),
		})
		f.mu.Unlock()

		w.Header().Set("ETag", `"deadbeef"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// setS3TestEnv sets static dummy AWS credentials so LoadDefaultConfig
// inside NewS3Destination succeeds without scanning the host's
// ~/.aws config or instance metadata.
func setS3TestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	// Prevent LoadDefaultConfig from picking up a real shared-config file.
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestNewS3Destination_NoEndpoint(t *testing.T) {
	setS3TestEnv(t)
	dest, err := NewS3Destination(context.Background(), "my-bucket", "path/to/key.jsonl", "us-west-2", "")
	if err != nil {
		t.Fatalf("NewS3Destination: %v", err)
	}
	if dest.bucket != "my-bucket" {
		t.Errorf("bucket = %q, want %q", dest.bucket, "my-bucket")
	}
	if dest.key != "path/to/key.jsonl" {
		t.Errorf("key = %q, want %q", dest.key, "path/to/key.jsonl")
	}
	if dest.client == nil {
		t.Fatal("client is nil")
	}
}

func TestNewS3Destination_WithEndpoint(t *testing.T) {
	setS3TestEnv(t)
	dest, err := NewS3Destination(context.Background(), "b", "k", "us-east-1", "http://minio:9000")
	if err != nil {
		t.Fatalf("NewS3Destination: %v", err)
	}
	if dest.client == nil {
		t.Fatal("client is nil")
	}
}

func TestS3Destination_Write(t *testing.T) {
	setS3TestEnv(t)
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	dest, err := NewS3Destination(context.Background(), "beads-sync", "beads.jsonl", "us-east-1", srv.URL)
	if err != nil {
		t.Fatalf("NewS3Destination: %v", err)
	}

	payload := []byte(`{"id":"gd-1","type":"task"}` + "\n" + `{"id":"gd-2","type":"epic"}` + "\n")
	if err := dest.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if len(fake.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", len(fake.puts))
	}
	got := fake.puts[0]

	wantPath := "/beads-sync/beads.jsonl"
	if got.Path != wantPath {
		t.Errorf("PUT path = %q, want %q", got.Path, wantPath)
	}
	if got.ContentType != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", got.ContentType)
	}
	if string(got.Body) != string(payload) {
		t.Errorf("body mismatch: got %q want %q", got.Body, payload)
	}
}

func TestS3Destination_Write_RoundTrip(t *testing.T) {
	setS3TestEnv(t)
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	dest, err := NewS3Destination(context.Background(), "round-trip", "out/data.jsonl", "us-east-1", srv.URL)
	if err != nil {
		t.Fatalf("NewS3Destination: %v", err)
	}

	payload := []byte(strings.Join([]string{
		`{"meta":"header"}`,
		`{"id":"gd-A","title":"alpha"}`,
		`{"id":"gd-B","title":"beta"}`,
		``,
	}, "\n"))

	if err := dest.Write(context.Background(), payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read back via the same fake server. Use a plain HTTP GET against the
	// path-style URL — this is what any consumer (gb audit) would do via
	// the S3 GetObject API on the same bucket/key.
	resp, err := http.Get(srv.URL + "/round-trip/out/data.jsonl")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("response Content-Type = %q, want application/x-ndjson", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(payload) {
		t.Errorf("round-trip body mismatch:\n got %q\nwant %q", body, payload)
	}

	// Also assert the headers captured during the PUT preserved Content-Type.
	fake.mu.Lock()
	hdrs := fake.headers["/round-trip/out/data.jsonl"]
	fake.mu.Unlock()
	if hdrs == nil {
		t.Fatal("no headers captured for PUT")
	}
	if got := hdrs.Get("Content-Type"); got != "application/x-ndjson" {
		t.Errorf("captured PUT Content-Type = %q, want application/x-ndjson", got)
	}
}

func TestS3Destination_Write_ServerError(t *testing.T) {
	setS3TestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dest, err := NewS3Destination(context.Background(), "b", "k", "us-east-1", srv.URL)
	if err != nil {
		t.Fatalf("NewS3Destination: %v", err)
	}

	err = dest.Write(context.Background(), []byte("data"))
	if err == nil {
		t.Fatal("expected error from Write, got nil")
	}
	if !strings.Contains(err.Error(), "s3 put object") {
		t.Errorf("error = %q, want it to wrap with 's3 put object'", err)
	}
}
