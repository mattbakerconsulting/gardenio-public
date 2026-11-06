package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// audienceCases are one escalation in each audience state the gate must tell
// apart (gd-bmOiteQi36): only an effective owner audience pings the owner.
var audienceCases = []struct {
	name   string
	fields map[string]string
	owner  bool
}{
	{"merge defaults to the desk", map[string]string{"operation_type": "gitlab_mr_merge"}, false},
	{"infra defaults to the operators", map[string]string{"operation_type": "helm_chart_change"}, false},
	{"routed to the operators", map[string]string{"operation_type": "gitlab_mr_merge", "audience": "operator"}, false},
	{"filed for the owner", map[string]string{"operation_type": "gitlab_mr_merge", "audience": "owner"}, true},
	{"needs credentials, filed for the desk", map[string]string{
		"operation_type": "gitlab_mr_merge", "audience": "desk", "credential_requirements": `[{"type":"github_sudo"}]`,
	}, true},
	{"secret, filed for the operators", map[string]string{
		"operation_type": "k8s_rbac", "audience": "operator", "sensitivity": "secret",
	}, true},
}

func TestEscalationOperatorDM_AudienceGate(t *testing.T) {
	for _, c := range audienceCases {
		t.Run(c.name, func(t *testing.T) {
			detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
			detail.Fields = c.fields
			e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			e.EscalationCreated(context.Background(), escalationEvent(detail))
			if got := len(sender.all()); got != map[bool]int{true: 1, false: 0}[c.owner] {
				t.Errorf("owner audience=%v: %d DM(s)", c.owner, got)
			}
		})
	}
}

func TestCrossOperatorNotify_AudienceGate(t *testing.T) {
	for _, c := range audienceCases {
		t.Run(c.name, func(t *testing.T) {
			fields := map[string]string{"approved_by": "gardenio-any-agent-fleet-ein"}
			for k, v := range c.fields {
				fields[k] = v
			}
			detail := &beadsapi.BeadDetail{
				ID: "gd-V6DZ8FFKD2", Type: "escalation", Status: "in_progress", Title: "an ask",
				OperatedBy: "oidc:terry.vance@acme.example", OperatedByID: i64(232), Fields: fields,
			}
			n, sender, _ := crossOperatorHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
			n.EscalationApproved(context.Background(), BeadEvent{
				ID: detail.ID, Type: "escalation", Status: "in_progress", Fields: fields,
			})
			if got := len(sender.all()); got != map[bool]int{true: 1, false: 0}[c.owner] {
				t.Errorf("owner audience=%v: %d notice(s)", c.owner, got)
			}
		})
	}
}

// The event's fields fill what a sparse read lacks, and the fresh read wins.
func TestEscalationAudienceFields(t *testing.T) {
	got := escalationAudienceFields(
		map[string]string{"audience": "owner", "sensitivity": ""},
		map[string]string{"audience": "desk", "operation_type": "gitlab_mr_merge", "sensitivity": "secret"},
	)
	if got["audience"] != "owner" || got["operation_type"] != "gitlab_mr_merge" || got["sensitivity"] != "secret" {
		t.Errorf("merged fields = %v", got)
	}
}
