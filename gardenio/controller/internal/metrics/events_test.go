package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEventMetrics_RegisterAndRecordLifecycle(t *testing.T) {
	em := NewEventMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := em.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Valid event with no session id (non-OOM).
	if !em.RecordLifecycleEvent(LifecycleSpawned, "acme", "crew", "") {
		t.Fatal("RecordLifecycleEvent(spawned) returned false")
	}
	// OOM with session id — the FSM drill-down case.
	if !em.RecordLifecycleEvent(LifecycleOOMKilled, "acme", "crew", "sess-123") {
		t.Fatal("RecordLifecycleEvent(oom_killed) returned false")
	}

	// Out-of-enum event must be dropped.
	if em.RecordLifecycleEvent("bogus", "acme", "crew", "") {
		t.Fatal("RecordLifecycleEvent(bogus) should return false")
	}

	if got := testutil.CollectAndCount(em.lifecycleEvents); got != 2 {
		t.Fatalf("expected 2 lifecycle series, got %d", got)
	}

	// The OOM series must carry env + session_id exactly as the FSM contract binds.
	want := `
# HELP gardenio_agent_lifecycle_events_total Agent lifecycle transitions by kind (enum: spawned/started/stopped/oom_killed/stuck/drained/despawned). Labels {event,env,project,role,session_id}; session_id is populated only for oom_killed (FSM composite-alert drill-down, FSM_CORRELATION_CONTRACT §5a). env is the cross-estate join key.
# TYPE gardenio_agent_lifecycle_events_total counter
gardenio_agent_lifecycle_events_total{env="prod",event="oom_killed",project="acme",role="crew",session_id="sess-123"} 1
gardenio_agent_lifecycle_events_total{env="prod",event="spawned",project="acme",role="crew",session_id=""} 1
`
	if err := testutil.CollectAndCompare(em.lifecycleEvents, strings.NewReader(want)); err != nil {
		t.Fatalf("lifecycle series mismatch: %v", err)
	}
}

func TestEventMetrics_PodPhaseTransitions(t *testing.T) {
	em := NewEventMetrics("test")

	if !em.RecordPodPhaseTransition("Pending", "Running", "acme", "crew") {
		t.Fatal("valid transition returned false")
	}
	// No-op transition (from==to) must be dropped.
	if em.RecordPodPhaseTransition("Running", "Running", "acme", "crew") {
		t.Fatal("self-transition should return false")
	}
	// Empty endpoints dropped.
	if em.RecordPodPhaseTransition("", "Running", "acme", "crew") {
		t.Fatal("empty from should return false")
	}

	if got := testutil.ToFloat64(em.podPhaseTransitions.WithLabelValues("Pending", "Running", "acme", "crew")); got != 1 {
		t.Fatalf("expected 1 Pending->Running, got %v", got)
	}
}

// TestEventMetrics_PodsPendingGauge exercises the current-state gauge at the
// method layer, independent of the status reporter: it asserts the exact
// exposed series (metric name, label set, values) the GAP-1 alert binds to, and
// that a follow-up SetPodsPending snapshot fully REPLACES the prior one so a
// (project,role) that dropped to zero disappears (the alert-clears path). This
// is the second, independent derivation of the pending count; the
// statusreporter TestSyncAll_EmitsPodsPendingGauge derives the same quantity
// end-to-end from fake pods — the two are cross-checked to agree.
func TestEventMetrics_PodsPendingGauge(t *testing.T) {
	em := NewEventMetrics("prod")

	em.SetPodsPending(map[[2]string]int{
		{"acme", "crew"}:     2,
		{"acme", "gardener"}: 1,
	})

	// The alert expr is sum(gardenio_agent_pods_pending) — assert the total and
	// the two grouped series match exactly what the rule reads.
	if got := testutil.CollectAndCount(em.podsPending); got != 2 {
		t.Fatalf("expected 2 pending series, got %d", got)
	}
	want := `
# HELP gardenio_agent_pods_pending Live count of agent pods currently in K8s phase=Pending, by {project,role}, re-derived on every controller pod-phase sweep. Current-state gauge backing the GAP-1 AgentPodsPendingTooLong alert (sum(gardenio_agent_pods_pending) > 0 for 10m); the incident-direct signal for agent-spawn stalls that gardenio_agent_pod_phase_transitions_total (a counter) cannot express.
# TYPE gardenio_agent_pods_pending gauge
gardenio_agent_pods_pending{project="acme",role="gardener"} 1
gardenio_agent_pods_pending{project="acme",role="crew"} 2
`
	if err := testutil.CollectAndCompare(em.podsPending, strings.NewReader(want)); err != nil {
		t.Fatalf("pending gauge series mismatch: %v", err)
	}

	// A second snapshot with only one group must fully replace the first: the
	// crew count updates and the gardener series DISAPPEARS (not linger stale).
	em.SetPodsPending(map[[2]string]int{{"acme", "crew"}: 5})
	if got := testutil.CollectAndCount(em.podsPending); got != 1 {
		t.Fatalf("snapshot replace: expected 1 series, got %d", got)
	}
	if got := testutil.ToFloat64(em.podsPending.WithLabelValues("acme", "crew")); got != 5 {
		t.Fatalf("snapshot replace: expected crew=5, got %v", got)
	}

	// An empty snapshot clears the gauge entirely — the healthy steady state
	// where sum() returns no data and the alert resets.
	em.SetPodsPending(map[[2]string]int{})
	if got := testutil.CollectAndCount(em.podsPending); got != 0 {
		t.Fatalf("empty snapshot must clear the gauge, got %d series", got)
	}
}

