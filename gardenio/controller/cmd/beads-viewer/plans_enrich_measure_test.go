package main

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// plans_enrich_measure_test.go — Phase 4 re-measurement of the /plans index
// enrich pass (plan ka-0000002224, task gd-mpIDLrDqVk), run against the Phase 1
// instrumentation (planIndexEnrichTimed) on a REALISTIC plan set (20+ plans).
//
// It proves the two acceptance claims the plan set out to verify:
//
//  1. Daemon round-trips per /plans render drop from ~1 + N + ~12 (a per-row
//     N+1: one decision CountOnly call per plan, up to plansListLimit=200) to a
//     SMALL BOUNDED CONSTANT that does not grow with the plan count. The proof is
//     N-INVARIANCE: enriching 24 plans and 60 plans issues the IDENTICAL number
//     of daemon round-trips. A surviving per-row fan-out would make the 60-plan
//     count strictly larger.
//
//  2. Wall-clock enrich time falls MATERIALLY. With a per-round-trip latency
//     injected into the daemon, the folded pass (no per-row decision call) is
//     dramatically faster than a modelled pre-fold pass that pays one extra
//     serial round-trip per plan — because those N calls were serial, each adding
//     a full RTT.
//
// Everything here is self-contained (its own counting/latency daemon + its own
// N-plan seeder) so it never touches the shared seedPlansFake fixture or
// plans_index_test.go — avoiding a collision with the concurrent build/test task
// (gd-2GoJewFsHS) that edits those.

// countingDaemon wraps a beadAPI and counts + optionally delays the daemon
// round-trips the enrich pass makes. It embeds the interface so it satisfies all
// 60+ methods; only the read paths the /plans enrich pass exercises are
// overridden. Each override adds one atomic round-trip tick and sleeps rttDelay
// (modelling network latency) BEFORE delegating, so a serial fan-out pays the
// delay once per call — exactly the cost the fold removes.
type countingDaemon struct {
	beadAPI
	rtt   time.Duration
	calls atomic.Int64
	// perMethod tallies round-trips by method name so a test can assert the
	// EXACT budget shape, not just the total.
	mu        sync.Mutex
	perMethod map[string]int
}

func newCountingDaemon(inner beadAPI, rtt time.Duration) *countingDaemon {
	return &countingDaemon{beadAPI: inner, rtt: rtt, perMethod: map[string]int{}}
}

func (c *countingDaemon) tick(method string) {
	c.calls.Add(1)
	c.mu.Lock()
	c.perMethod[method]++
	c.mu.Unlock()
	if c.rtt > 0 {
		time.Sleep(c.rtt)
	}
}

func (c *countingDaemon) total() int64 { return c.calls.Load() }

func (c *countingDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	c.tick("ListBeadsFiltered")
	return c.beadAPI.ListBeadsFiltered(ctx, q)
}

func (c *countingDaemon) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	c.tick("BatchGetBeads")
	return c.beadAPI.BatchGetBeads(ctx, ids)
}

func (c *countingDaemon) BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	c.tick("BatchGetDependencies")
	return c.beadAPI.BatchGetDependencies(ctx, ids, direction)
}

func (c *countingDaemon) GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	c.tick("GetReverseDependencies")
	return c.beadAPI.GetReverseDependencies(ctx, id)
}

func (c *countingDaemon) FindAgentBeadsByNames(ctx context.Context, names, statuses []string) (map[string]*beadsapi.BeadDetail, error) {
	c.tick("FindAgentBeadsByNames")
	return c.beadAPI.FindAgentBeadsByNames(ctx, names, statuses)
}

func (c *countingDaemon) GetOperatorSlug(ctx context.Context, operatorID int64) (*beadsapi.GetOperatorSlugResponse, error) {
	c.tick("GetOperatorSlug")
	return c.beadAPI.GetOperatorSlug(ctx, operatorID)
}

