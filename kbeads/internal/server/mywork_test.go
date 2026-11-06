package server

import (
	"context"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// seedMyWorkBeads populates the mock store with one bead per queue plus a
// few decoys so we can verify each predicate is honored independently.
// "matt" is the operator under test.
func seedMyWorkBeads(ms *mockStore, now time.Time) {
	add := func(b *model.Bead) {
		ms.beads[b.ID] = b
	}

	// 1. Pending decision (status=open, type=decision).
	add(&model.Bead{
		ID: "gd-dec-1", Type: model.TypeDecision, Status: model.StatusOpen,
		Title: "approve canary?", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	})
	// Decoy: closed decision shouldn't appear.
	add(&model.Bead{
		ID: "gd-dec-2", Type: model.TypeDecision, Status: model.StatusClosed,
		Title: "old decision", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour),
	})

	// 2. Mail to matt (type=message, assignee=matt, open).
	add(&model.Bead{
		ID: "gd-mail-1", Type: "message", Status: model.StatusOpen,
		Assignee: "matt", Title: "rebase blocked",
		CreatedAt: now.Add(-15 * time.Minute), UpdatedAt: now.Add(-15 * time.Minute),
	})
	// Decoy: mail to someone else.
	add(&model.Bead{
		ID: "gd-mail-2", Type: "message", Status: model.StatusOpen,
		Assignee: "alice", Title: "for alice",
		CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-10 * time.Minute),
	})

	// 3. Crew agents matt spawned (type=agent, created_by=matt).
	add(&model.Bead{
		ID: "gd-agent-fresh", Type: "agent", Status: model.StatusInProgress,
		CreatedBy: "matt", Title: "agent-foo",
		CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-5 * time.Minute),
	})
	add(&model.Bead{
		ID: "gd-agent-stale", Type: "agent", Status: model.StatusInProgress,
		CreatedBy: "matt", Title: "agent-bar",
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-90 * time.Minute),
	})
	// Decoy: agent created by someone else.
	add(&model.Bead{
		ID: "gd-agent-other", Type: "agent", Status: model.StatusInProgress,
		CreatedBy: "alice", Title: "agent-other",
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-90 * time.Minute),
	})

	// 4. Beads assigned to matt (in_progress task).
	add(&model.Bead{
		ID: "gd-task-1", Type: "task", Status: model.StatusInProgress,
		Assignee: "matt", Title: "ship the thing",
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour),
	})
	// Decoy: closed task — should not appear in assigned_open.
	add(&model.Bead{
		ID: "gd-task-closed", Type: "task", Status: model.StatusClosed,
		Assignee: "matt", Title: "shipped",
		CreatedAt: now.Add(-26 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour),
	})

	// 5. Recently closed in matt's lane (within 24h).
	add(&model.Bead{
		ID: "gd-recent-1", Type: "task", Status: model.StatusClosed,
		Assignee: "matt", Title: "just landed",
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-30 * time.Minute),
	})
	// Decoy: closed > 24h ago — should NOT be in recent_closed.
	// (gd-task-closed above doubles as that decoy.)
}

