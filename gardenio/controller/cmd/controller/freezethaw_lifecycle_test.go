package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/poolmanager"
)

// fakeNodeExec is a no-op NodeExecutor; the lifecycle gate tests never reach a
// node exec (shouldThaw/shouldFreeze short-circuit first), so the result is
// irrelevant.
type fakeNodeExec struct{}

func (fakeNodeExec) Exec(context.Context, string, string) (podmanager.NodeExecResult, error) {
	return podmanager.NodeExecResult{}, nil
}

func TestNewFreezeThawLifecycle_DisabledByDefault(t *testing.T) {
	cfg := &config.Config{FreezeThawEnabled: false}
	ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())
	if ft != nil {
		t.Fatal("expected nil lifecycle when FreezeThawEnabled is false")
	}
	// A nil lifecycle must be safe to call (cold-start fallback everywhere).
	if ft.shouldThaw(context.Background(), podmanager.AgentPodSpec{RuntimeClassName: "runsc"}) {
		t.Error("nil lifecycle shouldThaw must be false")
	}
	if ft.shouldFreeze(context.Background(), fake.NewClientset(), "p", "ns") {
		t.Error("nil lifecycle shouldFreeze must be false")
	}
	// C2 (ka-0000005834): nil lifecycle interrupt verbs are safe no-ops.
	if ft.abortFreeze("a") {
		t.Error("nil lifecycle abortFreeze must be false")
	}
	if ft.isFreezing("a") {
		t.Error("nil lifecycle isFreezing must be false")
	}
	// harden-thaw (ka-0000001355): nil lifecycle drain verbs are safe no-ops.
	if n := ft.inflightThawCount(); n != 0 {
		t.Errorf("nil lifecycle inflightThawCount = %d, want 0", n)
	}
	if n := ft.drainThaws(context.Background(), time.Second, 0); n != 0 {
		t.Errorf("nil lifecycle drainThaws = %d, want 0", n)
	}
}

// TestNewFreezeThawLifecycle_AppliesHardDeadline asserts harden-thaw
// (ka-0000001355): cfg.ThawHardDeadline (env THAW_HARD_DEADLINE) flows through to
// the manager so the detached post-commit critical section is capped, and an
// unset value leaves the manager on its computed default (0 field).
func TestNewFreezeThawLifecycle_AppliesHardDeadline(t *testing.T) {
	// Unset: manager keeps its computed default (field stays 0).
	cfg := &config.Config{FreezeThawEnabled: true}
	ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())
	if ft.mgr.ThawHardDeadline != 0 {
		t.Errorf("ThawHardDeadline must default to 0 (computed cap), got %v", ft.mgr.ThawHardDeadline)
	}
	// Opted in: flows through.
	cfgOn := &config.Config{FreezeThawEnabled: true, ThawHardDeadline: 4 * time.Minute}
	ftOn := newFreezeThawLifecycle(cfgOn, fake.NewClientset(), fakeNodeExec{}, discardLogger())
	if ftOn.mgr.ThawHardDeadline != 4*time.Minute {
		t.Errorf("ThawHardDeadline = %v, want 4m", ftOn.mgr.ThawHardDeadline)
	}
	// drainThaws with budget<=0 reports the current count without blocking.
	if n := ftOn.drainThaws(context.Background(), 0, 0); n != 0 {
		t.Errorf("drainThaws budget=0 with no thaws = %d, want 0", n)
	}
}

// TestNewFreezeThawLifecycle_InterruptFlag asserts C2 (ka-0000005834): the
// FreezeInterruptEnabled flag flows through to the manager's InterruptEnabled so
// the abort seam is armed only when opted in, and stays off (default) otherwise.
func TestNewFreezeThawLifecycle_InterruptFlag(t *testing.T) {
	// Default off: freeze/thaw on, interrupt not set.
	cfg := &config.Config{FreezeThawEnabled: true}
	ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())
	if ft == nil {
		t.Fatal("expected non-nil lifecycle")
	}
	if ft.mgr.InterruptEnabled {
		t.Error("InterruptEnabled must default off when FreezeInterruptEnabled is unset")
	}
	// Opted in: flag flows through.
	cfgOn := &config.Config{FreezeThawEnabled: true, FreezeInterruptEnabled: true}
	ftOn := newFreezeThawLifecycle(cfgOn, fake.NewClientset(), fakeNodeExec{}, discardLogger())
	if !ftOn.mgr.InterruptEnabled {
		t.Error("InterruptEnabled must be true when FreezeInterruptEnabled is set")
	}
}

