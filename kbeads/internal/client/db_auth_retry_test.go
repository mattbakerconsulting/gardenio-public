package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/clicreds/beadswire"
)

// dbAuthRetryHandler fails the first `failCount` requests with 503 + the given
// Retry-After header, then serves 200 with a bead body. Records the total call
// count so tests can assert how many attempts the client made.
type dbAuthRetryHandler struct {
	failCount  int32
	retryAfter string
	// failBody is the 503 response body. Empty defaults to a bare error body;
	// set it to a body carrying {"code":"query_canceled"} to exercise the
	// statement-cancellation path that must NOT be retried (bug ka-0000000979).
	failBody string
	calls    int32
}

func (h *dbAuthRetryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt32(&h.calls, 1)
	w.Header().Set("Content-Type", "application/json")
	if n <= h.failCount {
		if h.retryAfter != "" {
			w.Header().Set("Retry-After", h.retryAfter)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		body := h.failBody
		if body == "" {
			body = `{"error":"internal server error"}`
		}
		_, _ = w.Write([]byte(body))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"gd-created","title":"t"}`))
}

// newFastRetryClient points a client at srv and makes its DB-auth retry backoff
// instant (no wall-clock wait), so retry behavior is testable in microseconds.
func newFastRetryClient(t *testing.T, url string, retries int) *HTTPClient {
	t.Helper()
	c := NewHTTPClient(url, "")
	c.dbAuthRetries = retries
	c.dbAuthRetryBackoff = 0
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c
}

// TestDoJSON_RetriesTransientDBAuth verifies that a write which the daemon
// answers with 503 + Retry-After (a transient 28P01, ka-0000006554) is
// re-sent and ultimately succeeds instead of being silently dropped.
func TestDoJSON_RetriesTransientDBAuth(t *testing.T) {
	h := &dbAuthRetryHandler{failCount: 2, retryAfter: "5"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newFastRetryClient(t, srv.URL, 2)

	bead, err := c.CreateBead(context.Background(), &CreateBeadRequest{Title: "t"})
	if err != nil {
		t.Fatalf("CreateBead after 2 transient 503s = %v, want success", err)
	}
	if bead.ID != "gd-created" {
		t.Errorf("bead.ID = %q, want gd-created", bead.ID)
	}
	if got := atomic.LoadInt32(&h.calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (initial + 2 retries)", got)
	}
}

// TestDoJSON_GivesUpAfterRetryBudget verifies the retry is BOUNDED: once the
// budget is exhausted the caller sees the 503 rather than hanging forever.
func TestDoJSON_GivesUpAfterRetryBudget(t *testing.T) {
	h := &dbAuthRetryHandler{failCount: 100, retryAfter: "5"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newFastRetryClient(t, srv.URL, 2)

	_, err := c.CreateBead(context.Background(), &CreateBeadRequest{Title: "t"})
	if err == nil {
		t.Fatal("expected error after exhausting retry budget, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", apiErr.StatusCode)
	}
	// initial + 2 retries = 3 attempts, then give up.
	if got := atomic.LoadInt32(&h.calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (bounded)", got)
	}
}

// TestDoJSON_DoesNotRetry503WithoutRetryAfter verifies a permanent 503 (e.g.
// "charts backend not configured", which carries NO Retry-After) is surfaced
// immediately and NOT retried — the retry is scoped strictly to the daemon's
// transient DB-auth signal.
func TestDoJSON_DoesNotRetry503WithoutRetryAfter(t *testing.T) {
	h := &dbAuthRetryHandler{failCount: 100, retryAfter: ""} // no Retry-After
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newFastRetryClient(t, srv.URL, 2)

	_, err := c.CreateBead(context.Background(), &CreateBeadRequest{Title: "t"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want *APIError 503", err)
	}
	if got := atomic.LoadInt32(&h.calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (no retry without Retry-After)", got)
	}
}

// TestDoJSON_DoesNotRetryQueryCanceled503 verifies that a 503 carrying a
// Retry-After AND the query_canceled body code (SQLSTATE 57014 — a statement
// cancelled by the 30s deadline) is surfaced immediately and NOT retried. The
// query is deterministically too slow, so re-running it just burns another full
// deadline and stacks one failure into a multi-attempt ~90s hang (bug
// ka-0000000979). This is the case a bare "any 503+Retry-After is retryable"
// predicate got wrong.
func TestDoJSON_DoesNotRetryQueryCanceled503(t *testing.T) {
	h := &dbAuthRetryHandler{
		failCount:  100,
		retryAfter: "2",
		failBody:   `{"error":"failed to list beads","code":"` + beadswire.QueryCanceledErrorCode + `"}`,
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := newFastRetryClient(t, srv.URL, 2)

	var out any
	err := c.doJSON(context.Background(), http.MethodGet, "/v1/beads", nil, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want *APIError 503", err)
	}
	if got := atomic.LoadInt32(&h.calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (query_canceled must not be retried)", got)
	}
}
