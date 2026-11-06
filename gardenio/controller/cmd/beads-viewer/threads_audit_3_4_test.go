package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Phase 3 task 3-4 (ka-0000003162) — composer-body + prefix-stability audit
// gates. These suites prove the two NEW axes the audit endpoint now reports:
//
//   - COMPOSER BODY (rubric C4/K4): every type=message thread-say member's full
//     fields[text] renders as a conversation line; the 500-char Title is NEVER
//     substituted for the full body (a long composer reply must read verbatim);
//     CreatedBy operator-email lands on entry.Author.
//   - PREFIX STABILITY (rubric O5): the ordered per-entry hash sequence of any
//     `?since=` incremental response is an exact prefix-extension of the full-
//     fetch fingerprint for the same snapshot; an earlier entry that reorders
//     or vanishes between snapshots is caught by the axis.

// --- COMPOSER BODY axis (rubric C4/K4) -------------------------------------

// A thread carrying a >500-character composer message must PASS the audit:
// postThreadSayMessage truncates the bead Title to 500 chars but writes the
// full operator-typed body into fields[text], so the fuser MUST render the full
// body (the Title would silently truncate). This locks the composer-body gate
// to the SOURCE of the rubric C4/K4 contract.
func TestAudit_ComposerBody_LongBodyPasses(t *testing.T) {
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishL", "Wish", "gardenio", now, nil)
	f.beads["gd-wishL"].CreatedBy = "alice"

	// A 1000-character body: well beyond the 500-char Title cap that
	// postThreadSayMessage applies. The fuser MUST render the full fields[text].
	// No leading/trailing whitespace (threadMessageText TrimSpaces by design).
	longBody := strings.Repeat("the-operator-typed-a-very-long-message-line.", 25) // ~1100 chars
	if len(longBody) <= 500 {
		t.Fatalf("test fixture sanity: longBody is %d chars, want >500", len(longBody))
	}
	if longBody != strings.TrimSpace(longBody) {
		t.Fatalf("test fixture sanity: longBody should not have leading/trailing whitespace")
	}
	msg := f.addMemberByField("gd-msgL", truncate(longBody, 500), "gd-wishL",
		now.Add(time.Hour), now.Add(time.Hour))
	msg.Type = "message"
	msg.Status = "closed"
	msg.ClosedAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	msg.CreatedBy = "matt@example.com"
	msg.Fields["text"] = longBody

	rep, err := auditThreadTimeline(context.Background(), f, "gd-wishL")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("a long composer reply must PASS the audit: %+v", rep.Divergences)
	}
	if !rep.ComposerBody.Conformant || rep.ComposerBody.Members != 1 {
		t.Errorf("ComposerBody = %+v, want Conformant=true Members=1", rep.ComposerBody)
	}
}

// A fuser that renders the 500-char Title instead of the full fields[text] (a
// Title-fallback regression) must FAIL the audit with a body-truncation
// divergence pointing at the offending member. The synthetic check drives the
// pure helper directly so the failure is exercised independently of the (now
// correct) fuser.
func TestAudit_ComposerBody_TitleFallbackFlagged(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishT", "Wish", "gardenio", now, nil)
	f.beads["gd-wishT"].CreatedBy = "alice"

	longBody := strings.Repeat("a", 800)
	truncatedTitle := truncate(longBody, 500)
	msg := f.addMemberByField("gd-msgT", truncatedTitle, "gd-wishT", now, now)
	msg.Type = "message"
	msg.Fields["text"] = longBody
	msg.CreatedBy = "matt@example.com"

	members, err := threadMembers(ctx, f, "gd-wishT")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	// Simulate a fuser that emits the 500-char Title as entry.Text (the
	// regression the gate must catch).
	rendered := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-wishT", BeadTitle: "Wish", Author: "alice"},
		{At: now, Kind: threadEntryMessage, BeadID: "gd-msgT", BeadTitle: truncatedTitle, Author: "matt@example.com", Text: truncatedTitle},
	}

	got, divs, err := threadAuditCheckComposerBody(ctx, f, rendered, members, nil)
	if err != nil {
		t.Fatalf("checkComposerBody: %v", err)
	}
	if got.Conformant {
		t.Fatalf("Title-fallback rendering must NOT be conformant: %+v", got)
	}
	if len(divs) != 1 || divs[0].Code != threadAuditCodeComposerBodyTruncated {
		t.Fatalf("expected one composer_body_truncated divergence, got %+v", divs)
	}
	if divs[0].BeadID != "gd-msgT" {
		t.Errorf("divergence bead = %q, want gd-msgT", divs[0].BeadID)
	}
	if divs[0].Axis != threadAuditAxisComposerBody {
		t.Errorf("divergence axis = %q, want composer_body", divs[0].Axis)
	}
}

