package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func TestDefaultEscalationPolicy_IsD7(t *testing.T) {
	merges := []string{"github_pr_merge", "gitlab_mr_merge"}
	want := escalationPolicy{
		Rules: []escalationPolicyRule{
			{When: escalationPolicyWhen{OperationTypes: merges, Projects: []string{"gardenio"}}, Approvers: []string{"coordinator:merge-desk", "coordinator:laptop", "operator"}, ExecuteBy: "coordinator:merge-desk"},
			{When: escalationPolicyWhen{OperationTypes: merges, Lands: []string{"*"}}, Approvers: []string{"steward:{land}", "land-owner:{land}"}, ExecuteBy: "requester"},
			{When: escalationPolicyWhen{OperationTypes: []string{"manual_owner_action", "soak_verification"}}, Approvers: []string{"operator"}, ExecuteBy: "operator"},
			{When: escalationPolicyWhen{OperationTypes: []string{"create", "update", "close", "grant"}}, Approvers: []string{"operator"}, ExecuteBy: "server", HoldBy: "coordinator:laptop"},
			{Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"},
		},
		Coordinators: map[string]escalationCoordinator{
			"merge-desk": {Principals: []string{"role:reviewer"}, Audience: "desk"},
			"laptop":     {Principals: []string{"machine:gardenio-laptop-operator"}, Audience: "operator"},
		},
	}
	if got := defaultEscalationPolicy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("defaultEscalationPolicy() =\n%+v\nwant D7\n%+v", got, want)
	}
	raw, _ := json.Marshal(want)
	if reason := validateEscalationPolicy(raw); reason != "" {
		t.Fatalf("D7 must validate, got %q", reason)
	}
}

// TestEscalationForcedReason: the one floor (D3) is #756's forced-owner rule
// plus D3's operation types, with a reason for each.
func TestEscalationForcedReason(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	floor := map[string]string{
		`{"operation_type":"github_pr_merge","sensitivity":"secret"}`:           "sensitivity_secret",
		`{"operation_type":"github_pr_merge","credential_requirements":["gh"]}`: "credential_requirements",
		`{"operation_type":"credential_rotation"}`:                              "credential_rotation",
		`{"operation_type":"Secret_Write"}`:                                     "secret_write",
		`{"operation_type":"gardenio_user_capability"}`:                         "gardenio_user_capability",
		`{"operation_type":"escalation_policy_write"}`:                          "escalation_policy_write",
		`{"operation_type":"spawn_policy_write"}`:                               "spawn_policy_write",
		`{"operation_type":"role_write"}`:                                       "role_write",
		`{"operation_type":"keycloak_group_membership"}`:                        "keycloak_group_membership",
		`{"operation_type":"credential_provisioning"}`:                          "credential_provisioning",
		`{"operation_type":"secret_rotate_db"}`:                                 "secret_rotate_db",
		`{"operation_type":"create","approvers":["oidc:owner@example.com"]}`:    "approvers_name_person",
	}
	for raw, want := range floor {
		b := &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(raw)}
		if got := s.escalationForcedReason(context.Background(), b); got != want {
			t.Errorf("escalationForcedReason(%s) = %q, want %q", raw, got, want)
		}
		if !s.escalationAudienceForced(context.Background(), b) {
			t.Errorf("escalationAudienceForced(%s) = false; #756's wrapper must agree", raw)
		}
		if isEscalationFloorReason(want) != (want != "approvers_name_person") {
			t.Errorf("isEscalationFloorReason(%q) wrong", want)
		}
	}
	// Positive control: ordinary operations, and Task 2's own lane-ref
	// approvers, are not forced.
	for _, raw := range []string{
		`{"operation_type":"github_pr_merge","sensitivity":"internal"}`,
		`{"operation_type":"helm_deploy"}`,
		`{"operation_type":"create","operation_spec":{"type":"task","title":"x"}}`,
		`{"operation_type":"aws_ec2","sensitivity":"sensitive"}`,
		`{"operation_type":"github_pr_merge","approvers":["coordinator:merge-desk","coordinator:laptop","operator"]}`,
	} {
		b := &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(raw)}
		if got := s.escalationForcedReason(context.Background(), b); got != "" {
			t.Errorf("escalationForcedReason(%s) = %q, want not forced", raw, got)
		}
	}
}

