package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// seedPrivilegedRole stores a type:role bead roleIsPrivileged reads as
// privileged (makeRoleBead, spawn_authz_test.go), with a dedicated
// ServiceAccount, so its pod holds a kubeconfig (roleExecutorIdentities).
func seedPrivilegedRole(t *testing.T, ms *mockStore, name string) {
	t.Helper()
	seedExecutorRole(ms, name, map[string]any{"service_account": "gardenio-" + name})
}

// seedExecutorRole stores a privileged role bead with extra fields.
func seedExecutorRole(ms *mockStore, name string, extra map[string]any) {
	fields := map[string]any{"privileged": "true", "needs_k8s_api": "true"}
	for k, v := range extra {
		fields[k] = v
	}
	raw, _ := json.Marshal(fields)
	ms.beads["gd-role-"+name] = &model.Bead{ID: "gd-role-" + name, Title: name, Type: "role", Kind: "config", Status: model.StatusOpen, Fields: raw}
}

func escCreate(fields map[string]any) map[string]any {
	return map[string]any{"title": "e", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"}, "fields": fields}
}

// local requires a named, open, registered runner (spec §8.1, Q2).
func TestEscalationTargets_LocalNeedsRunner(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedRunnerAndAgent(ms, "gd-r1", "laptop-agent")
	spec := map[string]any{"m": 1}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": spec, "execution_target": "local"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("local without target_runner: %d, want 400", rec.Code)
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": spec, "execution_target": "local", "target_runner": "gd-nope"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("local with unknown runner: %d, want 400", rec.Code)
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": spec, "execution_target": "local", "target_runner": "gd-r1"}), nil); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: local with a real runner: %d; %s", rec.Code, rec.Body.String())
	}
}

// target_runner names the runner, not an agent session on it: the agent bead
// P7 Task 13 accepted (gb agent start --local, label execution_target:laptop)
// is refused now. Positive control: the runner that agent runs on.
func TestEscalationTargets_AgentBeadIsNotARunner(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedRunnerAndAgent(ms, "gd-r1", "matthew-laptop")
	ms.labels["gd-matthew-laptop"] = []string{"project:gardenio", "execution_target:laptop"}
	for runner, want := range map[string]int{"gd-matthew-laptop": http.StatusBadRequest, "gd-r1": http.StatusCreated} {
		if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "local", "target_runner": runner}), nil); rec.Code != want {
			t.Fatalf("target_runner=%s: %d, want %d; %s", runner, rec.Code, want, rec.Body.String())
		}
	}
}

// Approving a local escalation records the agent joined on its runner as
// executor, so that agent (and only it) may post the result (Task 10).
func TestApprove_LocalStampsRunnerAsExecutor(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedRunnerFor(ms, "gd-r2", "oidc:owner@example.com", "crew-a")
	seedEscalation(t, ms, "gd-l1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "local", "target_runner": "gd-r2"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-l1/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-l1"); f["executor"] != "crew-a" || f["execution_dispatch"] != "local" {
		t.Fatalf("executor=%v dispatch=%v", f["executor"], f["execution_dispatch"])
	}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-l1/result", testAgentToken, map[string]any{"result_status": "success"}, nil), http.StatusOK)
}

// An approve-time --target meets the same checks as a filed target: local
// with no runner is refused before the dispatch is claimed.
func TestApprove_TargetOverrideChecked(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedEscalation(t, ms, "gd-l2", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-l2/approve", "owner@example.com", map[string]any{"execution_target": "local"}, nil), http.StatusBadRequest)
	if f := escFields(t, ms, "gd-l2"); f["execution_dispatch"] != nil || f["approved_by"] != nil {
		t.Fatalf("refused override mutated the escalation: %v", f)
	}
}

// service_account needs a privileged spawn_role and an execution_script; the
// daemon creates the executor agent bead with that role (spec Q3).
func TestApprove_ServiceAccountSpawnsPrivilegedRole(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedPrivilegedRole(t, ms, "k8s-live")
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "service_account", "spawn_role": "crew", "execution_script": "kubectl apply -f x"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("service_account with a non-privileged role: %d, want 400", rec.Code)
	}
	// The requester's Slack routing rides onto the executor (gd-Q553TCVjN4).
	reqRaw, _ := json.Marshal(map[string]string{"agent": "asker", "slack_thread_channel": "C1", "slack_thread_ts": "171.1", "thread_id": "gd-wish"})
	if err := ms.CreateBead(context.Background(), &model.Bead{ID: "gd-asker", Title: "asker", Type: "agent", Kind: model.KindConfig, Status: model.StatusOpen, Fields: reqRaw}); err != nil {
		t.Fatal(err)
	}
	seedEscalation(t, ms, "gd-sa1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "service_account", "spawn_role": "k8s-live", "execution_script": "kubectl apply -f x", "requesting_agent": "gd-asker"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-sa1/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	f := escFields(t, ms, "gd-sa1")
	if f["executor"] != "escalation-sa-gd-sa1" {
		t.Fatalf("executor = %v", f["executor"])
	}
	agent, err := ms.FindAgentByName(context.Background(), "escalation-sa-gd-sa1", nil)
	if err != nil || agent == nil {
		t.Fatalf("no executor agent bead: %v", err)
	}
	af := escalationStringFields(agent)
	if af["role"] != "k8s-live" || af["spawned_by"] != "gd-sa1" || af["spawn_approved_by"] != "owner@example.com" {
		t.Fatalf("agent fields: %v", af)
	}
	if af["slack_thread_channel"] != "C1" || af["slack_thread_ts"] != "171.1" || af["thread_id"] != "gd-wish" || af["slack_thread_bead_id"] != "gd-wish" {
		t.Fatalf("executor lacks the requester's Slack routing: %v", af)
	}
}

// A service_account escalation filed before the create checks, with no
// privileged spawn_role, spawns nothing at approval. Its k8s_apply needs a
// kubeconfig no role grants it, so it dispatches none to the operator (P7.3).
func TestApprove_ServiceAccountWithoutPrivilegedRoleSpawnsNothing(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedEscalation(t, ms, "gd-sa2", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "service_account"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-sa2/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-sa2"); f["executor"] != nil || f["execution_dispatch"] != dispatchNone || f["execute_by"] != "operator" {
		t.Fatalf("executor=%v dispatch=%v execute_by=%v", f["executor"], f["execution_dispatch"], f["execute_by"])
	}
	if agent, _ := ms.FindAgentByName(context.Background(), "escalation-sa-gd-sa2", nil); agent != nil {
		t.Fatalf("spawned an executor with no privileged role: %v", escalationStringFields(agent))
	}
}
