package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

type memComment struct{ bead, author, text string }

type memSpawn struct {
	name, project, task, role, prompt string
	fields                            map[string]string
}

// UpdateBead applies a status change and an RFC-7386 field merge (nil deletes).
func (d *memDaemon) UpdateBead(_ context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.beads[id]
	if !ok {
		return errors.New("not found")
	}
	if req.Status != nil {
		b.Status = *req.Status
	}
	for k, v := range req.Fields {
		if v == nil {
			delete(b.Fields, k)
		} else {
			b.Fields[k] = fmt.Sprint(v)
		}
	}
	return nil
}

func (d *memDaemon) AddComment(_ context.Context, id, author, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.comments = append(d.comments, memComment{id, author, text})
	return nil
}

// SpawnAgent mints an open agent bead, like the daemon with no live namesake.
func (d *memDaemon) SpawnAgent(_ context.Context, name, project, task, role, prompt string, extra ...map[string]string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.spawnErr != nil {
		return "", d.spawnErr
	}
	fields := map[string]string{"agent": name, "project": project, "task_id": task, "role": role}
	for _, m := range extra {
		for k, v := range m {
			fields[k] = v
		}
	}
	d.spawns = append(d.spawns, memSpawn{name, project, task, role, prompt, fields})
	d.add(&beadsapi.BeadDetail{Type: "agent", Status: "open", Title: name, Fields: fields})
	return d.order[len(d.order)-1], nil
}

// parked adds an agent bead parked on a CI wait, with extra fields on top.
func (c *ciHarness) parked(id, status string, extra map[string]string) {
	fields := map[string]string{
		"agent":                      "agent-" + id,
		"project":                    "gardenio",
		"role":                       "crew",
		"task_id":                    "gd-task1",
		beadsapi.FieldCIWaitState:    beadsapi.CIWaitStatePending,
		beadsapi.FieldCIWaitKey:      ciKey,
		beadsapi.FieldCIWaitTask:     "gd-task1",
		beadsapi.FieldCIWaitDeadline: ciT0.Add(2 * time.Hour).Format(time.RFC3339),
	}
	for k, v := range extra {
		fields[k] = v
	}
	c.d.add(&beadsapi.BeadDetail{ID: id, Type: "agent", Status: status, Title: "agent-" + id, Fields: fields})
}

// dormant is what `gb wait ci` + `gb stop` leave: closed, successor=same.
var dormant = map[string]string{
	beadsapi.FieldSuccessor:     beadsapi.SuccessorSame,
	beadsapi.FieldAgentState:    "done",
	beadsapi.FieldNextRespawnAt: "2026-10-31T12:25:00Z",
	beadsapi.FieldRespawnCount:  "1",
}

func (c *ciHarness) settle() {
	c.at(5 * time.Minute)
	c.h.sweepOnce(context.Background())
}

func (c *ciHarness) taskComments() []string {
	var out []string
	for _, cm := range c.d.comments {
		if cm.bead == "gd-task1" {
			out = append(out, cm.text)
		}
	}
	return out
}

func TestCIWake_GreenWakesTheDormantIdentityInPlace(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.deliver(ciRepo, ciSHA, 102, "checks", 1, "completed", "success")
	c.parked("gd-agentA", "closed", dormant)
	c.settle()

	b, _ := c.d.GetBead(context.Background(), "gd-agentA")
	if b.Status != "open" {
		t.Fatalf("dormant identity not reopened: status %q", b.Status)
	}
	f := b.Fields
	if f[beadsapi.FieldWakeReason] != beadsapi.WakeReasonCI {
		t.Errorf("wake_reason = %q, want ci", f[beadsapi.FieldWakeReason])
	}
	if _, ok := f[beadsapi.FieldNextRespawnAt]; ok {
		t.Error("next_respawn_at not cleared on the in-place wake")
	}
	if f[beadsapi.FieldRespawnCount] != "1" {
		t.Errorf("respawn_count = %q: an event wake must not spend the budget", f[beadsapi.FieldRespawnCount])
	}
	if f[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateResolved {
		t.Errorf("ci_wait_state = %q, want resolved (left for gb wait ci --result)", f[beadsapi.FieldCIWaitState])
	}
	if len(c.d.spawns) != 0 {
		t.Errorf("woken in place but also spawned: %+v", c.d.spawns)
	}
	got := c.taskComments()
	if len(got) != 1 || !strings.Contains(got[0], "CI concluded GREEN") ||
		!strings.Contains(got[0], "ci (run 101)") || !strings.Contains(got[0], "gb wait ci --result") {
		t.Errorf("task nudge = %q", got)
	}
}

func TestCIWake_RedNamesTheFailingRun(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "failure")
	c.deliver(ciRepo, ciSHA, 102, "checks", 1, "completed", "success")
	c.parked("gd-agentA", "closed", dormant)
	c.settle()

	got := c.taskComments()
	if len(got) != 1 {
		t.Fatalf("task nudges = %q, want one", got)
	}
	for _, want := range []string{"CI concluded RED", "Red: ci (run 101): failure.", "Green: checks (run 102)."} {
		if !strings.Contains(got[0], want) {
			t.Errorf("nudge %q lacks %q", got[0], want)
		}
	}
}

