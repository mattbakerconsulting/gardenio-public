package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// seedLiveShapeEscalationPolicy seeds gd-r8WKkZb7H1's shape: #1 merges,
// #2 bead_replay (held by cluster, executed by the server), #3 bead_admin
// (held and executed by cluster), then a helm_deploy rule naming the desk (an
// agent-token coordinator), then the laptop's catch-all.
func seedLiveShapeEscalationPolicy(ms *mockStore) {
	lanes := []string{"coordinator:cluster", "coordinator:laptop", "operator"}
	pol, _ := json.Marshal(map[string]any{
		"rules": []any{
			map[string]any{"when": map[string]any{"projects": []string{"gardenio"}, "operation_types": []string{"github_pr_merge", "gitlab_mr_merge"}}, "approvers": []string{"coordinator:merge-desk", "coordinator:laptop", "operator"}, "execute_by": "coordinator:merge-desk"},
			map[string]any{"when": map[string]any{"operation_classes": []string{"bead_replay"}}, "hold_by": "coordinator:cluster", "approvers": lanes, "execute_by": "server"},
			map[string]any{"when": map[string]any{"operation_classes": []string{"bead_admin"}}, "hold_by": "coordinator:cluster", "approvers": lanes, "execute_by": "coordinator:cluster"},
			map[string]any{"when": map[string]any{"operation_types": []string{"helm_deploy"}}, "approvers": []string{"coordinator:merge-desk", "operator"}, "execute_by": "operator"},
			map[string]any{"when": map[string]any{}, "hold_by": "coordinator:laptop", "approvers": []string{"coordinator:laptop", "operator"}, "execute_by": "coordinator:laptop"},
		},
		"coordinators": map[string]any{
			"merge-desk": map[string]any{"principals": []string{"role:reviewer"}, "audience": "desk"},
			"laptop":     map[string]any{"principals": []string{"machine:gardenio-laptop-operator"}, "audience": "operator"},
			"cluster":    map[string]any{"principals": []string{"machine:gardenio-cluster-coordinator"}, "audience": "operator"},
		},
	})
	ms.beads["gd-pol-live"] = &model.Bead{ID: "gd-pol-live", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}
}

// D6(f) (gd-Gi140TvkbN): a coordinator lane of the escalation's stamped policy
// rule rejects an unapproved, non-floor, non-merge ask by recording a failure.
// gd-iYvyVsUaYy and gd-Tf7ViuofOq (authz infra-spawn asks, owner approvers,
// held by the owner lane) went moot and only the owner could clear them, and
// /reject is operator-only. It never posts success, and nobody outside the
// rule's coordinator lanes gains anything.
func TestCaptureEscalationResult_GateF_PolicyLaneRejects(t *testing.T) {
	ask := func(kv ...any) map[string]any {
		f := map[string]any{
			"operation_type": "create", "spawn_role": "infra", "policy_rule": "escalation#2",
			"operation_spec": map[string]any{"type": "agent", "title": "infra-x", "fields": map[string]any{"role": "infra", "project": "gardenio"}},
			"approvers":      []string{"oidc:owner@example.com"},
			"execute_by":     "server", "held_by": "operator", "operator_only": false,
			"requesting_agent": "gd-crew1", "requesting_agent_verified": "true",
		}
		for i := 0; i+1 < len(kv); i += 2 {
			f[kv[i].(string)] = kv[i+1]
		}
		return f
	}
	cluster, laptop := machineCtx("gardenio-cluster-coordinator"), machineCtx("gardenio-laptop-operator")
	desk := agentCtx("gd-rev1", "reviewer-a", "gardenio")
	cases := []struct {
		name   string
		ctx    context.Context
		f      map[string]any
		status string
		want   int
		gateOn bool // the case only holds with the D6 gate on (a machine result passes with it off)
	}{
		{"cluster rejects a spawn ask held by the operator", cluster, ask(), "failure", http.StatusOK, false},
		{"laptop rejects a spawn ask held by the server", laptop, ask("held_by", "server"), "failure", http.StatusOK, false},
		{"cluster rejects a non-spawn create", cluster, ask("operation_spec", map[string]any{"type": "task", "title": "t"}), "failure", http.StatusOK, false},
		{"cluster rejects an unapproved bead_admin ask it holds", cluster, ask("operation_type", "kbeads_admin_write", "policy_rule", "escalation#3", "execute_by", "coordinator:cluster", "held_by", "coordinator:cluster"), "failure", http.StatusOK, false},
		{"desk agent rejects an ask its rule names", desk, ask("operation_type", "helm_deploy", "policy_rule", "escalation#4", "execute_by", "operator"), "failure", http.StatusOK, false},
		{"desk agent on a rule that does not name it", desk, ask(), "failure", http.StatusForbidden, false},
		{"crew agent", agentCtx("gd-crew9", "crew-z", "gardenio"), ask("requesting_agent", "gd-other"), "failure", http.StatusForbidden, false},
		{"cluster posts success", cluster, ask(), "success", http.StatusForbidden, true},
		{"cluster on an approved ask", cluster, ask("approved_by", "owner@example.com", "approved_at", "2026-11-05T03:26:58Z"), "failure", http.StatusForbidden, true},
		{"cluster on a floor ask", cluster, ask("operator_only", true), "failure", http.StatusForbidden, true},
		{"cluster on the laptop's catch-all", cluster, ask("operation_type", "helm_deploy", "policy_rule", "escalation#5", "execute_by", "coordinator:laptop"), "failure", http.StatusForbidden, true},
		{"cluster on a rule that no longer matches the ask", cluster, ask("policy_rule", "escalation#3"), "failure", http.StatusForbidden, true},
		{"cluster on an ask with no policy_rule", cluster, ask("policy_rule", ""), "failure", http.StatusForbidden, true},
		{"desk on a merge it does not hold (D6(e) only)", desk, ask("operation_type", "github_pr_merge", "policy_rule", "escalation#1", "execute_by", "coordinator:merge-desk"), "failure", http.StatusForbidden, true},
		{"another machine", machineCtx("gardenio-controller"), ask(), "failure", http.StatusForbidden, true},
	}
	for _, gate := range []string{"false", "true"} {
		t.Run("gate="+gate, func(t *testing.T) {
			t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", gate)
			s, ms := newLaneTestServer()
			s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator", "gardenio-cluster-coordinator"})
			seedLiveShapeEscalationPolicy(ms)
			for i, c := range cases {
				if c.gateOn && gate != "true" {
					continue
				}
				id := fmt.Sprintf("gd-lr%d", i)
				seedRoutedEscalation(ms, id, "gardenio", c.f)
				if code := postResult(t, s, c.ctx, id, c.status); code != c.want {
					t.Errorf("%s: got %d, want %d", c.name, code, c.want)
				}
				if closed := ms.beads[id].Status == model.StatusClosed; closed != (c.want == http.StatusOK) {
					t.Errorf("%s: closed = %v", c.name, closed)
				}
				if v := escField(t, ms, id, "approved_at"); c.want == http.StatusOK && v != nil {
					t.Errorf("%s: approved_at = %v after a reject", c.name, v)
				}
			}
		})
	}
}
