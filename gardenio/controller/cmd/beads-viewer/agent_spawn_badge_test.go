package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// gd-jbY5UiWlN4: the agent detail card renders the three spawn-provenance
// cases distinctly — policy-approved, human-approved, and (for a privileged
// role with neither field recorded) a red "unreviewed" call-out — and stays
// silent for an ordinary, non-privileged spawn.

func agentSpawnBadgeFake(fields map[string]string, roles map[string]beadsapi.RoleInfo) *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-agent1": {
				ID:     "gd-agent1",
				Title:  "gardenio-peragent-record-of-55t",
				Kind:   "config",
				Type:   "agent",
				Status: "open",
				Fields: fields,
			},
		},
		rolesResp: roles,
	}
}

func TestAgentSpawnBadge_PolicyApproved(t *testing.T) {
	mux := mkServer(t, agentSpawnBadgeFake(map[string]string{
		"role":                "k8s-live",
		"spawn_approval_mode": "auto",
	}, nil))
	body := getSpawnBadgeBody(t, mux, "/gd-agent1")
	if !strings.Contains(body, `class="agent-spawn-badge agent-spawn-badge-policy"`) || !strings.Contains(body, "policy-approved") {
		t.Errorf("expected a policy-approved badge, got body:\n%s", body)
	}
	if strings.Contains(body, `agent-spawn-badge-unreviewed"`) {
		t.Errorf("policy-approved case must not also show an unreviewed badge")
	}
}

func TestAgentSpawnBadge_HumanApprovedWithApprover(t *testing.T) {
	mux := mkServer(t, agentSpawnBadgeFake(map[string]string{
		"role":                "k8s-live",
		"spawn_approval_mode": "human",
		"spawn_approved_by":   "oidc:matthewbaker1110@gmail.com",
	}, nil))
	body := getSpawnBadgeBody(t, mux, "/gd-agent1")
	if !strings.Contains(body, `class="agent-spawn-badge agent-spawn-badge-approved"`) ||
		!strings.Contains(body, "human-approved (oidc:matthewbaker1110@gmail.com)") {
		t.Errorf("expected a human-approved badge naming the approver, got body:\n%s", body)
	}
}

func TestAgentSpawnBadge_UnreviewedPrivilegedRole(t *testing.T) {
	mux := mkServer(t, agentSpawnBadgeFake(
		map[string]string{"role": "k8s-live"},
		map[string]beadsapi.RoleInfo{"k8s-live": {Name: "k8s-live", Privileged: true}},
	))
	body := getSpawnBadgeBody(t, mux, "/gd-agent1")
	if !strings.Contains(body, `class="agent-spawn-badge agent-spawn-badge-unreviewed"`) ||
		!strings.Contains(body, "unreviewed") {
		t.Errorf("expected an unreviewed badge for a privileged role with no approval, got body:\n%s", body)
	}
}

func TestAgentSpawnBadge_NonPrivilegedRoleNoBadge(t *testing.T) {
	mux := mkServer(t, agentSpawnBadgeFake(
		map[string]string{"role": "crew"},
		map[string]beadsapi.RoleInfo{"k8s-live": {Name: "k8s-live", Privileged: true}},
	))
	body := getSpawnBadgeBody(t, mux, "/gd-agent1")
	if strings.Contains(body, `class="agent-spawn-badge`) {
		t.Errorf("expected no spawn badge for a non-privileged role with no approval fields, got body:\n%s", body)
	}
}

func getSpawnBadgeBody(t *testing.T, mux http.Handler, path string) string {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for %s, got %d", path, w.Code)
	}
	return w.Body.String()
}

// A comma-joined role resolves to the privileged role bead in the pod spec, so
// the unreviewed call-out must fire for any element (gd-Yhbc5wdjwj). "crew,
// reviewer" stays the positive control: no privileged element, no badge.
func TestAgentSpawnBadge_MultiRoleAnyElementPrivileged(t *testing.T) {
	roles := map[string]beadsapi.RoleInfo{"infra": {Name: "infra", Privileged: true}, "crew": {Name: "crew"}}
	for role, want := range map[string]bool{
		"crew,infra":    true,
		" infra":        true,
		"infra ":        true,
		"x, infra ,y":   true,
		"crew,reviewer": false,
		"crew":          false,
	} {
		mux := mkServer(t, agentSpawnBadgeFake(map[string]string{"role": role}, roles))
		body := getSpawnBadgeBody(t, mux, "/gd-agent1")
		if got := strings.Contains(body, `class="agent-spawn-badge agent-spawn-badge-unreviewed"`); got != want {
			t.Errorf("role %q: unreviewed badge = %v, want %v", role, got, want)
		}
	}
}