// TestLaneAudience pins D11's mapping, the only place it lives.
func TestLaneAudience(t *testing.T) {
	p := defaultEscalationPolicy()
	for ref, want := range map[string]string{
		"operator": "owner", "land-owner:acme": "owner",
		"coordinator:merge-desk": "desk", "coordinator:laptop": "operator",
		"steward:acme": "desk", "requester": "operator", "server": "operator",
		// Unknown coordinators and refs fail toward the owner (Global Constraint 4).
		"coordinator:ghost": "owner", "bogus": "owner", "": "owner",
	} {
		if got := laneAudience(ref, p); got != want {
			t.Errorf("laneAudience(%q) = %q, want %q", ref, got, want)
		}
	}
	// A coordinator with no audience is operator (the default).
	p.Coordinators["x"] = escalationCoordinator{Principals: []string{"role:x"}}
	if got := laneAudience("coordinator:x", p); got != "operator" {
		t.Errorf("default coordinator audience = %q, want operator", got)
	}
}

func TestEvaluateEscalationPolicy_D7Routes(t *testing.T) {
	p := defaultEscalationPolicy()
	cases := []struct {
		f    escalationFacts
		want escalationRoute
	}{
		{escalationFacts{OperationType: "github_pr_merge", Project: "gardenio"},
			escalationRoute{Rule: "escalation#1", Approvers: []string{"coordinator:merge-desk", "coordinator:laptop", "operator"}, ExecuteBy: "coordinator:merge-desk"}},
		{escalationFacts{OperationType: "gitlab_mr_merge", Project: "acme", Land: "acme"},
			escalationRoute{Rule: "escalation#2", Approvers: []string{"steward:acme", "land-owner:acme"}, ExecuteBy: "requester"}},
		// claimlayer is an operator project (no tenant land_kind): a merge there is not rule 2.
		{escalationFacts{OperationType: "github_pr_merge", Project: "claimlayer"},
			escalationRoute{Rule: "escalation#5", Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"}},
		{escalationFacts{OperationType: "soak_verification", Project: "gardenio"},
			escalationRoute{Rule: "escalation#3", Approvers: []string{"operator"}, ExecuteBy: "operator"}},
		{escalationFacts{OperationType: "close"},
			escalationRoute{Rule: "escalation#4", Approvers: []string{"operator"}, ExecuteBy: "server", HoldBy: "coordinator:laptop"}},
		{escalationFacts{OperationType: "helm_deploy", Project: "gardenio"},
			escalationRoute{Rule: "escalation#5", Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"}},
		// The evaluator no longer floors (escalationForcedReason does, at stamp
		// time): a credential_rotation falls to the catch-all here.
		{escalationFacts{OperationType: "credential_rotation", Project: "gardenio"},
			escalationRoute{Rule: "escalation#5", Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"}},
	}
	for _, c := range cases {
		if got := evaluateEscalationPolicy(p, c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("evaluate(%+v) = %+v, want %+v", c.f, got, c.want)
		}
	}
	// Fail closed when nothing matches (CONTRACT ISSUE 15).
	got := evaluateEscalationPolicy(escalationPolicy{Rules: []escalationPolicyRule{{When: escalationPolicyWhen{OperationTypes: []string{"x"}}, Approvers: []string{"operator"}, ExecuteBy: "operator"}}}, escalationFacts{OperationType: "y"})
	if !got.OperatorOnly || got.Rule != "floor:no_matching_rule" || !reflect.DeepEqual(got.Approvers, []string{"operator"}) {
		t.Fatalf("no match: got %+v, want floor:no_matching_rule operator-only", got)
	}
}

func TestValidateEscalationPolicy(t *testing.T) {
	bad := map[string]string{
		"not json":             `{"rules":`,
		"no rules":             `{"rules":[],"coordinators":{}}`,
		"unknown rule key":     `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","mode":"auto"}],"coordinators":{}}`,
		"unknown when key":     `{"rules":[{"when":{"project":["gardenio"]},"approvers":["operator"],"execute_by":"operator"},{"when":{},"approvers":["operator"],"execute_by":"operator"}],"coordinators":{}}`,
		"no approvers":         `{"rules":[{"when":{},"approvers":[],"execute_by":"operator"}],"coordinators":{}}`,
		"bad lane":             `{"rules":[{"when":{},"approvers":["role:crew"],"execute_by":"operator"}],"coordinators":{}}`,
		"undefined coord":      `{"rules":[{"when":{},"approvers":["coordinator:ghost"],"execute_by":"operator"}],"coordinators":{}}`,
		"land without lands":   `{"rules":[{"when":{},"approvers":["steward:{land}"],"execute_by":"operator"}],"coordinators":{}}`,
		"requester approves":   `{"rules":[{"when":{},"approvers":["requester"],"execute_by":"operator"}],"coordinators":{}}`,
		"legacy execute_by":    `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"oidc:a@example.com"}],"coordinators":{}}`,
		"empty execute_by":     `{"rules":[{"when":{},"approvers":["operator"],"execute_by":""}],"coordinators":{}}`,
		"no catch-all":         `{"rules":[{"when":{"operation_types":["x"]},"approvers":["operator"],"execute_by":"operator"}],"coordinators":{}}`,
		"empty principals":     `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":[]}}}`,
		"bad principal":        `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":["user:x"]}}}`,
		"missing coordinators": `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator"}]}`,
		"owner coordinator":    `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":["role:x"],"audience":"owner"}}}`,
		"bad coordinator aud":  `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":["role:x"],"audience":"everyone"}}}`,
		"requester holds":      `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","hold_by":"requester"}],"coordinators":{}}`,
		"legacy holds":         `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","hold_by":"oidc:a@example.com"}],"coordinators":{}}`,
		"undefined holder":     `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","hold_by":"coordinator:ghost"}],"coordinators":{}}`,
	}
	for name, raw := range bad {
		if reason := validateEscalationPolicy(json.RawMessage(raw)); reason == "" {
			t.Errorf("%s: validateEscalationPolicy accepted %s", name, raw)
		}
	}
	// Positive control: a minimal valid policy with legacy approvers and a land rule.
	good := `{"rules":[
		{"when":{"lands":["*"]},"approvers":["steward:{land}","oidc:a@example.com"],"execute_by":"requester"},
		{"when":{},"approvers":["operator"],"execute_by":"coordinator:a","hold_by":"coordinator:a"}],
		"coordinators":{"a":{"principals":["role:reviewer","machine:gardenio-laptop-operator"],"audience":"desk"}}}`
	if reason := validateEscalationPolicy(json.RawMessage(good)); reason != "" {
		t.Fatalf("valid policy refused: %s", reason)
	}
}

func TestEscalationPolicyWrite_MalformedIs400(t *testing.T) {
	_, ms, h := newTestServer() // OIDC gardenio-admin: the type's write_policy min_role
	valid := map[string]any{
		"rules":        []any{map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "operator"}},
		"coordinators": map[string]any{},
	}
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{"title": "escalation policy", "type": "escalation_policy", "kind": "config", "fields": valid})
	requireStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id, _ := created["id"].(string)

	bad := map[string]any{"rules": []any{map[string]any{"when": map[string]any{}, "approvers": []string{"coordinator:ghost"}, "execute_by": "operator"}}, "coordinators": map[string]any{}}
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{"title": "bad policy", "type": "escalation_policy", "kind": "config", "fields": bad})
	requireStatus(t, rec, http.StatusBadRequest)

	before := string(ms.beads[id].Fields)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": bad})
	requireStatus(t, rec, http.StatusBadRequest)
	if string(ms.beads[id].Fields) != before {
		t.Fatal("a refused policy update changed the bead")
	}
}

func seedTenantLand(ms *mockStore, land string, stewardApproves []string) {
	f := map[string]any{"land_kind": "tenant"}
	if stewardApproves != nil {
		f["land_steward_approves"] = stewardApproves
	}
	raw, _ := json.Marshal(f)
	ms.beads["gd-proj-"+land] = &model.Bead{ID: "gd-proj-" + land, Kind: model.KindConfig, Type: model.BeadType("project"), Title: land, Status: model.StatusOpen, Fields: raw}
}

func createEscalationStatic(t *testing.T, h http.Handler, labels []string, fields map[string]any) map[string]any {
	t.Helper()
	if _, ok := fields["operation_spec"]; !ok {
		fields["operation_spec"] = map[string]any{}
	}
	// force: the tests file the same ask more than once to compare routes,
	// which the create dedupe (create_dedupe.go) would refuse.
	rec := doAgentEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": "routed", "type": "escalation", "kind": "data", "labels": labels, "fields": fields, "force": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, rec, &resp)
	out, _ := resp["fields"].(map[string]any)
	return out
}

func assertRoute(t *testing.T, got map[string]any, heldBy, executeBy, rule string, operatorOnly bool, approvers ...string) {
	t.Helper()
	var gotApprovers []string
	for _, a := range got["approvers"].([]any) {
		gotApprovers = append(gotApprovers, a.(string))
	}
	if at, _ := got["held_at"].(string); at == "" {
		t.Fatalf("held_at not stamped with held_by: %v", got)
	}
	// D11: audience always follows held_by (laneAudience on the D7 coordinators;
	// a coordinator a test defines without an audience is operator).
	wantAudience := "operator"
	switch {
	case heldBy == "operator", strings.HasPrefix(heldBy, "land-owner:"):
		wantAudience = "owner"
	case heldBy == "coordinator:merge-desk", strings.HasPrefix(heldBy, "steward:"):
		wantAudience = "desk"
	}
	if got["audience"] != wantAudience {
		t.Fatalf("audience = %v with held_by=%s, want %s (D11)", got["audience"], heldBy, wantAudience)
	}
	if got["held_by"] != heldBy || got["execute_by"] != executeBy || got["policy_rule"] != rule || got["operator_only"] != operatorOnly || !reflect.DeepEqual(gotApprovers, approvers) {
		t.Fatalf("route = held_by=%v execute_by=%v policy_rule=%v operator_only=%v approvers=%v; want %s %s %s %v %v",
			got["held_by"], got["execute_by"], got["policy_rule"], got["operator_only"], gotApprovers, heldBy, executeBy, rule, operatorOnly, approvers)
	}
}

func TestEscalationCreate_StampsRoute(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := escalationAgentHandler(s, ms, nil)
	seedTenantLand(ms, "acme", nil)
	seedTenantLand(ms, "delegated", []string{"github_pr_merge"})

	// gardenio merge: the merge desk (role:reviewer) is always staffed; the laptop
	// is not (BEADS_ESCALATION_COORDINATOR_CLIENTS unset), and it sits second anyway.
	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:merge-desk", "coordinator:merge-desk", "escalation#1", false, "coordinator:merge-desk", "coordinator:laptop", "operator")

	// Floor.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "credential_rotation"})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:credential_rotation", true, "operator")

	// Tenant land without delegation: the steward is skipped (Review Focus 5).
	got = createEscalationStatic(t, h, []string{"project:acme"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "land-owner:acme", "requester", "escalation#2", false, "steward:acme", "land-owner:acme")

	// Positive control: the same merge in a land that delegates merges to its steward.
	got = createEscalationStatic(t, h, []string{"project:delegated"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "steward:delegated", "requester", "escalation#2", false, "steward:delegated", "land-owner:delegated")

	// Everything else: held by the laptop (hold_by), which is not an approver.
	// It need not be staffed to hold (D11 rule 2): before Task 13 allowlists its
	// client the escalation still reads as #756's operator audience.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "helm_deploy"})
	assertRoute(t, got, "coordinator:laptop", "coordinator:laptop", "escalation#5", false, "operator")

	// The owner's own kind of work is held by the operator lane.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "manual_owner_action"})
	assertRoute(t, got, "operator", "operator", "escalation#3", false, "operator")
}

// TestEscalationCreate_AudienceFollowsHeldBy pins D11 rule 2 (Review Focus 4):
// a filer may raise an escalation to the owner, which holds it in an
// owner-audience lane; a filer's desk or operator is ignored (#756); an
// invalid value is still #756's 400.
func TestEscalationCreate_AudienceFollowsHeldBy(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := escalationAgentHandler(s, ms, nil)

	// Raised to the owner: same approvers, but held by the operator lane.
	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge", "audience": "owner"})
	assertRoute(t, got, "operator", "coordinator:merge-desk", "escalation#1", false, "coordinator:merge-desk", "coordinator:laptop", "operator")

	// Positive control: the same merge without the raise goes to the desk.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:merge-desk", "coordinator:merge-desk", "escalation#1", false, "coordinator:merge-desk", "coordinator:laptop", "operator")

	// A filer cannot lower: desk on a helm_deploy is ignored.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "helm_deploy", "audience": "desk"})
	assertRoute(t, got, "coordinator:laptop", "coordinator:laptop", "escalation#5", false, "operator")

	// #756's 400 on an invalid value is unchanged.
	rec := doAgentEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": "bad", "type": "escalation", "kind": "data", "fields": map[string]any{"operation_type": "helm_deploy", "audience": "everyone"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid audience: got %d, want 400", rec.Code)
	}

	// credential_requirements is a floor reason now (D3).
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge", "credential_requirements": []string{"gh-token"}})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:credential_requirements", true, "operator")
}

// Review Focus 3: a policy bead that hands credential_rotation to the laptop
// is overridden by the code floor.
func TestEscalationCreate_FloorBeatsConfig(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := escalationAgentHandler(s, ms, nil)
	pol, _ := json.Marshal(map[string]any{
		"rules": []any{
			map[string]any{"when": map[string]any{"operation_types": []string{"credential_rotation", "github_pr_merge"}}, "approvers": []string{"coordinator:laptop"}, "execute_by": "coordinator:laptop"},
			map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "operator"},
		},
		"coordinators": map[string]any{"laptop": map[string]any{"principals": []string{"machine:gardenio-laptop-operator"}, "audience": "operator"}},
	})
	ms.beads["gd-pol-floor"] = &model.Bead{ID: "gd-pol-floor", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator"})

	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "credential_rotation"})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:credential_rotation", true, "operator")
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge", "sensitivity": "secret"})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:sensitivity_secret", true, "operator")

	// Positive control: the same rule routes a non-floor operation as configured.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:laptop", "coordinator:laptop", "escalation#1", false, "coordinator:laptop")
}

