package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// versionHandler answers /v1/health with a canned X-Kbeads-Version header so
// the client's skew-observation hook can be exercised (ka-0000002965).
type versionHandler struct{ version string }

func (h *versionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.version != "" {
		w.Header().Set("X-Kbeads-Version", h.version)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func TestHTTPClient_OnDaemonVersion_FiresOnce(t *testing.T) {
	srv := httptest.NewServer(&versionHandler{version: "2026.201.5"})
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	var calls atomic.Int32
	var seen string
	c.SetOnDaemonVersion(func(v string) {
		calls.Add(1)
		seen = v
	})

	// Three round-trips; the callback must fire exactly once.
	for range 3 {
		if _, err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health() error = %v", err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("onDaemonVersion fired %d times, want 1", got)
	}
	if seen != "2026.201.5" {
		t.Errorf("onDaemonVersion saw %q, want 2026.201.5", seen)
	}
}

func TestHTTPClient_OnDaemonVersion_NoHeaderNoFire(t *testing.T) {
	srv := httptest.NewServer(&versionHandler{version: ""}) // no header
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	var calls atomic.Int32
	c.SetOnDaemonVersion(func(string) { calls.Add(1) })

	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("onDaemonVersion fired %d times with no header, want 0", got)
	}
}
