package main

// Phase 3 — log-correctness audit for the fused /threads timeline (epic
// ka-0000002477, task 3-2 ka-0000005790). This file is the RECONCILIATION
// layer: a read-only, viewer-scoped auditor that PROVES the timeline
// fuseThreadTimeline (threads.go) produces is COMPLETE, CORRECT, and IN-ORDER.
//
// Why a separate auditor instead of trusting the aggregator's own tests. CI
// green proves the fuser does what its tests say; it does NOT prove the fused
// stream on a live thread is conserved, un-fabricated, and unambiguously
// ordered. Matt's explicit ask (rubric ka-0000007635 §A) is a runtime gate that
// can be pointed at any real thread and return a structured PASS/FAIL. The
// auditor therefore re-derives the EXPECTED timeline INDEPENDENTLY from the same
// daemon sources (its own BatchGetBeads + per-member GetComments pass, and its
// own threadMembers call), then reconciles the fuser's output against it on
// three axes:
//
//   - COUNT (rubric C1/C6) — a conservation identity: the rendered entry count
//     must equal N members-with-body + K comments + M closable members, checked
//     in total, per-kind, and per-member. A drop or a duplicate breaks it.
//   - HASH (rubric K1/K2/K5) — a per-entry SHA256 content-hash multiset:
//     multiset(rendered) must equal multiset(expected). A hash present in
//     rendered but not expected is a FABRICATION; one present in expected but
//     not rendered is a DROP. The hash binds kind, bead, timestamp, author, and
//     comment identity, so an actor bleed or a mis-timestamped entry diverges.
//   - ORDER (rubric O1-O4) — the rendered stream must be strictly monotonic and
//     unique in the canonical 4-tuple (At asc, dep-first pos[BeadID] asc,
//     kindRank asc, CommentID asc), and its order fingerprint is byte-identical
//     across shuffled source insertion order.
//
// Two NON-composer silent-drop bugs the fuser carried are fixed in threads.go
// (a closed member with an empty/unparseable ClosedAt now emits a flagged
// bead_closed entry instead of vanishing; a BatchGetBeads-missing member keeps
// its comments). The auditor SURFACES the first as an `unparseable_closed_at`
// warning and the second as a `missing_member_body` warning — these are
// explicit, expected conditions, NOT failures: the timeline is correct (the
// close is preserved, the comments survive), and the warning makes the degraded
// input audit-visible. The composer body gate (C4/K4) and ?since= prefix
// stability (O5) are deferred to task 3-4 and are not checked here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Warning + divergence codes. Warnings flag expected/degraded-input conditions
// that the timeline handles correctly (they do NOT fail the audit). Divergences
// are genuine correctness failures (they DO).
const (
	threadAuditWarnUnparseableClosedAt = "unparseable_closed_at"
	threadAuditWarnMissingMemberBody   = "missing_member_body"

	threadAuditAxisMembership      = "membership"
	threadAuditAxisCount           = "count"
	threadAuditAxisHash            = "hash"
	threadAuditAxisOrder           = "order"
	threadAuditAxisTitle           = "title"
	threadAuditAxisComposerBody    = "composer_body"
	threadAuditAxisPrefixStability = "prefix_stability"

	// Composer-body divergence codes (rubric C4/K4, ka-0000003162 3-4).
	// A type=message thread-say member's full fields[text] is the authoritative
	// conversation body — postThreadSayMessage truncates the bead's TITLE to 500
	// chars but writes the full text into fields.text, so a long composer reply
	// MUST render as the full body, not the 500-char Title. These codes name the
	// three concrete ways that contract can fail.
	threadAuditCodeComposerBodyDropped     = "composer_body_dropped"     // non-empty fields.text absent from rendered entry
	threadAuditCodeComposerBodyTruncated   = "composer_body_truncated"   // rendered text is a strict prefix of fields.text (Title-fallback bug)
	threadAuditCodeComposerBodyMismatch    = "composer_body_mismatch"    // rendered text differs from fields.text (and isn't a prefix)
	threadAuditCodeComposerActorMismatch   = "composer_actor_mismatch"   // CreatedBy operator email did not land on entry.Author
	threadAuditCodeComposerEntryMissing    = "composer_entry_missing"    // type=message member produced no message entry at all
	threadAuditCodeComposerMultipleEntries = "composer_multiple_entries" // type=message member produced more than one message entry

	// Prefix-stability divergence codes (rubric O5, ka-0000003162 3-4).
	threadAuditCodePrefixHashMismatch    = "prefix_hash_mismatch"    // an earlier entry's hash changed between snapshots
	threadAuditCodePrefixLengthShortfall = "prefix_length_shortfall" // current snapshot has fewer entries before the cursor than the prior fingerprint
)

