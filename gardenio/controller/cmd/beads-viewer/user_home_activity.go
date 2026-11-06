package main

// Merged, reverse-chronological activity spine for /u/{slug} (bug
// ka-0000002041). The pre-existing homepage rendered ELEVEN disjoint
// section cards, each with its own independently-sorted dated list, so an
// operator's activity could not be read as one chronological stream. The
// Users-FLAT time-series audit scored that 3/10 (unified_timeline_present=0,
// disjoint_section_cards=4).
//
// This file collapses those populations into ONE merged stream:
//
//   - Eager bead populations already fetched by buildUserHomeSections
//     (open decisions, active agents, open MRs, merged MRs, closed work,
//     their automations, their gardeners, pinned) are flattened into events.
//   - The three formerly-lazy populations (forwarded mentions, recent
//     comments, and the bidirectional message inbox) are fetched eagerly
//     here so they interleave into the same stream rather than living in
//     separate lazy cards.
//
// Every event carries its true event time.Time, so the stream sorts strictly
// newest-first and each rendered row emits BOTH a relative `.meta` age (for
// at-a-glance ordering) AND an absolute ISO `title=` timestamp (for exact
// ordering on hover). Date rails (Today / This week / Earlier) are woven into
// the item list as non-row markers. The former section cards survive only as
// client-side facet chips over this one stream.

import (
	"context"
	"html/template"
	"sort"
	"sync"
	"time"
)

// activityFacet slugs — the facet a stream row belongs to, used by the
// client-side chip filter (data-facet on each row). Most are coarser than the
// old section slugs (e.g. every closed non-MR bead reads as "closed"). The one
// deliberate SPLIT is merged vs open MRs: they carry distinct slugs so the
// "Merged MRs" and "Open MRs" summary tiles each deep-link to the rows they
// actually count. Folding them into one "mrs" facet made both tiles filter to
// the identical row set, so "Merged MRs 0" clicked through to open drafts —
// bug ka-0000008262.
const (
	facetDecisions   = "decisions"
	facetAgents      = "agents"
	facetMRsMerged   = "mrs-merged"
	facetMRsOpen     = "mrs-open"
	facetClosed      = "closed"
	facetComments    = "comments"
	facetForwarded   = "forwarded"
	facetAutomations = "automations"
	facetGardeners   = "gardeners"
	facetPlans       = "plans"
	facetPinned      = "pinned"
	facetMessages    = "messages"
)

// activityFacetOrder is the render order of the facet chip strip. Only facets
// with a non-zero count render a chip.
var activityFacetOrder = []struct{ Slug, Label string }{
	{facetDecisions, "Decisions"},
	{facetMessages, "Messages"},
	{facetMRsMerged, "Merged MRs"},
	{facetMRsOpen, "Open MRs"},
	{facetClosed, "Closed work"},
	{facetComments, "Comments"},
	{facetAgents, "Agents"},
	{facetForwarded, "Forwarded"},
	{facetAutomations, "Automations"},
	{facetGardeners, "Gardeners"},
	{facetPlans, "Plans"},
	{facetPinned, "Pinned"},
}

// Date-rail bucket ids down the merged stream. Newest bucket first so the
// stream reads Today → This week → Earlier top-to-bottom.
const (
	railToday   = "Today"
	railWeek    = "This week"
	railEarlier = "Earlier"
)

// activityEvent is one row in the merged stream. Bead events (decisions, MRs,
// closed work, …) render id/type/status/title; message events render the
// author + linkified body. Both carry the relative age + absolute ISO stamp.
type activityEvent struct {
	ID       string
	Title    string
	Type     string
	Status   string
	Priority int
	Project  string
	// Facet is the coarse category slug driving the chip filter (data-facet).
	Facet string
	// FacetLabel is the short human label shown as a per-row pill.
	FacetLabel string
	Href       string

	// IsMessage switches the row template between the bead shape and the
	// chat-message shape. Message rows carry Author/Kind/BodyHTML.
	IsMessage bool
	Author    string
	Kind      string
	BodyHTML  template.HTML

	// Timestamp is the true event time; the stream sorts on it. Ago is the
	// relative token (`.meta`), TimestampRFC the absolute ISO `title=` value.
	Timestamp    time.Time
	Ago          string
	TimestampRFC string

	// Created is the bead's creation time, surfaced alongside the last-activity
	// Timestamp (ka-0000007477). CreatedAgo is the relative token shown as a
	// secondary "created …" annotation, CreatedRFC the absolute ISO value woven
	// into the hover tooltip. Both are empty when creation time is unknown or is
	// effectively the same instant as the last-activity time (see
	// beadRowToEvent), so untouched-since-creation rows don't render a redundant
	// "created = updated" line.
	CreatedAgo string
	CreatedRFC string
}

