package steward

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func liveGardener(id, state string, changed time.Time, sessions string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "open", UpdatedAt: changed,
		Labels: []string{"project:" + land, "role:" + GardenerRole},
		Fields: map[string]string{"project": land, "role": GardenerRole, "agent": id + "-name", "agent_state": state,
			"task_id": "tp-step-" + id, "session_count": sessions}}
}

func TestLiveAgents_QuietAndRestarts(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	d := newFarm()
	d.beads = append(d.beads,
		liveGardener("tp-a", "working", now.Add(-10*time.Minute), "1"),
		liveGardener("tp-b", "working", now.Add(-50*time.Minute), "2"),
		liveGardener("tp-c", "working", now.Add(-5*time.Minute), "5"),
		liveGardener("tp-parked", "stopping", now.Add(-3*time.Hour), "1"),
		&beadsapi.BeadDetail{ID: "tp-st", Type: "agent", Status: "open", UpdatedAt: now.Add(-2 * time.Hour), Labels: []string{"project:" + land},
			Fields: map[string]string{"project": land, "role": Role, "agent_state": "working"}},
	)
	got, err := liveAgents(context.Background(), d, land, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "tp-b" {
		t.Fatalf("live = %+v, want tp-b, tp-a, tp-c (no steward, no parked gardener)", got)
	}
	if !got[0].Quiet || got[1].Quiet || got[0].TooManyRestarts() {
		t.Errorf("tp-b quiet=%v, tp-a quiet=%v", got[0].Quiet, got[1].Quiet)
	}
	if c := got[2]; c.Restarts != 4 || !c.TooManyRestarts() {
		t.Errorf("tp-c = %+v, want 4 restarts, past the limit", c)
	}
	if line := describeLive(got[0]); !strings.Contains(line, "QUIET") || !strings.Contains(line, "50m") || !strings.Contains(line, "step tp-step-tp-b") {
		t.Errorf("line = %q", line)
	}
	if line := describeLive(got[2]); !strings.Contains(line, "4 restart(s), PAST THE LIMIT") {
		t.Errorf("line = %q", line)
	}
}

func TestRenderPrompt_LivenessAndAsk(t *testing.T) {
	l := testLand()
	l.Live = []LiveAgent{{ID: "tp-b", Name: "tp-b-name", Role: GardenerRole, State: "working", Task: "tp-s1", Age: 50 * time.Minute, Quiet: true}}
	out, err := RenderPrompt(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Live agents (oldest change first):\n- tp-b-name (tp-b, lands-gardener, step tp-s1, working): last change 50m0s ago, QUIET",
		"QUIET (no change in 45 minutes)",
		"more than 3 restarts",
		"--owner-approved <decision-id>",
		`--requesting-task <step-id>`,
		`{"id":"spawn","short":"Spawn the step anyway"},{"id":"wait","short":"Wait for credits"}`,
		`gb agent kill <agent> --reason`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q\n%s", want, out[strings.Index(out, "## The land now"):strings.Index(out, "## This wake")])
		}
	}
	l.Live = nil
	if out, _ := RenderPrompt(l); !strings.Contains(out, "Live agents: none but you.") {
		t.Error("an empty land does not say so")
	}
}
