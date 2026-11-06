package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/model"
)

// Tests for gd-Ms3cmrdW49: the untrusted-input taint fails closed on a tenant
// land, and a restrict taint is tolerated only by a grant to the agent itself.

func unusableCount(land, reason string) float64 {
	return testutil.ToFloat64(landsUntrustedInputUnusableTotal.WithLabelValues(land, reason))
}

// A tenant land cannot be provisioned, or an operator land turned into one,
// while untrusted-input is unregistered or not restrict-only; an existing
// tenant land's own edits still go through, and its bead creates are refused.
func TestTenantLand_RefusedWhileUntrustedInputUnusable(t *testing.T) {
	s, ms, h := newTestServer()
	project := func(title, kind string) map[string]any {
		fields := map[string]any{"prefix": "tp"}
		if kind != "" {
			fields["land_kind"] = kind
		}
		return map[string]any{"title": title, "type": "project", "kind": "config", "fields": fields}
	}
	wantUnusable := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403: %s", what, rec.Code, rec.Body.String())
		}
		var d AuthzDenial
		decodeJSON(t, rec, &d)
		if d.DeniedReason != deniedReasonUntrustedInputUnusable || d.Escalatable {
			t.Fatalf("%s: want a non-escalatable %s denial, got %+v", what, deniedReasonUntrustedInputUnusable, d)
		}
	}

	before := unusableCount("bean-row", untrustedInputUnregistered)
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", project("bean-row", "tenant"))
	wantUnusable("tenant land, taint unregistered", rec)
	if got := unusableCount("bean-row", untrustedInputUnregistered) - before; got != 1 {
		t.Fatalf("unusable counter rose by %v, want 1", got)
	}

	rec = doJSON(t, h, http.MethodPost, "/v1/beads", project("herb-bed", ""))
	requireStatus(t, rec, http.StatusCreated)
	var op model.Bead
	decodeJSON(t, rec, &op)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+op.ID, map[string]any{"fields": map[string]any{"land_kind": "tenant"}})
	wantUnusable("operator land made a tenant land", rec)

	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict, model.EffectGate)
	s.InvalidateTaintRegistryCache()
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", project("bean-row", "tenant"))
	wantUnusable("tenant land, gate taint", rec)

	// A land provisioned before the taint went bad keeps its own edits, and
	// refuses bead creates rather than make them untainted.
	ms.beads["gd-p-pea"] = &model.Bead{ID: "gd-p-pea", Type: "project", Kind: model.KindConfig, Status: model.StatusOpen, Title: "pea-row", Fields: []byte(`{"land_kind":"tenant"}`)}
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/gd-p-pea", map[string]any{"fields": map[string]any{"land_gatehouse": true}})
	requireStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{"title": "a stranger's task", "type": "task", "labels": []string{"project:pea-row"}})
	wantUnusable("bead create in a tenant land, gate taint", rec)

	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", project("bean-row", "tenant"))
	requireStatus(t, rec, http.StatusCreated)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{"title": "a stranger's task", "type": "task", "labels": []string{"project:pea-row"}})
	requireStatus(t, rec, http.StatusCreated)
	var task model.Bead
	decodeJSON(t, rec, &task)
	if rows := ms.beadTaints[task.ID]; len(rows) != 1 || rows[0].TaintID != 11 {
		t.Fatalf("tenant bead taints = %+v, want untrusted-input", rows)
	}
}

// The taint registry refuses an untrusted-input that would wedge every tenant
// land: one without restrict, or with gate.
func TestHandleCreateTaint_UntrustedInputMustBeRestrictOnly(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	for _, effects := range [][]string{nil, {model.EffectGate}, {model.EffectRestrict, model.EffectGate}} {
		rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
			"name": untrustedInputTaint, "owner_team": "gd-team-ops", "effects": effects,
		}, "admin@example.com")
		requireStatus(t, rec, http.StatusBadRequest)
		if _, ok := ms.taintRegistry[untrustedInputTaint]; ok {
			t.Fatalf("effects %v: an unusable untrusted-input was stored", effects)
		}
	}
	rec := taintDoJSONAs(t, h, "POST", "/v1/taints", map[string]any{
		"name": untrustedInputTaint, "owner_team": "gd-team-ops", "effects": []string{model.EffectRestrict},
	}, "admin@example.com")
	requireStatus(t, rec, http.StatusCreated)
}

// grantsOfAgent walks the spawn chain of the agent it names, not of the
// caller, which at escalation replay is the approver.
func TestGrantsOfAgent_WalksNamedAgentNotCaller(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 2, "alice-own", "some-team")
	seedOwnerAgentBead(t, ms, "alice-root", map[string]any{"operated_by": "oidc:alice@example.com"})
	seedOwnerAgentBead(t, ms, "alice-leaf", map[string]any{"spawned_by": "alice-root"})
	seedOwnerAgentBead(t, ms, "bob-approver", map[string]any{"operated_by": "oidc:bob@example.com"})
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7, "oidc\x00bob@example.com": 8}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 2, PrincipalKind: model.PrincipalOperator, Principal: "7", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	grants, err := s.grantsOfAgent(staticAuthCtx("bob-approver"), "alice-leaf")
	if err != nil {
		t.Fatalf("grantsOfAgent: %v", err)
	}
	if !slices.Contains(grants, 2) {
		t.Fatalf("grantsOfAgent(alice-leaf) as bob = %v, want alice's grant 2", grants)
	}
}

// A grant to the agent's spawn-chain root (the operator) does not tolerate a
// restrict taint the agent carries, and does not make it a trusted subject on
// read; a grant to the agent itself does both.
func TestRestrictTaint_RootGrantDoesNotTolerate(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	seedOwnerAgentBead(t, ms, "op-root", map[string]any{"operated_by": "oidc:alice@example.com"})
	seedOwnerAgentBead(t, ms, "op-leaf", map[string]any{"spawned_by": "op-root"})
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}
	ctx := context.Background()
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 11, PrincipalKind: model.PrincipalOperator, Principal: "7", GrantedBy: 1}); err != nil {
		t.Fatalf("seed operator grant: %v", err)
	}

	ms.beads["kd-tenant"] = &model.Bead{ID: "kd-tenant", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tenant", 11)
	requireStatus(t, getBeadAsAgent(t, s, "kd-tenant", "op-leaf"), http.StatusOK)
	if _, ok := beadTaintByID(ms.beadTaints["op-leaf"], 11); !ok {
		t.Fatalf("an agent rooted at a granted operator must still acquire the restrict taint on read, got %v", ms.beadTaints["op-leaf"])
	}

	replay := restrictedOpRequest{Op: opEscalationReplay, Action: "create", BeadType: model.TypeEscalation, Agent: "op-leaf"}
	if d := s.restrictedOpDenial(staticAuthCtx("op-root"), replay); d == nil || d.DeniedReason != deniedReasonTaintRestrict {
		t.Fatalf("root's grant tolerated the agent's restrict taint, got %+v", d)
	}

	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 11, PrincipalKind: model.PrincipalAgent, Principal: "op-leaf", GrantedBy: 1}); err != nil {
		t.Fatalf("seed agent grant: %v", err)
	}
	s.invalidateAgentGrantSet("op-leaf")
	if d := s.restrictedOpDenial(staticAuthCtx("op-root"), replay); d != nil {
		t.Fatalf("the agent's own grant tolerates, got %+v", d)
	}
}
