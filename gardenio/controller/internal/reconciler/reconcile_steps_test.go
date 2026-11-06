package reconciler

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

func stepsPod(name, ns string, phase corev1.PodPhase, labels, annotations map[string]string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func TestDesiredPods(t *testing.T) {
	desired := desiredPods([]beadsapi.AgentBead{
		{ID: "b1", Mode: "crew", Project: "gardenio", Role: "crew", AgentName: "gardenio-fix-bug"},
		{ID: "b2", Mode: "job", Project: "kbeads", Role: "crew", AgentName: "plain"},
		{ID: "b3", Mode: "crew", Project: "gardenio", Role: "crew", AgentName: "laptop-one",
			Metadata: map[string]string{"execution_target": "laptop"}},
	})
	if len(desired) != 2 {
		t.Fatalf("desired = %v, want 2 entries (laptop agent excluded)", desired)
	}
	if b, ok := desired["crew-gardenio-crew-fix-bug"]; !ok || b.ID != "b1" {
		t.Errorf("project prefix not trimmed from agent name: %v", desired)
	}
	if b, ok := desired["job-kbeads-crew-plain"]; !ok || b.ID != "b2" {
		t.Errorf("missing job-kbeads-crew-plain: %v", desired)
	}
}

func TestActualPodKey(t *testing.T) {
	thaw := map[string]string{podmanager.LabelThawPlaceholder: "true"}
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{"plain pod keeps its name", stepsPod("crew-p-r-a", "ns", "", nil, nil), "crew-p-r-a"},
		{"thaw placeholder trims suffix", stepsPod("crew-p-r-a"+podmanager.ThawPlaceholderSuffix, "ns", "", thaw, nil), "crew-p-r-a"},
		{"warm-B graft uses canonical annotation", stepsPod("warm-b-node-1", "ns", "", thaw,
			map[string]string{podmanager.AnnotationCanonicalPod: "crew-p-r-a"}), "crew-p-r-a"},
		{"annotation ignored without placeholder label", stepsPod("warm-b-node-1", "ns", "", nil,
			map[string]string{podmanager.AnnotationCanonicalPod: "crew-p-r-a"}), "warm-b-node-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := actualPodKey(tt.pod); got != tt.want {
				t.Errorf("actualPodKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindOrphans(t *testing.T) {
	desired := map[string]beadsapi.AgentBead{"a": {ID: "a"}}
	actual := map[string]corev1.Pod{
		"a": stepsPod("a", "ns", corev1.PodRunning, nil, nil),
		"b": stepsPod("b-thaw-b", "ns", corev1.PodRunning, nil, nil),
	}
	orphans := findOrphans(desired, actual)
	if len(orphans) != 1 || orphans[0].key != "b" || orphans[0].pod.Name != "b-thaw-b" {
		t.Fatalf("orphans = %+v, want only key b (pod b-thaw-b)", orphans)
	}
}

// TestGuardMassOrphans guards ka-0000005463: a pass that would orphan-delete
// most of the fleet is deferred until it repeats for 3 cycles, so a partial
// daemon read cannot wipe live pods.
func TestGuardMassOrphans(t *testing.T) {
	r := &Reconciler{logger: testLogger()}
	three := []orphanPod{{key: "a"}, {key: "b"}, {key: "c"}}

	// Few orphans relative to the fleet: delete now, reset the counter.
	r.massOrphanCycles = 2
	if got := r.guardMassOrphans(three, 6); len(got) != 3 || r.massOrphanCycles != 0 {
		t.Fatalf("minority orphaning: got %d orphans, cycles %d; want 3, 0", len(got), r.massOrphanCycles)
	}
	// Under three orphans is never a mass event, even if it is most pods.
	if got := r.guardMassOrphans(three[:2], 2); len(got) != 2 || r.massOrphanCycles != 0 {
		t.Fatalf("two orphans: got %d, cycles %d; want 2, 0", len(got), r.massOrphanCycles)
	}
	// Mass orphaning is deferred for two passes, then acted on.
	for pass := 1; pass <= 2; pass++ {
		if got := r.guardMassOrphans(three, 4); got != nil {
			t.Fatalf("pass %d: got %d orphans, want deferral", pass, len(got))
		}
	}
	if got := r.guardMassOrphans(three, 4); len(got) != 3 || r.massOrphanCycles != 3 {
		t.Fatalf("third pass: got %d orphans, cycles %d; want 3, 3", len(got), r.massOrphanCycles)
	}
}

func TestNewReconcilePassCountsActivePods(t *testing.T) {
	cfg := &config.Config{GVisorNamespace: "gvisor"}
	desired := map[string]beadsapi.AgentBead{
		"run":     {Project: "p1"},
		"pending": {Project: "p1"},
		"other":   {Project: "p2"},
		"gv":      {Project: "p1"},
		"failed":  {Project: "p1"},
		"done":    {Project: "p1"},
	}
	actual := map[string]corev1.Pod{
		"run":     stepsPod("run", "main", corev1.PodRunning, nil, nil),
		"pending": stepsPod("pending", "main", corev1.PodPending, nil, nil),
		"other":   stepsPod("other", "main", corev1.PodRunning, nil, nil),
		"gv":      stepsPod("gv", "gvisor", corev1.PodRunning, nil, nil),
		"failed":  stepsPod("failed", "main", corev1.PodFailed, nil, nil),
		"done":    stepsPod("done", "main", corev1.PodSucceeded, nil, nil),
		"orphan":  stepsPod("orphan", "main", corev1.PodRunning, nil, nil),
	}
	p := newReconcilePass(cfg, desired, actual)
	if p.activePods != 4 || p.activeGVisorPods != 1 ||
		p.activePodsPerProject["p1"] != 2 || p.activePodsPerProject["p2"] != 1 {
		t.Fatalf("active=%d gvisor=%d perProject=%v; want 4, 1, {p1:2 p2:1}",
			p.activePods, p.activeGVisorPods, p.activePodsPerProject)
	}
}

func TestNewReconcilePassBurstLimit(t *testing.T) {
	for _, tt := range []struct{ cfg, want int }{{0, 3}, {-1, 3}, {7, 7}} {
		p := newReconcilePass(&config.Config{CoopBurstLimit: tt.cfg}, nil, nil)
		if p.burstLimit != tt.want {
			t.Errorf("CoopBurstLimit=%d: burstLimit = %d, want %d", tt.cfg, p.burstLimit, tt.want)
		}
	}
}

func TestInGVisorNamespaceUnset(t *testing.T) {
	p := newReconcilePass(&config.Config{}, nil, nil)
	if p.inGVisorNamespace("") {
		t.Fatal("an empty GVisorNamespace must not match the empty namespace")
	}
}

func TestSortedByCreation(t *testing.T) {
	now := time.Now()
	got := sortedByCreation(map[string]beadsapi.AgentBead{
		"new": {CreatedAt: now},
		"old": {CreatedAt: now.Add(-time.Hour)},
		"mid": {CreatedAt: now.Add(-time.Minute)},
	})
	if len(got) != 3 || got[0].name != "old" || got[1].name != "mid" || got[2].name != "new" {
		t.Fatalf("order = %v, want old, mid, new", got)
	}
}

func TestIsFreezeRestingState(t *testing.T) {
	for state, want := range map[string]bool{
		"done": true, "failed": true, "rate_limited": true, "stopping": true,
		"working": false, "queued": false, "": false,
	} {
		if got := isFreezeRestingState(state); got != want {
			t.Errorf("isFreezeRestingState(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestCapDeferral(t *testing.T) {
	bead := beadsapi.AgentBead{Project: "p"}
	tests := []struct {
		name     string
		cfg      *config.Config
		pass     reconcilePass
		isGVisor bool
		want     string
	}{
		{"admitted", &config.Config{CoopMaxPods: 10, CoopMaxPodsPerProject: 5}, reconcilePass{burstLimit: 3}, false, ""},
		{"burst limit", &config.Config{}, reconcilePass{burstLimit: 2, created: 2}, false, "burst_limit"},
		{"global cap", &config.Config{CoopMaxPods: 4}, reconcilePass{burstLimit: 3, activePods: 4}, false, "max_pods"},
		{"per-project cap", &config.Config{CoopMaxPodsPerProject: 1},
			reconcilePass{burstLimit: 3, activePodsPerProject: map[string]int{"p": 1}}, false, "max_pods_per_project"},
		{"gVisor pod skips per-project cap", &config.Config{CoopMaxPodsPerProject: 1},
			reconcilePass{burstLimit: 3, activePodsPerProject: map[string]int{"p": 1}}, true, ""},
		{"gVisor cap", &config.Config{CoopMaxPodsGVisor: 1}, reconcilePass{burstLimit: 3, activeGVisorPods: 1}, true, "max_pods_gvisor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{cfg: tt.cfg, logger: testLogger()}
			reason, stop := r.capDeferral(&tt.pass, "pod", bead, tt.isGVisor)
			if reason != tt.want || stop {
				t.Fatalf("capDeferral = (%q, %v), want (%q, false)", reason, stop, tt.want)
			}
		})
	}
}

func TestCapDeferralRateLimitStopsPass(t *testing.T) {
	r := &Reconciler{cfg: &config.Config{}, logger: testLogger(), rateLimiter: NewCreationRateLimiter(1, time.Hour)}
	r.rateLimiter.Record()
	reason, stop := r.capDeferral(&reconcilePass{burstLimit: 3}, "pod", beadsapi.AgentBead{}, false)
	if reason != "rate_limit" || !stop {
		t.Fatalf("capDeferral = (%q, %v), want (rate_limit, true)", reason, stop)
	}
}