// threadAuditEntryRef is a compact, JSON-friendly reference to one timeline
// entry, used to point at the specific rows involved in a divergence (a
// fabricated/dropped hash, an out-of-order pair, a title bleed).
type threadAuditEntryRef struct {
	Kind      string `json:"kind"`
	BeadID    string `json:"bead_id"`
	At        string `json:"at"` // RFC3339Nano, UTC
	Author    string `json:"author,omitempty"`
	CommentID int64  `json:"comment_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Hash      string `json:"hash"`
}

// threadAuditCounts is the COUNT axis: the conservation identity and its
// per-kind decomposition. Conserved is the headline boolean; the raw figures
// are reported so a failing audit shows exactly which term is off.
type threadAuditCounts struct {
	// RenderedTotal is len(fuse output Entries).
	RenderedTotal int `json:"rendered_total"`
	// ExpectedTotal is the conservation target: MembersWithBody + Comments +
	// Closable (N + K + M).
	ExpectedTotal int `json:"expected_total"`
	// MembersWithBody (N) is the number of members the daemon returned a body
	// for — each contributes exactly one bead_created.
	MembersWithBody int `json:"members_with_body"`
	// Comments (K) is the total comment count across ALL members, including
	// members whose body was missing (their comments must still be emitted).
	Comments int `json:"comments"`
	// Closable (M) is the number of members-with-body in a closed status —
	// each contributes exactly one bead_closed.
	Closable int `json:"closable"`
	// Messages (P) is the number of type=message members (the composer's
	// bridge-mediated replies, squawks, Slack drops). A message member is
	// conversation, not work: it contributes EXACTLY one message entry in place
	// of the bead_created/bead_closed work pair and emits NO comment entries
	// (its body lives in fields.text, not in comments). It is counted on its own
	// axis — never folded into MembersWithBody/Closable — so the conservation
	// identity mirrors fuseThreadTimeline's per-member emission exactly (2-5,
	// ka-0000006034): a message member that fuse renders as one message entry
	// must be expected as one message entry, or a correct composer reply would
	// read as a fabrication+drop against the ka-0000005790 audit.
	Messages int `json:"messages"`
	// Conserved is true iff RenderedTotal == ExpectedTotal AND every per-kind
	// rendered count matches its expected count.
	Conserved      bool           `json:"conserved"`
	RenderedByKind map[string]int `json:"rendered_by_kind"`
	ExpectedByKind map[string]int `json:"expected_by_kind"`
}

// threadAuditOrder is the ORDER axis: whether the rendered stream is strictly
// monotonic and unique in the canonical 4-tuple, plus an order fingerprint that
// is byte-identical across shuffled source insertion order.
type threadAuditOrder struct {
	StrictlyMonotonic bool `json:"strictly_monotonic"`
	Unique            bool `json:"unique"`
	// Fingerprint is SHA256 over the newline-joined per-entry hashes in
	// rendered order — a single value a determinism test can compare across
	// runs/insertion orders.
	Fingerprint string `json:"fingerprint"`
}

// threadAuditHash is the HASH axis: whether the rendered content-hash multiset
// equals the expected one, with the divergent entries called out.
type threadAuditHash struct {
	Matched bool `json:"matched"`
	// Fabrications are entries rendered without a matching source (rendered
	// multiset count exceeds expected for that hash).
	Fabrications []threadAuditEntryRef `json:"fabrications,omitempty"`
	// Drops are source entries with no matching rendered entry (expected
	// multiset count exceeds rendered for that hash).
	Drops []threadAuditEntryRef `json:"drops,omitempty"`
}

// threadAuditWarning flags an explicit, expected degraded-input condition that
// the timeline handles correctly (an imputed close timestamp, a missing member
// body). Warnings are surfaced, never silently swallowed, but do NOT fail.
type threadAuditWarning struct {
	Code   string `json:"code"`
	BeadID string `json:"bead_id"`
	Detail string `json:"detail,omitempty"`
}

// threadAuditDivergence is one genuine correctness failure on some axis, with
// the offending entries attached when applicable.
type threadAuditDivergence struct {
	Axis    string                `json:"axis"`
	Code    string                `json:"code"`
	BeadID  string                `json:"bead_id,omitempty"`
	Detail  string                `json:"detail"`
	Entries []threadAuditEntryRef `json:"entries,omitempty"`
}

// threadAuditComposerBody is the COMPOSER-BODY axis (rubric C4/K4): for every
// type=message thread-say member, the rendered message entry MUST carry the
// FULL fields[text] body — not the 500-char Title, not a Title-fallback. The
// composer writer (thread_compose.postThreadSayMessage) deliberately truncates
// the bead's Title to 500 chars to satisfy the daemon's title contract while
// writing the full operator-typed body into fields[text]; a long reply (>500
// chars) therefore relies on the renderer reading fields[text], not Title.
// This axis fails the audit on any drop / truncation / Title-fallback /
// attribution bleed.
type threadAuditComposerBody struct {
	// Members is the count of type=message members evaluated on this axis.
	Members int `json:"members"`
	// Conformant is true iff every member's rendered message entry carries
	// fields[text] verbatim AND the operator-email CreatedBy lands on Author.
	Conformant bool `json:"conformant"`
}

// threadAuditPrefixStability is the PREFIX-STABILITY axis (rubric O5): the
// ordered per-entry hash sequence (= the order Fingerprint's pre-image) for a
// `?since=` incremental response MUST be an exact prefix-extension of the
// full-fetch fingerprint for the same snapshot — no earlier entry reorders or
// vanishes between snapshots.
//
// Semantics on a single audit run (Cursor==zero, the default): the auditor
// independently re-derives the EXPECTED entries from the daemon (the same path
// the count/hash axes use), recomputes the per-entry hash sequence in canonical
// order, and proves the FULL fingerprint is byte-identical with the fuser's
// reported fingerprint AND that any cursor-bounded sub-prefix of the
// independently-derived sequence is a strict prefix of the full sequence — the
// invariant the rubric demands.
//
// Semantics on a cursor-bound audit (Cursor!=zero, e.g. /threads/{id}/audit
// ?since=<rfc3339>): the same invariant is exercised at the SPECIFIC cursor the
// client carried — the auditor computes the prefix-fingerprint over entries with
// At <= Cursor and asserts the rendered timeline's prefix up to that index
// hashes identically. This is the rubric's "any incremental response" exercised
// at a real polling point.
//
// PriorFingerprint (optional, client-supplied) lets a long-running client carry
// the prior-snapshot full fingerprint forward across polls so the auditor can
// prove the current snapshot's prefix up to the previously-observed cursor
// still matches the prior fingerprint's prefix — the across-snapshot stability
// guarantee.
type threadAuditPrefixStability struct {
	// Stable is the headline boolean: true iff every prefix invariant evaluated
	// on this run holds.
	Stable bool `json:"stable"`
	// Cursor is the `?since=` cursor used to slice the prefix, in RFC3339Nano
	// UTC. Empty when the audit was run without a cursor (the default — the
	// auditor still proves the full fingerprint matches and that an EMPTY prefix
	// is trivially a prefix of itself).
	Cursor string `json:"cursor,omitempty"`
	// PrefixLength is the count of rendered entries whose At is <= Cursor (the
	// portion a `?since=Cursor` poll would carry forward from the prior
	// snapshot). When Cursor is empty this is 0.
	PrefixLength int `json:"prefix_length"`
	// PriorFingerprint is the prior-snapshot full fingerprint a client carried
	// in via `?prior_fingerprint=<hex>` when running an across-snapshot prefix
	// check. Empty when not supplied.
	PriorFingerprint string `json:"prior_fingerprint,omitempty"`
}

// threadAuditReport is the structured PASS/FAIL the /threads/{id}/audit endpoint
// returns. Pass is true iff membership is consistent, counts are conserved, the
// hash multisets match, the order is strictly monotonic + unique, no title
// bleed was found, the composer-body axis is conformant, and the
// prefix-stability invariant holds. Warnings never affect Pass.
type threadAuditReport struct {
	ThreadID        string                     `json:"thread_id"`
	Pass            bool                       `json:"pass"`
	Members         []string                   `json:"members"`
	Counts          threadAuditCounts          `json:"counts"`
	Order           threadAuditOrder           `json:"order"`
	Hash            threadAuditHash            `json:"hash"`
	ComposerBody    threadAuditComposerBody    `json:"composer_body"`
	PrefixStability threadAuditPrefixStability `json:"prefix_stability"`
	Warnings        []threadAuditWarning       `json:"warnings,omitempty"`
	Divergences     []threadAuditDivergence    `json:"divergences,omitempty"`
}

// threadAuditOptions carries the optional inputs an HTTP caller can pass via
// query string to scope the audit to a real polling point (`?since=`,
// `?prior_fingerprint=`). The default zero-value runs the same exhaustive
// fingerprint+prefix check the unit tests exercise, so the endpoint without
// query params remains the rubric's "full reconciliation".
type threadAuditOptions struct {
	// Since, when non-zero, slices the prefix-stability axis at this cursor:
	// the rendered entries with At <= Since must hash-identically to the
	// independently-derived expected entries' prefix at the same cursor.
	Since time.Time
	// PriorFingerprint is the full-fingerprint a prior audit run reported. When
	// non-empty, the prefix-stability axis additionally proves the current
	// snapshot's prefix at Since equals the prior snapshot's prefix at Since,
	// catching an earlier-entry reorder/vanish between the two polls.
	PriorFingerprint string
}

// threadEntryContentHash is the per-entry SHA256 content hash:
//
//	SHA256( kind | beadID | RFC3339Nano(At, UTC) | author | commentID | sha256(text) )
//
// Title is deliberately excluded — it is a rendering convenience that can drift
// without changing the event's identity (title fidelity is checked separately,
// against the member body, so a sibling/head bleed is still caught). Timestamps
// are UTC-normalised so two equal instants in different locations hash equally.
// The text is pre-hashed so an arbitrarily long comment body collapses to a
// fixed-width field and field separators can never be forged by body content.
func threadEntryContentHash(kind, beadID string, at time.Time, author string, commentID int64, text string) string {
	textHash := sha256.Sum256([]byte(text))
	payload := kind + "|" + beadID + "|" + at.UTC().Format(time.RFC3339Nano) + "|" +
		author + "|" + strconv.FormatInt(commentID, 10) + "|" + hex.EncodeToString(textHash[:])
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// threadEntryHash hashes a fused timeline entry via threadEntryContentHash.
func threadEntryHash(e threadTimelineEntry) string {
	return threadEntryContentHash(e.Kind, e.BeadID, e.At, e.Author, e.CommentID, e.Text)
}

// entryRef projects a fused entry into a compact audit reference (hash included).
func threadAuditRef(e threadTimelineEntry) threadAuditEntryRef {
	return threadAuditEntryRef{
		Kind:      e.Kind,
		BeadID:    e.BeadID,
		At:        e.At.UTC().Format(time.RFC3339Nano),
		Author:    e.Author,
		CommentID: e.CommentID,
		Title:     e.BeadTitle,
		Hash:      threadEntryHash(e),
	}
}

// auditThreadTimelineWithOptions is the cursor-aware audit entry point. The
// options carry the `?since=<rfc3339>` cursor and the optional
// `?prior_fingerprint=<hex>` a long-running client uses to prove
// across-snapshot prefix stability (rubric O5).
func auditThreadTimelineWithOptions(ctx context.Context, store threadTimelineStore, headID string, opts threadAuditOptions) (threadAuditReport, error) {
	report := threadAuditReport{
		ThreadID: headID,
		Counts:   threadAuditCounts{RenderedByKind: map[string]int{}, ExpectedByKind: map[string]int{}},
		// ComposerBody starts conformant for an empty thread (no messages to
		// disprove the contract) and PrefixStability starts stable (the empty
		// fingerprint is a trivial prefix of itself). Both are downgraded if any
		// member fails the corresponding gate below.
		ComposerBody:    threadAuditComposerBody{Conformant: true},
		PrefixStability: threadAuditPrefixStability{Stable: true, Cursor: threadAuditFormatCursor(opts.Since), PriorFingerprint: opts.PriorFingerprint},
	}
	if headID == "" {
		report.Pass = true
		return report, nil
	}

	// (1) The thing under test: the fuser's rendered timeline.
	tl, err := fuseThreadTimeline(ctx, store, headID)
	if err != nil {
		return report, err
	}
	report.Members = tl.Members

	// (2) Independent membership re-derivation. The fuser's tl.Members must
	// equal an independent threadMembers call exactly (set AND dep-first order),
	// or the fused stream is built over the wrong roster — a membership
	// divergence that everything downstream inherits. Resolve the auditor's OWN
	// field-query snapshot here (its own independent read) and thread it into the
	// derivations below, so the auditor applies the SAME BatchGetBeads-miss
	// fallback the fuser does (ka-0000006057 gap 3) and the two agree by
	// construction — while still catching a real fuser bug via the separate reads.
	expectedMembers, fieldSnap, err := threadMembersWithFieldSnapshot(ctx, store, headID)
	if err != nil {
		return report, err
	}
	if d, ok := threadAuditDiffMembers(expectedMembers, tl.Members); !ok {
		report.Divergences = append(report.Divergences, d)
	}

	// (3) Independent entry re-derivation: a fresh body + comment read over the
	// independently-resolved roster, producing the EXPECTED entry set and the
	// count terms (N, K, M) plus the input-degradation warnings. The expected
	// set also drives the composer-body (C4/K4) and prefix-stability (O5) axes —
	// the C4 check needs the source-of-truth bead body (fields[text] + CreatedBy)
	// the same independent read carries.
	expected, counts, warnings, err := threadAuditDeriveExpected(ctx, store, expectedMembers, fieldSnap)
	if err != nil {
		return report, err
	}
	report.Warnings = warnings

	// (4) COUNT axis — conservation identity + per-kind + per-member.
	counts.RenderedTotal = len(tl.Entries)
	for _, e := range tl.Entries {
		counts.RenderedByKind[e.Kind]++
	}
	counts.Conserved = counts.RenderedTotal == counts.ExpectedTotal &&
		counts.RenderedByKind[threadEntryBeadCreated] == counts.ExpectedByKind[threadEntryBeadCreated] &&
		counts.RenderedByKind[threadEntryComment] == counts.ExpectedByKind[threadEntryComment] &&
		counts.RenderedByKind[threadEntryBeadClosed] == counts.ExpectedByKind[threadEntryBeadClosed] &&
		counts.RenderedByKind[threadEntryMessage] == counts.ExpectedByKind[threadEntryMessage]
	report.Counts = counts
	if !counts.Conserved {
		report.Divergences = append(report.Divergences, threadAuditDivergence{
			Axis: threadAuditAxisCount,
			Code: "count_not_conserved",
			Detail: fmt.Sprintf("rendered=%d expected=%d (members=%d comments=%d closable=%d messages=%d); by-kind rendered=%v expected=%v",
				counts.RenderedTotal, counts.ExpectedTotal, counts.MembersWithBody, counts.Comments, counts.Closable, counts.Messages,
				counts.RenderedByKind, counts.ExpectedByKind),
		})
	}
	report.Divergences = append(report.Divergences, threadAuditPerMemberCountDivergences(expected, tl.Entries)...)

	// (5) HASH axis — multiset reconciliation of per-entry content hashes.
	report.Hash = threadAuditReconcileHashes(expected, tl.Entries)

	// (6) TITLE fidelity — every rendered entry's title must match its member's
	// canonical body title (or empty for a bodyless member): no sibling/head
	// bleed. Checked against the independently-derived expected set.
	report.Divergences = append(report.Divergences, threadAuditTitleDivergences(tl.Entries, expected)...)

	// (7) ORDER axis — strict-monotonic + unique 4-tuple + fingerprint.
	report.Order = threadAuditCheckOrder(tl.Entries, tl.Members)
	if !report.Order.StrictlyMonotonic || !report.Order.Unique {
		report.Divergences = append(report.Divergences, threadAuditOrderDivergences(tl.Entries, tl.Members)...)
	}

	// (8) COMPOSER BODY axis (rubric C4/K4, ka-0000003162 3-4) — every
	// type=message thread-say member's full fields[text] must render as a
	// conversation line (no Title truncation, no Title fallback), and the
	// operator-email CreatedBy must land on Author. This catches a regression in
	// fuseThreadTimeline that would substitute the 500-char Title for a >500-char
	// body, or that would mis-attribute a composer reply to the bridge instead of
	// the operator who hit Send.
	composerBody, composerDivs, err := threadAuditCheckComposerBody(ctx, store, tl.Entries, expectedMembers, fieldSnap)
	if err != nil {
		return report, err
	}
	report.ComposerBody = composerBody
	report.Divergences = append(report.Divergences, composerDivs...)

	// (9) PREFIX-STABILITY axis (rubric O5, ka-0000003162 3-4) — the per-entry
	// hash sequence in canonical order is the "ordered id+tuple fingerprint" the
	// rubric demands; any `?since=` incremental response must be an exact prefix
	// of that sequence. The auditor compares the fuser's rendered hash sequence
	// against the independently-derived expected sequence position-by-position
	// (catching an out-of-order or vanished EARLIER entry the hash multiset axis
	// would only see as a multiset-level fabrication+drop pair). When opts.Since
	// is set, the check is sharpened at that cursor; when opts.PriorFingerprint
	// is also set, the prior snapshot's prefix at the cursor must match the
	// current snapshot's prefix — the across-snapshot stability invariant.
	//
	// threadAuditDeriveExpected returns entries in per-member-iteration order
	// (the same shape the count/hash axes consume); the prefix axis re-sorts
	// them through the SAME canonical comparator the fuser uses so the position-
	// by-position comparison reads against the fuser's actual emission order. The
	// hash multiset axis catches any membership-level error; this axis is the
	// position-level proof that the emission order itself is stable.
	canonicalExpected := threadAuditCanonicalize(expected, tl.Members)
	report.PrefixStability, _ = threadAuditCheckPrefixStability(tl.Entries, canonicalExpected, opts)
	if !report.PrefixStability.Stable {
		report.Divergences = append(report.Divergences, threadAuditPrefixStabilityDivergences(tl.Entries, canonicalExpected, opts)...)
	}

	report.Pass = report.Counts.Conserved && report.Hash.Matched &&
		report.Order.StrictlyMonotonic && report.Order.Unique &&
		report.ComposerBody.Conformant && report.PrefixStability.Stable &&
		!threadAuditHasFailingDivergence(report.Divergences)
	return report, nil
}

// threadAuditDiffMembers compares the independently-resolved membership against
// the fuser's. Returns ok=false plus a divergence when they differ in set or in
// order.
func threadAuditDiffMembers(expected, rendered []string) (threadAuditDivergence, bool) {
	mismatch := len(expected) != len(rendered)
	if !mismatch {
		for i := range expected {
			if expected[i] != rendered[i] {
				mismatch = true
				break
			}
		}
	}
	if !mismatch {
		return threadAuditDivergence{}, true
	}
	return threadAuditDivergence{
		Axis:   threadAuditAxisMembership,
		Code:   "membership_mismatch",
		Detail: fmt.Sprintf("independent membership %v != fused membership %v", expected, rendered),
	}, false
}

// threadAuditDeriveExpected re-reads the member bodies and comments and produces
// the EXPECTED entry set (what a correct fuser must emit), the count terms, and
// the input-degradation warnings. It deliberately mirrors fuseThreadTimeline's
// per-member emission rules — but reached via its OWN daemon reads — so a real
// fuser bug (a drop, a fabrication, a mis-attribution) shows up as a divergence
// between this expected set and the fuser's output rather than being masked.
func threadAuditDeriveExpected(ctx context.Context, store threadTimelineStore, members []string, fieldSnap map[string]*beadsapi.BeadDetail) ([]threadTimelineEntry, threadAuditCounts, []threadAuditWarning, error) {
	counts := threadAuditCounts{RenderedByKind: map[string]int{}, ExpectedByKind: map[string]int{}}
	var expected []threadTimelineEntry
	var warnings []threadAuditWarning
	if len(members) == 0 {
		return expected, counts, warnings, nil
	}

	beads, err := store.BatchGetBeads(ctx, members)
	if err != nil {
		return nil, counts, nil, err
	}
	// Apply the same field-query fallback the fuser does, so a member the batch
	// fetch raced past is derived from the body the field query returned rather
	// than diverging as a fabricated/dropped entry (ka-0000006057 gap 3).
	beads = mergeFieldSnapshotFallback(beads, fieldSnap)

	for _, id := range members {
		b := beads[id]

		// A type=message member is conversation, not work (2-5, ka-0000006034).
		// fuseThreadTimeline emits EXACTLY one message entry for it — placed at
		// created_at, authored by CreatedBy, carrying threadMessageText — and
		// skips both the created/closed work pair AND the comment fetch. The
		// auditor must derive the SAME single entry, reached via its own bead
		// read, or a correct composer reply diverges as a fabricated message +
		// dropped created/closed against the ka-0000005790 reconciliation. This
		// branch is checked before the nil guard because isThreadMessageBead is
		// false for a nil body, so a daemon-missing member never lands here.
		if isThreadMessageBead(b) {
			counts.Messages++
			expected = append(expected, threadTimelineEntry{
				At: b.CreatedAt, Kind: threadEntryMessage, BeadID: b.ID, BeadTitle: b.Title, Author: b.CreatedBy, Text: threadMessageText(b),
			})
			continue
		}

		var beadTitle string
		if b != nil {
			beadTitle = b.Title
			counts.MembersWithBody++
			expected = append(expected, threadTimelineEntry{
				At: b.CreatedAt, Kind: threadEntryBeadCreated, BeadID: b.ID, BeadTitle: b.Title, Author: b.CreatedBy,
			})
			if b.Status == "closed" {
				counts.Closable++
				at, imputed := threadClosedAtOrImpute(b.ClosedAt, b.CreatedAt)
				expected = append(expected, threadTimelineEntry{
					At: at, Kind: threadEntryBeadClosed, BeadID: b.ID, BeadTitle: b.Title, Author: b.ClosedBy, ClosedAtImputed: imputed,
				})
				if imputed {
					warnings = append(warnings, threadAuditWarning{
						Code: threadAuditWarnUnparseableClosedAt, BeadID: b.ID,
						Detail: fmt.Sprintf("closed member has empty/unparseable closed_at %q; close event imputed to created_at, not dropped", b.ClosedAt),
					})
				}
			}
		} else {
			warnings = append(warnings, threadAuditWarning{
				Code: threadAuditWarnMissingMemberBody, BeadID: id,
				Detail: "member resolved by membership but BatchGetBeads returned no body; no created/closed entry, but its comments must still appear",
			})
		}

		comments, cErr := store.GetComments(ctx, id)
		if cErr != nil {
			return nil, counts, nil, cErr
		}
		for _, c := range comments {
			counts.Comments++
			expected = append(expected, threadTimelineEntry{
				At: c.CreatedAt, Kind: threadEntryComment, BeadID: id, BeadTitle: beadTitle, Author: c.Author, Text: c.Text, CommentID: c.ID,
			})
		}
	}

	counts.ExpectedTotal = counts.MembersWithBody + counts.Comments + counts.Closable + counts.Messages
	counts.ExpectedByKind[threadEntryBeadCreated] = counts.MembersWithBody
	counts.ExpectedByKind[threadEntryComment] = counts.Comments
	counts.ExpectedByKind[threadEntryBeadClosed] = counts.Closable
	counts.ExpectedByKind[threadEntryMessage] = counts.Messages
	return expected, counts, warnings, nil
}

// threadAuditReconcileHashes compares the rendered and expected per-entry hash
// multisets. A hash whose rendered count exceeds its expected count yields one
// fabrication ref per excess; the reverse yields one drop ref per shortfall.
func threadAuditReconcileHashes(expected, rendered []threadTimelineEntry) threadAuditHash {
	expCount := map[string]int{}
	expRef := map[string]threadAuditEntryRef{}
	for _, e := range expected {
		h := threadEntryHash(e)
		expCount[h]++
		expRef[h] = threadAuditRef(e)
	}
	renCount := map[string]int{}
	renRef := map[string]threadAuditEntryRef{}
	for _, e := range rendered {
		h := threadEntryHash(e)
		renCount[h]++
		renRef[h] = threadAuditRef(e)
	}

	out := threadAuditHash{Matched: true}
	// Fabrications: rendered in excess of expected.
	for h, rc := range renCount {
		if extra := rc - expCount[h]; extra > 0 {
			out.Matched = false
			for range extra {
				out.Fabrications = append(out.Fabrications, renRef[h])
			}
		}
	}
	// Drops: expected in excess of rendered.
	for h, ec := range expCount {
		if missing := ec - renCount[h]; missing > 0 {
			out.Matched = false
			for range missing {
				out.Drops = append(out.Drops, expRef[h])
			}
		}
	}
	threadAuditSortRefs(out.Fabrications)
	threadAuditSortRefs(out.Drops)
	return out
}

// threadAuditPerMemberCountDivergences reports, per member, any mismatch between
// the rendered and expected entry counts for that member — the per-member half
// of the conservation gate (rubric C1/C6).
func threadAuditPerMemberCountDivergences(expected, rendered []threadTimelineEntry) []threadAuditDivergence {
	exp := map[string]int{}
	for _, e := range expected {
		exp[e.BeadID]++
	}
	ren := map[string]int{}
	for _, e := range rendered {
		ren[e.BeadID]++
	}
	seen := map[string]bool{}
	var ids []string
	for id := range exp {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for id := range ren {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	var out []threadAuditDivergence
	for _, id := range ids {
		if exp[id] != ren[id] {
			out = append(out, threadAuditDivergence{
				Axis: threadAuditAxisCount, Code: "member_count_mismatch", BeadID: id,
				Detail: fmt.Sprintf("member %s: rendered %d entries, expected %d", id, ren[id], exp[id]),
			})
		}
	}
	return out
}

// threadAuditTitleDivergences checks title fidelity: every rendered entry's
// BeadTitle must equal its member's canonical body title (or empty when the
// member has no body), so a sibling's or the head's title can never bleed onto
// another member's entry (rubric K5).
//
// The canonical per-member title is taken from the INDEPENDENTLY-derived
// expected set: each member-with-body contributes exactly one bead_created entry
// (or, for a type=message member, one message entry — 2-5, ka-0000006034)
// carrying its real title; a bodyless member contributes none, so its canonical
// title is "" (its surviving comments must carry an empty title, not a sibling's
// — the C6 bug fix). Deriving wantTitle from `expected` rather than a fresh
// BatchGetBeads keeps this a pure function (no daemon, no ctx) and avoids a
// redundant round-trip, while still comparing against a source the fuser did
// not produce.
func threadAuditTitleDivergences(rendered, expected []threadTimelineEntry) []threadAuditDivergence {
	wantTitle := map[string]string{}
	for _, e := range expected {
		// bead_created is the title carrier for a work member; a type=message
		// member carries no bead_created, so its single message entry is the
		// canonical title source — include both, or a correct composer reply
		// reads as a title bleed (the message entry's real title vs a "" want).
		if e.Kind == threadEntryBeadCreated || e.Kind == threadEntryMessage {
			wantTitle[e.BeadID] = e.BeadTitle
		}
	}
	var out []threadAuditDivergence
	for _, e := range rendered {
		if e.BeadTitle != wantTitle[e.BeadID] {
			out = append(out, threadAuditDivergence{
				Axis: threadAuditAxisTitle, Code: "title_bleed", BeadID: e.BeadID,
				Detail:  fmt.Sprintf("entry on %s carries title %q, want %q", e.BeadID, e.BeadTitle, wantTitle[e.BeadID]),
				Entries: []threadAuditEntryRef{threadAuditRef(e)},
			})
		}
	}
	return out
}

// threadAuditTuple is the canonical 4-tuple a fused entry is ordered by.
type threadAuditTuple struct {
	at        time.Time
	pos       int
	kindRank  int
	commentID int64
}

func threadAuditTupleOf(e threadTimelineEntry, pos map[string]int) threadAuditTuple {
	return threadAuditTuple{at: e.At, pos: pos[e.BeadID], kindRank: threadEntryKindRank(e.Kind), commentID: e.CommentID}
}

// threadAuditTupleCmp returns -1/0/+1 comparing two canonical tuples in
// (At, pos, kindRank, CommentID) order.
func threadAuditTupleCmp(a, b threadAuditTuple) int {
	if !a.at.Equal(b.at) {
		if a.at.Before(b.at) {
			return -1
		}
		return 1
	}
	if a.pos != b.pos {
		if a.pos < b.pos {
			return -1
		}
		return 1
	}
	if a.kindRank != b.kindRank {
		if a.kindRank < b.kindRank {
			return -1
		}
		return 1
	}
	if a.commentID != b.commentID {
		if a.commentID < b.commentID {
			return -1
		}
		return 1
	}
	return 0
}

// threadAuditCheckOrder verifies the rendered stream is strictly increasing and
// unique in the canonical 4-tuple, and computes the order fingerprint (SHA256
// over the newline-joined per-entry hashes in rendered order).
func threadAuditCheckOrder(entries []threadTimelineEntry, members []string) threadAuditOrder {
	pos := make(map[string]int, len(members))
	for i, id := range members {
		pos[id] = i
	}
	out := threadAuditOrder{StrictlyMonotonic: true, Unique: true}
	for i := 1; i < len(entries); i++ {
		cmp := threadAuditTupleCmp(threadAuditTupleOf(entries[i-1], pos), threadAuditTupleOf(entries[i], pos))
		switch {
		case cmp == 0:
			out.Unique = false
		case cmp > 0:
			out.StrictlyMonotonic = false
		}
	}

	h := sha256.New()
	for i, e := range entries {
		if i > 0 {
			h.Write([]byte("\n"))
		}
		h.Write([]byte(threadEntryHash(e)))
	}
	out.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return out
}

// threadAuditOrderDivergences emits a divergence for each adjacent pair that is
// out of order (rendered tuple_{i-1} > tuple_i) or non-unique (equal tuples).
func threadAuditOrderDivergences(entries []threadTimelineEntry, members []string) []threadAuditDivergence {
	pos := make(map[string]int, len(members))
	for i, id := range members {
		pos[id] = i
	}
	var out []threadAuditDivergence
	for i := 1; i < len(entries); i++ {
		prev, cur := entries[i-1], entries[i]
		cmp := threadAuditTupleCmp(threadAuditTupleOf(prev, pos), threadAuditTupleOf(cur, pos))
		if cmp == 0 {
			out = append(out, threadAuditDivergence{
				Axis: threadAuditAxisOrder, Code: "duplicate_tuple",
				Detail:  fmt.Sprintf("entries %d and %d share an identical canonical 4-tuple (ambiguous order)", i-1, i),
				Entries: []threadAuditEntryRef{threadAuditRef(prev), threadAuditRef(cur)},
			})
		} else if cmp > 0 {
			out = append(out, threadAuditDivergence{
				Axis: threadAuditAxisOrder, Code: "out_of_order",
				Detail:  fmt.Sprintf("entry %d sorts before entry %d in the canonical 4-tuple", i, i-1),
				Entries: []threadAuditEntryRef{threadAuditRef(prev), threadAuditRef(cur)},
			})
		}
	}
	return out
}

// threadAuditHasFailingDivergence reports whether any divergence on a
// PASS-affecting axis is present. Title, membership, composer-body, and
// prefix-stability divergences fail the audit; count/order/hash are already
// folded into Pass via their booleans, but the per-member count divergences
// live only here, so count is included too.
func threadAuditHasFailingDivergence(ds []threadAuditDivergence) bool {
	for _, d := range ds {
		switch d.Axis {
		case threadAuditAxisMembership, threadAuditAxisTitle, threadAuditAxisCount, threadAuditAxisOrder,
			threadAuditAxisComposerBody, threadAuditAxisPrefixStability:
			return true
		}
	}
	return false
}

// threadAuditCanonicalize re-sorts an entry slice through the SAME canonical
// comparator fuseThreadTimeline uses (At asc, dep-first pos[BeadID] asc,
// kindRank asc, CommentID asc) so a position-by-position comparison against the
// fuser's output is meaningful. threadAuditDeriveExpected returns entries in
// per-member-iteration order — convenient for the count/hash axes (which
// reduce over multisets) but NOT directly comparable to the fuser's emission
// order. The canonical re-sort is what makes a position-level prefix gate (O5)
// possible.
//
// A members slice that does not include some BeadID maps that bead to position
// 0 (the head's slot) via the zero-value of the map lookup — harmless because
// the expected set never contains entries on beads outside `members` (it is
// derived FROM `members`).
func threadAuditCanonicalize(entries []threadTimelineEntry, members []string) []threadTimelineEntry {
	pos := make(map[string]int, len(members))
	for i, id := range members {
		pos[id] = i
	}
	out := make([]threadTimelineEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
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
	return out
}

// threadAuditFormatCursor renders a since-cursor as the RFC3339Nano UTC string
// the JSON report carries (empty when the cursor is the zero time).
func threadAuditFormatCursor(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// threadAuditCheckComposerBody is the COMPOSER-BODY axis (rubric C4/K4,
// ka-0000003162 3-4). For every type=message thread-say member, the rendered
// message entry MUST:
//
//  1. Exist exactly once (no drop, no duplicate). The composer writes one bead;
//     the fuser emits one entry; the auditor proves the 1:1 mapping.
//  2. Carry the FULL fields[text] body in entry.Text — not the 500-char Title,
//     not a strict-prefix truncation, not a substituted fallback string. The
//     composer writer (thread_compose.postThreadSayMessage) deliberately
//     truncates the bead Title to 500 chars to satisfy the daemon's title
//     contract while writing the full operator-typed body into fields[text]; a
//     long reply (>500 chars) therefore relies on the renderer reading
//     fields[text], not Title. A drop (Text=="" when fields[text]!="") or a
//     truncation (rendered Text is a strict prefix of fields[text], the
//     signature of a Title-fallback bug) is flagged with its own divergence
//     code so the regression is named, not lumped under "hash mismatch".
//  3. Attribute Author to the operator email the bead carries in CreatedBy. The
//     composer stamps CreatedBy with the operator's email at write time; if the
//     fuser bleeds a different author (the bridge, an agent), the message
//     reads as having been said by the wrong person.
//
// A type=message member whose fields[text] is EMPTY is allowed: the fuser falls
// back to the Title (threadMessageText), which is exactly the right behaviour
// for a title-only message bead (a path that wrote the body only in the title).
// The axis only fires when fields[text] is the source of truth — i.e. non-empty.
//
// The function issues no daemon reads beyond a BatchGetBeads over the
// already-resolved membership; the bead bodies are the source-of-truth for
// CreatedBy and fields[text].
func threadAuditCheckComposerBody(ctx context.Context, store threadTimelineStore, rendered []threadTimelineEntry, members []string, fieldSnap map[string]*beadsapi.BeadDetail) (threadAuditComposerBody, []threadAuditDivergence, error) {
	out := threadAuditComposerBody{Conformant: true}
	if len(members) == 0 {
		return out, nil, nil
	}

	// Index rendered message entries by bead id. A correct fuser emits exactly
	// one message entry per type=message member; the audit shape catches both a
	// drop (zero entries) and a duplicate (>1 entries).
	byMessageBead := map[string][]threadTimelineEntry{}
	for _, e := range rendered {
		if e.Kind == threadEntryMessage {
			byMessageBead[e.BeadID] = append(byMessageBead[e.BeadID], e)
		}
	}

	beads, err := store.BatchGetBeads(ctx, members)
	if err != nil {
		return out, nil, err
	}
	// Same field-query fallback as the fuser: a type=message member the batch
	// omits is still recognised via the field-query body (ka-0000006057 gap 3).
	beads = mergeFieldSnapshotFallback(beads, fieldSnap)

	var divs []threadAuditDivergence
	for _, id := range members {
		b := beads[id]
		if !isThreadMessageBead(b) {
			continue
		}
		out.Members++
		gotEntries := byMessageBead[id]

		// (1) Exactly one entry per type=message member.
		if len(gotEntries) == 0 {
			out.Conformant = false
			divs = append(divs, threadAuditDivergence{
				Axis: threadAuditAxisComposerBody, Code: threadAuditCodeComposerEntryMissing, BeadID: id,
				Detail: fmt.Sprintf("type=message member %s produced no message entry; the composer reply was dropped from the conversation", id),
			})
			continue
		}
		if len(gotEntries) > 1 {
			out.Conformant = false
			refs := make([]threadAuditEntryRef, 0, len(gotEntries))
			for _, e := range gotEntries {
				refs = append(refs, threadAuditRef(e))
			}
			divs = append(divs, threadAuditDivergence{
				Axis: threadAuditAxisComposerBody, Code: threadAuditCodeComposerMultipleEntries, BeadID: id,
				Detail:  fmt.Sprintf("type=message member %s produced %d message entries; want exactly 1", id, len(gotEntries)),
				Entries: refs,
			})
			continue
		}
		got := gotEntries[0]

		// (2) Body completeness, scoped to members whose fields[text] is the
		// authoritative source. A non-empty fields[text] MUST land verbatim on
		// entry.Text. A drop (got.Text=="") is the most direct failure; a strict
		// prefix is the signature of the Title-fallback regression (the renderer
		// substituted the 500-char Title for the full body); anything else that
		// differs is a generic mismatch.
		fieldsText := ""
		if b != nil {
			fieldsText = strings.TrimSpace(b.Fields["text"])
		}
		if fieldsText != "" && got.Text != fieldsText {
			out.Conformant = false
			switch {
			case got.Text == "":
				divs = append(divs, threadAuditDivergence{
					Axis: threadAuditAxisComposerBody, Code: threadAuditCodeComposerBodyDropped, BeadID: id,
					Detail:  fmt.Sprintf("type=message member %s has fields[text] of %d bytes but rendered entry text is empty; the composer body was dropped", id, len(fieldsText)),
					Entries: []threadAuditEntryRef{threadAuditRef(got)},
				})
			case len(got.Text) < len(fieldsText) && strings.HasPrefix(fieldsText, got.Text):
				divs = append(divs, threadAuditDivergence{
					Axis: threadAuditAxisComposerBody, Code: threadAuditCodeComposerBodyTruncated, BeadID: id,
					Detail:  fmt.Sprintf("type=message member %s rendered text is a %d-byte strict prefix of the %d-byte fields[text]; the Title (truncated by postThreadSayMessage) was substituted for the full body — a long composer reply will read as the 500-char Title, not the body", id, len(got.Text), len(fieldsText)),
					Entries: []threadAuditEntryRef{threadAuditRef(got)},
				})
			default:
				divs = append(divs, threadAuditDivergence{
					Axis: threadAuditAxisComposerBody, Code: threadAuditCodeComposerBodyMismatch, BeadID: id,
					Detail:  fmt.Sprintf("type=message member %s rendered text (%d bytes) does not match fields[text] (%d bytes) and is not a strict prefix; the composer body was rewritten", id, len(got.Text), len(fieldsText)),
					Entries: []threadAuditEntryRef{threadAuditRef(got)},
				})
			}
		}

		// (3) Operator attribution. CreatedBy is the source-of-truth: the
		// composer stamps it with the operator's email; the bridge stamps it
		// when it forwards a Slack message. The fuser MUST copy it onto
		// entry.Author verbatim. An empty CreatedBy is allowed (a legacy bead or
		// a daemon path that omitted the actor) — it would just render as "—" in
		// the conversation lane — but a non-empty CreatedBy that doesn't match is
		// an actor bleed that would mis-attribute the reply.
		wantAuthor := ""
		if b != nil {
			wantAuthor = b.CreatedBy
		}
		if wantAuthor != "" && got.Author != wantAuthor {
			out.Conformant = false
			divs = append(divs, threadAuditDivergence{
				Axis: threadAuditAxisComposerBody, Code: threadAuditCodeComposerActorMismatch, BeadID: id,
				Detail:  fmt.Sprintf("type=message member %s rendered author %q does not match CreatedBy %q; the composer reply was mis-attributed", id, got.Author, wantAuthor),
				Entries: []threadAuditEntryRef{threadAuditRef(got)},
			})
		}

	}

	// Sort divergences by bead id for byte-stable JSON reporting (map iteration
	// over byMessageBead is non-deterministic; the per-member loop is ordered by
	// `members`, so this is belt-and-braces in case a caller passes an unordered
	// roster).
	sort.SliceStable(divs, func(i, j int) bool {
		if divs[i].BeadID != divs[j].BeadID {
			return divs[i].BeadID < divs[j].BeadID
		}
		return divs[i].Code < divs[j].Code
	})
	return out, divs, nil
}

// threadAuditCheckPrefixStability is the PREFIX-STABILITY axis (rubric O5,
// ka-0000003162 3-4). The "ordered id+tuple fingerprint" the rubric demands is
// the per-entry hash sequence taken in canonical order — exactly the pre-image
// of threadAuditOrder.Fingerprint. Stability has two flavours:
//
//   - WITHIN A SINGLE SNAPSHOT (the default, no cursor): the fuser's rendered
//     hash sequence must equal the independently-derived expected hash sequence
//     position-by-position. This catches an out-of-order or vanished EARLIER
//     entry that the hash MULTISET axis would only see as a fabrication+drop
//     pair (two divergences that, taken individually, can't be distinguished
//     from a legitimate substitution). A position-by-position check pins the
//     failure to the row, which is the prefix invariant in its strongest form.
//
//   - ACROSS SNAPSHOTS (opts.Since + opts.PriorFingerprint): a client that
//     polled at time T1 with cursor C and carries the T1 full fingerprint
//     forward can prove that the T2 snapshot's prefix up to cursor C still
//     matches T1's prefix at C. The auditor:
//     (a) takes the rendered prefix (entries with At <= C),
//     (b) re-canonicalises the expected prefix at C,
//     (c) checks they hash-prefix-equal,
//     (d) when PriorFingerprint is supplied, also proves the T1 fingerprint's
//     prefix at C equals the current prefix's fingerprint.
//
// Within-snapshot stability is a deterministic property of the fuser (and is
// effectively re-proved by the hash multiset axis when memberships match), so
// the default-path failure mode here is almost always "the hash axis is also
// failing on the same entries". The cursor-bound flavour is the production
// gate: it lets a real polling client (or a gardener audit script) carry the
// prior fingerprint forward and prove stability across the wall-clock interval
// between two polls.
//
// Returns the report block + an internal helper signal (the rendered prefix
// length the divergences helper needs).
func threadAuditCheckPrefixStability(rendered, expected []threadTimelineEntry, opts threadAuditOptions) (threadAuditPrefixStability, int) {
	out := threadAuditPrefixStability{
		Stable:           true,
		Cursor:           threadAuditFormatCursor(opts.Since),
		PriorFingerprint: opts.PriorFingerprint,
	}

	// Within-snapshot full check: position-by-position hash equality between
	// rendered and expected. A length mismatch is already a count/multiset
	// failure, but the prefix gate STILL fires (the rendered sequence cannot be
	// a prefix-extension of itself when an earlier expected entry is missing).
	maxN := min(len(rendered), len(expected))
	for i := range maxN {
		if threadEntryHash(rendered[i]) != threadEntryHash(expected[i]) {
			out.Stable = false
			break
		}
	}
	if len(rendered) != len(expected) {
		out.Stable = false
	}

	// Cursor-bound check. Compute the rendered-prefix length (entries with At
	// <= Since) and prove it hash-equal to the expected prefix at the same
	// cursor. Then, if a prior fingerprint was supplied, prove the current
	// prefix's fingerprint matches it.
	if !opts.Since.IsZero() {
		renPrefixLen := threadAuditPrefixCount(rendered, opts.Since)
		expPrefixLen := threadAuditPrefixCount(expected, opts.Since)
		out.PrefixLength = renPrefixLen
		if renPrefixLen != expPrefixLen {
			out.Stable = false
		} else {
			for i := range renPrefixLen {
				if threadEntryHash(rendered[i]) != threadEntryHash(expected[i]) {
					out.Stable = false
					break
				}
			}
		}
		if out.PriorFingerprint != "" {
			gotFP := threadAuditPrefixFingerprint(rendered[:renPrefixLen])
			if gotFP != out.PriorFingerprint {
				out.Stable = false
			}
		}
	}

	return out, threadAuditPrefixCount(rendered, opts.Since)
}

// threadAuditPrefixStabilityDivergences attaches the offending pair refs when
// the prefix invariant breaks. Position-by-position diffs cap at 16 entries so
// a wildly-broken thread does not balloon the JSON report.
func threadAuditPrefixStabilityDivergences(rendered, expected []threadTimelineEntry, opts threadAuditOptions) []threadAuditDivergence {
	var out []threadAuditDivergence
	maxN := min(len(rendered), len(expected))
	mismatches := 0
	for i := range maxN {
		if threadEntryHash(rendered[i]) == threadEntryHash(expected[i]) {
			continue
		}
		mismatches++
		if mismatches > 16 {
			break
		}
		out = append(out, threadAuditDivergence{
			Axis: threadAuditAxisPrefixStability, Code: threadAuditCodePrefixHashMismatch, BeadID: rendered[i].BeadID,
			Detail:  fmt.Sprintf("entry at position %d diverged: rendered (%s on %s) does not hash to the expected (%s on %s) — an earlier entry was reordered or rewritten between snapshots", i, rendered[i].Kind, rendered[i].BeadID, expected[i].Kind, expected[i].BeadID),
			Entries: []threadAuditEntryRef{threadAuditRef(rendered[i]), threadAuditRef(expected[i])},
		})
	}
	if !opts.Since.IsZero() {
		renPrefixLen := threadAuditPrefixCount(rendered, opts.Since)
		expPrefixLen := threadAuditPrefixCount(expected, opts.Since)
		if renPrefixLen < expPrefixLen {
			out = append(out, threadAuditDivergence{
				Axis: threadAuditAxisPrefixStability, Code: threadAuditCodePrefixLengthShortfall,
				Detail: fmt.Sprintf("rendered prefix at cursor %s has %d entries; expected %d — an earlier entry vanished from the snapshot", threadAuditFormatCursor(opts.Since), renPrefixLen, expPrefixLen),
			})
		}
	}
	return out
}

// threadAuditPrefixCount returns the count of entries with At <= cursor. The
// canonical order is non-decreasing in At, so this is the first-out-of-prefix
// position. A zero cursor yields the FULL length so an "all entries" prefix is
// the trivial case (and the same path covers a cursor that lies after every
// entry).
func threadAuditPrefixCount(entries []threadTimelineEntry, cursor time.Time) int {
	if cursor.IsZero() {
		return len(entries)
	}
	n := 0
	for _, e := range entries {
		if e.At.After(cursor) {
			break
		}
		n++
	}
	return n
}

// threadAuditPrefixFingerprint computes the order-fingerprint over a prefix:
// SHA256 over the newline-joined per-entry hashes, identical in construction
// to threadAuditCheckOrder's Fingerprint so the two are directly comparable.
// An empty prefix produces the canonical empty-hash fingerprint, which a
// client should carry through unchanged.
func threadAuditPrefixFingerprint(entries []threadTimelineEntry) string {
	h := sha256.New()
	for i, e := range entries {
		if i > 0 {
			h.Write([]byte("\n"))
		}
		h.Write([]byte(threadEntryHash(e)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// threadAuditSortRefs gives fabrication/drop lists a deterministic order so the
// JSON report is byte-stable across runs (the underlying multiset diff iterates
// maps).
func threadAuditSortRefs(refs []threadAuditEntryRef) {
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].At != refs[j].At {
			return refs[i].At < refs[j].At
		}
		if refs[i].BeadID != refs[j].BeadID {
			return refs[i].BeadID < refs[j].BeadID
		}
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		if refs[i].CommentID != refs[j].CommentID {
			return refs[i].CommentID < refs[j].CommentID
		}
		return refs[i].Hash < refs[j].Hash
	})
}
