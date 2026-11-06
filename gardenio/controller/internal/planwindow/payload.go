package planwindow

import (
	"io"
	"time"
)

// MaxPayloadBytes caps a /v1/windows/self/view body. The largest part is the
// narrative (a bundle doc is capped at 128 KiB, internal/plans MaxDocLen); a
// plan with a few hundred steps is well under the rest. A bigger body is an
// error, which the page answers as its one 404 (PW-D8).
const MaxPayloadBytes = 2 << 20

// Payload is the body of GET /v1/windows/self/view: the daemon's projection
// of one plan's closure (narrative §3 PW-D4), kbeads' windowView
// (kbeads/internal/server/window_view.go). The two are one contract (the plan
// gardener's ruling, gd-oFsT8A8ndL): a field changes in both, with a comment
// on PW.1 gd-sCLo2acEFb and PW.2 gd-jX6ogU9l5T.
//
// The daemon decides which fields leave it, so every field here is already on
// the allowlist; the read model still runs hygiene over every string (PW-D5).
// It carries no bead ids: phases and steps are positional, in display order.
//
// Decoding is lenient: a key the read model does not know is ignored, and
// since View is built only from the fields below, an unknown key can never
// reach the page.
type Payload struct {
	Plan PlanFacts `json:"plan"`
	// Phases are the plan's phases in phase_order, each with its steps.
	// Empty for a flat plan.
	Phases []PhaseFacts `json:"phases"`
	// Steps are a flat plan's steps; empty when the plan is phased.
	Steps       []StepFacts     `json:"steps"`
	Decisions   []DecisionFacts `json:"decisions"`
	Escalations EscalationFacts `json:"escalations"`
	// Crew are the open type=agent beads reached by a reverse assigned edge
	// from a closure bead.
	Crew      []AgentFacts   `json:"crew"`
	Narrative NarrativeFacts `json:"narrative"`
	// Recent is the closure's state changes, newest first, at most ten. Build
	// sorts and caps again rather than trust it.
	Recent    []ChangeFacts `json:"recent"`
	ExpiresAt time.Time     `json:"expires_at"`
	// UpdatedAt is the newest updated_at in the closure.
	UpdatedAt time.Time `json:"updated_at"`
}

// PlanFacts is the plan epic's headline.
type PlanFacts struct {
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Heraldry   string `json:"heraldry,omitempty"`
	// Status is the epic's bead status.
	Status string `json:"status"`
	// State is fields.plan_state: draft, ready, building, complete,
	// abandoned, or "" for a plan that never entered the lifecycle.
	State string `json:"state,omitempty"`
	// CurrentPhase is the 1-based number of the first phase with a step not
	// closed (else the first phase not closed), 0 when there is none.
	CurrentPhase int `json:"current_phase"`
	StepsDone    int `json:"steps_done"`
	StepsTotal   int `json:"steps_total"`
}

// PhaseFacts is one phase sub-epic with its steps.
type PhaseFacts struct {
	Number     int         `json:"number"`
	Title      string      `json:"title"`
	ShortTitle string      `json:"short_title,omitempty"`
	Status     string      `json:"status"`
	Steps      []StepFacts `json:"steps"`
}

// StepFacts is one step, numbered within its phase or the flat list.
type StepFacts struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	// Status is the bead status: open, in_progress, blocked, deferred or
	// closed.
	Status string `json:"status"`
	// Kind is the bead type: task, bug, feature or chore.
	Kind string `json:"kind"`
	// DoneAt is closed_at, for a closed step.
	DoneAt *time.Time `json:"done_at,omitempty"`
}

// DecisionFacts is one decision in the closure.
type DecisionFacts struct {
	Title string `json:"title"`
	// State is open, resolved or cancelled.
	State string `json:"state"`
	// Chosen is the label of the chosen option, for a resolved decision.
	Chosen string `json:"chosen,omitempty"`
}

// EscalationFacts counts the closure's escalations. An item has no title on
// purpose: a bridge escalation's title embeds the question or command it
// carries (PW-D4).
type EscalationFacts struct {
	Open  int              `json:"open"`
	Total int              `json:"total"`
	Items []EscalationItem `json:"items"`
}

// EscalationItem is one escalation.
type EscalationItem struct {
	OperationType string `json:"operation_type"`
	// Sensitivity is public, internal or secret. The read model shows none of
	// them apart; it is carried so the owner's preview (PW.4) can.
	Sensitivity string `json:"sensitivity,omitempty"`
	// State is open or closed.
	State string `json:"state"`
}

// AgentFacts is one crew agent: its name and whether it is working now.
type AgentFacts struct {
	Name string `json:"name"`
	Live bool   `json:"live"`
}

// NarrativeFacts is the plan's narrative bundle: its headline and its
// markdown docs in position order.
type NarrativeFacts struct {
	Headline string    `json:"headline,omitempty"`
	Docs     []DocFact `json:"docs"`
}

// DocFact is one narrative doc.
type DocFact struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// The Recent strip's kinds.
const (
	KindStepClosed        = "step_closed"
	KindDecisionOpened    = "decision_opened"
	KindDecisionResolved  = "decision_resolved"
	KindDecisionCancelled = "decision_cancelled"
	KindEscalationOpened  = "escalation_opened"
	KindEscalationClosed  = "escalation_closed"
)

// ChangeFacts is one state change inside the closure.
type ChangeFacts struct {
	At time.Time `json:"at"`
	// Kind is one of the Kind* constants.
	Kind string `json:"kind"`
	// Subject is the step's or decision's title, or an escalation's
	// operation type.
	Subject string `json:"subject"`
}

// Decode reads a payload of at most MaxPayloadBytes.
func Decode(r io.Reader) (Payload, error) {
	var p Payload
	if err := readPayload(r, &p); err != nil {
		return Payload{}, err
	}
	return p, nil
}
