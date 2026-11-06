package main

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// composeForm builds an application/x-www-form-urlencoded POST with the
// operator email header the viewer's EmailFromRequest reads.
func composePost(t *testing.T, mux http.Handler, path, email string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// seedWishHead adds a Slack-bound wish head bead to the fake's store.
func seedWishHead(f *fakeDaemon, id, channel, threadTS string) {
	if f.beads == nil {
		f.beads = map[string]*beadsapi.BeadDetail{}
	}
	f.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Slack thread " + channel + "/" + threadTS,
		Status: "open",
		Labels: []string{"source:slack", threadBeadIdentityLabel(channel, threadTS), threadHeadLabel},
		Fields: map[string]string{
			threadSlackChannelField: channel,
			threadSlackTSField:      threadTS,
		},
	}
}

// fieldsOf decodes a CreateBeadRequest's Fields JSON for assertions.
func fieldsOf(t *testing.T, req beadsapi.CreateBeadRequest) map[string]string {
	t.Helper()
	out := map[string]string{}
	if len(req.Fields) == 0 {
		return out
	}
	if err := json.Unmarshal(req.Fields, &out); err != nil {
		t.Fatalf("decoding create fields: %v", err)
	}
	return out
}

// --- handleThreadReply: post into an existing wish thread ------------------

func TestThreadReply_PostsBridgeMediatedMessage(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("text", "hello from the composer")
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// Exactly one message bead created (the composer does NOT mint a head when
	// one already exists) and it was closed to fire the bridge forward.
	if f.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1 (message bead only)", f.createCalls)
	}
	if f.closeCalls != 1 {
		t.Fatalf("closeCalls = %d, want 1 (message bead closed to trigger forward)", f.closeCalls)
	}
	req := f.createLastReq
	if req.Type != "message" || req.Kind != "data" {
		t.Errorf("message bead type/kind = %q/%q, want message/data", req.Type, req.Kind)
	}
	if !slices.Contains(req.Labels, threadSayLabel) {
		t.Errorf("labels %v missing %q (the forwarder filter label)", req.Labels, threadSayLabel)
	}
	if !slices.Contains(req.Labels, threadMembershipLabel("gd-wish1")) {
		t.Errorf("labels %v missing membership label thread:gd-wish1", req.Labels)
	}
	fields := fieldsOf(t, req)
	if fields["text"] != "hello from the composer" {
		t.Errorf("text field = %q", fields["text"])
	}
	// The (channel, ts) binding must be copied onto the message bead so the
	// agent-less bridge forwarder can route it into the thread.
	if fields[threadSlackChannelField] != "C123" || fields[threadSlackTSField] != "1700000000.000100" {
		t.Errorf("binding fields = %q/%q, want C123/1700000000.000100",
			fields[threadSlackChannelField], fields[threadSlackTSField])
	}
	if fields[threadIDField] != "gd-wish1" {
		t.Errorf("thread_id = %q, want gd-wish1 (wish-pointer invariant)", fields[threadIDField])
	}
	// Operator attribution: CreatedBy is the operator email. (X-Operated-By is
	// stamped by operatorMiddleware in the production chain — out of scope for
	// the mux-only test harness, which exercises the handler + operatorOnly.)
	if req.CreatedBy != "matt@example.com" {
		t.Errorf("CreatedBy = %q, want matt@example.com", req.CreatedBy)
	}
	// The composer must NOT acquire a Slack token / call Slack itself — there
	// is no Slack client on the viewer Server at all (rubric gate A.7). The
	// only outward effect is the closed message bead the bridge forwards.
}

func TestThreadReply_EmptyText_400(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("text", "   ")
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 (no write on empty text)", f.createCalls)
	}
}

func TestThreadReply_TooLong_413(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("text", strings.Repeat("x", threadComposeMaxLen+1))
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", f.createCalls)
	}
}

