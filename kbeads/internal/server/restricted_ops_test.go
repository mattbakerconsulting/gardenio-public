package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// The shared restricted-ops check (restricted_ops.go, Lands Track B §5.1,
// step 10.1 gd-g77WcV22SK).

// Every op has a reviewed decision: a lands-tenant rule, a restrict-taint
// rule, or both. A new op fails here until someone decides.
func TestLandsTenantRules_EveryOp(t *testing.T) {
	want := map[restrictedOp]tenantRule{
		opSpawn:             tenantStewardSpawnsGardener,
		opEscalationFile:    tenantOwnLandMerge,
		opEscalationRetry:   tenantNever,
		opEscalationApprove: tenantStewardOwnLandMerge,
		opEscalationReject:  tenantNever,
		opTypeWrite:         tenantNever,
		opGatehouseRequest:  tenantGatehouseSteward,
	}
	for op, rule := range want {
		if got, ok := landsTenantRules[op]; !ok || got != rule {
			t.Errorf("landsTenantRules[%s] = %v, %v; want %v", op, got, ok, rule)
		}
	}
	for op := range landsTenantRules {
		if _, ok := want[op]; !ok {
			t.Errorf("landsTenantRules has %s with no reviewed decision here", op)
		}
	}
	wantTaint := map[restrictedOp]bool{opEscalationReplay: true, opSpawn: true, opEscalationApprove: true, opTypeWrite: true}
	if !maps.Equal(restrictTaintOps, wantTaint) {
		t.Errorf("restrictTaintOps = %v; want %v", restrictTaintOps, wantTaint)
	}
}

// Every known type x every reviewed action: landsTenantWrites says exactly
// what this reviewed table says, so a type is closed to tenant agents until a
// decision is written here (gd-bRLiEzvMsy). The rows not listed, among them
// reaction-rule, widget-type, dashboard, schedule, script, alert_rule,
// alert_mute, spawn_policy, escalation_policy and trigger, are refused.
func TestLandsTenantWrites_EveryType(t *testing.T) {
	work := []string{"create", "update", "close", "comment"}
	reviewed := map[model.BeadType][]string{
		model.TypeTask:       work,
		model.TypeBug:        work,
		model.TypeFeature:    work,
		model.TypeChore:      work,
		model.TypeEpic:       work,
		model.TypeDecision:   work,
		model.TypeEscalation: work,
		model.TypeReport:     {"create", "update", "comment"},
		model.TypeBundle:     {"create", "update", "comment"},
		typeMessage:          work,
		typeDoc:              {"create", "update", "comment"},
		typeAgent:            {"create", "update", "close", "comment", "hook-emit"},
		"session":            {"create", "update", "close"},
		typeGatehouseRequest: {"create"},
	}
	types := append(allWritableTypeCandidates(), "reaction-rule", "widget-type", "dashboard", "schedule", "script",
		"alert_rule", "alert_mute", "spawn_policy", "escalation_policy", "custom-type-added-later")
	for typ := range landsTenantWrites {
		if !slices.Contains(types, typ) {
			t.Errorf("landsTenantWrites names type %q, which no registry or model constant knows", typ)
		}
	}
	for _, typ := range types {
		for _, act := range reviewedActions {
			if got, want := landsTenantWrites[typ][act], slices.Contains(reviewed[typ], act); got != want {
				t.Errorf("landsTenantWrites[%s][%s] = %v, want %v", typ, act, got, want)
			}
		}
	}
}

func mergeEscalation(opType string, labels ...string) *model.Bead {
	f, _ := json.Marshal(map[string]any{"operation_type": opType})
	return &model.Bead{ID: "gd-esc", Type: model.TypeEscalation, Fields: f, Labels: labels}
}

