package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every agent pod must carry a SOFT hostname-topology spread so a single node
// loss (Spot reclaim / AWS health / expiry recycle) cannot take out N agents at
// once (Spot-hardening blast-radius proof, ka-0000005885).
func TestBuildPod_AgentTopologySpread(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(minimalSpec())

	tscs := pod.Spec.TopologySpreadConstraints
	if len(tscs) != 1 {
		t.Fatalf("expected exactly 1 topology spread constraint, got %d", len(tscs))
	}
	tsc := tscs[0]

	if tsc.MaxSkew != 1 {
		t.Errorf("MaxSkew = %d, want 1", tsc.MaxSkew)
	}
	if tsc.TopologyKey != corev1.LabelHostname {
		t.Errorf("TopologyKey = %q, want %q", tsc.TopologyKey, corev1.LabelHostname)
	}
	// Must be SOFT — a hard constraint could wedge a spawn behind
	// FailedScheduling under capacity pressure.
	if tsc.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("WhenUnsatisfiable = %q, want %q (soft)", tsc.WhenUnsatisfiable, corev1.ScheduleAnyway)
	}

	// Selector must match on LabelAgent Exists so it targets real agent pods
	// only — warm placeholders / probe pods deliberately omit LabelAgent.
	if tsc.LabelSelector == nil {
		t.Fatal("LabelSelector is nil; want a LabelAgent Exists selector")
	}
	if len(tsc.LabelSelector.MatchExpressions) != 1 {
		t.Fatalf("expected 1 match expression, got %d", len(tsc.LabelSelector.MatchExpressions))
	}
	me := tsc.LabelSelector.MatchExpressions[0]
	if me.Key != LabelAgent {
		t.Errorf("selector key = %q, want %q", me.Key, LabelAgent)
	}
	if me.Operator != metav1.LabelSelectorOpExists {
		t.Errorf("selector operator = %q, want %q", me.Operator, metav1.LabelSelectorOpExists)
	}
}
