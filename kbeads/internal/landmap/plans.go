package landmap

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

var planTypes = []model.BeadType{model.TypeEpic}

// planLabel marks an epic as a plan (the daemon's planMarkerLabel).
const planLabel = "plan"

func (m *Map) ListPlans(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListPlansParams) {
	if !m.admit(w, r, land) {
		return
	}
	q := url.Values{"sort": {"-updated_at"}, "labels": {"project:" + land + "," + planLabel}}
	beads, next, ok := m.list(w, r, "listPlans", land, q, params.Cursor, params.Limit, planTypes, nil)
	if !ok {
		return
	}
	page := landsv1.PlanPage{Items: make([]landsv1.Plan, 0, len(beads)), NextCursor: next}
	for i := range beads {
		steps, waiting, ok := m.steps(w, r, "listPlans", land, beads[i].ID)
		if !ok {
			return
		}
		page.Items = append(page.Items, toPlan(&beads[i], steps, waiting))
	}
	writeJSON(w, http.StatusOK, page)
}

// steps reads a plan's children in the land, and which of them wait.
func (m *Map) steps(w http.ResponseWriter, r *http.Request, op, land, plan string) ([]beadRecord, map[string]bool, bool) {
	out, waiting, err := m.readSteps(r, land, plan)
	if err != nil {
		fail(w, r, op, err)
		return nil, nil, false
	}
	return out, waiting, true
}

// readSteps is steps for a caller that writes no problem (the feed).
func (m *Map) readSteps(r *http.Request, land, plan string) ([]beadRecord, map[string]bool, error) {
	q := url.Values{
		"parent":             {plan},
		"labels":             {"project:" + land},
		"type":               {joinTypes(ticketTypes)},
		"sort":               {"created_at"},
		"limit":              {strconv.Itoa(maxPlanSteps)},
		"with_open_blockers": {"true"},
	}
	var pg beadPage
	if err := m.d.call(r, rListBeads, nil, q, nil, &pg); err != nil {
		return nil, nil, err
	}
	out := pg.Beads[:0]
	for _, b := range pg.Beads {
		if b.inLand(land) {
			out = append(out, b)
		}
	}
	waiting, err := m.waiting(r, land, out)
	if err != nil {
		return nil, nil, err
	}
	return out, waiting, nil
}

// getPlan reads a plan: an epic in the land carrying the plan label.
func (m *Map) getPlan(w http.ResponseWriter, r *http.Request, op, land, plan string) (*beadRecord, bool) {
	b, ok := m.getBead(w, r, op, land, plan, planTypes...)
	if !ok {
		return nil, false
	}
	if !slices.Contains(b.Labels, planLabel) {
		problem(w, landsv1.NotFound, "")
		return nil, false
	}
	return b, true
}

func (m *Map) GetPlan(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, plan landsv1.PlanPath) {
	if !m.admit(w, r, land) {
		return
	}
	b, ok := m.getPlan(w, r, "getPlan", land, plan)
	if !ok {
		return
	}
	steps, waiting, ok := m.steps(w, r, "getPlan", land, plan)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toPlan(b, steps, waiting))
}

func (m *Map) AnswerPlan(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, plan landsv1.PlanPath, _ landsv1.AnswerPlanParams) {
	if !m.writable(w, r, "answerPlan", land) {
		return
	}
	var in landsv1.PlanAnswer
	if !decodeBody(w, r, &in) {
		return
	}
	if _, ok := m.getPlan(w, r, "answerPlan", land, plan); !ok {
		return
	}
	d, ok := m.getBead(w, r, "answerPlan", land, in.DecisionId, decisionTypes...)
	if !ok {
		return
	}
	if d.fieldString("requesting_plan") != plan {
		problem(w, landsv1.NotFound, "")
		return
	}
	m.resolve(w, r, "answerPlan", land, d, in.OptionId, in.Note)
}
