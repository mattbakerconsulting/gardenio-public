package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// Phase 2 — net-new interactive /threads view (epic ka-0000000679, task
// ka-0000003406). This file is the DATA / AGGREGATOR layer only: pure
// aggregator functions plus their store queries. It registers NO HTTP routes
// (that is task 2-3, ka-0000003348), builds no composer (2-2), and renders no
// templates (2-4). Keeping it route-free lets 2-3 wire handlers on top.
//
// Conceptual spine (design bundle ka-0000005451): a "thread" is the user's
// WISH — the WHY. An epic is a slice of an agent's PLAN — the HOW. One wish
// spans many epics. A thread therefore aggregates work across many beads, not
// just one parent-child subtree.
//
// Data model (source of truth: internal/bridge/thread_bead.go):
//
//   - The thread (wish) HEAD is the bead carrying the bare `thread` kind label
//     (threadHeadLabel). It is minted regardless of origin — Slack mention,
//     agent/manual creation, or the future viewer "+ new wish" — so a single
//     label enumerates every wish head. 1a-1 (MR !4823) added that label +
//     the threadIdentityLabel helper.
//   - The head carries NO thread_id field: it IS the wish, so it does not
//     point at itself.
//   - DERIVATIVES (member tasks, messages, agents) carry fields.thread_id ==
//     the head's ka-id (the origin-agnostic "wish pointer"). 1a-2 (MR !4822)
//     added migration 037's partial index on fields->>'thread_id', so the
//     field query below is index-backed.
//
// Membership of a thread = UNION(field thread_id, parent-child closure). The
// two halves exist because 1b-3 (ka-0000002099, MR !4826) writes the durable
// parent-child dependency BEFORE it copies the thread_id field, so a bead can
// briefly carry the dep without the field (a "half-link"). The parent-child
// closure is therefore the authoritative membership signal and the field is
// the fast-path augmentation — see threadMembers for how that "dep-first"
// ordering is honored.

// threadHeadLabel is the bare, origin-agnostic kind label stamped on every
// thread (wish) head. It mirrors bridge.threadKindLabel
// (internal/bridge/thread_bead.go); the viewer keeps its own copy because the
// bridge constant is unexported and the two packages do not depend on each
// other.
const threadHeadLabel = "thread"

// threadHeadPriority is the priority stamped on a viewer-minted wish head. Like
// the label above it mirrors the bridge's threadBeadPriority
// (internal/bridge/thread_bead.go) — priority is a per-mint-site decision, not
// a cross-binary protocol value, so the viewer keeps its own copy rather than
// coupling to an exported symbol. It MUST be set explicitly: an omitted
// beadsapi.CreateBeadRequest.Priority (no omitempty) marshals as an explicit
// "priority":0 == P0, the top band, which is what let Slack-conversation heads
// sort above every real bug in `gb ready` and starve the work queue
// (ka-0000003760). P2 matches `kd create`'s CLI default and the bridge mint, so
// a wish head minted from the viewer "+ new wish" agrees with a bridge-minted
// one. The bridge mint site was fixed first; this closes the second, sibling
// path the initial fix did not touch.
const threadHeadPriority = 2

// threadIDField is the wish-pointer field carried by thread derivatives,
// holding the head's ka-id. Mirrors the field written by bridge thread
// machinery and inherited by spawn.go's slackFieldsToInherit.
const threadIDField = "thread_id"

// threadRootTextField is the optional field on a Slack-origin head holding the
// verbatim text of the thread's opening message — the clearest rendering of
// the user's wish. Falls back to the head title when absent.
const threadRootTextField = "slack_root_text"

// threadSlackChannelField / threadSlackTSField are the (channel, thread_ts)
// binding fields a Slack-origin wish head carries (bridge thread_bead.go
// stamps them at mint time). They are the recoverable route back to the
// originating Slack thread: the composer WRITE path (thread_compose.go) reads
// them off the head to address the bridge-mediated send, and copies them onto
// each message bead so the slack-bridge forwarder can route without an agent.
const (
	threadSlackChannelField = "slack_thread_channel"
	threadSlackTSField      = "slack_thread_ts"
)

// Thread enumeration / membership queries apply NO status filter: membership
// is lifecycle-agnostic. A thread is the user's wish and its members span every
// status — a deferred or blocked member, and a closed head whose wish was
// granted, are all still part of the thread's history. Beads carry more
// lifecycle statuses than the open/in_progress/closed trio (also blocked,
// deferred, partial — see beadsapi.allTaskStatuses and kbeads model.Status), so
// passing a fixed subset would silently drop heads/members in the omitted
// states. The daemon treats an empty Statuses list as "all statuses" (no
// status clause; kbeads queries.go), which is exactly what threads want, and it
// keeps the field half consistent with the closure half (BatchGetDependencies
// applies no status filter either). The route layer (2-3) can narrow by status
// at render time if it wants an "active wishes" lens.

