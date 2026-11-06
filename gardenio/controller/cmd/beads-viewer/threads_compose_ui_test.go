package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Tests for the compose-new-wish UI on /threads and the reopen-on-message
// behaviour (ka-0000007860). The endpoint POST /threads/compose was fully wired
// + unit-tested (thread_compose_test.go) but had ZERO templates referencing it;
// these tests cover the index composer that now drives it, plus the "reopen a
// fulfilled wish when a new message arrives, just like Slack" path the operator
// asked for (ka-0000000329).

// --- index composer render + viewerOnly parity -----------------------------

// TestHandleThreads_RendersOperatorComposer proves the compose form renders for
// an operator on the index and posts channel+thread_ts+text+project to the
// dead-code-no-more POST /threads/compose endpoint.
func TestHandleThreads_RendersOperatorComposer(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{} // empty index is fine; we test the composer
	mux := mkServerFromFake(t, f)

	body, code := getBodyWithRoles(t, mux, "/threads", "matt@example.com", []string{oidcauth.RoleOperator})
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d, want 200", code)
	}
	// The form posts to the previously-dead endpoint.
	if !strings.Contains(body, `hx-post="/threads/compose"`) {
		t.Errorf("compose form must post to /threads/compose, body:\n%s", body)
	}
	// All four handler-read fields must be present as form inputs. Field names
	// are read verbatim by handleThreadCompose (r.PostForm.Get): channel,
	// thread_ts, text, project — they must match the handler exactly.
	for _, name := range []string{`name="channel"`, `name="thread_ts"`, `name="text"`, `name="project"`} {
		if !strings.Contains(body, name) {
			t.Errorf("compose form missing input %s", name)
		}
	}
}

// TestHandleThreads_ViewerSeesComposer proves a plain gardenio-viewer DOES see
// the compose box (ka-0000003462): viewerOnly parity with the route gate on POST
// /threads/compose (a viewer's post now succeeds). This is the regression guard
// for "I don't even see the UX" — the live AVA default role is gardenio-viewer,
// so the composer MUST render for viewers or no live user can reach it.
func TestHandleThreads_ViewerSeesComposer(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{}
	mux := mkServerFromFake(t, f)

	body, code := getBodyWithRoles(t, mux, "/threads", "viewer@example.com", []string{oidcauth.RoleViewer})
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d, want 200", code)
	}
	if !strings.Contains(body, `hx-post="/threads/compose"`) {
		t.Errorf("a gardenio-viewer MUST see the compose form (viewer-gated since ka-0000003462), body:\n%s", body)
	}
}

// TestHandleThreads_UnauthenticatedNoComposer proves an anonymous viewer (no
// email) never sees the composer even when RBAC is not enforced (no roles).
// CanCompose requires an authenticated identity to attribute the mint to.
func TestHandleThreads_UnauthenticatedNoComposer(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{}
	mux := mkServerFromFake(t, f)

	// No email header, no roles: dev-bypass posture, but CanCompose also gates on
	// email != "" so the composer stays hidden (the POST stamps CreatedBy from the
	// email and 401s without one).
	body, code := getBody(t, mux, "/threads")
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d, want 200", code)
	}
	if strings.Contains(body, `hx-post="/threads/compose"`) {
		t.Errorf("unauthenticated viewer must NOT see the compose form")
	}
}

// TestHandleThreads_ComposerPrefillsProjectScope proves the ?project= scope
// pre-fills the composer's project field so an operator on a scoped index mints
// into that project by default.
func TestHandleThreads_ComposerPrefillsProjectScope(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{}
	mux := mkServerFromFake(t, f)

	body, code := getBodyWithRoles(t, mux, "/threads?project=gardenio", "matt@example.com", []string{oidcauth.RoleOperator})
	if code != http.StatusOK {
		t.Fatalf("GET /threads?project=gardenio = %d, want 200", code)
	}
	if !strings.Contains(body, `name="project"`) {
		t.Fatalf("compose form missing project input")
	}
	if !strings.Contains(body, `value="gardenio"`) {
		t.Errorf("project field should pre-fill the ?project scope, body:\n%s", body)
	}
}

// --- reopen-on-message: "just like Slack" (ka-0000000329) ------------------

