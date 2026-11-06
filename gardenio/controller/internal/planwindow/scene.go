package planwindow

import (
	"fmt"
	"time"
)

// Scene is a land view in the shape of the signed-in farm document
// (gardenhub's farm_model.go, gardenio/docs/homestead-farm-read-model.md), cut
// to the part the World scene reads: gardenhub's web/world.js lays it out and
// places its gardeners with no new layout code (LW.3 gd-VEbAvsTNDe draws it).
// Every key in it is a key of the matching Farm* type
// (TestSceneKeysAreFarmKeys); it has no cost, wishes, meadow, piles or meta,
// no hrefs and no people but the crew's handles.
//
// The land view carries no bead ids, so the scene's ids are positional, made
// up here: plan-<n> is the n-th plan of the view (1-based), plan-<n>/phase-<m>
// its m-th phase, plan-<n>/phase-<m>/step-<k> and plan-<n>/step-<k> its
// steps, plan-<n>/decision-<k> its open decisions and crew-<n> the n-th
// distinct crew handle. Nor does it carry created_at: a plan's or a step's
// created is its place in the view's order (1-based), which is all the layout
// orders by.
type Scene struct {
	// Plans are the open plans that are being built (any plan_state but
	// draft and ready), in the view's order.
	Plans []ScenePlan `json:"plans"`
	// Seeds are the open plans not yet approved: plan_state draft (a frosted
	// pot in the nursery) or ready (staked out on a plot, with its steps).
	Seeds []SceneSeed `json:"seeds"`
	// Gardeners are the crew of every plan, one per handle (a withheld
	// handle is one per plan it crews).
	Gardeners   []SceneGardener  `json:"gardeners"`
	ClosedPlans SceneClosedPlans `json:"closed_plans"`
}

// ScenePlan is a FarmPlan.
type ScenePlan struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	State      string `json:"state"`
	Status     string `json:"status"`
	Project    string `json:"project,omitempty"`
	Created    int64  `json:"created"`
	Updated    int64  `json:"updated"`
	Activity   int64  `json:"activity"`
	Heraldry   string `json:"heraldry,omitempty"`
	// Phases are the plan's phases in order; Unphased holds its steps in no
	// phase.
	Phases          []ScenePhase `json:"phases"`
	Unphased        ScenePhase   `json:"unphased"`
	Counts          SceneCounts  `json:"counts"`
	OpenDecisions   int          `json:"open_decisions"`
	OpenEscalations int          `json:"open_escalations"`
	// Waiting lists the open decisions by title. The land view names no
	// escalation, so escalations are only counted (OpenEscalations). It
	// carries no needs_human: the view cannot tell an approved escalation
	// from a pending one, so world.js counts every open decision and
	// escalation as needing you, as it does for a document without it.
	Waiting []SceneWait `json:"waiting,omitempty"`
}

// ScenePhase is a FarmPhase: one bed of a plot.
type ScenePhase struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	ShortTitle string      `json:"short_title,omitempty"`
	Status     string      `json:"status"`
	Counts     SceneCounts `json:"counts"`
	// Tasks are the phase's steps that are not closed, in order.
	Tasks []SceneTask `json:"tasks"`
	// Closed are its closed steps, in order.
	Closed []SceneClosedStep `json:"closed,omitempty"`
}

// SceneCounts is a FarmCounts. The land view knows nothing of merge
// requests, so review is always 0.
type SceneCounts struct {
	InProgress int `json:"in_progress"`
	Review     int `json:"review"`
	Blocked    int `json:"blocked"`
	Open       int `json:"open"`
	Deferred   int `json:"deferred"`
	Closed     int `json:"closed"`
}

// SceneTask is a FarmTask: a step that is not closed.
type SceneTask struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type"`
	// State is the step's status: in_progress, blocked, open or deferred.
	State   string `json:"state"`
	Project string `json:"project,omitempty"`
	Created int64  `json:"created"`
}

// SceneClosedStep is a FarmClosedStep.
type SceneClosedStep struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type"`
	State      string `json:"state"` // always closed
	// Updated is when the step closed (done_at), 0 when the view did not
	// say.
	Updated int64 `json:"updated"`
	Created int64 `json:"created"`
}

