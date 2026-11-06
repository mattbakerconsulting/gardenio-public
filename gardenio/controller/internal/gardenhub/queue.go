package gardenhub

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The Queues panel (Lands workbench-glass spec WB-6, step 5.5, gd-HuJdsFabSb): the viewer's three attention queues, Inbox,
// Decisions and Escalations, as one Workbench kind (web/queue.js, /garden?queue=<tab>), opened from the Mailbox. Each tab
// is read by the viewer's own builders for its page (cmd/beads-viewer queue_garden.go reuses inbox.go, decisions.go and
// escalations.go), on the request's bearer, so the daemon cuts each answer as it cuts the page. The panel's writes are
// the viewer's POST routes (answer.js; pin and worthy, dismiss and reflag in queue.js), never a second implementation.

// Queue tabs, in the panel's order.
const (
	QueueInbox       = "inbox"
	QueueDecisions   = "decisions"
	QueueEscalations = "escalations"
)

// QueueTabs are the tabs GET /garden/queue/{kind} answers.
var QueueTabs = []string{QueueInbox, QueueDecisions, QueueEscalations}

// QueueReader reads one queue tab for the request's viewer: the viewer's own default ("mine": the decisions addressed to
// them, the escalations for the owner, their inbox), or everyone's when all is set. It says which rows need the viewer;
// the panel sorts them (sortQueue).
type QueueReader interface {
	Queue(ctx context.Context, kind string, all bool) (*QueueDoc, error)
}

const (
	queueTimeout = 10 * time.Second
	queueRowsMax = 200 // rows sent per tab: the pages' own caps (decisionsListLimit, escalationsListLimit)
	queueTextMax = 400 // runes of a row's prompt or context
)

// QueueOption is one answer a decision offers.
type QueueOption struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Label string `json:"label,omitempty"`
}

// QueueRef is a bead a row names (the asking agent, its task, its plan), titled when the daemon returned it.
type QueueRef struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// QueueRow is one row of a tab.
type QueueRow struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type,omitempty"`
	// Kind is what the row is: intake, forwarded, epic or agent in the inbox; decision; escalation.
	Kind     string `json:"kind"`
	Project  string `json:"project,omitempty"`
	Status   string `json:"status,omitempty"`
	Priority int    `json:"priority,omitempty"`
	// Text is the row's one paragraph: a decision's prompt, an escalation's context, an intake's description.
	Text string `json:"text,omitempty"`
	// Actor is who asked or acted: the requester, the operator a row is for.
	Actor   string        `json:"actor,omitempty"`
	Agent   *QueueRef     `json:"agent,omitempty"`
	Task    *QueueRef     `json:"task,omitempty"`
	Plan    *QueueRef     `json:"plan,omitempty"`
	Options []QueueOption `json:"options,omitempty"`
	// Operation, Sensitivity and Audience are an escalation's (owner, operator or desk; gd-bmOiteQi36). State is where it
	// stands (EscalationState): the panel offers Approve and Reject only while it is pending, and words any other state
	// (an approved one is still in_progress until its executor closes it, gd-yuOG6cX6VI).
	Operation   string `json:"operation,omitempty"`
	State       string `json:"state,omitempty"`
	Sensitivity string `json:"sensitivity,omitempty"`
	Audience    string `json:"audience,omitempty"`
	// Held names the agent lane holding a non-floor escalation (AgentLaneHolder): its answer box offers only
	// "Approve anyway" (gd-Mq0uDXHswK).
	Held string `json:"held,omitempty"`
	// Since is when the row started waiting (unix seconds): created for an ask, last activity for an inbox row.
	Since int64 `json:"since,omitempty"`
	// NeedsYou says the row waits on this viewer, and Why says so in words ("pinned", "forwarded to you", "asked of
	// you", "for the owner"); state is never colour alone.
	NeedsYou bool   `json:"needs_you,omitempty"`
	Why      string `json:"why,omitempty"`
	// Pinned and Worthy are the inbox's own flags: the viewer's pin and flag:inbox-worthy.
	Pinned bool   `json:"pinned,omitempty"`
	Worthy bool   `json:"worthy,omitempty"`
	MR     string `json:"mr,omitempty"` // a linked merge request's state
	MRURL  string `json:"mr_url,omitempty"`
}

// QueueDoc is one tab as the panel needs it.
type QueueDoc struct {
	Kind string `json:"kind"`
	// All says the rows are everyone's; Scoped says the viewer has a "mine" to go back to (an operator identity), so the
	// panel offers the toggle.
	All    bool       `json:"all"`
	Scoped bool       `json:"scoped"`
	Rows   []QueueRow `json:"rows"`
	// More counts rows past queueRowsMax, or past the page's own cap, that are not sent.
	More int `json:"more,omitempty"`
	// InHand is an escalation tab's folded line for the desk's and the operators' (owner view only).
	InHand string `json:"in_hand,omitempty"`
	// Note is an honest line about what could not be read (an unprovisioned viewer, a source that failed).
	Note string `json:"note,omitempty"`
	// Approve and Edit say which writes the panel offers: Approve/Reject (CanApproveEscalation), and dismiss, reflag,
	// pin and worthy (CanEdit). The routes check again.
	Approve bool `json:"approve,omitempty"`
	Edit    bool `json:"edit,omitempty"`
}

// queueView serves GET /garden/queue/{kind}[?all=1].
func (h *Handler) queueView(w http.ResponseWriter, r *http.Request) {
	if h.Queues == nil {
		http.Error(w, "queues not enabled", http.StatusServiceUnavailable)
		return
	}
	kind := r.PathValue("kind")
	if !isQueueTab(kind) {
		http.Error(w, "no such queue", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queueTimeout)
	defer cancel()
	doc, err := h.Queues.Queue(ctx, kind, queueAll(r))
	if err != nil || doc == nil {
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		return
	}
	doc.Kind = kind
	finishQueue(doc)
	doc.Approve = kind == QueueEscalations && h.can(r, h.CanApproveEscalation)
	doc.Edit = h.can(r, h.CanEdit)
	writeDoc(w, "kbeads", doc)
}

func isQueueTab(kind string) bool {
	for _, k := range QueueTabs {
		if k == kind {
			return true
		}
	}
	return false
}

// queueAll reads ?all as /decisions does: 1, true, yes, on or a bare ?all.
func queueAll(r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("all") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("all"))) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// finishQueue sorts the rows by attention, clips their text and caps them, so every reader sends the same shape.
func finishQueue(d *QueueDoc) {
	if d.Rows == nil {
		d.Rows = []QueueRow{}
	}
	for i := range d.Rows {
		d.Rows[i].Text = clip(d.Rows[i].Text, queueTextMax)
	}
	sortQueue(d.Rows)
	if len(d.Rows) > queueRowsMax {
		d.More += len(d.Rows) - queueRowsMax
		d.Rows = d.Rows[:queueRowsMax]
	}
}

// sortQueue is the coopmux sidebar's attention order (coop/crates/web SessionSidebar.tsx sortByAttention): what needs
// the viewer first, then the one that has waited longest; the id breaks a tie, so a reload never reshuffles.
func sortQueue(rows []QueueRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.NeedsYou != b.NeedsYou {
			return a.NeedsYou
		}
		if a.Since != b.Since {
			if a.Since == 0 || b.Since == 0 {
				return b.Since == 0 // an unknown age goes last
			}
			return a.Since < b.Since
		}
		return a.ID < b.ID
	})
}
