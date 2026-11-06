package podmanager

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// This file is the ka-0000003398 acceptance suite for fix B: the pre-thaw memory
// gate must fail-closed against min(nodeMemAvailable, podCgroupMemoryLimit), so a
// restore that would cgroup-OOM-SIGKILL against Pod B's container memory.max is
// refused (safe cold-start) EVEN when the node has abundant free RAM — the exact
// canary #15 shape (bug ka-0000000413): fat node, tight cgroup.

// warmPlaceholderWithMemLimit returns a Ready warm-B pool placeholder whose single
// app container carries the given memory LIMIT — the live cgroup memory.max the
// graft restore will run against. A "512Mi" here is the stale pre-fix pool pod.
func warmPlaceholderWithMemLimit(name, ns, node, uid, memLimit string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "agent",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memLimit)},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// TestPreThawMemoryGate_RefusesWhenPodCgroupLimitTooSmall is THE canary #15
// regression (bug ka-0000000413): a warm-B graft onto a pod whose container memory
// LIMIT is 512Mi (the stale pre-fix pool default) for a ~3G pages.img MUST be
// REFUSED — even though the node has 14.4Gi free — because the runsc restore would
// cgroup-OOM-SIGKILL against that 512Mi memory.max. The gate now models the pod
// cgroup limit as the binding constraint, not just node MemAvailable.
func TestPreThawMemoryGate_RefusesWhenPodCgroupLimitTooSmall(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	// Fat node: 14.4Gi free — the exact canary #15 node headroom. Node avail alone
	// would PASS the gate (need ≈ 6.9Gi < 14.4Gi). The 512Mi cgroup limit is what
	// must trip the refusal.
	const fatNodeAvail int64 = 15_461_882_265 // ~14.4 GiB
	client := fake.NewClientset(warmPlaceholderWithMemLimit("warm-b-stale", ns, node, "warm-uid", "512Mi"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: memGateStdout(fatNodeAvail, "")}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: canary15Containers,
		PagesBytes:  canary15PagesBytes,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	// Premise: prove the NODE term alone would have PASSED (this is the whole point
	// — node avail is the wrong denominator). If this flips, the test no longer
	// isolates the cgroup-limit constraint.
	if need := m.modeledRestoreNeed(canary15PagesBytes, canary15Containers); fatNodeAvail < need {
		t.Fatalf("test premise broken: node avail %d < modeled need %d — node term would already refuse; "+
			"this test must isolate the cgroup-limit refusal", fatNodeAvail, need)
	}

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-stale"})
	var ime *ThawInsufficientMemoryError
	if !errors.As(err, &ime) {
		t.Fatalf("gate must refuse a 512Mi-cgroup graft of a 3G restore, got %T: %v", err, err)
	}
	if ime.Retryable() {
		t.Error("cgroup-limit refusal must NOT be retryable — same pod, same limit on retry")
	}
	if ime.PodCgroupLimit != 512*1024*1024 {
		t.Errorf("PodCgroupLimit = %d, want 512Mi (the stale pool limit)", ime.PodCgroupLimit)
	}
	if !ime.LimitBound {
		t.Error("LimitBound must be true — the cgroup limit (not node avail) was the binding constraint")
	}
	if ime.MemAvail != fatNodeAvail {
		t.Errorf("MemAvail = %d, want the fat-node %d (recorded for the diagnostic)", ime.MemAvail, fatNodeAvail)
	}
	// The error message must name the cgroup as the binding constraint.
	if !strings.Contains(ime.Error(), "cgroup") {
		t.Errorf("error should attribute the refusal to the cgroup limit: %s", ime.Error())
	}
	// Only the mem-gate probe ran — no barrier restore attempted against the doomed pod.
	if got := len(exec.scripts); got != 1 {
		t.Errorf("expected only the mem-gate probe exec (1 script), got %d", got)
	}
}

// TestPreThawMemoryGate_PassesWarmGraftWithAdequateLimit: the same fat-node,
// ~3G-restore graft PASSES when the warm-B pod carries a limit that covers the
// restore peak (a post-fix pool pod minted with the raised limit). Proves the
// cgroup term is not a blanket "always refuse warm-B" — it only refuses a tight
// cgroup.
func TestPreThawMemoryGate_PassesWarmGraftWithAdequateLimit(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	const fatNodeAvail int64 = 15_461_882_265
	// Post-fix pool pod: limit sized to the restore-peak model (fits the 3G restore).
	adequate := resource.NewQuantity(modeledRestorePeakLimit(canary15PagesBytes, canary15Containers), resource.BinarySI)
	client := fake.NewClientset(warmPlaceholderWithMemLimit("warm-b-fresh", ns, node, "warm-uid", adequate.String()))
	exec := &fakeNodeExecutor{
		resultFn: func(_, script string) (NodeExecResult, error) {
			if strings.Contains(script, "MemAvailable") {
				return NodeExecResult{Stdout: memGateStdout(fatNodeAvail, "")}, nil
			}
			return NodeExecResult{Stdout: thawOKStdout}, nil
		},
	}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: canary15Containers,
		PagesBytes:  canary15PagesBytes,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-fresh"})
	if err != nil {
		t.Fatalf("graft with an adequate cgroup limit must succeed: %v", err)
	}
	if res == nil || res.RestoredPID == "" {
		t.Errorf("expected a restored result, got %+v", res)
	}
	// mem-gate probe THEN barrier restore.
	if got := len(exec.scripts); got != 2 {
		t.Errorf("expected mem-gate + barrier restore (2 scripts), got %d", got)
	}
}

// TestPreThawMemoryGate_UnboundedWarmGraftBindsOnNodeOnly: a warm-B pod with NO
// memory limit has no cgroup ceiling to OOM against, so the cgroup term does not
// bind and the decision falls back to node MemAvailable alone (a fat node passes).
func TestPreThawMemoryGate_UnboundedWarmGraftBindsOnNodeOnly(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	const fatNodeAvail int64 = 15_461_882_265
	unbounded := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-b-unbounded", Namespace: ns, UID: types.UID("u")},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "agent"}}, // no limits at all
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	client := fake.NewClientset(unbounded)
	exec := &fakeNodeExecutor{
		resultFn: func(_, script string) (NodeExecResult, error) {
			if strings.Contains(script, "MemAvailable") {
				return NodeExecResult{Stdout: memGateStdout(fatNodeAvail, "")}, nil
			}
			return NodeExecResult{Stdout: thawOKStdout}, nil
		},
	}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: canary15Containers,
		PagesBytes:  canary15PagesBytes,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-unbounded"})
	if err != nil {
		t.Fatalf("unbounded graft on a fat node must pass (no cgroup ceiling): %v", err)
	}
	if res == nil || res.RestoredPID == "" {
		t.Errorf("expected a restored result, got %+v", res)
	}
}

// livePodAWithRoleLimit is livePodA with the gvisor-poc ROLE memory LIMIT (8Gi)
// stamped on the agent container — the EXACT shape the create-then-restore path
// DeepCopies from the frozen Pod A (bug ka-0000000413: create-path Pod B inherits
// the 8Gi role limit and cgroup-OOMs the root restore). livePodA itself is
// unbounded, which is not representative of the prod create path.
func livePodAWithRoleLimit(name, ns, node string) *corev1.Pod {
	pod := livePodA(name, ns, node)
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
	}
	return pod
}

