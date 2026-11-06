package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The escalation-executor grant (escalation routing Task 15 Step 1,
// gd-sZNUPjzygT). Every refusal here has its acceptance on the same fixture
// (Global Constraint 6): the fixture's one approved, claimed bead_close
// escalation lets the cluster coordinator write gd-xt1 and nothing else.

const (
	clusterClient = "gardenio-cluster-coordinator"
	clusterActor  = "machine:" + clusterClient
	executorRole  = "gardenio-machine-escalation-executor"
	// executorGrantRow is the ownership row machineGrantRows derives for it.
	executorGrantRow = "machine:escalation-executor/approved-targets"
)

// clusterCtx is the cluster coordinator's machine principal.
func clusterCtx() context.Context { return machineCtx(clusterClient, executorRole) }

// newExecutorGrantServer seeds C1's policy (the C3 rule and coordinators.cluster),
// allowlists both coordinator clients, and seeds the targets: gd-xt1 (listed)
// and gd-xt2 (not), both owned by a gone crew.
func newExecutorGrantServer(t *testing.T) (*BeadsServer, *mockStore) {
	t.Helper()
	s, ms := newLaneTestServer()
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator", clusterClient})
	pol, _ := json.Marshal(map[string]any{
		"rules": []any{
			map[string]any{"when": map[string]any{"operation_types": []string{"github_pr_merge"}}, "approvers": []string{"coordinator:merge-desk", "operator"}, "execute_by": "coordinator:merge-desk"},
			map[string]any{"when": map[string]any{"operation_types": []string{"kbeads_admin_write", "kbeads_config_write", "release_record", "release_record_backfill", "bead_close", "bead_label"}}, "approvers": []string{"operator"}, "execute_by": "coordinator:cluster", "hold_by": "coordinator:cluster"},
			map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "hold_by": "coordinator:laptop"},
		},
		"coordinators": map[string]any{
			"merge-desk": map[string]any{"principals": []string{"role:reviewer"}, "audience": "desk"},
			"laptop":     map[string]any{"principals": []string{"machine:gardenio-laptop-operator"}, "audience": "operator"},
			"cluster":    map[string]any{"principals": []string{"machine:" + clusterClient}, "audience": "operator"},
		},
	})
	ms.beads["gd-xpol"] = &model.Bead{ID: "gd-xpol", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}
	for _, id := range []string{"gd-xt1", "gd-xt2"} {
		seedExecutorTarget(ms, id, model.TypeTask, "gardenio")
	}
	return s, ms
}

// seedExecutorTarget seeds a bead owned by a gone crew in project.
func seedExecutorTarget(ms *mockStore, id string, typ model.BeadType, project string) {
	kind := model.KindIssue
	if typ == "role" || typ == "config" {
		kind = model.KindConfig
	}
	ms.beads[id] = &model.Bead{ID: id, Kind: kind, Type: typ, Status: model.StatusOpen, CreatedBy: "gone-crew-x1"}
	ms.labels[id] = []string{"project:" + project} // GetBead reads labels from here
}

// clusterEscalation is an approved bead_close escalation executed and held by
// coordinator:cluster, claimed by the cluster coordinator, naming gd-xt1;
// fields override the defaults (nil deletes).
func clusterEscalation(ms *mockStore, id string, override map[string]any) {
	f := map[string]any{
		"operation_type": "bead_close",
		"approvers":      []string{"operator"},
		"execute_by":     "coordinator:cluster", "held_by": "coordinator:cluster",
		"operator_only": false, "policy_rule": "escalation#6",
		"approved_at":        "2026-11-02T09:30:00Z",
		"approved_by":        "owner@example.com",
		"claimed_by":         clusterActor,
		approvedTargetsField: []string{"gd-xt1"},
	}
	for k, v := range override {
		if v == nil {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	b := seedRoutedEscalation(ms, id, "gardenio", f)
	b.Status = model.StatusInProgress
}

func ownershipAdmits(t *testing.T, s *BeadsServer, ctx context.Context, action, id string) bool {
	t.Helper()
	b, err := s.store.GetBead(ctx, id)
	if err != nil || b == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return s.authorizeOwnership(ctx, action, b) == nil
}

func TestEscalationExecutorGrant_ListedTargetOnly(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	clusterEscalation(ms, "gd-xesc", nil)
	ctx := clusterCtx()

	for _, action := range []string{"update", "close"} {
		if !ownershipAdmits(t, s, ctx, action, "gd-xt1") {
			t.Errorf("%s gd-xt1: listed in approved_targets of the claimed approved escalation, must pass", action)
		}
		if ownershipAdmits(t, s, ctx, action, "gd-xt2") {
			t.Errorf("%s gd-xt2: not listed in approved_targets, must stay denied", action)
		}
	}
	if ownershipAdmits(t, s, ctx, "delete", "gd-xt1") {
		t.Error("delete gd-xt1: the grant is update and close only")
	}
	// The same escalation without the executor role grants the client nothing.
	if ownershipAdmits(t, s, machineCtx(clusterClient), "close", "gd-xt1") {
		t.Error("close gd-xt1 without gardenio-machine-escalation-executor must stay denied")
	}
}

func TestEscalationExecutorGrant_ClaimHeld(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	ctx := clusterCtx()

	clusterEscalation(ms, "gd-xesc", map[string]any{"claimed_by": ""})
	if ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("unclaimed escalation: close must stay denied")
	}
	clusterEscalation(ms, "gd-xesc", map[string]any{"claimed_by": "machine:gardenio-laptop-operator"})
	if ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("escalation claimed by someone else: close must stay denied")
	}
	clusterEscalation(ms, "gd-xesc", nil)
	if !ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("claim held: close must pass")
	}
	// A closed escalation names nothing, claim or not.
	ms.beads["gd-xesc"].Status = model.StatusClosed
	if ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("closed escalation: close must stay denied")
	}
}

