package bridge

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// darkLaneTrigger builds a type=trigger spawn_agent fixture for the dark-lane
// sweep. last_run/last_checked are set from the given ages; extra overrides any
// field (action, enabled, max_concurrent, cron, project, ...).
func darkLaneTrigger(id, title string, lastRunAgo, lastCheckedAgo time.Duration, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"action":         "spawn_agent",
		"enabled":        "true",
		"project":        "gardenio",
		"cron":           "13,43 * * * *", // 30m cadence, the gardener-bugfix lane
		"max_concurrent": "1",
		"last_run":       time.Now().UTC().Add(-lastRunAgo).Format(time.RFC3339),
		"last_checked":   time.Now().UTC().Add(-lastCheckedAgo).Format(time.RFC3339),
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

// darkLaneHolder builds an agent bead that pins triggerID's max_concurrent lease
// (fresh so it is NOT released by either lease bound — the lane is genuinely
// suppressed by a live holder).
func darkLaneHolder(id, triggerID string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "agent", Status: "in_progress",
		Fields:    map[string]string{"spawned_by": "trigger:" + triggerID, "agent": id + "-name"},
		UpdatedAt: time.Now().UTC().Add(-2 * time.Minute),  // fresh churn
		CreatedAt: time.Now().UTC().Add(-30 * time.Minute), // young — under the 6h ceiling
	}
}

// darkLaneEngine wires a TriggerEngine with the dark-lane sweep enabled and a
// recording notifier over the supplied fixtures.
func darkLaneEngine(t *testing.T, notify, createBead bool, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:             daemon,
		Notifier:           rec.notify,
		Logger:             slog.Default(),
		DarkLaneEnabled:    true,
		DarkLaneNotify:     notify,
		DarkLaneCreateBead: createBead,
	})
	return engine, daemon, rec
}

// THE acceptance: an enabled spawn trigger whose last_run is many cadences
// behind a FRESH last_checked, with its max_concurrent lease held by a live
// agent, is flagged as concurrency-suppressed — exactly the silent 15h dark lane
// that the liveness sweep (which reads last_checked) cannot see. One alert, no
// spawn.
func TestDarkLaneSweep_Suppressed_Alerts(t *testing.T) {
	// last_run 15h ago, last_checked current: gap 15h ≫ 4× 30m = 2h threshold.
	trig := darkLaneTrigger("t1", "gardener-bugfix dispatch", 15*time.Hour, 0, nil)
	holder := darkLaneHolder("gd-holder", "t1")
	engine, daemon, rec := darkLaneEngine(t, true, false, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 dark-lane notify, got %d", rec.count())
	}
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("dark-lane sweep must not spawn, spawned %d", len(daemon.spawnedNames))
	}
}

// A healthy lane that fired recently (last_run fresh) must not alert even though
// last_checked is also fresh.
func TestDarkLaneSweep_FreshLastRun_NoAlert(t *testing.T) {
	trig := darkLaneTrigger("t1", "healthy lane", 5*time.Minute, 0, nil)
	holder := darkLaneHolder("gd-holder", "t1")
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("fresh last_run must not alert, got %d", rec.count())
	}
}

// The critical false-positive guard: a lane with a stale last_run but NO live
// holder is not suppressed by concurrency — its last_run is stale because the
// match set is empty (nothing to fire) or a pre-flight gate keeps it dormant.
// Must NOT alert.
func TestDarkLaneSweep_StaleButNoHolder_NoAlert(t *testing.T) {
	// last_run 15h ago (stale) but NO agent holder pins the lease.
	trig := darkLaneTrigger("t1", "idle-but-not-wedged", 15*time.Hour, 0, nil)
	engine, _, rec := darkLaneEngine(t, true, false, trig) // no holder bead

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("stale last_run with an empty lease must not alert, got %d", rec.count())
	}
}

