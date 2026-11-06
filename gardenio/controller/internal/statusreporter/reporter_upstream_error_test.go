package statusreporter

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// Leg A of the Claude-outage detector (ka-0000003454, epic ka-0000000487).
//
// The gap these tests close: coop has always reported error_category on
// /api/v1/agent and the controller has always ACTED on server_error (gb init's
// dismiss + short backoff + re-nudge branch), but queryCoopAgentState's switch
// mapped only rate_limited and budget_exceeded — to an agent_state pill — and
// dropped every other category on the floor. Nothing counted anything, so the
// 2026-07-29 Claude outage ("Elevated errors across all models", status.claude.com
// incident q2kg8n613kr3) was invisible fleet-wide while the controller was
// recovering agents from it the entire time.

// TestSyncAll_ServerError_CountsWithoutStatePill is the core regression: a
// server_error must reach the gauge AND the transition counter, while
// deliberately NOT producing an agent_state override (server_error is handled
// in-pod, so it gets no bead pill — that asymmetry is exactly what let it go
// uncounted).
func TestSyncAll_ServerError_CountsWithoutStatePill(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"working","error_category":"server_error"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[[3]string]int{{"server_error", "proj", "dev"}: 1}
	got := rec.lastUpstream()
	if len(got) != len(want) {
		t.Fatalf("gauge snapshot = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("gauge[%v] = %d, want %d", k, got[k], v)
		}
	}

	if len(rec.upstreamTransitions) != 1 {
		t.Fatalf("expected 1 transition, got %d: %+v", len(rec.upstreamTransitions), rec.upstreamTransitions)
	}
	if tr := rec.upstreamTransitions[0]; tr.category != "server_error" || tr.project != "proj" || tr.role != "dev" {
		t.Errorf("transition = %+v, want {server_error proj dev}", tr)
	}

	// server_error must NOT become an agent_state pill — it falls through to
	// normal pod-phase reporting. If this ever flips to "server_error" the SSE
	// watcher would treat it as a failover-eligible state.
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update (pod-phase fallback), got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("state = %q, want working (server_error must not set a pill)", daemon.stateCalls[0].state)
	}
}

// TestSyncAll_RateLimited_CountsAndStillPills guards the no-regression half: the
// two categories that DO map to a pill must keep doing so, and must also now be
// counted. The counting happens before the overrideState branch's `goto
// metadata`, so a regression that moved it after would silently stop counting
// exactly these two.
func TestSyncAll_RateLimited_CountsAndStillPills(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"working","error_category":"rate_limited"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if n := rec.lastUpstream()[[3]string{"rate_limited", "proj", "dev"}]; n != 1 {
		t.Errorf("gauge[rate_limited] = %d, want 1 (counted despite the early goto)", n)
	}
	if len(rec.upstreamTransitions) != 1 {
		t.Errorf("expected 1 transition, got %d", len(rec.upstreamTransitions))
	}
	// Existing behavior preserved: the pill is still written.
	if len(daemon.stateCalls) != 1 || daemon.stateCalls[0].state != "rate_limited" {
		t.Errorf("stateCalls = %+v, want one rate_limited pill", daemon.stateCalls)
	}
}

// TestSyncAll_HealthyAgent_ClearsGauge asserts the property that makes the alert
// RESOLVE: a sweep with no erroring agent must flush an EMPTY snapshot, so the
// gauge Reset() clears the series rather than leaving it latched at its last
// value. Without this an outage alert would fire once and never recover.
func TestSyncAll_HealthyAgent_ClearsGauge(t *testing.T) {
	srv, bodyPtr, pod := stuckCoopServer(t)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	// Sweep 1: erroring.
	*bodyPtr = `{"state":"working","error_category":"server_error","since_seq":1,"screen_seq":1}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if n := rec.lastUpstream()[[3]string{"server_error", "proj", "dev"}]; n != 1 {
		t.Fatalf("sweep 1 gauge = %d, want 1", n)
	}

	// Sweep 2: recovered.
	*bodyPtr = `{"state":"working","since_seq":2,"screen_seq":2}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if got := rec.lastUpstream(); len(got) != 0 {
		t.Errorf("sweep 2 gauge = %v, want empty (alert must be able to resolve)", got)
	}
}

// TestSyncAll_PersistentError_CounterIsEdgeTriggered is the poll-cadence trap
// this design exists to avoid. The reporter OBSERVES error state on a fixed
// cadence, so an agent stuck on one error screen is seen erroring on every
// sweep. The gauge should stay at 1 across sweeps (it is level-triggered) while
// the counter increments exactly ONCE — otherwise the counter degenerates into
// "outage duration x poll rate" and stops being an independent cross-check of
// the gauge (advice ka-0000005924).
func TestSyncAll_PersistentError_CounterIsEdgeTriggered(t *testing.T) {
	srv, bodyPtr, pod := stuckCoopServer(t)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	*bodyPtr = `{"state":"working","error_category":"server_error","since_seq":1,"screen_seq":1}`
	for i := 0; i < 3; i++ {
		if err := r.SyncAll(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
	}

	if n := rec.lastUpstream()[[3]string{"server_error", "proj", "dev"}]; n != 1 {
		t.Errorf("gauge after 3 sweeps = %d, want 1 (level-triggered, not accumulating)", n)
	}
	if len(rec.upstreamTransitions) != 1 {
		t.Errorf("transitions after 3 sweeps = %d, want 1 (edge-triggered): %+v",
			len(rec.upstreamTransitions), rec.upstreamTransitions)
	}
}

// TestSyncAll_ErrorRecoversThenRecurs_CountsTwice is the other half of the edge
// contract: once an agent recovers, a later recurrence is a genuinely new
// failure and must count again. A dedup that never cleared would hide a
// flapping outage after its first occurrence.
func TestSyncAll_ErrorRecoversThenRecurs_CountsTwice(t *testing.T) {
	srv, bodyPtr, pod := stuckCoopServer(t)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	erroring := `{"state":"working","error_category":"server_error","since_seq":1,"screen_seq":1}`
	healthy := `{"state":"working","since_seq":2,"screen_seq":2}`
	for i, body := range []string{erroring, healthy, erroring} {
		*bodyPtr = body
		if err := r.SyncAll(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i+1, err)
		}
	}

	if len(rec.upstreamTransitions) != 2 {
		t.Errorf("transitions = %d, want 2 (recurrence after recovery must count): %+v",
			len(rec.upstreamTransitions), rec.upstreamTransitions)
	}
}