// A non-empty fields[text] absent from any rendered entry (a complete drop) is
// flagged with composer_body_dropped — the explicit C4 surface visibility:
// the audit names the regression, never lumps it as a generic hash mismatch.
func TestAudit_ComposerBody_DropFlagged(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishD", "Wish", "gardenio", now, nil)
	msg := f.addMemberByField("gd-msgD", "title", "gd-wishD", now, now)
	msg.Type = "message"
	msg.Fields["text"] = "the operator's actual message"
	msg.CreatedBy = "matt@example.com"

	members, err := threadMembers(ctx, f, "gd-wishD")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	// Simulate a fuser that emits a message entry with EMPTY text — the body
	// was dropped along the rendering path.
	rendered := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-wishD", BeadTitle: "Wish"},
		{At: now, Kind: threadEntryMessage, BeadID: "gd-msgD", BeadTitle: "title", Author: "matt@example.com", Text: ""},
	}

	got, divs, err := threadAuditCheckComposerBody(ctx, f, rendered, members, nil)
	if err != nil {
		t.Fatalf("checkComposerBody: %v", err)
	}
	if got.Conformant {
		t.Fatalf("a dropped composer body must NOT be conformant: %+v", got)
	}
	if len(divs) != 1 || divs[0].Code != threadAuditCodeComposerBodyDropped {
		t.Fatalf("expected one composer_body_dropped divergence, got %+v", divs)
	}
}

// A type=message member that produced NO message entry at all (the fuser
// dropped the entire row) is flagged with composer_entry_missing — distinct
// from a body drop because the conversation has lost the whole utterance.
func TestAudit_ComposerBody_MissingEntryFlagged(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishM", "Wish", "gardenio", now, nil)
	msg := f.addMemberByField("gd-msgM", "title", "gd-wishM", now, now)
	msg.Type = "message"
	msg.Fields["text"] = "the operator's reply"

	members, err := threadMembers(ctx, f, "gd-wishM")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	// Simulate a fuser that emitted NO message entry for the message member.
	rendered := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-wishM", BeadTitle: "Wish"},
	}

	got, divs, err := threadAuditCheckComposerBody(ctx, f, rendered, members, nil)
	if err != nil {
		t.Fatalf("checkComposerBody: %v", err)
	}
	if got.Conformant {
		t.Fatalf("a dropped message entry must NOT be conformant: %+v", got)
	}
	if len(divs) != 1 || divs[0].Code != threadAuditCodeComposerEntryMissing {
		t.Fatalf("expected one composer_entry_missing divergence, got %+v", divs)
	}
	if got.Members != 1 {
		t.Errorf("ComposerBody.Members = %d, want 1 (the message member is counted even though its entry is missing)", got.Members)
	}
}

