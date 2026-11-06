package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-PtbbUxKWds: an agent bound to a tenant land is a tenant agent whatever its
// role. A Commons spawn in a land runs role thread unless told otherwise, and
// thread holds no tier, so keying the tier on the role alone let a stranger's
// @-mention start an agent with operator-shaped rights. This is the 11.7
// check: a tenant-spawned agent runs with tier rights.

// newThreadInLandFixture adds tenant land plot-a's project bead and a thread
// agent in it to the land fixture. The returned context is the thread agent's
// per-agent token, bound to plot-a as the controller mints it.
func newThreadInLandFixture(t *testing.T) (*landFixture, context.Context) {
	t.Helper()
	f := newLandFixture(t)
	makeRoleBead(f.ms, "gd-role-thread", "thread", false)
	f.ms.beads["gd-proj-a"] = &model.Bead{ID: "gd-proj-a", Type: model.TypeProject, Title: "plot-a",
		Status: model.StatusOpen, Fields: json.RawMessage(`{"land_kind":"tenant"}`)}
	f.ms.beads["gd-thr"] = &model.Bead{ID: "gd-thr", Type: "agent", Fields: agentFields("plot-a", "thread")}
	ctx := WithTokenProject(withAuthMethod(agentFleetCtx("gd-thr", "thread-a"), authMethodStatic), "plot-a")
	return f, ctx
}

