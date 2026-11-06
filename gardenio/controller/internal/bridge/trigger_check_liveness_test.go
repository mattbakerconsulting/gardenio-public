package bridge

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// intakeTriggerBead builds a NON-gardener jira intake trigger fixture — the
// shape of the three lanes in the ka-0000004245 outage. Deliberately carries NO
// `gardener` field, which is exactly why SweepGardenerLiveness skips it.
func intakeTriggerBead(id, title, lastChecked string, cooldownMinutes string, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"enabled":          "true",
		"source":           "jira",
		"action":           "spawn_agent",
		"role":             "crew",
		"project":          "monorepo",
		"cooldown_minutes": cooldownMinutes,
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

// checkLivenessTestEngine wires an engine with ONLY the check-liveness sweep
// armed. The gardener liveness sweep is deliberately left OFF so an alert in
// these tests can only have come from SweepTriggerCheckLiveness.
func checkLivenessTestEngine(t *testing.T, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *recordingNotifier) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:               daemon,
		Notifier:             rec.notify,
		Logger:               slog.Default(),
		CheckLivenessEnabled: true,
		CheckLivenessNotify:  true,
	})
	return engine, rec
}

// TestCheckLiveness_ControlVsBroken is the CORE acceptance test, written as a
// matched pair per advice ka-0000002586: a liveness signal is only evidence if
// it reads DIFFERENTLY on a known-good control and a known-broken instance.
//
// Both fixtures are byte-identical except last_checked — same type, same
// enabled=true, same source, same cooldown. `enabled` is `true` on both, which
// is precisely why every existing health surface read healthy through an 11-day
// outage. If this test's two halves ever agree, the sweep is measuring nothing.
func TestCheckLiveness_ControlVsBroken(t *testing.T) {
	t.Run("known-good control stays SILENT", func(t *testing.T) {
		// A healthy lane: the engine stamped last_checked one cooldown ago.
		healthy := intakeTriggerBead("gd-healthy", "PE gardenio-wanted (healthy)",
			rfc3339Ago(5*time.Minute), "5", nil)
		engine, rec := checkLivenessTestEngine(t, healthy)

		engine.SweepTriggerCheckLiveness(context.Background())

		if rec.count() != 0 {
			t.Fatalf("healthy trigger must not alert, got %d: %v", rec.count(), rec.calls)
		}
	})

	t.Run("known-broken instance ALERTS", func(t *testing.T) {
		// The literal outage: last_checked frozen 11 days, enabled still true.
		broken := intakeTriggerBead("ka-0000005979", "Auto-pour Golden Formula on PE tickets",
			rfc3339Ago(11*24*time.Hour), "5", nil)
		engine, rec := checkLivenessTestEngine(t, broken)

		engine.SweepTriggerCheckLiveness(context.Background())

		if rec.count() != 1 {
			t.Fatalf("11-day-frozen trigger must alert exactly once, got %d: %v", rec.count(), rec.calls)
		}
		msg := rec.calls[0]
		for _, want := range []string{"ka-0000005979", "UNCHECKED", "ka-0000004245"} {
			if !strings.Contains(msg, want) {
				t.Errorf("alert must mention %q for the operator to triage; got: %s", want, msg)
			}
		}
	})

	t.Run("both together: only the broken one alerts", func(t *testing.T) {
		healthy := intakeTriggerBead("gd-healthy", "healthy lane", rfc3339Ago(5*time.Minute), "5", nil)
		broken := intakeTriggerBead("gd-broken", "dark lane", rfc3339Ago(11*24*time.Hour), "5", nil)
		engine, rec := checkLivenessTestEngine(t, healthy, broken)

		engine.SweepTriggerCheckLiveness(context.Background())

		if rec.count() != 1 {
			t.Fatalf("exactly one alert expected, got %d: %v", rec.count(), rec.calls)
		}
		if !strings.Contains(rec.calls[0], "gd-broken") {
			t.Errorf("the BROKEN lane must be the one named; got: %s", rec.calls[0])
		}
		if strings.Contains(rec.calls[0], "gd-healthy") {
			t.Errorf("the healthy lane must not be named; got: %s", rec.calls[0])
		}
	})
}

