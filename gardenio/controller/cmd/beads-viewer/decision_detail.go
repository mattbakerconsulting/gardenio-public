package main

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// decisionContextRelatedLimit caps how many related MRs / mentioned beads the
// context block renders. The lineage walk is best-effort and bounded so a
// decision with a large blast radius can't blow the page up.
const decisionContextRelatedLimit = 12

// decisionRelatedMR is one merge-request link surfaced in the context block.
type decisionRelatedMR struct {
	URL   string
	Title string
	State string
}

// decisionLineageLink is one bead the context block links to (parent task,
// parent epic, owning plan). Title is best-effort; a lookup miss leaves it
// empty and the template falls back to the bare id.
type decisionLineageLink struct {
	ID    string
	Title string
}

// decisionReflagInfo carries the reflag history stamped by `gb decision reflag`
// (reflag_count / reflagged_at / reflagged_by / reflag_reason) so the
// interaction page can show "reflagged 2× · 3h ago" and the operator can judge
// how hard the agent (or a gardener sweep) has been pushing on this decision.
type decisionReflagInfo struct {
	Count     int
	At        string // humanized age ("3h")
	By        string
	Reason    string
	Escalated bool // carries the "escalated" label (P0/P1 surfaced to Slack)
}

// decisionDetailView is the full view model for the /decisions/{id}
// interaction page. It embeds the same *decisionView the type_decision partial
// consumes (so resolve/dismiss/other reuse the shared card + modal verbatim)
// and layers the richer context block + reflag affordance on top.
type decisionDetailView struct {
	// Decision is the shared card view model (prompt, options, resolved banner,
	// CanResolve gating). Rendered via the type_decision partial.
	Decision *decisionView

	// Bead is the raw decision bead, for the header + metadata the card omits.
	Bead *beadsapi.BeadDetail

	// Title / Project / Priority / status chrome for the page header.
	Title      string
	Project    string
	Priority   int
	Status     string
	Ago        string // created-at age
	UpdatedAgo string // last-activity age
	Resolved   bool

	// Context block ---------------------------------------------------------
	// Operator is the resolved operator chip (who the decision is addressed to).
	Operator template.HTML
	// RequestedBy / RequestingAgent identify who filed the decision.
	RequestedBy       string
	RequestingAgent   string
	RequestingAgentID string // requesting_agent_bead_id, for a /{id} deep link
	// Lineage links: the task the decision blocks, its epic, the owning plan.
	RequestingTask *decisionLineageLink
	RequestingPlan *decisionLineageLink
	Blocks         []decisionLineageLink // beads this decision blocks (reverse deps)
	// RelatedMRs are merge requests reachable from the requesting agent's
	// produces-lineage — "what the agent shipped / touched" context.
	RelatedMRs []decisionRelatedMR
	// SlackURL deep-links into the originating Slack thread when the decision
	// (or its requesting agent) carries slack_thread_channel/ts.
	SlackURL string

	// Reflag affordance -----------------------------------------------------
	Reflag decisionReflagInfo
	// CanReflag gates the reflag button: an authenticated viewer on an open
	// decision. A resolved decision cannot be reflagged (matches the CLI, which
	// refuses reflag on a closed bead).
	CanReflag bool
	// ReflagError renders inline when a reflag round-trip fails.
	ReflagError string
}

// handleDecisionDetail renders GET /decisions/{id} — the rich per-decision
// interaction page. It reuses the shared type_decision card (resolve / dismiss
// / other) and adds a context block (who filed it, what they tried, lineage,
// related MRs, Slack thread) plus a reflag affordance.
//
// A missing bead 404s; a non-decision bead 302-redirects to the generic /{id}
// bead-show page (the correct surface for every other type) rather than
// rendering a decision shell around it.
func (s *Server) handleDecisionDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Decision not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting decision bead", "id", id, "error", err)
		http.Error(w, "Failed to load decision", http.StatusInternalServerError)
		return
	}
	if bead.Type != "decision" {
		// Not a decision — send the viewer to the generic bead page rather than
		// rendering a decision shell around a non-decision bead.
		http.Redirect(w, r, s.basePath+"/"+id, http.StatusFound)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	view := s.buildDecisionDetailView(r.Context(), bead, email, "")

	data := map[string]any{
		"DecisionDetail": view,
		"Decision":       view.Decision, // type_decision partial reads .Decision
		"Bead":           bead,          // type_decision partial reads .Bead.Title
		"AuthorEmail":    email,
		"ViewerEmail":    email,
		"Title":          bead.Title,
		"GardenHref":     s.gardenQueuesBanner(r, gardenQueueTarget(gardenhub.QueueDecisions)),
	}
	r = s.prewarmAgentLinksFromBeads(r, []*beadsapi.BeadDetail{bead})
	s.render(w, r, "decision_detail.html", data)
}