// TestPreThawMemoryGate_CreatePathPassesOnFatNode: on the CREATE path (no supplied
// placeholder), the gate models the limit shapePlaceholderSpec WILL mint. Because
// that limit is >= the modeled need by construction, the create path with a fat
// node PASSES (the cgroup term is belt-and-braces, never the binding constraint at
// production defaults). It also proves fix A end-to-end: the minted Pod B carries a
// LIMIT RAISED from the inherited 8Gi role limit to the restore-peak model.
func TestPreThawMemoryGate_CreatePathPassesOnFatNode(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	const fatNodeAvail int64 = 15_461_882_265
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{
		resultFn: func(_, script string) (NodeExecResult, error) {
			if strings.Contains(script, "MemAvailable") {
				return NodeExecResult{Stdout: memGateStdout(fatNodeAvail, "")}, nil
			}
			return NodeExecResult{Stdout: thawOKStdout}, nil
		},
	}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: canary15Containers,
		PagesBytes: canary15PagesBytes,
		// Inherits the 8Gi role limit — the prod create-path shape.
		PodTemplate: sanitizePodTemplate(livePodAWithRoleLimit("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("create path on a fat node must pass (fix A limit >= need): %v", err)
	}
	if res == nil || res.RestoredPID == "" {
		t.Errorf("expected a restored result, got %+v", res)
	}
	// Prove the minted Pod B carries a limit RAISED from the inherited 8Gi to the
	// restore-peak model — the create-path fix A end-to-end through buildPlaceholderPodB.
	podB, gerr := client.CoreV1().Pods(ns).Get(context.Background(), "agentsim-thaw-b", metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("Pod B not created: %v", gerr)
	}
	lim := podB.Spec.Containers[0].Resources.Limits.Memory()
	wantLim := modeledRestorePeakLimit(canary15PagesBytes, canary15Containers)
	if lim.Value() != wantLim {
		t.Errorf("minted Pod B memory limit = %d, want restore-peak model %d (raised from inherited 8Gi)", lim.Value(), wantLim)
	}
	// And the request stays the pages fence, not the fat limit — admission-safe.
	if got := podB.Spec.Containers[0].Resources.Requests.Memory(); got.Value() != canary15PagesBytes {
		t.Errorf("minted Pod B memory request = %d, want pages fence %d", got.Value(), canary15PagesBytes)
	}
}

// TestPreThawMemoryGate_CreatePathUnboundedTemplateBindsOnNodeOnly: on the create
// path, a snapshot template with NO memory limit is minted UNBOUNDED by
// shapePlaceholderSpec (raise-only never imposes a limit), so the gate must model
// NO cgroup ceiling and bind on node MemAvailable alone. This proves the
// create-path term faithfully mirrors the minted pod rather than over-stating a
// modeledRestorePeakLimit the pod will not actually carry.
func TestPreThawMemoryGate_CreatePathUnboundedTemplateBindsOnNodeOnly(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	// Tight node that is BELOW the modeled need but the template is unbounded — if
	// the gate over-stated the ceiling it would still refuse on the node term, so
	// use a FAT node and assert PASS: only a faithfully-unbounded ceiling passes.
	const fatNodeAvail int64 = 15_461_882_265
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{
		resultFn: func(_, script string) (NodeExecResult, error) {
			if strings.Contains(script, "MemAvailable") {
				return NodeExecResult{Stdout: memGateStdout(fatNodeAvail, "")}, nil
			}
			return NodeExecResult{Stdout: thawOKStdout}, nil
		},
	}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: canary15Containers,
		PagesBytes: canary15PagesBytes,
		// livePodA's agent container is UNBOUNDED (no memory limit).
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("create path with an unbounded template on a fat node must pass: %v", err)
	}
	if res == nil || res.RestoredPID == "" {
		t.Errorf("expected a restored result, got %+v", res)
	}
	// The minted Pod B must stay unbounded (raise-only never imposes a limit).
	podB, gerr := client.CoreV1().Pods(ns).Get(context.Background(), "agentsim-thaw-b", metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("Pod B not created: %v", gerr)
	}
	if _, bounded := podB.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; bounded {
		t.Errorf("unbounded template must mint an unbounded Pod B, got limit %s", podB.Spec.Containers[0].Resources.Limits.Memory())
	}
}

