package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// boundedStopBlob builds a stop JSON blob carrying a hard_stop wall_clock of
// the given duration. An empty duration omits the wall_clock entry entirely.
func boundedStopBlob(t *testing.T, wallClock string) string {
	t.Helper()
	hard := scheduler.StopSlot{}
	if wallClock != "" {
		hard.Any = append(hard.Any, scheduler.StopEntry{
			Kind:     scheduler.StopKindWallClock,
			Duration: wallClock,
		})
	}
	// Always include a non-wall_clock hard_stop entry so the slot is non-empty
	// even when wallClock is "" — proves the guard keys on the wall_clock
	// SPECIFICALLY, not on "is hard_stop populated".
	hard.Any = append(hard.Any, scheduler.StopEntry{
		Kind:  scheduler.StopKindConsecutiveFailures,
		Value: "5",
	})
	spec := scheduler.StopSpec{HardStop: hard, OnLoopEnd: "close"}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshalling stop blob: %v", err)
	}
	return string(raw)
}

func TestValidateGardenerBounds_Bounded(t *testing.T) {
	// run_budget>0 AND a bounded hard_stop wall_clock ⇒ no violation.
	if v := ValidateGardenerBounds(20, boundedStopBlob(t, "168h")); v != nil {
		t.Fatalf("expected nil violation for bounded gardener, got: %s", v.Error())
	}
	// Exactly at the ceiling is allowed.
	if v := ValidateGardenerBounds(1, boundedStopBlob(t, "720h")); v != nil {
		t.Fatalf("expected nil at exactly the ceiling, got: %s", v.Error())
	}
}

func TestValidateGardenerBounds_FailClosed(t *testing.T) {
	cases := []struct {
		name          string
		runBudget     int
		stopRaw       string
		wantBudgetBad bool
		wantWallBad   bool
	}{
		{"missing run_budget", 0, boundedStopBlob(t, "168h"), true, false},
		{"negative run_budget", -1, boundedStopBlob(t, "168h"), true, false},
		{"missing wall_clock entirely", 20, boundedStopBlob(t, ""), false, true},
		{"empty stop blob", 20, "", false, true},
		{"both missing", 0, "", true, true},
		{"unparseable wall_clock duration", 20, boundedStopBlob(t, "forever"), false, true},
		{"zero wall_clock", 20, boundedStopBlob(t, "0s"), false, true},
		{"negative wall_clock", 20, boundedStopBlob(t, "-1h"), false, true},
		{"wall_clock exceeds ceiling", 20, boundedStopBlob(t, "100000h"), false, true},
		{"malformed stop JSON", 20, "{not json", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateGardenerBounds(tc.runBudget, tc.stopRaw)
			if v == nil {
				t.Fatalf("expected a violation, got nil (this is a fail-OPEN bug)")
			}
			if v.MissingRunBudget != tc.wantBudgetBad {
				t.Errorf("MissingRunBudget = %v, want %v", v.MissingRunBudget, tc.wantBudgetBad)
			}
			if v.MissingWallClock != tc.wantWallBad {
				t.Errorf("MissingWallClock = %v, want %v (reason: %q)", v.MissingWallClock, tc.wantWallBad, v.WallClockReason)
			}
			// The error message must name the offending constraint(s) so an
			// operator can act on it.
			msg := v.Error()
			if tc.wantBudgetBad && !strings.Contains(msg, "run_budget") {
				t.Errorf("violation message omits run_budget: %q", msg)
			}
			if tc.wantWallBad && !strings.Contains(msg, "wall_clock") {
				t.Errorf("violation message omits wall_clock: %q", msg)
			}
		})
	}
}

// TestGardenerBoundsViolation_WallClockHintIsHonest guards the message fix for
// ka-0000007024. The old remediation hint hardcoded a bare `168h` example with
// no caveat, which is an instant self-close when applied to a gardener older
// than the wall_clock grandfather epoch (its window measures from the epoch, so
// 168h can already be expired). The rewritten hint must (a) still offer a
// concrete example so the shape is obvious, and (b) warn that a pre-epoch
// gardener measures from the epoch and point at `gb gardener status` for the real
// expiry — so no one follows the hint into a revive that silently reverts.
func TestGardenerBoundsViolation_WallClockHintIsHonest(t *testing.T) {
	// Trigger the wall_clock branch specifically (run_budget is fine).
	v := ValidateGardenerBounds(20, boundedStopBlob(t, ""))
	if v == nil || !v.MissingWallClock {
		t.Fatalf("expected a MissingWallClock violation, got %v", v)
	}
	msg := v.Error()
	for _, want := range []string{
		"168h",               // still shows a concrete example shape
		"grandfather epoch",  // names the floor that makes 168h a footgun
		"EXPIRED",            // warns the window may already be past
		"gb gardener status", // routes to the surface that prints the real expiry
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("wall_clock remediation hint must mention %q — got:\n%s", want, msg)
		}
	}
}