// An actor bleed (entry.Author != CreatedBy operator email) is flagged with
// composer_actor_mismatch — the rubric "operator attribution CreatedBy ==
// operator-email -> entry.Author" gate.
func TestAudit_ComposerBody_ActorMismatchFlagged(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishA", "Wish", "gardenio", now, nil)
	msg := f.addMemberByField("gd-msgA", "msg", "gd-wishA", now, now)
	msg.Type = "message"
	msg.Fields["text"] = "operator says hi"
	msg.CreatedBy = "matt@example.com"

	members, err := threadMembers(ctx, f, "gd-wishA")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	// Author is wrong — a bleed from the bridge or an agent identity.
	rendered := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-wishA", BeadTitle: "Wish"},
		{At: now, Kind: threadEntryMessage, BeadID: "gd-msgA", BeadTitle: "msg", Author: "agent:wrong", Text: "operator says hi"},
	}

	got, divs, err := threadAuditCheckComposerBody(ctx, f, rendered, members, nil)
	if err != nil {
		t.Fatalf("checkComposerBody: %v", err)
	}
	if got.Conformant {
		t.Fatalf("an actor bleed must NOT be conformant: %+v", got)
	}
	if len(divs) != 1 || divs[0].Code != threadAuditCodeComposerActorMismatch {
		t.Fatalf("expected one composer_actor_mismatch divergence, got %+v", divs)
	}
}

// A title-only message bead (no fields[text]) is conformant: the fuser falls
// back to the Title, which is exactly the right behaviour. The gate must NOT
// fire on the empty-fields[text] case — only the source-of-truth has to match
// when there IS a source-of-truth.
func TestAudit_ComposerBody_EmptyFieldsTextIsConformant(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishE", "Wish", "gardenio", now, nil)
	msg := f.addMemberByField("gd-msgE", "title is the body", "gd-wishE", now, now)
	msg.Type = "message"
	// No fields[text] set.
	msg.CreatedBy = "matt@example.com"

	members, err := threadMembers(ctx, f, "gd-wishE")
	if err != nil {
		t.Fatalf("threadMembers: %v", err)
	}
	rendered := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-wishE", BeadTitle: "Wish"},
		{At: now, Kind: threadEntryMessage, BeadID: "gd-msgE", BeadTitle: "title is the body", Author: "matt@example.com", Text: "title is the body"},
	}

	got, divs, err := threadAuditCheckComposerBody(ctx, f, rendered, members, nil)
	if err != nil {
		t.Fatalf("checkComposerBody: %v", err)
	}
	if !got.Conformant || len(divs) != 0 {
		t.Errorf("empty fields[text] (title fallback) must be conformant, got %+v / %+v", got, divs)
	}
}

// Live end-to-end on the fuser: a 600-character composer message produces a
// rendered Text that equals the full body (the >500 fields[text] is not
// truncated to Title), Author equals CreatedBy. This is the rubric C4/K4
// end-to-end PASS over the real fuser (not the synthetic-rendered checks).
func TestAudit_ComposerBody_FuserRendersLongBodyVerbatim(t *testing.T) {
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishF", "Wish", "gardenio", now, nil)
	// Build a long body that does NOT begin or end with whitespace
	// (threadMessageText TrimSpaces the field by design, so a body with a
	// trailing space would render trimmed — not a bug, just not what this
	// long-body gate is asserting).
	body := strings.Repeat("the-operator-wrote-a-long-line.", 25) // ~775 chars
	if len(body) <= 500 {
		t.Fatalf("sanity: long body length %d <= 500", len(body))
	}
	if body != strings.TrimSpace(body) {
		t.Fatalf("test fixture sanity: long body should not have leading/trailing whitespace")
	}
	msg := f.addMemberByField("gd-msgF", truncate(body, 500), "gd-wishF",
		now.Add(time.Hour), now.Add(time.Hour))
	msg.Type = "message"
	msg.CreatedBy = "matt@example.com"
	msg.Fields["text"] = body

	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wishF")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	var found bool
	for _, e := range tl.Entries {
		if e.Kind == threadEntryMessage && e.BeadID == "gd-msgF" {
			found = true
			if e.Text != body {
				t.Errorf("rendered Text length %d != fields[text] length %d (a Title-fallback bug would render %d chars)", len(e.Text), len(body), len(truncate(body, 500)))
			}
			if e.Author != "matt@example.com" {
				t.Errorf("rendered Author %q != CreatedBy %q (rubric C4/K4 attribution)", e.Author, "matt@example.com")
			}
		}
	}
	if !found {
		t.Fatal("fuser produced no message entry for gd-msgF")
	}
}

