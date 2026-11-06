package podmanager

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WarmPlaceholderSpec describes a single warm runsc placeholder pod the
// poolmanager pre-creates so a later gVisor thaw can graft its checkpoint onto
// an already-Ready pod instead of paying the kubelet pod-admission cost on the
// hot path (sub-second-resume-integration.md C3, ka-0000004263).
//
// The shape mirrors FreezeThawManager.buildPlaceholderPodB: runsc RuntimeClass,
// a sleeping shell so the body never races the restore, a /work emptyDir whose
// mount destination lines up with the restored container's, pinned to a specific
// gVisor node (a snapshot's checkpoint image is node-local), and the same
// PSA-restricted SecurityContext every gardenio agent pod runs under. Unlike a
// real agent pod it carries NO gardenio.io/agent label, so the reconciler's
// orphan sweep leaves it alone — the poolmanager owns its lifecycle.
type WarmPlaceholderSpec struct {
	// Name is the pod name. Must be unique within Namespace; the poolmanager
	// generates a stable, node-scoped name.
	Name string

	// Namespace is the PSA-privileged gVisor namespace the runsc agents and
	// probe pods live in (cfg.GVisorNamespace).
	Namespace string

	// Node pins the placeholder to a specific gVisor node so it can serve a
	// thaw whose checkpoint image is resident on that node.
	Node string

	// Image is the agent container image. A placeholder only ever runs the
	// sleep shell, but it must be a real pullable image so the pod reaches
	// Ready; reuse the fleet agent image.
	Image string

	// Resources optionally overrides the placeholder's compute request/limit.
	// A placeholder holds node capacity while idle, so callers should keep this
	// modest. Nil uses a small built-in default.
	Resources *corev1.ResourceRequirements

	// PriorityClassName, when non-empty, is stamped on the placeholder so a cold
	// real agent can preempt an idle placeholder when nodes are tight — a
	// placeholder holds node capacity while idle, same as a parked prewarmed pod
	// (ka-0000000722). Wired from the pool's PrewarmedPriorityClass, which the
	// chart sets only when it renders the negative-priority class. Empty leaves
	// the placeholder at default priority (legacy behavior).
	PriorityClassName string
}

// warmPlaceholderCommand is the passive sleep shell every warm placeholder
// runs. It mirrors buildPlaceholderPodB: ensure /work/repo exists (the restore
// repoints into /work) then sleep, so the container holds the kubelet-owned
// bindings without racing a future graft.
var warmPlaceholderCommand = []string{"/bin/sh", "-c", "mkdir -p /work/repo 2>/dev/null; exec sleep 86400"}

// warmPlaceholderNominalPagesBytes is the nominal pages.img size the LEGACY bare
// warm-placeholder builder (BuildWarmPlaceholderPod) sizes its memory LIMIT off
// (ka-0000003398). Unlike the snapshot-aware builder, the bare builder has no
// Snapshot in hand — it can only serve a thaw that mounts just /work — so it uses
// a fleet-representative ~3.0 GiB checkpoint to size the restore-peak ceiling.
const warmPlaceholderNominalPagesBytes int64 = 3 * 1024 * 1024 * 1024

// defaultWarmPlaceholderResources is the request/limit a bare warm placeholder
// reserves while idle. The REQUEST is kept small on purpose (100m / 256Mi): warm
// placeholders hold node capacity via their request, so the pool MinSize times
// the request is the standing cost (the plan calls for 1–2 per node).
//
// The LIMIT, however, must cover the runsc restore PEAK (ka-0000003398 / bug
// ka-0000000413): the eager pages.img reconstruction runs inside this pod's
// CONTAINER-scope cgroup, and canary #15 proved a 512Mi limit cgroup-OOM-SIGKILLs
// a multi-GiB restore against memory.max long before node RAM is exhausted. So
// the limit is sized to modeledRestorePeakLimit at the nominal ~3.0 GiB
// checkpoint — the SAME model the snapshot-aware placeholders use — while the
// request stays at the 256Mi floor (limits do not reserve node capacity, so a fat
// ceiling costs nothing standing; the gVisor namespace carries no LimitRange, so
// there is no admission cap to trip). The restored agent's real resource envelope
// still comes from the checkpoint graft, not this spec.
func defaultWarmPlaceholderResources() *corev1.ResourceRequirements {
	limit := modeledRestorePeakLimit(warmPlaceholderNominalPagesBytes, freezeBaselineContainerCount)
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(limit, resource.BinarySI),
		},
	}
}

