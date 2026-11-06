package main

import (
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// readyBead is a tiny constructor for a ready row with a title, description, and
// id, used by the target-liveness lint tests.
func readyBead(id, title, desc string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Title: title, Description: desc, Status: "open"}
}

func target(id, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Status: status}
}

func TestExtractKDRefs(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		selfID string
		want   []string
	}{
		{
			name: "no refs",
			text: "just plain prose with no bead id",
			want: nil,
		},
		{
			name: "single ref in title",
			text: "close/disable trigger ka-0000005296",
			want: []string{"ka-0000005296"},
		},
		{
			name: "dedup and sort multiple refs",
			text: "route ka-0000004474 and ka-0000000242 and ka-0000004474 again",
			want: []string{"ka-0000000242", "ka-0000004474"},
		},
		{
			name:   "self ref excluded",
			text:   "gd-a8wfEfRhdf is itself the survey, also targets ka-0000005296",
			selfID: "gd-a8wfEfRhdf",
			want:   []string{"ka-0000005296"},
		},
		{
			name: "too-short id not matched",
			text: "not a real ref gd-short here",
			want: nil,
		},
		{
			name: "ref in parenthetical",
			text: "finalize MR !3194 + stage Step-B CI->shim (ka-0000003829)",
			want: []string{"ka-0000003829"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractKDRefs(tt.text, tt.selfID)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractKDRefs(%q, %q) = %v, want %v", tt.text, tt.selfID, got, tt.want)
			}
		})
	}
}

func TestLintReadyTargets(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		// All targets closed → flagged.
		readyBead("gd-5VMWGxszG8", "close/disable trigger ka-0000005296", ""),
		// One target still open → NOT flagged (abstain on any live target).
		readyBead("gd-openref00", "depends on gd-liveTarget1 landing", ""),
		// Target unresolvable (typo/deleted) → NOT flagged.
		readyBead("gd-aa2tbetj0i", "reword advice gd-ghosttarget", ""),
		// No refs at all → NOT flagged.
		readyBead("gd-dchglzfadz", "just do the thing, no bead ref", ""),
		// Multiple refs, all closed → flagged, refs sorted.
		readyBead("gd-aiod1puvoa", "route ka-0000004474 and ka-0000000242 to reviver", ""),
		// Multiple refs, one open → NOT flagged.
		readyBead("gd-71x8aodqhc", "ka-0000000242 closed but gd-liveTarget1 open", ""),
		// Action-target ref only in description → flagged.
		readyBead("gd-wscyzenq19", "finalize the migration", "reword advice ka-0000005296"),
	}
	resolved := map[string]*beadsapi.BeadDetail{
		"ka-0000005296":  target("ka-0000005296", "closed"),
		"gd-liveTarget1": target("gd-liveTarget1", "open"),
		"ka-0000004474":  target("ka-0000004474", "closed"),
		"ka-0000000242":  target("ka-0000000242", "closed"),
		// gd-ghosttarget deliberately absent → unresolvable.
	}

	findings := lintReadyTargets(beads, resolved)

	gotIDs := make([]string, 0, len(findings))
	for _, f := range findings {
		gotIDs = append(gotIDs, f.ID)
	}
	want := []string{"gd-5VMWGxszG8", "gd-aiod1puvoa", "gd-wscyzenq19"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("flagged rows = %v, want %v", gotIDs, want)
	}

	// The multi-ref finding must carry both closed targets, sorted.
	for _, f := range findings {
		if f.ID == "gd-aiod1puvoa" {
			wantTargets := []string{"ka-0000000242", "ka-0000004474"}
			if !reflect.DeepEqual(f.ClosedTargets, wantTargets) {
				t.Errorf("gd-aiod1puvoa ClosedTargets = %v, want %v", f.ClosedTargets, wantTargets)
			}
		}
	}
}

