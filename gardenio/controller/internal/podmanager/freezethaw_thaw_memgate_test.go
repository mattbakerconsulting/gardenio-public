package podmanager

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestFreeze_PersistsPagesBytes asserts the freeze script's FT_PAGES_BYTES
// sentinel is captured into Snapshot.PagesBytes (ka-0000000088). The pre-thaw
// memory gate reads this on later thaws to decide whether the pinned node has
// headroom for the eager pages.img restore.
func TestFreeze_PersistsPagesBytes(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	stdout := freezeOKStdout + "FT_PAGES_BYTES=3221225472\n" // exactly 3.0 GiB
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	const want int64 = 3221225472
	if snap.PagesBytes != want {
		t.Errorf("Snapshot.PagesBytes = %d, want %d", snap.PagesBytes, want)
	}
	got, err := store.Get(context.Background(), "agentsim")
	if err != nil {
		t.Fatalf("Get persisted snap: %v", err)
	}
	if got.PagesBytes != want {
		t.Errorf("persisted PagesBytes = %d, want %d", got.PagesBytes, want)
	}
}

// TestFreeze_LegacyPagesBytesMissing: a freeze script that pre-dates FT_PAGES_BYTES
// (no sentinel) must still persist a valid snapshot with PagesBytes==0 (the
// "unknown" signal the pre-thaw gate honors as INDETERMINATE → proceed).
func TestFreeze_LegacyPagesBytesMissing(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	// freezeOKStdout omits FT_PAGES_BYTES (the exact pre-ka-0000000088 shape).
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)
	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap.PagesBytes != 0 {
		t.Errorf("legacy snap PagesBytes = %d, want 0 (unknown → gate proceeds)", snap.PagesBytes)
	}
}

// memGateStdout emits the mem-gate probe sentinels for a given MemAvailable
// and pages.img size, in the exact FT_-prefixed shape the script's template
// renders. Passing pages="" simulates a legacy snapshot / unreadable image.
func memGateStdout(memAvail int64, pages string) string {
	return "" +
		"FT_NODE_MEM_AVAIL_BYTES=" + i2s(memAvail) + "\n" +
		"FT_PAGES_STAT_BYTES=" + pages + "\n" +
		"FT_MEM_GATE_OK=yes\n"
}

func i2s(n int64) string {
	if n == 0 {
		return "0"
	}
	// stdlib is fine; keep the helper local so tests read linearly.
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestPreThawMemoryGate_RefusesInsufficientMemory: a canary #12 shape: a 3.0G
// pages.img on a c6a.large's ~2.1Gi free MemAvailable. The gate MUST return a
// non-retryable *ThawInsufficientMemoryError so main.go's error branch
// cold-starts instead of driving Pod B into a doomed same-node restore that
// SIGKILLs ~7s in.
func TestPreThawMemoryGate_RefusesInsufficientMemory(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	exec := &fakeNodeExecutor{result: NodeExecResult{
		// c6a.large after backfill: ~2.1 GiB free. 3.0 GiB pages + 512 MiB
		// margin >> 2.1 GiB → refuse.
		Stdout: memGateStdout(2_200_000_000, ""),
	}}
	store := NewInMemorySnapshotStore()
	// Seed a snapshot whose PagesBytes=3.0 GiB (canary #12 subject).
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PagesBytes:  3_221_225_472,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false // opt IN (helper default-disables for legacy tests)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil {
		t.Fatal("Thaw must fail on structural memory shortfall")
	}
	var ime *ThawInsufficientMemoryError
	if !errors.As(err, &ime) {
		t.Fatalf("want *ThawInsufficientMemoryError, got %T: %v", err, err)
	}
	if ime.Retryable() {
		t.Error("ThawInsufficientMemoryError must NOT be retryable — same-node retry deterministically re-burns")
	}
	if ime.PagesBytes != 3_221_225_472 {
		t.Errorf("PagesBytes = %d, want 3221225472", ime.PagesBytes)
	}
	if ime.MemAvail != 2_200_000_000 {
		t.Errorf("MemAvail = %d, want 2200000000", ime.MemAvail)
	}
	if ime.Node != node {
		t.Errorf("Node = %q, want %q", ime.Node, node)
	}
	// Only one node exec (the mem-gate probe) — no Pod B created, no barrier
	// restore attempted. Total scripts must be exactly 1.
	if got := len(exec.scripts); got != 1 {
		t.Errorf("expected only the mem-gate probe exec (1 script), got %d", got)
	}
	// No placeholder Pod B should have been created.
	pods, _ := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	for _, pod := range pods.Items {
		if strings.HasSuffix(pod.Name, "-thaw-b") {
			t.Errorf("mem-gate refusal must not create a Pod B, but found %q", pod.Name)
		}
	}
}

