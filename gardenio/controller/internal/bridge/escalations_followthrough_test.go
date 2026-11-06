package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// followThroughFixture is a resolved success escalation whose requesting crew
// (test-agent, agent_state=done) is gone, so handleClosed's nudge fails and the
// follow-through gate decides what happens next (gd-nlR2YIDhB7).
type followThroughFixture struct {
	client *mockEscalationBeadClient
	esc    *Escalations
	nudged []string // agents the stubbed nudge delivered to
	failTo map[string]bool
}

func newFollowThroughFixture(taskStatus, taskAssignee string) *followThroughFixture {
	f := &followThroughFixture{
		client: newMockEscalationBeadClient(),
		failTo: map[string]bool{"test-agent": true},
	}
	f.client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
		ID:     "gd-agent-req",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       "test-agent",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-task-1",
			"role":        "crew",
			"seat_key":    "gardenio/crew/test-agent",
		},
	}
	f.client.beads["gd-task-1"] = &beadsapi.BeadDetail{
		ID:       "gd-task-1",
		Type:     "task",
		Status:   taskStatus,
		Assignee: taskAssignee,
	}
	f.esc = NewEscalations(EscalationsConfig{Daemon: f.client, Logger: slog.Default()})
	f.esc.nudge = func(_ context.Context, agent, _ string) error {
		if f.failTo[agent] {
			return errors.New("agent not running")
		}
		f.nudged = append(f.nudged, agent)
		return nil
	}
	return f
}

func (f *followThroughFixture) addAgent(id string, fields map[string]string) {
	f.client.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "open", Fields: fields}
}

func (f *followThroughFixture) close(t *testing.T) {
	t.Helper()
	event := BeadEvent{
		ID:   "gd-esc-ft",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type":   "github_pr_merge",
			"result_status":    "success",
			"result":           `{"output":"merged"}`,
			"requesting_agent": "gd-agent-req",
			"requesting_task":  "gd-task-1",
		},
	}
	data, _ := json.Marshal(map[string]any{"bead": event})
	f.esc.handleClosed(context.Background(), data)
}

func (f *followThroughFixture) escalationNotes() []string {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	var out []string
	for _, c := range f.client.comments {
		if c.beadID == "gd-esc-ft" {
			out = append(out, c.text)
		}
	}
	return out
}

// A requesting task that is already closed (gd-shUO6KOGKi, or the executor's
// close_requesting_task in gd-3SA34wWj3u) gets no follow-through crew, and the
// escalation says why.
func TestEscalations_FollowThrough_SkipsClosedTask(t *testing.T) {
	f := newFollowThroughFixture("closed", "")
	f.close(t)

	if n := len(f.client.spawnedAgents); n != 0 {
		t.Fatalf("spawned %d agents for a closed requesting task, want 0", n)
	}
	notes := f.escalationNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "gd-task-1 is already closed") {
		t.Errorf("escalation notes = %q, want one note that the task is already closed", notes)
	}
}

// gd-ku3EREJlf1: a plan gardener re-dispatched the seat onto the task, then the
// escalation resolved 3 min later. The fresh crew holds the task (assignee and
// task_id), so it is nudged with the verdict and no second crew is spawned.
func TestEscalations_FollowThrough_NudgesGardenerRedispatchedCrew(t *testing.T) {
	f := newFollowThroughFixture("in_progress", "test-agent-r2d")
	f.addAgent("gd-agent-new", map[string]string{
		"agent":            "test-agent-r2d",
		"agent_state":      "spawning",
		"project":          "gardenio",
		"task_id":          "gd-task-1",
		"role":             "crew",
		"last_progress_at": time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339),
	})
	f.close(t)

	if n := len(f.client.spawnedAgents); n != 0 {
		t.Fatalf("spawned %d agents beside a live crew on the task, want 0", n)
	}
	if len(f.nudged) != 1 || f.nudged[0] != "test-agent-r2d" {
		t.Fatalf("nudged %q, want the live crew test-agent-r2d", f.nudged)
	}
	notes := f.escalationNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "nudged live agent test-agent-r2d") {
		t.Errorf("escalation notes = %q, want one note naming the nudged agent", notes)
	}
}

// A live pod on the requester's seat counts as staffed even when it has not
// claimed the task yet (no assignee, task_id elsewhere).
func TestEscalations_FollowThrough_NudgesLiveSeat(t *testing.T) {
	f := newFollowThroughFixture("open", "")
	f.addAgent("gd-agent-seat", map[string]string{
		"agent":       "test-agent-x7q",
		"agent_state": "working",
		"project":     "gardenio",
		"role":        "crew",
		"seat_key":    "gardenio/crew/test-agent",
	})
	f.close(t)

	if n := len(f.client.spawnedAgents); n != 0 {
		t.Fatalf("spawned %d agents beside a live pod on the seat, want 0", n)
	}
	if len(f.nudged) != 1 || f.nudged[0] != "test-agent-x7q" {
		t.Fatalf("nudged %q, want the live seat pod test-agent-x7q", f.nudged)
	}
}

// An agent bead that is open but stalled (no progress past the single-claim
// threshold) or terminal is not live, so the follow-through crew still spawns.
func TestEscalations_FollowThrough_SpawnsPastStalledOrTerminalIncumbent(t *testing.T) {
	f := newFollowThroughFixture("in_progress", "test-agent-old")
	f.addAgent("gd-agent-old", map[string]string{
		"agent":            "test-agent-old",
		"agent_state":      "working",
		"task_id":          "gd-task-1",
		"last_progress_at": time.Now().Add(-2 * beadsapi.ProgressStaleThreshold).UTC().Format(time.RFC3339),
	})
	f.addAgent("gd-agent-fail", map[string]string{
		"agent":       "test-agent-dead",
		"agent_state": "failed",
		"task_id":     "gd-task-1",
	})
	f.close(t)

	if len(f.nudged) != 0 {
		t.Errorf("nudged %q, want no incumbent nudge", f.nudged)
	}
	if n := len(f.client.spawnedAgents); n != 1 {
		t.Fatalf("spawned %d agents, want 1 follow-through crew", n)
	}
}

// If the incumbent cannot be nudged after all, the gate falls back to the
// usual spawn rather than dropping the verdict.
func TestEscalations_FollowThrough_SpawnsWhenIncumbentNudgeFails(t *testing.T) {
	f := newFollowThroughFixture("in_progress", "test-agent-r2d")
	f.addAgent("gd-agent-new", map[string]string{
		"agent":       "test-agent-r2d",
		"agent_state": "working",
		"task_id":     "gd-task-1",
	})
	f.failTo["test-agent-r2d"] = true
	f.close(t)

	if n := len(f.client.spawnedAgents); n != 1 {
		t.Fatalf("spawned %d agents, want 1 follow-through crew after the failed incumbent nudge", n)
	}
	if notes := f.escalationNotes(); len(notes) != 0 {
		t.Errorf("escalation notes = %q, want none when the spawn path ran", notes)
	}
}

// An open, unstaffed task keeps today's behavior: one follow-through crew.
func TestEscalations_FollowThrough_SpawnsForOpenUnstaffedTask(t *testing.T) {
	f := newFollowThroughFixture("in_progress", "test-agent")
	f.close(t)

	if n := len(f.client.spawnedAgents); n != 1 {
		t.Fatalf("spawned %d agents, want 1 follow-through crew", n)
	}
}