// TestPreThawMemoryGate_NodeTermStillBindsWhenTighterThanLimit: the node term must
// STILL fire when the node is the tighter constraint (canary #12/#14 shape) — fix
// B is min(node, cgroup), so a tight node with a fat cgroup still refuses, and the
// refusal is attributed to the node (LimitBound=false).
func TestPreThawMemoryGate_NodeTermStillBindsWhenTighterThanLimit(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	// Tight node: 2.1Gi free (canary #12 shape). Pod carries a FAT limit, so the
	// node is the binding constraint.
	const tightNodeAvail int64 = 2_200_000_000
	fatLimit := resource.NewQuantity(modeledRestorePeakLimit(canary15PagesBytes, canary15Containers), resource.BinarySI)
	client := fake.NewClientset(warmPlaceholderWithMemLimit("warm-b-fatlimit", ns, node, "warm-uid", fatLimit.String()))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: memGateStdout(tightNodeAvail, "")}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: canary15Containers,
		PagesBytes:  canary15PagesBytes,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-fatlimit"})
	var ime *ThawInsufficientMemoryError
	if !errors.As(err, &ime) {
		t.Fatalf("tight node must still refuse, got %T: %v", err, err)
	}
	if ime.LimitBound {
		t.Error("LimitBound must be false — the NODE (not the fat cgroup) was the binding constraint here")
	}
	if ime.MemAvail != tightNodeAvail {
		t.Errorf("MemAvail = %d, want %d", ime.MemAvail, tightNodeAvail)
	}
	if !strings.Contains(ime.Error(), "node MemAvailable") {
		t.Errorf("error should attribute the refusal to node MemAvailable: %s", ime.Error())
	}
}

