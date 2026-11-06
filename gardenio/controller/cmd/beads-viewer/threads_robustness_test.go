package main

import (
	"context"
	"testing"
	"time"
)

// Latent-robustness fixes for the /threads viewer (ka-0000006057). Each test
// pins one of the five documented gaps against regression. Gaps 4 (dead
// membership label — a doc correction) and 5 (composer JS concurrency —
// client-only, no Go surface) carry no Go test; gap 1 (dedup-key trim drift)
// is additionally pinned by the golden-vector additions in threads_polish_test.go.

// --- Gap 1: dedup-key trim drift (NEL / BOM) -------------------------------

// TestThreadKeyTrim_MatchesJSUnionSet proves threadKeyTrim strips the SAME
// whitespace set as the composer JS keyTrim() — the UNION of Go's unicode.IsSpace
// and JS \s: crucially U+0085 (NEL) AND U+FEFF (BOM), the two chars where plain
// strings.TrimSpace and JS .trim() disagree. A leading/trailing NEL or BOM must
// therefore NOT change the key, so the server key equals the client key and the
// optimistic bubble reconciles instead of lingering as a permanent duplicate.
func TestThreadKeyTrim_MatchesJSUnionSet(t *testing.T) {
	const (
		nel = "\u0085" // NEL: Go unicode.IsSpace strips it, JS \s does NOT
		bom = "\uFEFF" // BOM/ZWNBSP: JS \s strips it, Go unicode.IsSpace does NOT
	)
	cases := []struct{ in, want string }{
		{"  spaced  ", "spaced"},
		{nel + "hi" + nel, "hi"},   // NEL edge
		{bom + "yes" + bom, "yes"}, // BOM edge
		{bom + nel + " mix " + nel + bom, "mix"},
		{"\tnormal\n", "normal"},
	}
	for _, c := range cases {
		if got := threadKeyTrim(c.in); got != c.want {
			t.Errorf("threadKeyTrim(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// The key itself must be NEL/BOM-insensitive on both halves (the actual bug:
	// a NEL/BOM-wrapped draft would otherwise key differently from its echo).
	if threadMsgKey("bob", "yes") != threadMsgKey(bom+"bob"+bom, bom+"yes"+bom) {
		t.Error("threadMsgKey must be BOM-insensitive (server echo would never match a BOM-wrapped draft)")
	}
	if threadMsgKey("a", "hi") != threadMsgKey("a", nel+"hi"+nel) {
		t.Error("threadMsgKey must be NEL-insensitive (Go stripped NEL that JS kept, or vice versa)")
	}
}

// --- Gap 2: missing head body drops the "opened this wish" marker ----------

// TestBuildThreadDetailView_SynthesizesHeadOpenMarker proves that when
// BatchGetBeads omits the head body (a daemon split / deletion race) but the
// handler's GetBead resolved it (head != nil), buildThreadDetailView synthesizes
// the head bead_created entry so Conversation() still renders the "opened this
// wish" anchor and the lead pane is never the empty void C15 forbids. OpenedAt
// likewise recovers.
func TestBuildThreadDetailView_SynthesizesHeadOpenMarker(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	f.omitFromBatch = map[string]bool{"gd-wish": true} // batch drops the head body
	opened := ts("2026-06-20T09:00:00Z")
	head := f.addHead("gd-wish", "the wish", "gardenio", opened, nil)
	head.CreatedBy = "alice"
	// A reply-less wish: without the synthesized marker the lead pane is blank.

	tl, err := fuseThreadTimeline(ctx, f, "gd-wish")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	// Precondition: the fuser really did drop the head bead_created entry.
	for _, e := range tl.Entries {
		if e.Kind == threadEntryBeadCreated && e.BeadID == "gd-wish" {
			t.Fatalf("test precondition broken: fuser emitted a head bead_created despite the batch omission")
		}
	}

	v := buildThreadDetailView(head, tl)
	convo := v.Conversation()
	if len(convo) != 1 {
		t.Fatalf("Conversation len = %d, want 1 (the synthesized open marker); got %+v", len(convo), convo)
	}
	if convo[0].IsComment || convo[0].Marker != "opened this wish" {
		t.Errorf("convo[0] = %+v, want the 'opened this wish' marker", convo[0])
	}
	if convo[0].Author != "alice" {
		t.Errorf("marker author = %q, want alice (from the resolved head)", convo[0].Author)
	}
	if !v.HasConversation() {
		t.Error("HasConversation() = false; the lead pane would be the empty void the guarantee forbids")
	}
	if got := v.OpenedAt(); !got.Equal(opened) {
		t.Errorf("OpenedAt() = %v, want %v (recovered from the resolved head)", got, opened)
	}
}

// TestBuildThreadDetailView_HeadMarkerNotDuplicatedWhenPresent proves the
// synthesis is a FALLBACK only: when the fuser already emitted the head
// bead_created (the normal, non-degraded path), no second marker is injected.
func TestBuildThreadDetailView_HeadMarkerNotDuplicatedWhenPresent(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	opened := ts("2026-06-20T09:00:00Z")
	head := f.addHead("gd-wish", "the wish", "gardenio", opened, nil)
	head.CreatedBy = "alice"

	tl, err := fuseThreadTimeline(ctx, f, "gd-wish")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	v := buildThreadDetailView(head, tl)
	openMarkers := 0
	for _, c := range v.Conversation() {
		if c.Marker == "opened this wish" {
			openMarkers++
		}
	}
	if openMarkers != 1 {
		t.Fatalf("open-marker count = %d, want exactly 1 (no duplication on the normal path)", openMarkers)
	}
}

// --- Gap 3: missing message body mis-files a message into the WORK lane -----

// TestFuseThreadTimeline_MessageBodyRaceStaysConversation proves that a
// type=message member whose body the BatchGetBeads raced past is STILL filed as
// conversation (via the thread_id field-query snapshot fallback), not mis-filed
// into the work lane. Without the fallback, isThreadMessageBead(nil)==false would
// drop it to the generic missing-member path and buildThreadDetailView would set
// IsMessage=false, landing the operator's reply in WorkMembers.
func TestFuseThreadTimeline_MessageBodyRaceStaysConversation(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	opened := ts("2026-06-20T09:00:00Z")
	head := f.addHead("gd-wish", "the wish", "gardenio", opened, nil)
	head.CreatedBy = "alice"

	msg := f.addMemberByField("gd-msg", "hi from the operator", "gd-wish",
		opened.Add(time.Hour), opened.Add(time.Hour))
	msg.Type = "message"
	msg.CreatedBy = "matt@example.com"
	msg.Fields["text"] = "hi from the operator"

	// The race: the field query still returns gd-msg, but BatchGetBeads omits it.
	f.omitFromBatch = map[string]bool{"gd-msg": true}

	tl, err := fuseThreadTimeline(ctx, f, "gd-wish")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}

	// The fused stream must carry exactly one message entry for gd-msg, with the
	// operator body and author recovered from the field snapshot — NOT a
	// bead_created work entry.
	var msgEntries, createdForMsg int
	for _, e := range tl.Entries {
		if e.BeadID != "gd-msg" {
			continue
		}
		switch e.Kind {
		case threadEntryMessage:
			msgEntries++
			if e.Text != "hi from the operator" {
				t.Errorf("message entry text = %q, want the operator body", e.Text)
			}
			if e.Author != "matt@example.com" {
				t.Errorf("message entry author = %q, want the operator email", e.Author)
			}
		case threadEntryBeadCreated:
			createdForMsg++
		}
	}
	if msgEntries != 1 {
		t.Fatalf("message entries for gd-msg = %d, want 1 (mis-filed as work otherwise)", msgEntries)
	}
	if createdForMsg != 0 {
		t.Errorf("gd-msg emitted %d bead_created (work) entries, want 0 — it is conversation", createdForMsg)
	}

	// And the view must flag it as a message so WorkMembers excludes it.
	v := buildThreadDetailView(head, tl)
	for _, m := range v.Members {
		if m.ID == "gd-msg" && !m.IsMessage {
			t.Error("gd-msg member row IsMessage=false; it would land in the WORK lane")
		}
	}
	for _, wm := range v.WorkMembers() {
		if wm.ID == "gd-msg" {
			t.Error("gd-msg appears in WorkMembers(); a conversation reply was mis-filed as agent work")
		}
	}
}

// TestAuditThreadTimeline_MessageBodyRacePasses proves the audit endpoint's
// independent re-derivation applies the SAME field-snapshot fallback, so a
// message-body race does not manufacture a spurious count/hash divergence — the
// fuser and auditor agree by construction.
func TestAuditThreadTimeline_MessageBodyRacePasses(t *testing.T) {
	ctx := context.Background()
	f := newThreadFake()
	opened := ts("2026-06-20T09:00:00Z")
	f.addHead("gd-wish", "the wish", "gardenio", opened, nil)
	f.beads["gd-wish"].CreatedBy = "alice"

	msg := f.addMemberByField("gd-msg", "hi from the operator", "gd-wish",
		opened.Add(time.Hour), opened.Add(time.Hour))
	msg.Type = "message"
	msg.CreatedBy = "matt@example.com"
	msg.Fields["text"] = "hi from the operator"
	f.omitFromBatch = map[string]bool{"gd-msg": true}

	rep, err := auditThreadTimeline(ctx, f, "gd-wish")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("audit must PASS under a message-body race (fuser+auditor agree): %+v", rep.Divergences)
	}
	if rep.Counts.Messages != 1 {
		t.Errorf("audit counted %d messages, want 1 (the raced message via the snapshot fallback)", rep.Counts.Messages)
	}
}