// --- PREFIX STABILITY axis (rubric O5) -------------------------------------

// A canonical, well-formed thread must PASS the prefix-stability axis on a
// zero-cursor full-snapshot audit: the fuser's hash sequence must equal the
// expected sequence position-by-position.
func TestAudit_PrefixStability_FullSnapshotPasses(t *testing.T) {
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishP", "Wish", "gardenio", now, nil)
	f.beads["gd-wishP"].CreatedBy = "alice"
	f.addChild("gd-epicP", "epic", "gd-wishP", "gd-wishP", now.Add(time.Hour), true)
	f.beads["gd-epicP"].CreatedBy = "agent-a"
	f.comments["gd-epicP"] = []beadsapi.Comment{
		{ID: 1, Author: "bob", Text: "wip", CreatedAt: now.Add(90 * time.Minute)},
	}

	rep, err := auditThreadTimeline(context.Background(), f, "gd-wishP")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("canonical thread must PASS, divergences: %+v", rep.Divergences)
	}
	if !rep.PrefixStability.Stable {
		t.Errorf("PrefixStability = %+v, want Stable=true", rep.PrefixStability)
	}
	if rep.PrefixStability.Cursor != "" {
		t.Errorf("PrefixStability.Cursor = %q, want empty on zero-cursor audit", rep.PrefixStability.Cursor)
	}
}

// The rubric's CORE invariant: the prefix of the FULL fingerprint at any
// cursor C must equal the fingerprint computed over the entries at-or-before
// C. The auditor's `?since=` flag exercises this at a real polling instant.
func TestAudit_PrefixStability_CursorBoundIsAPrefixOfFull(t *testing.T) {
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishX", "Wish", "gardenio", now, nil)
	f.beads["gd-wishX"].CreatedBy = "alice"
	// A spread of events: head open, two comments, a member spawn.
	f.addChild("gd-taskX", "task", "gd-wishX", "gd-wishX", now.Add(2*time.Hour), true)
	f.beads["gd-taskX"].CreatedBy = "agent-a"
	f.comments["gd-wishX"] = []beadsapi.Comment{
		{ID: 1, Author: "alice", Text: "early", CreatedAt: now.Add(time.Hour)},
		{ID: 2, Author: "bob", Text: "later", CreatedAt: now.Add(3 * time.Hour)},
	}

	// Run the audit with cursor at "after the first comment" — the prefix
	// should be {head_created, comment#1}.
	cursor := now.Add(time.Hour)
	opts := threadAuditOptions{Since: cursor}
	rep, err := auditThreadTimelineWithOptions(context.Background(), f, "gd-wishX", opts)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.PrefixStability.Stable {
		t.Fatalf("cursor-bound prefix must be stable: %+v", rep.PrefixStability)
	}
	if rep.PrefixStability.PrefixLength != 2 {
		t.Errorf("PrefixLength = %d, want 2 (head_created + comment#1)", rep.PrefixStability.PrefixLength)
	}
	if rep.PrefixStability.Cursor == "" {
		t.Errorf("Cursor should be set when opts.Since != zero")
	}

	// Now feed the prior fingerprint (computed over the cursor-bound prefix)
	// back in: the across-snapshot stability check must accept it.
	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wishX")
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	prefixLen := threadAuditPrefixCount(tl.Entries, cursor)
	priorFP := threadAuditPrefixFingerprint(tl.Entries[:prefixLen])

	opts2 := threadAuditOptions{Since: cursor, PriorFingerprint: priorFP}
	rep2, err := auditThreadTimelineWithOptions(context.Background(), f, "gd-wishX", opts2)
	if err != nil {
		t.Fatalf("audit with prior fingerprint: %v", err)
	}
	if !rep2.PrefixStability.Stable {
		t.Fatalf("across-snapshot prefix must be stable when prior fingerprint matches: %+v", rep2.PrefixStability)
	}
	if rep2.PrefixStability.PriorFingerprint != priorFP {
		t.Errorf("PriorFingerprint echo = %q, want %q", rep2.PrefixStability.PriorFingerprint, priorFP)
	}
}

