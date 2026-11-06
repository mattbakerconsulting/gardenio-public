package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestLocalResumeLogLoadable(t *testing.T) {
	dir := t.TempDir()

	tiny := dir + "/tiny.jsonl"
	if err := os.WriteFile(tiny, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	full := dir + "/full.jsonl"
	if err := os.WriteFile(full, bytes.Repeat([]byte("x"), minResumeLogBytes+8), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"empty path", "", false},
		{"missing file", dir + "/missing.jsonl", false},
		{"directory", dir, false},
		{"tiny file below threshold", tiny, false},
		{"full file at threshold", full, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := localResumeLogLoadable(tt.path); got != tt.want {
				t.Errorf("localResumeLogLoadable(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// captureS3ResumeOutcomes installs a recorder stub that buffers every
// outcome until t finishes. Returns a pointer to the slice (read after
// the call under test).
func captureS3ResumeOutcomes(t *testing.T) *[]s3ResumeOutcome {
	t.Helper()
	var (
		mu  sync.Mutex
		out []s3ResumeOutcome
	)
	prev := s3ResumeOutcomeRecorder
	s3ResumeOutcomeRecorder = func(_ context.Context, outcome s3ResumeOutcome, _ string, _ string, _ error) {
		mu.Lock()
		out = append(out, outcome)
		mu.Unlock()
	}
	t.Cleanup(func() { s3ResumeOutcomeRecorder = prev })
	return &out
}

func TestClassifyS3ResumeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want s3ResumeOutcome
	}{
		{"DeadlineExceeded", context.DeadlineExceeded, s3ResumeOutcomeListTimeout},
		{"Canceled", context.Canceled, s3ResumeOutcomeListTimeout},
		{"WrappedTimeout",
			fmt.Errorf("operation error S3: ListObjectsV2: %w", context.DeadlineExceeded),
			s3ResumeOutcomeListTimeout},
		{"MessageOnlyTimeout",
			errors.New(`Get "https://s3...": context deadline exceeded`),
			s3ResumeOutcomeListTimeout},
		{"AccessDenied", errors.New("AccessDenied: not allowed"), s3ResumeOutcomeListOther},
		{"GenericError", errors.New("boom"), s3ResumeOutcomeListOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyS3ResumeError(tc.err)
			if got != tc.want {
				t.Errorf("classifyS3ResumeError(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestS3ResumeSessionExists_NoBucketConfig covers the no-bucket-config
// outcome: COOP_S3_BUCKET unset. Must short-circuit, return false, and
// emit a no-bucket-config outcome (not an error outcome).
func TestS3ResumeSessionExists_NoBucketConfig(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("VMAGENT_URL", "")
	outcomes := captureS3ResumeOutcomes(t)

	if got := s3ResumeSessionExists(context.Background(), "sess-x"); got {
		t.Errorf("expected false when no bucket configured, got true")
	}
	if len(*outcomes) != 1 || (*outcomes)[0] != s3ResumeOutcomeNoBucket {
		t.Errorf("got outcomes %v, want [%s]", *outcomes, s3ResumeOutcomeNoBucket)
	}
}

// TestS3ResumeSessionExists_EmptySessionID covers the same short-circuit
// path when bucket is set but sessionID is empty.
func TestS3ResumeSessionExists_EmptySessionID(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "b")
	t.Setenv("VMAGENT_URL", "")
	outcomes := captureS3ResumeOutcomes(t)

	if got := s3ResumeSessionExists(context.Background(), ""); got {
		t.Errorf("expected false on empty sessionID, got true")
	}
	if len(*outcomes) != 1 || (*outcomes)[0] != s3ResumeOutcomeNoBucket {
		t.Errorf("got outcomes %v, want [%s]", *outcomes, s3ResumeOutcomeNoBucket)
	}
}

// TestPushS3ResumeOutcomeMetric_PostsExpectedBody verifies the metric
// payload reaches vmagent in the prometheus exposition format. We do not
// hit real S3 — only assert the HTTP request shape.
func TestPushS3ResumeOutcomeMetric_PostsExpectedBody(t *testing.T) {
	var (
		mu    sync.Mutex
		got   string
		hits  int
		path  string
		ctype string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = string(body)
		hits++
		path = r.URL.Path
		ctype = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-Agent1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	pushS3ResumeOutcomeMetric(context.Background(), s3ResumeOutcomeListTimeout)

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("vmagent hits = %d, want 1", hits)
	}
	if path != "/api/v1/import/prometheus" {
		t.Errorf("path = %q, want /api/v1/import/prometheus", path)
	}
	if ctype != "text/plain" {
		t.Errorf("content-type = %q, want text/plain", ctype)
	}
	wantLine := `gardenio_s3_resume_check_total{outcome="list-error-timeout",agent="gd-Agent1"} 1`
	if !strings.Contains(got, wantLine) {
		t.Errorf("body missing outcome line\n got: %q\nwant substring: %q", got, wantLine)
	}
}

// TestPushS3ResumeOutcomeMetric_NoVmagentSilent ensures the push is a no-op
// when VMAGENT_URL is empty — important because most local-dev environments
// do not run vmagent.
func TestPushS3ResumeOutcomeMetric_NoVmagentSilent(t *testing.T) {
	t.Setenv("VMAGENT_URL", "")
	// Should not panic, should not block, should not error.
	pushS3ResumeOutcomeMetric(context.Background(), s3ResumeOutcomeArchiveExists)
}
