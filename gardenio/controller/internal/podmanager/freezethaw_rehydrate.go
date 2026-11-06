package podmanager

// freezethaw_rehydrate.go — the controller-side rehydration trigger
// (rehydration-design §3–5 Stage 0/5, ka-0000004017 / ka-0000001515). This is
// the seam Phase-4 wiring left nil (freezethaw_lifecycle.go): the
// RehydrationTrigger that FreezeThawManager.Thaw fires after a verified restore.
//
// # What it does
//
// After Thaw proves the process tree is back in Pod B's kubelet-owned sandbox,
// the agent inside is still living in the past — its in-RAM credentials and NATS
// 4-tuples are stale (design §0). The trigger:
//
//  1. writes a monotonic thaw-epoch marker (thawepoch.Marker) onto the node so
//     the in-pod gb + coop watchers see "you were thawed; rehydrate now";
//  2. waits for both consumers to write OK acks for that epoch, bounded by the
//     deadline — fail-closed (design C5): if either consumer never acks OK, the
//     trigger returns an error and Thaw reports restore-succeeded-but-
//     rehydration-failed, so the controller does NOT mark the agent live on a
//     half-rehydrated identity.
//
// # Why over NodeExecutor, not local FS
//
// The thawepoch package does local-FS I/O — perfect for the agent-side watchers
// and for unit tests, but the controller is not on the node. The marker/ack
// rendezvous lives on a per-agent hostPath (manager.ThawStateHostBase) that the
// in-pod watchers reach through their COOP_THAW_STATE_DIR mount; the controller
// reaches the SAME inode by running file I/O through the NodeExecutor (the
// privileged probe chroot, exactly the access the freeze/thaw verbs already
// use). So this file re-implements marker-write / ack-read AS NODE SCRIPTS that
// emit the identical JSON thawepoch.Marker / parse the identical thawepoch.Ack —
// byte-compatible with both the Go watcher and the coop watcher.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/thawepoch"
)

// NodeRehydrationTrigger is the production RehydrationTrigger: it drops the
// thaw-epoch marker on the node's per-agent thaw-state dir and gates on the
// agent-side acks, all via a NodeExecutor (the same privileged-probe access the
// freeze/thaw verbs use). A nil trigger means "skip rehydration" (the pre-C3
// seam); a non-nil one makes thaw a warm boot.
type NodeRehydrationTrigger struct {
	exec   NodeExecutor
	logger *slog.Logger

	// Required is the set of consumers whose OK ack gates release. Defaults to
	// {gb, coop} (thawepoch.ConsumerGB / ConsumerCoop) when empty.
	Required []string

	// PollInterval is how often the trigger re-reads acks from the node.
	// Defaults to thawepoch.DefaultPollInterval.
	PollInterval time.Duration

	// AckGrace is the bounded post-deadline window during which the gate keeps
	// re-reading acks before fail-closing. Defaults to DefaultAckGrace.
	AckGrace time.Duration

	// nowFunc is overridable in tests; defaults to time.Now.
	nowFunc func() time.Time
}

// DefaultAckGrace sizes the gate's post-deadline grace re-read. The consumers
// anchor their deadline window at marker DETECTION, up to one watcher poll tick
// (thawepoch.DefaultPollInterval) after the gate's marker-write T0, so a
// consumer that succeeds near its deadline writes its OK ack up to ~1 tick past
// the gate's window (ka-0000003746 RANK5). One extra poll interval on top
// covers the ack-write + node-read round trip.
const DefaultAckGrace = 2 * thawepoch.DefaultPollInterval

