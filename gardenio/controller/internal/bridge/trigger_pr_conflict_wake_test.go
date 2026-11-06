package bridge

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakePRWakeDaemon is an in-memory prOwnerWakeDaemon: beads by id, forward
// dependencies by bead, and a record of mail, comments and updates.
type fakePRWakeDaemon struct {
	mu       sync.Mutex
	beads    map[string]*beadsapi.BeadDetail
	deps     map[string][]beadsapi.Dependency
	mail     []beadsapi.CreateBeadRequest
	comments map[string][]string
	updates  map[string][]beadsapi.UpdateBeadRequest
}

func newFakePRWakeDaemon(beads ...*beadsapi.BeadDetail) *fakePRWakeDaemon {
	d := &fakePRWakeDaemon{
		beads:    map[string]*beadsapi.BeadDetail{},
		deps:     map[string][]beadsapi.Dependency{},
		comments: map[string][]string{},
		updates:  map[string][]beadsapi.UpdateBeadRequest{},
	}
	for _, b := range beads {
		d.beads[b.ID] = b
	}
	return d
}

func (d *fakePRWakeDaemon) parent(child, parent string) {
	d.deps[child] = append(d.deps[child], beadsapi.Dependency{BeadID: child, DependsOnID: parent, Type: "parent-child"})
}

func (d *fakePRWakeDaemon) assigned(agent, epic string) {
	d.deps[agent] = append(d.deps[agent], beadsapi.Dependency{BeadID: agent, DependsOnID: epic, Type: "assigned"})
}

func (d *fakePRWakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.beads[id]; ok {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "not found"}
}

func (d *fakePRWakeDaemon) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deps[id], nil
}

func (d *fakePRWakeDaemon) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []beadsapi.Dependency
	for _, ds := range d.deps {
		for _, dep := range ds {
			if dep.DependsOnID == id {
				out = append(out, dep)
			}
		}
	}
	return out, nil
}

func (d *fakePRWakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res := &beadsapi.ListBeadsResult{}
	for _, b := range d.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		ok := true
		for k, v := range q.Fields {
			ok = ok && b.Fields[k] == v
		}
		for _, l := range q.Labels {
			ok = ok && slices.Contains(b.Labels, l)
		}
		if ok {
			res.Beads = append(res.Beads, b)
		}
	}
	return res, nil
}

func (d *fakePRWakeDaemon) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]*beadsapi.BeadDetail{}
	for _, id := range ids {
		if b, ok := d.beads[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func (d *fakePRWakeDaemon) FindAgentBead(_ context.Context, name string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.beads {
		if b.Type == "agent" && b.Fields["agent"] == name && b.Status != "closed" {
			return b, nil
		}
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "no agent " + name}
}

func (d *fakePRWakeDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mail = append(d.mail, req)
	return "gd-mail", nil
}

func (d *fakePRWakeDaemon) AddComment(_ context.Context, id, _, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.comments[id] = append(d.comments[id], text)
	return nil
}

func (d *fakePRWakeDaemon) UpdateBead(_ context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updates[id] = append(d.updates[id], req)
	return nil
}

func (d *fakePRWakeDaemon) AddLabel(context.Context, string, string) error { return nil }
func (d *fakePRWakeDaemon) AddDependency(context.Context, string, string, string, string) error {
	return nil
}
func (d *fakePRWakeDaemon) UpdateBeadFieldsTyped(context.Context, string, map[string]any) error {
	return nil
}

var prWakeNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func conflictingTask() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: "gd-task", Type: "task", Status: "in_progress",
		Fields: map[string]string{
			"mr_url":            "https://github.com/o/r/pull/2201",
			"mr_merge_state":    "conflicting",
			"mr_conflict_kind":  "hand",
			"mr_conflict_since": "2026-11-04T11:55:00Z",
			"mr_conflict_cause": "abc1234 #2199",
			"mr_conflict_files": `["a.go","b.go"]`,
		},
	}
}

func crewAgent(id, name, task, state string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "agent", Status: "in_progress",
		Fields: map[string]string{
			"agent": name, "task_id": task, "agent_state": state,
			"last_progress_at": prWakeNow.Add(-5 * time.Minute).Format(time.RFC3339),
		},
	}
}

