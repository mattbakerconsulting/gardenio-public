package main

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"gardenio/controller/internal/beadsapi"
)

// inboxStreamKind enumerates the row kinds the unified Gmail-density inbox
// stream interleaves. Each value also doubles as the `?kind=` query filter
// and the data attribute on the rendered row, so the chip strip can hide
// rows client-side without a re-fetch.
const (
	inboxKindIntake    = "intake"
	inboxKindDecision  = "decision"
	inboxKindAgent     = "agent"
	inboxKindEpic      = "epic"
	inboxKindForwarded = "forwarded"
)

// inboxStreamKinds is the canonical kind ordering for the chip strip and
// for `isValidInboxKind`. "" means the All chip / unfiltered stream.
var inboxStreamKinds = []string{
	inboxKindIntake,
	inboxKindDecision,
	inboxKindAgent,
	inboxKindEpic,
	inboxKindForwarded,
}

// inboxStreamRow is one row in the unified Gmail-density activity stream.
// Every "object" the inbox surfaces — intakes, pending decisions, agents
// (active + recent), open epics, and forwarded beads — collapses to one
// of these rows in chronological order. The Kind discriminator drives
// glyph and chip rendering; kind-specific fields (DecisionPrompt,
// AgentTaskID, etc.) stay empty for kinds that do not use them.
type inboxStreamRow struct {
	Kind         string
	ID           string
	Title        string
	ShortTitle   string // shown in place of Title when set (gd-V5nGseRS5f)
	Type         string
	Source       string
	Project      string
	Actor        string
	DescPreview  string
	Ago          string
	ActivityAt   time.Time
	Priority     int
	Pinned       bool
	Forwarded    bool
	ForwardOrder int
	Flagged      bool
	InboxWorthy  bool
	HasReport    bool
	BlockedOn    bool
	Href         string
	UpdatedAtRFC string
	// Decision-only.
	DecisionPrompt    string
	DecisionRequester string
	DecisionOptions   []decisionOption
	// Agent-only.
	AgentName      string
	AgentRole      string
	AgentTaskID    string
	AgentTaskTitle string
	AgentState     string
	PodPhase       string
	SpawnedBy      string
	AgentClosed    bool
	// Epic-only.
	EpicStatus string
	// MR chip (ka-0000007315) — populated when the bead is type=merge-request
	// (read from bead.Fields) or a task bead with a `produces` edge to one
	// (resolved via inboxMRResolver). Empty MRState suppresses the chip.
	MRState string
	MRURL   string
}

// inboxKindChipView is one chip in the kind filter strip. The chip strip
// renders below the existing scope chip strip.
//
// Collapsed is set by buildInboxKindChips when >=4 of 6 kind counts are zero
// (ka-0000001480). The template renders Collapsed=true chips inside a single
// "+N empty" group at the end of the row, keeping each one a click target.
type inboxKindChipView struct {
	Label     string
	Kind      string // "" == All
	Count     int
	Active    bool
	Href      string
	Collapsed bool
}

// inboxKindCollapseThreshold is the number of zero-count chips required
// before the kind-chip strip collapses zeros into a single "+N empty" group.
// 4 of 6 zeros => half the strip is dead pixels; collapsing reclaims that row.
const inboxKindCollapseThreshold = 4

// isValidInboxKind reports whether the given query value names a known
// kind chip.
func isValidInboxKind(k string) bool {
	if k == "" {
		return true
	}
	return slices.Contains(inboxStreamKinds, k)
}