func TestEventMetrics_PodsPendingNilSafe(t *testing.T) {
	var em *EventMetrics
	// Must not panic on a nil receiver (unwired reporter).
	em.SetPodsPending(map[[2]string]int{{"p", "r"}: 1})
}

func TestEventMetrics_SessionDuration(t *testing.T) {
	em := NewEventMetrics("")

	if !em.RecordSessionDuration(42, "acme", "crew", OutcomeDone) {
		t.Fatal("valid duration returned false")
	}
	// Negative duration dropped.
	if em.RecordSessionDuration(-1, "acme", "crew", OutcomeDone) {
		t.Fatal("negative duration should return false")
	}
	// Out-of-enum outcome dropped.
	if em.RecordSessionDuration(10, "acme", "crew", "bogus") {
		t.Fatal("bogus outcome should return false")
	}

	if got := testutil.CollectAndCount(em.sessionDuration); got != 1 {
		t.Fatalf("expected 1 session-duration series, got %d", got)
	}
}

func TestEventMetrics_LandSpawnToWorking(t *testing.T) {
	em := NewEventMetrics("test")
	if !em.ObserveLandSpawnToWorking("acme", 42) {
		t.Fatal("valid observation returned false")
	}
	if !em.ObserveLandSpawnToWorking("acme", 400) {
		t.Fatal("valid observation returned false")
	}
	if em.ObserveLandSpawnToWorking("", 10) {
		t.Fatal("empty land should return false")
	}
	if em.ObserveLandSpawnToWorking("acme", -1) {
		t.Fatal("negative duration should return false")
	}

	if got := testutil.CollectAndCount(em.landSpawnToWorking); got != 1 {
		t.Fatalf("expected 1 land series, got %d", got)
	}
	// The SLO reads le="300": one of the two samples is under it.
	want := `
# HELP gardenio_lands_spawn_to_working_seconds Seconds from a tenant-land agent bead's creation to the first time the controller reports agent_state=working for it, observed once per agent. Labels {land}. The Lands tenant_spawn_to_working SLI (threshold 300s).
# TYPE gardenio_lands_spawn_to_working_seconds histogram
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="10"} 0
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="30"} 0
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="60"} 1
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="120"} 1
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="300"} 1
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="600"} 2
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="1200"} 2
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="3600"} 2
gardenio_lands_spawn_to_working_seconds_bucket{land="acme",le="+Inf"} 2
gardenio_lands_spawn_to_working_seconds_sum{land="acme"} 442
gardenio_lands_spawn_to_working_seconds_count{land="acme"} 2
`
	if err := testutil.CollectAndCompare(em.landSpawnToWorking, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestEventMetrics_NilSafe(t *testing.T) {
	var em *EventMetrics
	if em.RecordLifecycleEvent(LifecycleSpawned, "p", "r", "") {
		t.Fatal("nil RecordLifecycleEvent should return false")
	}
	if em.RecordPodPhaseTransition("a", "b", "p", "r") {
		t.Fatal("nil RecordPodPhaseTransition should return false")
	}
	if em.RecordSessionDuration(1, "p", "r", OutcomeDone) {
		t.Fatal("nil RecordSessionDuration should return false")
	}
	if em.RecordInvoluntaryDeath("stop_gate_cooldown_involuntary: x", "p", "r") {
		t.Fatal("nil RecordInvoluntaryDeath should return false")
	}
	if em.ObserveLandSpawnToWorking("acme", 1) {
		t.Fatal("nil ObserveLandSpawnToWorking should return false")
	}
}

// TestInvoluntaryDeathFamily pins the close_reason → family classifier against
// the EXACT close_reason strings hook.go / init_agent_loop.go stamp in prod
// (verbatim from live agent beads, e.g. ka-0000004720's gd-TDM012XFoX), plus the
// reasons that must NOT count (clean idle-out, graceful stop, genuine crash).
func TestInvoluntaryDeathFamily(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		wantFamily string
		wantOK     bool
	}{
		{
			name:       "stop_gate_cooldown involuntary (canonical ka-0000004720 shape)",
			reason:     "stop_gate_cooldown_involuntary: claude exited mid-work without gb done; reopened ka-0000004720",
			wantFamily: FamilyStopGateCooldown,
			wantOK:     true,
		},
		{
			name:       "context_cancelled involuntary (SIGTERM mid-work)",
			reason:     "context_cancelled_involuntary: pod terminated mid-work before gb done",
			wantFamily: FamilyContextCancelled,
			wantOK:     true,
		},
		{
			name:       "stop_gate_escape_hatch involuntary",
			reason:     "stop_gate_escape_hatch_involuntary: claude exited mid-work without gb done",
			wantFamily: FamilyStopGateEscapeHatch,
			wantOK:     true,
		},
		{
			name:       "stop_gate_rate_limited involuntary",
			reason:     "stop_gate_rate_limited_involuntary: claude exited mid-work without gb done",
			wantFamily: FamilyStopGateRateLimited,
			wantOK:     true,
		},
		{
			name:       "novel involuntary shape falls to other, still counted",
			reason:     "some_new_path_involuntary: died mid-work",
			wantFamily: FamilyOther,
			wantOK:     true,
		},
		{
			name:       "unclaimed mid-turn kill (C9 ka-0000001146) counted without the marker",
			reason:     "context_cancelled_unclaimed: pod terminated with tool calls but no claimed issue",
			wantFamily: FamilyContextCancelledUnclaimed,
			wantOK:     true,
		},
		{
			name:       "claim-lookup-failed mid-turn kill (gd-TYjMaxCMZR) counted without the marker",
			reason:     "context_cancelled_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified",
			wantFamily: FamilyContextCancelledClaimLookupFailed,
			wantOK:     true,
		},
		{
			name:       "outage-driven death (ka-0000000202) counts as upstream_api_error",
			reason:     "upstream_api_error_involuntary: pod terminated during a sustained Claude upstream outage (server_error)",
			wantFamily: FamilyUpstreamAPIError,
			wantOK:     true,
		},
		{
			name:       "outage-driven max-restarts exhaustion also upstream_api_error",
			reason:     "upstream_api_error_involuntary: restart loop exhausted during a sustained Claude upstream outage (no_internet)",
			wantFamily: FamilyUpstreamAPIError,
			wantOK:     true,
		},
		{
			name:       "rate-limit livelock give-up (ka-0000001209) counts as rate_limit_livelock",
			reason:     "rate_limit_livelock_involuntary: gave up after 8 consecutive rate_limited parks (account monthly-spend cap); reopened gd-TASK1",
			wantFamily: FamilyRateLimitLivelock,
			wantOK:     true,
		},
		{
			name:   "clean stop-gate idle-out (no _involuntary) not counted",
			reason: "stop_gate_cooldown: claude exited without gb done",
			wantOK: false,
		},
		{
			name:   "graceful zero-work context_cancelled teardown not counted",
			reason: "context_cancelled",
			wantOK: false,
		},
		{
			name:   "empty close_reason not counted",
			reason: "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			family, ok := InvoluntaryDeathFamily(tc.reason)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v (reason=%q)", ok, tc.wantOK, tc.reason)
			}
			if ok && family != tc.wantFamily {
				t.Fatalf("family=%q, want %q (reason=%q)", family, tc.wantFamily, tc.reason)
			}
		})
	}
}

