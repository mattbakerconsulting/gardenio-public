package commonstest

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// narrative is the Commons v2 plan's own narrative (gd-c3bFiScn5P, as of
// 2026-11-01): the fixture plan's face, at the size a real one is.
//
//go:embed workbench_narrative.md
var narrative string

// The workbench fixture's roots (Commons v2 step 3, gd-qlk31NBV6o).
const (
	WorkbenchPlan   = "cl-plan"
	WorkbenchPhase  = "cl-ph1"
	WorkbenchEpic   = "cl-epic"
	WorkbenchThread = "cl-wish1"
	// WorkbenchAgent is the crew at work on the plan's first open step.
	WorkbenchAgent = "cl-crew-s3"
	// WorkbenchStep is the step WorkbenchAgent works.
	WorkbenchStep = "cl-s3"
)

// Workbench adds to d (TwoLands) the trees the workbench opens on, all in
// claimlayer, with the edges kbeads keeps between them:
//
//   - a plan in two phases of four steps and one step under no phase, its
//     narrative bundle and doc, and its steward (a gardener loop). Each step
//     after the first is blocked by the one before; the done steps have closed
//     crews and merged PRs, the open ones a crew at work, an open PR, an open
//     decision and a done escalation;
//   - a plain epic of five tasks, one with two subtasks, and a bead it holds up;
//   - cl-wish1's thread: two agents bound to it, one of which spawned another.
//
// Nothing is logged: the event log replays what TwoLands' does.
func Workbench(d *Daemon, t0 time.Time) {
	w := workbench{d: d, t0: t0}
	w.plan()
	w.epic()
	w.thread()
}

// workbench puts the fixture's beads into d, at minutes after t0.
type workbench struct {
	d  *Daemon
	t0 time.Time
}

func (w workbench) at(m int) time.Time { return w.t0.Add(time.Duration(m) * time.Minute) }

// put adds b in claimlayer unless it names its labels.
func (w workbench) put(b *beadsapi.BeadDetail) {
	if b.Labels == nil {
		b.Labels = []string{"project:claimlayer"}
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = b.CreatedAt
	}
	w.d.Put(b)
}

// plan is the plan, its phases and steps, their crews, PRs and asks.
func (w workbench) plan() {
	d, put, at := w.d, w.put, w.at
	body := "Build the piece of the workbench this step names, inside the step-1 budgets, and prove it with the harness. " +
		"Open as a draft, run only the tests of the packages it touches, and keep the JS budget whole."

	put(&beadsapi.BeadDetail{ID: WorkbenchPlan, Type: "epic", Status: "open", Title: "Commons v2: a workbench for any bead and its children, fast first",
		ShortTitle: "Commons v2", Labels: []string{"plan", "project:claimlayer"}, CreatedAt: at(30),
		Description: "Replace the Commons band with a workbench that opens on any bead with children.",
		Fields:      map[string]string{"plan_state": "building", "phase_order": `["cl-ph1","cl-ph2"]`, "step_order": `["cl-s9"]`, "thread_id": WorkbenchThread}})
	put(&beadsapi.BeadDetail{ID: "cl-bundle", Type: "bundle", Status: "open", Title: "Plan narrative: Commons v2", CreatedAt: at(31)})
	put(&beadsapi.BeadDetail{ID: "cl-doc", Type: "doc", Status: "open", Title: "Commons v2 — plan narrative", CreatedAt: at(31),
		Fields: map[string]string{"content": narrative, "format": "md"}})
	d.Link("cl-bundle", WorkbenchPlan, "parent-child")
	d.Link("cl-doc", "cl-bundle", "parent-child")
	put(&beadsapi.BeadDetail{ID: "cl-gard", Type: "trigger", Status: "open", Title: "Plan gardener for cl-plan",
		Labels: []string{"project:claimlayer", "gardener-epic:" + WorkbenchPlan, "plan-gardener"}, CreatedAt: at(31), Fields: map[string]string{"enabled": "true"}})

	phases := [][2]string{{"cl-ph1", "Phase 1: the stream and the model"}, {"cl-ph2", "Phase 2: the workbench"}}
	steps := []struct {
		id, phase, title, short, status string
	}{
		{"cl-s1", "cl-ph1", "Latency budgets and the harness, before any pixel", "Latency budgets", "closed"},
		{"cl-s2", "cl-ph1", "One stream that carries content", "One stream", "closed"},
		{"cl-s3", "cl-ph1", "The node model: any bead, its children and its jobs, in one fragment", "The node model", "in_progress"},
		{"cl-s4", "cl-ph1", "The workbench on the desktop", "Desktop workbench", "open"},
		{"cl-s5", "cl-ph2", "The transcript as documents", "Transcript documents", "open"},
		{"cl-s6", "cl-ph2", "Composer and switch latency", "Composer latency", "open"},
		{"cl-s7", "cl-ph2", "The phone: a push stack", "Phone push stack", "open"},
		{"cl-s8", "cl-ph2", "Retire the band", "Retire the band", "open"},
		{"cl-s9", "", "The perf gate in CI and the check-in", "Perf gate", "open"},
	}
	for i, ph := range phases {
		var order []string
		for _, s := range steps {
			if s.phase == ph[0] {
				order = append(order, `"`+s.id+`"`)
			}
		}
		put(&beadsapi.BeadDetail{ID: ph[0], Type: "epic", Status: "open", Title: ph[1], CreatedAt: at(32 + i),
			Description: "The steps of " + ph[1] + ".", Fields: map[string]string{"step_order": "[" + strings.Join(order, ",") + "]"}})
		d.Link(ph[0], WorkbenchPlan, "parent-child")
	}
	for i, s := range steps {
		parent := s.phase
		if parent == "" {
			parent = WorkbenchPlan
		}
		put(&beadsapi.BeadDetail{ID: s.id, Type: "task", Status: s.status, Title: s.title, ShortTitle: s.short, CreatedAt: at(34 + i),
			Description: body, Fields: map[string]string{"thread_id": WorkbenchThread}})
		d.Link(s.id, parent, "parent-child")
		if i > 0 {
			d.Link(s.id, steps[i-1].id, "blocks")
		}
		if s.status == "open" {
			continue
		}
		// a crew per started step: done on a closed one, at work on the open one
		crew, state, status := "cl-crew-"+s.id[3:], "done", "closed"
		if s.status != "closed" {
			state, status = "working", "open"
		}
		put(&beadsapi.BeadDetail{ID: crew, Type: "agent", Status: status, Title: "claimlayer-" + s.short, CreatedAt: at(40 + i),
			Fields: map[string]string{"agent": "claimlayer-crew-" + s.id[3:], "role": "crew", "agent_state": state, "task_id": s.id,
				"thread_id": WorkbenchThread, "spawned_by": "claimlayer-plan-gardener"}})
		d.Link(crew, s.id, "assigned")
		mrState := "merged"
		if s.status != "closed" {
			mrState = "opened"
		}
		mr := "cl-mr-" + s.id[3:]
		put(&beadsapi.BeadDetail{ID: mr, Type: "merge-request", Status: map[bool]string{true: "closed", false: "open"}[mrState == "merged"],
			Title: "PR #" + fmt.Sprint(1790+i) + ": " + s.short, CreatedAt: at(50 + i),
			Fields: map[string]string{"mr_url": fmt.Sprintf("https://github.com/acme/app/pull/%d", 1790+i), "mr_state": mrState}})
		d.Link(s.id, mr, "produces")
	}
	// The crew at work asks a decision; an escalation for the first step is done.
	put(&beadsapi.BeadDetail{ID: "cl-wdec", Type: "decision", Status: "open", Title: "Serve jobs from memory or from the agent bead?", CreatedAt: at(60),
		Fields: map[string]string{"prompt": "Serve jobs from memory or from the agent bead?"}})
	d.Link(WorkbenchStep, "cl-wdec", "blocks")
	put(&beadsapi.BeadDetail{ID: "cl-wesc", Type: "escalation", Status: "closed", Title: "Merge PR #1790: latency budgets", CreatedAt: at(61),
		Fields: map[string]string{"operation_type": "gitlab_mr_merge", "result_status": "success"}})
	d.Link("cl-s1", "cl-wesc", "blocks")
}