// The rubric's NEGATIVE invariant: if an earlier entry reorders or vanishes
// between snapshots, the prior-fingerprint check must FAIL the prefix-stability
// gate. Drive it by feeding a TAMPERED prior fingerprint (the prior snapshot
// had a different prefix at the cursor — e.g. an earlier comment was edited or
// dropped from the source).
func TestAudit_PrefixStability_TamperedPriorFingerprintFails(t *testing.T) {
	f := newThreadFake()
	now := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wishR", "Wish", "gardenio", now, nil)
	f.beads["gd-wishR"].CreatedBy = "alice"
	f.comments["gd-wishR"] = []beadsapi.Comment{
		{ID: 1, Author: "alice", Text: "early", CreatedAt: now.Add(time.Hour)},
	}

	cursor := now.Add(2 * time.Hour) // after both head_created + comment#1
	tamperedPriorFP := strings.Repeat("0", 64)

	opts := threadAuditOptions{Since: cursor, PriorFingerprint: tamperedPriorFP}
	rep, err := auditThreadTimelineWithOptions(context.Background(), f, "gd-wishR", opts)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if rep.PrefixStability.Stable {
		t.Fatalf("a tampered prior fingerprint must FAIL the prefix-stability gate: %+v", rep.PrefixStability)
	}
	if rep.Pass {
		t.Fatalf("audit must FAIL when prefix-stability fails; divergences: %+v", rep.Divergences)
	}
}

// The pure helper directly: a rendered hash sequence that diverges from the
// expected at position N is reported as a prefix_hash_mismatch divergence on
// that position. This exercises the position-level prefix gate independently of
// the (correct) live fuser, so a regression that REORDERS an EARLIER entry is
// pinned to the row.
func TestAudit_PrefixStability_PositionLevelHashMismatch(t *testing.T) {
	now := ts("2026-06-20T09:00:00Z")
	expected := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-a", Author: "alice"},
		{At: now.Add(time.Minute), Kind: threadEntryComment, BeadID: "gd-a", Author: "bob", Text: "earlier", CommentID: 1},
		{At: now.Add(2 * time.Minute), Kind: threadEntryComment, BeadID: "gd-a", Author: "carol", Text: "later", CommentID: 2},
	}
	// Simulate a fuser that swapped comments #1 and #2 (an earlier-entry reorder
	// — the WHOLE point of O5).
	rendered := []threadTimelineEntry{
		expected[0],
		expected[2], // OUT OF ORDER (was #1)
		expected[1], // OUT OF ORDER (was #2)
	}
	out, _ := threadAuditCheckPrefixStability(rendered, expected, threadAuditOptions{})
	if out.Stable {
		t.Fatalf("a reordered earlier entry must NOT be prefix-stable: %+v", out)
	}
	divs := threadAuditPrefixStabilityDivergences(rendered, expected, threadAuditOptions{})
	if len(divs) == 0 {
		t.Fatalf("expected position-level divergences, got none")
	}
	if divs[0].Code != threadAuditCodePrefixHashMismatch {
		t.Errorf("divergence[0].Code = %q, want %q", divs[0].Code, threadAuditCodePrefixHashMismatch)
	}
	if divs[0].Axis != threadAuditAxisPrefixStability {
		t.Errorf("divergence[0].Axis = %q, want %q", divs[0].Axis, threadAuditAxisPrefixStability)
	}
}

