package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// --- Unit tests: planStateFromFields normalization ---------------------------

func TestPlanStateFromFields_Normalization(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		want   planStateValue
	}{
		{"nil", "", planStateUnset},
		{"empty object", `{}`, planStateUnset},
		{"no plan_state key", `{"phase_order":["gd-a"]}`, planStateUnset},
		{"explicit empty string", `{"plan_state":""}`, planStateUnset},
		{"unknown value", `{"plan_state":"done"}`, planStateUnset},
		{"non-string value", `{"plan_state":3}`, planStateUnset},
		{"malformed json", `{"plan_state":`, planStateUnset},
		{"draft", `{"plan_state":"draft"}`, planStateDraft},
		{"ready", `{"plan_state":"ready"}`, planStateReady},
		{"building", `{"plan_state":"building"}`, planStateBuilding},
		{"complete", `{"plan_state":"complete"}`, planStateComplete},
		{"abandoned", `{"plan_state":"abandoned"}`, planStateAbandoned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.fields != "" {
				raw = json.RawMessage(tc.fields)
			}
			if got := planStateFromFields(raw); got != tc.want {
				t.Errorf("planStateFromFields(%q) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}

// --- Unit tests: validatePlanStateTransition legal edges ---------------------

func fieldsWithState(st string) json.RawMessage {
	if st == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(`{"plan_state":"` + st + `"}`)
}

func TestValidatePlanStateTransition_LegalEdges(t *testing.T) {
	legal := []struct{ from, to string }{
		{"draft", "ready"},        // Mark ready
		{"ready", "draft"},        // Keep editing
		{"ready", "building"},     // Approve & build
		{"building", "complete"},  // Mark complete
		{"complete", "building"},  // Reopen
		{"draft", "abandoned"},    // Abandon a never-built draft (cheap exit)
		{"ready", "abandoned"},    // Abandon a ready plan (cheap exit)
		{"building", "abandoned"}, // Abandon a building plan (cheap exit)
		{"abandoned", "building"}, // Reopen an abandoned plan
	}
	for _, tc := range legal {
		t.Run(tc.from+"→"+tc.to, func(t *testing.T) {
			if err := validatePlanStateTransition(fieldsWithState(tc.from), fieldsWithState(tc.to)); err != nil {
				t.Errorf("transition %s→%s should be legal, got: %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestValidatePlanStateTransition_IllegalJumps(t *testing.T) {
	illegal := []struct{ from, to string }{
		{"draft", "building"},     // skips ready
		{"draft", "complete"},     // skips the whole chain
		{"ready", "complete"},     // skips building
		{"building", "draft"},     // can't drop back to authoring
		{"building", "ready"},     // pause the gardener instead
		{"complete", "draft"},     // reopen goes to building, not draft
		{"complete", "ready"},     // reopen goes to building, not ready
		{"complete", "abandoned"}, // terminal states are never adjacent — reopen first
		{"abandoned", "complete"}, // terminal states are never adjacent — reopen first
		{"abandoned", "draft"},    // reopen goes to building, not draft
		{"abandoned", "ready"},    // reopen goes to building, not ready
	}
	for _, tc := range illegal {
		t.Run(tc.from+"→"+tc.to, func(t *testing.T) {
			err := validatePlanStateTransition(fieldsWithState(tc.from), fieldsWithState(tc.to))
			if err == nil {
				t.Fatalf("transition %s→%s should be rejected, got nil", tc.from, tc.to)
			}
			if !strings.Contains(err.Error(), "plan_state") ||
				!strings.Contains(err.Error(), tc.from) ||
				!strings.Contains(err.Error(), tc.to) {
				t.Errorf("error should name the field and both states: %v", err)
			}
		})
	}
}

func TestValidatePlanStateTransition_SelfTransitionIsNoOp(t *testing.T) {
	for _, st := range []string{"", "draft", "ready", "building", "complete", "abandoned"} {
		if err := validatePlanStateTransition(fieldsWithState(st), fieldsWithState(st)); err != nil {
			t.Errorf("self-transition %q→%q should be a no-op, got: %v", st, st, err)
		}
	}
}

// Backward-compat is load-bearing: an UNSET plan_state is ungated. A legacy or
// hand-marked plan may enter ANY lifecycle state (opt in), and any lifecycle
// state may be cleared back to unset (opt out) — neither is an illegal jump.
func TestValidatePlanStateTransition_UnsetIsUngated(t *testing.T) {
	t.Run("opt-in from unset to any state", func(t *testing.T) {
		for _, to := range []string{"draft", "ready", "building", "complete", "abandoned"} {
			if err := validatePlanStateTransition(fieldsWithState(""), fieldsWithState(to)); err != nil {
				t.Errorf("unset→%s (opt-in) should be legal, got: %v", to, err)
			}
		}
	})
	t.Run("clear-back from any state to unset", func(t *testing.T) {
		for _, from := range []string{"draft", "ready", "building", "complete", "abandoned"} {
			if err := validatePlanStateTransition(fieldsWithState(from), fieldsWithState("")); err != nil {
				t.Errorf("%s→unset (clear) should be legal, got: %v", from, err)
			}
		}
	})
}

// A patch that does not touch plan_state at all (the dominant update case: a
// step reorder, a phase add, an unrelated field write) must never be rejected —
// prior and next carry the same state, so it is a no-op for the guard.
func TestValidatePlanStateTransition_UnrelatedFieldPatch(t *testing.T) {
	prior := json.RawMessage(`{"plan_state":"draft","phase_order":["gd-a"]}`)
	next := json.RawMessage(`{"plan_state":"draft","phase_order":["gd-a","gd-b"]}`)
	if err := validatePlanStateTransition(prior, next); err != nil {
		t.Errorf("unrelated-field patch (plan_state unchanged) should be legal, got: %v", err)
	}
}

// --- Integration tests: enforcement through s.updateBead ---------------------

// ctxAsPlanAuthor returns a context that clears both the ownership gate and the
// type:epic write policy, so an updateBead call surfaces the plan_state guard's
// verdict rather than an authz 403 masking it. Admin role bypasses ownership
// (authorizeOwnership) and satisfies any min_role write policy.
func ctxAsPlanAuthor() context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: defaultTestSubject})
	return WithRoles(ctx, []string{RoleAdmin})
}

func seedPlanEpic(t *testing.T, ms *mockStore, id, state string) {
	t.Helper()
	epic := &model.Bead{
		ID:     id,
		Title:  "Plan epic",
		Type:   model.TypeEpic,
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
		Labels: []string{"project:gardenio", "plan"},
		Fields: fieldsWithState(state),
	}
	if err := ms.CreateBead(context.Background(), epic); err != nil {
		t.Fatalf("seed epic: %v", err)
	}
	for _, l := range epic.Labels {
		_ = ms.AddLabel(context.Background(), id, l)
	}
}

func TestUpdateBead_PlanState_IllegalJumpRejected(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-illegal", "draft")

	patch := json.RawMessage(`{"plan_state":"complete"}`)
	_, err := s.updateBead(ctxAsPlanAuthor(), "gd-plan-illegal", updateBeadInput{Fields: patch})
	if err == nil {
		t.Fatal("draft→complete via updateBead should be rejected, got nil")
	}
	if _, ok := err.(inputError); !ok {
		t.Fatalf("expected inputError (→HTTP 400 / InvalidArgument), got %T: %v", err, err)
	}
	// The illegal write must NOT be persisted — the epic stays draft.
	if got := planStateFromFields(ms.beads["gd-plan-illegal"].Fields); got != planStateDraft {
		t.Errorf("rejected transition must not persist; plan_state = %q, want draft", got)
	}
}

func TestUpdateBead_PlanState_LegalTransitionAccepted(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-legal", "ready")

	patch := json.RawMessage(`{"plan_state":"building"}`)
	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-plan-legal", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("ready→building via updateBead should be accepted, got: %v", err)
	}
	if got := planStateFromFields(ms.beads["gd-plan-legal"].Fields); got != planStateBuilding {
		t.Errorf("legal transition should persist; plan_state = %q, want building", got)
	}
}

// A legacy epic with no plan_state may opt into the lifecycle at any point.
func TestUpdateBead_PlanState_UnsetOptInAccepted(t *testing.T) {
	s, ms, _ := newTestServer()
	seedPlanEpic(t, ms, "gd-plan-legacy", "") // no plan_state

	patch := json.RawMessage(`{"plan_state":"building"}`)
	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-plan-legacy", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("unset→building (legacy opt-in) via updateBead should be accepted, got: %v", err)
	}
	if got := planStateFromFields(ms.beads["gd-plan-legacy"].Fields); got != planStateBuilding {
		t.Errorf("opt-in should persist; plan_state = %q, want building", got)
	}
}

// A non-epic bead that happens to carry a plan_state-shaped field is never
// gated — the guard is scoped to type=epic. (Other types would 400 the
// undeclared field at schema validation anyway; this asserts the guard itself
// does not fire.)
func TestUpdateBead_PlanState_NonEpicNotGated(t *testing.T) {
	s, ms, _ := newTestServer()
	task := &model.Bead{
		ID: "gd-task-1", Title: "t", Type: model.TypeTask, Kind: model.KindIssue,
		Status: model.StatusOpen, Labels: []string{"project:gardenio"},
	}
	_ = ms.CreateBead(context.Background(), task)
	_ = ms.AddLabel(context.Background(), "gd-task-1", "project:gardenio")

	// Patch an ordinary declared field; the plan_state guard must not fire on a task.
	patch := json.RawMessage(`{"spawn_source":"slack-thread"}`)
	if _, err := s.updateBead(ctxAsPlanAuthor(), "gd-task-1", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("task field update should not be plan_state-gated, got: %v", err)
	}
}