// activityStreamItem is one entry in the interleaved render list: either a
// date rail (IsRail) or an event.
type activityStreamItem struct {
	IsRail    bool
	RailLabel string
	Event     activityEvent
}

// activityFacetChip is one entry in the facet chip strip.
type activityFacetChip struct {
	Slug  string
	Label string
	Count int
}

// activityView is the merged-stream view-model handed to user_home.html.
type activityView struct {
	Items  []activityStreamItem
	Facets []activityFacetChip
	Total  int
	// WeekCount is the number of stream ROWS (not rails) whose timestamp falls in
	// the trailing week — the Today + This week date-rail buckets. The narrow
	// (480px) spine seeds its initial lazy-append reveal at this count (floored +
	// capped in the template JS), so a typical week reads top-to-bottom with NO
	// "Show all" click (feature ka-0000003056), while the older "Earlier" tail —
	// the deep-history rows that make a rich record's spine 85 entries tall, the
	// original ka-0000003169 mega-scroll source — still collapses and appends on
	// scroll (ka-0000000142). Desktop is unaffected (collapse is CSS-gated to
	// <1100px), and every measure-flat / measure-users-camp signal greps the
	// serialized markup where all Total rows are always present.
	WeekCount int
}

// buildUserHomeActivity merges every homepage population into one reverse-
// chronological stream. It reuses the already-fetched eager `sections`
// (buildUserHomeSections) for the bead populations and eagerly fetches the
// three formerly-lazy populations (forwarded / recent comments / inbox) so
// they interleave into the same stream. Pure assembly + rail insertion lives
// in assembleActivityStream so it unit-tests without a daemon.
func (s *Server) buildUserHomeActivity(
	ctx context.Context,
	operatorID int64,
	slug string,
	isViewerSelf bool,
	sections []userHomeSection,
) activityView {
	now := s.now()

	// The three formerly-lazy populations, fetched in parallel so eager-izing
	// them for the merged stream doesn't serialize three round-trips.
	var (
		forwarded userHomeSection
		comments  userHomeSection
		inbox     userInboxView
		wg        sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		forwarded = s.buildForwardedSection(ctx, operatorID, slug)
	}()
	go func() {
		defer wg.Done()
		comments = s.buildRecentCommentsSection(ctx, operatorID, slug)
	}()
	go func() {
		defer wg.Done()
		inbox = s.buildUserInbox(ctx, operatorID, slug, isViewerSelf)
	}()
	wg.Wait()

	// Facet + label for each eager section slug. Merged and open MRs carry
	// DISTINCT facets (mrs-merged / mrs-open) so their summary tiles filter to
	// disjoint row sets — folding them into one "mrs" facet is bug ka-0000008262.
	facetForSection := map[string]struct{ slug, label string }{
		sectionOpenDecisions:     {facetDecisions, "decision"},
		sectionActiveAgents:      {facetAgents, "agent"},
		sectionOpenMRs:           {facetMRsOpen, "open MR"},
		sectionRecentCommits:     {facetMRsMerged, "merged MR"},
		sectionRecentMerged:      {facetClosed, "closed"},
		sectionTheirAutomations:  {facetAutomations, "automation"},
		sectionTheirGardeners:    {facetGardeners, "gardener"},
		sectionTheirPlans:        {facetPlans, "plan"},
		sectionPinned:            {facetPinned, "pinned"},
		sectionForwardedMentions: {facetForwarded, "forwarded"},
		sectionRecentComments:    {facetComments, "comment"},
	}
	bySlug := make(map[string]userHomeSection, len(sections))
	for _, sec := range sections {
		bySlug[sec.Slug] = sec
	}

	// Population precedence for dedup: a bead that matches two populations
	// (e.g. a touched-since-creation "comment" that is also an open decision)
	// is rendered once, under the higher-precedence facet. Bead facets win
	// over the generic comments/forwarded/pinned buckets.
	var events []activityEvent
	appendSection := func(sec userHomeSection) {
		fs, ok := facetForSection[sec.Slug]
		if !ok {
			return
		}
		for _, row := range sec.Beads {
			ev := beadRowToEvent(row, fs.slug, fs.label, now)
			if ev.Timestamp.IsZero() {
				continue // un-timestamped rows can't be placed in the stream
			}
			events = append(events, ev)
		}
	}
	// Eager bead populations in precedence order.
	for _, slugID := range []string{
		sectionOpenDecisions,
		sectionActiveAgents,
		sectionOpenMRs,
		sectionRecentCommits,
		sectionRecentMerged,
		sectionTheirAutomations,
		sectionTheirGardeners,
		sectionTheirPlans,
	} {
		appendSection(bySlug[slugID])
	}
	// Messages interleave next (type=message beads never collide with the
	// bead facets above).
	events = append(events, messagesToEvents(inbox, s.basePath)...)
	// Then the lower-precedence buckets.
	appendSection(comments)
	appendSection(forwarded)
	appendSection(bySlug[sectionPinned])

	return assembleActivityStream(events, now)
}