// buildInboxStream merges per-kind raw bead slices into a single time-
// ordered stream. The caller is responsible for fetching and pre-filtering
// each kind to the viewer's scope; this function just normalizes, dedupes
// across kinds (intake/forwarded same bead → forwarded wins), and sorts.
//
// Sort order:
//  1. Pinned rows first.
//  2. Forwarded rows (in ForwardOrder asc) above non-forwarded —
//     preserves the ka-0000007204 contract that freshly-forwarded beads
//     land at the top of the rail.
//  3. Activity timestamp DESC (newest first).
//  4. ID asc as the stable tiebreaker.
func buildInboxStream(
	intakeBeads []*beadsapi.BeadDetail,
	forwardOrder map[string]int,
	decisionRows []decisionRowView,
	decisionBeads []*beadsapi.BeadDetail,
	activeAgents []agentRowView,
	activeAgentBeads []*beadsapi.BeadDetail,
	recentAgents []agentRowView,
	recentAgentBeads []*beadsapi.BeadDetail,
	epicRows []inboxOpenEpicRowView,
	epicBeads []*beadsapi.BeadDetail,
	mr *inboxMRResolver,
	basePath string,
	now time.Time,
) []inboxStreamRow {
	rows := make([]inboxStreamRow, 0, len(intakeBeads)+len(decisionRows)+len(activeAgents)+len(recentAgents)+len(epicRows))

	for _, b := range intakeBeads {
		if b == nil {
			continue
		}
		row := inboxStreamRowFromIntake(b, forwardOrder, mr, basePath, now)
		rows = append(rows, row)
	}

	// Decisions zip against the row + bead pair so we can read
	// CreatedAt/UpdatedAt from the bead while keeping the projected fields
	// (prompt snippet, parsed options) from the existing row builder.
	for i, r := range decisionRows {
		var b *beadsapi.BeadDetail
		if i < len(decisionBeads) {
			b = decisionBeads[i]
		}
		rows = append(rows, inboxStreamRowFromDecision(r, b, basePath))
	}

	for i, a := range activeAgents {
		var b *beadsapi.BeadDetail
		if i < len(activeAgentBeads) {
			b = activeAgentBeads[i]
		}
		rows = append(rows, inboxStreamRowFromAgent(a, b, basePath, false))
	}
	for i, a := range recentAgents {
		var b *beadsapi.BeadDetail
		if i < len(recentAgentBeads) {
			b = recentAgentBeads[i]
		}
		rows = append(rows, inboxStreamRowFromAgent(a, b, basePath, true))
	}

	for i, e := range epicRows {
		var b *beadsapi.BeadDetail
		if i < len(epicBeads) {
			b = epicBeads[i]
		}
		rows = append(rows, inboxStreamRowFromEpic(e, b, basePath, now))
	}

	dedupeStreamRows(&rows)
	sortInboxStream(rows)
	return rows
}

// dedupeStreamRows collapses rows with the same bead ID. When the same bead
// surfaces from multiple sources (the canonical case: an epic / agent /
// decision bead that is also `flag:inbox-worthy` is returned by both the
// intake/worthy union AND its type-specific query), the more-specific kind
// wins so the operator sees the type's chrome (epic status pill, agent
// state, decision prompt) instead of a second generic intake row for the
// same bead. Forwarded rows take precedence over everything else because
// "somebody hand-routed this at me" is the strongest user-intent signal.
//
// Precedence (highest to lowest): forwarded > epic > agent > decision >
// intake. Same-kind duplicates (agent active + recent on a status-flip
// race) collapse to the first occurrence.
//
// ka-0000000893: prior implementation deduped by `(Kind, ID)`, leaving
// cross-kind duplicates intentionally; in practice the worthy-side union
// returns beads of mixed types and the type-specific queries return the
// same beads, so "most rows render twice" was the visible failure mode.
func dedupeStreamRows(rows *[]inboxStreamRow) {
	if len(*rows) < 2 {
		return
	}
	rank := func(kind string) int {
		switch kind {
		case inboxKindForwarded:
			return 5
		case inboxKindEpic:
			return 4
		case inboxKindAgent:
			return 3
		case inboxKindDecision:
			return 2
		case inboxKindIntake:
			return 1
		default:
			return 0
		}
	}
	best := make(map[string]int, len(*rows))
	for i, r := range *rows {
		j, ok := best[r.ID]
		if !ok || rank(r.Kind) > rank((*rows)[j].Kind) {
			best[r.ID] = i
		}
	}
	out := (*rows)[:0]
	for i, r := range *rows {
		if best[r.ID] == i {
			out = append(out, r)
		}
	}
	*rows = out
}