// threadMemberQueryLimit caps the thread_id field-equality membership queries.
// It is set to the daemon's maximum single-request page size (kbeads
// maxListBeadsLimit = 10000) rather than left at 0, because an omitted limit
// silently defaults to the daemon's 1000-row cap — which would truncate the
// membership of a large thread and break threadMembers' "exact union" contract.
// A thread with more than 10000 field-bound derivatives is pathological and
// would need pagination (out of scope for this aggregator layer).
const threadMemberQueryLimit = 10000

// maxThreadClosureDepth bounds the parent-child closure BFS in threadMembers.
// Thread member subtrees are shallow in practice (a wish, its member tasks,
// their epics and spawned agents); 64 mirrors beadsapi.maxRollupDepth and still
// terminates in a bounded number of round-trips on a degenerate or cyclic
// graph (the visited set guards cycles; this caps pathological depth).
const maxThreadClosureDepth = 64

// --- Narrow store interfaces (accept interfaces, return structs) ----------
//
// The aggregators below take the smallest daemon surface each one needs, so
// they are trivially testable with a purpose-built fake and never pull in the
// full beadAPI. *Server.daemon (beadAPI) satisfies all three structurally.

// threadLister is the store surface threadList needs: a single label-filtered
// enumeration plus per-head field-membership queries, PLUS the batched forward-
// dependency walk and bead fetch that resolve each thread's produced MRs
// (ka-0000000064 — index cards now surface associated MRs). The two batch
// methods are already on threadMemberStore/threadMRStore and satisfied
// structurally by *Server.daemon; threadFake implements all four in tests.
type threadLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// threadMemberStore is the store surface threadMembers needs: the field
// query (the fast-path half) plus the batched reverse-dependency walk (the
// authoritative parent-child closure half).
type threadMemberStore interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
}

// threadTimelineStore is the store surface fuseThreadTimeline needs: membership
// resolution (threadMemberStore) plus the batched bead fetch and per-member
// comment fetch that supply the fused entries.
type threadTimelineStore interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error)
}

// --- Thread-list aggregator ------------------------------------------------

// threadSummary is one row of the /threads index. It carries exactly the
// fields the index will need to render and sort a list of wishes.
type threadSummary struct {
	// ThreadID is the ka-id of the thread (wish) head.
	ThreadID string
	// Title is the head bead's title.
	Title string
	// WishText is the user's wish as text — the head's slack_root_text field
	// when present, otherwise the title. This is the human-facing "what was
	// asked for" line.
	WishText string
	// Status is the head bead's lifecycle status.
	Status string
	// Project is the head's project (the project:<name> label with the prefix
	// stripped), or "" when unlabeled.
	Project string
	// MemberCount is the number of beads bound to this thread by the
	// thread_id field, PLUS the head itself (which carries no thread_id). It
	// is the indexed field fast-path count: half-links (a parent-child dep
	// written by 1b-3 before its thread_id field landed) are a sub-second
	// window and are not reflected here. The detail view's threadMembers
	// computes the exact field∪closure union.
	MemberCount int
	// LastActivity is the most recent updated_at across the head and its
	// field-bound members — the sort key for "what changed most recently".
	LastActivity time.Time
	// CreatedAt is the head bead's creation time.
	CreatedAt time.Time
	// Starter is the person who opened the wish — the head bead's resolved human
	// operator (ka-0000000064). Zero-valued (Display=="") when the head carries
	// no resolvable human operator, so the template omits the "started by" line.
	Starter threadParticipant
	// Participants are the distinct humans who took part in the thread — the head
	// opener plus every member bead's human operator — most-recently-active
	// first, deduped by identity. Drives the row of avatar initials. Includes the
	// Starter. Capped at threadParticipantCap; Overflow carries the remainder.
	Participants []threadParticipant
	// ParticipantOverflow is the count of distinct participants beyond the capped
	// Participants slice, rendered as a "+N" avatar so a busy thread still shows
	// its full headcount without an unbounded avatar row.
	ParticipantOverflow int
	// LastMessage is the most recent conversation utterance in the thread — the
	// latest type=message member's text + author + timestamp (ka-0000000064). Zero-
	// valued (Text=="") when the thread has no message members yet, so the
	// template falls back to the wish text alone.
	LastMessage threadLastMessage
	// ProducedMRs are the merge requests the thread's members produced (via
	// forward `produces` edges), projected onto the shared mr_pill shape. Empty
	// when the thread shipped no code. A walk failure degrades to nil (the
	// section is simply omitted) rather than failing the whole index.
	ProducedMRs []wrapUpMRView
	// Herder is the herder's last verdict on the open wish, shown as a tag on
	// the row (threads_herder.go); zero before its first pass.
	Herder threadHerder
}

// threadParticipant is one human who took part in a thread — rendered as an
// avatar (initials) on the index card and, on hover, their full display name.
// It is a thin projection of the shared operatorChip so the initials, display,
// and /u/{slug} link match every other operator surface exactly.
type threadParticipant struct {
	// RawID is the operator id (slack:U… / oidc:<email>) — the identity key and
	// the value the (future) filter link would carry.
	RawID string
	// Display is the resolved human label (bot-name re-attribution + email
	// humanization via resolveOperatorChip); falls back to the raw id.
	Display string
	// Initials is the 1–3 char monogram for the avatar bubble (operatorInitials).
	Initials string
	// Slug, when non-empty, links the avatar to the operator's /u/{slug} homepage.
	Slug string
}