func TestThreadReply_HeadNotFound_404(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("text", "hi")
	w := composePost(t, mux, "/b/gd-missing/thread-reply", "matt@example.com", form)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestThreadReply_HeadNotSlackBound_409(t *testing.T) {
	f := newFake()
	// A head with no slack binding fields — not a Slack-bound wish.
	f.beads["gd-wish1"] = &beadsapi.BeadDetail{
		ID: "gd-wish1", Title: "manual wish", Status: "open",
		Labels: []string{threadHeadLabel}, Fields: map[string]string{},
	}
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("text", "hi")
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (no slack binding); body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", f.createCalls)
	}
}

// viewerOnly gate (ka-0000003462): the compose route was lowered from operator
// to viewer so a signed-in user can post into a thread (same trust level as a
// bead comment). A caller carrying the gardenio-viewer role now PASSES — this is
// the live AVA default-role posture, which the old operatorOnly gate rejected,
// making the composer invisible to everyone.
func TestThreadReply_ViewerRolePasses(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("text", "viewer says hi")
	req := httptest.NewRequest(http.MethodPost, "/b/gd-wish1/thread-reply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "viewer@example.com")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for viewer role (composer is viewer-gated); body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", f.createCalls)
	}
}

// viewerOnly still rejects a caller whose only role ranks BELOW viewer — e.g. a
// full-path AVA group string ("Acme/Platform/gardenio-viewer") that does not
// match any bare role name in the rank map. RequireRole passes through only
// when NO roles are present (dev/internal bypass), so the negative test injects
// an unmapped role explicitly. This documents that the gate is still a real
// gate, not an open door — and that the live fix is "match the bare role name",
// not "drop RBAC".
func TestThreadReply_SubViewerRole_403(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("text", "hi")
	req := httptest.NewRequest(http.MethodPost, "/b/gd-wish1/thread-reply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "stranger@example.com")
	// An unmapped role (rank 0) — mirrors a full-path AVA group that never
	// matches a bare gardenio-* role name.
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{"Acme/Platform/gardenio-viewer"}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for sub-viewer (unmapped) role", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 (gate blocks before any write)", f.createCalls)
	}
}

func TestThreadReply_OperatorRolePasses(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("text", "operator says hi")
	req := httptest.NewRequest(http.MethodPost, "/b/gd-wish1/thread-reply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for operator role; body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", f.createCalls)
	}
}

func TestThreadReply_Unauthenticated_401(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	mux := mkServer(t, f)
	// No X-Auth-Request-Email header → EmailFromRequest == "".
	form := url.Values{}
	form.Set("text", "hi")
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "", form)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", f.createCalls)
	}
}

