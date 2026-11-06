package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// iterBead builds a minimal agent iteration bead for the timeline tests.
// created is parsed from an RFC3339 string ("" => zero time).
func iterBead(id, prior, created, status string, runsSpawned, maxRuns int) *beadsapi.BeadDetail {
	f := map[string]string{}
	if prior != "" {
		f[fieldSchedulePriorAgentID] = prior
	}
	if runsSpawned > 0 {
		f[fieldScheduleRunsSpawned] = strconv.Itoa(runsSpawned)
	}
	if maxRuns > 0 {
		f[fieldScheduleMaxRuns] = strconv.Itoa(maxRuns)
	}
	var ts time.Time
	if created != "" {
		ts, _ = time.Parse(time.RFC3339, created)
	}
	if status == "" {
		status = "closed"
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "agent",
		Status:    status,
		Fields:    f,
		CreatedAt: ts,
	}
}

// withScheduleKey stamps a schedule_key fire-slot on a bead (fluent helper for
// the trigger-path ordering tests) and returns the same bead.
func withScheduleKey(b *beadsapi.BeadDetail, key string) *beadsapi.BeadDetail {
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	b.Fields[fieldScheduleKey] = key
	return b
}

// idBuild is a trivial build func: it only carries the bead ID through into the
// agentRowView so tests can assert ordering without a *Server.
func idBuild(b *beadsapi.BeadDetail) agentRowView {
	return agentRowView{ID: b.ID, Status: b.Status}
}

// runNumbersByID flattens the result into id -> RunNumber for assertions
// independent of the newest-first display order.
func runNumbersByID(views []gardenerIterationView) map[string]int {
	m := make(map[string]int, len(views))
	for _, v := range views {
		m[v.ID] = v.RunNumber
	}
	return m
}

func TestOrderGardenerIterations_ChainCanonical(t *testing.T) {
	// Causal chain a(root) <- b <- c(head). created_at deliberately does NOT
	// match the chain order, to prove the chain — not time — is authoritative.
	beads := []*beadsapi.BeadDetail{
		iterBead("c", "b", "2020-01-01T00:00:00Z", "closed", 3, 10), // earliest time, but newest run
		iterBead("a", "", "2020-01-03T00:00:00Z", "closed", 1, 10),  // latest time, but root
		iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 2, 10),
	}
	views := orderGardenerIterations(beads, "c", idBuild)

	if len(views) != 3 {
		t.Fatalf("want 3 views, got %d", len(views))
	}
	nums := runNumbersByID(views)
	if nums["a"] != 1 || nums["b"] != 2 || nums["c"] != 3 {
		t.Fatalf("chain numbering wrong: a=%d b=%d c=%d (want 1,2,3)", nums["a"], nums["b"], nums["c"])
	}
	for _, v := range views {
		if v.ByTime {
			t.Errorf("run %s should NOT be ByTime when chain is intact", v.ID)
		}
	}
	// Newest-first display: head (c, run 3) on top.
	if views[0].ID != "c" || views[len(views)-1].ID != "a" {
		t.Fatalf("display order not newest-first: top=%s bottom=%s", views[0].ID, views[len(views)-1].ID)
	}
}

func TestOrderGardenerIterations_HeadFromNothingPointsAt(t *testing.T) {
	// No lastAgentID supplied; head must be inferred as the run nothing points
	// at (c). Chain still fully covers the set, so NOT ByTime.
	beads := []*beadsapi.BeadDetail{
		iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 1, 0),
		iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 2, 0),
		iterBead("c", "b", "2020-01-03T00:00:00Z", "closed", 3, 0),
	}
	views := orderGardenerIterations(beads, "", idBuild)
	nums := runNumbersByID(views)
	if nums["a"] != 1 || nums["b"] != 2 || nums["c"] != 3 {
		t.Fatalf("inferred-head numbering wrong: %v", nums)
	}
	for _, v := range views {
		if v.ByTime {
			t.Errorf("intact chain should not be ByTime, got ByTime for %s", v.ID)
		}
	}
}

