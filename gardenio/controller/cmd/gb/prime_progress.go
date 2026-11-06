package main

// prime_progress.go — emits the "## Loop Progress" block in `gb prime`
// when the agent's schedule bead opts in via progress.surface.in_prime.
// See plan §T5 / §3.3 and progress.RenderPrimeBlock.

import (
	"context"
	"fmt"
	"io"
	"time"

	"gardenio/controller/internal/scheduler/progress"
)

// outputProgressBlock writes the "## Loop Progress" section when the
// agent's schedule has progress.surface.in_prime: true. Silent no-op for
// non-scheduled agents and for schedules without progress instrumentation.
//
// Live-state, not system-prompt: counts shift every reconcile tick, so
// this is rendered into the dynamic body and not the cached system prompt.
func outputProgressBlock(w io.Writer, agentID string) {
	if agentID == "" {
		return
	}
	ctx := context.Background()
	agentBead, err := daemon.GetBead(ctx, agentID)
	if err != nil || agentBead == nil {
		return
	}
	scheduleID := agentBead.Fields["schedule_id"]
	if scheduleID == "" {
		return
	}
	sched, err := daemon.GetBead(ctx, scheduleID)
	if err != nil || sched == nil {
		return
	}
	spec, err := progress.ParseSpec(sched.Fields["progress"])
	if err != nil || spec.IsZero() || !spec.Surface.InPrime {
		return
	}
	state, err := progress.ParseState(sched.Fields["progress_state"])
	if err != nil {
		return
	}
	resolved := spec.ResolveTemplates(sched.Fields["assign_bead"], scheduleID)
	out := progress.RenderPrimeBlock(resolved, state, time.Now())
	if out == "" {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprint(w, out)
}
