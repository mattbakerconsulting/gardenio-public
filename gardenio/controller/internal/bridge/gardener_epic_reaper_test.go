package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// reaperEngine wires a TriggerEngine with the gardener-epic-reaper sweep enabled,
// a recording notifier, and a MUTABLE clock (via the returned pointer) so the
// once-per-hour throttle can be exercised deterministically. Mirrors drainEngine.
func reaperEngine(t *testing.T, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier, *time.Time) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	clk := &now
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                  daemon,
		Notifier:                rec.notify,
		Logger:                  slog.Default(),
		ClosedEpicReaperEnabled: true,
		ClosedEpicReaperNotify:  true,
		Clock:                   func() time.Time { return *clk },
	})
	return engine, daemon, rec, clk
}

// openGardener builds an OPEN gardener trigger fixture pinned to an epic via the
// gardener-epic:<id> label, with the given enabled flag and terminal_goal prose.
// A non-empty `gardener` blob is what makes isGardenerTrigger true.
func openGardener(id, title, epicID string, enabled bool, terminalGoal string, labels ...string) *beadsapi.BeadDetail {
	blob := `{"name":"` + title + `"`
	if terminalGoal != "" {
		blob += `,"terminal_goal":` + quoteJSON(terminalGoal)
	}
	blob += `}`
	en := "true"
	if !enabled {
		en = "false"
	}
	allLabels := append([]string{"gardener-epic:" + epicID}, labels...)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "open",
		Title:  title,
		Labels: allLabels,
		Fields: map[string]string{
			"gardener": blob,
			"role":     "gardener",
			"enabled":  en,
			"project":  "gardenio",
		},
	}
}

// quoteJSON renders s as a JSON string literal (handles quotes/backslashes) so a
// terminal_goal with punctuation embeds cleanly in the gardener blob.
func quoteJSON(s string) string {
	var b []byte
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		default:
			b = append(b, string(r)...)
		}
	}
	b = append(b, '"')
	return string(b)
}

// reaperEpic builds an epic bead of the given status.
func reaperEpic(id, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "epic", Status: status, Fields: map[string]string{}}
}

// reaperWasClosed reports whether beadID received a CloseBead call.
func reaperWasClosed(m *mockTriggerDaemon, beadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.closedIDs {
		if id == beadID {
			return true
		}
	}
	return false
}

// reaperWasCommented reports whether beadID received an AddComment call.
func reaperWasCommented(m *mockTriggerDaemon, beadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.commentedIDs {
		if id == beadID {
			return true
		}
	}
	return false
}

// ── Category C: enabled-but-inert on a CLOSED epic ─────────────────────────

func TestGardenerEpicReaper_ClosesEnabledGardenerOnClosedEpic(t *testing.T) {
	cap := openGardener("gd-cap-c", "inert gardener", "gd-epic-closed", true, "drain the epic")
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, rec, _ := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if !reaperWasClosed(daemon, "gd-cap-c") {
		t.Fatalf("expected enabled gardener on a CLOSED epic to be reaped (CloseBead)")
	}
	if !reaperWasCommented(daemon, "gd-cap-c") {
		t.Errorf("expected an audit comment on the reaped gardener")
	}
	// Disarmed (enabled=false) so it does not re-enter the closed+enabled pool.
	if !drainDisarmWasTypedBool(daemon, "gd-cap-c") {
		t.Errorf("expected the reaped gardener to be disarmed (typed enabled=false)")
	}
	// NEVER spawns.
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("reaper must not spawn agents, spawned %d", len(daemon.spawnedNames))
	}
	if rec.count() == 0 {
		t.Errorf("expected a notify summary for a run that reaped")
	}
}

// ── Category A: dead (disabled) on a CLOSED epic ───────────────────────────

func TestGardenerEpicReaper_ClosesDisabledGardenerOnClosedEpic(t *testing.T) {
	cap := openGardener("gd-cap-a", "dead gardener", "gd-epic-closed", false, "drain the epic")
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, _, _ := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if !reaperWasClosed(daemon, "gd-cap-a") {
		t.Fatalf("expected disabled gardener on a CLOSED epic to be reaped (category A)")
	}
}

// ── Open-epic lane: reviver's territory, left alone ────────────────────────

