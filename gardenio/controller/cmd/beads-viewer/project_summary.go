package main

import (
	"context"
	"slices"
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// projectSummaryLimit caps the size of the "all active beads in this project"
// query that backs the right-rail summary panel. The summary is a count-only
// view of open + in_progress beads — at realistic project size (hundreds, not
// thousands of active beads) this single query produces all the facets we
// render without round-tripping per dimension.
const projectSummaryLimit = 500

// projectSummary is the right-rail facet panel on /p/<project>. All counts are
// computed in-memory from a single project-scoped daemon query so the sidebar
// never doubles the daemon's load — the cost is one extra ListBeadsFiltered
// call per page render.
type projectSummary struct {
	// Status counts across open + in_progress (closed is intentionally
	// excluded — we don't want to pay for the historical tail just to render
	// a number on the sidebar).
	Open       int
	InProgress int
	TotalOpen  int // Open + InProgress, the "active universe" sum.

	// TypeCounts maps a chip Value ("task", "bug", "issues", "all", ...) to
	// the count for that filter slice within the active universe. Used by
	// the chip strip to render "Tasks 88" etc.
	TypeCounts map[string]int

	// Top assignees by active bead count. Capped at 5 entries; ties broken
	// by name asc.
	TopAssignees []projectAssigneeCount

	// PriorityCounts is the [P0, P1, P2, P3, P4] distribution across the
	// active universe. Indexed by priority value (0-4).
	PriorityCounts [5]int

	// Blocked is the count of active beads with BlockedOnDecision=true —
	// surfaces "this work is waiting on a human decision" at a glance.
	Blocked int

	// StaleInProgress is the count of in_progress beads whose UpdatedAt is
	// older than 24h. Same heuristic used by `gb news` for "stale in_progress".
	StaleInProgress int

	// Created24h is the count of beads created within the last 24h. Helps
	// answer "what came in today" without a separate page.
	Created24h int

	// BlockedBeads is the oldest few beads with BlockedOnDecision=true. Renders
	// in the right rail as a clickable list so operators can jump straight to
	// the work that's waiting on a human decision, not just see a count.
	BlockedBeads []projectSummaryListItem

	// StaleBeads is the oldest in_progress beads whose UpdatedAt is older than
	// 24h — the same beads counted in StaleInProgress, but with id+title so
	// the right rail can link to them directly.
	StaleBeads []projectSummaryListItem

	// Truncated indicates the underlying query hit projectSummaryLimit and
	// the counts represent the first N rows — surface a banner so operators
	// know the numbers are a floor, not a total.
	Truncated bool
}

// projectAssigneeCount is one row in the right-rail "Top assignees" list.
type projectAssigneeCount struct {
	Assignee string
	Count    int
}

// projectSummaryListItem is one compact row in the right-rail attention lists
// ("Blocked" and "Stale in-progress"). Carries just enough to render a clickable
// title + age — full bead detail lives on the bead-show page.
type projectSummaryListItem struct {
	ID    string
	Title string
	Ago   string
}

// projectSummaryListLimit caps the number of beads surfaced in each attention
// list. Five keeps the right rail compact while still showing the oldest few
// items that demand human attention.
const projectSummaryListLimit = 5

// buildProjectSummary computes the right-rail facet panel for /p/<project>.
// Issues one daemon query for the project's active universe (open +
// in_progress, all types) and slices it into counts in-memory.
//
// Best-effort: a daemon error returns nil and the caller renders the page
// without the panel rather than failing the whole render.
func buildProjectSummary(ctx context.Context, daemon beadAPI, project string, now time.Time) (*projectSummary, error) {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		// Kinds:["issue"] matches the default `kd list` semantics — without
		// it the project's active universe is dominated by type=session /
		// type=event / agent / advice / schedule rows and the 500-row
		// scan fills with non-issue beads, starving the right-rail facets
		// (ka-0000002522). Mirrors ka-0000006881.
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{"project:" + project},
		Limit:    projectSummaryLimit,
		// Only "is there more" is shown (Truncated), so a full page skips the
		// project-wide COUNT(*) (gd-I2zntgOsCq).
		NoTotal: true,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return &projectSummary{TypeCounts: map[string]int{}}, nil
	}

	sum := &projectSummary{
		TypeCounts: map[string]int{},
		Truncated:  res.MayHaveMore(),
	}
	assignees := map[string]int{}
	staleCutoff := now.Add(-24 * time.Hour)
	createdCutoff := now.Add(-24 * time.Hour)

	type listCandidate struct {
		ID        string
		Title     string
		UpdatedAt time.Time
	}
	var blockedCandidates, staleCandidates []listCandidate

	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		switch b.Status {
		case "open":
			sum.Open++
		case "in_progress":
			sum.InProgress++
			if !b.UpdatedAt.IsZero() && b.UpdatedAt.Before(staleCutoff) {
				sum.StaleInProgress++
				staleCandidates = append(staleCandidates, listCandidate{ID: b.ID, Title: b.Title, UpdatedAt: b.UpdatedAt})
			}
		}
		sum.TotalOpen++

		if b.Priority >= 0 && b.Priority < len(sum.PriorityCounts) {
			sum.PriorityCounts[b.Priority]++
		}

		if b.BlockedOnDecision {
			sum.Blocked++
			blockedCandidates = append(blockedCandidates, listCandidate{ID: b.ID, Title: b.Title, UpdatedAt: b.UpdatedAt})
		}
		if !b.CreatedAt.IsZero() && b.CreatedAt.After(createdCutoff) {
			sum.Created24h++
		}
		if a := b.Assignee; a != "" {
			assignees[a]++
		}

		// Per-chip counts. "all" matches everything; "issues" is the iOS
		// issue-macro slice; everything else is an exact type match.
		sum.TypeCounts["all"]++
		if isIssueMacroType(b.Type) {
			sum.TypeCounts["issues"]++
		}
		sum.TypeCounts[b.Type]++
	}

	// Top 5 assignees, ties broken by name asc so the panel is stable
	// across renders.
	rows := make([]projectAssigneeCount, 0, len(assignees))
	for name, count := range assignees {
		rows = append(rows, projectAssigneeCount{Assignee: name, Count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Assignee < rows[j].Assignee
	})
	if len(rows) > 5 {
		rows = rows[:5]
	}
	sum.TopAssignees = rows

	// Oldest-first ordering: the items operators most need to triage are the
	// ones that have been stuck longest. Ties break on bead id for stable
	// renders across cache misses.
	sortByOldest := func(items []listCandidate) {
		sort.Slice(items, func(i, j int) bool {
			if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
				return items[i].UpdatedAt.Before(items[j].UpdatedAt)
			}
			return items[i].ID < items[j].ID
		})
	}
	sortByOldest(blockedCandidates)
	sortByOldest(staleCandidates)

	toList := func(items []listCandidate) []projectSummaryListItem {
		if len(items) > projectSummaryListLimit {
			items = items[:projectSummaryListLimit]
		}
		out := make([]projectSummaryListItem, len(items))
		for i, it := range items {
			out[i] = projectSummaryListItem{
				ID:    it.ID,
				Title: it.Title,
				Ago:   formatAgo(it.UpdatedAt),
			}
		}
		return out
	}
	sum.BlockedBeads = toList(blockedCandidates)
	sum.StaleBeads = toList(staleCandidates)

	return sum, nil
}

// isIssueMacroType reports whether t is one of the issue-macro types
// (task / feature / bug / epic / chore). Kept in sync with issueMacroTypes
// on server.go.
func isIssueMacroType(t string) bool {
	return slices.Contains(issueMacroTypes, t)
}
