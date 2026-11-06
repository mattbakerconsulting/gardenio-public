package main

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Phase 2 — net-new interactive /threads view (epic ka-0000000679, task 2-4
// ka-0000006184). This file is the CURATION layer for the two-pane
// /threads/{id} detail: pure methods over the already-built threadDetailView
// (defined in threads_routes.go, owned by 2-3) that implement the rubric's
// headline gate B11 — "conversation first, work collapsed".
//
// Why a curation layer at all (rubric B11 + plan objection E). The fused
// timeline 2-1 produces (threads.go fuseThreadTimeline) interleaves the human
// conversation (comments) with the agent's work lifecycle (a bead_created /
// bead_closed event for every member bead). Matt's own model — "many plans and
// intents may be carried out before the user's wishes are all fulfilled" —
// means a long-lived wish accrues a LOT of work, so an un-curated merge-sort is
// exactly the "advanced-prototype noise" the plan devalues. 2-4 therefore
// splits the one fused stream into two lanes the template renders very
// differently:
//
//   - CONVERSATION leads, visible by default: the human back-and-forth — every
//     comment — plus the wish's own open/close lifecycle (events on the head
//     bead itself). This is the WHY the user reads first.
//   - WORK is collapsed behind a <details> toggle, expandable on demand: every
//     OTHER member bead's spawn/close lifecycle — the agent epics, tasks, and
//     agents, the HOW. The member roster (threadDetailView.Members) renders in
//     the same collapsed pane. A 50-agent wish thus opens to a readable
//     conversation, with the firehose one click away.
//
// Every method here is pure (no daemon, no request) and derives solely from the
// view model's already-sorted Timeline + Members, so the split is exhaustively
// unit-testable and never re-fetches or re-sorts. fuseThreadTimeline already
// established the canonical order (At asc, dep-first member position,
// lifecycle-kind, comment-id); these partitions are stable because they iterate
// that slice in place and preserve its order within each lane.

// threadConversationEntry is one rendered row of the conversation lane. It is a
// thin re-projection of a threadTimelineEntry carrying a render-time Verb so the
// template does not branch on raw kind strings: a comment renders as a body
// bubble, a wish lifecycle event renders as a one-line marker ("opened this
// wish" / "closed this wish").
type threadConversationEntry struct {
	// At is when the entry occurred (the comment's or the head's timestamp).
	At time.Time
	// Author is the comment author or the head's creator/closer.
	Author string
	// Text is the comment body; empty for a lifecycle marker.
	Text string
	// IsComment is true for a real reply (render the body bubble); false for a
	// wish open/close marker (render the compact marker line).
	IsComment bool
	// Marker is the human verb for a lifecycle row: "opened this wish" or
	// "closed this wish". Empty for a comment.
	Marker string
	// Key is the content-derived de-dup key (threadMsgKey of Author+Text) the
	// template stamps onto each rendered conversation bubble as data-msg-key.
	// The composer's optimistic-render JS computes the SAME key from the
	// operator's draft, so when the server echo of a just-sent message arrives
	// in a ?since= swap, the optimistic bubble is reconciled away by an exact
	// key match instead of producing a duplicate (rubric (e)/ka-0000001087).
	// Empty for lifecycle markers (they carry no body and are never optimistic).
	Key string
}

// threadWorkEntry is one rendered row of the collapsed work lane: a member
// bead's spawn or close, attributed and linkable back to the member's page.
type threadWorkEntry struct {
	// At is when the work event occurred.
	At time.Time
	// Verb is the human verb: "spawned" (bead_created) or "closed"
	// (bead_closed).
	Verb string
	// Actor is the member bead's creator (spawn) or closer (close).
	Actor string
	// BeadID / BeadTitle identify the member the event belongs to so the row
	// links to /{BeadID} and shows a readable title.
	BeadID    string
	BeadTitle string
}

// Conversation returns the conversation lane: the human dialogue (every comment)
// plus the wish's own open/close lifecycle (events on the head bead itself),
// preserving the fused timeline's chronological order. This is the lane the
// detail page LEADS with (rubric B11). A wish with no replies yet yields a slice
// holding just its "opened this wish" marker — never nil-when-there-is-a-head —
// so the template's lead pane is never an empty white void (rubric C15).
func (v threadDetailView) Conversation() []threadConversationEntry {
	out := make([]threadConversationEntry, 0, len(v.Timeline))
	for _, e := range v.Timeline {
		switch {
		case e.Kind == threadEntryComment:
			out = append(out, threadConversationEntry{
				At: e.At, Author: e.Author, Text: e.Text, IsComment: true,
				Key: threadMsgKey(e.Author, e.Text),
			})
		case e.Kind == threadEntryMessage:
			// A type=message member — the composer's bridge-mediated reply, a
			// squawk, or a Slack drop (2-5). It reads as a conversation bubble,
			// identical to a comment, so the operator's reply appears in the lane
			// it leads. The wish-pointer message carries its body in e.Text.
			out = append(out, threadConversationEntry{
				At: e.At, Author: e.Author, Text: e.Text, IsComment: true,
				Key: threadMsgKey(e.Author, e.Text),
			})
		case e.BeadID == v.ThreadID && e.Kind == threadEntryBeadCreated:
			// The wish itself opening — the conversation's anchor.
			out = append(out, threadConversationEntry{
				At: e.At, Author: e.Author, Marker: "opened this wish",
			})
		case e.BeadID == v.ThreadID && e.Kind == threadEntryBeadClosed:
			// The wish being marked done — a conversation-level milestone.
			out = append(out, threadConversationEntry{
				At: e.At, Author: e.Author, Marker: "closed this wish",
			})
		}
	}
	return out
}

// WorkEvents returns the collapsed work lane: the spawn/close lifecycle of every
// NON-head member bead (the agent epics/tasks/agents — the "how"), preserving
// the fused timeline's order. Head lifecycle events are deliberately excluded —
// they belong to the conversation lane (the wish open/close) — so the work pane
// is purely the agent work, which is what gets collapsed by default (B11).
func (v threadDetailView) WorkEvents() []threadWorkEntry {
	out := make([]threadWorkEntry, 0, len(v.Timeline))
	for _, e := range v.Timeline {
		if e.BeadID == v.ThreadID {
			continue // wish lifecycle is conversation, not work.
		}
		switch e.Kind {
		case threadEntryBeadCreated:
			out = append(out, threadWorkEntry{
				At: e.At, Verb: "spawned", Actor: e.Author,
				BeadID: e.BeadID, BeadTitle: e.BeadTitle,
			})
		case threadEntryBeadClosed:
			out = append(out, threadWorkEntry{
				At: e.At, Verb: "closed", Actor: e.Author,
				BeadID: e.BeadID, BeadTitle: e.BeadTitle,
			})
		}
	}
	return out
}

// HasConversation reports whether the conversation lane has any rows — used by
// the template to choose between the lane and its empty state.
func (v threadDetailView) HasConversation() bool { return len(v.Conversation()) > 0 }

// ReplyCount is the number of real replies in the conversation — comments AND
// type=message members (the composer's bridge-mediated replies, 2-5) — excluding
// the wish open/close lifecycle markers. It heads the conversation pane
// ("Conversation · N replies") so the count reflects dialogue, not work events.
func (v threadDetailView) ReplyCount() int {
	n := 0
	for _, e := range v.Timeline {
		if e.Kind == threadEntryComment || e.Kind == threadEntryMessage {
			n++
		}
	}
	return n
}

// WorkMembers returns the member rows that make up the collapsed work pane —
// every member EXCEPT the wish head itself (element 0 of Members is always the
// head; see buildThreadDetailView) AND except conversation (type=message)
// members, which live in the conversation lane (2-5). These are the agent
// epics/tasks/agents the pane lists below the work-event lane.
func (v threadDetailView) WorkMembers() []threadMemberRow {
	out := make([]threadMemberRow, 0, len(v.Members))
	for _, m := range v.Members {
		if m.IsHead || m.IsMessage {
			continue
		}
		out = append(out, m)
	}
	return out
}

// WorkMemberCount is the number of non-head, non-message member beads — the
// headline figure on the collapsed "Work (N)" toggle. Message members are
// conversation, not work, so they are excluded (2-5).
func (v threadDetailView) WorkMemberCount() int {
	n := 0
	for _, m := range v.Members {
		if !m.IsHead && !m.IsMessage {
			n++
		}
	}
	return n
}

// WorkDoneCount is the number of distinct non-head member beads that have a
// close event in the timeline — the "M done" half of the collapsed work
// summary chip. It reads the bead_closed entries rather than a per-member status
// field (threadMemberRow carries none), which the fused timeline already
// surfaces.
func (v threadDetailView) WorkDoneCount() int {
	done := make(map[string]bool)
	for _, e := range v.Timeline {
		if e.Kind == threadEntryBeadClosed && e.BeadID != v.ThreadID {
			done[e.BeadID] = true
		}
	}
	return len(done)
}

// HasWork reports whether the wish has any agent work at all — either spawned
// member beads or work lifecycle events. When false the template renders a
// single muted "no work yet" line instead of an empty collapsible, so a
// conversation-only wish does not show a hollow expandable (rubric C15).
func (v threadDetailView) HasWork() bool {
	return v.WorkMemberCount() > 0 || len(v.WorkEvents()) > 0
}

// LatestActivity returns the timestamp of the most recent fused-timeline entry
// across BOTH lanes — the newest of every comment, message, spawn, and close.
// It is the `?since=` polling cursor (2-5): the client echoes it back on each
// poll, and handleThreadTimeline 304s when the cursor still matches the latest,
// so an idle thread does no DOM swap. Because it spans the whole fused stream,
// a new spawned-work event advances the cursor exactly as a new reply does, so
// the poll refreshes the timeline on EITHER kind of activity (rubric B12).
// Returns the zero time for an empty timeline (a freshly-minted, eventless wish),
// which the template renders as an empty cursor so the first poll always swaps.
func (v threadDetailView) LatestActivity() time.Time {
	var latest time.Time
	for _, e := range v.Timeline {
		if e.At.After(latest) {
			latest = e.At
		}
	}
	return latest
}

// OpenedAt returns the timestamp the wish head was created, derived from its
// bead_created timeline entry, or the zero time when the timeline carries no
// such entry (e.g. a degraded render where the membership fetch failed). The
// rail uses it for an "opened N ago" line without threading a separate CreatedAt
// through the view model; the template guards on .IsZero so a missing value
// simply omits the row rather than printing a Unix-epoch date.
func (v threadDetailView) OpenedAt() time.Time {
	for _, e := range v.Timeline {
		if e.Kind == threadEntryBeadCreated && e.BeadID == v.ThreadID {
			return e.At
		}
	}
	return time.Time{}
}

// IsClosed reports whether the wish head is in a closed state — drives the
// rail's "fulfilled" treatment and (when 2-2/2-5 land the composer) the
// reopen-vs-close affordance choice. Kept tolerant of the daemon's several
// terminal spellings.
func (v threadDetailView) IsClosed() bool {
	switch v.Status {
	case "closed", "done", "resolved", "completed":
		return true
	}
	return false
}

// IsNeedsTriage reports whether the wish is in the needs-triage lifecycle — an
// agent failed and left the wish unfulfilled, so it stays open until a human
// picks it up (plan §5 1b-7 failed→triage split). The detail page surfaces this
// as a loud amber banner above the panes (rubric C15 — a non-blank, actionable
// state). The daemon spells this status "needs_triage"; we also accept the
// hyphenated form defensively.
func (v threadDetailView) IsNeedsTriage() bool {
	switch v.Status {
	case "needs_triage", "needs-triage":
		return true
	}
	return false
}

// --- Index-row (threadSummary) curation helpers ----------------------------
//
// The /threads index card needs the same "how much work" and "fulfilled?" cues
// the detail rail shows, derived from the cheap threadSummary that threadList
// produces (it carries MemberCount = head + field-bound derivatives, but no
// per-member detail — the index stays one indexed query per head).

// WorkCount is the number of agent-work beads bound to this wish — every member
// EXCEPT the head itself (threadSummary.MemberCount always counts the head as
// member 1). It is the index card's "N beads of work" figure.
func (s threadSummary) WorkCount() int {
	if s.MemberCount <= 1 {
		return 0
	}
	return s.MemberCount - 1
}

// threadKeyTrim strips leading/trailing whitespace for the de-dup key using the
// EXACT SAME character set as the composer JS's keyTrim() (thread_composer.js),
// so the two halves produce a byte-identical key. It is deliberately NOT plain
// strings.TrimSpace: Go's unicode.IsSpace set and JavaScript String.prototype
// .trim()'s set are NOT equal at the edges (ka-0000006057 gap 1) —
//
//   - Go's unicode.IsSpace includes U+0085 (NEL); JS .trim()'s \s does NOT.
//   - JS .trim() strips U+FEFF (BOM/ZWNBSP); Go's unicode.IsSpace does NOT.
//
// A leading/trailing NEL or BOM in an operator message would therefore make the
// server key differ from the client key, so the optimistic bubble would never
// reconcile and would linger as a permanent duplicate. threadKeyTrim closes the
// gap by trimming the UNION of the two sets (unicode.IsSpace ∪ {U+FEFF}, which
// equals JS's set ∪ {U+0085}); keyTrim() in thread_composer.js trims the same
// union, so the two are byte-identical for ALL inputs, not just ASCII.
func threadKeyTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF'
	})
}