// consumerDeadlineSafetyMargin is the headroom the gate reserves ABOVE the
// budget it tells the consumers (ka-0000005572 R4). The controller enforces the
// full gate deadline in waitForAcks, but the marker's DeadlineMS advertises a
// SMALLER budget to the in-pod coop/gb watchers, so a consumer that consumes its
// entire advertised budget still writes its OK ack BEFORE the gate's own window
// closes. Without this, marker.DeadlineMS == the gate deadline (a consumer==gate
// deadline collision): a consumer that detects the marker ~1 poll tick late and
// then uses its full budget acks AFTER the gate has already fail-closed — a
// spurious both-consumer fail-close on an otherwise-healthy resume.
//
// This composes with (does not replace) the post-deadline AckGrace: the margin
// shrinks the CONSUMER's window so it finishes early; the grace widens how long
// the GATE keeps LOOKING after its own deadline. Sized at TWO poll ticks — the
// same two costs AckGrace absorbs: the ≤1-tick marker-DETECTION latency (the
// consumer anchors its window at detection, up to one watcher poll after the
// gate's marker-write T0) PLUS the ack-write→node-read round trip (ka-0000003746).
// A one-tick margin would leave the worst-case consumer ack (detected 1 tick
// late, then using its full advertised budget) landing EXACTLY at the gate
// deadline — a race the grace then has to rescue; two ticks makes the consumer
// finish a full poll before the gate closes, so the ack is observed inside the
// gate's own window. The consumer budget is clamped to a positive floor
// (consumerDeadlineFloor) so a small deadline never drives DeadlineMS to <=0 —
// which Marker.Deadline() reads as "unset" and silently expands back to
// thawepoch.DefaultDeadline, defeating the whole point and re-colliding consumer
// with gate.
const consumerDeadlineSafetyMargin = 2 * thawepoch.DefaultPollInterval

// consumerDeadlineFloor is the minimum consumer budget the marker ever
// advertises. It must stay strictly positive (a zero/negative DeadlineMS is
// treated as "unset" by Marker.Deadline()) and comfortably above the watcher's
// own poll cadence so the consumer still has room to detect + run its handler.
const consumerDeadlineFloor = 2 * thawepoch.DefaultPollInterval

// consumerDeadline returns the budget the marker advertises to the in-pod
// watchers for a gate deadline of gateDeadline: gateDeadline minus the safety
// margin, floored at consumerDeadlineFloor and never exceeding gateDeadline
// itself (so a gate deadline already at/below the floor advertises the gate
// deadline verbatim rather than a larger — colliding or exceeding — value).
func consumerDeadline(gateDeadline time.Duration) time.Duration {
	d := max(gateDeadline-consumerDeadlineSafetyMargin, consumerDeadlineFloor)
	// A gate deadline already at/below the floor must advertise the gate deadline
	// verbatim rather than a larger (colliding/exceeding) value.
	return min(d, gateDeadline)
}

// NewNodeRehydrationTrigger constructs the production trigger over a
// NodeExecutor. logger may be nil (defaults to slog.Default).
func NewNodeRehydrationTrigger(exec NodeExecutor, logger *slog.Logger) *NodeRehydrationTrigger {
	if logger == nil {
		logger = slog.Default()
	}
	return &NodeRehydrationTrigger{
		exec:    exec,
		logger:  logger,
		nowFunc: time.Now,
	}
}

func (t *NodeRehydrationTrigger) now() time.Time {
	if t.nowFunc != nil {
		return t.nowFunc()
	}
	return time.Now()
}

func (t *NodeRehydrationTrigger) required() []string {
	if len(t.Required) > 0 {
		return t.Required
	}
	return []string{thawepoch.ConsumerGB, thawepoch.ConsumerCoop}
}

func (t *NodeRehydrationTrigger) pollInterval() time.Duration {
	if t.PollInterval > 0 {
		return t.PollInterval
	}
	return thawepoch.DefaultPollInterval
}

func (t *NodeRehydrationTrigger) ackGrace() time.Duration {
	if t.AckGrace > 0 {
		return t.AckGrace
	}
	return DefaultAckGrace
}