// TestPreThawMemoryGate_PassesWhenNodeHasHeadroom: a genuinely fat node with
// plenty of MemAvailable must let the thaw proceed. The gate is not a general
// "always fail" — it only refuses when the node cannot hold the FATTENED
// modeled restore peak (ka-0000007579). NOTE: pre-fatten this test used
// memAvail=5.0e9 with a 3.0 GiB pages.img, which the OLD pages+512M model
// treated as "fits". The fattened model puts that same restore's peak at
// ~7.0e9 bytes (3.0GiB×1.5 + 768Mi×2 + 512Mi) — so 5.0e9 no longer has
// headroom (that was exactly the under-model that OOMed canary #14). The node
// here is sized so the REAL modeled peak fits: an m6a.xlarge-shaped ~9.0e9 free
// clears the ~7.0e9 need with room to spare.
func TestPreThawMemoryGate_PassesWhenNodeHasHeadroom(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// m6a.xlarge-shaped: ~9.0e9 free → fattened 3.0 GiB restore peak (~7.0e9) fits.
	exec := &fakeNodeExecutor{
		resultFn: func(_, script string) (NodeExecResult, error) {
			if strings.Contains(script, "FT_MEM_GATE_OK") ||
				strings.Contains(script, "MemAvailable") {
				return NodeExecResult{Stdout: memGateStdout(9_000_000_000, "")}, nil
			}
			return NodeExecResult{Stdout: thawOKStdout}, nil
		},
	}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PagesBytes:  3_221_225_472,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw with headroom must succeed: %v", err)
	}
	if res == nil || res.RestoredPID == "" {
		t.Errorf("expected restored result, got %+v", res)
	}
	// Two node execs: the mem-gate probe THEN the barrier restore.
	if got := len(exec.scripts); got != 2 {
		t.Errorf("expected mem-gate + barrier restore (2 scripts), got %d", got)
	}
}

// TestPreThawMemoryGate_LegacySnapshotFallsBackToNodeStat: a snapshot with
// PagesBytes=0 (frozen by a controller pre-ka-0000000088) MUST still be gated
// against MemAvailable via the mem-gate probe's own FT_PAGES_STAT_BYTES stat.
// This closes the "legacy snapshots skip the gate entirely" gap.
func TestPreThawMemoryGate_LegacySnapshotFallsBackToNodeStat(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	// PagesBytes=0 (legacy snapshot). Probe emits the node-stat fallback →
	// 3 GiB pages, 2 GiB free → refuse.
	exec := &fakeNodeExecutor{result: NodeExecResult{
		Stdout: memGateStdout(2_100_000_000, "3221225472"),
	}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		// PagesBytes intentionally zero: legacy.
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	var ime *ThawInsufficientMemoryError
	if !errors.As(err, &ime) {
		t.Fatalf("want *ThawInsufficientMemoryError, got %v", err)
	}
	if !ime.Fallback {
		t.Errorf("legacy-snapshot refusal must set Fallback=true (source=%q)", ime.Error())
	}
	if ime.PagesBytes != 3_221_225_472 {
		t.Errorf("PagesBytes from stat = %d, want 3221225472", ime.PagesBytes)
	}
}

// TestPreThawMemoryGate_IndeterminateProceeds: a legacy snapshot with no
// PagesBytes AND a probe that could not read pages.img (empty stat) must
// PROCEED — the barrier restore surfaces the real error rather than
// fail-closing on a probe blind spot.
func TestPreThawMemoryGate_IndeterminateProceeds(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{
		resultFn: func(_, script string) (NodeExecResult, error) {
			if strings.Contains(script, "MemAvailable") {
				return NodeExecResult{Stdout: memGateStdout(2_000_000_000, "")}, nil
			}
			return NodeExecResult{Stdout: thawOKStdout}, nil
		},
	}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		// PagesBytes=0 + probe stat empty → indeterminate.
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("indeterminate mem-gate must proceed to the barrier restore: %v", err)
	}
	if res == nil {
		t.Fatal("expected barrier restore to run and produce a result")
	}
}

