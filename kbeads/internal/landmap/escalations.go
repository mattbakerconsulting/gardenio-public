package landmap

import (
	"net/http"
	"net/url"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

var escalationTypes = []model.BeadType{model.TypeEscalation}

// tenantEscalationOperation is the operation_type of an escalation a tenant
// files: a request an operator carries out by hand. A scoped principal may
// not approve it (scopedApprovableOperations), so it waits for the operator.
const tenantEscalationOperation = "manual_operator_action"

func (m *Map) ListEscalations(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListEscalationsParams) {
	if !m.admit(w, r, land) {
		return
	}
	q := url.Values{"sort": {"-created_at"}}
	var keep func(*beadRecord) bool
	if params.Status != nil {
		want := *params.Status
		switch want {
		case landsv1.EscalationStatusOpen, landsv1.EscalationStatusApproved, landsv1.EscalationStatusRejected:
			keep = func(b *beadRecord) bool { return escalationStatus(b) == want }
		default:
			problem(w, landsv1.InvalidRequest, "status is not an escalation status")
			return
		}
		if want == landsv1.EscalationStatusRejected {
			q.Set("status", string(model.StatusClosed))
		}
	}
	beads, next, ok := m.list(w, r, "listEscalations", land, q, params.Cursor, params.Limit, escalationTypes, keep)
	if !ok {
		return
	}
	page := landsv1.EscalationPage{Items: make([]landsv1.Escalation, 0, len(beads)), NextCursor: next}
	for i := range beads {
		page.Items = append(page.Items, toEscalation(&beads[i]))
	}
	writeJSON(w, http.StatusOK, page)
}

func (m *Map) GetEscalation(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, escalation landsv1.Id) {
	if !m.admit(w, r, land) {
		return
	}
	if b, ok := m.getBead(w, r, "getEscalation", land, escalation, escalationTypes...); ok {
		writeJSON(w, http.StatusOK, toEscalation(b))
	}
}

func (m *Map) CreateEscalation(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.CreateEscalationParams) {
	if !m.writable(w, r, "createEscalation", land) {
		return
	}
	var in landsv1.EscalationCreate
	if !decodeBody(w, r, &in) || !textOK(w, "title", in.Title, 1, 500) || !textOK(w, "reason", in.Reason, 1, 65536) {
		return
	}
	body := map[string]any{
		"title":       in.Title,
		"type":        string(model.TypeEscalation),
		"kind":        string(model.KindData),
		"description": in.Reason,
		"labels":      []string{"project:" + land},
		"fields": map[string]any{
			"operation_type": tenantEscalationOperation,
			// The ask itself, so the create dedupe tells two asks apart.
			"operation_spec": map[string]any{"title": in.Title, "reason": in.Reason},
			"reason":         in.Reason,
		},
	}
	if params.IdempotencyKey != nil {
		body["idempotency_key"] = *params.IdempotencyKey
	}
	var b beadRecord
	if err := m.d.call(r, rCreateBead, nil, nil, body, &b); err != nil {
		fail(w, r, "createEscalation", err)
		return
	}
	// The create response is the bead before the escalation hooks ran; read
	// it back so the answer is what a get would say.
	got, ok := m.getBead(w, r, "createEscalation", land, b.ID, escalationTypes...)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, toEscalation(got))
}