func TestOrderGardenerIterations_StaleLastAgentIDFallsBackToInferredHead(t *testing.T) {
	// lastAgentID points at a non-head (b is referenced by c). The inferred
	// head (c) must win; the chain stays canonical.
	beads := []*beadsapi.BeadDetail{
		iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 1, 0),
		iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 2, 0),
		iterBead("c", "b", "2020-01-03T00:00:00Z", "closed", 3, 0),
	}
	views := orderGardenerIterations(beads, "b", idBuild)
	nums := runNumbersByID(views)
	if nums["a"] != 1 || nums["b"] != 2 || nums["c"] != 3 {
		t.Fatalf("stale lastAgentID should not corrupt numbering: %v", nums)
	}
	if views[0].ID != "c" {
		t.Fatalf("head should still be c, got top=%s", views[0].ID)
	}
}

func TestOrderGardenerIterations_DanglingPriorFallsBackToTime(t *testing.T) {
	// b's prior points at "ghost" which is not in the set => chain broken =>
	// created_at fallback, every card ByTime.
	beads := []*beadsapi.BeadDetail{
		iterBead("b", "ghost", "2020-01-02T00:00:00Z", "closed", 5, 10),
		iterBead("c", "b", "2020-01-03T00:00:00Z", "closed", 6, 10),
	}
	views := orderGardenerIterations(beads, "c", idBuild)
	nums := runNumbersByID(views)
	// Time order: b (Jan 2) = run 1, c (Jan 3) = run 2.
	if nums["b"] != 1 || nums["c"] != 2 {
		t.Fatalf("time-fallback numbering wrong: %v", nums)
	}
	for _, v := range views {
		if !v.ByTime {
			t.Errorf("dangling chain should mark %s ByTime", v.ID)
		}
	}
}

func TestOrderGardenerIterations_PartialChainFallsBackToTime(t *testing.T) {
	// a<-b is a chain, but d is an off-chain orphan (empty prior). Two heads
	// (b and d) means no unique chain over the whole set => fallback.
	beads := []*beadsapi.BeadDetail{
		iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 1, 0),
		iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 2, 0),
		iterBead("d", "", "2020-01-04T00:00:00Z", "closed", 1, 0), // restart-root orphan
	}
	views := orderGardenerIterations(beads, "", idBuild)
	nums := runNumbersByID(views)
	if nums["a"] != 1 || nums["b"] != 2 || nums["d"] != 3 {
		t.Fatalf("partial-chain fallback numbering wrong: %v", nums)
	}
	for _, v := range views {
		if !v.ByTime {
			t.Errorf("broken-coverage chain should mark %s ByTime", v.ID)
		}
	}
}

func TestOrderGardenerIterations_CycleFallsBackToTime(t *testing.T) {
	// a<->b cycle (a.prior=b, b.prior=a). No root, no unique head => fallback,
	// and the walk must terminate (no infinite loop).
	beads := []*beadsapi.BeadDetail{
		iterBead("a", "b", "2020-01-01T00:00:00Z", "closed", 0, 0),
		iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 0, 0),
	}
	views := orderGardenerIterations(beads, "a", idBuild)
	if len(views) != 2 {
		t.Fatalf("want 2 views, got %d", len(views))
	}
	for _, v := range views {
		if !v.ByTime {
			t.Errorf("cyclic chain should mark %s ByTime", v.ID)
		}
	}
}

func TestOrderGardenerIterations_ScheduleKeyOrderIsGenuine(t *testing.T) {
	// Trigger-path gardener: NO schedule_prior_agent_id chain, but every
	// iteration carries a schedule_key fire-slot. The fire-slot order is real,
	// so cards must NOT be ByTime. created_at deliberately disagrees with the
	// fire-slot order to prove schedule_key — not created_at — is the key.
	beads := []*beadsapi.BeadDetail{
		withScheduleKey(iterBead("c", "", "2020-01-01T00:00:00Z", "closed", 0, 0), "2026-06-02T07:11"), // earliest time, latest slot
		withScheduleKey(iterBead("a", "", "2020-01-03T00:00:00Z", "closed", 0, 0), "2026-06-02T06:11"), // latest time, earliest slot
		withScheduleKey(iterBead("b", "", "2020-01-02T00:00:00Z", "closed", 0, 0), "2026-06-02T06:41"),
	}
	views := orderGardenerIterations(beads, "", idBuild)
	nums := runNumbersByID(views)
	if nums["a"] != 1 || nums["b"] != 2 || nums["c"] != 3 {
		t.Fatalf("schedule_key numbering wrong: a=%d b=%d c=%d (want 1,2,3)", nums["a"], nums["b"], nums["c"])
	}
	for _, v := range views {
		if v.ByTime {
			t.Errorf("schedule_key order is genuine; %s should NOT be ByTime", v.ID)
		}
		// Per-iteration basis: the fire-slot tier, labeled "(by fire order)"
		// — never the heuristic "(by time)".
		if v.Basis != basisFireKey || !v.ByFireOrder() {
			t.Errorf("%s: Basis=%d ByFireOrder=%v, want basisFireKey", v.ID, v.Basis, v.ByFireOrder())
		}
		if !strings.HasSuffix(v.RunLabel, "(by fire order)") {
			t.Errorf("%s: RunLabel=%q should end with (by fire order)", v.ID, v.RunLabel)
		}
	}
	// Newest-first display: latest fire-slot (c, run 3) on top.
	if views[0].ID != "c" || views[len(views)-1].ID != "a" {
		t.Fatalf("display order not newest-first: top=%s bottom=%s", views[0].ID, views[len(views)-1].ID)
	}
}