// A tenant agent files a merge escalation about its own land and nothing
// else; any other caller is not held to the tenant rule.
func TestLandsTenantEscalation_FileOwnLandMerge(t *testing.T) {
	f := newLandFixture(t)
	srv := NewBeadsServer(f.ms, &events.NoopPublisher{})
	for _, tc := range []struct {
		name  string
		esc   *model.Bead
		allow bool
	}{
		{"github merge in its land", mergeEscalation("github_pr_merge", "project:plot-a"), true},
		{"gitlab merge in its land", mergeEscalation("gitlab_mr_merge", "project:plot-a"), true},
		{"merge in another land", mergeEscalation("github_pr_merge", "project:plot-b"), false},
		{"merge naming two lands", mergeEscalation("github_pr_merge", "project:plot-a", "project:plot-b"), false},
		{"merge with no land", mergeEscalation("github_pr_merge"), false},
		{"other operation in its land", mergeEscalation("manual_operator_action", "project:plot-a"), false},
		{"grant in its land", mergeEscalation("grant", "project:plot-a"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := restrictedOpRequest{Op: opEscalationFile, Action: "create", BeadType: model.TypeEscalation, Escalation: tc.esc}
			for _, who := range []context.Context{f.steward, f.gardener} {
				d := srv.restrictedOpDenial(who, req)
				if tc.allow && d != nil {
					t.Fatalf("want allowed, got %+v", d)
				}
				if !tc.allow {
					requireTenantDenial(t, d)
				}
			}
			if d := srv.restrictedOpDenial(f.crew, req); d != nil {
				t.Fatalf("a crew agent is not a tenant, got %+v", d)
			}
		})
	}
	// The retry path (X-Escalate) files only replays and grant requests, so it
	// stays closed even to a merge.
	requireTenantDenial(t, srv.restrictedOpDenial(f.steward, restrictedOpRequest{Op: opEscalationRetry, Action: "create", BeadType: model.TypeEscalation}))
	// Approving: the steward may try a merge in its land; the gardener may not.
	approve := restrictedOpRequest{Op: opEscalationApprove, Action: actionApprove, BeadType: model.TypeEscalation, Escalation: mergeEscalation("github_pr_merge", "project:plot-a")}
	if d := srv.restrictedOpDenial(f.steward, approve); d != nil {
		t.Fatalf("steward approving a merge in its land: want allowed, got %+v", d)
	}
	requireTenantDenial(t, srv.restrictedOpDenial(f.gardener, approve))
}

// Over HTTP: the tenant's merge escalation in its own land is filed, and a
// fleet-configuring bead in its own project is refused, while a crew agent on
// the same token class is unaffected.
func TestLandsTenant_HTTPFileMergeAndFleetTypes(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	makeTierRoleBead(ms, "gd-role-lg", landsGardenerRole, `{"capability_tiers":["lands-tenant"]}`)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", landsGardenerRole)}

	merge := map[string]any{
		"title": "merge PR 7", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"operation_type": "github_pr_merge", "operation_spec": map[string]any{"pr": 7}},
	}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, merge, nil), http.StatusCreated)

	// The reaction-rule is the gd-bRLiEzvMsy probe: the slack-bridge spawns
	// a fleet agent from one, so it must not be a tenant's to write.
	reactionRule := map[string]any{"emoji": "eyes", "prompt": "p", "project": "gardenio"}
	for _, typ := range []string{"config", "role", "advice", "user", "reaction-rule", "dashboard"} {
		fields := map[string]any{}
		if typ == "reaction-rule" {
			fields = reactionRule
		}
		body := map[string]any{"title": "x-" + typ, "type": typ, "kind": "config", "labels": []string{"project:gardenio"}, "fields": fields}
		rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil)
		requireStatus(t, rec, http.StatusForbidden)
		var d AuthzDenial
		decodeJSON(t, rec, &d)
		if d.DeniedReason != landsTenantDeniedReason || d.Escalatable || !strings.Contains(d.Message, "#support") {
			t.Fatalf("tenant %s create: want a lands_tenant denial pointing at #support, got %+v", typ, d)
		}
	}
	task := map[string]any{"title": "do a thing", "type": "task", "labels": []string{"project:gardenio"}}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, task, nil), http.StatusCreated)

	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	cfg := map[string]any{"title": "crew-config", "type": "config", "kind": "config", "labels": []string{"project:gardenio"}, "fields": map[string]any{}}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, cfg, nil); rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), landsTenantDeniedReason) {
		t.Fatalf("crew config create refused by the tenant rule: %s", rec.Body.String())
	}
}

func seedRestrictTaint(t *testing.T, ms *mockStore, id int64, name string, effects ...string) {
	t.Helper()
	if err := ms.SetTaint(context.Background(), &model.Taint{ID: id, Name: name, OwnerTeam: "team-x", Effects: effects}); err != nil {
		t.Fatalf("SetTaint(%s): %v", name, err)
	}
	got, err := ms.GetTaint(context.Background(), name)
	if err != nil || got.ID != id {
		t.Fatalf("GetTaint(%s) = %+v, %v; want id %d", name, got, err, id)
	}
}

