package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The plan cockpit (Lands workbench-glass spec WB-7 and WB-8, step 5.9, gd-nTPjJ2yllf): one plan as a Workbench kind
// (web/plan.js, /garden?plan=<id>), opened from a plan's face ("Open the cockpit"), a plan crate on the farm and every
// retired /plans/{id} link. The reader is the viewer's own cockpit builders (cmd/beads-viewer plan_garden.go reuses
// plans.go's timeline, decisions, crew, escalations and narrative), on the request's bearer, so the daemon cuts the
// answer as it cuts the page. Every write is the viewer's own /plans/{id}/* POST route asked for JSON (they call the
// internal/plans service, step 5.8), never a second implementation; a decision is answered and an escalation approved
// through the Mailbox's own answer box (web/answer.js).

// PlanReader reads one plan's cockpit for the request's viewer. A bead that is not a plan cockpit (not an epic) is
// ErrNotPlan; one the daemon does not show the viewer is the daemon's 404.
type PlanReader interface {
	Plan(ctx context.Context, id string) (*PlanDoc, error)
	// Pour lists the formulas the plan's project may pour into it as a phase (WB-8).
	Pour(ctx context.Context, id string) (*PourDoc, error)
}

// ErrNotPlan is a PlanReader's answer for a bead that has no cockpit.
var ErrNotPlan = errors.New("not a plan")

const (
	planTimeout     = 15 * time.Second
	planStepsMax    = 400 // steps sent across the phases: the cockpit page draws them all, so this is only a backstop
	planTextMax     = 600 // runes of a decision's prompt
	planDocMax      = 40000
	planDocsMax     = 12
	planCrewMax     = 60
	planFormulasMax = 100
)

// PlanStep is one step of the timeline.
type PlanStep struct {
	ID         string `json:"id"`
	Number     int    `json:"n,omitempty"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type,omitempty"`
	Status     string `json:"status,omitempty"`
	Closed     bool   `json:"closed,omitempty"`
	Priority   int    `json:"priority"`
	Assignee   string `json:"assignee,omitempty"`
	// MR is the step's merge request: its state (opened, merged, closed) and link.
	MR *PlanMR `json:"mr,omitempty"`
	// BlockedBy is what the step waits on: open beads it depends on, an escalation among them named so.
	BlockedBy []PlanBlocker `json:"blocked_by,omitempty"`
	// Updated is the step's last change (unix seconds).
	Updated int64 `json:"updated,omitempty"`
}

// PlanMR is a step's merge request.
type PlanMR struct {
	State string `json:"state,omitempty"`
	URL   string `json:"url,omitempty"`
}

// PlanBlocker is a bead a step waits on.
type PlanBlocker struct {
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	Escalation bool   `json:"escalation,omitempty"`
	Operation  string `json:"operation,omitempty"`
}

// PlanPhase is a phase sub-epic and its steps, in phase_order.
type PlanPhase struct {
	ID      string     `json:"id"`
	Number  int        `json:"n,omitempty"`
	Title   string     `json:"title"`
	Status  string     `json:"status,omitempty"`
	Closed  bool       `json:"closed,omitempty"`
	Total   int        `json:"total"`
	Done    int        `json:"done"`
	Formula string     `json:"formula,omitempty"` // the formula it was poured from
	Steps   []PlanStep `json:"steps"`
}

// PlanDecision is one of the plan's clarifying-question decisions.
type PlanDecision struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Prompt      string        `json:"prompt,omitempty"`
	Status      string        `json:"status,omitempty"`
	Resolved    bool          `json:"resolved,omitempty"`
	RequestedBy string        `json:"requested_by,omitempty"`
	Options     []QueueOption `json:"options,omitempty"`
	// Chosen is the answer a resolved decision took, in words.
	Chosen string `json:"chosen,omitempty"`
	Since  int64  `json:"since,omitempty"`
	// Mine says it is asked of this viewer; AskedOf names the operator it is asked of otherwise, "" when no name is known.
	Mine    bool   `json:"mine,omitempty"`
	AskedOf string `json:"asked_of,omitempty"`
}

