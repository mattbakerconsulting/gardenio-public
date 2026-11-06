package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Test5xxRootCauseLog exercises the loggingMiddleware behavior introduced for
// ka-0000002244: when a handler returns a 5xx, the middleware must emit BOTH
// the access-log entry AND a single WARN with the wrapped error chain. The
// access log for successful requests must be unchanged.
func Test5xxRootCauseLog(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	h := slog.NewTextHandler(&lockedWriter{buf: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Handler that attaches a wrapped error then writes a 500. The error chain
	// mimics the kind of wrapped DB/NATS failure that the escalation ticket
	// gd-vo76ZQAJ5I wanted visible.
	rootErr := errors.New("dial tcp: connection refused")
	wrapped := fmt.Errorf("list beads: query: %w", rootErr)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, r *http.Request) {
		AttachServerError(r.Context(), "handleListBeads", wrapped)
		writeError(w, http.StatusInternalServerError, "internal error")
	})
	mux.HandleFunc("GET /v1/ok", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	handler := loggingMiddleware(nil, mux)

	// 5xx case.
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer sk-super-secret-token-xyz")
	req.Header.Set("X-Request-Id", "req-abc-123")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", rr.Code)
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	// Must have the access-log entry (pre-existing behavior).
	if !strings.Contains(out, `msg="http request"`) && !strings.Contains(out, `msg="http request (slow)"`) {
		t.Errorf("expected access-log entry (msg=\"http request\"...); got:\n%s", out)
	}

	// Must have exactly one 5xx root-cause WARN.
	if got := strings.Count(out, `msg="http 5xx root cause"`); got != 1 {
		t.Errorf("expected exactly 1 '5xx root cause' log, got %d; out:\n%s", got, out)
	}

	// Must include wrapped error chain.
	if !strings.Contains(out, "list beads: query: dial tcp: connection refused") {
		t.Errorf("expected wrapped error chain in log; got:\n%s", out)
	}

	// Must include handler name and request_id.
	if !strings.Contains(out, "handler=handleListBeads") {
		t.Errorf("expected handler=handleListBeads; got:\n%s", out)
	}
	if !strings.Contains(out, "request_id=req-abc-123") {
		t.Errorf("expected request_id=req-abc-123; got:\n%s", out)
	}

	// MUST NOT leak the Authorization header value or bearer token.
	if strings.Contains(out, "sk-super-secret-token-xyz") {
		t.Errorf("bearer token leaked into logs:\n%s", out)
	}
	// A paranoid grep for the word "Bearer <anything>" that isn't redacted:
	// the only occurrences of "Bearer" in the output should be followed by
	// [REDACTED] (we don't expect any in this test, but guard the invariant).
	for _, line := range strings.Split(out, "\n") {
		if idx := strings.Index(line, "Bearer "); idx >= 0 {
			rest := line[idx:]
			if !strings.HasPrefix(rest, "Bearer [REDACTED]") {
				t.Errorf("unsanitized 'Bearer ...' fragment in log line: %s", line)
			}
		}
	}

	// 2xx case: access-log entry only, no root-cause WARN.
	mu.Lock()
	buf.Reset()
	mu.Unlock()

	req2 := httptest.NewRequest(http.MethodGet, "/v1/ok", nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr2.Code)
	}

	mu.Lock()
	out2 := buf.String()
	mu.Unlock()

	if strings.Contains(out2, "5xx root cause") {
		t.Errorf("unexpected 5xx root-cause log on 2xx response:\n%s", out2)
	}
	// Access log entry must still be present on success.
	if !strings.Contains(out2, "http request") {
		t.Errorf("expected access-log entry on 2xx; got:\n%s", out2)
	}
}

// TestSanitizeErrorString covers the redaction of bearer tokens and
// Authorization header contents from error strings.
func TestSanitizeErrorString(t *testing.T) {
	cases := []struct {
		in     string
		forbid []string
	}{
		{
			in:     `post failed: header "Authorization: Bearer sk-abc123" rejected`,
			forbid: []string{"sk-abc123"},
		},
		{
			in:     `upstream: bearer eyJhbGciOi.secrets.here rejected`,
			forbid: []string{"eyJhbGciOi.secrets.here"},
		},
		{
			in:     `Authorization=Bearer tok-xyz`,
			forbid: []string{"tok-xyz"},
		},
	}
	for _, tc := range cases {
		got := sanitizeErrorString(tc.in)
		for _, f := range tc.forbid {
			if strings.Contains(got, f) {
				t.Errorf("sanitizeErrorString(%q) = %q; still contains %q", tc.in, got, f)
			}
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("sanitizeErrorString(%q) = %q; expected [REDACTED] marker", tc.in, got)
		}
	}
}

// TestAttachServerError_NilContext ensures AttachServerError is a safe no-op
// when the context was not initialised by loggingMiddleware.
func TestAttachServerError_NilContext(t *testing.T) {
	// Must not panic.
	AttachServerError(t.Context(), "handler", errors.New("x"))
	AttachServerError(t.Context(), "handler", nil)
}

// lockedWriter serialises writes from slog (which may be invoked concurrently
// if a handler spawns goroutines) so tests can safely read buf.String().
type lockedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
