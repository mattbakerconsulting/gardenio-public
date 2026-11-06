package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
	"gardenio/controller/internal/spawnguard"
)

// The wake_pr_owner action (plan gd-nTCENSwMOH, Task 3.1, gd-xTJWewKG71). A
// bead trigger fires it when github-bridge's conflict scanner writes
// mr_merge_state=conflicting onto a work bead (docs/pr-conflicts.md). It tells
// the one agent that owns the PR, the first that applies:
//
//	(a) the task's live crew agent: a mail nudge;
//	(b) else the gardener of the plan the task sits under: a mail nudge when it
//	    is live, a planagent wake (status in_progress, session resumed, the
//	    message as its prompt) when it is parked;
//	(c) else nobody.
//
// Every fire also comments the message on the task, naming who was told, so it
// is on the bead whichever branch ran. A failed nudge or wake falls through to
// the next branch rather than failing the fire.

// prConflictActor is the author of the task comment.
const prConflictActor = "trigger-engine"

// prConflictMaxFiles bounds the files the message names.
const prConflictMaxFiles = 10

// fieldMRMergeState is the work-bead field the conflict scanner writes.
const fieldMRMergeState = "mr_merge_state"

// prPlanWalkDepth bounds the parent-chain walk from a task to its plan epic.
const prPlanWalkDepth = 8

// prOwnerWakeDaemon is the daemon surface the action needs: agent lookup for
// the nudge, the parent walk, and planagent's resolve and wake. The production
// *beadsapi.Client satisfies it.
type prOwnerWakeDaemon interface {
	NudgeBeadClient
	planagent.DaemonClient
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	AddComment(ctx context.Context, beadID, author, text string) error
}

var _ prOwnerWakeDaemon = (*beadsapi.Client)(nil)

// prOwnerTarget names which branch told the PR's owner.
type prOwnerTarget string

const (
	prOwnerCrew          prOwnerTarget = "crew"
	prOwnerGardener      prOwnerTarget = "gardener"
	prOwnerGardenerWoken prOwnerTarget = "gardener-woken"
	prOwnerNone          prOwnerTarget = "none"
)

// prOwnerWake is the outcome of one wake: the branch, the agent told, and for
// the gardener branches the plan epic.
type prOwnerWake struct {
	Target prOwnerTarget
	Agent  string
	Plan   string
}

// prOwnerWaker resolves and tells a conflicting PR's owner.
type prOwnerWaker struct {
	daemon prOwnerWakeDaemon
	logger *slog.Logger
	guard  *spawnguard.SpawnGuard // gates the parked-gardener wake; nil skips the gate
	now    func() time.Time
}

// actionWakePROwner is the engine adapter for the wake_pr_owner action: m is a
// bead-state match on the conflicting work bead.
func (e *TriggerEngine) actionWakePROwner(ctx context.Context, trig TriggerDef, m TriggerMatch) error {
	d, ok := e.daemon.(prOwnerWakeDaemon)
	if !ok {
		return fmt.Errorf("trigger %s: wake_pr_owner needs a daemon client with agent and dependency lookups", trig.ID)
	}
	taskID := m.Fields["source_bead_id"]
	if taskID == "" {
		return fmt.Errorf("trigger %s: wake_pr_owner match %q names no bead", trig.ID, m.Key)
	}
	w := prOwnerWaker{daemon: d, logger: e.logger, guard: e.guard, now: time.Now}
	res, err := w.wake(ctx, taskID)
	if err != nil {
		return err
	}
	e.logger.Info("triggers: pr conflict owner told",
		"trigger", trig.ID, "task", taskID, "target", string(res.Target), "agent", res.Agent, "plan", res.Plan)
	return nil
}