// threadLastMessage is the most recent conversation utterance in a thread,
// surfaced on the index card so the row previews where the conversation stands
// (ka-0000000064) rather than only the opening wish.
type threadLastMessage struct {
	// Text is the message body (threadMessageText of the latest type=message
	// member). Empty when the thread has no message members.
	Text string
	// Author is the resolved human label of who sent it (same resolution as a
	// participant), falling back to the raw operator/creator id.
	Author string
	// At is when it was sent (the message bead's created_at).
	At time.Time
}

// HasStarter reports whether a resolvable thread starter is known — a template
// convenience so the "started by" line is a single {{if}}.
func (s threadSummary) HasStarter() bool { return s.Starter.Display != "" }

// HasParticipants reports whether any participant avatars are present.
func (s threadSummary) HasParticipants() bool { return len(s.Participants) > 0 }

// HasLastMessage reports whether a last-message preview is present.
func (s threadSummary) HasLastMessage() bool { return s.LastMessage.Text != "" }

// HasProducedMRs reports whether any produced MRs are present, so the card
// renders the pill row only when there is code to show.
func (s threadSummary) HasProducedMRs() bool { return len(s.ProducedMRs) > 0 }

// ProducedMRCount is the number of produced MRs — a template convenience.
func (s threadSummary) ProducedMRCount() int { return len(s.ProducedMRs) }

// threadParticipantCap bounds the avatar row on an index card. Past a handful
// the row stops being scannable; the overflow "+N" avatar carries the rest.
const threadParticipantCap = 5

// threadList enumerates every thread (wish) head and returns a summary per
// thread, sorted by LastActivity descending (most recently active first),
// tie-broken by ThreadID for determinism.
//
// When project is non-empty the enumeration is scoped to that project's
// project:<name> label; an empty project lists threads across the fleet.
//
// Cost: one label-filtered enumeration for the heads, then one indexed
// field-equality query per head for its member rows (used for the member tally,
// the last-activity max, the participant/starter avatars, and the last-message
// preview — all off that single round-trip). The per-head query is the
// migration-037 fast-path; the index page deliberately does NOT compute the full
// parent-child closure per thread (that is the detail view's job, via
// threadMembers) so the list stays cheap on large fleets. Associated MRs are
// resolved in exactly two additional batched round-trips TOTAL (not per head):
// one forward-dependency walk over every thread's members, then one bead fetch
// over the produced candidates (threadProducedMRsByThread).
func threadList(ctx context.Context, store threadLister, project string) ([]threadSummary, error) {
	headQ := beadsapi.ListBeadsQuery{
		Labels: []string{threadHeadLabel},
		Sort:   "-updated_at",
		Limit:  threadMemberQueryLimit,
	}
	if project != "" {
		headQ.Labels = append(headQ.Labels, "project:"+project)
	}
	heads, err := store.ListBeadsFiltered(ctx, headQ)
	if err != nil {
		return nil, err
	}
	if heads == nil || len(heads.Beads) == 0 {
		return nil, nil
	}

	out := make([]threadSummary, 0, len(heads.Beads))
	// memberIDsByThread feeds the single batched produced-MR walk after the loop:
	// the head plus every field-bound member is a candidate producer.
	memberIDsByThread := make(map[string][]string, len(heads.Beads))
	for _, head := range heads.Beads {
		if head == nil {
			continue
		}
		s := threadSummary{
			ThreadID:     head.ID,
			Title:        head.Title,
			WishText:     threadWishText(head),
			Status:       head.Status,
			Project:      projectFromBeadLabels(head.Labels),
			MemberCount:  1, // the head is always a member of its own thread.
			LastActivity: head.UpdatedAt,
			CreatedAt:    head.CreatedAt,
			Herder:       threadHerderOf(head),
		}

		// Field fast-path: the indexed thread_id-equality query returns every
		// derivative bound to this head. One round-trip supplies the member tally,
		// the last-activity max, the participant/starter avatars, and the
		// last-message preview.
		members, mErr := store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Fields: map[string]string{threadIDField: head.ID},
			Limit:  threadMemberQueryLimit,
		})
		if mErr != nil {
			return nil, mErr
		}

		memberIDs := []string{head.ID}
		var memberBeads []*beadsapi.BeadDetail
		if members != nil {
			for _, m := range members.Beads {
				if m == nil {
					continue
				}
				s.MemberCount++
				if m.UpdatedAt.After(s.LastActivity) {
					s.LastActivity = m.UpdatedAt
				}
				memberIDs = append(memberIDs, m.ID)
				memberBeads = append(memberBeads, m)
			}
		}
		memberIDsByThread[head.ID] = memberIDs

		// Starter, participant avatars, and the last-message preview all derive
		// from the head + its field members — no extra round-trip.
		s.Starter = threadStarterOf(head)
		s.Participants, s.ParticipantOverflow = threadParticipantsOf(head, memberBeads)
		s.LastMessage = threadLastMessageOf(memberBeads)

		out = append(out, s)
	}

	// Associated MRs (ka-0000000064): two batched round-trips total, attributed
	// back to each thread. A walk failure degrades to no MR pills rather than
	// failing the whole index — losing the pills must never blank the list.
	if mrsByThread := threadProducedMRsByThread(ctx, store, memberIDsByThread); mrsByThread != nil {
		for i := range out {
			out[i].ProducedMRs = mrsByThread[out[i].ThreadID]
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.After(out[j].LastActivity)
		}
		return out[i].ThreadID < out[j].ThreadID
	})
	return out, nil
}

