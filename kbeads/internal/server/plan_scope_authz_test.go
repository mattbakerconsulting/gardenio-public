package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// Plan-scoped writes (gd-Mk64Bko3XO): an agent bound to plan epic P may update
// and close beads whose parent chain reaches P, and nothing else.

// planScopeFixture is plan gd-plan (an owner-minted plan bound to thread head
// gd-head) with a phase, a step under the phase, a narrative bundle and a doc,
// a gardener trigger pinned to it, and a sibling plan gd-plan2 with a step.
// Every bead was filed by the owner, so none of the agents below owns any of
// them.
func planScopeFixture(ms *mockStore) {
	issue := func(id string, typ model.BeadType, labels []string, fields string) {
		ms.beads[id] = &model.Bead{
			ID: id, Kind: model.KindIssue, Type: typ, Title: id, Status: model.StatusOpen,
			CreatedBy: "owner@example.com", Fields: json.RawMessage(fields),
		}
		ms.labels[id] = labels
	}
	child := func(id, parent string) {
		ms.deps[id] = append(ms.deps[id], &model.Dependency{BeadID: id, DependsOnID: parent, Type: model.DepParentChild})
	}
	proj := "project:gardenio"
	issue("gd-plan", model.TypeEpic, []string{proj, "plan"}, `{"plan_state":"building","thread_id":"gd-head"}`)
	ms.beads["gd-plan"].Assignee = "crew-plan-owner"
	issue("gd-phase", model.TypeEpic, []string{proj}, `{}`)
	child("gd-phase", "gd-plan")
	issue("gd-step", model.TypeTask, []string{proj}, `{}`)
	child("gd-step", "gd-phase")
	issue("gd-bundle", model.TypeBundle, []string{proj}, `{}`)
	child("gd-bundle", "gd-plan")
	issue("gd-doc", typeDoc, []string{proj}, `{}`)
	child("gd-doc", "gd-bundle")
	issue("gd-esc", model.TypeEscalation, []string{proj}, `{}`)
	child("gd-esc", "gd-plan")

	ms.beads["gd-trig"] = &model.Bead{
		ID: "gd-trig", Kind: model.KindData, Type: "trigger", Title: "gardener", Status: model.StatusOpen,
		CreatedBy: "owner@example.com", Fields: json.RawMessage(`{"role":"plan-gardener","enabled":true}`),
	}
	ms.labels["gd-trig"] = []string{proj, "role:gardener", gardenerEpicLabelPrefix + "gd-plan", "plan-gardener"}
	child("gd-trig", "gd-plan")

	issue("gd-plan2", model.TypeEpic, []string{proj, "plan"}, `{"thread_id":"gd-head2"}`)
	issue("gd-step2", model.TypeTask, []string{proj}, `{}`)
	child("gd-step2", "gd-plan2")
}

// planAgent adds an agent bead and returns its per-agent-token context.
func planAgent(ms *mockStore, beadID, name, role string, extra map[string]string) context.Context {
	f := map[string]string{"agent": name, "role": role, "project": "gardenio"}
	for k, v := range extra {
		f[k] = v
	}
	raw, _ := json.Marshal(f)
	ms.beads[beadID] = &model.Bead{ID: beadID, Kind: model.KindConfig, Type: "agent", Title: name, Status: model.StatusOpen, Fields: raw}
	ms.labels[beadID] = []string{"project:gardenio"}
	return WithTokenProject(agentFleetCtx(beadID, name), "gardenio")
}

