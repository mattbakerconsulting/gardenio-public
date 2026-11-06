package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// gd-jbY5UiWlN4: the three spawn-provenance cases on an agent bead must
// render distinctly — policy-approved, human-approved, and (for a privileged
// role with neither field recorded) a red "unreviewed" call-out. A
// non-privileged role with no approval fields gets no badge at all: the
// badge is a problem signal, not decoration on an ordinary spawn.

func agentSpawnBadgeTestClient(t *testing.T, roleBeads []*model.Bead) *mockBeadsClient {
	t.Helper()
	mc := newMockClient()
	mc.ListBeadsResult = roleBeads

	prevClient := beadsClient
	t.Cleanup(func() { beadsClient = prevClient })
	beadsClient = mc
	return mc
}

func privilegedRoleBead(role string) *model.Bead {
	fields, _ := json.Marshal(map[string]any{"privileged": true})
	return &model.Bead{ID: "gd-role1", Type: "role", Kind: "config", Title: role, Fields: fields}
}

func TestSpawnBadgeLabel_PolicyApproved(t *testing.T) {
	agentSpawnBadgeTestClient(t, nil)
	fields := map[string]any{"spawn_approval_mode": "auto", "role": "k8s-live"}
	label, danger := spawnBadgeLabel(context.Background(), fields)
	if label != "policy-approved" || danger {
		t.Fatalf("got label=%q danger=%v, want policy-approved/false", label, danger)
	}
}

func TestSpawnBadgeLabel_HumanApprovedWithApprover(t *testing.T) {
	agentSpawnBadgeTestClient(t, nil)
	fields := map[string]any{
		"spawn_approval_mode": "human",
		"spawn_approved_by":   "oidc:matthewbaker1110@gmail.com",
		"role":                "k8s-live",
	}
	label, danger := spawnBadgeLabel(context.Background(), fields)
	want := "human-approved (oidc:matthewbaker1110@gmail.com)"
	if label != want || danger {
		t.Fatalf("got label=%q danger=%v, want %q/false", label, danger, want)
	}
}

func TestSpawnBadgeLabel_HumanApprovedNoApproverName(t *testing.T) {
	agentSpawnBadgeTestClient(t, nil)
	fields := map[string]any{"spawn_approval_mode": "human", "role": "k8s-live"}
	label, danger := spawnBadgeLabel(context.Background(), fields)
	if label != "human-approved" || danger {
		t.Fatalf("got label=%q danger=%v, want human-approved/false", label, danger)
	}
}

func TestSpawnBadgeLabel_UnreviewedPrivilegedRole(t *testing.T) {
	agentSpawnBadgeTestClient(t, []*model.Bead{privilegedRoleBead("k8s-live")})
	fields := map[string]any{"role": "k8s-live"}
	label, danger := spawnBadgeLabel(context.Background(), fields)
	if !danger || !strings.Contains(label, "unreviewed") {
		t.Fatalf("got label=%q danger=%v, want an unreviewed/true result", label, danger)
	}
}

func TestSpawnBadgeLabel_NonPrivilegedRoleNoBadge(t *testing.T) {
	agentSpawnBadgeTestClient(t, []*model.Bead{privilegedRoleBead("k8s-live")})
	fields := map[string]any{"role": "crew"}
	label, danger := spawnBadgeLabel(context.Background(), fields)
	if label != "" || danger {
		t.Fatalf("got label=%q danger=%v, want no badge for a non-privileged role", label, danger)
	}
}

func TestSpawnBadgeLabel_NoRoleFieldNoBadge(t *testing.T) {
	agentSpawnBadgeTestClient(t, nil)
	label, danger := spawnBadgeLabel(context.Background(), map[string]any{})
	if label != "" || danger {
		t.Fatalf("got label=%q danger=%v, want no badge with no role recorded", label, danger)
	}
}

func TestPrintAgentSpawnBadge_NonAgentBeadPrintsNothing(t *testing.T) {
	agentSpawnBadgeTestClient(t, []*model.Bead{privilegedRoleBead("k8s-live")})
	fields, _ := json.Marshal(map[string]any{"role": "k8s-live"})
	bead := &model.Bead{ID: "gd-task1", Type: "task", Fields: fields}
	got := captureShowStdout(t, func() { printAgentSpawnBadge(context.Background(), bead) })
	if got != "" {
		t.Fatalf("expected no output for a non-agent bead, got %q", got)
	}
}

func TestPrintAgentSpawnBadge_AgentUnreviewedPrints(t *testing.T) {
	agentSpawnBadgeTestClient(t, []*model.Bead{privilegedRoleBead("k8s-live")})
	fields, _ := json.Marshal(map[string]any{"role": "k8s-live"})
	bead := &model.Bead{ID: "gd-agent1", Type: "agent", Fields: fields}
	got := captureShowStdout(t, func() { printAgentSpawnBadge(context.Background(), bead) })
	if !strings.Contains(got, "Spawn:") || !strings.Contains(got, "unreviewed") {
		t.Fatalf("got %q, want a Spawn: line with unreviewed", got)
	}
}

// A comma-joined role resolves to the privileged role bead in the pod spec, so
// the unreviewed call-out must fire for any element (gd-Yhbc5wdjwj). "crew,
// reviewer" is the positive control: no privileged element, no badge.
func TestSpawnBadgeLabel_MultiRoleAnyElementPrivileged(t *testing.T) {
	agentSpawnBadgeTestClient(t, []*model.Bead{privilegedRoleBead("infra")})
	for role, want := range map[string]bool{
		"crew,infra":    true,
		" infra":        true,
		"infra ":        true,
		"x, infra ,y":   true,
		"crew,reviewer": false,
		"crew":          false,
	} {
		_, danger := spawnBadgeLabel(context.Background(), map[string]any{"role": role})
		if danger != want {
			t.Errorf("role %q: danger = %v, want %v", role, danger, want)
		}
	}
}