// TestCheckLiveness_AllThreeOutageLanes replays all three real triggers with
// their real cooldowns and their real frozen last_checked. Every one must
// alarm — this is the "would the alarm have caught the incident" assertion.
func TestCheckLiveness_AllThreeOutageLanes(t *testing.T) {
	frozen := rfc3339Ago(11 * 24 * time.Hour)
	beads := []*beadsapi.BeadDetail{
		intakeTriggerBead("ka-0000005979", "PE gardenio-wanted", frozen, "5", nil),
		intakeTriggerBead("ka-0000004986", "DEVOPS gardenio-wanted", frozen, "10", nil),
		intakeTriggerBead("ka-0000005997", "FSM new tickets", frozen, "10", nil),
	}
	engine, rec := checkLivenessTestEngine(t, beads...)

	engine.SweepTriggerCheckLiveness(context.Background())

	if rec.count() != 3 {
		t.Fatalf("all three dark lanes must alarm, got %d: %v", rec.count(), rec.calls)
	}
}

// TestCheckLiveness_GardenerSweepCannotSeeThese pins the GAP this sweep exists to
// close: the same three fixtures, run through SweepGardenerLiveness with the
// gardener sweep fully enabled, produce ZERO alerts because isGardenerTrigger
// gates its roster. If a future change makes the gardener sweep cover plain
// triggers, this test fails and the two sweeps must be reconciled to avoid
// double-alerting.
func TestCheckLiveness_GardenerSweepCannotSeeThese(t *testing.T) {
	frozen := rfc3339Ago(11 * 24 * time.Hour)
	bead := intakeTriggerBead("ka-0000005979", "PE gardenio-wanted", frozen, "5", nil)

	engine, _, rec := livenessEngine(t, true, false, bead)
	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("gardener sweep is expected to be BLIND to plain triggers (that is the gap); got %d alerts: %v",
			rec.count(), rec.calls)
	}
}

// TestCheckLiveness_SkipsGardeners: gardeners are owned by SweepGardenerLiveness
// with a tighter cadence-aware threshold. Sweeping them here would double-alert.
func TestCheckLiveness_SkipsGardeners(t *testing.T) {
	bead := gardenerBead("gd-cap-1", "dark gardener", rfc3339Ago(11*24*time.Hour), nil)
	engine, rec := checkLivenessTestEngine(t, bead)

	engine.SweepTriggerCheckLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("gardeners belong to the gardener sweep, got %d alerts", rec.count())
	}
}

// TestCheckLiveness_DedupPerEpisode: an alert fires once per dark episode, and
// re-arms after recovery. Without this a permanently-dark lane would notify
// every 30s poll forever — itself an incident.
func TestCheckLiveness_DedupPerEpisode(t *testing.T) {
	bead := intakeTriggerBead("gd-dedup", "dark lane", rfc3339Ago(11*24*time.Hour), "5", nil)
	engine, rec := checkLivenessTestEngine(t, bead)

	for range 5 {
		engine.SweepTriggerCheckLiveness(context.Background())
	}
	if rec.count() != 1 {
		t.Fatalf("expected exactly 1 alert across 5 sweeps, got %d", rec.count())
	}

	// Recovery: the stamp starts landing again.
	bead.Fields["last_checked"] = rfc3339Ago(1 * time.Minute)
	engine.SweepTriggerCheckLiveness(context.Background())
	if rec.count() != 1 {
		t.Fatalf("recovered lane must not alert, got %d", rec.count())
	}

	// Relapse: a NEW episode alerts again.
	bead.Fields["last_checked"] = rfc3339Ago(11 * 24 * time.Hour)
	engine.SweepTriggerCheckLiveness(context.Background())
	if rec.count() != 2 {
		t.Fatalf("a new dark episode must re-alert, got %d", rec.count())
	}
}

