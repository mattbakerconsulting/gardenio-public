package tests

import (
	"strings"
	"testing"
)

// Regression guards for ka-0000005233 — the litellm Deployment's update
// strategy must be conditional on replicaCount:
//
//   - single replica (default, or replicaCount=1): type: Recreate.
//     A RollingUpdate maxSurge:1 briefly runs a SECOND litellm pod, needing 2x
//     capacity for the overlap. On the prod cluster every Karpenter nodepool is
//     tainted and the untainted system-node pool has no headroom, so the surge
//     replica sits Pending (FailedScheduling) and the atomic prod terraform
//     apply's helm --wait hangs to its 40m timeout, leaving a pending-upgrade
//     rev that blocks the next release. Recreate needs no extra node.
//   - HA (replicaCount > 1, epic ka-0000005228): RollingUpdate surge is
//     restored (maxSurge:1 / maxUnavailable:0) so a real multi-replica proxy
//     rolls without dropping capacity, backed by the PDB.
//
// Mirrors coopmux's conditional strategy (templates/coopmux/deployment.yaml).
//
// NOTE ON THE DEFAULT (ka-0000007221, operator decision ka-0000007407): the
// chart default is now replicaCount=2, so the DEFAULT render is RollingUpdate.
// These tests therefore pin the CONDITIONAL (1 => Recreate, >1 => surge), which
// is the actual invariant from ka-0000005233 — not "the default is Recreate",
// which was only ever true because the default happened to be 1.

func TestLitellm_SingleReplica_RecreateStrategy(t *testing.T) {
	t.Parallel()
	// replicaCount=1 must be Recreate: a surge pod needs 2x capacity on the
	// untainted infra pool litellm is confined to, and when it cannot schedule
	// the atomic prod helm --wait hangs to its 40m timeout (ka-0000005233).
	out1 := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.replicaCount=1",
	)
	if !strings.Contains(out1, "kind: Deployment") {
		t.Fatalf("expected a litellm Deployment; got:\n%s", out1)
	}
	// Assert on the rendered strategy TYPE, not bare substrings — the
	// explanatory comment in the template legitimately mentions "maxSurge" and
	// "RollingUpdate" as prose, so we match the emitted YAML keys precisely.
	if !strings.Contains(out1, "type: Recreate") {
		t.Errorf("replicaCount=1 litellm must use Recreate (no surge pod); got:\n%s", out1)
	}
	if strings.Contains(out1, "type: RollingUpdate") || strings.Contains(out1, "maxSurge: 1") {
		t.Errorf("replicaCount=1 litellm must NOT surge a second pod; got:\n%s", out1)
	}
	// At one replica the HA scheduling terms are inert and must not render — a
	// DoNotSchedule-style constraint over a single pod is pure hazard, and the
	// single-replica render must stay byte-identical to the pre-HA chart.
	if strings.Contains(out1, "topologySpreadConstraints:") {
		t.Errorf("replicaCount=1 must NOT render topologySpreadConstraints; got:\n%s", out1)
	}
	if strings.Contains(out1, "podAntiAffinity:") {
		t.Errorf("replicaCount=1 must NOT render podAntiAffinity; got:\n%s", out1)
	}
}

// The shipped default must be HA. This is the guard on the actual operator
// decision (ka-0000007407, option `ha-now`): litellm leaks (~190Mi/min,
// ka-0000001241) and OOMKills at ANY limit, so at one replica every kill was a
// TOTAL fleet LLM outage — 159 of them in 24h, ~14% of the day unavailable. Two
// replicas behind the ClusterIP turn that into a blip. If someone drops the
// default below two, that regression must fail here loudly.
//
// As of H5 (epic ka-0000005228) the default also ships the HPA, which OWNS the
// replica count and holds the HA floor at minReplicas>=2 — so the Deployment
// intentionally omits its own `replicas` field (rendering it would reset
// replicas on every `helm upgrade` and fight the autoscaler). HA-by-default is
// therefore now guaranteed by the HPA floor, not a static replicas:2. This test
// pins that guarantee in its new form; the static-replicas path (autoscaling
// off) is covered by TestLitellm_AutoscalingOff_PinsStaticReplicas.
func TestLitellm_DefaultIsHA(t *testing.T) {
	t.Parallel()
	// HA floor: the default HPA must render and never let the proxy drop below 2.
	hpa := helmTemplate(t, "templates/litellm/hpa.yaml", "litellm.enabled=true")
	if !strings.Contains(hpa, "kind: HorizontalPodAutoscaler") {
		t.Fatalf("chart default must ship the litellm HPA (H5); got:\n%s", hpa)
	}
	if !strings.Contains(hpa, "minReplicas: 2") {
		t.Errorf("default litellm HPA must hold the HA floor at minReplicas: 2 — a single replica makes every OOMKill a fleet-wide LLM outage; got:\n%s", hpa)
	}
	// Deployment: never a single replica, and under the active HPA it must not
	// pin `replicas` at all (the HPA owns them).
	dep := helmTemplate(t, "templates/litellm/deployment.yaml", "litellm.enabled=true")
	if strings.Contains(dep, "replicas: 1") {
		t.Errorf("default litellm must never render a single replica; got:\n%s", dep)
	}
	if strings.Contains(dep, "\n  replicas:") {
		t.Errorf("with the HPA active by default the Deployment must OMIT `replicas` so it does not fight the autoscaler; got:\n%s", dep)
	}
	if !strings.Contains(dep, "type: RollingUpdate") {
		t.Errorf("default (HA) litellm must roll, not Recreate; got:\n%s", dep)
	}
}

