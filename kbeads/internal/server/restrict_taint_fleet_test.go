package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// A restrict taint refuses a fleet agent's spawn, approval and fleet-type
// write, not only replay (gd-gaGdksntAG). A tenant agent is held to its tier
// instead, so the always-tainted steward keeps its carve-outs.

func requireTaintDenial(t *testing.T, d *AuthzDenial) {
	t.Helper()
	if d == nil || d.DeniedReason != deniedReasonTaintRestrict || d.Escalatable || !slices.Equal(d.TaintNames, []string{untrustedInputTaint}) {
		t.Fatalf("want a non-escalatable taint_restrict denial naming %s, got %+v", untrustedInputTaint, d)
	}
}

func TestRestrictTaint_FleetAgentOps(t *testing.T) {
	f := newLandFixture(t)
	seedRestrictTaint(t, f.ms, 11, untrustedInputTaint, model.EffectRestrict)
	taintAgent(t, f.ms, "gd-crew1", 11)
	taintAgent(t, f.ms, "gd-stew", 11)
	srv := NewBeadsServer(f.ms, &events.NoopPublisher{})

	spawn := restrictedOpRequest{Op: opSpawn, Action: "create", BeadType: typeAgent, Role: "crew", Project: "gardenio"}
	approve := restrictedOpRequest{Op: opEscalationApprove, Action: actionApprove, BeadType: model.TypeEscalation, Escalation: mergeEscalation("github_pr_merge", "project:gardenio")}
	fleetWrite := restrictedOpRequest{Op: opTypeWrite, Action: "create", BeadType: model.TypeAdvice}
	for _, req := range []restrictedOpRequest{spawn, approve, fleetWrite} {
		requireTaintDenial(t, srv.restrictedOpDenial(f.crew, req))
	}
	for _, req := range []restrictedOpRequest{
		{Op: opEscalationFile, Action: "create", BeadType: model.TypeEscalation, Escalation: mergeEscalation("github_pr_merge", "project:gardenio")},
		{Op: opEscalationReject, Action: "reject", BeadType: model.TypeEscalation},
	} {
		if d := srv.restrictedOpDenial(f.crew, req); d != nil {
			t.Fatalf("a tainted agent may still %s, got %+v", req.Op, d)
		}
	}

	// The tainted steward keeps what its tier admits.
	stewardSpawn := restrictedOpRequest{Op: opSpawn, Action: "create", BeadType: typeAgent, Role: landsGardenerRole, Project: "plot-a"}
	stewardApprove := restrictedOpRequest{Op: opEscalationApprove, Action: actionApprove, BeadType: model.TypeEscalation, Escalation: mergeEscalation("github_pr_merge", "project:plot-a")}
	for _, req := range []restrictedOpRequest{stewardSpawn, stewardApprove} {
		if d := srv.restrictedOpDenial(f.steward, req); d != nil {
			t.Fatalf("tainted steward %s: want the tier's carve-out, got %+v", req.Op, d)
		}
	}

	exp := time.Now().Add(time.Hour)
	if err := f.ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 11, PrincipalKind: model.PrincipalAgent, Principal: "gd-crew1", GrantedBy: 1, ExpiresAt: &exp}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}
	srv = NewBeadsServer(f.ms, &events.NoopPublisher{}) // fresh taint-context cache
	for _, req := range []restrictedOpRequest{spawn, approve, fleetWrite} {
		if d := srv.restrictedOpDenial(f.crew, req); d != nil {
			t.Fatalf("a granted agent is tolerated for %s, got %+v", req.Op, d)
		}
	}
}

// Over HTTP: the create, write and config gates each refuse the tainted
// agent, and its ordinary work still lands.
func TestRestrictTaint_HTTPFleetAgentGates(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	taintAgent(t, ms, "gd-a1", 11)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"spawn": doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, map[string]any{
			"title": "child", "type": "agent", "labels": []string{"project:gardenio"},
			"fields": map[string]any{"agent": "child-a", "project": "gardenio", "role": "crew"},
		}, nil),
		"advice": doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, map[string]any{
			"title": "steered advice", "type": "advice", "labels": []string{"project:gardenio"},
		}, nil),
		"reaction-rule": doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, map[string]any{
			"title": "steered rule", "type": "reaction-rule", "kind": "config", "labels": []string{"project:gardenio"},
			"fields": map[string]any{"emoji": "eyes", "prompt": "p", "project": "gardenio"},
		}, nil),
		"config kv": doBearerJSON(t, h, "PUT", "/v1/configs/view:steered", testAgentToken, map[string]any{"value": map[string]any{}}, nil),
	} {
		requireStatus(t, rec, http.StatusForbidden)
		var d AuthzDenial
		decodeJSON(t, rec, &d)
		if d.DeniedReason != deniedReasonTaintRestrict {
			t.Fatalf("%s by a tainted agent: want taint_restrict, got %+v", name, d)
		}
	}
	ms.beads["gd-adv1"] = &model.Bead{ID: "gd-adv1", Type: model.TypeAdvice, Status: model.StatusOpen}
	ms.labels["gd-adv1"] = []string{"project:gardenio"}
	fieldRec := doBearerJSON(t, h, "POST", "/v1/beads/gd-adv1/increment", testAgentToken, map[string]float64{"hits": 1}, nil)
	requireStatus(t, fieldRec, http.StatusForbidden)
	var fd AuthzDenial
	if decodeJSON(t, fieldRec, &fd); fd.DeniedReason != deniedReasonTaintRestrict {
		t.Fatalf("field endpoint by a tainted agent: want taint_restrict, got %+v", fd)
	}
	task := map[string]any{"title": "do a thing", "type": "task", "labels": []string{"project:gardenio"}}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, task, nil), http.StatusCreated)
}

// A role change on an agent bead is a spawn by another route: the tainted
// agent is refused it, an untainted one is not.
func TestRestrictTaint_HTTPAgentRolePatch(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	makeRoleBead(ms, "gd-role-rev", "reviewer", false)
	for _, id := range []string{"gd-a1", "gd-a2"} {
		ms.beads[id] = &model.Bead{ID: id, Title: id, Kind: model.KindConfig, Type: "agent", Status: model.StatusOpen, Fields: agentFields("gardenio", "crew")}
		ms.labels[id] = []string{"project:gardenio"}
	}
	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	taintAgent(t, ms, "gd-a1", 11)
	patch := map[string]any{"fields": map[string]any{"role": "reviewer"}}

	rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-a1", testAgentToken, patch, nil)
	requireStatus(t, rec, http.StatusForbidden)
	var d AuthzDenial
	if decodeJSON(t, rec, &d); d.DeniedReason != deniedReasonTaintRestrict {
		t.Fatalf("role PATCH by a tainted agent: want taint_restrict, got %+v", d)
	}
	requireStatus(t, doBearerJSON(t, h, "PATCH", "/v1/beads/gd-a2", testAgentToken2, patch, nil), http.StatusOK)
}