// threadStarterOf resolves the person who opened a wish — the head bead's human
// operator — into a threadParticipant avatar. Returns a zero-valued participant
// (Display=="") when the head carries no resolvable human operator (e.g. an
// agent- or system-minted head), so the template omits the "started by" line.
func threadStarterOf(head *beadsapi.BeadDetail) threadParticipant {
	if head == nil || !bridge.IsHumanOperator(strings.TrimSpace(head.OperatedBy)) {
		return threadParticipant{}
	}
	return threadParticipantFromBead(head)
}

// threadParticipantFromBead projects a bead's operator into a participant avatar
// via the shared chip resolver, so the initials/display/slug match every other
// operator surface exactly. basePath="" suppresses the chip's filter link (the
// avatar builds its own link when a slug is present); viewerEmail="" is fine —
// the index avatar carries no "(self)" treatment.
func threadParticipantFromBead(b *beadsapi.BeadDetail) threadParticipant {
	chip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, "", "")
	return threadParticipant{
		RawID:    strings.TrimSpace(b.OperatedBy),
		Display:  chip.Display,
		Initials: operatorInitials(chip),
		Slug:     chip.Slug,
	}
}

// threadParticipantsOf returns the distinct HUMAN participants of a thread — the
// head opener plus every member bead's human operator — most-recently-active
// first, deduped by identity. It returns the capped avatar slice and the count
// of participants beyond the cap (rendered as a "+N" avatar). Non-human
// operators (agent/system/scheduler-minted rows with no human operated_by) are
// skipped — avatars represent people.
//
// Identity folding keys on the resolved email when present (so a person's
// slack:U… and oidc:<email> aliases collapse to one avatar), else the raw
// operated_by id — mirroring buildPlanPeopleStrip's email-first fold.
func threadParticipantsOf(head *beadsapi.BeadDetail, members []*beadsapi.BeadDetail) ([]threadParticipant, int) {
	type agg struct {
		p    threadParticipant
		last time.Time
	}
	byKey := map[string]*agg{}
	var order []string

	consider := func(b *beadsapi.BeadDetail, at time.Time) {
		if b == nil || !bridge.IsHumanOperator(strings.TrimSpace(b.OperatedBy)) {
			return
		}
		key := threadParticipantKey(b)
		a, ok := byKey[key]
		if !ok {
			a = &agg{p: threadParticipantFromBead(b), last: at}
			byKey[key] = a
			order = append(order, key)
			return
		}
		if at.After(a.last) {
			a.last = at
		}
	}

	if head != nil {
		consider(head, head.UpdatedAt)
	}
	for _, m := range members {
		consider(m, m.UpdatedAt)
	}
	if len(order) == 0 {
		return nil, 0
	}

	aggs := make([]*agg, 0, len(order))
	for _, k := range order {
		aggs = append(aggs, byKey[k])
	}
	// Most-recently-active first; tie-broken by display for a stable order.
	sort.SliceStable(aggs, func(i, j int) bool {
		if !aggs[i].last.Equal(aggs[j].last) {
			return aggs[i].last.After(aggs[j].last)
		}
		return strings.ToLower(aggs[i].p.Display) < strings.ToLower(aggs[j].p.Display)
	})

	overflow := 0
	if len(aggs) > threadParticipantCap {
		overflow = len(aggs) - threadParticipantCap
		aggs = aggs[:threadParticipantCap]
	}
	people := make([]threadParticipant, 0, len(aggs))
	for _, a := range aggs {
		people = append(people, a.p)
	}
	return people, overflow
}

// threadParticipantKey folds a bead's operator into a stable identity key:
// the resolved email when present (collapses slack:/oidc: aliases into one
// person), else the lowercased raw operated_by id.
func threadParticipantKey(b *beadsapi.BeadDetail) string {
	if e := strings.ToLower(strings.TrimSpace(b.OperatedByEmail)); e != "" {
		return "email:" + e
	}
	return "id:" + strings.ToLower(strings.TrimSpace(b.OperatedBy))
}