func TestNewFreezeThawLifecycle_EnabledConstructs(t *testing.T) {
	cfg := &config.Config{FreezeThawEnabled: true}
	ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())
	if ft == nil {
		t.Fatal("expected non-nil lifecycle when FreezeThawEnabled is true")
	}
}

// TestNewFreezeThawLifecycle_AppliesRehydrateDeadline asserts C3
// (ka-0000004017): the cfg.ThawRehydrateDeadline (env THAW_REHYDRATE_DEADLINE)
// flows through to the manager's RehydrateDeadline so the rehydration gate
// fails-closed within the operator-tuned budget.
func TestNewFreezeThawLifecycle_AppliesRehydrateDeadline(t *testing.T) {
	cfg := &config.Config{FreezeThawEnabled: true, ThawRehydrateDeadline: 50 * time.Millisecond}
	ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())
	if ft == nil {
		t.Fatal("expected non-nil lifecycle")
	}
	if ft.mgr.RehydrateDeadline != 50*time.Millisecond {
		t.Errorf("ThawRehydrateDeadline not applied to manager: got %v, want 50ms", ft.mgr.RehydrateDeadline)
	}
}

// TestNewFreezeThawLifecycle_LogsEffectiveFreezeDeadline asserts FIX 1
// (task ka-0000006783): the startup log echoes the EFFECTIVE freeze hard-deadline
// so the freeze cap gets the same boot-time visibility the rehydrate /
// create-restore timeouts already had. With no override the baseline computed cap
// (runscTimeout + per-container margin at the bare-agent baseline = 240s) must
// appear, tagged as a non-override; with a positive override the override value
// must appear, tagged as an override.
func TestNewFreezeThawLifecycle_LogsEffectiveFreezeDeadline(t *testing.T) {
	t.Run("computed default", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		cfg := &config.Config{FreezeThawEnabled: true}
		ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, logger)
		if ft == nil {
			t.Fatal("expected non-nil lifecycle")
		}
		out := buf.String()
		if !strings.Contains(out, "freezeHardDeadline=4m0s") {
			t.Errorf("startup log missing effective freeze deadline (want 4m0s baseline), got:\n%s", out)
		}
		if !strings.Contains(out, "freezeHardDeadlineOverride=false") {
			t.Errorf("computed default must log freezeHardDeadlineOverride=false, got:\n%s", out)
		}
		if strings.Contains(out, "FREEZE_HARD_DEADLINE set but ineffective") {
			t.Errorf("no override set: must not warn about an ineffective override, got:\n%s", out)
		}
	})
	t.Run("operator override", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		cfg := &config.Config{FreezeThawEnabled: true, FreezeHardDeadline: 7 * time.Minute}
		ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, logger)
		if ft == nil {
			t.Fatal("expected non-nil lifecycle")
		}
		out := buf.String()
		if !strings.Contains(out, "freezeHardDeadline=7m0s") {
			t.Errorf("startup log must echo the operator override (7m0s), got:\n%s", out)
		}
		if !strings.Contains(out, "freezeHardDeadlineOverride=true") {
			t.Errorf("override set: must log freezeHardDeadlineOverride=true, got:\n%s", out)
		}
	})
}