func TestPlanScope_Authorize(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	assignee := planAgent(ms, "gd-a-assignee", "crew-plan-owner", "crew", nil)
	thread := planAgent(ms, "gd-a-thread", "thread-c90", "thread", map[string]string{"thread_id": "gd-head"})
	legacyThread := planAgent(ms, "gd-a-legacy", "thread-old", "thread", map[string]string{"slack_thread_bead_id": "gd-head"})
	gardener := planAgent(ms, "gd-a-gardener", "gardenio-plan-gardener-for-gd-plan-x1", "plan-gardener", map[string]string{"trigger_id": "gd-trig"})
	unrelated := planAgent(ms, "gd-a-crew", "crew-other", "crew", nil)
	otherThread := planAgent(ms, "gd-a-thread2", "thread-c91", "thread", map[string]string{"thread_id": "gd-head2"})
	crewInThread := planAgent(ms, "gd-a-crew2", "crew-in-thread", "crew", map[string]string{"thread_id": "gd-head"})
	crewWithTrigger := planAgent(ms, "gd-a-fake", "crew-fake", "crew", map[string]string{"trigger_id": "gd-trig"})
	otherProject := WithTokenProject(agentFleetCtx("gd-a-thread", "thread-c90"), "other")

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		action string
		bead   string
		rule   planScopeRule
	}{
		{"assignee updates the plan", assignee, "update", "gd-plan", planScopeRuleAssignee},
		{"assignee closes a step under a phase", assignee, "close", "gd-step", planScopeRuleAssignee},
		{"thread agent updates a phase", thread, "update", "gd-phase", planScopeRuleThreadAgent},
		{"thread agent updates a bundle doc", thread, "update", "gd-doc", planScopeRuleThreadAgent},
		{"legacy thread link closes the bundle", legacyThread, "close", "gd-bundle", planScopeRuleThreadAgent},
		{"gardener iteration closes a step", gardener, "close", "gd-step", planScopeRulePlanGardener},
		{"gardener iteration updates the plan", gardener, "update", "gd-plan", planScopeRulePlanGardener},

		{"unrelated agent", unrelated, "update", "gd-step", ""},
		{"sibling plan's thread agent", otherThread, "update", "gd-step", ""},
		{"thread agent on the sibling plan's step", thread, "close", "gd-step2", ""},
		{"gardener on the sibling plan", gardener, "update", "gd-plan2", ""},
		{"crew spawned in the plan's thread is not its thread agent", crewInThread, "update", "gd-step", ""},
		{"trigger link without the plan-gardener role", crewWithTrigger, "update", "gd-step", ""},
		{"token for another project", otherProject, "update", "gd-step", ""},
		{"delete is never admitted", assignee, "delete", "gd-step", ""},
		{"escalation under the plan", assignee, "update", "gd-esc", ""},
		{"gardener trigger under the plan", gardener, "update", "gd-trig", ""},
		{"static token is not an agent token", WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "crew-plan-owner"}), "update", "gd-step", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead, _ := ms.GetBead(context.Background(), tc.bead)
			adm, ok := s.authorizePlanScope(tc.ctx, tc.action, bead)
			if ok != (tc.rule != "") || adm.Rule != tc.rule {
				t.Fatalf("authorizePlanScope = (%+v, %v), want rule %q", adm, ok, tc.rule)
			}
			if ok && adm.PlanID != "gd-plan" {
				t.Fatalf("plan = %q, want gd-plan", adm.PlanID)
			}
		})
	}
}

// The ownership gate admits a bound agent, refuses everyone else with the
// ordinary 403, and writes the admitting rule to mutation history.
func TestPlanScope_OwnershipGateAndAudit(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	gardener := planAgent(ms, "gd-a-gardener", "gardenio-plan-gardener-for-gd-plan-x1", "plan-gardener", map[string]string{"trigger_id": "gd-trig"})
	unrelated := planAgent(ms, "gd-a-crew", "crew-other", "crew", nil)

	get := func(id string) *model.Bead {
		b, _ := ms.GetBead(context.Background(), id)
		return b
	}
	step := get("gd-step")
	if d := s.authorizeOwnership(unrelated, "update", step); d == nil {
		t.Fatal("an unrelated agent must get the ownership 403")
	}
	if d := s.authorizeOwnership(gardener, "update", step); d != nil {
		t.Fatalf("the plan's gardener must pass: %v", d)
	}
	// An escalation and the trigger stay out of plan scope; the trigger keeps
	// its ordinary ownership 403 even for the bound gardener.
	if d := s.authorizeOwnership(gardener, "update", get("gd-trig")); d == nil {
		t.Fatal("a trigger under the plan must not be writable through plan scope")
	}

	resp, err := s.CloseBead(gardener, &beadsv1.CloseBeadRequest{Id: "gd-step"})
	if err != nil || resp.GetBead().GetStatus() != string(model.StatusClosed) {
		t.Fatalf("gardener close of a plan step = %v, %v; want closed", resp, err)
	}
	var audit *model.MutationRow
	for _, m := range ms.mutations {
		if m.BeadID == "gd-step" && m.Field == planScopeAuditField {
			audit = m
		}
	}
	if audit == nil || audit.NewValue != "plan_gardener plan=gd-plan action=close" {
		t.Fatalf("admission row = %+v, want the rule and plan; mutations = %+v", audit, ms.mutations)
	}

	if _, err := s.CloseBead(unrelated, &beadsv1.CloseBeadRequest{Id: "gd-step2"}); err == nil {
		t.Fatal("an unrelated agent's close must be refused")
	}
}

