package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/buildinfo"
)

// TestVersionHeaderMiddleware verifies the daemon stamps its build version on
// every response so stale clients can detect skew (ka-0000002965).
func TestVersionHeaderMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // an early WriteHeader must still carry the header
	})
	h := versionHeaderMiddleware(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	h.ServeHTTP(rec, req)

	want := buildinfo.Get()
	if got := rec.Header().Get("X-Kbeads-Version"); got != want.Version {
		t.Errorf("X-Kbeads-Version = %q, want %q", got, want.Version)
	}
	if got := rec.Header().Get("X-Kbeads-Commit"); got != want.Commit {
		t.Errorf("X-Kbeads-Commit = %q, want %q", got, want.Commit)
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (middleware must not swallow the inner status)", rec.Code, http.StatusTeapot)
	}
}