func TestCIWake_TimedOutStillWakes(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "in_progress", "")
	c.parked("gd-agentA", "closed", dormant)
	c.at(3 * time.Hour)
	c.h.sweepOnce(context.Background())

	b, _ := c.d.GetBead(context.Background(), "gd-agentA")
	if b.Status != "open" || b.Fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateTimedOut {
		t.Fatalf("timed-out wait: status %q state %q, want open + timed_out", b.Status, b.Fields[beadsapi.FieldCIWaitState])
	}
	got := c.taskComments()
	if len(got) != 1 || !strings.Contains(got[0], "TIMED OUT") || !strings.Contains(got[0], "Not concluded: ci (run 101): in_progress.") {
		t.Errorf("task nudge = %q", got)
	}
}

func TestCIWake_WakesOnceThenTheWaitIsNoLongerPending(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.parked("gd-agentA", "closed", nil) // terminal: spawned, not reopened
	c.settle()
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success") // redelivery
	c.at(10 * time.Minute)
	c.h.sweepOnce(context.Background())
	if len(c.d.spawns) != 1 || len(c.taskComments()) != 1 {
		t.Errorf("woken %d times with %d nudges, want once", len(c.d.spawns), len(c.taskComments()))
	}
}

func TestCIWake_FrozenAgentIsThawed(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.parked("gd-agentA", "open", map[string]string{
		beadsapi.FieldAgentState: "stopping",
		"runtime_class":          "runsc",
		"plan_id":                "gd-plan1",
		"operated_by":            "oidc:op@example.com",
	})
	c.settle()

	b, _ := c.d.GetBead(context.Background(), "gd-agentA")
	if b.Status != "closed" {
		t.Errorf("frozen bead status %q: it must be closed so the same-name spawn is not deduped", b.Status)
	}
	if len(c.d.spawns) != 1 {
		t.Fatalf("spawns = %+v, want one thaw spawn", c.d.spawns)
	}
	s := c.d.spawns[0]
	if s.name != "agent-gd-agentA" || s.task != "gd-task1" || s.project != "gardenio" || s.role != "crew" {
		t.Errorf("spawn = %+v, want the same agent on its task", s)
	}
	for k, want := range map[string]string{
		"runtime_class":          "runsc",
		beadsapi.FieldWakeReason: beadsapi.WakeReasonCI,
		"spawn_source":           ciWaitSpawnSource,
		"spawned_by":             "gd-agentA",
		"plan_id":                "gd-plan1",
		"operated_by":            "oidc:op@example.com",
	} {
		if s.fields[k] != want {
			t.Errorf("spawn field %s = %q, want %q", k, s.fields[k], want)
		}
	}
	if !strings.Contains(s.prompt, `"conclusion":"success"`) {
		t.Errorf("spawn prompt lacks the per-workflow result: %q", s.prompt)
	}
}

func TestCIWake_FailedThawSpawnReopensTheFrozenBead(t *testing.T) {
	d := newMemDaemon()
	d.spawnErr = errors.New("daemon refused")
	c := newCIHarness(t, d)
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.parked("gd-agentA", "open", map[string]string{beadsapi.FieldAgentState: "stopping"})
	c.settle()

	b, _ := d.GetBead(context.Background(), "gd-agentA")
	if b.Status != "open" || b.Fields[beadsapi.FieldAgentState] != "stopping" {
		t.Errorf("after a failed thaw: status %q agent_state %q, want the frozen bead back open", b.Status, b.Fields[beadsapi.FieldAgentState])
	}
	if b.Fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateResolved {
		t.Errorf("ci_wait_state = %q: the resolution stands even if the wake failed", b.Fields[beadsapi.FieldCIWaitState])
	}
	if len(c.taskComments()) != 0 {
		t.Errorf("nudged although nothing woke: %q", c.taskComments())
	}
}

