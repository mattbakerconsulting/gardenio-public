package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// This suite is the Phase 3 log-correctness gate (rubric ka-0000007635 §A,
// task ka-0000005790): it proves auditThreadTimeline reconciles the fused
// timeline on COUNT, HASH, and ORDER, surfaces the two fixed silent-drop bugs
// as warnings (not failures), and that the audit catches a fuser that drops,
// fabricates, mis-attributes, or mis-orders an entry. The pure reconciliation
// helpers are exercised directly with synthetic rendered-vs-expected slices so
// the failure detection is proven independently of the (correct) live fuser.

// --- COUNT axis: conservation identity --------------------------------------

// A representative multi-member thread (head + closure epic/task + field-only
// message, scattered comments, one close) must PASS, and the count vector must
// equal the conservation identity len(Entries) == N members-with-body + K
// comments + M closable, in total and per-kind.
func TestAudit_CountConservation_Passes(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "gardenio", ts("2026-06-20T09:00:00Z"), nil)
	f.beads["gd-wish1"].CreatedBy = "alice"
	f.addChild("gd-epic", "epic", "gd-wish1", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)
	f.beads["gd-epic"].CreatedBy = "agent-a"
	f.addChild("gd-task", "task", "gd-epic", "gd-wish1", ts("2026-06-20T10:00:00Z"), true)
	f.beads["gd-task"].CreatedBy = "agent-b"
	f.addMemberByField("gd-msg", "msg", "gd-wish1", ts("2026-06-20T09:45:00Z"), ts("2026-06-20T09:45:00Z"))
	f.beads["gd-msg"].CreatedBy = "alice"

	f.comments["gd-wish1"] = []beadsapi.Comment{
		{ID: 1, Author: "alice", Text: "hi", CreatedAt: ts("2026-06-20T11:00:00Z")},
		{ID: 2, Author: "bob", Text: "yo", CreatedAt: ts("2026-06-20T11:05:00Z")},
	}
	f.comments["gd-task"] = []beadsapi.Comment{
		{ID: 3, Author: "carol", Text: "wip", CreatedAt: ts("2026-06-20T10:30:00Z")},
	}
	// Close the task with a parseable timestamp -> one bead_closed.
	f.beads["gd-task"].Status = "closed"
	f.beads["gd-task"].ClosedAt = "2026-06-20T12:00:00Z"
	f.beads["gd-task"].ClosedBy = "carol"

	rep, err := auditThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("auditThreadTimeline: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("expected PASS, got FAIL: %+v", rep.Divergences)
	}
	// N=4 members with bodies, K=3 comments, M=1 closable.
	if rep.Counts.MembersWithBody != 4 || rep.Counts.Comments != 3 || rep.Counts.Closable != 1 {
		t.Errorf("count terms = N%d K%d M%d, want N4 K3 M1", rep.Counts.MembersWithBody, rep.Counts.Comments, rep.Counts.Closable)
	}
	if rep.Counts.ExpectedTotal != 8 || rep.Counts.RenderedTotal != 8 {
		t.Errorf("totals expected=%d rendered=%d, want 8/8", rep.Counts.ExpectedTotal, rep.Counts.RenderedTotal)
	}
	if !rep.Counts.Conserved {
		t.Errorf("conservation identity should hold: %+v", rep.Counts)
	}
	if rep.Counts.RenderedByKind[threadEntryBeadCreated] != 4 ||
		rep.Counts.RenderedByKind[threadEntryComment] != 3 ||
		rep.Counts.RenderedByKind[threadEntryBeadClosed] != 1 {
		t.Errorf("per-kind rendered = %v, want created4 comment3 closed1", rep.Counts.RenderedByKind)
	}
}