// Trigger implements RehydrationTrigger. It writes the next epoch marker into
// the agent's node-side thaw-state dir, then blocks on the agent-side acks until
// every required consumer acks OK for that epoch or the deadline expires
// (fail-closed). The returned error is non-nil iff rehydration did not release.
func (t *NodeRehydrationTrigger) Trigger(ctx context.Context, req RehydrationRequest) error {
	if t.exec == nil {
		return errors.New("rehydrate: nil node executor")
	}
	if req.AgentName == "" || req.Node == "" {
		return fmt.Errorf("rehydrate: AgentName and Node are required (got %q/%q)", req.AgentName, req.Node)
	}
	// INVARIANT (ka-0000008265 / canary #7): this WRITER/POLL dir — where the epoch
	// marker is written and the consumer .done acks are polled — MUST derive from
	// the SAME agent-name key as the restore mount SOURCE in freezethaw.go's thaw
	// barrier (ThawStateDir: thawStateHostDir(req.AgentName)). Both key off
	// req.AgentName, which equals snap.AgentName for every thaw today. A future
	// rename-on-thaw that forked one derivation (e.g. keyed the mount source off
	// snap.AgentName while this writer stayed on req.AgentName, or vice versa) would
	// re-point the two at DIFFERENT dirs — the controller writes+polls one inode
	// while the restored coop/gb watchers ack into another — silently re-introducing
	// the both-consumer fail-close of bug ka-0000000673 (the canary-#7 class).
	// thawStateHostDir is the single source of truth; both call sites passing the
	// SAME name is guarded by TestThawStateKey_WriterAndMountSourceAgree.
	stateDir := thawStateHostDir(req.AgentName)
	deadline := req.Deadline
	if deadline <= 0 {
		deadline = thawepoch.DefaultDeadline
	}

	// --- Stage 0: write the monotonic marker on the node -------------------
	epoch, err := t.nextEpoch(ctx, req.Node, stateDir)
	if err != nil {
		return fmt.Errorf("rehydrate: resolve next epoch: %w", err)
	}
	// The gate ENFORCES the full `deadline` (waitForAcks below); the marker
	// ADVERTISES a smaller consumer budget so a consumer that uses its whole
	// window still acks before the gate's own window closes (R4, ka-0000005572).
	// NEVER advertise DeadlineMS >= the gate deadline: a consumer==gate collision
	// lets a late-detecting consumer ack after the gate has fail-closed.
	consumerBudget := consumerDeadline(deadline)
	marker := thawepoch.Marker{
		Epoch:      epoch,
		PodUID:     req.PodUID,
		Node:       req.Node,
		TS:         t.now().UTC(),
		FrozenAt:   req.FrozenAt,
		DeadlineMS: consumerBudget.Milliseconds(),
	}
	if err := t.writeMarker(ctx, req.Node, stateDir, marker); err != nil {
		return fmt.Errorf("rehydrate: write marker (epoch %d): %w", epoch, err)
	}
	t.logger.Info("rehydrate: thaw-epoch marker written",
		"agent", req.AgentName, "node", req.Node, "epoch", epoch, "podUID", req.PodUID,
		"gateDeadline", deadline, "consumerBudget", consumerBudget)

	// --- Stage 5: wait for acks, fail-closed under the FULL gate deadline ---
	// (the consumers were told a smaller consumerBudget so they finish first).
	res := t.waitForAcks(ctx, req.Node, stateDir, epoch, deadline)
	if !res.released {
		return fmt.Errorf("rehydrate: not released for epoch %d after %s; failed consumers: %s",
			epoch, res.elapsed.Round(time.Millisecond), strings.Join(res.failed, ", "))
	}
	t.logger.Info("rehydrate: released",
		"agent", req.AgentName, "epoch", epoch, "elapsed", res.elapsed.Round(time.Millisecond))
	return nil
}

// nextEpoch reads the current marker from the node and returns one greater (1 if
// none), mirroring thawepoch.NextEpoch but over the node FS. A monotonic epoch
// guarantees a re-frozen-then-thawed agent never replays an old epoch
// (design §8).
func (t *NodeRehydrationTrigger) nextEpoch(ctx context.Context, node, stateDir string) (int64, error) {
	cur, ok, err := t.readMarker(ctx, node, stateDir)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 1, nil
	}
	return cur.Epoch + 1, nil
}

