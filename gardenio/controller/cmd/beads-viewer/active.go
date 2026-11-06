package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// activeListLimit caps the number of in-progress beads pulled from the daemon.
// The Active page is a "what's happening right now" overview — at any
// realistic load the in-progress set is well under 500 beads. Cap matches the
// iOS InProgressTab sensibility (no pagination, just truncate).
const activeListLimit = 500

// activeDescPreviewLimit is the rune-budget for the per-card description
// preview. Wider than the project page (120) because the active card is the
// only signal an operator gets that something has actually moved — so we want
// a sentence or two of context, not just a title.
const activeDescPreviewLimit = 140

// activeBeadView is the row view-model used by saved views (/views/{id}) —
// /active itself now renders projectBeadView via the project_row partial. The
// Row field carries the pre-computed action-view for the search-results-style
// table on view_show.html.
type activeBeadView struct {
	ID                string
	Title             string
	Type              string
	Priority          int
	Assignee          string
	Project           string
	UpdatedAt         time.Time
	Ago               string
	OperatedBy        string
	OperatedByDisplay string
	CreatedBy         string
	Row               beadActionsView
}

// activeProjectGroup is one project section on the /active page. Beads carry
// the same projectBeadView shape used by /p/<project>, so the page can render
// each group with the existing project_row partial.
type activeProjectGroup struct {
	Project string
	Beads   []projectBeadView
}

// activeSummary is the top-of-page counts bar.
type activeSummary struct {
	Active    int // non-agent in-progress beads
	Agents    int // type=agent in-progress beads
	Decisions int // type=decision open beads
	Queued    int // open, non-agent beads (work waiting to be picked up)
	// Blocked counts in-progress work items stalled on an open decision —
	// the ⏸ rows. These are the beads where an operator answer unblocks
	// forward motion, so they anchor the "Needs attention" section.
	Blocked int
	// Reports counts in-progress work items carrying an attached report
	// awaiting review — the 📄 rows. A report is a hand-off asking for a
	// human read, so it belongs in "Needs attention" alongside blocked work.
	Reports int
}

