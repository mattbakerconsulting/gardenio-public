package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func operatorCtx(email string) context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: email, ClientID: testBrowserClient})
	return WithRoles(withAuthMethod(ctx, authMethodOIDC), []string{RoleAdmin})
}

func humanCtx(email string, roles []string, groups ...string) context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: email, ClientID: testBrowserClient})
	return WithGroups(WithRoles(withAuthMethod(ctx, authMethodOIDC), roles), groups)
}

// agentCtx is an agent on its own per-agent token, project-bound like the real ones.
func agentCtx(beadID, name, project string) context.Context {
	return WithTokenProject(agentFleetCtx(beadID, name), project)
}

func seedAgent(ms *mockStore, id, role, project string) {
	f, _ := json.Marshal(map[string]any{"role": role, "project": project})
	ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: model.BeadType("agent"), Title: id, Status: model.StatusOpen, Fields: f, Labels: []string{"project:" + project}}
	ms.labels[id] = []string{"project:" + project}
}

// seedRoutedEscalation seeds an escalation as Task 2 stamps it.
func seedRoutedEscalation(ms *mockStore, id, project string, f map[string]any) *model.Bead {
	if _, ok := f["operation_spec"]; !ok {
		f["operation_spec"] = map[string]any{}
	}
	raw, _ := json.Marshal(f)
	b := &model.Bead{ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: id, Status: model.StatusOpen, Fields: raw, Labels: []string{"project:" + project}, CreatedBy: "crew-a"}
	ms.beads[id] = b
	ms.labels[id] = b.Labels // GetBead reads labels from here, not from the bead
	return b
}

func mergeDeskEscalation(requestingAgent string) map[string]any {
	return map[string]any{
		"operation_type": "github_pr_merge",
		"approvers":      []string{"coordinator:merge-desk", "coordinator:laptop", "operator"},
		"execute_by":     "coordinator:merge-desk", "held_by": "coordinator:merge-desk",
		"operator_only": false, "policy_rule": "escalation#1",
		"requesting_agent": requestingAgent, "requesting_agent_verified": "true",
	}
}

func newLaneTestServer() (*BeadsServer, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	s.SetForwardAuthClients([]string{testBrowserClient})
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator"})
	seedAgent(ms, "gd-rev1", "reviewer", "gardenio")
	seedAgent(ms, "gd-crew1", "crew", "gardenio")
	seedAgent(ms, "gd-stew1", "steward", "acme")
	return s, ms
}

// callEscalationVerb calls an escalation handler directly with ctx as the
// authenticated principal.
func callEscalationVerb(t *testing.T, ctx context.Context, h http.HandlerFunc, id, verb string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/escalations/"+id+"/"+verb, bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func TestCallerInLane(t *testing.T) {
	s, ms := newLaneTestServer()
	seedTenantLand(ms, "acme", nil)
	seedTenantLand(ms, "delegated", []string{"github_pr_merge"})
	seedAgent(ms, "gd-stew2", "steward", "delegated")
	p := defaultEscalationPolicy()
	esc := seedRoutedEscalation(ms, "gd-e1", "gardenio", mergeDeskEscalation("gd-crew1"))
	static := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "gd-rev1"})

	cases := []struct {
		name string
		lane string
		ctx  context.Context
		want bool
	}{
		{"operator: admin on browser client", "operator", operatorCtx("owner@example.com"), true},
		{"operator: gardenio-operator is not admin", "operator", humanCtx("op@example.com", []string{RoleOperator}), false},
		{"operator: kd login device client", "operator", WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "owner@example.com", ClientID: DeviceGrantClientID}), authMethodOIDC), []string{RoleAdmin}), false},
		{"operator: static token", "operator", static, false},
		{"coordinator:merge-desk: reviewer agent", "coordinator:merge-desk", agentCtx("gd-rev1", "reviewer-a", "gardenio"), true},
		{"coordinator:merge-desk: crew agent", "coordinator:merge-desk", agentCtx("gd-crew1", "crew-a", "gardenio"), false},
		{"coordinator:merge-desk: static X-Actor naming the reviewer", "coordinator:merge-desk", static, false},
		{"coordinator:laptop: allowlisted machine", "coordinator:laptop", machineCtx("gardenio-laptop-operator"), true},
		{"coordinator:laptop: other machine", "coordinator:laptop", machineCtx("gardenio-controller"), false},
		{"coordinator:ghost: undefined", "coordinator:ghost", machineCtx("gardenio-laptop-operator"), false},
		{"steward: undelegated land", "steward:acme", agentCtx("gd-stew1", "steward-acme", "acme"), false},
		{"steward: delegated land", "steward:delegated", agentCtx("gd-stew2", "steward-d", "delegated"), true},
		{"steward: steward of another land", "steward:delegated", agentCtx("gd-stew1", "steward-acme", "acme"), false},
		{"land-owner: owner group", "land-owner:acme", humanCtx("o@acme.test", []string{RoleProjectMember}, "/project-acme/owners"), true},
		{"land-owner: member group", "land-owner:acme", humanCtx("m@acme.test", []string{RoleProjectMember}, "/project-acme"), false},
		{"land-owner: not a tenant land", "land-owner:gardenio", humanCtx("o@x.test", []string{RoleProjectMember}, "/project-gardenio/owners"), false},
		{"requester: the verified requesting agent", "requester", agentCtx("gd-crew1", "crew-a", "gardenio"), true},
		{"requester: another agent", "requester", agentCtx("gd-rev1", "reviewer-a", "gardenio"), false},
		{"server: daemon replay", "server", withMachineWrite(context.Background()), true},
		{"server: machine principal", "server", machineCtx("gardenio-laptop-operator"), false},
	}
	for _, c := range cases {
		lane, err := parseLaneRef(c.lane)
		if err != nil {
			t.Fatal(err)
		}
		e := esc
		if lane.Kind == laneSteward || lane.Kind == laneLandOwner {
			e = seedRoutedEscalation(ms, "gd-e-"+lane.Name, lane.Name, map[string]any{"operation_type": "github_pr_merge"})
		}
		got, err := s.callerInLane(c.ctx, lane, e, p)
		if err != nil || got != c.want {
			t.Errorf("%s: callerInLane = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestCheckMayApprove_HumanLegacyApproversUnchanged(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-legacy", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"oidc:alice@example.com"}})
	if c := s.checkMayApprove(humanCtx("alice@example.com", []string{RoleOperator}), esc); !c.Allowed {
		t.Fatalf("named legacy approver refused: %+v", c)
	}
	if c := s.checkMayApprove(humanCtx("bob@example.com", []string{RoleOperator}), esc); c.Allowed {
		t.Fatalf("unnamed human allowed: %+v", c)
	}
}