// A thread carrying a type=message member (the 2-5 composer's bridge-mediated
// reply, ka-0000006034) must PASS the audit. The fuser emits ONE message entry
// for it — no bead_created/bead_closed work pair and no comment fetch — so the
// auditor must derive the SAME single message entry and count it on its own
// Messages (P) term, never as a member-with-body or closable. Without that
// (threadAuditDeriveExpected message-blind), a correct composer reply would read
// as a fabricated message + dropped created/closed/comments against the
// ka-0000005790 reconciliation — a false FAIL on correct output. This locks the
// fuser and auditor into agreeing on the message rule by construction.
func TestAudit_MessageMember_Passes(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wishm", "Wish", "gardenio", ts("2026-06-20T09:00:00Z"), nil)
	f.beads["gd-wishm"].CreatedBy = "alice"
	// A real work child (closed) so the thread mixes work + conversation.
	f.addChild("gd-taskm", "task", "gd-wishm", "gd-wishm", ts("2026-06-20T09:30:00Z"), true)
	f.beads["gd-taskm"].CreatedBy = "agent-a"
	f.beads["gd-taskm"].Status = "closed"
	f.beads["gd-taskm"].ClosedAt = "2026-06-20T12:00:00Z"
	f.beads["gd-taskm"].ClosedBy = "agent-a"
	// A closed type=message member — exactly what postThreadSayMessage writes:
	// type=message, closed, body in fields.text, authored by the operator.
	msg := f.addMemberByField("gd-msgm", "msg title", "gd-wishm",
		ts("2026-06-20T10:00:00Z"), ts("2026-06-20T10:00:00Z"))
	msg.Type = "message"
	msg.Status = "closed"
	msg.ClosedAt = "2026-06-20T10:00:01Z"
	msg.CreatedBy = "matt@example.com"
	msg.Fields["text"] = "hello from the composer"
	// A comment on the wish head, to prove message handling does not disturb the
	// ordinary comment path.
	f.comments["gd-wishm"] = []beadsapi.Comment{
		{ID: 1, Author: "alice", Text: "hi", CreatedAt: ts("2026-06-20T11:00:00Z")},
	}

	rep, err := auditThreadTimeline(context.Background(), f, "gd-wishm")
	if err != nil {
		t.Fatalf("auditThreadTimeline: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("expected PASS for a message-bearing thread, got FAIL: %+v", rep.Divergences)
	}
	// The message member is counted on its OWN term: P=1, and it is NOT a
	// member-with-body or a closable (those are the head + the work task only).
	if rep.Counts.Messages != 1 {
		t.Errorf("Messages (P) = %d, want 1", rep.Counts.Messages)
	}
	if rep.Counts.MembersWithBody != 2 || rep.Counts.Closable != 1 || rep.Counts.Comments != 1 {
		t.Errorf("count terms = N%d K%d M%d P%d, want N2 K1 M1 P1",
			rep.Counts.MembersWithBody, rep.Counts.Comments, rep.Counts.Closable, rep.Counts.Messages)
	}
	// Conservation: N(2 created) + K(1 comment) + M(1 closed) + P(1 message) = 5.
	if rep.Counts.ExpectedTotal != 5 || rep.Counts.RenderedTotal != 5 || !rep.Counts.Conserved {
		t.Errorf("totals expected=%d rendered=%d conserved=%v, want 5/5/true",
			rep.Counts.ExpectedTotal, rep.Counts.RenderedTotal, rep.Counts.Conserved)
	}
	if rep.Counts.RenderedByKind[threadEntryMessage] != 1 {
		t.Errorf("per-kind rendered message = %d, want 1", rep.Counts.RenderedByKind[threadEntryMessage])
	}
	// The message member contributes NO work entries.
	if rep.Counts.RenderedByKind[threadEntryBeadCreated] != 2 ||
		rep.Counts.RenderedByKind[threadEntryBeadClosed] != 1 {
		t.Errorf("per-kind rendered = %v, want created2 closed1 (message member adds neither)", rep.Counts.RenderedByKind)
	}
}

// A fuser that DROPPED an entry breaks conservation: the synthetic check proves
// the count axis flags a shortfall even though the live fuser never drops.
func TestAudit_CountConservation_DetectsDrop(t *testing.T) {
	expected := []threadTimelineEntry{
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryBeadCreated, BeadID: "gd-a"},
		{At: ts("2026-06-20T09:01:00Z"), Kind: threadEntryComment, BeadID: "gd-a", CommentID: 1, Author: "x", Text: "c"},
	}
	rendered := expected[:1] // the comment was dropped.
	divs := threadAuditPerMemberCountDivergences(expected, rendered)
	if len(divs) == 0 {
		t.Fatal("expected a per-member count divergence for the dropped comment")
	}
	if divs[0].BeadID != "gd-a" || divs[0].Axis != threadAuditAxisCount {
		t.Errorf("divergence = %+v, want count axis on gd-a", divs[0])
	}
}

// --- HASH axis: multiset reconciliation -------------------------------------