func TestOrderGardenerIterations_ScheduleKeyTieBreaksByCreatedAt(t *testing.T) {
	// Two fires in the same minute slot: the schedule_key tie must break by
	// created_at (then ID), still a genuine (non-ByTime) order.
	beads := []*beadsapi.BeadDetail{
		withScheduleKey(iterBead("b", "", "2020-01-02T00:00:00Z", "closed", 0, 0), "2026-06-02T07:11"),
		withScheduleKey(iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 0, 0), "2026-06-02T07:11"),
	}
	views := orderGardenerIterations(beads, "", idBuild)
	nums := runNumbersByID(views)
	if nums["a"] != 1 || nums["b"] != 2 {
		t.Fatalf("same-slot tie should break by created_at: %v", nums)
	}
	for _, v := range views {
		if v.ByTime {
			t.Errorf("schedule_key order (with tie) is genuine; %s should NOT be ByTime", v.ID)
		}
	}
}

func TestOrderGardenerIterations_PartialScheduleKeyFallsBackToTime(t *testing.T) {
	// Mixed set: one iteration is missing schedule_key, so the fire-slot order
	// cannot be trusted over the whole set — drop to the created_at heuristic
	// and mark every card ByTime.
	beads := []*beadsapi.BeadDetail{
		withScheduleKey(iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 0, 0), "2026-06-02T07:11"),
		iterBead("b", "", "2020-01-02T00:00:00Z", "closed", 0, 0), // no schedule_key
	}
	views := orderGardenerIterations(beads, "", idBuild)
	nums := runNumbersByID(views)
	// created_at order: a (Jan 1) = run 1, b (Jan 2) = run 2.
	if nums["a"] != 1 || nums["b"] != 2 {
		t.Fatalf("partial-key fallback numbering wrong: %v", nums)
	}
	for _, v := range views {
		if !v.ByTime {
			t.Errorf("partial schedule_key coverage should mark %s ByTime", v.ID)
		}
		if v.Basis != basisTime || v.ByFireOrder() {
			t.Errorf("%s: Basis=%d ByFireOrder=%v, want basisTime", v.ID, v.Basis, v.ByFireOrder())
		}
		if !strings.HasSuffix(v.RunLabel, "(by time)") {
			t.Errorf("%s: RunLabel=%q should end with (by time)", v.ID, v.RunLabel)
		}
	}
}

