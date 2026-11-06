package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// livenessAllowListEngine wires a TriggerEngine with the liveness sweep enabled,
// a recording notifier, and the given watched-id allow-list, over the supplied
// bead fixtures. Mirrors livenessEngine (gardener_liveness_test.go) but sets
// LivenessWatchIDs so SweepGardenerLivenessAllowList has work to do.
func livenessAllowListEngine(t *testing.T, notify, createBead bool, watchIDs []string, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:             daemon,
		Notifier:           rec.notify,
		Logger:             slog.Default(),
		LivenessEnabled:    true,
		LivenessNotify:     notify,
		LivenessCreateBead: createBead,
		LivenessWatchIDs:   watchIDs,
	})
	return engine, daemon, rec
}

// closedGardenerBead builds a gardener fixture that has self-closed on a stop
// condition — the exact shape of the two dark gardeners in bug ka-0000004451
// (gardener_terminated_kind / gardener_terminated_slot stamped by gardenerTerminate,
// still enabled=true, last_checked frozen at its final cycle).
func closedGardenerBead(id, title, kind string) *beadsapi.BeadDetail {
	b := gardenerBead(id, title, rfc3339Ago(15*24*time.Hour), map[string]string{
		"gardener_terminated_kind": kind,
		"gardener_terminated_slot": "loop_end",
	})
	b.Status = "closed"
	return b
}

// TestLivenessAllowList_SelfClosedGardener_Alerts is the core acceptance for bug
// ka-0000004451: a watched gardener that self-closed (no_progress_for/max_runs)
// weeks ago — invisible to the open-only main sweep AND to the transition-only
// assert once its one close event was missed — is caught by the all-status poll.
func TestLivenessAllowList_SelfClosedGardener_Alerts(t *testing.T) {
	closed := closedGardenerBead("ka-0000007481", "investigative janitor", "no_progress_for")
	engine, daemon, rec := livenessAllowListEngine(t, true, false, []string{"ka-0000007481"}, closed)

	// The MAIN sweep, being open-only, must NOT see it (this is the blind spot).
	engine.SweepGardenerLiveness(context.Background())
	if rec.count() != 0 {
		t.Fatalf("main open-only sweep must not see a closed gardener, got %d", rec.count())
	}

	// The ALLOW-LIST sweep polls across all statuses and DOES catch it.
	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 1 {
		t.Fatalf("allow-list sweep must alert on a self-closed watched gardener, got %d", rec.count())
	}
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("allow-list sweep must not spawn agents, spawned %d", len(daemon.spawnedNames))
	}
	if got := rec.calls[0]; !strings.Contains(got, "ka-0000007481") || !strings.Contains(got, "status=closed") {
		t.Errorf("alert should name the gardener and its closed status, got %q", got)
	}
}

// TestLivenessAllowList_DisabledWatchedGardener_Alerts: a watched gardener that
// was disabled (enabled=false) is the other half of the transition-only blind
// spot — the main sweep skips enabled==false by design. The explicit watch list
// still surfaces it.
func TestLivenessAllowList_DisabledWatchedGardener_Alerts(t *testing.T) {
	off := gardenerBead("gd-off-watch", "disabled janitor", rfc3339Ago(1*time.Minute),
		map[string]string{"enabled": "false"})
	engine, _, rec := livenessAllowListEngine(t, true, false, []string{"gd-off-watch"}, off)

	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 1 {
		t.Fatalf("allow-list sweep must alert on a disabled watched gardener, got %d", rec.count())
	}
	if got := rec.calls[0]; !strings.Contains(got, "disabled") {
		t.Errorf("alert should name the disabled reason, got %q", got)
	}
}

// TestLivenessAllowList_OpenFreshGardener_NoAlert: a watched gardener that is open,
// enabled, and freshly evaluated is healthy — the poll must stay silent (the two
// healthy gardeners in the bug that never transitioned).
func TestLivenessAllowList_OpenFreshGardener_NoAlert(t *testing.T) {
	healthy := gardenerBead("ka-0000006579", "release-owner v3", rfc3339Ago(1*time.Minute), nil)
	engine, _, rec := livenessAllowListEngine(t, true, false, []string{"ka-0000006579"}, healthy)

	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 0 {
		t.Fatalf("open+enabled+fresh watched gardener must not alert, got %d", rec.count())
	}
}

// TestLivenessAllowList_OpenStaleGardener_Alerts: a watched gardener that is still
// open+enabled but has stopped being evaluated (stale last_checked) is dark —
// the same freshness violation the main sweep catches, here also reachable via
// the poll so the two passes agree on the open-stale case.
func TestLivenessAllowList_OpenStaleGardener_Alerts(t *testing.T) {
	stale := gardenerBead("gd-open-stale", "wedged gardener", rfc3339Ago(10*time.Hour), nil)
	engine, _, rec := livenessAllowListEngine(t, true, false, []string{"gd-open-stale"}, stale)

	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 1 {
		t.Fatalf("open+stale watched gardener must alert, got %d", rec.count())
	}
	if got := rec.calls[0]; !strings.Contains(got, "stale") {
		t.Errorf("alert should name the stale reason, got %q", got)
	}
}