func TestEscalationCreate_KeepsServerStampedSpawnApprovers(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ctx := withServerStampedEscalation(context.Background())
	fields, _ := json.Marshal(map[string]any{"operation_type": "create", "approvers": []string{"oidc:owner@example.com"}})
	b := &model.Bead{Type: model.TypeEscalation, Fields: fields}
	if err := s.stampEscalationRoute(ctx, b); err != nil {
		t.Fatal(err)
	}
	approvers, _ := escalationApprovers(b)
	if !reflect.DeepEqual(approvers, []string{"oidc:owner@example.com"}) {
		t.Fatalf("spawn-rule approvers overwritten: %v", approvers)
	}
	if v, _, _ := exactFieldString(b.Fields, "held_by"); v != "operator" {
		t.Fatalf("held_by = %q, want operator (a legacy entry is not a lane)", v)
	}
	if v, _, _ := exactFieldString(b.Fields, "execute_by"); v != "server" {
		t.Fatalf("execute_by = %q, want server (rule 4)", v)
	}
	// approvers_name_person forces the owner without the floor: the named
	// person must still be able to approve, so it is not operator_only, and
	// rule 4's hold_by is overridden.
	if v, _, _ := exactFieldString(b.Fields, "audience"); v != "owner" {
		t.Fatalf("audience = %q, want owner (forced)", v)
	}
	if v := escalationFieldValues(b.Fields, []string{"operator_only"})["operator_only"]; v != false {
		t.Fatalf("operator_only = %v, want false for approvers_name_person", v)
	}
}