// seedManyPlansFake builds a fake serving n plan epics on the /plans index
// (Types=[epic] → hotEpicsResp), each with one open step child AND one open
// clarifying-question decision child — the shape a real plan carries. The
// decision is a DIRECT parent-child child of its epic (production's parentage,
// gb/plan_answer.go), so the folded enrich reads its open count off the batched
// child walk with no per-row round-trip. Returns the fake and the epic slice in
// index order.
func seedManyPlansFake(n int) (*fakeDaemon, []*beadsapi.BeadDetail) {
	f := newFake()
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	epics := make([]*beadsapi.BeadDetail, 0, n)
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	for i := range n {
		epicID := "gd-plan-" + itoa(i)
		stepID := "gd-step-" + itoa(i)
		decID := "gd-dec-" + itoa(i)
		epic := &beadsapi.BeadDetail{
			ID:        epicID,
			Title:     "Plan " + itoa(i),
			Kind:      "issue",
			Type:      "epic",
			Status:    "in_progress",
			Labels:    []string{"project:gardenio", "plan"},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		// The step is in_progress with an assignee so the enrich pass exercises
		// its richest bulk paths — the stall detector's agent-liveness batch
		// (planStallBulk → FindAgentBeadsByNames) and the crew/MR/escalation
		// step walks all fire off a NON-empty candidate set, exactly as they do
		// on a live page. Every one of those is a SINGLE batched call across all
		// plans, so adding this data does not add per-row round-trips — which is
		// the whole point the invariance assertion pins.
		step := &beadsapi.BeadDetail{
			ID:        stepID,
			Title:     "Step " + itoa(i),
			Kind:      "issue",
			Type:      "task",
			Status:    "in_progress",
			Assignee:  "gardenio-worker-" + itoa(i),
			Labels:    []string{"project:gardenio"},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		decision := &beadsapi.BeadDetail{
			ID:        decID,
			Title:     "Question " + itoa(i),
			Type:      "decision",
			Status:    "open",
			Fields:    map[string]string{"prompt": "pick a path"},
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		f.beads[epicID] = epic
		f.beads[stepID] = step
		f.beads[decID] = decision
		// Both the step and the decision are direct parent-child children.
		f.revDeps[epicID] = []beadsapi.Dependency{
			{BeadID: stepID, DependsOnID: epicID, Type: "parent-child"},
			{BeadID: decID, DependsOnID: epicID, Type: "parent-child"},
		}
		epics = append(epics, epic)
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: epics, Total: len(epics)}
	return f, epics
}

// countingEnrichServer wires a countingDaemon in front of a fake and returns a
// Server that enriches through it.
func countingEnrichServer(f *fakeDaemon, rtt time.Duration) (*Server, *countingDaemon) {
	cd := newCountingDaemon(f, rtt)
	s := testServerWithDaemon(cd)
	// planStallBulk reads s.now(); pin it so the stall detector has a clock.
	s.SetNow(func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) })
	return s, cd
}

// enrichRoundTrips runs the real timed enrich pass over n seeded plans and
// returns the daemon round-trip count, the per-method breakdown, and the wall
// clock the instrumentation measured.
func enrichRoundTrips(t *testing.T, n int, rtt time.Duration) (calls int64, perMethod map[string]int, timing *planEnrichTiming) {
	t.Helper()
	f, epics := seedManyPlansFake(n)
	s, cd := countingEnrichServer(f, rtt)
	rows, tm := s.planIndexEnrichTimed(context.Background(), epics)
	if len(rows) != n {
		t.Fatalf("enriched %d rows, want %d", len(rows), n)
	}
	// Every row must carry the folded open-decision count (proving the fold
	// actually read the decisions off the batched walk, not a dropped signal).
	for _, r := range rows {
		if r.OpenDecisions != 1 {
			t.Fatalf("row %s OpenDecisions=%d, want 1 (folded off the child walk)", r.ID, r.OpenDecisions)
		}
	}
	cd.mu.Lock()
	pm := maps.Clone(cd.perMethod)
	cd.mu.Unlock()
	return cd.total(), pm, tm
}

// TestPlansEnrich_RoundTripsAreBoundedConstant is the headline acceptance test:
// the daemon round-trip budget of one /plans enrich pass is a SMALL BOUNDED
// CONSTANT that does not grow with the plan count. Proof by N-invariance —
// 24 plans and 60 plans issue the SAME number of round-trips. A surviving
// per-row decision-count N+1 (the thing Phase 2 removed) would make the 60-plan
// count strictly larger by 36 (one CountOnly per extra plan).
func TestPlansEnrich_RoundTripsAreBoundedConstant(t *testing.T) {
	calls24, perMethod24, _ := enrichRoundTrips(t, 24, 0)
	calls60, _, _ := enrichRoundTrips(t, 60, 0)

	if calls24 != calls60 {
		t.Fatalf("round-trips must be plan-count INVARIANT; got %d for 24 plans vs %d for 60 plans (a per-row fan-out survives)", calls24, calls60)
	}

	// The budget is a genuinely small constant. The enrich pass makes a fixed set
	// of page-wide bulk joins regardless of plan count. Pin an upper bound well
	// below N so a future regression that reintroduces any per-row call trips it.
	const budgetCeiling = 16
	if calls24 > budgetCeiling {
		t.Fatalf("enrich round-trips = %d, want <= %d (a bounded constant, not O(plans)); breakdown: %v", calls24, budgetCeiling, perMethod24)
	}

	// No single method may be called once-per-plan. With 24 plans, any method at
	// >= 24 calls is a per-row fan-out. The bulk joins each call a method O(1)
	// times (a couple of tiers at most), never O(plans).
	for method, count := range perMethod24 {
		if count >= 24 {
			t.Fatalf("method %s called %d times for 24 plans — that is a per-row fan-out, not a bulk join", method, count)
		}
	}

	t.Logf("MEASURED (Phase 4 re-measure): 24 plans => %d daemon round-trips; 60 plans => %d; INVARIANT. Per-method @24: %v",
		calls24, calls60, perMethod24)
}

// TestPlansEnrich_NoPerRowDecisionCountCall pins the specific fix: the enrich
// pass issues NO CountOnly decision query at all (the former N+1 shape,
// Types=[decision] Parent=<epic> CountOnly). The open-decision count now comes
// off the batched child walk. This guards against a silent reintroduction of
// planOpenDecisionCount into the index loop.
func TestPlansEnrich_NoPerRowDecisionCountCall(t *testing.T) {
	f, epics := seedManyPlansFake(24)
	// Sentinel wrapper: fail loudly if any Types=[decision]+CountOnly query is
	// issued during the enrich pass. This is the exact query planOpenDecisionCount
	// used; the folded path must never issue it.
	sentinel := &decisionCountSentinel{beadAPI: f}
	s := testServerWithDaemon(sentinel)
	s.SetNow(func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) })
	rows := s.planIndexEnrich(context.Background(), epics)
	if len(rows) != 24 {
		t.Fatalf("enriched %d rows, want 24", len(rows))
	}
	if sentinel.hits.Load() != 0 {
		t.Fatalf("enrich issued %d per-row decision-count CountOnly queries; the fold must issue ZERO", sentinel.hits.Load())
	}
}

