package main

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// escalationsListLimit caps the number of escalations pulled from the daemon
// for the /escalations page. Mirrors decisionsListLimit — a truncation banner
// surfaces the cap if the queue ever blows past it.
const escalationsListLimit = 200

// healthEscalationsTileLimit caps the daemon fetch for the health tile. Three
// rows render in "next 3 (oldest first)"; we ask for a few extra so the count
// reflects a real queue rather than the tile slice.
const healthEscalationsTileLimit = 50

// escalationRowView is one row on the /escalations list page.
type escalationRowView struct {
	ID            string
	Title         string
	OperationType string
	Sensitivity   string
	Status        string
	// State is where it stands (gardenhub.EscalationState): pending, approved,
	// done, failed or rejected. Only a pending one takes Approve or Reject; an
	// approved one is still in_progress until its executor closes it.
	State           string
	Project         string
	RequestingAgent string
	RequestingTask  string
	// SpawnRole flags this row as a privileged-role spawn request
	// (gd-hmzdQ5N6ix) so the queue lists it distinctly from a generic
	// operation escalation before the approver even opens the card.
	SpawnRole    string
	Context      string
	Ago          string
	UpdatedAtRFC string
	// Audience is who it is for (owner, operator or desk, gd-bmOiteQi36);
	// an InHand row (not the owner's) goes in the folded "In hand" list.
	Audience string
	InHand   bool
	// HeldBy names the holding lane (escalation-routing D9), e.g.
	// "coordinator laptop"; empty for an unstamped escalation, whose chip
	// then shows Audience as #756 did. LandFold marks an In-hand row held by
	// another land's owner.
	HeldBy   string
	LandFold bool
	// Agent is gardenhub.AgentLaneHolder: the agent lane holding a non-floor
	// escalation, whose /garden answer box offers only "Approve anyway"
	// (gd-Mq0uDXHswK).
	Agent string
	// Origin is the agent / task / plan strip (Slack thread gd-AREKDbHx70):
	// the requesting agent by NAME (requesting_agent holds a bead id) linked
	// to its bead, the task by title, the plan by title.
	Origin requestOrigin
	// ContextHTML is the FULL `context` field rendered as markdown for the
	// inline "details" disclosure; Context above stays the one-line snippet.
	// HasDetails is true when the snippet clipped the text.
	ContextHTML template.HTML
	HasDetails  bool
	// OperatorChipCompactHTML is the initials-only operator chip for the
	// dense escalations list row (ka-0000000013). Empty when the chip kind
	// has no compact treatment (scheduler / empty) — the row then omits the
	// operator marker entirely.
	OperatorChipCompactHTML template.HTML
	// ReviewURL is the bridge-SPA deep link for approve/reject. Empty when
	// ESCALATION_REVIEW_URL is unset (the link-out is then suppressed in the
	// template). The auth boundary lives at the bridge SPA — the viewer never
	// mutates escalations.
	ReviewURL string
	// created is the bead's, for the Queues panel's attention order
	// (queue_garden.go).
	created time.Time
}

// healthEscalationRowView is one row in the escalations tile's "next 3" list.
type healthEscalationRowView struct {
	ID            string
	Title         string
	OperationType string
	Age           string
}

// healthEscalationsTileView is the template view model for the escalations tile.
type healthEscalationsTileView struct {
	Pending int // the owner's escalations only (gd-bmOiteQi36)
	InHand  int // the desk's and the operators'
	Next    []healthEscalationRowView
	Error   string
	AsOf    string
}

// handleEscalations renders /escalations — a list of open + in_progress
// escalations sorted by created_at descending (newest first). Read-only:
// approve/reject is gated to the bridge SPA at ESCALATION_REVIEW_URL.
//
// Optional ?project=<name> narrows by project label. Status defaults to
// "open + in_progress" (the kbeads server default); ?status=open or
// ?status=in_progress narrows further.
func (s *Server) handleEscalations(w http.ResponseWriter, r *http.Request) {
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))

	data := map[string]any{
		"ProjectFilter":    projectFilter,
		"StatusFilter":     statusFilter,
		"Escalations":      []escalationRowView{},
		"ReviewURLEnabled": s.escalationReviewURL != "",
		"GardenHref":       s.gardenQueuesBanner(r, gardenQueueTarget(gardenhub.QueueEscalations)),
	}

	rows, fetched, err := s.escalationRows(r.Context(), statusFilter, projectFilter)
	if err != nil {
		s.logger.Error("listing escalations", "error", err)
		data["Error"] = "Failed to load escalations."
		s.render(w, r, "escalations.html", data)
		return
	}

	// Only the owner's escalations are listed in full and counted; the desk's
	// and the operators' fold into one "In hand" line (gd-bmOiteQi36).
	var owner, inHand []escalationRowView
	var tally beadsapi.InHand
	for _, row := range rows {
		if row.InHand {
			inHand = append(inHand, row)
			if row.LandFold {
				tally.AddLand()
			} else {
				tally.Add(row.Audience)
			}
			continue
		}
		owner = append(owner, row)
	}
	data["Escalations"] = owner
	data["InHand"] = inHand
	data["InHandLine"] = tally.Line()
	data["Truncated"] = fetched >= escalationsListLimit
	data["Limit"] = escalationsListLimit
	s.render(w, r, "escalations.html", data)
}