// TestPreThawMemoryGate_UnknownMemAvailFailsClosed: a probe that returns no
// MemAvailable (broken /proc/meminfo) with a KNOWN PagesBytes must fail-closed
// rather than degrade silently — a lost telemetry read must not produce the
// very OOM the gate exists to prevent.
func TestPreThawMemoryGate_UnknownMemAvailFailsClosed(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	// Empty MemAvailable, empty stat. PagesBytes=3.0G from the snapshot.
	exec := &fakeNodeExecutor{result: NodeExecResult{
		Stdout: "FT_NODE_MEM_AVAIL_BYTES=\nFT_PAGES_STAT_BYTES=\nFT_MEM_GATE_OK=yes\n",
	}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PagesBytes:  3_221_225_472,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	var ime *ThawInsufficientMemoryError
	if !errors.As(err, &ime) {
		t.Fatalf("unknown MemAvailable with known PagesBytes must fail-closed with mem-gate error, got %v", err)
	}
	if ime.MemAvail != 0 {
		t.Errorf("unknown MemAvail should be reported as 0, got %d", ime.MemAvail)
	}
}

// TestPreThawMemoryGate_ProbeTransportFailureProceeds: a NodeExecutor transport
// error on the mem-gate probe (unrelated to node capacity — e.g. probe pod
// missing) must NOT masquerade as a mem-gate refusal. Proceed to the barrier
// restore so the same transport failure surfaces there with its real
// diagnostic.
func TestPreThawMemoryGate_ProbeTransportFailureProceeds(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	call := 0
	exec := &fakeNodeExecutor{
		resultFn: func(_, script string) (NodeExecResult, error) {
			call++
			if strings.Contains(script, "MemAvailable") {
				return NodeExecResult{}, errors.New("probe pod not ready")
			}
			return NodeExecResult{Stdout: thawOKStdout}, nil
		},
	}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PagesBytes:  3_221_225_472,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("probe transport failure must NOT surface as mem-gate refusal: %v", err)
	}
	if res == nil || res.RestoredPID == "" {
		t.Errorf("expected barrier restore to run and produce a result")
	}
}

// TestThawBarrierError_RC137NotRetryable: ka-0000000088 belt-and-braces —
// SubRC=137 (kernel OOM-killer SIGKILL) must be non-retryable even in the
// "SubRC>0 / RootRC==0" shape that would otherwise pass Retryable().
func TestThawBarrierError_RC137NotRetryable(t *testing.T) {
	cases := []struct {
		name    string
		root    int
		sub     int
		wantRet bool
	}{
		{"pre-fix retryable /sys-walk", 0, 128, true},
		{"OOM SIGKILL on agent sub", 0, 137, false},
		{"OOM SIGKILL on root sandbox", 137, 0, false},
		{"root netns fail-closed non-retryable", 128, 0, false},
		{"clean sub-only failure retryable", 0, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &ThawBarrierError{RootRC: tc.root, SubRC: tc.sub}
			if got := e.Retryable(); got != tc.wantRet {
				t.Errorf("Retryable() = %v, want %v (RootRC=%d SubRC=%d)", got, tc.wantRet, tc.root, tc.sub)
			}
		})
	}
}

// canary14PagesBytes / canary14MemAvail are the EXACT numbers captured on canary
// #14 (2026-07-04, node ip-10-30-141-152 m6a.large, dir-medium): pagesBytes from
// the freeze-side FT_PAGES_BYTES, MemAvailable from the pre-thaw probe. The old
// gate PASSED this shape (memAvail 6.43e9 ≫ pages 3.19e9 + 512Mi = 3.73e9,
// "2.7G headroom") and the agent-sub barrier restore then OOM-SIGKILLed anyway
// (rc=137). These constants pin the exact regression.
const (
	canary14PagesBytes int64 = 3_192_066_048 // 3.19 GB pages.img
	canary14MemAvail   int64 = 6_428_459_008 // 6.43 GB MemAvailable at probe
	canary14Containers int   = 2             // dir-medium bare agent: pause + agent
)