// wake re-reads the task, and while it is still conflicting tells its owner
// and comments on it. It returns an error only when the task cannot be read or
// nobody was told and the comment failed too, so the engine retries the match.
func (w prOwnerWaker) wake(ctx context.Context, taskID string) (prOwnerWake, error) {
	task, err := w.daemon.GetBead(ctx, taskID)
	if err != nil {
		return prOwnerWake{}, fmt.Errorf("reading %s: %w", taskID, err)
	}
	// The scanner may have written clean (or unknown) since the event: the
	// conflict is gone, so there is nothing to tell.
	if task.Fields[fieldMRMergeState] != "conflicting" {
		return prOwnerWake{Target: prOwnerNone}, nil
	}
	msg := prConflictMessage(task)
	res := w.tell(ctx, task, msg)
	if cerr := w.daemon.AddComment(ctx, task.ID, prConflictActor, prConflictNote(msg, res)); cerr != nil {
		if res.Target == prOwnerNone {
			return res, fmt.Errorf("commenting on %s: %w", task.ID, cerr)
		}
		w.logger.Warn("pr conflict wake: told the owner but the task comment failed", "task", task.ID, "error", cerr)
	}
	return res, nil
}

// tell runs branches (a) and (b) in order and reports which one landed.
func (w prOwnerWaker) tell(ctx context.Context, task *beadsapi.BeadDetail, msg string) prOwnerWake {
	if name := w.liveTaskCrew(ctx, task.ID); name != "" {
		err := NudgeAgent(ctx, w.daemon, w.logger, name, msg)
		if err == nil {
			return prOwnerWake{Target: prOwnerCrew, Agent: name}
		}
		w.logger.Warn("pr conflict wake: crew nudge failed; trying the plan gardener", "task", task.ID, "agent", name, "error", err)
	}

	plan := w.planEpicOf(ctx, task)
	if plan == nil {
		return prOwnerWake{Target: prOwnerNone}
	}
	eng := planagent.NewEngine(planagent.Config{Daemon: w.daemon, Logger: w.logger, Now: w.now})
	gardener := eng.Resolve(ctx, plan.ID)
	if gardener == nil {
		// No gardener has run on this plan; minting one is the plan
		// gardener trigger's job, not this action's.
		return prOwnerWake{Target: prOwnerNone, Plan: plan.ID}
	}
	name := strings.TrimSpace(gardener.Fields["agent"])
	if name == "" {
		name = gardener.Title
	}
	if eng.IsLive(gardener) {
		err := NudgeAgent(ctx, w.daemon, w.logger, name, msg)
		if err == nil {
			return prOwnerWake{Target: prOwnerGardener, Agent: name, Plan: plan.ID}
		}
		if !errors.Is(err, ErrAgentNotNudgeable) {
			w.logger.Warn("pr conflict wake: gardener nudge failed", "plan", plan.ID, "agent", name, "error", err)
			return prOwnerWake{Target: prOwnerNone, Plan: plan.ID}
		}
		// Stopping or gone: wake it instead.
	}
	if plan.Status == "closed" {
		return prOwnerWake{Target: prOwnerNone, Plan: plan.ID}
	}
	if w.guard != nil {
		if ok, reason := w.guard.Allow(); !ok {
			w.logger.Warn("pr conflict wake: gardener wake blocked by spawnguard", "plan", plan.ID, "agent", name, "reason", string(reason))
			return prOwnerWake{Target: prOwnerNone, Plan: plan.ID}
		}
	}
	if _, _, err := eng.Wake(ctx, prConflictActor, gardener, msg); err != nil {
		w.logger.Warn("pr conflict wake: gardener wake failed", "plan", plan.ID, "agent", name, "error", err)
		return prOwnerWake{Target: prOwnerNone, Plan: plan.ID}
	}
	if w.guard != nil {
		w.guard.Record()
	}
	return prOwnerWake{Target: prOwnerGardenerWoken, Agent: name, Plan: plan.ID}
}