// escalationRows builds the /escalations rows, newest first: the open and
// in-progress escalations (statusFilter narrows), in projectFilter's project
// when it is set, each with its origin strip, details and audience. fetched is
// how many the daemon returned, for the truncation banner. The Queues panel in
// /garden reads the same rows (queue_garden.go).
func (s *Server) escalationRows(ctx context.Context, statusFilter, projectFilter string) (rows []escalationRowView, fetched int, err error) {
	// kbeads /v1/escalations defaults to open + in_progress when status is
	// empty. We pass statusFilter through so ?status=open narrows server-side.
	escalations, err := s.daemon.ListEscalations(ctx, statusFilter, escalationsListLimit)
	if err != nil {
		return nil, 0, err
	}

	// Populate operator homepage slugs on the underlying issue beads so each
	// row's compact operator chip can emit /u/{slug} links (mirrors /active and
	// the other list surfaces).
	issues := make([]*beadsapi.BeadDetail, 0, len(escalations))
	for _, ed := range escalations {
		if ed.Issue != nil {
			issues = append(issues, ed.Issue)
		}
	}
	s.populateOperatedBySlugs(ctx, issues)
	viewerEmail := oidcauth.EmailFromContext(ctx)
	lanes := s.viewerLaneRefs(ctx, viewerEmail)
	origins := s.resolveRequestOrigins(ctx, issues)

	now := time.Now()
	rows = make([]escalationRowView, 0, len(escalations))
	for _, ed := range escalations {
		// Daemon shape: {"escalation": <typed-fields envelope, no id>, "issue": <bead>}.
		// The bead (id/title/status/labels/fields/timestamps) lives in Issue.
		if ed.Issue == nil {
			continue
		}
		b := ed.Issue
		project := projectFromLabels(b.Labels)
		if projectFilter != "" && project != projectFilter {
			continue
		}
		escChip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, s.basePath, viewerEmail)
		row := escalationRowView{
			ID:                      b.ID,
			Title:                   b.Title,
			OperationType:           b.Fields["operation_type"],
			Sensitivity:             b.Fields["sensitivity"],
			Status:                  b.Status,
			State:                   gardenhub.EscalationState(b),
			Project:                 project,
			RequestingAgent:         b.Fields["requesting_agent"],
			RequestingTask:          b.Fields["requesting_task"],
			SpawnRole:               b.Fields["spawn_role"],
			Context:                 snippet(b.Fields["context"], 240),
			Ago:                     fmtTimeAgo(b.CreatedAt, now),
			UpdatedAtRFC:            rfcTime(b.UpdatedAt),
			ReviewURL:               s.buildEscalationReviewURL(b.ID),
			OperatorChipCompactHTML: renderOperatorChipCompact(escChip),
			Origin:                  origins[b.ID],
			Audience:                beadsapi.EscalationAudience(b.Fields),
			created:                 b.CreatedAt,
		}
		owner, landFold := escalationOwnerRow(b.Fields, lanes)
		row.InHand, row.LandFold = !owner, landFold
		row.HeldBy = beadsapi.LaneLabel(beadsapi.HeldByFromFields(b.Fields))
		row.Agent = gardenhub.AgentLaneHolder(b.Fields)
		if full := strings.TrimSpace(b.Fields["context"]); full != "" && len(full) > len(row.Context) {
			row.ContextHTML = fieldMarkdownHTML(s.md, full, s.basePath)
			row.HasDetails = row.ContextHTML != ""
		}
		rows = append(rows, row)
	}

	// Sort newest-first by underlying created_at by leveraging the daemon's
	// FIFO-ish ordering inverted. The daemon currently returns in create-asc;
	// we want create-desc for the list view (matching /decisions).
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].UpdatedAtRFC > rows[j].UpdatedAtRFC
	})
	return rows, len(escalations), nil
}

// handleHealthTileEscalations renders the escalations tile fragment for hx-get
// refresh on /health. Counts open + in_progress and renders the three oldest.
func (s *Server) handleHealthTileEscalations(w http.ResponseWriter, r *http.Request) {
	view := s.buildEscalationsTileView(r.Context(), time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_escalations.html", "health_tile_escalations", map[string]any{
		"Escalations": view,
	})
}