// readMarker reads + parses the thaw-epoch marker from the node FS. ok is false
// (nil error) when the marker is absent.
func (t *NodeRehydrationTrigger) readMarker(ctx context.Context, node, stateDir string) (thawepoch.Marker, bool, error) {
	var m thawepoch.Marker
	path := stateDir + "/" + thawepoch.MarkerName
	out, ok, err := t.catFile(ctx, node, path)
	if err != nil || !ok {
		return m, false, err
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return m, false, fmt.Errorf("parse marker %s: %w", path, err)
	}
	return m, true, nil
}

// writeMarker creates the state dir (chowned to the agent UID so the in-pod
// watchers can write their acks beside the marker), then writes the marker JSON
// atomically (temp + mv) onto the node so a watcher never sees a partial file.
func (t *NodeRehydrationTrigger) writeMarker(ctx context.Context, node, stateDir string, m thawepoch.Marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	// mkdir + chown to the agent UID/GID so the runsc pod (running as AgentUID)
	// can write ack files into the same dir; then atomic write of the marker.
	// The marker JSON is delivered via base64 to avoid any shell-quoting hazard
	// on the JSON payload. Paths are assembled from shell variables (dir/name)
	// so the quoted dir is never re-wrapped — building "$dir/$name.tmp" keeps
	// the temp path well-formed on the node.
	script := fmt.Sprintf(`set -e
dir=%[1]s
name=%[4]s
mkdir -p "$dir"
chown %[2]d:%[3]d "$dir" 2>/dev/null || true
chmod 0775 "$dir" 2>/dev/null || true
tmp="$dir/.$name.tmp.$$"
printf '%%s' %[5]s | base64 -d > "$tmp"
chown %[2]d:%[3]d "$tmp" 2>/dev/null || true
mv -f "$tmp" "$dir/$name"
echo FT_MARKER_WRITTEN=yes`,
		shellQuote(stateDir), AgentUID, AgentGID, shellQuote(thawepoch.MarkerName), shellQuote(base64Encode(b)))

	r, err := t.exec.Exec(ctx, node, script)
	if err != nil {
		return fmt.Errorf("node exec: %w", err)
	}
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "FT_MARKER_WRITTEN=yes") {
		return fmt.Errorf("marker write script rc=%d: %s", r.ExitCode, tail(r.Stderr, 400))
	}
	return nil
}

// readAck reads + parses a consumer's ack from the node FS.
func (t *NodeRehydrationTrigger) readAck(ctx context.Context, node, stateDir, consumer string) (thawepoch.Ack, bool, error) {
	var a thawepoch.Ack
	path := thawepoch.AckPath(stateDir, consumer)
	out, ok, err := t.catFile(ctx, node, path)
	if err != nil || !ok {
		return a, false, err
	}
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		return a, false, fmt.Errorf("parse ack %s: %w", path, err)
	}
	return a, true, nil
}

// catFile reads a file from the node FS. ok is false (nil error) when the file
// does not exist (so callers can poll without spurious errors). The script
// emits a sentinel framing so an empty file is distinguishable from absent.
func (t *NodeRehydrationTrigger) catFile(ctx context.Context, node, path string) (string, bool, error) {
	script := fmt.Sprintf(`if [ -f %[1]s ]; then echo FT_FOUND=yes; cat %[1]s; else echo FT_FOUND=no; fi`,
		shellQuote(path))
	r, err := t.exec.Exec(ctx, node, script)
	if err != nil {
		return "", false, fmt.Errorf("node exec read %s: %w", path, err)
	}
	if r.ExitCode != 0 {
		return "", false, fmt.Errorf("read %s rc=%d: %s", path, r.ExitCode, tail(r.Stderr, 200))
	}
	const foundYes = "FT_FOUND=yes\n"
	idx := strings.Index(r.Stdout, foundYes)
	if idx < 0 {
		// Either FT_FOUND=no, or the file was absent.
		return "", false, nil
	}
	return r.Stdout[idx+len(foundYes):], true, nil
}

