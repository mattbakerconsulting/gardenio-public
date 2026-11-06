package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// agentCardRestartView is the restart-condition line rendered on the HTML agent
// card (type_agent.html), mirroring the Slack card's "↻ Restart:" line (parent
// epic ka-0000000754, "think how cool it would be if the agent printed its
// restart condition"; parity task ka-0000008218).
//
// It describes when and why the controller will bring this agent identity back.
// Because a gardener's individual RUN is itself a type=agent bead carrying the
// schedule_* fields, the same card — and this same view — covers gardener cards
// too (the "Gardener refires" arm below). Empty Text suppresses the whole line
// so terminal runs with no expected restart render unchanged.
type agentCardRestartView struct {
	// Text is the human-readable restart condition, e.g.
	// "Resumes ~20:30 (respawn 2/5)", "Wakes when ka-X merges", or
	// "Gardener refires (run 3/8)". Already trimmed. Empty ⇒ suppress the line.
	Text string
}

// buildAgentCardRestart derives the restart-condition line for the HTML agent
// card, porting the Slack bridge's restartConditionLine ladder
// (internal/bridge/bot_agents_wrapup.go) so the two surfaces stay in lockstep.
//
// Three resume mechanisms are covered, in the SAME precedence the Slack card
// uses (respawn > wake-trigger > gardener refire):
//
//   - respawn        "Resumes ~20:30 (respawn 2/5)" — a terminal_goal not met
//     (goal_satisfied:no) or an explicit respawn_after; next_respawn_at /
//     respawn_count / max_respawns are the controller's scheduled-respawn state.
//   - wake-trigger   "Wakes when ka-X merges"       — the agent's own declared
//     restart_when (bead ka-0000007398).
//   - gardener refire "Gardener refires (run 3/8)"    — a scheduled gardener loop
//     with runs left in its budget (schedule_runs_spawned / schedule_max_runs).
//
// Gated on the same terminal condition as buildAgentCardTLDR: a live agent has
// no settled restart state yet, so its card keeps the working-state header
// untouched. Returns a zero view (empty Text) whenever no restart is expected.
func buildAgentCardRestart(bead *beadsapi.BeadDetail) agentCardRestartView {
	if bead == nil || bead.Fields == nil {
		return agentCardRestartView{}
	}
	// Only terminal runs have a settled wrap-up / respawn decision; a live agent
	// is still running, not restarting.
	live := bead.Status != "closed" && isAgentStateIn(bead.Fields["agent_state"], agentStateAlive)
	if live {
		return agentCardRestartView{}
	}

	var w struct {
		RestartWhen   string `json:"restart_when"`
		RespawnAfter  string `json:"respawn_after"`
		GoalSatisfied string `json:"goal_satisfied"`
	}
	if wrapup := strings.TrimSpace(bead.Fields["wrapup"]); wrapup != "" {
		_ = json.Unmarshal([]byte(wrapup), &w)
	}

	if line := respawnRestartText(w.RespawnAfter, w.GoalSatisfied, bead.Fields); line != "" {
		return agentCardRestartView{Text: line}
	}
	if line := wakeTriggerRestartText(w.RestartWhen); line != "" {
		return agentCardRestartView{Text: line}
	}
	if line := gardenerRefireRestartText(bead.Fields); line != "" {
		return agentCardRestartView{Text: line}
	}
	return agentCardRestartView{}
}

// respawnRestartText renders the reconciler-respawn arm, or "" when no respawn
// is armed. Armed-ness is deliberately narrow: only next_respawn_at (controller
// state), goal_satisfied:no, or a respawn_after count — NOT restart_when, which
// is the wake-trigger arm below.
func respawnRestartText(respawnAfter, goalSatisfied string, fields map[string]string) string {
	nextAt := strings.TrimSpace(fields["next_respawn_at"])
	respawnCount := strings.TrimSpace(fields["respawn_count"])
	maxRespawns := strings.TrimSpace(fields["max_respawns"])
	respawnAfter = strings.TrimSpace(respawnAfter)
	goalUnmet := strings.EqualFold(strings.TrimSpace(goalSatisfied), "no")

	if nextAt == "" && respawnAfter == "" && !goalUnmet {
		return ""
	}

	// When the respawn lands: prefer the controller's concrete timestamp, fall
	// back to the agent's requested delay, else a generic phrasing.
	when := "Resumes when respawn fires"
	switch {
	case nextAt != "":
		when = fmt.Sprintf("Resumes ~%s", restartNextAtDisplay(nextAt))
	case respawnAfter != "":
		if strings.EqualFold(respawnAfter, "immediate") || respawnAfter == "0" {
			when = "Resumes immediately"
		} else {
			when = fmt.Sprintf("Resumes after %s", respawnAfter)
		}
	}

	if budget := restartRespawnBudget(respawnCount, maxRespawns); budget != "" {
		when += " " + budget
	}
	return when
}

// wakeTriggerRestartText renders the wake-trigger arm from the agent's own
// declared restart_when (bead ka-0000007398). The text is rendered verbatim
// after a "Wakes " lead-in, unless the agent already phrased it as a full
// clause ("Wakes …"/"Waiting …"/"Resumes …"), in which case it is used as-is so
// we don't produce "Wakes Wakes when …". Returns "" when unset.
func wakeTriggerRestartText(restartWhen string) string {
	rw := strings.TrimSpace(restartWhen)
	if rw == "" {
		return ""
	}
	switch lower := strings.ToLower(rw); {
	case strings.HasPrefix(lower, "wake"),
		strings.HasPrefix(lower, "wait"),
		strings.HasPrefix(lower, "resume"):
		return rw
	default:
		return "Wakes " + rw
	}
}

// gardenerRefireRestartText renders the gardener-loop arm: a scheduled gardener
// with runs remaining in its budget refires on its cadence. Returns "" for a
// non-gardener agent (no schedule_runs_spawned), or a gardener on its LAST run
// (runs_spawned >= max_runs with a positive cap) — promising a restart there
// would be wrong. A non-positive/absent cap is an unbounded loop that always
// refires.
func gardenerRefireRestartText(fields map[string]string) string {
	spawnedStr := strings.TrimSpace(fields["schedule_runs_spawned"])
	if spawnedStr == "" {
		return "" // not a gardener run
	}
	spawned, err := strconv.Atoi(spawnedStr)
	if err != nil {
		return ""
	}
	maxRuns, _ := strconv.Atoi(strings.TrimSpace(fields["schedule_max_runs"]))

	// Bounded budget already fully spent → this is the final run, no refire.
	if maxRuns > 0 && spawned >= maxRuns {
		return ""
	}

	line := "Gardener refires"
	switch {
	case maxRuns > 0:
		line += fmt.Sprintf(" (run %d/%d)", spawned, maxRuns)
	case spawned > 0:
		line += fmt.Sprintf(" (run %d)", spawned)
	}
	return line
}

// restartNextAtDisplay normalizes an RFC3339 next_respawn_at into a compact
// display string. On parse failure it returns the raw value so nothing is lost.
func restartNextAtDisplay(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.UTC().Format("15:04 MST")
}

// restartRespawnBudget formats the respawn budget counters into "(respawn N/M)"
// (or a partial form), or "" when neither counter is a usable value.
func restartRespawnBudget(count, max string) string {
	switch {
	case count != "" && max != "":
		return fmt.Sprintf("(respawn %s/%s)", count, max)
	case count != "":
		return fmt.Sprintf("(respawn %s)", count)
	case max != "":
		return fmt.Sprintf("(respawn cap %s)", max)
	}
	return ""
}
