package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

func newTestServerForTeams(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	return NewServer(daemon, slog.Default(), "", "http://test", "", nil)
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling %v: %v", v, err)
	}
	return json.RawMessage(b)
}

func TestHandleTeamPage_HappyPath(t *testing.T) {
	daemon := newFake()
	expires := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID:     "gd-team1",
			Slug:       "data-platform",
			Display:    "Data Platform",
			Members:    []json.RawMessage{rawJSON(t, "oidc:alice@example.com"), rawJSON(t, 42)},
			Owners:     []json.RawMessage{rawJSON(t, "bob@example.com")},
			OIDCGroups: []string{"idp-data-team"},
		},
	}
	daemon.taintRegistryResp = []beadsapi.TaintRegistryEntry{
		{Name: "acme-production", OwnerTeam: "gd-team1", Description: "prod PII", BeadCount: intPtr(7), OpenScrubRequests: 1},
		{Name: "other-taint", OwnerTeam: "gd-other", BeadCount: intPtr(3)},
	}
	daemon.teamTaintSummaryResp = map[string]*beadsapi.TeamTaintSummary{
		"gd-team1": {
			HeldGrants:    []beadsapi.TeamTaintGrant{{TaintName: "growth-scratch", OwnerTeam: "growth", ExpiresAt: &expires}},
			OpenApprovals: []beadsapi.TeamOpenApproval{{ID: "gd-req1", Title: "Scrub request", TaintName: "acme-production", Kind: "scrub", RequestedBy: "carol@example.com"}},
		},
	}

	srv := newTestServerForTeams(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/teams/data-platform", nil)
	req.SetPathValue("team", "data-platform")
	rec := httptest.NewRecorder()
	srv.handleTeamPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"Data Platform",
		"alice@example.com",
		"operator #42",
		"bob@example.com",
		"idp-data-team",
		"acme-production",
		"prod PII",
		"growth-scratch",
		"owned by growth",
		"Scrub request",
		"requested by carol@example.com",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Contains(body, "other-taint") {
		t.Errorf("expected owned taints to be filtered to this team's bead ID only")
	}
	if strings.Contains(body, "Failed to load") {
		t.Errorf("unexpected error banner in body")
	}
}

func TestHandleTeamPage_NotFound(t *testing.T) {
	daemon := newFake()
	srv := newTestServerForTeams(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/teams/nope", nil)
	req.SetPathValue("team", "nope")
	rec := httptest.NewRecorder()
	srv.handleTeamPage(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleTeamPage_DaemonErrors(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {BeadID: "gd-team1", Slug: "data-platform"},
	}
	daemon.taintRegistryErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "down"}
	daemon.teamTaintSummaryErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "down"}

	srv := newTestServerForTeams(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/teams/data-platform", nil)
	req.SetPathValue("team", "data-platform")
	rec := httptest.NewRecorder()
	srv.handleTeamPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrades to an error banner)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failed to load") {
		t.Errorf("expected error banner in body:\n%s", rec.Body.String())
	}
}

// TestHandleTeamPage_CanEditMembershipGating verifies the acceptance
// criteria: membership edit controls are visible only for the team's own
// owners and platform admins (ADR docs/adr/taints.md §9.1, p1-team write
// policy).
func TestHandleTeamPage_CanEditMembershipGating(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID:  "gd-team1",
			Slug:    "data-platform",
			Members: []json.RawMessage{rawJSON(t, "dave@example.com")},
			Owners:  []json.RawMessage{rawJSON(t, "oidc:bob@example.com")},
		},
	}
	srv := newTestServerForTeams(t, daemon)

	cases := []struct {
		name      string
		roles     []string
		email     string
		wantForms bool
	}{
		{"no roles (dev bypass) = not admin", nil, "", false},
		{"viewer, not an owner", []string{oidcauth.RoleViewer}, "eve@example.com", false},
		{"viewer, is an owner (bare email stored with oidc: prefix)", []string{oidcauth.RoleViewer}, "bob@example.com", true},
		{"viewer, is an owner, case-insensitive", []string{oidcauth.RoleViewer}, "BOB@EXAMPLE.COM", true},
		{"admin role", []string{oidcauth.RoleAdmin}, "", true},
		{"viewer, is a member but not an owner", []string{oidcauth.RoleViewer}, "dave@example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/teams/data-platform", nil)
			req.SetPathValue("team", "data-platform")
			ctx := req.Context()
			if tc.roles != nil {
				ctx = oidcauth.ContextWithRoles(ctx, tc.roles)
			}
			if tc.email != "" {
				ctx = oidcauth.ContextWithEmail(ctx, tc.email)
			}
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			srv.handleTeamPage(rec, req)

			got := strings.Contains(rec.Body.String(), "Add member")
			if got != tc.wantForms {
				t.Errorf("membership-edit forms present = %v, want %v\n--- body ---\n%s", got, tc.wantForms, rec.Body.String())
			}
		})
	}
}

