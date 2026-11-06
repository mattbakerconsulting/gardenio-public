package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// outputScheduledLoopContext writes the "## Scheduled Loop Context" block when
// the agent bead was spawned by a schedule (i.e., has schedule_id set). For
// non-scheduled agents this is a no-op.
//
// Content (see proposals/ephemeral-schedules-gardener-primitive.md §2):
//   - Run N of M with schedule title/ID and terminal goal
//   - Prior-run wrap-up accomplishments (fetched from schedule_prior_agent_id)
//   - Runs remaining after this one
//   - Explicit gb yield vs gb stop semantics — the inverse of non-scheduled
//     gardener behavior, so this block must be inline and cannot be elided.
func outputScheduledLoopContext(w io.Writer, agentID string) {
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

	title := agentBead.Fields["schedule_title"]
	if title == "" {
		title = scheduleID
	}
	terminalGoal := agentBead.Fields["schedule_terminal_goal"]
	runsSpawned, _ := strconv.Atoi(agentBead.Fields["schedule_runs_spawned"])
	maxRuns, _ := strconv.Atoi(agentBead.Fields["schedule_max_runs"])
	priorAgentID := agentBead.Fields["schedule_prior_agent_id"]

	fmt.Fprintln(w, "\n## Scheduled Loop Context")
	fmt.Fprintln(w)
	if maxRuns > 0 {
		fmt.Fprintf(w, "You are run **%d of %d** of schedule **%q** (%s).\n\n",
			runsSpawned, maxRuns, title, scheduleID)
	} else {
		fmt.Fprintf(w, "You are run **%d** of schedule **%q** (%s) — `max_runs` is unlimited.\n\n",
			runsSpawned, title, scheduleID)
	}

	if terminalGoal != "" {
		fmt.Fprintf(w, "**Terminal goal:** %s\n\n", terminalGoal)
	}

	if prior := fetchPriorRunHandoff(ctx, priorAgentID); prior != nil {
		fmt.Fprintf(w, "**Where the last run left off** (run's own wrap-up, bead `%s`", priorAgentID)
		if prior.CloseReason != "" {
			fmt.Fprintf(w, ", close_reason `%s`", prior.CloseReason)
		}
		fmt.Fprintln(w, "):")
		for line := range strings.SplitSeq(strings.TrimRight(prior.Text, "\n"), "\n") {
			fmt.Fprintf(w, "> %s\n", line)
		}
		fmt.Fprintln(w)
	}

	if maxRuns > 0 {
		remaining := max(maxRuns-runsSpawned, 0)
		if remaining > 0 {
			fmt.Fprintf(w, "**Runs remaining after this one:** %d. If the goal is reached, stop with\n`gb stop --wrapup '{\"goal_reached\":true,\"goal_reason\":\"…\"}'` to close the loop.\n\n", remaining)
		} else {
			fmt.Fprintf(w, "**Runs remaining after this one:** 0. This is the final run under `max_runs=%d`. If the goal is reached, stop with\n`gb stop --wrapup '{\"goal_reached\":true,\"goal_reason\":\"…\"}'` to close the loop cleanly.\n\n", maxRuns)
		}
	}

	budgetLabel := "your " + strconv.Itoa(maxRuns) + " runs"
	if maxRuns <= 0 {
		budgetLabel = "your run budget"
	}
	fmt.Fprintln(w, "**Run lifecycle (READ CAREFULLY — this is the opposite of non-scheduled")
	fmt.Fprintln(w, "gardener behavior):**")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- `gb stop` **consumes one of %s.** Use it only when this run is\n  complete — meaning your crew has finished, you have resolved their\n  decisions, and you have produced a wrap-up.\n", budgetLabel)
	fmt.Fprintln(w, "- `gb yield` **does NOT consume a run.** Use it whenever you are waiting on")
	fmt.Fprintln(w, "  crew, on a human decision, or on an event. The pod de-hydrates; you wake")
	fmt.Fprintln(w, "  when the thing you were waiting for happens. This is how you \"finish a")
	fmt.Fprintln(w, "  run\" that takes hours.")
	fmt.Fprintln(w, "- If your crew raises a decision that you can answer yourself (approve a")
	fmt.Fprintln(w, "  merge, pick between two implementation options with clear criteria,")
	fmt.Fprintln(w, "  re-prompt them), answer it and keep the run going. Only escalate to a")
	fmt.Fprintln(w, "  human (`gb decision create` then `gb yield`) when you genuinely cannot")
	fmt.Fprintln(w, "  decide.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "In short: **a run is an attempt to finish the goal, not a wake-up.** Spawn")
	fmt.Fprintln(w, "crew, yield while they work, resolve what they raise, then stop.")
	fmt.Fprintln(w)
}

// priorRunHandoff is what the previous run of a scheduled loop passes forward:
// the text it wrote for its successor, paired with the system-recorded fact of
// how that run ended.
type priorRunHandoff struct {
	// Text is the forward-looking wrap-up prose — handoff_notes preferred,
	// falling back to blockers, then tldr.
	Text string
	// CloseReason is how the prior run's bead closed. Platform-recorded, not
	// agent-authored: it is what lets the reader tell a deliberate `gb stop`
	// from a reaped or restart-exhausted death before weighting the prose.
	CloseReason string
}

// fetchPriorRunHandoff returns what the prior run of this schedule left for its
// successor. Nil if there is no prior agent, no wrap-up, no forward-looking
// content, or a parse failure — all non-fatal.
//
// WHAT CHANGED AND WHY (task ka-0000002280). This function used to read
// `accomplishments` and nothing else, which made it the ONLY place in the
// system that replayed a wrap-up field into a successor's context — and the
// field it chose was the one that is purely an agent's assessment of its own
// achievement, with no record behind it. Meanwhile `handoff_notes`, the field
// the wrap-up contract explicitly describes as "context the next agent needs to
// pick up where you left off", was dropped on the floor.
//
// That is backwards on retention grounds alone: 1.84M characters of
// accomplishments were written over 7 days and only ~1/3 were ever read back,
// while the field authored FOR the reader was never read at all. It is worse
// than backwards on signal grounds — replaying a run's self-congratulation into
// the next run is exactly the channel by which an unchecked claim accretes into
// a premise across a long-running loop (operator direction, 2026-08-16).
//
// The preference order is forward-looking-first: handoff_notes (written for the
// successor) → blockers (a wall the successor will otherwise re-derive) → tldr
// (a one-line statement of outcome). Accomplishments is deliberately not in the
// chain at any priority; a run that wrote only accomplishments renders nothing
// here, and its bead id is still printed so the successor can go read it.
func fetchPriorRunHandoff(ctx context.Context, priorAgentID string) *priorRunHandoff {
	if priorAgentID == "" {
		return nil
	}
	prior, err := daemon.GetBead(ctx, priorAgentID)
	if err != nil || prior == nil {
		return nil
	}
	out := &priorRunHandoff{CloseReason: strings.TrimSpace(prior.Fields["close_reason"])}

	raw := prior.Fields["wrapup"]
	if raw == "" || raw == "null" {
		return nil
	}
	var wu struct {
		HandoffNotes string `json:"handoff_notes"`
		Blockers     string `json:"blockers"`
		TLDR         string `json:"tldr"`
	}
	if err := json.Unmarshal([]byte(raw), &wu); err != nil {
		return nil
	}
	for _, candidate := range []string{wu.HandoffNotes, wu.Blockers, wu.TLDR} {
		if v := strings.TrimSpace(candidate); v != "" {
			out.Text = v
			return out
		}
	}
	return nil
}
