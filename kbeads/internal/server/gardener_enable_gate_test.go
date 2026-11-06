package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// --- Unit: gardenerEnableGatedByPlanState mirrors the cockpit gate -------------

func TestGardenerEnableGatedByPlanState(t *testing.T) {
	cases := []struct {
		st    planStateValue
		gated bool
	}{
		{planStateUnset, false},    // legacy / non-plan gardener — ungated
		{planStateDraft, true},     // still authoring — blocked
		{planStateReady, true},     // awaiting Approve & build — blocked
		{planStateBuilding, false}, // approved — enabling permitted
		{planStateComplete, false}, // terminal — enabling permitted (e.g. reopen path)
	}
	for _, tc := range cases {
		if got := gardenerEnableGatedByPlanState(tc.st); got != tc.gated {
			t.Errorf("gardenerEnableGatedByPlanState(%q) = %v, want %v", tc.st, got, tc.gated)
		}
	}
}

// --- Unit: isGardenerBead classifier ------------------------------------------

func TestIsGardenerBead(t *testing.T) {
	cases := []struct {
		name   string
		typ    model.BeadType
		labels []string
		fields string
		want   bool
	}{
		{"plain trigger, no gardener markers", "trigger", []string{"trigger:webhook"}, `{"role":"plan-gardener"}`, false},
		{"retired role:captain label no longer marks a gardener", "trigger", []string{"role:captain"}, `{}`, false},
		{"retired captain blob no longer marks a gardener", "trigger", nil, `{"captain":"{\"assign_bead\":\"gd-x\"}"}`, false},
		{"non-gardener type with role:gardener label", "epic", []string{"role:gardener"}, `{}`, false},
		{"trigger with role:gardener label", "trigger", []string{"role:gardener"}, `{}`, true},
		{"trigger with role=gardener field", "trigger", nil, `{"role":"gardener"}`, true},
		{"trigger with gardener blob", "trigger", nil, `{"gardener":"{\"assign_bead\":\"gd-x\"}"}`, true},
		{"empty gardener blob is not a gardener", "trigger", nil, `{"gardener":"  "}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGardenerBead(tc.typ, tc.labels, json.RawMessage(tc.fields)); got != tc.want {
				t.Errorf("isGardenerBead(%q,%v,%s) = %v, want %v", tc.typ, tc.labels, tc.fields, got, tc.want)
			}
		})
	}
}

// --- Unit: effectiveGardenerEnabled honors asymmetric loader defaults ---------

func TestEffectiveGardenerEnabled(t *testing.T) {
	cases := []struct {
		name   string
		typ    model.BeadType
		fields string
		want   bool
	}{
		// trigger: enabled unless explicitly false (absent ⇒ enabled).
		{"trigger absent ⇒ enabled", "trigger", `{}`, true},
		{"trigger enabled=true bool", "trigger", `{"enabled":true}`, true},
		{"trigger enabled=false bool", "trigger", `{"enabled":false}`, false},
		{"trigger enabled=\"true\" string", "trigger", `{"enabled":"true"}`, true},
		{"trigger enabled=\"false\" string", "trigger", `{"enabled":"false"}`, false},
		// schedule: enabled iff explicitly true (absent ⇒ disabled).
		{"schedule absent ⇒ disabled", "schedule", `{}`, false},
		{"schedule enabled=true bool", "schedule", `{"enabled":true}`, true},
		{"schedule enabled=false bool", "schedule", `{"enabled":false}`, false},
		{"schedule enabled=\"true\" string", "schedule", `{"enabled":"true"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveGardenerEnabled(tc.typ, json.RawMessage(tc.fields)); got != tc.want {
				t.Errorf("effectiveGardenerEnabled(%q,%s) = %v, want %v", tc.typ, tc.fields, got, tc.want)
			}
		})
	}
}

// --- Unit: gardenerEpicFromLabels ---------------------------------------------

func TestGardenerEpicFromLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"pinned", []string{"role:gardener", "gardener-epic:gd-plan1", "plan-gardener"}, "gd-plan1"},
		{"none", []string{"role:gardener", "project:gardenio"}, ""},
		{"empty suffix ignored", []string{"gardener-epic:", "gardener-epic:gd-real"}, "gd-real"},
		{"nil labels", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gardenerEpicFromLabels(tc.labels); got != tc.want {
				t.Errorf("gardenerEpicFromLabels(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// --- Integration: enforcement through s.updateBead ---------------------------

// seedGardenerTrigger seeds a plan-gardener trigger bead bound to planID via the
// gardener-epic label, with the given enabled state. Mirrors the shape
// plangardener.Bind mints.
func seedGardenerTrigger(t *testing.T, ms *mockStore, id, planID string, enabled bool) {
	t.Helper()
	fields, _ := json.Marshal(map[string]any{
		"action":  "spawn_agent",
		"role":    "plan-gardener",
		"prompt":  "playbook",
		"enabled": enabled,
	})
	labels := []string{"project:gardenio", "trigger:schedule", roleGardenerLabel, gardenerEpicLabelPrefix + planID, "plan-gardener"}
	cap := &model.Bead{
		ID: id, Title: "Plan gardener for " + planID, Type: "trigger", Kind: model.KindData,
		Status: model.StatusOpen, Labels: labels, Fields: fields,
	}
	if err := ms.CreateBead(context.Background(), cap); err != nil {
		t.Fatalf("seed gardener: %v", err)
	}
	for _, l := range labels {
		_ = ms.AddLabel(context.Background(), id, l)
	}
}

// enableGardenerPatch is the enable write the cockpit performs: a JSON-bool
// enabled=true typed field patch.
func enableGardenerPatch() json.RawMessage {
	return json.RawMessage(`{"enabled":true}`)
}

func TestUpdateBead_GardenerEnable_BlockedOnDraftPlan(t *testing.T) {
	for _, planState := range []string{"draft", "ready"} {
		t.Run(planState, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedPlanEpic(t, ms, "gd-plan-"+planState, planState)
			seedGardenerTrigger(t, ms, "gd-cap-"+planState, "gd-plan-"+planState, false)

			_, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-"+planState, updateBeadInput{Fields: enableGardenerPatch()})
			if err == nil {
				t.Fatalf("enabling a gardener on a %s plan should be rejected, got nil", planState)
			}
			if _, ok := err.(inputError); !ok {
				t.Fatalf("expected inputError (→400 / InvalidArgument), got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), "gd-plan-"+planState) || !strings.Contains(err.Error(), planState) {
				t.Errorf("error should name the plan and its state: %v", err)
			}
			// The rejected enable must NOT persist — the gardener stays disabled.
			if effectiveGardenerEnabled("trigger", ms.beads["gd-cap-"+planState].Fields) {
				t.Errorf("rejected enable must not persist; gardener is enabled")
			}
		})
	}
}

func TestUpdateBead_GardenerEnable_AllowedOnBuildingPlan(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-b", "building")
	seedGardenerTrigger(t, ms, "gd-cap-b", "gd-plan-b", false)

	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-b", updateBeadInput{Fields: enableGardenerPatch()}); err != nil {
		t.Fatalf("enabling a gardener on a building plan should be accepted, got: %v", err)
	}
	if !effectiveGardenerEnabled("trigger", ms.beads["gd-cap-b"].Fields) {
		t.Errorf("legal enable should persist; gardener is not enabled")
	}
}

// Pausing (disable) a gardener is always allowed, even on a draft plan — the
// gate is on the enable direction only.
func TestUpdateBead_GardenerDisable_AlwaysAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-d", "draft")
	seedGardenerTrigger(t, ms, "gd-cap-d", "gd-plan-d", true) // somehow already enabled

	patch := json.RawMessage(`{"enabled":false}`)
	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-d", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("disabling a gardener on a draft plan should always be allowed, got: %v", err)
	}
	if effectiveGardenerEnabled("trigger", ms.beads["gd-cap-d"].Fields) {
		t.Errorf("disable should persist; gardener is still enabled")
	}
}