// TestPreThawMemoryGate_RejectsCanary14RestorePeak is the RED-bar regression for
// ka-0000007579: the EXACT canary #14 shape that the OLD (pages+512M) gate PASSED
// and that then OOM-SIGKILLed mid-restore MUST now be REFUSED by the fattened
// modeled-restore-peak gate — fail-closing to a safe cold start instead of
// driving Pod B into a doomed restore. This is the canary the whole fix exists
// to catch, so the test asserts BOTH halves explicitly: (1) the old model would
// have passed, (2) the new model rejects.
func TestPreThawMemoryGate_RejectsCanary14RestorePeak(t *testing.T) {
	// (1) RED BAR: prove the OLD model (pages.img + flat 512Mi margin) would
	// have PASSED this shape. If this inequality ever flips, the test no longer
	// guards the regression it claims to.
	if oldNeed := canary14PagesBytes + defaultMemGateMargin; canary14MemAvail < oldNeed {
		t.Fatalf("test premise broken: OLD model already refused canary #14 "+
			"(memAvail %d < pages+margin %d) — this test must reproduce a shape the OLD gate PASSED",
			canary14MemAvail, oldNeed)
	}

	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	// Match the positive-path siblings (lines 169/245/312): install the
	// readyPodReactor so that IF the gate wrongly ADMITS this shape (e.g. a
	// fix-reversion drops the multiplier 1500->1000, dropping modeled need
	// below MemAvailable), the wrongly-created Pod B reports Ready immediately
	// and the thaw completes fast — the crisp errors.As/ModeledNeed assertions
	// below then fire deterministically instead of the test hanging in
	// waitPodReady until the package timeout panics (gd-4btyeksbdd). On the
	// correct refuse path the gate short-circuits before any Pod B is created,
	// so this reactor never fires — zero behaviour change on the green path.
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{
		Stdout: memGateStdout(canary14MemAvail, ""),
	}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: canary14Containers,
		PagesBytes:  canary14PagesBytes,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.MemGateDisabled = false

	// (2) GREEN BAR under the fix: the fattened gate must refuse.
	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	var ime *ThawInsufficientMemoryError
	if !errors.As(err, &ime) {
		t.Fatalf("fattened gate must refuse the canary #14 shape, got %T: %v", err, err)
	}
	if ime.Retryable() {
		t.Error("ThawInsufficientMemoryError must NOT be retryable — same-node retry re-burns")
	}
	if ime.PagesBytes != canary14PagesBytes {
		t.Errorf("PagesBytes = %d, want %d", ime.PagesBytes, canary14PagesBytes)
	}
	if ime.MemAvail != canary14MemAvail {
		t.Errorf("MemAvail = %d, want %d", ime.MemAvail, canary14MemAvail)
	}
	if ime.ContainerCount != canary14Containers {
		t.Errorf("ContainerCount = %d, want %d", ime.ContainerCount, canary14Containers)
	}
	// The modeled need must exceed MemAvailable (that is WHY it refused) and must
	// equal the documented model: pages×1.5 + 768Mi×2 + 512Mi.
	wantNeed := canary14PagesBytes*defaultMemGateRestoreMultiplierPermille/1000 +
		defaultMemGateSentryOverhead*int64(canary14Containers) +
		defaultMemGateMargin
	if ime.ModeledNeed != wantNeed {
		t.Errorf("ModeledNeed = %d, want %d", ime.ModeledNeed, wantNeed)
	}
	if ime.ModeledNeed <= canary14MemAvail {
		t.Errorf("modeled need %d must EXCEED memAvail %d to refuse", ime.ModeledNeed, canary14MemAvail)
	}
	// Only the mem-gate probe ran — no Pod B created, no barrier restore attempted.
	if got := len(exec.scripts); got != 1 {
		t.Errorf("expected only the mem-gate probe exec (1 script), got %d", got)
	}
	pods, _ := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	for _, pod := range pods.Items {
		if strings.HasSuffix(pod.Name, "-thaw-b") {
			t.Errorf("mem-gate refusal must not create a Pod B, but found %q", pod.Name)
		}
	}
}