// PlanEscalation is an open escalation that holds up one of the plan's steps.
type PlanEscalation struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Operation   string `json:"operation,omitempty"`
	Sensitivity string `json:"sensitivity,omitempty"`
	// Audience is who approves it: owner, operator or desk (gd-bmOiteQi36).
	Audience string `json:"audience,omitempty"`
	Since    int64  `json:"since,omitempty"`
}

// PlanCrewMember is an agent the plan dispatched onto a step.
type PlanCrewMember struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`
	State string `json:"state,omitempty"` // agent_state
	// Bucket is how it is going, in a word: working, starting, idle, done or failed.
	Bucket  string    `json:"bucket,omitempty"`
	Live    bool      `json:"live,omitempty"`
	Step    *QueueRef `json:"step,omitempty"`
	StepN   int       `json:"step_n,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Since   int64     `json:"since,omitempty"`
}

// PlanCrew is the plan's crew roster.
type PlanCrew struct {
	Total    int              `json:"total"`
	Working  int              `json:"working,omitempty"`
	Starting int              `json:"starting,omitempty"`
	Idle     int              `json:"idle,omitempty"`
	Done     int              `json:"done,omitempty"`
	Failed   int              `json:"failed,omitempty"`
	Members  []PlanCrewMember `json:"members"`
}

// PlanNarrativeDoc is one document of the plan's narrative bundle.
type PlanNarrativeDoc struct {
	ID       string `json:"id"`
	Headline string `json:"headline,omitempty"`
	Format   string `json:"format,omitempty"`
	// Content is the doc's markdown (or text), editable in place; Clipped says it was cut for the panel, which then
	// edits it on the old cockpit.
	Content string `json:"content,omitempty"`
	Clipped bool   `json:"clipped,omitempty"`
	// URL is a doc kept elsewhere (fields.content_url), read there.
	URL string `json:"url,omitempty"`
}

// PlanNarrative is the plan's prose: the epic's description (its summary) and its narrative bundle's docs.
type PlanNarrative struct {
	Summary        string             `json:"summary,omitempty"`
	SummaryClipped bool               `json:"summary_clipped,omitempty"`
	Bundle         string             `json:"bundle,omitempty"`
	BundleTitle    string             `json:"bundle_title,omitempty"`
	Docs           []PlanNarrativeDoc `json:"docs,omitempty"`
}

// PlanDoc is one plan's cockpit as the panel needs it.
type PlanDoc struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Project    string `json:"project,omitempty"`
	Status     string `json:"status,omitempty"`
	// State is the authoring lifecycle (fields.plan_state): draft, ready, building, complete or abandoned; "" for an
	// epic that has never been in it.
	State string `json:"state,omitempty"`
	// Plan says the epic carries the plan label; an epic without it offers "Mark as a plan".
	Plan bool `json:"plan"`
	// Edit says the viewer may write here (the /plans/{id}/* routes are operator-gated, and check it again); Approve that
	// it may approve an escalation (the answer box offers Approve and Reject).
	Edit    bool `json:"edit,omitempty"`
	Approve bool `json:"approve,omitempty"`

	Total  int `json:"total"`
	Closed int `json:"closed"`
	Pct    int `json:"pct"`
	// LastChange is when the plan last moved: the epic, its steps, its crew's heartbeats (unix seconds).
	LastChange int64 `json:"last_change,omitempty"`

	// Phases are in phase_order; Steps are the steps under no phase.
	Phases      []PlanPhase      `json:"phases,omitempty"`
	Steps       []PlanStep       `json:"steps,omitempty"`
	Decisions   []PlanDecision   `json:"decisions,omitempty"`
	Escalations []PlanEscalation `json:"escalations,omitempty"`
	Crew        *PlanCrew        `json:"crew,omitempty"`
	Narrative   PlanNarrative    `json:"narrative"`
	// More says the steps were capped at planStepsMax.
	More bool `json:"more,omitempty"`
}