func TestEscalationExecutorGrant_Approved(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	ctx := clusterCtx()

	clusterEscalation(ms, "gd-xesc", map[string]any{"approved_at": nil})
	if ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("unapproved escalation (no approved_at): close must stay denied")
	}
	clusterEscalation(ms, "gd-xesc", nil)
	if !ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("approved escalation: close must pass")
	}
}

func TestEscalationExecutorGrant_LaneAndOperation(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	ctx := clusterCtx()

	for name, override := range map[string]map[string]any{
		"execute_by a lane it is not in":     {"execute_by": "coordinator:laptop"},
		"execute_by operator":                {"execute_by": "operator"},
		"not a bead-only operation":          {"operation_type": "github_pr_merge"},
		"operator-only (D3 floor) stamp":     {"operator_only": true},
		"approved_targets not a string list": {approvedTargetsField: "gd-xt1"},
	} {
		clusterEscalation(ms, "gd-xesc", override)
		if ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
			t.Errorf("%s: close must stay denied", name)
		}
	}
	// The client dropped from BEADS_ESCALATION_COORDINATOR_CLIENTS (the kill
	// switch) is in no lane.
	clusterEscalation(ms, "gd-xesc", nil)
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator"})
	if ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("client not allowlisted as a coordinator: close must stay denied")
	}
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator", clusterClient})
	if !ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("positive control: allowlisted cluster coordinator must pass")
	}
}

// P9.5 (gd-q4ebFp0p4f): every operation the policy sends to coordinator:cluster
// for execution (D7 rule 4, classes bead_admin and bead_close) executes under
// the grant, plan lifecycle and dependency edits included; a floor type in
// those classes, advice and an unrelated class stay denied on the same fixture.
func TestEscalationExecutorGrant_BeadAdminClassExecutes(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	ctx := clusterCtx()

	for _, op := range []string{
		"kbeads_plan_lifecycle", "plan_complete", "kbeads_dep_edit", "kbeads_admin_write",
		"release_record_backfill", "bead_admin_close", "kbeads_bead_close", "release_record", "bead_label",
	} {
		clusterEscalation(ms, "gd-xesc", map[string]any{"operation_type": op})
		if !ownershipAdmits(t, s, ctx, "update", "gd-xt1") {
			t.Errorf("%s: bead-only, routed to coordinator:cluster; update gd-xt1 must pass", op)
		}
	}
	for _, op := range []string{"kbeads_role_update", "user_identity_pin", "kbeads_advice_write", "escalation_policy_write", "kbeads_trigger_update"} {
		clusterEscalation(ms, "gd-xesc", map[string]any{"operation_type": op})
		if ownershipAdmits(t, s, ctx, "update", "gd-xt1") {
			t.Errorf("%s: not bead-only (floor, advice or another class); update gd-xt1 must stay denied", op)
		}
	}
}