// handleActive renders /active — a project-grouped list of all in-progress
// beads with a 4-count summary bar (Agents · Active · Decisions · Queued)
// and decision banner. Mirrors iOS InProgressTab.
//
// Per-project groups sort by UpdatedAt desc with Priority asc + ID asc as
// tiebreakers — recency first so the page feels live.
//
// Optional ?project=<name> filters to one project; default = all projects.
// Scheduled-task beads (auto-spawned by the cron scheduler each tick) are
// hidden by default — pass ?include_scheduled=1 to surface them
// (ka-0000006652).
func (s *Server) handleActive(w http.ResponseWriter, r *http.Request) {
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	includeScheduled := isTruthy(r.URL.Query().Get("include_scheduled"))
	operatedByRaw := strings.TrimSpace(r.URL.Query().Get("operated_by"))

	email := oidcauth.EmailFromContext(r.Context())
	operatedBy, isMe := resolveOperatedByParam(operatedByRaw, email)

	// R3 §1: /active is the default-on surface — when the URL key is absent
	// and the viewer is authenticated, scope the daemon query to their
	// operator id. URL-explicit values (incl. the empty "?operated_by=" that
	// means "show all") win. MyWorkSummaryMode below is intentionally
	// independent: it stays driven by the literal `?operated_by=me` alias.
	effectiveOperatedBy := resolveDefaultOperatedBy(r, operatedBy)

	urlHasOperatedBy := r.URL.Query().Has("operated_by")
	operatedByIsUnowned := operatedByRaw == "null" || strings.EqualFold(operatedByRaw, "(none)")
	chipExtra := r.URL.Query()
	chipExtra.Del("operated_by")

	data := map[string]any{
		"ProjectFilter":       projectFilter,
		"IncludeScheduled":    includeScheduled,
		"OperatedByFilter":    operatedByRaw,
		"OperatedByMe":        isMe,
		"OperatedByActive":    operatedByRaw != "",
		"OperatedByURLHasKey": urlHasOperatedBy,
		"EffectiveOperatedBy": effectiveOperatedBy,
		"OperatedByIsUnowned": operatedByIsUnowned,
		"OperatedByDisplay":   "",
		"ChipBasePath":        s.basePath + "/active",
		"ChipExtraQuery":      chipExtra.Encode(),
		"MyWorkSummaryMode":   isMe, // R8 §5.4: scopes the active view to the viewer
		"ViewerEmail":         email,
		"InboxCount":          s.inboxCountForViewer(r.Context(), email),
		// /active is a default-on surface per V-R2 §5.3 rule 3. Banner gates
		// (OIDC + non-Matt + non-dismissed + V-R4 sunset flag/backstop) are
		// computed in shouldRenderMigrationBanner.
		"ShowMigrationBanner": s.shouldRenderMigrationBanner(r),
		"Summary":             activeSummary{},
		"Groups":              []activeProjectGroup{},
		"Attention":           []projectBeadView{},
		"HasDecisions":        false,
		// R5: /active is moving into the garden's project panel (garden_retire.go).
		"GardenHref": s.gardenBanner(r, retireActivity, gardenActivityTarget),
	}

	// Active rows render projectViewFromBead → row_badges.html, which reads
	// HasReport / BlockedOnDecision. Opt the projections in so the badges
	// stay populated; default daemon behavior is to return placeholders.
	q := beadsapi.ListBeadsQuery{
		Statuses:              []string{"in_progress"},
		Limit:                 activeListLimit,
		ExcludeScheduled:      !includeScheduled,
		WithHasReport:         true,
		WithBlockedOnDecision: true,
	}
	if projectFilter != "" {
		q.Labels = []string{"project:" + projectFilter}
	}
	// Daemon migration 024 dropped the operated_by TEXT column and its
	// IS NULL semantics; operatedByIsUnowned now has no daemon-side
	// successor and the unowned chip filters to no-op (see ka-0000003586).
	if !operatedByIsUnowned && effectiveOperatedBy != "" {
		q.OperatedByAliases = []string{effectiveOperatedBy}
	}
	res, err := s.daemon.ListBeadsFiltered(r.Context(), q)
	if err != nil {
		s.logger.Error("listing active beads", "error", err)
		data["Error"] = "Failed to load active beads."
		s.render(w, r, "active.html", data)
		return
	}

	// Decisions count is best-effort: a daemon error here only hides the
	// banner, the rest of the page still renders.
	decisions, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Limit:    activeListLimit,
	})
	if err != nil {
		s.logger.Warn("listing decision beads", "error", err)
		decisions = &beadsapi.ListBeadsResult{}
	}

	// Queued count is best-effort: open beads that are not agents (i.e. work
	// waiting to be picked up). Daemon has no ExcludeTypes filter, so we
	// post-filter for Type != "agent" client-side. Failure → zero, page
	// still renders.
	queuedCount := s.countQueuedBeads(r, projectFilter, includeScheduled)

	// Sort all in-progress beads by recency first so per-project groups
	// surface the freshest activity at the top. Priority and ID act as
	// stable tiebreakers within a recency bucket.
	beads := make([]*beadsapi.BeadDetail, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		beads = append(beads, b)
	}
	sort.SliceStable(beads, func(i, j int) bool {
		a, b := beads[i], beads[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.ID < b.ID
	})

	// Per-user-homepage M2 §6 (ka-0000007787): populate slugs before chip
	// render so each /active row links to /u/{slug}.
	s.populateOperatedBySlugs(r.Context(), beads)

	groups := map[string][]projectBeadView{}
	attention := make([]projectBeadView, 0, len(beads))
	summary := activeSummary{Decisions: len(decisions.Beads), Queued: queuedCount}

	for _, b := range beads {
		project := projectFromLabels(b.Labels)
		if project == "" {
			project = "untagged"
		}
		if b.Type == "agent" {
			summary.Agents++
		} else {
			summary.Active++
		}
		view := projectViewFromBead(b, email, project, s.basePath)
		view.DescPreview = snippet(b.Description, activeDescPreviewLimit)
		groups[project] = append(groups[project], view)

		// "Needs attention" (ka-0000002647): a work item is actionable when
		// it is blocked on an open decision (operator answer unblocks it) or
		// carries a report awaiting review. Agents and decisions are process
		// rows, not work items, so they never enter this cross-project pull.
		if b.Type == "agent" || b.Type == "decision" {
			continue
		}
		blocked := b.BlockedOnDecision
		if blocked {
			summary.Blocked++
		}
		if b.HasReport {
			summary.Reports++
		}
		if blocked || b.HasReport {
			attention = append(attention, view)
		}
	}

	// Attention rows lead with the most urgent: lowest priority number, then
	// freshest activity, then id for stability. This is the one section on
	// /active sorted by importance rather than pure recency.
	sort.SliceStable(attention, func(i, j int) bool {
		a, b := attention[i], attention[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.UpdatedAtRFC != b.UpdatedAtRFC {
			return a.UpdatedAtRFC > b.UpdatedAtRFC
		}
		return a.ID < b.ID
	})

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]activeProjectGroup, 0, len(names))
	for _, name := range names {
		out = append(out, activeProjectGroup{Project: name, Beads: groups[name]})
	}

	data["Summary"] = summary
	data["Groups"] = out
	data["Attention"] = attention
	data["HasDecisions"] = summary.Decisions > 0
	data["Truncated"] = res.Total > len(res.Beads)
	data["Limit"] = activeListLimit
	r = s.prewarmAgentLinksFromBeads(r, beads)
	s.render(w, r, "active.html", data)
}

// countQueuedBeads returns the number of open, non-agent beads matching the
// active page's project / scheduled-bead filters. Best-effort: a daemon error
// is logged and zero is returned so the rest of the page still renders.
//
// We ask the daemon to drop type=agent rows server-side and request Limit=1,
// then read the count from `total` in the response. The previous
// pull-500-rows-and-client-filter pattern fetched ~740 KB per poll on
// gardenio-e2e (status=open matches ~240k rows) and forced the daemon to run
// the per-row projections over all of them; with this filter the response
// carries one bead row, the count comes from the COUNT(*) OVER() that the
// daemon already computes, and the projection cost no longer scales with
// the queued-count tile's poll cadence.
func (s *Server) countQueuedBeads(r *http.Request, projectFilter string, includeScheduled bool) int {
	q := beadsapi.ListBeadsQuery{
		Statuses:         []string{"open"},
		ExcludeTypes:     []string{"agent"},
		CountOnly:        true,
		ExcludeScheduled: !includeScheduled,
	}
	if projectFilter != "" {
		q.Labels = []string{"project:" + projectFilter}
	}
	res, err := s.daemon.ListBeadsFiltered(r.Context(), q)
	if err != nil {
		s.logger.Warn("listing queued beads", "error", err)
		return 0
	}
	if res == nil {
		return 0
	}
	return res.Total
}

// isTruthy reports whether a query-string value should be treated as on.
// Accepts "1", "true", "yes" (case-insensitive); empty strings are false so
// the default-off behavior is preserved.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// fmtTimeAgo formats a relative duration like the iOS "ago" formatter:
// seconds <60s, minutes <60m, hours <24h, days otherwise. Returns "" when
// the timestamp is zero (e.g. missing on the bead).
func fmtTimeAgo(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(now.Sub(t), 0)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