func TestAudit_HashReconcile_MatchesIdentical(t *testing.T) {
	entries := []threadTimelineEntry{
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryBeadCreated, BeadID: "gd-a", BeadTitle: "A", Author: "alice"},
		{At: ts("2026-06-20T09:01:00Z"), Kind: threadEntryComment, BeadID: "gd-a", BeadTitle: "A", Author: "bob", Text: "hello", CommentID: 5},
	}
	got := threadAuditReconcileHashes(entries, entries)
	if !got.Matched || len(got.Fabrications) != 0 || len(got.Drops) != 0 {
		t.Fatalf("identical multisets must match cleanly: %+v", got)
	}
}

// A fabricated rendered entry (present in rendered, absent in expected) is
// reported as a fabrication; a dropped expected entry as a drop.
func TestAudit_HashReconcile_DetectsFabricationAndDrop(t *testing.T) {
	expected := []threadTimelineEntry{
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryBeadCreated, BeadID: "gd-a", Author: "alice"},
		{At: ts("2026-06-20T09:02:00Z"), Kind: threadEntryComment, BeadID: "gd-a", Author: "bob", Text: "real", CommentID: 1},
	}
	rendered := []threadTimelineEntry{
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryBeadCreated, BeadID: "gd-a", Author: "alice"},
		// the real comment is dropped; a fabricated one appears instead.
		{At: ts("2026-06-20T09:05:00Z"), Kind: threadEntryComment, BeadID: "gd-a", Author: "mallory", Text: "fake", CommentID: 99},
	}
	got := threadAuditReconcileHashes(expected, rendered)
	if got.Matched {
		t.Fatal("expected a hash mismatch")
	}
	if len(got.Fabrications) != 1 || got.Fabrications[0].CommentID != 99 {
		t.Errorf("fabrications = %+v, want the fake comment 99", got.Fabrications)
	}
	if len(got.Drops) != 1 || got.Drops[0].CommentID != 1 {
		t.Errorf("drops = %+v, want the real comment 1", got.Drops)
	}
}

// An actor bleed (same event, wrong author) changes the content hash, so it is
// caught as a paired drop+fabrication — the hash binds the author.
func TestAudit_HashReconcile_CatchesActorBleed(t *testing.T) {
	expected := []threadTimelineEntry{
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryBeadClosed, BeadID: "gd-a", Author: "carol"},
	}
	rendered := []threadTimelineEntry{
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryBeadClosed, BeadID: "gd-a", Author: "WRONG"},
	}
	got := threadAuditReconcileHashes(expected, rendered)
	if got.Matched {
		t.Fatal("an actor bleed must break the hash match")
	}
	if len(got.Fabrications) != 1 || len(got.Drops) != 1 {
		t.Errorf("actor bleed should be one drop + one fabrication, got %+v", got)
	}
}

// The content hash is timestamp-sensitive to the nanosecond and
// timezone-invariant (UTC-normalised): two equal instants in different
// locations hash equally; a one-nanosecond shift does not.
func TestAudit_ContentHash_TimestampAndTZSensitivity(t *testing.T) {
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	loc := time.FixedZone("X", 3600)
	hUTC := threadEntryContentHash(threadEntryComment, "gd-a", base, "a", 1, "t")
	hTZ := threadEntryContentHash(threadEntryComment, "gd-a", base.In(loc), "a", 1, "t")
	if hUTC != hTZ {
		t.Errorf("equal instants in different zones must hash equally")
	}
	hNano := threadEntryContentHash(threadEntryComment, "gd-a", base.Add(time.Nanosecond), "a", 1, "t")
	if hUTC == hNano {
		t.Errorf("a one-nanosecond shift must change the hash")
	}
}

// --- ORDER axis: strict-monotonic + unique 4-tuple --------------------------

func TestAudit_Order_PassesOnCanonicalStream(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
	f.addChild("gd-epic", "epic", "gd-wish1", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)
	f.comments["gd-epic"] = []beadsapi.Comment{
		{ID: 1, Author: "a", Text: "x", CreatedAt: ts("2026-06-20T09:45:00Z")},
	}
	rep, err := auditThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.Order.StrictlyMonotonic || !rep.Order.Unique {
		t.Errorf("canonical stream must be strictly monotonic + unique: %+v", rep.Order)
	}
	if rep.Order.Fingerprint == "" {
		t.Error("order fingerprint must be set")
	}
}