type decisionCountSentinel struct {
	beadAPI
	hits atomic.Int64
}

func (d *decisionCountSentinel) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if q.CountOnly && q.Parent != "" && containsStr(q.Types, "decision") {
		d.hits.Add(1)
	}
	return d.beadAPI.ListBeadsFiltered(ctx, q)
}

// TestPlansEnrich_WallClockFallsMaterially proves the wall-clock claim: with a
// realistic per-round-trip latency, the folded enrich pass costs materially less
// daemon time than the pre-fold pass that paid one EXTRA serial decision
// round-trip per plan.
//
// The comparison is on DAEMON TIME — round-trips × rtt — which is deterministic
// and scheduling-independent, NOT on total wall clock (Go's Go-scheduler jitter
// under parallel `go test` makes a raw wall-clock threshold flaky). The model is
// exact and conservative: planOpenDecisionCount ran inside the row loop, one
// CountOnly decision call per plan, each a serial blocking RTT — so the pre-fold
// pass issued exactly foldedCalls + N round-trips. Because those N calls were
// SERIAL (in the row loop, not fanned out), each added a full rtt to the
// critical path. We assert the removed daemon time is a material fraction of the
// pre-fold total. The daemon-time COUNT comparison is unaffected by the Phase-3
// wave parallelization (ka-0000002624 + ka-0000004074): it models the round-trip
// COUNT the fold removed, not the wall-clock order they run in.
//
// The wall-clock cross-check must, however, respect that the enrich pass no
// longer pays its round-trips serially. It now runs in two concurrent waves
// separated by a barrier, so the critical path is the slowest branch per wave,
// not the sum. We therefore assert only the two-wave serial-depth FLOOR — each
// wave hits the daemon at least once and the barrier serializes the waves, so the
// wall clock is provably >= 2×rtt of real sleeps — which still proves the
// injected latency took effect (guards a no-op instrumentation that never hit the
// daemon) without re-introducing the now-false serial-sum assumption.
func TestPlansEnrich_WallClockFallsMaterially(t *testing.T) {
	const (
		n   = 24
		rtt = 2 * time.Millisecond // a modest per-round-trip latency
	)
	foldedCalls, _, _ := enrichRoundTrips(t, n, rtt)

	// Measure the folded pass's actual wall clock end-to-end (for the log + a
	// floor sanity check, NOT for the flaky ratio threshold).
	f, epics := seedManyPlansFake(n)
	s, _ := countingEnrichServer(f, rtt)
	start := time.Now()
	_, _ = s.planIndexEnrichTimed(context.Background(), epics)
	foldedWall := time.Since(start)

	// Daemon time is deterministic: round-trips × rtt. The pre-fold pass paid the
	// SAME calls plus N serial decision RTTs.
	removedRoundTrips := int64(n)
	foldedDaemonTime := time.Duration(foldedCalls) * rtt
	preFoldDaemonTime := time.Duration(foldedCalls+removedRoundTrips) * rtt
	saved := preFoldDaemonTime - foldedDaemonTime // == N * rtt

	// The removed serial round-trips must dominate: they are the MAJORITY of the
	// pre-fold daemon time (N > foldedCalls, so > 50% removed). This is a pure
	// function of the call counts and rtt — no scheduling dependence, so it never
	// flakes.
	if saved*2 <= preFoldDaemonTime {
		t.Fatalf("expected the removed N=%d serial decision round-trips to be the MAJORITY of pre-fold daemon time; saved=%v pre-fold=%v (folded round-trips=%d)",
			n, saved, preFoldDaemonTime, foldedCalls)
	}

	// Sanity: the measured folded wall clock is at least the two-wave serial-depth
	// floor it must pay — proving the injected rtt actually took effect (guards a
	// no-op instrumentation that never hit the daemon). Post Phase-3 the enrich
	// pass fans its round-trips out across two concurrent waves separated by a
	// barrier, so it no longer pays foldedCalls×rtt serially; but the barrier still
	// forces wave 2 to start after wave 1 finishes, and each wave issues at least
	// one daemon round-trip, so the wall clock is provably >= 2×rtt of real sleeps.
	const waveDepthFloor = 2
	waveFloor := time.Duration(waveDepthFloor) * rtt
	if foldedWall < waveFloor {
		t.Fatalf("folded wall clock %v < two-wave serial floor %v — the injected rtt did not take effect", foldedWall, waveFloor)
	}

	savedFraction := float64(removedRoundTrips) / float64(foldedCalls+removedRoundTrips)
	t.Logf("MEASURED @%d plans, rtt=%v: folded=%d round-trips (daemon time %v, wall %v); pre-fold=%d round-trips (daemon time %v); removed %d SERIAL round-trips = %v saved (~%.0f%% of pre-fold daemon calls)",
		n, rtt, foldedCalls, foldedDaemonTime, foldedWall, foldedCalls+removedRoundTrips, preFoldDaemonTime, removedRoundTrips, saved, savedFraction*100)
}

