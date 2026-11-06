package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/agentwake"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/ciwait"
)

// The CI wake (plan gardenio/plans/wake-on-ci-completion.md §2.4, P3, task
// gd-T7aFA3tFoW). Once apply has written a concluded wait (resolved or
// timed_out, with its result) it wakes the agent whose bead carries the wait,
// through the same agentwake.Wake the slack-bridge uses for decisions and
// escalations: a dormant identity (`gb stop` closed it successor=same) is
// reopened in place with wake_reason=ci, a frozen one is thawed, anything else
// is spawned fresh with the conclusions in its prompt.
//
// Dedup is ci_wait_state: the sweep lists only pending waits, so a wait is
// woken once, by the sweep that concluded it. A wake that fails leaves the
// wait resolved and unconsumed; the stop's next_respawn_at backstop still
// revives the agent, and P4's sweep retries the wake (wakeCIWaiter is safe to
// repeat: a woken bead is open, so the in-place wake is a no-op and
// SpawnAgent's name dedup returns the live agent).

// ciWaitSpawnSource marks an agent spawned fresh because its CI wait concluded.
const ciWaitSpawnSource = "ci-wait-resume"

// e2eTestProject is the throwaway project the E2E suites seed fixtures under;
// no real pod is ever spawned into it (internal/bridge, ka-0000000809).
const e2eTestProject = "e2e-test"