// An operational write (e.g. last_run) on an ALREADY-enabled gardener bound to a
// draft plan must not re-trip the gate — the effective enabled state is
// unchanged, so it is not an enable transition.
func TestUpdateBead_GardenerOperationalWrite_NotGated(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-op", "draft")
	seedGardenerTrigger(t, ms, "gd-cap-op", "gd-plan-op", true) // already enabled

	patch := json.RawMessage(`{"last_run":"2026-07-23T00:00:00Z"}`)
	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-op", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("operational write on an already-enabled gardener must not be gated, got: %v", err)
	}
}

// A gardener with NO gardener-epic label (a generic epic gardener / non-plan
// trigger) is never gated — enabling it succeeds regardless of any plan state.
func TestUpdateBead_NonPlanGardenerEnable_NotGated(t *testing.T) {
	s, ms, _ := newTestServer()
	fields, _ := json.Marshal(map[string]any{"action": "spawn_agent", "role": "gardener", "prompt": "p", "enabled": false})
	labels := []string{"project:gardenio", "trigger:schedule", roleGardenerLabel}
	cap := &model.Bead{
		ID: "gd-cap-generic", Title: "generic gardener", Type: "trigger", Kind: model.KindData,
		Status: model.StatusOpen, Labels: labels, Fields: fields,
	}
	_ = ms.CreateBead(context.Background(), cap)
	for _, l := range labels {
		_ = ms.AddLabel(context.Background(), "gd-cap-generic", l)
	}

	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-generic", updateBeadInput{Fields: enableGardenerPatch()}); err != nil {
		t.Fatalf("enabling a non-plan gardener should not be gated, got: %v", err)
	}
	if !effectiveGardenerEnabled("trigger", ms.beads["gd-cap-generic"].Fields) {
		t.Errorf("non-plan gardener enable should persist")
	}
}

// A gardener bound to its plan ONLY via gardener-epic: (no gardener-epic: label
// at all) is gated identically to a gardener-epic-bound one (gd-Cizer9ykpz
// dual-read, merge-desk review gd-tuetjHsiUm blocker_2).
func TestUpdateBead_GardenerEnable_BlockedOnDraftPlan_ViaGardenerEpicOnly(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-g", "draft")
	fields, _ := json.Marshal(map[string]any{"action": "spawn_agent", "role": "gardener", "prompt": "p", "enabled": false})
	labels := []string{"project:gardenio", "trigger:schedule", roleGardenerLabel, gardenerEpicLabelPrefix + "gd-plan-g"}
	cap := &model.Bead{
		ID: "gd-cap-g", Title: "gardener-only gardener", Type: "trigger", Kind: model.KindData,
		Status: model.StatusOpen, Labels: labels, Fields: fields,
	}
	_ = ms.CreateBead(context.Background(), cap)
	for _, l := range labels {
		_ = ms.AddLabel(context.Background(), "gd-cap-g", l)
	}

	_, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-g", updateBeadInput{Fields: enableGardenerPatch()})
	if err == nil {
		t.Fatal("enabling a gardener-only gardener on a draft plan should be rejected, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→400 / InvalidArgument), got %T: %v", err, err)
	}
}

// Deleting the `enabled` field (RFC 7386 null) on a trigger gardener flips it to
// effectively-enabled (absent ⇒ enabled for a trigger). The gate compares the
// EFFECTIVE state, so this field-deletion bypass is blocked on a draft plan too.
func TestUpdateBead_GardenerEnable_FieldDeletionBypassBlocked(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-del", "draft")
	seedGardenerTrigger(t, ms, "gd-cap-del", "gd-plan-del", false) // paused: enabled=false

	patch := json.RawMessage(`{"enabled":null}`) // delete the field ⇒ effectively enabled
	_, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-del", updateBeadInput{Fields: patch})
	if err == nil {
		t.Fatal("deleting enabled to re-enable a gardener on a draft plan should be rejected, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError, got %T: %v", err, err)
	}
	// The paused state must be preserved — the deletion did not persist.
	if effectiveGardenerEnabled("trigger", ms.beads["gd-cap-del"].Fields) {
		t.Errorf("rejected field-deletion enable must not persist; gardener is enabled")
	}
}

