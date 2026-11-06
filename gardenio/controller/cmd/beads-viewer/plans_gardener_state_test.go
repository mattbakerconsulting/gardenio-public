package main

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// plans_gardener_state_test.go — step 1 of the plan-gardeners control-room epic
// (ka-0000004792, task ka-0000004720): the data-layer derivation that joins each
// /plans index row to its bound plan gardener and folds the gardener's loop state
// with the bound assistant's liveness into the unified planAgentChip. These
// tests pin the derivation itself (planGardenerLoopState + the planIndexEnrich
// join), NOT the rendering — surfacing the chip on the row is step 2
// (ka-0000007131).

// TestPlanGardenerLoopState is the pure four-state truth table the index chip and
// the /plans/{id} cockpit both read through. It asserts the trigger:schedule
// default-on convention (an enabled field that is absent, not "false", reads
// running) and that a closed gardener surfaces as retired here — distinct from
// the stall signal's gardenerStateOf, which collapses closed to none.
func TestPlanGardenerLoopState(t *testing.T) {
	cases := []struct {
		name     string
		gardener *beadsapi.BeadDetail
		want     planGardenerState
	}{
		{"nil gardener is none", nil, planGardenerStateNone},
		{
			"trigger:schedule with no enabled field defaults to running",
			&beadsapi.BeadDetail{ID: "gd-c", Type: "trigger", Status: "open",
				Labels: []string{"trigger:schedule", "role:gardener"}, Fields: map[string]string{}},
			planGardenerStateRunning,
		},
		{
			"explicit enabled=true is running",
			&beadsapi.BeadDetail{ID: "gd-c", Type: "trigger", Status: "open",
				Labels: []string{"trigger:schedule", "role:gardener"}, Fields: map[string]string{"enabled": "true"}},
			planGardenerStateRunning,
		},
		{
			"enabled=false is paused",
			&beadsapi.BeadDetail{ID: "gd-c", Type: "trigger", Status: "open",
				Labels: []string{"trigger:schedule", "role:gardener"}, Fields: map[string]string{"enabled": "false"}},
			planGardenerStatePaused,
		},
		{
			"closed gardener is retired (not none — the chip surfaces retirement)",
			&beadsapi.BeadDetail{ID: "gd-c", Type: "trigger", Status: "closed",
				Labels: []string{"trigger:schedule", "role:gardener"}, Fields: map[string]string{"enabled": "true"}},
			planGardenerStateRetired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planGardenerLoopState(tc.gardener); got != tc.want {
				t.Errorf("planGardenerLoopState = %q, want %q", got, tc.want)
			}
		})
	}
}

// enrichRow runs the real batch-enrich pass over the seeded index epic and
// returns the single plan row. The fixture (seedPlansFake) serves one plan epic
// on the index route; a test seeds a bound plan gardener / assistant onto it
// (seedPlanGardener / seedAssistantBead) so planIndexEnrich resolves both through
// the real bulk joins (planGardenersByEpic via the trigger list route;
// planAssistantBulk via the role-scoped agent list route).
func enrichRow(t *testing.T, f *fakeDaemon) planRowView {
	t.Helper()
	s := planAssistantTestServer(f)
	rows := s.planIndexEnrich(context.Background(), f.hotEpicsResp.Beads)
	if len(rows) != 1 {
		t.Fatalf("expected exactly one enriched row, got %d", len(rows))
	}
	return rows[0]
}

// TestPlanIndexEnrich_RunningGardenerLiveAssistant proves the join surfaces a
// running gardener's id + state AND folds the live assistant into the compound
// "running · live" chip with a live liveness dot — the happy-path row.
func TestPlanIndexEnrich_RunningGardenerLiveAssistant(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)

	row := enrichRow(t, f)
	if row.GardenerID != "gd-cap1" {
		t.Errorf("GardenerID = %q, want gd-cap1", row.GardenerID)
	}
	if row.GardenerState != planGardenerStateRunning {
		t.Errorf("GardenerState = %q, want running", row.GardenerState)
	}
	if row.AssistantState != string(planAssistantStateLive) {
		t.Fatalf("precondition: AssistantState = %q, want live", row.AssistantState)
	}
	if row.Chip.Label != "running · live" {
		t.Errorf("Chip.Label = %q, want %q", row.Chip.Label, "running · live")
	}
	if row.Chip.Dot != "live" {
		t.Errorf("Chip.Dot = %q, want live", row.Chip.Dot)
	}
	if row.Chip.Tone != "running" {
		t.Errorf("Chip.Tone = %q, want running", row.Chip.Tone)
	}
}