// TestModeledRestoreNeed pins the fattened model arithmetic (ka-0000007579)
// independent of the Thaw machinery: need = pages×mult/1000 + overhead×cc +
// margin, with the container count floored to the bare-agent baseline.
func TestModeledRestoreNeed(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		var m FreezeThawManager // zero value → every term uses its default
		// pages=2.0e9, cc=2: 2.0e9×1.5 + 768Mi×2 + 512Mi.
		got := m.modeledRestoreNeed(2_000_000_000, 2)
		want := int64(2_000_000_000)*defaultMemGateRestoreMultiplierPermille/1000 +
			defaultMemGateSentryOverhead*2 + defaultMemGateMargin
		if got != want {
			t.Errorf("modeledRestoreNeed(2e9, 2) = %d, want %d", got, want)
		}
	})
	t.Run("overridden terms are exact", func(t *testing.T) {
		m := FreezeThawManager{
			MemGateRestoreMultiplierPermille: 2000, // 2.0×
			MemGateSentryOverhead:            100,
			MemGateMargin:                    50,
		}
		// pages=1000 ×2.0 = 2000; +100×3 = 300; +50 = 2350.
		if got := m.modeledRestoreNeed(1000, 3); got != 2350 {
			t.Errorf("modeledRestoreNeed(1000, 3) = %d, want 2350", got)
		}
	})
	t.Run("container count floored to baseline", func(t *testing.T) {
		// margin=7 (a small POSITIVE value so memGateMargin honors it rather than
		// falling back to the 512Mi default — the field's zero/negative sentinel
		// means "use default"), mult 1.0×, overhead 100 to isolate the clamp.
		m := FreezeThawManager{
			MemGateRestoreMultiplierPermille: 1000, // 1.0× to isolate the overhead term
			MemGateSentryOverhead:            100,
			MemGateMargin:                    7,
		}
		// cc<=1 must clamp to baseline (2) → overhead 200, never 0/100.
		for _, cc := range []int{-3, 0, 1, 2} {
			if got := m.modeledRestoreNeed(1000, cc); got != 1000+200+7 {
				t.Errorf("modeledRestoreNeed(1000, cc=%d) = %d, want %d (baseline-floored overhead)", cc, got, 1207)
			}
		}
		// cc above baseline is honored: cc=4 → overhead 400.
		if got := m.modeledRestoreNeed(1000, 4); got != 1000+400+7 {
			t.Errorf("modeledRestoreNeed(1000, 4) = %d, want %d", got, 1407)
		}
	})
	t.Run("nonsensical multiplier falls back to default", func(t *testing.T) {
		// A permille < 1000 would model the restore as needing LESS than the
		// image it must hold resident — never fail-safe, so the guard rejects it.
		m := FreezeThawManager{MemGateRestoreMultiplierPermille: 500}
		if got := m.memGateRestoreMultiplierPermille(); got != defaultMemGateRestoreMultiplierPermille {
			t.Errorf("sub-unity multiplier = %d, want default %d", got, defaultMemGateRestoreMultiplierPermille)
		}
	})
}

// TestModeledNeedOverPagesPermille pins the WATCH-signal ratio (ka-0000002024):
// modeledRestoreNeed/pages in per-mille, plus the non-positive guard.
func TestModeledNeedOverPagesPermille(t *testing.T) {
	// need=2010, pages=1000 → 2010 permille (2.01×).
	if got := modeledNeedOverPagesPermille(2010, 1000); got != 2010 {
		t.Errorf("modeledNeedOverPagesPermille(2010,1000) = %d, want 2010", got)
	}
	// Either term non-positive → 0 (nothing to ratio).
	for _, tc := range []struct{ need, pages int64 }{{0, 1000}, {2010, 0}, {-1, 5}, {5, -1}} {
		if got := modeledNeedOverPagesPermille(tc.need, tc.pages); got != 0 {
			t.Errorf("modeledNeedOverPagesPermille(%d,%d) = %d, want 0", tc.need, tc.pages, got)
		}
	}
}

