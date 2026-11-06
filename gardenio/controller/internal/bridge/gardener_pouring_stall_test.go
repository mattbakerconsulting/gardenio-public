package bridge

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// pourStallEngine wires a TriggerEngine with the pouring-stall sweep enabled, a
// recording notifier, and a FIXED clock so molecule-age comparisons are
// deterministic. Returns the engine, its mock daemon, the recorder, and the
// clock's "now".
func pourStallEngine(t *testing.T, notify, createBead bool, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier, time.Time) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                 daemon,
		Notifier:               rec.notify,
		Logger:                 slog.Default(),
		PouringStallEnabled:    true,
		PouringStallNotify:     notify,
		PouringStallCreateBead: createBead,
		Clock:                  func() time.Time { return now },
	})
	return engine, daemon, rec, now
}

// pourTrigger builds an enabled apply_formula trigger bead with the given cron
// cadence. A non-empty last_checked keeps it out of the runtime-bootstrap
// path (the mock auto-injects one anyway).
func pourTrigger(id, title string, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"action":  "apply_formula",
		"enabled": "true",
		"project": "gardenio",
		"cron":    "0 * * * *", // hourly ⇒ inert threshold = max(3h, 1h) = 3h
	}
	for k, v := range extra {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "open",
		Title:  title,
		Fields: fields,
	}
}

// molBead builds an open molecule bead with the given created/updated times.
func molBead(id string, created, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "molecule",
		Status:    "open",
		Title:     "molecule " + id,
		Fields:    map[string]string{},
		CreatedAt: created,
		UpdatedAt: updated,
	}
}

// stepBead builds a step bead (child of a molecule) with a status and
// created/updated times.
func stepBead(id, status string, created, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "task",
		Status:    status,
		Title:     "step " + id,
		Fields:    map[string]string{},
		CreatedAt: created,
		UpdatedAt: updated,
	}
}

// wireMolecule seeds a triggered-by edge molecule->trigger and parent-child
// edges step->molecule into the mock's addedDeps, mirroring what
// instantiateFormula + writeTriggerLineage write in production.
func wireMolecule(d *mockTriggerDaemon, trigID, molID string, stepIDs ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addedDeps = append(d.addedDeps, triggerMockDep{BeadID: molID, DependsOnID: trigID, DepType: "triggered-by"})
	for _, s := range stepIDs {
		d.addedDeps = append(d.addedDeps, triggerMockDep{BeadID: s, DependsOnID: molID, DepType: "parent-child"})
	}
}

// TestPourStall_AccumulatedInert_Alerts is the core acceptance: an
// apply_formula trigger with >= pourStallMinMolecules inert molecules (each
// poured long ago, never worked) alerts exactly once and spawns nothing.
func TestPourStall_AccumulatedInert_Alerts(t *testing.T) {
	trig := pourTrigger("gd-flat-1", "FLAT gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	// 3 molecules poured 8h ago, each with a single never-touched open step.
	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-mol-a", "gd-mol-b", "gd-mol-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-step-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 pouring-stall alert, got %d", rec.count())
	}
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("pouring-stall sweep must not spawn agents, spawned %d", len(daemon.spawnedNames))
	}
	// The verdict must be stamped on the trigger bead.
	if !stampedField(daemon, trig.ID, pourStallField, "true") {
		t.Fatalf("expected pour_stall=true stamped on trigger, updates=%v", daemon.updatedFields)
	}
}