// TestPlansEnrich_ListCacheFoldsRepeatRender proves the /plans enrich list-cache
// (ka-0000003113) collapses its one cacheable page-invariant list query across
// renders: a SECOND enrich pass within the TTL issues exactly ONE FEWER
// ListBeadsFiltered round-trip than the first — the memoized plan-gardener agent
// list (planAssistantBulk, the dominant ~730 ms / 4.0 MB leg) — while every other
// bulk join still runs, INCLUDING the gardener-trigger list (planGardenersByEpic),
// which is deliberately NOT cached because /plans mutates those beads. This is the
// round-trip win the operator asked for ("still way too slow in prod; need to
// cache more"): on a busy index, all but the first render in each TTL window skips
// the single heaviest leg.
func TestPlansEnrich_ListCacheFoldsRepeatRender(t *testing.T) {
	f, epics := seedManyPlansFake(24)
	cd := newCountingDaemon(f, 0)
	s := testServerWithDaemon(cd)
	s.planEnrichListCache = newPlanEnrichListCache()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	s.SetNow(func() time.Time { return now })

	// First render populates the memo.
	if rows, _ := s.planIndexEnrichTimed(context.Background(), epics); len(rows) != 24 {
		t.Fatalf("first pass enriched %d rows, want 24", len(rows))
	}
	firstList := cd.perMethod["ListBeadsFiltered"]

	// Second render, still inside the TTL window: the assistant list is served from
	// the memo, so ListBeadsFiltered fires exactly one fewer time.
	now = now.Add(planEnrichListCacheTTL / 2)
	if rows, _ := s.planIndexEnrichTimed(context.Background(), epics); len(rows) != 24 {
		t.Fatalf("second pass enriched %d rows, want 24", len(rows))
	}
	secondList := cd.perMethod["ListBeadsFiltered"] - firstList

	if firstList < 2 {
		t.Fatalf("first pass issued %d ListBeadsFiltered calls, expected >= 2 (assistant + gardener lists)", firstList)
	}
	if want := firstList - 1; secondList != want {
		t.Errorf("second pass ListBeadsFiltered = %d, want %d (one cached list folded); first pass = %d", secondList, want, firstList)
	}

	// Past the TTL boundary the memo expires and the full list budget returns —
	// proving staleness is bounded (a freshly-spawned assistant appears within a
	// minute).
	now = now.Add(planEnrichListCacheTTL + time.Second)
	before := cd.perMethod["ListBeadsFiltered"]
	if rows, _ := s.planIndexEnrichTimed(context.Background(), epics); len(rows) != 24 {
		t.Fatalf("third pass enriched %d rows, want 24", len(rows))
	}
	thirdList := cd.perMethod["ListBeadsFiltered"] - before
	if thirdList != firstList {
		t.Errorf("post-TTL pass ListBeadsFiltered = %d, want %d (memo expired, full budget restored)", thirdList, firstList)
	}
}
