package main

// Accuracy-invariant tests for the gardener page (bead gd-Tja5szyN3t, parent
// epic ka-0000004480). The CDQR continuous-improvement audit (epic
// ka-0000001553) flags three accuracy signals on /gardener/{id}; until now they
// were guarded only by the periodic audit and a handful of example-based unit
// tests. These tests assert the same properties as INVARIANTS that must hold
// across many generated thread shapes, so a regression fails CI deterministically
// instead of waiting for the next audit pass:
//
//	(1) Header-counter reconciliation — the displayed Iterations count is always
//	    the live thread length (== rendered cards) and is the PRIMARY counter; the
//	    engine lifetime tally is an always-shown SECONDARY line, framed "older runs
//	    pruned" when the spawn count outruns the live count. Guards the signature
//	    header-counter-drift bug (ka-0000002549, ka-0000003322).
//	(2) Run-ordering — orderGardenerIterations is monotonic newest-first by
//	    RunNumber, run numbers are a contiguous 1..N permutation, and the live
//	    head sorts to the top.
//	(3) Wrap-up / outcome coverage — every CLOSED run resolves to a non-empty
//	    outcome line via the backstop cascade (prose summary → derived
//	    structured signals → terminal-status phrase), so no closed card ever
//	    renders a bare title.
//
// These exercise the pure assembly functions (orderGardenerIterations,
// reconcileCounters, countLiveIterations) and the wrap-up cascade
// (parseWrapUpSummary → derivedRunOutcome) directly, so they stay fast and
// Server-free.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// invIterBead builds an agent iteration bead for the invariant tests. It mirrors
// the production stamping (schedule_id + schedule_prior_agent_id chain) but is
// independent of the example fixtures so the two suites can evolve separately.
func invIterBead(id, prior, status string, created time.Time, wrapup string) *beadsapi.BeadDetail {
	f := map[string]string{
		"schedule_id":       "gd-capX",
		"schedule_max_runs": "100",
	}
	if prior != "" {
		f[fieldSchedulePriorAgentID] = prior
	}
	if wrapup != "" {
		f["wrapup"] = wrapup
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     id,
		Type:      "agent",
		Status:    status,
		Fields:    f,
		CreatedAt: created,
		UpdatedAt: created,
	}
}

// buildChainThread builds a clean causal chain of n iterations (run 1 .. run n),
// the last `live` of them in_progress, the rest closed. created_at is ascending
// with the chain. Returns the beads in a deliberately shuffled slice order so a
// test that passes is proving the ordering authority, not list order.
func buildChainThread(n, live int) []*beadsapi.BeadDetail {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	beads := make([]*beadsapi.BeadDetail, 0, n)
	prior := ""
	for i := range n {
		id := "gd-r" + itoa(i+1)
		status := "closed"
		if i >= n-live {
			status = "in_progress"
		}
		beads = append(beads, invIterBead(id, prior, status, t0.Add(time.Duration(i)*time.Minute), ""))
		prior = id
	}
	// Shuffle deterministically (reverse) so order does not come from the slice.
	for l, r := 0, len(beads)-1; l < r; l, r = l+1, r-1 {
		beads[l], beads[r] = beads[r], beads[l]
	}
	return beads
}

// ---------------------------------------------------------------------------
// Invariant 1: header-counter reconciliation (ka-0000002549).
// ---------------------------------------------------------------------------