// ciWakeBeads is the daemon subset the wake adds to the resolver's.
type ciWakeBeads interface {
	agentwake.Client
	AddComment(ctx context.Context, beadID, author, text string) error
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// wakeCIWaiter wakes the agent on bead b, whose wait apply has just settled as
// out. It never returns an error: the wait is already written, and a failed
// wake is logged for the backstops above.
func (h *ciWaitHandler) wakeCIWaiter(ctx context.Context, b *beadsapi.BeadDetail, w beadsapi.CIWait, out ciwait.Outcome) agentwake.Result {
	name := strings.TrimSpace(firstNonEmpty(b.Fields["agent"], b.Assignee))
	if name == "" {
		h.logger.Warn("ci wait: concluded wait on an agent bead with no agent name; not woken", "agent_bead", b.ID)
		return agentwake.Result{Outcome: agentwake.Declined}
	}
	// A laptop agent is never respawned as a pod (local-agents spec §7,
	// gd-XXYOPF1sCs); its runner's next session reads the wait itself.
	if b.Fields["execution_target"] == "laptop" || slices.Contains(b.Labels, "execution_target:laptop") {
		h.logger.Info("ci wait: concluded wait on a laptop agent; not woken as a pod", "agent", name, "agent_bead", b.ID)
		return agentwake.Result{Outcome: agentwake.Declined}
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	task := firstNonEmpty(strings.TrimSpace(w.Task), b.Fields["task_id"])
	if state, done := h.ciWaitTaskFinished(ctx, task); done {
		h.logger.Info("ci wait: task finished; not woken", "agent", name, "agent_bead", b.ID, "task", task, "task_state", state)
		return agentwake.Result{Outcome: agentwake.Declined}
	}
	role := firstNonEmpty(b.Fields["role"], "crew")
	project := b.Fields["project"]
	nudge := ciWaitNudge(w.Key, out.State, out.Result)

	res := agentwake.Wake(ctx, h.beads, h.logger, agentwake.Request{
		AgentName:         name,
		AgentBead:         b,
		Reason:            beadsapi.WakeReasonCI,
		Operator:          agentOperator(b),
		Project:           project,
		TaskID:            task,
		Role:              role,
		ExtraFields:       map[string]string{"spawn_source": ciWaitSpawnSource},
		CarrySlackRouting: true,
		Prepare: func(context.Context) (string, bool) {
			if project == e2eTestProject {
				h.logger.Info("ci wait: not spawning into the e2e-test fixture project", "agent", name, "agent_bead", b.ID)
				return "", false
			}
			return ciWaitSpawnPrompt(nudge, out.Result, task), true
		},
		LogAttrs: []any{"agent_bead", b.ID, "key", w.Key, "state", out.State},
	})
	switch res.Outcome {
	case agentwake.Woken, agentwake.Spawned:
		h.logger.Info("ci wait: agent woken", "agent", name, "agent_bead", b.ID,
			"woken_bead", res.AgentBeadID, "in_place", res.Outcome == agentwake.Woken, "thawed", res.Thawed)
		// The nudge, where the woken agent looks first: its task's comments.
		// A resumed agent still has its wait to read; a fresh one has the
		// result in its prompt.
		note := nudge
		if res.Outcome == agentwake.Woken {
			note += " Run `gb wait ci --result` for the per-workflow conclusions."
		}
		if task != "" {
			if err := h.beads.AddComment(ctx, task, bridgeActor, note); err != nil {
				h.logger.Warn("ci wait: woke the agent but the task comment failed", "task", task, "error", err)
			}
		}
	case agentwake.Failed:
		h.logger.Warn("ci wait: wake failed; the stop's timer backstop still revives the agent",
			"agent", name, "agent_bead", b.ID, "error", res.Err)
	}
	return res
}

// ciWaitTaskFinished reports whether task needs no wake: closed (or done), or
// awaits_release, i.e. its PR merged and the release closes it (gd-dLjqHLllYL).
// No GitHub PR-state lookup: these cover a merged PR without an API call per
// wake. An empty task or a failed read is not finished, so a daemon blip never
// strands a live task.
func (h *ciWaitHandler) ciWaitTaskFinished(ctx context.Context, task string) (string, bool) {
	if task == "" {
		return "", false
	}
	t, err := h.beads.GetBead(ctx, task)
	if err != nil {
		h.logger.Warn("ci wait: task read failed; waking anyway", "task", task, "error", err)
		return "", false
	}
	switch {
	case t.Status == "closed" || t.Status == "done":
		return t.Status, true
	case beadsapi.AwaitsRelease(t.Fields):
		return beadsapi.AwaitsReleaseField, true
	}
	return t.Status, false
}

// agentOperator is the human the agent bead works for, if it names one the
// daemon accepts (bridge.humanOperatorFromAgentBead's rule).
func agentOperator(b *beadsapi.BeadDetail) string {
	if op := b.Fields["operated_by"]; humanOperator(op) {
		return op
	}
	if uid := b.Fields["slack_user_id"]; uid != "" && humanOperator("slack:"+uid) {
		return "slack:" + uid
	}
	return ""
}

// ciWaitNudge is the one-paragraph message naming the red and green runs.
func ciWaitNudge(key, state, result string) string {
	var results []ciwait.Result
	_ = json.Unmarshal([]byte(result), &results)
	var red, green, open []string
	for _, r := range results {
		label := r.Name
		if r.RunID != 0 {
			label = fmt.Sprintf("%s (run %d)", r.Name, r.RunID)
		}
		switch ciConclusionClass(r.Conclusion) {
		case "green":
			green = append(green, label)
		case "red":
			red = append(red, fmt.Sprintf("%s: %s", label, r.Conclusion))
		default:
			open = append(open, fmt.Sprintf("%s: %s", label, r.Conclusion))
		}
	}
	var sb strings.Builder
	if state == beadsapi.CIWaitStateTimedOut {
		fmt.Fprintf(&sb, "CI wait on %s TIMED OUT before every run concluded.", key)
	} else {
		verdict := "GREEN"
		if len(red) > 0 {
			verdict = "RED"
		}
		fmt.Fprintf(&sb, "CI concluded %s on %s.", verdict, key)
	}
	if len(red) > 0 {
		fmt.Fprintf(&sb, " Red: %s.", strings.Join(red, "; "))
	}
	if len(green) > 0 {
		fmt.Fprintf(&sb, " Green: %s.", strings.Join(green, ", "))
	}
	if len(open) > 0 {
		fmt.Fprintf(&sb, " Not concluded: %s.", strings.Join(open, "; "))
	}
	return sb.String()
}

// ciConclusionClass sorts a run conclusion into green, red or open (a status
// reported in its place because the run never completed).
func ciConclusionClass(c string) string {
	switch c {
	case "success", "skipped", "neutral":
		return "green"
	case "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
		return "red"
	}
	return "open"
}

// ciWaitSpawnPrompt is the prompt for an agent spawned fresh (not resumed) on a
// concluded wait. Its new bead carries no ci_wait_* fields, so the result rides
// in the prompt rather than behind `gb wait ci --result`.
func ciWaitSpawnPrompt(nudge, result, task string) string {
	p := "You have been respawned because the CI run you parked on with `gb wait ci` concluded " +
		"while you were not running.\n\n" + nudge + "\n\nPer-workflow result: " + result
	if task != "" {
		p += fmt.Sprintf("\n\nYour task is %s. Run `kd show %s` for the full context, then act on the conclusions: "+
			"fix a red run, or carry a green one forward (e.g. `gb pr ready`).", task, task)
	}
	return p
}