// TestLivenessAllowList_MissingWatchedID_Alerts: a watched id that no longer
// resolves (deleted, or an authoring typo that silently rots) is itself a
// violation — the watch list should surface its own decay, not fail silent.
func TestLivenessAllowList_MissingWatchedID_Alerts(t *testing.T) {
	// No bead seeded for the watched id.
	engine, daemon, rec := livenessAllowListEngine(t, true, false, []string{"gd-gone"})

	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 1 {
		t.Fatalf("a vanished watched id must alert, got %d", rec.count())
	}
	if got := rec.calls[0]; !strings.Contains(got, "not found") {
		t.Errorf("alert should say the id was not found, got %q", got)
	}
	// Nothing to parent to, so no dep even if create_bead were on.
	_ = daemon
}

// TestLivenessAllowList_DedupPerEpisode: a watched gardener that stays dark across
// cycles alerts once, then re-alerts after a recovery + new dark episode. Dedup
// lives in a dedicated state map independent of the main sweep's.
func TestLivenessAllowList_DedupPerEpisode(t *testing.T) {
	closed := closedGardenerBead("gd-dedup", "dark janitor", "max_runs")
	engine, daemon, rec := livenessAllowListEngine(t, true, false, []string{"gd-dedup"}, closed)
	ctx := context.Background()

	engine.SweepGardenerLivenessAllowList(ctx)
	engine.SweepGardenerLivenessAllowList(ctx)
	engine.SweepGardenerLivenessAllowList(ctx)
	if rec.count() != 1 {
		t.Fatalf("dark watched gardener across 3 cycles should alert once, got %d", rec.count())
	}

	// Recover: re-opened + fresh. No new alert; dark flag clears.
	daemon.mu.Lock()
	closed.Status = "open"
	closed.Fields["last_checked"] = rfc3339Ago(1 * time.Minute)
	daemon.mu.Unlock()
	engine.SweepGardenerLivenessAllowList(ctx)
	if rec.count() != 1 {
		t.Fatalf("recovered watched gardener should not re-alert, got %d", rec.count())
	}

	// Go dark again: a NEW episode re-alerts.
	daemon.mu.Lock()
	closed.Status = "closed"
	daemon.mu.Unlock()
	engine.SweepGardenerLivenessAllowList(ctx)
	if rec.count() != 2 {
		t.Fatalf("new dark episode should re-alert (total 2), got %d", rec.count())
	}
}

// TestLivenessAllowList_DedupIndependentOfMainSweep: the allow-list and main
// sweep track the SAME gardener id in separate dedup state maps, so an open+stale
// watched gardener can alert via BOTH passes in the same cycle without one
// suppressing the other.
func TestLivenessAllowList_DedupIndependentOfMainSweep(t *testing.T) {
	stale := gardenerBead("gd-both", "stale watched gardener", rfc3339Ago(10*time.Hour), nil)
	engine, _, rec := livenessAllowListEngine(t, true, false, []string{"gd-both"}, stale)
	ctx := context.Background()

	engine.SweepGardenerLiveness(ctx)          // main sweep: open+stale → 1 alert
	engine.SweepGardenerLivenessAllowList(ctx) // allow-list: open+stale → 1 alert (independent key)
	if rec.count() != 2 {
		t.Fatalf("main + allow-list should each alert once for the same id, got %d", rec.count())
	}
}

// TestLivenessAllowList_CreateBeadLeg: with create_bead enabled, a dark watched
// gardener files a bug bead parented to the gardener.
func TestLivenessAllowList_CreateBeadLeg(t *testing.T) {
	closed := closedGardenerBead("gd-cb", "dark janitor", "no_progress_for")
	engine, daemon, _ := livenessAllowListEngine(t, false, true, []string{"gd-cb"}, closed)

	engine.SweepGardenerLivenessAllowList(context.Background())

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 dark-gardener bead, got %d", len(daemon.createdBeads))
	}
	got := daemon.createdBeads[0]
	if got.Type != "bug" {
		t.Errorf("expected bug bead, got %q", got.Type)
	}
	if got.Priority != 1 {
		t.Errorf("expected P1, got %d", got.Priority)
	}
	if len(daemon.addedDeps) != 1 {
		t.Fatalf("expected 1 parent-child dep, got %d", len(daemon.addedDeps))
	}
	if dep := daemon.addedDeps[0]; dep.DependsOnID != "gd-cb" || dep.DepType != "parent-child" {
		t.Errorf("expected parent-child dep onto gd-cb, got %+v", dep)
	}
}