// TestSyncAll_CategoryChange_CountsNewEntry: server_error -> rate_limited is a
// distinct failure the oncall may need to see, so it counts as an entry into the
// new category rather than being suppressed as "still erroring".
func TestSyncAll_CategoryChange_CountsNewEntry(t *testing.T) {
	srv, bodyPtr, pod := stuckCoopServer(t)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	*bodyPtr = `{"state":"working","error_category":"server_error","since_seq":1,"screen_seq":1}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	*bodyPtr = `{"state":"working","error_category":"rate_limited","since_seq":2,"screen_seq":2}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}

	if len(rec.upstreamTransitions) != 2 {
		t.Fatalf("transitions = %d, want 2: %+v", len(rec.upstreamTransitions), rec.upstreamTransitions)
	}
	if c := rec.upstreamTransitions[1].category; c != "rate_limited" {
		t.Errorf("second transition category = %q, want rate_limited", c)
	}
	// The gauge is a snapshot, so only the current category is present.
	got := rec.lastUpstream()
	if got[[3]string{"rate_limited", "proj", "dev"}] != 1 {
		t.Errorf("gauge missing current category: %v", got)
	}
	if _, stale := got[[3]string{"server_error", "proj", "dev"}]; stale {
		t.Errorf("gauge retained stale category: %v", got)
	}
}

// TestSyncAll_UnknownCategory_CollapsesToOther is the cardinality guard: a
// coop-side category the controller does not know about must fold to `other`
// rather than minting its own series. error_detail is unbounded provider prose
// and must never reach a label; this asserts the same protection for a novel
// category string.
func TestSyncAll_UnknownCategory_CollapsesToOther(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"working","error_category":"some_new_coop_category"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := rec.lastUpstream()
	if got[[3]string{"other", "proj", "dev"}] != 1 {
		t.Errorf("gauge = %v, want the unknown category folded to `other`", got)
	}
	if _, leaked := got[[3]string{"some_new_coop_category", "proj", "dev"}]; leaked {
		t.Errorf("unknown category leaked into a label: %v", got)
	}
}

// TestSyncAll_UnreachableCoop_DoesNotCount: an unreachable or malformed coop
// response yields an empty coopAgentInfo, which must NOT be counted as an
// upstream provider error. Conflating "we could not ask" with "Claude is
// erroring" would make the outage alert fire on a controller-side network blip.
func TestSyncAll_UnreachableCoop_DoesNotCount(t *testing.T) {
	srv, pod := newCoopTestServer(t, `not-valid-json{{`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := rec.lastUpstream(); len(got) != 0 {
		t.Errorf("gauge = %v, want empty (a coop query failure is not a provider error)", got)
	}
	if len(rec.upstreamTransitions) != 0 {
		t.Errorf("transitions = %+v, want none", rec.upstreamTransitions)
	}
}

// TestGCUpstreamError_DropsVanishedBeads guards the memory leak: the transition
// dedup map must not retain entries for pods that no longer exist, mirroring
// gcStagnation / gcPhaseTracking. A lingering entry would also suppress a
// legitimate transition count if the bead ID were seen again later.
func TestGCUpstreamError_DropsVanishedBeads(t *testing.T) {
	r := NewHTTPReporter(&mockBeadUpdater{}, fake.NewSimpleClientset(), "ns", testLogger())
	r.SetEventRecorder(&fakeEventRecorder{})

	r.lastUpstreamError["gd-gone"] = "server_error"
	r.lastUpstreamError["gd-here"] = "rate_limited"

	r.gcUpstreamError(map[string]struct{}{"gd-here": {}})

	if _, ok := r.lastUpstreamError["gd-gone"]; ok {
		t.Error("gcUpstreamError kept an entry for a vanished bead")
	}
	if _, ok := r.lastUpstreamError["gd-here"]; !ok {
		t.Error("gcUpstreamError dropped an entry for a live bead")
	}
}

// TestTrackUpstreamError_NoRecorder_DoesNotPanic: events is nil until
// SetEventRecorder wires it, and SyncAll runs regardless. An unwired reporter
// must be a clean no-op, not a nil-map write panic.
func TestTrackUpstreamError_NoRecorder_DoesNotPanic(t *testing.T) {
	r := NewHTTPReporter(&mockBeadUpdater{}, fake.NewSimpleClientset(), "ns", testLogger())
	r.trackUpstreamError("gd-x", "server_error", "proj", "dev")
	r.gcUpstreamError(map[string]struct{}{})
	if len(r.lastUpstreamError) != 0 {
		t.Errorf("unwired reporter tracked state: %v", r.lastUpstreamError)
	}
}
