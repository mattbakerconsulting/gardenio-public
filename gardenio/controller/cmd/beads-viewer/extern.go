package main

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// externListLimit caps the number of externally-sourced beads (MRs + JIRA)
// pulled per source. The open MR set alone runs a few hundred; a generous cap
// keeps the page complete while a truncation banner surfaces overflow.
const externListLimit = 500

// externSourceMR / externSourceJIRA are the two ?source= filter values the
// /extern page understands. Empty means "both".
const (
	externSourceMR   = "mr"
	externSourceJIRA = "jira"
)

// externSort* are the ?sort= values. Default is newest-activity-first.
const (
	externSortUpdated = "updated" // newest updated_at first (default)
	externSortOldest  = "oldest"  // oldest updated_at first
	externSortTitle   = "title"   // case-insensitive by title
)

// externGroup* are the ?group= values. Empty means a single flat list.
const (
	externGroupNone    = ""
	externGroupProject = "project"
	externGroupState   = "state"  // MR state / JIRA status
	externGroupSource  = "source" // merge-request vs jira
)

// externRowView is one row on the /extern page. It is the unified projection
// of either a type=merge-request bead or a source:jira bead onto a single
// nice-link + status-pill shape. Source disambiguates the two so the template
// can pick the right pills, glyph, and external-link label.
type externRowView struct {
	// Source is "mr" or "jira" — drives which status pills render.
	Source string

	// BeadID is the gardenio bead backing this external object. Always links
	// back into the viewer at /{BeadID}.
	BeadID string
	// Title is the human title (MR title or JIRA summary), falling back to the
	// bead title.
	Title string
	// Project is the boat project label (project:<name>) when present.
	Project string
	// Status is the bead's own lifecycle status (open/closed/in_progress) —
	// surfaced as a secondary chip so an operator can see the gardenio-side
	// state alongside the external state.
	Status string
	// Ago is a humanized "updated N ago" string.
	Ago string
	// UpdatedAtRFC is the RFC3339 updated_at, used as the stable sort key and
	// for the row's data-last-activity-ts attribute.
	UpdatedAtRFC string

	// ExternalURL deep-links to the external object (GitLab MR / JIRA issue).
	ExternalURL string
	// ExternalLabel is the short external identifier rendered as the primary
	// link text: "!4459" for an MR, "PE-7001" for a JIRA issue.
	ExternalLabel string

	// --- MR-only fields (Source == "mr") ---
	MRState        string // opened | merged | closed | locked
	PipelineStatus string // success | running | failed | …
	PipelineURL    string
	TargetBranch   string
	Author         string

	// --- JIRA-only fields (Source == "jira") ---
	JiraStatus   string // e.g. "In Progress", "Done"
	JiraType     string // Story | Bug | Epic | Task | …
	JiraAssignee string

	// LinkedAgent / LinkedTask carry the producing agent / task bead id when
	// resolvable (MRs are linked via the produces edge; left empty otherwise).
	LinkedTask string

	// SearchText is the precomputed, lowercased haystack the client-side search
	// box substring-matches against (title + external id + bead id + project +
	// author/assignee + state/status fields). Built once server-side so the
	// per-keystroke filter is a single cheap indexOf per row.
	SearchText string
}

// externCounts is the top-of-page summary bar.
type externCounts struct {
	MRs  int
	Jira int
}

// externGroup is one section when the page is grouped (?group=). Rows preserve
// the already-applied sort order; groups themselves are ordered largest-first
// then alphabetically by label.
type externGroup struct {
	// Key is the stable, lowercased grouping key (used for anchors / CSS).
	Key string
	// Label is the human-facing section heading.
	Label string
	// Rows are the rows in this group, in sort order.
	Rows []externRowView
}

// externControl is a single filter/sort/group chip rendered in the control
// strip: a label, the URL that applies it, whether it is currently active, and
// an optional count badge (0 hides the badge).
type externControl struct {
	Label  string
	URL    string
	Active bool
	Count  int
}

// externControls bundles the three control rows the template renders.
type externControls struct {
	Sources []externControl
	Sorts   []externControl
	Groups  []externControl
}

// externState captures the parsed query filters for one /extern render. It is
// the single source of truth for building control-chip URLs that preserve the
// other dimensions.
type externState struct {
	source  string
	project string
	status  string
	sortKey string
	group   string
	base    string // chip base path (s.basePath + "/extern")
}

