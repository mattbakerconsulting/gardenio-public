package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/cmd/beads-viewer/viewer"
	"gardenio/controller/internal/beadsapi"
)

// statusMessageFakeLookup maps an OIDC subject to (operator_id, kind, found).
// Mirrors inboxFakeLookup but kept local so this file is self-contained.
type statusMessageFakeLookup struct {
	rows map[string]struct {
		id    int64
		kind  string
		found bool
	}
}

func (f *statusMessageFakeLookup) LookupOperatorByAlias(_ context.Context, _, subject string) (*beadsapi.LookupOperatorByAliasResponse, error) {
	row, ok := f.rows[subject]
	if !ok {
		return &beadsapi.LookupOperatorByAliasResponse{Found: false}, nil
	}
	return &beadsapi.LookupOperatorByAliasResponse{
		OperatorID:   row.id,
		OperatorKind: row.kind,
		Found:        row.found,
	}, nil
}

// statusMessageTestEmailCtxKey carries the viewer email across the test
// boundary into the resolver's custom email extractor.
type statusMessageTestEmailCtxKey struct{}

// mkStatusMessageMux builds a server + mux wired with a resolver that pulls
// the viewer email from a per-request context value. lookup may be nil to
// exercise the non-self code path (no resolver attached).
func mkStatusMessageMux(t *testing.T, daemon beadAPI, lookup viewer.AliasLookup) http.Handler {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", "", "", nil)
	if lookup != nil {
		srv.SetResolver(viewer.NewResolver(lookup, viewer.WithEmailExtractor(func(ctx context.Context) string {
			v, _ := ctx.Value(statusMessageTestEmailCtxKey{}).(string)
			return v
		})))
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}

// statusMessageRequest builds a request with the viewer email in both the
// X-Auth-Request-Email header (oidcauth.EmailFromRequest) and a context value
// (read by the resolver's custom extractor). Both are needed for the self
// check to evaluate.
func statusMessageRequest(method, target, email string, body url.Values) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
		ctx := context.WithValue(req.Context(), statusMessageTestEmailCtxKey{}, email)
		req = req.WithContext(ctx)
	}
	return req
}

// statusMessageFake seeds a fakeDaemon with a slug→operator mapping and a
// listResp containing the user bead. The user bead carries operator_id="42"
// so lookupUserBeadByOperator matches, and status_message is the field
// under test.
func statusMessageFake(message string) *fakeDaemon {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	userBead := &beadsapi.BeadDetail{
		ID:    "gd-user-alice",
		Type:  "user",
		Kind:  "config",
		Title: "alice",
		Fields: map[string]string{
			"operator_id":    "42",
			"email":          "alice@example.com",
			"status_message": message,
		},
	}
	// The fakeDaemon's ListBeadsFiltered falls through to listResp for the
	// type=user kind=config query the lookup uses.
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{userBead}}
	f.beads[userBead.ID] = userBead
	return f
}

// TestLookupUserBeadByOperator_EmailFallback covers the email fallback path:
// a user bead that carries `email` but NO `operator_id` field must still be
// resolved from its operator id by way of the operator directory. This is the
// common production shape (ka-0000001751) — without the fallback, pinned beads
// and status messages are written by email but never read back.
func TestLookupUserBeadByOperator_EmailFallback(t *testing.T) {
	f := newFake()
	// User bead has email but no operator_id — the real-world shape.
	userBead := &beadsapi.BeadDetail{
		ID:    "gd-user-matt",
		Type:  "user",
		Kind:  "config",
		Title: "Matt Baker",
		Fields: map[string]string{
			"email":         "matt@example.com",
			"profile_prefs": `{"pinned_bead_ids":["gd-aaa"]}`,
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{userBead}}
	// Operator directory maps operator 229 → matt@example.com.
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 229, Email: "matt@example.com", Slug: "op-229"},
		},
	}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	got, err := srv.lookupUserBeadByOperator(context.Background(), 229)
	if err != nil {
		t.Fatalf("lookupUserBeadByOperator: %v", err)
	}
	if got == nil {
		t.Fatal("expected user bead via email fallback, got nil")
	}
	if got.ID != "gd-user-matt" {
		t.Errorf("got bead %q, want gd-user-matt", got.ID)
	}

	// The pinned-section read path must now see the persisted pins.
	prefs, err := srv.lookupProfilePrefByOperator(context.Background(), 229)
	if err != nil {
		t.Fatalf("lookupProfilePrefByOperator: %v", err)
	}
	if len(prefs.PinnedBeadIDs) != 1 || prefs.PinnedBeadIDs[0] != "gd-aaa" {
		t.Errorf("pinned ids = %v, want [gd-aaa]", prefs.PinnedBeadIDs)
	}
}

