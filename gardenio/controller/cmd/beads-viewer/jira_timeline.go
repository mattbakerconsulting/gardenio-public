package main

// Bead-show — Jira conversation timeline (design-doc F-C, epic ka-0000007401).
//
// The Jira conversational bridge already threads a full two-way exchange
// through the bead graph:
//
//   - an inbound `@gardenio` mention spawns a type=task mention bead, and the
//     bridge stamps a `triggered-by` edge from that task to the ticket's
//     `jira:<KEY>` mirror bead (writeMirrorTriggeredBy, jira_mentions.go);
//   - each outbound `gb jira reply` records a type=message bead (label
//     `jira-reply`) and stamps the SAME `triggered-by` edge back to the mirror
//     (linkJiraReplyToMirror, cmd/gb/jira.go, ka-0000004978).
//
// So the mirror bead is the hub of the conversation — but beads-viewer rendered
// a `source:jira` mirror as a bare generic field-dump. The exchange existed in
// the graph only as constellation node disks, with no single place that reads
// the conversation in order (mention → spawn → replies → follow-ups).
//
// This file closes that gap: a pure projection from the mirror's reverse
// `triggered-by` edges (the rows GetReverseDependencies already returns and the
// handler already fetches) onto a compact, read-only timeline. It touches no
// daemon and no http — it operates on the reverse-dep rows plus the hydrated
// BeadDetail map the caller supplies — so it unit-tests in isolation, mirroring
// the buildBeadChat / maps_chat precedent.

import (
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// jiraSourceLabel marks a Jira-mirror bead: the poller / gitlab-bridge stamp it
// alongside the canonical `jira:<KEY>` label. The timeline panel renders ONLY
// for a bead carrying it, so an ordinary task that happens to have inbound
// triggered-by edges never sprouts a Jira panel.
const jiraSourceLabel = "source:jira"

// jiraReplyLabel marks an outbound `gb jira reply` message bead (see
// recordJiraReplyBead in cmd/gb/jira.go). Distinguishes an agent's reply row
// from an inbound mention-task row on the timeline.
const jiraReplyLabel = "jira-reply"

// jiraTimelineMaxEntries caps the rendered timeline. A busy ticket can
// accumulate many mention/reply beads; keep the newest window so the panel
// stays legible and the page stays bounded, and report the dropped older count.
const jiraTimelineMaxEntries = 40

// jiraTimelineKind classifies one timeline row for its badge + styling.
type jiraTimelineKind string

const (
	// jiraKindMention is an inbound `@gardenio` mention that spawned an agent
	// (a type=task mention bead linked to the mirror).
	jiraKindMention jiraTimelineKind = "mention"
	// jiraKindReply is an outbound agent reply posted back to the ticket
	// (a type=message bead labelled jira-reply).
	jiraKindReply jiraTimelineKind = "reply"
	// jiraKindLinked is any other bead linked to the mirror by a triggered-by
	// edge — rendered generically so the timeline never silently drops a row.
	jiraKindLinked jiraTimelineKind = "linked"
)

// jiraTimelineEntry is one rendered row: a mention, a reply, or a generic
// linked bead, projected to a uniform shape the template renders.
type jiraTimelineEntry struct {
	ID     string
	Href   string
	Kind   jiraTimelineKind
	Badge  string // human-facing badge text ("mention" | "reply" | "linked")
	Author string // agent / operator handle, best-effort
	// Visibility is the reply's Jira comment visibility ("internal" / "public"),
	// empty for non-reply rows. Surfaced so an operator can see at a glance
	// whether a reply was customer-visible.
	Visibility string
	// Status is the linked bead's lifecycle status (open/in_progress/closed) —
	// meaningful mainly for a mention task so an operator sees whether the
	// spawned agent is still working.
	Status       string
	Summary      string // one-line body/title text, trimmed
	Ago          string
	TimestampRFC string
}

// jiraTimelineView is the panel view-model. HasEntries drives the {{with}} in
// the template so an empty timeline renders nothing at all.
type jiraTimelineView struct {
	Entries     []jiraTimelineEntry
	Shown       int
	Total       int
	OlderHidden bool
	HiddenCount int
	HasEntries  bool
}

// isJiraMirrorBead reports whether a hydrated bead is a Jira-mirror bead — the
// hub the timeline hangs off. A nil bead is never a mirror.
func isJiraMirrorBead(b *beadsapi.BeadDetail) bool {
	return hasLabel(b, jiraSourceLabel)
}

// collectJiraTimelineIDs pulls the de-duplicated ids of every bead linked to
// the mirror by a reverse `triggered-by` edge — the mention tasks and reply
// messages that make up the conversation. selfID is never collected. The
// caller hydrates the returned ids (BatchGetBeads) and hands them plus the
// reverse-dep rows to buildJiraTimeline.
func collectJiraTimelineIDs(selfID string, revDeps []beadsapi.Dependency) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0, len(revDeps))
	for _, d := range revDeps {
		if !strings.EqualFold(d.Type, "triggered-by") {
			continue
		}
		id := d.BeadID // reverse dep: "BeadID depends on us (the mirror)"
		if id == "" || id == selfID {
			continue
		}
		if _, dupe := seen[id]; dupe {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// buildJiraTimeline projects the hydrated linked beads into the timeline
// view-model. ids is the set from collectJiraTimelineIDs; beads is the hydrated
// BeadDetail map. Rows are ordered oldest→newest by created-else-updated time
// (a conversation reads top-to-bottom) and capped to the newest
// jiraTimelineMaxEntries, reporting the dropped older count. Pure →
// unit-testable.
func buildJiraTimeline(
	ids []string,
	beads map[string]*beadsapi.BeadDetail,
	basePath string,
	now time.Time,
) jiraTimelineView {
	if len(ids) == 0 {
		return jiraTimelineView{}
	}

	type stamped struct {
		ts    time.Time
		entry jiraTimelineEntry
	}
	rows := make([]stamped, 0, len(ids))
	for _, id := range ids {
		b := beads[id]
		if b == nil || b.ID == "" {
			continue // un-hydrated — skip rather than render an empty row
		}
		ts, ok := pickAgeTimestamp(b.CreatedAt, b.UpdatedAt)
		if !ok {
			ts = time.Time{} // sorts to the front; still rendered
		}
		rows = append(rows, stamped{ts: ts, entry: projectJiraTimelineEntry(b, basePath, now)})
	}
	if len(rows) == 0 {
		return jiraTimelineView{}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ts.Equal(rows[j].ts) {
			return rows[i].ts.Before(rows[j].ts)
		}
		return rows[i].entry.ID < rows[j].entry.ID // stable tiebreak for golden tests
	})

	total := len(rows)
	hidden := 0
	// Keep the NEWEST window (the tail of the ascending list) so the live end of
	// the conversation always survives the cap; the dropped head is older history.
	if total > jiraTimelineMaxEntries {
		hidden = total - jiraTimelineMaxEntries
		rows = rows[total-jiraTimelineMaxEntries:]
	}

	entries := make([]jiraTimelineEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, r.entry)
	}
	return jiraTimelineView{
		Entries:     entries,
		Shown:       len(entries),
		Total:       total,
		OlderHidden: hidden > 0,
		HiddenCount: hidden,
		HasEntries:  true,
	}
}