// TestPourStall_ActivelyWorked_NoAlert: molecules whose subtree shows recent
// activity (a step updated within the inert threshold) are being worked and
// must not alert, even if there are many of them.
func TestPourStall_ActivelyWorked_NoAlert(t *testing.T) {
	trig := pourTrigger("gd-healthy-1", "healthy gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-8 * time.Hour)
	fresh := now.Add(-10 * time.Minute) // within the 3h inert threshold
	for i, molID := range []string{"gd-hm-a", "gd-hm-b", "gd-hm-c"} {
		mol := molBead(molID, old, old)
		// Step was claimed recently — subtree is fresh.
		step := stepBead("gd-hs-"+string(rune('a'+i)), "in_progress", old, fresh)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())

	if rec.count() != 0 {
		t.Fatalf("actively-worked molecules must not alert, got %d", rec.count())
	}
}

// TestPourStall_PartialWorkThenDied_Alerts is the critique's blocker FN: a
// molecule with one step CLOSED long ago (driver did one step then died) is
// still inert — the freshness criterion catches it even though it is not
// zero-work. Three such molecules trip the accumulation arm.
func TestPourStall_PartialWorkThenDied_Alerts(t *testing.T) {
	trig := pourTrigger("gd-partial-1", "partial-work gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-8 * time.Hour) // last activity (the close) was 8h ago
	for i, molID := range []string{"gd-pm-a", "gd-pm-b", "gd-pm-c"} {
		mol := molBead(molID, old, old)
		closedStep := stepBead("gd-pcs-"+string(rune('a'+i)), "closed", old, old)
		openStep := stepBead("gd-pos-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, closedStep, openStep)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, closedStep.ID, openStep.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())

	if rec.count() != 1 {
		t.Fatalf("partial-work-then-died molecules should alert, got %d", rec.count())
	}
}

// TestPourStall_SingleBigMolecule_Alerts is the critique's blocker FN: a
// trigger that pours ONE big molecule never reaches pourStallMinMolecules, so
// the hard-stale (time-based) arm must fire on a single molecule that has been
// quiet past pourStallHardStaleMinAge (6h floor).
func TestPourStall_SingleBigMolecule_Alerts(t *testing.T) {
	trig := pourTrigger("gd-big-1", "one-big-molecule gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-9 * time.Hour) // past the 6h hard-stale floor
	mol := molBead("gd-big-mol", old, old)
	step := stepBead("gd-big-step", "open", old, old)
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, mol, step)
	daemon.mu.Unlock()
	wireMolecule(daemon, trig.ID, mol.ID, step.ID)

	engine.SweepPouringButNeverWorked(context.Background())

	if rec.count() != 1 {
		t.Fatalf("single molecule stale past the hard-stale floor should alert, got %d", rec.count())
	}
}

// TestPourStall_SingleFreshlyInertMolecule_NoAlert: a single molecule that is
// inert but only mildly stale (past the 3h inert threshold but below the 6h
// hard-stale floor) must NOT alert on its own — the count arm needs 3, the
// hard-stale arm needs 6h.
func TestPourStall_SingleFreshlyInertMolecule_NoAlert(t *testing.T) {
	trig := pourTrigger("gd-mild-1", "mildly-stale gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-4 * time.Hour) // > 3h inert, < 6h hard-stale
	mol := molBead("gd-mild-mol", old, old)
	step := stepBead("gd-mild-step", "open", old, old)
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, mol, step)
	daemon.mu.Unlock()
	wireMolecule(daemon, trig.ID, mol.ID, step.ID)

	engine.SweepPouringButNeverWorked(context.Background())

	if rec.count() != 0 {
		t.Fatalf("one mildly-stale molecule must not alert alone, got %d", rec.count())
	}
}

// TestPourStall_FreshPourGrace: molecules poured within the inert threshold are
// given grace (a driver may still be spawning/claiming) and must not alert even
// if there are many.
func TestPourStall_FreshPourGrace(t *testing.T) {
	trig := pourTrigger("gd-fresh-pour-1", "fresh-pour gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	recent := now.Add(-30 * time.Minute) // within the 3h grace
	for i, molID := range []string{"gd-fp-a", "gd-fp-b", "gd-fp-c", "gd-fp-d"} {
		mol := molBead(molID, recent, recent)
		step := stepBead("gd-fps-"+string(rune('a'+i)), "open", recent, recent)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())

	if rec.count() != 0 {
		t.Fatalf("freshly-poured molecules must be graced, got %d", rec.count())
	}
}

// TestPourStall_DedupAndRecovery: a trigger that stays stalled across cycles
// alerts once; when the molecules are worked/closed (recovery) the verdict
// clears and a subsequent stall re-alerts.
func TestPourStall_DedupAndRecovery(t *testing.T) {
	trig := pourTrigger("gd-dedup-1", "dedup gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)
	ctx := context.Background()

	old := now.Add(-8 * time.Hour)
	var mols []*beadsapi.BeadDetail
	for i, molID := range []string{"gd-dm-a", "gd-dm-b", "gd-dm-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-ds-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
		mols = append(mols, mol)
	}

	// Two consecutive sweeps: dedup to a single alert. (The recheck-TTL means
	// the second sweep re-uses the cached verdict, but the dedup on the alert
	// leg is what we assert here.)
	engine.SweepPouringButNeverWorked(ctx)
	engine.SweepPouringButNeverWorked(ctx)
	if rec.count() != 1 {
		t.Fatalf("stalled trigger across 2 cycles should alert once, got %d", rec.count())
	}

	// Recover: close the molecules so they drop out of the open-molecule filter.
	// Force a re-walk by clearing the recheck-TTL cache.
	daemon.mu.Lock()
	for _, m := range mols {
		m.Status = "closed"
	}
	daemon.mu.Unlock()
	engine.pouringStallState.mu.Lock()
	if st := engine.pouringStallState.byTrigger[trig.ID]; st != nil {
		st.lastWalkAt = time.Time{}
	}
	engine.pouringStallState.mu.Unlock()

	engine.SweepPouringButNeverWorked(ctx)
	if rec.count() != 1 {
		t.Fatalf("recovered trigger must not re-alert, got %d", rec.count())
	}
	if !stampedField(daemon, trig.ID, pourStallField, "") {
		t.Fatalf("recovery should clear pour_stall field")
	}
}

// TestPourStall_GrowingCountReStamped: while a stall persists, a fresh walk
// that finds MORE inert molecules must refresh the stamped count (so read
// surfaces show current severity) WITHOUT re-alerting the operator.
func TestPourStall_GrowingCountReStamped(t *testing.T) {
	trig := pourTrigger("gd-grow-1", "growing-stall gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)
	ctx := context.Background()

	old := now.Add(-8 * time.Hour)
	addInert := func(molID string) {
		mol := molBead(molID, old, old)
		step := stepBead(molID+"-s", "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}
	for _, m := range []string{"gd-gm-a", "gd-gm-b", "gd-gm-c"} {
		addInert(m)
	}

	engine.SweepPouringButNeverWorked(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected 1 alert on first detection, got %d", rec.count())
	}
	if !stampedFieldValue(daemon, trig.ID, pourStallMoleculesField, "3") {
		t.Fatalf("expected pour_stall_molecules=3 after first detection")
	}

	// Grow to 5 inert molecules; force a fresh walk past the recheck-TTL.
	addInert("gd-gm-d")
	addInert("gd-gm-e")
	engine.pouringStallState.mu.Lock()
	if st := engine.pouringStallState.byTrigger[trig.ID]; st != nil {
		st.lastWalkAt = time.Time{}
	}
	engine.pouringStallState.mu.Unlock()

	engine.SweepPouringButNeverWorked(ctx)
	if rec.count() != 1 {
		t.Fatalf("growing count must NOT re-alert, got %d", rec.count())
	}
	if !stampedFieldValue(daemon, trig.ID, pourStallMoleculesField, "5") {
		t.Fatalf("expected pour_stall_molecules refreshed to 5, updates=%v", daemon.updatedFields)
	}
}

// TestPourStall_SkippedTriggerKeepsState: a trigger that goes dormant mid-stall
// must keep its dedup state across sweeps (dormant triggers are skipped but not
// pruned), so it does not re-alert when it becomes active again.
func TestPourStall_SkippedTriggerKeepsState(t *testing.T) {
	trig := pourTrigger("gd-keep-1", "dormant-then-active gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)
	ctx := context.Background()

	old := now.Add(-8 * time.Hour)
	for _, molID := range []string{"gd-km-a", "gd-km-b", "gd-km-c"} {
		mol := molBead(molID, old, old)
		step := stepBead(molID+"-s", "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected 1 alert, got %d", rec.count())
	}

	// Trigger goes dormant: skipped this cycle. Its state must survive the prune.
	daemon.mu.Lock()
	trig.Fields["dormant_on_escalation"] = "true"
	daemon.mu.Unlock()
	engine.SweepPouringButNeverWorked(ctx)

	// Trigger active again, still stalled: must NOT re-alert (state preserved).
	daemon.mu.Lock()
	delete(trig.Fields, "dormant_on_escalation")
	daemon.mu.Unlock()
	engine.pouringStallState.mu.Lock()
	if st := engine.pouringStallState.byTrigger[trig.ID]; st != nil {
		st.lastWalkAt = time.Time{} // force a fresh walk
	} else {
		engine.pouringStallState.mu.Unlock()
		t.Fatalf("dormant trigger's state was pruned — dedup lost")
	}
	engine.pouringStallState.mu.Unlock()

	engine.SweepPouringButNeverWorked(ctx)
	if rec.count() != 1 {
		t.Fatalf("re-activated still-stalled trigger must not re-alert, got %d", rec.count())
	}
}

// TestPourStall_SkipsDisabled: a disabled trigger is expected to stop pouring
// and its molecules to sit — it must not be flagged.
func TestPourStall_SkipsDisabled(t *testing.T) {
	trig := pourTrigger("gd-off-2", "disabled pour gardener", map[string]string{"enabled": "false"})
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-om-a", "gd-om-b", "gd-om-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-os-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("disabled trigger must not alert, got %d", rec.count())
	}
}

// TestPourStall_SkipsNonApplyFormula: a spawn_agent (non-pour) trigger cannot
// exhibit this failure mode and must be skipped even with stale molecules
// erroneously wired to it.
func TestPourStall_SkipsNonApplyFormula(t *testing.T) {
	trig := pourTrigger("gd-spawn-1", "spawn_agent trigger", map[string]string{"action": "spawn_agent"})
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-sm-a", "gd-sm-b", "gd-sm-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-ss-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("non-apply_formula trigger must not be swept, got %d", rec.count())
	}
}

// TestPourStall_SkipsDormant: a trigger parked dormant_on_escalation is
// legitimately not firing; its molecules waiting is expected.
func TestPourStall_SkipsDormant(t *testing.T) {
	trig := pourTrigger("gd-dorm-2", "dormant pour gardener", map[string]string{"dormant_on_escalation": "true"})
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-dmm-a", "gd-dmm-b", "gd-dmm-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-dms-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("dormant trigger must not alert, got %d", rec.count())
	}
}

// TestPourStall_SkipsBlockedByOpenDecision: a trigger blocked by an open
// decision is paused by the engine; its unworked molecules are expected.
func TestPourStall_SkipsBlockedByOpenDecision(t *testing.T) {
	trig := pourTrigger("gd-blk-1", "blocked pour gardener", map[string]string{"blocked_by": "gd-dec-p1"})
	decision := &beadsapi.BeadDetail{ID: "gd-dec-p1", Type: "decision", Status: "open", Title: "open decision"}
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig, decision)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-bm-a", "gd-bm-b", "gd-bm-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-bs-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("trigger blocked by an open decision must not alert, got %d", rec.count())
	}
}

// TestPourStall_SkipsAtConcurrencyLease: a trigger at its live max_concurrent
// lease is waiting on live drivers — its molecules are expected to be in flight.
func TestPourStall_SkipsAtConcurrencyLease(t *testing.T) {
	trig := pourTrigger("gd-lease-1", "at-lease gardener", map[string]string{"max_concurrent": "1"})
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	// One live agent spawned_by this trigger ⇒ liveSpawnCount==1 == max_concurrent.
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-live-agent", Type: "agent", Status: "in_progress", Title: "live driver",
		Fields: map[string]string{"spawned_by": "trigger:" + trig.ID},
	})
	daemon.mu.Unlock()

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-lm-a", "gd-lm-b", "gd-lm-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-ls-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("trigger at its concurrency lease must not alert, got %d", rec.count())
	}
}

// TestPourStall_SkipsWhenGardenersPaused: an open gardeners-pause gate means the
// whole fleet is deliberately held — the sweep must skip entirely so a pause
// never manufactures a stall.
func TestPourStall_SkipsWhenGardenersPaused(t *testing.T) {
	trig := pourTrigger("gd-pause-1", "paused-fleet gardener", nil)
	pauseGate := &beadsapi.BeadDetail{ID: "gd-gate-1", Type: "gate", Status: "open", Title: gardenersPauseTitle}
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig, pauseGate)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-pm-x", "gd-pm-y", "gd-pm-z"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-ps-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("gardeners-pause must skip the whole sweep, got %d", rec.count())
	}
}

