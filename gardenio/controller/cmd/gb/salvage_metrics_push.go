package main

// salvage_metrics_push.go — instrumentation for `gb salvage` (plan
// ka-0000003747 step C8, task ka-0000000550). Salvage runs in the pod preStop
// hook, whose stderr is DISCARDED — before this, whether end-of-life WIP
// rescue succeeded was invisible except by reading per-agent S3 manifests or
// task-bead comments by hand. These pushes make the fleet-wide salvage
// success/failure rate, rescued-byte volume, and run latency chartable.
//
// Emitted series (all best-effort via vmagent, see vmagent_push.go):
//
//	gardenio_salvage_runs_total{outcome,agent}   — one per salvage run that found
//	    dirty worktrees. outcome ∈ success (every target bundled+uploaded),
//	    partial (some uploaded), failed (none uploaded / S3 client init failed).
//	    Quiet no-op runs (no dirty worktree, no S3 sink) are NOT counted — they
//	    rescue nothing and would drown the signal in every clean pod teardown.
//	gardenio_salvage_targets_total{result,agent} — dirty worktrees seen, split
//	    into result ∈ uploaded | skipped, so partial runs are quantifiable.
//	gardenio_salvage_bytes_total{agent}          — bundle bytes durably uploaded.
//	gardenio_salvage_duration_seconds{agent}     — wall-clock of the run (gauge;
//	    one-shot per pod teardown, same shape as gardenio_session_duration_seconds
//	    in session_metrics_push.go).
//
// The agent label matches the other gb-push metrics (KD_AGENT_ID /
// BOAT_AGENT_BEAD_ID — the agent bead id, the drill-down key for
// `gb salvage restore --agent-bead <id>`).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// salvageRunStats summarizes one salvage run for the metrics push.
type salvageRunStats struct {
	outcome  string // success | partial | failed
	targets  int    // dirty worktrees discovered
	uploaded int    // targets whose bundle landed in S3
	bytes    int64  // total bundle bytes uploaded
	duration time.Duration
}

// pushSalvageMetrics emits the salvage run counters to vmagent. Best-effort
// and detached from the (likely near-expired) salvage deadline — see
// pushVMAgentLines. A 3s ceiling keeps it inside the preStop grace budget.
func pushSalvageMetrics(ctx context.Context, s salvageRunStats) {
	agent := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))

	var b strings.Builder
	fmt.Fprintf(&b, "gardenio_salvage_runs_total{outcome=%q,agent=%q} 1\n", s.outcome, agent)
	if s.uploaded > 0 {
		fmt.Fprintf(&b, "gardenio_salvage_targets_total{result=\"uploaded\",agent=%q} %d\n", agent, s.uploaded)
	}
	if skipped := s.targets - s.uploaded; skipped > 0 {
		fmt.Fprintf(&b, "gardenio_salvage_targets_total{result=\"skipped\",agent=%q} %d\n", agent, skipped)
	}
	if s.bytes > 0 {
		fmt.Fprintf(&b, "gardenio_salvage_bytes_total{agent=%q} %d\n", agent, s.bytes)
	}
	fmt.Fprintf(&b, "gardenio_salvage_duration_seconds{agent=%q} %.3f\n", agent, s.duration.Seconds())

	pushVMAgentLines(ctx, b.String(), 3*time.Second)
}