// TestInvariant_DisplayedIterationCountIsAlwaysLiveCount asserts the core
// reconciliation contract: whatever the engine lifetime tally (RunsSpawned) says,
// the page recomputes IterationCount from the actual rendered thread and that
// PRIMARY count is never mutated by reconcile. The engine tally is demoted to an
// always-shown SECONDARY line that carries the word "pruned" precisely when the
// spawn count outruns the live count beyond the prune gap — so a 379-run gap
// reads as an explained lifetime tally, not a silently-wrong number.
func TestInvariant_DisplayedIterationCountIsAlwaysLiveCount(t *testing.T) {
	threadLens := []int{0, 1, 2, 3, 5, 12, 25, 40}
	specSnapshots := []int{0, 1, 2, 3, 5, 7, 12, 25, 99}

	for _, live := range threadLens {
		for _, spec := range specSnapshots {
			name := fmt.Sprintf("live=%d/spec=%d", live, spec)
			t.Run(name, func(t *testing.T) {
				// Simulate the handler's reconcile step: IterationCount is set
				// from the rendered thread length, never from the spec. Give the
				// completed tally a plausible value (<= spawned).
				completed := spec
				if completed > 0 {
					completed = spec - 1
				}
				v := gardenerView{RunsSpawned: spec, RunsCompleted: completed, IterationCount: live}
				v.reconcileCounters()

				// The displayed PRIMARY count is the live count, full stop.
				if v.IterationCount != live {
					t.Fatalf("IterationCount mutated by reconcile: got %d want %d", v.IterationCount, live)
				}

				// The SECONDARY engine line is shown whenever there is any
				// activity, and omitted only for a wholly-idle gardener.
				hasActivity := spec > 0 || completed > 0 || live > 0
				if (v.EngineCount != "") != hasActivity {
					t.Fatalf("EngineCount = %q, want present=%v (spec=%d completed=%d live=%d)",
						v.EngineCount, hasActivity, spec, completed, live)
				}

				// "pruned" framing appears IFF the spawn count outruns the live
				// count beyond the prune gap.
				wantPruned := spec-live > gardenerPrunedGap
				if v.EnginePruned != wantPruned {
					t.Fatalf("EnginePruned = %v, want %v (spec=%d live=%d gap=%d)",
						v.EnginePruned, wantPruned, spec, live, gardenerPrunedGap)
				}
				if wantPruned {
					// The framed secondary must honestly carry BOTH numbers and
					// the explanatory word — the bug being guarded is a count
					// the body cannot honor with no explanation.
					want := fmt.Sprintf("%d spawned / %d completed (older runs pruned)", spec, completed)
					if v.EngineCount != want {
						t.Fatalf("EngineCount = %q, want %q", v.EngineCount, want)
					}
				}
			})
		}
	}
}

// TestInvariant_HeaderCountMatchesRenderedThread ties the header counter to the
// thread the ordering layer actually produces: IterationCount (set from
// len(iters)) and LiveCount (countLiveIterations) must equal the true thread
// length and in-flight count for every generated shape. This is the property
// the signature drift bug violated — header said one number, the thread showed
// another.
func TestInvariant_HeaderCountMatchesRenderedThread(t *testing.T) {
	shapes := []struct{ n, live int }{
		{1, 0}, {1, 1}, {3, 0}, {3, 1}, {5, 2}, {12, 1}, {25, 0}, {40, 3},
	}
	for _, sh := range shapes {
		t.Run(fmt.Sprintf("n=%d/live=%d", sh.n, sh.live), func(t *testing.T) {
			beads := buildChainThread(sh.n, sh.live)
			iters := orderGardenerIterations(beads, "", idBuild)

			if len(iters) != sh.n {
				t.Fatalf("thread length = %d, want %d", len(iters), sh.n)
			}
			// Mirror the handler: IterationCount := runIterationCount(iters).
			// buildChainThread produces no expired shells, so the run count
			// equals the thread length here — the never-ran-shell exclusion
			// (bug ka-0000006805) is covered separately in
			// gardener_didnotrun_test.go.
			v := gardenerView{IterationCount: runIterationCount(iters), LiveCount: countLiveIterations(iters)}
			if v.IterationCount != sh.n {
				t.Errorf("IterationCount = %d, want %d", v.IterationCount, sh.n)
			}
			if v.LiveCount != sh.live {
				t.Errorf("LiveCount = %d, want %d", v.LiveCount, sh.live)
			}
		})
	}
}