func TestThreadReply_GetBeadError_500(t *testing.T) {
	f := newFake()
	f.getBeadErr = errors.New("daemon down")
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("text", "hi")
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestThreadReply_CreateError_502(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	f.createErr = errors.New("create failed")
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("text", "hi")
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestThreadReply_CloseError_502(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	f.closeErr = errors.New("close failed")
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("text", "hi")
	w := composePost(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (close failure surfaces as post failure)", w.Code)
	}
	// The message bead was created and the close was attempted.
	if f.createCalls != 1 || f.closeCalls != 1 {
		t.Errorf("createCalls=%d closeCalls=%d, want 1/1", f.createCalls, f.closeCalls)
	}
}

// --- handleThreadCompose: mint-or-resolve on first message -----------------

func TestThreadCompose_MintsWishAndPostsFirstMessage(t *testing.T) {
	f := newFake()
	// Distinct ids for the head (task) vs the message (data) create.
	f.createIDForReq = func(req beadsapi.CreateBeadRequest) string {
		if req.Type == "task" {
			return "gd-head1"
		}
		return "gd-msg1"
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("project", "gardenio")
	form.Set("text", "please build the thing")
	w := composePost(t, mux, "/threads/compose", "matt@example.com", form)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Redirect"); got != "/threads/gd-head1" {
		t.Errorf("HX-Redirect = %q, want /threads/gd-head1", got)
	}
	// Two creates: the wish head mint + the first message bead.
	if f.createCalls != 2 {
		t.Fatalf("createCalls = %d, want 2 (head mint + message)", f.createCalls)
	}
	// First create = the head, minted via the IdempotencyKey dedup guard.
	head := f.createReqs[0]
	if head.Type != "task" || head.Kind != "issue" {
		t.Errorf("head type/kind = %q/%q, want task/issue", head.Type, head.Kind)
	}
	// The head must be minted at an EXPLICIT priority, never the P0 zero value
	// (ka-0000003760): an omitted CreateBeadRequest.Priority marshals as an
	// explicit "priority":0 == P0, which is what let Slack-conversation heads
	// saturate `gb ready` and starve the bug lane. This is the beads-viewer
	// "+ new wish" sibling of the bridge mint site fixed first.
	if head.Priority != threadHeadPriority {
		t.Errorf("head Priority = %d, want %d (default, not the P0 zero value)", head.Priority, threadHeadPriority)
	}
	if head.Priority == 0 {
		t.Error("viewer-minted wish head landed at P0 — the exact ka-0000003760 regression")
	}
	wantKey := threadBeadIdentityLabel("C999", "1700000000.000200")
	if head.IdempotencyKey != wantKey {
		t.Errorf("head IdempotencyKey = %q, want %q", head.IdempotencyKey, wantKey)
	}
	if !slices.Contains(head.Labels, threadHeadLabel) {
		t.Errorf("head labels %v missing the bare %q kind label", head.Labels, threadHeadLabel)
	}
	if !slices.Contains(head.Labels, wantKey) {
		t.Errorf("head labels %v missing the canonical identity label", head.Labels)
	}
	if !slices.Contains(head.Labels, "project:gardenio") {
		t.Errorf("head labels %v missing project:gardenio", head.Labels)
	}
	headFields := fieldsOf(t, head)
	if headFields[threadSlackChannelField] != "C999" || headFields[threadSlackTSField] != "1700000000.000200" {
		t.Errorf("head binding fields = %q/%q", headFields[threadSlackChannelField], headFields[threadSlackTSField])
	}
	if headFields[threadRootTextField] != "please build the thing" {
		t.Errorf("head slack_root_text = %q, want the opening message", headFields[threadRootTextField])
	}
	// The head must NOT carry thread_id — it IS the wish.
	if _, ok := headFields[threadIDField]; ok {
		t.Errorf("head must not carry thread_id (it is the wish), got %q", headFields[threadIDField])
	}
	// Second create = the message bead, pointing back at the minted head.
	msg := f.createReqs[1]
	if msg.Type != "message" {
		t.Errorf("message bead type = %q, want message", msg.Type)
	}
	msgFields := fieldsOf(t, msg)
	if msgFields[threadIDField] != "gd-head1" {
		t.Errorf("message thread_id = %q, want gd-head1", msgFields[threadIDField])
	}
}

// THE rubric gate A.8 test — RACE-BACKSTOP path: two submits both MISS the
// label fast-path (the first head is not yet visible to the second lookup, the
// concurrent-miss case) and race into CreateBead. The daemon's IdempotencyKey
// resolve-or-create collapses them to ONE head; the steady-state fast-path is
// covered separately by TestThreadCompose_DoubleSubmit_FastPathResolvesOneWish.
// Here createOnSuccess is intentionally nil so submit #1's head is NOT seeded
// into the store — forcing both submits down the IdempotencyKey backstop, which
// is the only guard that holds when two racers (even across binaries) both miss
// the pre-lookup.
func TestThreadCompose_DoubleSubmit_MintsExactlyOneWish(t *testing.T) {
	f := newFake()
	headMints := 0
	f.createIDForReq = func(req beadsapi.CreateBeadRequest) string {
		if req.Type == "task" {
			headMints++ // counts only genuine (non-deduped) head inserts
			return "gd-head1"
		}
		return "gd-msg"
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "build it")

	w1 := composePost(t, mux, "/threads/compose", "matt@example.com", form)
	w2 := composePost(t, mux, "/threads/compose", "matt@example.com", form)

	if w1.Code != http.StatusNoContent || w2.Code != http.StatusNoContent {
		t.Fatalf("statuses = %d,%d, want 204,204", w1.Code, w2.Code)
	}
	// Exactly one genuine head insert despite two submits: the second
	// CreateBead(IdempotencyKey=…) resolved to the first head and minted
	// nothing new (createIDForReq, where headMints lives, runs only on the
	// non-deduped insert path).
	if headMints != 1 {
		t.Fatalf("genuine head mints = %d, want 1 (double-submit must dedup to one wish)", headMints)
	}
	// Both submits resolved to the SAME head id.
	if id := f.createByIdemKey[threadBeadIdentityLabel("C999", "1700000000.000200")]; id != "gd-head1" {
		t.Errorf("deduped head id = %q, want gd-head1", id)
	}
	// Both submits posted a message (the dedup is on the head, not the message).
	msgCreates := 0
	for _, req := range f.createReqs {
		if req.Type == "message" {
			msgCreates++
		}
	}
	if msgCreates != 2 {
		t.Errorf("message creates = %d, want 2 (one per submit)", msgCreates)
	}
}

// Rubric gate A.8 — STEADY-STATE fast-path: the literal sequential double-submit
// where submit #1's head IS committed and visible to submit #2. createOnSuccess
// seeds the minted head into the store, so submit #2's resolveOrMintWishHead
// label pre-lookup (thread_compose.go) resolves the existing head and never
// reaches CreateBead at all — proving dedup holds on the cheap path too, not
// just the IdempotencyKey backstop.
func TestThreadCompose_DoubleSubmit_FastPathResolvesOneWish(t *testing.T) {
	f := newFake()
	headMints := 0
	f.createIDForReq = func(req beadsapi.CreateBeadRequest) string {
		if req.Type == "task" {
			headMints++
			return "gd-head1"
		}
		return "gd-msg"
	}
	// Seed each minted head into the store so the next submit's label fast-path
	// sees it (the steady-state, head-already-committed case).
	f.createOnSuccess = func(fd *fakeDaemon, id string, req beadsapi.CreateBeadRequest) {
		if req.Type != "task" {
			return
		}
		var fields map[string]string
		_ = json.Unmarshal(req.Fields, &fields)
		fd.beads[id] = &beadsapi.BeadDetail{ID: id, Title: req.Title, Status: "open", Labels: req.Labels, Fields: fields}
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "build it")

	w1 := composePost(t, mux, "/threads/compose", "matt@example.com", form)
	w2 := composePost(t, mux, "/threads/compose", "matt@example.com", form)

	if w1.Code != http.StatusNoContent || w2.Code != http.StatusNoContent {
		t.Fatalf("statuses = %d,%d, want 204,204", w1.Code, w2.Code)
	}
	if headMints != 1 {
		t.Fatalf("genuine head mints = %d, want 1 (fast-path must resolve the committed head)", headMints)
	}
	// Both submits redirect to the same wish detail.
	if w1.Header().Get("HX-Redirect") != "/threads/gd-head1" || w2.Header().Get("HX-Redirect") != "/threads/gd-head1" {
		t.Errorf("redirects = %q,%q, want both /threads/gd-head1",
			w1.Header().Get("HX-Redirect"), w2.Header().Get("HX-Redirect"))
	}
}

// Pre-existing head (minted earlier by the slack-bridge on a Slack mention):
// compose resolves it via the label fast-path and does NOT mint a second head.
func TestThreadCompose_ResolvesExistingHead_NoSecondMint(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-existing", "C999", "1700000000.000200")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "follow-up message")
	w := composePost(t, mux, "/threads/compose", "matt@example.com", form)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Redirect"); got != "/threads/gd-existing" {
		t.Errorf("HX-Redirect = %q, want /threads/gd-existing", got)
	}
	// Only the message bead is created — the head already existed, resolved by
	// the label fast-path, so no head mint.
	for _, req := range f.createReqs {
		if req.Type == "task" {
			t.Errorf("unexpected head mint when head already exists: %+v", req)
		}
	}
	if f.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (message bead only)", f.createCalls)
	}
}