func TestMyWorkSummary_PartitionsByQueue(t *testing.T) {
	srv, ms, _ := newTestServer()
	now := time.Now().UTC()
	seedMyWorkBeads(ms, now)

	resp, err := srv.MyWorkSummary(context.Background(), &beadsv1.MyWorkSummaryRequest{
		OperatorId:   "matt",
		StaleMinutes: 30,
		Limit:        50,
	})
	if err != nil {
		t.Fatalf("MyWorkSummary: %v", err)
	}

	if got := idsOf(resp.GetDecisionsPending()); len(got) != 1 || got[0] != "gd-dec-1" {
		t.Errorf("decisions_pending = %v, want [gd-dec-1]", got)
	}
	if resp.GetDecisionsPendingCount() != 1 {
		t.Errorf("decisions_pending_count = %d, want 1", resp.GetDecisionsPendingCount())
	}

	if got := idsOf(resp.GetMailUnread()); len(got) != 1 || got[0] != "gd-mail-1" {
		t.Errorf("mail_unread = %v, want [gd-mail-1]", got)
	}

	staleIDs := idsOf(resp.GetStaleAgents())
	if len(staleIDs) != 1 || staleIDs[0] != "gd-agent-stale" {
		t.Errorf("stale_agents = %v, want [gd-agent-stale]", staleIDs)
	}
	if resp.GetStaleAgentsCount() != 1 {
		t.Errorf("stale_agents_count = %d, want 1", resp.GetStaleAgentsCount())
	}

	if got := idsOf(resp.GetAssignedOpen()); len(got) != 1 || got[0] != "gd-task-1" {
		t.Errorf("assigned_open = %v, want [gd-task-1]", got)
	}

	if got := idsOf(resp.GetRecentClosed()); len(got) != 1 || got[0] != "gd-recent-1" {
		t.Errorf("recent_closed = %v, want [gd-recent-1]", got)
	}

	if resp.GetOperatorId() != "matt" {
		t.Errorf("operator_id echo = %q", resp.GetOperatorId())
	}
	if resp.GetStaleMinutes() != 30 {
		t.Errorf("stale_minutes echo = %d", resp.GetStaleMinutes())
	}
}

func TestMyWorkSummary_DefaultsApply(t *testing.T) {
	srv, ms, _ := newTestServer()
	now := time.Now().UTC()
	// One decision so we get a non-trivial response.
	ms.beads["gd-dec-1"] = &model.Bead{
		ID: "gd-dec-1", Type: model.TypeDecision, Status: model.StatusOpen,
		Title: "x", CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour),
	}

	resp, err := srv.MyWorkSummary(context.Background(), &beadsv1.MyWorkSummaryRequest{
		OperatorId: "",
	})
	if err != nil {
		t.Fatalf("MyWorkSummary: %v", err)
	}
	if resp.GetStaleMinutes() != defaultStaleAgentMinutes {
		t.Errorf("default stale minutes = %d, want %d", resp.GetStaleMinutes(), defaultStaleAgentMinutes)
	}
	// Decisions should still surface even without operator id.
	if got := resp.GetDecisionsPendingCount(); got != 1 {
		t.Errorf("decisions count without operator = %d, want 1", got)
	}
	// Other queues should be empty when no operator id is supplied.
	if len(resp.GetMailUnread()) != 0 || len(resp.GetAssignedOpen()) != 0 || len(resp.GetStaleAgents()) != 0 {
		t.Errorf("non-decision queues should be empty without operator id")
	}
}

func TestMyWorkSummary_StaleThresholdRespected(t *testing.T) {
	srv, ms, _ := newTestServer()
	now := time.Now().UTC()
	// Idle 20 minutes — would be fresh at 30min threshold, stale at 15min.
	ms.beads["gd-agent-1"] = &model.Bead{
		ID: "gd-agent-1", Type: "agent", Status: model.StatusInProgress,
		CreatedBy: "matt", Title: "borderline",
		CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-20 * time.Minute),
	}

	respDefault, err := srv.MyWorkSummary(context.Background(), &beadsv1.MyWorkSummaryRequest{
		OperatorId: "matt",
	})
	if err != nil {
		t.Fatalf("MyWorkSummary default: %v", err)
	}
	if got := len(respDefault.GetStaleAgents()); got != 0 {
		t.Errorf("at default 30m threshold, expected no stale agents, got %d", got)
	}

	respTight, err := srv.MyWorkSummary(context.Background(), &beadsv1.MyWorkSummaryRequest{
		OperatorId:   "matt",
		StaleMinutes: 15,
	})
	if err != nil {
		t.Fatalf("MyWorkSummary tight: %v", err)
	}
	if got := len(respTight.GetStaleAgents()); got != 1 {
		t.Errorf("at 15m threshold, expected 1 stale agent, got %d", got)
	}
}