// When autoscaling is turned OFF (e.g. the single-node in-box overlay, which has
// nowhere to scale out) the Deployment must fall back to pinning the static
// replicaCount so it is not left replica-less. Renders `replicas: 2` (the base
// default replicaCount) and no HPA.
func TestLitellm_AutoscalingOff_PinsStaticReplicas(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.autoscaling.enabled=false",
	)
	if !strings.Contains(dep, "replicas: 2") {
		t.Errorf("autoscaling-off litellm must pin the static replicaCount (2); got:\n%s", dep)
	}
	out1, err := helmTemplateAllowFail(t, "templates/litellm/hpa.yaml",
		"litellm.enabled=true", "litellm.autoscaling.enabled=false")
	if err == nil && strings.Contains(out1, "kind: HorizontalPodAutoscaler") {
		t.Errorf("autoscaling-off litellm must NOT render an HPA; got:\n%s", out1)
	}
}

// Two replicas pinned to one node do not remove the SPOF, so the spread terms
// are load-bearing, not cosmetic. Both must be SOFT: litellm carries no
// tolerations and is confined to the untainted infra nodegroup, so a hard
// (DoNotSchedule / required) term would strand the second replica Pending when
// that pool is tight — reintroducing the ka-0000005233 helm --wait hang.
func TestLitellm_HA_SpreadAcrossNodesAndZones_Soft(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.replicaCount=2",
	)
	if !strings.Contains(out, "topologySpreadConstraints:") {
		t.Fatalf("HA litellm must spread its replicas; got:\n%s", out)
	}
	for _, key := range []string{
		"topologyKey: kubernetes.io/hostname",
		"topologyKey: topology.kubernetes.io/zone",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("HA litellm must spread on %q; got:\n%s", key, out)
		}
	}
	if strings.Contains(out, "whenUnsatisfiable: DoNotSchedule") {
		t.Errorf("spread must be SOFT (ScheduleAnyway) — a hard constraint strands the 2nd replica Pending on the untainted pool and re-creates the helm --wait hang (ka-0000005233); got:\n%s", out)
	}
	if !strings.Contains(out, "whenUnsatisfiable: ScheduleAnyway") {
		t.Errorf("spread must set whenUnsatisfiable: ScheduleAnyway; got:\n%s", out)
	}
	// Soft anti-affinity, and it must be preferred-not-required for the same
	// stranding reason.
	if !strings.Contains(out, "podAntiAffinity:") {
		t.Errorf("HA litellm must set soft podAntiAffinity; got:\n%s", out)
	}
	if !strings.Contains(out, "preferredDuringSchedulingIgnoredDuringExecution:") {
		t.Errorf("podAntiAffinity must be PREFERRED, not required; got:\n%s", out)
	}
	if strings.Contains(out, "requiredDuringSchedulingIgnoredDuringExecution:") {
		t.Errorf("podAntiAffinity must NOT be required — that strands the 2nd replica; got:\n%s", out)
	}
	// An explicit affinity override must win outright (and suppress the default
	// anti-affinity) so an operator can always take back control.
	ov := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.replicaCount=2",
		"litellm.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=kubernetes.io/os",
		"litellm.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=In",
		"litellm.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]=linux",
	)
	if !strings.Contains(ov, "nodeAffinity:") {
		t.Errorf("explicit litellm.affinity must render; got:\n%s", ov)
	}
	if strings.Contains(ov, "podAntiAffinity:") {
		t.Errorf("explicit litellm.affinity must WIN and suppress the default podAntiAffinity; got:\n%s", ov)
	}
}