// threadLastMessageOf returns the most recent conversation utterance across a
// thread's members — the latest type=message member's text, author, and
// timestamp. Returns a zero-valued threadLastMessage (Text=="") when the thread
// has no message members, so the card falls back to the wish text alone.
func threadLastMessageOf(members []*beadsapi.BeadDetail) threadLastMessage {
	var latest *beadsapi.BeadDetail
	for _, m := range members {
		if !isThreadMessageBead(m) {
			continue
		}
		if latest == nil || m.CreatedAt.After(latest.CreatedAt) {
			latest = m
		}
	}
	if latest == nil {
		return threadLastMessage{}
	}
	msg := threadLastMessage{
		Text: threadMessageText(latest),
		At:   latest.CreatedAt,
	}
	if bridge.IsHumanOperator(strings.TrimSpace(latest.OperatedBy)) {
		msg.Author = threadParticipantFromBead(latest).Display
	} else if cb := strings.TrimSpace(latest.CreatedBy); cb != "" {
		msg.Author = cb
	}
	return msg
}

// threadProducedMRsByThread resolves the merge requests produced by every
// thread's members in exactly two batched round-trips TOTAL (one forward-dep
// walk over the union of all member ids, one bead fetch over the produced
// candidates), then attributes each MR back to the thread(s) that produced it.
// This keeps the index at O(1) MR round-trips regardless of thread count,
// instead of the per-thread walk the detail page can afford.
//
// Returns nil on any transport failure so the caller degrades to no MR pills
// rather than failing the whole index — losing the pills must never blank the
// list. Threads that produced nothing simply have no entry in the map.
func threadProducedMRsByThread(ctx context.Context, store threadMRStore, memberIDsByThread map[string][]string) map[string][]wrapUpMRView {
	if store == nil || len(memberIDsByThread) == 0 {
		return nil
	}

	// Union of every member id across all threads (deduped) for one batched walk.
	seenMember := map[string]bool{}
	var allMembers []string
	for _, ids := range memberIDsByThread {
		for _, id := range ids {
			if id == "" || seenMember[id] {
				continue
			}
			seenMember[id] = true
			allMembers = append(allMembers, id)
		}
	}
	if len(allMembers) == 0 {
		return nil
	}

	fwd, err := store.BatchGetDependencies(ctx, allMembers, "forward")
	if err != nil {
		return nil
	}

	// Collect the unique produced-MR candidate ids across every member.
	seenCand := map[string]bool{}
	var candidateIDs []string
	for _, deps := range fwd {
		for _, d := range deps {
			if d.Type != "produces" || d.DependsOnID == "" {
				continue
			}
			if seenCand[d.DependsOnID] {
				continue
			}
			seenCand[d.DependsOnID] = true
			candidateIDs = append(candidateIDs, d.DependsOnID)
		}
	}
	if len(candidateIDs) == 0 {
		return nil
	}

	beads, err := store.BatchGetBeads(ctx, candidateIDs)
	if err != nil {
		return nil
	}

	// Attribute each resolved MR back to every thread whose member produced it,
	// deduped per thread and ordered by member position (dep-first) then
	// discovery order — the same stable ordering threadProducedMRs uses.
	out := map[string][]wrapUpMRView{}
	for threadID, memberIDs := range memberIDsByThread {
		perThreadSeen := map[string]bool{}
		var pills []wrapUpMRView
		for _, mid := range memberIDs {
			for _, d := range fwd[mid] {
				if d.Type != "produces" || d.DependsOnID == "" || perThreadSeen[d.DependsOnID] {
					continue
				}
				b := beads[d.DependsOnID]
				if b == nil || b.Type != "merge-request" {
					continue
				}
				url := b.Fields["mr_url"]
				if url == "" {
					continue
				}
				perThreadSeen[d.DependsOnID] = true
				pills = append(pills, wrapUpMRViewFromBead(url, b))
			}
		}
		if len(pills) > 0 {
			out[threadID] = pills
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// threadWishText returns the clearest text of the user's wish for a head bead:
// the verbatim opening-message text (slack_root_text) when present, else the
// head title.
func threadWishText(head *beadsapi.BeadDetail) string {
	if head == nil {
		return ""
	}
	if v := strings.TrimSpace(head.Fields[threadRootTextField]); v != "" {
		return v
	}
	return head.Title
}

// threadMessageText returns the literal body of a type=message member: the
// fields.text the composer (and the bridge thread-say forwarder) write, falling
// back to the bead title when the field is absent (a message bead minted by a
// path that put the body only in the title). Never returns the head's
// slack_root_text — that is the wish, not a reply.
func threadMessageText(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if v := strings.TrimSpace(b.Fields["text"]); v != "" {
		return v
	}
	return b.Title
}

// projectFromBeadLabels returns the project name carried by a bead's
// project:<name> label, or "" when none is present.
func projectFromBeadLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			return v
		}
	}
	return ""
}

// --- Membership resolver (field ∪ parent-child closure) --------------------