// TestCheckLiveness_SkipsSilentByDesign covers every state where a frozen
// last_checked is CORRECT and alerting would be a false page. Each of these is
// a real no-stamp path in evalOne — the skip set must track it or the sweep
// becomes noise the operator learns to ignore.
func TestCheckLiveness_SkipsSilentByDesign(t *testing.T) {
	frozen := rfc3339Ago(11 * 24 * time.Hour)
	cases := []struct {
		name   string
		mutate func(*beadsapi.BeadDetail)
	}{
		{"disabled on purpose", func(b *beadsapi.BeadDetail) { b.Fields["enabled"] = "false" }},
		{"e2e fixture (never fires by design)", func(b *beadsapi.BeadDetail) {
			b.Labels = append(b.Labels, e2eFixtureLabelPrefix+"seed")
		}},
		{"dormant on escalation", func(b *beadsapi.BeadDetail) {
			b.Fields["dormant_on_escalation"] = "true"
		}},
		{"closed (frozen by design, owned by the drain sweep)", func(b *beadsapi.BeadDetail) {
			b.Status = "closed"
		}},
		{"never evaluated (just created)", func(b *beadsapi.BeadDetail) {
			b.Fields["last_checked"] = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := intakeTriggerBead("gd-skip", "lane", frozen, "5", nil)
			tc.mutate(bead)
			engine, rec := checkLivenessTestEngine(t, bead)

			engine.SweepTriggerCheckLiveness(context.Background())

			if rec.count() != 0 {
				t.Fatalf("%s must not alert, got %d: %v", tc.name, rec.count(), rec.calls)
			}
		})
	}
}

// TestCheckLiveness_DisabledSweepIsNoOp: the kill switch works.
func TestCheckLiveness_DisabledSweepIsNoOp(t *testing.T) {
	bead := intakeTriggerBead("gd-off", "dark lane", rfc3339Ago(11*24*time.Hour), "5", nil)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{bead}}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:               daemon,
		Notifier:             rec.notify,
		Logger:               slog.Default(),
		CheckLivenessEnabled: false,
	})

	engine.SweepTriggerCheckLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("sweep disabled must be a no-op, got %d", rec.count())
	}
}

// TestCheckLivenessThreshold verifies the threshold arithmetic, including the
// floor. The real lanes' cooldowns (5m, 10m) both land on the floor, so the
// floor — not the factor — is what actually governs in production.
func TestCheckLivenessThreshold(t *testing.T) {
	cases := []struct {
		cooldown time.Duration
		want     time.Duration
	}{
		{30 * time.Second, 30 * time.Minute}, // 3m → floored
		{5 * time.Minute, 30 * time.Minute},  // 30m → exactly the floor
		{10 * time.Minute, 60 * time.Minute}, // 6× clears the floor
		{2 * time.Hour, 12 * time.Hour},
	}
	for _, tc := range cases {
		if got := checkLivenessThreshold(tc.cooldown); got != tc.want {
			t.Errorf("checkLivenessThreshold(%s) = %s, want %s", tc.cooldown, got, tc.want)
		}
	}
}

// TestCheckLiveness_JustPastCooldownIsNotDark guards the false-positive edge: a
// lane checked slightly longer ago than its cooldown is normal jitter, not dark.
func TestCheckLiveness_JustPastCooldownIsNotDark(t *testing.T) {
	bead := intakeTriggerBead("gd-jitter", "slightly late lane", rfc3339Ago(12*time.Minute), "5", nil)
	engine, rec := checkLivenessTestEngine(t, bead)

	engine.SweepTriggerCheckLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a lane 12m stale with a 5m cooldown (threshold 30m) must not alert, got %d", rec.count())
	}
}