func TestHandleTeamMembershipEdit_AddMember(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID:  "gd-team1",
			Slug:    "data-platform",
			Members: []json.RawMessage{rawJSON(t, "alice@example.com")},
			Owners:  []json.RawMessage{rawJSON(t, "bob@example.com")},
		},
	}
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=members&action=add&value=carol@example.com")
	req := httptest.NewRequest(http.MethodPost, "/teams/data-platform/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "data-platform")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleAdmin}))
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.updateFieldsCalls != 1 {
		t.Fatalf("expected 1 UpdateBeadFieldsTyped call, got %d", daemon.updateFieldsCalls)
	}
	if daemon.updateFieldsLastID != "gd-team1" {
		t.Errorf("updated bead ID = %q, want gd-team1", daemon.updateFieldsLastID)
	}
	members, _ := daemon.updateFieldsLastVal["members"].([]any)
	want := []any{"alice@example.com", "carol@example.com"}
	if len(members) != len(want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
	for i := range want {
		if members[i] != want[i] {
			t.Errorf("members[%d] = %v, want %v", i, members[i], want[i])
		}
	}
}

func TestHandleTeamMembershipEdit_AddDuplicateIsNoop(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID:  "gd-team1",
			Slug:    "data-platform",
			Members: []json.RawMessage{rawJSON(t, "alice@example.com")},
			Owners:  []json.RawMessage{rawJSON(t, "bob@example.com")},
		},
	}
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=members&action=add&value=alice@example.com")
	req := httptest.NewRequest(http.MethodPost, "/teams/data-platform/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "data-platform")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleAdmin}))
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.updateFieldsCalls != 0 {
		t.Errorf("expected no write for a duplicate add, got %d calls", daemon.updateFieldsCalls)
	}
}

func TestHandleTeamMembershipEdit_RemoveOwner(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID: "gd-team1",
			Slug:   "data-platform",
			Owners: []json.RawMessage{rawJSON(t, "bob@example.com"), rawJSON(t, "oidc:carol@example.com")},
		},
	}
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=owners&action=remove&value=bob@example.com")
	req := httptest.NewRequest(http.MethodPost, "/teams/data-platform/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "data-platform")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleAdmin}))
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
	owners, _ := daemon.updateFieldsLastVal["owners"].([]any)
	if len(owners) != 1 || owners[0] != "oidc:carol@example.com" {
		t.Errorf("owners = %v, want [oidc:carol@example.com]", owners)
	}
}

// TestHandleTeamMembershipEdit_UnauthorizedRejected verifies a caller who is
// neither an admin nor one of the team's own owners is rejected with 403 and
// never reaches the daemon write.
func TestHandleTeamMembershipEdit_UnauthorizedRejected(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID:  "gd-team1",
			Slug:    "data-platform",
			Members: []json.RawMessage{rawJSON(t, "alice@example.com")},
			Owners:  []json.RawMessage{rawJSON(t, "bob@example.com")},
		},
	}
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=members&action=add&value=eve@example.com")
	req := httptest.NewRequest(http.MethodPost, "/teams/data-platform/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "data-platform")
	ctx := oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer})
	ctx = oidcauth.ContextWithEmail(ctx, "eve@example.com")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.updateFieldsCalls != 0 {
		t.Errorf("expected no daemon write for an unauthorized edit, got %d calls", daemon.updateFieldsCalls)
	}
}

// TestHandleTeamMembershipEdit_OwnerAuthorized verifies the team's own owner
// (identified by email, independent of any OIDC role) can edit membership.
func TestHandleTeamMembershipEdit_OwnerAuthorized(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID: "gd-team1",
			Slug:   "data-platform",
			Owners: []json.RawMessage{rawJSON(t, "oidc:bob@example.com")},
		},
	}
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=members&action=add&value=frank@example.com")
	req := httptest.NewRequest(http.MethodPost, "/teams/data-platform/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "data-platform")
	ctx := oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer})
	ctx = oidcauth.ContextWithEmail(ctx, "bob@example.com")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.updateFieldsCalls != 1 {
		t.Errorf("expected the owner's write to reach the daemon, got %d calls", daemon.updateFieldsCalls)
	}
}

func TestHandleTeamMembershipEdit_TeamNotFound(t *testing.T) {
	daemon := newFake()
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=members&action=add&value=eve@example.com")
	req := httptest.NewRequest(http.MethodPost, "/teams/nope/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "nope")
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleTeamMembershipEdit_UnknownField(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {BeadID: "gd-team1", Slug: "data-platform"},
	}
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=display&action=add&value=New+Name")
	req := httptest.NewRequest(http.MethodPost, "/teams/data-platform/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "data-platform")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleAdmin}))
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.updateFieldsCalls != 0 {
		t.Errorf("expected no daemon write for an unwhitelisted field, got %d calls", daemon.updateFieldsCalls)
	}
}

// TestHandleTeamMembershipEdit_DaemonForbidden verifies a 403 from the
// daemon's own authorizeTeamMembershipBypass gate (e.g. the caller lost
// ownership between page load and submit) surfaces a clear message rather
// than a generic failure.
func TestHandleTeamMembershipEdit_DaemonForbidden(t *testing.T) {
	daemon := newFake()
	daemon.teamsBySlug = map[string]beadsapi.TeamInfo{
		"data-platform": {
			BeadID:  "gd-team1",
			Slug:    "data-platform",
			Members: []json.RawMessage{rawJSON(t, "alice@example.com")},
			Owners:  []json.RawMessage{rawJSON(t, "bob@example.com")},
		},
	}
	daemon.updateFieldsErr = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "no longer an owner"}
	srv := newTestServerForTeams(t, daemon)

	form := strings.NewReader("field=members&action=add&value=eve@example.com")
	req := httptest.NewRequest(http.MethodPost, "/teams/data-platform/members", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("team", "data-platform")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleAdmin}))
	rec := httptest.NewRecorder()
	srv.handleTeamMembershipEdit(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no longer authorized") {
		t.Errorf("expected the forbidden-specific message, got:\n%s", rec.Body.String())
	}
}
