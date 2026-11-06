package bridge

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// recordingNotifier captures TriggerNotifier invocations for assertions.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []string // captured messages
}

func (r *recordingNotifier) notify(_ context.Context, _, message, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, message)
}

func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// gardenerBead builds a type=trigger gardener fixture with the given fields. The
// `gardener` field (non-empty) is what marks it a gardener for the liveness
// sweep; lastChecked controls freshness (empty ⇒ never-evaluated).
func gardenerBead(id, title string, lastChecked string, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"gardener": `{"name":"` + title + `"}`,
		"enabled":  "true",
		"project":  "gardenio",
	}
	if lastChecked != "" {
		fields["last_checked"] = lastChecked
	}
	maps.Copy(fields, extra)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "open",
		Title:  title,
		Fields: fields,
	}
}

// livenessEngine wires a TriggerEngine with the liveness sweep enabled and a
// recording notifier, over the supplied bead fixtures.
func livenessEngine(t *testing.T, notify, createBead bool, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier) {
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
	})
	return engine, daemon, rec
}

func rfc3339Ago(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format(time.RFC3339)
}

// TestLivenessSweep_DarkGardener_Alerts is the core acceptance: a gardener whose
// last_checked is stale (well past N× the default 2h cadence) produces exactly
// one notify alert, with no agent spawn.
func TestLivenessSweep_DarkGardener_Alerts(t *testing.T) {
	// Default cadence 2h, stale factor 3 ⇒ threshold 6h. 10h is dark.
	bead := gardenerBead("gd-dark-1", "investigative janitor", rfc3339Ago(10*time.Hour), nil)
	engine, daemon, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 liveness notify, got %d", rec.count())
	}
	// NO agent spawn — the whole point of B2.
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("liveness sweep must not spawn agents, spawned %d", len(daemon.spawnedNames))
	}
}

// TestLivenessSweep_FreshGardener_NoAlert: a gardener checked within its cadence
// is healthy and must not alert.
func TestLivenessSweep_FreshGardener_NoAlert(t *testing.T) {
	bead := gardenerBead("gd-fresh-1", "fresh gardener", rfc3339Ago(1*time.Minute), nil)
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("fresh gardener must not alert, got %d alerts", rec.count())
	}
}

