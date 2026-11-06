package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Tests for ADR docs/adr/taints.md §4.2 row 2 / §5.2 (gd-Kebd0XJHZ2):
// taint propagation on assignment. Every acceptance criterion on the task is
// covered here:
//
//   - assigning a tainted task to an agent whose assigner holds the taint's
//     grant grants the agent bead that taint, expiring at agent-bead close;
//   - assigning a tainted task where the assigner does NOT hold the grant
//     leaves the assignment itself intact, and still propagates the
//     bead_taints marker (D2: union, never automatic removal), but withholds
//     the taint_grants row (the "assignment succeeds, no taint grant
//     created" reading of the ambiguous acceptance criterion — see
//     resolveAssignTaints' doc comment for why);
//   - assigning a tainted task with no server-resolvable granter at all (a
//     bare static/machine caller relaying no human) behaves the same way:
//     marker yes, grant no;
//   - the grant is revoked when the agent bead closes.

// seedTaskTaint records taintID/name as already applied to taskBeadID, as if
// a prior `kd taint apply` had run — the shape resolveAssignTaints reads via
// GetBeadTaints.
func seedTaskTaint(ms *mockStore, taskBeadID string, taintID int64, taintName string) {
	if ms.beadTaints == nil {
		ms.beadTaints = make(map[string][]*model.BeadTaint)
	}
	ms.beadTaints[taskBeadID] = append(ms.beadTaints[taskBeadID], &model.BeadTaint{
		BeadID:    taskBeadID,
		TaintID:   taintID,
		TaintName: taintName,
		Source:    model.TaintSourceExplicit,
		AppliedBy: "someone",
	})
}

// agentCreateFields builds a minimal valid type=agent Fields blob (agent,
// role, project are required by the builtin type:agent config) carrying
// task_id, the createBead half of ADR §4.2 row 2.
func agentCreateFields(taskID string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{
		"agent":   "test-agent",
		"role":    "crew",
		"project": "gardenio",
		"task_id": taskID,
	})
	return raw
}

func TestCreateBead_AgentWithTaintedTask_AssignerHoldsGrant_PropagatesTaintAndGrant(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTaskTaint(ms, "gd-task1", 1, "acme-production")

	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}
	ctx := asOIDC(ctxAsTestSubject(), "alice@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})

	bead, err := s.createBead(ctx, createBeadInput{
		Title:  "spawn agent for tainted task",
		Type:   "agent",
		Fields: agentCreateFields("gd-task1"),
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 1)
	if !ok {
		t.Fatalf("expected agent bead to carry taint 1, got rows %v", rows)
	}
	if row.Source != model.TaintSourceInheritAssign {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritAssign)
	}
	if row.SourceBeadID != "gd-task1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-task1")
	}

	grant, ok := ms.taintGrants[taintGrantKey(1, model.PrincipalAgent, bead.ID)]
	if !ok {
		t.Fatalf("expected a taint_grants row for agent %s, got %v", bead.ID, ms.taintGrants)
	}
	if grant.GrantedBy != 7 {
		t.Errorf("GrantedBy = %d; want 7 (alice)", grant.GrantedBy)
	}
	if grant.ExpiresAt == nil || !grant.ExpiresAt.After(time.Now()) {
		t.Errorf("expected a future ExpiresAt bound, got %v", grant.ExpiresAt)
	}
}

// TestCreateBead_AgentWithTaintedTask_AssignerLacksGrant_TaintYesGrantNo
// covers the coordinator-review fix on PR #39: an assigner with a
// resolvable identity but no grant for the task's taint still produces the
// bead_taints marker (D2 union), only the grant is withheld.
func TestCreateBead_AgentWithTaintedTask_AssignerLacksGrant_TaintYesGrantNo(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 1, "acme-production", "gd-other-team")
	seedTaskTaint(ms, "gd-task1", 1, "acme-production")

	// A plain OIDC human with no admin role and no membership in the
	// taint's owner team: callerHoldsTaintGrant must deny, but the identity
	// itself resolves fine (granterOperatorID succeeds) — this is NOT the
	// no-granter case, see the test below for that.
	ctx := ctxAsTestSubject()

	bead, err := s.createBead(ctx, createBeadInput{
		Title:  "spawn agent for tainted task, ungranted assigner",
		Type:   "agent",
		Fields: agentCreateFields("gd-task1"),
	})
	if err != nil {
		t.Fatalf("createBead should succeed even when the assigner lacks the grant: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 1)
	if !ok {
		t.Fatalf("expected agent bead to still carry taint 1 (marker propagates unconditionally), got rows %v", rows)
	}
	if row.Source != model.TaintSourceInheritAssign {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritAssign)
	}
	if _, ok := ms.taintGrants[taintGrantKey(1, model.PrincipalAgent, bead.ID)]; ok {
		t.Errorf("expected no taint_grants row for agent %s (assigner lacks the grant)", bead.ID)
	}
}

