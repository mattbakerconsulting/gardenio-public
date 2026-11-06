package main

import (
	"net/http"
	"testing"
)

func TestParseProbeHeaders_PerWindowResets(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-reset", "1700000000")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1700500000")
	h.Set("anthropic-ratelimit-unified-reset", "1700000000")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.71")

	var r probeResult
	parseProbeHeaders(h, &r)

	if r.Reset5h != 1700000000 {
		t.Errorf("Reset5h = %d, want 1700000000", r.Reset5h)
	}
	if r.Reset7d != 1700500000 {
		t.Errorf("Reset7d = %d, want 1700500000", r.Reset7d)
	}
	if r.Reset != 1700000000 {
		t.Errorf("Reset = %d, want 1700000000", r.Reset)
	}
}

func TestParseProbeHeaders_MissingResetsAreZero(t *testing.T) {
	h := http.Header{}
	var r probeResult
	parseProbeHeaders(h, &r)
	if r.Reset5h != 0 || r.Reset7d != 0 {
		t.Errorf("expected zero resets when headers absent, got (5h=%d, 7d=%d)", r.Reset5h, r.Reset7d)
	}
}