func inboxStreamRowFromIntake(b *beadsapi.BeadDetail, forwardOrder map[string]int, mr *inboxMRResolver, basePath string, now time.Time) inboxStreamRow {
	row := inboxStreamRow{
		Kind:         inboxKindIntake,
		ID:           b.ID,
		Title:        b.Title,
		ShortTitle:   b.ShortTitle,
		Type:         b.Type,
		Project:      projectFromLabels(b.Labels),
		Actor:        b.OperatedByDisplay,
		DescPreview:  snippet(b.Description, inboxDescPreviewLimit),
		Ago:          fmtTimeAgo(b.UpdatedAt, now),
		ActivityAt:   b.UpdatedAt,
		Priority:     b.Priority,
		ForwardOrder: -1,
		Href:         basePath + "/inbox/" + b.ID,
		UpdatedAtRFC: rfcTime(b.UpdatedAt),
	}
	if info := mr.Lookup(b.ID); info.State != "" {
		row.MRState = info.State
		row.MRURL = info.URL
	}
	for _, l := range b.Labels {
		switch {
		case l == "flag:operator-blocking":
			row.Flagged = true
		case l == labelInboxWorthy:
			row.InboxWorthy = true
		case l == labelInboxPinned:
			row.Pinned = true
		case strings.HasPrefix(l, "source:"):
			if row.Source == "" {
				row.Source = strings.TrimPrefix(l, "source:")
			}
		}
	}
	if order, ok := forwardOrder[b.ID]; ok {
		row.Forwarded = true
		row.ForwardOrder = order
		// Re-classify a forwarded row's primary kind so the kind-chip
		// counts include it under "Forwarded" rather than "Intakes".
		// This mirrors the design's chip semantics — a freshly-forwarded
		// bead is a forwarded thing first, intake second.
		row.Kind = inboxKindForwarded
	}
	return row
}

func inboxStreamRowFromDecision(r decisionRowView, b *beadsapi.BeadDetail, basePath string) inboxStreamRow {
	row := inboxStreamRow{
		Kind:              inboxKindDecision,
		ID:                r.ID,
		Title:             r.Title,
		Type:              "decision",
		Project:           r.Project,
		Actor:             r.RequestedBy,
		Ago:               r.Ago,
		Href:              basePath + "/" + r.ID,
		UpdatedAtRFC:      r.UpdatedAtRFC,
		DecisionPrompt:    r.Prompt,
		DecisionRequester: r.RequestedBy,
		DecisionOptions:   r.Options,
		ForwardOrder:      -1,
	}
	if b != nil {
		// Pending decisions: sort by created_at — newest pending first
		// is the operator's lens. UpdatedAt drifts when option lists get
		// edited, which is not the activity signal we want.
		if !b.CreatedAt.IsZero() {
			row.ActivityAt = b.CreatedAt
		} else {
			row.ActivityAt = b.UpdatedAt
		}
	}
	return row
}

func inboxStreamRowFromAgent(a agentRowView, b *beadsapi.BeadDetail, basePath string, closed bool) inboxStreamRow {
	row := inboxStreamRow{
		Kind:           inboxKindAgent,
		ID:             a.ID,
		Title:          a.Name,
		Type:           "agent",
		Project:        a.Project,
		Actor:          a.OperatedByDisplay,
		DescPreview:    a.Desc,
		Ago:            a.Ago,
		Href:           basePath + "/" + a.ID,
		UpdatedAtRFC:   a.UpdatedAtRFC,
		AgentName:      a.Name,
		AgentRole:      a.Role,
		AgentTaskID:    a.TaskID,
		AgentTaskTitle: a.TaskTitle,
		AgentState:     a.AgentState,
		PodPhase:       a.PodPhase,
		SpawnedBy:      a.SpawnedBy,
		AgentClosed:    closed,
		ForwardOrder:   -1,
	}
	if b != nil {
		row.ActivityAt = b.UpdatedAt
	}
	return row
}

func inboxStreamRowFromEpic(e inboxOpenEpicRowView, b *beadsapi.BeadDetail, basePath string, now time.Time) inboxStreamRow {
	row := inboxStreamRow{
		Kind:         inboxKindEpic,
		ID:           e.ID,
		Title:        e.Title,
		Type:         "epic",
		Priority:     e.Priority,
		EpicStatus:   e.Status,
		Href:         e.Href,
		ForwardOrder: -1,
	}
	if row.Href == "" {
		row.Href = basePath + "/inbox/" + e.ID
	}
	if b != nil {
		row.Project = projectFromLabels(b.Labels)
		row.Actor = b.OperatedByDisplay
		row.DescPreview = snippet(b.Description, inboxDescPreviewLimit)
		row.Ago = fmtTimeAgo(b.UpdatedAt, now)
		row.ActivityAt = b.UpdatedAt
		row.UpdatedAtRFC = rfcTime(b.UpdatedAt)
	}
	return row
}

