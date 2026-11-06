package main

import (
	"testing"
	"time"
)

// TestDecodeModelGroups_FableSplit proves gb account limits decodes the per-model-
// group field the probe writes (ka-0000003806), surfacing Fable capacity separately
// from Opus, with derived remaining-% and a reset countdown, sorted by key.
func TestDecodeModelGroups_FableSplit(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// fable resets 1h out; opus has no reset.
	raw := `{"7d_fable":{"window":"7d","group":"fable","utilization":0.95,"reset":1700003600,"status":"rejected"},"7d_opus":{"window":"7d","group":"opus","utilization":0.10}}`

	got := decodeModelGroups(raw, now)
	if len(got) != 2 {
		t.Fatalf("decodeModelGroups len = %d, want 2 (%+v)", len(got), got)
	}
	// Sorted by key: 7d_fable before 7d_opus.
	fable := got[0]
	if fable.Key != "7d_fable" || fable.Group != "fable" || fable.Window != "7d" {
		t.Errorf("group[0] = %+v, want key=7d_fable group=fable window=7d", fable)
	}
	if fable.Utilization != 0.95 {
		t.Errorf("fable util = %v, want 0.95", fable.Utilization)
	}
	if fable.RemainingPct < 4.9 || fable.RemainingPct > 5.1 {
		t.Errorf("fable remaining = %v, want ~5", fable.RemainingPct)
	}
	if fable.Status != "rejected" {
		t.Errorf("fable status = %q, want rejected", fable.Status)
	}
	if fable.ResetIn != "1h" {
		t.Errorf("fable reset_in = %q, want 1h", fable.ResetIn)
	}

	opus := got[1]
	if opus.Key != "7d_opus" || opus.Utilization != 0.10 {
		t.Errorf("group[1] = %+v, want key=7d_opus util=0.10", opus)
	}
	if opus.RemainingPct < 89.9 || opus.RemainingPct > 90.1 {
		t.Errorf("opus remaining = %v, want ~90", opus.RemainingPct)
	}
	// No reset in source → empty countdown, not "past".
	if opus.ResetIn != "" {
		t.Errorf("opus reset_in = %q, want empty", opus.ResetIn)
	}
}

// TestDecodeModelGroups_EmptyAndMalformed proves an account without the split (or
// with a corrupt field) renders unchanged — decode returns nil, never errors.
func TestDecodeModelGroups_EmptyAndMalformed(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	if g := decodeModelGroups("", now); g != nil {
		t.Errorf("empty field → %v, want nil", g)
	}
	if g := decodeModelGroups("not json", now); g != nil {
		t.Errorf("malformed field → %v, want nil", g)
	}
	if g := decodeModelGroups("{}", now); len(g) != 0 {
		t.Errorf("empty object → %v, want no groups", g)
	}
}

// TestDecodeModelGroups_PastReset proves a reset time already elapsed renders as
// "past" rather than a negative/garbage duration.
func TestDecodeModelGroups_PastReset(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	raw := `{"7d_fable":{"window":"7d","group":"fable","utilization":1.0,"reset":1699990000}}`
	got := decodeModelGroups(raw, now)
	if len(got) != 1 || got[0].ResetIn != "past" {
		t.Errorf("past reset → %+v, want reset_in=past", got)
	}
}
