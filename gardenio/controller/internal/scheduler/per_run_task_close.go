// ClosePerRunTaskBead is the free-function form of
// (*Scheduler).closePerRunTaskBead, shared with the bridge gardener evaluator
// (Phase 3b/B7-c2) so type=trigger gardeners close their per-run task beads
// with identical observable behavior to type=schedule gardeners.

package scheduler

import (
	"context"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// PerRunTaskCloseDaemon is the narrow daemon surface ClosePerRunTaskBead
// needs. *beadsapi.Client satisfies it transparently and bridge mocks can
// stub it with two methods.
type PerRunTaskCloseDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// ClosePerRunTaskBead closes the task bead the owner (schedule or gardener
// trigger) created for this run when the agent terminates. No-op when:
//
//   - assignBead is non-empty: the schedule pins a fixed task that is
//     reused across runs and must never be auto-closed.
//   - agentDetail is nil or carries no `task_id` field (legacy or
//     hand-spawned agents).
//   - the task can't be fetched or is already closed.
//   - the task's `schedule_id` field does not match ownerID (paranoia
//     against someone re-pointing task_id at an unrelated bead).
//
// ownerKind / ownerID are used only for log fields so the same helper
// powers both the scheduler ("schedule") and bridge gardener ("trigger")
// call sites without losing audit context.
func ClosePerRunTaskBead(
	ctx context.Context,
	daemon PerRunTaskCloseDaemon,
	logger *slog.Logger,
	ownerKind, ownerID, assignBead string,
	agentDetail *beadsapi.BeadDetail,
) {
	if assignBead != "" {
		return
	}
	if agentDetail == nil {
		return
	}
	taskID := agentDetail.Fields["task_id"]
	if taskID == "" {
		return
	}
	task, err := daemon.GetBead(ctx, taskID)
	if err != nil || task == nil {
		return
	}
	if task.Status == "closed" {
		return
	}
	// schedule_id is the durable pointer the spawn path stamps on the task
	// bead. Both schedules and gardener triggers persist their own ID under
	// this key so the check is uniform.
	if task.Fields["schedule_id"] != ownerID {
		return
	}
	if err := daemon.CloseBead(ctx, taskID, nil); err != nil {
		if logger != nil {
			logger.Warn("failed to close per-run task bead",
				ownerKind, ownerID, "task", taskID,
				"agent", agentDetail.ID, "error", err)
		}
		return
	}
	if logger != nil {
		logger.Info("closed per-run task bead",
			ownerKind, ownerID, "task", taskID, "agent", agentDetail.ID)
	}
}