// Dedup: a lane that stays dark across cycles alerts once, then re-alerts after
// it recovers (last_run catches up) and goes dark again.
func TestDarkLaneSweep_DedupPerEpisode(t *testing.T) {
	trig := darkLaneTrigger("t1", "dark lane", 15*time.Hour, 0, nil)
	holder := darkLaneHolder("gd-holder", "t1")
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder)
	ctx := context.Background()

	engine.SweepDarkLaneByConcurrency(ctx)
	engine.SweepDarkLaneByConcurrency(ctx)
	engine.SweepDarkLaneByConcurrency(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected 1 alert across 3 dark sweeps (dedup), got %d", rec.count())
	}

	// Recovery: last_run catches up to now (fired again).
	trig.Fields["last_run"] = time.Now().UTC().Format(time.RFC3339)
	engine.SweepDarkLaneByConcurrency(ctx)
	if rec.count() != 1 {
		t.Fatalf("recovery sweep must not alert, got %d", rec.count())
	}

	// New dark episode re-arms the alert.
	trig.Fields["last_run"] = time.Now().UTC().Add(-15 * time.Hour).Format(time.RFC3339)
	engine.SweepDarkLaneByConcurrency(ctx)
	if rec.count() != 2 {
		t.Fatalf("new dark episode must re-alert, got %d", rec.count())
	}
}

// A disabled trigger is expected to be silent — not dark.
func TestDarkLaneSweep_SkipsDisabled(t *testing.T) {
	trig := darkLaneTrigger("t1", "disabled", 15*time.Hour, 0, map[string]string{"enabled": "false"})
	holder := darkLaneHolder("gd-holder", "t1")
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("disabled trigger must not alert, got %d", rec.count())
	}
}

// A non-spawn trigger (notify/create_bead) has no max_concurrent lease to be
// suppressed by — skip it.
func TestDarkLaneSweep_SkipsNonSpawnAction(t *testing.T) {
	trig := darkLaneTrigger("t1", "notify-only", 15*time.Hour, 0, map[string]string{"action": "notify"})
	engine, _, rec := darkLaneEngine(t, true, false, trig)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("non-spawn action must not alert, got %d", rec.count())
	}
}

// A trigger with no max_concurrent cap (0) has no lease to be suppressed by.
func TestDarkLaneSweep_SkipsNoCap(t *testing.T) {
	trig := darkLaneTrigger("t1", "no cap", 15*time.Hour, 0, map[string]string{"max_concurrent": "0"})
	holder := darkLaneHolder("gd-holder", "t1")
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("uncapped trigger must not alert, got %d", rec.count())
	}
}

// A trigger dormant behind an open operator escalation is frozen by design.
func TestDarkLaneSweep_SkipsDormant(t *testing.T) {
	trig := darkLaneTrigger("t1", "dormant", 15*time.Hour, 0, map[string]string{"dormant_on_escalation": "true"})
	holder := darkLaneHolder("gd-holder", "t1")
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("dormant-on-escalation trigger must not alert, got %d", rec.count())
	}
}

// A never-fired trigger (no last_run) has no fire-gap to measure — a bead-query
// pre-flight may legitimately keep it dormant. Must not alert.
func TestDarkLaneSweep_NeverFired_NoAlert(t *testing.T) {
	trig := darkLaneTrigger("t1", "never fired", 0, 0, nil)
	delete(trig.Fields, "last_run")
	holder := darkLaneHolder("gd-holder", "t1")
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("never-fired trigger must not alert, got %d", rec.count())
	}
}

// The create_bead leg files a durable bug bead parented to the suppressed
// trigger.
func TestDarkLaneSweep_CreateBeadLeg(t *testing.T) {
	trig := darkLaneTrigger("t1", "gardener-bugfix dispatch", 15*time.Hour, 0, nil)
	holder := darkLaneHolder("gd-holder", "t1")
	engine, daemon, _ := darkLaneEngine(t, false, true, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 dark-lane bead, got %d", len(daemon.createdBeads))
	}
	got := daemon.createdBeads[0]
	if got.Type != "bug" {
		t.Errorf("dark-lane bead type = %q, want bug", got.Type)
	}
	if !slices.Contains(got.Labels, "lane-dark") || !slices.Contains(got.Labels, "project:gardenio") {
		t.Errorf("dark-lane bead labels missing expected entries: %v", got.Labels)
	}
	// Parented to the trigger.
	found := false
	for _, d := range daemon.addedDeps {
		if d.DependsOnID == "t1" {
			found = true
		}
	}
	if !found {
		t.Errorf("dark-lane bead not parented to trigger t1, deps=%v", daemon.addedDeps)
	}
}