// threadMembersWithFieldSnapshot resolves the full membership of a thread as
// UNION(field thread_id, parent-child closure), returned in dep-first order.
//
// Ordering — the "dep-first" principle 1b-3 (ka-0000002099) established:
//
//  1. The parent-child closure is the AUTHORITATIVE membership signal (it is
//     written durably before the thread_id field), so it comes first: a
//     breadth-first walk from the head over reverse parent-child dependencies
//     (a child depends-on its parent, so children are reverse-dep rows of
//     Type "parent-child"), head included. Within each BFS level newly
//     discovered children are sorted by id for deterministic output.
//  2. Field-only members — beads carrying fields.thread_id == headID that the
//     closure did NOT reach (cross-epic derivatives bound only by the wish
//     pointer, e.g. messages/agents that are not parent-child descendants of
//     the head) — are appended after the closure, ordered by created_at then
//     id.
//
// The head itself is always element 0. The returned slice is deduplicated.
//
// It also returns the field-query bead snapshots gathered during Half 2 — the
// full BeadDetail rows the thread_id field query already returned, keyed by id. Callers that later re-fetch member
// bodies via a SEPARATE BatchGetBeads use this as a fallback for members that
// fetch raced past (ka-0000006057 gap 3): a type=message member the batch omits
// would otherwise have no body, so isThreadMessageBead(nil)==false mis-files it
// into the WORK lane instead of the conversation. The snapshot lets the fuser
// (and the auditor, so both agree by construction) render such a member from the
// body the field query already saw. The snapshot covers EVERY field-bound member
// (not just the field-only ones appended after the closure) so a message that is
// also a parent-child descendant still has a fallback body.
func threadMembersWithFieldSnapshot(ctx context.Context, store threadMemberStore, headID string) ([]string, map[string]*beadsapi.BeadDetail, error) {
	if headID == "" {
		return nil, nil, nil
	}

	// Half 1 — authoritative parent-child closure (reverse-dep BFS from head).
	seen := map[string]bool{headID: true}
	order := []string{headID}
	level := []string{headID}
	for depth := 0; len(level) > 0 && depth <= maxThreadClosureDepth; depth++ {
		revs, err := store.BatchGetDependencies(ctx, level, "reverse")
		if err != nil {
			return nil, nil, err
		}
		var next []string
		for _, id := range level {
			for _, d := range revs[id] {
				if d.Type != "parent-child" || d.BeadID == "" {
					continue
				}
				child := d.BeadID
				if seen[child] {
					continue
				}
				seen[child] = true
				next = append(next, child)
			}
		}
		sort.Strings(next) // deterministic intra-level order
		order = append(order, next...)
		level = next
	}

	// Half 2 — field fast-path members not already reached by the closure.
	fieldRes, err := store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Fields: map[string]string{threadIDField: headID},
		Limit:  threadMemberQueryLimit,
	})
	if err != nil {
		return nil, nil, err
	}
	var fieldSnap map[string]*beadsapi.BeadDetail
	var fieldOnly []*beadsapi.BeadDetail
	if fieldRes != nil {
		fieldSnap = make(map[string]*beadsapi.BeadDetail, len(fieldRes.Beads))
		for _, b := range fieldRes.Beads {
			if b == nil || b.ID == "" {
				continue
			}
			// Snapshot every field-bound member's body regardless of the closure
			// dedup below, so a member reached by BOTH halves still has a fallback.
			fieldSnap[b.ID] = b
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			fieldOnly = append(fieldOnly, b)
		}
	}
	sort.SliceStable(fieldOnly, func(i, j int) bool {
		if !fieldOnly[i].CreatedAt.Equal(fieldOnly[j].CreatedAt) {
			return fieldOnly[i].CreatedAt.Before(fieldOnly[j].CreatedAt)
		}
		return fieldOnly[i].ID < fieldOnly[j].ID
	})
	for _, b := range fieldOnly {
		order = append(order, b.ID)
	}

	return order, fieldSnap, nil
}

// mergeFieldSnapshotFallback fills BatchGetBeads misses with the field-query
// snapshots captured during membership resolution, so a member the batch fetch
// raced past (but the thread_id field query returned) still contributes its
// body. This chiefly rescues a type=message member — without its body,
// isThreadMessageBead(nil)==false would mis-file the operator's reply as generic
// agent work (ka-0000006057 gap 3). Only fills genuine misses; a body the batch
// DID return always wins (it is the fresher read). Returns the (possibly newly
// allocated) beads map so a nil input is safe.
func mergeFieldSnapshotFallback(beads, snap map[string]*beadsapi.BeadDetail) map[string]*beadsapi.BeadDetail {
	if len(snap) == 0 {
		return beads
	}
	if beads == nil {
		beads = make(map[string]*beadsapi.BeadDetail, len(snap))
	}
	for id, b := range snap {
		if b != nil && beads[id] == nil {
			beads[id] = b
		}
	}
	return beads
}

// --- Fused-timeline aggregator ---------------------------------------------