// TestEventMetrics_InvoluntaryDeaths verifies the counter increments once per
// involuntary close, carries the {family,env,project,role} label set, and does
// NOT increment for a non-involuntary close.
func TestEventMetrics_InvoluntaryDeaths(t *testing.T) {
	em := NewEventMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := em.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if !em.RecordInvoluntaryDeath("stop_gate_cooldown_involuntary: reopened ka-0000004720", "gardenio", "crew") {
		t.Fatal("involuntary cooldown death returned false")
	}
	if !em.RecordInvoluntaryDeath("context_cancelled_involuntary: pod terminated", "gardenio", "thread") {
		t.Fatal("involuntary context_cancelled death returned false")
	}
	if !em.RecordInvoluntaryDeath("context_cancelled_unclaimed: pod terminated with tool calls but no claimed issue", "gardenio", "gardener") {
		t.Fatal("unclaimed context_cancelled death returned false")
	}
	if !em.RecordInvoluntaryDeath("context_cancelled_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified", "gardenio", "crew") {
		t.Fatal("claim-lookup-failed context_cancelled death returned false")
	}
	// An outage-driven death (ka-0000000202) must count under family=upstream_api_error.
	if !em.RecordInvoluntaryDeath("upstream_api_error_involuntary: pod terminated during a sustained Claude upstream outage (server_error)", "gardenio", "crew") {
		t.Fatal("upstream_api_error death returned false")
	}
	// A clean idle-out must NOT increment the counter.
	if em.RecordInvoluntaryDeath("stop_gate_cooldown: claude exited without gb done", "gardenio", "crew") {
		t.Fatal("clean idle-out should return false and not increment")
	}

	if got := testutil.CollectAndCount(em.involuntaryDeaths); got != 5 {
		t.Fatalf("expected 5 involuntary-death series, got %d", got)
	}

	want := `
# HELP gardenio_agent_involuntary_deaths_total Agent sessions that closed with an involuntary mid-work death (close_reason carries the ` + "`_involuntary`" + ` marker stamped when Claude exited or the pod was SIGTERM'd while a claimed task was in flight, or the context_cancelled_unclaimed prefix for a SIGTERM'd busy-but-taskless agent, or the context_cancelled_claim_lookup_failed prefix for a SIGTERM'd agent whose claimed-task lookup failed). Labels {family,env,project,role}; family enum: stop_gate_cooldown/stop_gate_escape_hatch/stop_gate_rate_limited/context_cancelled/context_cancelled_unclaimed/context_cancelled_claim_lookup_failed/upstream_api_error/rate_limit_livelock/other. This is the 'killed mid-work' signal (ka-0000008189) — its rate()/increase() proves the regression is fixed; slice family="upstream_api_error" to see outage-driven deaths distinctly (ka-0000000487), or family="rate_limit_livelock" to see agents that gave up on an account monthly-spend cap (ka-0000001209). Distinct from agent_session_duration_seconds outcome, which is event-type-derived and mislabels a stop-gate involuntary close as done.
# TYPE gardenio_agent_involuntary_deaths_total counter
gardenio_agent_involuntary_deaths_total{env="prod",family="context_cancelled",project="gardenio",role="thread"} 1
gardenio_agent_involuntary_deaths_total{env="prod",family="context_cancelled_claim_lookup_failed",project="gardenio",role="crew"} 1
gardenio_agent_involuntary_deaths_total{env="prod",family="context_cancelled_unclaimed",project="gardenio",role="gardener"} 1
gardenio_agent_involuntary_deaths_total{env="prod",family="stop_gate_cooldown",project="gardenio",role="crew"} 1
gardenio_agent_involuntary_deaths_total{env="prod",family="upstream_api_error",project="gardenio",role="crew"} 1
`
	if err := testutil.CollectAndCompare(em.involuntaryDeaths, strings.NewReader(want)); err != nil {
		t.Fatalf("involuntary-death series mismatch: %v", err)
	}
}