// TestLivenessSweep_DedupPerEpisode: a gardener that stays dark across multiple
// sweep cycles alerts only once, then re-alerts after a recovery + new dark
// episode.
func TestLivenessSweep_DedupPerEpisode(t *testing.T) {
	bead := gardenerBead("gd-dark-2", "dark gardener", rfc3339Ago(10*time.Hour), nil)
	engine, daemon, rec := livenessEngine(t, true, false, bead)
	ctx := context.Background()

	engine.SweepGardenerLiveness(ctx)
	engine.SweepGardenerLiveness(ctx)
	engine.SweepGardenerLiveness(ctx)
	if rec.count() != 1 {
		t.Fatalf("dark gardener across 3 cycles should alert once (dedup), got %d", rec.count())
	}

	// Recover: last_checked goes fresh. No new alert, and the dark flag clears.
	daemon.mu.Lock()
	bead.Fields["last_checked"] = rfc3339Ago(1 * time.Minute)
	daemon.mu.Unlock()
	engine.SweepGardenerLiveness(ctx)
	if rec.count() != 1 {
		t.Fatalf("recovered gardener should not alert again, got %d", rec.count())
	}

	// Go dark again: a NEW episode re-alerts.
	daemon.mu.Lock()
	bead.Fields["last_checked"] = rfc3339Ago(10 * time.Hour)
	daemon.mu.Unlock()
	engine.SweepGardenerLiveness(ctx)
	if rec.count() != 2 {
		t.Fatalf("new dark episode should re-alert (total 2), got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsDisabled: a disabled gardener is expected to be silent,
// not dark — it must not alert (B3 owns the disable-transition signal).
func TestLivenessSweep_SkipsDisabled(t *testing.T) {
	bead := gardenerBead("gd-off-1", "disabled gardener", rfc3339Ago(10*time.Hour),
		map[string]string{"enabled": "false"})
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("disabled gardener must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsDormant: a gardener parked behind an open operator
// escalation/decision is dormant by design, not dark.
func TestLivenessSweep_SkipsDormant(t *testing.T) {
	bead := gardenerBead("gd-dormant-1", "dormant gardener", rfc3339Ago(10*time.Hour),
		map[string]string{"dormant_on_escalation": "true"})
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("dormant gardener must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsBlockedByOpenDecision: a gardener blocked_by an OPEN
// decision is paused by the engine WITHOUT stamping last_checked, so its
// last_checked freezes while it is perfectly alive. The sweep must not flag it.
func TestLivenessSweep_SkipsBlockedByOpenDecision(t *testing.T) {
	gardener := gardenerBead("gd-blocked-1", "blocked gardener", rfc3339Ago(10*time.Hour),
		map[string]string{"blocked_by": "gd-dec-1"})
	decision := &beadsapi.BeadDetail{ID: "gd-dec-1", Type: "decision", Status: "open", Title: "open decision"}
	engine, _, rec := livenessEngine(t, true, false, gardener, decision)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("gardener blocked by an open decision must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_BlockedByClosedDecision_Alerts: once the blocking decision
// closes, the gardener is no longer legitimately paused — a stale last_checked
// then means it really went dark, so the sweep alerts.
func TestLivenessSweep_BlockedByClosedDecision_Alerts(t *testing.T) {
	gardener := gardenerBead("gd-blocked-2", "unblocked dark gardener", rfc3339Ago(10*time.Hour),
		map[string]string{"blocked_by": "gd-dec-2"})
	decision := &beadsapi.BeadDetail{ID: "gd-dec-2", Type: "decision", Status: "closed", Title: "resolved decision"}
	engine, _, rec := livenessEngine(t, true, false, gardener, decision)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 1 {
		t.Fatalf("gardener past a closed decision that went dark should alert, got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsOneSpawnWithRunningAgent: a one_spawn_at_a_time gardener
// single-threaded behind a still-running agent is paused without stamping
// last_checked. It is alive — the sweep must not flag it.
func TestLivenessSweep_SkipsOneSpawnWithRunningAgent(t *testing.T) {
	gardener := gardenerBead("gd-1spawn-1", "single-threaded gardener", rfc3339Ago(10*time.Hour),
		map[string]string{"gardener": `{"name":"st","one_spawn_at_a_time":true,"last_agent_id":"gd-agent-1"}`})
	agent := &beadsapi.BeadDetail{ID: "gd-agent-1", Type: "agent", Status: "in_progress", Title: "running agent"}
	engine, _, rec := livenessEngine(t, true, false, gardener, agent)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("one_spawn gardener behind a running agent must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_OneSpawnWithFinishedAgent_Alerts: once the prior agent
// terminates, a one_spawn gardener that has gone dark (stopped being evaluated)
// is flaggable again.
func TestLivenessSweep_OneSpawnWithFinishedAgent_Alerts(t *testing.T) {
	gardener := gardenerBead("gd-1spawn-2", "single-threaded dark gardener", rfc3339Ago(10*time.Hour),
		map[string]string{"gardener": `{"name":"st","one_spawn_at_a_time":true,"last_agent_id":"gd-agent-2"}`})
	agent := &beadsapi.BeadDetail{ID: "gd-agent-2", Type: "agent", Status: "closed", Title: "finished agent"}
	engine, _, rec := livenessEngine(t, true, false, gardener, agent)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 1 {
		t.Fatalf("one_spawn gardener with a finished agent that went dark should alert, got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsE2EFixture: E2E fixtures are seeded enabled but never
// fire, so their last_checked is intentionally frozen — sweeping them would
// alert forever.
func TestLivenessSweep_SkipsE2EFixture(t *testing.T) {
	bead := gardenerBead("gd-fixture-1", "e2e fixture gardener", rfc3339Ago(10*time.Hour), nil)
	bead.Labels = []string{e2eFixtureLabelPrefix + "schedule"}
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("e2e fixture gardener must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsClosedGardener locks the SCOPE BOUNDARY: a CLOSED but
// still-enabled gardener (self-closed on a stop condition while never
// operator-disabled) has a frozen last_checked and would read permanently
// stale, yet the sweep must NOT alert on it — that dark class is owned by
// trigger-reviver (ka-0000006342), which drains it human-gated and oldest-first
// so ~130+ retired lineages don't flood the alert channel. This is the
// intake-groomer blind spot from bug ka-0000004633; the sweep's open-only roster
// query is what enforces the boundary, and this test guards against a
// well-meaning widen to status=closed silently turning the floor into a flood.
func TestLivenessSweep_SkipsClosedGardener(t *testing.T) {
	bead := gardenerBead("gd-closed-enabled-1", "self-closed groomer", rfc3339Ago(10*time.Hour),
		map[string]string{"gardener_terminated_kind": "no_progress_for"})
	// Closed but still enabled — the exact dark class of the dark intake-groomer.
	bead.Status = "closed"
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("closed+enabled gardener is owned by trigger-reviver, not the sweep — must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsNeverEvaluated: a gardener the engine has never seen
// (last_checked unset) may have just been created — not yet dark.
func TestLivenessSweep_SkipsNeverEvaluated(t *testing.T) {
	bead := gardenerBead("gd-new-1", "brand new gardener", "", nil)
	// Defeat the mock's auto-inject of a past last_checked by forcing empty.
	bead.Fields["last_checked"] = ""
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("never-evaluated gardener must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_SkipsNonGardener: a plain source-only trigger (no gardener
// block) has no liveness contract and must be ignored even if stale.
func TestLivenessSweep_SkipsNonGardener(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-plain-1",
		Type:   "trigger",
		Status: "open",
		Title:  "plain gitlab trigger",
		Fields: map[string]string{
			"enabled":      "true",
			"last_checked": rfc3339Ago(10 * time.Hour),
		},
	}
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("non-gardener trigger must not alert, got %d", rec.count())
	}
}

// TestLivenessSweep_CreateBeadLeg: with create_bead enabled, a dark gardener
// files a bug bead parented to the gardener.
func TestLivenessSweep_CreateBeadLeg(t *testing.T) {
	bead := gardenerBead("gd-dark-3", "dark gardener", rfc3339Ago(10*time.Hour), nil)
	engine, daemon, _ := livenessEngine(t, false, true, bead)

	engine.SweepGardenerLiveness(context.Background())

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 dark-gardener bead, got %d", len(daemon.createdBeads))
	}
	got := daemon.createdBeads[0]
	if got.Type != "bug" {
		t.Errorf("expected bug bead, got %q", got.Type)
	}
	// Parented to the gardener.
	if len(daemon.addedDeps) != 1 {
		t.Fatalf("expected 1 parent-child dep, got %d", len(daemon.addedDeps))
	}
	dep := daemon.addedDeps[0]
	if dep.DependsOnID != "gd-dark-3" || dep.DepType != "parent-child" {
		t.Errorf("expected parent-child dep onto gd-dark-3, got %+v", dep)
	}
}

// TestLivenessSweep_Disabled_NoOp: with the sweep disabled, even a dark gardener
// produces no alert and no bead.
func TestLivenessSweep_Disabled_NoOp(t *testing.T) {
	bead := gardenerBead("gd-dark-4", "dark gardener", rfc3339Ago(10*time.Hour), nil)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{bead}}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:          daemon,
		Notifier:        rec.notify,
		Logger:          slog.Default(),
		LivenessEnabled: false, // disabled
		LivenessNotify:  true,
	})

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 || len(daemon.createdBeads) != 0 {
		t.Fatalf("disabled sweep must be a no-op, got %d alerts / %d beads",
			rec.count(), len(daemon.createdBeads))
	}
}

// TestLivenessSweep_FiresViaEvaluate: the sweep runs as part of the engine's
// per-cycle Evaluate(), so a stale gardener in the roster alerts without any
// explicit SweepGardenerLiveness call — the in-engine heartbeat the acceptance
// asks for. With no sources registered, Evaluate's only effect is the sweep.
func TestLivenessSweep_FiresViaEvaluate(t *testing.T) {
	bead := gardenerBead("gd-dark-eval", "dark gardener", rfc3339Ago(10*time.Hour), nil)
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.Evaluate(context.Background())

	if rec.count() != 1 {
		t.Fatalf("Evaluate() must run the liveness sweep and alert once, got %d", rec.count())
	}
}

// TestGardenerCadence_FromCron: a gardener with a cron expression derives its
// cadence from the cron period, tightening the staleness threshold below the
// 2h default.
func TestGardenerCadence_FromCron(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

	// Every 30 minutes ⇒ 30m cadence.
	b := gardenerBead("gd-cron-1", "30m gardener", "", map[string]string{"cron": "*/30 * * * *"})
	if got := gardenerCadence(b, now); got != 30*time.Minute {
		t.Errorf("expected 30m cadence from */30 cron, got %s", got)
	}

	// No cron ⇒ default cadence.
	b2 := gardenerBead("gd-cron-2", "no-cron gardener", "", nil)
	if got := gardenerCadence(b2, now); got != livenessDefaultCadence {
		t.Errorf("expected default cadence for no-cron, got %s", got)
	}

	// Malformed cron ⇒ default cadence (fail-safe, not zero).
	b3 := gardenerBead("gd-cron-3", "bad-cron gardener", "", map[string]string{"cron": "not a cron"})
	if got := gardenerCadence(b3, now); got != livenessDefaultCadence {
		t.Errorf("expected default cadence for malformed cron, got %s", got)
	}
}

// TestLivenessStaleThreshold_Floor: a tight cron's threshold is floored so a
// single slow poll does not flag the gardener.
func TestLivenessStaleThreshold_Floor(t *testing.T) {
	// 1m cadence × 3 = 3m, floored to livenessMinStaleThreshold.
	if got := livenessStaleThreshold(1 * time.Minute); got != livenessMinStaleThreshold {
		t.Errorf("expected floored threshold %s, got %s", livenessMinStaleThreshold, got)
	}
	// 2h cadence × 3 = 6h, above the floor.
	if got := livenessStaleThreshold(2 * time.Hour); got != 6*time.Hour {
		t.Errorf("expected 6h threshold, got %s", got)
	}
}

// TestLivenessSweep_CronCadence_DarkUsesTightThreshold: a 30m-cron gardener whose
// last_checked is 2h stale is dark (threshold 90m), even though 2h would be
// fresh under the 6h default-cadence threshold — proving the sweep honors the
// gardener's own cadence.
func TestLivenessSweep_CronCadence_DarkUsesTightThreshold(t *testing.T) {
	bead := gardenerBead("gd-cron-dark", "30m janitor", rfc3339Ago(2*time.Hour),
		map[string]string{"cron": "*/30 * * * *"})
	engine, _, rec := livenessEngine(t, true, false, bead)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 1 {
		t.Fatalf("30m-cron gardener 2h stale should be dark, got %d alerts", rec.count())
	}
}