// TestNewFreezeThawLifecycle_WarnsIneffectiveFreezeDeadline asserts FIX 1's
// silent-drop surfacing (task ka-0000006783): when config parsing flagged a
// set-but-ineffective FREEZE_HARD_DEADLINE (unparseable / non-positive → dropped
// to the computed cap), the startup path emits a WARN naming it, and the
// effective deadline reported still reflects the computed cap (no gating change).
func TestNewFreezeThawLifecycle_WarnsIneffectiveFreezeDeadline(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// FreezeHardDeadline stays 0 (the drop), FreezeHardDeadlineInvalid records that
	// the operator DID set the knob — exactly what Parse produces on a bad value.
	cfg := &config.Config{FreezeThawEnabled: true, FreezeHardDeadlineInvalid: true}
	ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, logger)
	if ft == nil {
		t.Fatal("expected non-nil lifecycle")
	}
	out := buf.String()
	if !strings.Contains(out, "FREEZE_HARD_DEADLINE set but ineffective") {
		t.Errorf("invalid override must emit a WARN, got:\n%s", out)
	}
	// The warn carries the effective (computed) cap so the operator sees what
	// actually applies — the baseline 4m0s, not a phantom zero.
	if !strings.Contains(out, "freezeHardDeadlineEffective=4m0s") {
		t.Errorf("ineffective-override warn must carry the computed effective cap (4m0s), got:\n%s", out)
	}
}

func TestShouldThaw_OnlyRunscWithSnapshot(t *testing.T) {
	cfg := &config.Config{FreezeThawEnabled: true}
	ft := newFreezeThawLifecycle(cfg, fake.NewClientset(), fakeNodeExec{}, discardLogger())

	// Non-runsc agent: never thaws regardless of snapshot state.
	if ft.shouldThaw(context.Background(), podmanager.AgentPodSpec{AgentName: "a", RuntimeClassName: ""}) {
		t.Error("non-runsc agent must not thaw")
	}
	// runsc agent, no snapshot yet (first spawn): cold start, not thaw.
	runscSpec := podmanager.AgentPodSpec{AgentName: "a", RuntimeClassName: podmanager.RuntimeClassRunsc}
	if ft.shouldThaw(context.Background(), runscSpec) {
		t.Error("runsc agent with no snapshot must cold-start, not thaw")
	}
	// Seed a snapshot for the agent → now it should thaw.
	store := podmanager.NewInMemorySnapshotStore()
	if err := store.Put(context.Background(), podmanager.Snapshot{AgentName: "a", SandboxCID: "cid"}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	// Rebuild the lifecycle around the seeded store so HasSnapshot sees it.
	mgr := podmanager.NewFreezeThawManager(fake.NewClientset(), fakeNodeExec{}, store, nil, discardLogger())
	ft = &freezeThawLifecycle{mgr: mgr, logger: discardLogger()}
	if !ft.shouldThaw(context.Background(), runscSpec) {
		t.Error("runsc agent with a snapshot must thaw")
	}
}

// TestShouldThaw_SessionResumeFalseVetoes locks in the --fresh half of task
// ka-0000008188: even for a runsc agent WITH a snapshot, an explicit
// SessionResume=false vetoes the thaw so the pod cold-starts a clean session
// instead of restoring the frozen conversation. A nil SessionResume (default)
// and an explicit true both keep thawing.
func TestShouldThaw_SessionResumeFalseVetoes(t *testing.T) {
	store := podmanager.NewInMemorySnapshotStore()
	if err := store.Put(context.Background(), podmanager.Snapshot{AgentName: "a", SandboxCID: "cid"}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	mgr := podmanager.NewFreezeThawManager(fake.NewClientset(), fakeNodeExec{}, store, nil, discardLogger())
	ft := &freezeThawLifecycle{mgr: mgr, logger: discardLogger()}

	base := podmanager.AgentPodSpec{AgentName: "a", RuntimeClassName: podmanager.RuntimeClassRunsc}

	// Default (nil SessionResume): thaws as before.
	if !ft.shouldThaw(context.Background(), base) {
		t.Error("runsc agent with a snapshot and nil SessionResume must thaw")
	}
	// Explicit true: still thaws.
	resumeTrue := true
	specTrue := base
	specTrue.SessionResume = &resumeTrue
	if !ft.shouldThaw(context.Background(), specTrue) {
		t.Error("runsc agent with a snapshot and SessionResume=true must thaw")
	}
	// Explicit false (--fresh): thaw is vetoed → clean cold start.
	resumeFalse := false
	specFalse := base
	specFalse.SessionResume = &resumeFalse
	if ft.shouldThaw(context.Background(), specFalse) {
		t.Error("SessionResume=false must veto the thaw so --fresh starts clean")
	}
}

func TestShouldFreeze_OnlyRunscLivePod(t *testing.T) {
	cfg := &config.Config{FreezeThawEnabled: true}

	runsc := podmanager.RuntimeClassRunsc
	runscPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runsc-pod", Namespace: "ns"},
		Spec:       corev1.PodSpec{RuntimeClassName: &runsc},
	}
	plainPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-pod", Namespace: "ns"},
		Spec:       corev1.PodSpec{},
	}
	client := fake.NewClientset(runscPod, plainPod)
	ft := newFreezeThawLifecycle(cfg, client, fakeNodeExec{}, discardLogger())

	if !ft.shouldFreeze(context.Background(), client, "runsc-pod", "ns") {
		t.Error("runsc pod should freeze")
	}
	if ft.shouldFreeze(context.Background(), client, "plain-pod", "ns") {
		t.Error("non-runsc pod must not freeze")
	}
	if ft.shouldFreeze(context.Background(), client, "missing-pod", "ns") {
		t.Error("missing pod must not freeze")
	}
}

