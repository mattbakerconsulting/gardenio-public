package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestHandUpOperatorClass_PutsMergeOnTheFloor pins P10.F4 (gd-QiDj0JvkFr): a
// PR merge the desk names an operator class reaches the owner as a floor item
// (approvers operator alone, operator_only, policy_rule floor:*), so the
// owner's approval is a plain one, and the desk can no longer approve it.
func TestHandUpOperatorClass_PutsMergeOnTheFloor(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-oc1", "gardenio", mergeDeskEscalation("gd-crew1"))
	if c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), esc); !c.Allowed {
		t.Fatalf("positive control: the desk may approve before the hand-up: %+v", c)
	}

	body := map[string]any{"reason": "OPERATOR-ONLY (2): widens the steward approver lane", "operator_class": "2"}
	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleHandUpEscalation, "gd-oc1", "hand-up", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("desk operator-class hand-up: got %d %s", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]any{
		"held_by": "operator", "audience": "owner", "operator_only": true, "operator_class": "2",
		"policy_rule": "floor:operator_class_merge", "execute_by": "coordinator:merge-desk",
		"handed_up_by": "gd-rev1",
	} {
		if got := escField(t, ms, "gd-oc1", k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	esc = ms.beads["gd-oc1"]
	if got, _ := escalationApprovers(esc); !slices.Equal(got, []string{"operator"}) {
		t.Errorf("approvers = %v, want [operator]", got)
	}
	if r := s.escalationForcedReason(context.Background(), esc); r != operatorClassMergeReason || !isEscalationFloorReason(r) {
		t.Errorf("forced reason = %q, want the floor reason %q", r, operatorClassMergeReason)
	}
	if c := s.checkMayApprove(operatorCtx("owner@example.com"), esc); !c.Allowed || c.As != "operator" {
		t.Errorf("owner on the floor merge: %+v, want allowed as operator", c)
	}
	if c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), esc); c.Allowed {
		t.Errorf("the desk approved a merge it put on the floor: %+v", c)
	}
	// A re-route keeps it on the floor and keeps the desk as its executor.
	fields, err := s.escalationRouteFieldsFor(context.Background(), esc)
	if err != nil {
		t.Fatal(err)
	}
	if fields["policy_rule"] != "floor:operator_class_merge" || fields["execute_by"] != "coordinator:merge-desk" || fields["operator_only"] != true {
		t.Errorf("re-route fields = %v, want floor:operator_class_merge executed by the desk", fields)
	}
}

func TestHandUpOperatorClass_Refusals(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-oc2", "gardenio", mergeDeskEscalation("gd-crew1"))
	desk := agentCtx("gd-rev1", "reviewer-a", "gardenio")
	for _, c := range []struct {
		name string
		ctx  context.Context
		body map[string]any
		want int
	}{
		{"unknown class", desk, map[string]any{"reason": "x", "operator_class": "4"}, http.StatusBadRequest},
		{"non-holder", agentCtx("gd-crew1", "crew-a", "gardenio"), map[string]any{"reason": "x", "operator_class": "1"}, http.StatusForbidden},
	} {
		if rec := callEscalationVerb(t, c.ctx, s.handleHandUpEscalation, "gd-oc2", "hand-up", c.body); rec.Code != c.want {
			t.Errorf("%s: got %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if got := escField(t, ms, "gd-oc2", "operator_class"); got != nil || escField(t, ms, "gd-oc2", "held_by") != "coordinator:merge-desk" {
		t.Fatalf("a refused hand-up stamped operator_class=%v or moved held_by", got)
	}

	// Not a merge: the class is the desk's, for PRs only.
	deploy := mergeDeskEscalation("gd-crew1")
	deploy["operation_type"] = "helm_deploy"
	seedRoutedEscalation(ms, "gd-oc3", "gardenio", deploy)
	if rec := callEscalationVerb(t, desk, s.handleHandUpEscalation, "gd-oc3", "hand-up", map[string]any{"reason": "x", "operator_class": "1"}); rec.Code != http.StatusBadRequest {
		t.Errorf("operator class on a helm_deploy: got %d, want 400", rec.Code)
	}

	// Already approved: stamping the floor now would relabel an approval
	// that was not a floor one.
	approved := mergeDeskEscalation("gd-crew1")
	approved["approved_at"], approved["approved_by"] = "2026-11-05T15:32:37Z", "owner@example.com"
	seedRoutedEscalation(ms, "gd-oc4", "gardenio", approved)
	if rec := callEscalationVerb(t, desk, s.handleHandUpEscalation, "gd-oc4", "hand-up", map[string]any{"reason": "x", "operator_class": "2"}); rec.Code != http.StatusConflict {
		t.Errorf("operator class on an approved merge: got %d, want 409", rec.Code)
	}
}

// TestOperatorClass_ServerOnly: only the hand-up verb writes operator_class.
// A filer or holder who could set it at create or by PATCH could not lower
// anything, but one who could remove it would take a merge off the floor.
func TestOperatorClass_ServerOnly(t *testing.T) {
	ctx := agentCtx("gd-crew1", "crew-a", "gardenio")
	f := mergeDeskEscalation("gd-crew1")
	f["operator_class"] = "1"
	raw, _ := json.Marshal(f)
	bead := &model.Bead{Type: model.TypeEscalation, Fields: raw}
	if err := checkEscalationStampedFieldsOnCreate(ctx, bead); err == nil {
		t.Error("an agent filed an escalation carrying operator_class")
	}
	prior, _ := json.Marshal(mergeDeskEscalation("gd-crew1"))
	if err := checkEscalationApprovalFieldsImmutable(ctx, bead, prior, model.StatusOpen); err == nil {
		t.Error("an agent PATCH added operator_class")
	}
	// Removal by an admin's generic PATCH is refused too (frozen).
	if err := checkEscalationFrozenFields(operatorCtx("owner@example.com"), &model.Bead{Type: model.TypeEscalation, Fields: prior}, raw); err == nil {
		t.Error("an admin PATCH removed operator_class")
	}
}
