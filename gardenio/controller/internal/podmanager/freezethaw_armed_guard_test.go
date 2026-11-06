package podmanager

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/thawepoch"
)

// armedAwareExec branches on the script shape: the pre-freeze armed probe (which
// contains "FT_ARMED=" echo lines) returns a stdout listing the consumers in
// `present`; the checkpoint exec returns freezeOKStdout. This lets one fake
// serve both the guard probe and the checkpoint in a single Freeze call.
type armedAwareExec struct {
	present   []string // consumers whose sentinel "exists" on the node
	probeErr  error    // if set, the armed probe exec fails (fail-safe path)
	probeRC   int      // if non-zero, the armed probe exits non-zero (fail-safe path)
	sawProbe  bool
	sawFreeze bool
}

func (e *armedAwareExec) Exec(_ context.Context, _ string, script string) (NodeExecResult, error) {
	if strings.Contains(script, "FT_ARMED=") {
		e.sawProbe = true
		if e.probeErr != nil {
			return NodeExecResult{}, e.probeErr
		}
		if e.probeRC != 0 {
			return NodeExecResult{ExitCode: e.probeRC, Stderr: "probe boom"}, nil
		}
		var b strings.Builder
		for _, c := range e.present {
			b.WriteString("FT_ARMED=" + c + "\n")
		}
		return NodeExecResult{Stdout: b.String()}, nil
	}
	e.sawFreeze = true
	return NodeExecResult{Stdout: freezeOKStdout}, nil
}

func newArmedGuardManager(t *testing.T, client *fake.Clientset, exec NodeExecutor) *FreezeThawManager {
	t.Helper()
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil)
	m.ArmedGuardEnabled = true
	return m
}

// Both watchers armed → freeze proceeds and persists a snapshot (ka-0000006667).
func TestFreeze_ArmedGuard_AllArmed_Proceeds(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &armedAwareExec{present: []string{thawepoch.ConsumerCoop, thawepoch.ConsumerGB}}
	m := newArmedGuardManager(t, client, exec)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap == nil || snap.SandboxCID != "sandboxcid222" {
		t.Fatalf("expected persisted snapshot, got %+v", snap)
	}
	if !exec.sawProbe || !exec.sawFreeze {
		t.Fatalf("expected both armed probe and checkpoint to run (probe=%v freeze=%v)", exec.sawProbe, exec.sawFreeze)
	}
}

// One watcher not armed → freeze is refused with ErrFreezeWatchersNotArmed and
// the checkpoint never runs (no wasted 3.0G image; ka-0000006667).
func TestFreeze_ArmedGuard_PartialArmed_Refuses(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &armedAwareExec{present: []string{thawepoch.ConsumerCoop}} // gb missing
	m := newArmedGuardManager(t, client, exec)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if !errors.Is(err, ErrFreezeWatchersNotArmed) {
		t.Fatalf("want ErrFreezeWatchersNotArmed, got %v", err)
	}
	if snap != nil {
		t.Fatalf("expected no snapshot on refusal, got %+v", snap)
	}
	if exec.sawFreeze {
		t.Fatal("checkpoint must not run when watchers are not armed")
	}
}

// No watchers armed → refused (ka-0000006667).
func TestFreeze_ArmedGuard_NoneArmed_Refuses(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &armedAwareExec{present: nil}
	m := newArmedGuardManager(t, client, exec)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if !errors.Is(err, ErrFreezeWatchersNotArmed) {
		t.Fatalf("want ErrFreezeWatchersNotArmed, got %v", err)
	}
	if exec.sawFreeze {
		t.Fatal("checkpoint must not run when no watchers are armed")
	}
}

// Guard disabled (default) → the armed probe never runs and freeze proceeds even
// with zero sentinels: zero blast radius for the Phase-4 behaviour (ka-0000006667).
func TestFreeze_ArmedGuard_Disabled_SkipsProbe(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &armedAwareExec{present: nil}
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil) // ArmedGuardEnabled stays false

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err != nil {
		t.Fatalf("Freeze (guard off): %v", err)
	}
	if snap == nil {
		t.Fatal("expected snapshot with guard disabled")
	}
	if exec.sawProbe {
		t.Fatal("armed probe must not run when guard is disabled")
	}
}

// A node-probe error is FAIL-SAFE: the freeze proceeds (never silently disable
// checkpointing on a flaky probe; ka-0000006667).
func TestFreeze_ArmedGuard_ProbeError_FailsSafe(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &armedAwareExec{probeErr: errors.New("node unreachable")}
	m := newArmedGuardManager(t, client, exec)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err != nil {
		t.Fatalf("expected fail-safe proceed, got %v", err)
	}
	if snap == nil || !exec.sawFreeze {
		t.Fatalf("expected checkpoint to run on fail-safe (snap=%v freeze=%v)", snap != nil, exec.sawFreeze)
	}
}

// A non-zero probe rc is likewise fail-safe (ka-0000006667).
func TestFreeze_ArmedGuard_ProbeNonZeroRC_FailsSafe(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &armedAwareExec{probeRC: 2}
	m := newArmedGuardManager(t, client, exec)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err != nil {
		t.Fatalf("expected fail-safe proceed on probe rc, got %v", err)
	}
	if snap == nil || !exec.sawFreeze {
		t.Fatalf("expected checkpoint to run on fail-safe rc (snap=%v freeze=%v)", snap != nil, exec.sawFreeze)
	}
}