// A duplicated canonical 4-tuple (same At, member, kind, comment-id) is
// ambiguous order — the uniqueness check must flag it.
func TestAudit_Order_DetectsDuplicateTuple(t *testing.T) {
	members := []string{"gd-a"}
	dup := threadTimelineEntry{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryComment, BeadID: "gd-a", CommentID: 1, Author: "x", Text: "t"}
	entries := []threadTimelineEntry{dup, dup}
	ord := threadAuditCheckOrder(entries, members)
	if ord.Unique {
		t.Error("two identical tuples must NOT be unique")
	}
	divs := threadAuditOrderDivergences(entries, members)
	if len(divs) != 1 || divs[0].Code != "duplicate_tuple" {
		t.Errorf("expected one duplicate_tuple divergence, got %+v", divs)
	}
}

// An out-of-order pair (a later canonical tuple placed before an earlier one) is
// caught by the monotonicity check.
func TestAudit_Order_DetectsOutOfOrder(t *testing.T) {
	members := []string{"gd-a"}
	entries := []threadTimelineEntry{
		{At: ts("2026-06-20T10:00:00Z"), Kind: threadEntryComment, BeadID: "gd-a", CommentID: 2, Author: "x"},
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryComment, BeadID: "gd-a", CommentID: 1, Author: "x"},
	}
	ord := threadAuditCheckOrder(entries, members)
	if ord.StrictlyMonotonic {
		t.Error("a backwards pair must not be strictly monotonic")
	}
	divs := threadAuditOrderDivergences(entries, members)
	if len(divs) != 1 || divs[0].Code != "out_of_order" {
		t.Errorf("expected one out_of_order divergence, got %+v", divs)
	}
}

// Equal-timestamp matrix (rubric O4 "documented one-instant order reproduced"):
// when every event shares ONE instant, the canonical tie-break is
// (dep-first member position, kindRank [created<comment<closed], comment-id).
// The audit must PASS and the fused order must reproduce exactly that.
func TestAudit_EqualTimestampMatrix_DeterministicOrder(t *testing.T) {
	instant := "2026-06-20T09:00:00Z"
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts(instant), nil)
	f.beads["gd-wish1"].CreatedBy = "alice"
	f.addChild("gd-epic", "epic", "gd-wish1", "gd-wish1", ts(instant), true)
	f.beads["gd-epic"].CreatedBy = "a"
	f.addChild("gd-task", "task", "gd-epic", "gd-wish1", ts(instant), true)
	f.beads["gd-task"].CreatedBy = "b"
	// Two comments on the head at the same instant, distinct ids -> ordered by id.
	f.comments["gd-wish1"] = []beadsapi.Comment{
		{ID: 20, Author: "z", Text: "second", CreatedAt: ts(instant)},
		{ID: 10, Author: "y", Text: "first", CreatedAt: ts(instant)},
	}
	// Close the head at the same instant -> created < comments < closed by kindRank.
	f.beads["gd-wish1"].Status = "closed"
	f.beads["gd-wish1"].ClosedAt = instant
	f.beads["gd-wish1"].ClosedBy = "alice"

	rep, err := auditThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("equal-timestamp thread must PASS: %+v", rep.Divergences)
	}
	tl, _ := fuseThreadTimeline(context.Background(), f, "gd-wish1")
	// Expected one-instant order:
	//   head created (pos0, kind created)
	//   head comment 10 (pos0, kind comment, id10)
	//   head comment 20 (pos0, kind comment, id20)
	//   head closed (pos0, kind closed)
	//   epic created (pos1)
	//   task created (pos2)
	type k struct {
		bead string
		kind string
		cid  int64
	}
	want := []k{
		{"gd-wish1", threadEntryBeadCreated, 0},
		{"gd-wish1", threadEntryComment, 10},
		{"gd-wish1", threadEntryComment, 20},
		{"gd-wish1", threadEntryBeadClosed, 0},
		{"gd-epic", threadEntryBeadCreated, 0},
		{"gd-task", threadEntryBeadCreated, 0},
	}
	if len(tl.Entries) != len(want) {
		t.Fatalf("entries = %d, want %d\n%s", len(tl.Entries), len(want), dumpEntries(tl.Entries))
	}
	for i, w := range want {
		e := tl.Entries[i]
		if e.BeadID != w.bead || e.Kind != w.kind || e.CommentID != w.cid {
			t.Errorf("entry[%d] = {%s %s %d}, want {%s %s %d}\n%s",
				i, e.BeadID, e.Kind, e.CommentID, w.bead, w.kind, w.cid, dumpEntries(tl.Entries))
		}
	}
}