func TestCIWake_TerminalAgentIsSpawnedWithItsSlackRouting(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.parked("gd-agentA", "closed", map[string]string{
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1700000000.000100",
		"slack_user_id":        "U0OP",
	})
	c.settle()
	if len(c.d.spawns) != 1 {
		t.Fatalf("spawns = %+v, want one", c.d.spawns)
	}
	s := c.d.spawns[0]
	for k, want := range map[string]string{
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1700000000.000100",
		"slack_user_id":        "U0OP",
		"operated_by":          "slack:U0OP",
	} {
		if s.fields[k] != want {
			t.Errorf("spawn field %s = %q, want %q", k, s.fields[k], want)
		}
	}
	if _, ok := s.fields["runtime_class"]; ok {
		t.Error("a non-frozen spawn must not be pinned to a runtime class")
	}
}

func TestCIWake_NeverSpawnsIntoTheE2EProject(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.parked("gd-agentA", "closed", map[string]string{"project": e2eTestProject})
	c.settle()
	if len(c.d.spawns) != 0 || len(c.taskComments()) != 0 {
		t.Errorf("e2e fixture woke: spawns %+v, nudges %q", c.d.spawns, c.taskComments())
	}
}

func TestCIWake_LaptopAgentIsNeverWokenAsAPod(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	laptop := map[string]string{"execution_target": "laptop"}
	for k, v := range dormant {
		laptop[k] = v
	}
	c.parked("gd-agentA", "closed", laptop)
	c.settle()
	b, _ := c.d.GetBead(context.Background(), "gd-agentA")
	if b.Status != "closed" || len(c.d.spawns) != 0 {
		t.Errorf("laptop agent woken as a pod: status %q, spawns %+v", b.Status, c.d.spawns)
	}
	if b.Fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateResolved {
		t.Errorf("ci_wait_state = %q, want resolved for the runner's next session", b.Fields[beadsapi.FieldCIWaitState])
	}
}

// task adds the wait's task bead, gd-task1, in status with fields.
func (c *ciHarness) task(status string, fields map[string]string) {
	c.d.add(&beadsapi.BeadDetail{ID: "gd-task1", Type: "task", Status: status, Title: "task", Fields: fields})
}

func TestCIWake_FinishedTaskIsNotWoken(t *testing.T) {
	for name, tc := range map[string]struct {
		status string
		fields map[string]string
	}{
		"closed":         {"closed", nil},
		"done":           {"done", nil},
		"awaits_release": {"in_progress", map[string]string{beadsapi.AwaitsReleaseField: "true"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCIHarness(t, newMemDaemon())
			c.deliver(ciRepo, ciSHA, 101, "ci", 1, "in_progress", "")
			c.task(tc.status, tc.fields)
			c.parked("gd-agentA", "closed", nil)
			c.at(3 * time.Hour) // the reported case: a stale wait times out
			c.h.sweepOnce(context.Background())

			b, _ := c.d.GetBead(context.Background(), "gd-agentA")
			if b.Status != "closed" || len(c.d.spawns) != 0 {
				t.Errorf("finished task woke its agent: status %q, spawns %+v", b.Status, c.d.spawns)
			}
			if len(c.taskComments()) != 0 {
				t.Errorf("nudged a finished task: %q", c.taskComments())
			}
			if b.Fields[beadsapi.FieldCIWaitState] != beadsapi.CIWaitStateTimedOut {
				t.Errorf("ci_wait_state = %q, want timed_out (settled, never re-swept)", b.Fields[beadsapi.FieldCIWaitState])
			}
		})
	}
}

func TestCIWake_OpenTaskStillWakes(t *testing.T) {
	c := newCIHarness(t, newMemDaemon())
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.task("in_progress", map[string]string{beadsapi.AwaitsReleaseField: "false"})
	c.parked("gd-agentA", "closed", nil)
	c.settle()
	if len(c.d.spawns) != 1 || len(c.taskComments()) != 1 {
		t.Errorf("open task: %d spawns, %d nudges, want one each", len(c.d.spawns), len(c.taskComments()))
	}
}

func TestCIWake_TaskReadFailureStillWakes(t *testing.T) {
	c := newCIHarness(t, newMemDaemon()) // no gd-task1: GetBead errors
	c.deliver(ciRepo, ciSHA, 101, "ci", 1, "completed", "success")
	c.parked("gd-agentA", "closed", nil)
	c.settle()
	if len(c.d.spawns) != 1 {
		t.Errorf("task read failed and the agent was stranded: spawns %+v", c.d.spawns)
	}
}
