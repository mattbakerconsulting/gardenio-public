package main

// Unit tests for probeAuthStatus — the probe's terminal-auth verdict, distinct
// from the transient rate-limit Throttled() signal (ka-0000002085). A 401/403
// is terminal ("rejected"); a 200 or a 429 proves the credential is live
// ("ok"); any other outcome is transient infrastructure noise ("").

import (
	"net/http"
	"testing"
)

func TestProbeAuthStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"200 OK is ok", http.StatusOK, "ok"},
		{"429 throttle is ok (credential is live)", http.StatusTooManyRequests, "ok"},
		{"401 unauthorized is rejected", http.StatusUnauthorized, "rejected"},
		{"403 forbidden is rejected (org disabled Claude Code)", http.StatusForbidden, "rejected"},
		{"500 server error is transient (unknown)", http.StatusInternalServerError, ""},
		{"502 bad gateway is transient (unknown)", http.StatusBadGateway, ""},
		{"0 (no response / network error) is transient (unknown)", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := probeAuthStatus(probeResult{HTTPStatus: c.status}); got != c.want {
				t.Errorf("probeAuthStatus(HTTPStatus=%d) = %q, want %q", c.status, got, c.want)
			}
		})
	}
}
