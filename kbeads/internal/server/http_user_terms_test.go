package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
)

// Lands step 7.4: POST /v1/users/me/terms writes terms_version and
// terms_accepted_at on the caller's own user bead and nothing else.

// termsRequest is an OIDC request from email holding roles (nil = an
// applicant with no gardenio role).
func termsRequest(email string, roles []string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/users/me/terms", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	return req.WithContext(WithRoles(ctx, roles))
}

func TestAcceptTerms_WritesOnlyTermsFieldsOnOwnBead(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com", "status_message": "hi", "allowed_projects": "p1"})
	seedLinkUserBead(ms, "u-bob", "auto-provision", map[string]any{"email": "bob@example.com"})
	bobBefore := string(ms.beads["u-bob"].Fields)

	before := time.Now().UTC().Add(-time.Second)
	rec := httptest.NewRecorder()
	s.handleAcceptTerms(rec, termsRequest("alice@example.com", nil, map[string]any{"terms_version": "2026-10-19"}))
	requireStatus(t, rec, http.StatusOK)

	var resp UserTermsResponse
	decodeJSON(t, rec, &resp)
	if resp.UserBeadID != "u-alice" || resp.TermsVersion != "2026-10-19" {
		t.Fatalf("resp = %+v", resp)
	}
	f := userBeadFields(ms.beads["u-alice"])
	if f["terms_version"] != "2026-10-19" || f["terms_accepted_at"] != resp.TermsAcceptedAt {
		t.Fatalf("terms not recorded: %v", f)
	}
	at, err := time.Parse(time.RFC3339, resp.TermsAcceptedAt)
	if err != nil || at.Before(before) {
		t.Fatalf("terms_accepted_at %q is not a fresh server timestamp", resp.TermsAcceptedAt)
	}
	// Exactly two keys were added; everything else is as it was.
	want := map[string]any{"email": "alice@example.com", "status_message": "hi", "allowed_projects": "p1",
		"terms_version": "2026-10-19", "terms_accepted_at": resp.TermsAcceptedAt}
	if len(f) != len(want) {
		t.Fatalf("fields = %v, want exactly %v", f, want)
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %v, want %v", k, f[k], v)
		}
	}
	if after := string(ms.beads["u-bob"].Fields); after != bobBefore {
		t.Fatalf("another person's bead changed:\n before %s\n after  %s", bobBefore, after)
	}

	// Accepting a newer version later replaces both fields.
	rec = httptest.NewRecorder()
	s.handleAcceptTerms(rec, termsRequest("alice@example.com", nil, map[string]any{"terms_version": "2027-01-01"}))
	requireStatus(t, rec, http.StatusOK)
	if got := userBeadFields(ms.beads["u-alice"])["terms_version"]; got != "2027-01-01" {
		t.Fatalf("terms_version = %v after re-accept", got)
	}
}

// The request names no bead and no field but the version: a caller cannot aim
// the write at someone else's bead, set another field, or pick the timestamp.
func TestAcceptTerms_SelfOnly(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com"})
	seedLinkUserBead(ms, "u-bob", "auto-provision", map[string]any{"email": "bob@example.com"})
	bobBefore := string(ms.beads["u-bob"].Fields)

	rec := httptest.NewRecorder()
	s.handleAcceptTerms(rec, termsRequest("alice@example.com", nil, map[string]any{
		"terms_version":     "v1",
		"user_bead_id":      "u-bob",
		"id":                "u-bob",
		"email":             "bob@example.com",
		"terms_accepted_at": "2001-01-01T00:00:00Z",
		"allowed_roles":     []string{RoleAdmin},
	}))
	requireStatus(t, rec, http.StatusOK)
	if after := string(ms.beads["u-bob"].Fields); after != bobBefore {
		t.Fatalf("bob's bead changed: %s", after)
	}
	f := userBeadFields(ms.beads["u-alice"])
	if f["email"] != "alice@example.com" || f["allowed_roles"] != nil {
		t.Fatalf("a body key leaked into the bead: %v", f)
	}
	if f["terms_accepted_at"] == "2001-01-01T00:00:00Z" {
		t.Fatalf("client-chosen terms_accepted_at was honoured")
	}
}

