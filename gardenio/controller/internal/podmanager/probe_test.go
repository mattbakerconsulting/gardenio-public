package podmanager

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

// TestBuildProbePod_Shape asserts the invariants the probe-pods plan
// (ka-0000000544) requires of a probe pod: slim resources, RestartPolicy Never,
// a bounded activeDeadlineSeconds, NO coop probes, NO Claude OAuth / ANTHROPIC
// env, and the `/bin/sh -lc "$script"` entrypoint.
func TestBuildProbePod_Shape(t *testing.T) {
	pod := BuildProbePod(ProbePodSpec{
		Name:      "probe-gd-test",
		Namespace: "gardenio",
		Image:     "registry.example/agent:latest",
		Script:    "echo hi && exit 0",
		BeadID:    "gd-test",
		Project:   "gardenio",
	})

	if got := pod.Spec.RestartPolicy; string(got) != "Never" {
		t.Errorf("RestartPolicy = %q, want Never", got)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("ActiveDeadlineSeconds is nil; a probe pod must be bounded")
	}
	if *pod.Spec.ActiveDeadlineSeconds != defaultProbeActiveDeadlineSeconds {
		t.Errorf("ActiveDeadlineSeconds = %d, want default %d", *pod.Spec.ActiveDeadlineSeconds, defaultProbeActiveDeadlineSeconds)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken must be explicitly false on a probe pod")
	}

	if n := len(pod.Spec.Containers); n != 1 {
		t.Fatalf("probe pod has %d containers, want exactly 1", n)
	}
	c := pod.Spec.Containers[0]
	if c.Name != ProbeContainerName {
		t.Errorf("container name = %q, want %q", c.Name, ProbeContainerName)
	}

	// Entrypoint: /bin/sh -lc, with the script as the single arg.
	wantCmd := []string{"/bin/sh", "-lc"}
	if len(c.Command) != len(wantCmd) || c.Command[0] != wantCmd[0] || c.Command[1] != wantCmd[1] {
		t.Errorf("Command = %v, want %v", c.Command, wantCmd)
	}
	if len(c.Args) != 1 || c.Args[0] != "echo hi && exit 0" {
		t.Errorf("Args = %v, want the script body as a single arg", c.Args)
	}

	// NO coop health/readiness/startup probes — a plain script never serves the
	// coop ports, so any probe would fail the pod.
	if c.ReadinessProbe != nil || c.LivenessProbe != nil || c.StartupProbe != nil {
		t.Error("probe pod container must have NO readiness/liveness/startup probes")
	}

	// NO Claude OAuth / ANTHROPIC_* env on the pod (the bead's Verify contract).
	for _, e := range c.Env {
		switch {
		case e.Name == "CLAUDE_CODE_OAUTH_TOKEN":
			t.Errorf("probe pod must NOT carry CLAUDE_CODE_OAUTH_TOKEN")
		case len(e.Name) >= 10 && e.Name[:10] == "ANTHROPIC_":
			t.Errorf("probe pod must NOT carry ANTHROPIC_* env (%s)", e.Name)
		}
	}
}

// TestBuildProbePod_ResourceCeiling verifies the memory request/limit is <=128Mi
// — the bead's Verify criterion ("Pod requests <=128Mi") and the whole point of
// the primitive (not paying the agent's 2 CPU / 8Gi envelope for a pre-check).
func TestBuildProbePod_ResourceCeiling(t *testing.T) {
	pod := BuildProbePod(ProbePodSpec{
		Name:      "probe-gd-ceil",
		Namespace: "gardenio",
		Image:     "img",
		Script:    "true",
	})
	c := pod.Spec.Containers[0]

	ceil := resource.MustParse("128Mi")
	reqMem := c.Resources.Requests.Memory()
	if reqMem.Cmp(ceil) > 0 {
		t.Errorf("memory request = %s, want <= 128Mi", reqMem.String())
	}
	limMem := c.Resources.Limits.Memory()
	if limMem.Cmp(ceil) > 0 {
		t.Errorf("memory limit = %s, want <= 128Mi", limMem.String())
	}

	// CPU request kept modest (a probe holds node capacity via its request).
	cpuCeil := resource.MustParse("100m")
	reqCPU := c.Resources.Requests.Cpu()
	if reqCPU.Cmp(cpuCeil) > 0 {
		t.Errorf("cpu request = %s, want <= 100m", reqCPU.String())
	}
}