// buildDecisionDetailView assembles the interaction-page view model. Every
// sub-fetch (lineage, MRs, reverse deps) is best-effort so the page always
// renders — a daemon hiccup degrades the context block, never the whole page.
// reflagErr, when non-empty, surfaces inline on the reflag control after a
// failed reflag round-trip.
func (s *Server) buildDecisionDetailView(ctx context.Context, bead *beadsapi.BeadDetail, email, reflagErr string) *decisionDetailView {
	now := time.Now()

	dv := buildDecisionView(bead, s.decisionMarkdown)
	if dv != nil {
		dv.CanResolve = !dv.Resolved && email != ""
		dv.Evidence = s.decisionEvidenceOf(ctx, bead)
	}

	s.populateOperatedBySlug(ctx, bead)
	viewerEmail := email
	chip := resolveOperatorChip(bead.OperatedBy, bead.OperatedByDisplay, bead.OperatedByEmail, bead.OperatedBySlug, bead.CreatedBy, s.basePath, viewerEmail)

	v := &decisionDetailView{
		Decision:          dv,
		Bead:              bead,
		Title:             bead.Title,
		Project:           projectFromLabels(bead.Labels),
		Priority:          bead.Priority,
		Status:            bead.Status,
		Ago:               fmtTimeAgo(bead.CreatedAt, now),
		UpdatedAgo:        fmtTimeAgo(bead.UpdatedAt, now),
		Resolved:          dv != nil && dv.Resolved,
		Operator:          renderOperatorChip(chip),
		RequestedBy:       bead.Fields["requested_by"],
		RequestingAgent:   bead.Fields["requesting_agent"],
		RequestingAgentID: strings.TrimSpace(bead.Fields["requesting_agent_bead_id"]),
	}
	if v.RequestedBy == "" {
		v.RequestedBy = bead.CreatedBy
	}

	// Reflag history + affordance.
	v.Reflag = buildDecisionReflagInfo(bead, now)
	v.CanReflag = !v.Resolved && email != ""
	v.ReflagError = reflagErr

	// Lineage: the task the decision blocks (requesting_task) and the owning
	// plan (requesting_plan), both stamped by `gb decision create`.
	if task := strings.TrimSpace(bead.Fields["requesting_task"]); task != "" {
		v.RequestingTask = s.resolveLineageLink(ctx, task)
	}
	if plan := strings.TrimSpace(bead.Fields["requesting_plan"]); plan != "" {
		v.RequestingPlan = s.resolveLineageLink(ctx, plan)
	}

	// Reverse deps: beads that this decision blocks (a decision→work blocks edge
	// points task→decision, so the decision is on the DependsOnID side; the
	// beads blocked by it are its reverse deps of type blocks).
	if revDeps, err := s.daemon.GetReverseDependencies(ctx, bead.ID); err == nil {
		for _, d := range revDeps {
			if d.Type != "blocks" || d.BeadID == "" {
				continue
			}
			// Skip the requesting task we already surface above to avoid a
			// duplicate row.
			if v.RequestingTask != nil && d.BeadID == v.RequestingTask.ID {
				continue
			}
			if link := s.resolveLineageLink(ctx, d.BeadID); link != nil {
				v.Blocks = append(v.Blocks, *link)
			}
			if len(v.Blocks) >= decisionContextRelatedLimit {
				break
			}
		}
	}

	// Related MRs: walk the requesting agent's produces-lineage. Best-effort and
	// bounded — a decision with no agent bead simply shows no MRs.
	if v.RequestingAgentID != "" {
		if mrs, err := s.daemon.MRBeadsForAgent(ctx, v.RequestingAgentID); err == nil {
			for _, mr := range mrs {
				if mr == nil {
					continue
				}
				url := strings.TrimSpace(mr.Fields["mr_url"])
				if url == "" {
					continue
				}
				// MR beads carry state under `mr_state`, with an older/foreign
				// alias `state` — mirror decorateRecentRunsWithMRState's fallback
				// (agents.go) so the chip isn't blank on beads that only set `state`.
				state := strings.TrimSpace(mr.Fields["mr_state"])
				if state == "" {
					state = strings.TrimSpace(mr.Fields["state"])
				}
				v.RelatedMRs = append(v.RelatedMRs, decisionRelatedMR{
					URL:   url,
					Title: mr.Title,
					State: state,
				})
				if len(v.RelatedMRs) >= decisionContextRelatedLimit {
					break
				}
			}
		}
	}

	// Slack thread deep-link: the decision itself, or its requesting agent bead.
	v.SlackURL = s.slackThreadURL(bead)
	if v.SlackURL == "" && v.RequestingAgentID != "" {
		if agentBead, err := s.daemon.GetBead(ctx, v.RequestingAgentID); err == nil && agentBead != nil {
			v.SlackURL = s.slackThreadURL(agentBead)
		}
	}

	return v
}

// buildDecisionReflagInfo reads the reflag bookkeeping fields stamped by
// `gb decision reflag` / `gb escalation reflag` (runReflag in cmd/gb/decision.go)
// into a display-ready struct.
func buildDecisionReflagInfo(bead *beadsapi.BeadDetail, now time.Time) decisionReflagInfo {
	info := decisionReflagInfo{
		By:     bead.Fields["reflagged_by"],
		Reason: bead.Fields["reflag_reason"],
	}
	if v := strings.TrimSpace(bead.Fields["reflag_count"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			info.Count = n
		}
	}
	if at := strings.TrimSpace(bead.Fields["reflagged_at"]); at != "" {
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			info.At = fmtTimeAgo(t, now)
		}
	}
	info.Escalated = hasLabel(bead, "escalated")
	return info
}

// resolveLineageLink hydrates a bead id into a display link (id + title + type),
// best-effort: a lookup miss returns a link carrying just the id so the template
// still renders a usable /{id} anchor.
func (s *Server) resolveLineageLink(ctx context.Context, id string) *decisionLineageLink {
	link := &decisionLineageLink{ID: id}
	if b, err := s.daemon.GetBead(ctx, id); err == nil && b != nil {
		link.Title = b.Title
	}
	return link
}