// TestLivenessAllowList_MissingID_CreateBeadUnparented: a vanished watched id
// still files a bead (create_bead on) but has nothing to parent to.
func TestLivenessAllowList_MissingID_CreateBeadUnparented(t *testing.T) {
	engine, daemon, _ := livenessAllowListEngine(t, false, true, []string{"gd-vanished"})

	engine.SweepGardenerLivenessAllowList(context.Background())

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 bead for the vanished id, got %d", len(daemon.createdBeads))
	}
	if len(daemon.addedDeps) != 0 {
		t.Fatalf("a vanished id has no gardener to parent to, got %d deps", len(daemon.addedDeps))
	}
}

// TestLivenessAllowList_EmptyList_NoOp: with no watched ids configured the pass
// is a no-op — it never touches the daemon and never alerts. This is what keeps
// it from flooding on the fleet-wide closed pool the main sweep excludes.
func TestLivenessAllowList_EmptyList_NoOp(t *testing.T) {
	closed := closedGardenerBead("gd-unwatched", "some closed gardener", "max_runs")
	engine, daemon, rec := livenessAllowListEngine(t, true, true, nil, closed)

	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 0 || len(daemon.createdBeads) != 0 {
		t.Fatalf("empty watch list must be a no-op, got %d alerts / %d beads",
			rec.count(), len(daemon.createdBeads))
	}
}

// TestLivenessAllowList_SweepDisabled_NoOp: with LivenessEnabled=false the pass
// is a no-op even with a populated watch list.
func TestLivenessAllowList_SweepDisabled_NoOp(t *testing.T) {
	closed := closedGardenerBead("gd-off", "dark janitor", "max_runs")
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{closed}}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:           daemon,
		Notifier:         rec.notify,
		Logger:           slog.Default(),
		LivenessEnabled:  false, // disabled
		LivenessNotify:   true,
		LivenessWatchIDs: []string{"gd-off"},
	})

	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 0 {
		t.Fatalf("disabled sweep must be a no-op, got %d", rec.count())
	}
}

// TestLivenessAllowList_OnlyWatchedIDsChecked: a closed gardener NOT on the watch
// list is left alone — the poll is scoped to exactly the operator-named ids, so
// it cannot flood on the ~130 fleet-wide closed+enabled lineages.
func TestLivenessAllowList_OnlyWatchedIDsChecked(t *testing.T) {
	watched := closedGardenerBead("gd-watched", "watched dark", "max_runs")
	unwatched := closedGardenerBead("gd-unwatched", "retired lineage", "max_runs")
	engine, _, rec := livenessAllowListEngine(t, true, false, []string{"gd-watched"}, watched, unwatched)

	engine.SweepGardenerLivenessAllowList(context.Background())
	if rec.count() != 1 {
		t.Fatalf("only the watched id should alert, got %d", rec.count())
	}
	if got := rec.calls[0]; !strings.Contains(got, "gd-watched") {
		t.Errorf("the watched id should be the one alerted, got %q", got)
	}
}

// TestLivenessAllowList_FiresViaEvaluate: the allow-list pass runs as part of
// Evaluate(), so a watched dark gardener alerts without an explicit sweep call.
func TestLivenessAllowList_FiresViaEvaluate(t *testing.T) {
	closed := closedGardenerBead("gd-eval", "dark via evaluate", "no_progress_for")
	engine, _, rec := livenessAllowListEngine(t, true, false, []string{"gd-eval"}, closed)

	engine.Evaluate(context.Background())
	if rec.count() != 1 {
		t.Fatalf("Evaluate() must run the allow-list pass and alert once, got %d", rec.count())
	}
}

// TestLivenessAllowList_DedupSurvivesMainSweepPrune is the regression guard for
// the flood bug: the main sweep runs prune(live) every Evaluate cycle with only
// its OPEN-roster ids. A watched gardener that is CLOSED is never in that set, so
// if the allow-list pass shared the main sweep's dedup map its dark flag would be
// pruned every cycle and re-alert forever. The dedicated livenessAllowListState
// must keep dedup intact across full Evaluate cycles → exactly one alert.
func TestLivenessAllowList_DedupSurvivesMainSweepPrune(t *testing.T) {
	closed := closedGardenerBead("gd-noflood", "closed watched gardener", "max_runs")
	engine, _, rec := livenessAllowListEngine(t, true, false, []string{"gd-noflood"}, closed)
	ctx := context.Background()

	// Multiple full Evaluate cycles (each of which runs the main sweep's prune).
	engine.Evaluate(ctx)
	engine.Evaluate(ctx)
	engine.Evaluate(ctx)

	if rec.count() != 1 {
		t.Fatalf("closed watched gardener across 3 Evaluate cycles must alert exactly once "+
			"(main-sweep prune must not wipe the allow-list dedup), got %d", rec.count())
	}
}

