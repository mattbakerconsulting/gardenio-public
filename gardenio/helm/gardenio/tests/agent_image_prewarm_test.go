package tests

import (
	"strings"
	"testing"
)

// agent-image-prewarm.yaml renders a per-pool DaemonSet that pulls the agent
// image onto every node of the target pool BEFORE an agent is scheduled there,
// collapsing the cold-pull spawn-latency penalty (worst on the gVisor pool:
// ~4–5 min cold vs ~135s runc). Task ka-0000000861.

func TestPrewarm_DefaultOff(t *testing.T) {
	t.Parallel()
	// --show-only on a template that renders nothing errors in helm; use the
	// full render and assert the DaemonSet is absent.
	out := helmTemplate(t, "")
	if strings.Contains(out, "gardenio-agent-prewarm") {
		t.Errorf("prewarm DaemonSet should NOT render when prewarmImage.enabled is unset (default off); got:\n%s", out)
	}
}

func TestPrewarm_BothPoolsRenderWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
	)
	if !strings.Contains(out, "test-agent-prewarm-agent") {
		t.Error("expected standard agent-pool prewarm DaemonSet")
	}
	if !strings.Contains(out, "test-agent-prewarm-gvisor") {
		t.Error("expected gVisor-pool prewarm DaemonSet")
	}
	if strings.Count(out, "kind: DaemonSet") != 2 {
		t.Errorf("expected exactly 2 DaemonSets (agent + gvisor); got:\n%s", out)
	}
}

func TestPrewarm_PerPoolGating(t *testing.T) {
	t.Parallel()
	// Disabling the standard agent pool leaves only the gVisor DaemonSet.
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
		"agents.prewarmImage.pools.agent.enabled=false",
	)
	if strings.Contains(out, "test-agent-prewarm-agent\n") || strings.Contains(out, "name: test-agent-prewarm-agent") {
		t.Errorf("standard agent-pool DaemonSet should be gated off; got:\n%s", out)
	}
	if !strings.Contains(out, "test-agent-prewarm-gvisor") {
		t.Error("gVisor-pool DaemonSet should still render")
	}
}

// The prewarm pod MUST run the SAME agent image as spawned agent pods, resolved
// via gardenio.imageRef so a release bump re-pulls the new layers automatically.
func TestPrewarm_UsesAgentImageRef(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
		"agents.agentImage.repository=agent",
		"agents.agentImage.tag=2026.99.9",
	)
	if !strings.Contains(out, "/agent:2026.99.9") {
		t.Errorf("prewarm pod image must track agents.agentImage tag; got:\n%s", out)
	}
}

// A release-pinned digest on agentImage must flow through to the prewarm image
// ref so the prewarm pull is byte-identical to what the agent pod will run.
func TestPrewarm_HonorsAgentImageDigest(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
		"agents.agentImage.tag=2026.99.9",
		"agents.agentImage.digest="+digest,
	)
	if !strings.Contains(out, "@"+digest) {
		t.Errorf("prewarm image ref must carry the agentImage digest; got:\n%s", out)
	}
}

// gVisor pool pod must select the gvisor=enabled node label and tolerate the
// gvisor=enabled:NoSchedule taint — mirroring the gvisor-probe DaemonSet — so it
// lands exactly one pod per runsc node.
func TestPrewarm_GvisorPoolScheduling(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
		"agents.prewarmImage.pools.agent.enabled=false",
	)
	if !strings.Contains(out, "gvisor: enabled") {
		t.Error("gVisor prewarm pod must select gvisor=enabled nodes")
	}
	if !strings.Contains(out, "key: gvisor") {
		t.Error("gVisor prewarm pod must tolerate the gvisor taint")
	}
	// The prewarm pod runs under the node default runtime (runc) — it must NOT
	// set runtimeClassName: runsc (it only needs the image layers cached).
	if strings.Contains(out, "runtimeClassName") {
		t.Errorf("prewarm pod must not set runtimeClassName; got:\n%s", out)
	}
}

// Standard agent pool pod must select gardenio.io/pool=agent and tolerate the
// gardenio.agent taint.
func TestPrewarm_AgentPoolScheduling(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
		"agents.prewarmImage.pools.gvisor.enabled=false",
	)
	if !strings.Contains(out, "gardenio.io/pool: agent") {
		t.Error("agent-pool prewarm pod must select gardenio.io/pool=agent nodes")
	}
	if !strings.Contains(out, "key: gardenio.agent") {
		t.Error("agent-pool prewarm pod must tolerate the gardenio.agent taint")
	}
}

// The prewarm pod carries no credentials and needs no K8s API; it must not
// automount a SA token.
func TestPrewarm_NoServiceAccountToken(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
	)
	if !strings.Contains(out, "automountServiceAccountToken: false") {
		t.Errorf("prewarm pod must not automount a SA token; got:\n%s", out)
	}
}

// The prewarm pods MUST carry the gardenio.io/prewarm-pool label on the POD
// template (not just the DaemonSet), and that label must be UNIQUE to prewarm
// pods. The prod release readiness gate (the RWX-era verify-prod step) excluded
// the best-effort prewarm DaemonSet from the gate via the DoesNotExist selector
// term `!gardenio.io/prewarm-pool` — because a multi-minute cold pull of the
// fresh agent image on a gVisor node is not a core-app release-readiness signal
// and otherwise red-lines the release even after the core stack rolled out
// (ka-0000005457: 2026.181.11 + .12 FALSE-RED). This test pins the invariant so
// a future refactor cannot drop/rename the label and silently re-couple the
// release verdict to the prewarm cache.
func TestPrewarm_PrewarmPoolLabelOnPodTemplate(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
	)
	// Both pools carry the label keyed to the pool name. The gate's
	// DoesNotExist term keys on the label KEY alone, so any value suffices.
	if !strings.Contains(out, "gardenio.io/prewarm-pool: \"agent\"") {
		t.Errorf("agent-pool prewarm pod must carry gardenio.io/prewarm-pool=agent; got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio.io/prewarm-pool: \"gvisor\"") {
		t.Errorf("gVisor-pool prewarm pod must carry gardenio.io/prewarm-pool=gvisor; got:\n%s", out)
	}
	// The label must appear on the pod template (under spec.template.metadata),
	// not only on the DaemonSet metadata — the gate selects PODS. Each rendered
	// DaemonSet should carry the label at least 3x (DS metadata + matchLabels +
	// pod template); assert >=2 occurrences per pool as a floor.
	if got := strings.Count(out, "gardenio.io/prewarm-pool: \"gvisor\""); got < 2 {
		t.Errorf("gardenio.io/prewarm-pool=gvisor must appear on the pod template (>=2 occurrences); got %d:\n%s", got, out)
	}
}

// Pod must satisfy restricted PSA admission (it renders into the restricted
// main namespace): runAsNonRoot, drop ALL caps, no privilege escalation,
// RuntimeDefault seccomp.
func TestPrewarm_RestrictedSecurityContext(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/agent-image-prewarm.yaml",
		"agents.prewarmImage.enabled=true",
	)
	for _, want := range []string{
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"drop:",
		"type: RuntimeDefault",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prewarm pod missing restricted-PSA field %q; got:\n%s", want, out)
		}
	}
}

// TestPrewarm_ProdOverlayEnablesBothDaemonSets (prod-wiring guard against
// ../../terraform/composition/canonical-prod/image-prewarm-values.yaml) was
// deleted — canonical-prod is out of scope for this repo's test suite. See
// gardenio/helm/gardenio/tests/README.md.
