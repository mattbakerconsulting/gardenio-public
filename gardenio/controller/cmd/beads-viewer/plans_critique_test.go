package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestCritiqueAnchor_RoundTrip is the acceptance requirement: encode -> decode
// must preserve block_id/quote/prefix/suffix and hand back the clean body.
func TestCritiqueAnchor_RoundTrip(t *testing.T) {
	anchor := critiqueAnchor{
		BlockID: "blk-7",
		Quote:   "the quick brown fox",
		Prefix:  "...before the ",
		Suffix:  " jumped over...",
	}
	body := "This sentence is **wrong** — reword it.\n\nSecond paragraph."

	encoded := encodeCritiqueComment(anchor, body)
	if !strings.HasPrefix(encoded, critiqueAnchorOpen) {
		t.Fatalf("encoded text missing anchor prefix: %q", encoded)
	}

	gotAnchor, gotBody := decodeCritiqueComment(encoded)
	if gotAnchor != anchor {
		t.Errorf("anchor round-trip mismatch:\n want %+v\n  got %+v", anchor, gotAnchor)
	}
	if gotBody != body {
		t.Errorf("body round-trip mismatch:\n want %q\n  got %q", body, gotBody)
	}
}

// TestCritiqueAnchor_RoundTripAdversarialQuote proves a quote containing the
// literal close delimiter `-->` cannot corrupt the codec — encoding/json
// escapes `<`, `>`, and `&`, so the delimiter never appears raw in the payload.
func TestCritiqueAnchor_RoundTripAdversarialQuote(t *testing.T) {
	anchor := critiqueAnchor{
		BlockID: "blk-1",
		Quote:   "a comment closer --> and an <!--critique: opener too",
		Prefix:  "x&y<z>",
		Suffix:  "trailing -->",
	}
	body := "body with a --> in it too"

	gotAnchor, gotBody := decodeCritiqueComment(encodeCritiqueComment(anchor, body))
	if gotAnchor != anchor {
		t.Errorf("adversarial anchor round-trip mismatch:\n want %+v\n  got %+v", anchor, gotAnchor)
	}
	if gotBody != body {
		t.Errorf("adversarial body round-trip mismatch:\n want %q\n  got %q", body, gotBody)
	}
}

// TestCritiqueAnchor_EmptyAnchorNoPrefix verifies an unanchored comment is
// stored as plain text (no anchor prefix), and decodes back unchanged.
func TestCritiqueAnchor_EmptyAnchorNoPrefix(t *testing.T) {
	body := "plan-level comment, no line anchor"
	encoded := encodeCritiqueComment(critiqueAnchor{}, body)
	if encoded != body {
		t.Errorf("empty anchor should not add a prefix: got %q", encoded)
	}
	gotAnchor, gotBody := decodeCritiqueComment(encoded)
	if !gotAnchor.isEmpty() {
		t.Errorf("expected empty anchor, got %+v", gotAnchor)
	}
	if gotBody != body {
		t.Errorf("body mismatch: want %q, got %q", body, gotBody)
	}
}

// TestCritiqueComment_DecodePlainText verifies pre-existing / non-critique
// comments (no prefix) decode to a zero anchor and the verbatim text — nothing
// is lost or mis-parsed.
func TestCritiqueComment_DecodePlainText(t *testing.T) {
	for _, text := range []string{
		"a normal comment",
		"<!--critique: malformed with no close",
		"<!--critique:not-json-->\nbody",
		"",
	} {
		a, body := decodeCritiqueComment(text)
		if !a.isEmpty() {
			t.Errorf("text %q: expected empty anchor, got %+v", text, a)
		}
		if body != text {
			t.Errorf("text %q: expected verbatim body, got %q", text, body)
		}
	}
}