// TestThaw_RestorePeakMeasurementSurfaced is the fix-C plumbing test: the node
// script's FT_RESTORE_PEAK_BYTES sentinel is parsed onto ThawResult.RestorePeakBytes.
func TestThaw_RestorePeakMeasurementSurfaced(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// Append the fix-C sentinel to the OK thaw output: an observed 8.5G peak.
	const observedPeak int64 = 8_589_934_592 // 8.0 GiB
	stdout := thawOKStdout + "FT_RESTORE_PEAK_BYTES=" + i2s(observedPeak) + "\n"
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PagesBytes:  canary15PagesBytes,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if res.RestorePeakBytes != observedPeak {
		t.Errorf("RestorePeakBytes = %d, want %d (fix-C measurement)", res.RestorePeakBytes, observedPeak)
	}
}

// TestThaw_RestorePeakMeasurementBestEffort: a thaw whose node script does NOT emit
// FT_RESTORE_PEAK_BYTES (legacy script / cgroup-v1 node) still succeeds; the field
// is left 0 — the measurement never gates the verified restore.
func TestThaw_RestorePeakMeasurementBestEffort(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// thawOKStdout has no FT_RESTORE_PEAK_BYTES.
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PagesBytes:  canary15PagesBytes,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw must succeed without the measurement sentinel: %v", err)
	}
	if res.RestorePeakBytes != 0 {
		t.Errorf("RestorePeakBytes = %d, want 0 when unmeasured", res.RestorePeakBytes)
	}
}

// TestThawScript_EmitsRestorePeakSentinel proves the node thaw script actually
// contains the fix-C memory.peak read + FT_RESTORE_PEAK_BYTES emit (guards a
// template revert that would silently drop the measurement plumbing).
func TestThawScript_EmitsRestorePeakSentinel(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "warm-b-1", BPodUID: "u", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apu", ImagePath: "/img", BundlePath: "/bundle", ContainerCount: 2, RunscTimeout: 120,
	})
	for _, want := range []string{"FT_RESTORE_PEAK_BYTES=", "memory.peak", "peak_for_cid"} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing fix-C measurement token %q", want)
		}
	}
}