func TestThreadCompose_InvalidBinding_400(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	cases := []struct{ channel, ts string }{
		{"", "1700000000.000200"},  // no channel
		{"C999", ""},               // no ts
		{"C999", "no-dot-here"},    // malformed ts
		{"C", "1700000000.000200"}, // channel too short
	}
	for _, c := range cases {
		form := url.Values{}
		form.Set("channel", c.channel)
		form.Set("thread_ts", c.ts)
		form.Set("text", "hi")
		w := composePost(t, mux, "/threads/compose", "matt@example.com", form)
		if w.Code != http.StatusBadRequest {
			t.Errorf("channel=%q ts=%q: status = %d, want 400", c.channel, c.ts, w.Code)
		}
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 (no mint on invalid binding)", f.createCalls)
	}
}

func TestThreadCompose_EmptyText_400(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "  ")
	w := composePost(t, mux, "/threads/compose", "matt@example.com", form)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", f.createCalls)
	}
}

func TestThreadCompose_Unauthenticated_401(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "hi")
	w := composePost(t, mux, "/threads/compose", "", form)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestThreadCompose_MintError_502(t *testing.T) {
	f := newFake()
	f.createErr = errors.New("mint failed")
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "hi")
	w := composePost(t, mux, "/threads/compose", "matt@example.com", form)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on mint failure", w.Code)
	}
}

