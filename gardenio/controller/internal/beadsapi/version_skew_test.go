package beadsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestOnDaemonVersion_FiresOnce verifies the gb client observes the daemon's
// X-Kbeads-Version header exactly once across many round-trips, so the CLI can
// emit a single version-skew warning (ka-0000002965).
func TestOnDaemonVersion_FiresOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kbeads-Version", "2026.201.5")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var calls atomic.Int32
	var seen string
	c.SetOnDaemonVersion(func(v string) {
		calls.Add(1)
		seen = v
	})

	for range 3 {
		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health: %v", err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("onDaemonVersion fired %d times, want 1", got)
	}
	if seen != "2026.201.5" {
		t.Errorf("onDaemonVersion saw %q, want 2026.201.5", seen)
	}
}

// TestOnDaemonVersion_NoHeaderNoFire verifies the callback stays quiet when the
// daemon does not advertise a version (e.g. an old daemon or a proxy that
// strips the header).
func TestOnDaemonVersion_NoHeaderNoFire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var calls atomic.Int32
	c.SetOnDaemonVersion(func(string) { calls.Add(1) })

	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("onDaemonVersion fired %d times with no header, want 0", got)
	}
}
