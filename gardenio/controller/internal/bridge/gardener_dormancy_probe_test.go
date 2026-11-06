package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// dormancyProbeTrigger builds a type=trigger BeadDetail carrying the given
// gardener spec, scoped to project monorepo (so the actionable-count bead_query,
// which is project-scoped, sees children labeled project:monorepo). This mirrors
// how ParseTriggerDef reads the `gardener` / `project` fields off the bead.
func dormancyProbeTrigger(t *testing.T, spec GardenerSpec) *beadsapi.BeadDetail {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal gardener spec: %v", err)
	}
	return &beadsapi.BeadDetail{
		ID:     "gd-cap-1",
		Type:   "trigger",
		Status: "open",
		Title:  "test gardener",
		Fields: map[string]string{
			"gardener": string(raw),
			"project":  "monorepo",
			"source":   "schedule",
			"cron":     "27,57 * * * *",
		},
	}
}

// wedgedFrontierDaemon seeds a mock daemon with an OPEN epic whose single open
// child is blocked_by an OPEN escalation — a fully operator-wedged frontier
// (actionable-count 0). The child carries project:monorepo so it passes the
// project-scoped actionable-count query before being subtracted by the
// exclude_blocked_by_open_types filter.
func wedgedFrontierDaemon(escID string) *mockTriggerDaemon {
	m := &mockTriggerDaemon{}
	m.beads = append(m.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-child-1", Type: "task", Status: "open", Title: "blocked child", Labels: []string{"project:monorepo"}},
		&beadsapi.BeadDetail{ID: escID, Type: "escalation", Status: "open", Title: "operator: merge MRs"},
	)
	m.parentOf = map[string]string{"gd-child-1": "gd-epic-1", escID: "gd-epic-1"}
	m.addedDeps = []triggerMockDep{{BeadID: "gd-child-1", DependsOnID: escID, DepType: "blocks"}}
	return m
}

// TestReconcileDormancyProbe_OperatorWedge_ParkedZeroActionable is the core
// regression for bug ka-0000007872: a gardener whose entire open frontier is
// single-threaded behind an open operator escalation (actionable-count 0) must
// probe as PARKED — with the LIVE blocker id and a computed actionable count of
// 0 — so `gb trigger reconcile` reads its by-design non-fires as parks, not the
// 336 phantom missed fires the bug reported.
func TestReconcileDormancyProbe_OperatorWedge_ParkedZeroActionable(t *testing.T) {
	daemon := wedgedFrontierDaemon("gd-esc-1")
	bead := dormancyProbeTrigger(t, GardenerSpec{AssignBead: "gd-epic-1"})

	d := ReconcileDormancyProbe(context.Background(), daemon, nil, bead)
	if d == nil {
		t.Fatal("expected a dormancy verdict for a gardener trigger, got nil")
	}
	if !d.Parked {
		t.Errorf("fully-wedged frontier must probe PARKED, got Parked=false (%+v)", d)
	}
	if d.Kind != gardenerDormantKindOperatorWedge {
		t.Errorf("kind: want %q, got %q", gardenerDormantKindOperatorWedge, d.Kind)
	}
	if d.LiveBlocker != "gd-esc-1" {
		t.Errorf("live_blocker: want gd-esc-1 (recomputed live), got %q", d.LiveBlocker)
	}
	if d.ActionableChildren != 0 {
		t.Errorf("actionable_children: want 0 (the wedge), got %d", d.ActionableChildren)
	}
}