func TestEscalationCreate_ActivePolicyBeadWins(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := escalationAgentHandler(s, ms, nil)
	pol, _ := json.Marshal(map[string]any{
		"rules":        []any{map[string]any{"when": map[string]any{}, "approvers": []string{"coordinator:desk", "operator"}, "execute_by": "operator"}},
		"coordinators": map[string]any{"desk": map[string]any{"principals": []string{"role:reviewer"}}},
	})
	ms.beads["gd-pol1"] = &model.Bead{ID: "gd-pol1", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}

	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:desk", "operator", "escalation#1", false, "coordinator:desk", "operator")

	// Positive control: the floor still wins over the configured catch-all.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "secret_write"})
	assertRoute(t, got, "operator", "operator", "floor:secret_write", true, "operator")
}

func TestFirstStaffedLane(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTenantLand(ms, "acme", nil)
	p := defaultEscalationPolicy()
	esc := &model.Bead{ID: "gd-e", Type: model.TypeEscalation, Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`)}
	ctx := context.Background()

	if got := s.firstStaffedLane(ctx, esc, []string{"coordinator:laptop", "operator"}, p); got != "operator" {
		t.Fatalf("laptop with no allowlisted client: got %q, want operator", got)
	}
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator"})
	if got := s.firstStaffedLane(ctx, esc, []string{"coordinator:laptop", "operator"}, p); got != "coordinator:laptop" {
		t.Fatalf("laptop allowlisted: got %q, want coordinator:laptop", got)
	}
	if got := s.firstStaffedLane(ctx, esc, []string{"bogus:x", "requester", "steward:acme", "land-owner:acme"}, p); got != "land-owner:acme" {
		t.Fatalf("got %q, want land-owner:acme (bogus, unverified requester and undelegated steward skipped)", got)
	}
	if got := s.firstStaffedLane(ctx, esc, []string{"land-owner:notaland"}, p); got != "operator" {
		t.Fatalf("land-owner of a non-tenant project: got %q, want operator", got)
	}
}

func TestEscalationRouteSweep_StampsOnlyUnstamped(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seed := func(id string, status model.Status, f map[string]any) {
		raw, _ := json.Marshal(f)
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: id, Status: status, Fields: raw, Labels: []string{"project:gardenio"}}
	}
	seed("gd-legacy", model.StatusOpen, map[string]any{"operation_type": "manual_owner_action"})
	seed("gd-legacy-ip", model.StatusInProgress, map[string]any{"operation_type": "github_pr_merge"})
	seed("gd-stamped", model.StatusOpen, map[string]any{"operation_type": "github_pr_merge", "held_by": "coordinator:laptop"})
	seed("gd-closed", model.StatusClosed, map[string]any{"operation_type": "github_pr_merge"})
	// #756 already stamped (or an operator re-routed) these: the sweep keeps
	// the audience and picks a lane of it (D11 rule 4).
	seed("gd-routed-desk", model.StatusOpen, map[string]any{"operation_type": "helm_deploy", "audience": "desk"})
	seed("gd-routed-owner", model.StatusOpen, map[string]any{"operation_type": "github_pr_merge", "audience": "owner"})

	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy"].Fields, "held_by"); v != "operator" {
		t.Errorf("gd-legacy held_by = %q, want operator (rule 3)", v)
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy"].Fields, "held_at"); v == "" {
		t.Error("gd-legacy held_at not stamped by the sweep")
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy-ip"].Fields, "held_by"); v != "coordinator:merge-desk" {
		t.Errorf("gd-legacy-ip held_by = %q, want coordinator:merge-desk", v)
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy-ip"].Fields, "audience"); v != "desk" {
		t.Errorf("gd-legacy-ip audience = %q, want desk (laneAudience)", v)
	}
	for id, want := range map[string][2]string{
		"gd-routed-desk":  {"coordinator:merge-desk", "desk"}, // no desk approver: the desk coordinator holds it
		"gd-routed-owner": {"operator", "owner"},
	} {
		held, _, _ := exactFieldString(ms.beads[id].Fields, "held_by")
		aud, _, _ := exactFieldString(ms.beads[id].Fields, "audience")
		if held != want[0] || aud != want[1] {
			t.Errorf("%s: held_by=%q audience=%q, want %q %q", id, held, aud, want[0], want[1])
		}
	}
	if v, _, _ := exactFieldString(ms.beads["gd-stamped"].Fields, "held_by"); v != "coordinator:laptop" {
		t.Errorf("an already-routed escalation was re-stamped: held_by = %q", v)
	}
	if _, present, _ := exactFieldString(ms.beads["gd-closed"].Fields, "held_by"); present {
		t.Error("a closed escalation was stamped")
	}
}

// unrouteEscalation strips the create-time route stamp from a bead in ms, so a
// test that predates routing approves it as an escalation filed before Task 2
// (Global Constraint 7: no held_by, today's approval rule). A stamped
// approvers list goes too when it holds only lane refs; spawn-rule approvers
// (legacy entries) stay. The approver is an operator-role human, whom D2's
// operator lane (gardenio-admin) no longer admits on a routed escalation
// (CONTRACT ISSUE 6). A test using it should say why.
func unrouteEscalation(t *testing.T, ms *mockStore, id string) {
	t.Helper()
	ms.beadsMu.Lock()
	defer ms.beadsMu.Unlock()
	b, ok := ms.beads[id]
	if !ok {
		t.Fatalf("unrouteEscalation: no bead %s", id)
	}
	var f map[string]any
	if err := json.Unmarshal(b.Fields, &f); err != nil {
		t.Fatalf("unrouteEscalation: %v", err)
	}
	for _, k := range []string{"held_by", "held_at", "execute_by", "operator_only", "policy_rule"} {
		delete(f, k)
	}
	if list, _ := f["approvers"].([]any); len(list) > 0 {
		lanes := true
		for _, e := range list {
			if lane, err := parseLaneRef(e.(string)); err != nil || lane.Kind == laneLegacy {
				lanes = false
			}
		}
		if lanes {
			delete(f, "approvers")
		}
	}
	b.Fields, _ = json.Marshal(f)
}

// TestApproveRoutedEscalation_OperatorLaneIsAdmin: until Task 3, a stamped
// approvers list admits a gardenio-admin through the operator lane (D2) and
// nobody through any other lane; an operator-role human is refused
// (CONTRACT ISSUE 6). Before the bridge in escalationApproverListed, "operator"
// read as a bare email and no human could approve a routed escalation
// (CONTRACT ISSUE 5).
func TestApproveRoutedEscalation_OperatorLaneIsAdmin(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	file := func() string {
		rec := requesterRequest(h, "POST", "/v1/beads", map[string]any{
			"title": "deploy", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"}, "force": true,
			"fields": map[string]any{"operation_type": "helm_deploy", "operation_spec": map[string]any{}},
		})
		requireStatus(t, rec, 201)
		var esc map[string]any
		decodeJSON(t, rec, &esc)
		return esc["id"].(string)
	}

	id := file()
	if got := escalationFieldValues(ms.beads[id].Fields, []string{"approvers"})["approvers"]; !reflect.DeepEqual(got, []any{"operator"}) {
		t.Fatalf("approvers = %v, want the stamped [operator]", got)
	}
	requireStatus(t, approveSpawnEscalation(h, id, approverOperatorTok), 403)
	requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 200)

	// A list of lanes no human is a member of admits nobody, admin included.
	id = file()
	ms.beads[id].Fields = setFieldValue(ms.beads[id].Fields, "approvers", []string{"coordinator:merge-desk", "steward:acme"})
	requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 403)

	// Positive control: unrouted, the operator-role human approves as before
	// (Global Constraint 7).
	id = file()
	unrouteEscalation(t, ms, id)
	requireStatus(t, approveSpawnEscalation(h, id, approverOperatorTok), 200)
}