// sortInboxStream applies the Gmail-density sort order described in
// buildInboxStream. Stable so callers controlling input order get a
// predictable tiebreak.
func sortInboxStream(rows []inboxStreamRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Pinned != b.Pinned {
			return a.Pinned
		}
		if a.Forwarded != b.Forwarded {
			return a.Forwarded
		}
		if a.Forwarded && b.Forwarded && a.ForwardOrder != b.ForwardOrder {
			return a.ForwardOrder < b.ForwardOrder
		}
		if !a.ActivityAt.Equal(b.ActivityAt) {
			return a.ActivityAt.After(b.ActivityAt)
		}
		return a.ID < b.ID
	})
}

// buildInboxKindChips constructs the kind chip strip with per-kind counts
// computed against the unfiltered stream so the operator can always see
// "All · 12 / Decisions · 3" etc. When project is set, the chip hrefs
// preserve it so toggling kind does not clear the project filter.
func buildInboxKindChips(rows []inboxStreamRow, basePath, scope, activeKind, project string) []inboxKindChipView {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Kind]++
	}
	defs := []struct {
		label string
		kind  string
	}{
		{"All", ""},
		{"Intakes", inboxKindIntake},
		{"Decisions", inboxKindDecision},
		{"Agents", inboxKindAgent},
		{"Epics", inboxKindEpic},
		{"Forwarded", inboxKindForwarded},
	}
	out := make([]inboxKindChipView, 0, len(defs))
	for _, d := range defs {
		count := len(rows)
		if d.kind != "" {
			count = counts[d.kind]
		}
		href := basePath + "/inbox"
		var qs []string
		if scope != "" && scope != "mine" {
			qs = append(qs, "scope="+scope)
		}
		if d.kind != "" {
			qs = append(qs, "kind="+d.kind)
		}
		if project != "" {
			qs = append(qs, "project="+project)
		}
		if len(qs) > 0 {
			href += "?" + strings.Join(qs, "&")
		}
		out = append(out, inboxKindChipView{
			Label:  d.label,
			Kind:   d.kind,
			Count:  count,
			Active: d.kind == activeKind,
			Href:   href,
		})
	}
	markCollapsedKindChips(out)
	return out
}

// markCollapsedKindChips flips Collapsed=true on zero-count chips when the
// row hits the collapse threshold. The currently-active chip is never
// collapsed — leaving it inside the +N group would hide the user's own
// filter selection.
func markCollapsedKindChips(chips []inboxKindChipView) {
	zeros := 0
	for _, c := range chips {
		if c.Count == 0 {
			zeros++
		}
	}
	if zeros < inboxKindCollapseThreshold {
		return
	}
	for i := range chips {
		if chips[i].Count == 0 && !chips[i].Active {
			chips[i].Collapsed = true
		}
	}
}

// collapsedKindChips returns the subset of chips marked Collapsed=true,
// preserving order. The template uses this for the "+N empty:" group; an
// empty slice means the row renders without collapse (no +N affordance).
func collapsedKindChips(chips []inboxKindChipView) []inboxKindChipView {
	var out []inboxKindChipView
	for _, c := range chips {
		if c.Collapsed {
			out = append(out, c)
		}
	}
	return out
}

// inboxProjectOption is one entry in the inbox project dropdown.
type inboxProjectOption struct {
	Value    string // empty for the "All projects" sentinel
	Label    string
	Count    int
	Selected bool
}

// buildInboxProjectOptions returns the dropdown options derived from the
// projects represented in the unfiltered stream. The first entry is the
// "All projects" sentinel (value=""); the rest are sorted alphabetically.
// Rows with an empty project are grouped under "(unlabeled)" so they remain
// reachable when the operator narrows the view.
func buildInboxProjectOptions(rows []inboxStreamRow, activeProject string) []inboxProjectOption {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Project]++
	}
	projects := make([]string, 0, len(counts))
	hasUnlabeled := false
	for p := range counts {
		if p == "" {
			hasUnlabeled = true
			continue
		}
		projects = append(projects, p)
	}
	sort.Strings(projects)

	out := make([]inboxProjectOption, 0, len(projects)+2)
	out = append(out, inboxProjectOption{
		Value:    "",
		Label:    "All projects",
		Count:    len(rows),
		Selected: activeProject == "",
	})
	for _, p := range projects {
		out = append(out, inboxProjectOption{
			Value:    p,
			Label:    p,
			Count:    counts[p],
			Selected: p == activeProject,
		})
	}
	if hasUnlabeled {
		out = append(out, inboxProjectOption{
			Value:    "(none)",
			Label:    "(unlabeled)",
			Count:    counts[""],
			Selected: activeProject == "(none)",
		})
	}
	return out
}