// Timeline entry kinds. The rank values double as the lifecycle tie-breaker
// when two entries on the same member share a timestamp (created before any
// comment before closed).
//
// threadEntryMessage (2-5) is the conversation event a type=message member bead
// contributes — the composer's bridge-mediated reply (thread_compose.go writes
// these) and any other type=message bead bound to the wish. A message bead emits
// EXACTLY this one entry in place of the bead_created/bead_closed work pair, so
// the operator's reply reads as a conversation bubble (curation lane) rather
// than as "spawned work". It carries no rank collision risk: a message bead
// emits only this single entry, so the intra-bead lifecycle tie-break never
// fires for it.
const (
	threadEntryBeadCreated = "bead_created"
	threadEntryMessage     = "message"
	threadEntryComment     = "comment"
	threadEntryBeadClosed  = "bead_closed"
)

func threadEntryKindRank(kind string) int {
	switch kind {
	case threadEntryBeadCreated:
		return 0
	case threadEntryMessage:
		return 1
	case threadEntryComment:
		return 1
	case threadEntryBeadClosed:
		return 2
	default:
		return 3
	}
}

// isThreadMessageBead reports whether a member bead is a conversation message
// (type=message) rather than agent work. The composer (thread_compose.go) writes
// the operator's reply as a closed type=message bead bound to the wish by the
// thread_id field; squawks/Slack drops bound to the wish are the same shape.
// These surface as conversation bubbles, not as spawn/close work events.
func isThreadMessageBead(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Type == "message"
}

// threadTimelineEntry is one chronologically-placed event in a thread's fused
// timeline. Entries span ALL member beads, fused into a single stream.
type threadTimelineEntry struct {
	// At is the timestamp the entry is placed at.
	At time.Time
	// Kind is one of the threadEntry* constants.
	Kind string
	// BeadID is the member bead this entry belongs to.
	BeadID string
	// BeadTitle is that member's title (rendering convenience).
	BeadTitle string
	// Author is the actor: the bead's creator (bead_created / bead_closed) or
	// the comment author (comment).
	Author string
	// Text is the comment body for comment entries; empty otherwise.
	Text string
	// CommentID is the comment's id for comment entries; 0 otherwise. Used as
	// a final, stable tie-breaker.
	CommentID int64
	// ClosedAtImputed is true on a bead_closed entry whose source ClosedAt was
	// empty or unparseable, so the timestamp was imputed (to the bead's
	// created_at) rather than parsed. The close event is ALWAYS emitted for a
	// closed bead — never silently dropped — and this flag lets the renderer
	// and the /threads/{id}/audit reconciliation surface the degraded timestamp
	// as an explicit, audit-visible marker (rubric C5/K3, ka-0000005790). It is
	// always false for bead_created / comment entries and for a closed bead with
	// a parseable ClosedAt.
	ClosedAtImputed bool
}

// threadTimeline is the fused result for a single thread.
type threadTimeline struct {
	// ThreadID is the head bead's ka-id.
	ThreadID string
	// Members are the member bead ids in dep-first order (see threadMembers).
	Members []string
	// Entries are every member's lifecycle + comment events, fused and sorted
	// chronologically.
	Entries []threadTimelineEntry
}

