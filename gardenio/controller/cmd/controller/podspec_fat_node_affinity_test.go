package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/podmanager"
)

// Fat-node soft affinity for freeze-eligible agents (ka-0000002348, interim half
// of the node-fit blocker fix ka-0000006507): a freeze-eligible runsc agent's
// ORIGINAL spawn should be steered — via a SOFT (preferred) node-affinity term —
// toward a gVisor node with enough total RAM to clean-rehydrate a heavy
// checkpoint on a later thaw. The term keys on Karpenter's well-known
// `karpenter.k8s.aws/instance-memory` (integer MiB) with operator Gt against
// FatNodeMemoryFloorMiB. It must appear in BOTH spec builders, weigh above the
// default c-family preference, and never appear for ineligible (runc /
// freeze-disabled) agents.

// findFatNodePreference returns the fat-node preferred term (matching
// karpenter.k8s.aws/instance-memory Gt FatNodeMemoryFloorMiB) from a spec's
// node affinity, or nil if none is present.
func findFatNodePreference(spec podmanager.AgentPodSpec) *corev1.PreferredSchedulingTerm {
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		return nil
	}
	for i := range spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		term := &spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[i]
		for _, expr := range term.Preference.MatchExpressions {
			if expr.Key == "karpenter.k8s.aws/instance-memory" &&
				expr.Operator == corev1.NodeSelectorOpGt &&
				len(expr.Values) == 1 && expr.Values[0] == FatNodeMemoryFloorMiB {
				return term
			}
		}
	}
	return nil
}

// maxCFamilyWeight returns the highest weight of any preferred term that steers
// toward a specific instance-family. The vestigial c-family preference was
// removed (ka-0000006729), so this is 0 today; the assertion below guards
// against a future instance-family preference being reintroduced at a weight
// that would outrank the fat-node steer (an m6a must still beat a c6a).
func maxCFamilyWeight(spec podmanager.AgentPodSpec) int32 {
	var maxW int32
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		return maxW
	}
	for _, term := range spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, expr := range term.Preference.MatchExpressions {
			if expr.Key == "karpenter.k8s.aws/instance-family" {
				if term.Weight > maxW {
					maxW = term.Weight
				}
			}
		}
	}
	return maxW
}

func TestBuildAgentPodSpec_FatNodeAffinity_RunscAndEnabled(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	spec := buildAgentPodSpec(cfg, runscEvent())

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	term := findFatNodePreference(spec)
	if term == nil {
		t.Fatalf("expected a fat-node preferred affinity term for a freeze-eligible runsc agent, got none")
	}
	if term.Weight != fatNodeAffinityWeight {
		t.Errorf("fat-node term weight = %d, want %d", term.Weight, fatNodeAffinityWeight)
	}
	// The fat-node preference must outrank any instance-family preference so an
	// m6a node wins over a c6a within the gVisor pool. The vestigial c-family
	// preference is gone (ka-0000006729), so this is 0 today — guards regression.
	if cw := maxCFamilyWeight(spec); term.Weight <= cw {
		t.Errorf("fat-node weight %d must exceed instance-family weight %d so m6a beats c6a", term.Weight, cw)
	}
}

func TestBuildAgentPodSpec_FatNodeAffinity_RunscButDisabled(t *testing.T) {
	// runsc resolved, but freeze/thaw is off — NOT eligible, no fat-node steer.
	cfg := freezeEligibleCfg(false)
	spec := buildAgentPodSpec(cfg, runscEvent())

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if term := findFatNodePreference(spec); term != nil {
		t.Errorf("fat-node affinity present with freeze/thaw disabled, want none")
	}
}

func TestBuildAgentPodSpec_FatNodeAffinity_RuncStaysUntouched(t *testing.T) {
	// freeze/thaw enabled but the agent is runc (no runsc role) — NOT eligible.
	cfg := freezeEligibleCfg(true)
	spec := buildAgentPodSpec(cfg, runcEvent())

	if spec.RuntimeClassName == podmanager.RuntimeClassRunsc {
		t.Fatalf("crew role should not resolve to runsc")
	}
	if term := findFatNodePreference(spec); term != nil {
		t.Errorf("fat-node affinity present for a runc agent, want none")
	}
}

// SOFT by construction: the fat-node steer must be a PREFERRED term only — it
// must never add a RequiredDuringScheduling node-selector term that could block
// a spawn when no fat node exists. (The default DoesNotExist per-mr required
// term may remain, but the fat-node key must NOT appear in any required term.)
func TestBuildAgentPodSpec_FatNodeAffinity_IsSoftNeverRequired(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	spec := buildAgentPodSpec(cfg, runscEvent())

	na := spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return // no required terms at all — trivially soft
	}
	for _, nst := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range nst.MatchExpressions {
			if expr.Key == "karpenter.k8s.aws/instance-memory" {
				t.Errorf("fat-node memory floor appeared as a REQUIRED term — must stay soft/preferred")
			}
		}
	}
}

// The reconciler path (BuildSpecFromBeadInfo) must apply the same steer so a
// runsc agent recreated/reconciled out-of-band still prefers a fat node.
func TestBuildSpecFromBeadInfo_FatNodeAffinity_RunscAndEnabled(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "", "gvisor-poc", "canary", map[string]string{})

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	term := findFatNodePreference(spec)
	if term == nil {
		t.Fatalf("expected a fat-node preferred affinity term (reconciler path), got none")
	}
	if term.Weight != fatNodeAffinityWeight {
		t.Errorf("fat-node term weight = %d, want %d (reconciler path)", term.Weight, fatNodeAffinityWeight)
	}
}

func TestBuildSpecFromBeadInfo_FatNodeAffinity_RuncStaysUntouched(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "", "crew", "worker", map[string]string{})

	if term := findFatNodePreference(spec); term != nil {
		t.Errorf("fat-node affinity present for a runc agent (reconciler path), want none")
	}
}
