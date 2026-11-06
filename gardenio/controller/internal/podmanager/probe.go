package podmanager

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Probe-pod constants (probe-pods plan ka-0000000544). A probe pod is a one-off
// scripted pre-check: a bare `/bin/sh -lc $SCRIPT` container that runs to
// completion and reports done/failed via its exit code, with NONE of the coop
// agent lifecycle. It is the cheap "did anything change?" primitive that
// replaces a full no-op gardener inference — the whole point is to delete the 2
// CPU / 8Gi coop pod cost, so a probe pod MUST NOT reuse the agent container
// shape (coop entrypoint + three probed ports); it only reuses the agent IMAGE
// so the script has git/jq/kd/gb/glab on PATH.
const (
	// LabelProbe marks a pod as a one-off probe pod. It is deliberately NOT the
	// gardenio.io/agent label, so the reconciler's desired-vs-actual agent sweep
	// (which keys on LabelAgent) leaves probe pods alone — `gb probe run` owns a
	// probe pod's lifecycle end-to-end, exactly as the poolmanager owns warm
	// placeholders (which carry LabelWarmPlaceholder, not LabelAgent).
	LabelProbe = "gardenio.io/probe"

	// ProbeContainerName is the single container in a probe pod. Named "probe"
	// (not "agent") to make it unmistakable in kubectl/logs that this is not a
	// coop agent pod.
	ProbeContainerName = "probe"

	// ProbeWorkPath is the scratch emptyDir mount the script gets a writable
	// working tree at. A probe runs as the non-root agent UID, whose $HOME may
	// be read-only in the image, so give the script somewhere to write.
	ProbeWorkPath = "/work"

	// defaultProbeActiveDeadlineSeconds bounds how long a probe pod may run
	// before the kubelet kills it (→ PodFailed → bead failed). A probe is a
	// fast pre-check (a few git ls-remote + bead reads), so the ceiling is
	// short. Callers may override per-bead via activity_deadline_seconds.
	defaultProbeActiveDeadlineSeconds int64 = 120
)

// ProbePodSpec describes a single one-off probe pod. It is intentionally a
// small, self-contained value (not the full AgentPodSpec) — a probe pod has no
// account, no OAuth, no coop, no sidecars — so the CLI can build one without the
// agent-pod machinery, mirroring WarmPlaceholderSpec.
type ProbePodSpec struct {
	// Name is the pod name. Must be unique within Namespace; the runner
	// generates a stable, bead-scoped name.
	Name string

	// Namespace is the K8s namespace the probe pod runs in (the agent
	// namespace).
	Namespace string

	// Image is the container image. Reuse the fleet agent image so the script
	// has the standard toolchain (git/jq/kd/gb/glab) on PATH. The image's
	// entrypoint is overridden — the probe never launches coop.
	Image string

	// Script is the shell body executed as `/bin/sh -lc "$Script"`. Its exit
	// code becomes the pod's exit code, which the runner maps to done (0) /
	// failed (non-zero).
	Script string

	// BeadID is the type=script bead this probe realizes. Stamped as the
	// gardenio.io/bead-id annotation for traceability.
	BeadID string

	// Project is the owning project, stamped as gardenio.io/project.
	Project string

	// ActiveDeadlineSeconds bounds the pod's total runtime. Zero uses
	// defaultProbeActiveDeadlineSeconds.
	ActiveDeadlineSeconds int64

	// ImagePullSecrets names the K8s Secrets used to pull the (private) agent
	// image. A probe reuses the fleet agent image, which lives in a private
	// registry, so without the pull secret the probe pod ImagePullBackOffs
	// (403) and never runs — exactly the pull secret the controller stamps on
	// every agent pod (cfg.ImagePullSecrets, IMAGE_PULL_SECRETS). Empty is
	// valid for a public image / a node with the layers pre-cached.
	ImagePullSecrets []string

	// Resources optionally overrides the compute request/limit. Nil uses a
	// slim built-in default (50m / 128Mi) — vs the 500m / 2Gi an agent pod
	// requests by default, which is the cost this whole primitive exists to avoid.
	Resources *corev1.ResourceRequirements
}

