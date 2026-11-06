package podmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/thawepoch"
)

// nodeFSExecutor is a NodeExecutor that REALLY RUNS the trigger's node scripts
// through bash against a temp dir standing in for the node FS. Absolute node
// paths (/var/lib/gardenio/...) are rebased under f.root so the marker/ack files
// land in the temp tree. Running the actual scripts — not a regex
// approximation — exercises the real shell idiom end to end: base64 decode,
// atomic temp-then-mv, the FT_FOUND read framing, mkdir/chmod under `set -e`.
// An optional onWrite hook fires after a marker write so a test can simulate the
// agent-side watchers acking (or not).
type nodeFSExecutor struct {
	mu      sync.Mutex
	root    string
	onWrite func(fs *nodeFSExecutor)
	execErr error // when set, every Exec returns this (transport failure)
	calls   int
}

func newNodeFS(t *testing.T) *nodeFSExecutor {
	t.Helper()
	return &nodeFSExecutor{root: t.TempDir()}
}

// get reads a node-absolute path from the temp FS.
func (f *nodeFSExecutor) get(path string) (string, bool) {
	b, err := os.ReadFile(f.root + path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// put writes a node-absolute path into the temp FS.
func (f *nodeFSExecutor) put(path, contents string) {
	full := f.root + path
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	_ = os.WriteFile(full, []byte(contents), 0o644)
}

func (f *nodeFSExecutor) Exec(ctx context.Context, _ string, script string) (NodeExecResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	// Respect ctx exactly like the production ProbeNodeExecutor
	// (exec.StreamWithContext): a cancelled or expired ctx fails the exec
	// instantly. Ignoring ctx here masked the dead final-eval bug — the gate's
	// deadline-expiry re-read ran over the already-cancelled dctx and only
	// "worked" because this fake kept reading (ka-0000003308 RANK6).
	if err := ctx.Err(); err != nil {
		return NodeExecResult{}, err
	}
	if f.execErr != nil {
		return NodeExecResult{}, f.execErr
	}
	// Rebase absolute node paths under the temp root. The trigger always
	// single-quotes paths it interpolates, so the prefix is stable.
	rebased := strings.ReplaceAll(script, "'"+ThawStateHostBase, "'"+f.root+ThawStateHostBase)
	cmd := exec.CommandContext(ctx, "bash", "-c", rebased)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	res := NodeExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			return res, runErr
		}
	}
	if f.onWrite != nil && strings.Contains(res.Stdout, "FT_MARKER_WRITTEN=yes") {
		f.onWrite(f)
	}
	return res, nil
}

// writeAckToNode simulates an agent-side consumer writing its ack onto the node
// FS (the bytes the in-pod watcher would write through its mount).
func (f *nodeFSExecutor) writeAckToNode(t *testing.T, stateDir, consumer string, ack thawepoch.Ack) {
	t.Helper()
	b, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}
	f.put(thawepoch.AckPath(stateDir, consumer), string(b))
}

func mustMarshalMarker(t *testing.T, m thawepoch.Marker) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	return string(b)
}

func newTrigger(exec NodeExecutor) *NodeRehydrationTrigger {
	tr := NewNodeRehydrationTrigger(exec, testLogger())
	tr.PollInterval = time.Millisecond
	// A small grace keeps the fail-closed tests fast; tests that exercise the
	// late-ack window widen it explicitly.
	tr.AckGrace = 25 * time.Millisecond
	return tr
}

// writeOKAcksAfter spawns a timer that writes OK acks for the current marker
// epoch delay after the marker lands, modeling consumers whose detection-
// latency-offset window runs past the gate's marker-write-anchored window. The
// returned channel closes once the acks are on the fake node FS (or the marker
// could not be read), so tests can join before TempDir cleanup.
func writeOKAcksAfter(fs *nodeFSExecutor, stateDir string, delay time.Duration) <-chan struct{} {
	done := make(chan struct{})
	fs.onWrite = func(fs *nodeFSExecutor) {
		m, ok, err := readMarkerRaw(fs, stateDir)
		if err != nil || !ok {
			close(done)
			return
		}
		go func() {
			defer close(done)
			time.Sleep(delay)
			for _, c := range []string{thawepoch.ConsumerGB, thawepoch.ConsumerCoop} {
				b, _ := json.Marshal(thawepoch.Ack{Epoch: m.Epoch, Consumer: c, OK: true})
				fs.put(thawepoch.AckPath(stateDir, c), string(b))
			}
		}()
	}
	return done
}