// PourVar is one variable a formula takes.
type PourVar struct {
	Name     string   `json:"name"`
	Note     string   `json:"note,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  string   `json:"default,omitempty"`
	Enum     []string `json:"enum,omitempty"`
}

// PourFormula is a formula the plan may pour.
type PourFormula struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	Steps int       `json:"steps,omitempty"`
	Vars  []PourVar `json:"vars,omitempty"`
	// Vapor says it pours ephemeral (a vapor-phase formula).
	Vapor bool `json:"vapor,omitempty"`
}

// PourDoc is what the pour fold offers.
type PourDoc struct {
	Plan     string        `json:"plan"`
	Project  string        `json:"project,omitempty"`
	Formulas []PourFormula `json:"formulas"`
}

// planID is the path's {id}, or "" when it is not one.
func planID(r *http.Request) string {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		return ""
	}
	return id
}

// planReadFailed answers a reader's error: a bead with no cockpit or one the viewer cannot see is a 404, anything else
// the daemon's silence.
func planReadFailed(w http.ResponseWriter, err error) {
	var ae *beadsapi.APIError
	switch {
	case errors.Is(err, ErrNotPlan), errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden):
		http.Error(w, "no such plan", http.StatusNotFound)
	default:
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
	}
}

// planView serves GET /garden/plan/{id}.
func (h *Handler) planView(w http.ResponseWriter, r *http.Request) {
	if h.Plans == nil {
		http.Error(w, "plan cockpit not enabled", http.StatusServiceUnavailable)
		return
	}
	id := planID(r)
	if id == "" {
		http.Error(w, "bad plan id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), planTimeout)
	defer cancel()
	doc, err := h.Plans.Plan(ctx, id)
	if err != nil || doc == nil {
		if err == nil {
			err = ErrNotPlan
		}
		planReadFailed(w, err)
		return
	}
	doc.Edit = h.can(r, h.CanEdit)
	doc.Approve = h.can(r, h.CanApproveEscalation)
	finishPlan(doc)
	writeTagged(w, r, "kbeads", doc)
}

// planPour serves GET /garden/plan/{id}/pour: the formulas the pour fold offers. Only for a viewer who may write here,
// since only they can pour (POST /plans/{id}/pours is operator-gated).
func (h *Handler) planPour(w http.ResponseWriter, r *http.Request) {
	if h.Plans == nil {
		http.Error(w, "plan cockpit not enabled", http.StatusServiceUnavailable)
		return
	}
	id := planID(r)
	if id == "" {
		http.Error(w, "bad plan id", http.StatusBadRequest)
		return
	}
	if !h.can(r, h.CanEdit) {
		http.Error(w, "only an operator can pour a formula", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), planTimeout)
	defer cancel()
	doc, err := h.Plans.Pour(ctx, id)
	if err != nil || doc == nil {
		if err == nil {
			err = ErrNotPlan
		}
		planReadFailed(w, err)
		return
	}
	doc.Plan = id
	if doc.Formulas == nil {
		doc.Formulas = []PourFormula{}
	}
	if len(doc.Formulas) > planFormulasMax {
		doc.Formulas = doc.Formulas[:planFormulasMax]
	}
	writeDoc(w, "kbeads", doc)
}

// finishPlan caps what a reader sent, so every reader sends the same shape: the steps across the phases, the crew, the
// narrative's docs, a decision's prompt.
func finishPlan(d *PlanDoc) {
	left := planStepsMax
	take := func(s []PlanStep) []PlanStep {
		if len(s) > left {
			s, d.More = s[:left], true
		}
		left -= len(s)
		return s
	}
	for i := range d.Phases {
		d.Phases[i].Steps = take(d.Phases[i].Steps)
		if d.Phases[i].Steps == nil {
			d.Phases[i].Steps = []PlanStep{}
		}
	}
	d.Steps = take(d.Steps)
	for i := range d.Decisions {
		d.Decisions[i].Prompt = clip(d.Decisions[i].Prompt, planTextMax)
	}
	if d.Crew != nil && len(d.Crew.Members) > planCrewMax {
		d.Crew.Members = d.Crew.Members[:planCrewMax]
	}
	n := &d.Narrative
	if len([]rune(n.Summary)) > planDocMax {
		n.Summary, n.SummaryClipped = clipKeep(n.Summary, planDocMax), true
	}
	if len(n.Docs) > planDocsMax {
		n.Docs = n.Docs[:planDocsMax]
	}
	for i := range n.Docs {
		if len([]rune(n.Docs[i].Content)) > planDocMax {
			n.Docs[i].Content, n.Docs[i].Clipped = clipKeep(n.Docs[i].Content, planDocMax), true
		}
	}
}

// planIDs is every bead the cockpit draws whose move should repaint it: the plan, its phases and steps, its decisions
// and escalations, and its narrative bundle and docs. Not the crew: an agent's heartbeat moves its bead every few
// seconds, and the step it works moves when the work does.
func planIDs(d *PlanDoc) []string {
	ids := []string{d.ID}
	for _, ph := range d.Phases {
		ids = append(ids, ph.ID)
		for _, s := range ph.Steps {
			ids = append(ids, s.ID)
		}
	}
	for _, s := range d.Steps {
		ids = append(ids, s.ID)
	}
	for _, x := range d.Decisions {
		ids = append(ids, x.ID)
	}
	for _, x := range d.Escalations {
		ids = append(ids, x.ID)
	}
	if d.Narrative.Bundle != "" {
		ids = append(ids, d.Narrative.Bundle)
	}
	for _, x := range d.Narrative.Docs {
		ids = append(ids, x.ID)
	}
	return ids
}

// planEvents serves GET /garden/plan/{id}/events: an SSE stream that says "moved" (data {"id"}) when anything the
// cockpit draws moves (planIDs), as beadEvents does for one bead. The connect reads the plan as the viewer, so a plan the
// viewer cannot see is a 404. The ids are the ones drawn at connect: a step added later moves the plan (a new
// parent-child edge names both ends), the panel refetches, and the next connect (the panel's stream is opened once per
// plan) watches it, so the connection is renewed every planEventsRenew to pick new beads up.
func (h *Handler) planEvents(w http.ResponseWriter, r *http.Request) {
	if h.Hub == nil || h.Plans == nil {
		http.Error(w, "live plan cockpit not enabled", http.StatusServiceUnavailable)
		return
	}
	id := planID(r)
	if id == "" {
		http.Error(w, "bad plan id", http.StatusBadRequest)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	// The plan itself is subscribed before the read, so a move while it is out is not lost.
	moved, stop := h.Hub.moves.subscribe(id)
	defer stop()
	ctx, cancel := context.WithTimeout(r.Context(), planTimeout)
	doc, err := h.Plans.Plan(ctx, id)
	cancel()
	if err != nil || doc == nil {
		if err == nil {
			err = ErrNotPlan
		}
		planReadFailed(w, err)
		return
	}
	finishPlan(doc)
	rest, stopRest := h.Hub.moves.subscribeAll(planIDs(doc)[1:])
	defer stopRest()
	data, _ := json.Marshal(struct {
		ID string `json:"id"`
	}{id})

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	fl.Flush()

	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	renew := time.NewTimer(planEventsRenew)
	defer renew.Stop()
	var settle <-chan time.Time
	done := r.Context().Done()
	for {
		select {
		case <-done:
			return
		case <-renew.C:
			// end the stream; EventSource reconnects (retry above) and the next connect watches the beads drawn now
			return
		case _, ok := <-moved:
			if !ok {
				return
			}
			if settle == nil {
				settle = time.After(moveSettle)
			}
		case _, ok := <-rest:
			if !ok {
				return
			}
			if settle == nil {
				settle = time.After(moveSettle)
			}
		case <-settle:
			settle = nil
			fmt.Fprintf(w, "event: moved\ndata: %s\n\n", data)
			fl.Flush()
		case <-hb.C:
			fmt.Fprint(w, "event: hb\ndata: {}\n\n")
			fl.Flush()
		}
	}
}

// planEventsRenew is how long one plan stream watches the beads it drew at connect.
var planEventsRenew = 10 * time.Minute
