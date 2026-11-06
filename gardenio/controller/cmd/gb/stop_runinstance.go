package main

import (
	"context"
	"fmt"
	"os"
	"slices"

	"gardenio/controller/internal/beadsapi"
)

// forcedStopRunInstanceLabels are the trigger-engine source labels that mark a
// task bead as a single-shot schedule/cron/event run-instance — the exact set
// the stale-reaper reconciles when their assignee agent disappears
// (statusreporter.runInstanceSourceLabels, mirrored again in
// news.go newsRunInstanceLabels). Kept in sync with those two lists.
var forcedStopRunInstanceLabels = []string{"source:schedule", "source:multi", "source:schedule-mirror"}

// isRunInstanceTaskDetail reports whether a claimed task is a per-cycle
// run-instance produced by the trigger engine — identified by a source label
// or a schedule/multi fire-key field. Mirrors statusreporter.isRunInstanceTask
// so the truthful-close set here matches the set the reaper would otherwise
// reconcile with false crash/OOM provenance.
func isRunInstanceTaskDetail(t *beadsapi.BeadDetail) bool {
	for _, l := range t.Labels {
		if slices.Contains(forcedStopRunInstanceLabels, l) {
			return true
		}
	}
	if t.Fields["schedule_fire_key"] != "" || t.Fields["multi_fire_key"] != "" {
		return true
	}
	return false
}

// closeForcedRunInstanceTasks truthfully closes any single-shot run-instance
// task beads still claimed in_progress by this agent when it despawns via
// `gb stop --force` (ka-0000006105). Without this, `--force` skips the
// claimed-work check, leaves the run-instance task in_progress, and the
// stale-reaper later closes it with close_reason "run-instance assignee agent
// gone (crash/OOM, not reaped)" — a false crash record for an agent that
// actually stopped cleanly with a wrap-up. Closing the task here (with a
// truthful reason that names the forced stop) both fixes the audit trail and
// ensures the reaper never sees the task at all.
//
// Scope is deliberately limited to run-instance tasks — the reaper's own
// reconcile set. Ordinary claimed tasks are left untouched (the reaper skips
// them too): closing them would masquerade unfinished real work as complete.
//
// Best-effort: every failure is a warning, never blocking the despawn. Returns
// the count closed for logging/tests. Only meaningful on the terminal --force
// path — the caller gates on stopForce and must NOT invoke it on the suspend
// (freeze/thaw) path, where the agent keeps its claim to resume later.
func closeForcedRunInstanceTasks(ctx context.Context, agentActor string) int {
	if daemon == nil || agentActor == "" {
		return 0
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"task"},
		Kinds:    []string{"issue"},
		Statuses: []string{"in_progress"},
		Assignee: agentActor,
		Limit:    100,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not list claimed run-instance tasks to close on --force: %v\n", err)
		return 0
	}

	reason := fmt.Sprintf("gb stop --force by %s (work not completed)", agentActor)
	closed := 0
	for _, t := range result.Beads {
		if t == nil || !isRunInstanceTaskDetail(t) {
			continue
		}
		// Attribute the close to the agent (not the default "gardenio") so
		// closed_by names who forced the stop, matching close_reason.
		if err := daemon.CloseBeadAs(ctx, t.ID, agentActor, map[string]string{
			"close_reason": reason,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not close claimed run-instance task %s on --force: %v\n", t.ID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "Closed claimed run-instance task %s (%s).\n", t.ID, reason)
		closed++
	}
	return closed
}

// closeOwnRunInstanceTask closes the scheduled run's own run-instance task on a
// clean (non-force) terminal stop, the task claimedWorkGate let through
// (gd-mOnsZZyug9). The close names the stop, so the reaper never closes it
// later as "assignee agent gone (crash/OOM)". Best-effort like the --force
// close: a failure is a warning and the reaper still catches the task.
func closeOwnRunInstanceTask(ctx context.Context, taskID, agentActor string) bool {
	if daemon == nil || taskID == "" || agentActor == "" {
		return false
	}
	reason := fmt.Sprintf("scheduled run ended: gb stop by %s", agentActor)
	if err := daemon.CloseBeadAs(ctx, taskID, agentActor, map[string]string{
		"close_reason": reason,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not close this run's task %s: %v\n", taskID, err)
		return false
	}
	fmt.Fprintf(os.Stderr, "Closed this run's task %s (%s).\n", taskID, reason)
	return true
}