// TestResolveAssignTaints_NoResolvableGranter_TaintYesGrantNo covers a bare
// static/machine caller relaying no human identity at all
// (granterOperatorID returns hasGranter=false): the marker still resolves,
// and — with no principal to attribute a grant to — no grant does. Exercised
// directly against resolveAssignTaints rather than through createBead/
// updateBead: createBead rejects an unresolvable operator up front via
// resolveAuditOperatorRequired (beads.go:152), and updateBead now refuses the
// same shape before resolveAssignTaints ever runs (assigneeBead != nil &&
// appliedBy == "", beads.go ~1527 — see
// TestUpdateBead_AssigneeSetToAgent_UnresolvableOperator_Refused) — so a bare
// static caller can never reach this path through either HTTP-facing method;
// only a caller relaying no human via X-Operated-By at all would, and that
// shape is exactly what granterOperatorID's hasGranter=false branch exists to
// handle regardless of which call site reaches it.
func TestResolveAssignTaints_NoResolvableGranter_TaintYesGrantNo(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	seedTaskTaint(ms, "gd-task1", 1, "acme-production")

	ctx := withTestPrivilegedAgent(context.Background(), "static")

	pendingTaints, pendingGrants, err := s.resolveAssignTaints(ctx, "gd-task1")
	if err != nil {
		t.Fatalf("resolveAssignTaints error: %v", err)
	}
	found := false
	for _, pt := range pendingTaints {
		if pt.TaintID == 1 && pt.Source == model.TaintSourceInheritAssign && pt.SourceBeadID == "gd-task1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected taint 1 to still resolve as a pending marker (marker propagates unconditionally), got %v", pendingTaints)
	}
	if len(pendingGrants) != 0 {
		t.Errorf("expected no pending grants with no resolvable granter, got %v", pendingGrants)
	}
}

func TestUpdateBead_AssigneeSetToAgent_PropagatesTaintAndGrant(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 2, "customer-pii", "gd-owner-team")
	seedTaskTaint(ms, "gd-task2", 2, "customer-pii")
	ms.beads["gd-task2"] = &model.Bead{ID: "gd-task2", Type: "task", Kind: model.KindIssue, Title: "task2", Status: model.StatusOpen}
	ms.beads["gd-agent2"] = &model.Bead{ID: "gd-agent2", Type: "agent", Kind: model.KindConfig, Title: "agent2", Status: model.StatusOpen}

	ms.operatorAliases = map[string]int64{"oidc\x00bob@example.com": 9}
	ctx := asOIDC(ctxAsTestSubject(), "bob@example.com")
	ctx = WithRoles(ctx, []string{RoleAdmin})

	assignee := "gd-agent2"
	updated, err := s.updateBead(ctx, "gd-task2", updateBeadInput{Assignee: &assignee})
	if err != nil {
		t.Fatalf("updateBead error: %v", err)
	}
	if updated.Assignee != "gd-agent2" {
		t.Fatalf("expected assignee to be updated, got %q", updated.Assignee)
	}

	rows := ms.beadTaints["gd-agent2"]
	row, ok := beadTaintByID(rows, 2)
	if !ok {
		t.Fatalf("expected assignee agent bead to carry taint 2, got rows %v", rows)
	}
	if row.Source != model.TaintSourceInheritAssign {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritAssign)
	}
	if row.SourceBeadID != "gd-task2" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-task2")
	}

	grant, ok := ms.taintGrants[taintGrantKey(2, model.PrincipalAgent, "gd-agent2")]
	if !ok {
		t.Fatalf("expected a taint_grants row for gd-agent2, got %v", ms.taintGrants)
	}
	if grant.GrantedBy != 9 {
		t.Errorf("GrantedBy = %d; want 9 (bob)", grant.GrantedBy)
	}
}

func TestUpdateBead_AssigneeSetToNonAgent_NoPropagation(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 3, "acme-production", "gd-owner-team")
	seedTaskTaint(ms, "gd-task3", 3, "acme-production")
	ms.beads["gd-task3"] = &model.Bead{ID: "gd-task3", Type: "task", Kind: model.KindIssue, Title: "task3", Status: model.StatusOpen}
	ms.beads["gd-human-owner"] = &model.Bead{ID: "gd-human-owner", Type: model.TypeTeam, Kind: model.KindIssue, Title: "team", Status: model.StatusOpen}

	ctx := ctxAsTestSubject()
	ctx = WithRoles(ctx, []string{RoleAdmin})

	assignee := "gd-human-owner"
	if _, err := s.updateBead(ctx, "gd-task3", updateBeadInput{Assignee: &assignee}); err != nil {
		t.Fatalf("updateBead error: %v", err)
	}

	if rows := ms.beadTaints["gd-human-owner"]; len(rows) != 0 {
		t.Errorf("expected no taint propagation onto a non-agent assignee, got %v", rows)
	}
}

