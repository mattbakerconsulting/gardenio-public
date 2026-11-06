package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestParseModelGroupHeaders_GenericSplit proves parseProbeHeaders captures the
// per-model-group unified headers Anthropic emits (ka-0000003806) generically —
// opus, sonnet, AND fable — without the parser enumerating any group name, and
// keeps the aggregate per-window fields intact.
func TestParseModelGroupHeaders_GenericSplit(t *testing.T) {
	h := http.Header{}
	// Aggregate windows (already handled) — must remain untouched.
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.71")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1700500000")
	// Per-group split: fable is the group Matt flagged; opus/sonnet also present.
	h.Set("anthropic-ratelimit-unified-7d_fable-utilization", "0.95")
	h.Set("anthropic-ratelimit-unified-7d_fable-reset", "1700900000")
	h.Set("anthropic-ratelimit-unified-7d_fable-status", "rejected")
	h.Set("anthropic-ratelimit-unified-7d_opus-utilization", "0.10")
	h.Set("anthropic-ratelimit-unified-5h_sonnet-utilization", "0.33")

	var r probeResult
	parseProbeHeaders(h, &r)

	// Aggregate fields still parsed.
	if r.Utilization5h != 0.42 || r.Utilization7d != 0.71 {
		t.Fatalf("aggregate util wrong: 5h=%v 7d=%v", r.Utilization5h, r.Utilization7d)
	}

	if got := len(r.ModelGroups); got != 3 {
		t.Fatalf("ModelGroups len = %d, want 3 (%v)", got, r.ModelGroups)
	}

	fable, ok := r.ModelGroups["7d_fable"]
	if !ok {
		t.Fatal("missing 7d_fable group")
	}
	if fable.Window != "7d" || fable.Group != "fable" {
		t.Errorf("fable window/group = %q/%q, want 7d/fable", fable.Window, fable.Group)
	}
	if fable.Utilization != 0.95 {
		t.Errorf("fable util = %v, want 0.95", fable.Utilization)
	}
	if fable.Reset != 1700900000 {
		t.Errorf("fable reset = %d, want 1700900000", fable.Reset)
	}
	if fable.Status != "rejected" {
		t.Errorf("fable status = %q, want rejected", fable.Status)
	}

	opus := r.ModelGroups["7d_opus"]
	if opus.Utilization != 0.10 {
		t.Errorf("opus util = %v, want 0.10", opus.Utilization)
	}
	// A group Anthropic reports only in the 5h window is keyed by its own window.
	sonnet, ok := r.ModelGroups["5h_sonnet"]
	if !ok || sonnet.Window != "5h" || sonnet.Utilization != 0.33 {
		t.Errorf("5h_sonnet = %+v (ok=%v), want window=5h util=0.33", sonnet, ok)
	}
}

// TestParseModelGroupHeaders_UnknownGroupFlowsThrough proves a NEW group name
// gardenio has never seen is captured with no code change — the whole reason the
// parser is generic rather than keyed on a hardcoded (guessed) group list.
func TestParseModelGroupHeaders_UnknownGroupFlowsThrough(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-7d_futuremodel-utilization", "0.55")

	var r probeResult
	parseProbeHeaders(h, &r)

	g, ok := r.ModelGroups["7d_futuremodel"]
	if !ok {
		t.Fatal("unknown group 7d_futuremodel was dropped; parser is not generic")
	}
	if g.Group != "futuremodel" || g.Utilization != 0.55 {
		t.Errorf("futuremodel = %+v, want group=futuremodel util=0.55", g)
	}
}

// TestParseModelGroupHeaders_NoGroupHeaders proves the common (older-account)
// path allocates nothing: an aggregate-only response yields a nil ModelGroups.
func TestParseModelGroupHeaders_NoGroupHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.71")
	// A non-anthropic header and the aggregate reset must NOT be misread as groups.
	h.Set("anthropic-ratelimit-unified-reset", "1700000000")
	h.Set("content-type", "application/json")

	var r probeResult
	parseProbeHeaders(h, &r)

	if r.ModelGroups != nil {
		t.Errorf("ModelGroups = %v, want nil for aggregate-only response", r.ModelGroups)
	}
}

// TestParseModelGroupHeaders_MalformedIgnored proves headers that carry the
// prefix but not the "<window>_<group>-<metric>" shape are ignored rather than
// producing junk groups.
func TestParseModelGroupHeaders_MalformedIgnored(t *testing.T) {
	h := http.Header{}
	// Missing the underscore → this is an aggregate per-window header, not a group.
	h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
	// Missing a metric segment.
	h.Set("anthropic-ratelimit-unified-7d_opus", "x")
	// Empty group.
	h.Set("anthropic-ratelimit-unified-7d_-utilization", "0.5")

	var r probeResult
	parseProbeHeaders(h, &r)

	if len(r.ModelGroups) != 0 {
		t.Errorf("ModelGroups = %v, want empty (all inputs malformed)", r.ModelGroups)
	}
}

// TestEncodeModelGroupLimits_Deterministic proves the persisted JSON is stable
// (sorted keys) so identical probe state never churns the account bead field, and
// round-trips back to the same groups.
func TestEncodeModelGroupLimits_Deterministic(t *testing.T) {
	groups := map[string]modelGroupLimit{
		"7d_fable": {Window: "7d", Group: "fable", Utilization: 0.95, Reset: 1700900000, Status: "rejected"},
		"7d_opus":  {Window: "7d", Group: "opus", Utilization: 0.10},
	}
	a := encodeModelGroupLimits(groups)
	b := encodeModelGroupLimits(groups)
	if a != b {
		t.Fatalf("encode not deterministic: %q != %q", a, b)
	}
	if a == "" {
		t.Fatal("encode returned empty for non-empty map")
	}

	var back map[string]modelGroupLimit
	if err := json.Unmarshal([]byte(a), &back); err != nil {
		t.Fatalf("round-trip unmarshal failed: %v", err)
	}
	if back["7d_fable"].Utilization != 0.95 || back["7d_fable"].Status != "rejected" {
		t.Errorf("round-trip lost fable data: %+v", back["7d_fable"])
	}

	// Empty/nil map → empty string so the caller leaves the bead field unset.
	if encodeModelGroupLimits(nil) != "" {
		t.Error("encode(nil) should be empty")
	}
	if encodeModelGroupLimits(map[string]modelGroupLimit{}) != "" {
		t.Error("encode(empty) should be empty")
	}
}