// Each type of a class the policy executes on coordinator:cluster is either
// bead-only or on the floor; nothing in them falls between the two.
func TestBeadOnlyOperationsCoverTheirClasses(t *testing.T) {
	for _, c := range operationClasses {
		if !slices.Contains(beadOnlyOperationClasses, c.Name) {
			for _, typ := range c.Types {
				if isBeadOnlyOperation(typ) {
					t.Errorf("%s (class %s) is bead-only outside the bead-only classes", typ, c.Name)
				}
			}
			continue
		}
		for _, typ := range c.Types {
			if isBeadOnlyOperation(typ) == (escalationFloorOperation(typ) != "") {
				t.Errorf("%s (class %s): bead-only=%v floor=%q, want exactly one", typ, c.Name, isBeadOnlyOperation(typ), escalationFloorOperation(typ))
			}
		}
	}
}

// An approved_targets entry that is not an executor type reaches nothing:
// a decision or trigger is outside the row's types, and a role or config bead
// is config-kind, refused by its type's write policy.
func TestEscalationExecutorGrant_FloorTypesRefused(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	ctx := clusterCtx()
	seedExecutorTarget(ms, "gd-xdec", model.TypeDecision, "gardenio")
	seedExecutorTarget(ms, "gd-xtrg", "trigger", "gardenio")
	seedExecutorTarget(ms, "gd-xrole", "role", "gardenio")
	seedExecutorTarget(ms, "gd-xcfg", "config", "gardenio")
	clusterEscalation(ms, "gd-xesc", map[string]any{approvedTargetsField: []string{"gd-xt1", "gd-xdec", "gd-xtrg", "gd-xrole", "gd-xcfg"}})

	// A decision a machine may answer passes the decision-queue row, never
	// this one, so the check is on the admitting row.
	for _, id := range []string{"gd-xdec", "gd-xtrg"} {
		b, _ := s.store.GetBead(ctx, id)
		if adm, ok := s.evaluateOwnershipGrants(ctx, "close", b); ok && adm.grant == executorGrantRow {
			t.Errorf("close %s: not an executor type, admitted by %s", id, adm.grant)
		}
	}
	if ownershipAdmits(t, s, ctx, "close", "gd-xtrg") {
		t.Error("close gd-xtrg: not an executor type, must stay denied")
	}
	for _, id := range []string{"gd-xrole", "gd-xcfg"} {
		b, _ := s.store.GetBead(ctx, id)
		if s.authorizeWriteForBead(ctx, "update", b) == nil {
			t.Errorf("update %s: config-kind, must stay denied", id)
		}
	}
	b, _ := s.store.GetBead(ctx, "gd-xt1")
	if d := s.authorizeWriteForBead(ctx, "close", b); d != nil {
		t.Errorf("positive control: close gd-xt1 on the same escalation must pass, got %v", d)
	}
	if adm, ok := s.evaluateOwnershipGrants(ctx, "close", b); !ok || adm.grant != executorGrantRow {
		t.Errorf("positive control: close gd-xt1 admitted by %q, want %s", adm.grant, executorGrantRow)
	}
}

// The stamp reads only the immutable fields (operation_spec.bead_ids,
// target_bead_id), and keeps executor types in the escalation's project.
func TestEscalationApprovedTargets_ImmutableFieldsOnly(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	seedExecutorTarget(ms, "gd-xb1", model.TypeBug, "gardenio")
	seedExecutorTarget(ms, "gd-xe1", model.TypeEpic, "gardenio")
	seedExecutorTarget(ms, "gd-xother", model.TypeTask, "other")
	seedExecutorTarget(ms, "gd-xdec", model.TypeDecision, "gardenio")
	seedExecutorTarget(ms, "gd-xrole", "role", "gardenio")
	// A mention in editable text names nothing.
	ms.mentions = []*model.BeadReferenceSummary{{ID: "gd-xt1", Type: "task", SourceField: "description"}}
	ms.mentionsTotal = 1
	clusterEscalation(ms, "gd-xesc", map[string]any{
		"target_bead_id": "gd-xe1",
		"operation_spec": map[string]any{"bead_ids": []string{"gd-xt2", "gd-xb1", "gd-xt2", "gd-xother", "gd-xdec", "gd-xrole", "gd-xgone", "gd-xesc"}},
	})

	got := s.escalationApprovedTargets(context.Background(), ms.beads["gd-xesc"])
	if want := []string{"gd-xb1", "gd-xe1", "gd-xt2"}; !slices.Equal(got, want) {
		t.Fatalf("approved_targets = %v, want %v (no mention, no other project, no floor type, no dangling id)", got, want)
	}
}