func TestCheckMayApprove_OperatorLaneEntry(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-op", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "operator"})
	c := s.checkMayApprove(operatorCtx("owner@example.com"), esc)
	if !c.Allowed || c.Method != "web_ui" || c.As != "operator" {
		t.Fatalf("admin on the operator lane: %+v, want allowed web_ui as operator", c)
	}
	if c := s.checkMayApprove(humanCtx("op@example.com", []string{RoleOperator}), esc); c.Allowed {
		t.Fatalf("gardenio-operator (not admin) allowed on an operator-lane escalation: %+v", c)
	}
	// No coordinator lane is listed, so the laptop is refused.
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), esc); c.Allowed {
		t.Fatalf("laptop allowed on an operator-only lane list: %+v", c)
	}
}

func TestCheckMayApprove_CoordinatorPath(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-m1", "gardenio", mergeDeskEscalation("gd-crew1"))
	c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), esc)
	if !c.Allowed || c.Method != approvalMethodCoordinator || c.As != "coordinator:merge-desk" {
		t.Fatalf("merge desk on a crew merge: %+v", c)
	}
	if c := s.checkMayApprove(agentCtx("gd-crew1", "crew-a", "gardenio"), esc); c.Allowed {
		t.Fatalf("the requesting crew agent may not approve its own merge: %+v", c)
	}
	unstamped := seedRoutedEscalation(ms, "gd-m2", "gardenio", map[string]any{"operation_type": "github_pr_merge"})
	if c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), unstamped); c.Allowed {
		t.Fatalf("an unstamped escalation must refuse coordinators (fail closed): %+v", c)
	}
}

// TestCoordinatorApprove_NeedsTheHold pins D4 as tightened in the #756
// revision: a coordinator approves only while its lane holds the escalation
// and is listed in approvers. Otherwise the laptop could approve a merge the
// desk audience still has in hand.
func TestCoordinatorApprove_NeedsTheHold(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-hold", "gardenio", mergeDeskEscalation("gd-crew1")) // held by the merge desk
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), esc); c.Allowed {
		t.Fatalf("laptop approved an escalation the merge desk holds: %+v", c)
	}
	// Positive control: once an operator routes it to the operators (D11 rule
	// 3 picks coordinator:laptop), the laptop holds it and may approve.
	f := mergeDeskEscalation("gd-crew1")
	f["held_by"] = "coordinator:laptop"
	handed := seedRoutedEscalation(ms, "gd-hold2", "gardenio", f)
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), handed); !c.Allowed || c.As != "coordinator:laptop" {
		t.Fatalf("laptop holding a merge: %+v, want allowed as coordinator:laptop", c)
	}
	// Holding is not enough: the hold_by lane of rule 5 is not an approver.
	g := map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "policy_rule": "escalation#5"}
	held := seedRoutedEscalation(ms, "gd-hold3", "gardenio", g)
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), held); c.Allowed {
		t.Fatalf("laptop approved a rule-5 escalation it only holds: %+v", c)
	}
	// The operator can approve whoever holds it (humans need no hold).
	if c := s.checkMayApprove(operatorCtx("owner@example.com"), held); !c.Allowed {
		t.Fatalf("operator refused on a laptop-held escalation: %+v", c)
	}
}