// isValidInboxProject reports whether the requested project narrows the
// stream to at least one row in the unfiltered set. Unknown values fall
// back to "all projects" so a stale bookmark does not strand the operator
// on an empty page.
func isValidInboxProject(rows []inboxStreamRow, project string) bool {
	if project == "" {
		return true
	}
	if project == "(none)" {
		for _, r := range rows {
			if r.Project == "" {
				return true
			}
		}
		return false
	}
	for _, r := range rows {
		if r.Project == project {
			return true
		}
	}
	return false
}

// filterStreamByKind drops rows that do not match the active kind. Empty
// activeKind returns the input unchanged.
func filterStreamByKind(rows []inboxStreamRow, activeKind string) []inboxStreamRow {
	if activeKind == "" {
		return rows
	}
	out := make([]inboxStreamRow, 0, len(rows))
	for _, r := range rows {
		if r.Kind == activeKind {
			out = append(out, r)
		}
	}
	return out
}

// filterStreamByProject drops rows that do not match the active project.
// Empty activeProject returns the input unchanged. The "(none)" sentinel
// matches rows with no project label so the dropdown's unlabeled bucket is
// reachable.
func filterStreamByProject(rows []inboxStreamRow, activeProject string) []inboxStreamRow {
	if activeProject == "" {
		return rows
	}
	out := make([]inboxStreamRow, 0, len(rows))
	for _, r := range rows {
		if activeProject == "(none)" {
			if r.Project == "" {
				out = append(out, r)
			}
			continue
		}
		if r.Project == activeProject {
			out = append(out, r)
		}
	}
	return out
}

// fetchInboxStreamSources runs the per-kind queries the inbox needs in
// dependency-free parallel order. Returns raw beads alongside view rows
// so the stream builder can read activity timestamps without re-querying.
//
// Best-effort: a query that errors degrades that kind to empty without
// failing the whole page. Error logs go to s.logger so operators can see
// which axis is misbehaving.
func (s *Server) fetchInboxStreamSources(
	ctx context.Context,
	opID int64,
) (
	intake []*beadsapi.BeadDetail,
	forwardOrder map[string]int,
	decisions []decisionRowView,
	decisionTrunc bool,
	decisionBeads []*beadsapi.BeadDetail,
	activeAgents []agentRowView,
	activeAgentBeads []*beadsapi.BeadDetail,
	recentAgents []agentRowView,
	recentAgentBeads []*beadsapi.BeadDetail,
	epics []inboxOpenEpicRowView,
	epicBeads []*beadsapi.BeadDetail,
	intakeTotal int,
	intakeErr error,
) {
	// Four independent kind-queries — fan them out under errgroup so wall-clock
	// is the slowest branch, not the sum. Each helper is best-effort and
	// returns zero values on its own failure (only listInboxBeadsForViewer
	// surfaces an error to the caller). Sharing one errgroup is safe because
	// no goroutine writes to a value another reads.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		intake, forwardOrder, intakeTotal, intakeErr = s.listInboxBeadsForViewer(gctx, opID)
		// Don't propagate — intakeErr is the named-return the caller checks.
		// Returning would cancel the other branches.
		return nil
	})
	g.Go(func() error {
		decisions, decisionTrunc, decisionBeads = s.loadInboxPendingDecisionsWithBeads(gctx)
		return nil
	})
	g.Go(func() error {
		activeAgents, activeAgentBeads, recentAgents, recentAgentBeads = s.computeInboxAgentsWithBeads(gctx, opID)
		return nil
	})
	g.Go(func() error {
		epics, epicBeads = s.computeInboxOpenEpicsWithBeads(gctx, opID)
		return nil
	})
	_ = g.Wait()
	return
}
