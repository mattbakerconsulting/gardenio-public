package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// closedAgent builds a terminal agent bead with the given fields for the
// restart-condition tests.
func closedAgent(fields map[string]string) *beadsapi.BeadDetail {
	if fields == nil {
		fields = map[string]string{}
	}
	return &beadsapi.BeadDetail{ID: "gd-agt", Type: "agent", Status: "closed", Fields: fields}
}

// TestBuildAgentCardRestart_WakeTrigger asserts a wrap-up restart_when becomes a
// "Wakes …" line (parity with the Slack card's wake-trigger arm).
func TestBuildAgentCardRestart_WakeTrigger(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"wrapup": `{"restart_when":"when gd-X merges"}`,
	}))
	if got.Text != "Wakes when gd-X merges" {
		t.Errorf("Text = %q, want %q", got.Text, "Wakes when gd-X merges")
	}
}

// TestBuildAgentCardRestart_WakeTriggerAlreadyClause asserts a restart_when that
// already reads as a clause ("Waiting …") is used verbatim, not double-prefixed.
func TestBuildAgentCardRestart_WakeTriggerAlreadyClause(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"wrapup": `{"restart_when":"Waiting on decision gd-Z"}`,
	}))
	if got.Text != "Waiting on decision gd-Z" {
		t.Errorf("Text = %q, want verbatim clause", got.Text)
	}
}

// TestBuildAgentCardRestart_RespawnNextAt asserts next_respawn_at drives a
// concrete "Resumes ~HH:MM" line with the respawn budget annotation.
func TestBuildAgentCardRestart_RespawnNextAt(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"next_respawn_at": "2026-08-27T20:30:00Z",
		"respawn_count":   "2",
		"max_respawns":    "5",
	}))
	if got.Text != "Resumes ~20:30 UTC (respawn 2/5)" {
		t.Errorf("Text = %q, want %q", got.Text, "Resumes ~20:30 UTC (respawn 2/5)")
	}
}

// TestBuildAgentCardRestart_RespawnGoalUnmet asserts goal_satisfied:no with no
// concrete timestamp falls back to the generic respawn phrasing.
func TestBuildAgentCardRestart_RespawnGoalUnmet(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"wrapup": `{"goal_satisfied":"no"}`,
	}))
	if got.Text != "Resumes when respawn fires" {
		t.Errorf("Text = %q, want the generic respawn line", got.Text)
	}
}

// TestBuildAgentCardRestart_RespawnBeatsWakeTrigger asserts precedence: an armed
// respawn outranks a declared restart_when.
func TestBuildAgentCardRestart_RespawnBeatsWakeTrigger(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"next_respawn_at": "2026-08-27T20:30:00Z",
		"wrapup":          `{"restart_when":"when gd-X merges"}`,
	}))
	if got.Text != "Resumes ~20:30 UTC" {
		t.Errorf("Text = %q, want respawn to win", got.Text)
	}
}

// TestBuildAgentCardRestart_GardenerRefire asserts a gardener run with budget left
// renders the "Gardener refires (run N/M)" arm.
func TestBuildAgentCardRestart_GardenerRefire(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"schedule_runs_spawned": "3",
		"schedule_max_runs":     "8",
	}))
	if got.Text != "Gardener refires (run 3/8)" {
		t.Errorf("Text = %q, want %q", got.Text, "Gardener refires (run 3/8)")
	}
}

// TestBuildAgentCardRestart_GardenerUnbounded asserts an unbounded gardener loop
// (no positive cap) still refires.
func TestBuildAgentCardRestart_GardenerUnbounded(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"schedule_runs_spawned": "12",
		"schedule_max_runs":     "0",
	}))
	if got.Text != "Gardener refires (run 12)" {
		t.Errorf("Text = %q, want unbounded refire", got.Text)
	}
}

// TestBuildAgentCardRestart_GardenerLastRun asserts a gardener on its final run
// (spawned >= cap) promises no refire.
func TestBuildAgentCardRestart_GardenerLastRun(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"schedule_runs_spawned": "8",
		"schedule_max_runs":     "8",
	}))
	if got.Text != "" {
		t.Errorf("Text = %q, want empty on the final gardener run", got.Text)
	}
}

// TestBuildAgentCardRestart_LiveSuppressed asserts a live (non-terminal) agent
// renders no restart line — its restart state is not yet settled.
func TestBuildAgentCardRestart_LiveSuppressed(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent_state": "working",
			"wrapup":      `{"restart_when":"when gd-X merges"}`,
		},
	}
	if got := buildAgentCardRestart(bead); got.Text != "" {
		t.Errorf("Text = %q, want empty for a live agent", got.Text)
	}
}

// TestBuildAgentCardRestart_TerminalNoRestart asserts a clean terminal run with
// no restart signal renders nothing (the template omits the line).
func TestBuildAgentCardRestart_TerminalNoRestart(t *testing.T) {
	got := buildAgentCardRestart(closedAgent(map[string]string{
		"wrapup": `{"tldr":"done","accomplishments":"x"}`,
	}))
	if got.Text != "" {
		t.Errorf("Text = %q, want empty for a terminal run with no restart", got.Text)
	}
}

// TestBuildAgentCardRestart_NilSafe asserts nil / field-less input degrades to
// an empty view rather than panicking.
func TestBuildAgentCardRestart_NilSafe(t *testing.T) {
	if got := buildAgentCardRestart(nil); got.Text != "" {
		t.Errorf("nil bead Text = %q, want empty", got.Text)
	}
	if got := buildAgentCardRestart(&beadsapi.BeadDetail{ID: "x", Type: "agent"}); got.Text != "" {
		t.Errorf("field-less bead Text = %q, want empty", got.Text)
	}
}