// TestNormalizeSessionExitCode verifies the bounded code enum: the known coop
// exit codes map to their fixed label, and anything outside that set falls to
// "other" rather than inflating cardinality.
func TestNormalizeSessionExitCode(t *testing.T) {
	cases := []struct {
		exitCode int
		want     string
	}{
		{0, ExitCodeLabelClean},
		{10, ExitCodeLabelCredentialSwitch},
		{129, ExitCodeLabelSIGHUP},
		{130, ExitCodeLabelSIGINT},
		{137, ExitCodeLabelSIGKILL},
		{143, ExitCodeLabelSIGTERM},
		{1, ExitCodeLabelOther},
		{255, ExitCodeLabelOther},
		{-1, ExitCodeLabelOther},
	}
	for _, tc := range cases {
		if got := normalizeSessionExitCode(tc.exitCode); got != tc.want {
			t.Errorf("normalizeSessionExitCode(%d) = %q, want %q", tc.exitCode, got, tc.want)
		}
	}
}

// TestEventMetrics_RecordSessionExitCode verifies the counter increments once
// per session close with the {code,project,role} label set, normalizing a
// rare exit code to "other".
func TestEventMetrics_RecordSessionExitCode(t *testing.T) {
	em := NewEventMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := em.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if !em.RecordSessionExitCode(0, "gardenio", "crew") {
		t.Fatal("clean exit returned false")
	}
	if !em.RecordSessionExitCode(143, "gardenio", "crew") {
		t.Fatal("SIGTERM exit returned false")
	}
	if !em.RecordSessionExitCode(99, "gardenio", "gardener") {
		t.Fatal("unmapped exit returned false")
	}

	want := `
# HELP gardenio_session_exit_code_total Coop process exit codes observed at session-bead close, one increment per coop restart-loop iteration. Labels {code,project,role}; code enum: 0 (clean), 10 (credential switch), 129 (SIGHUP), 130 (SIGINT), 137 (SIGKILL, the kernel OOM-killer's signal), 143 (SIGTERM), other. Makes the exit-code split (previously a manual bead-list scan) queryable over time via rate()/increase() (gd-5MTDWjwNjm).
# TYPE gardenio_session_exit_code_total counter
gardenio_session_exit_code_total{code="0",project="gardenio",role="crew"} 1
gardenio_session_exit_code_total{code="143",project="gardenio",role="crew"} 1
gardenio_session_exit_code_total{code="other",project="gardenio",role="gardener"} 1
`
	if err := testutil.CollectAndCompare(em.sessionExitCode, strings.NewReader(want)); err != nil {
		t.Fatalf("session exit code series mismatch: %v", err)
	}
}