// Both approve paths stamp approved_targets at the first approval, and a later
// approve never re-stamps it, whatever the escalation names by then.
func TestEscalationApprove_StampsApprovedTargetsOnce(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	spec := map[string]any{"bead_ids": []string{"gd-xt1"}}

	clusterEscalation(ms, "gd-xh", map[string]any{"approved_at": nil, "approved_by": nil, approvedTargetsField: nil, "claimed_by": nil, "operation_spec": spec})
	ms.beads["gd-xh"].Status = model.StatusOpen
	rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-xh", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("human approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-xh", approvedTargetsField); !jsonEqual(got, []any{"gd-xt1"}) {
		t.Fatalf("human approve: approved_targets = %v, want [gd-xt1]", got)
	}

	merge := mergeDeskEscalation("gd-crew1")
	merge["operation_spec"] = spec
	seedRoutedEscalation(ms, "gd-xm", "gardenio", merge)
	rec = callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-xm", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("coordinator approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-xm", approvedTargetsField); !jsonEqual(got, []any{"gd-xt1"}) {
		t.Fatalf("coordinator approve: approved_targets = %v, want [gd-xt1]", got)
	}

	// Already approved, now naming gd-xt2 too: the stamp is never recomputed.
	clusterEscalation(ms, "gd-xre", map[string]any{"claimed_by": nil, "operation_spec": map[string]any{"bead_ids": []string{"gd-xt1", "gd-xt2"}}})
	f := map[string]any{}
	_ = json.Unmarshal(ms.beads["gd-xre"].Fields, &f)
	delete(f, approvedTargetsField)
	ms.beads["gd-xre"].Fields, _ = json.Marshal(f)
	_ = callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-xre", "approve", map[string]any{})
	if got := escField(t, ms, "gd-xre", approvedTargetsField); got != nil {
		t.Fatalf("re-approve of an approved escalation stamped approved_targets = %v, want none", got)
	}
}

// A target outside the escalation's project, missing or empty approved_targets,
// a fleet token, and another machine holding the role but not the claim get
// nothing; the cluster coordinator on the same fixture does.
func TestEscalationExecutorGrant_ProjectAndCallerRefusals(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	seedExecutorTarget(ms, "gd-xother", model.TypeTask, "other")
	clusterEscalation(ms, "gd-xesc", map[string]any{approvedTargetsField: []string{"gd-xt1", "gd-xother"}})

	if ownershipAdmits(t, s, clusterCtx(), "close", "gd-xother") {
		t.Error("close gd-xother: another project than the escalation's, must stay denied")
	}
	for name, ctx := range map[string]context.Context{
		"fleet token named as the claimant": fleetCtx(clusterActor),
		"another machine with the role":     machineCtx("gardenio-laptop-operator", executorRole),
	} {
		if ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
			t.Errorf("%s: close gd-xt1 must stay denied", name)
		}
	}
	if !ownershipAdmits(t, s, clusterCtx(), "close", "gd-xt1") {
		t.Fatal("positive control: the claimant closes gd-xt1")
	}
	for name, v := range map[string]any{"missing": nil, "empty": []string{}} {
		clusterEscalation(ms, "gd-xesc", map[string]any{approvedTargetsField: v})
		if ownershipAdmits(t, s, clusterCtx(), "close", "gd-xt1") {
			t.Errorf("approved_targets %s: close gd-xt1 must stay denied", name)
		}
	}
}