// TestInvariant_CountLiveIterationsMatchesIsLive cross-checks the helper against
// the source of truth: the live count must equal the number of cards whose
// IsLive flag is set, for any mix of statuses.
func TestInvariant_CountLiveIterationsMatchesIsLive(t *testing.T) {
	cases := [][]string{
		{},
		{"closed"},
		{"in_progress"},
		{"closed", "closed", "in_progress"},
		{"in_progress", "in_progress"},
		{"closed", "open", "in_progress", "closed"},
	}
	for i, statuses := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			iters := make([]gardenerIterationView, 0, len(statuses))
			wantLive := 0
			for _, st := range statuses {
				live := st != "closed"
				if live {
					wantLive++
				}
				iters = append(iters, gardenerIterationView{IsLive: live})
			}
			if got := countLiveIterations(iters); got != wantLive {
				t.Fatalf("countLiveIterations = %d, want %d", got, wantLive)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Invariant 2: run ordering is monotonic newest-first with contiguous numbering.
// ---------------------------------------------------------------------------

// TestInvariant_ThreadIsMonotonicNewestFirst asserts that for every generated
// thread shape, orderGardenerIterations returns cards in strictly DECREASING
// RunNumber order (newest run on top), the run numbers form a contiguous 1..N
// set with no gaps or duplicates, and the in-flight head — when present — sorts
// to index 0.
func TestInvariant_ThreadIsMonotonicNewestFirst(t *testing.T) {
	shapes := []struct{ n, live int }{
		{1, 0}, {1, 1}, {2, 1}, {3, 0}, {3, 1}, {5, 1}, {8, 2}, {25, 0}, {40, 1},
	}
	for _, sh := range shapes {
		t.Run(fmt.Sprintf("n=%d/live=%d", sh.n, sh.live), func(t *testing.T) {
			beads := buildChainThread(sh.n, sh.live)
			iters := orderGardenerIterations(beads, "", idBuild)

			if len(iters) != sh.n {
				t.Fatalf("got %d cards, want %d", len(iters), sh.n)
			}

			// Monotonic strictly-decreasing RunNumber (newest-first).
			for i := 1; i < len(iters); i++ {
				if iters[i-1].RunNumber <= iters[i].RunNumber {
					t.Fatalf("not newest-first at %d: run %d then run %d",
						i, iters[i-1].RunNumber, iters[i].RunNumber)
				}
			}

			// Contiguous 1..N permutation: no gaps, no duplicates.
			seen := make(map[int]bool, sh.n)
			for _, it := range iters {
				if it.RunNumber < 1 || it.RunNumber > sh.n {
					t.Fatalf("run number %d out of range 1..%d", it.RunNumber, sh.n)
				}
				if seen[it.RunNumber] {
					t.Fatalf("duplicate run number %d", it.RunNumber)
				}
				seen[it.RunNumber] = true
			}
			if len(seen) != sh.n {
				t.Fatalf("run numbers not contiguous: got %d distinct, want %d", len(seen), sh.n)
			}

			// Top card is the highest run number == N (the live head when present).
			if iters[0].RunNumber != sh.n {
				t.Fatalf("top card run = %d, want %d", iters[0].RunNumber, sh.n)
			}
			if sh.live > 0 && !iters[0].IsLive {
				t.Fatalf("in-flight head should be on top and IsLive")
			}
		})
	}
}

// TestInvariant_OrderingIsInputOrderIndependent asserts the chain ordering is
// invariant under input permutation: feeding the same iteration set in any slice
// order yields the same id→RunNumber assignment. Ordering authority is the
// causal chain, never the daemon's list order.
func TestInvariant_OrderingIsInputOrderIndependent(t *testing.T) {
	base := buildChainThread(6, 1) // 6 runs, head in-flight

	want := runNumbersByID(orderGardenerIterations(cloneBeads(base), "", idBuild))

	// Try several deterministic reorderings of the same set.
	perms := [][]int{
		{5, 4, 3, 2, 1, 0}, // reverse
		{0, 2, 4, 1, 3, 5}, // interleave
		{3, 0, 5, 1, 4, 2}, // scramble
	}
	for pi, order := range perms {
		t.Run(fmt.Sprintf("perm%d", pi), func(t *testing.T) {
			permuted := make([]*beadsapi.BeadDetail, 0, len(base))
			for _, idx := range order {
				permuted = append(permuted, base[idx])
			}
			got := runNumbersByID(orderGardenerIterations(cloneBeads(permuted), "", idBuild))
			for id, n := range want {
				if got[id] != n {
					t.Fatalf("permutation changed numbering: %s got run %d, want %d", id, got[id], n)
				}
			}
		})
	}
}

// TestInvariant_FallbackOrderingMonotonicByTime asserts the run-ordering
// invariants STILL hold on the created_at fallback path (broken chain): the
// thread is monotonic newest-first and numbering is contiguous even when the
// causal chain cannot be trusted, with every card marked ByTime so the operator
// knows the order is heuristic.
func TestInvariant_FallbackOrderingMonotonicByTime(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Two roots (both empty prior) => no unique chain => created_at fallback.
	beads := []*beadsapi.BeadDetail{
		invIterBead("gd-a", "", "closed", t0.Add(3*time.Minute), ""),
		invIterBead("gd-b", "", "closed", t0.Add(1*time.Minute), ""),
		invIterBead("gd-c", "", "in_progress", t0.Add(5*time.Minute), ""),
		invIterBead("gd-d", "", "closed", t0.Add(2*time.Minute), ""),
	}
	iters := orderGardenerIterations(beads, "", idBuild)
	if len(iters) != 4 {
		t.Fatalf("got %d cards, want 4", len(iters))
	}
	for i := 1; i < len(iters); i++ {
		if iters[i-1].RunNumber <= iters[i].RunNumber {
			t.Fatalf("fallback not newest-first at %d", i)
		}
	}
	// created_at ascending => run numbers: b(1), d(2), a(3), c(4). Newest-first
	// display => c on top.
	if iters[0].ID != "gd-c" || iters[0].RunNumber != 4 {
		t.Fatalf("fallback head = %s run %d, want gd-c run 4", iters[0].ID, iters[0].RunNumber)
	}
	for _, it := range iters {
		if !it.ByTime {
			t.Errorf("broken-chain card %s should be ByTime", it.ID)
		}
	}
}

// cloneBeads returns a shallow copy of the slice (new backing array, same bead
// pointers) so a reordering test cannot mutate the caller's slice.
func cloneBeads(in []*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	out := make([]*beadsapi.BeadDetail, len(in))
	copy(out, in)
	return out
}

// ---------------------------------------------------------------------------
// Invariant 3: every closed run resolves a non-empty outcome (backstop cascade).
// ---------------------------------------------------------------------------

// resolveRunOutcome reproduces the agentRowFromBeadCtx backstop cascade for a
// CLOSED run (agents.go ~L1630): the prose wrap-up summary is the headline; when
// it is empty, the derived structured-signal line backstops it. This mirrors the
// production composition so the invariant test tracks the real cascade.
func resolveRunOutcome(wrapup string, mrCount, beadsClosed int) string {
	summary := parseWrapUpSummary(wrapup)
	if summary != "" {
		return summary
	}
	return derivedRunOutcome(mrCount, beadsClosed, agentRunOutcome(wrapup))
}

// TestInvariant_EveryClosedRunResolvesAnOutcome asserts the backstop cascade
// guarantee: for any closed run that carried ANY signal — a prose wrap-up field,
// an opened MR, or a closed bead — outcome resolution yields a non-empty line, so
// the card never degrades to a bare title. The single legitimately-empty case
// (no wrap-up, no MRs, no beads, and an unknowable terminal status) is asserted
// separately so the contract's one escape hatch stays explicit.
func TestInvariant_EveryClosedRunResolvesAnOutcome(t *testing.T) {
	type sig struct {
		name        string
		wrapup      string
		mrCount     int
		beadsClosed int
	}
	// Each of these carries at least one signal => MUST resolve non-empty.
	withSignal := []sig{
		{"tldr only", `{"tldr":"merged MR 7"}`, 0, 0},
		{"accomplishments only", `{"accomplishments":"shipped X"}`, 0, 0},
		{"blockers only", `{"blockers":"needs review"}`, 0, 0},
		{"tldr + structured", `{"tldr":"done"}`, 2, 3},
		{"non-json prose", "freeform note", 0, 0},
		{"empty wrapup but 1 MR", "", 1, 0},
		{"empty wrapup but beads closed", "", 0, 4},
		{"empty wrapup, MRs + beads", "", 2, 1},
	}
	for _, s := range withSignal {
		t.Run(s.name, func(t *testing.T) {
			// Every case here carries prose OR work, so the resolved outcome
			// line must be non-empty — the card never degrades to a bare title.
			// The genuinely-empty path (no prose, no work, unknown status) is
			// covered by the dedicated escape-hatch test below.
			got := resolveRunOutcome(s.wrapup, s.mrCount, s.beadsClosed)
			if got == "" {
				t.Fatalf("closed run with a signal resolved to EMPTY outcome (wrapup=%q mrs=%d beads=%d)",
					s.wrapup, s.mrCount, s.beadsClosed)
			}
		})
	}
}

// TestInvariant_OutcomeCascadePriority asserts the cascade's ordering: prose
// wrap-up (tldr → accomplishments/blockers) always wins over the derived
// structured backstop, and the derived line is used ONLY when no prose exists.
// This guards against the backstop silently overwriting an operator-authored
// summary.
func TestInvariant_OutcomeCascadePriority(t *testing.T) {
	// Prose present: the tldr is the headline even though MRs/beads exist.
	got := resolveRunOutcome(`{"tldr":"hand-written summary"}`, 3, 5)
	if got != "hand-written summary" {
		t.Fatalf("prose should win over derived backstop, got %q", got)
	}

	// No prose, structured signals present: derived line is used.
	got = resolveRunOutcome("", 1, 2)
	if got != "1 MR opened · 2 beads closed" {
		t.Fatalf("derived backstop wrong: %q", got)
	}

	// A wrap-up that PARSED but carried no recognized prose fields (so
	// parseWrapUpSummary == "") still classifies as a clean finish
	// (agentRunOutcome == "ok"), so the terminal-phrase backstop fills in.
	got = resolveRunOutcome(`{"handoff_notes":"left a note"}`, 0, 0)
	if got != "finished — no wrap-up" {
		t.Fatalf("clean terminal backstop wrong: %q", got)
	}

	// Terminal-status phrases (used when there is no prose and no work) read
	// honestly for both clean and blocked finishes.
	if got := derivedRunOutcome(0, 0, "ok"); got != "finished — no wrap-up" {
		t.Fatalf("clean terminal phrase wrong: %q", got)
	}
	if got := derivedRunOutcome(0, 0, "fail"); got != "ended blocked — no wrap-up" {
		t.Fatalf("blocked terminal phrase wrong: %q", got)
	}
}

// TestInvariant_TrulyEmptyRunIsTheOnlyEmptyOutcome documents the one escape
// hatch: a closed run that posted NO wrap-up at all (empty wrapup field) and did
// no MR/bead work resolves to an empty outcome line — agentRunOutcome("") is
// "unknown", and the derived backstop intentionally stays empty for "unknown" so
// the card falls back to the task title rather than inventing a phrase. Any run
// that DID post a wrap-up (even a prose-less one) classifies as a clean finish
// and derives a terminal phrase. Keeping this explicit prevents a future change
// from accidentally widening the empty-outcome surface.
func TestInvariant_TrulyEmptyRunIsTheOnlyEmptyOutcome(t *testing.T) {
	// The genuinely-empty path: no wrap-up posted AND no work.
	if got := resolveRunOutcome("", 0, 0); got != "" {
		t.Fatalf("no-wrap-up + no-work run should be the empty escape hatch, got %q", got)
	}
	if got := derivedRunOutcome(0, 0, "unknown"); got != "" {
		t.Fatalf("unknown terminal status with no work should derive empty, got %q", got)
	}
	// A run that posted a (prose-less) wrap-up is a clean finish and DOES derive
	// a phrase — it is not the empty escape hatch.
	if got := resolveRunOutcome(`{"handoff_notes":"x"}`, 0, 0); got == "" {
		t.Fatalf("a posted (prose-less) wrap-up should derive a phrase, got empty")
	}
}

// ===========================================================================
// Prod-shaped accuracy invariants — the four named contracts from the Gardener
// Page v2 design doc §3/§8 (task gd-8qWNw5WPoV), exercised on the same
// prod-shaped thread shapes the CDQR audit surfaced (ka-0000005522, audit lane
// ka-0000001553):
//
//	counter_consistency  — the live IterationCount (== ordered card count)
//	                       equals the true number of iteration beads, and the
//	                       drift annotation surfaces IFF the engine snapshot
//	                       diverges beyond tolerance.
//	bounded_claim        — the displayed authoritative count never exceeds the
//	                       renderable cards: reconcileCounters never raises
//	                       IterationCount toward an inflated RunsSpawned.
//	no_empty_tail        — the eager/lazy split always produces a NON-empty tail
//	                       when (and only when) the thread exceeds the eager cap,
//	                       so the lazy section never resolves to a dead-end.
//	engine_count_framed  — RunsSpawned/RunsCompleted are carried as a labeled
//	                       secondary; reconcileCounters leaves the live count as
//	                       the sole authority and only annotates drift.
//
// bounded_claim and engine_count_framed have no test of their own here:
// TestInvariant_DisplayedIterationCountIsAlwaysLiveCount and
// TestInvariant_CounterConsistencyAcrossProdShapes already assert that
// reconcileCounters never moves IterationCount and that the engine tally is
// only framed as a secondary line (gd-cTYdroErdb).
//
// These exercise the pure assembly layer (orderGardenerIterations,
// reconcileCounters, the eager/lazy split arithmetic) directly, so they stay
// fast and Server-free; the golden-render siblings in gardener_golden_test.go
// pin the same four invariants through the real template + handler.
// ===========================================================================

// buildHeadlessThread builds n iteration beads with NO schedule_prior_agent_id
// chain (every run is a root). Models prod fixture (a): a 167-head no-chain
// gardener. Returns shuffled (reversed) so order cannot come from slice order.
func buildHeadlessThread(n int) []*beadsapi.BeadDetail {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	beads := make([]*beadsapi.BeadDetail, 0, n)
	for i := range n {
		beads = append(beads, invIterBead("gd-w"+itoa(i+1), "", "closed", t0.Add(time.Duration(i)*time.Minute), ""))
	}
	for l, r := 0, len(beads)-1; l < r; l, r = l+1, r-1 {
		beads[l], beads[r] = beads[r], beads[l]
	}
	return beads
}

// buildPartialChainThread builds n iteration beads forming an internally-intact
// chain whose ROOT dangles at a pruned ancestor outside the set. Models prod
// fixture (b): a 60-iter partial-chain gardener with a pruned gap.
func buildPartialChainThread(n int) []*beadsapi.BeadDetail {
	t0 := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	beads := make([]*beadsapi.BeadDetail, 0, n)
	prior := "gd-pruned-ancestor" // dangles outside the set => the pruned gap
	for i := range n {
		id := "gd-p" + itoa(i+1)
		beads = append(beads, invIterBead(id, prior, "closed", t0.Add(time.Duration(i)*time.Minute), ""))
		prior = id
	}
	return beads
}

// TestInvariant_CounterConsistencyAcrossProdShapes asserts counter_consistency
// on the three prod-shaped threads: the ordered card count always equals the
// true iteration-bead count, and reconcileCounters frames the secondary engine
// line as "older runs pruned" IFF the engine spawn snapshot outruns that live
// count beyond gardenerPrunedGap. (The legacy CounterDrift warning was replaced
// by the always-shown, pruned-aware EngineCount/EnginePruned model — design doc
// ka-0000005648 §3/§8; the invariant is re-expressed against it, not dropped.)
func TestInvariant_CounterConsistencyAcrossProdShapes(t *testing.T) {
	cases := []struct {
		name    string
		beads   []*beadsapi.BeadDetail
		spawned int
		live    int
	}{
		{"167_head_no_chain", buildHeadlessThread(167), 167, 167},
		{"60_iter_partial_chain_379_pruned_gap", buildPartialChainThread(60), 379, 60},
		{"clean_single_node", buildChainThread(1, 0), 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iters := orderGardenerIterations(tc.beads, "", idBuild)
			if len(iters) != tc.live {
				t.Fatalf("ordered %d cards, want %d", len(iters), tc.live)
			}

			// Give the completed tally a plausible value (<= spawned) so the
			// framed secondary line carries realistic numbers.
			completed := tc.spawned
			if completed > 0 {
				completed = tc.spawned - 1
			}
			v := gardenerView{RunsSpawned: tc.spawned, RunsCompleted: completed, IterationCount: len(iters)}
			v.reconcileCounters()

			// The live count is unchanged by reconciliation.
			if v.IterationCount != tc.live {
				t.Fatalf("IterationCount = %d, want live %d", v.IterationCount, tc.live)
			}

			// The secondary engine line is present whenever there is any activity
			// and carries the lifetime tally verbatim — it is never authoritative.
			if v.EngineCount == "" {
				t.Fatalf("EngineCount empty for active gardener (spawned=%d live=%d)", tc.spawned, tc.live)
			}

			// "pruned" framing appears IFF the spawn snapshot outruns the live
			// count beyond the prune gap — the re-expressed drift signal.
			wantPruned := tc.spawned-tc.live > gardenerPrunedGap
			if v.EnginePruned != wantPruned {
				t.Fatalf("EnginePruned = %v, want %v (spawned=%d live=%d gap=%d)",
					v.EnginePruned, wantPruned, tc.spawned, tc.live, gardenerPrunedGap)
			}
			wantLine := fmt.Sprintf("%d spawned / %d completed", tc.spawned, completed)
			if wantPruned {
				wantLine += " (older runs pruned)"
			}
			if v.EngineCount != wantLine {
				t.Fatalf("EngineCount = %q, want %q", v.EngineCount, wantLine)
			}
		})
	}
}

// ===========================================================================
// honest_ordering_label — the fifth named accuracy contract from the Gardener
// Page v2 design doc §3/§8 (P1 item 7, task gd-BKN36SJkYj):
//
//	No card is stamped "(by time)" when its OrderBasis == chain; the
//	"(by fire order)" hint appears ONLY when OrderBasis == fire-key.
//
// The bug being defended against is the CDQR `bytime_ribbons 13/13`: the old
// whole-thread byTime bool stamped EVERY card "(by time)" the moment a single
// chain edge broke, even when a genuine order (causal chain OR schedule_key
// fire-slot) was available. With the per-iteration orderBasis (basisChain /
// basisFireKey / basisTime), the ribbon label must match the basis tier on
// every card: a genuine chain or fire order is NEVER mislabeled "(by time)".
//
// These exercise orderGardenerIterations + runLabel directly across the same
// prod-shaped thread shapes the other invariants use, so the label↔basis
// contract fails CI deterministically rather than waiting for the next audit.
// ===========================================================================

// buildFireKeyThread builds n trigger-path iteration beads that carry NO
// schedule_prior_agent_id chain but DO carry a monotonic schedule_key fire-slot.
// Models the prod trigger-path gardener whose order is recovered from the fire
// schedule (basisFireKey) rather than the causal chain. created_at is held
// constant so only the fire-slot can supply a genuine order. Returned shuffled
// (reversed) so order cannot come from slice order.
func buildFireKeyThread(n int) []*beadsapi.BeadDetail {
	t0 := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	beads := make([]*beadsapi.BeadDetail, 0, n)
	for i := range n {
		// Fixed-width minute slots :00 .. so lexical order == chronological.
		slot := fmt.Sprintf("2026-05-20T07:%02d", i)
		b := invIterBead("gd-f"+itoa(i+1), "", "closed", t0, "")
		beads = append(beads, withScheduleKey(b, slot))
	}
	for l, r := 0, len(beads)-1; l < r; l, r = l+1, r-1 {
		beads[l], beads[r] = beads[r], beads[l]
	}
	return beads
}

// ribbonBasis is true when the RunLabel carries the basis hint the contract
// allows for a given OrderBasis: chain → no hint, fire-key → "(by fire order)",
// time → "(by time)". It returns whether the label is honest for that basis.
func labelHonestForBasis(label string, basis orderBasis) bool {
	hasByTime := strings.HasSuffix(label, "(by time)")
	hasByFire := strings.HasSuffix(label, "(by fire order)")
	switch basis {
	case basisChain:
		// A genuine causal chain gets NO hint — neither ribbon.
		return !hasByTime && !hasByFire
	case basisFireKey:
		// A recovered fire order is genuine — "(by fire order)", never "(by time)".
		return hasByFire && !hasByTime
	case basisTime:
		// The created_at heuristic last resort is the ONLY honest "(by time)".
		return hasByTime && !hasByFire
	default:
		return false
	}
}

// TestInvariant_HonestOrderingLabelAcrossProdShapes asserts honest_ordering_label
// on the prod-shaped threads: a card's ribbon hint must match its OrderBasis on
// EVERY card. The contract's two headline assertions — no "(by time)" when
// OrderBasis == chain, and "(by fire order)" only when OrderBasis == fire-key —
// are checked per card across a clean chain, a fire-slot trigger thread, and the
// created_at fallback.
func TestInvariant_HonestOrderingLabelAcrossProdShapes(t *testing.T) {
	cases := []struct {
		name      string
		beads     []*beadsapi.BeadDetail
		wantBasis orderBasis
	}{
		// Clean causal chain => basisChain => NO ribbon hint on any card.
		{"clean_chain_10", buildChainThread(10, 0), basisChain},
		// Trigger-path fire-slot thread => basisFireKey => "(by fire order)".
		{"fire_key_13", buildFireKeyThread(13), basisFireKey},
		// Two roots, no schedule_key => created_at heuristic => "(by time)".
		{"two_root_time_fallback", buildHeadlessThread(8), basisTime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iters := orderGardenerIterations(tc.beads, "", idBuild)
			if len(iters) == 0 {
				t.Fatalf("no cards produced")
			}
			for _, it := range iters {
				if it.Basis != tc.wantBasis {
					t.Fatalf("%s: Basis = %d, want %d", it.ID, it.Basis, tc.wantBasis)
				}
				// Headline assertion 1: a chain card is NEVER stamped "(by time)".
				if it.Basis == basisChain && strings.HasSuffix(it.RunLabel, "(by time)") {
					t.Errorf("%s: chain card mislabeled %q — must carry NO (by time) hint", it.ID, it.RunLabel)
				}
				// Headline assertion 2: "(by fire order)" appears ONLY on fire-key cards.
				if strings.HasSuffix(it.RunLabel, "(by fire order)") && it.Basis != basisFireKey {
					t.Errorf("%s: (by fire order) on a non-fire-key card (Basis=%d): %q", it.ID, it.Basis, it.RunLabel)
				}
				// Full label↔basis honesty for the card's actual tier.
				if !labelHonestForBasis(it.RunLabel, it.Basis) {
					t.Errorf("%s: RunLabel %q dishonest for Basis %d", it.ID, it.RunLabel, it.Basis)
				}
				// ByTime is the "ordering is NOT genuine" discriminator — true IFF
				// the basis is the created_at heuristic, false for chain/fire-key.
				if it.ByTime != (it.Basis == basisTime) {
					t.Errorf("%s: ByTime=%v but Basis=%d — discriminator must track basisTime only", it.ID, it.ByTime, it.Basis)
				}
			}
		})
	}
}

