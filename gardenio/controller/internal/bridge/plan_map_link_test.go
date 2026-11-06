package bridge

// plan_map_link_test.go pins gd-BJLVTSlAbO: a decision card that points at a
// plan carries exactly one "Open on the map" URL button to
// <viewer>/garden?at=<project>/<plan-id>, and a card that does not resolve to
// a plan carries none.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// depsDaemon is mockDaemon with the optional dependencyLister capability, so a
// step's parent can be read the way the production client reads it.
type depsDaemon struct {
	*mockDaemon
	deps map[string][]beadsapi.Dependency
}

func (d *depsDaemon) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	return d.deps[id], nil
}

// mapButtons are the map buttons on a posted card.
func mapButtons(t *testing.T, blocksJSON string) []*slack.ButtonBlockElement {
	t.Helper()
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(blocksJSON), &blocks); err != nil {
		t.Fatal(err)
	}
	var out []*slack.ButtonBlockElement
	for _, bl := range blocks.BlockSet {
		ab, ok := bl.(*slack.ActionBlock)
		if !ok || ab.Elements == nil {
			continue
		}
		for _, el := range ab.Elements.ElementSet {
			if btn, ok := el.(*slack.ButtonBlockElement); ok && btn.ActionID == planMapActionID {
				out = append(out, btn)
			}
		}
	}
	return out
}

func seedPlanBeads(daemon *mockDaemon) {
	daemon.beads["gd-PlanRel01"] = &beadsapi.BeadDetail{
		ID: "gd-PlanRel01", Type: "epic", Status: "open", Title: "Releases",
		Labels: []string{"plan", "project:gardenio"},
	}
	daemon.beads["gd-StepRel01"] = &beadsapi.BeadDetail{
		ID: "gd-StepRel01", Type: "task", Status: "open", Title: "Cut the tag",
		Labels: []string{"project:gardenio"}, ParentID: "gd-PlanRel01",
	}
	daemon.beads["gd-StepRel02"] = &beadsapi.BeadDetail{
		ID: "gd-StepRel02", Type: "task", Status: "open", Title: "Roll the cut",
		Labels: []string{"project:gardenio"},
	}
	daemon.beads["gd-Loose01"] = &beadsapi.BeadDetail{
		ID: "gd-Loose01", Type: "task", Status: "open", Title: "Loose task",
		Labels: []string{"project:gardenio"},
	}
	daemon.beads["gd-EpicNoPlan"] = &beadsapi.BeadDetail{
		ID: "gd-EpicNoPlan", Type: "epic", Status: "open", Title: "Not a plan",
		Labels: []string{"project:gardenio"},
	}
	daemon.beads["gd-PlanNoProj"] = &beadsapi.BeadDetail{
		ID: "gd-PlanNoProj", Type: "epic", Status: "open", Title: "Plan with no project",
		Labels: []string{"plan"},
	}
}

func TestNotifyDecision_OpenOnTheMap(t *testing.T) {
	const viewer = "https://beads.test"
	const want = viewer + "/garden?at=gardenio/gd-PlanRel01"
	cases := []struct {
		name   string
		fields map[string]string
		viewer string
		want   string // "" = no button
	}{
		{"the plan is the requesting task (run-it card)", map[string]string{"requesting_task": "gd-PlanRel01"}, viewer, want},
		{"a step whose parent is the plan", map[string]string{"requesting_task": "gd-StepRel01"}, viewer, want},
		{"a step whose parent is read from its deps", map[string]string{"requesting_task": "gd-StepRel02"}, viewer, want},
		{"requesting_plan names the plan", map[string]string{"requesting_task": "gd-Loose01", "requesting_plan": "gd-PlanRel01"}, viewer, want},
		{"a task with no plan", map[string]string{"requesting_task": "gd-Loose01"}, viewer, ""},
		{"an epic that is not a plan", map[string]string{"requesting_task": "gd-EpicNoPlan"}, viewer, ""},
		{"a plan with no project label", map[string]string{"requesting_task": "gd-PlanNoProj"}, viewer, ""},
		{"no requesting task", map[string]string{}, viewer, ""},
		{"no viewer URL", map[string]string{"requesting_task": "gd-PlanRel01"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockDaemon()
			seedPlanBeads(mock)
			daemon := &depsDaemon{mockDaemon: mock, deps: map[string][]beadsapi.Dependency{
				"gd-StepRel02": {
					{BeadID: "gd-StepRel02", DependsOnID: "gd-Loose01", Type: "blocks"},
					{BeadID: "gd-StepRel02", DependsOnID: "gd-PlanRel01", Type: "parent-child"},
				},
			}}
			srv, posts := recordingSlack(t)
			b := newTestBot(daemon, srv)
			b.channel = "C1"
			b.beadsViewerURL = tc.viewer

			opts, _ := json.Marshal([]string{"approve", "drop"})
			fields := map[string]string{"prompt": "Run it?", "options": string(opts)}
			for k, v := range tc.fields {
				fields[k] = v
			}
			if err := b.NotifyDecision(context.Background(), BeadEvent{
				ID: "gd-DecMap01", Type: "decision", Title: "Run it?", Priority: 2, Fields: fields,
			}); err != nil {
				t.Fatal(err)
			}
			if len(posts()) == 0 {
				t.Fatal("no card posted")
			}
			btns := mapButtons(t, posts()[0])
			if tc.want == "" {
				if len(btns) != 0 {
					t.Errorf("card carries %d map buttons, want none: %s", len(btns), posts()[0])
				}
				return
			}
			if len(btns) != 1 {
				t.Fatalf("card carries %d map buttons, want exactly one: %s", len(btns), posts()[0])
			}
			if btns[0].URL != tc.want {
				t.Errorf("map button URL = %q, want %q", btns[0].URL, tc.want)
			}
			if btns[0].Text == nil || btns[0].Text.Text != "Open on the map" {
				t.Errorf("map button label = %+v, want \"Open on the map\"", btns[0].Text)
			}
		})
	}
}

func TestPlanMapHref(t *testing.T) {
	plan := &beadsapi.BeadDetail{ID: "gd-PlanRel01", Type: "epic", Labels: []string{"plan", "project:tomato patch"}}
	if got, want := planMapHref("https://v.test/beads/", plan), "https://v.test/beads/garden?at=tomato+patch/gd-PlanRel01"; got != want {
		t.Errorf("planMapHref = %q, want %q (project segment escaped, one slash)", got, want)
	}
	if got := planMapHref("https://v.test", &beadsapi.BeadDetail{ID: "not an id", Labels: []string{"project:p"}}); got != "" {
		t.Errorf("planMapHref of a malformed id = %q, want none", got)
	}
}