// seedCrossProjectDecisions adds two pending decisions in distinct projects so
// a scope regression can assert the caller only sees its authorized project.
func seedCrossProjectDecisions(ms *mockStore, now time.Time) {
	ms.beads["gd-dec-gardenio"] = &model.Bead{
		ID: "gd-dec-gardenio", Type: model.TypeDecision, Status: model.StatusOpen,
		Title: "gardenio decision", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}
	ms.labels["gd-dec-gardenio"] = []string{"project:gardenio"}
	ms.beads["gd-dec-secret"] = &model.Bead{
		ID: "gd-dec-secret", Type: model.TypeDecision, Status: model.StatusOpen,
		Title: "secretproj decision", Description: "confidential cross-project prose",
		CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour),
	}
	ms.labels["gd-dec-secret"] = []string{"project:secretproj"}
}

// TestMyWorkSummary_ScopesCrossProjectReads is the regression for the
// ka-0000008132 read-authz family gap on the my-work aggregator: the
// DecisionsPending queue carries no operator predicate, so before this fix it
// returned full decision beads from EVERY project to any RoleViewer. All five
// hand-built ListBeads filters must now flow through s.scopeListFilter.
func TestMyWorkSummary_ScopesCrossProjectReads(t *testing.T) {
	now := time.Now().UTC()

	// A project-restricted OIDC user (allowed_projects=gardenio) must NOT see the
	// secretproj decision.
	t.Run("oidc_restricted", func(t *testing.T) {
		srv, ms, _ := newTestServer()
		seedCrossProjectDecisions(ms, now)
		makeUserBead(ms, "gd-user-mallory", "mallory@example.com", "gardenio", "crew")

		ctx := oidcCtx("mallory@example.com", []string{RoleViewer})
		sum, err := srv.computeMyWorkSummary(ctx, "", 30, 50)
		if err != nil {
			t.Fatalf("computeMyWorkSummary: %v", err)
		}
		got := beadIDsOf(sum.DecisionsPending)
		if len(got) != 1 || got[0] != "gd-dec-gardenio" {
			t.Errorf("restricted OIDC decisions = %v, want [gd-dec-gardenio] (secretproj must be filtered)", got)
		}
	})

	// A project-bound fleet token scoped to secretproj must ONLY see secretproj.
	t.Run("fleet_token", func(t *testing.T) {
		srv, ms, _ := newTestServer()
		seedCrossProjectDecisions(ms, now)

		ctx := WithTokenProject(context.Background(), "secretproj")
		sum, err := srv.computeMyWorkSummary(ctx, "", 30, 50)
		if err != nil {
			t.Fatalf("computeMyWorkSummary: %v", err)
		}
		got := beadIDsOf(sum.DecisionsPending)
		if len(got) != 1 || got[0] != "gd-dec-secret" {
			t.Errorf("secretproj token decisions = %v, want [gd-dec-secret] (gardenio must be filtered)", got)
		}
	})

	// An unrestricted caller (no token, no OIDC scope) keeps the fleet-wide view.
	t.Run("unrestricted", func(t *testing.T) {
		srv, ms, _ := newTestServer()
		seedCrossProjectDecisions(ms, now)

		sum, err := srv.computeMyWorkSummary(context.Background(), "", 30, 50)
		if err != nil {
			t.Fatalf("computeMyWorkSummary: %v", err)
		}
		if got := len(sum.DecisionsPending); got != 2 {
			t.Errorf("unrestricted decisions count = %d, want 2 (fleet-wide)", got)
		}
	})
}

func idsOf(beads []*beadsv1.Bead) []string {
	out := make([]string, 0, len(beads))
	for _, b := range beads {
		out = append(out, b.GetId())
	}
	return out
}