// A vanished earlier entry (the rendered prefix at cursor C has fewer entries
// than the expected prefix at C) is reported with prefix_length_shortfall.
func TestAudit_PrefixStability_VanishedEarlierEntryFlagged(t *testing.T) {
	now := ts("2026-06-20T09:00:00Z")
	expected := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-a", Author: "alice"},
		{At: now.Add(time.Minute), Kind: threadEntryComment, BeadID: "gd-a", Author: "bob", Text: "ghost", CommentID: 1},
		{At: now.Add(2 * time.Minute), Kind: threadEntryComment, BeadID: "gd-a", Author: "carol", Text: "later", CommentID: 2},
	}
	// Simulate a fuser that DROPPED the earlier comment.
	rendered := []threadTimelineEntry{
		expected[0],
		expected[2],
	}
	cursor := now.Add(90 * time.Second) // after the dropped earlier comment
	opts := threadAuditOptions{Since: cursor}
	divs := threadAuditPrefixStabilityDivergences(rendered, expected, opts)
	var sawShortfall bool
	for _, d := range divs {
		if d.Code == threadAuditCodePrefixLengthShortfall {
			sawShortfall = true
		}
	}
	if !sawShortfall {
		t.Errorf("a vanished earlier entry must surface as prefix_length_shortfall, got %+v", divs)
	}
}

// threadAuditPrefixCount + threadAuditPrefixFingerprint round-trip: building
// the prefix at every cursor along the timeline and hashing it produces a
// monotonically-extending sequence (the rubric's "prefix-extension" property).
func TestAudit_PrefixFingerprint_MonotoneExtension(t *testing.T) {
	now := ts("2026-06-20T09:00:00Z")
	entries := []threadTimelineEntry{
		{At: now, Kind: threadEntryBeadCreated, BeadID: "gd-a"},
		{At: now.Add(time.Minute), Kind: threadEntryComment, BeadID: "gd-a", CommentID: 1, Author: "a", Text: "x"},
		{At: now.Add(2 * time.Minute), Kind: threadEntryComment, BeadID: "gd-a", CommentID: 2, Author: "b", Text: "y"},
		{At: now.Add(3 * time.Minute), Kind: threadEntryBeadClosed, BeadID: "gd-a", Author: "c"},
	}
	full := threadAuditPrefixFingerprint(entries)

	// At each cursor, the prefix fingerprint must equal the fingerprint
	// computed over the literal entry prefix. (This is the trivial direction
	// of the prefix invariant; the audit gate verifies the same property
	// against an independently-derived expected set.)
	for i := 0; i <= len(entries); i++ {
		var cursor time.Time
		if i == 0 {
			cursor = now.Add(-time.Hour) // before everything
		} else {
			cursor = entries[i-1].At
		}
		got := threadAuditPrefixFingerprint(entries[:threadAuditPrefixCount(entries, cursor)])
		want := threadAuditPrefixFingerprint(entries[:i])
		if got != want {
			t.Errorf("prefix at cursor %v (i=%d) fingerprint = %s, want %s", cursor, i, got, want)
		}
	}
	// The full prefix at a far-future cursor equals the full fingerprint.
	if got := threadAuditPrefixFingerprint(entries[:threadAuditPrefixCount(entries, now.Add(time.Hour))]); got != full {
		t.Errorf("future cursor must yield the full fingerprint")
	}
}

// --- HTTP endpoint: ?since= + ?prior_fingerprint= --------------------------

// The handler must honour `?since=<rfc3339>` so a real polling client can
// exercise the O5 gate at a cursor it carried into a /threads/{id}/timeline
// poll. A malformed since must fall back to the full-snapshot audit, never
// 500.
func TestHandleThreadAudit_HonoursSinceParam(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wishH": {
			ID: "gd-wishH", Title: "Wish", Status: "open",
			Labels: []string{threadHeadLabel, "project:gardenio"}, CreatedBy: "alice", CreatedAt: now,
		},
	}
	f.comments = map[string][]beadsapi.Comment{
		"gd-wishH": {
			{ID: 1, Author: "alice", Text: "hi", CreatedAt: now.Add(time.Hour)},
		},
	}
	mux := mkServerFromFake(t, f)

	cursor := now.Add(30 * time.Minute).UTC().Format(commentTimeFormat)
	body, code := getBody(t, mux, "/threads/gd-wishH/audit?since="+cursor)
	if code != http.StatusOK {
		t.Fatalf("GET audit?since = %d, want 200; body=%s", code, body)
	}
	var rep threadAuditReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if rep.PrefixStability.Cursor == "" {
		t.Errorf("PrefixStability.Cursor should be set when ?since= is supplied")
	}
	// At the 30-minute cursor, only head_created (at 09:00:00) is before-or-at;
	// the comment at 10:00:00 is after.
	if rep.PrefixStability.PrefixLength != 1 {
		t.Errorf("PrefixLength = %d, want 1 (only head_created before cursor)", rep.PrefixStability.PrefixLength)
	}
	if !rep.PrefixStability.Stable {
		t.Errorf("a clean snapshot must be prefix-stable: %+v", rep.PrefixStability)
	}
}