func planEpic() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}}
}

func gardenerAgent(state, status string, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: "gd-gardener", Title: "gardenio-plan-gardener-for-gd-plan-x1", Type: "agent", Status: status,
		Labels:    []string{"role:plan-gardener"},
		UpdatedAt: updated,
		Fields: map[string]string{
			"agent": "gardenio-plan-gardener-for-gd-plan-x1", "task_id": "gd-plan", "agent_state": state,
		},
	}
}

func newPRWaker(d *fakePRWakeDaemon) prOwnerWaker {
	return prOwnerWaker{daemon: d, logger: slog.Default(), now: func() time.Time { return prWakeNow }}
}

// (a) A live crew on the task gets the mail; the gardener is not touched.
func TestPROwnerWake_LiveCrew(t *testing.T) {
	d := newFakePRWakeDaemon(conflictingTask(), crewAgent("gd-a1", "crew-one", "gd-task", "working"),
		planEpic(), gardenerAgent("working", "in_progress", prWakeNow))
	d.parent("gd-task", "gd-plan")
	d.assigned("gd-gardener", "gd-plan")

	res, err := newPRWaker(d).wake(context.Background(), "gd-task")
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != prOwnerCrew || res.Agent != "crew-one" {
		t.Fatalf("got %+v, want crew crew-one", res)
	}
	if len(d.mail) != 1 || d.mail[0].Assignee != "crew-one" {
		t.Fatalf("mail = %+v, want one to crew-one", d.mail)
	}
	if !strings.HasPrefix(d.mail[0].Description, "PR #2201 now conflicts with main (since 2026-11-04T11:55:00Z) on: a.go, b.go. First conflicting main commit: abc1234 #2199.") {
		t.Errorf("message = %q", d.mail[0].Description)
	}
	if c := d.comments["gd-task"]; len(c) != 1 || !strings.Contains(c[0], "Nudged crew crew-one.") {
		t.Errorf("task comments = %q", c)
	}
}

// (b) No live crew (the only one is done): the live plan gardener gets the mail.
func TestPROwnerWake_LiveGardener(t *testing.T) {
	d := newFakePRWakeDaemon(conflictingTask(), crewAgent("gd-a1", "crew-one", "gd-task", "done"),
		&beadsapi.BeadDetail{ID: "gd-phase", Type: "epic", Status: "open"},
		planEpic(), gardenerAgent("working", "in_progress", prWakeNow.Add(-time.Minute)))
	d.parent("gd-task", "gd-phase")
	d.parent("gd-phase", "gd-plan")
	d.assigned("gd-gardener", "gd-plan")

	res, err := newPRWaker(d).wake(context.Background(), "gd-task")
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != prOwnerGardener || res.Plan != "gd-plan" {
		t.Fatalf("got %+v, want gardener of gd-plan", res)
	}
	if len(d.mail) != 1 || d.mail[0].Assignee != "gardenio-plan-gardener-for-gd-plan-x1" {
		t.Fatalf("mail = %+v, want one to the gardener", d.mail)
	}
	if len(d.updates["gd-gardener"]) != 0 {
		t.Errorf("a live gardener was woken: %+v", d.updates["gd-gardener"])
	}
}

// (b) A parked gardener is woken in place with the message as its prompt.
func TestPROwnerWake_ParkedGardenerWoken(t *testing.T) {
	d := newFakePRWakeDaemon(conflictingTask(), planEpic(), gardenerAgent("done", "closed", prWakeNow.Add(-3*time.Hour)))
	d.parent("gd-task", "gd-plan")
	d.assigned("gd-gardener", "gd-plan")

	res, err := newPRWaker(d).wake(context.Background(), "gd-task")
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != prOwnerGardenerWoken {
		t.Fatalf("got %+v, want gardener-woken", res)
	}
	if len(d.mail) != 0 {
		t.Errorf("a parked gardener was mailed: %+v", d.mail)
	}
	ups := d.updates["gd-gardener"]
	if len(ups) != 1 || ups[0].Status == nil || *ups[0].Status != "in_progress" {
		t.Fatalf("gardener updates = %+v, want one reopen", ups)
	}
	if p, _ := ups[0].Fields["prompt"].(string); !strings.HasPrefix(p, "PR #2201 now conflicts") {
		t.Errorf("wake prompt = %q", p)
	}
}