// ackWaitResult is the outcome of waiting on rehydration acks for one epoch.
type ackWaitResult struct {
	released bool
	failed   []string
	elapsed  time.Duration
}

// waitForAcks polls the node for the required consumers' acks until every one
// acks OK for epoch, or the deadline (plus a bounded grace, see below) expires.
// It is the controller-side fail-closed gate (design C5) — the node-FS analogue
// of thawepoch.WaitForAcks, which the in-cluster/unit path uses directly. ctx
// cancellation aborts early with released=false.
//
// The gate's window is anchored at marker-WRITE time, but each consumer anchors
// its window at marker-DETECTION — up to one watcher poll tick later — so a
// consumer that succeeds near its deadline can write its OK ack shortly after
// the gate's window closes (ka-0000003746 RANK5). On deadline expiry the gate
// therefore keeps re-reading for a bounded AckGrace before fail-closing, and
// those re-reads run over a FRESH context derived from the parent: the expired
// dctx would make the production NodeExecutor fail every read instantly, so an
// ack already sitting on the node FS could never be observed (ka-0000003308
// RANK6).
func (t *NodeRehydrationTrigger) waitForAcks(ctx context.Context, node, stateDir string, epoch int64, deadline time.Duration) ackWaitResult {
	required := t.required()
	start := t.now()
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	tick := time.NewTicker(t.pollInterval())
	defer tick.Stop()

	eval := func(rctx context.Context) (ackWaitResult, bool) { // OK: Go closure identifier named "eval", not a shell eval invocation
		var failed []string
		allOK := true
		for _, c := range required {
			a, ok, err := t.readAck(rctx, node, stateDir, c)
			if err != nil || !ok || a.Epoch != epoch || !a.OK {
				allOK = false
				failed = append(failed, c)
			}
		}
		sort.Strings(failed)
		return ackWaitResult{released: allOK, failed: failed, elapsed: t.now().Sub(start)}, allOK
	}

	// Evaluate immediately — the watchers fire on an eager nudge and acks can
	// land before the gate's first tick.
	if res, done := eval(dctx); done {
		return res
	}
	for {
		select {
		case <-dctx.Done():
			if ctx.Err() != nil {
				// Parent cancellation, not deadline expiry: abort now — no
				// grace, and no read could succeed anyway.
				failed := append([]string(nil), required...)
				sort.Strings(failed)
				return ackWaitResult{released: false, failed: failed, elapsed: t.now().Sub(start)}
			}
			return t.graceWait(ctx, eval, start)
		case <-tick.C:
			if res, done := eval(dctx); done {
				return res
			}
		}
	}
}

// graceWait is the gate's bounded post-deadline observation window: it keeps
// polling acks for AckGrace over a fresh context derived from the (still live)
// parent ctx, releasing the moment every required consumer's OK ack appears.
// It only ever widens how long the gate LOOKS — a consumer that never acked OK
// still fails the thaw closed when the grace expires.
func (t *NodeRehydrationTrigger) graceWait(ctx context.Context, readAcks func(context.Context) (ackWaitResult, bool), start time.Time) ackWaitResult {
	gctx, cancel := context.WithTimeout(ctx, t.ackGrace())
	defer cancel()

	// Immediate re-read first: this is the "final ack raced in" case — the ack
	// is already on disk, only the read needs a live context.
	res, done := readAcks(gctx)
	if done {
		return res
	}
	last := res

	tick := time.NewTicker(t.pollInterval())
	defer tick.Stop()
	for {
		select {
		case <-gctx.Done():
			last.elapsed = t.now().Sub(start)
			return last // fail-closed: grace expired without a full OK set
		case <-tick.C:
			res, done := readAcks(gctx)
			if done {
				return res
			}
			if gctx.Err() == nil {
				last = res // keep the freshest per-consumer view for the error
			}
		}
	}
}

// base64Encode returns the standard base64 of b (used to ship the marker JSON to
// the node without any shell-quoting hazard on the payload). shellQuote (the
// single-quote escaper used for path args) lives in initclone.go.
func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
