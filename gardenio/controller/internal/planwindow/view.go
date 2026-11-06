package planwindow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RecentLimit is how many changes the Recent strip shows.
const RecentLimit = 10

// View is what a window shows: the page renders it and plan.json is its JSON.
// Every string in it has been through the hygiene check (a withheld one reads
// WithheldText), and it holds no bead ids, no people and no URLs.
type View struct {
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Heraldry   string `json:"heraldry,omitempty"`
	// State is the plan's lifecycle state: draft, ready, building, complete,
	// abandoned, or active for a plan that never entered the lifecycle.
	State      string `json:"state"`
	StateLabel string `json:"state_label"`
	// Phase is the current phase's title. "" for a flat plan, or when no
	// phase is current.
	Phase    string   `json:"phase,omitempty"`
	Progress Progress `json:"progress"`
	// Phases are the plan's phases in order with their steps. A flat plan has
	// one untitled phase; a phased plan's top-level steps follow its phases
	// under OtherStepsTitle.
	Phases  []Phase  `json:"phases"`
	Waiting Waiting  `json:"waiting"`
	Crew    []Agent  `json:"crew"`
	Recent  []Change `json:"recent"`
	// Headline is the narrative's one-line status.
	Headline  string `json:"headline,omitempty"`
	Narrative []Doc  `json:"narrative"`
	// ExpiresAt is when the window stops opening.
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Withheld lists the fields hygiene withheld, for the owner's preview. It
	// is not part of plan.json.
	Withheld []Withheld `json:"-"`
}

// Progress is how many steps are done.
type Progress struct {
	Done    int `json:"done"`
	Total   int `json:"total"`
	Percent int `json:"percent"`
}

// Phase is one phase and its steps.
type Phase struct {
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	// Status is done (every step done, or a closed phase with none), working
	// (some step started or done) or todo.
	Status string `json:"status"`
	Done   int    `json:"done"`
	Total  int    `json:"total"`
	Steps  []Step `json:"steps"`
}

// OtherStepsTitle heads a phased plan's top-level steps, the ones in no
// phase. It is the read model's own words, so hygiene does not check it.
const OtherStepsTitle = "Other steps"

// Step statuses, the page's vocabulary for the bead statuses.
const (
	StatusTodo     = "todo"
	StatusWorking  = "working"
	StatusBlocked  = "blocked"
	StatusDeferred = "deferred"
	StatusDone     = "done"
)

// Step is one step.
type Step struct {
	Title      string     `json:"title"`
	ShortTitle string     `json:"short_title,omitempty"`
	Status     string     `json:"status"`
	Kind       string     `json:"kind"`
	DoneAt     *time.Time `json:"done_at,omitempty"`
}

// Waiting is what is waiting on the operator.
type Waiting struct {
	// Decisions are the plan's decisions: open ones first, then resolved,
	// then cancelled.
	Decisions   []Decision        `json:"decisions"`
	Escalations EscalationSummary `json:"escalations"`
}

// Decision states.
const (
	DecisionOpen      = "open"
	DecisionResolved  = "resolved"
	DecisionCancelled = "cancelled"
)

// Decision is one decision by title, with the chosen option once resolved.
type Decision struct {
	Title  string `json:"title"`
	State  string `json:"state"`
	Chosen string `json:"chosen,omitempty"`
}