// TestUpdateBead_AssigneeSetToAgent_AssignerLacksGrant_TaintYesGrantNo is the
// update-path sibling of
// TestCreateBead_AgentWithTaintedTask_AssignerLacksGrant_TaintYesGrantNo (PR
// #39 re-review item 3): an assigner with a resolvable identity but no grant
// for the task's taint still produces the bead_taints marker (D2 union) when
// PATCHing assignee=<agent>; only the grant is withheld.
func TestUpdateBead_AssigneeSetToAgent_AssignerLacksGrant_TaintYesGrantNo(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 5, "acme-production", "gd-other-team")
	seedTaskTaint(ms, "gd-task5", 5, "acme-production")
	ms.beads["gd-task5"] = &model.Bead{ID: "gd-task5", Type: "task", Kind: model.KindIssue, Title: "task5", Status: model.StatusOpen}
	ms.beads["gd-agent5"] = &model.Bead{ID: "gd-agent5", Type: "agent", Kind: model.KindConfig, Title: "agent5", Status: model.StatusOpen}

	// A plain OIDC human with no admin role and no membership in the taint's
	// owner team: callerHoldsTaintGrant must deny, but the identity itself
	// resolves fine (appliedBy != "") — this is NOT the unresolvable-operator
	// case covered by
	// TestUpdateBead_AssigneeSetToAgent_UnresolvableOperator_Refused below.
	ctx := ctxAsTestSubject()

	assignee := "gd-agent5"
	updated, err := s.updateBead(ctx, "gd-task5", updateBeadInput{Assignee: &assignee})
	if err != nil {
		t.Fatalf("updateBead should succeed even when the assigner lacks the grant: %v", err)
	}
	if updated.Assignee != "gd-agent5" {
		t.Fatalf("expected assignee to be updated, got %q", updated.Assignee)
	}

	rows := ms.beadTaints["gd-agent5"]
	row, ok := beadTaintByID(rows, 5)
	if !ok {
		t.Fatalf("expected assignee agent bead to still carry taint 5 (marker propagates unconditionally), got %v", rows)
	}
	if row.Source != model.TaintSourceInheritAssign {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritAssign)
	}
	if _, ok := ms.taintGrants[taintGrantKey(5, model.PrincipalAgent, "gd-agent5")]; ok {
		t.Errorf("expected no taint_grants row for gd-agent5 (assigner lacks the grant)")
	}
}

// TestUpdateBead_AssigneeSetToAgent_UnresolvableOperator_Refused covers the
// PR #39 re-review fix (item 2): a caller with no server-resolvable
// operator (no X-Operated-By header, no agent-bead fallback) PATCHing
// assignee=<agent> must be refused with the standard operator-refusal shape
// (newOperatorRefusal) — not the opaque store error
// ("applying taint ... applied_by: is required") the unguarded
// applyPendingBeadTaints call used to surface, and not a silent commit of the
// assignee change with taint propagation dropped.
func TestUpdateBead_AssigneeSetToAgent_UnresolvableOperator_Refused(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 6, "acme-production", "gd-owner-team")
	seedTaskTaint(ms, "gd-task6", 6, "acme-production")
	ms.beads["gd-task6"] = &model.Bead{ID: "gd-task6", Type: "task", Kind: model.KindIssue, Title: "task6", Status: model.StatusOpen}
	ms.beads["gd-agent6"] = &model.Bead{ID: "gd-agent6", Type: "agent", Kind: model.KindConfig, Title: "agent6", Status: model.StatusOpen}

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Privileged: true}) // no subject: nothing to resolve an operator from

	assignee := "gd-agent6"
	_, err := s.updateBead(ctx, "gd-task6", updateBeadInput{Assignee: &assignee})
	if err == nil {
		t.Fatalf("expected updateBead to refuse an unresolvable-operator assignment to an agent bead")
	}
	var refusal *operatorRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("expected an *operatorRefusalError, got %T: %v", err, err)
	}

	if stored := ms.beads["gd-task6"]; stored.Assignee != "" {
		t.Errorf("assignee mutated despite refusal: got %q", stored.Assignee)
	}
	if rows := ms.beadTaints["gd-agent6"]; len(rows) != 0 {
		t.Errorf("expected no taint propagation on a refused assignment, got %v", rows)
	}
}

func TestAfterBeadClosed_AgentClose_RevokesAssignTaintGrant(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 4, "acme-production", "gd-owner-team")
	seedTaskTaint(ms, "gd-agent4", 4, "acme-production") // agent's OWN bead_taints, as inherit:assign would leave it
	if ms.taintGrants == nil {
		ms.taintGrants = make(map[string]*model.TaintGrant)
	}
	future := time.Now().Add(365 * 24 * time.Hour)
	ms.taintGrants[taintGrantKey(4, model.PrincipalAgent, "gd-agent4")] = &model.TaintGrant{
		TaintID: 4, PrincipalKind: model.PrincipalAgent, Principal: "gd-agent4",
		GrantedBy: 1, ExpiresAt: &future,
	}

	closedAt := time.Now()
	agentBead := &model.Bead{ID: "gd-agent4", Type: "agent", Kind: model.KindConfig, Status: model.StatusClosed, ClosedAt: &closedAt}
	s.afterBeadClosed(ctxAsTestSubject(), agentBead)

	if _, ok := ms.taintGrants[taintGrantKey(4, model.PrincipalAgent, "gd-agent4")]; ok {
		t.Error("expected the agent's taint grant to be revoked on close")
	}
}