func TestAcceptTerms_Refusals(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLinkUserBead(ms, "u-alice", "auto-provision", map[string]any{"email": "alice@example.com"})
	before := string(ms.beads["u-alice"].Fields)

	// Static token: no "me".
	rec := httptest.NewRecorder()
	req := termsRequest("alice@example.com", nil, map[string]any{"terms_version": "v1"})
	req = req.WithContext(WithAuthIdentity(withAuthMethod(req.Context(), authMethodStatic), AuthIdentity{Type: "static", Subject: "static"}))
	s.handleAcceptTerms(rec, req)
	requireStatus(t, rec, http.StatusForbidden)

	// Machine principal: no "me" either.
	rec = httptest.NewRecorder()
	req = termsRequest("alice@example.com", nil, map[string]any{"terms_version": "v1"})
	req = req.WithContext(WithAuthIdentity(withAuthMethod(req.Context(), authMethodMachine), AuthIdentity{Type: "machine", Subject: "machine:beads-viewer"}))
	s.handleAcceptTerms(rec, req)
	requireStatus(t, rec, http.StatusForbidden)

	for _, bad := range []any{
		map[string]any{},
		map[string]any{"terms_version": ""},
		map[string]any{"terms_version": "has space"},
		map[string]any{"terms_version": "-leading-dash"},
		map[string]any{"terms_version": string(bytes.Repeat([]byte("v"), 65))},
	} {
		rec = httptest.NewRecorder()
		s.handleAcceptTerms(rec, termsRequest("alice@example.com", nil, bad))
		requireStatus(t, rec, http.StatusBadRequest)
	}
	if after := string(ms.beads["u-alice"].Fields); after != before {
		t.Fatalf("a refused request wrote: %s", after)
	}

	// Signed in, but no user bead.
	rec = httptest.NewRecorder()
	s.handleAcceptTerms(rec, termsRequest("nobody@example.com", nil, map[string]any{"terms_version": "v1"}))
	requireStatus(t, rec, http.StatusNotFound)
}

// rolelessVerifier treats the bearer as the subject and grants no role: a
// stranger who signed in through the realm and holds no gardenio role (B-D1).
type rolelessVerifier struct{}

func (rolelessVerifier) Verify(context.Context, string) error { return nil }
func (rolelessVerifier) VerifyAndIdentify(_ context.Context, raw string) (string, error) {
	return raw, nil
}
func (rolelessVerifier) ExtractRoles(context.Context, string) ([]string, error) { return nil, nil }

// End to end through the real HTTP chain with the user provisioner on: an
// applicant with no role reaches POST /v1/users/me/terms and gets 200 on their
// auto-provisioned bead, while every bead route still refuses them, and the
// generic PATCH cannot write the terms fields even on their own bead.
func TestAcceptTerms_ApplicantWithNoRole_EndToEnd(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(rolelessVerifier{})

	do := func(method, path string, body any) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer stranger@example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/v1/users/me/terms", map[string]any{"terms_version": "2026-10-19"})
	requireStatus(t, rec, http.StatusOK)
	var resp UserTermsResponse
	decodeJSON(t, rec, &resp)
	bead := ms.beads[resp.UserBeadID]
	if bead == nil || bead.Type != "user" {
		t.Fatalf("terms landed on %q, not the caller's user bead", resp.UserBeadID)
	}
	if f := userBeadFields(bead); f["email"] != "stranger@example.com" || f["terms_version"] != "2026-10-19" {
		t.Fatalf("user bead fields = %v", f)
	}

	requireStatus(t, do(http.MethodGet, "/v1/beads", nil), http.StatusForbidden)
	requireStatus(t, do(http.MethodGet, "/v1/beads/"+resp.UserBeadID, nil), http.StatusForbidden)
	requireStatus(t, do(http.MethodPatch, "/v1/beads/"+resp.UserBeadID,
		map[string]any{"fields": map[string]any{"terms_version": "forged"}}), http.StatusForbidden)
	requireStatus(t, do(http.MethodPost, "/v1/beads", plotApplicationBody(validPlotApplicationFields())), http.StatusForbidden)
	for _, b := range ms.beads {
		if b.Type == typePlotApplication {
			t.Fatalf("an applicant's own bearer filed an application: %s", b.ID)
		}
	}
	if got := userBeadFields(ms.beads[resp.UserBeadID])["terms_version"]; got != "2026-10-19" {
		t.Fatalf("terms_version = %v", got)
	}
}

// An OIDC caller who is not an admin cannot set the terms fields on any user
// bead through the generic paths; an admin can (an operator correcting a
// record).
func TestUserTermsFields_GenericWriteGate(t *testing.T) {
	s, ms, _ := newTestServer()
	seedIdentityUserBead(t, ms, "gd-user1", `{"email":"owner@example.com"}`)
	for _, f := range []string{"terms_version", "terms_accepted_at"} {
		val := any("v2")
		if f == "terms_accepted_at" {
			val = "2026-10-19T00:00:00Z"
		}
		patch, _ := json.Marshal(map[string]any{f: val})
		if d := s.authorizeUserIdentityFields(machineUserCtx(), "update", "user", patch, ms.beads["gd-user1"].Fields); d == nil {
			t.Errorf("static token may write %s", f)
		}
		if d := s.authorizeUserIdentityFields(WithRoles(oidcCtx("owner@example.com", nil), []string{RoleOperator}), "update", "user", patch, ms.beads["gd-user1"].Fields); d == nil {
			t.Errorf("non-admin OIDC caller may write %s", f)
		}
		if d := s.authorizeUserIdentityFields(adminOIDCCtx(), "update", "user", patch, ms.beads["gd-user1"].Fields); d != nil {
			t.Errorf("admin refused on %s: %v", f, d)
		}
	}
	if !isGatedUserField("terms_version") || userIdentityFields["terms_version"] {
		t.Fatalf("terms_version must be gated but not an identity field (user-identity-fields.md)")
	}
}