// WS3 canary role allowlist at the controller seam (task ka-0000000138). When
// FREEZE_THAW_ROLE_ALLOWLIST is non-empty, shouldFreeze checkpoints a runsc pod
// only if its gardenio.io/role label is allow-listed — so freeze/thaw can be
// proven on one role (e.g. plan-gardener) while every other runsc role stays on
// cold-start. This must stay in lockstep with the spawn-time isFreezeEligible
// signal so the gb-stop suspend decision and the actual checkpoint never drift.
func TestShouldFreeze_RoleAllowlist(t *testing.T) {
	runsc := podmanager.RuntimeClassRunsc
	runscPodWithRole := func(name, role string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "ns",
				Labels:    map[string]string{podmanager.LabelRole: role},
			},
			Spec: corev1.PodSpec{RuntimeClassName: &runsc},
		}
	}
	capPod := runscPodWithRole("cap-pod", "plan-gardener")
	threadPod := runscPodWithRole("thread-pod", "thread")
	client := fake.NewClientset(capPod, threadPod)

	// Allowlist = {plan-gardener}: only the gardener freezes; thread stays cold.
	cfg := &config.Config{FreezeThawEnabled: true, FreezeThawRoleAllowlist: []string{"plan-gardener"}}
	ft := newFreezeThawLifecycle(cfg, client, fakeNodeExec{}, discardLogger())
	if !ft.shouldFreeze(context.Background(), client, "cap-pod", "ns") {
		t.Error("allow-listed plan-gardener runsc pod should freeze")
	}
	if ft.shouldFreeze(context.Background(), client, "thread-pod", "ns") {
		t.Error("thread runsc pod absent from the allowlist must NOT freeze")
	}

	// Empty allowlist: every runsc role freezes (pre-WS3 behaviour preserved).
	cfgOpen := &config.Config{FreezeThawEnabled: true}
	ftOpen := newFreezeThawLifecycle(cfgOpen, client, fakeNodeExec{}, discardLogger())
	if !ftOpen.shouldFreeze(context.Background(), client, "thread-pod", "ns") {
		t.Error("empty allowlist should freeze every runsc role, including thread")
	}
}

// --- C2 warm-B wake-path routing (ka-0000005266) ----------------------------

// readyWarmPlaceholder builds a Ready warm-B placeholder pod on node in ns. It
// carries the topology label for an empty Pod template, matching the snapshot
// newWarmWakeLifecycle seeds (also an empty PodTemplate), so the topology-aware
// claim (ka-0000002219) hands it out for that snapshot's thaw.
func readyWarmPlaceholder(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				podmanager.LabelApp:             podmanager.LabelAppValue,
				podmanager.LabelWarmPlaceholder: podmanager.WarmPlaceholderReady,
				podmanager.LabelWarmTopology:    podmanager.PodTemplateTopologyKey(corev1.PodSpec{}),
			},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// failingNodeExec makes the on-node restore script "succeed" at the exec layer