// --- DETERMINISM: shuffled fake-map insertion -> byte-identical -------------

// The fake's bead map iterates in random order, so building the SAME logical
// thread via different insertion sequences exercises the fuser's stability. The
// audit's order fingerprint AND the full JSON report must be byte-identical
// across insertion orders — proving the canonical sort, not map iteration,
// fixes the output (rubric O3).
func TestAudit_Determinism_ShuffledInsertionByteIdentical(t *testing.T) {
	build := func(order []int) *threadFake {
		f := newThreadFake()
		f.addHead("gd-wish1", "Wish", "gardenio", ts("2026-06-20T09:00:00Z"), nil)
		f.beads["gd-wish1"].CreatedBy = "alice"
		// Three children, all created at the SAME instant so ONLY dep-first
		// position (not timestamp) disambiguates — the determinism stress.
		children := []string{"gd-c1", "gd-c2", "gd-c3"}
		for _, idx := range order {
			id := children[idx]
			f.addChild(id, "child "+id, "gd-wish1", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)
			f.beads[id].CreatedBy = "agent"
			f.comments[id] = []beadsapi.Comment{
				{ID: int64(idx + 1), Author: "u", Text: "c", CreatedAt: ts("2026-06-20T09:30:00Z")},
			}
		}
		return f
	}

	insertionOrders := [][]int{
		{0, 1, 2}, {2, 1, 0}, {1, 0, 2}, {0, 2, 1}, {2, 0, 1}, {1, 2, 0},
	}
	var firstFP, firstJSON string
	for run, ord := range insertionOrders {
		f := build(ord)
		rep, err := auditThreadTimeline(context.Background(), f, "gd-wish1")
		if err != nil {
			t.Fatalf("run %d audit: %v", run, err)
		}
		if !rep.Pass {
			t.Fatalf("run %d must PASS: %+v", run, rep.Divergences)
		}
		js, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("run %d marshal: %v", run, err)
		}
		if run == 0 {
			firstFP = rep.Order.Fingerprint
			firstJSON = string(js)
			continue
		}
		if rep.Order.Fingerprint != firstFP {
			t.Errorf("run %d (insertion %v) fingerprint %s != %s", run, ord, rep.Order.Fingerprint, firstFP)
		}
		if string(js) != firstJSON {
			t.Errorf("run %d (insertion %v) report JSON diverged from run 0", run, ord)
		}
	}
}

// --- WARNINGS: the two fixed bugs surface, never fail ------------------------

// A closed member with an empty/unparseable ClosedAt produces an
// unparseable_closed_at WARNING (not a divergence); the audit still PASSES and
// the close event is conserved.
func TestAudit_UnparseableClosedAt_WarnsNotFails(t *testing.T) {
	for _, bad := range []string{"", "garbage-stamp"} {
		f := newThreadFake()
		f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
		f.beads["gd-wish1"].CreatedBy = "alice"
		f.beads["gd-wish1"].Status = "closed"
		f.beads["gd-wish1"].ClosedAt = bad
		f.beads["gd-wish1"].ClosedBy = "alice"

		rep, err := auditThreadTimeline(context.Background(), f, "gd-wish1")
		if err != nil {
			t.Fatalf("closed_at=%q audit: %v", bad, err)
		}
		if !rep.Pass {
			t.Fatalf("closed_at=%q must PASS (warning, not failure): %+v", bad, rep.Divergences)
		}
		var found bool
		for _, wn := range rep.Warnings {
			if wn.Code == threadAuditWarnUnparseableClosedAt && wn.BeadID == "gd-wish1" {
				found = true
			}
		}
		if !found {
			t.Errorf("closed_at=%q: expected an unparseable_closed_at warning, got %+v", bad, rep.Warnings)
		}
		// The close event is still counted (M=1) and conserved.
		if rep.Counts.Closable != 1 || !rep.Counts.Conserved {
			t.Errorf("closed_at=%q: close must be conserved, counts=%+v", bad, rep.Counts)
		}
	}
}