func TestGardenerEpicReaper_SkipsOpenEpicLane(t *testing.T) {
	cap := openGardener("gd-cap-open", "live lane", "gd-epic-open", true, "drain the epic")
	epic := reaperEpic("gd-epic-open", "open")
	engine, daemon, _, _ := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-open") {
		t.Fatalf("gardener on an OPEN epic must NOT be reaped (reviver's uncovered-lane territory)")
	}
}

// ── Standing safety: machine marker AND terminal_goal prose ────────────────

func TestGardenerEpicReaper_SkipsStandingGardenerByMachineMarker(t *testing.T) {
	// perpetual:true label is a machine marker → gardenerIsStanding true → skip.
	cap := openGardener("gd-cap-standing", "perpetual guard", "gd-epic-closed", true, "drive main to golden", "perpetual:true")
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, _, _ := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-standing") {
		t.Fatalf("standing gardener (perpetual:true marker) must NEVER be reaped, even on a closed epic (ka-0000006241)")
	}
}

func TestGardenerEpicReaper_SkipsProseDeclaredStandingWithoutMarker(t *testing.T) {
	// The live ka-0000001526 trap: STANDING declared in terminal_goal prose,
	// NO machine marker. gardenerIsStanding is false, so only the prose check
	// saves it from being wrongly reaped.
	goal := "STANDING monthly measure-only guard against CI-minute spend. Do NOT stale-stop for the closed parent epic: operator revived this standing guard."
	cap := openGardener("gd-cap-prose", "measure-only guard", "gd-epic-closed", true, goal)
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, _, _ := reaperEngine(t, cap, epic)

	// Sanity: this gardener is NOT standing by machine marker (the exact trap).
	trig := ParseTriggerDef(cap, "test")
	if gardenerIsStanding(trig) {
		t.Fatalf("test fixture invalid: prose-standing gardener must NOT carry a machine marker")
	}

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-prose") {
		t.Fatalf("prose-declared standing gardener (no marker) must NOT be reaped — repeats ka-0000006241")
	}
}

// ── Non-gardener / manual / fixture guards ──────────────────────────────────

func TestGardenerEpicReaper_SkipsNonGardenerTrigger(t *testing.T) {
	// A plain source-only trigger (no gardener blob) has no epic-coupled
	// lifecycle even if it happens to carry a gardener-epic label.
	plain := &beadsapi.BeadDetail{
		ID: "gd-plain", Type: "trigger", Status: "open", Title: "plain trigger",
		Labels: []string{"gardener-epic:gd-epic-closed"},
		Fields: map[string]string{"action": "notify", "enabled": "true"},
	}
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, _, _ := reaperEngine(t, plain, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-plain") {
		t.Fatalf("non-gardener trigger must NOT be reaped")
	}
}

func TestGardenerEpicReaper_SkipsManualTrigger(t *testing.T) {
	cap := openGardener("gd-cap-manual", "manual gardener", "gd-epic-closed", true, "drain")
	cap.Fields["trigger_mode"] = "manual"
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, _, _ := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-manual") {
		t.Fatalf("manual trigger must NOT be auto-reaped")
	}
}

func TestGardenerEpicReaper_SkipsE2EFixture(t *testing.T) {
	cap := openGardener("gd-cap-e2e", "e2e fixture gardener", "gd-epic-closed", true, "drain", e2eFixtureLabelPrefix+"smoke")
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, _, _ := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-e2e") {
		t.Fatalf("E2E fixture must NOT be reaped")
	}
}

// ── Missing / unresolvable epic: conservatively skipped ────────────────────

func TestGardenerEpicReaper_MissingEpicIsConservativelySkipped(t *testing.T) {
	cap := openGardener("gd-cap-orphan", "orphan anchor", "gd-epic-gone", true, "drain")
	engine, daemon, _, _ := reaperEngine(t, cap) // epic absent

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-orphan") {
		t.Fatalf("gardener with an unresolvable anchor epic must NOT be reaped")
	}
}

func TestGardenerEpicReaper_NoPinnedEpicIsSkipped(t *testing.T) {
	// A gardener with no gardener-epic label and no assign_bead has no frontier
	// epic — nothing to reap against.
	cap := &beadsapi.BeadDetail{
		ID: "gd-cap-nopin", Type: "trigger", Status: "open", Title: "unpinned gardener",
		Fields: map[string]string{"gardener": `{"name":"x"}`, "role": "gardener", "enabled": "true"},
	}
	engine, daemon, _, _ := reaperEngine(t, cap)

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-nopin") {
		t.Fatalf("gardener with no pinned epic must NOT be reaped")
	}
}