func TestThreadCompose_MessagePostError_502(t *testing.T) {
	f := newFake()
	// Head mint succeeds; the message-bead close fails.
	f.createIDForReq = func(req beadsapi.CreateBeadRequest) string {
		if req.Type == "task" {
			return "gd-head1"
		}
		return "gd-msg1"
	}
	f.closeErr = errors.New("close failed")
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "hi")
	w := composePost(t, mux, "/threads/compose", "matt@example.com", form)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when the first message fails to post", w.Code)
	}
}

// viewerOnly gate (ka-0000003462): a gardenio-viewer can now compose a new
// thread — the route was lowered from operator to viewer so live AVA users
// (default role gardenio-viewer) can reach it.
func TestThreadCompose_ViewerRolePasses(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "hi")
	req := httptest.NewRequest(http.MethodPost, "/threads/compose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "viewer@example.com")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for viewer role (compose is viewer-gated); body=%s", w.Code, w.Body.String())
	}
}

// viewerOnly still rejects a role ranking BELOW viewer (rank 0 / unmapped) —
// e.g. a full-path AVA group that never matches a bare gardenio-* role name.
func TestThreadCompose_SubViewerRole_403(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "hi")
	req := httptest.NewRequest(http.MethodPost, "/threads/compose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "stranger@example.com")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{"Acme/Platform/gardenio-viewer"}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for sub-viewer (unmapped) role", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", f.createCalls)
	}
}

// --- unit coverage for the small helpers -----------------------------------

func TestValidThreadBinding(t *testing.T) {
	cases := []struct {
		channel, ts string
		want        bool
	}{
		{"C123", "1700000000.000100", true},
		{"D9", "1.2", true},
		{"", "1.2", false},
		{"C123", "", false},
		{"C", "1.2", false},
		{"C123", "nodot", false},
	}
	for _, c := range cases {
		if got := validThreadBinding(c.channel, c.ts); got != c.want {
			t.Errorf("validThreadBinding(%q,%q) = %v, want %v", c.channel, c.ts, got, c.want)
		}
	}
}

