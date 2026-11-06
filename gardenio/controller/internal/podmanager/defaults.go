package podmanager

import (
	"strings"

	"gardenio/controller/internal/lands/landns"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodDefaults holds default pod template values that can be overridden
// at each level of the merge hierarchy:
//
//	Gardenio defaults < Project overrides < Mode overrides < AgentPool template
type PodDefaults struct {
	Image              string
	Resources          *corev1.ResourceRequirements
	ServiceAccountName string
	NodeSelector       map[string]string
	Tolerations        []corev1.Toleration
	Affinity           *corev1.Affinity
	Env                map[string]string
	SecretEnv          []SecretEnvSource
	ConfigMapName      string
	WorkspaceStorage   *WorkspaceStorageSpec
	ImagePullSecrets   []string
}

// ApplyDefaults applies PodDefaults to an AgentPodSpec, filling in
// any fields that aren't already set on the spec.
func ApplyDefaults(spec *AgentPodSpec, defaults *PodDefaults) {
	if defaults == nil {
		return
	}

	if spec.Image == "" && defaults.Image != "" {
		spec.Image = defaults.Image
	}
	if spec.Resources == nil && defaults.Resources != nil {
		spec.Resources = defaults.Resources
	}
	if spec.ServiceAccountName == "" && defaults.ServiceAccountName != "" {
		spec.ServiceAccountName = defaults.ServiceAccountName
	}
	if len(spec.NodeSelector) == 0 && len(defaults.NodeSelector) > 0 {
		spec.NodeSelector = defaults.NodeSelector
	}
	if len(spec.Tolerations) == 0 && len(defaults.Tolerations) > 0 {
		spec.Tolerations = defaults.Tolerations
	}
	if spec.Affinity == nil && defaults.Affinity != nil {
		spec.Affinity = defaults.Affinity
	}
	if spec.ConfigMapName == "" && defaults.ConfigMapName != "" {
		spec.ConfigMapName = defaults.ConfigMapName
	}
	if spec.WorkspaceStorage == nil && defaults.WorkspaceStorage != nil {
		spec.WorkspaceStorage = defaults.WorkspaceStorage
	}
	if len(spec.ImagePullSecrets) == 0 && len(defaults.ImagePullSecrets) > 0 {
		spec.ImagePullSecrets = defaults.ImagePullSecrets
	}
	// Merge env maps (spec values take precedence over defaults).
	if len(defaults.Env) > 0 {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		for k, v := range defaults.Env {
			if _, exists := spec.Env[k]; !exists {
				spec.Env[k] = v
			}
		}
	}

	// Append default secret env sources that aren't already in the spec.
	if len(defaults.SecretEnv) > 0 {
		existing := make(map[string]bool)
		for _, se := range spec.SecretEnv {
			existing[se.EnvName] = true
		}
		for _, se := range defaults.SecretEnv {
			if !existing[se.EnvName] {
				spec.SecretEnv = append(spec.SecretEnv, se)
			}
		}
	}
}

// roleRequiresOnDemand reports whether any role in a (possibly
// comma-separated) role string hosts a live human conversation: thread or dm.
// Only these pods require on-demand capacity (see capacityAffinity; on k3s
// it is only preferred): a Spot reclaim force-deletes every agent on the node
// with ~2min notice, defeating karpenter.sh/do-not-disrupt (which only blocks
// Karpenter-initiated disruption, not AWS reclaim), and that would cut a
// human off mid-reply.
//
// Gardeners are deliberately NOT here (gd-Fyz3w0tOnf): nobody is waiting on
// them turn by turn, and they resume from S3 after an interruption like crew
// do. Pinning them on-demand left at most one gardener running on the small
// on-demand pool while the rest sat Pending on Insufficient cpu
// (gd-7GdfTU7oPa). They take the batch affinity (Spot preferred, on-demand
// allowed) and keep their preemption priority via roleHasInteractivePriority.
//
// Unlike modeForRole/roleIsLongLived (which key on the PRIMARY role only), this
// matches if ANY of the comma-separated tokens is thread/dm. The goal is
// protective: a "crew,thread" or "thread,crew" agent carries a human-waiting
// component either way and must not die to Spot.
func roleRequiresOnDemand(role string) bool {
	for _, tok := range strings.Split(role, ",") {
		switch strings.TrimSpace(tok) {
		case "thread", "dm":
			return true
		}
	}
	return false
}

// roleHasInteractivePriority reports whether an agent pod gets the
// InteractivePriorityClass, which lets it preempt batch crew pods when nodes
// are full (gd-Vtnt5lAGeQ): the thread/dm roles of roleRequiresOnDemand plus
// any gardener flavor (gardener / plan-gardener — any *gardener suffix), the
// long-lived orchestrators the rest of the fleet waits on. Like
// roleRequiresOnDemand, ANY matching comma-separated token counts.
func roleHasInteractivePriority(role string) bool {
	if roleRequiresOnDemand(role) {
		return true
	}
	for _, tok := range strings.Split(role, ",") {
		if strings.HasSuffix(strings.TrimSpace(tok), "gardener") {
			return true
		}
	}
	return false
}

// scheduledGardenerRun reports whether an agent pod is a gardener that a
// schedule or fire-now started and no operator has since woken, which gets
// the non-preempting ScheduledPriorityClass instead of the interactive one
// (gd-Y2KaNE233g). Such a run is a watchdog that can wait for room; killing a
// working crew to place it loses the crew's run. A thread/dm token, or a wake
// by `gb wake`, the viewer's Wake or a mention, means a person is waiting, so
// the pod keeps the preempting class.
func scheduledGardenerRun(spec AgentPodSpec) bool {
	if roleRequiresOnDemand(spec.Role) || !roleHasInteractivePriority(spec.Role) {
		return false
	}
	switch spec.WakeReason {
	case "manual", "mention": // beadsapi.WakeReasonManual, WakeReasonMention
		return false
	}
	switch spec.SpawnSource {
	case "schedule", "fire-now":
		return true
	}
	return false
}

// lifecycleLabel is the k3s node label carrying the IMDS instance-life-cycle
// ("spot" / "on-demand"), set at node registration by node.sh.tftpl.
const lifecycleLabel = "gardenio.io/lifecycle"

// capacityAffinity builds the node affinity that steers a pod onto the right
// Karpenter capacity type (ka-0000002487, role-split). The single agent-pool
// NodePool offers BOTH on-demand and Spot capacity; this affinity is what
// splits the fleet by role:
//
//   - Interactive roles (thread/dm, roleRequiresOnDemand): capacity-type=on-demand
//     is a HARD requirement, so a human-waiting agent can never be scheduled
//     onto — nor provisioned onto — a Spot node that AWS can reclaim
//     mid-conversation. The one exception is the k3s Spot pool (below).
//   - Batch roles (crew/job/gardener): capacity-type=spot is only PREFERRED, so
//     these pods bias toward cheap Spot capacity but remain schedulable
//     on-demand when Spot is unavailable (except on the k3s on-demand
//     platform nodes, below). Karpenter provisions the cheapest matching
//     capacity (Spot) for the unconstrained requirement.
//
// Both classes keep the hard per-mr exclusion (agent pods never land on the
// ephemeral per-MR nodes).
//
// Batch roles also carry a HARD lifecycleLabel NotIn [on-demand]
// (gd-sDztRMaAag). On the k3s cluster the on-demand nodes are the fixed
// platform pair that runs postgres, kbeads, MinIO, slack-bridge and grafana,
// and the preferred Spot term above is only a score: when the Spot nodes are
// full the scheduler binds a crew to the pair at once rather than wait for
// the cluster-autoscaler to add a Spot node. On 2026-10-25 two crews did that
// to ip-10-90-2-170 (2 vCPU), the node went NotReady for two minutes and
// slack-bridge, viewer-authproxy and grafana-bridge restarted. With the
// requirement a Pending batch pod waits for the autoscaler's Spot node
// instead. Only the k3s node bootstrap sets gardenio.io/lifecycle
// (terraform/modules/k3s/templates/node.sh.tftpl); NotIn matches a node that
// lacks the label, so Karpenter, gVisor and local clusters are unchanged.
//
// Interactive roles on k3s (gd-4nac3mPMgo): the same platform pair is the
// only on-demand capacity there, and once postgres, kbeads, MinIO, grafana and
// the bridge have their requests it holds ONE 500m thread agent. With the
// hard on-demand pin every second mention sat Pending until the first thread
// agent exited (4-50 min on 2026-10-25), and the cluster-autoscaler never
// added a node for it ("predicate NodeAffinity did not pass" on the Spot
// group). So the required affinity for thread/dm is two ORed terms: on-demand
// capacity anywhere, OR a node with lifecycleLabel=spot (only the k3s Spot
// pool carries it, and its autoscaler node-template does too, so scale-up
// works). On-demand stays PREFERRED, so a thread agent uses the pair when it
// has room and otherwise a Spot node, where a reclaim is covered by the
// 2-minute notice and the S3 session resume crews already rely on. On
// Karpenter no node carries lifecycleLabel, so the second term matches
// nothing and the pin is unchanged there.
//
// There is deliberately NO compute-family (c5/c6i/…) preference here: it was
// vestigial from when the agent pool was c-family only (crew-pod-resource-trim,
// April 2026). The agent NodePool is m5-only today, so the old weight-80
// preferred nodeAffinity for c5/c5a/c6i/c6a/c7i matched nothing on the agent
// pool and, on the gVisor pool (which does offer c6a), actively biased
// freeze-eligible agents toward the small c6a nodes the fat-node steer
// (preferFatNodeForFreeze) then had to fight. Removed to reconcile the pod spec
// with the NodePool (ka-0000006729).
func capacityAffinity(role string) *corev1.Affinity {
	var preferred []corev1.PreferredSchedulingTerm

	// Hard exclusion of the ephemeral per-MR nodes — shared by both classes.
	required := &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key:      "per-mr",
				Operator: corev1.NodeSelectorOpDoesNotExist,
			}},
		}},
	}

	if roleRequiresOnDemand(role) {
		// Interactive: REQUIRE on-demand capacity OR a k3s Spot node (terms
		// are ORed; each keeps the per-mr exclusion), and PREFER on-demand.
		perMR := required.NodeSelectorTerms[0].MatchExpressions[0]
		required.NodeSelectorTerms = []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{perMR, {
				Key:      "karpenter.sh/capacity-type",
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"on-demand"},
			}}},
			{MatchExpressions: []corev1.NodeSelectorRequirement{perMR, {
				Key:      lifecycleLabel,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"spot"},
			}}},
		}
		preferred = append(preferred, corev1.PreferredSchedulingTerm{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "karpenter.sh/capacity-type",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"on-demand"},
				}},
			},
		})
	} else {
		// Batch: never the k3s on-demand platform nodes (see
		// lifecycleLabel), wherever that label exists.
		for i := range required.NodeSelectorTerms {
			required.NodeSelectorTerms[i].MatchExpressions = append(
				required.NodeSelectorTerms[i].MatchExpressions,
				corev1.NodeSelectorRequirement{
					Key:      lifecycleLabel,
					Operator: corev1.NodeSelectorOpNotIn,
					Values:   []string{"on-demand"},
				},
			)
		}
		// Batch: PREFER Spot capacity (cheap), fall back to on-demand.
		preferred = append([]corev1.PreferredSchedulingTerm{{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "karpenter.sh/capacity-type",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"spot"},
				}},
			},
		}}, preferred...)
	}

	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: preferred,
			RequiredDuringSchedulingIgnoredDuringExecution:  required,
		},
	}
}