// but report a non-zero root restore rc, so Thaw fails its post-state assertion.
// This lets the routing tests observe WHICH Pod B a thaw targeted (the warm
// placeholder vs a freshly-created one) without depending on podmanager-private
// success-stdout fixtures.
type failingNodeExec struct{}

func (failingNodeExec) Exec(context.Context, string, string) (podmanager.NodeExecResult, error) {
	return podmanager.NodeExecResult{Stdout: "FT_IMAGE_OK=yes\nFT_ROOT_RC=1\n", ExitCode: 1}, nil
}

// trackPodBCreate installs a non-mutating create reactor that flips *bool true
// the first time a pod named podBName is created, and returns that flag. The
// create-path routing tests assert on CREATION rather than post-thaw presence
// because the create-path teardown defer (createPathPodBTeardown, ka-0000005365)
// deletes Pod B on the barrier-restore failure by design.
func trackPodBCreate(client *fake.Clientset, podBName string) *bool {
	created := new(bool)
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		if pod, ok := action.(ktesting.CreateAction).GetObject().(*corev1.Pod); ok && pod.Name == podBName {
			*created = true
		}
		return false, nil, nil
	})
	return created
}

// newWarmWakeLifecycle builds a lifecycle whose manager + pool share one fake
// client and namespace, with a snapshot seeded for agentName on node.
func newWarmWakeLifecycle(t *testing.T, client *fake.Clientset, ns, agentName, node string, withPool bool) *freezeThawLifecycle {
	t.Helper()
	store := podmanager.NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), podmanager.Snapshot{
		AgentName: agentName, Namespace: ns, PodName: agentName, Node: node,
		ImagePath: "/x", BundlePath: "/y", SandboxCID: "s", AgentCID: "a", PodUID: "u", ContainerCount: 2,
	})
	mgr := podmanager.NewFreezeThawManager(client, failingNodeExec{}, store, nil, discardLogger())
	mgr.PodReadyTimeout = 500 * time.Millisecond
	// The create-then-restore path now consults its own clone-aware budget
	// (ka-0000004449); bound it tight too so the warm-miss/no-pool tests that
	// drive Pod B to a Ready-timeout fail fast instead of waiting the 600s default.
	mgr.CreateRestorePodReadyTimeout = 500 * time.Millisecond
	mgr.PodReadyPollInterval = time.Millisecond
	ft := &freezeThawLifecycle{mgr: mgr, logger: discardLogger()}
	if withPool {
		pool := poolmanager.NewPlaceholderPool(client, poolmanager.PlaceholderPoolConfig{
			Namespace: ns, Image: "agent:latest", MinSize: 1,
		}, discardLogger())
		ft.setPlaceholderPool(pool)
	}
	return ft
}

// TestThaw_WarmHitGraftFailureTearsDownPromoted asserts the C2 warm path both
// routes correctly (claims the co-resident placeholder) AND, when the subsequent
// graft fails WITHOUT a result, tears the promoted pod down so it does not leak
// alongside the cold-start fallback (the ka-0000007549 leak class). failingNodeExec
// makes the on-node restore report rc=1, so Thaw returns (nil, err) — the exact
// case main.go's res!=nil teardown cannot see.
func TestThaw_WarmHitGraftFailureTearsDownPromoted(t *testing.T) {
	ns, agent, node := "gvisor", "agentsim", "node-a"
	client := fake.NewClientset(readyWarmPlaceholder("warm-b-node-a-1", ns, node))
	ft := newWarmWakeLifecycle(t, client, ns, agent, node, true)

	spec := podmanager.AgentPodSpec{AgentName: agent, Project: "gardenio", Mode: "crew", Role: "crew",
		RuntimeClassName: podmanager.RuntimeClassRunsc}
	res, resumePath, err := ft.thaw(context.Background(), spec, "gd-bead")
	if err == nil {
		t.Fatal("expected a restore failure from failingNodeExec")
	}
	if res != nil {
		t.Fatalf("rc=1 graft must return a nil result, got %+v", res)
	}
	if resumePath != gardeniometrics.ResumePathThawWarm {
		t.Errorf("warm graft must report thaw-warm path, got %q", resumePath)
	}
	// The warm path was taken (the create-path Pod B would be "<agent>-thaw-b"):
	// the claimed warm pod must have been deleted after the failed graft, NOT
	// left running as a promoted duplicate.
	if _, gerr := client.CoreV1().Pods(ns).Get(context.Background(), "warm-b-node-a-1", metav1.GetOptions{}); !apierrors.IsNotFound(gerr) {
		t.Fatalf("promoted warm pod must be torn down after a failed graft; get err=%v", gerr)
	}
	// And it must NOT have fallen back to creating a -thaw-b create-path pod
	// (that would mean a double-thaw, not a clean teardown).
	if _, gerr := client.CoreV1().Pods(ns).Get(context.Background(), agent+"-thaw-b", metav1.GetOptions{}); !apierrors.IsNotFound(gerr) {
		t.Errorf("warm-path failure must not also create a create-path Pod B")
	}
}