func TestThreadIdentityLabels(t *testing.T) {
	if got := threadBeadIdentityLabel("C1", "2.3"); got != "slack-thread:C1:2.3" {
		t.Errorf("threadBeadIdentityLabel = %q", got)
	}
	if got := threadMembershipLabel("gd-x"); got != "thread:gd-x" {
		t.Errorf("threadMembershipLabel = %q", got)
	}
}

// viewerThreadSayContract is internal/bridge/testdata/viewer_thread_say.json:
// the thread-say bead the composer writes for a head, which the bridge test
// TestViewerReply_ContractBeadNudgesSameLandAgent feeds to the forwarder.
type viewerThreadSayContract struct {
	Head struct {
		ID     string            `json:"id"`
		Labels []string          `json:"labels"`
		Fields map[string]string `json:"fields"`
	} `json:"head"`
	Operator string `json:"operator"`
	Message  struct {
		Labels []string          `json:"labels"`
		Fields map[string]string `json:"fields"`
	} `json:"message"`
}

func loadViewerThreadSayContract(t *testing.T) viewerThreadSayContract {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "bridge", "testdata", "viewer_thread_say.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c viewerThreadSayContract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A reply the composer writes carries its head's land as project:<land>, the
// only place the bridge reads a thread-say's land (gd-oG21yWzoKL): it is the
// exact bead the bridge's contract test shows nudging that land's agent.
func TestThreadReply_WritesTheBridgeContractBead(t *testing.T) {
	c := loadViewerThreadSayContract(t)
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{c.Head.ID: {
		ID: c.Head.ID, Title: "a wish", Status: "open", Labels: c.Head.Labels, Fields: c.Head.Fields,
	}}
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("text", c.Message.Fields["text"])
	if w := composePost(t, mux, "/b/"+c.Head.ID+"/thread-reply", c.Operator, form); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	req := f.createLastReq
	labels := slices.Clone(req.Labels)
	slices.Sort(labels)
	want := slices.Clone(c.Message.Labels)
	slices.Sort(want)
	if !slices.Equal(labels, want) {
		t.Errorf("message labels = %v, want the contract's %v", labels, want)
	}
	if got := fieldsOf(t, req); !maps.Equal(got, c.Message.Fields) {
		t.Errorf("message fields = %v, want the contract's %v", got, c.Message.Fields)
	}
	if req.CreatedBy != c.Operator {
		t.Errorf("CreatedBy = %q, want %q", req.CreatedBy, c.Operator)
	}
}

// The land stamped on a composer message is the head's, never the form's
// project, and a head of no single land gives none.
func TestThreadCompose_StampsTheHeadsLandNotTheForms(t *testing.T) {
	for _, c := range []struct {
		name   string
		labels []string
		want   string // "" = no project label
	}{
		{"one land", []string{"thread", "project:gardenio"}, "gardenio"},
		{"no land", []string{"thread"}, ""},
		{"two lands", []string{"thread", "project:gardenio", "project:otherland"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			seedWishHead(f, "gd-wishl", "C0L", "1700000000.000300")
			f.beads["gd-wishl"].Labels = append(f.beads["gd-wishl"].Labels, c.labels...)
			mux := mkServer(t, f)
			form := url.Values{}
			form.Set("channel", "C0L")
			form.Set("thread_ts", "1700000000.000300")
			form.Set("project", "elsewhere")
			form.Set("text", "hello")
			if w := composePost(t, mux, "/threads/compose", "matt@example.com", form); w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
			}
			var got []string
			for _, l := range f.createLastReq.Labels {
				if strings.HasPrefix(l, "project:") {
					got = append(got, l)
				}
			}
			switch {
			case c.want == "" && len(got) != 0:
				t.Errorf("project labels = %v, want none", got)
			case c.want != "" && !slices.Equal(got, []string{"project:" + c.want}):
				t.Errorf("project labels = %v, want [project:%s]", got, c.want)
			}
		})
	}
}
