package tests

import (
	"strings"
	"testing"
)

// Idle-node detection env wiring on the agent-controller deployment
// (P1b-follow-up, ka-0000001398). The chart contract these tests pin:
//   - NODE_LIFECYCLE_IDLE_POD_NAMESPACES renders only when BOTH
//     agents.nodeLifecycle.enabled AND agents.nodeLifecycle.idleNodes.enabled,
//   - empty podNamespaces defaults the value to the release namespace,
//   - an explicit podNamespaces list renders comma-joined.
//
// Detection reuses the controller's existing namespace-scoped pods:list RBAC —
// there is NO new RBAC template to gate on, so these env-wiring assertions are
// the chart-side contract for the feature.

const idleEnvTpl = "templates/controller/deployment.yaml"

// TestIdleNode_EnvOffByDefault: the node-lifecycle collector alone (idle-node
// detection off) must render no idle-pod-namespaces env, so the controller
// leaves the idle gauge unconstructed.
func TestIdleNode_EnvOffByDefault(t *testing.T) {
	t.Parallel()
	// Collector on, idle detection off (the default).
	out := helmTemplate(t, idleEnvTpl,
		"agents.enabled=true",
		"agents.nodeLifecycle.enabled=true",
	)
	if strings.Contains(out, "NODE_LIFECYCLE_IDLE_POD_NAMESPACES") {
		t.Errorf("NODE_LIFECYCLE_IDLE_POD_NAMESPACES must NOT render with idleNodes.enabled=false; got:\n%s", out)
	}

	// Idle detection requested but the whole collector off — still nothing,
	// because the idle env is nested under the nodeLifecycle.enabled block.
	out = helmTemplate(t, idleEnvTpl,
		"agents.enabled=true",
		"agents.nodeLifecycle.enabled=false",
		"agents.nodeLifecycle.idleNodes.enabled=true",
	)
	if strings.Contains(out, "NODE_LIFECYCLE_IDLE_POD_NAMESPACES") {
		t.Errorf("NODE_LIFECYCLE_IDLE_POD_NAMESPACES must NOT render when the collector is off; got:\n%s", out)
	}
}

// TestIdleNode_EnvDefaultsToReleaseNamespace: with idle detection on and no
// explicit podNamespaces, the env defaults to the release namespace (where agent
// pods run).
func TestIdleNode_EnvDefaultsToReleaseNamespace(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio-system", idleEnvTpl,
		"agents.enabled=true",
		"agents.nodeLifecycle.enabled=true",
		"agents.nodeLifecycle.idleNodes.enabled=true",
	)
	if !strings.Contains(out, "NODE_LIFECYCLE_IDLE_POD_NAMESPACES") {
		t.Fatalf("NODE_LIFECYCLE_IDLE_POD_NAMESPACES must render when idleNodes.enabled=true; got:\n%s", out)
	}
	if !strings.Contains(out, `value: "gardenio-system"`) {
		t.Errorf("empty podNamespaces must default to the release namespace; got:\n%s", out)
	}
}

// TestIdleNode_EnvExplicitNamespacesJoined: an explicit podNamespaces list is
// rendered comma-joined (the controller splits it back to a slice).
func TestIdleNode_EnvExplicitNamespacesJoined(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, idleEnvTpl,
		"agents.enabled=true",
		"agents.nodeLifecycle.enabled=true",
		"agents.nodeLifecycle.idleNodes.enabled=true",
		"agents.nodeLifecycle.idleNodes.podNamespaces={gardenio-system,gardenio-gvisor}",
	)
	if !strings.Contains(out, `value: "gardenio-system,gardenio-gvisor"`) {
		t.Errorf("explicit podNamespaces must render comma-joined; got:\n%s", out)
	}
}