// params renders the non-default filter dimensions as a query map, then applies
// override (an empty override value deletes that key). Defaults (source="",
// status="open", sort="updated", group="") are omitted so URLs stay clean.
func (st externState) params(override map[string]string) map[string]string {
	m := map[string]string{}
	if st.source != "" {
		m["source"] = st.source
	}
	if st.project != "" {
		m["project"] = st.project
	}
	if st.status != "" && !strings.EqualFold(st.status, "open") {
		m["status"] = st.status
	}
	if st.sortKey != "" && st.sortKey != externSortUpdated {
		m["sort"] = st.sortKey
	}
	if st.group != externGroupNone {
		m["group"] = st.group
	}
	for k, v := range override {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

// chipURL builds the chip URL for the current state with override applied.
func (st externState) chipURL(override map[string]string) string {
	q := url.Values{}
	for k, v := range st.params(override) {
		q.Set(k, v)
	}
	if len(q) == 0 {
		return st.base
	}
	return st.base + "?" + q.Encode()
}

// controls builds the source / sort / group chip rows for the current state.
func (st externState) controls(counts externCounts) externControls {
	c := externControls{}

	c.Sources = []externControl{
		{Label: "All", URL: st.chipURL(map[string]string{"source": ""}), Active: st.source == ""},
		{Label: "Merge requests", URL: st.chipURL(map[string]string{"source": externSourceMR}), Active: st.source == externSourceMR, Count: counts.MRs},
		{Label: "JIRA", URL: st.chipURL(map[string]string{"source": externSourceJIRA}), Active: st.source == externSourceJIRA, Count: counts.Jira},
	}

	c.Sorts = []externControl{
		{Label: "Newest", URL: st.chipURL(map[string]string{"sort": ""}), Active: st.sortKey == externSortUpdated},
		{Label: "Oldest", URL: st.chipURL(map[string]string{"sort": externSortOldest}), Active: st.sortKey == externSortOldest},
		{Label: "Title", URL: st.chipURL(map[string]string{"sort": externSortTitle}), Active: st.sortKey == externSortTitle},
	}

	c.Groups = []externControl{
		{Label: "Flat", URL: st.chipURL(map[string]string{"group": ""}), Active: st.group == externGroupNone},
		{Label: "Project", URL: st.chipURL(map[string]string{"group": externGroupProject}), Active: st.group == externGroupProject},
		{Label: "State", URL: st.chipURL(map[string]string{"group": externGroupState}), Active: st.group == externGroupState},
		{Label: "Source", URL: st.chipURL(map[string]string{"group": externGroupSource}), Active: st.group == externGroupSource},
	}
	return c
}

// handleExtern renders /extern — a unified list of externally-sourced gardenio
// objects (GitLab merge-requests + JIRA tickets) presented as first-class rows
// with deep links and status pills.
//
// Query params:
//   - ?source=mr|jira            — restrict to one source (default: both)
//   - ?project=<name>            — restrict to a boat project label
//   - ?status=<status>           — bead lifecycle status (default: open; "all" = any)
//   - ?sort=updated|oldest|title — row ordering (default: newest updated_at)
//   - ?group=project|state|source — section the list (default: flat)
//
// Read-only: the page never mutates external objects; status reflects the
// last-synced bead fields written by the gitlab / jira bridges, plus a live
// GitLab MR-status strip (60s-cached) for the configured gardenio project.
func (s *Server) handleExtern(w http.ResponseWriter, r *http.Request) {
	sourceFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if sourceFilter != externSourceMR && sourceFilter != externSourceJIRA {
		sourceFilter = ""
	}
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	if statusFilter == "" {
		statusFilter = "open"
	}
	sortKey := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))
	switch sortKey {
	case externSortOldest, externSortTitle:
		// recognized
	default:
		sortKey = externSortUpdated
	}
	groupKey := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group")))
	switch groupKey {
	case externGroupProject, externGroupState, externGroupSource:
		// recognized
	default:
		groupKey = externGroupNone
	}

	st := externState{
		source:  sourceFilter,
		project: projectFilter,
		status:  statusFilter,
		sortKey: sortKey,
		group:   groupKey,
		base:    s.basePath + "/extern",
	}

	data := map[string]any{
		"SourceFilter":  sourceFilter,
		"ProjectFilter": projectFilter,
		"StatusFilter":  statusFilter,
		"SortKey":       sortKey,
		"GroupKey":      groupKey,
		"Rows":          []externRowView{},
		"Counts":        externCounts{},
		"ChipBasePath":  st.base,
		// ClearURL drops project/status/sort/group but keeps the source lens —
		// the empty-state and "× clear" affordance both point here.
		"ClearURL": st.chipURL(map[string]string{"project": "", "status": "", "sort": "", "group": ""}),
		// AllStatusURL keeps every other dimension but widens status to "all",
		// the most common reason an open-only filter looks empty.
		"AllStatusURL": st.chipURL(map[string]string{"status": "all"}),
	}

	now := s.now()
	var rows []externRowView
	counts := externCounts{}
	var loadErr bool
	truncated := false

	statuses := []string{statusFilter}
	if strings.EqualFold(statusFilter, "all") {
		statuses = nil
	}

	// --- Merge requests ---
	if sourceFilter == "" || sourceFilter == externSourceMR {
		q := beadsapi.ListBeadsQuery{
			Types:    []string{"merge-request"},
			Statuses: statuses,
			Limit:    externListLimit,
			Sort:     "-updated_at",
		}
		if projectFilter != "" {
			q.Labels = []string{"project:" + projectFilter}
		}
		res, err := s.daemon.ListBeadsFiltered(r.Context(), q)
		if err != nil {
			s.logger.Error("extern: listing merge-request beads", "error", err)
			loadErr = true
		} else {
			truncated = truncated || len(res.Beads) >= externListLimit
			for _, b := range res.Beads {
				if b == nil {
					continue
				}
				rows = append(rows, externRowFromMR(b, now))
				counts.MRs++
			}
		}
	}

	// --- JIRA tickets ---
	if sourceFilter == "" || sourceFilter == externSourceJIRA {
		q := beadsapi.ListBeadsQuery{
			Labels:   []string{"source:jira"},
			Statuses: statuses,
			Limit:    externListLimit,
			Sort:     "-updated_at",
		}
		if projectFilter != "" {
			q.Labels = append(q.Labels, "project:"+projectFilter)
		}
		res, err := s.daemon.ListBeadsFiltered(r.Context(), q)
		if err != nil {
			s.logger.Error("extern: listing jira beads", "error", err)
			loadErr = true
		} else {
			truncated = truncated || len(res.Beads) >= externListLimit
			for _, b := range res.Beads {
				if b == nil {
					continue
				}
				rows = append(rows, externRowFromJira(b, now))
				counts.Jira++
			}
		}
	}

	sortExternRows(rows, sortKey)

	data["Rows"] = rows
	data["Counts"] = counts
	data["Controls"] = st.controls(counts)
	data["Truncated"] = truncated
	data["Limit"] = externListLimit
	if groupKey != externGroupNone {
		data["Groups"] = groupExternRows(rows, groupKey)
	}

	// Live GitLab MR-status strip (60s-cached) — reuses the /index panel's
	// lister + cache. Only meaningful when MRs are in view and no project
	// filter narrows below the configured gardenio project, since the live
	// counts speak for that project alone. computeIndexMRStatus returns
	// Available=false when no lister is wired, so the strip self-hides.
	if sourceFilter != externSourceJIRA && projectFilter == "" {
		data["MRLive"] = s.computeIndexMRStatus(r.Context())
	}

	if loadErr && len(rows) == 0 {
		data["Error"] = "Failed to load external objects."
	}
	s.render(w, r, "extern.html", data)
}