// composePostWithRoles posts a compose/reply form with an email AND an explicit
// operator role so the viewerOnly route admits it (operator outranks viewer).
// composePost in thread_compose_test.go omits roles, which passes only because
// RequireRole is a no-op when no roles are present.
func composePostWithRoles(t *testing.T, mux http.Handler, path, email string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
	}
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestThreadCompose_ReopensFulfilledWish proves that composing into an EXISTING
// fulfilled (closed) wish reopens it before the message posts — the operator's
// "reopen a wish and add messages, just like in slack" ask (ka-0000000329). The
// head is resolved by the label fast-path (no second mint), reopened to "open",
// then the message bead is created + closed for the bridge forward.
func TestThreadCompose_ReopensFulfilledWish(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-iafvsz3vyb", "C999", "1700000000.000200")
	f.beads["gd-iafvsz3vyb"].Status = "closed" // a fulfilled wish

	mux := mkServerFromFake(t, f)
	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "actually, one more thing")
	w := composePostWithRoles(t, mux, "/threads/compose", "matt@example.com", form)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// The head was reopened: exactly one UpdateBead flipping status to open.
	if f.updateCalls != 1 {
		t.Fatalf("updateCalls = %d, want 1 (reopen the fulfilled head)", f.updateCalls)
	}
	if f.updateLastID != "gd-iafvsz3vyb" {
		t.Errorf("reopened id = %q, want gd-iafvsz3vyb", f.updateLastID)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "open" {
		t.Errorf("reopen status = %v, want open", f.updateLastReq.Status)
	}
	if f.beads["gd-iafvsz3vyb"].Status != "open" {
		t.Errorf("head status after compose = %q, want open", f.beads["gd-iafvsz3vyb"].Status)
	}
	// No second head mint (resolved via fast-path); the only create is the
	// message bead, which was closed to fire the bridge forward.
	for _, req := range f.createReqs {
		if req.Type == "task" {
			t.Errorf("unexpected head mint when reopening existing wish: %+v", req)
		}
	}
	if f.createCalls != 1 || f.closeCalls != 1 {
		t.Errorf("createCalls=%d closeCalls=%d, want 1/1 (message bead only)", f.createCalls, f.closeCalls)
	}
}

// TestThreadCompose_OpenWishNotReopened proves composing into an already-open
// wish does NOT issue a redundant status update — reopen is a no-op unless the
// head is closed.
func TestThreadCompose_OpenWishNotReopened(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-openwish", "C999", "1700000000.000200") // seeded Status: "open"
	mux := mkServerFromFake(t, f)

	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "follow-up")
	w := composePostWithRoles(t, mux, "/threads/compose", "matt@example.com", form)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0 (open wish needs no reopen)", f.updateCalls)
	}
}

// TestThreadCompose_NewWishNotReopened proves the new-wish mint path issues no
// reopen UpdateBead — a freshly minted head is already open, so the
// resolveOrMintWishHead "existing == nil" guard skips the reopen entirely.
func TestThreadCompose_NewWishNotReopened(t *testing.T) {
	f := newFake()
	f.createIDForReq = func(req beadsapi.CreateBeadRequest) string {
		if req.Type == "task" {
			return "gd-head1"
		}
		return "gd-msg1"
	}
	mux := mkServerFromFake(t, f)

	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "please build the thing")
	w := composePostWithRoles(t, mux, "/threads/compose", "matt@example.com", form)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0 (a freshly minted head is already open)", f.updateCalls)
	}
}

// TestThreadReply_ReopensFulfilledWish proves the per-thread reply path (POST
// /b/{id}/thread-reply) also reopens a fulfilled wish before posting, so the
// detail-page composer revives a thread the same way the index composer does.
func TestThreadReply_ReopensFulfilledWish(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-wish1", "C123", "1700000000.000100")
	f.beads["gd-wish1"].Status = "closed"
	mux := mkServerFromFake(t, f)

	form := url.Values{}
	form.Set("text", "reviving this thread")
	w := composePostWithRoles(t, mux, "/b/gd-wish1/thread-reply", "matt@example.com", form)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 || f.updateLastID != "gd-wish1" {
		t.Fatalf("reopen: updateCalls=%d id=%q, want 1/gd-wish1", f.updateCalls, f.updateLastID)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "open" {
		t.Errorf("reopen status = %v, want open", f.updateLastReq.Status)
	}
	// The message bead is still posted + closed for the bridge forward.
	if f.createCalls != 1 || f.closeCalls != 1 {
		t.Errorf("createCalls=%d closeCalls=%d, want 1/1", f.createCalls, f.closeCalls)
	}
}

// TestReopenClosedWishHead_BestEffort proves a reopen failure does NOT abort the
// compose — the operator's message is the point, so the send still succeeds
// (204) and the message bead is created even when the status flip errors.
func TestThreadCompose_ReopenFailureStillPosts(t *testing.T) {
	f := newFake()
	seedWishHead(f, "gd-iafvsz3vyb", "C999", "1700000000.000200")
	f.beads["gd-iafvsz3vyb"].Status = "closed"
	f.updateErr = errThreadTest // reopen will fail
	mux := mkServerFromFake(t, f)

	form := url.Values{}
	form.Set("channel", "C999")
	form.Set("thread_ts", "1700000000.000200")
	form.Set("text", "still want this posted")
	w := composePostWithRoles(t, mux, "/threads/compose", "matt@example.com", form)

	// Best-effort reopen: the failed UpdateBead is logged, not surfaced, so the
	// send still succeeds and the message bead is created + closed.
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (reopen failure must not block the send); body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 || f.closeCalls != 1 {
		t.Errorf("createCalls=%d closeCalls=%d, want 1/1 (message still posts on reopen failure)", f.createCalls, f.closeCalls)
	}
}