// TestPourStall_ClosedMoleculesIgnored: closed molecules were worked (or
// manually resolved) and must not count toward the inert set.
func TestPourStall_ClosedMoleculesIgnored(t *testing.T) {
	trig := pourTrigger("gd-closed-1", "closed-molecules gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-cm-a", "gd-cm-b", "gd-cm-c"} {
		mol := molBead(molID, old, old)
		mol.Status = "closed" // worked & closed
		step := stepBead("gd-cs-"+string(rune('a'+i)), "closed", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("closed molecules must not count as inert, got %d", rec.count())
	}
}

// TestPourStall_ManyHistoricalClosedMolecules_NoAlert: a healthy long-lived
// trigger accumulates hundreds of CLOSED molecules over its lifetime. Those
// must never be mistaken for a stall — only OPEN inert molecules count. Here a
// trigger has 60 old CLOSED molecules (> the walk cap) plus one freshly-poured
// open one; it must NOT alert (the cap applies to OPEN molecules only, and the
// closed ones are filtered out before the cap).
func TestPourStall_ManyHistoricalClosedMolecules_NoAlert(t *testing.T) {
	trig := pourTrigger("gd-longlived-1", "long-lived healthy gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-30 * 24 * time.Hour) // a month of daily closed pours
	for i := 0; i < 60; i++ {
		molID := "gd-hist-mol-" + strconv.Itoa(i)
		mol := molBead(molID, old, old)
		mol.Status = "closed"
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID)
	}
	// One fresh open molecule being worked right now.
	fresh := now.Add(-5 * time.Minute)
	openMol := molBead("gd-fresh-open", fresh, fresh)
	openStep := stepBead("gd-fresh-open-step", "in_progress", fresh, fresh)
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, openMol, openStep)
	daemon.mu.Unlock()
	wireMolecule(daemon, trig.ID, openMol.ID, openStep.ID)

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 {
		t.Fatalf("a healthy trigger with many historical CLOSED molecules must not alert, got %d", rec.count())
	}
}