// LabelControlPlane is the label k3s and kubeadm put on control-plane nodes.
const LabelControlPlane = "node-role.kubernetes.io/control-plane"

// LandTenantAffinity is the whole affinity of a tenant land's pod
// (gd-NDJIOutLsv; Lands Track B spec §5.2). Tenant pods run on runc
// (gd-iyZom5u11A), so they share a kernel with whatever runs beside them, and
// this keeps that from being the fleet:
//
//   - Node affinity, REQUIRED: never a per-MR node, never a node labelled
//     lifecycleLabel=on-demand (the k3s platform pair: postgres, kbeads, the
//     bridge), never a control-plane node. Spot is preferred. Unlike
//     capacityAffinity, the role has no say: a tenant thread or dm agent gets
//     no on-demand term.
//   - Pod anti-affinity, REQUIRED, per node: no pod labelled LabelAgent in a
//     namespace without the land label (every fleet agent, the gVisor
//     namespace's included). The scheduler also applies a required
//     anti-affinity term of a running pod to every pod that arrives, so a
//     fleet agent is not placed on a node that holds a tenant pod either.
func LandTenantAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "per-mr", Operator: corev1.NodeSelectorOpDoesNotExist},
						{Key: lifecycleLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"on-demand"}},
						{Key: LabelControlPlane, Operator: corev1.NodeSelectorOpDoesNotExist},
					},
				}},
			},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
				Weight: 100,
				Preference: corev1.NodeSelectorTerm{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "karpenter.sh/capacity-type",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"spot"},
					}},
				},
			}},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: corev1.LabelHostname,
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      LabelAgent,
						Operator: metav1.LabelSelectorOpExists,
					}},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      landns.LabelLand,
						Operator: metav1.LabelSelectorOpDoesNotExist,
					}},
				},
			}},
		},
	}
}

