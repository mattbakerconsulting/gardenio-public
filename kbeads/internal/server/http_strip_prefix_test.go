package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPHandlerBehindStripPrefix locks the W-E-4 contract: kbeads HTTP
// daemon stays root-routed at /v1/... and works correctly when fronted
// by Traefik StripPrefix=/kd. Plan ref: gardenio/plans/stage3-ava-cli-eb-rollout.md.
//
// The daemon must not require any BASE_PATH env var; clients call
// https://gardenio.<env>/kd/v1/... and Traefik strips /kd before forwarding
// /v1/... to the kbeads Service.
func TestHTTPHandlerBehindStripPrefix(t *testing.T) {
	_, _, handler := newTestServer()
	stripped := http.StripPrefix("/kd", handler)

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"health", http.MethodGet, "/kd/v1/health", http.StatusOK},
		{"list beads", http.MethodGet, "/kd/v1/beads", http.StatusOK},
		{"ready queue", http.MethodGet, "/kd/v1/ready", http.StatusOK},
		{"unknown route still 404", http.MethodGet, "/kd/v1/does-not-exist", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			stripped.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s: status = %d, want %d (body: %s)",
					tc.method, tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestHTTPHandlerRejectsUnstrippedPrefix asserts the failure mode if Traefik
// is misconfigured (no StripPrefix) and forwards /kd/v1/... verbatim — the
// daemon must 404, not silently route. This locks the strip-vs-preserve
// convention chosen in the plan (we strip).
func TestHTTPHandlerRejectsUnstrippedPrefix(t *testing.T) {
	_, _, handler := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/kd/v1/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unstripped /kd/v1/health: status = %d, want 404 (daemon must not match unstripped paths)", rec.Code)
	}
}