// ackAfterFirstEvalExec wraps a nodeFSExecutor and lands both consumers' OK
// acks on the node FS the moment the gate's FIRST eval has read (and missed)
// both consumers — deterministically "mid-window": too late for the immediate
// post-marker eval, guaranteed on disk long before the deadline expires.
// Unlike a sleep-then-write goroutine this cannot lose a scheduling race on a
// loaded machine (gd-X9MVmKhzQx): the writes happen synchronously inside the
// gate's own (sequential) Exec calls. Ack reads are counted even when the exec
// failed on a dead ctx — the eval still attempted both consumers, and the acks
// must be down before the NEXT read either way.
type ackAfterFirstEvalExec struct {
	fs       *nodeFSExecutor
	stateDir string
	ackReads int
}

func (e *ackAfterFirstEvalExec) Exec(ctx context.Context, node, script string) (NodeExecResult, error) {
	res, err := e.fs.Exec(ctx, node, script)
	// Only the ack-read scripts reference the ".done" ack files; marker
	// reads/writes reference thaw-epoch.json only.
	if strings.Contains(script, ".done") {
		e.ackReads++
		if e.ackReads == 2 {
			if m, ok, merr := readMarkerRaw(e.fs, e.stateDir); merr == nil && ok {
				for _, c := range []string{thawepoch.ConsumerGB, thawepoch.ConsumerCoop} {
					b, _ := json.Marshal(thawepoch.Ack{Epoch: m.Epoch, Consumer: c, OK: true})
					e.fs.put(thawepoch.AckPath(e.stateDir, c), string(b))
				}
			}
		}
	}
	return res, err
}

