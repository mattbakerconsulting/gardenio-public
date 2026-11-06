package bridge

import (
	"reflect"
	"testing"
)

// TestProjectSyncGate_DefaultOff_MatchesLegacy proves the gate with the FSM
// toggle OFF reproduces the legacy deny-by-default verdict exactly: PE/DEVOPS
// allowed, FSM blocked, everything else denied. This is the invariant that
// makes the change additive (ka-0000001679).
func TestProjectSyncGate_DefaultOff_MatchesLegacy(t *testing.T) {
	g := DefaultProjectSyncGate(false)
	cases := []struct {
		key  string
		want bool
	}{
		{"PE-1234", true},
		{"DEVOPS-7", true},
		{"FSM-100", false}, // customer service desk — blocked while toggle off
		{"ACME-1", false},  // not on the allow-list
		{"", false},        // empty
		{"NODASH", false},  // no project key resolvable to allow-list
		{"pe-1", false},    // project key is case-sensitive against the allow-list
	}
	for _, c := range cases {
		if got := g.AllowProject(c.key); got != c.want {
			t.Errorf("AllowProject(%q) with FSM off = %v, want %v", c.key, got, c.want)
		}
	}
}

// TestProjectSyncGate_FSMToggle proves the SINGLE toggle is the only thing that
// flips FSM, and that it flips ONLY FSM — PE/DEVOPS/other verdicts are
// unchanged whether the toggle is on or off (ka-0000001679).
func TestProjectSyncGate_FSMToggle(t *testing.T) {
	off := DefaultProjectSyncGate(false)
	on := DefaultProjectSyncGate(true)

	if off.AllowProject("FSM-100") {
		t.Error("FSM must be DENIED when toggle off")
	}
	if !on.AllowProject("FSM-100") {
		t.Error("FSM must be ALLOWED when toggle on")
	}
	// Flipping the toggle changes nothing else.
	for _, key := range []string{"PE-1", "DEVOPS-2", "ACME-3", ""} {
		if off.AllowProject(key) != on.AllowProject(key) {
			t.Errorf("toggle changed non-FSM verdict for %q: off=%v on=%v",
				key, off.AllowProject(key), on.AllowProject(key))
		}
	}
	if off.FSMSyncEnabled() {
		t.Error("FSMSyncEnabled() should be false for off gate")
	}
	if !on.FSMSyncEnabled() {
		t.Error("FSMSyncEnabled() should be true for on gate")
	}
}

// TestProjectSyncGate_NilSafe proves a nil *ProjectSyncGate behaves as the
// safe default (deny-by-default, FSM blocked) — so an unwired path never
// crashes and never accidentally writes FSM.
func TestProjectSyncGate_NilSafe(t *testing.T) {
	var g *ProjectSyncGate
	if g.AllowProject("FSM-1") {
		t.Error("nil gate must deny FSM")
	}
	if !g.AllowProject("PE-1") {
		t.Error("nil gate must allow PE (legacy default)")
	}
	if g.FSMSyncEnabled() {
		t.Error("nil gate FSMSyncEnabled() must be false")
	}
	if g.AllowedProjects() != nil {
		t.Error("nil gate AllowedProjects() must be nil")
	}
}

// TestProjectSyncGate_BlockWinsForNonFSM proves the block-list still wins over
// the allow-list for a non-FSM project (an explicit block is honored), while
// FSM is governed solely by the toggle and ignores its block-list membership.
func TestProjectSyncGate_BlockWinsForNonFSM(t *testing.T) {
	g := NewProjectSyncGate(
		map[string]bool{"PE": true, "FSM": true}, // PE explicitly blocked too
		map[string]bool{"PE": true, "DEVOPS": true},
		true, // FSM toggle ON
	)
	if g.AllowProject("PE-1") {
		t.Error("PE must be DENIED when on both allow and block lists (block wins)")
	}
	if !g.AllowProject("FSM-1") {
		t.Error("FSM must be ALLOWED by the toggle even though it is on the block-list")
	}
	if !g.AllowProject("DEVOPS-1") {
		t.Error("DEVOPS must remain allowed")
	}
}

// TestProjectSyncGate_AllowedProjects proves the diagnostic projection: FSM is
// listed only when the toggle is on; non-FSM blocked projects are excluded.
func TestProjectSyncGate_AllowedProjects(t *testing.T) {
	if got := DefaultProjectSyncGate(false).AllowedProjects(); !reflect.DeepEqual(got, []string{"DEVOPS", "PE"}) {
		t.Errorf("FSM-off writable set = %v, want [DEVOPS PE]", got)
	}
	if got := DefaultProjectSyncGate(true).AllowedProjects(); !reflect.DeepEqual(got, []string{"DEVOPS", "FSM", "PE"}) {
		t.Errorf("FSM-on writable set = %v, want [DEVOPS FSM PE]", got)
	}
}

// TestProjectSyncGate_CopiesInputMaps proves the constructor copies its input
// sets so later mutation of a caller's map cannot change the gate's verdicts —
// the gate's decision must be stable for the lifetime of the cutover.
func TestProjectSyncGate_CopiesInputMaps(t *testing.T) {
	allow := map[string]bool{"PE": true}
	g := NewProjectSyncGate(map[string]bool{"FSM": true}, allow, false)
	allow["ACME"] = true // mutate after construction
	if g.AllowProject("ACME-1") {
		t.Error("gate must not observe post-construction mutation of the allow-list")
	}
}