// TestModeledRatioCrossesEmpiricalFloorAt4GiB pins the WATCH crossover for
// ka-0000002024 at the bare-agent baseline (cc=2): the modeled need/pages ratio
// stays ABOVE the ≈2.01× canary #14 empirical OOM floor for the canary #14 image
// size (~3.2GiB) but decays BELOW it once pages.img exceeds ~4GiB. This is the
// exact condition the watch WARN fires on.
func TestModeledRatioCrossesEmpiricalFloorAt4GiB(t *testing.T) {
	var m FreezeThawManager // zero value → all defaults (1.5× mult, 768Mi×cc, 512Mi margin)

	// At the canary #14 image size the fattened model is ABOVE the empirical floor
	// (documented ≈2.17× effective at baseline) — no watch.
	ratio14 := modeledNeedOverPagesPermille(m.modeledRestoreNeed(canary14PagesBytes, 2), canary14PagesBytes)
	if ratio14 < empiricalPeakOverPagesFloorPermille {
		t.Errorf("canary #14 modeled ratio %d must stay >= empirical floor %d", ratio14, empiricalPeakOverPagesFloorPermille)
	}

	// Just under 4GiB: still above the floor (additive overhead still lifts it).
	const justUnder4GiB int64 = 3_900_000_000
	if r := modeledNeedOverPagesPermille(m.modeledRestoreNeed(justUnder4GiB, 2), justUnder4GiB); r < empiricalPeakOverPagesFloorPermille {
		t.Errorf("pages=%d modeled ratio %d must stay >= floor %d (crossover is ABOVE 4GiB)", justUnder4GiB, r, empiricalPeakOverPagesFloorPermille)
	}

	// A >4GiB image: modeled ratio has decayed BELOW the empirical floor — the
	// regime the watch WARN exists to surface.
	const over4GiB int64 = 5 << 30 // 5 GiB
	if r := modeledNeedOverPagesPermille(m.modeledRestoreNeed(over4GiB, 2), over4GiB); r >= empiricalPeakOverPagesFloorPermille {
		t.Errorf("pages=%d (>4GiB) modeled ratio %d must decay BELOW floor %d (watch regime)", over4GiB, r, empiricalPeakOverPagesFloorPermille)
	}
}

// TestPreThawMemoryGate_WatchWarnsOnLargePagesButStillAdmits: a >4GiB pages.img
// that the gate still PASSES (node has ample MemAvailable) must emit the
// ka-0000002024 watch WARN (modeled ratio below the ≈2.01× empirical floor) WITHOUT
// changing the gate decision — the thaw still succeeds. A canary-#14-sized image on
// the same fat node must NOT warn (its ratio stays above the floor).
func TestPreThawMemoryGate_WatchWarnsOnLargePagesButStillAdmits(t *testing.T) {
	run := func(t *testing.T, pages int64, wantWarn bool) {
		ns, node := "gvisor-poc", "node-1"
		client := fake.NewClientset()
		client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
		// A very fat node so the gate PASSES even a 5GiB image (need ≈ 5GiB×1.5 +
		// 2Gi ≈ 9.5Gi) — isolates the WATCH warn from a refusal.
		exec := &fakeNodeExecutor{
			resultFn: func(_, script string) (NodeExecResult, error) {
				if strings.Contains(script, "FT_MEM_GATE_OK") || strings.Contains(script, "MemAvailable") {
					return NodeExecResult{Stdout: memGateStdout(40_000_000_000, "")}, nil
				}
				return NodeExecResult{Stdout: thawOKStdout}, nil
			},
		}
		store := NewInMemorySnapshotStore()
		_ = store.Put(context.Background(), Snapshot{
			AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
			SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
			PagesBytes:  pages,
			PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
		})
		m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
		m.MemGateDisabled = false
		var logs strings.Builder
		m.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

		res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
		if err != nil {
			t.Fatalf("Thaw with fat node must succeed regardless of watch warn: %v", err)
		}
		if res == nil || res.RestoredPID == "" {
			t.Fatalf("expected restored result (gate decision unchanged), got %+v", res)
		}
		gotWarn := strings.Contains(logs.String(), "ka-0000002024")
		if gotWarn != wantWarn {
			t.Errorf("watch WARN present=%v, want %v (pages=%d)\nlogs:\n%s", gotWarn, wantWarn, pages, logs.String())
		}
	}
	t.Run("5GiB image warns", func(t *testing.T) { run(t, 5<<30, true) })
	t.Run("canary #14 size does not warn", func(t *testing.T) { run(t, canary14PagesBytes, false) })
}
