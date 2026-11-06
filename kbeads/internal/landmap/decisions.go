package landmap

import (
	"net/http"
	"net/url"
	"slices"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

var decisionTypes = []model.BeadType{model.TypeDecision}

func (m *Map) ListDecisions(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListDecisionsParams) {
	if !m.admit(w, r, land) {
		return
	}
	q := url.Values{"sort": {"-created_at"}}
	var keep func(*beadRecord) bool
	if params.Status != nil {
		want := *params.Status
		switch want {
		case landsv1.DecisionStatusOpen:
			q.Set("status", joinStatuses([]model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred}))
		case landsv1.DecisionStatusResolved, landsv1.DecisionStatusCancelled:
			q.Set("status", string(model.StatusClosed))
			keep = func(b *beadRecord) bool { return decisionStatus(b) == want }
		default:
			problem(w, landsv1.InvalidRequest, "status is not a decision status")
			return
		}
	}
	beads, next, ok := m.list(w, r, "listDecisions", land, q, params.Cursor, params.Limit, decisionTypes, keep)
	if !ok {
		return
	}
	page := landsv1.DecisionPage{Items: make([]landsv1.Decision, 0, len(beads)), NextCursor: next}
	for i := range beads {
		page.Items = append(page.Items, toDecision(&beads[i]))
	}
	writeJSON(w, http.StatusOK, page)
}

func (m *Map) GetDecision(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, decision landsv1.DecisionPath) {
	if !m.admit(w, r, land) {
		return
	}
	if b, ok := m.getBead(w, r, "getDecision", land, decision, decisionTypes...); ok {
		writeJSON(w, http.StatusOK, m.withEvidence(r, land, b, toDecision(b)))
	}
}

func (m *Map) ResolveDecision(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, decision landsv1.DecisionPath, _ landsv1.ResolveDecisionParams) {
	if !m.writable(w, r, "resolveDecision", land) {
		return
	}
	var in landsv1.DecisionResolve
	if !decodeBody(w, r, &in) {
		return
	}
	b, ok := m.getBead(w, r, "resolveDecision", land, decision, decisionTypes...)
	if !ok {
		return
	}
	m.resolve(w, r, "resolveDecision", land, b, in.OptionId, in.Note)
}

// resolve answers an open decision with one of its options. Resolving it
// again with the option it was resolved with is a no-op, so a retry needs no
// key; any other answer to a closed decision is a conflict.
func (m *Map) resolve(w http.ResponseWriter, r *http.Request, op, land string, b *beadRecord, option string, note *string) {
	if note != nil && !textOK(w, "note", *note, 0, 2000) {
		return
	}
	d := toDecision(b)
	if !slices.ContainsFunc(d.Options, func(o landsv1.DecisionOption) bool { return o.Id == option }) {
		problem(w, landsv1.InvalidRequest, "option_id is not one of the decision's options")
		return
	}
	switch {
	case d.Status == landsv1.DecisionStatusResolved && d.Chosen != nil && *d.Chosen == option:
	case d.Status != landsv1.DecisionStatusOpen:
		problem(w, landsv1.Conflict, "")
		return
	default:
		body := map[string]any{"selected_option": option}
		if note != nil {
			body["response_text"] = *note
		}
		if err := m.d.call(r, rResolveDecision, []string{b.ID}, nil, body, nil); err != nil {
			fail(w, r, op, err)
			return
		}
	}
	m.GetDecision(w, r, land, b.ID)
}

func (m *Map) CancelDecision(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, decision landsv1.DecisionPath, _ landsv1.CancelDecisionParams) {
	if !m.writable(w, r, "cancelDecision", land) {
		return
	}
	var in landsv1.DecisionCancel
	if !decodeBody(w, r, &in) || !textOK(w, "reason", in.Reason, 1, 2000) {
		return
	}
	b, ok := m.getBead(w, r, "cancelDecision", land, decision, decisionTypes...)
	if !ok {
		return
	}
	switch decisionStatus(b) {
	case landsv1.DecisionStatusCancelled:
	case landsv1.DecisionStatusResolved:
		problem(w, landsv1.Conflict, "")
		return
	default:
		if err := m.d.call(r, rCancelDecision, []string{decision}, nil, map[string]any{"reason": in.Reason}, nil); err != nil {
			fail(w, r, "cancelDecision", err)
			return
		}
	}
	m.GetDecision(w, r, land, decision)
}

func (m *Map) CommentDecision(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, decision landsv1.DecisionPath, _ landsv1.CommentDecisionParams) {
	m.comment(w, r, "commentDecision", land, decision, decisionTypes...)
}