// TestLookupUserBeadByOperator_OperatorIDMatchWins ensures the operator_id
// field remains the primary key: when present it matches without consulting
// the operator directory.
func TestLookupUserBeadByOperator_OperatorIDMatchWins(t *testing.T) {
	f := newFake()
	userBead := &beadsapi.BeadDetail{
		ID:     "gd-user-42",
		Type:   "user",
		Kind:   "config",
		Fields: map[string]string{"operator_id": "42", "email": "x@example.com"},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{userBead}}
	// Directory deliberately empty — a primary-key match must not need it.
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	got, err := srv.lookupUserBeadByOperator(context.Background(), 42)
	if err != nil {
		t.Fatalf("lookupUserBeadByOperator: %v", err)
	}
	if got == nil || got.ID != "gd-user-42" {
		t.Fatalf("got %v, want gd-user-42", got)
	}
}

// TestLookupUserBeadByOperator_NoMatch returns (nil, nil) when neither key
// resolves — operators without a provisioned user bead simply have no prefs.
func TestLookupUserBeadByOperator_NoMatch(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{
		ID:     "gd-user-other",
		Type:   "user",
		Kind:   "config",
		Fields: map[string]string{"email": "someone@example.com"},
	}}}
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 7, Email: "nobody@example.com"},
		},
	}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	got, err := srv.lookupUserBeadByOperator(context.Background(), 7)
	if err != nil {
		t.Fatalf("lookupUserBeadByOperator: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for no match, got %q", got.ID)
	}
}