// delayedArmExec is an armed-probe fake whose sentinels become present only
// AFTER `armAfterProbes` probe round-trips — modelling a slow-booting agent loop
// that arms its watchers shortly after the freeze is requested. Probe/freeze
// bookkeeping is mutex-guarded so the wait loop and assertions race-cleanly.
type delayedArmExec struct {
	mu             sync.Mutex
	all            []string // consumers that eventually arm
	armAfterProbes int      // number of probes before `all` becomes present
	probes         int
	sawFreeze      bool
}

func (e *delayedArmExec) Exec(_ context.Context, _ string, script string) (NodeExecResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.Contains(script, "FT_ARMED=") {
		e.probes++
		var b strings.Builder
		if e.probes > e.armAfterProbes {
			for _, c := range e.all {
				b.WriteString("FT_ARMED=")
				b.WriteString(c)
				b.WriteString("\n")
			}
		}
		return NodeExecResult{Stdout: b.String()}, nil
	}
	e.sawFreeze = true
	return NodeExecResult{Stdout: freezeOKStdout}, nil
}

func (e *delayedArmExec) probeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.probes
}

// ArmedWait > 0 and the watchers arm within the window → Freeze WAITS, then
// proceeds and persists a thawable snapshot (direction [A]). This is the core
// of the fix: a graceful stop arriving before the agent loop armed no longer
// cold-starts — it waits out the boot (ka-0000006667).
func TestFreeze_ArmedGuard_WaitsAndProceedsWhenWatchersArmInWindow(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &delayedArmExec{
		all:            []string{thawepoch.ConsumerCoop, thawepoch.ConsumerGB},
		armAfterProbes: 2, // armed from the 3rd probe onward
	}
	m := newArmedGuardManager(t, client, exec)
	m.ArmedWait = 5 * time.Second
	m.ArmedPollInterval = 5 * time.Millisecond

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: expected success after waiting for arm, got %v", err)
	}
	if snap == nil {
		t.Fatal("expected persisted snapshot after watchers armed within wait window")
	}
	if !exec.sawFreeze {
		t.Fatal("checkpoint must run once watchers arm within the wait window")
	}
	if got := exec.probeCount(); got < 3 {
		t.Fatalf("expected at least 3 probes (initial + waits until arm), got %d", got)
	}
}

// ArmedWait > 0 but the watchers NEVER arm within the window → Freeze refuses
// with ErrFreezeWatchersNotArmed (same outcome as the single-shot probe, just
// after waiting out the bounded window). No un-thawable snapshot is minted (ka-0000006667).
func TestFreeze_ArmedGuard_WaitTimesOut_Refuses(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &delayedArmExec{
		all:            []string{thawepoch.ConsumerCoop, thawepoch.ConsumerGB},
		armAfterProbes: 1 << 30, // effectively never arms
	}
	m := newArmedGuardManager(t, client, exec)
	m.ArmedWait = 40 * time.Millisecond
	m.ArmedPollInterval = 5 * time.Millisecond

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if !errors.Is(err, ErrFreezeWatchersNotArmed) {
		t.Fatalf("want ErrFreezeWatchersNotArmed after wait timeout, got %v", err)
	}
	if snap != nil {
		t.Fatalf("expected no snapshot when wait times out, got %+v", snap)
	}
	if exec.sawFreeze {
		t.Fatal("checkpoint must not run when the wait window elapses without arming")
	}
	if got := exec.probeCount(); got < 2 {
		t.Fatalf("expected the guard to re-probe during the wait, got only %d probe(s)", got)
	}
}

// ArmedWait > 0 but the watchers are ALREADY armed at request time → no wait,
// freeze proceeds immediately on the first probe (the wait is only paid when the
// initial probe misses; ka-0000006667).
func TestFreeze_ArmedGuard_WaitSet_AlreadyArmed_ProceedsImmediately(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &delayedArmExec{
		all:            []string{thawepoch.ConsumerCoop, thawepoch.ConsumerGB},
		armAfterProbes: 0, // armed from the first probe
	}
	m := newArmedGuardManager(t, client, exec)
	m.ArmedWait = 10 * time.Second // a long window we must NOT pay
	m.ArmedPollInterval = 5 * time.Millisecond

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap == nil || !exec.sawFreeze {
		t.Fatalf("expected immediate freeze when already armed (snap=%v freeze=%v)", snap != nil, exec.sawFreeze)
	}
	if got := exec.probeCount(); got != 1 {
		t.Fatalf("expected exactly 1 probe when already armed (no wait), got %d", got)
	}
}

// A cancelled context ends the wait promptly and refuses (the guard never
// outlives the freeze's bounded context; ka-0000006667).
func TestFreeze_ArmedGuard_WaitHonoursContextCancel(t *testing.T) {
	m := newArmedGuardManager(t, fake.NewClientset(), &delayedArmExec{
		all:            []string{thawepoch.ConsumerCoop, thawepoch.ConsumerGB},
		armAfterProbes: 1 << 30, // never arms
	})
	m.ArmedWait = 10 * time.Second
	m.ArmedPollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	armed, why := m.watchersArmedWithin(ctx, "node-1", "agentsim", m.ArmedWait)
	if armed {
		t.Fatalf("expected not-armed on a cancelled context, got armed (%s)", why)
	}
	if !strings.Contains(why, "context ended") {
		t.Fatalf("expected a context-ended reason, got %q", why)
	}
}