// buildEscalationsTileView fetches the escalation queue and shapes the three
// oldest into the tile view model. Failure surfaces a clean error pill rather
// than an empty state.
func (s *Server) buildEscalationsTileView(ctx context.Context, now time.Time) healthEscalationsTileView {
	v := healthEscalationsTileView{AsOf: now.UTC().Format(time.RFC3339)}
	escalations, err := s.daemon.ListEscalations(ctx, "", healthEscalationsTileLimit)
	if err != nil {
		v.Error = "escalations snapshot unavailable"
		return v
	}
	// Pending = the owner's escalations of the full set returned (open +
	// in_progress per server default); the desk's and the operators' are in
	// hand and counted apart (gd-bmOiteQi36). Next is drawn from the owner's.
	escalations, v.InHand = ownerEscalations(escalations, s.viewerLaneRefs(ctx, oidcauth.EmailFromContext(ctx)))
	v.Pending = len(escalations)

	// "Next 3 oldest first" — escalation age IS the urgency signal. Sort by
	// created_at asc and take the first three. A nil escalation slot is rare
	// (server bug) but we tolerate it silently.
	rows := make([]healthEscalationRowView, 0, len(escalations))
	for _, ed := range escalations {
		// Bead data lives in Issue; the Escalation envelope is a typed-fields
		// map with no id/title/timestamps (see daemon http_escalations.go).
		if ed.Issue == nil {
			continue
		}
		b := ed.Issue
		rows = append(rows, healthEscalationRowView{
			ID:            b.ID,
			Title:         b.Title,
			OperationType: b.Fields["operation_type"],
			Age:           fmtTimeAgo(b.CreatedAt, now),
		})
	}
	// We don't have created_at on the row view, so sort the underlying
	// EscalationDetail slice by CreatedAt asc and re-zip. Keep this scoped to
	// the rows we just built so a future schema change doesn't silently bypass
	// the sort.
	type ageSortable struct {
		row       healthEscalationRowView
		createdAt time.Time
	}
	sortable := make([]ageSortable, 0, len(escalations))
	idx := 0
	for _, ed := range escalations {
		if ed.Issue == nil {
			continue
		}
		sortable = append(sortable, ageSortable{row: rows[idx], createdAt: ed.Issue.CreatedAt})
		idx++
	}
	sort.SliceStable(sortable, func(i, j int) bool {
		// Zero CreatedAt sorts last so it doesn't claim the "oldest" slot on a
		// daemon that omits the field.
		if sortable[i].createdAt.IsZero() {
			return false
		}
		if sortable[j].createdAt.IsZero() {
			return true
		}
		return sortable[i].createdAt.Before(sortable[j].createdAt)
	})
	if len(sortable) > 3 {
		sortable = sortable[:3]
	}
	v.Next = make([]healthEscalationRowView, len(sortable))
	for i := range sortable {
		v.Next[i] = sortable[i].row
	}
	return v
}

// buildEscalationReviewURL returns the review deep link for the given
// escalation. When ESCALATION_REVIEW_URL is set it points at the bridge-SPA
// approve/reject page (honoring `?bead_id=` to scroll-to / expand). When the
// env var is unset it falls back to the in-viewer bead page (`{basePath}/{id}`),
// which can now approve/reject the escalation natively — so every escalation
// surface links somewhere useful by default.
func (s *Server) buildEscalationReviewURL(escalationID string) string {
	if escalationID == "" {
		return ""
	}
	if s.escalationReviewURL == "" {
		// Fall back to the in-viewer bead page, which supports in-place
		// approve/reject (ka-0000006884).
		return s.basePath + "/" + url.PathEscape(escalationID)
	}
	// Preserve any existing query/fragment on the base URL.
	sep := "?"
	if strings.Contains(s.escalationReviewURL, "?") {
		sep = "&"
	}
	return s.escalationReviewURL + sep + "bead_id=" + url.QueryEscape(escalationID)
}

// ownerEscalations splits an escalation list into the viewer's own (in order)
// and a count of the rest, which the desk, the operators or another land's
// owner have (gd-bmOiteQi36, escalation-routing D9). lanes is the viewer's
// (viewerLaneRefs); nil keeps #756's owner-audience split.
func ownerEscalations(all []beadsapi.EscalationDetail, lanes []string) ([]beadsapi.EscalationDetail, int) {
	owner := make([]beadsapi.EscalationDetail, 0, len(all))
	inHand := 0
	for _, ed := range all {
		if ed.Issue != nil {
			if mine, _ := escalationOwnerRow(ed.Issue.Fields, lanes); !mine {
				inHand++
				continue
			}
		}
		owner = append(owner, ed)
	}
	return owner, inHand
}