// authedCritiquePost builds an operator-authenticated POST to the critique
// endpoint with the step query param and form body.
func authedCritiquePost(planID, stepID string, form url.Values) *http.Request {
	req := httptest.NewRequest("POST", "/plans/"+planID+"/comments?step="+stepID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	return req
}

// TestAddPlanCritiqueComment_HappyPath posts an anchored critique comment and
// verifies it is stored on the STEP bead (not the plan) with the critique
// prefix, and that the re-rendered fragment shows the clean body + anchor.
func TestAddPlanCritiqueComment_HappyPath(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{
		"text":     {"This step is underspecified."},
		"block_id": {"blk-3"},
		"quote":    {"do the thing"},
		"prefix":   {"we should "},
		"suffix":   {" and ship"},
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedCritiquePost("gd-plan", "gd-step", form))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addCommentCalls != 1 {
		t.Fatalf("expected 1 AddComment call, got %d", f.addCommentCalls)
	}
	if f.addCommentLastID != "gd-step" {
		t.Errorf("comment must attach to the STEP bead, got %q", f.addCommentLastID)
	}
	if f.addCommentLastAuth != authorEmail {
		t.Errorf("author = %q, want %q", f.addCommentLastAuth, authorEmail)
	}
	if !strings.HasPrefix(f.addCommentLastText, critiqueAnchorOpen) {
		t.Errorf("stored text should carry the critique anchor prefix, got %q", f.addCommentLastText)
	}
	gotAnchor, gotBody := decodeCritiqueComment(f.addCommentLastText)
	if gotAnchor.BlockID != "blk-3" || gotAnchor.Quote != "do the thing" {
		t.Errorf("stored anchor mismatch: %+v", gotAnchor)
	}
	if gotBody != "This step is underspecified." {
		t.Errorf("stored body mismatch: %q", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, "This step is underspecified.") {
		t.Errorf("fragment missing rendered body\nbody=%s", body)
	}
	if !strings.Contains(body, `data-block-id="blk-3"`) {
		t.Errorf("fragment missing anchor data attribute\nbody=%s", body)
	}
	// The raw anchor prefix must never leak into the rendered HTML.
	if strings.Contains(body, critiqueAnchorOpen) {
		t.Errorf("rendered fragment leaked the raw anchor prefix\nbody=%s", body)
	}
}

// TestAddPlanCritiqueComment_Unauthenticated verifies the POST rejects an
// anonymous request with 401 and never writes (defense-in-depth behind
// operatorOnly).
func TestAddPlanCritiqueComment_Unauthenticated(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{"text": {"hello"}}
	req := httptest.NewRequest("POST", "/plans/gd-plan/comments?step=gd-step", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("must not write when unauthenticated, calls=%d", f.addCommentCalls)
	}
}

// TestAddPlanCritiqueComment_EmptyText verifies empty/whitespace text is a 400
// with no write.
func TestAddPlanCritiqueComment_EmptyText(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{"text": {"   "}}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedCritiquePost("gd-plan", "gd-step", form))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty text, got %d", w.Code)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("must not write for empty text, calls=%d", f.addCommentCalls)
	}
}

// TestAddPlanCritiqueComment_MissingStep verifies a POST with no ?step= is a
// 400 (the step bead is where the comment must land).
func TestAddPlanCritiqueComment_MissingStep(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{"text": {"hello"}}
	req := httptest.NewRequest("POST", "/plans/gd-plan/comments", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing step, got %d", w.Code)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("must not write without a step, calls=%d", f.addCommentCalls)
	}
}

// TestPlanCritiqueComments_GetRendersThread verifies the GET handler renders
// the flat thread fragment for the step's existing comments, decoding the
// anchor into data attributes and showing only the clean body.
func TestPlanCritiqueComments_GetRendersThread(t *testing.T) {
	f := newFake()
	stored := encodeCritiqueComment(critiqueAnchor{BlockID: "blk-9", Quote: "risky bit"}, "reconsider this")
	f.comments["gd-step"] = []beadsapi.Comment{{
		ID:     42,
		Author: "reviewer@example.com",
		Text:   stored,
	}}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan/comments?step=gd-step", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "reconsider this") {
		t.Errorf("fragment missing clean body\nbody=%s", body)
	}
	if !strings.Contains(body, `data-block-id="blk-9"`) {
		t.Errorf("fragment missing decoded anchor\nbody=%s", body)
	}
	if strings.Contains(body, critiqueAnchorOpen) {
		t.Errorf("GET fragment leaked the raw anchor prefix\nbody=%s", body)
	}
}