// liveTaskCrew returns the name of a live agent working taskID, or "".
func (w prOwnerWaker) liveTaskCrew(ctx context.Context, taskID string) string {
	res, err := w.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Fields:   map[string]string{"task_id": taskID},
	})
	if err != nil {
		w.logger.Warn("pr conflict wake: listing the task's agents failed", "task", taskID, "error", err)
		return ""
	}
	if res == nil {
		return ""
	}
	now := w.now()
	for _, b := range res.Beads {
		if b == nil || b.Fields["task_id"] != taskID || b.Fields["stop_requested"] == "true" {
			continue
		}
		if !beadsapi.AgentFieldsLive(b.Fields, now) {
			continue
		}
		if name := strings.TrimSpace(b.Fields["agent"]); name != "" {
			return name
		}
	}
	return ""
}

// planEpicOf walks task's parent-child chain to the first ancestor labelled
// plan, or returns nil.
func (w prOwnerWaker) planEpicOf(ctx context.Context, task *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	cur := task.ID
	seen := map[string]bool{cur: true}
	for range prPlanWalkDepth {
		deps, err := w.daemon.GetDependencies(ctx, cur)
		if err != nil {
			w.logger.Warn("pr conflict wake: reading dependencies failed", "bead", cur, "error", err)
			return nil
		}
		parent := ""
		for _, d := range deps {
			if d.Type == "parent-child" && d.BeadID == cur && d.DependsOnID != "" {
				parent = d.DependsOnID
				break
			}
		}
		if parent == "" || seen[parent] {
			return nil
		}
		seen[parent] = true
		p, err := w.daemon.GetBead(ctx, parent)
		if err != nil || p == nil {
			w.logger.Warn("pr conflict wake: reading parent failed", "bead", parent, "error", err)
			return nil
		}
		if slices.Contains(p.Labels, planagent.PlanLabel) {
			return p
		}
		cur = parent
	}
	return nil
}

// prConflictMessage is the wake text, from the scanner's fields on task.
func prConflictMessage(task *beadsapi.BeadDetail) string {
	f := task.Fields
	since := orUnknown(f["mr_conflict_since"])
	cause := orUnknown(f["mr_conflict_cause"])
	return fmt.Sprintf("%s now conflicts with main (since %s) on: %s. First conflicting main commit: %s. "+
		"Merge main into your branch, resolve, push; do not wait for review. (%s, %s)",
		prRef(f["mr_url"]), since, conflictFilesText(f["mr_conflict_files"], prConflictMaxFiles), cause,
		task.ID, f["mr_url"])
}

// prConflictNote is the task comment: the message and who it went to.
func prConflictNote(msg string, res prOwnerWake) string {
	var to string
	switch res.Target {
	case prOwnerCrew:
		to = "Nudged crew " + res.Agent + "."
	case prOwnerGardener:
		to = "Nudged " + res.Agent + ", the gardener of plan " + res.Plan + "."
	case prOwnerGardenerWoken:
		to = "Woke " + res.Agent + ", the gardener of plan " + res.Plan + "."
	default:
		to = "No live crew or plan gardener to tell; whoever picks this task up next should do it."
	}
	return msg + "\n\n" + to
}

// prRef renders mr_url as "PR #N" when it ends in /pull/N.
func prRef(mrURL string) string {
	if i := strings.LastIndex(mrURL, "/pull/"); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSuffix(mrURL[i+len("/pull/"):], "/")); err == nil {
			return "PR #" + strconv.Itoa(n)
		}
	}
	if mrURL == "" {
		return "The PR"
	}
	return "PR " + mrURL
}

// conflictFilesText renders the scanner's mr_conflict_files JSON array as at
// most limit paths, then how many more. The scanner caps the array itself and
// ends a capped one with "…+N more"; that count is carried into the total.
func conflictFilesText(raw string, limit int) string {
	var files []string
	if err := json.Unmarshal([]byte(raw), &files); err != nil || len(files) == 0 {
		return "(files unknown)"
	}
	more := 0
	if last := files[len(files)-1]; strings.HasPrefix(last, "…+") {
		if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(last, "…+"), " more")); err == nil {
			more = n
			files = files[:len(files)-1]
		}
	}
	if len(files) > limit {
		more += len(files) - limit
		files = files[:limit]
	}
	out := strings.Join(files, ", ")
	if more > 0 {
		out += fmt.Sprintf(" and %d more", more)
	}
	return out
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}