// Updating through updateBead: a bound agent writes plan_state and step order
// on an owner-minted plan.
func TestPlanScope_UpdatePlanFields(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	ms.beads["gd-plan"].Fields = json.RawMessage(`{"plan_state":"ready","thread_id":"gd-head"}`)
	thread := planAgent(ms, "gd-a-thread", "thread-c90", "thread", map[string]string{"thread_id": "gd-head"})

	fields := json.RawMessage(`{"step_order":["gd-step"]}`)
	if _, err := s.updateBead(thread, "gd-plan", updateBeadInput{Fields: fields}); err != nil {
		t.Fatalf("the plan's thread agent must update step_order: %v", err)
	}
	if !strings.Contains(string(ms.beads["gd-plan"].Fields), "gd-step") {
		t.Fatalf("step_order not stored: %s", ms.beads["gd-plan"].Fields)
	}
}

// A spawn field stays refused even when the caller is bound: plan scope never
// opens the gardener trigger, so its enable flag and run budget keep the gates
// they had.
func TestPlanScope_SpawnFieldsStayRefused(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	gardener := planAgent(ms, "gd-a-gardener", "gardenio-plan-gardener-for-gd-plan-x1", "plan-gardener", map[string]string{"trigger_id": "gd-trig"})
	ms.beads["gd-plan"].Assignee = "gardenio-plan-gardener-for-gd-plan-x1"

	for _, patch := range []string{`{"enabled":false}`, `{"max_runs":99}`} {
		_, err := s.updateBead(gardener, "gd-trig", updateBeadInput{Fields: json.RawMessage(patch)})
		var d *AuthzDenial
		if !errors.As(err, &d) {
			t.Fatalf("bound gardener patching %s on its trigger: err = %v, want a 403", patch, err)
		}
	}
}

// The agent-side links the rule reads are fixed at spawn: an agent token may
// not point its bead (or another agent's) at a different thread or trigger.
func TestPlanScope_BindingKeysLocked(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	crew := planAgent(ms, "gd-a-crew", "crew-other", "crew", map[string]string{"thread_id": "gd-head9"})

	for _, patch := range []string{
		`{"thread_id":"gd-head"}`,
		`{"slack_thread_bead_id":"gd-head"}`,
		`{"trigger_id":"gd-trig"}`,
		`{"thread_id":""}`,
	} {
		_, err := s.updateBead(crew, "gd-a-crew", updateBeadInput{Fields: json.RawMessage(patch)})
		var d *AuthzDenial
		if !errors.As(err, &d) {
			t.Fatalf("agent token patching %s: err = %v, want a 403", patch, err)
		}
	}
	// An echo of the stored value and unrelated fields pass.
	if _, err := s.updateBead(crew, "gd-a-crew", updateBeadInput{Fields: json.RawMessage(`{"thread_id":"gd-head9","agent_state":"working"}`)}); err != nil {
		t.Fatalf("echoed link with an unrelated field must pass: %v", err)
	}
	// The field-mutation endpoints refuse the keys outright for an agent token.
	if d := s.authorizeUserBeadFieldWrite(crew, "edit", ms.beads["gd-a-crew"], "trigger_id"); d == nil {
		t.Fatal("field endpoint: an agent token must not edit trigger_id")
	}
	if d := s.authorizeUserBeadFieldWrite(crew, "edit", ms.beads["gd-a-crew"], "agent_state"); d != nil {
		t.Fatalf("field endpoint: unrelated key refused: %v", d)
	}
	// Callers that are not agent tokens (here an OIDC admin) are not gated.
	if _, err := s.updateBead(oidcCtx("admin@example.com", []string{RoleAdmin}), "gd-a-crew", updateBeadInput{Fields: json.RawMessage(`{"thread_id":"gd-head"}`)}); err != nil {
		t.Fatalf("admin must be able to rebind an agent: %v", err)
	}
}

// The walk stops at the nearest plan: a plan nested in another belongs to
// itself alone.
func TestPlanScope_NestedPlanBelongsToItself(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	ms.deps["gd-plan2"] = append(ms.deps["gd-plan2"], &model.Dependency{BeadID: "gd-plan2", DependsOnID: "gd-plan", Type: model.DepParentChild})
	thread := planAgent(ms, "gd-a-thread", "thread-c90", "thread", map[string]string{"thread_id": "gd-head"})

	if _, ok := s.authorizePlanScope(thread, "update", ms.beads["gd-step2"]); ok {
		t.Fatal("the outer plan's agent must not reach into a nested plan")
	}
}