// The PDB (H1, !4418) only renders above one replica — at a single replica it
// would block node drains entirely. Now that the default IS HA, assert it is
// actually armed by default, since it is half of what makes drains safe.
func TestLitellm_HA_PDBRendersByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/pdb.yaml", "litellm.enabled=true")
	if !strings.Contains(out, "kind: PodDisruptionBudget") {
		t.Fatalf("litellm PDB must render at the HA default; got:\n%s", out)
	}
	if !strings.Contains(out, "maxUnavailable: 1") {
		t.Errorf("litellm PDB must bound disruption to maxUnavailable:1; got:\n%s", out)
	}
	// Renders at one replica too (gd-27vcHmrurh, revising the original H1
	// contract): unlike minAvailable:1, maxUnavailable:1 does NOT block a
	// managed-nodegroup drain at replicaCount=1 — it only blocks eviction
	// while the pod is currently unhealthy — so withholding it below HA
	// bought no safety and cost a real drain guard. See the WHY comment in
	// templates/litellm/pdb.yaml.
	out1 := helmTemplate(t, "templates/litellm/pdb.yaml",
		"litellm.enabled=true", "litellm.replicaCount=1")
	if !strings.Contains(out1, "kind: PodDisruptionBudget") {
		t.Errorf("single-replica litellm must still get a maxUnavailable:1 PDB as a drain guard; got:\n%s", out1)
	}
	if !strings.Contains(out1, "maxUnavailable: 1") {
		t.Errorf("single-replica litellm PDB must use maxUnavailable, not minAvailable (minAvailable:1 wedges drains); got:\n%s", out1)
	}
}

func TestLitellm_HA_RollingUpdateSurge(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.replicaCount=2",
	)
	if !strings.Contains(out, "type: RollingUpdate") {
		t.Errorf("HA litellm (replicaCount>1) must use RollingUpdate; got:\n%s", out)
	}
	if !strings.Contains(out, "maxSurge: 1") || !strings.Contains(out, "maxUnavailable: 0") {
		t.Errorf("HA litellm must surge maxSurge:1/maxUnavailable:0; got:\n%s", out)
	}
	if strings.Contains(out, "type: Recreate") {
		t.Errorf("HA litellm must NOT use Recreate; got:\n%s", out)
	}
}

// gd-cTobdw0kAL: litellm.nodeAffinity is a separate lever from the full
// litellm.affinity override — it must layer onto the default soft
// podAntiAffinity (both render together at replicas>1) rather than replacing
// it, since the two express independent concerns (which nodes vs. spreading
// across them). It must also render at replicaCount=1, where podAntiAffinity
// is inert, since that is litellm's actual deployed shape today.
func TestLitellm_NodeAffinity_LayersOntoPodAntiAffinity(t *testing.T) {
	t.Parallel()
	single := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.replicaCount=1",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight=100",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].key=gardenio.io/lifecycle",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].operator=In",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].values[0]=spot",
	)
	if !strings.Contains(single, "nodeAffinity:") || !strings.Contains(single, "gardenio.io/lifecycle") {
		t.Errorf("litellm.nodeAffinity must render at replicaCount=1; got:\n%s", single)
	}
	if strings.Contains(single, "podAntiAffinity:") {
		t.Errorf("podAntiAffinity must stay inert at replicaCount=1 even with nodeAffinity set; got:\n%s", single)
	}

	ha := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.replicaCount=2",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight=100",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].key=gardenio.io/lifecycle",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].operator=In",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].values[0]=spot",
	)
	if !strings.Contains(ha, "nodeAffinity:") || !strings.Contains(ha, "gardenio.io/lifecycle") {
		t.Errorf("litellm.nodeAffinity must render alongside HA podAntiAffinity; got:\n%s", ha)
	}
	if !strings.Contains(ha, "podAntiAffinity:") {
		t.Errorf("litellm.nodeAffinity must NOT suppress the default HA podAntiAffinity; got:\n%s", ha)
	}

	// The full litellm.affinity override still wins outright and suppresses
	// nodeAffinity entirely — confirming the two knobs don't fight.
	override := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.replicaCount=2",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight=100",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].key=gardenio.io/lifecycle",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].operator=In",
		"litellm.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].values[0]=spot",
		"litellm.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=kubernetes.io/os",
		"litellm.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=In",
		"litellm.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]=linux",
	)
	if strings.Contains(override, "gardenio.io/lifecycle") {
		t.Errorf("explicit litellm.affinity must WIN outright and suppress litellm.nodeAffinity too; got:\n%s", override)
	}
	if !strings.Contains(override, "kubernetes.io/os") {
		t.Errorf("explicit litellm.affinity must still render; got:\n%s", override)
	}
}
