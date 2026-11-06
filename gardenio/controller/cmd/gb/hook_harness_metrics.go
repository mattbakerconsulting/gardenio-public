package main

// hook_harness_metrics.go — gardenio_tool_harness_overhead_seconds{phase,agent,harness}, the
// per-tool-call cost of gardenio's OWN hook plumbing (gd-RpflV68KX1, plan
// gardenio/plans/agent-tool-latency-attribution.md P3, epic gd-Oc3GVm9gc6).
//
// P0 forwards Claude's own duration_ms (tool execution time); the
// Pre->Post wall clock additionally contains gardenio's PreToolUse/PostToolUse
// hook commands (gb hook relay, /hooks/drain-queue.sh) and any permission
// gate. That delta is gardenio's own tax, and the only direct way to measure
// it is to time each hook command from inside gb hook itself — not infer it
// from the central HOOK_EVENTS consumer (P2's separate, bounded-cardinality
// aggregation), which can only ever see the Pre/Post delta, not a breakdown
// by hook command.
//
// phase is a small hand-picked enum (relay | bus_publish | drain_queue) —
// deliberately NOT per-tool or per-pod-unique, matching the plan's explicit
// cardinality rejection (§4: per-tool/per-pod combinations would be ~49k
// series). Every gb hook invocation is a fresh, stateless CLI process, so a
// genuine Prometheus Counter/Histogram (monotonically non-decreasing across
// the many hook calls one pod makes over its life) needs a long-lived
// aggregator. That is `gb init` (PID 1 of the agent container, alive for the
// whole pod): runHarnessOverheadFlusher folds the spool into an in-memory
// cumulative histogram and pushes the FULL cumulative snapshot each time. That
// is what makes rate()/histogram_quantile() over the resulting _bucket series
// correct: each (agent,phase) series is a real monotonic counter, not an
// independent single-shot sample re-pushed under a shared label set (which
// would make rate() bounce on every push instead of accumulating).
//
// Nothing here may add to the latency it measures, and nothing may leave a
// process behind. Claude Code hooks are synchronous, and the container's PID 1
// (gb init) does not reap orphans, so a hook must NOT fork a detached recorder:
// each one becomes a permanent zombie and ~3 per tool call exhausts pids.max
// after a few thousand calls (merge-desk review of PR #202). Instead a hook
// reports by appending one short line per observation ("<phase> <seconds>\n")
// to a per-pod spool file with a single O_APPEND write — atomic for writes
// under PIPE_BUF, so concurrent hooks need no lock — and does no network and no
// fork. The flusher (init) drains the spool every harnessOverheadFlushInterval.
//
// Buckets run 10ms..5s: dense enough to resolve the "trivial sed 26-40ms"
// floor and the gb squawk ~415ms reference point from the task, with 1s
// landing exactly on a boundary so the p95 > 1s alert reads a real bucket
// edge rather than an interpolated estimate.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/harness"
)

var harnessOverheadBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 2, 5}

// harnessOverheadFlushInterval is how often the init-hosted flusher drains the
// spool and pushes. Prometheus scrape-side resolution does not need better.
const harnessOverheadFlushInterval = 15 * time.Second

// harnessOverheadSpoolMax caps the spool so a pod without a running flusher
// (local dev, a crashed init) cannot fill /tmp; hooks drop observations past it.
const harnessOverheadSpoolMax = 1 << 20

// harnessOverheadRotateSettle is how long the flusher waits after renaming the
// spool before reading it, so a hook that opened the old file just before the
// rename finishes its (microsecond) append before the read.
const harnessOverheadRotateSettle = 100 * time.Millisecond

// hookHarnessOverheadPhaseValid is the bounded phase enum: an unknown phase is
// dropped by the flusher instead of silently growing a new label value.
func hookHarnessOverheadPhaseValid(phase string) bool {
	switch phase {
	case "relay", "bus_publish", "drain_queue":
		return true
	default:
		return false
	}
}