// (c) No crew and no plan: only the task comment.
func TestPROwnerWake_CommentOnly(t *testing.T) {
	d := newFakePRWakeDaemon(conflictingTask())

	res, err := newPRWaker(d).wake(context.Background(), "gd-task")
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != prOwnerNone {
		t.Fatalf("got %+v, want none", res)
	}
	if len(d.mail) != 0 {
		t.Errorf("mail sent with no owner: %+v", d.mail)
	}
	if c := d.comments["gd-task"]; len(c) != 1 || !strings.Contains(c[0], "No live crew or plan gardener") {
		t.Errorf("task comments = %q", c)
	}
}

// A task that went clean before the fire ran gets nothing.
func TestPROwnerWake_NoLongerConflicting(t *testing.T) {
	task := conflictingTask()
	task.Fields["mr_merge_state"] = "clean"
	d := newFakePRWakeDaemon(task, crewAgent("gd-a1", "crew-one", "gd-task", "working"))

	if _, err := newPRWaker(d).wake(context.Background(), "gd-task"); err != nil {
		t.Fatal(err)
	}
	if len(d.mail) != 0 || len(d.comments["gd-task"]) != 0 {
		t.Errorf("clean task was acted on: mail %+v comments %q", d.mail, d.comments["gd-task"])
	}
}

func TestConflictFilesText(t *testing.T) {
	many := `["f01","f02","f03","f04","f05","f06","f07","f08","f09","f10","f11","f12"]`
	capped := `["f01","f02","f03","f04","f05","f06","f07","f08","f09","f10","f11","…+40 more"]`
	for _, tc := range []struct{ raw, want string }{
		{`["a.go"]`, "a.go"},
		{many, "f01, f02, f03, f04, f05, f06, f07, f08, f09, f10 and 2 more"},
		{capped, "f01, f02, f03, f04, f05, f06, f07, f08, f09, f10 and 41 more"},
		{"", "(files unknown)"},
	} {
		if got := conflictFilesText(tc.raw, prConflictMaxFiles); got != tc.want {
			t.Errorf("conflictFilesText(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// The trigger's conditions fire once per conflict episode: on the transition
// to conflicting, not on a later unrelated update of the still-conflicting
// bead, and never for a mechanical-only conflict (Task 3.2's).
func TestPROwnerWake_TriggerFiresOncePerEpisode(t *testing.T) {
	fields := map[string]string{"mr_merge_state": "conflicting", "mr_conflict_kind": "hand"}
	mech := map[string]string{"mr_merge_state": "conflicting", "mr_conflict_kind": "mechanical"}
	src := NewBeadStateTriggerSource(slog.Default())
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-task", Type: "task", Fields: fields,
			Changes: map[string]any{"mr_merge_state": "conflicting", "mr_conflict_kind": "hand", "mr_conflict_files": `["a.go"]`}}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-task", Type: "task", Fields: fields,
			Changes: map[string]any{"title": "renamed"}}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-task", Type: "task", Fields: fields,
			Changes: map[string]any{"mr_conflict_files": `["a.go","b.go"]`}}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-other", Type: "task", Fields: mech,
			Changes: map[string]any{"mr_merge_state": "conflicting"}}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-mr", Type: "merge-request", Fields: fields,
			Changes: map[string]any{"mr_merge_state": "conflicting"}}},
	}
	src.mu.Unlock()

	trig := TriggerDef{ID: "trig-pr-conflict", Action: TriggerActionWakePROwner, SourceData: beadTriggerData{
		EventTypes: []string{"updated"},
		Conditions: []beadCondition{
			{Field: "mr_merge_state", Op: "changed_to", Value: "conflicting"},
			{Field: "mr_conflict_kind", Op: "==", Value: "hand"},
			{Field: "type", Op: "in", Value: "task,bug,feature,chore"},
		},
	}}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Fields["source_bead_id"] != "gd-task" {
		t.Fatalf("matches = %+v, want exactly the gd-task transition", matches)
	}
}