// TestPourStall_Disabled_NoOp: when the sweep is not enabled, it is a total
// no-op even with an obviously stalled trigger.
func TestPourStall_Disabled_NoOp(t *testing.T) {
	trig := pourTrigger("gd-noop-1", "would-stall gardener", nil)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{trig}}
	rec := &recordingNotifier{}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:              daemon,
		Notifier:            rec.notify,
		Logger:              slog.Default(),
		PouringStallEnabled: false, // OFF
		Clock:               func() time.Time { return now },
	})
	old := now.Add(-8 * time.Hour)
	mol := molBead("gd-noop-mol", old, old)
	step := stepBead("gd-noop-step", "open", old, old)
	daemon.beads = append(daemon.beads, mol, step)
	wireMolecule(daemon, trig.ID, mol.ID, step.ID)

	engine.SweepPouringButNeverWorked(context.Background())
	if rec.count() != 0 || len(daemon.updatedFields) != 0 {
		t.Fatalf("disabled sweep must be a no-op, alerts=%d updates=%d", rec.count(), len(daemon.updatedFields))
	}
}

// TestPourStall_CreateBeadLeg: with create_bead enabled, the sweep files a bug
// bead parented to the stalled trigger and does NOT notify.
func TestPourStall_CreateBeadLeg(t *testing.T) {
	trig := pourTrigger("gd-cb-1", "create-bead gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, false, true, trig)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-cbm-a", "gd-cbm-b", "gd-cbm-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-cbs-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.SweepPouringButNeverWorked(context.Background())

	if rec.count() != 0 {
		t.Fatalf("notify disabled: expected 0 notifies, got %d", rec.count())
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	found := false
	for _, cb := range daemon.createdBeads {
		if cb.Type == "bug" {
			found = true
		}
	}
	if !found {
		t.Fatalf("create_bead leg should file a bug bead, created=%d", len(daemon.createdBeads))
	}
}

// TestPourStall_FiresViaEvaluate exercises the full in-engine wiring: the sweep
// runs as part of Evaluate() with no sources registered.
func TestPourStall_FiresViaEvaluate(t *testing.T) {
	trig := pourTrigger("gd-eval-1", "evaluate-path gardener", nil)
	engine, daemon, rec, now := pourStallEngine(t, true, false, trig)

	old := now.Add(-8 * time.Hour)
	for i, molID := range []string{"gd-em-a", "gd-em-b", "gd-em-c"} {
		mol := molBead(molID, old, old)
		step := stepBead("gd-es-"+string(rune('a'+i)), "open", old, old)
		daemon.mu.Lock()
		daemon.beads = append(daemon.beads, mol, step)
		daemon.mu.Unlock()
		wireMolecule(daemon, trig.ID, molID, step.ID)
	}

	engine.Evaluate(context.Background())
	if rec.count() != 1 {
		t.Fatalf("Evaluate() should run the pouring-stall sweep once, got %d alerts", rec.count())
	}
}

// stampedField reports whether the mock recorded an UpdateBeadFields call for
// beadID that set field to want.
func stampedField(d *mockTriggerDaemon, beadID, field, want string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, id := range d.updatedIDs {
		if id != beadID {
			continue
		}
		if v, ok := d.updatedFields[i][field]; ok && v == want {
			return true
		}
	}
	return false
}

// stampedFieldValue reports whether the LATEST recorded UpdateBeadFields call
// for beadID that touched field set it to want (the most recent write wins).
func stampedFieldValue(d *mockTriggerDaemon, beadID, field, want string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	last, seen := "", false
	for i, id := range d.updatedIDs {
		if id != beadID {
			continue
		}
		if v, ok := d.updatedFields[i][field]; ok {
			last, seen = v, true
		}
	}
	return seen && last == want
}