// TestPlanIndexEnrich_PausedGardenerNoAssistant proves a paused gardener surfaces
// as paused with no liveness dot when no assistant is bound — the loop word
// carries the whole chip (a parked/none agent adds nothing).
func TestPlanIndexEnrich_PausedGardenerNoAssistant(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")

	row := enrichRow(t, f)
	if row.GardenerID != "gd-cap1" {
		t.Errorf("GardenerID = %q, want gd-cap1", row.GardenerID)
	}
	if row.GardenerState != planGardenerStatePaused {
		t.Errorf("GardenerState = %q, want paused", row.GardenerState)
	}
	if row.AssistantState != string(planAssistantStateNone) {
		t.Fatalf("precondition: AssistantState = %q, want none", row.AssistantState)
	}
	if row.Chip.Label != "paused" {
		t.Errorf("Chip.Label = %q, want paused", row.Chip.Label)
	}
	if row.Chip.Dot != "" {
		t.Errorf("Chip.Dot = %q, want empty (no active iteration)", row.Chip.Dot)
	}
}

// TestPlanIndexEnrich_FiringModeGlyph proves the index chip carries the gardener's
// firing mode (Task 8): a cron gardener → "cron", a mode-flipped one → "event", so
// the /plans index card renders ⏱/⚡ off the SAME classifier + chip helper the
// cockpit uses — the two surfaces can never disagree.
func TestPlanIndexEnrich_FiringModeGlyph(t *testing.T) {
	t.Run("cron gardener", func(t *testing.T) {
		f := seedPlansFake()
		// The index seed omits a cron field; a real clock-fired plan gardener
		// carries one (the staggered default), so set it to get the cron shape.
		seedPlanGardener(f, "gd-cap1", "gd-plan1", "true").Fields["cron"] = "7,37 * * * *"
		if got := enrichRow(t, f).Chip.FiringMode; got != "cron" {
			t.Errorf("Chip.FiringMode = %q, want cron", got)
		}
	})
	t.Run("flipped event gardener", func(t *testing.T) {
		f := seedPlansFake()
		seedPlanGardenerEvent(f, "gd-cap1", "gd-plan1", "true")
		if got := enrichRow(t, f).Chip.FiringMode; got != "event" {
			t.Errorf("Chip.FiringMode = %q, want event", got)
		}
	})
	t.Run("gardener-less plan has no glyph", func(t *testing.T) {
		f := seedPlansFake()
		if got := enrichRow(t, f).Chip.FiringMode; got != "" {
			t.Errorf("Chip.FiringMode = %q, want empty", got)
		}
	})
}

// TestPlanIndexEnrich_NoGardenerFallsBackToAssistant proves a gardener-less plan
// leaves GardenerID empty and renders the assistant-only chip fallback — the
// legacy/ad-hoc plan shape, matching planAgentChip's gardener-less branch.
func TestPlanIndexEnrich_NoGardenerFallsBackToAssistant(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)

	row := enrichRow(t, f)
	if row.GardenerID != "" {
		t.Errorf("GardenerID = %q, want empty (no gardener bound)", row.GardenerID)
	}
	if row.GardenerState != planGardenerStateNone {
		t.Errorf("GardenerState = %q, want none", row.GardenerState)
	}
	// Gardener-less: the chip reflects the assistant alone (Tone follows the
	// agent, no separate liveness dot).
	if row.Chip.Label != "live" {
		t.Errorf("Chip.Label = %q, want live (assistant-only fallback)", row.Chip.Label)
	}
	if row.Chip.Tone != "live" {
		t.Errorf("Chip.Tone = %q, want live", row.Chip.Tone)
	}
	if row.Chip.Dot != "" {
		t.Errorf("Chip.Dot = %q, want empty (gardener-less plan has no loop dot)", row.Chip.Dot)
	}
}

// TestPlanIndexEnrich_NoGardenerNoAssistant proves the fully-cold plan reads
// none/none — GardenerState none, and the assistant-only fallback chip renders
// the pre-unification "not started" copy.
func TestPlanIndexEnrich_NoGardenerNoAssistant(t *testing.T) {
	f := seedPlansFake()

	row := enrichRow(t, f)
	if row.GardenerID != "" {
		t.Errorf("GardenerID = %q, want empty", row.GardenerID)
	}
	if row.GardenerState != planGardenerStateNone {
		t.Errorf("GardenerState = %q, want none", row.GardenerState)
	}
	if row.Chip.Label != "not started" {
		t.Errorf("Chip.Label = %q, want %q", row.Chip.Label, "not started")
	}
	if row.Chip.Tone != "none" {
		t.Errorf("Chip.Tone = %q, want none", row.Chip.Tone)
	}
}