func TestLandsTenantByLand_ThreadAgentRunsWithTierRights(t *testing.T) {
	f, thread := newThreadInLandFixture(t)

	caller, err := landsTenantCallerFrom(thread, f.ms)
	if err != nil || caller == nil || !caller.ByLand || caller.Project != "plot-a" || caller.isSteward() || caller.Home {
		t.Fatalf("thread agent in tenant land: caller = %+v, %v; want a by-land, non-steward tenant of plot-a", caller, err)
	}

	// It spawns nothing, not even lands-gardener in its own land.
	for _, role := range []string{"thread", "crew", landsGardenerRole} {
		requireTenantDenial(t, authorizeAgentSpawn(thread, f.ms, agentFields("plot-a", role)))
	}
	// It files no escalation but a merge about its own land.
	srv := &BeadsServer{store: f.ms}
	for _, op := range []string{"k8s_apply", "helm_deploy", "secret_rotate", "manual_operator_action"} {
		esc := &model.Bead{Type: model.TypeEscalation, Labels: []string{"project:plot-a"},
			Fields: json.RawMessage(`{"operation_type":"` + op + `"}`)}
		if d := srv.restrictedOpDenial(thread, restrictedOpRequest{Op: opEscalationFile, Action: "create",
			BeadType: model.TypeEscalation, Escalation: esc}); d == nil || d.DeniedReason != landsTenantDeniedReason {
			t.Errorf("escalation %s from a thread agent in a tenant land: got %+v, want a lands_tenant denial", op, d)
		}
	}
	for project, allowed := range map[string]bool{"plot-a": true, "gardenio": false} {
		esc := &model.Bead{Type: model.TypeEscalation, Labels: []string{"project:" + project},
			Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`)}
		d := srv.restrictedOpDenial(thread, restrictedOpRequest{Op: opEscalationFile, Action: "create",
			BeadType: model.TypeEscalation, Escalation: esc})
		if (d == nil) != allowed {
			t.Errorf("merge escalation about %s: denial %+v, want allowed=%v", project, d, allowed)
		}
	}
	// It writes no bead that configures the fleet.
	for _, bt := range []model.BeadType{"config", model.TypeAdvice, "role", "trigger"} {
		if d := landsTenantTypeDenial(thread, f.ms, "create", bt); d == nil {
			t.Errorf("%s write from a thread agent in a tenant land must be refused", bt)
		}
	}
}

// A steward whose role bead holds no tier is a tenant by its land only, and
// gets none of the steward's carve-outs: it approves no own-land merge, files
// no gatehouse_request and does not own its land's notes. The control is the
// same agent once the role bead holds the tier.
func TestLandsTenantByLand_UntieredStewardGetsNoCarveOuts(t *testing.T) {
	f, ctx := newThreadInLandFixture(t)
	f.ms.beads["gd-thr"].Fields = agentFields("plot-a", landsStewardRole)
	f.ms.beads["gd-proj-a"].Fields = json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true,"land_gatehouse_tools":true}`)
	srv := &BeadsServer{store: f.ms}
	merge := &model.Bead{Type: model.TypeEscalation, Labels: []string{"project:plot-a"},
		Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`)}
	notes := &model.Bead{ID: "gd-n", Type: typeDoc, Kind: "data", Title: "steward-notes:plot-a", Labels: []string{"project:plot-a"}, CreatedBy: "controller"}
	gates := func() map[string]bool {
		return map[string]bool{
			"approve own-land merge": srv.restrictedOpDenial(ctx, restrictedOpRequest{Op: opEscalationApprove, Action: "approve",
				BeadType: model.TypeEscalation, Escalation: merge}) == nil,
			"gatehouse_request create": srv.restrictedOpDenial(ctx, restrictedOpRequest{Op: opGatehouseRequest, Action: "create",
				BeadType: typeGatehouseRequest, Project: "plot-a"}) == nil,
			"steward notes update": srv.authorizeOwnership(ctx, "update", notes) == nil,
		}
	}
	for _, tier := range []string{`{}`, `{"capability_tiers":["lands-tenant"]}`} {
		makeTierRoleBead(f.ms, "gd-role-steward", landsStewardRole, tier)
		for gate, allowed := range gates() {
			if want := tier != `{}`; allowed != want {
				t.Errorf("steward role %s: %s allowed = %v, want %v", tier, gate, allowed, want)
			}
		}
	}
}

// A restrict taint still binds a ByLand caller: the tier exempts only an agent
// whose role holds it, and the config route (PUT /v1/configs) checks the
// taint with no tier check before it. The tiered steward control is exempt.
func TestLandsTenantByLand_RestrictTaintStillBinds(t *testing.T) {
	f, thread := newThreadInLandFixture(t)
	seedRestrictTaint(t, f.ms, 11, untrustedInputTaint, model.EffectRestrict)
	taintAgent(t, f.ms, "gd-thr", 11)
	taintAgent(t, f.ms, "gd-stew", 11)
	srv := NewBeadsServer(f.ms, &events.NoopPublisher{})
	for _, bt := range []model.BeadType{"config", model.TypeAdvice} {
		req := restrictedOpRequest{Op: opTypeWrite, Action: "update", BeadType: bt}
		requireTaintDenial(t, srv.restrictTaintOpDenial(thread, req))
		if d := srv.restrictTaintOpDenial(f.steward, req); d != nil {
			t.Fatalf("tainted tiered steward %s write: want the tier's exemption, got %+v", bt, d)
		}
	}
}

// The token's project is the binding: rewriting the agent bead's project does
// not lift the restriction. The bead's project alone binds too, for a token
// minted without a project scope.
func TestLandsTenantByLand_BindingFromTokenOrBead(t *testing.T) {
	f, thread := newThreadInLandFixture(t)
	f.ms.beads["gd-thr"].Fields = agentFields("gardenio", "thread")
	if c, err := landsTenantCallerFrom(thread, f.ms); err != nil || c == nil || c.Project != "plot-a" {
		t.Fatalf("token bound to plot-a, bead says gardenio: caller = %+v, %v; want a tenant of plot-a", c, err)
	}

	f.ms.beads["gd-thr"].Fields = agentFields("plot-a", "thread")
	unscoped := withAuthMethod(agentFleetCtx("gd-thr", "thread-a"), authMethodStatic)
	if c, err := landsTenantCallerFrom(unscoped, f.ms); err != nil || c == nil || !c.ByLand {
		t.Fatalf("unscoped token, bead in plot-a: caller = %+v, %v; want a by-land tenant", c, err)
	}
	// An agent with no role at all is still bound to its land.
	f.ms.beads["gd-thr"].Fields = agentFields("plot-a", "")
	if c, err := landsTenantCallerFrom(thread, f.ms); err != nil || c == nil || !c.ByLand {
		t.Fatalf("role-less agent in plot-a: caller = %+v, %v; want a by-land tenant", c, err)
	}
}

// Controls: an operator land, the home garden and a project with no project
// bead bind nobody, so a thread or crew agent there keeps its rights.
func TestLandsTenantByLand_OperatorProjectsUnaffected(t *testing.T) {
	f, _ := newThreadInLandFixture(t)
	f.ms.beads["gd-proj-op"] = &model.Bead{ID: "gd-proj-op", Type: model.TypeProject, Title: "home",
		Status: model.StatusOpen, Fields: json.RawMessage(`{"land_kind":"operator"}`)}
	for _, project := range []string{"home", "gardenio"} {
		f.ms.beads["gd-thr"].Fields = agentFields(project, "thread")
		ctx := WithTokenProject(withAuthMethod(agentFleetCtx("gd-thr", "thread-a"), authMethodStatic), project)
		if c, err := landsTenantCallerFrom(ctx, f.ms); err != nil || c != nil {
			t.Errorf("thread agent in %s: caller = %+v, %v; want not a tenant", project, c, err)
		}
		if err := authorizeAgentSpawn(ctx, f.ms, agentFields(project, "crew")); err != nil {
			t.Errorf("thread agent in %s spawning crew: %v", project, err)
		}
	}
}

// A project lookup that fails refuses: the caller cannot be shown not to be a
// tenant.
func TestLandsTenantByLand_LookupFailsClosed(t *testing.T) {
	f, thread := newThreadInLandFixture(t)
	st := projectLookupErrStore{f.ms}
	if _, err := landsTenantCallerFrom(thread, st); err == nil {
		t.Fatal("a project lookup error must be an error")
	}
	var d *AuthzDenial
	if err := authorizeAgentSpawn(thread, st, agentFields("plot-a", "crew")); !errors.As(err, &d) || d.Escalatable {
		t.Fatalf("a project lookup error must refuse the spawn, got %v", err)
	}
}

type projectLookupErrStore struct {
	*mockStore
}

func (s projectLookupErrStore) FindProjectByName(context.Context, string) (*model.Bead, error) {
	return nil, errors.New("store unavailable")
}