// ── assign_bead pinning path ───────────────────────────────────────────────

func TestGardenerEpicReaper_ResolvesEpicViaAssignBead(t *testing.T) {
	// A legacy gardener pins its epic via the assign_bead field, not a label.
	cap := &beadsapi.BeadDetail{
		ID: "gd-cap-assign", Type: "trigger", Status: "open", Title: "assign-bead gardener",
		Fields: map[string]string{
			"gardener": `{"name":"x","assign_bead":"gd-epic-assign"}`,
			"role":     "gardener", "enabled": "true", "project": "gardenio",
		},
	}
	epic := reaperEpic("gd-epic-assign", "closed")
	engine, daemon, _, _ := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())

	if !reaperWasClosed(daemon, "gd-cap-assign") {
		t.Fatalf("expected gardener pinned via assign_bead to a CLOSED epic to be reaped")
	}
}

// ── Per-run cap ─────────────────────────────────────────────────────────────

func TestGardenerEpicReaper_RespectsPerRunCap(t *testing.T) {
	beads := []*beadsapi.BeadDetail{reaperEpic("gd-epic-cap", "closed")}
	total := closedEpicReaperCap + 5
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < total; i++ {
		c := openGardener("gd-capn-"+itoa(i), "retired", "gd-epic-cap", true, "drain")
		// Distinct created_at so the oldest-first ordering is deterministic.
		c.CreatedAt = base.Add(time.Duration(i) * time.Hour)
		beads = append(beads, c)
	}
	engine, daemon, _, _ := reaperEngine(t, beads...)

	engine.SweepClosedEpicGardeners(context.Background())

	closed := 0
	for i := 0; i < total; i++ {
		if reaperWasClosed(daemon, "gd-capn-"+itoa(i)) {
			closed++
		}
	}
	if closed != closedEpicReaperCap {
		t.Fatalf("expected exactly the per-run cap %d reaped, got %d", closedEpicReaperCap, closed)
	}
}

// ── Throttle ────────────────────────────────────────────────────────────────

func TestGardenerEpicReaper_ThrottledToOncePerHour(t *testing.T) {
	cap := openGardener("gd-cap-throttle", "inert", "gd-epic-closed", true, "drain")
	epic := reaperEpic("gd-epic-closed", "closed")
	engine, daemon, _, clk := reaperEngine(t, cap, epic)

	engine.SweepClosedEpicGardeners(context.Background())
	if !reaperWasClosed(daemon, "gd-cap-throttle") {
		t.Fatalf("first run should reap")
	}
	// A fresh (reopened) candidate 30 minutes later must be skipped by the throttle.
	cap2 := openGardener("gd-cap-throttle-2", "inert2", "gd-epic-closed", true, "drain")
	daemon.beads = append(daemon.beads, cap2)
	*clk = clk.Add(30 * time.Minute)
	engine.SweepClosedEpicGardeners(context.Background())
	if reaperWasClosed(daemon, "gd-cap-throttle-2") {
		t.Fatalf("second run within the throttle window must be a no-op")
	}
	// Past the throttle, the sweep runs again.
	*clk = clk.Add(31 * time.Minute)
	engine.SweepClosedEpicGardeners(context.Background())
	if !reaperWasClosed(daemon, "gd-cap-throttle-2") {
		t.Fatalf("third run past the throttle window should reap the new candidate")
	}
}

// ── Config gate ─────────────────────────────────────────────────────────────

func TestGardenerEpicReaper_DisabledIsNoOp(t *testing.T) {
	cap := openGardener("gd-cap-off", "inert", "gd-epic-closed", true, "drain")
	epic := reaperEpic("gd-epic-closed", "closed")
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{cap, epic}}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                  daemon,
		Logger:                  slog.Default(),
		ClosedEpicReaperEnabled: false, // default-off
	})

	engine.SweepClosedEpicGardeners(context.Background())

	if reaperWasClosed(daemon, "gd-cap-off") {
		t.Fatalf("disabled reaper must be a complete no-op")
	}
}
