package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// planCardEvent is the closed SSE event for a plan's run-it card, filed the
// way the nursery backfill files one by hand: the plan as requesting_task and
// the steward's "Plan <id>: <title> - run it?" question.
func planCardEvent(fields map[string]string) []byte {
	f := map[string]string{
		"prompt":                   "Plan gd-plan-mobile: Lands mobile readiness pass - run it?",
		"chosen":                   "approve",
		"requesting_task":          "gd-plan-mobile",
		"requesting_agent_bead_id": "bd-req-agent",
	}
	for k, v := range fields {
		if v == "" {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	return marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-plan-card",
		Type:     "decision",
		Assignee: "gardenio-standup-convergence-dead",
		Labels:   []string{"project:gardenio"},
		Fields:   f,
	})
}

// TestDecisions_HandleClosed_PlanCardResumeRunsPlanAnswer is the regression for
// gd-I9ktuYVV2Y: the owner approved a nursery run-it card, the backfill agent
// was resumed with no word of gb lands plan answer, closed done, and the plan
// stayed ready with no gardener. The resume prompt must name the command.
func TestDecisions_HandleClosed_PlanCardResumeRunsPlanAnswer(t *testing.T) {
	for _, chosen := range []string{"approve", "change", "drop"} {
		t.Run(chosen, func(t *testing.T) {
			daemon := newMockDaemon()
			seedDismissDecisionFixture(daemon, "")
			d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})

			d.handleClosed(context.Background(), planCardEvent(map[string]string{"chosen": chosen}))

			spawned := findDecisionResumeSpawn(daemon)
			if spawned == nil {
				t.Fatal("expected a decision-resume respawn for the answered plan card")
			}
			prompt := spawned.Fields["prompt"]
			if !strings.Contains(prompt, "`gb lands plan answer gardenio dec-plan-card`") {
				t.Errorf("resume prompt must tell the agent to run gb lands plan answer gardenio dec-plan-card; got:\n%s", prompt)
			}
			if !strings.Contains(prompt, "gd-plan-mobile") {
				t.Errorf("resume prompt must name the plan gd-plan-mobile; got:\n%s", prompt)
			}
		})
	}
}

// TestDecisions_HandleClosed_PlanCardPromptBackfilled covers a close event that
// dropped the question: the resume reads it off the stored decision.
func TestDecisions_HandleClosed_PlanCardPromptBackfilled(t *testing.T) {
	daemon := newMockDaemon()
	seedDismissDecisionFixture(daemon, "")
	daemon.beads["dec-plan-card"] = &beadsapi.BeadDetail{
		ID: "dec-plan-card", Type: "decision", Status: "closed",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":          "Plan gd-plan-mobile: Lands mobile readiness pass - run it?",
			"chosen":          "approve",
			"requesting_task": "gd-plan-mobile",
		},
	}
	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})

	d.RespawnForDecision(context.Background(), "gardenio-standup-convergence-dead", *ParseBeadEvent(planCardEvent(map[string]string{"prompt": ""})), "Decision dec-plan-card resolved\nChosen: approve")

	spawned := findDecisionResumeSpawn(daemon)
	if spawned == nil {
		t.Fatal("expected a decision-resume respawn")
	}
	if !strings.Contains(spawned.Fields["prompt"], "gb lands plan answer gardenio dec-plan-card") {
		t.Errorf("resume prompt must name gb lands plan answer from the stored question; got:\n%s", spawned.Fields["prompt"])
	}
}

// TestDecisions_HandleClosed_NonPlanCardResumeNoPlanAnswer keeps the note off
// every other decision: an ordinary gate, a dismissed plan card (the answer
// changes nothing), and a "Plan X" question about a different bead.
func TestDecisions_HandleClosed_NonPlanCardResumeNoPlanAnswer(t *testing.T) {
	cases := map[string]map[string]string{
		"gate decision":     {"prompt": "Approve the cross-post to #daily-stand-up?"},
		"dismissed card":    {"chosen": "dismissed"},
		"other plan's card": {"requesting_task": "gd-some-step"},
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedDismissDecisionFixture(daemon, "")
			d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})

			d.handleClosed(context.Background(), planCardEvent(fields))

			spawned := findDecisionResumeSpawn(daemon)
			if spawned == nil {
				t.Fatal("expected a decision-resume respawn")
			}
			if strings.Contains(spawned.Fields["prompt"], "gb lands plan answer") {
				t.Errorf("resume prompt must not ask for gb lands plan answer; got:\n%s", spawned.Fields["prompt"])
			}
		})
	}
}

func TestPlanAnswerNote(t *testing.T) {
	q := "Plan gd-U4BtfWKjvc: Lands mobile readiness pass: every Lands surface is usable on a 390 px phone (portrait and landscape, iOS Safari and ... - run it?"
	if got := planAnswerNote(q, "gd-U4BtfWKjvc", "gardenio", "gd-2xEhswSNcz"); !strings.Contains(got, "`gb lands plan answer gardenio gd-2xEhswSNcz`") {
		t.Errorf("note for the gd-2xEhswSNcz card = %q, want the gb lands plan answer command", got)
	}
	if got := planAnswerNote(q, "gd-U4BtfWKjvc", "", "gd-2xEhswSNcz"); got != "" {
		t.Errorf("no land: note = %q, want empty", got)
	}
	if got := planAnswerNote("Plan gd-U4BtfWKjvc: mobile - ship it?", "gd-U4BtfWKjvc", "gardenio", "d"); got != "" {
		t.Errorf("not a run-it question: note = %q, want empty", got)
	}
}