func TestThaw_WarmMissFallsBackToCreatePath(t *testing.T) {
	ns, agent, node := "gvisor", "agentsim", "node-a"
	// Pool enabled, but NO warm placeholder on the snapshot's node → miss.
	client := fake.NewClientset(readyWarmPlaceholder("warm-b-other-1", ns, "node-OTHER"))
	createdPodB := trackPodBCreate(client, agent+"-thaw-b")
	ft := newWarmWakeLifecycle(t, client, ns, agent, node, true)

	spec := podmanager.AgentPodSpec{AgentName: agent, Project: "gardenio", Mode: "crew", Role: "crew",
		RuntimeClassName: podmanager.RuntimeClassRunsc}
	if _, resumePath, err := ft.thaw(context.Background(), spec, "gd-bead"); err == nil {
		t.Fatal("expected a thaw failure on the create path (waitPodReady times out)")
	} else if resumePath != gardeniometrics.ResumePathThawFresh {
		t.Errorf("warm miss must fall back to thaw-fresh path, got %q", resumePath)
	}
	// Create-path Pod B is "<snap.PodName>-thaw-b": its creation proves we did
	// NOT graft a warm pod and instead fell back to create-then-restore. We
	// assert on CREATION (not post-thaw presence) because the create-path
	// teardown defer deletes Pod B on the barrier-restore failure by design
	// (createPathPodBTeardown, ka-0000005365). The off-node warm placeholder
	// must remain Ready (never claimed).
	if !*createdPodB {
		t.Fatalf("create-path Pod B %q was not created — warm miss did not fall back", agent+"-thaw-b")
	}
	other, _ := client.CoreV1().Pods(ns).Get(context.Background(), "warm-b-other-1", metav1.GetOptions{})
	if other.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderReady {
		t.Errorf("off-node warm placeholder must stay Ready, got %q", other.Labels[podmanager.LabelWarmPlaceholder])
	}
}

func TestThaw_NoPoolUsesCreatePath(t *testing.T) {
	ns, agent, node := "gvisor", "agentsim", "node-a"
	client := fake.NewClientset(readyWarmPlaceholder("warm-b-node-a-1", ns, node))
	createdPodB := trackPodBCreate(client, agent+"-thaw-b")
	ft := newWarmWakeLifecycle(t, client, ns, agent, node, false) // pool NOT wired

	spec := podmanager.AgentPodSpec{AgentName: agent, Project: "gardenio", Mode: "crew", Role: "crew",
		RuntimeClassName: podmanager.RuntimeClassRunsc}
	if _, resumePath, err := ft.thaw(context.Background(), spec, "gd-bead"); err == nil {
		t.Fatal("expected a thaw failure on the create path (waitPodReady times out)")
	} else if resumePath != gardeniometrics.ResumePathThawFresh {
		t.Errorf("no-pool thaw must report thaw-fresh path, got %q", resumePath)
	}
	// No pool wired → create-path Pod B "<agent>-thaw-b" must be created. Assert
	// on CREATION (not post-thaw presence): the create-path teardown defer
	// deletes Pod B on the barrier-restore failure by design
	// (createPathPodBTeardown, ka-0000005365).
	if !*createdPodB {
		t.Fatalf("no-pool thaw must create %q", agent+"-thaw-b")
	}
	// The pre-existing warm placeholder must be untouched (not promoted).
	got, _ := client.CoreV1().Pods(ns).Get(context.Background(), "warm-b-node-a-1", metav1.GetOptions{})
	if got.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderReady {
		t.Errorf("warm pod must remain Ready when no pool is wired; label=%q", got.Labels[podmanager.LabelWarmPlaceholder])
	}
}