// TestBuildProbePod_DeadlineOverride verifies a per-bead
// activity_deadline_seconds overrides the default.
func TestBuildProbePod_DeadlineOverride(t *testing.T) {
	pod := BuildProbePod(ProbePodSpec{
		Name:                  "probe-gd-dl",
		Namespace:             "gardenio",
		Image:                 "img",
		Script:                "true",
		ActiveDeadlineSeconds: 45,
	})
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 45 {
		t.Errorf("ActiveDeadlineSeconds = %v, want 45", pod.Spec.ActiveDeadlineSeconds)
	}
}

// TestBuildProbePod_Labels verifies a probe pod carries LabelProbe (so the
// reconciler's agent sweep ignores it) and NOT LabelAgent, plus the project
// label and bead-id annotation for traceability.
func TestBuildProbePod_Labels(t *testing.T) {
	pod := BuildProbePod(ProbePodSpec{
		Name:      "probe-gd-lbl",
		Namespace: "gardenio",
		Image:     "img",
		Script:    "true",
		BeadID:    "gd-lbl",
		Project:   "gardenio",
	})
	if pod.Labels[LabelProbe] != "true" {
		t.Errorf("missing %s=true label", LabelProbe)
	}
	if _, ok := pod.Labels[LabelAgent]; ok {
		t.Errorf("probe pod must NOT carry %s (agent sweep would reap it)", LabelAgent)
	}
	// The agent-egress NetworkPolicies peer on gardenio.io/app=gardenio, the SAME
	// value agent pods stamp — a probe needs DNS/beads/git egress, so it must
	// carry LabelAppValue verbatim (not the literal "true").
	if pod.Labels[LabelGardenioApp] != LabelAppValue {
		t.Errorf("%s = %q, want %q (agent-egress NetworkPolicy peer value)", LabelGardenioApp, pod.Labels[LabelGardenioApp], LabelAppValue)
	}
	if pod.Labels[LabelProject] != "gardenio" {
		t.Errorf("project label = %q, want gardenio", pod.Labels[LabelProject])
	}
	if pod.Annotations[AnnotationBeadID] != "gd-lbl" {
		t.Errorf("bead-id annotation = %q, want gd-lbl", pod.Annotations[AnnotationBeadID])
	}
}

// TestBuildProbePod_ImagePullSecrets verifies the probe pod carries the same
// registry pull secret the controller stamps on agent pods. A probe reuses the
// PRIVATE fleet agent image, so without this the pod ImagePullBackOffs (403) and
// NEVER runs — the whole primitive is dead-on-arrival. Empty entries are dropped
// and an empty spec yields no ImagePullSecrets (a public image / pre-cached node
// needs none).
func TestBuildProbePod_ImagePullSecrets(t *testing.T) {
	pod := BuildProbePod(ProbePodSpec{
		Name:             "probe-gd-pull",
		Namespace:        "gardenio",
		Image:            "registry.example/agent:latest",
		Script:           "true",
		ImagePullSecrets: []string{"registry-pull-secret", "", "second-secret"},
	})
	got := pod.Spec.ImagePullSecrets
	if len(got) != 2 {
		t.Fatalf("ImagePullSecrets = %v, want 2 entries (empty dropped)", got)
	}
	if got[0].Name != "registry-pull-secret" || got[1].Name != "second-secret" {
		t.Errorf("ImagePullSecrets = %v, want [registry-pull-secret second-secret]", got)
	}

	// No pull secret configured → none stamped (public image / pre-cached node).
	bare := BuildProbePod(ProbePodSpec{
		Name: "probe-gd-nopull", Namespace: "gardenio", Image: "img", Script: "true",
	})
	if len(bare.Spec.ImagePullSecrets) != 0 {
		t.Errorf("ImagePullSecrets = %v, want none when unset", bare.Spec.ImagePullSecrets)
	}
}