// TestReconcileDormancyProbe_ActionableChild_NotParked is the "break it" test the
// acceptance criteria demand: arm a trigger dormant (DormantOnEscalation=true)
// but give its frontier an ACTIONABLE (un-blocked) open child. The probe must
// recompute the LIVE predicate and report NOT parked — so reconcile still
// surfaces real drift — never trusting the stale DormantOnEscalation flag.
func TestReconcileDormancyProbe_ActionableChild_NotParked(t *testing.T) {
	daemon := wedgedFrontierDaemon("gd-esc-1")
	// Add a second, un-blocked open child ⇒ actionable-count 1 ⇒ not wedged.
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-child-2", Type: "task", Status: "open", Title: "actionable", Labels: []string{"project:monorepo"}},
	)
	daemon.parentOf["gd-child-2"] = "gd-epic-1"

	// The stamped fields LIE: they say dormant behind gd-esc-1, but the frontier
	// is actionable now.
	bead := dormancyProbeTrigger(t, GardenerSpec{
		AssignBead:          "gd-epic-1",
		DormantOnEscalation: true,
		DormantBlocker:      "gd-esc-1",
		DormantBlockerKind:  gardenerDormantKindOperatorWedge,
	})

	d := ReconcileDormancyProbe(context.Background(), daemon, nil, bead)
	if d == nil {
		t.Fatal("expected a dormancy verdict, got nil")
	}
	if d.Parked {
		t.Errorf("an actionable frontier must probe NOT parked even with dormant_on_escalation=true (the field lies), got Parked=true (%+v)", d)
	}
	if !d.DormantDeclared {
		t.Error("dormant_declared must echo the stamped flag (true) so the reader sees the declared-vs-live divergence")
	}
	if d.ActionableChildren != 1 {
		t.Errorf("actionable_children: want 1 (the un-blocked child — genuine drift signal survives), got %d", d.ActionableChildren)
	}
}

// TestReconcileDormancyProbe_StaleStampedBlocker proves ASK 2: when a live wedge
// exists on a blocker DIFFERENT from the persisted write-once dormant_blocker,
// the probe flags StampedBlockerStale and surfaces the LIVE blocker as
// authoritative. The stamped field pointed at a superseded escalation; the wedge
// has since moved to a new one.
func TestReconcileDormancyProbe_StaleStampedBlocker(t *testing.T) {
	// Live wedge is behind gd-esc-NEW…
	daemon := wedgedFrontierDaemon("gd-esc-NEW")
	// …but the gardener was first parked behind gd-esc-OLD (now closed/superseded).
	bead := dormancyProbeTrigger(t, GardenerSpec{
		AssignBead:          "gd-epic-1",
		DormantOnEscalation: true,
		DormantBlocker:      "gd-esc-OLD",
		DormantBlockerKind:  gardenerDormantKindOperatorWedge,
	})

	d := ReconcileDormancyProbe(context.Background(), daemon, nil, bead)
	if d == nil {
		t.Fatal("expected a dormancy verdict, got nil")
	}
	if !d.Parked {
		t.Fatalf("still wedged (on a new blocker) — must probe PARKED, got %+v", d)
	}
	if d.LiveBlocker != "gd-esc-NEW" {
		t.Errorf("live_blocker: want gd-esc-NEW (recomputed), got %q", d.LiveBlocker)
	}
	if d.StampedBlocker != "gd-esc-OLD" {
		t.Errorf("stamped_blocker: want gd-esc-OLD (echoed write-once field), got %q", d.StampedBlocker)
	}
	if !d.StampedBlockerStale {
		t.Error("stamped_blocker_stale must be TRUE: the live wedge is on a bead different from the stamped one")
	}
}

// TestReconcileDormancyProbe_NonGardener_ReturnsNil: a plain (non-gardener) trigger
// bead has no gardener block, so dormancy is inapplicable and the probe returns
// nil — the caller then leaves its drift chain untouched.
func TestReconcileDormancyProbe_NonGardener_ReturnsNil(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-trig-1",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"cron": "*/5 * * * *", "project": "monorepo"},
	}
	if d := ReconcileDormancyProbe(context.Background(), &mockTriggerDaemon{}, nil, bead); d != nil {
		t.Errorf("non-gardener trigger must return nil dormancy, got %+v", d)
	}
}