// A gardener-epic label pointing at a missing plan (dangling) degrades to ALLOW —
// the gate is defense-in-depth and must never wedge a write on a lookup miss.
func TestUpdateBead_GardenerEnable_DanglingPlanAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedGardenerTrigger(t, ms, "gd-cap-dangling", "gd-plan-gone", false) // plan never seeded

	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-cap-dangling", updateBeadInput{Fields: enableGardenerPatch()}); err != nil {
		t.Fatalf("enabling a gardener whose plan is missing should degrade to allow, got: %v", err)
	}
	if !effectiveGardenerEnabled("trigger", ms.beads["gd-cap-dangling"].Fields) {
		t.Errorf("dangling-plan gardener enable should persist")
	}
}

// --- Integration: enforcement through s.createBead ---------------------------

// A gardener born enabled while bound to a draft plan is rejected at create.
func TestCreateBead_GardenerEnable_BornEnabledOnDraftRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-create", "draft")

	fields, _ := json.Marshal(map[string]any{
		"action": "spawn_agent", "project": "gardenio", "role": "plan-gardener", "prompt": "p", "enabled": true,
	})
	_, err := s.createBead(ctxAsPlanAuthor(), createBeadInput{
		Title: "born-enabled gardener", Type: "trigger",
		Labels: []string{"project:gardenio", "trigger:schedule", roleGardenerLabel, gardenerEpicLabelPrefix + "gd-plan-create", "plan-gardener"},
		Fields: json.RawMessage(fields),
	})
	if err == nil {
		t.Fatal("creating an enabled gardener on a draft plan should be rejected, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError, got %T: %v", err, err)
	}
}

// A gardener born PAUSED on a draft plan is fine (the normal Bind path).
func TestCreateBead_GardenerEnable_BornPausedOnDraftAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-create-ok", "draft")

	fields, _ := json.Marshal(map[string]any{
		"action": "spawn_agent", "project": "gardenio", "role": "plan-gardener", "prompt": "p", "enabled": false,
	})
	bead, err := s.createBead(ctxAsPlanAuthor(), createBeadInput{
		Title: "paused gardener", Type: "trigger",
		Labels: []string{"project:gardenio", "trigger:schedule", roleGardenerLabel, gardenerEpicLabelPrefix + "gd-plan-create-ok", "plan-gardener"},
		Fields: json.RawMessage(fields),
	})
	if err != nil {
		t.Fatalf("creating a paused gardener on a draft plan should be allowed, got: %v", err)
	}
	if effectiveGardenerEnabled("trigger", ms.beads[bead.ID].Fields) {
		t.Errorf("gardener should be born paused")
	}
}

// A gardener born enabled on a building plan is fine.
func TestCreateBead_GardenerEnable_BornEnabledOnBuildingAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-create-b", "building")

	fields, _ := json.Marshal(map[string]any{
		"action": "spawn_agent", "project": "gardenio", "role": "plan-gardener", "prompt": "p", "enabled": true,
	})
	if _, err := s.createBead(ctxAsPlanAuthor(), createBeadInput{
		Title: "enabled gardener", Type: "trigger",
		Labels: []string{"project:gardenio", "trigger:schedule", roleGardenerLabel, gardenerEpicLabelPrefix + "gd-plan-create-b", "plan-gardener"},
		Fields: json.RawMessage(fields),
	}); err != nil {
		t.Fatalf("creating an enabled gardener on a building plan should be allowed, got: %v", err)
	}
}