// SceneWait is a FarmWait: one open decision, which waits on a human.
type SceneWait struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`  // always decision
	State string `json:"state"` // always unanswered
	Human bool   `json:"human"` // always true
}

// SceneSeed is a FarmSeed.
type SceneSeed struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	State   string `json:"state"` // frosted (draft) or ready
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Phases  int    `json:"phases"`
	// Tasks counts the steps that are not closed.
	Tasks int `json:"tasks"`
	// Steps and ShortTitle are set on a ready seed only, as in the farm:
	// its steps that are not closed, in order.
	Steps      []SceneSeedStep `json:"steps,omitempty"`
	ShortTitle string          `json:"short_title,omitempty"`
}

// SceneSeedStep is a FarmSeedStep.
type SceneSeedStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	N      int    `json:"n"`
	Status string `json:"status"`
}

// SceneGardener is a FarmGardener: a crew handle. Its live flag is the
// farm's bucket: alive for a live agent (state working, standing on its
// plan's plot), dormant for one that is not (at the barn).
type SceneGardener struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Role    string `json:"role"` // always crew
	Project string `json:"project,omitempty"`
	State   string `json:"state"`
	Bucket  string `json:"bucket"`
	Task    string `json:"task"` // always "": the view does not say which step
	// Plot is the plan the handle crews, null when that plan is not on a plot
	// (a draft seed or a closed plan).
	Plot   *ScenePlot `json:"plot"`
	Reason *struct{}  `json:"reason"` // always null: the view says nothing of why one waits
}

// ScenePlot is a FarmPlot.
type ScenePlot struct {
	Plan  string `json:"plan"`
	Phase string `json:"phase"`
}

// SceneClosedPlans is a FarmClosedPlans: the land view's closed plans,
// newest first.
type SceneClosedPlans struct {
	Total int               `json:"total"`
	Shown []SceneClosedPlan `json:"shown"`
}

// SceneClosedPlan is a FarmClosedPlan.
type SceneClosedPlan struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Project string `json:"project,omitempty"`
	Updated int64  `json:"updated"`
}

// The farm's gardener buckets a Scene uses (gardenhub's Gardener*).
const (
	sceneAlive   = "alive"
	sceneDormant = "dormant"
)

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// ToScene shapes a cleaned land view for the World scene. It reads only v,
// so whatever hygiene withheld stays withheld, and it is pure.
func ToScene(v LandView) Scene {
	s := Scene{Plans: []ScenePlan{}, Seeds: []SceneSeed{}, Gardeners: []SceneGardener{}, ClosedPlans: SceneClosedPlans{Shown: []SceneClosedPlan{}}}
	project := v.Land
	crew := map[string]int{} // handle -> index in s.Gardeners
	for i, p := range v.Plans {
		id, n := fmt.Sprintf("plan-%d", i+1), int64(i+1)
		updated := unix(p.UpdatedAt)
		var plot *ScenePlot
		switch {
		case p.Status == "closed":
			s.ClosedPlans.Shown = append(s.ClosedPlans.Shown, SceneClosedPlan{ID: id, Title: p.Title, State: p.State, Project: project, Updated: updated})
		case p.State == "draft" || p.State == "ready":
			seed := SceneSeed{ID: id, Title: p.Title, State: "frosted", Status: p.Status, Project: project, Created: n, Updated: updated, Phases: len(p.Phases)}
			if p.State == "ready" {
				seed.State, seed.ShortTitle, seed.Steps = "ready", p.ShortTitle, []SceneSeedStep{}
				plot = &ScenePlot{Plan: id}
			}
			forEachStep(id, p, func(sid string, _ int, st StepFacts) {
				if st.Status == "closed" {
					return
				}
				seed.Tasks++
				if seed.Steps != nil {
					seed.Steps = append(seed.Steps, SceneSeedStep{ID: sid, Title: st.Title, N: len(seed.Steps) + 1, Status: st.Status})
				}
			})
			s.Seeds = append(s.Seeds, seed)
		default:
			s.Plans = append(s.Plans, scenePlan(id, n, project, p))
			plot = &ScenePlot{Plan: id}
		}
		for _, a := range p.Crew {
			// A handle on several plans stands on the first it is live on. A
			// withheld one is never merged: two of them may be two people.
			if k, ok := crew[a.Name]; ok && a.Name != WithheldText {
				if g := &s.Gardeners[k]; a.Live && g.Bucket != sceneAlive {
					g.State, g.Bucket, g.Plot = "working", sceneAlive, plot
				}
				continue
			}
			g := SceneGardener{ID: fmt.Sprintf("crew-%d", len(s.Gardeners)+1), Agent: a.Name, Role: "crew", Project: project, Bucket: sceneDormant, Plot: plot}
			if a.Live {
				g.State, g.Bucket = "working", sceneAlive
			}
			crew[a.Name] = len(s.Gardeners)
			s.Gardeners = append(s.Gardeners, g)
		}
	}
	s.ClosedPlans.Total = len(s.ClosedPlans.Shown)
	return s
}

// forEachStep calls f for each of a plan's steps in order, with its scene id
// and its phase's index (-1 for a step in no phase).
func forEachStep(plan string, p LandPlanFacts, f func(id string, phase int, st StepFacts)) {
	for m, ph := range p.Phases {
		for k, st := range ph.Steps {
			f(fmt.Sprintf("%s/phase-%d/step-%d", plan, m+1, k+1), m, st)
		}
	}
	for k, st := range p.Steps {
		f(fmt.Sprintf("%s/step-%d", plan, k+1), -1, st)
	}
}

func scenePlan(id string, n int64, project string, p LandPlanFacts) ScenePlan {
	updated := unix(p.UpdatedAt)
	sp := ScenePlan{
		ID: id, Title: p.Title, ShortTitle: p.ShortTitle, State: p.State, Status: p.Status, Project: project,
		Created: n, Updated: updated, Activity: updated, Heraldry: p.Heraldry,
		Phases: make([]ScenePhase, 0, len(p.Phases)), Unphased: ScenePhase{Tasks: []SceneTask{}},
		OpenDecisions: p.OpenDecisions, OpenEscalations: p.OpenEscalations,
	}
	for m, ph := range p.Phases {
		sp.Phases = append(sp.Phases, ScenePhase{ID: fmt.Sprintf("%s/phase-%d", id, m+1), Title: ph.Title, ShortTitle: ph.ShortTitle, Status: ph.Status, Tasks: []SceneTask{}})
	}
	created := int64(0)
	forEachStep(id, p, func(sid string, phase int, st StepFacts) {
		ph := &sp.Unphased
		if phase >= 0 {
			ph = &sp.Phases[phase]
		}
		created++
		switch st.Status {
		case "closed":
			ph.Counts.Closed++
			cs := SceneClosedStep{ID: sid, Title: st.Title, ShortTitle: st.ShortTitle, Type: st.Kind, State: "closed", Created: created}
			if st.DoneAt != nil {
				cs.Updated = st.DoneAt.Unix()
			}
			ph.Closed = append(ph.Closed, cs)
			return
		case "in_progress":
			ph.Counts.InProgress++
		case "blocked":
			ph.Counts.Blocked++
		case "deferred":
			ph.Counts.Deferred++
		default:
			ph.Counts.Open++
		}
		ph.Tasks = append(ph.Tasks, SceneTask{ID: sid, Title: st.Title, ShortTitle: st.ShortTitle, Type: st.Kind, State: st.Status, Project: project, Created: created})
	})
	add := func(c SceneCounts) {
		sp.Counts.InProgress += c.InProgress
		sp.Counts.Blocked += c.Blocked
		sp.Counts.Open += c.Open
		sp.Counts.Deferred += c.Deferred
		sp.Counts.Closed += c.Closed
	}
	for _, ph := range sp.Phases {
		add(ph.Counts)
	}
	add(sp.Unphased.Counts)
	for k, d := range p.Decisions {
		sp.Waiting = append(sp.Waiting, SceneWait{ID: fmt.Sprintf("%s/decision-%d", id, k+1), Title: d, Kind: "decision", State: "unanswered", Human: true})
	}
	return sp
}