// probeCommand is the fixed entrypoint every probe pod runs. The script body is
// passed as the single argument to `/bin/sh -lc`, so a non-zero `exit N`
// anywhere in the script (or an uncaught command failure under a shell that
// `set -e`s) surfaces as the container's terminated exit code.
var probeCommand = []string{"/bin/sh", "-lc"}

// defaultProbeResources is the request/limit a probe pod reserves. Kept slim on
// purpose (50m / 128Mi): a probe is a short scripted pre-check, and the whole
// point of the primitive is to NOT pay the agent pod's 500m / 2Gi envelope for
// a "nothing to do" conclusion.
func defaultProbeResources() *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

// BuildProbePod mints a one-off probe pod from spec. Like BuildWarmPlaceholderPod
// it is a standalone builder (not a K8sManager method) so `gb probe run` can
// construct a probe pod without an agent bead or the AgentPodSpec machinery.
//
// The pod is deliberately minimal and coop-free:
//   - Command is `/bin/sh -lc "$Script"` — the image entrypoint (which would
//     launch coop) is overridden, so none of the 3 coop ports are ever served.
//   - NO readiness/liveness/startup probes — a plain script never serves those
//     ports, and a probe on a never-listening port would fail the pod (the exact
//     trap the bead's POD SHAPE note calls out).
//   - NO Claude OAuth / ANTHROPIC_* env — a probe runs a script, not Claude.
//   - RestartPolicy Never + activeDeadlineSeconds — a probe runs once to a
//     terminal phase; PodSucceeded (exit 0) / PodFailed (non-zero, OR the
//     deadline) is the whole signal.
//   - Slim resources (50m / 128Mi) — the cost being reclaimed.
//   - PSA-restricted SecurityContext identical to every gardenio agent pod, so it
//     is admitted in the restricted agent namespace.
//   - ImagePullSecrets (when set) — the probe reuses the PRIVATE fleet agent
//     image, so it needs the same registry pull secret the controller stamps on
//     every agent pod, or the pod ImagePullBackOffs (403) and never runs.
//   - LabelProbe (not LabelAgent), so the reconciler's agent sweep ignores it.
func BuildProbePod(spec ProbePodSpec) *corev1.Pod {
	never := corev1.RestartPolicyNever
	automount := false

	res := spec.Resources
	if res == nil {
		res = defaultProbeResources()
	}

	deadline := spec.ActiveDeadlineSeconds
	if deadline <= 0 {
		deadline = defaultProbeActiveDeadlineSeconds
	}

	container := corev1.Container{
		Name:       ProbeContainerName,
		Image:      spec.Image,
		Command:    probeCommand,
		Args:       []string{spec.Script},
		WorkingDir: ProbeWorkPath,
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeWork, MountPath: ProbeWorkPath},
		},
		Resources: *res,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                intPtr(AgentUID),
			RunAsGroup:               intPtr(AgentGID),
			RunAsNonRoot:             boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}

	var pullSecrets []corev1.LocalObjectReference
	for _, name := range spec.ImagePullSecrets {
		if name == "" {
			continue
		}
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:                never,
		ActiveDeadlineSeconds:        intPtr(deadline),
		AutomountServiceAccountToken: &automount,
		ImagePullSecrets:             pullSecrets,
		Containers:                   []corev1.Container{container},
		Volumes: []corev1.Volume{
			{
				Name:         VolumeWork,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		},
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:    intPtr(AgentUID),
			RunAsGroup:   intPtr(AgentGID),
			RunAsNonRoot: boolPtr(true),
			FSGroup:      intPtr(AgentGID),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}

	labels := map[string]string{
		LabelApp: LabelAppValue,
		// gardenio.io/app=gardenio (LabelAppValue) — the SAME value every agent pod
		// stamps (manager.go buildPod). The chart's agent-egress NetworkPolicies
		// peer on this exact value (e.g. the litellm-inference egress rule), so a
		// probe that needs DNS / beads / git egress must carry it verbatim; the
		// literal "true" here would land the pod outside the policy's peer set.
		LabelGardenioApp: LabelAppValue,
		LabelProbe:       "true",
	}
	if spec.Project != "" {
		labels[LabelProject] = truncateLabel(spec.Project)
	}

	annotations := map[string]string{}
	if spec.BeadID != "" {
		annotations[AnnotationBeadID] = spec.BeadID
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Name,
			Namespace:   spec.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: podSpec,
	}
}