// Disabled sweep is a strict no-op.
func TestDarkLaneSweep_Disabled_NoOp(t *testing.T) {
	trig := darkLaneTrigger("t1", "would be dark", 15*time.Hour, 0, nil)
	holder := darkLaneHolder("gd-holder", "t1")
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{trig, holder}}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:          daemon,
		Notifier:        rec.notify,
		Logger:          slog.Default(),
		DarkLaneEnabled: false, // off
		DarkLaneNotify:  true,
	})

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("disabled sweep must be a no-op, got %d alerts", rec.count())
	}
}

// darkLaneGapThreshold: the gap threshold is 4× cadence, floored at 1h.
func TestDarkLaneGapThreshold(t *testing.T) {
	// 30m cadence → 4× = 2h (above the 1h floor).
	if got := darkLaneGapThreshold(30 * time.Minute); got != 2*time.Hour {
		t.Errorf("darkLaneGapThreshold(30m) = %v, want 2h", got)
	}
	// 5m cadence → 4× = 20m, floored to 1h.
	if got := darkLaneGapThreshold(5 * time.Minute); got != darkLaneMinGap {
		t.Errorf("darkLaneGapThreshold(5m) = %v, want floor %v", got, darkLaneMinGap)
	}
}

// --- spend-exhaustion classification (bug ka-0000001246) ---

// lastMsg returns the most recent notify message.
func (r *recordingNotifier) lastMsg() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return ""
	}
	return r.calls[len(r.calls)-1]
}

// darkLaneErrorHolder builds an agent bead pinning triggerID's lease that is
// parked in a credential/spend error state (fresh updated_at + young created_at
// so it is NOT lease-released — it genuinely holds the slot). state is the
// agent_state; extra overrides any field (account, project, ...).
func darkLaneErrorHolder(id, triggerID, state string, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"spawned_by":  "trigger:" + triggerID,
		"agent":       id + "-name",
		"agent_state": state,
		"project":     "gardenio",
	}
	maps.Copy(fields, extra)
	return &beadsapi.BeadDetail{
		ID: id, Type: "agent", Status: "in_progress",
		Fields:    fields,
		UpdatedAt: time.Now().UTC().Add(-1 * time.Minute),  // fresh churn (rate-limit backoff loop)
		CreatedAt: time.Now().UTC().Add(-90 * time.Minute), // young — under the 6h ceiling
	}
}

// darkLaneAccount builds a project:gardenio account bead carrying the given spend
// fields, so the classifier can resolve spend_limit / current_spend / days_remaining.
func darkLaneAccount(id, title string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{}
	maps.Copy(f, fields)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "account",
		Status: "open",
		Title:  title,
		Labels: []string{"project:gardenio"},
		Fields: f,
	}
}

