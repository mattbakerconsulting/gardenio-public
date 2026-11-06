package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gardenio/clicreds/beadswire"
)

// connResetHandler hijacks and abruptly closes the underlying connection
// (no response written) for the first `failCount` requests, simulating the
// beads daemon pod dying mid-request (Phase 3 drill 3, gd-Mc3cPhsInt), then
// serves 200 with a bead body.
type connResetHandler struct {
	failCount int32
	calls     int32
}

func (h *connResetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt32(&h.calls, 1)
	if n <= h.failCount {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("ResponseWriter does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			panic(err)
		}
		_ = conn.Close()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"gd-created","title":"t"}`))
}

// TestDoJSON_RetriesGETOnConnReset verifies a GET that hits a raw connection
// failure (the daemon pod going away mid-request) is retried and ultimately
// succeeds, rather than surfacing the transport error on the first attempt.
func TestDoJSON_RetriesGETOnConnReset(t *testing.T) {
	h := &connResetHandler{failCount: beadswire.ConnRetries}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	c.sleep = beadswire.SleepWithContext // real sleeper is fine: beadswire.ConnRetryBackoff is small (150ms base)

	var out map[string]any
	if err := c.doJSON(context.Background(), http.MethodGet, "/v1/beads/gd-created", nil, &out); err != nil {
		t.Fatalf("doJSON GET after %d conn resets = %v, want success", beadswire.ConnRetries, err)
	}
	if got := atomic.LoadInt32(&h.calls); got != beadswire.ConnRetries+1 {
		t.Errorf("server calls = %d, want %d (initial + %d conn-reset retries)", got, beadswire.ConnRetries+1, beadswire.ConnRetries)
	}
}

// TestDoJSON_GivesUpAfterConnRetryBudget verifies the connection-reset retry
// is BOUNDED: once the budget is exhausted the caller sees the transport
// error rather than retrying forever.
func TestDoJSON_GivesUpAfterConnRetryBudget(t *testing.T) {
	h := &connResetHandler{failCount: 100}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	c.sleep = beadswire.SleepWithContext

	var out map[string]any
	err := c.doJSON(context.Background(), http.MethodGet, "/v1/beads", nil, &out)
	if err == nil {
		t.Fatal("expected error after exhausting conn-retry budget, got nil")
	}
	if got := atomic.LoadInt32(&h.calls); got != beadswire.ConnRetries+1 {
		t.Errorf("server calls = %d, want %d (bounded)", got, beadswire.ConnRetries+1)
	}
}

// TestDoJSON_DoesNotRetryPOSTOnConnReset verifies a non-idempotent write is
// NEVER retried on a connection failure: unlike the transient-DB-auth 503
// (which fails at dial, before any query runs), a connection reset gives no
// guarantee the daemon never applied the mutation. A blind resend could
// double a write (a duplicate escalation, comment, or bead create).
func TestDoJSON_DoesNotRetryPOSTOnConnReset(t *testing.T) {
	h := &connResetHandler{failCount: 100}
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	c.sleep = beadswire.SleepWithContext

	var out map[string]any
	err := c.doJSON(context.Background(), http.MethodPost, "/v1/beads", map[string]any{"title": "t"}, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := atomic.LoadInt32(&h.calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (POST must not be retried on conn reset)", got)
	}
}
