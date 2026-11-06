package planwindow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// LandPayload is the body of GET /v1/windows/self/view for a land window
// (Lands: the land window, plan gd-4iVYF9tGsn, steps LW.1 gd-BXNhQf4ZqV and
// LW.2 gd-y7CRpIXUTI): kbeads' windowLandView
// (kbeads/internal/server/window_view.go). Like Payload it is one contract
// with the daemon, pinned by testdata/land/daemon.json, a byte-for-byte copy
// of kbeads' window_land_view.golden.json (TestLandGoldenMatchesKbeads).
//
// It holds the land's open plans in creation order, then up to three closed
// ones, newest first. It carries no bead ids, no narrative, no Recent, no cost
// and no escalation titles. Decoding is lenient, as for Payload.
type LandPayload struct {
	// Land is the land's name: the project the window is on.
	Land  string          `json:"land"`
	Plans []LandPlanFacts `json:"plans"`
	// OpenPlans counts the land's open plans, which may be more than Plans
	// shows.
	OpenPlans int       `json:"open_plans"`
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LandPlanFacts is one plan of a land: the plan view's headline, its phases
// and unphased steps, its open decisions by title, its open decision and
// escalation counts, and its crew.
type LandPlanFacts struct {
	PlanFacts
	Phases []PhaseFacts `json:"phases"`
	// Steps are the steps in no phase: a flat plan's steps, or a phased
	// plan's top-level ones.
	Steps           []StepFacts `json:"steps"`
	OpenDecisions   int         `json:"open_decisions"`
	OpenEscalations int         `json:"open_escalations"`
	// Decisions are the open decisions' titles.
	Decisions []string     `json:"decisions"`
	Crew      []AgentFacts `json:"crew"`
	// Withheld is set when the plan's closure was past the daemon's walk
	// bound: only the headline is filled.
	Withheld  bool      `json:"withheld,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FieldLand is the hygiene field of a land's name.
const FieldLand = "land"

// LandGrants is the plain sentence about what a land window's link grants,
// printed by gb lands share with every mint and shown by the land's Share
// panel (Lands: the land window, LW.5 gd-aUQRspcbeS). It says what
// LandPayload carries and what it never does.
const LandGrants = "Anyone who has this link can watch the whole land, with no account, until it expires or is revoked: its World, and for every open plan and the last three finished ones, the title, state, phases and steps, the titles of open decisions, how many escalations wait, and the crew's handles. It shows no cost, comments, narratives or other lands, and nothing on it can be pressed. Chat apps that unfurl links (Slack, Discord, iMessage) will fetch the page too."

// errNotLand is DecodeLand's answer to a body with no land: a plan window's
// payload, or an empty object.
var errNotLand = errors.New("planwindow: not a land view")

// readPayload reads a view body of at most MaxPayloadBytes into v.
func readPayload(r io.Reader, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxPayloadBytes+1))
	if err != nil {
		return fmt.Errorf("planwindow: read payload: %w", err)
	}
	if len(b) > MaxPayloadBytes {
		return fmt.Errorf("planwindow: payload over %d bytes", MaxPayloadBytes)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("planwindow: decode payload: %w", err)
	}
	return nil
}

// DecodeLand reads a land payload of at most MaxPayloadBytes. A body that
// names no land is an error, so a plan window's payload can never be drawn as
// an empty land.
func DecodeLand(r io.Reader) (LandPayload, error) {
	var p LandPayload
	if err := readPayload(r, &p); err != nil {
		return LandPayload{}, err
	}
	if strings.TrimSpace(p.Land) == "" {
		return LandPayload{}, errNotLand
	}
	return p, nil
}

// LandView is a land payload after the hygiene check (PW-D5), in the
// payload's own shape: every free-text field (the land's name, the plans',
// phases' and steps' titles and short titles, the arms, the open decisions'
// titles and the crew's names) is trimmed and checked, and a hit reads
// WithheldText. Statuses and kinds are normalised to the bead vocabulary: an
// unknown step status reads open and an unknown kind task, as in View.
type LandView struct {
	LandPayload
	// Withheld lists the fields hygiene withheld, for the owner's preview.
	Withheld []Withheld `json:"-"`
}

// landStepStatuses are the step statuses a LandView keeps; any other reads
// open.
var landStepStatuses = map[string]bool{"open": true, "in_progress": true, "blocked": true, "deferred": true, "closed": true}

// CleanLand runs the hygiene check over a land payload. It is pure apart from
// counting withheld fields in WithheldTotal, and it never changes p.
func CleanLand(p LandPayload) LandView {
	var sc scrub
	v := LandView{LandPayload: LandPayload{
		Land:      sc.text(FieldLand, "land", strings.TrimSpace(p.Land)),
		Plans:     make([]LandPlanFacts, 0, len(p.Plans)),
		OpenPlans: max(p.OpenPlans, 0),
		ExpiresAt: p.ExpiresAt.UTC(),
		UpdatedAt: p.UpdatedAt.UTC(),
	}}
	for i, pl := range p.Plans {
		v.Plans = append(v.Plans, cleanLandPlan(&sc, fmt.Sprintf("plans[%d]", i), pl))
	}
	v.Withheld = sc.withheld
	return v
}

func cleanLandPlan(sc *scrub, path string, p LandPlanFacts) LandPlanFacts {
	out := LandPlanFacts{
		PlanFacts: PlanFacts{
			Title:        sc.text(FieldPlanTitle, path+".title", strings.TrimSpace(p.Title)),
			ShortTitle:   sc.text(FieldPlanTitle, path+".short_title", strings.TrimSpace(p.ShortTitle)),
			Heraldry:     sc.text(FieldPlanTitle, path+".heraldry", strings.TrimSpace(p.Heraldry)),
			Status:       strings.ToLower(strings.TrimSpace(p.Status)),
			State:        strings.ToLower(strings.TrimSpace(p.State)),
			CurrentPhase: max(p.CurrentPhase, 0),
			StepsDone:    max(p.StepsDone, 0),
			StepsTotal:   max(p.StepsTotal, 0),
		},
		Phases:          make([]PhaseFacts, 0, len(p.Phases)),
		OpenDecisions:   max(p.OpenDecisions, 0),
		OpenEscalations: max(p.OpenEscalations, 0),
		Decisions:       []string{},
		Crew:            []AgentFacts{},
		Withheld:        p.Withheld,
		UpdatedAt:       p.UpdatedAt.UTC(),
	}
	for i, ph := range p.Phases {
		pp := fmt.Sprintf("%s.phases[%d]", path, i)
		out.Phases = append(out.Phases, PhaseFacts{
			Number:     ph.Number,
			Title:      sc.text(FieldPhaseTitle, pp+".title", strings.TrimSpace(ph.Title)),
			ShortTitle: sc.text(FieldPhaseTitle, pp+".short_title", strings.TrimSpace(ph.ShortTitle)),
			Status:     strings.ToLower(strings.TrimSpace(ph.Status)),
			Steps:      cleanLandSteps(sc, pp+".steps", ph.Steps),
		})
	}
	out.Steps = cleanLandSteps(sc, path+".steps", p.Steps)
	for _, d := range p.Decisions {
		if d = strings.TrimSpace(d); d != "" {
			out.Decisions = append(out.Decisions, sc.text(FieldDecisionTitle, fmt.Sprintf("%s.decisions[%d]", path, len(out.Decisions)), d))
		}
	}
	out.OpenDecisions = max(out.OpenDecisions, len(out.Decisions))
	for _, a := range p.Crew {
		if a.Name = strings.TrimSpace(a.Name); a.Name != "" {
			out.Crew = append(out.Crew, AgentFacts{Name: sc.text(FieldAgentName, fmt.Sprintf("%s.crew[%d].name", path, len(out.Crew)), a.Name), Live: a.Live})
		}
	}
	return out
}

func cleanLandSteps(sc *scrub, path string, steps []StepFacts) []StepFacts {
	out := make([]StepFacts, 0, len(steps))
	for i, st := range steps {
		sp := fmt.Sprintf("%s[%d]", path, i)
		s := StepFacts{
			Number:     st.Number,
			Title:      sc.text(FieldStepTitle, sp+".title", strings.TrimSpace(st.Title)),
			ShortTitle: sc.text(FieldStepTitle, sp+".short_title", strings.TrimSpace(st.ShortTitle)),
			Status:     strings.ToLower(strings.TrimSpace(st.Status)),
			Kind:       strings.ToLower(strings.TrimSpace(st.Kind)),
		}
		if !landStepStatuses[s.Status] {
			s.Status = "open"
		}
		if !stepKinds[s.Kind] {
			s.Kind = "task"
		}
		if s.Status == "closed" && st.DoneAt != nil && !st.DoneAt.IsZero() {
			t := st.DoneAt.UTC()
			s.DoneAt = &t
		}
		out = append(out, s)
	}
	return out
}
