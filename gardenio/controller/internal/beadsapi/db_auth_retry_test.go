package beadsapi

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

// newFastRetryClient points a client at srv with an instant retry sleep, so
// retry behavior is testable without a wall-clock wait.
func newFastRetryClient(srv *httptest.Server) *Client {
	c := newTestClient(srv)
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c
}

// TestDoJSON_RetriesTransientDBAuth verifies that a create which the daemon
// answers with 503 + Retry-After (a transient 28P01, ka-0000006554) is re-sent
// and ultimately succeeds instead of being dropped (gd-W58VfF7TYi).
func TestDoJSON_RetriesTransientDBAuth(t *testing.T) {
	h := &dbAuthRetryHandler{failCount: beadswire.DBAuthRetries, retryAfter: "5"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	id, err := newFastRetryClient(srv).CreateBead(context.Background(), CreateBeadRequest{Title: "t"})
	if err != nil {
		t.Fatalf("CreateBead after %d transient 503s = %v, want success", beadswire.DBAuthRetries, err)
	}
	if id != "gd-created" {
		t.Errorf("id = %q, want gd-created", id)
	}
	if got := atomic.LoadInt32(&h.calls); got != beadswire.DBAuthRetries+1 {
		t.Errorf("server calls = %d, want %d (initial + retries)", got, beadswire.DBAuthRetries+1)
	}
}

// TestDoJSON_GivesUpAfterDBAuthRetryBudget verifies the retry is BOUNDED: once
// the budget is exhausted the caller sees the 503 rather than hanging.
func TestDoJSON_GivesUpAfterDBAuthRetryBudget(t *testing.T) {
	h := &dbAuthRetryHandler{failCount: 100, retryAfter: "5"}
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, err := newFastRetryClient(srv).CreateBead(context.Background(), CreateBeadRequest{Title: "t"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want *APIError 503", err)
	}
	if got := atomic.LoadInt32(&h.calls); got != beadswire.DBAuthRetries+1 {
		t.Errorf("server calls = %d, want %d (bounded)", got, beadswire.DBAuthRetries+1)
	}
}

// TestDoJSON_DoesNotRetryNonDBAuth503 verifies the 503s that are not the
// transient DB-auth signal are surfaced on the first attempt: one without a
// Retry-After (e.g. "charts backend not configured") and the
// statement-cancellation 503, whose query is deterministically too slow
// (SQLSTATE 57014, bug ka-0000000979).
func TestDoJSON_DoesNotRetryNonDBAuth503(t *testing.T) {
	cases := []struct {
		name string
		h    *dbAuthRetryHandler
	}{
		{"no Retry-After", &dbAuthRetryHandler{failCount: 100}},
		{"query canceled", &dbAuthRetryHandler{
			failCount:  100,
			retryAfter: "2",
			failBody:   `{"error":"failed to list beads","code":"` + beadswire.QueryCanceledErrorCode + `"}`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()

			_, err := newFastRetryClient(srv).GetBead(context.Background(), "gd-created")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("err = %v, want *APIError 503", err)
			}
			if got := atomic.LoadInt32(&tc.h.calls); got != 1 {
				t.Errorf("server calls = %d, want 1 (not retried)", got)
			}
		})
	}
}

// TestDoJSONAsUser_RetriesTransientDBAuth verifies the per-user bearer path
// (CreateTaint, MeAs, Slack identity links) shares doJSON's DB-auth retry: a
// transient 503 + Retry-After is re-sent on the same bearer and succeeds.
func TestDoJSONAsUser_RetriesTransientDBAuth(t *testing.T) {
	h := &dbAuthRetryHandler{failCount: beadswire.DBAuthRetries, retryAfter: "5"}
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearers = append(bearers, r.Header.Get("Authorization"))
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	var resp map[string]any
	err := newFastRetryClient(srv).doJSONAsUser(context.Background(), http.MethodPost, "/v1/taints", "user-jwt", map[string]string{"name": "x"}, &resp)
	if err != nil {
		t.Fatalf("doJSONAsUser after %d transient 503s = %v, want success", beadswire.DBAuthRetries, err)
	}
	if got := atomic.LoadInt32(&h.calls); got != beadswire.DBAuthRetries+1 {
		t.Errorf("server calls = %d, want %d (initial + retries)", got, beadswire.DBAuthRetries+1)
	}
	for i, b := range bearers {
		if b != "Bearer user-jwt" {
			t.Errorf("attempt %d Authorization = %q, want Bearer user-jwt", i, b)
		}
	}
}

// TestDoJSONAsUser_DoesNotRetryQueryCanceled503 verifies the per-user path
// keeps the statement-cancellation exclusion (ka-0000000979).
func TestDoJSONAsUser_DoesNotRetryQueryCanceled503(t *testing.T) {
	h := &dbAuthRetryHandler{
		failCount:  100,
		retryAfter: "2",
		failBody:   `{"error":"failed","code":"` + beadswire.QueryCanceledErrorCode + `"}`,
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	err := newFastRetryClient(srv).doJSONAsUser(context.Background(), http.MethodGet, "/v1/me", "user-jwt", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want *APIError 503", err)
	}
	if got := atomic.LoadInt32(&h.calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (not retried)", got)
	}
}