// TestPlanCritiqueComments_GetEmpty verifies an unknown step renders the empty
// state without error.
func TestPlanCritiqueComments_GetEmpty(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan/comments?step=gd-lmprguvqbl", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "plan-critique-thread") {
		t.Errorf("expected thread section markup, got %s", w.Body.String())
	}
}

// TestHandlePlanEvents_CommentsFrameOnStepComment is the T5 acceptance path
// (ka-0000007360): a critique comment posted on a step IN the plan's interest
// set emits a `plan-comments-updated` frame carrying the freshly-rendered
// critique thread for that step. The frame's data-step-id lets the gutter shim
// swap it into an open overlay bound to the same step.
func TestHandlePlanEvents_CommentsFrameOnStepComment(t *testing.T) {
	f := seedPlansFake()
	// The comment is re-read from live daemon state (not the event payload), so
	// seed the step's comment the same way the write path would have persisted
	// it — anchor-encoded, cleaned on render.
	stored := encodeCritiqueComment(critiqueAnchor{BlockID: "blk-3", Quote: "second thoughts"}, "please reconsider this step")
	f.comments["gd-step1"] = []beadsapi.Comment{{
		ID:     7,
		Author: "reviewer2@example.com",
		Text:   stored,
	}}
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// A comment lands on the plan's step child (in filters.steps).
	ch <- beadsapi.SSEEvent{
		Event: "beads.comment.added",
		Data:  []byte(`{"comment":{"id":7,"bead_id":"gd-step1","author":"reviewer2@example.com"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-comments-updated") {
		t.Fatalf("expected a plan-comments-updated frame, got: %s", got)
	}
	if !strings.Contains(got, "plan-critique-thread") {
		t.Errorf("expected the re-rendered critique thread fragment, got: %s", got)
	}
	if !strings.Contains(got, `data-step-id="gd-step1"`) {
		t.Errorf("expected the fragment to carry the step id for client-side matching, got: %s", got)
	}
	if !strings.Contains(got, "please reconsider this step") {
		t.Errorf("expected the freshly-rendered comment body, got: %s", got)
	}
	// The anchor prefix must never leak into the rendered frame.
	if strings.Contains(got, critiqueAnchorOpen) {
		t.Errorf("SSE frame leaked the raw anchor prefix, got: %s", got)
	}
	// A comment must not flash the step timeline or the narrative — only the
	// critique lane cares about comment events.
	if strings.Contains(got, "event: plan-updated") {
		t.Errorf("comment event should not emit a plan-updated (steps) frame, got: %s", got)
	}
	if strings.Contains(got, "event: plan-narrative-updated") {
		t.Errorf("comment event should not emit a narrative frame, got: %s", got)
	}
}

// TestHandlePlanEvents_CommentOnOutOfPlanStepDropped: a comment on a bead that
// is NOT in the plan's step interest set must be filtered out — no frame. This
// guards the "2nd operator's comment refreshes the RIGHT overlay" contract:
// the server never emits a frame for a step the stream does not own.
func TestHandlePlanEvents_CommentOnOutOfPlanStepDropped(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	ch <- beadsapi.SSEEvent{
		Event: "beads.comment.added",
		Data:  []byte(`{"comment":{"id":9,"bead_id":"gd-someoneelse","author":"x@example.com"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if strings.Contains(got, "event: plan-comments-updated") {
		t.Errorf("out-of-plan comment should not emit a frame, got: %s", got)
	}
}

// TestHandlePlanEvents_CommentOnEpicDropped: filters.steps is a superset that
// also contains the plan epic itself (buildPlanEventFilters seeds it so the
// epic's own lifecycle events re-render the timeline). A critique comment on
// the epic must NOT emit a plan-comments-updated frame — the critique overlay
// only ever binds to a real step child, so a frame carrying the epic id would
// be a wasted round-trip that no open overlay can match. Guards the
// gate-tightening in writePlanEvent's comment branch (ka-0000007360).
func TestHandlePlanEvents_CommentOnEpicDropped(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// gd-plan1 IS in filters.steps (the epic seeds itself), so this exercises
	// the epic-id guard specifically, not the out-of-plan path above.
	ch <- beadsapi.SSEEvent{
		Event: "beads.comment.added",
		Data:  []byte(`{"comment":{"id":11,"bead_id":"gd-plan1","author":"x@example.com"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if strings.Contains(got, "event: plan-comments-updated") {
		t.Errorf("comment on the plan epic should not emit a critique frame, got: %s", got)
	}
}

// TestHandlePlanEvents_CommentOnPhaseEpicDropped is the symmetric sibling of
// TestHandlePlanEvents_CommentOnEpicDropped for the SECOND non-step member of
// filters.steps: a phase sub-epic. buildPlanEventFilters seeds every phase
// sub-epic into both filters.steps (so its own status flip re-renders the
// timeline) AND filters.phaseEpicIDs. A critique comment on a phase sub-epic
// must NOT emit a plan-comments-updated frame — like the epic, a phase sub-epic
// is a timeline row, never a gutter critique step, so a frame carrying its id
// can match no open overlay. Pins the phase-epic drop guard in writePlanEvent's
// comment branch (plans.go ~L1081) that its epic-id sibling above leaves
// uncovered — seedPlansFake() has no phase_order, so filters.phaseEpicIDs is
// empty there and the guard is otherwise untested. Mutation check: neuter the
// `if _, isPhaseEpic := filters.phaseEpicIDs[stepID]; isPhaseEpic { return }`
// guard and this test fails (gd-Oh1DPG8OJJ).
func TestHandlePlanEvents_CommentOnPhaseEpicDropped(t *testing.T) {
	f := seedPhasedPlanFake() // seeds gd-phaseR/gd-phaseI as phase sub-epics
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// gd-phaseR IS in filters.steps AND filters.phaseEpicIDs (phase descent), so
	// this exercises the phase-epic guard specifically — it clears the in-steps
	// and the epic-id (stepID == epicID) gates and is dropped only by the
	// phaseEpicIDs check.
	ch <- beadsapi.SSEEvent{
		Event: "beads.comment.added",
		Data:  []byte(`{"comment":{"id":13,"bead_id":"gd-phaseR","author":"x@example.com"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if strings.Contains(got, "event: plan-comments-updated") {
		t.Errorf("comment on a phase sub-epic should not emit a critique frame, got: %s", got)
	}
}

// TestHandlePlanEvents_WidenedTopicsNoStepRegression is the T5 regression guard
// (ka-0000007360): widening planEventsTopics with beads.comment.added must NOT
// stop the existing lanes from firing. The subscription string carries the new
// topic AND a step close still live-updates the timeline (plan-updated).
func TestHandlePlanEvents_WidenedTopicsNoStepRegression(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// A step close — the canonical existing behavior that must survive widening.
	f.beads["gd-step1"].Status = "closed"
	enqueuePlanEvent(t, ch, "beads.bead.closed", "gd-step1")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-updated") {
		t.Errorf("step close must still emit a plan-updated frame after topic widening, got: %s", got)
	}
	// The subscription must include the new comment topic (so comments arrive at
	// all) without dropping the pre-existing bead/dependency topics.
	if f.eventStreamLastTopics != planEventsTopics {
		t.Errorf("EventStream topics = %q, want %q", f.eventStreamLastTopics, planEventsTopics)
	}
	if !strings.Contains(planEventsTopics, "beads.comment.added") {
		t.Errorf("planEventsTopics must include beads.comment.added, got %q", planEventsTopics)
	}
	for _, want := range []string{"beads.bead.>", "beads.dependency.added", "beads.dependency.removed"} {
		if !strings.Contains(planEventsTopics, want) {
			t.Errorf("widening dropped pre-existing topic %q from planEventsTopics %q", want, planEventsTopics)
		}
	}
}