// beadRowToEvent projects a userHomeBeadRow (already carrying its UpdatedAt +
// relative Ago) into a stream event under the given facet.
func beadRowToEvent(row userHomeBeadRow, facet, label string, now time.Time) activityEvent {
	ev := activityEvent{
		ID:         row.ID,
		Title:      row.Title,
		Type:       row.Type,
		Status:     row.Status,
		Priority:   row.Priority,
		Project:    row.Project,
		Facet:      facet,
		FacetLabel: label,
		Href:       row.Href,
		Timestamp:  row.UpdatedAt,
	}
	if !row.UpdatedAt.IsZero() {
		ev.Ago = fmtTimeAgo(row.UpdatedAt, now)
		ev.TimestampRFC = row.UpdatedAt.UTC().Format(time.RFC3339)
	}
	// Surface created time (ka-0000007477) — but only when it is meaningfully
	// distinct from the last-activity time, so untouched-since-creation rows
	// (created == updated) don't render a redundant "created = updated" line.
	// "Meaningfully distinct" = the two stamps land in different one-minute
	// buckets, which is also the resolution fmtTimeAgo renders at the low end.
	if !row.CreatedAt.IsZero() && createdDistinctFromUpdated(row.CreatedAt, row.UpdatedAt) {
		ev.CreatedAgo = fmtTimeAgo(row.CreatedAt, now)
		ev.CreatedRFC = row.CreatedAt.UTC().Format(time.RFC3339)
	}
	return ev
}

// createdDistinctFromUpdated reports whether a bead's created time is far
// enough from its updated time to be worth showing separately. Rows never
// touched since creation carry updated == created (or a sub-minute delta from
// the write path), and rendering "created 3d ago · updated 3d ago" on those is
// pure noise. A zero updated time means we only have created, which is itself
// distinct enough to show.
func createdDistinctFromUpdated(created, updated time.Time) bool {
	if updated.IsZero() {
		return true
	}
	d := updated.Sub(created)
	if d < 0 {
		d = -d
	}
	return d >= time.Minute
}

