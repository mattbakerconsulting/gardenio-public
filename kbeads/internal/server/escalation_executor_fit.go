package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Executor fit (agent-access programme P7.3, gd-9s9CLCfbB6): an approval
// spawns a service_account executor pod only when that pod can do the
// operation. An escalation filed execution_target=local runs on the filer's
// machine, never a pod, and an operation class that needs an identity the
// spawn_role's pod lacks is not handed to it. Either way no pod is spawned
// and execution goes to P7.1's lane (executionLane).

// executorIdentity is something an operation's work needs that an executor
// pod holds only when its role grants it.
type executorIdentity string

const (
	// A dedicated ServiceAccount token: the role sets needs_k8s_api and its own
	// service_account, which is how the controller mounts one (podspec.go).
	identityKube executorIdentity = "kubeconfig"
	// AWS keys: the role's secrets set AWS_ACCESS_KEY_ID.
	identityAWS executorIdentity = "AWS"
	// A gardenio-admin OIDC session. No pod holds one.
	identityAdmin executorIdentity = "admin login"
	// The owner at a device, a browser or an external account. Never a pod.
	identityOwner executorIdentity = "owner"
)

// operationClassIdentities are the identities each registered class's work
// needs (escalation_op_registry.go). A class not listed needs none a pod can
// lack; an unregistered type is not checked.
var operationClassIdentities = map[string][]executorIdentity{
	"release_roll":  {identityKube, identityAWS}, // release-deploy.sh checks ECR, then helm upgrades
	"deploy_values": {identityKube},
	"k8s_apply":     {identityKube},
	"k8s_rbac":      {identityKube},
	"edge":          {identityKube},
	"prod_read":     {identityKube},
	"database":      {identityKube},
	"credential":    {identityKube},
	"identity":      {identityKube},
	"terraform":     {identityAWS},
	"cloud_infra":   {identityAWS},
	"bead_admin":    {identityAdmin},
	"bead_close":    {identityAdmin},
	"advice":        {identityAdmin}, // a pod; the home steward executes its project's on its own token (escalation_steward_advice.go)
	"trigger":       {identityAdmin},
	"kbeads_access": {identityAdmin},
	"app_access":    {identityOwner},
	"owner_action":  {identityOwner},
}

// operationTypeIdentities overrides the class for a type whose work spans two.
var operationTypeIdentities = map[string][]executorIdentity{
	"helm_values_and_terraform_apply": {identityKube, identityAWS},
}

// operationIdentities returns the identities op's work needs.
func operationIdentities(op string) []executorIdentity {
	if ids, ok := operationTypeIdentities[canonicalOperationType(op)]; ok {
		return ids
	}
	return operationClassIdentities[operationClassOf(op)]
}

// roleExecutorFields are the role bead fields that decide a pod's identities.
type roleExecutorFields struct {
	ServiceAccount string `json:"service_account"`
	NeedsK8sAPI    any    `json:"needs_k8s_api"`
	Secrets        []struct {
		Env string `json:"env"`
	} `json:"secrets"`
}

// roleExecutorIdentities returns the identities a pod of role holds: the
// union over its elements' role beads. A role with no bead holds none.
func (s *BeadsServer) roleExecutorIdentities(ctx context.Context, role string) (map[executorIdentity]bool, error) {
	held := map[executorIdentity]bool{}
	elems := parseRoleList(role)
	if len(elems) == 0 {
		return held, nil
	}
	beads, err := roleBeadsByTitle(ctx, s.store, elems)
	if err != nil {
		return nil, err
	}
	for _, b := range beads {
		var f roleExecutorFields
		if len(b.Fields) == 0 || json.Unmarshal(b.Fields, &f) != nil {
			continue
		}
		if strings.TrimSpace(f.ServiceAccount) != "" && (f.NeedsK8sAPI == true || f.NeedsK8sAPI == "true") {
			held[identityKube] = true
		}
		for _, sec := range f.Secrets {
			if sec.Env == "AWS_ACCESS_KEY_ID" {
				held[identityAWS] = true
			}
		}
	}
	return held, nil
}

// executorPodRefusal says why approving esc as target (esc with any
// approve-time execution_target) must not spawn its service_account executor
// pod, or "" when it may (or target spawns no pod).
func (s *BeadsServer) executorPodRefusal(ctx context.Context, esc, target *model.Bead) (string, error) {
	if escalationDispatchOf(target) != dispatchServiceAccount {
		return "", nil
	}
	if escalationStringFields(esc)["execution_target"] == dispatchLocal {
		return "filed execution_target=local: it runs on the filer's own machine, not in a pod", nil
	}
	f := escalationStringFields(target)
	needs := operationIdentities(f["operation_type"])
	if len(needs) == 0 {
		return "", nil
	}
	held, err := s.roleExecutorIdentities(ctx, f["spawn_role"])
	if err != nil {
		return "", fmt.Errorf("read spawn_role %q: %w", f["spawn_role"], err)
	}
	var missing []string
	for _, id := range needs {
		if !held[id] {
			missing = append(missing, string(id))
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	return fmt.Sprintf("operation %s needs %s, which a %s pod does not hold",
		normalizeOperationType(f["operation_type"]), strings.Join(missing, " and "), f["spawn_role"]), nil
}

// approvalDispatch returns the bead an approval dispatches (esc with the
// approver's execution_target override) and its dispatch, with P7.3 applied:
// when executorPodRefusal refuses the pod, the override is dropped, a filed
// service_account dispatches none, and refused says why.
func (s *BeadsServer) approvalDispatch(ctx context.Context, esc *model.Bead, execTarget string, extra map[string]any) (target *model.Bead, dispatch, refused string, err error) {
	target = withEscalationTarget(esc, execTarget)
	if refused, err = s.executorPodRefusal(ctx, esc, target); err != nil || refused == "" {
		return target, escalationDispatchOf(target), refused, err
	}
	delete(extra, "execution_target")
	if dispatch = escalationDispatchOf(esc); dispatch == dispatchServiceAccount {
		dispatch = dispatchNone
	}
	s.refuseExecutorPod(ctx, esc, dispatch, refused, extra)
	return esc, dispatch, refused, nil
}

// refuseExecutorPod records in extra that no pod runs esc and why, and, when
// nothing else will execute it (dispatch none), moves execute_by to
// executionLane's lane, which stampEscalationHolder then holds it by.
func (s *BeadsServer) refuseExecutorPod(ctx context.Context, esc *model.Bead, dispatch, why string, extra map[string]any) {
	extra["executor_refused"] = why
	slog.Info("escalation approve: no executor pod", "escalation", esc.ID, "reason", why, "dispatch", dispatch)
	if dispatch != dispatchNone {
		return
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		p = defaultEscalationPolicy()
	}
	if lane := s.executionLane(ctx, esc, laneRef{}, p); lane != "" {
		extra["execute_by"] = lane
	}
}