// TestCheckMayApprove_UnstampedOperatorOnly: escalationOperatorOnly re-derives
// the one floor (escalationForcedReason) at decision time, so a floor
// escalation with no operator_only stamp still refuses non-operators.
func TestCheckMayApprove_UnstampedOperatorOnly(t *testing.T) {
	s, ms := newLaneTestServer()
	for _, f := range []map[string]any{
		{"operation_type": "credential_rotation", "approvers": []string{"coordinator:laptop", "operator"}, "held_by": "coordinator:laptop"},
		{"operation_type": "github_pr_merge", "credential_requirements": []string{"gh"}, "approvers": []string{"coordinator:laptop", "operator"}, "held_by": "coordinator:laptop"},
	} {
		esc := seedRoutedEscalation(ms, "gd-uoo", "gardenio", f)
		if !s.escalationOperatorOnly(context.Background(), esc) {
			t.Fatalf("escalationOperatorOnly(%v) = false", f)
		}
		if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), esc); c.Allowed {
			t.Fatalf("laptop allowed on unstamped floor %v: %+v", f, c)
		}
		if c := s.checkMayApprove(operatorCtx("owner@example.com"), esc); !c.Allowed || c.As != "operator" {
			t.Fatalf("operator on unstamped floor %v: %+v", f, c)
		}
	}
	// approvers_name_person forces the owner audience but is not operator-only:
	// the named person still approves.
	esc := seedRoutedEscalation(ms, "gd-uoo2", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"oidc:alice@example.com"}})
	if s.escalationOperatorOnly(context.Background(), esc) {
		t.Fatal("a named-person escalation must not be operator-only")
	}
}

func TestCanApproveEndpoint(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-ca1", "gardenio", mergeDeskEscalation("gd-crew1"))
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		if rec.Code != http.StatusOK {
			t.Fatalf("can-approve: got %d %s", rec.Code, rec.Body.String())
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	got := decode(callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleCanApproveEscalation, "gd-ca1", "can-approve", map[string]any{}))
	if got["allowed"] != true || got["method"] != "coordinator" || got["as"] != "coordinator:merge-desk" {
		t.Fatalf("reviewer: %v", got)
	}
	got = decode(callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "gardenio"), s.handleCanApproveEscalation, "gd-ca1", "can-approve", map[string]any{}))
	if got["allowed"] != false || got["reason"] == "" {
		t.Fatalf("crew requester: %v, want allowed=false with a reason", got)
	}
	// Dry run: nothing changed.
	if b := ms.beads["gd-ca1"]; b.Status != model.StatusOpen {
		t.Fatalf("can-approve mutated the escalation: status %s", b.Status)
	}
}

func TestApproveEscalation_HumanOnOperatorLaneStillWorks(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-h1", "gardenio", map[string]any{"operation_type": "manual_owner_action", "approvers": []string{"operator"}, "execute_by": "operator", "held_by": "operator"})
	rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-h1", "approve", map[string]any{"approval_method": "web_ui"})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin approve of an operator-lane escalation: got %d %s", rec.Code, rec.Body.String())
	}
	// Refusal sibling: a gardenio-operator is not on the operator lane.
	seedRoutedEscalation(ms, "gd-h2", "gardenio", map[string]any{"operation_type": "manual_owner_action", "approvers": []string{"operator"}, "execute_by": "operator", "held_by": "operator"})
	rec = callEscalationVerb(t, humanCtx("op@example.com", []string{RoleOperator}), s.handleApproveEscalation, "gd-h2", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator-role approve of an operator-lane escalation: got %d, want 403", rec.Code)
	}
}

// TestApproveEscalation_LandOwnerLaneMatches pins the matcher swap: a
// land-owner:<land> approver entry is satisfied by an owner of that tenant
// land. escalationApproverListed alone matches no lane but operator.
func TestApproveEscalation_LandOwnerLaneMatches(t *testing.T) {
	s, ms := newLaneTestServer()
	seedTenantLand(ms, "acme", nil)
	seedRoutedEscalation(ms, "gd-lo1", "acme", map[string]any{"operation_type": "manual_owner_action", "approvers": []string{"land-owner:acme"}, "execute_by": "land-owner:acme", "held_by": "land-owner:acme"})
	owner := humanCtx("o@acme.test", []string{RoleOperator}, "/project-acme/owners")
	if c := s.checkMayApprove(owner, ms.beads["gd-lo1"]); !c.Allowed || c.As != "land-owner:acme" {
		t.Fatalf("land owner on its own land: %+v, want allowed as land-owner:acme", c)
	}
	rec := callEscalationVerb(t, owner, s.handleApproveEscalation, "gd-lo1", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("land owner approve: got %d %s", rec.Code, rec.Body.String())
	}
	// Refusal sibling: a member of the land is not its owner.
	seedRoutedEscalation(ms, "gd-lo2", "acme", map[string]any{"operation_type": "manual_owner_action", "approvers": []string{"land-owner:acme"}, "execute_by": "land-owner:acme", "held_by": "land-owner:acme"})
	rec = callEscalationVerb(t, humanCtx("m@acme.test", []string{RoleOperator}, "/project-acme"), s.handleApproveEscalation, "gd-lo2", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("land member approve: got %d, want 403", rec.Code)
	}
}