// registerUntrustedInput registers the untrusted-input taint restrict-only,
// as the operator does before any tenant land is provisioned, unless it is
// already registered.
func registerUntrustedInput(t *testing.T, ms *mockStore) {
	t.Helper()
	if _, err := ms.GetTaint(context.Background(), untrustedInputTaint); err == nil {
		return
	}
	seedRestrictTaint(t, ms, 9011, untrustedInputTaint, model.EffectRestrict)
}

func taintAgent(t *testing.T, ms *mockStore, agent string, taintID int64) {
	t.Helper()
	if _, err := ms.ApplyTaint(context.Background(), &model.BeadTaint{BeadID: agent, TaintID: taintID, Source: model.TaintSourceInheritRead, AppliedBy: "test"}); err != nil {
		t.Fatalf("ApplyTaint: %v", err)
	}
}

// An agent carrying a restrict taint is refused a replay; a grant tolerates
// it; a gate-only taint is not a restriction.
func TestRestrictTaint_RefusesReplayUntilGranted(t *testing.T) {
	ms := newMockStore()
	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	seedRestrictTaint(t, ms, 12, "pii", model.EffectGate)
	ms.beads["gd-crew1"] = &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	ms.beads["gd-crew2"] = &model.Bead{ID: "gd-crew2", Type: "agent", Fields: agentFields("gardenio", "crew")}
	taintAgent(t, ms, "gd-crew1", 11)
	taintAgent(t, ms, "gd-crew2", 12)
	req := func(agent string) restrictedOpRequest {
		return restrictedOpRequest{Op: opEscalationReplay, Action: "create", BeadType: model.TypeEscalation, Agent: agent}
	}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	d := srv.restrictedOpDenial(context.Background(), req("gd-crew1"))
	if d == nil || d.DeniedReason != deniedReasonTaintRestrict || d.Escalatable || !slices.Equal(d.TaintNames, []string{untrustedInputTaint}) {
		t.Fatalf("restrict-tainted replay: want a taint_restrict denial naming %s, got %+v", untrustedInputTaint, d)
	}
	if d := srv.restrictedOpDenial(context.Background(), req("gd-crew2")); d != nil {
		t.Fatalf("a gate-only taint restricts nothing, got %+v", d)
	}

	exp := time.Now().Add(time.Hour)
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 11, PrincipalKind: model.PrincipalAgent, Principal: "gd-crew1", GrantedBy: 1, ExpiresAt: &exp}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}
	srv = NewBeadsServer(ms, &events.NoopPublisher{}) // fresh taint-context cache
	if d := srv.restrictedOpDenial(context.Background(), req("gd-crew1")); d != nil {
		t.Fatalf("a granted agent is tolerated, got %+v", d)
	}
}

// replayEscalation records the refusal and runs nothing.
func TestRestrictTaint_ReplayEscalationRunsNothing(t *testing.T) {
	ms := newMockStore()
	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	ms.beads["gd-crew1"] = &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	taintAgent(t, ms, "gd-crew1", 11)
	spec, _ := json.Marshal(createBeadInput{Title: "steered", Type: "task", Labels: []string{"project:gardenio"}})
	fields, _ := json.Marshal(map[string]any{"operation_type": "create", "operation_spec": json.RawMessage(spec), "requesting_agent": "gd-crew1"})
	esc := &model.Bead{ID: "gd-esc-r", Type: model.TypeEscalation, Kind: model.KindData, Status: model.StatusOpen, Fields: fields, Labels: []string{"project:gardenio"}}
	ms.beads[esc.ID] = esc
	before := len(ms.beads)

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	res := srv.replayEscalation(adminOIDCCtx(), esc, "oidc:admin@example.com", nil)
	if res == nil || res["status"] != "failure" || !strings.Contains(res["error"].(string), untrustedInputTaint) {
		t.Fatalf("replay by a restrict-tainted requester: want a failure naming the taint, got %v", res)
	}
	if len(ms.beads) != before {
		t.Fatalf("the refused replay created a bead (%d beads, want %d)", len(ms.beads), before)
	}
}