// EscalationSummary counts escalations, never naming one: "2 waiting, 1
// needs a merge".
type EscalationSummary struct {
	Open  int    `json:"open"`
	Total int    `json:"total"`
	Needs []Need `json:"needs,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Need is how many open escalations need one kind of operator action.
type Need struct {
	What  string `json:"what"`
	Count int    `json:"count"`
}

// Agent is one crew handle and whether it is live.
type Agent struct {
	Name string `json:"name"`
	Live bool   `json:"live"`
}

// Change is one row of the Recent strip.
type Change struct {
	At time.Time `json:"at"`
	// Kind is one of the Kind* constants.
	Kind string `json:"kind"`
	// Title is the step or decision title; for an escalation, what it needs
	// ("a merge"), never its title or operation type.
	Title string `json:"title"`
}

// Doc is one narrative doc: its title and its blocks (paragraphs, lists,
// headings), split on blank lines and each checked on its own, so one
// internal reference withholds its block rather than the whole doc. Markdown,
// rendered by the page as text.
type Doc struct {
	Title  string   `json:"title,omitempty"`
	Blocks []string `json:"blocks"`
}

var stateLabels = map[string]string{
	"draft":     "Draft",
	"ready":     "Ready",
	"building":  "Building",
	"complete":  "Complete",
	"abandoned": "Abandoned",
	"active":    "Active",
}

var stepKinds = map[string]bool{"task": true, "bug": true, "feature": true, "chore": true, "epic": true}

var recentKinds = map[string]bool{
	KindStepClosed: true, KindDecisionOpened: true, KindDecisionResolved: true,
	KindDecisionCancelled: true, KindEscalationOpened: true, KindEscalationClosed: true,
}

// needs names what an escalation's operation type asks the operator for. A
// type not listed needs "an operator".
var needs = map[string]string{
	"gitlab_mr_merge":   "a merge",
	"github_pr_merge":   "a merge",
	"helm_deploy":       "a deploy",
	"agent_despawn":     "a despawn",
	"secret_mint":       "a secret",
	"github_repo_grant": "access",
	"aws_iam":           "access",
}

const needOperator = "an operator"

// Build turns a payload into the View. It is pure apart from counting withheld
// fields in WithheldTotal: it calls nothing and reads no clock.
//
// Progress and the current phase are the daemon's (plan.steps_done,
// steps_total, current_phase). The read model works them out from the steps
// only as a fallback, when the payload carries steps but no steps_total.
func Build(p Payload) View {
	var sc scrub
	v := View{
		Title:      sc.text(FieldPlanTitle, "title", strings.TrimSpace(p.Plan.Title)),
		ShortTitle: sc.text(FieldPlanTitle, "short_title", strings.TrimSpace(p.Plan.ShortTitle)),
		Heraldry:   sc.text(FieldPlanTitle, "heraldry", strings.TrimSpace(p.Plan.Heraldry)),
		State:      planState(p.Plan.State),
		ExpiresAt:  p.ExpiresAt.UTC(),
		UpdatedAt:  p.UpdatedAt.UTC(),
	}
	v.StateLabel = stateLabels[v.State]
	v.Phases = buildPhases(&sc, p)
	v.Progress, v.Phase = progress(p, v.Phases)
	v.Waiting = buildWaiting(&sc, p)
	v.Crew = buildCrew(&sc, p.Crew)
	v.Recent = buildRecent(&sc, p.Recent)
	v.Headline = sc.text(FieldNarrative, "headline", strings.TrimSpace(p.Narrative.Headline))
	v.Narrative = buildNarrative(&sc, p.Narrative.Docs)
	v.Withheld = sc.withheld
	return v
}

// StateLabel is a plan state's words, as View.StateLabel says them: an
// unknown state is Active.
func StateLabel(state string) string { return stateLabels[planState(state)] }

func planState(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := stateLabels[s]; ok && s != "active" {
		return s
	}
	return "active"
}

func stepStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "closed":
		return StatusDone
	case "in_progress":
		return StatusWorking
	case "blocked":
		return StatusBlocked
	case "deferred":
		return StatusDeferred
	default:
		return StatusTodo
	}
}

// progress is the daemon's progress and current phase, or the read model's
// own when the payload has steps but no steps_total.
func progress(p Payload, phases []Phase) (Progress, string) {
	var counted Progress
	for _, ph := range phases {
		counted.Done += ph.Done
		counted.Total += ph.Total
	}
	pr := Progress{Done: p.Plan.StepsDone, Total: p.Plan.StepsTotal}
	fallback := pr.Total <= 0 && counted.Total > 0
	if fallback {
		pr = counted
	}
	pr.Total = max(pr.Total, 0)
	pr.Done = min(max(pr.Done, 0), pr.Total)
	if pr.Total > 0 {
		pr.Percent = pr.Done * 100 / pr.Total
	}

	if len(p.Phases) == 0 {
		return pr, ""
	}
	if !fallback {
		if n := p.Plan.CurrentPhase; n >= 1 && n <= len(p.Phases) {
			return pr, phases[n-1].Title
		} else if n == 0 {
			return pr, ""
		}
	}
	for _, ph := range phases[:len(p.Phases)] {
		if ph.Done < ph.Total {
			return pr, ph.Title
		}
	}
	return pr, ""
}

func buildSteps(sc *scrub, phase int, steps []StepFacts) []Step {
	out := make([]Step, 0, len(steps))
	for i, st := range steps {
		path := fmt.Sprintf("phases[%d].steps[%d]", phase, i)
		s := Step{
			Title:      sc.text(FieldStepTitle, path+".title", strings.TrimSpace(st.Title)),
			ShortTitle: sc.text(FieldStepTitle, path+".short_title", strings.TrimSpace(st.ShortTitle)),
			Status:     stepStatus(st.Status),
			Kind:       strings.ToLower(strings.TrimSpace(st.Kind)),
		}
		if !stepKinds[s.Kind] {
			s.Kind = "task"
		}
		if s.Status == StatusDone && st.DoneAt != nil && !st.DoneAt.IsZero() {
			t := st.DoneAt.UTC()
			s.DoneAt = &t
		}
		out = append(out, s)
	}
	return out
}

func buildPhases(sc *scrub, p Payload) []Phase {
	phases := make([]Phase, 0, len(p.Phases)+1)
	for i, ph := range p.Phases {
		path := fmt.Sprintf("phases[%d]", i)
		phases = append(phases, Phase{
			Title:      sc.text(FieldPhaseTitle, path+".title", strings.TrimSpace(ph.Title)),
			ShortTitle: sc.text(FieldPhaseTitle, path+".short_title", strings.TrimSpace(ph.ShortTitle)),
			Status:     strings.ToLower(strings.TrimSpace(ph.Status)),
			Steps:      buildSteps(sc, i, ph.Steps),
		})
	}
	// A flat plan's steps go in one untitled phase. A phased plan can carry
	// top-level steps too (steps that are children of the epic, not of a
	// phase; PW.1 sends them beside the phases): they follow the titled
	// phases as OtherStepsTitle rather than vanish or sit under no heading.
	if len(p.Steps) > 0 || len(phases) == 0 {
		loose := Phase{Steps: buildSteps(sc, len(phases), p.Steps)}
		if len(phases) > 0 {
			loose.Title = OtherStepsTitle
		}
		phases = append(phases, loose)
	}
	for i := range phases {
		ph := &phases[i]
		closed := ph.Status == "closed"
		started := false
		for _, s := range ph.Steps {
			ph.Total++
			switch s.Status {
			case StatusDone:
				ph.Done++
				started = true
			case StatusWorking:
				started = true
			}
		}
		switch {
		case ph.Total > 0 && ph.Done == ph.Total, ph.Total == 0 && closed:
			ph.Status = StatusDone
		case started:
			ph.Status = StatusWorking
		default:
			ph.Status = StatusTodo
		}
	}
	return phases
}

func decisionState(s string) string {
	switch s = strings.ToLower(strings.TrimSpace(s)); s {
	case DecisionResolved, DecisionCancelled:
		return s
	default:
		return DecisionOpen
	}
}

func buildWaiting(sc *scrub, p Payload) Waiting {
	w := Waiting{Decisions: []Decision{}}
	byState := map[string][]DecisionFacts{}
	for _, d := range p.Decisions {
		st := decisionState(d.State)
		byState[st] = append(byState[st], d)
	}
	for _, st := range []string{DecisionOpen, DecisionResolved, DecisionCancelled} {
		for _, d := range byState[st] {
			path := fmt.Sprintf("waiting.decisions[%d]", len(w.Decisions))
			dv := Decision{Title: sc.text(FieldDecisionTitle, path+".title", strings.TrimSpace(d.Title)), State: st}
			if st == DecisionResolved {
				dv.Chosen = sc.text(FieldDecisionChosen, path+".chosen", strings.TrimSpace(d.Chosen))
			}
			w.Decisions = append(w.Decisions, dv)
		}
	}

	e := &w.Escalations
	open, counts := 0, map[string]int{}
	for _, it := range p.Escalations.Items {
		if strings.ToLower(strings.TrimSpace(it.State)) == "closed" {
			continue
		}
		open++
		counts[need(it.OperationType)]++
	}
	e.Open = max(p.Escalations.Open, open)
	e.Total = max(p.Escalations.Total, len(p.Escalations.Items), e.Open)
	for what, n := range counts {
		e.Needs = append(e.Needs, Need{What: what, Count: n})
	}
	sort.Slice(e.Needs, func(i, j int) bool {
		a, b := e.Needs[i], e.Needs[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.What < b.What
	})
	e.Text = escalationText(*e)
	return w
}

func need(operationType string) string {
	if n, ok := needs[strings.ToLower(strings.TrimSpace(operationType))]; ok {
		return n
	}
	return needOperator
}

// escalationText says the summary in words: "2 waiting, 1 needs a merge". A
// generic "needs an operator" adds nothing to "waiting", so it is left out.
func escalationText(s EscalationSummary) string {
	if s.Open == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d waiting", s.Open)}
	for _, n := range s.Needs {
		if n.What == needOperator {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s %s", n.Count, plural(n.Count, "needs", "need"), n.What))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// buildCrew lists live agents first, then by name.
func buildCrew(sc *scrub, agents []AgentFacts) []Agent {
	var named []AgentFacts
	for _, a := range agents {
		a.Name = strings.TrimSpace(a.Name)
		if a.Name != "" {
			named = append(named, a)
		}
	}
	sort.SliceStable(named, func(i, j int) bool {
		if named[i].Live != named[j].Live {
			return named[i].Live
		}
		return named[i].Name < named[j].Name
	})
	crew := make([]Agent, 0, len(named))
	for i, a := range named {
		crew = append(crew, Agent{Name: sc.text(FieldAgentName, fmt.Sprintf("crew[%d].name", i), a.Name), Live: a.Live})
	}
	return crew
}

// buildRecent keeps the newest RecentLimit changes, newest first. A change
// with no time or a kind off the allowlist is dropped. Ties keep the daemon's
// order.
func buildRecent(sc *scrub, changes []ChangeFacts) []Change {
	var rows []ChangeFacts
	for _, c := range changes {
		c.Kind = strings.ToLower(strings.TrimSpace(c.Kind))
		if c.At.IsZero() || !recentKinds[c.Kind] {
			continue
		}
		rows = append(rows, c)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	if len(rows) > RecentLimit {
		rows = rows[:RecentLimit]
	}
	out := make([]Change, 0, len(rows))
	for n, c := range rows {
		ch := Change{At: c.At.UTC(), Kind: c.Kind}
		if strings.HasPrefix(c.Kind, "escalation_") {
			ch.Title = need(c.Subject)
		} else {
			ch.Title = sc.text(FieldRecentTitle, fmt.Sprintf("recent[%d].title", n), strings.TrimSpace(c.Subject))
		}
		out = append(out, ch)
	}
	return out
}

var blankLines = regexp.MustCompile(`\n[ \t]*\n`)

// buildNarrative splits each doc into blocks on blank lines and checks each
// one. A doc with no text is dropped.
func buildNarrative(sc *scrub, docs []DocFact) []Doc {
	out := []Doc{}
	for _, d := range docs {
		var blocks []string
		for _, block := range blankLines.Split(strings.ReplaceAll(d.Content, "\r\n", "\n"), -1) {
			if block = strings.Trim(block, "\n"); strings.TrimSpace(block) != "" {
				blocks = append(blocks, block)
			}
		}
		if len(blocks) == 0 {
			continue
		}
		path := fmt.Sprintf("narrative[%d]", len(out))
		doc := Doc{Title: sc.text(FieldNarrative, path+".title", strings.TrimSpace(d.Title)), Blocks: make([]string, len(blocks))}
		for i, b := range blocks {
			doc.Blocks[i] = sc.text(FieldNarrative, fmt.Sprintf("%s.blocks[%d]", path, i), b)
		}
		out = append(out, doc)
	}
	return out
}