// A member resolved by membership but missing from BatchGetBeads produces a
// missing_member_body WARNING; its comments are still conserved and the audit
// PASSES.
func TestAudit_MissingMemberBody_WarnsAndConservesComments(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
	f.beads["gd-wish1"].CreatedBy = "alice"
	f.addChild("gd-ghost", "ghost", "gd-wish1", "gd-wish1", ts("2026-06-20T09:30:00Z"), true)
	delete(f.beads, "gd-ghost") // body gone, membership remains
	f.comments["gd-ghost"] = []beadsapi.Comment{
		{ID: 1, Author: "carol", Text: "still here", CreatedAt: ts("2026-06-20T10:00:00Z")},
	}

	rep, err := auditThreadTimeline(context.Background(), f, "gd-wish1")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("missing-body member must PASS (warning): %+v", rep.Divergences)
	}
	var found bool
	for _, wn := range rep.Warnings {
		if wn.Code == threadAuditWarnMissingMemberBody && wn.BeadID == "gd-ghost" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a missing_member_body warning, got %+v", rep.Warnings)
	}
	// N=1 (only the head has a body), K=1 (the ghost's comment survives), M=0.
	if rep.Counts.MembersWithBody != 1 || rep.Counts.Comments != 1 || rep.Counts.Closable != 0 {
		t.Errorf("counts = N%d K%d M%d, want N1 K1 M0", rep.Counts.MembersWithBody, rep.Counts.Comments, rep.Counts.Closable)
	}
}

// --- TITLE fidelity ----------------------------------------------------------

func TestAudit_TitleBleed_Detected(t *testing.T) {
	// The expected set carries gd-a's canonical title via its bead_created entry;
	// the rendered comment entry carries a bled title -> a title_bleed divergence.
	expected := []threadTimelineEntry{
		{At: ts("2026-06-20T08:59:00Z"), Kind: threadEntryBeadCreated, BeadID: "gd-a", BeadTitle: "right title"},
	}
	rendered := []threadTimelineEntry{
		{At: ts("2026-06-20T08:59:00Z"), Kind: threadEntryBeadCreated, BeadID: "gd-a", BeadTitle: "right title"},
		{At: ts("2026-06-20T09:00:00Z"), Kind: threadEntryComment, BeadID: "gd-a", BeadTitle: "WRONG", CommentID: 1, Author: "x"},
	}
	divs := threadAuditTitleDivergences(rendered, expected)
	if len(divs) != 1 || divs[0].Code != "title_bleed" {
		t.Fatalf("expected a title_bleed divergence, got %+v", divs)
	}
	if divs[0].BeadID != "gd-a" {
		t.Errorf("divergence bead = %q, want gd-a", divs[0].BeadID)
	}
}

// A bodyless member's canonical title is empty: its surviving comments must
// carry an empty title (no bleed), and the title check must PASS for them.
func TestAudit_TitleFidelity_BodylessMemberEmptyTitleOK(t *testing.T) {
	expected := []threadTimelineEntry{
		// no bead_created for gd-ghost (bodyless) -> canonical title "".
		{At: ts("2026-06-20T10:00:00Z"), Kind: threadEntryComment, BeadID: "gd-ghost", BeadTitle: "", CommentID: 1, Author: "x"},
	}
	rendered := expected
	if divs := threadAuditTitleDivergences(rendered, expected); len(divs) != 0 {
		t.Fatalf("bodyless member's empty-title comment must not be a bleed, got %+v", divs)
	}
}

