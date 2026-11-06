package main

import (
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// inboxRecentMentionsLimit caps the "Recent mentions" section on /inbox.
// 5 rows keeps the section dense enough to read at a glance when the
// operator's mine-scope is empty, without burning a screenful when many
// adjacent beads exist.
const inboxRecentMentionsLimit = 5

// inboxMentionContextLimit is the rune budget for the "short context"
// column of a recent-mention row. The compact stream truncates with
// ellipsis at the CSS layer; this trim keeps the markup compact too.
const inboxMentionContextLimit = 80

// inboxMentionRow is one single-line row in the dedicated /inbox
// "Recent mentions" section (ka-0000005641). Each row collapses a
// recent agent or decision touchpoint into the three columns the
// section's density mode targets: ago, actor, short context.
//
// Pure view-model — populated from data the inbox already fetches for
// the unified stream, so no daemon round-trip is added.
type inboxMentionRow struct {
	// Source labels the upstream bead kind so the template can attach a
	// glyph or chip without re-deriving from the bead. Either "agent"
	// or "decision" today.
	Source string
	// ID is the originating bead so a click-through routes to its
	// canonical detail page.
	ID string
	// Ago is the prerendered "5m ago" / "yesterday" string for the
	// activity timestamp. Empty when the upstream bead has no usable
	// timestamp; the template suppresses the column in that case.
	Ago string
	// Actor is the operator-or-agent name that produced the mention.
	// SpawnedBy for agents, RequestedBy for decisions. Empty actors are
	// suppressed in the rendered row rather than left as a dangling
	// dot-separator.
	Actor string
	// Context is the short, single-line summary the operator scans for
	// "is this worth a click?" — the agent task title for agent rows,
	// the decision prompt for decision rows. Trimmed to
	// inboxMentionContextLimit runes.
	Context string
	// Href points at the originating bead's canonical detail page.
	Href string
	// UpdatedAtRFC is the machine-readable timestamp used for the
	// ago-column tooltip and any sort-by-time client-side refinement.
	UpdatedAtRFC string
	// ActivityAt is the unprojected timestamp used internally for
	// sorting. Not rendered.
	ActivityAt time.Time
}

// computeInboxRecentMentions derives the operator's recent @-mentions
// from the bead slices the inbox already loads for the unified stream.
// Strictly RENDERING-only — no new daemon queries — per ka-0000005641
// scope (adjacent-buckets sub-bucket of decision ka-0000003122).
//
// Sources today:
//
//   - Active and recently-closed agent beads operated by the viewer.
//     A non-empty `spawned_by` field marks the agent as having been
//     queued on the operator's behalf by another actor — that actor
//     is the @-mention. Self-spawned agents (no spawner) are skipped:
//     they have no third-party mention shape.
//   - Pending decision beads. A decision is by definition a mention of
//     the operator (the requester is asking them to pick); the prompt
//     is the short context.
//
// Rows are deduplicated by (Source, ID), sorted activity-DESC, and
// capped at inboxRecentMentionsLimit. Empty input yields an empty
// slice — the template suppresses the section entirely in that case.
func computeInboxRecentMentions(
	activeAgentBeads []*beadsapi.BeadDetail,
	recentAgentBeads []*beadsapi.BeadDetail,
	decisionBeads []*beadsapi.BeadDetail,
	basePath string,
	now time.Time,
) []inboxMentionRow {
	rows := make([]inboxMentionRow, 0, len(activeAgentBeads)+len(recentAgentBeads)+len(decisionBeads))

	addAgent := func(b *beadsapi.BeadDetail) {
		if b == nil {
			return
		}
		spawner := strings.TrimSpace(b.Fields["spawned_by"])
		if spawner == "" {
			// No third-party mention shape — skip rather than render a
			// row with a blank Actor column.
			return
		}
		ctx := strings.TrimSpace(b.Fields["task_title"])
		if ctx == "" {
			ctx = b.Title
		}
		rows = append(rows, inboxMentionRow{
			Source:       "agent",
			ID:           b.ID,
			Ago:          fmtTimeAgo(b.UpdatedAt, now),
			Actor:        spawner,
			Context:      snippet(ctx, inboxMentionContextLimit),
			Href:         basePath + "/" + b.ID,
			UpdatedAtRFC: rfcTime(b.UpdatedAt),
			ActivityAt:   b.UpdatedAt,
		})
	}
	for _, b := range activeAgentBeads {
		addAgent(b)
	}
	for _, b := range recentAgentBeads {
		addAgent(b)
	}

	for _, b := range decisionBeads {
		if b == nil {
			continue
		}
		actor := strings.TrimSpace(b.Fields["requested_by"])
		if actor == "" {
			actor = strings.TrimSpace(b.CreatedBy)
		}
		if actor == "" {
			// A decision with no identifiable requester is not a
			// @-mention of the operator in any useful sense — skip.
			continue
		}
		ctx := strings.TrimSpace(b.Fields["prompt"])
		if ctx == "" {
			ctx = b.Title
		}
		// Decisions sort by created_at — the activity signal the
		// operator wants is "when did this land in my queue", not
		// "when was the option list last edited". This mirrors the
		// inboxStreamRowFromDecision rule in inbox_stream.go.
		activity := b.CreatedAt
		if activity.IsZero() {
			activity = b.UpdatedAt
		}
		rows = append(rows, inboxMentionRow{
			Source:       "decision",
			ID:           b.ID,
			Ago:          fmtTimeAgo(activity, now),
			Actor:        actor,
			Context:      snippet(ctx, inboxMentionContextLimit),
			Href:         basePath + "/" + b.ID,
			UpdatedAtRFC: rfcTime(activity),
			ActivityAt:   activity,
		})
	}

	dedupeMentionRows(&rows)
	sortInboxMentions(rows)
	if len(rows) > inboxRecentMentionsLimit {
		rows = rows[:inboxRecentMentionsLimit]
	}
	return rows
}

// dedupeMentionRows collapses rows with the same (Source, ID) tuple.
// Active and recent agent rows can overlap when the daemon races a
// status flip; the dedupe is cheap and keeps the section honest.
func dedupeMentionRows(rows *[]inboxMentionRow) {
	if len(*rows) < 2 {
		return
	}
	seen := make(map[string]struct{}, len(*rows))
	out := (*rows)[:0]
	for _, r := range *rows {
		key := r.Source + "|" + r.ID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	*rows = out
}

// sortInboxMentions orders rows newest-activity first; ID is the
// stable tiebreaker so the test surface stays deterministic.
func sortInboxMentions(rows []inboxMentionRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.ActivityAt.Equal(b.ActivityAt) {
			return a.ActivityAt.After(b.ActivityAt)
		}
		return a.ID < b.ID
	})
}