// A malformed `?since=` value must fall back to the zero-cursor full-snapshot
// audit, never 500.
func TestHandleThreadAudit_MalformedSinceFallsBack(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wishH": {
			ID: "gd-wishH", Title: "Wish", Status: "open",
			Labels: []string{threadHeadLabel, "project:gardenio"}, CreatedBy: "alice", CreatedAt: now,
		},
	}
	mux := mkServerFromFake(t, f)

	body, code := getBody(t, mux, "/threads/gd-wishH/audit?since=not-a-timestamp")
	if code != http.StatusOK {
		t.Fatalf("GET audit?since=bad = %d, want 200; body=%s", code, body)
	}
	var rep threadAuditReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if rep.PrefixStability.Cursor != "" {
		t.Errorf("PrefixStability.Cursor = %q on malformed since, want empty (fell back to zero cursor)", rep.PrefixStability.Cursor)
	}
	if !rep.Pass {
		t.Errorf("audit must PASS on a clean snapshot with malformed since, got %+v", rep.Divergences)
	}
}

// The handler echoes the prior_fingerprint and uses it to harden the
// across-snapshot prefix stability gate. A correct prior fingerprint passes;
// a tampered prior fingerprint fails the audit with a prefix-stability
// divergence.
func TestHandleThreadAudit_PriorFingerprintEnforced(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wishP": {
			ID: "gd-wishP", Title: "Wish", Status: "open",
			Labels: []string{threadHeadLabel}, CreatedBy: "alice", CreatedAt: now,
		},
	}
	f.comments = map[string][]beadsapi.Comment{
		"gd-wishP": {
			{ID: 1, Author: "alice", Text: "first", CreatedAt: now.Add(time.Hour)},
		},
	}
	mux := mkServerFromFake(t, f)

	// Compute the canonical prior fingerprint at the cursor "after the first
	// comment" by going through the same fuser the audit consults.
	cursor := now.Add(time.Hour)
	tl, err := fuseThreadTimeline(context.Background(), &threadFake{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-wishP": f.beads["gd-wishP"],
		},
		comments: map[string][]beadsapi.Comment{
			"gd-wishP": f.comments["gd-wishP"],
		},
	}, "gd-wishP")
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	prefixLen := threadAuditPrefixCount(tl.Entries, cursor)
	priorFP := threadAuditPrefixFingerprint(tl.Entries[:prefixLen])

	// Correct fingerprint: PASS.
	body, code := getBody(t, mux, "/threads/gd-wishP/audit?since="+cursor.UTC().Format(commentTimeFormat)+"&prior_fingerprint="+priorFP)
	if code != http.StatusOK {
		t.Fatalf("audit (correct prior) = %d, want 200; body=%s", code, body)
	}
	var rep threadAuditReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if !rep.PrefixStability.Stable {
		t.Errorf("correct prior fingerprint must yield a stable prefix: %+v", rep.PrefixStability)
	}
	if rep.PrefixStability.PriorFingerprint != priorFP {
		t.Errorf("PriorFingerprint echo = %q, want %q", rep.PrefixStability.PriorFingerprint, priorFP)
	}
	if !rep.Pass {
		t.Errorf("clean snapshot + correct prior must PASS: %+v", rep.Divergences)
	}

	// Tampered fingerprint: FAIL with a prefix-stability divergence.
	tamperedFP := strings.Repeat("0", 64)
	body2, code2 := getBody(t, mux, "/threads/gd-wishP/audit?since="+cursor.UTC().Format(commentTimeFormat)+"&prior_fingerprint="+tamperedFP)
	if code2 != http.StatusOK {
		t.Fatalf("audit (tampered prior) = %d, want 200; body=%s", code2, body2)
	}
	var rep2 threadAuditReport
	if err := json.Unmarshal([]byte(body2), &rep2); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body2)
	}
	if rep2.PrefixStability.Stable {
		t.Errorf("tampered prior fingerprint must NOT be stable: %+v", rep2.PrefixStability)
	}
	if rep2.Pass {
		t.Errorf("audit must FAIL when prefix-stability fails on tampered prior")
	}
}