// sortExternRows orders rows in place per the resolved sort key. The default
// (and any unrecognized key) is newest-activity-first, matching the daemon's
// own -updated_at ordering so the two stay consistent.
func sortExternRows(rows []externRowView, sortKey string) {
	switch sortKey {
	case externSortOldest:
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].UpdatedAtRFC < rows[j].UpdatedAtRFC
		})
	case externSortTitle:
		sort.SliceStable(rows, func(i, j int) bool {
			return strings.ToLower(rows[i].Title) < strings.ToLower(rows[j].Title)
		})
	default: // externSortUpdated — newest first across both sources.
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].UpdatedAtRFC > rows[j].UpdatedAtRFC
		})
	}
}

// groupExternRows buckets rows by the resolved group key, preserving the
// incoming (sorted) row order within each bucket. Groups are returned
// largest-first, then alphabetically by label, so the densest section leads.
// Returns nil for an unrecognized / none key.
func groupExternRows(rows []externRowView, groupKey string) []externGroup {
	keyFn := externGroupKeyFunc(groupKey)
	if keyFn == nil {
		return nil
	}
	byKey := map[string]*externGroup{}
	var order []string
	for _, row := range rows {
		key, label := keyFn(row)
		g, ok := byKey[key]
		if !ok {
			g = &externGroup{Key: key, Label: label}
			byKey[key] = g
			order = append(order, key)
		}
		g.Rows = append(g.Rows, row)
	}
	groups := make([]externGroup, 0, len(order))
	for _, key := range order {
		groups = append(groups, *byKey[key])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i].Rows) != len(groups[j].Rows) {
			return len(groups[i].Rows) > len(groups[j].Rows)
		}
		return groups[i].Label < groups[j].Label
	})
	return groups
}