// fuseThreadTimeline produces a single chronologically-fused timeline across
// all member beads of the thread headed by headID.
//
// Membership is resolved via threadMembers (field ∪ parent-child closure, in
// dep-first order). For each member bead the fused stream emits:
//
//   - a bead_created entry at the bead's created_at,
//   - one comment entry per comment, at the comment's created_at,
//   - a bead_closed entry at the bead's closed_at when the bead is closed.
//
// Entries sort by (At asc, dep-first member position asc, lifecycle-kind asc,
// comment-id asc). The member-position tie-breaker is exactly where the
// "dep-first ordering" 1b-3 established is consumed: when two entries carry
// the same timestamp, the one on the bead that is earlier in the authoritative
// parent-child closure wins, so the fused order is stable and dependency-led
// rather than dependent on map iteration.
func fuseThreadTimeline(ctx context.Context, store threadTimelineStore, headID string) (threadTimeline, error) {
	tl := threadTimeline{ThreadID: headID}
	if headID == "" {
		return tl, nil
	}

	members, fieldSnap, err := threadMembersWithFieldSnapshot(ctx, store, headID)
	if err != nil {
		return tl, err
	}
	tl.Members = members
	if len(members) == 0 {
		return tl, nil
	}

	// dep-first member position, used as the secondary sort key.
	pos := make(map[string]int, len(members))
	for i, id := range members {
		pos[id] = i
	}

	beads, err := store.BatchGetBeads(ctx, members)
	if err != nil {
		return tl, err
	}
	// Fill any member the batch fetch raced past with the body the thread_id
	// field query already returned, so a type=message member is not mis-filed as
	// work for lack of a body (ka-0000006057 gap 3).
	beads = mergeFieldSnapshotFallback(beads, fieldSnap)

	var entries []threadTimelineEntry
	for _, id := range members {
		b := beads[id]
		// A type=message member (the composer's reply, a squawk, a Slack drop)
		// is conversation, not work: it emits ONE message entry carrying its
		// literal text instead of the bead_created/bead_closed work pair (2-5,
		// ka-0000006034). The composer creates these beads already-closed
		// (thread_compose.go) to fire the bridge forward, so a created+closed
		// rendering would double-count a single utterance AND mis-file it under
		// agent work. Place the entry at created_at — the moment the operator
		// hit send — and skip the comment fetch (a message bead carries its body
		// in fields.text, not in comments). Authoring flows from CreatedBy,
		// which thread_compose.go stamps with the operator email.
		//
		// isThreadMessageBead is false for a nil body, so a daemon-missing
		// member never matches here — it falls through to the comment-preserving
		// path below (ka-0000005790 bug fix C6), keeping both behaviors.
		if isThreadMessageBead(b) {
			entries = append(entries, threadTimelineEntry{
				At:        b.CreatedAt,
				Kind:      threadEntryMessage,
				BeadID:    b.ID,
				BeadTitle: b.Title,
				Author:    b.CreatedBy,
				Text:      threadMessageText(b),
			})
			continue
		}

		// A member the daemon's BatchGetBeads did not return (deleted /
		// cross-fleet) has no body, so it contributes no bead_created /
		// bead_closed entry and an empty title. It must NOT, however, lose its
		// comments: GetComments is keyed on the member id, not the bead body,
		// so a missing body must not skip the comment fetch (bug fix, rubric
		// C6, ka-0000005790 — the old code `continue`d here and dropped them).
		// The comment loop below therefore reads beadID/beadTitle defensively
		// off whatever the daemon returned (empty when b == nil), never off a
		// sibling, so a missing member's comments stay attributed to it with no
		// title bleed.
		var beadTitle string
		if b != nil {
			beadTitle = b.Title
			entries = append(entries, threadTimelineEntry{
				At:        b.CreatedAt,
				Kind:      threadEntryBeadCreated,
				BeadID:    b.ID,
				BeadTitle: b.Title,
				Author:    b.CreatedBy,
			})
			if b.Status == "closed" {
				// A closed bead ALWAYS emits a bead_closed entry. When its
				// ClosedAt is empty or unparseable the timestamp is imputed to
				// the bead's created_at (so the entry still sorts sanely) and
				// flagged ClosedAtImputed, rather than being silently dropped
				// (bug fix, rubric C5/K3, ka-0000005790 — the old code dropped
				// the entry whenever parseThreadClosedAt returned ok=false). The
				// imputation is audit-visible: /threads/{id}/audit reports it as
				// an unparseable_closed_at warning.
				at, imputed := threadClosedAtOrImpute(b.ClosedAt, b.CreatedAt)
				entries = append(entries, threadTimelineEntry{
					At:              at,
					Kind:            threadEntryBeadClosed,
					BeadID:          b.ID,
					BeadTitle:       b.Title,
					Author:          b.ClosedBy,
					ClosedAtImputed: imputed,
				})
			}
		}

		comments, cErr := store.GetComments(ctx, id)
		if cErr != nil {
			return tl, cErr
		}
		for _, c := range comments {
			entries = append(entries, threadTimelineEntry{
				At:        c.CreatedAt,
				Kind:      threadEntryComment,
				BeadID:    id,
				BeadTitle: beadTitle,
				Author:    c.Author,
				Text:      c.Text,
				CommentID: c.ID,
			})
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if pa, pb := pos[a.BeadID], pos[b.BeadID]; pa != pb {
			return pa < pb
		}
		if ra, rb := threadEntryKindRank(a.Kind), threadEntryKindRank(b.Kind); ra != rb {
			return ra < rb
		}
		return a.CommentID < b.CommentID
	})

	tl.Entries = entries
	return tl, nil
}

// threadClosedAtOrImpute resolves the timestamp for a closed bead's
// bead_closed entry. It returns the parsed ClosedAt when the string is a valid
// RFC3339(-ish) timestamp (imputed=false), and otherwise imputes the bead's
// created_at (imputed=true) so the close event is still placed on the timeline
// rather than dropped. Imputing to created_at — never the zero time — keeps the
// fused order sane (a close cannot precede its own creation) while the imputed
// flag preserves the fact that the source timestamp was missing/malformed for
// the audit layer to surface. This is the single source of the C5/K3 "never a
// silent drop" guarantee; fuseThreadTimeline and auditThreadTimeline both route
// through it so they agree by construction.
func threadClosedAtOrImpute(closedAt string, createdAt time.Time) (time.Time, bool) {
	if at, ok := parseThreadClosedAt(closedAt); ok {
		return at, false
	}
	return createdAt, true
}

// parseThreadClosedAt parses the BeadDetail.ClosedAt string (an RFC3339
// timestamp the daemon emits as a string, distinct from the time.Time
// CreatedAt/UpdatedAt columns). Returns ok=false when the field is empty or
// unparseable. Callers must NOT drop the bead_closed entry on ok=false — route
// through threadClosedAtOrImpute, which imputes created_at and flags the entry
// so the close is preserved and the degradation is audit-visible (rubric
// C5/K3, ka-0000005790).
func parseThreadClosedAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