// threadAuditSortRefs must impose a total order across every tie-break axis
// (At, then BeadID, then Kind, then CommentID, then Hash) so the divergence
// lists in the JSON report are byte-stable regardless of map iteration order.
func TestAudit_SortRefs_TotalOrderAcrossAllAxes(t *testing.T) {
	in := []threadAuditEntryRef{
		{At: "2026-06-20T10:00:00Z", BeadID: "gd-b", Kind: threadEntryComment, CommentID: 2, Hash: "h9"},
		{At: "2026-06-20T09:00:00Z", BeadID: "gd-z", Kind: threadEntryComment, CommentID: 1, Hash: "h1"}, // earliest At wins
		{At: "2026-06-20T10:00:00Z", BeadID: "gd-a", Kind: threadEntryBeadClosed, CommentID: 0, Hash: "h3"},
		{At: "2026-06-20T10:00:00Z", BeadID: "gd-a", Kind: threadEntryBeadCreated, CommentID: 0, Hash: "h2"}, // same At+bead, Kind tiebreak
		{At: "2026-06-20T10:00:00Z", BeadID: "gd-a", Kind: threadEntryComment, CommentID: 5, Hash: "h5"},
		{At: "2026-06-20T10:00:00Z", BeadID: "gd-a", Kind: threadEntryComment, CommentID: 4, Hash: "h4"},  // same up to CommentID
		{At: "2026-06-20T10:00:00Z", BeadID: "gd-a", Kind: threadEntryComment, CommentID: 4, Hash: "h4b"}, // same up to Hash
	}
	threadAuditSortRefs(in)
	// Sorted: At asc first, then within the 10:00 group by BeadID (gd-a before
	// gd-b), then Kind (string-compared: "bead_closed" < "bead_created" <
	// "comment"), then CommentID, then Hash. So gd-a's bead_closed (h3) precedes
	// its bead_created (h2).
	wantHash := []string{"h1", "h3", "h2", "h4", "h4b", "h5", "h9"}
	got := make([]string, len(in))
	for i, r := range in {
		got[i] = r.Hash
	}
	for i := range wantHash {
		if got[i] != wantHash[i] {
			t.Fatalf("sorted hashes = %v, want %v", got, wantHash)
		}
	}
	// Sorting is idempotent (stable total order).
	threadAuditSortRefs(in)
	for i := range wantHash {
		if in[i].Hash != wantHash[i] {
			t.Fatalf("re-sort not idempotent at %d: %v", i, in)
		}
	}
}

// threadAuditHasFailingDivergence must treat membership / title / count / order
// divergences as PASS-affecting failures, but ignore non-failing axes (e.g. an
// informational hash entry, which is folded into Pass via Hash.Matched instead).
func TestAudit_HasFailingDivergence_AxisSelectivity(t *testing.T) {
	failing := []string{threadAuditAxisMembership, threadAuditAxisTitle, threadAuditAxisCount, threadAuditAxisOrder}
	for _, axis := range failing {
		if !threadAuditHasFailingDivergence([]threadAuditDivergence{{Axis: axis}}) {
			t.Errorf("axis %q should count as a failing divergence", axis)
		}
	}
	if threadAuditHasFailingDivergence(nil) {
		t.Error("no divergences must not be a failure")
	}
	if threadAuditHasFailingDivergence([]threadAuditDivergence{{Axis: threadAuditAxisHash}}) {
		t.Error("a hash-axis divergence is folded into Pass via Hash.Matched, not this gate")
	}
}

// End-to-end membership-mismatch path: a divergence on the membership axis fails
// the audit even when counts/hash/order would otherwise pass. Driven through the
// pure differ so it doesn't depend on a fuser bug.
func TestAudit_DiffMembers_DetectsOrderAndSetMismatch(t *testing.T) {
	if _, ok := threadAuditDiffMembers([]string{"gd-a", "gd-b"}, []string{"gd-a", "gd-b"}); !ok {
		t.Error("identical membership should be ok")
	}
	if d, ok := threadAuditDiffMembers([]string{"gd-a", "gd-b"}, []string{"gd-b", "gd-a"}); ok || d.Axis != threadAuditAxisMembership {
		t.Errorf("reordered membership should diverge on the membership axis, got ok=%v d=%+v", ok, d)
	}
	if _, ok := threadAuditDiffMembers([]string{"gd-a"}, []string{"gd-a", "gd-b"}); ok {
		t.Error("differing-length membership should diverge")
	}
}

// --- error propagation -------------------------------------------------------

func TestAudit_PropagatesDaemonError(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish1", "Wish", "", ts("2026-06-20T09:00:00Z"), nil)
	f.commentErr = context.DeadlineExceeded
	if _, err := auditThreadTimeline(context.Background(), f, "gd-wish1"); err == nil {
		t.Fatal("expected the comment error to propagate")
	}
}

func TestAudit_EmptyHeadIsTrivialPass(t *testing.T) {
	f := newThreadFake()
	rep, err := auditThreadTimeline(context.Background(), f, "")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.Pass {
		t.Errorf("empty head should be a trivial PASS")
	}
}

// --- HTTP endpoint: read-only, viewer-scoped, JSON PASS/FAIL ----------------