// externGroupKeyFunc maps a group key to a row→(key,label) projector, or nil
// for none/unrecognized.
func externGroupKeyFunc(groupKey string) func(externRowView) (string, string) {
	switch groupKey {
	case externGroupProject:
		return func(r externRowView) (string, string) {
			if r.Project == "" {
				return "~none", "(no project)"
			}
			return r.Project, r.Project
		}
	case externGroupState:
		return func(r externRowView) (string, string) {
			s := externRowState(r)
			return strings.ToLower(s), s
		}
	case externGroupSource:
		return func(r externRowView) (string, string) {
			if r.Source == externSourceMR {
				return externSourceMR, "Merge requests"
			}
			return externSourceJIRA, "JIRA tickets"
		}
	}
	return nil
}

// externRowState is the unified "state" of a row for group=state: MR state for
// merge requests, JIRA status for tickets, "unknown" when neither is set.
func externRowState(r externRowView) string {
	if r.Source == externSourceMR {
		if r.MRState != "" {
			return r.MRState
		}
		return "unknown"
	}
	if r.JiraStatus != "" {
		return r.JiraStatus
	}
	return "unknown"
}

// externRowFromMR projects a type=merge-request bead onto the unified row.
func externRowFromMR(b *beadsapi.BeadDetail, now time.Time) externRowView {
	f := b.Fields
	row := externRowView{
		Source:       externSourceMR,
		BeadID:       b.ID,
		Title:        firstStr(f["mr_title"], b.Title),
		Project:      projectFromLabels(b.Labels),
		Status:       b.Status,
		Ago:          fmtTimeAgo(b.UpdatedAt, now),
		UpdatedAtRFC: rfcTime(b.UpdatedAt),
		ExternalURL:  f["mr_url"],

		MRState:        firstStr(f["mr_state"], "unknown"),
		PipelineStatus: firstStr(f["mr_pipeline_status"], f["pipeline_status"]),
		PipelineURL:    f["mr_pipeline_url"],
		TargetBranch:   f["target_branch"],
		Author:         f["author"],
	}
	if iid := firstStr(f["mr_iid"], f["gitlab_mr_iid"]); iid != "" {
		row.ExternalLabel = "!" + iid
	}
	row.SearchText = externSearchText(row)
	return row
}

// externRowFromJira projects a source:jira bead onto the unified row.
func externRowFromJira(b *beadsapi.BeadDetail, now time.Time) externRowView {
	f := b.Fields
	row := externRowView{
		Source:        externSourceJIRA,
		BeadID:        b.ID,
		Title:         b.Title,
		Project:       projectFromLabels(b.Labels),
		Status:        b.Status,
		Ago:           fmtTimeAgo(b.UpdatedAt, now),
		UpdatedAtRFC:  rfcTime(b.UpdatedAt),
		ExternalURL:   f["jira_url"],
		ExternalLabel: f["jira_key"],

		JiraStatus:   f["jira_status"],
		JiraType:     f["jira_type"],
		JiraAssignee: f["jira_assignee"],
	}
	if row.ExternalURL == "" && row.ExternalLabel != "" {
		// Fall back to a constructed browse URL when the field is absent
		// (legacy beads predate jira_url).
		row.ExternalURL = jiraBaseURL() + "/browse/" + url.PathEscape(row.ExternalLabel)
	}
	row.SearchText = externSearchText(row)
	return row
}

// externSearchText builds the lowercased substring haystack for one row,
// joining every operator-meaningful field (title, external id, bead id,
// project, author/assignee, MR state / pipeline / target branch, JIRA
// type/status) with spaces. The client-side search box matches the typed
// query against this string with a single indexOf, so the per-keystroke
// filter never touches the DOM text or re-queries the server.
func externSearchText(r externRowView) string {
	parts := []string{
		r.Title, r.ExternalLabel, r.BeadID, r.Project, r.Status,
		r.MRState, r.PipelineStatus, r.TargetBranch, r.Author,
		r.JiraStatus, r.JiraType, r.JiraAssignee,
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// firstStr returns the first argument that is not the empty string, or ""
// when all are empty. Keeps the field-fallback chains in the projections terse.
// (bead_preview.go's firstNonEmpty is map-keyed; this variadic form is local.)
func firstStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