// TestStatusMessageFromBead covers the nil-safe field extractor.
func TestStatusMessageFromBead(t *testing.T) {
	if got := statusMessageFromBead(nil); got != "" {
		t.Errorf("nil bead → %q, want empty", got)
	}
	if got := statusMessageFromBead(&beadsapi.BeadDetail{}); got != "" {
		t.Errorf("no fields → %q, want empty", got)
	}
	bead := &beadsapi.BeadDetail{Fields: map[string]string{"status_message": "  hello  "}}
	if got := statusMessageFromBead(bead); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestHandleUserStatusMessageView renders the read-only block fragment with
// the persisted status_message and no edit affordance for a peer viewer.
func TestHandleUserStatusMessageView_Peer(t *testing.T) {
	mux := mkStatusMessageMux(t, statusMessageFake("shipping the slug feature"), nil)

	req := httptest.NewRequest("GET", "/u/alice/status-message", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="user-status-message"`) {
		t.Errorf("missing #user-status-message wrapper: %s", body)
	}
	if !strings.Contains(body, "shipping the slug feature") {
		t.Errorf("missing status_message value: %s", body)
	}
	// Peer viewers see no edit affordance.
	if strings.Contains(body, "Set a status message") || strings.Contains(body, ">Edit<") {
		t.Errorf("peer view should not include edit affordance: %s", body)
	}
}

// TestHandleUserStatusMessageView_Self renders the read-only block with the
// inline edit button for a viewer whose operator id matches the slug.
func TestHandleUserStatusMessageView_Self(t *testing.T) {
	lookup := &statusMessageFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{"alice@example.com": {id: 42, kind: "human", found: true}},
	}
	mux := mkStatusMessageMux(t, statusMessageFake("self-status"), lookup)

	req := statusMessageRequest("GET", "/u/alice/status-message", "alice@example.com", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "self-status") {
		t.Errorf("missing status_message value: %s", body)
	}
	if !strings.Contains(body, "/u/alice/status-message/edit") {
		t.Errorf("self view should include edit affordance: %s", body)
	}
}

// TestHandleUserStatusMessageEdit_RequiresSelf rejects a viewer whose operator
// id does not match the slug-owner's operator id with 403.
func TestHandleUserStatusMessageEdit_RequiresSelf(t *testing.T) {
	lookup := &statusMessageFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: 42, kind: "human", found: true},
			"bob@example.com":   {id: 99, kind: "human", found: true},
		},
	}
	mux := mkStatusMessageMux(t, statusMessageFake("private"), lookup)

	// Anonymous → 403.
	req := httptest.NewRequest("GET", "/u/alice/status-message/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("anonymous edit: got %d want 403", w.Code)
	}

	// Different operator → 403.
	req = statusMessageRequest("GET", "/u/alice/status-message/edit", "bob@example.com", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("peer edit: got %d want 403", w.Code)
	}
}

// TestHandleUserStatusMessageEdit_Self renders the editor pre-filled with the
// current value when the viewer's operator id matches the slug-owner.
func TestHandleUserStatusMessageEdit_Self(t *testing.T) {
	lookup := &statusMessageFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{"alice@example.com": {id: 42, kind: "human", found: true}},
	}
	mux := mkStatusMessageMux(t, statusMessageFake("on call"), lookup)

	req := statusMessageRequest("GET", "/u/alice/status-message/edit", "alice@example.com", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="status_message"`,
		`value="on call"`,
		`hx-post="/u/alice/status-message"`,
		`hx-get="/u/alice/status-message"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor body missing %q: %s", want, body)
		}
	}
}

// TestHandleUpdateUserStatusMessage_RequiresSelf blocks POSTs from peers /
// anonymous viewers with 403.
func TestHandleUpdateUserStatusMessage_RequiresSelf(t *testing.T) {
	lookup := &statusMessageFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: 42, kind: "human", found: true},
			"bob@example.com":   {id: 99, kind: "human", found: true},
		},
	}
	f := statusMessageFake("")
	mux := mkStatusMessageMux(t, f, lookup)

	// Anonymous POST → 403, no write attempted.
	form := url.Values{"status_message": []string{"hijack"}}
	req := httptest.NewRequest("POST", "/u/alice/status-message", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("anonymous POST: got %d want 403", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("anonymous POST should not write fields; got %d calls", f.updateFieldsCalls)
	}

	// Peer POST → 403, no write attempted.
	req = statusMessageRequest("POST", "/u/alice/status-message", "bob@example.com", form)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("peer POST: got %d want 403", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("peer POST should not write fields; got %d calls", f.updateFieldsCalls)
	}
}

// TestHandleUpdateUserStatusMessage_Self persists the trimmed value to the
// operator's user bead and re-renders the read-only block.
func TestHandleUpdateUserStatusMessage_Self(t *testing.T) {
	lookup := &statusMessageFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{"alice@example.com": {id: 42, kind: "human", found: true}},
	}
	f := statusMessageFake("")
	mux := mkStatusMessageMux(t, f, lookup)

	form := url.Values{"status_message": []string{"  heads-down on M2 §3  "}}
	req := statusMessageRequest("POST", "/u/alice/status-message", "alice@example.com", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("expected 1 UpdateBeadFieldsTyped call, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-user-alice" {
		t.Errorf("wrote to wrong bead: %s", f.updateFieldsLastID)
	}
	if got, _ := f.updateFieldsLastVal["status_message"].(string); got != "heads-down on M2 §3" {
		t.Errorf("persisted status_message = %q, want %q", got, "heads-down on M2 §3")
	}
	body := w.Body.String()
	if !strings.Contains(body, "heads-down on M2 §3") {
		t.Errorf("response should re-render block with new value: %s", body)
	}
	if !strings.Contains(body, `id="user-status-message"`) {
		t.Errorf("response should render the block wrapper: %s", body)
	}
}

// TestHandleUpdateUserStatusMessage_Clear submits an empty form to clear the
// field — the server should persist "" and not reject the request.
func TestHandleUpdateUserStatusMessage_Clear(t *testing.T) {
	lookup := &statusMessageFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{"alice@example.com": {id: 42, kind: "human", found: true}},
	}
	f := statusMessageFake("old message")
	mux := mkStatusMessageMux(t, f, lookup)

	form := url.Values{"status_message": []string{""}}
	req := statusMessageRequest("POST", "/u/alice/status-message", "alice@example.com", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got, _ := f.updateFieldsLastVal["status_message"].(string); got != "" {
		t.Errorf("clear: persisted = %q, want empty", got)
	}
}

// TestHandleUpdateUserStatusMessage_TooLong rejects oversized payloads with
// 413 and preserves the draft in the editor re-render.
func TestHandleUpdateUserStatusMessage_TooLong(t *testing.T) {
	lookup := &statusMessageFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{"alice@example.com": {id: 42, kind: "human", found: true}},
	}
	f := statusMessageFake("")
	mux := mkStatusMessageMux(t, f, lookup)

	tooLong := strings.Repeat("x", maxStatusMessageLen+1)
	form := url.Values{"status_message": []string{tooLong}}
	req := statusMessageRequest("POST", "/u/alice/status-message", "alice@example.com", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("oversize POST should not write fields; got %d calls", f.updateFieldsCalls)
	}
}

// TestHandleUserHome_RendersStatusMessage asserts the profile card on
// /u/{slug} includes the status_message text when the user bead carries one.
func TestHandleUserHome_RendersStatusMessage(t *testing.T) {
	mux := mkStatusMessageMux(t, statusMessageFake("debugging M2"), nil)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "debugging M2") {
		t.Errorf("profile card missing status_message: %s", body)
	}
	if !strings.Contains(body, `id="user-status-message"`) {
		t.Errorf("profile card missing status-message block: %s", body)
	}
}
