package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRuntimeID_ReadsPersistedField verifies RuntimeID returns the bead's
// runtime_id field, which the MicroVM warm-pool bind-once Launch guard consults
// to refuse a re-mint of an already-bound per-run identity (ka-0000006503).
func TestRuntimeID_ReadsPersistedField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(beadJSON{
			ID:     "gd-warm",
			Type:   "agent",
			Status: "open",
			Fields: json.RawMessage(`{"runtime_id":"mvm-xyz","agent_state":"prewarmed"}`),
		})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).RuntimeID(context.Background(), "gd-warm")
	if err != nil {
		t.Fatalf("RuntimeID: %v", err)
	}
	if got != "mvm-xyz" {
		t.Errorf("RuntimeID = %q, want mvm-xyz", got)
	}
}

// TestRuntimeID_EmptyWhenAbsent: a bead that has not launched a runtime carries
// no runtime_id field, so the guard sees "" and proceeds with a first launch.
func TestRuntimeID_EmptyWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(beadJSON{
			ID:     "gd-cold",
			Type:   "agent",
			Status: "open",
			Fields: json.RawMessage(`{"agent_state":"prewarmed"}`),
		})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).RuntimeID(context.Background(), "gd-cold")
	if err != nil {
		t.Fatalf("RuntimeID: %v", err)
	}
	if got != "" {
		t.Errorf("RuntimeID = %q, want empty for a bead with no runtime", got)
	}
}

// TestRuntimeID_PropagatesFetchError: a fetch failure is surfaced so the caller
// (the Launch guard) can fail open rather than silently treating it as "not
// launched".
func TestRuntimeID_PropagatesFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := newTestClient(srv).RuntimeID(context.Background(), "gd-x"); err == nil {
		t.Fatal("RuntimeID: want error on fetch failure, got nil")
	}
}
