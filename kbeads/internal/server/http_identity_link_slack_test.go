package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func slackLinkRequest(method, email, clientID string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	req := oidcLinkRequest(method, "/v1/identity-links/slack", email, raw)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email, ClientID: clientID})
	return req.WithContext(WithRoles(ctx, []string{RoleOperator}))
}

func slackLinkFields(t *testing.T, ms *mockStore, id string) map[string]any {
	t.Helper()
	b, _ := ms.GetBead(context.Background(), id)
	if b == nil {
		t.Fatalf("bead %s missing", id)
	}
	return userBeadFields(b)
}

func TestLinkSlackIdentity_BindsOwnUserBead(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-owner", "oidc:owner@example.com", map[string]any{"email": "owner@example.com"})

	rec := httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "owner@example.com", "gardenio-slack-link",
		map[string]any{"slack_user_id": "U123", "slack_team_id": "T1", "linked_via": "gardenio-slack-link"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp SlackIdentityLinkResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.UserBeadID != "u-owner" || resp.SlackUserID != "U123" || resp.AlreadyLinked {
		t.Fatalf("resp = %+v", resp)
	}
	f := slackLinkFields(t, ms, "u-owner")
	if f["slack_id"] != "U123" {
		t.Fatalf("slack_id not set: %v", f)
	}
	link := f["identity_link"].(map[string]any)["slack"].(map[string]any)
	if link["slack_id"] != "U123" || link["subject"] != "owner@example.com" || link["client"] != "gardenio-slack-link" || link["team_id"] != "T1" {
		t.Fatalf("identity_link.slack = %v", link)
	}
	if id, err := ms.LookupOperatorAlias(context.Background(), "slack", "U123"); err != nil || id == 0 {
		t.Fatalf("slack operator alias not attached: %d %v", id, err)
	}

	// Re-link is idempotent.
	rec = httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "owner@example.com", "gardenio-slack-link",
		map[string]any{"slack_user_id": "U123"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("relink status %d: %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.AlreadyLinked {
		t.Fatalf("relink should report already_linked: %+v", resp)
	}
}

func TestLinkSlackIdentity_RefusesNonOIDCAndMissingBead(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-owner", "oidc:owner@example.com", map[string]any{"email": "owner@example.com"})

	// Static/fleet token (no OIDC auth method): refused even with a body.
	raw, _ := json.Marshal(map[string]any{"slack_user_id": "U123"})
	rec := httptest.NewRecorder()
	req := oidcLinkRequest(http.MethodPost, "/v1/identity-links/slack", "owner@example.com", raw)
	req = req.WithContext(WithAuthIdentity(withAuthMethod(req.Context(), authMethodNone), AuthIdentity{Type: "static", Subject: "gardenio"}))
	s.handleLinkSlackIdentity(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("static token must be refused, got %d", rec.Code)
	}
	if f := slackLinkFields(t, ms, "u-owner"); f["slack_id"] != nil {
		t.Fatalf("static token must not write: %v", f)
	}

	// OIDC person with no user bead: 404, nothing written.
	rec = httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "nobody@example.com", "gardenio-slack-link", map[string]any{"slack_user_id": "U999"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no user bead must be 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Missing slack_user_id: 400.
	rec = httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "owner@example.com", "gardenio-slack-link", map[string]any{}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty slack_user_id must be 400, got %d", rec.Code)
	}
}

func TestLinkSlackIdentity_ConflictsNeverMerge(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-owner", "oidc:owner@example.com", map[string]any{"email": "owner@example.com", "slack_id": "UOWNER"})
	seedLinkUserBead(ms, "u-other", "oidc:other@example.com", map[string]any{"email": "other@example.com", "slack_id": "UOTHER"})

	// The caller's bead is bound to a different Slack id: 409.
	rec := httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "owner@example.com", "gardenio-slack-link", map[string]any{"slack_user_id": "UNEW"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("different slack_id on own bead must be 409, got %d: %s", rec.Code, rec.Body.String())
	}
	// Someone else's Slack id: 409 (the other bead is untouched).
	seedLinkUserBead(ms, "u-third", "oidc:third@example.com", map[string]any{"email": "third@example.com"})
	rec = httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "third@example.com", "gardenio-slack-link", map[string]any{"slack_user_id": "UOTHER"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("another bead's slack_id must be 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if f := slackLinkFields(t, ms, "u-third"); f["slack_id"] != nil || f["identity_link"] != nil {
		t.Fatalf("conflict must write nothing: %v", f)
	}
	if f := slackLinkFields(t, ms, "u-other"); f["slack_id"] != "UOTHER" {
		t.Fatalf("other bead must be untouched: %v", f)
	}
}

func TestUnlinkSlackIdentity_ClearsOwnBind(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-owner", "oidc:owner@example.com", map[string]any{"email": "owner@example.com"})
	rec := httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "owner@example.com", "gardenio-slack-link", map[string]any{"slack_user_id": "U123"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}

	// Another person cannot unlink it (their own bead has nothing; 404 or no-op).
	rec = httptest.NewRecorder()
	s.handleUnlinkSlackIdentity(rec, slackLinkRequest(http.MethodDelete, "intruder@example.com", "gardenio-slack-link", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("intruder unlink: %d", rec.Code)
	}
	if f := slackLinkFields(t, ms, "u-owner"); f["slack_id"] != "U123" {
		t.Fatalf("intruder must not clear the bind: %v", f)
	}

	rec = httptest.NewRecorder()
	s.handleUnlinkSlackIdentity(rec, slackLinkRequest(http.MethodDelete, "owner@example.com", "gardenio-slack-link", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unlink: %d %s", rec.Code, rec.Body.String())
	}
	f := slackLinkFields(t, ms, "u-owner")
	if f["slack_id"] != nil || f["identity_link"] != nil {
		t.Fatalf("unlink must clear slack_id and identity_link: %v", f)
	}
	// Idempotent.
	rec = httptest.NewRecorder()
	s.handleUnlinkSlackIdentity(rec, slackLinkRequest(http.MethodDelete, "owner@example.com", "gardenio-slack-link", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second unlink: %d", rec.Code)
	}
}

func TestUnlinkSlackIdentity_KeepsAdminWrittenSlackIDWhenDifferent(t *testing.T) {
	s, ms, _ := newTestServer()
	// Bead bound by an admin to UADMIN; identity_link recorded for the same id
	// later. Unlink clears both only because they agree.
	seedLinkUserBead(ms, "u-owner", "oidc:owner@example.com", map[string]any{"email": "owner@example.com", "slack_id": "UADMIN"})
	rec := httptest.NewRecorder()
	s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "owner@example.com", "gardenio-slack-link", map[string]any{"slack_user_id": "UADMIN"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("link same id as admin wrote: %d %s", rec.Code, rec.Body.String())
	}
	b, _ := ms.GetBead(context.Background(), "u-owner")
	if b.Status != model.StatusOpen {
		t.Fatal("status changed")
	}
	rec = httptest.NewRecorder()
	s.handleUnlinkSlackIdentity(rec, slackLinkRequest(http.MethodDelete, "owner@example.com", "gardenio-slack-link", nil))
	if f := slackLinkFields(t, ms, "u-owner"); f["slack_id"] != nil {
		t.Fatalf("slack_id equal to the linked id is cleared on unlink: %v", f)
	}
}

// gd-L55f9EYFjN: an address listed in email_aliases is a label. Signing in as
// that address finds no user bead, so the Slack bind (and unlink) is refused
// and the bead that lists the address is never written.
func TestLinkSlackIdentity_EmailAliasDoesNotSelectTheUserBead(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-owner", "oidc:owner@example.com", map[string]any{
		"email": "owner@example.com", "email_aliases": []string{"crew-github@example.com"},
	})
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := slackLinkRequest(method, "crew-github@example.com", "gardenio-slack-link", map[string]any{"slack_user_id": "U123"})
		if method == http.MethodPost {
			s.handleLinkSlackIdentity(rec, req)
		} else {
			s.handleUnlinkSlackIdentity(rec, req)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s as an email_aliases address must be 404, got %d: %s", method, rec.Code, rec.Body.String())
		}
	}
	if f := slackLinkFields(t, ms, "u-owner"); f["slack_id"] != nil || f["identity_link"] != nil {
		t.Fatalf("the bead listing the alias must not be written: %v", f)
	}
}

// The self-serve operator link needs the caller's email to be the bead's
// `email`; an email_aliases entry no longer backs it.
func TestUserBeadBacksSelfLink_EmailAliasDoesNotBackIt(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedLinkUserBead(ms, "gd-owner", "admin@example.com", map[string]any{
		"email": "owner@example.com", "email_aliases": []string{"crew-github@example.com"}, "slack_id": "U1",
	})
	ok, err := srv.userBeadBacksSelfLink(context.Background(), "owner@example.com", "slack", "U1")
	if err != nil || !ok {
		t.Fatalf("the bead's own email must back its slack alias: ok=%v err=%v", ok, err)
	}
	ok, err = srv.userBeadBacksSelfLink(context.Background(), "crew-github@example.com", "slack", "U1")
	if err != nil || ok {
		t.Fatalf("an email_aliases address must not back a self-link: ok=%v err=%v", ok, err)
	}
}