// DefaultPodDefaults returns sensible defaults for a given mode and role.
//
// The role determines capacity-type affinity (ka-0000002487): interactive
// roles (thread/dm) hard-require on-demand nodes so they never die to a Spot
// reclaim (on k3s they only prefer them), while batch roles
// (crew/job/gardener) prefer cheap Spot. See capacityAffinity for the split.
func DefaultPodDefaults(mode, role string) *PodDefaults {
	defaults := &PodDefaults{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(DefaultCPURequest),
				corev1.ResourceMemory: resource.MustParse(DefaultMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(DefaultCPULimit),
				corev1.ResourceMemory: resource.MustParse(DefaultMemoryLimit),
			},
		},
		// Agent image is x86-only; pin to amd64 nodes.
		NodeSelector: map[string]string{
			"kubernetes.io/arch": "amd64",
		},
		// Tolerate the dedicated agent-pool taint so spawned agent pods can
		// land on the Karpenter agent NodePool, which taints its nodes with
		// gardenio.agent=true:NoSchedule to keep non-agent workloads off.
		Tolerations: []corev1.Toleration{{
			Key:      "gardenio.agent",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		// Capacity-type affinity split by role; exclude per-MR nodes.
		Affinity: capacityAffinity(role),
	}

	switch mode {
	case "crew":
		// Crew pods use EmptyDir — S3 handles session persistence across
		// pod restarts. Git work must be pushed before gb done.
		// Previously crew pods used PVCs but emptyDir simplifies scheduling,
		// avoids PVC lifecycle management, and reduces costs.
	case "job":
		// Jobs use EmptyDir (no WorkspaceStorage).
	}

	return defaults
}