// epic is a plain epic of five tasks, one with two subtasks, and a bead it
// holds up.
func (w workbench) epic() {
	d, put, at := w.d, w.put, w.at
	put(&beadsapi.BeadDetail{ID: WorkbenchEpic, Type: "epic", Status: "open", Title: "Chart lints again", CreatedAt: at(70),
		Description: "The values schema stopped linting after the box wiring; make the chart lint again and keep it linting."})
	for i, t := range []string{"Fix the values schema", "Pin the lint image", "Add the box values to the schema", "Lint in the chart job", "Document the schema"} {
		id := fmt.Sprintf("cl-et%d", i+1)
		status := "open"
		if i < 2 {
			status = "closed"
		}
		put(&beadsapi.BeadDetail{ID: id, Type: "task", Status: status, Title: t, CreatedAt: at(71 + i)})
		d.Link(id, WorkbenchEpic, "parent-child")
	}
	for i, t := range []string{"Schema for box.name", "Schema for box.kind"} {
		id := fmt.Sprintf("cl-et3-%d", i+1)
		put(&beadsapi.BeadDetail{ID: id, Type: "task", Status: "open", Title: t, CreatedAt: at(80 + i)})
		d.Link(id, "cl-et3", "parent-child")
	}
	put(&beadsapi.BeadDetail{ID: "cl-edep", Type: "task", Status: "open", Title: "Release the chart", CreatedAt: at(82)})
	d.Link("cl-edep", WorkbenchEpic, "blocks")
}

// thread is cl-wish1's thread: an agent bound to it that spawned another.
func (w workbench) thread() {
	put, at := w.put, w.at
	put(&beadsapi.BeadDetail{ID: "cl-tagent1", Type: "agent", Status: "open", Title: "claimlayer-thread-crew", CreatedAt: at(85),
		Fields: map[string]string{"agent": "claimlayer-thread-crew", "role": "crew", "agent_state": "working", "thread_id": WorkbenchThread, "spawned_by": "slack"}})
	put(&beadsapi.BeadDetail{ID: "cl-tagent2", Type: "agent", Status: "open", Title: "claimlayer-thread-helper", CreatedAt: at(86),
		Fields: map[string]string{"agent": "claimlayer-thread-helper", "role": "crew", "agent_state": "idle", "thread_id": WorkbenchThread, "spawned_by": "claimlayer-thread-crew"}})
}