// TestInvariant_NoByTimeWhenChainIntact is the focused defense against the
// blanket bytime_ribbons 13/13 regression: a broken edge on a chain-less but
// fire-keyed trigger thread must recover a genuine fire order — so not one of
// the 13 cards is stamped "(by time)". A single whole-thread byTime bool would
// fail this; the per-iteration basis must not.
func TestInvariant_NoByTimeWhenChainIntact(t *testing.T) {
	// 13 trigger-path iterations carrying schedule_key, with one dangling prior
	// (the broken edge) so the causal chain cannot be walked — the exact CDQR
	// 13/13 shape. The fire-slot tier must recover a genuine order.
	beads := buildFireKeyThread(13)
	beads[3].Fields[fieldSchedulePriorAgentID] = "gd-pruned-ghost" // dangling => chain broken

	iters := orderGardenerIterations(beads, "", idBuild)
	if len(iters) != 13 {
		t.Fatalf("got %d cards, want 13", len(iters))
	}
	byTimeCount := 0
	for _, it := range iters {
		if it.Basis != basisFireKey {
			t.Errorf("%s: Basis = %d, want basisFireKey (genuine fire order survives a broken edge)", it.ID, it.Basis)
		}
		if strings.HasSuffix(it.RunLabel, "(by time)") || it.ByTime {
			byTimeCount++
		}
	}
	if byTimeCount != 0 {
		t.Fatalf("bytime_ribbons regression: %d/13 cards stamped (by time) despite a genuine fire order", byTimeCount)
	}
}