func TestEventMetrics_RecordSessionExitCode_NilSafe(t *testing.T) {
	var em *EventMetrics
	if em.RecordSessionExitCode(0, "gardenio", "crew") {
		t.Fatal("nil receiver should return false")
	}
}

// --- Upstream provider errors (leg A of the Claude-outage detector) ---------
//
// ka-0000003454 / epic ka-0000000487. The statusreporter tests derive the same
// quantities end-to-end from fake coop pods; these assert the emitter contract
// (label sets, snapshot-replace semantics, cardinality bounds) directly.

func TestEventMetrics_UpstreamErrorActiveGauge(t *testing.T) {
	em := NewEventMetrics("prod")

	em.SetUpstreamErrorActive(map[[3]string]int{
		{CategoryServerError, "gardenio", "crew"}:   3,
		{CategoryServerError, "gardenio", "thread"}: 1,
		{CategoryRateLimited, "acme", "crew"}:       2,
	})

	if got := testutil.CollectAndCount(em.upstreamErrorActive); got != 3 {
		t.Fatalf("expected 3 series, got %d", got)
	}
	// The alert expr is sum(...{category="server_error"}) — assert it reads 4.
	total := testutil.ToFloat64(em.upstreamErrorActive.WithLabelValues(CategoryServerError, "gardenio", "crew")) +
		testutil.ToFloat64(em.upstreamErrorActive.WithLabelValues(CategoryServerError, "gardenio", "thread"))
	if total != 4 {
		t.Errorf("sum over server_error = %v, want 4", total)
	}

	// Snapshot replace: the recovered groups must DISAPPEAR, not linger stale —
	// this is what lets ClaudeUpstreamOutage resolve instead of latching.
	em.SetUpstreamErrorActive(map[[3]string]int{{CategoryServerError, "gardenio", "crew"}: 1})
	if got := testutil.CollectAndCount(em.upstreamErrorActive); got != 1 {
		t.Fatalf("snapshot replace: expected 1 series, got %d", got)
	}
	if got := testutil.ToFloat64(em.upstreamErrorActive.WithLabelValues(CategoryServerError, "gardenio", "crew")); got != 1 {
		t.Errorf("snapshot replace: crew = %v, want 1", got)
	}

	// Empty snapshot clears to no-data: no agent anywhere is erroring upstream.
	em.SetUpstreamErrorActive(map[[3]string]int{})
	if got := testutil.CollectAndCount(em.upstreamErrorActive); got != 0 {
		t.Errorf("empty snapshot: expected 0 series, got %d", got)
	}
}