// harnessOverheadSpoolPath returns the per-pod spool file path. It is a fixed
// /tmp path (not os.TempDir) because the writer (a hook, whose TMPDIR Claude
// Code may override) and the reader (gb init) must agree; drain-queue.sh
// hardcodes the same path. Overridable via GARDENIO_HARNESS_OVERHEAD_SPOOL
// for tests.
func harnessOverheadSpoolPath() string {
	if p := os.Getenv("GARDENIO_HARNESS_OVERHEAD_SPOOL"); p != "" {
		return p
	}
	return "/tmp/gardenio-harness-overhead.spool"
}

// harnessOverheadPhaseState is the cumulative histogram for one phase.
// Counts[i] is the number of observations <= harnessOverheadBuckets[i]
// (cumulative, mirroring the Prometheus bucket convention).
type harnessOverheadPhaseState struct {
	Counts []int64
	Sum    float64
	Count  int64
}

// harnessOverheadState is the full in-memory state, keyed by phase.
type harnessOverheadState map[string]*harnessOverheadPhaseState

// apply folds obs into the state, dropping negative durations and unknown
// phases. It reports whether anything was recorded.
func (st harnessOverheadState) apply(obs []harnessObservation) bool {
	changed := false
	for _, o := range obs {
		if o.Seconds < 0 || !hookHarnessOverheadPhaseValid(o.Phase) {
			continue
		}
		ps := st[o.Phase]
		if ps == nil {
			ps = &harnessOverheadPhaseState{Counts: make([]int64, len(harnessOverheadBuckets))}
			st[o.Phase] = ps
		}
		for i, bound := range harnessOverheadBuckets {
			if o.Seconds <= bound {
				ps.Counts[i]++
			}
		}
		ps.Sum += o.Seconds
		ps.Count++
		changed = true
	}
	return changed
}

// harnessObservation is one phase duration awaiting recording.
type harnessObservation struct {
	Phase   string
	Seconds float64
}

// formatHarnessSpoolLines renders obs as spool lines. Together the lines of one
// invocation are a single write, so they stay contiguous and atomic.
func formatHarnessSpoolLines(obs []harnessObservation) []byte {
	var b bytes.Buffer
	for _, o := range obs {
		fmt.Fprintf(&b, "%s %s\n", o.Phase, strconv.FormatFloat(o.Seconds, 'f', -1, 64))
	}
	return b.Bytes()
}

// parseHarnessSpool parses spool content, skipping malformed lines (a torn or
// hand-edited line must not poison the batch).
func parseHarnessSpool(data []byte) []harnessObservation {
	var obs []harnessObservation
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		phase, secs, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || !hookHarnessOverheadPhaseValid(phase) {
			continue
		}
		seconds, err := strconv.ParseFloat(secs, 64)
		if err != nil || seconds < 0 {
			continue
		}
		obs = append(obs, harnessObservation{Phase: phase, Seconds: seconds})
	}
	return obs
}

// appendHarnessSpool appends obs to the spool with one O_APPEND write: no
// lock, no network, no fork. Best-effort — a failure drops the observation
// (reported to stderr) and never fails the hook it is instrumenting.
func appendHarnessSpool(obs []harnessObservation) {
	if len(obs) == 0 {
		return
	}
	data := formatHarnessSpoolLines(obs)
	path := harnessOverheadSpoolPath()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: gb hook harness-overhead spool: %v\n", err)
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > harnessOverheadSpoolMax {
		return
	}
	if _, err := f.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "warning: gb hook harness-overhead spool: %v\n", err)
	}
}

// harnessRecorder collects a hook command's phase timings in memory and appends
// them to the spool on finish. Methods are nil-safe, and the recorder stays
// inert until arm is called, so a command can time itself from process start
// and only commit once it knows the event is in scope.
type harnessRecorder struct {
	start time.Time
	armed bool
	obs   []harnessObservation
}

func newHarnessRecorder() *harnessRecorder {
	return &harnessRecorder{start: time.Now()}
}

// arm marks the invocation as one that should be reported. The relay arms it
// only for tool events (Pre/PostToolUse[Failure]): the metric and its alert
// are described as the tax on every tool call, and the same command also
// handles SessionStart, Notification, Task* etc. that would otherwise dilute it.
func (r *harnessRecorder) arm() {
	if r != nil {
		r.armed = true
	}
}