func TestValidateGardenerBounds_LoopEndWallClockDoesNotSatisfy(t *testing.T) {
	// A wall_clock in loop_end (or run_end) is NOT a hard ceiling — only
	// hard_stop counts. A gardener with run_budget>0 and a loop_end wall_clock
	// but NO hard_stop wall_clock must still be rejected.
	spec := scheduler.StopSpec{
		LoopEnd: scheduler.StopSlot{Any: []scheduler.StopEntry{
			{Kind: scheduler.StopKindWallClock, Duration: "168h"},
		}},
		HardStop: scheduler.StopSlot{Any: []scheduler.StopEntry{
			{Kind: scheduler.StopKindConsecutiveFailures, Value: "5"},
		}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	v := ValidateGardenerBounds(20, string(raw))
	if v == nil {
		t.Fatal("expected rejection: loop_end wall_clock is not a hard ceiling")
	}
	if !v.MissingWallClock {
		t.Errorf("expected MissingWallClock=true, got violation: %s", v.Error())
	}
}

func TestValidateGardenerSpec(t *testing.T) {
	// nil spec ⇒ no violation (a nil gardener is not a gardener).
	if v := ValidateGardenerSpec(nil); v != nil {
		t.Errorf("nil spec should yield nil violation, got: %s", v.Error())
	}
	// Bounded spec ⇒ nil.
	bounded := &GardenerSpec{RunBudget: 20, Stop: json.RawMessage(boundedStopBlob(t, "168h"))}
	if v := ValidateGardenerSpec(bounded); v != nil {
		t.Errorf("bounded spec should pass, got: %s", v.Error())
	}
	// Unbounded spec ⇒ violation.
	unbounded := &GardenerSpec{RunBudget: 0}
	if v := ValidateGardenerSpec(unbounded); v == nil {
		t.Error("unbounded spec should be rejected")
	}
}

func TestValidateGardenerBlob(t *testing.T) {
	// Empty blob is unbounded by definition (no loop spec).
	if v := ValidateGardenerBlob(""); v == nil {
		t.Error("empty blob should be rejected (no loop spec)")
	} else if !v.MissingRunBudget || !v.MissingWallClock {
		t.Errorf("empty blob should flag BOTH constraints, got: %s", v.Error())
	}
	// Unparseable blob is fail-closed.
	if v := ValidateGardenerBlob("{not json"); v == nil {
		t.Error("unparseable blob should be rejected")
	}
	// A well-formed bounded gardener blob passes.
	spec := GardenerSpec{RunBudget: 20, Stop: json.RawMessage(boundedStopBlob(t, "168h"))}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if v := ValidateGardenerBlob(string(raw)); v != nil {
		t.Errorf("bounded blob should pass, got: %s", v.Error())
	}
}

// ── firing-time gate: gardenerPreFireCheck ─────────────────────────────────

// boundsGuardDaemon is a self-contained gardenerDaemon stub serving a
// configurable `gardeners-pause` kill-switch gate and recording any writes. It
// implements the full gardenerDaemon method set directly (GetBead,
// ListBeadsFiltered, UpdateBeadFields, AddComment, CloseBead, MRBeadForTask).
// The generalized guard NEVER disables a gardener at firing time (warn-with-grace
// only), so disabled/comments staying empty is itself part of the contract.
type boundsGuardDaemon struct {
	killSwitchOpen bool
	disabled       bool
	comments       []string
}

func (d *boundsGuardDaemon) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (d *boundsGuardDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if d.killSwitchOpen && q.Search == gardenersPauseTitle {
		return &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			{ID: "gd-gate", Title: gardenersPauseTitle},
		}}, nil
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func (d *boundsGuardDaemon) UpdateBeadFields(_ context.Context, _ string, fields map[string]string) error {
	if fields["enabled"] == "false" {
		d.disabled = true
	}
	return nil
}

func (d *boundsGuardDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

func (d *boundsGuardDaemon) AddComment(_ context.Context, _, _, text string) error {
	d.comments = append(d.comments, text)
	return nil
}

func (d *boundsGuardDaemon) AddLabel(_ context.Context, _, _ string) error {
	return nil
}

func (d *boundsGuardDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (d *boundsGuardDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return nil
}

func (d *boundsGuardDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func boundsGardenerTrigger(t *testing.T, runBudget int, wallClock string, labels []string) TriggerDef {
	t.Helper()
	spec := GardenerSpec{
		AssignBead: "gd-epic",
		RunBudget:  runBudget,
		Stop:       json.RawMessage(boundedStopBlob(t, wallClock)),
	}
	return TriggerDef{
		ID:       "gd-cap",
		Project:  "gardenio",
		Labels:   labels,
		Gardener: &spec,
	}
}

// TestGardenerPreFireCheck_UnboundedFiresWithGrace: at firing time an unbounded
// LIVE gardener is NOT blocked or disabled by the bounds guard — it fires
// (warn-with-grace). This is the asymmetric rollout: authoring is fail-closed,
// firing is warn-only so gardeners predating the guard are not knocked offline.
func TestGardenerPreFireCheck_UnboundedFiresWithGrace(t *testing.T) {
	d := &boundsGuardDaemon{}
	trig := boundsGardenerTrigger(t, 0, "168h", []string{"role:gardener", "project:gardenio"})
	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && reason == gardenerSkipPaused {
		t.Fatalf("unbounded gardener wrongly blocked by bounds guard: reason=%q", reason)
	}
	if d.disabled {
		t.Error("firing-time bounds guard must NEVER disable a gardener (warn-with-grace only)")
	}
	if len(d.comments) != 0 {
		t.Errorf("firing-time bounds guard must not comment/disable, got %d comments", len(d.comments))
	}
}

// TestGardenerPreFireCheck_KillSwitch: an open `gardeners-pause` gate halts ANY
// gardener fleet-wide, even a perfectly bounded one — and must NOT disable the
// bead (a temporary halt, released by closing the gate).
func TestGardenerPreFireCheck_KillSwitch(t *testing.T) {
	d := &boundsGuardDaemon{killSwitchOpen: true}
	trig := boundsGardenerTrigger(t, 20, "168h", []string{"role:gardener", "project:gardenio"})
	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if !skip || reason != gardenerSkipPaused {
		t.Fatalf("expected gardeners_paused skip, got skip=%v reason=%q", skip, reason)
	}
	if d.disabled {
		t.Error("kill-switch must not disable the gardener bead")
	}
}

// TestGardenerPreFireCheck_KillSwitchIsLabelAgnostic: the kill-switch applies to
// ALL gardeners regardless of labels/project — there is no dark discriminator.
func TestGardenerPreFireCheck_KillSwitchIsLabelAgnostic(t *testing.T) {
	d := &boundsGuardDaemon{killSwitchOpen: true}
	// A formerly-"dark"-shaped gardener and a plain gardener must behave
	// identically under the global kill-switch.
	for _, labels := range [][]string{
		{"role:gardener", "monorepo-dark"},
		{"role:gardener", "project:gardenio"},
	} {
		trig := boundsGardenerTrigger(t, 20, "168h", labels)
		skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
		if !skip || reason != gardenerSkipPaused {
			t.Fatalf("labels=%v: expected gardeners_paused, got skip=%v reason=%q", labels, skip, reason)
		}
	}
}

// TestGardenerPreFireCheck_BoundedNotPausedFires: a bounded gardener with no
// kill-switch is not blocked by the bounds/kill-switch gate.
func TestGardenerPreFireCheck_BoundedNotPausedFires(t *testing.T) {
	d := &boundsGuardDaemon{}
	trig := boundsGardenerTrigger(t, 20, "168h", []string{"role:gardener", "project:gardenio"})
	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && reason == gardenerSkipPaused {
		t.Fatalf("bounded gardener wrongly paused: reason=%q", reason)
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (d *boundsGuardDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *boundsGuardDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *boundsGuardDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