// A merge escalation is executed by the bridge from the approval event, not by
// replay, so a restrict-tainted requester's github_pr_merge approve records no
// result: refusing it here would refuse nothing and stamp a false failure (ADR
// §15.7: the merge desk can still move a tenant's PRs).
func TestRestrictTaint_ApproveMergeRecordsNoResult(t *testing.T) {
	_, ms, h := newTestServer()
	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	ms.beads["gd-crew1"] = &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	taintAgent(t, ms, "gd-crew1", 11)
	spec, _ := json.Marshal(map[string]any{"mr_url": "https://github.com/acme/app/pull/1"})
	fields, _ := json.Marshal(map[string]any{"operation_type": "github_pr_merge", "operation_spec": json.RawMessage(spec), "requesting_agent": "gd-crew1"})
	ms.beads["gd-esc-m"] = &model.Bead{ID: "gd-esc-m", Type: model.TypeEscalation, Kind: model.KindData, Status: model.StatusOpen, Fields: fields, Labels: []string{"project:gardenio"}}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-m/approve", map[string]any{})
	requireStatus(t, rec, 200)
	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if r, ok := resp["replay"]; ok {
		t.Fatalf("a merge approve is not replayed by the daemon, got replay %v", r)
	}
	var got map[string]any
	if err := json.Unmarshal(ms.beads["gd-esc-m"].Fields, &got); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if rs, ok := got["result_status"]; ok {
		t.Fatalf("a tainted requester's merge approve recorded result_status %v (error %v), want none", rs, got["result"])
	}
}

// Every bead created in a tenant land carries untrusted-input, when the taint
// is registered restrict-only; an operator land does not. While the taint is
// unusable, a tenant bead create is refused rather than made untainted.
func TestTenantLandIntake_UntrustedInput(t *testing.T) {
	ms := newMockStore()
	ms.beads["gd-p-a"] = &model.Bead{ID: "gd-p-a", Type: "project", Title: "plot-a", Fields: json.RawMessage(`{"land_kind":"tenant"}`)}
	ms.beads["gd-p-op"] = &model.Bead{ID: "gd-p-op", Type: "project", Title: "gardenio", Fields: json.RawMessage(`{}`)}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	ctx := context.Background()

	wantUnusable := func(what, reason string) {
		t.Helper()
		before := unusableCount("plot-a", reason)
		got, err := srv.resolveProjectIntakeTaints(ctx, "plot-a")
		var d *AuthzDenial
		if !errors.As(err, &d) || d.DeniedReason != deniedReasonUntrustedInputUnusable || len(got) != 0 {
			t.Fatalf("%s: got %+v, %v; want an %s denial", what, got, err, deniedReasonUntrustedInputUnusable)
		}
		if n := unusableCount("plot-a", reason) - before; n != 1 {
			t.Fatalf("%s: counter{reason=%s} rose by %v, want 1", what, reason, n)
		}
	}

	// Not registered yet: the create is refused.
	seedRestrictTaint(t, ms, 12, "pii", model.EffectGate)
	srv.InvalidateTaintRegistryCache()
	wantUnusable("unregistered untrusted-input", untrustedInputUnregistered)
	if got, err := srv.resolveProjectIntakeTaints(ctx, "gardenio"); err != nil || len(got) != 0 {
		t.Fatalf("operator land, unregistered: got %+v, %v; want none", got, err)
	}

	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict)
	got, err := srv.resolveProjectIntakeTaints(ctx, "plot-a")
	if err != nil || len(got) != 1 || got[0].TaintID != 11 || got[0].Source != model.TaintSourceInheritProjectIntake {
		t.Fatalf("tenant land: got %+v, %v; want untrusted-input from project intake", got, err)
	}
	if got, err := srv.resolveProjectIntakeTaints(ctx, "gardenio"); err != nil || len(got) != 0 {
		t.Fatalf("operator land: got %+v, %v; want none", got, err)
	}

	// A gate effect would hide the land's Commons from its members: refused.
	seedRestrictTaint(t, ms, 11, untrustedInputTaint, model.EffectRestrict, model.EffectGate)
	wantUnusable("gate-effect untrusted-input", untrustedInputGate)
	seedRestrictTaint(t, ms, 11, untrustedInputTaint)
	wantUnusable("effect-less untrusted-input", untrustedInputNoRestrict)
}