// An unnormalized category must not reach the gauge. The caller normalizes, so
// this is the second guard: a mis-wired caller cannot mint arbitrary series.
func TestEventMetrics_UpstreamErrorActive_DropsUnnormalizedCategory(t *testing.T) {
	em := NewEventMetrics("prod")
	em.SetUpstreamErrorActive(map[[3]string]int{
		{"totally_made_up", "gardenio", "crew"}:   1,
		{CategoryServerError, "gardenio", "crew"}: 1,
	})
	if got := testutil.CollectAndCount(em.upstreamErrorActive); got != 1 {
		t.Errorf("expected the unnormalized key dropped, got %d series", got)
	}
}

func TestEventMetrics_UpstreamErrorTransitions(t *testing.T) {
	em := NewEventMetrics("prod")

	if !em.RecordUpstreamErrorTransition(CategoryServerError, "gardenio", "crew") {
		t.Error("expected server_error transition to count")
	}
	if !em.RecordUpstreamErrorTransition(CategoryServerError, "gardenio", "crew") {
		t.Error("expected a second entry to count (caller owns the edge dedup)")
	}
	// An unrecognised category folds into `other` rather than its own series.
	if !em.RecordUpstreamErrorTransition("some_future_coop_category", "gardenio", "crew") {
		t.Error("expected an unknown category to count as other")
	}
	// An empty category is "no error" and must increment nothing.
	if em.RecordUpstreamErrorTransition("", "gardenio", "crew") {
		t.Error("empty category must not count")
	}

	if got := testutil.ToFloat64(em.upstreamErrorTransitions.WithLabelValues(CategoryServerError, "gardenio", "crew")); got != 2 {
		t.Errorf("server_error transitions = %v, want 2", got)
	}
	if got := testutil.ToFloat64(em.upstreamErrorTransitions.WithLabelValues(CategoryOther, "gardenio", "crew")); got != 1 {
		t.Errorf("other transitions = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(em.upstreamErrorTransitions); got != 2 {
		t.Errorf("expected exactly 2 series (server_error + other), got %d", got)
	}
}

func TestNormalizeUpstreamErrorCategory(t *testing.T) {
	// Every value here is a coop ErrorCategory::as_str() wire string
	// (coop/crates/cli/src/driver/error_category.rs). If coop gains a category
	// and this list is not updated, it folds to `other` rather than inflating
	// cardinality — that fallback is the contract, asserted below.
	for _, c := range []string{
		CategoryServerError, CategoryRateLimited, CategoryModelRateLimited,
		CategoryNoInternet, CategoryOutOfCredits, CategoryBudgetExceeded,
		CategoryUnauthorized, CategoryOther,
	} {
		if got := NormalizeUpstreamErrorCategory(c); got != c {
			t.Errorf("NormalizeUpstreamErrorCategory(%q) = %q, want passthrough", c, got)
		}
	}
	if got := NormalizeUpstreamErrorCategory(""); got != "" {
		t.Errorf(`empty = %q, want "" (distinguishes "no error" from "unnameable error")`, got)
	}
	for _, c := range []string{"weird", "SERVER_ERROR", "server error", "529"} {
		if got := NormalizeUpstreamErrorCategory(c); got != CategoryOther {
			t.Errorf("NormalizeUpstreamErrorCategory(%q) = %q, want other", c, got)
		}
	}
}

// The registry must accept the new collectors — a duplicate or malformed
// declaration would fail here rather than at controller startup.
func TestEventMetrics_RegistersUpstreamErrorCollectors(t *testing.T) {
	em := NewEventMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := em.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	em.SetUpstreamErrorActive(map[[3]string]int{{CategoryServerError, "gardenio", "crew"}: 1})
	em.RecordUpstreamErrorTransition(CategoryServerError, "gardenio", "crew")

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, name := range []string{
		"gardenio_agent_upstream_error_active",
		"gardenio_agent_upstream_error_transitions_total",
	} {
		if !seen[name] {
			t.Errorf("%s not exposed by the registry", name)
		}
	}
}