// --- retry-once-then-cold-start (bug ka-0000005365 step 4) -------------------

// countingNodeExec returns a scripted stdout/exit per attempt and counts calls,
// so the retry tests can both observe HOW MANY thaw attempts ran and inject a
// retryable-barrier failure then a different result on the retry.
type countingNodeExec struct {
	calls   int
	results []podmanager.NodeExecResult
}

func (c *countingNodeExec) Exec(context.Context, string, string) (podmanager.NodeExecResult, error) {
	i := c.calls
	c.calls++
	if i < len(c.results) {
		return c.results[i], nil
	}
	// After the scripted results are exhausted, keep returning the last one.
	return c.results[len(c.results)-1], nil
}

// retryableBarrierStdout makes mgr.Thaw return a *ThawBarrierError whose
// Retryable() is true: the agent SUB restore failed (FT_SUB_RC=1) while the
// pause sandbox restored clean (FT_ROOT_RC=0) — the /sys-walk class.
const retryableBarrierStdout = "FT_IMAGE_OK=yes\nFT_ROOT_RC=0\nFT_SUB_RC=1\n"

// newRetryLifecycle builds a create-path lifecycle (no pool) wired with a
// counting node-exec and a reactor that makes every created Pod B Ready, so the
// create path reaches the barrier restore on each attempt.
func newRetryLifecycle(t *testing.T, exec podmanager.NodeExecutor) (*freezeThawLifecycle, *fake.Clientset) {
	t.Helper()
	ns, agent, node := "gvisor", "agentsim", "node-a"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = "b-uid"
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		return false, pod, nil
	})
	store := podmanager.NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), podmanager.Snapshot{
		AgentName: agent, Namespace: ns, PodName: agent, Node: node,
		ImagePath: "/x", BundlePath: "/y", SandboxCID: "s", AgentCID: "a", PodUID: "u", ContainerCount: 2,
	})
	mgr := podmanager.NewFreezeThawManager(client, exec, store, nil, discardLogger())
	mgr.PodReadyTimeout = 500 * time.Millisecond
	mgr.PodReadyPollInterval = time.Millisecond
	// The ka-0000000088 pre-thaw memory gate adds a node exec before the
	// barrier restore; disable it here so these tests' countingNodeExec.calls
	// counter reflects barrier-restore invocations only (the invariant they
	// assert: "retryable barrier ⇒ 2 attempts, non-retryable ⇒ 1 attempt").
	mgr.MemGateDisabled = true
	return &freezeThawLifecycle{mgr: mgr, logger: discardLogger()}, client
}

// TestThaw_BarrierFailureDoesNotRetry: a retryable barrier failure (sub rc!=0,
// root rc==0) gets exactly one thaw attempt, then the caller cold-starts.
func TestThaw_BarrierFailureDoesNotRetry(t *testing.T) {
	exec := &countingNodeExec{results: []podmanager.NodeExecResult{{Stdout: retryableBarrierStdout}}}
	ft, _ := newRetryLifecycle(t, exec)
	spec := podmanager.AgentPodSpec{AgentName: "agentsim", Project: "gardenio", Mode: "crew", Role: "crew",
		RuntimeClassName: podmanager.RuntimeClassRunsc}
	if _, _, err := ft.thaw(context.Background(), spec, "gd-bead"); err == nil {
		t.Fatal("expected a barrier error")
	}
	if exec.calls != 1 {
		t.Errorf("a barrier failure must drive exactly 1 thaw attempt, got %d", exec.calls)
	}
}