// messagesToEvents projects the bidirectional inbox chat log into message
// stream events. Each chatMessage already carries its linkified body, author,
// kind badge, relative Ago, and absolute TimestampRFC (set by
// projectChatMessage), so we parse the RFC stamp back to a time.Time for the
// merge sort and reuse the rest verbatim.
func messagesToEvents(inbox userInboxView, basePath string) []activityEvent {
	if !inbox.HasMessages {
		return nil
	}
	out := make([]activityEvent, 0, len(inbox.Chat.Messages))
	for _, m := range inbox.Chat.Messages {
		ts := parseRFC3339OrZero(m.TimestampRFC)
		if ts.IsZero() {
			continue
		}
		href := m.Href
		if href == "" && m.ID != "" {
			href = basePath + "/" + m.ID
		}
		out = append(out, activityEvent{
			ID:           m.ID,
			Facet:        facetMessages,
			FacetLabel:   "message",
			Href:         href,
			IsMessage:    true,
			Author:       m.Author,
			Kind:         m.Kind,
			BodyHTML:     m.BodyHTML,
			Timestamp:    ts,
			Ago:          m.Ago,
			TimestampRFC: m.TimestampRFC,
		})
	}
	return out
}

// parseRFC3339OrZero parses an RFC3339 timestamp, returning the zero time on
// any error (the caller drops zero-time events from the ordered stream).
func parseRFC3339OrZero(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// assembleActivityStream dedups by bead id (first-seen wins, honoring the
// caller's precedence-ordered input), sorts strictly newest-first, weaves in
// the Today / This week / Earlier date rails, and tallies per-facet counts.
// Pure → unit-testable without a daemon.
//
// Sorting strictly on the true timestamp guarantees the rendered relative
// ages are monotonically non-decreasing down the DOM (fmtTimeAgo is monotone
// in age), which is exactly what the time-series audit measures.
func assembleActivityStream(events []activityEvent, now time.Time) activityView {
	seen := make(map[string]struct{}, len(events))
	deduped := make([]activityEvent, 0, len(events))
	counts := make(map[string]int)
	for _, ev := range events {
		if ev.Timestamp.IsZero() {
			continue
		}
		if ev.ID != "" {
			if _, dup := seen[ev.ID]; dup {
				continue
			}
			seen[ev.ID] = struct{}{}
		}
		deduped = append(deduped, ev)
		counts[ev.Facet]++
	}

	// Newest first. Tie-break on id so the order is deterministic for tests.
	sort.SliceStable(deduped, func(i, j int) bool {
		if !deduped[i].Timestamp.Equal(deduped[j].Timestamp) {
			return deduped[i].Timestamp.After(deduped[j].Timestamp)
		}
		return deduped[i].ID < deduped[j].ID
	})

	items := make([]activityStreamItem, 0, len(deduped)+3)
	lastRail := ""
	weekCount := 0
	for _, ev := range deduped {
		rail := railBucket(ev.Timestamp, now)
		if rail != lastRail {
			items = append(items, activityStreamItem{IsRail: true, RailLabel: rail})
			lastRail = rail
		}
		// Rows in the trailing week (Today + This week) seed the narrow spine's
		// initial reveal so a typical week reads without a "Show all" click
		// (feature ka-0000003056). Deduped is sorted newest-first, so these are a
		// contiguous prefix; only the older Earlier tail lazy-appends on scroll.
		if rail == railToday || rail == railWeek {
			weekCount++
		}
		items = append(items, activityStreamItem{Event: ev})
	}

	facets := make([]activityFacetChip, 0, len(activityFacetOrder))
	for _, f := range activityFacetOrder {
		if c := counts[f.Slug]; c > 0 {
			facets = append(facets, activityFacetChip{Slug: f.Slug, Label: f.Label, Count: c})
		}
	}

	return activityView{Items: items, Facets: facets, Total: len(deduped), WeekCount: weekCount}
}

// railBucket classifies an event timestamp into its date rail relative to now.
// Buckets are calendar-anchored in UTC: Today = same UTC date; This week =
// within the trailing 7 days but not today; Earlier = older. A future
// timestamp (clock skew) is treated as Today.
func railBucket(ts, now time.Time) string {
	tu, nu := ts.UTC(), now.UTC()
	ty, tm, td := tu.Date()
	ny, nm, nd := nu.Date()
	if ty == ny && tm == nm && td == nd {
		return railToday
	}
	if tu.After(nu) {
		return railToday
	}
	if nu.Sub(tu) < 7*24*time.Hour {
		return railWeek
	}
	return railEarlier
}