// coordinator:cluster is never an approver (C3): can-approve refuses it on
// the escalation it holds, and allows the operator on the same one.
func TestEscalationExecutorGrant_ClusterCannotApprove(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	clusterEscalation(ms, "gd-xu", map[string]any{"approved_at": nil, "approved_by": nil, "claimed_by": nil})
	ms.beads["gd-xu"].Status = model.StatusOpen

	can := func(ctx context.Context) map[string]any {
		t.Helper()
		rec := callEscalationVerb(t, ctx, s.handleCanApproveEscalation, "gd-xu", "can-approve", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("can-approve: got %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if got := can(clusterCtx()); got["allowed"] != false {
		t.Errorf("cluster coordinator can-approve = %v, want allowed:false", got)
	}
	if got := can(operatorCtx("owner@example.com")); got["allowed"] != true {
		t.Errorf("operator can-approve = %v, want allowed:true (positive control)", got)
	}
	rec := callEscalationVerb(t, clusterCtx(), s.handleApproveEscalation, "gd-xu", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Errorf("cluster coordinator approve: got %d %s, want 403", rec.Code, rec.Body.String())
	}
}

func jsonEqual(got any, want any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	return string(a) == string(b)
}

// A forged approved_targets on /result is a 400 and lands nothing; the same
// result without it is recorded (create and PATCH: forgedRouteValues).
func TestEscalationResult_RefusesForgedApprovedTargets(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "")
	_, ms, h := newClassedTestServer(t)
	seedRoutedEscalation(ms, "gd-xr1", "gardenio", mergeDeskEscalation("gd-crew1"))
	body := map[string]any{"result_status": "success", "fields": map[string]any{approvedTargetsField: []string{"gd-forged"}}}
	if rec := doBearerJSON(t, h, http.MethodPost, "/v1/escalations/gd-xr1/result", testAgentToken, body, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("forged approved_targets on /result: got %d %s, want 400", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-xr1", approvedTargetsField); got != nil || ms.beads["gd-xr1"].Status != model.StatusOpen {
		t.Fatalf("refused /result wrote approved_targets=%v status=%s", got, ms.beads["gd-xr1"].Status)
	}
	delete(body, "fields")
	if rec := doBearerJSON(t, h, http.MethodPost, "/v1/escalations/gd-xr1/result", testAgentToken, body, nil); rec.Code != http.StatusOK {
		t.Fatalf("positive control /result: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// A bundle closes under the grant only when an approved bead-close escalation
// names it, and only by close: the stamp keeps it only for a bead-close type,
// and the grant refuses its update and a bundle no escalation names
// (gd-dNYrSP9KkW: every bundle close was handed up to the owner).
func TestEscalationExecutorGrant_BundleCloseOnly(t *testing.T) {
	s, ms := newExecutorGrantServer(t)
	ctx := clusterCtx()
	seedExecutorTarget(ms, "gd-xbun", model.TypeBundle, "gardenio")
	seedExecutorTarget(ms, "gd-xbun2", model.TypeBundle, "gardenio")
	seedExecutorTarget(ms, "gd-xdoc", typeDoc, "gardenio")
	spec := map[string]any{"bead_ids": []string{"gd-xt1", "gd-xbun", "gd-xdoc"}}

	clusterEscalation(ms, "gd-xesc", map[string]any{"operation_type": "kbeads_bead_close", "operation_spec": spec})
	if got, want := s.escalationApprovedTargets(context.Background(), ms.beads["gd-xesc"]), []string{"gd-xbun", "gd-xt1"}; !slices.Equal(got, want) {
		t.Fatalf("bead-close stamp = %v, want %v (bundle in, doc out)", got, want)
	}
	clusterEscalation(ms, "gd-xesc", map[string]any{"operation_type": "kbeads_plan_lifecycle", "operation_spec": spec})
	if got, want := s.escalationApprovedTargets(context.Background(), ms.beads["gd-xesc"]), []string{"gd-xt1"}; !slices.Equal(got, want) {
		t.Fatalf("bead_admin stamp = %v, want %v (a bundle only for a bead-close type)", got, want)
	}

	clusterEscalation(ms, "gd-xesc", map[string]any{"operation_type": "kbeads_bead_close", approvedTargetsField: []string{"gd-xt1", "gd-xbun"}})
	b, _ := s.store.GetBead(ctx, "gd-xbun")
	if d := s.authorizeWriteForBead(ctx, "close", b); d != nil {
		t.Fatalf("close gd-xbun: named by the claimed approved bead-close escalation, must pass, got %v", d)
	}
	if adm, ok := s.evaluateOwnershipGrants(ctx, "close", b); !ok || adm.grant != "machine:escalation-executor/approved-close" {
		t.Errorf("close gd-xbun admitted by %q, want machine:escalation-executor/approved-close", adm.grant)
	}
	if ownershipAdmits(t, s, ctx, "update", "gd-xbun") {
		t.Error("update gd-xbun: a bundle is close-only under the grant, must stay denied")
	}
	if ownershipAdmits(t, s, ctx, "close", "gd-xbun2") {
		t.Error("close gd-xbun2: a bundle no escalation names, must stay denied")
	}
	// A forged or legacy stamp on a non-close escalation still closes no bundle.
	clusterEscalation(ms, "gd-xesc", map[string]any{"operation_type": "kbeads_plan_lifecycle", approvedTargetsField: []string{"gd-xt1", "gd-xbun"}})
	if ownershipAdmits(t, s, ctx, "close", "gd-xbun") {
		t.Error("close gd-xbun under a bead_admin escalation: must stay denied")
	}
	if !ownershipAdmits(t, s, ctx, "close", "gd-xt1") {
		t.Error("positive control: close gd-xt1 under the same escalation must pass")
	}
}