// --- end-to-end through the real fuser: a long composer reply passes ALL ----

// The most important integration: build a thread that mixes work + a long
// composer message, drive it through the real audit endpoint, and verify
// PASS with ComposerBody.Conformant + PrefixStability.Stable. This is the
// rubric C4/K4 + O5 end-to-end over the real /threads/{id}/audit handler.
func TestHandleThreadAudit_LongComposerMessagePasses(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	longBody := strings.Repeat("the-operator-typed-a-long-line.", 25) // ~775 chars
	if len(longBody) <= 500 {
		t.Fatalf("sanity: long body %d <= 500", len(longBody))
	}
	if longBody != strings.TrimSpace(longBody) {
		t.Fatalf("test fixture sanity: long body should not have leading/trailing whitespace")
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wishF": {
			ID: "gd-wishF", Title: "Wish", Status: "open",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{},
			CreatedBy: "alice", CreatedAt: now,
		},
		"gd-msgF": {
			ID:        "gd-msgF",
			Type:      "message",
			Status:    "closed",
			Labels:    []string{threadSayLabel, threadMembershipLabel("gd-wishF")},
			Fields:    map[string]string{threadIDField: "gd-wishF", "text": longBody},
			Title:     truncate(longBody, 500),
			CreatedBy: "matt@example.com",
			CreatedAt: now.Add(time.Hour),
			ClosedAt:  now.Add(time.Hour).Format(time.RFC3339Nano),
			ClosedBy:  "matt@example.com",
		},
	}
	mux := mkServerFromFake(t, f)

	body, code := getBody(t, mux, "/threads/gd-wishF/audit")
	if code != http.StatusOK {
		t.Fatalf("audit = %d, want 200; body=%s", code, body)
	}
	var rep threadAuditReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if !rep.Pass {
		t.Fatalf("long composer reply must PASS the audit end-to-end: divergences=%+v", rep.Divergences)
	}
	if !rep.ComposerBody.Conformant {
		t.Errorf("ComposerBody.Conformant = false, want true: %+v", rep.ComposerBody)
	}
	if rep.ComposerBody.Members != 1 {
		t.Errorf("ComposerBody.Members = %d, want 1", rep.ComposerBody.Members)
	}
	if !rep.PrefixStability.Stable {
		t.Errorf("PrefixStability.Stable = false, want true: %+v", rep.PrefixStability)
	}
}

// Sanity: dump a small audit report for hand-eye debugging — leaves a JSON
// snapshot the next maintainer can compare against if the report shape ever
// drifts. Belt-and-braces: also asserts the new axes are present in the JSON.
func TestHandleThreadAudit_NewAxesAppearInJSON(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wishJ": {
			ID: "gd-wishJ", Title: "Wish", Status: "open",
			Labels: []string{threadHeadLabel}, CreatedBy: "alice", CreatedAt: now,
		},
	}
	mux := mkServerFromFake(t, f)
	body, _ := getBody(t, mux, "/threads/gd-wishJ/audit")
	for _, want := range []string{`"composer_body"`, `"prefix_stability"`, `"conformant"`, `"stable"`} {
		if !strings.Contains(body, want) {
			t.Errorf("audit JSON missing %s\n%s", want, body)
		}
	}
}

// --- helpers ----------------------------------------------------------------
