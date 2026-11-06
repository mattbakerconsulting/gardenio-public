package server

import (
	"context"
	"net/http"
	"testing"
)

// executorBounceShapes are the approved escalations whose executor pod had no
// identity for the work (P7.3, gd-9s9CLCfbB6; listed from gd-OzKA0KptyC and
// gd-p57IcSTGIZ): the operation type and the filed execution_target.
var executorBounceShapes = []struct{ id, op, target string }{
	{"gd-OzKA0KptyC", "kbeads_bead_close", "local"},
	{"gd-p57IcSTGIZ", "gardener_run_budget", "local"},
	{"gd-KTN1wOka0n", "gardener_run_budget", ""},
	{"gd-Z7Pz2reaBG", "kbeads_bead_close", "server"},
	{"gd-7zjEgAp7vf", "bead_close", "local"},
	{"gd-xFUgnULUQL", "beads_admin_edit", "local"},
	{"gd-GoPqbwmNUf", "k8s_secret_seed", ""},
	{"gd-JayGpjjX8f", "trigger_prompt_reseal", ""},
	{"gd-7A9MxmhLzf", "k8s_manifest_apply", "server"},
	{"gd-AfHrMMedS0", "beads_dep_add", "local"},
	{"gd-Cxm5zRSDvI", "helm_upgrade", "local"},
	{"gd-fSLU7vQJlR", "helm_values_and_terraform_apply", ""},
	{"gd-seCXUzmKFv", "release_roll", "local"},
	{"gd-DyvcBCrawO", "helm_deploy", ""},
	{"gd-ho9qaUi6IO", "helm_deploy", "local"},
	{"gd-iYEq8F3rX0", "credential_rotation", "local"},
	{"gd-jwPVEIyhWY", "keycloak_idp_rollout", "local"},
	{"gd-C6ubPkpjrj", "manual_operator_action", "local"},
	{"gd-587LZrJFYI", "release_helm_upgrade", "local"},
}

// TestApprove_NoBounceShapeSpawnsAPod: each shape, approved into the pod it
// bounced from (a privileged crew role with no ServiceAccount, AWS keys or
// admin login), spawns no pod. A filed local one runs on its runner; any
// other goes to the operator, who also holds it.
func TestApprove_NoBounceShapeSpawnsAPod(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedExecutorRole(ms, "crew-pod", nil)
	seedRunnerFor(ms, "gd-r1", "oidc:owner@example.com", "laptop-agent")
	for _, c := range executorBounceShapes {
		t.Run(c.id, func(t *testing.T) {
			id := "gd-b-" + c.id[3:]
			f := map[string]any{
				"operation_type": canonicalOperationType(c.op), "operation_spec": map[string]any{},
				"spawn_role": "crew-pod", "execution_script": "run it", "execution_target": c.target,
			}
			body := map[string]any{}
			if c.target == "local" {
				f["target_runner"] = "gd-r1"
				body["execution_target"] = "service_account"
			} else {
				f["execution_target"] = "service_account"
			}
			seedEscalation(t, ms, id, "", f)
			rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", "owner@example.com", body, nil)
			requireStatus(t, rec, http.StatusOK)
			if agent, _ := ms.FindAgentByName(context.Background(), "escalation-sa-"+id, nil); agent != nil {
				t.Fatalf("spawned an executor pod: %v", escalationStringFields(agent))
			}
			got := escFields(t, ms, id)
			if got["executor_refused"] == nil {
				t.Errorf("no executor_refused recorded: %v", got)
			}
			if c.target == "local" {
				if got["execution_dispatch"] != dispatchLocal || got["executor"] != "laptop-agent" || got["execution_target"] != "local" {
					t.Errorf("local: dispatch=%v executor=%v target=%v", got["execution_dispatch"], got["executor"], got["execution_target"])
				}
				return
			}
			if got["execution_dispatch"] != dispatchNone || got["executor"] != nil {
				t.Errorf("dispatch=%v executor=%v, want none and no executor", got["execution_dispatch"], got["executor"])
			}
			if got["execute_by"] != "operator" || got["held_by"] != "operator" {
				t.Errorf("execute_by=%v held_by=%v, want operator", got["execute_by"], got["held_by"])
			}
		})
	}
}

// A role whose pod holds what the work needs still gets its executor: a
// ServiceAccount for k8s_apply, a ServiceAccount and AWS keys for a release
// roll.
func TestApprove_ExecutorPodWithTheIdentitySpawns(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedPrivilegedRole(t, ms, "k8s-live")
	seedExecutorRole(ms, "roller", map[string]any{"service_account": "gardenio-roller", "secrets": []map[string]string{{"env": "AWS_ACCESS_KEY_ID", "secret": "s", "key": "k"}}})
	for id, c := range map[string]struct{ op, role string }{
		"gd-fit1": {"k8s_apply", "k8s-live"},
		"gd-fit2": {"helm_deploy", "roller"},
	} {
		seedEscalation(t, ms, id, "", map[string]any{"operation_type": c.op, "execution_target": "service_account", "spawn_role": c.role, "execution_script": "run it"})
		requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
		if f := escFields(t, ms, id); f["executor"] != "escalation-sa-"+id || f["executor_refused"] != nil {
			t.Errorf("%s: executor=%v refused=%v", id, f["executor"], f["executor_refused"])
		}
	}
	// Negative control: the same release roll for k8s-live, which has no AWS keys.
	seedEscalation(t, ms, "gd-fit3", "", map[string]any{"operation_type": "helm_deploy", "execution_target": "service_account", "spawn_role": "k8s-live", "execution_script": "run it"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-fit3/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-fit3"); f["executor"] != nil || f["executor_refused"] != "operation helm_deploy needs AWS, which a k8s-live pod does not hold" {
		t.Errorf("gd-fit3: executor=%v refused=%v", f["executor"], f["executor_refused"])
	}
}

// A refused bead-only escalation goes where P7.1 sends it: coordinator:cluster
// when that lane is staffed, else the operator.
func TestRefuseExecutorPod_BeadOnlyGoesToTheClusterLane(t *testing.T) {
	for staffed, want := range map[bool]string{true: clusterCoordinatorLane, false: "operator"} {
		s, ms := newRetargetTestServer(staffed)
		seedRoutedEscalation(ms, "gd-bo", "gardenio", map[string]any{"operation_type": "bead_close", "approvers": []string{"operator"}, "operator_only": false})
		esc, _ := ms.GetBead(context.Background(), "gd-bo")
		extra := map[string]any{}
		s.refuseExecutorPod(context.Background(), esc, dispatchNone, "why", extra)
		if extra["execute_by"] != want || extra["executor_refused"] != "why" {
			t.Errorf("staffed=%v: %v, want execute_by %s", staffed, extra, want)
		}
	}
}

// Every class operationClassIdentities names is a registered class, so a
// rename in the registry cannot drop a class's check unseen.
func TestOperationClassIdentities_NameRegisteredClasses(t *testing.T) {
	for class := range operationClassIdentities {
		if !isOperationClass(class) {
			t.Errorf("operationClassIdentities names %q, which the registry lacks", class)
		}
	}
	for op := range operationTypeIdentities {
		if operationClassOf(op) == "" {
			t.Errorf("operationTypeIdentities names %q, which the registry lacks", op)
		}
	}
}