// TestOrderGardenerIterations_OneBrokenEdgeDoesNotStampWholeThread is the core
// design-doc §2.2/§8 grievance: orderGardenerIterations used to return ONE
// whole-thread byTime bool, so a single broken chain edge stamped EVERY card
// "(by time)" (the CDQR bytime_ribbons 13/13). With the schedule_key fire-slot
// tier carried PER ITERATION as basisFireKey, a broken chain on a trigger-path
// gardener now recovers a genuine "(by fire order)" label — never "(by time)".
func TestOrderGardenerIterations_OneBrokenEdgeDoesNotStampWholeThread(t *testing.T) {
	// 13 trigger-path iterations, all carrying schedule_key, with a dangling
	// prior on one bead (the broken edge) so the causal chain cannot be walked.
	beads := make([]*beadsapi.BeadDetail, 0, 13)
	for i := range 13 {
		prior := ""
		if i == 7 {
			prior = "gd-pruned-ghost" // dangling predecessor => chain broken
		}
		// monotonic fire-slots :00 .. :12 (fixed-width so lexical == chronological)
		slot := "2026-06-02T07:" + strconv.Itoa(10 + i)[len(strconv.Itoa(10+i))-2:]
		created := "2020-01-01T00:00:00Z"
		beads = append(beads, withScheduleKey(iterBead("k"+strconv.Itoa(i), prior, created, "closed", 0, 0), slot))
	}
	views := orderGardenerIterations(beads, "", idBuild)
	if len(views) != 13 {
		t.Fatalf("want 13 views, got %d", len(views))
	}
	for _, v := range views {
		if v.ByTime || v.Basis != basisFireKey {
			t.Errorf("%s: one broken edge must NOT stamp the thread by-time: Basis=%d ByTime=%v", v.ID, v.Basis, v.ByTime)
		}
		if !strings.HasSuffix(v.RunLabel, "(by fire order)") {
			t.Errorf("%s: RunLabel=%q should read (by fire order)", v.ID, v.RunLabel)
		}
	}
}

func TestOrderGardenerIterations_ChainBeatsScheduleKey(t *testing.T) {
	// When BOTH a valid prior-agent chain AND schedule_keys are present, the
	// causal chain wins (PRIMARY tier). schedule_key deliberately disagrees
	// with the chain to prove precedence.
	beads := []*beadsapi.BeadDetail{
		withScheduleKey(iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 0, 0), "2026-06-02T09:00"),
		withScheduleKey(iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 0, 0), "2026-06-02T08:00"),
		withScheduleKey(iterBead("c", "b", "2020-01-03T00:00:00Z", "closed", 0, 0), "2026-06-02T07:00"),
	}
	views := orderGardenerIterations(beads, "c", idBuild)
	nums := runNumbersByID(views)
	if nums["a"] != 1 || nums["b"] != 2 || nums["c"] != 3 {
		t.Fatalf("chain should beat schedule_key: %v", nums)
	}
	for _, v := range views {
		if v.ByTime {
			t.Errorf("intact chain should not be ByTime, got ByTime for %s", v.ID)
		}
	}
}

func TestOrderGardenerIterations_BrokenChainWithScheduleKeyUsesKey(t *testing.T) {
	// Dangling prior (b->ghost) breaks the chain, but both iterations carry a
	// schedule_key — so we recover a genuine fire-slot order rather than the
	// created_at heuristic. created_at disagrees with the slot order to prove
	// the key is used.
	beads := []*beadsapi.BeadDetail{
		withScheduleKey(iterBead("b", "ghost", "2020-01-03T00:00:00Z", "closed", 0, 0), "2026-06-02T07:11"),
		withScheduleKey(iterBead("c", "b", "2020-01-02T00:00:00Z", "closed", 0, 0), "2026-06-02T07:41"),
	}
	views := orderGardenerIterations(beads, "", idBuild)
	nums := runNumbersByID(views)
	// Fire-slot order: b (07:11) = run 1, c (07:41) = run 2 — NOT created_at.
	if nums["b"] != 1 || nums["c"] != 2 {
		t.Fatalf("broken chain should recover via schedule_key: %v", nums)
	}
	for _, v := range views {
		if v.ByTime {
			t.Errorf("schedule_key recovery is genuine; %s should NOT be ByTime", v.ID)
		}
	}
}

func TestOrderGardenerIterations_InFlightTopWithLabel(t *testing.T) {
	// Latest run c is open => IsLive, "(in progress)", and on top of the
	// newest-first thread.
	beads := []*beadsapi.BeadDetail{
		iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 1, 10),
		iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 2, 10),
		iterBead("c", "b", "2020-01-03T00:00:00Z", "in_progress", 3, 10),
	}
	views := orderGardenerIterations(beads, "c", idBuild)

	top := views[0]
	if top.ID != "c" {
		t.Fatalf("in-flight run should be on top, got %s", top.ID)
	}
	if !top.IsLive {
		t.Fatalf("open run should be IsLive")
	}
	if top.RunLabel != "Iteration 3 (in progress)" {
		t.Fatalf("in-flight label wrong: %q", top.RunLabel)
	}
	// A closed run shows "of N".
	var bView gardenerIterationView
	for _, v := range views {
		if v.ID == "b" {
			bView = v
		}
	}
	if bView.RunLabel != "Iteration 2 of 10" {
		t.Fatalf("closed label wrong: %q", bView.RunLabel)
	}
}