// TestSanitizeWatchIDs trims, de-blanks, and de-dupes an operator-supplied list.
func TestSanitizeWatchIDs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"all-blank", []string{"", "  ", "\t"}, nil},
		{"trim", []string{"  gd-a  ", "gd-b"}, []string{"gd-a", "gd-b"}},
		{"dedupe", []string{"gd-a", "gd-a", "gd-b", "gd-a"}, []string{"gd-a", "gd-b"}},
		{"order-preserving", []string{"gd-c", "gd-a", "gd-b"}, []string{"gd-c", "gd-a", "gd-b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeWatchIDs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("sanitizeWatchIDs(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("sanitizeWatchIDs(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestLivenessAllowListViolation covers the poll invariant directly: it mirrors
// the declarative assert ka-0000002431 clause-for-clause but over current state.
func TestLivenessAllowListViolation(t *testing.T) {
	now := time.Now()

	open := gardenerBead("gd-v1", "healthy", rfc3339Ago(1*time.Minute), nil)
	if r := livenessAllowListViolation(open, now); r != "" {
		t.Errorf("open+enabled+fresh should hold, got violation %q", r)
	}

	closed := closedGardenerBead("gd-v2", "closed", "max_runs")
	if r := livenessAllowListViolation(closed, now); !strings.Contains(r, "status=closed") {
		t.Errorf("closed should violate on status, got %q", r)
	}

	off := gardenerBead("gd-v3", "disabled", rfc3339Ago(1*time.Minute), map[string]string{"enabled": "false"})
	if r := livenessAllowListViolation(off, now); !strings.Contains(r, "disabled") {
		t.Errorf("disabled should violate, got %q", r)
	}

	stale := gardenerBead("gd-v4", "stale", rfc3339Ago(10*time.Hour), nil)
	if r := livenessAllowListViolation(stale, now); !strings.Contains(r, "stale") {
		t.Errorf("open+stale should violate on freshness, got %q", r)
	}

	never := gardenerBead("gd-v5", "never-evaluated", "", nil)
	if r := livenessAllowListViolation(never, now); !strings.Contains(r, "unset") {
		t.Errorf("open+enabled but never evaluated should violate, got %q", r)
	}
}

// ctxCapturingNotifier records the context each notify call carries, so a test
// can assert the operator attribution that makes the downstream notice-bead
// CreateBead resolvable. The real NoticeNotifier turns that ctx into the write
// the daemon audit-resolves; the recordingNotifier drops it.
type ctxCapturingNotifier struct {
	mu   sync.Mutex
	ctxs []context.Context
}

func (c *ctxCapturingNotifier) notify(ctx context.Context, _, _, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ctxs = append(c.ctxs, ctx)
}

// TestLivenessSweeps_StampSystemOperator pins gd-8TlhYNyJVk: both liveness
// sweeps must attach an X-System-Operator to the context they hand the
// notifier. Without it the real NoticeNotifier's CreateBead for the alert bead
// is rejected by the daemon's fail-closed audit resolver ("operator could not
// be resolved"), so the DARK gardener is detected but the Slack alert is never
// delivered — the exact silent-drop this fix closes. Mirrors the attribution
// closed-enabled-drain / gardener-epic-reaper already do.
func TestLivenessSweeps_StampSystemOperator(t *testing.T) {
	// A watched gardener that self-closed (allow-list sweep alerts) and, for the
	// main open-only sweep, an open+stale gardener (which it does catch).
	closed := closedGardenerBead("ka-0000007481", "investigative janitor", "no_progress_for")
	stale := gardenerBead("gd-open-stale-op", "wedged gardener", rfc3339Ago(10*time.Hour), nil)

	cap := &ctxCapturingNotifier{}
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{closed, stale}}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:           daemon,
		Notifier:         cap.notify,
		Logger:           slog.Default(),
		LivenessEnabled:  true,
		LivenessNotify:   true,
		LivenessWatchIDs: []string{"ka-0000007481"},
	})

	engine.SweepGardenerLiveness(context.Background())
	engine.SweepGardenerLivenessAllowList(context.Background())

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.ctxs) < 2 {
		t.Fatalf("expected both sweeps to alert (>=2 notify calls), got %d", len(cap.ctxs))
	}
	for i, ctx := range cap.ctxs {
		if got := beadsapi.SystemOperatorFromContext(ctx); got != livenessSystemOperator {
			t.Errorf("notify call %d ctx missing system operator: got %q, want %q", i, got, livenessSystemOperator)
		}
	}
}