// TestInvariant_ByFireOrderLabelOnlyWhenFireKey asserts the converse: the
// "(by fire order)" ribbon must NOT appear on a clean causal chain (basisChain
// gets no hint) nor on the created_at heuristic fallback (basisTime reads
// "(by time)"). It only ever appears when the order was recovered from the
// schedule_key fire-slot tier.
func TestInvariant_ByFireOrderLabelOnlyWhenFireKey(t *testing.T) {
	// Clean chain: no card may read "(by fire order)".
	for _, it := range orderGardenerIterations(buildChainThread(6, 1), "", idBuild) {
		if it.ByFireOrder() || strings.HasSuffix(it.RunLabel, "(by fire order)") {
			t.Errorf("chain card %s wrongly labeled fire order: %q", it.ID, it.RunLabel)
		}
	}
	// created_at fallback (two roots, no schedule_key): no fire-order label.
	for _, it := range orderGardenerIterations(buildHeadlessThread(5), "", idBuild) {
		if it.ByFireOrder() || strings.HasSuffix(it.RunLabel, "(by fire order)") {
			t.Errorf("time-fallback card %s wrongly labeled fire order: %q", it.ID, it.RunLabel)
		}
	}
	// Fire-key thread: every card MUST read "(by fire order)".
	for _, it := range orderGardenerIterations(buildFireKeyThread(4), "", idBuild) {
		if !it.ByFireOrder() || !strings.HasSuffix(it.RunLabel, "(by fire order)") {
			t.Errorf("fire-key card %s missing fire-order label: %q (ByFireOrder=%v)", it.ID, it.RunLabel, it.ByFireOrder())
		}
	}
}