// threadMsgKey computes the content-derived de-dup key for a conversation
// bubble: an FNV-1a/32 hex digest of the whitespace-trimmed author and text,
// joined by a single NUL byte. The composer's optimistic-render JS
// (thread_composer.js) computes the byte-identical key from the operator's
// draft (same trim + NUL-join + FNV-1a/32), so when the server echo of a
// just-sent message lands in a ?since= timeline swap, the optimistic bubble is
// matched to its authoritative twin by an exact key equality and removed — the
// reconcile is a replace, never a duplicate (rubric (e)/ka-0000001087).
//
// Keep this in lockstep with msgKey() in templates/static/thread_composer.js:
// both trim VIA threadKeyTrim/keyTrim (the SAME union whitespace set — see
// threadKeyTrim; plain TrimSpace/.trim() would drift on NEL/BOM), both join
// author+text with a single 0x00 byte, both run FNV-1a/32 over the UTF-8 bytes,
// both render the result as base-16. A drift in either half silently breaks the
// de-dup (the bubble would linger or duplicate), so thread_compose_test.go pins
// this function's output against known vectors (including NEL/BOM edge cases).
func threadMsgKey(author, text string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(threadKeyTrim(author)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(threadKeyTrim(text)))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// IsClosed reports whether the wish head is fulfilled (closed), so the index can
// give granted wishes the muted "fulfilled" treatment. Mirrors the detail
// view's terminal-status set.
func (s threadSummary) IsClosed() bool {
	switch s.Status {
	case "closed", "done", "resolved", "completed":
		return true
	}
	return false
}

// IsNeedsTriage reports whether the wish needs human triage (a failed agent left
// it unfulfilled), so the index can flag it. Mirrors the detail view's set.
func (s threadSummary) IsNeedsTriage() bool {
	switch s.Status {
	case "needs_triage", "needs-triage":
		return true
	}
	return false
}