// THE spend-observability acceptance (bug ka-0000001246): a dark lane pinned by a
// rate_limited holder whose account is over its monthly spend limit fires the
// SPEND-EXHAUSTED variant of the alert, carrying spend_limit + current_spend +
// days_remaining and the correct remediation (do NOT kill the holder).
func TestDarkLaneSweep_SpendExhausted_Alerts(t *testing.T) {
	trig := darkLaneTrigger("t1", "gardener-delivery merge lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "rate_limited", nil)
	acct := darkLaneAccount("gd-acct", "gardenio", map[string]string{
		"spend_limit_usd":     "500",
		"total_cost_usd":      "500",
		"spend_at_last_reset": "0",
		"spend_reset_at":      time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339),
	})
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder, acct)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 dark-lane notify, got %d", rec.count())
	}
	msg := rec.lastMsg()
	for _, want := range []string{"SPEND-EXHAUSTED", "spend_limit=$500.00", "current_spend=$500.00", "days_remaining=3.0d", "Do NOT", "rate_limited", `"gardenio"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("spend-exhausted alert missing %q\n  msg=%s", want, msg)
		}
	}
}

// budget_exceeded is the sibling error state and triggers the spend variant too.
func TestDarkLaneSweep_BudgetExceededHolder_SpendVariant(t *testing.T) {
	trig := darkLaneTrigger("t1", "lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "budget_exceeded", nil)
	acct := darkLaneAccount("gd-acct", "gardenio", map[string]string{
		"max_budget":     "25",
		"total_cost_usd": "30",
	})
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder, acct)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 alert, got %d", rec.count())
	}
	msg := rec.lastMsg()
	if !strings.Contains(msg, "SPEND-EXHAUSTED") || !strings.Contains(msg, "budget_exceeded") {
		t.Errorf("expected spend-exhausted alert naming budget_exceeded, got: %s", msg)
	}
	// max_budget fallback ($25) supplies the limit; current_spend = 30.
	if !strings.Contains(msg, "spend_limit=$25.00") || !strings.Contains(msg, "current_spend=$30.00") {
		t.Errorf("expected max_budget fallback numbers, got: %s", msg)
	}
}

// A dark lane pinned by a HEALTHY (working) holder is plain concurrency
// suppression — the classic alert, NOT the spend variant. Guards against
// mislabelling a genuinely-busy long run as spend-exhausted.
func TestDarkLaneSweep_HealthyHolder_PlainVariant(t *testing.T) {
	trig := darkLaneTrigger("t1", "lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "working", nil)
	acct := darkLaneAccount("gd-acct", "gardenio", map[string]string{
		"spend_limit_usd": "500", "total_cost_usd": "500",
	})
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder, acct)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 alert, got %d", rec.count())
	}
	msg := rec.lastMsg()
	if strings.Contains(msg, "SPEND-EXHAUSTED") {
		t.Errorf("a working holder must NOT be labelled spend-exhausted, got: %s", msg)
	}
	if !strings.Contains(msg, "SUPPRESSED BY CONCURRENCY") {
		t.Errorf("expected the plain concurrency alert, got: %s", msg)
	}
}

// Spend-exhausted but the account is UNRESOLVED (no project account bead): the
// alert still fires — the dark lane is real — with the numbers as "unknown"
// rather than being suppressed or fabricating a value.
func TestDarkLaneSweep_SpendExhausted_UnknownAccount(t *testing.T) {
	trig := darkLaneTrigger("t1", "lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "rate_limited", nil)
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder) // no account bead

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 alert even with an unresolved account, got %d", rec.count())
	}
	msg := rec.lastMsg()
	if !strings.Contains(msg, "SPEND-EXHAUSTED") {
		t.Errorf("expected spend-exhausted classification from the holder state alone, got: %s", msg)
	}
	for _, want := range []string{"spend_limit=unknown", "current_spend=unknown", "days_remaining=unknown", "(unknown)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected honest 'unknown' for the unresolved account, missing %q\n  msg=%s", want, msg)
		}
	}
}

// With several project accounts, the classifier picks the one that actually
// looks exhausted so the surfaced numbers match the wall the holder hit.
func TestDarkLaneSweep_PicksExhaustedAccount(t *testing.T) {
	trig := darkLaneTrigger("t1", "lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "rate_limited", nil)
	healthy := darkLaneAccount("gd-healthy", "gardenio-healthy", map[string]string{
		"throttle_status": "allowed", "total_cost_usd": "10",
	})
	exhausted := darkLaneAccount("gd-exhausted", "gardenio-exhausted", map[string]string{
		"throttle_status": "rejected", "spend_limit_usd": "100", "total_cost_usd": "100",
	})
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder, healthy, exhausted)

	engine.SweepDarkLaneByConcurrency(context.Background())

	msg := rec.lastMsg()
	if !strings.Contains(msg, `"gardenio-exhausted"`) {
		t.Errorf("expected the exhausted account to be surfaced, got: %s", msg)
	}
	if !strings.Contains(msg, "spend_limit=$100.00") {
		t.Errorf("expected the exhausted account's numbers, got: %s", msg)
	}
}

// The direct holder→account link (holder's `account` field) wins over pool
// heuristics when present.
func TestDarkLaneSweep_HolderAccountLinkWins(t *testing.T) {
	trig := darkLaneTrigger("t1", "lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "rate_limited", map[string]string{"account": "gardenio-named"})
	other := darkLaneAccount("gd-other", "gardenio-exhausted", map[string]string{
		"throttle_status": "rejected", "spend_limit_usd": "100", "total_cost_usd": "100",
	})
	named := darkLaneAccount("gd-named", "gardenio-named", map[string]string{
		"spend_limit_usd": "250", "total_cost_usd": "250",
	})
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder, other, named)

	engine.SweepDarkLaneByConcurrency(context.Background())

	msg := rec.lastMsg()
	if !strings.Contains(msg, `"gardenio-named"`) || !strings.Contains(msg, "spend_limit=$250.00") {
		t.Errorf("expected the holder's named account to win, got: %s", msg)
	}
}

// The create_bead leg for a spend-exhausted lane appends the labelled spend
// block so the durable bug bead carries the numbers.
func TestDarkLaneSweep_SpendExhausted_CreateBeadDetail(t *testing.T) {
	trig := darkLaneTrigger("t1", "lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "rate_limited", nil)
	acct := darkLaneAccount("gd-acct", "gardenio", map[string]string{
		"spend_limit_usd": "500", "total_cost_usd": "480",
	})
	engine, daemon, _ := darkLaneEngine(t, false, true, trig, holder, acct)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 dark-lane bead, got %d", len(daemon.createdBeads))
	}
	desc := daemon.createdBeads[0].Description
	for _, want := range []string{"spend_exhausted: true", "spend_limit_usd: $500.00", "current_spend_usd: $480.00", "holder: gd-holder"} {
		if !strings.Contains(desc, want) {
			t.Errorf("dark-lane bead description missing %q\n  desc=%s", want, desc)
		}
	}
}

// A past reset instant yields days_remaining 0 (reset is due), not a negative or
// fabricated number.
func TestDarkLaneSweep_PastReset_ZeroDays(t *testing.T) {
	trig := darkLaneTrigger("t1", "lane", 5*time.Hour, 0, nil)
	holder := darkLaneErrorHolder("gd-holder", "t1", "rate_limited", nil)
	acct := darkLaneAccount("gd-acct", "gardenio", map[string]string{
		"spend_limit_usd": "500", "total_cost_usd": "500",
		"throttle_reset_at": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder, acct)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if !strings.Contains(rec.lastMsg(), "days_remaining=0.0d") {
		t.Errorf("expected days_remaining=0.0d for a past reset, got: %s", rec.lastMsg())
	}
}

// --- condition-suppressed dark lane (bug ka-0000001127) ---

// intakeGroomerCondition mirrors the real ka-0000006210 pre-flight: a
// project-scoped bead_query that fires on `> 0` open needs-intake work but
// subtracts any child blocked_by an OPEN task. When every matched child is so
// blocked, the effective count floors to 0 (the self-subtraction bug shape,
// ka-0000006656) while the raw lane is non-empty.
const intakeGroomerCondition = `[{"type":"bead_query","op":">","value":"0","project":"gardenio","labels":["needs-intake"],"types":["task"],"statuses":["open"],"exclude_blocked_by_open_types":["task"]}]`

// darkLaneWork builds an open task in the needs-intake lane the intake-groomer
// pre-flight matches (project:gardenio + needs-intake).
func darkLaneWork(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "task", Status: "open",
		Labels: []string{"project:gardenio", "needs-intake"},
		Fields: map[string]string{},
	}
}

// darkLaneBlockerTask builds an OPEN task that keeps a matched child
// non-actionable via a `blocks` dep. It carries NO needs-intake label so the
// pre-flight's own list query never matches it — it only surfaces as a blocker.
func darkLaneBlockerTask(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "task", Status: "open",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{},
	}
}

// THE condition-suppression acceptance: an enabled spawn trigger with a
// bead_query pre-flight, a stale last_run behind a fresh last_checked, and an
// EMPTY lease, whose effective pre-flight subtracts to 0 while matching beads
// really exist, is flagged as CONDITION-suppressed — the class the
// concurrency-only path is blind to. One alert, no spawn.
func TestDarkLaneSweep_ConditionSuppressed_Alerts(t *testing.T) {
	trig := darkLaneTrigger("t1", "intake-groomer", 15*time.Hour, 0,
		map[string]string{"conditions": intakeGroomerCondition})
	w1, w2 := darkLaneWork("gd-w1"), darkLaneWork("gd-w2")
	blk := darkLaneBlockerTask("gd-blk")
	engine, daemon, rec := darkLaneEngine(t, true, false, trig, w1, w2, blk)
	// Both matched work beads are blocked_by the OPEN task, so the effective
	// pre-flight subtracts them to 0 while the raw lane counts 2.
	daemon.addedDeps = []triggerMockDep{
		{BeadID: "gd-w1", DependsOnID: "gd-blk", DepType: "blocks"},
		{BeadID: "gd-w2", DependsOnID: "gd-blk", DepType: "blocks"},
	}

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 condition-suppressed dark-lane notify, got %d", rec.count())
	}
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("dark-lane sweep must not spawn, spawned %d", len(daemon.spawnedNames))
	}
	msg := rec.lastMsg()
	for _, want := range []string{"CONDITION-SUPPRESSED", "effective count of 0", "2 matching bead(s)", "exclude_blocked_by_open_types=task", "ka-0000001127"} {
		if !strings.Contains(msg, want) {
			t.Errorf("condition-suppressed alert missing %q\n  msg=%s", want, msg)
		}
	}
	// It must NOT be mislabelled as the concurrency variant.
	if strings.Contains(msg, "SUPPRESSED BY CONCURRENCY") {
		t.Errorf("condition-suppressed lane must not use the concurrency alert, got: %s", msg)
	}
}

// The critical false-positive guard: a lane with the SAME pre-flight but a
// genuinely-empty raw lane (no matching beads at all) is legitimately idle, not
// dark. Must NOT alert — this is the noise the detector deliberately suppresses.
func TestDarkLaneSweep_ConditionEmptyRawLane_NoAlert(t *testing.T) {
	trig := darkLaneTrigger("t1", "intake-groomer", 15*time.Hour, 0,
		map[string]string{"conditions": intakeGroomerCondition})
	engine, _, rec := darkLaneEngine(t, true, false, trig) // no work beads at all

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a genuinely-empty raw lane must not alert, got %d", rec.count())
	}
}

// A lane whose effective pre-flight WOULD fire (matched work is unblocked, so
// raw == effective > 0) is about to fire, not dark. Must NOT alert — guards
// against flagging a lane that is merely between fires.
func TestDarkLaneSweep_ConditionEffectiveWouldFire_NoAlert(t *testing.T) {
	trig := darkLaneTrigger("t1", "intake-groomer", 15*time.Hour, 0,
		map[string]string{"conditions": intakeGroomerCondition})
	w1 := darkLaneWork("gd-w1") // unblocked → effective count 1 > 0
	engine, _, rec := darkLaneEngine(t, true, false, trig, w1)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a lane whose pre-flight would fire must not alert, got %d", rec.count())
	}
}

// A pre-flight bead_query with NO exclude subtraction can never diverge
// raw-from-effective, so it can never be condition-suppressed — skip it even
// with a stale last_run and empty lease.
func TestDarkLaneSweep_ConditionNoExcludes_NoAlert(t *testing.T) {
	cond := `[{"type":"bead_query","op":">","value":"0","project":"gardenio","labels":["needs-intake"],"types":["task"],"statuses":["open"]}]`
	trig := darkLaneTrigger("t1", "no-exclude lane", 15*time.Hour, 0,
		map[string]string{"conditions": cond})
	// Even with matching work present, an exclude-free pre-flight would simply
	// fire — there is no subtraction to hide it.
	w1 := darkLaneWork("gd-w1")
	engine, _, rec := darkLaneEngine(t, true, false, trig, w1)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 0 {
		t.Fatalf("an exclude-free pre-flight must not alert, got %d", rec.count())
	}
}

// A full concurrency lease still routes to the CONCURRENCY alert, not the
// condition probe — the condition path is strictly the empty-lease case.
func TestDarkLaneSweep_ConditionButFullLease_ConcurrencyVariant(t *testing.T) {
	trig := darkLaneTrigger("t1", "intake-groomer", 15*time.Hour, 0,
		map[string]string{"conditions": intakeGroomerCondition})
	holder := darkLaneHolder("gd-holder", "t1") // fills the max_concurrent=1 lease
	engine, _, rec := darkLaneEngine(t, true, false, trig, holder)

	engine.SweepDarkLaneByConcurrency(context.Background())

	if rec.count() != 1 {
		t.Fatalf("a full lease must alert (concurrency variant), got %d", rec.count())
	}
	if !strings.Contains(rec.lastMsg(), "SUPPRESSED BY CONCURRENCY") {
		t.Errorf("a full lease must use the concurrency alert, got: %s", rec.lastMsg())
	}
}

// The create_bead leg for a condition-suppressed lane files a durable bug bead
// with the distinct lane-dark-condition label and the raw-vs-effective counts,
// parented to the trigger.
func TestDarkLaneSweep_ConditionSuppressed_CreateBeadLeg(t *testing.T) {
	trig := darkLaneTrigger("t1", "intake-groomer", 15*time.Hour, 0,
		map[string]string{"conditions": intakeGroomerCondition})
	w1 := darkLaneWork("gd-w1")
	blk := darkLaneBlockerTask("gd-blk")
	engine, daemon, _ := darkLaneEngine(t, false, true, trig, w1, blk)
	daemon.addedDeps = []triggerMockDep{
		{BeadID: "gd-w1", DependsOnID: "gd-blk", DepType: "blocks"},
	}

	engine.SweepDarkLaneByConcurrency(context.Background())

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 condition-dark-lane bead, got %d", len(daemon.createdBeads))
	}
	got := daemon.createdBeads[0]
	if got.Type != "bug" {
		t.Errorf("condition-dark-lane bead type = %q, want bug", got.Type)
	}
	if !strings.Contains(got.Title, "condition-suppressed") {
		t.Errorf("condition-dark-lane bead title should name the class, got %q", got.Title)
	}
	for _, want := range []string{"lane-dark", "lane-dark-condition", "project:gardenio"} {
		if !slices.Contains(got.Labels, want) {
			t.Errorf("condition-dark-lane bead labels missing %q: %v", want, got.Labels)
		}
	}
	for _, want := range []string{"raw_lane_count: 1", "effective_preflight_count: 0", "exclude_blocked_by_open_types: task"} {
		if !strings.Contains(got.Description, want) {
			t.Errorf("condition-dark-lane bead description missing %q\n  desc=%s", want, got.Description)
		}
	}
	found := false
	for _, d := range daemon.addedDeps {
		if d.DependsOnID == "t1" {
			found = true
		}
	}
	if !found {
		t.Errorf("condition-dark-lane bead not parented to trigger t1, deps=%v", daemon.addedDeps)
	}
}

// Dedup: a condition-suppressed lane alerts once per episode, then re-arms after
// it recovers (last_run catches up) and goes dark again.
func TestDarkLaneSweep_ConditionSuppressed_DedupPerEpisode(t *testing.T) {
	trig := darkLaneTrigger("t1", "intake-groomer", 15*time.Hour, 0,
		map[string]string{"conditions": intakeGroomerCondition})
	w1 := darkLaneWork("gd-w1")
	blk := darkLaneBlockerTask("gd-blk")
	engine, daemon, rec := darkLaneEngine(t, true, false, trig, w1, blk)
	daemon.addedDeps = []triggerMockDep{{BeadID: "gd-w1", DependsOnID: "gd-blk", DepType: "blocks"}}
	ctx := context.Background()

	engine.SweepDarkLaneByConcurrency(ctx)
	engine.SweepDarkLaneByConcurrency(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected 1 alert across 2 dark sweeps (dedup), got %d", rec.count())
	}

	// Recovery: last_run catches up (the lane fired again).
	trig.Fields["last_run"] = time.Now().UTC().Format(time.RFC3339)
	engine.SweepDarkLaneByConcurrency(ctx)
	if rec.count() != 1 {
		t.Fatalf("recovery sweep must not alert, got %d", rec.count())
	}

	// New dark episode re-arms the alert.
	trig.Fields["last_run"] = time.Now().UTC().Add(-15 * time.Hour).Format(time.RFC3339)
	engine.SweepDarkLaneByConcurrency(ctx)
	if rec.count() != 2 {
		t.Fatalf("new dark episode must re-alert, got %d", rec.count())
	}
}
