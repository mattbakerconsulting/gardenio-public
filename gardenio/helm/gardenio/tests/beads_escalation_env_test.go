package tests

import "testing"

// Escalation routing T13 (gd-WxmVN15PQe): the beads Deployment renders
// BEADS_ESCALATION_COORDINATOR_CLIENTS and BEADS_ESCALATION_RESULT_MACHINES
// only when set, and BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL always
// (kbeads/internal/server/escalation_lane_members.go, escalation_result_gate.go).

const (
	escCoordinatorClientsEnv = "BEADS_ESCALATION_COORDINATOR_CLIENTS"
	escResultMachinesEnv     = "BEADS_ESCALATION_RESULT_MACHINES"
	escResultApprovalEnv     = "BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL"
	escApproverClientsEnv    = "BEADS_ESCALATION_APPROVER_CLIENTS"
)

func TestEscalationEnv_ChartDefaults(t *testing.T) {
	t.Parallel()
	d := renderDeployment(t, beadsDeploymentTemplate, nil)
	if _, ok := d.env("", "BEADS_HTTP_ADDR"); !ok {
		t.Fatalf("control: beads Deployment did not render the daemon env")
	}
	for _, name := range []string{escCoordinatorClientsEnv, escResultMachinesEnv} {
		if e, ok := d.env("", name); ok {
			t.Errorf("default render sets %s=%q; it must render only when non-empty", name, e.Value)
		}
	}
	if e, ok := d.env("", escResultApprovalEnv); !ok || e.Value != "false" {
		t.Errorf("%s = %q (present %v), want \"false\" always rendered", escResultApprovalEnv, e.Value, ok)
	}
}

func TestEscalationEnv_SetRendersAll(t *testing.T) {
	t.Parallel()
	d := renderDeployment(t, beadsDeploymentTemplate, nil,
		"beads.escalationCoordinatorClients=a-client",
		"beads.escalationResultMachines=a-reaper",
		"beads.escalationResultRequiresApproval=true")
	for name, want := range map[string]string{
		escCoordinatorClientsEnv: "a-client",
		escResultMachinesEnv:     "a-reaper",
		escResultApprovalEnv:     "true",
	} {
		if e, ok := d.env("", name); !ok || e.Value != want {
			t.Errorf("%s = %q (present %v), want %q", name, e.Value, ok, want)
		}
	}
}

func TestEscalationEnv_K3sDeployValues(t *testing.T) {
	t.Parallel()
	d := renderDeployment(t, beadsDeploymentTemplate, []string{k3sOverlayPath(t)})
	// Task 15 (gd-yT5xAPB0uJ) adds the coordinator:cluster lane's client.
	const wantCoordinators = "gardenio-laptop-operator,gardenio-cluster-coordinator"
	if e, ok := d.env("", escCoordinatorClientsEnv); !ok || e.Value != wantCoordinators {
		t.Errorf("%s = %q (present %v), want %s", escCoordinatorClientsEnv, e.Value, ok, wantCoordinators)
	}
	// Empty on purpose: no live machine witnesses merges (plan Contract Issue 4).
	if e, ok := d.env("", escResultMachinesEnv); ok {
		t.Errorf("%s = %q; the k3s overlay leaves it empty, so it must not render", escResultMachinesEnv, e.Value)
	}
	// Task 14 Step 6 (gd-VJ7Pti7c5q) turned the /result gate on.
	if e, ok := d.env("", escResultApprovalEnv); !ok || e.Value != "true" {
		t.Errorf("%s = %q (present %v), want \"true\"", escResultApprovalEnv, e.Value, ok)
	}
	// P10.B (gd-4mEAhIE01U): the daemon no longer reads it; only the page
	// session approves.
	if e, ok := d.env("", escApproverClientsEnv); ok {
		t.Errorf("%s = %q; the knob was dropped, so it must not render", escApproverClientsEnv, e.Value)
	}
}