// projectJiraTimelineEntry classifies and projects one linked bead onto a
// timeline row. A jira-reply message is a reply row (carrying its visibility);
// a task is a mention row; anything else is a generic linked row so a stray
// edge is surfaced rather than dropped.
func projectJiraTimelineEntry(b *beadsapi.BeadDetail, basePath string, now time.Time) jiraTimelineEntry {
	kind := classifyJiraTimelineBead(b)
	entry := jiraTimelineEntry{
		ID:      b.ID,
		Href:    basePath + "/" + b.ID,
		Kind:    kind,
		Badge:   string(kind),
		Author:  messageAuthor(b),
		Status:  b.Status,
		Summary: jiraTimelineSummary(b),
	}
	if kind == jiraKindReply && b.Fields != nil {
		entry.Visibility = strings.TrimSpace(b.Fields["visibility"])
	}
	if ts, ok := pickAgeTimestamp(b.CreatedAt, b.UpdatedAt); ok {
		entry.Ago = fmtTimeAgo(ts, now)
		entry.TimestampRFC = ts.UTC().Format(time.RFC3339)
	}
	return entry
}

// classifyJiraTimelineBead maps a linked bead to its timeline kind. The
// jira-reply label is checked first (an outbound reply is recorded as a
// type=message with that label); a task is an inbound mention; everything else
// is generic.
func classifyJiraTimelineBead(b *beadsapi.BeadDetail) jiraTimelineKind {
	if b == nil {
		return jiraKindLinked
	}
	if hasLabel(b, jiraReplyLabel) {
		return jiraKindReply
	}
	if strings.EqualFold(b.Type, "task") {
		return jiraKindMention
	}
	return jiraKindLinked
}

// jiraTimelineSummary returns a one-line body for a timeline row: the message
// text (for a reply) via the shared messageBodyText extractor, else the bead
// title. Trimmed to a single collapsed line and length-bounded so a long reply
// never blows out the panel.
func jiraTimelineSummary(b *beadsapi.BeadDetail) string {
	s := messageBodyText(b)
	if s == "" {
		s = strings.TrimSpace(b.Title)
	}
	s = strings.Join(strings.Fields(s), " ")
	const max = 240
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}
