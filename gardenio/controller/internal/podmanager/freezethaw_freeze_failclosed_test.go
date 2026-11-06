package podmanager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// freezethaw_freeze_failclosed_test.go — RED-BAR guards for the freeze-side
// snapshot_persisted fail-closed fix (bug ka-0000004931). The freeze exec fused
// timeout-bounded runsc ops with UNBOUNDED host-ns ops on the ~3.0G checkpoint
// image; a wedged host-ns op hung the freeze until it was killed and the sentinel
// stream TRUNCATED (FT_IMAGE_PRESENT empty / FT_CHECKPOINT_RC absent), so a hang
// mislooked like image-absent. These tests fail if either fix is reverted:
//
//   Fix A (HMNTT): the read-only image probes are bounded, so a wedged probe
//     fails fast into the EXISTING fail-closed marker (FT_IMAGE_PRESENT=no) and
//     the script runs to completion — proven behaviorally below.
//   Fix C (freezeHardDeadline): the whole freeze node exec is wall-clock bounded,
//     so a wedge in an UNBOUNDED mutating node op makes Freeze() fail closed
//     (no snapshot persisted) instead of hanging the controller goroutine.

// lineContaining returns the single rendered-script line that contains sub, or
// fails the test. Used to extract a REAL probe line from the freeze template so
// the behavioral test exercises exactly the wiring that ships (a revert of the
// probe from HMNTT back to HMNT changes the extracted line, flipping this test
// RED via the hang path below).
func lineContaining(t *testing.T, script, sub string) string {
	t.Helper()
	var hit string
	for _, ln := range strings.Split(script, "\n") {
		if strings.Contains(ln, sub) {
			if hit != "" {
				t.Fatalf("expected exactly one freeze-script line containing %q; found >1", sub)
			}
			hit = ln
		}
	}
	if hit == "" {
		t.Fatalf("no freeze-script line contains %q — the probe moved/renamed; update this test (do NOT let it silently no-op)", sub)
	}
	return hit
}

// wedgeBinDir writes a directory of executables that stand in for the host-ns
// image probes and all WEDGE — each ignores its args and blocks far longer than
// the test tolerates, modelling the D-state stat()/read()/execve on the ~3.0G
// image mount that hung the real freeze. Every command the probe lines actually
// invoke through the helpers (test, du, stat, bash, strings, plus the template's
// log/cut helpers used on the same line) resolves here, so the extracted probe
// line runs UNCHANGED — the helper (HMNT vs HMNTT) is the only variable. Returned
// so PATH can be pointed at it.
//
// This is deliberately a REAL on-disk wedge (not a shell function): timeout(1)
// execs its argument via execvp, exactly as the shipping HMNTT does around the
// real nsenter — a function would be invisible to it and would not model the fix.
func wedgeBinDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// The wedging probe commands: each blocks. `sleep 3600` >> any test ceiling.
	for _, name := range []string{"test", "du", "stat", "strings"} {
		mustWriteExec(t, filepath.Join(dir, name), "#!/bin/sh\nexec sleep 3600\n")
	}
	// The template lines also pipe through `cut` and (on the else path) call `log`.
	// These must NOT wedge — only the host-ns access does — so give them trivial
	// real implementations. `bash` (used by the strings probe via `HMNTT bash -c`)
	// must be a REAL bash so the -c wedges only inside, matching the template.
	mustWriteExec(t, filepath.Join(dir, "log"), "#!/bin/sh\n:\n")
	return dir
}

func mustWriteExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// runWedgedProbe runs ONE extracted image-probe line with the probe commands
// shadowed by the wedging shims (PATH-prepended), and returns (stdout,
// wallElapsed, timedOut). HMNT (the pre-fix helper) invokes the wedge UNBOUNDED;
// HMNTT (the Fix-A helper) wraps it in the same RUNSC_TIMEOUT KILL-signal
// discipline the real template applies (minus the un-testable, root-only
// `nsenter -t 1 -m` host-ns entry — that is not the wedge; the timeout wrapper IS
// the fix). Whichever helper the template's probe line names decides the outcome:
//   - fixed  (HMNTT ...): timeout SIGKILLs the wedge at RUNSC_TIMEOUT, the probe
//     returns non-zero, the else-branch emits the fail-closed marker, line ends
//     in ~RUNSC_TIMEOUT.
//   - reverted (HMNT ...): the wedge runs unbounded; only the outer ctx ceiling
//     ends the whole bash — timedOut=true, the RED signal.
func runWedgedProbe(t *testing.T, probeLine string) (stdout string, elapsed time.Duration, timedOut bool) {
	t.Helper()
	bin := wedgeBinDir(t)
	// RUNSC_TIMEOUT=1s so the Fix-A path fails fast; the shims sleep 3600s so the
	// unbounded (reverted) path cannot finish on its own — only the ctx ceiling can
	// end it, which is exactly the hang being guarded against. The 20s ceiling is
	// >> 1s (fixed path finishes) and << 3600s (reverted path would otherwise hang).
	prelude := `set -uo pipefail
export PATH="` + bin + `:$PATH"
RUNSC_TIMEOUT=1
IMG=/gardenio-test/wedged-image-dir
# HMNT: the pre-fix UNBOUNDED host-ns helper. env execvp's the probe command
# (like the real nsenter -t 1 -m -- CMD), so test/stat/strings resolve to the
# wedging PATH shims rather than the shell builtins — a wedge here hangs forever,
# exactly the pre-fix truncation vector.
HMNT()  { env "$@"; }
# HMNTT: the Fix-A bounded helper — the SAME RUNSC_TIMEOUT KILL-signal discipline
# the real template applies, minus the un-testable nsenter -t 1 host-ns entry.
# timeout(1) execvp's its argument, so the probe command resolves to the wedging
# PATH shim just as under HMNT — the timeout wrapper is the ONLY difference.
HMNTT() { timeout -s KILL -k 5s "${RUNSC_TIMEOUT}" "$@"; }
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, "bash", "-c", prelude+probeLine)
	out, _ := cmd.CombinedOutput()
	elapsed = time.Since(start)
	// ctx deadline exceeded => the probe hung past the ceiling (the reverted path).
	return string(out), elapsed, ctx.Err() == context.DeadlineExceeded
}

// TestFreezeProbe_WedgedImageProbeFailsClosed is the Fix-A behavioral red-bar. It
// extracts the REAL FT_IMAGE_PRESENT and FT_IMAGE_OK probe lines from the rendered
// freeze template and proves that, when the host-ns access wedges, each probe
// fails FAST into its fail-closed marker (=no) and the line completes — instead of
// hanging until a killer truncates the stream. Revert either probe from HMNTT to
// the unbounded HMNT and the wedge is no longer bounded: the line hangs past the
// 20s ceiling, timedOut=true, and this test fails.
func TestFreezeProbe_WedgedImageProbeFailsClosed(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	fs := m.buildFreezeScript(freezeScriptParams{
		PodName: "p", RunscTimeout: 120,
		ImagePath: "/var/lib/gardenio/ckpt/ag", BundlePath: "/var/lib/gardenio/bundle/ag",
	})

	cases := []struct {
		name       string
		probeMark  string // unique substring identifying the probe line
		emitMarker string // appended so the resolved fail-closed value is observable
		wantMarker string // the fail-closed sentinel the wedged probe must still emit
	}{
		{
			name: "FT_IMAGE_PRESENT probe (checkpoint-stage, node ~597)",
			// This line echoes the sentinel inline, so no extra emit is needed.
			probeMark:  `then echo "FT_IMAGE_PRESENT=yes"`,
			emitMarker: "",
			wantMarker: "FT_IMAGE_PRESENT=no",
		},
		{
			name: "FT_IMAGE_OK guard probe ([3d/8], node ~945 — gates snapshot persistence)",
			// This line only ASSIGNS IMAGE_OK; the template echoes it on the NEXT line
			// (FT_IMAGE_OK=$IMAGE_OK). Append that echo so the wedged fail-closed value
			// (IMAGE_OK=no) is observable — the exact FT_IMAGE_OK=no the Go post-state
			// check fail-closes on.
			probeMark:  `IMAGE_OK=yes; else IMAGE_OK=no`,
			emitMarker: `; echo "FT_IMAGE_OK=$IMAGE_OK"`,
			wantMarker: "FT_IMAGE_OK=no",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := lineContaining(t, fs, c.probeMark)
			// Precondition: the shipping probe MUST be on the bounded helper. This is
			// the wiring Fix A guarantees; if it regresses to HMNT the behavioral hang
			// below would also catch it, but assert it directly for a crisp failure.
			if !strings.Contains(line, "HMNTT ") {
				t.Fatalf("probe line is not on the bounded HMNTT helper (Fix A reverted?):\n  %s", line)
			}
			out, elapsed, timedOut := runWedgedProbe(t, line+c.emitMarker)
			if timedOut {
				t.Fatalf("wedged probe HUNG past the ceiling — the read-only probe is not timeout-bounded (Fix A reverted). elapsed=%v out=%q", elapsed, out)
			}
			if !strings.Contains(out, c.wantMarker) {
				t.Fatalf("wedged probe did not fail closed: want marker %q, got out=%q (elapsed=%v)", c.wantMarker, out, elapsed)
			}
			// The fixed path fails fast at ~RUNSC_TIMEOUT (1s). A generous 15s bound
			// still separates it unmistakably from the 3600s unbounded wedge, so the
			// test is not timing-flaky yet still proves the timeout fired.
			if elapsed > 15*time.Second {
				t.Fatalf("wedged probe fail-closed but too slowly (elapsed=%v) — the timeout budget is not bounding the probe", elapsed)
			}
		})
	}
}

// blockingUntilCtx blocks inside Exec until its ctx is cancelled (the ONLY way it
// ever returns), recording that it was entered. It models a freeze node exec
// wedged in an UNBOUNDED mutating host-ns op (the relocation cp / pre-checkpoint
// gofer copy) that no in-script timeout bounds — so only a Go-side wall-clock cap
// (Fix C, freezeHardDeadline) can end it.
type blockingUntilCtx struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingUntilCtx) Exec(ctx context.Context, _ string, _ string) (NodeExecResult, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done() // never returns until the ctx is cancelled/deadlined
	return NodeExecResult{}, ctx.Err()
}

// TestFreeze_HardDeadlineFailsClosedOnWedgedExec is the Fix-C red-bar. A freeze
// node exec that wedges (never returns on its own) must be ended by the freeze
// wall-clock hard deadline: Freeze() returns an error, NO snapshot is persisted,
// and it returns promptly (bounded by FreezeHardDeadline). Revert Fix C (drop the
// WithTimeout(cctx, freezeHardDeadline()) around the exec) and the exec ctx never
// deadlines: Freeze hangs forever and this test fails on the outer time.After.
func TestFreeze_HardDeadlineFailsClosedOnWedgedExec(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &blockingUntilCtx{entered: make(chan struct{})}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)
	// A tight hard deadline so the test is fast; the default (runscTimeout +
	// relocation margin = minutes) is far too long for a unit test. Setting the
	// field also exercises the FreezeHardDeadline knob + freezeHardDeadline().
	m.FreezeHardDeadline = 150 * time.Millisecond

	type result struct {
		snap *Snapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, _, err := m.Freeze(context.Background(), FreezeRequest{
			AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
		})
		done <- result{snap, err}
	}()

	// The exec must be entered (freeze reached the node op) so we know the deadline,
	// not some earlier guard, is what ends the freeze.
	select {
	case <-exec.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("freeze never reached the node exec")
	}

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("Freeze returned nil error on a wedged exec; want a hard-deadline failure")
		}
		// The wedge ends via ctx deadline; the error must NOT be ErrFreezeAborted
		// (no abort fired) — a pure timeout, so the caller cold-starts.
		if errors.Is(r.err, ErrFreezeAborted) {
			t.Fatalf("Freeze err = ErrFreezeAborted on a pure hard-deadline timeout; want a non-abort exec error, got %v", r.err)
		}
		if !strings.Contains(r.err.Error(), "node exec") {
			t.Errorf("Freeze err = %v, want the node-exec failure wrapper", r.err)
		}
		if r.snap != nil {
			t.Errorf("snap = %+v, want nil — a wedged/hard-deadlined freeze must persist NO snapshot", r.snap)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Freeze did not return after the 150ms hard deadline — the freeze exec is not wall-clock bounded (Fix C reverted?)")
	}

	if _, err := store.Get(context.Background(), "agentsim"); err == nil {
		t.Errorf("a snapshot was persisted despite the wedged/hard-deadlined freeze; want none (fail-closed)")
	}
}

// TestFreezeHardDeadline_DefaultAndOverride locks the freezeHardDeadline()
// baseline derivation: unset defers to runscTimeout + the baseline (2-container)
// relocation margin (never truncates a healthy freeze), and an explicit field
// wins. A revert of the field wiring or the default computation flips this RED.
func TestFreezeHardDeadline_DefaultAndOverride(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	// Baseline default: runscTimeout (120s) + freezeRelocationMargin (120s) = 240s.
	// It MUST strictly exceed one runsc-checkpoint budget so the cap never fires on
	// a healthy freeze whose single bounded runsc op runs to its own timeout.
	if got, min := m.freezeHardDeadline(), m.runscTimeout(); got <= min {
		t.Errorf("default freezeHardDeadline=%v must exceed runscTimeout=%v (else it truncates a healthy freeze)", got, min)
	}
	if want := DefaultRunscTimeout + freezeRelocationMargin; m.freezeHardDeadline() != want {
		t.Errorf("default freezeHardDeadline=%v, want %v (runscTimeout + baseline relocation margin)", m.freezeHardDeadline(), want)
	}
	// The baseline wrapper MUST equal the count-aware getter at the baseline count —
	// they are the same cap for a bare agent (no behavior change for count<=2 pods).
	if got, want := m.freezeHardDeadline(), m.freezeHardDeadlineFor(freezeBaselineContainerCount); got != want {
		t.Errorf("freezeHardDeadline()=%v must equal freezeHardDeadlineFor(baseline)=%v", got, want)
	}
	// Explicit override wins (count-independent).
	m.FreezeHardDeadline = 42 * time.Second
	if got := m.freezeHardDeadline(); got != 42*time.Second {
		t.Errorf("freezeHardDeadline with field set = %v, want 42s", got)
	}
}

// TestFreezeHardDeadlineFor_ScalesWithContainerCount locks the size-aware
// derivation (ka-0000007927): the computed freeze cap grows per checkpointed
// container so multi-sidecar agents get proportional copy headroom, a below-
// baseline count is floored (never truncates), and the operator override wins
// unconditionally regardless of count. Revert the scaling and the curve flips RED.
func TestFreezeHardDeadlineFor_ScalesWithContainerCount(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	base := m.runscTimeout() // 120s
	cases := []struct {
		count int
		want  time.Duration
	}{
		{count: 0, want: base + freezeRelocationPerContainerMargin*freezeBaselineContainerCount},  // floored to baseline (240s)
		{count: 1, want: base + freezeRelocationPerContainerMargin*freezeBaselineContainerCount},  // floored to baseline (240s)
		{count: 2, want: base + freezeRelocationPerContainerMargin*2},                             // bare agent: pause+agent (240s)
		{count: 3, want: base + freezeRelocationPerContainerMargin*3},                             // +1 sidecar (300s)
		{count: 4, want: base + freezeRelocationPerContainerMargin*4},                             // DinD+Postgres (360s)
	}
	for _, tc := range cases {
		if got := m.freezeHardDeadlineFor(tc.count); got != tc.want {
			t.Errorf("freezeHardDeadlineFor(%d)=%v, want %v", tc.count, got, tc.want)
		}
	}
	// Monotonic: more containers is never LESS headroom.
	if m.freezeHardDeadlineFor(4) <= m.freezeHardDeadlineFor(2) {
		t.Errorf("a 4-container agent must get MORE headroom than a bare agent (%v vs %v)",
			m.freezeHardDeadlineFor(4), m.freezeHardDeadlineFor(2))
	}
	// A heavy pod must clear the ~170s single-clone data point (ka-0000005425) with
	// real margin — the whole point of the fix.
	if got := m.freezeHardDeadlineFor(4); got < 300*time.Second {
		t.Errorf("freezeHardDeadlineFor(4)=%v, want >=300s of headroom for a multi-sidecar agent", got)
	}
	// Operator override wins for ANY count.
	m.FreezeHardDeadline = 42 * time.Second
	for _, count := range []int{2, 4, 8} {
		if got := m.freezeHardDeadlineFor(count); got != 42*time.Second {
			t.Errorf("override should win regardless of count; freezeHardDeadlineFor(%d)=%v, want 42s", count, got)
		}
	}
}

// TestEstimateFreezeContainerCount checks the pre-exec pod-shape estimate that
// sizes the freeze deadline: pause(1) + app containers + always-on sidecars, with
// run-to-completion init-clone excluded and a bare/empty spec floored to baseline.
func TestEstimateFreezeContainerCount(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	tests := []struct {
		name string
		spec corev1.PodSpec
		want int
	}{
		{
			name: "empty spec floors to baseline",
			spec: corev1.PodSpec{},
			want: freezeBaselineContainerCount,
		},
		{
			name: "bare agent = pause + agent",
			spec: corev1.PodSpec{
				Containers:     []corev1.Container{{Name: ContainerName}},
				InitContainers: []corev1.Container{{Name: InitCloneName}}, // run-to-completion: excluded
			},
			want: 2,
		},
		{
			name: "dind+postgres agent = pause + agent + 2 sidecars",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: ContainerName}},
				InitContainers: []corev1.Container{
					{Name: InitCloneName},                                    // excluded
					{Name: DindContainerName, RestartPolicy: &always},        // sidecar
					{Name: PostgresContainerName, RestartPolicy: &always},    // sidecar
				},
			},
			want: 4,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateFreezeContainerCount(tc.spec); got != tc.want {
				t.Errorf("estimateFreezeContainerCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// deadlineCapturingExec records the ctx deadline the freeze exec was invoked with,
// then returns a canned OK result. It lets a test observe the wall-clock budget the
// Freeze path threaded into the node exec ctx without running a real checkpoint.
type deadlineCapturingExec struct {
	stdout   string
	deadline time.Time
	hadDL    bool
}

func (e *deadlineCapturingExec) Exec(ctx context.Context, _ string, _ string) (NodeExecResult, error) {
	e.deadline, e.hadDL = ctx.Deadline()
	return NodeExecResult{Stdout: e.stdout}, nil
}

// TestFreeze_HardDeadlineScalesWithPodShape is the ka-0000007927 integration guard:
// a multi-sidecar agent's Freeze must thread a LARGER wall-clock budget into the
// node exec ctx than a bare agent's, because the per-container relocation/gofer
// copies scale with container count. It captures the ctx deadline the exec saw and
// asserts (a) the heavy pod's budget exceeds the bare pod's by the per-container
// margin, and (b) both match freezeHardDeadlineFor(estimated count). Revert the
// call-site sizing (back to the flat freezeHardDeadline()) and part (a) goes RED —
// both budgets would be identical and the heavy agent would be starved again.
func TestFreeze_HardDeadlineScalesWithPodShape(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"

	measure := func(t *testing.T, pod *corev1.Pod, stdout string, wantCount int) time.Duration {
		t.Helper()
		client := fake.NewClientset(pod)
		exec := &deadlineCapturingExec{stdout: stdout}
		store := NewInMemorySnapshotStore()
		m := newFreezeThawManager(t, client, exec, store, nil)
		start := time.Now()
		if _, _, err := m.Freeze(context.Background(), FreezeRequest{
			AgentName: pod.Name, BeadID: "gd-AGENT", PodName: pod.Name, Namespace: ns,
		}); err != nil {
			t.Fatalf("Freeze(%s): %v", pod.Name, err)
		}
		if !exec.hadDL {
			t.Fatalf("freeze exec ctx for %s had no deadline — the hard-deadline cap was not applied", pod.Name)
		}
		budget := exec.deadline.Sub(start)
		// The WithTimeout deadline is computed as (t_inside_freeze + want) where
		// t_inside_freeze >= start, so the captured budget is [want, want+elapsed].
		// Allow a small slack for the wall-clock elapsed between start and the
		// WithTimeout call; it must never be BELOW want (that would be truncation).
		want := m.freezeHardDeadlineFor(wantCount)
		if budget < want || budget > want+5*time.Second {
			t.Errorf("%s: exec-ctx budget ~%v, want ≈ freezeHardDeadlineFor(%d)=%v", pod.Name, budget, wantCount, want)
		}
		return want
	}

	// Bare agent (pause + agent = 2): baseline 240s cap.
	bareWant := measure(t, livePodA("bare", ns, node), freezeOKStdout, 2)
	// DinD+Postgres agent (pause + agent + 2 sidecars = 4): scaled 360s cap.
	heavyWant := measure(t, livePodAWithSidecars("heavy", ns, node), freezeOKStdoutSidecars, 4)

	if heavyWant <= bareWant {
		t.Fatalf("multi-sidecar freeze budget (%v) must exceed bare-agent budget (%v) — the scaling regressed", heavyWant, bareWant)
	}
	if diff, want := heavyWant-bareWant, freezeRelocationPerContainerMargin*2; diff != want {
		t.Errorf("heavy-vs-bare headroom delta = %v, want %v (2 extra sidecars × per-container margin)", diff, want)
	}
}