// observe records the duration of an inner phase. No-op until armed.
func (r *harnessRecorder) observe(phase string, d time.Duration) {
	if r == nil || !r.armed {
		return
	}
	r.obs = append(r.obs, harnessObservation{Phase: phase, Seconds: d.Seconds()})
}

// finish records the whole-command duration as phase and appends everything
// collected to the spool. No-op until armed. The clock stops just before the
// append, so the append itself is the one unmeasured (microsecond) cost.
func (r *harnessRecorder) finish(phase string) {
	if r == nil || !r.armed {
		return
	}
	r.observe(phase, time.Since(r.start))
	appendHarnessSpool(r.obs)
}

// renderHarnessOverheadLines formats the full cumulative state as Prometheus
// exposition lines, labeled {phase,agent,harness} to match the agent-cardinality
// convention every other gb push metric uses (salvage_metrics_push.go,
// session_metrics_push.go) — bounded by fleet-lifetime pod count, not by
// tool-call count, since each pod's series is overwritten in place by its
// own later (larger) cumulative snapshot rather than growing a new series
// per call. harness (claude|codex, from GARDENIO_HARNESS, ADR 0004) is constant
// for a pod's life, so it splits the series per agent CLI without adding any
// series per pod.
func renderHarnessOverheadLines(state harnessOverheadState) string {
	agent := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	harnessName := harness.MustFromEnv()

	var b strings.Builder
	for phase, ps := range state {
		labels := fmt.Sprintf("phase=%q,agent=%q,harness=%q", phase, agent, harnessName)
		for i, bound := range harnessOverheadBuckets {
			fmt.Fprintf(&b, "gardenio_tool_harness_overhead_seconds_bucket{%s,le=%q} %d\n", labels, formatBucketLe(bound), ps.Counts[i])
		}
		fmt.Fprintf(&b, "gardenio_tool_harness_overhead_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, ps.Count)
		fmt.Fprintf(&b, "gardenio_tool_harness_overhead_seconds_sum{%s} %s\n", labels, strconv.FormatFloat(ps.Sum, 'f', -1, 64))
		fmt.Fprintf(&b, "gardenio_tool_harness_overhead_seconds_count{%s} %d\n", labels, ps.Count)
	}
	return b.String()
}

// formatBucketLe formats a bucket boundary the way Prometheus client
// libraries do: minimal decimal representation, no scientific notation for
// these magnitudes.
func formatBucketLe(bound float64) string {
	return strconv.FormatFloat(bound, 'f', -1, 64)
}

// flushHarnessOverheadSpool drains the spool into state and, if anything new
// arrived, pushes the full cumulative snapshot once. The spool is rotated
// (renamed) rather than truncated so hooks appending concurrently never lose a
// line to a read-then-truncate race; a rotated file left by a previous crashed
// flush is picked up first. Runs only in the long-lived flusher, so the push's
// network wait never touches a hook.
func flushHarnessOverheadSpool(ctx context.Context, state harnessOverheadState) {
	spool := harnessOverheadSpoolPath()
	rotated := spool + ".flush"

	if _, err := os.Stat(rotated); err != nil {
		if err := os.Rename(spool, rotated); err != nil {
			return // no spool yet, or unreadable: nothing to do
		}
		time.Sleep(harnessOverheadRotateSettle)
	}
	data, err := os.ReadFile(rotated)
	if err != nil {
		return
	}
	_ = os.Remove(rotated)

	if !state.apply(parseHarnessSpool(data)) {
		return
	}
	pushVMAgentLines(ctx, renderHarnessOverheadLines(state), 2*time.Second)
}

// runHarnessOverheadFlusher is the per-pod aggregator, hosted by gb init for
// the life of the container. It returns when ctx is done, after a final drain
// so observations from the last interval are not lost on a clean shutdown.
func runHarnessOverheadFlusher(ctx context.Context, interval time.Duration) {
	state := harnessOverheadState{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushHarnessOverheadSpool(context.WithoutCancel(ctx), state)
			return
		case <-t.C:
			flushHarnessOverheadSpool(ctx, state)
		}
	}
}