// TestNodeRehydration_JustInTimeAckVisibleToFinalEval is the RED→GREEN proof for
// ka-0000003308 (RANK6): an OK ack that is ON DISK before the deadline but that
// no regular tick observed (here: the poll interval outlives the deadline, so
// the only reads are the immediate first eval and the deadline-expiry one) MUST
// still release the gate. On the broken code the deadline-expiry eval re-read
// acks over the already-cancelled dctx; with a context-RESPECTING executor —
// like the production ProbeNodeExecutor — every read failed instantly and the
// gate fail-closed with the ack sitting on the node FS.
func TestNodeRehydration_JustInTimeAckVisibleToFinalEval(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	// Acks land right after the first eval misses them — on disk mid-window —
	// and the gate never ticks inside the window: PollInterval >> Deadline, so
	// the deadline-expiry re-read is the only reader that can see them.
	nx := &ackAfterFirstEvalExec{fs: fs, stateDir: stateDir}
	tr := newTrigger(nx)
	tr.PollInterval = time.Hour
	// The deadline-expiry re-read shells out through real bash (twice); the
	// 25ms newTrigger grace starved those spawns on a loaded machine and
	// fail-closed on acks already on disk (gd-X9MVmKhzQx). What's under test
	// is WHICH context the final eval runs over, not how fast bash forks —
	// widen the grace like the other late-ack tests. On the broken code the
	// re-read ran over the already-expired dctx and failed instantly no matter
	// how wide the grace, so the RED→GREEN property is intact; on the fixed
	// code the immediate grace re-read releases, so the wide grace adds no
	// wall time.
	tr.AckGrace = 2 * time.Second

	err := tr.Trigger(context.Background(), RehydrationRequest{
		AgentName: agent, Node: "node-1", Deadline: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("gate must release on an ack present at deadline expiry (final eval needs a live ctx): %v", err)
	}
}

// TestNodeRehydration_LateAckWithinGraceReleases is the RED→GREEN proof for
// ka-0000003746 (RANK5, gate side): the consumers anchor their deadline at
// marker DETECTION — up to one watcher poll tick after the gate's marker-write
// T0 — so a consumer that succeeds near its deadline writes an OK ack shortly
// AFTER the gate's window. The gate's bounded post-deadline grace re-read must
// observe it and release instead of fail-closing a thaw that succeeded.
func TestNodeRehydration_LateAckWithinGraceReleases(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	// Acks land ~80ms past the 100ms gate window — inside the grace.
	acked := writeOKAcksAfter(fs, stateDir, 180*time.Millisecond)
	tr := newTrigger(fs)
	tr.AckGrace = 2 * time.Second

	err := tr.Trigger(context.Background(), RehydrationRequest{
		AgentName: agent, Node: "node-1", Deadline: 100 * time.Millisecond,
	})
	<-acked
	if err != nil {
		t.Fatalf("gate must release on an OK ack landing within the post-deadline grace: %v", err)
	}
}

// TestNodeRehydration_GraceExpiryStillFailsClosed pins the fail-closed contract
// around the grace: a consumer that NEVER acks holds the gate closed through
// deadline + grace (the grace widens the observation window a bounded amount;
// it never converts a real failure into a release).
func TestNodeRehydration_GraceExpiryStillFailsClosed(t *testing.T) {
	fs := newNodeFS(t) // never acks
	tr := newTrigger(fs)
	tr.AckGrace = 50 * time.Millisecond

	start := time.Now()
	err := tr.Trigger(context.Background(), RehydrationRequest{
		AgentName: "agentsim", Node: "node-1", Deadline: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected fail-closed after deadline + grace, got release")
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("gate returned after %v; expected it to hold through deadline+grace (~100ms)", elapsed)
	}
}

func TestNodeRehydration_ReleasesWhenBothAck(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	// As soon as the marker is written, both consumers ack OK for epoch 1.
	fs.onWrite = func(fs *nodeFSExecutor) {
		m, ok, err := readMarkerRaw(fs, stateDir)
		if err != nil || !ok {
			return
		}
		fs.writeAckToNode(t, stateDir, thawepoch.ConsumerGB, thawepoch.Ack{Epoch: m.Epoch, Consumer: thawepoch.ConsumerGB, OK: true})
		fs.writeAckToNode(t, stateDir, thawepoch.ConsumerCoop, thawepoch.Ack{Epoch: m.Epoch, Consumer: thawepoch.ConsumerCoop, OK: true})
	}
	tr := newTrigger(fs)

	err := tr.Trigger(context.Background(), RehydrationRequest{
		AgentName: agent, Namespace: "gvisor-poc", PodName: "agentsim-thaw-b",
		Node: "node-1", RestoredPID: "123", PodUID: "b-uid", Deadline: time.Second,
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// The marker landed with epoch 1 and the right identity hints.
	m, ok, err := readMarkerRaw(fs, stateDir)
	if err != nil || !ok {
		t.Fatalf("marker not on node FS: ok=%v err=%v", ok, err)
	}
	if m.Epoch != 1 {
		t.Errorf("epoch = %d, want 1", m.Epoch)
	}
	if m.PodUID != "b-uid" {
		t.Errorf("marker PodUID = %q, want b-uid", m.PodUID)
	}
	if m.DeadlineMS != 1000 {
		t.Errorf("marker DeadlineMS = %d, want 1000", m.DeadlineMS)
	}
}

func TestNodeRehydration_FailClosedWhenConsumerNeverAcks(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	// Only gb acks; coop never does — the gate must fail closed.
	fs.onWrite = func(fs *nodeFSExecutor) {
		m, ok, _ := readMarkerRaw(fs, stateDir)
		if ok {
			fs.writeAckToNode(t, stateDir, thawepoch.ConsumerGB, thawepoch.Ack{Epoch: m.Epoch, Consumer: thawepoch.ConsumerGB, OK: true})
		}
	}
	tr := newTrigger(fs)

	err := tr.Trigger(context.Background(), RehydrationRequest{
		AgentName: agent, Node: "node-1", Deadline: 80 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), thawepoch.ConsumerCoop) {
		t.Errorf("error should name the failed consumer coop: %v", err)
	}
}

func TestNodeRehydration_FailClosedWhenConsumerAcksNotOK(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	fs.onWrite = func(fs *nodeFSExecutor) {
		m, ok, _ := readMarkerRaw(fs, stateDir)
		if !ok {
			return
		}
		fs.writeAckToNode(t, stateDir, thawepoch.ConsumerGB, thawepoch.Ack{Epoch: m.Epoch, Consumer: thawepoch.ConsumerGB, OK: true})
		// coop acks but FAILS a required stage.
		fs.writeAckToNode(t, stateDir, thawepoch.ConsumerCoop, thawepoch.Ack{
			Epoch: m.Epoch, Consumer: thawepoch.ConsumerCoop, OK: false, Error: "nats reconnect failed",
		})
	}
	tr := newTrigger(fs)

	err := tr.Trigger(context.Background(), RehydrationRequest{
		AgentName: agent, Node: "node-1", Deadline: 80 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected fail-closed on not-OK ack, got nil")
	}
}

func TestNodeRehydration_StaleEpochAckDoesNotRelease(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	// Both consumers ack, but for a PREVIOUS epoch (e.g. a leftover from an
	// earlier thaw). The gate keys on the current epoch, so this must NOT
	// release.
	fs.onWrite = func(fs *nodeFSExecutor) {
		m, ok, _ := readMarkerRaw(fs, stateDir)
		if !ok {
			return
		}
		stale := m.Epoch - 1
		fs.writeAckToNode(t, stateDir, thawepoch.ConsumerGB, thawepoch.Ack{Epoch: stale, Consumer: thawepoch.ConsumerGB, OK: true})
		fs.writeAckToNode(t, stateDir, thawepoch.ConsumerCoop, thawepoch.Ack{Epoch: stale, Consumer: thawepoch.ConsumerCoop, OK: true})
	}
	tr := newTrigger(fs)

	// Seed a prior marker so the next epoch is 2 and the stale acks are epoch 1.
	fs.put(thawStateHostDir(agent)+"/"+thawepoch.MarkerName, mustMarshalMarker(t, thawepoch.Marker{Epoch: 1, Node: "node-1"}))

	err := tr.Trigger(context.Background(), RehydrationRequest{
		AgentName: agent, Node: "node-1", Deadline: 80 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("stale-epoch acks should fail closed, got release")
	}
}

func TestNodeRehydration_EpochIsMonotonic(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	// Pre-seed a marker at epoch 5; the next thaw must write epoch 6.
	fs.put(stateDir+"/"+thawepoch.MarkerName, mustMarshalMarker(t, thawepoch.Marker{Epoch: 5, Node: "node-1"}))
	fs.onWrite = func(fs *nodeFSExecutor) {
		m, ok, _ := readMarkerRaw(fs, stateDir)
		if ok {
			fs.writeAckToNode(t, stateDir, thawepoch.ConsumerGB, thawepoch.Ack{Epoch: m.Epoch, Consumer: thawepoch.ConsumerGB, OK: true})
			fs.writeAckToNode(t, stateDir, thawepoch.ConsumerCoop, thawepoch.Ack{Epoch: m.Epoch, Consumer: thawepoch.ConsumerCoop, OK: true})
		}
	}
	tr := newTrigger(fs)
	if err := tr.Trigger(context.Background(), RehydrationRequest{AgentName: agent, Node: "node-1", Deadline: time.Second}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	m, _, _ := readMarkerRaw(fs, stateDir)
	if m.Epoch != 6 {
		t.Errorf("epoch = %d, want 6 (monotonic after seeded 5)", m.Epoch)
	}
}

func TestNodeRehydration_ContextCancelAborts(t *testing.T) {
	fs := newNodeFS(t) // never acks
	tr := newTrigger(fs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := tr.Trigger(ctx, RehydrationRequest{AgentName: "a", Node: "node-1", Deadline: 5 * time.Second})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestNodeRehydration_NodeExecErrorOnMarkerWrite(t *testing.T) {
	fs := newNodeFS(t)
	fs.execErr = errors.New("probe gone")
	tr := newTrigger(fs)
	err := tr.Trigger(context.Background(), RehydrationRequest{AgentName: "a", Node: "node-1", Deadline: time.Second})
	if err == nil {
		t.Fatal("expected error when node exec fails")
	}
}

func TestNodeRehydration_RequiresAgentAndNode(t *testing.T) {
	tr := newTrigger(newNodeFS(t))
	if err := tr.Trigger(context.Background(), RehydrationRequest{Node: "n"}); err == nil {
		t.Error("expected error when AgentName empty")
	}
	if err := tr.Trigger(context.Background(), RehydrationRequest{AgentName: "a"}); err == nil {
		t.Error("expected error when Node empty")
	}
}

// TestConsumerDeadline_ReservesGateHeadroom is the R4 (ka-0000005572) unit guard
// on the consumer-vs-gate deadline split. The controller ENFORCES the full gate
// deadline but must ADVERTISE a strictly-smaller budget to the in-pod watchers, so
// a consumer that uses its whole window still acks before the gate closes — never
// a consumer==gate deadline collision. The clamp must also keep the advertised
// budget strictly POSITIVE (a <=0 DeadlineMS is read as "unset" by Marker.Deadline
// and silently expands back to DefaultDeadline, re-colliding consumer with gate).
func TestConsumerDeadline_ReservesGateHeadroom(t *testing.T) {
	poll := thawepoch.DefaultPollInterval
	for _, tc := range []struct {
		name string
		gate time.Duration
		want time.Duration
	}{
		{"prod-60s", 60 * time.Second, 60*time.Second - consumerDeadlineSafetyMargin},
		{"default-10s", thawepoch.DefaultDeadline, thawepoch.DefaultDeadline - consumerDeadlineSafetyMargin},
		// A gate at the floor cannot subtract the margin without going sub-floor;
		// the floor wins but is clamped back down to the gate (never exceed it).
		{"gate-eq-floor", consumerDeadlineFloor, consumerDeadlineFloor},
		// A gate BELOW the floor advertises the gate verbatim (min clamp), never a
		// larger value that would exceed — or collide past — the gate.
		{"gate-below-floor", poll, poll},
		{"tiny-gate", 10 * time.Millisecond, 10 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := consumerDeadline(tc.gate)
			if got != tc.want {
				t.Fatalf("consumerDeadline(%v) = %v, want %v", tc.gate, got, tc.want)
			}
			if got <= 0 {
				t.Fatalf("consumerDeadline(%v) = %v; must stay strictly positive (a <=0 DeadlineMS is read as unset -> DefaultDeadline, re-colliding consumer with gate)", tc.gate, got)
			}
			if got > tc.gate {
				t.Fatalf("consumerDeadline(%v) = %v EXCEEDS the gate deadline — a consumer given more time than the gate acks after it fail-closes", tc.gate, got)
			}
			// For any gate strictly above the floor, the consumer budget must be
			// strictly LESS than the gate (real headroom, not a boundary collision).
			if tc.gate > consumerDeadlineFloor && got >= tc.gate {
				t.Fatalf("consumerDeadline(%v) = %v is NOT strictly less than the gate — the consumer==gate collision R4 fixes", tc.gate, got)
			}
		})
	}
}

// TestNodeRehydration_MarkerAdvertisesSmallerConsumerBudget is the R4 behavioral
// guard: driving Trigger with a prod-realistic 60s gate deadline, the marker the
// consumers actually read off the node FS must carry a DeadlineMS strictly below
// the gate deadline (by the safety margin), while the gate still waits the full
// 60s. A regression that writes DeadlineMS = gate deadline reintroduces the
// consumer==gate collision (a late-detecting consumer acks after fail-close).
func TestNodeRehydration_MarkerAdvertisesSmallerConsumerBudget(t *testing.T) {
	fs := newNodeFS(t)
	agent := "agentsim"
	stateDir := thawStateHostDir(agent)
	tr := newTrigger(fs)

	const gate = 60 * time.Second
	// The trigger fail-closes (no acks) — we only inspect the marker it wrote,
	// so run with a short grace to keep the test fast.
	tr.AckGrace = time.Millisecond
	// Cancel the wait immediately after the marker lands so we don't burn 60s;
	// the marker write happens before waitForAcks, so it is already on the FS.
	ctx, cancel := context.WithCancel(context.Background())
	fs.onWrite = func(*nodeFSExecutor) { cancel() }
	_ = tr.Trigger(ctx, RehydrationRequest{AgentName: agent, Node: "node-1", Deadline: gate})

	m, ok, err := readMarkerRaw(fs, stateDir)
	if err != nil || !ok {
		t.Fatalf("marker not on node FS: ok=%v err=%v", ok, err)
	}
	wantConsumer := consumerDeadline(gate)
	if m.Deadline() != wantConsumer {
		t.Fatalf("marker consumer budget = %v, want %v (gate %v minus safety margin)", m.Deadline(), wantConsumer, gate)
	}
	if m.Deadline() >= gate {
		t.Fatalf("marker consumer budget %v is NOT strictly below the gate deadline %v — the consumer==gate collision (R4, ka-0000005572): a consumer detecting the marker ~1 poll tick late then using its full budget acks AFTER the gate fail-closed", m.Deadline(), gate)
	}
}

// readMarkerRaw reads the marker the trigger wrote onto the fake node FS.
func readMarkerRaw(fs *nodeFSExecutor, stateDir string) (thawepoch.Marker, bool, error) {
	v, ok := fs.get(stateDir + "/" + thawepoch.MarkerName)
	if !ok {
		return thawepoch.Marker{}, false, nil
	}
	var m thawepoch.Marker
	err := json.Unmarshal([]byte(v), &m)
	return m, err == nil, err
}
