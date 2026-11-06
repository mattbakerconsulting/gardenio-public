package landmap

import (
	"errors"
	"net/http"
	"net/url"
	"slices"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

func (m *Map) ListTickets(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListTicketsParams) {
	if !m.admit(w, r, land) {
		return
	}
	q := url.Values{"sort": {"-updated_at"}}
	if params.Status != nil {
		st := ticketStatusFilter(*params.Status)
		if st == nil {
			problem(w, landsv1.InvalidRequest, "status is not a ticket status")
			return
		}
		q.Set("status", joinStatuses(st))
	}
	m.ticketPage(w, r, "listTickets", land, q, params.Status, params.Cursor, params.Limit)
}

func (m *Map) SearchTickets(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.SearchTicketsParams) {
	if !m.admit(w, r, land) || !textOK(w, "q", params.Q, 1, 256) {
		return
	}
	m.ticketPage(w, r, "searchTickets", land, url.Values{"search": {params.Q}}, nil, params.Cursor, params.Limit)
}

// ticketPage lists one page of the land's tickets. With want set it keeps
// only the tickets that read as that status; the page may then be short.
func (m *Map) ticketPage(w http.ResponseWriter, r *http.Request, op, land string, q url.Values, want *landsv1.TicketStatus, cursor *landsv1.Cursor, limit *int) {
	q.Set("with_open_blockers", "true")
	beads, next, ok := m.list(w, r, op, land, q, cursor, limit, ticketTypes, nil)
	if !ok {
		return
	}
	waiting, err := m.waiting(r, land, beads)
	if err != nil {
		fail(w, r, op, err)
		return
	}
	page := landsv1.TicketPage{Items: make([]landsv1.Ticket, 0, len(beads)), NextCursor: next}
	for i := range beads {
		if t := toTicket(&beads[i], waiting[beads[i].ID]); want == nil || t.Status == *want {
			page.Items = append(page.Items, t)
		}
	}
	writeJSON(w, http.StatusOK, page)
}

// waiting is the contract's waiting rule (TicketStatus, openapi.yaml): a
// ticket waits when one of its open blockers is something the land can see,
// a ticket, decision, escalation or plan of this land, read as the caller. A
// ticket blocked on anything else (another land, a type the land never sees,
// a blocked status set with no blocker) does not. It answers for the given
// records, which are tickets of the land.
func (m *Map) waiting(r *http.Request, land string, beads []beadRecord) (map[string]bool, error) {
	out := map[string]bool{}
	sees := map[string]bool{}
	for i := range beads {
		b := &beads[i]
		if b.Status == model.StatusClosed {
			continue
		}
		for _, id := range b.blockers() {
			seen, ok := sees[id]
			if !ok {
				var err error
				if seen, err = m.landSees(r, land, id); err != nil {
					return nil, err
				}
				sees[id] = seen
			}
			if seen {
				out[b.ID] = true
				break
			}
		}
	}
	return out, nil
}

// blockers are the ids that may hold b back: its open blockers when the list
// read them, else its blocking edges. landSees decides each.
func (b *beadRecord) blockers() []string {
	var out []string
	if b.OpenBlockers != nil {
		for _, x := range *b.OpenBlockers {
			if !x.OutOfScope && seenType(x.Type) {
				out = append(out, x.ID)
			}
		}
		return out
	}
	for _, d := range b.Dependencies {
		if slices.Contains(blockingDeps, d.Type) && d.DependsOnID != "" {
			out = append(out, d.DependsOnID)
		}
	}
	return out
}

// seenType reports whether a land is ever shown a bead of type t.
func seenType(t model.BeadType) bool {
	return isTicketType(t) || t == model.TypeDecision || t == model.TypeEscalation || slices.Contains(planTypes, t)
}

// landSees reports whether blocker id is open and something the land can see:
// readable by the caller, in the land, and a noun the land is shown. A
// refused or absent read is a blocker the land cannot see.
func (m *Map) landSees(r *http.Request, land, id string) (bool, error) {
	var b beadRecord
	if err := m.d.call(r, rGetBead, []string{id}, nil, nil, &b); err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && (ue.status == http.StatusForbidden || ue.status == http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	if !b.inLand(land) || !seenType(b.Type) || !slices.Contains(openStatuses, b.Status) {
		return false, nil
	}
	if slices.Contains(planTypes, b.Type) && !slices.Contains(b.Labels, planLabel) {
		return false, nil
	}
	return true, nil
}

func (m *Map) GetTicket(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, ticket landsv1.TicketPath) {
	if !m.admit(w, r, land) {
		return
	}
	b, ok := m.getBead(w, r, "getTicket", land, ticket, ticketTypes...)
	if !ok {
		return
	}
	waiting, err := m.waiting(r, land, []beadRecord{*b})
	if err != nil {
		fail(w, r, "getTicket", err)
		return
	}
	writeJSON(w, http.StatusOK, toTicket(b, waiting[b.ID]))
}

func (m *Map) CreateTicket(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.CreateTicketParams) {
	if !m.writable(w, r, "createTicket", land) {
		return
	}
	var in landsv1.TicketCreate
	if !decodeBody(w, r, &in) || !textOK(w, "title", in.Title, 1, 500) || !priorityOK(w, in.Priority) {
		return
	}
	if !isTicketType(model.BeadType(in.Kind)) {
		problem(w, landsv1.InvalidRequest, "kind must be task, bug or feature")
		return
	}
	priority := 2
	if in.Priority != nil {
		priority = *in.Priority
	}
	body := map[string]any{
		"title":    in.Title,
		"type":     string(in.Kind),
		"kind":     string(model.KindIssue),
		"priority": priority,
		"labels":   []string{"project:" + land},
	}
	if in.Body != nil {
		if !textOK(w, "body", *in.Body, 0, 65536) {
			return
		}
		body["description"] = *in.Body
	}
	// Arms pass through; the daemon parses them, holds the register and
	// refuses them on a type that bears none yet.
	if in.Heraldry != nil {
		if !textOK(w, "heraldry", *in.Heraldry, 1, 200) {
			return
		}
		body["heraldry"] = *in.Heraldry
	}
	if params.IdempotencyKey != nil {
		body["idempotency_key"] = *params.IdempotencyKey
	}
	var b beadRecord
	if err := m.d.call(r, rCreateBead, nil, nil, body, &b); err != nil {
		fail(w, r, "createTicket", err)
		return
	}
	// A ticket just made has no blockers.
	writeJSON(w, http.StatusCreated, toTicket(&b, false))
}

func (m *Map) UpdateTicket(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, ticket landsv1.TicketPath) {
	if !m.writable(w, r, "updateTicket", land) {
		return
	}
	var in landsv1.TicketUpdate
	if !decodeBody(w, r, &in) || !priorityOK(w, in.Priority) {
		return
	}
	patch := map[string]any{}
	if in.Title != nil {
		if !textOK(w, "title", *in.Title, 1, 500) {
			return
		}
		patch["title"] = *in.Title
	}
	if in.Body != nil {
		if !textOK(w, "body", *in.Body, 0, 65536) {
			return
		}
		patch["description"] = *in.Body
	}
	if in.Priority != nil {
		patch["priority"] = *in.Priority
	}
	// "" removes the arms.
	if in.Heraldry != nil {
		if !textOK(w, "heraldry", *in.Heraldry, 0, 200) {
			return
		}
		patch["heraldry"] = *in.Heraldry
	}
	if len(patch) == 0 {
		problem(w, landsv1.InvalidRequest, "nothing to change")
		return
	}
	if _, ok := m.getBead(w, r, "updateTicket", land, ticket, ticketTypes...); !ok {
		return
	}
	if err := m.d.call(r, rUpdateBead, []string{ticket}, nil, patch, nil); err != nil {
		fail(w, r, "updateTicket", err)
		return
	}
	m.GetTicket(w, r, land, ticket)
}

// CommentTicket posts a comment. The daemon keeps no idempotency record for
// comments, so Idempotency-Key is accepted and not yet honoured.
func (m *Map) CommentTicket(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, ticket landsv1.TicketPath, _ landsv1.CommentTicketParams) {
	m.comment(w, r, "commentTicket", land, ticket, ticketTypes...)
}

func (m *Map) comment(w http.ResponseWriter, r *http.Request, op, land, id string, types ...model.BeadType) {
	if !m.writable(w, r, op, land) {
		return
	}
	var in landsv1.CommentCreate
	if !decodeBody(w, r, &in) || !textOK(w, "text", in.Text, 1, 65536) {
		return
	}
	if _, ok := m.getBead(w, r, op, land, id, types...); !ok {
		return
	}
	var c commentRecord
	if err := m.d.call(r, rAddComment, []string{id}, nil, map[string]any{"text": in.Text}, &c); err != nil {
		fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusCreated, toComment(id, &c))
}

func (m *Map) CloseTicket(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, ticket landsv1.TicketPath, _ landsv1.CloseTicketParams) {
	if !m.writable(w, r, "closeTicket", land) {
		return
	}
	var in landsv1.TicketClose
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Reason != nil && !textOK(w, "reason", *in.Reason, 0, 2000) {
		return
	}
	b, ok := m.getBead(w, r, "closeTicket", land, ticket, ticketTypes...)
	if !ok {
		return
	}
	// Closing a done ticket is a no-op, so a retry needs no key.
	if b.Status != model.StatusClosed {
		body := map[string]any{}
		if in.Reason != nil {
			body["reason"] = *in.Reason
		}
		if err := m.d.call(r, rCloseBead, []string{ticket}, nil, body, nil); err != nil {
			fail(w, r, "closeTicket", err)
			return
		}
	}
	m.GetTicket(w, r, land, ticket)
}

func (m *Map) ReopenTicket(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, ticket landsv1.TicketPath, _ landsv1.ReopenTicketParams) {
	if !m.writable(w, r, "reopenTicket", land) {
		return
	}
	b, ok := m.getBead(w, r, "reopenTicket", land, ticket, ticketTypes...)
	if !ok {
		return
	}
	if b.Status == model.StatusClosed {
		if err := m.d.call(r, rUpdateBead, []string{ticket}, nil, map[string]any{"status": string(model.StatusOpen)}, nil); err != nil {
			fail(w, r, "reopenTicket", err)
			return
		}
	}
	m.GetTicket(w, r, land, ticket)
}