func TestRunLabel(t *testing.T) {
	cases := []struct {
		runNumber, maxRuns int
		isLive             bool
		basis              orderBasis
		want               string
	}{
		{3, 10, false, basisChain, "Iteration 3 of 10"},
		{3, 0, false, basisChain, "Iteration 3"},
		{7, 10, true, basisChain, "Iteration 7 (in progress)"},
		// Fire-slot recovery is genuine — labeled "(by fire order)", not "(by time)".
		{3, 10, false, basisFireKey, "Iteration 3 of 10 (by fire order)"},
		{7, 10, true, basisFireKey, "Iteration 7 (in progress) (by fire order)"},
		// created_at heuristic last resort.
		{3, 10, false, basisTime, "Iteration 3 of 10 (by time)"},
		{7, 10, true, basisTime, "Iteration 7 (in progress) (by time)"},
		{1, 0, true, basisChain, "Iteration 1 (in progress)"},
	}
	for _, c := range cases {
		got := runLabel(c.runNumber, c.maxRuns, c.isLive, c.basis)
		if got != c.want {
			t.Errorf("runLabel(%d,%d,%v,%v)=%q want %q", c.runNumber, c.maxRuns, c.isLive, c.basis, got, c.want)
		}
	}
}

func TestParseScheduleInt(t *testing.T) {
	f := map[string]string{"a": "7", "b": "-2", "c": "x", "d": ""}
	if got := parseScheduleInt(f, "a"); got != 7 {
		t.Errorf("a: got %d want 7", got)
	}
	if got := parseScheduleInt(f, "b"); got != 0 { // negative => 0
		t.Errorf("b: got %d want 0", got)
	}
	if got := parseScheduleInt(f, "c"); got != 0 { // unparseable => 0
		t.Errorf("c: got %d want 0", got)
	}
	if got := parseScheduleInt(f, "missing"); got != 0 {
		t.Errorf("missing: got %d want 0", got)
	}
	if got := parseScheduleInt(nil, "a"); got != 0 {
		t.Errorf("nil fields: got %d want 0", got)
	}
}

func TestOrderGardenerIterations_Empty(t *testing.T) {
	if got := orderGardenerIterations(nil, "", idBuild); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
	// Beads slice with only nil/blank entries.
	if got := orderGardenerIterations([]*beadsapi.BeadDetail{nil, {ID: ""}}, "", idBuild); len(got) != 0 {
		t.Errorf("blank beads should yield no views, got %d", len(got))
	}
}

func TestOrderGardenerIterations_Single(t *testing.T) {
	// One in-flight run: the common "gardener just started" case.
	beads := []*beadsapi.BeadDetail{
		iterBead("a", "", "2020-01-01T00:00:00Z", "in_progress", 1, 5),
	}
	views := orderGardenerIterations(beads, "a", idBuild)
	if len(views) != 1 {
		t.Fatalf("want 1 view, got %d", len(views))
	}
	v := views[0]
	if v.RunNumber != 1 || !v.IsLive || v.ByTime {
		t.Fatalf("single run wrong: num=%d live=%v byTime=%v", v.RunNumber, v.IsLive, v.ByTime)
	}
	if v.RunLabel != "Iteration 1 (in progress)" {
		t.Fatalf("single run label wrong: %q", v.RunLabel)
	}
}

func TestOrderGardenerIterations_FieldsOverlaid(t *testing.T) {
	// The schedule_* fields are surfaced onto the view for display.
	b := iterBead("b", "a", "2020-01-02T00:00:00Z", "closed", 2, 10)
	b.Fields[fieldScheduleTerminalGoal] = "ship the page"
	beads := []*beadsapi.BeadDetail{
		iterBead("a", "", "2020-01-01T00:00:00Z", "closed", 1, 10),
		b,
	}
	views := orderGardenerIterations(beads, "b", idBuild)
	var bv gardenerIterationView
	for _, v := range views {
		if v.ID == "b" {
			bv = v
		}
	}
	if bv.RunsSpawned != 2 || bv.MaxRuns != 10 || bv.PriorAgentID != "a" || bv.TerminalGoal != "ship the page" {
		t.Fatalf("overlaid fields wrong: %+v", bv)
	}
}
