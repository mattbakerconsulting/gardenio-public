package server

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestApproveEscalation_TeamApproversAllowsMember verifies the taints ADR §8
// team-based approver gate (gd-yuG2d2CkIj): an escalation naming
// "team:<team bead id>" in its approvers field can be approved by a member of
// that team, resolved through s.TeamsOf — the type:team membership machinery
// gd-731gBeGgl7 was built alongside but did not yet wire up.
func TestApproveEscalation_TeamApproversAllowsMember(t *testing.T) {
	_, ms, h := newTestServer()

	ms.operatorAliases = map[string]int64{"oidc\x00" + defaultTestSubject: 1}
	seedTeamBead(ms, "gd-team-admins", `{"team":"gc-admins","members":["oidc:`+defaultTestSubject+`"]}`)

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
		"approvers":      []string{"team:gd-team-admins"},
	})
	ms.beads["gd-esc-team-member"] = &model.Bead{
		ID: "gd-esc-team-member", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U999",
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-team-member/approve", map[string]any{})
	requireStatus(t, rec, 200)
}

// TestApproveEscalation_TeamApproversBlocksNonMember is the acceptance-
// criterion negative case for gd-yuG2d2CkIj: a caller who does not belong to
// any of the escalation's approver teams is refused, even though they are
// otherwise a valid OIDC approver (would pass escalationApprover and
// four-eyes).
func TestApproveEscalation_TeamApproversBlocksNonMember(t *testing.T) {
	_, ms, h := newTestServer()

	// The team exists, but the approving identity (defaultTestSubject) is not
	// among its members.
	seedTeamBead(ms, "gd-team-admins", `{"team":"gc-admins","members":["oidc:someone-else@example.com"]}`)

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
		"approvers":      []string{"team:gd-team-admins"},
	})
	ms.beads["gd-esc-team-nonmember"] = &model.Bead{
		ID: "gd-esc-team-nonmember", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U999",
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-team-nonmember/approve", map[string]any{})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-esc-team-nonmember"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected escalation to remain open after team-approver refusal, got %s", bead.Status)
	}
}

// TestApproveEscalation_TeamApproversNotEnforcedWithoutField verifies the
// team-approver gate is opt-in per escalation, like requires_four_eyes: an
// escalation with no approvers field set is unaffected, so every escalation
// that predates this field keeps approving exactly as before.
func TestApproveEscalation_TeamApproversNotEnforcedWithoutField(t *testing.T) {
	_, ms, h := newTestServer()

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
	})
	ms.beads["gd-esc-team-off"] = &model.Bead{
		ID: "gd-esc-team-off", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U999",
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-team-off/approve", map[string]any{})
	requireStatus(t, rec, 200)
}

// TestApproveEscalation_TeamApproversStillEnforcesFourEyes confirms the two
// checks compose: a team member who is ALSO the escalation's own requester is
// still blocked by four-eyes, so adding an approvers list never weakens the
// existing "four-eyes is evaluated on operators" guarantee (gd-731gBeGgl7).
func TestApproveEscalation_TeamApproversStillEnforcesFourEyes(t *testing.T) {
	_, ms, h := newTestServer()

	ms.operatorAliases = map[string]int64{
		"slack\x00U123":                 1,
		"oidc\x00" + defaultTestSubject: 1,
	}
	seedTeamBead(ms, "gd-team-admins", `{"team":"gc-admins","members":["oidc:`+defaultTestSubject+`"]}`)

	fields, _ := json.Marshal(map[string]any{
		"operation_type":     "test",
		"operation_spec":     map[string]any{},
		"requires_four_eyes": true,
		"approvers":          []string{"team:gd-team-admins"},
	})
	ms.beads["gd-esc-team-4eyes"] = &model.Bead{
		ID: "gd-esc-team-4eyes", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U123",
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-team-4eyes/approve", map[string]any{})
	requireStatus(t, rec, 403)
}

// TestApproveEscalation_TeamApproversRefusesOperatedByRelay is the
// regression test for the PR #147 round-3 send-back: the team-approver gate
// must not be satisfiable through the static/fleet-token X-Operated-By
// relay that escalationApprover itself honors for approved_by. A caller
// authenticated with only the shared primary static token — no OIDC/edge
// identity of its own — cannot spoof team membership by naming a real
// member's email in X-Operated-By, even though that same header is enough
// to satisfy escalationApprover's approved_by derivation (the beads-viewer
// production path, TestApproveEscalation_StaticTokenWithOperatedByHuman).
// Before the fix, TeamsOf resolved teams via the same relay TeamsOfDirect
// now refuses, so this request incorrectly returned 200.
func TestApproveEscalation_TeamApproversRefusesOperatedByRelay(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))

	const operator = "oidc:matt@example.com"
	ms.operatorAliases = map[string]int64{"oidc\x00matt@example.com": 1}
	seedTeamBead(ms, "gd-team-admins", `{"team":"gc-admins","members":["`+operator+`"]}`)

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
		"approvers":      []string{"team:gd-team-admins"},
	})
	ms.beads["gd-esc-team-relay-spoof"] = &model.Bead{
		ID: "gd-esc-team-relay-spoof", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U999",
	}

	rec := doStaticTokenWithOperator(t, h, "POST", "/v1/escalations/gd-esc-team-relay-spoof/approve",
		operator, map[string]any{})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-esc-team-relay-spoof"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected escalation to remain open after relay-spoofed team-approver refusal, got %s", bead.Status)
	}
}

// TestApproveEscalation_MalformedApproversDenies is the fail-closed
// regression for escalationApprovers: an approvers value that is present but
// not a []string (here, a bare string instead of an array) must deny
// approval rather than being silently swallowed into "no approvers set" and
// letting the gate be skipped.
func TestApproveEscalation_MalformedApproversDenies(t *testing.T) {
	_, ms, h := newTestServer()

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
		"approvers":      "team:gd-team-admins",
	})
	ms.beads["gd-esc-team-malformed"] = &model.Bead{
		ID: "gd-esc-team-malformed", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U999",
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-team-malformed/approve", map[string]any{})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-esc-team-malformed"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected escalation to remain open after malformed-approvers refusal, got %s", bead.Status)
	}
}