// BuildWarmPlaceholderPod mints a Ready-able runsc placeholder pod for the
// warm-B pool. It is deliberately a standalone builder (not a method on
// K8sManager) so the poolmanager can construct placeholders without an agent
// bead or the full AgentPodSpec machinery — a warm placeholder is a bare,
// node-pinned sleep pod, not a configured agent.
func BuildWarmPlaceholderPod(spec WarmPlaceholderSpec) *corev1.Pod {
	rc := RuntimeClassRunsc
	never := corev1.RestartPolicyNever
	automount := false

	res := spec.Resources
	if res == nil {
		res = defaultWarmPlaceholderResources()
	}

	container := corev1.Container{
		Name:    "agent",
		Image:   spec.Image,
		Command: warmPlaceholderCommand,
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeWork, MountPath: WarmPlaceholderWorkPath},
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

	podSpec := corev1.PodSpec{
		NodeName:                     spec.Node,
		RuntimeClassName:             &rc,
		PriorityClassName:            spec.PriorityClassName,
		RestartPolicy:                never,
		AutomountServiceAccountToken: &automount,
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

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				LabelApp:             LabelAppValue,
				LabelWarmPlaceholder: WarmPlaceholderReady,
			},
			// The pool stamps AnnotationWarmImage with its configured image after
			// building (PlaceholderPool.createPlaceholder, ka-0000000890) so the
			// upgrade-staleness reaper can cheaply detect a placeholder minted by
			// an OLD controller. Stamping at the pool layer keeps the signal
			// uniform across this legacy builder and the snapshot builder (whose
			// container image comes from the template, not the pool's image).
		},
		Spec: podSpec,
	}
}

// BuildWarmPlaceholderFromSnapshot mints a topology- and repo-aware warm
// placeholder from an existing snapshot's preserved Pod A template
// (ka-0000002219). Unlike BuildWarmPlaceholderPod — which builds a generic bare
// /work emptyDir pod that can only serve a thaw whose checkpoint happens to mount
// just /work — this builder reuses shapePlaceholderSpec (the SAME spec-shaper the
// create-path buildPlaceholderPodB uses), so the placeholder's mount-destination
// set, kept init-clone, and always-on sidecars match the snapshot exactly:
//
//   - MOUNT SHAPE: the template's full mount set (workspace at /home/agent/bot,
//     projected SA token, /work, resolv.conf/hosts) is carried verbatim, so
//     runsc RestoreValidateSpec's destination match succeeds (gap 1).
//   - REPO: the preserved init-clone container runs at pod-fill time and
//     re-hydrates the workspace with the project repo BEFORE the graft, so a
//     grafted agent wakes into a populated working tree, not an empty one (gap 2,
//     mirroring C6 strategy b on the create-path).
//   - SIDECARS: always-on DinD/Postgres sidecars are kept as sleeping shells so
//     the kubelet provisions their container slots for the N-container barrier
//     restore (ka-0000003561).
//
// The pod is stamped with LabelWarmTopology=<SnapshotTopologyKey> so the wake
// path only ever claims it for an agent whose snapshot is graft-compatible. The
// node is taken from the snapshot (a path-b thaw is node-local); callers that
// pre-warm for a node a snapshot has not yet landed on should pass the node
// explicitly via the override. Name and Image come from the spec; Resources is
// ignored here (the placeholder inherits the snapshot template's envelope so the
// kubelet admits the same shape the checkpoint expects).
func BuildWarmPlaceholderFromSnapshot(name string, snap Snapshot, node string) *corev1.Pod {
	if node == "" {
		node = snap.Node
	}
	spec := shapePlaceholderSpec(snap.PodTemplate, node, snap.PagesBytes, snap.ContainerCount)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: snap.Namespace,
			Labels: map[string]string{
				LabelApp:             LabelAppValue,
				LabelWarmPlaceholder: WarmPlaceholderReady,
				LabelWarmTopology:    SnapshotTopologyKey(snap),
			},
			// The pool stamps AnnotationWarmImage with its configured image after
			// building (PlaceholderPool.createPlaceholder, ka-0000000890): the
			// staleness signal is "minted by the current controller", and the
			// snapshot path's container image comes from the template (not the
			// pool's configured image), so the stamp is applied at the pool layer
			// uniformly rather than derived here.
		},
		Spec: spec,
	}
}