func TestRefIsEnablementOrContext(t *testing.T) {
	ref := "ka-0000005296"
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"follow-up to", "follow-up to ka-0000005296", true},
		{"followup one word", "followup ka-0000005296 needs the split", true},
		{"deferred from", "deferred from ka-0000005296", true},
		{"once ships", "once ka-0000005296 ships, wire the flag", true},
		{"blocked on", "blocked on ka-0000005296 landing", true},
		{"after lands", "after ka-0000005296 lands do X", true},
		{"when merges", "when ka-0000005296 merges", true},
		{"depends on", "depends on ka-0000005296", true},
		{"spawned by", "gap spawned by ka-0000005296", true},
		{"revealed by", "gap revealed by ka-0000005296 incident", true},
		{"typed mention parent", "parent: ka-0000005296", true},
		{"typed mention related", "related: ka-0000005296", true},
		{"unblocked by", "unblocked by ka-0000005296", true},
		// Action targets: NOT enablement/context — must return false.
		{"close trigger", "close/disable trigger ka-0000005296", false},
		{"reword advice", "reword advice ka-0000005296", false},
		{"finalize MR", "finalize MR (ka-0000005296)", false},
		{"bare ref", "ka-0000005296 cleanup", false},
		// superseded-by is intentionally a genuine mootness signal, NOT suppressed.
		{"superseded by", "superseded by ka-0000005296", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refIsEnablementOrContext(tt.text, ref); got != tt.want {
				t.Errorf("refIsEnablementOrContext(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestLintReadyTargets_EnablementRefsExcluded(t *testing.T) {
	// The dominant false-positive class from bug ka-0000001923: a row whose only
	// closed ref is an ENABLEMENT ref (follow-up to / deferred from / once ships /
	// blocked on / typed-mention prefix) must NOT be flagged, even though the ref
	// is closed. And an action-target ref that is still open must keep abstaining.
	beads := []*beadsapi.BeadDetail{
		// Enablement ref, closed → NOT flagged (was the 90% false-positive).
		readyBead("gd-followup000", "follow-up to ka-0000005296: land the shim", ""),
		readyBead("gd-deferred000", "deferred from ka-0000005296", ""),
		readyBead("gd-oncegate000", "once ka-0000005296 ships, flip the flag", ""),
		readyBead("gd-typedment00", "", "parent: ka-0000005296\nrelated: ka-0000004474"),
		// Action-target ref, closed → still flagged (real residue).
		readyBead("gd-action00000", "close/disable trigger ka-0000005296", ""),
		// Mixed: an enablement ref (closed) plus an ACTION ref that is still open →
		// NOT flagged (open action target abstains).
		readyBead("gd-vxzfofzbrq", "follow-up to ka-0000005296: reword advice gd-liveTarget1", ""),
	}
	resolved := map[string]*beadsapi.BeadDetail{
		"ka-0000005296":  target("ka-0000005296", "closed"),
		"ka-0000004474":  target("ka-0000004474", "closed"),
		"gd-liveTarget1": target("gd-liveTarget1", "open"),
	}

	findings := lintReadyTargets(beads, resolved)
	gotIDs := make([]string, 0, len(findings))
	for _, f := range findings {
		gotIDs = append(gotIDs, f.ID)
	}
	want := []string{"gd-action00000"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("flagged rows = %v, want %v", gotIDs, want)
	}
}

func TestLintReadyTargets_InProgressTargetIsLive(t *testing.T) {
	// A referenced target that is in_progress / deferred is still live work, so a
	// row naming only such targets must NOT be flagged.
	beads := []*beadsapi.BeadDetail{
		readyBead("gd-7d9ilvntat", "waiting on ka-0000005296", ""),
		readyBead("gd-sge5m56ysz", "waiting on gd-jjnghb6oqy", ""),
	}
	resolved := map[string]*beadsapi.BeadDetail{
		"ka-0000005296": target("ka-0000005296", "in_progress"),
		"gd-jjnghb6oqy": target("gd-jjnghb6oqy", "deferred"),
	}
	if findings := lintReadyTargets(beads, resolved); len(findings) != 0 {
		t.Fatalf("expected no findings for live (in_progress/deferred) targets, got %v", findings)
	}
}

func TestIsClosedStatus(t *testing.T) {
	tests := map[string]bool{
		"closed":      true,
		"Closed":      true,
		" closed ":    true,
		"open":        false,
		"in_progress": false,
		"deferred":    false,
		"":            false,
	}
	for status, want := range tests {
		if got := isClosedStatus(status); got != want {
			t.Errorf("isClosedStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestLintReadyTargets_NilBeadSkipped(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		nil,
		readyBead("gd-5VMWGxszG8", "close trigger ka-0000005296", ""),
	}
	resolved := map[string]*beadsapi.BeadDetail{
		"ka-0000005296": target("ka-0000005296", "closed"),
	}
	findings := lintReadyTargets(beads, resolved)
	if len(findings) != 1 || findings[0].ID != "gd-5VMWGxszG8" {
		t.Fatalf("expected 1 finding for gd-5VMWGxszG8, got %v", findings)
	}
}
