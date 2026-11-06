package main

import (
	"strings"
	"testing"
)

// TestDecisionTruncationNotice_Truncated verifies that when the daemon's full
// match count (total) exceeds the rows shown on screen, the notice fires and
// reports both counts plus an actionable --limit suggestion equal to total —
// the whole point of the fix (ka-0000008161): a clipped window must never read
// as a complete result set.
func TestDecisionTruncationNotice_Truncated(t *testing.T) {
	notice := decisionTruncationNotice(20, 36)
	if notice == "" {
		t.Fatal("expected a truncation notice when total (36) > shown (20), got empty string")
	}
	if !strings.Contains(notice, "20") {
		t.Errorf("notice should report the shown count (20), got: %s", notice)
	}
	if !strings.Contains(notice, "36") {
		t.Errorf("notice should report the total count (36), got: %s", notice)
	}
	// The suggested --limit must be the full total so a re-run shows everything.
	if !strings.Contains(notice, "--limit 36") {
		t.Errorf("notice should suggest --limit 36 (the full total), got: %s", notice)
	}
}

// TestDecisionTruncationNotice_Complete confirms no notice is printed when every
// matching decision is already on screen (total == shown) — the common,
// non-truncated case must stay silent.
func TestDecisionTruncationNotice_Complete(t *testing.T) {
	if notice := decisionTruncationNotice(12, 12); notice != "" {
		t.Errorf("expected no notice when total == shown, got: %q", notice)
	}
}

// TestDecisionTruncationNotice_Empty confirms an empty result set (0 shown, 0
// total) produces no notice.
func TestDecisionTruncationNotice_Empty(t *testing.T) {
	if notice := decisionTruncationNotice(0, 0); notice != "" {
		t.Errorf("expected no notice for an empty result set, got: %q", notice)
	}
}

// TestDecisionTruncationNotice_NeverNegative is a defensive guard: if the daemon
// ever reports a total smaller than the shown rows (should not happen, since
// total is COUNT(*) OVER() >= the page size), the notice must not fire rather
// than print a nonsensical "showing 20 of 5".
func TestDecisionTruncationNotice_NeverNegative(t *testing.T) {
	if notice := decisionTruncationNotice(20, 5); notice != "" {
		t.Errorf("expected no notice when total < shown (defensive), got: %q", notice)
	}
}

// TestDecisionListSearchFlagsRegistered guards that `gb decision list` keeps the
// --search / --ref reference-check flags (ka-0000008161). The fleet-mandated
// per-MR stale-decision sweep relies on `gb decision list --search <mr>`; if
// either flag is dropped the sweep silently regresses to unfiltered-list+grep.
func TestDecisionListSearchFlagsRegistered(t *testing.T) {
	for _, name := range []string{"search", "ref"} {
		f := decisionListCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("gb decision list is missing the --%s flag (reference-check, ka-0000008161)", name)
		}
		if f.Value.Type() != "string" {
			t.Errorf("--%s flag should be a string, got %q", name, f.Value.Type())
		}
	}
}