func TestHandleThreadAudit_ReturnsJSONPass(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {
			ID: "gd-wish1", Title: "Wish", Status: "open",
			Labels: []string{threadHeadLabel, "project:gardenio"}, CreatedBy: "alice", CreatedAt: now,
		},
		"gd-task1": {
			ID: "gd-task1", Title: "task", Status: "open",
			Fields: map[string]string{threadIDField: "gd-wish1"}, CreatedBy: "bot", CreatedAt: now.Add(time.Hour),
		},
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-wish1": {{BeadID: "gd-task1", DependsOnID: "gd-wish1", Type: "parent-child"}},
	}
	f.comments = map[string][]beadsapi.Comment{
		"gd-wish1": {{ID: 1, Author: "alice", Text: "hi", CreatedAt: now.Add(30 * time.Minute)}},
	}

	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish1/audit")
	if code != http.StatusOK {
		t.Fatalf("GET /threads/gd-wish1/audit = %d, want 200\n%s", code, body)
	}
	var rep threadAuditReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, body)
	}
	if !rep.Pass {
		t.Errorf("expected PASS, got %+v", rep.Divergences)
	}
	if rep.ThreadID != "gd-wish1" {
		t.Errorf("ThreadID = %q, want gd-wish1", rep.ThreadID)
	}
	// N=2 created + K=1 comment.
	if rep.Counts.RenderedTotal != 3 {
		t.Errorf("rendered total = %d, want 3", rep.Counts.RenderedTotal)
	}
}

func TestHandleThreadAudit_NotFound(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{}
	mux := mkServerFromFake(t, f)
	_, code := getBody(t, mux, "/threads/gd-missing/audit")
	if code != http.StatusNotFound {
		t.Fatalf("GET /threads/gd-missing/audit = %d, want 404", code)
	}
}

func TestHandleThreadAudit_HeadErrorIs500(t *testing.T) {
	f := newFake()
	f.getBeadErr = errThreadTest
	mux := mkServerFromFake(t, f)
	_, code := getBody(t, mux, "/threads/gd-x/audit")
	if code != http.StatusInternalServerError {
		t.Fatalf("GET audit (head error) = %d, want 500", code)
	}
}

// The audit endpoint is READ-ONLY: serving it must not invoke ANY mutating
// daemon method. We assert on the write-call counters the fake tracks.
func TestHandleThreadAudit_ReadOnly_NoWrites(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {
			ID: "gd-wish1", Title: "Wish", Status: "closed",
			Labels: []string{threadHeadLabel}, CreatedBy: "alice", CreatedAt: now,
			ClosedAt: "garbage", ClosedBy: "alice", // exercises the imputed-close path too
		},
	}
	mux := mkServerFromFake(t, f)
	_, code := getBody(t, mux, "/threads/gd-wish1/audit")
	if code != http.StatusOK {
		t.Fatalf("audit = %d, want 200", code)
	}
	if f.createCalls != 0 || f.updateCalls != 0 || f.closeCalls != 0 ||
		f.addCommentCalls != 0 || f.addDepCalls != 0 || f.addLabelCalls != 0 ||
		f.removeLabelCalls != 0 || f.deleteCalls != 0 || f.resolveCalls != 0 || f.cancelCalls != 0 {
		t.Errorf("audit endpoint performed a write: create=%d update=%d close=%d comment=%d dep=%d addLabel=%d rmLabel=%d delete=%d resolve=%d cancel=%d",
			f.createCalls, f.updateCalls, f.closeCalls, f.addCommentCalls, f.addDepCalls,
			f.addLabelCalls, f.removeLabelCalls, f.deleteCalls, f.resolveCalls, f.cancelCalls)
	}
}

// The audit endpoint is content-typed application/json.
func TestHandleThreadAudit_ContentTypeJSON(t *testing.T) {
	f := newFake()
	now := ts("2026-06-20T09:00:00Z")
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {ID: "gd-wish1", Title: "Wish", Status: "open", Labels: []string{threadHeadLabel}, CreatedAt: now},
	}
	mux := mkServerFromFake(t, f)
	req := httptest.NewRequest("GET", "/threads/gd-wish1/audit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// --- route precedence: /threads/{id}/audit is its own pattern ---------------

func TestThreadAuditRouteDoesNotShadow(t *testing.T) {
	mux := mkRawMux(t, newFake())
	cases := []struct {
		path, want string
	}{
		{"/threads/gd-abc/audit", "GET /threads/{id}/audit"},
		{"/threads/gd-abc", "GET /threads/{id}"},
		{"/threads", "GET /threads"},
		{"/gd-abc", "GET /{id}"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		_, pattern := mux.Handler(req)
		if pattern != tc.want {
			t.Errorf("%s routed to %q, want %q", tc.path, pattern, tc.want)
		}
	}
}