// WarmPlaceholderPromotion carries the canonical agent identity stamped onto a
// warm placeholder when the wake path claims it for a thaw graft (C2,
// ka-0000005266).
type WarmPlaceholderPromotion struct {
	// AgentName is the agent the placeholder is being grafted for; it becomes
	// the pod's gardenio.io/agent label so the reconciler's desired-vs-actual
	// sweep treats the pod as that agent's live pod.
	AgentName string

	// BeadID is the agent bead id; it becomes the gardenio.io/bead-id annotation
	// so the status reporter maps the pod back to its bead.
	BeadID string

	// CanonicalPod is the canonical agent pod name (mode-project-role-agent)
	// the reconciler keys desired state by. A warm placeholder keeps its pool
	// name ("warm-b-<node>-…"), which the -thaw-b suffix-trim cannot recover, so
	// it is recorded in gardenio.io/canonical-pod for the reconciler remap.
	CanonicalPod string
}

// ApplyWarmPlaceholderPromotion mutates a claimed warm placeholder's metadata
// in place so it becomes a live thawed agent pod, matching the label/annotation
// shape buildPlaceholderPodB produces for the create-path Pod B. It is the
// single source of truth for "what a grafted warm-B pod must look like to the
// reconciler" (C2, ka-0000005266):
//
//   - DROP gardenio.io/warm-placeholder entirely. While labeled (even "claimed")
//     the C3 pool reaper deletes the pod on its next Reconcile — including
//     mid-thaw, which would tear down the kubelet sandbox the restore grafts
//     onto (the ka-0000007549 failure class). Removing the label hands lifecycle
//     ownership from the pool to the reconciler in one atomic step.
//   - ADD gardenio.io/agent + gardenio.io/thaw-placeholder=true so the reconciler
//     recognizes the pod as a live thawed agent (it exempts thaw placeholders
//     from drift recreation).
//   - ADD gardenio.io/bead-id and gardenio.io/canonical-pod so the status reporter
//     and the reconciler's desired-vs-actual remap resolve the pod's identity
//     despite its non-canonical pool name.
func ApplyWarmPlaceholderPromotion(pod *corev1.Pod, promo WarmPlaceholderPromotion) {
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	delete(pod.Labels, LabelWarmPlaceholder)
	pod.Labels[LabelAgent] = truncateLabel(promo.AgentName)
	pod.Labels[LabelThawPlaceholder] = "true"

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	if promo.BeadID != "" {
		pod.Annotations[AnnotationBeadID] = promo.BeadID
	}
	if promo.CanonicalPod != "" {
		pod.Annotations[AnnotationCanonicalPod] = promo.CanonicalPod
	}
	// Every grafted agent must survive Karpenter node consolidation, mirroring
	// the create-path buildPod stamp (investigation ka-0000006340). Applied here
	// at graft time since the bare warm placeholder didn't carry it.
	pod.Annotations[AnnotationKarpenterDoNotDisrupt] = "true"
}