// TestReconcileDormancyProbe_NoOpenBlocker_NotParked: an idle gardener with an
// empty frontier (no open escalation/decision) is NOT operator-wedged — "idle"
// is not "parked". Guards against over-parking a merely-quiet gardener, which
// would launder a genuinely dark lane into a false PARKED.
func TestReconcileDormancyProbe_NoOpenBlocker_NotParked(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
	)
	daemon.parentOf = map[string]string{} // opt into subtree filtering (empty subtree)
	bead := dormancyProbeTrigger(t, GardenerSpec{AssignBead: "gd-epic-1"})

	d := ReconcileDormancyProbe(context.Background(), daemon, nil, bead)
	if d == nil {
		t.Fatal("expected a dormancy verdict, got nil")
	}
	if d.Parked {
		t.Errorf("idle gardener (no open blocker) must NOT probe parked, got Parked=true (%+v)", d)
	}
}

// TestReconcileDormancyProbe_NilDaemon_EvaluatedFalse: with no daemon client the
// probe cannot run — it returns Evaluated=false and Parked=false so the caller's
// drift residual stands (fail-safe toward reporting drift, never masking it).
func TestReconcileDormancyProbe_NilDaemon_EvaluatedFalse(t *testing.T) {
	bead := dormancyProbeTrigger(t, GardenerSpec{AssignBead: "gd-epic-1"})
	d := ReconcileDormancyProbe(context.Background(), nil, nil, bead)
	if d == nil {
		t.Fatal("expected a (non-nil) verdict even with nil daemon so the caller can render it, got nil")
	}
	if d.Evaluated {
		t.Error("evaluated must be FALSE with a nil daemon")
	}
	if d.Parked {
		t.Error("parked must be FALSE with a nil daemon (fail-safe toward reporting drift)")
	}
}

// resumeSpentProbeTrigger is a gardener parked as resume_budget_spent on the
// actionable plan of actionableFrontierDaemon, stamped with the plan's digest
// as it reads now.
func resumeSpentProbeTrigger(t *testing.T, daemon *mockTriggerDaemon) *beadsapi.BeadDetail {
	t.Helper()
	digest, err := gardenerPlanActivityDigest(context.Background(), daemon, "gd-epic-1")
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return dormancyProbeTrigger(t, GardenerSpec{
		AssignBead:            "gd-epic-1",
		DormantOnEscalation:   true,
		DormantBlocker:        "gd-epic-1",
		DormantBlockerKind:    gardenerDormantKindResumeBudgetSpent,
		DormantActivityDigest: digest,
	})
}

// TestReconcileDormancyProbe_ResumeBudgetSpent: a gardener parked on a quiet
// plan after spending its resume budget probes PARKED although its plan has
// actionable children (gd-dNxyjFLGRJ); once the plan changes, or the plan
// cannot be read, it probes not parked.
func TestReconcileDormancyProbe_ResumeBudgetSpent(t *testing.T) {
	daemon := actionableFrontierDaemon()
	bead := resumeSpentProbeTrigger(t, daemon)

	d := ReconcileDormancyProbe(context.Background(), daemon, nil, bead)
	if !d.Parked || d.Kind != gardenerDormantKindResumeBudgetSpent || d.LiveBlocker != "gd-epic-1" {
		t.Errorf("quiet plan: want parked kind %q on gd-epic-1, got %+v", gardenerDormantKindResumeBudgetSpent, d)
	}
	if d.ActionableChildren <= 0 {
		t.Errorf("fixture: want actionable children, got %d", d.ActionableChildren)
	}

	daemon.listBeadsErrForParent = map[string]error{"gd-epic-1": errors.New("simulated read failure")}
	if d := ReconcileDormancyProbe(context.Background(), daemon, nil, bead); d.Parked {
		t.Errorf("read failure: probe must fail toward not parked, got %+v", d)
	}
	daemon.listBeadsErrForParent = nil

	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{ID: "gd-child-3", Type: "task", Status: "open", Title: "new step"})
	daemon.parentOf["gd-child-3"] = "gd-epic-1"
	if d := ReconcileDormancyProbe(context.Background(), daemon, nil, bead); d.Parked {
		t.Errorf("plan changed: want not parked, got %+v", d)
	}
}
