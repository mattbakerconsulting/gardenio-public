package podmanager

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// freezeStdoutWithPauseRC returns a freeze-OK node stdout with FT_PAUSE_RC set to
// the given value. It reuses the canonical freezeOKStdout fixture (which passes
// every post-state assertion) and appends the pause sentinel so the round-trip
// exercises the real parse + log path.
func freezeStdoutWithPauseRC(rc string) string {
	return freezeOKStdout + "FT_PAUSE_RC=" + rc + "\n"
}

// bufLoggerManager builds a freeze/thaw manager whose logger writes to buf, so a
// test can assert on the post-state log line. Mirrors newFreezeThawManager's
// tight-timer setup but keeps the (info-level) logger instead of the error-only
// testLogger, so freeze: snapshot persisted (an Info line) is captured.
func bufLoggerManager(t *testing.T, buf *bytes.Buffer, client *fake.Clientset, exec NodeExecutor, snaps SnapshotStore) *FreezeThawManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m := NewFreezeThawManager(client, exec, snaps, nil, logger)
	m.PodReadyTimeout = 2 * time.Second
	m.PodReadyPollInterval = time.Millisecond
	m.MemGateDisabled = true
	return m
}

// TestFreeze_PauseRCRoundTripsCleanPause asserts FIX 3 (task ka-0000006783):
// FT_PAUSE_RC=0 (the sentry was cleanly paused for the consistent-snapshot window)
// round-trips into the "freeze: snapshot persisted" post-state log as pauseRC=0 /
// pauseAtomic=true, and does NOT emit the degraded WARN. Log-only — the freeze
// still succeeds and persists a snapshot exactly as before.
func TestFreeze_PauseRCRoundTripsCleanPause(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPauseRC("0")}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap == nil {
		t.Fatal("expected a persisted snapshot on a clean freeze")
	}
	out := buf.String()
	if !strings.Contains(out, "freeze: snapshot persisted") {
		t.Fatalf("post-state persisted log missing, got:\n%s", out)
	}
	if !strings.Contains(out, "pauseRC=0") {
		t.Errorf("clean pause must log pauseRC=0, got:\n%s", out)
	}
	if !strings.Contains(out, "pauseAtomic=true") {
		t.Errorf("clean pause must log pauseAtomic=true, got:\n%s", out)
	}
	if strings.Contains(out, "consistent-snapshot pause degraded") {
		t.Errorf("clean pause must NOT emit the degraded WARN, got:\n%s", out)
	}
}

// TestFreeze_PauseRCRoundTripsDegraded asserts FIX 3's degraded path: a non-zero
// FT_PAUSE_RC (runsc pause failed; the payload copy ran UNPAUSED, non-atomic)
// round-trips as pauseRC=<rc> / pauseAtomic=false AND emits the degraded WARN.
// Critically, the freeze is NOT gated: it still succeeds and persists a snapshot,
// because a lost freeze is strictly worse than a degraded-but-usable one (the
// review explicitly flagged not gating on it).
func TestFreeze_PauseRCRoundTripsDegraded(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPauseRC("1")}}
	var buf bytes.Buffer
	store := NewInMemorySnapshotStore()
	m := bufLoggerManager(t, &buf, client, exec, store)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	// Log-only: a degraded pause must NOT fail the freeze.
	if err != nil {
		t.Fatalf("degraded pause must not gate the freeze, got err: %v", err)
	}
	if snap == nil {
		t.Fatal("degraded pause must still persist a (usable) snapshot")
	}
	if got, err := store.Get(context.Background(), "agentsim"); err != nil || got.SandboxCID == "" {
		t.Fatalf("snapshot not persisted on degraded pause: got=%+v err=%v", got, err)
	}
	out := buf.String()
	if !strings.Contains(out, "pauseRC=1") {
		t.Errorf("degraded pause must log pauseRC=1, got:\n%s", out)
	}
	if !strings.Contains(out, "pauseAtomic=false") {
		t.Errorf("degraded pause must log pauseAtomic=false, got:\n%s", out)
	}
	if !strings.Contains(out, "consistent-snapshot pause degraded") {
		t.Errorf("degraded pause must emit the WARN, got:\n%s", out)
	}
}

// TestFreeze_PauseRCMissingIsUnknownNotClean asserts the older-node-script case:
// a freeze stdout with NO FT_PAUSE_RC sentinel must report pauseRC=-1 /
// pauseAtomic=false, so "unknown" never masquerades as a clean, atomic pause.
func TestFreeze_PauseRCMissingIsUnknownNotClean(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	// Canonical fixture carries no FT_PAUSE_RC.
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	if _, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	}); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "pauseRC=-1") {
		t.Errorf("missing FT_PAUSE_RC must log pauseRC=-1 (unknown), got:\n%s", out)
	}
	if !strings.Contains(out, "pauseAtomic=false") {
		t.Errorf("missing FT_PAUSE_RC must log pauseAtomic=false (not clean), got:\n%s", out)
	}
	if strings.Contains(out, "consistent-snapshot pause degraded") {
		t.Errorf("missing sentinel is unknown, not degraded — must not WARN, got:\n%s", out)
	}
}
