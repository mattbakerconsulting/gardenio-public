package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// hubReq builds a GET request whose context carries the given realm roles, so
// the IsAdmin gate in buildProjectHub can be exercised. Nil roles simulates the
// dev-bypass / no-OIDC path (RBAC not enforced).
func hubReq(roles []string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/p/gardenio", nil)
	if roles != nil {
		r = r.WithContext(oidcauth.ContextWithRoles(r.Context(), roles))
	}
	return r
}

func hubServer(f *fakeDaemon) *Server {
	return &Server{daemon: f, logger: slog.Default(), basePath: ""}
}

// TestBuildProjectHub_EagerAssembly verifies the happy path: config from
// ListProjectBeads, health from buildProjectSummary, the chip strip with
// counts, and the hoisted config-bead id/status — all from the reused queries.
func TestBuildProjectHub_EagerAssembly(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Hour)

	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {
			Name:    "gardenio",
			BeadID:  "gd-proj1",
			Status:  "open",
			GitURL:  "https://example.com/repo.git",
			Account: "acct-1",
		},
	}
	// The config bead behind the project. buildProjectValidity fetches it by ID
	// and runs the canonical validator over its raw fields; well-formed fields
	// → no findings (green).
	f.beads["gd-proj1"] = &beadsapi.BeadDetail{
		ID:     "gd-proj1",
		Type:   "project",
		Status: "open",
		Fields: map[string]string{
			"git_url": "https://example.com/repo.git",
			"secrets": `[{"env":"FOO","secret":"s","key":"k"}]`,
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-1", Type: "task", Status: "open", Priority: 2, Assignee: "alice", CreatedAt: fresh, UpdatedAt: fresh},
			{ID: "gd-2", Type: "bug", Status: "in_progress", Priority: 1, Assignee: "bob", CreatedAt: fresh, UpdatedAt: fresh},
		},
		Total: 2,
	}

	s := hubServer(f)
	view := s.buildProjectHub(context.Background(), hubReq(nil), "gardenio", chipQuery{Active: "issues"}, now)

	if view == nil {
		t.Fatal("view is nil")
	}
	if view.Name != "gardenio" {
		t.Errorf("Name = %q, want gardenio", view.Name)
	}
	if view.Config.GitURL != "https://example.com/repo.git" {
		t.Errorf("Config.GitURL = %q, want repo url", view.Config.GitURL)
	}
	if view.ConfigBeadID != "gd-proj1" {
		t.Errorf("ConfigBeadID = %q, want gd-proj1", view.ConfigBeadID)
	}
	if view.ConfigBeadStatus != "open" {
		t.Errorf("ConfigBeadStatus = %q, want open", view.ConfigBeadStatus)
	}
	if view.Summary == nil {
		t.Fatal("Summary is nil, want populated health panel")
	}
	if view.Summary.TotalOpen != 2 {
		t.Errorf("Summary.TotalOpen = %d, want 2", view.Summary.TotalOpen)
	}
	if len(view.Chips) != len(projectTypeChips) {
		t.Fatalf("len(Chips) = %d, want %d", len(view.Chips), len(projectTypeChips))
	}
	// Counts are wired from the summary when it loaded.
	for _, c := range view.Chips {
		if !c.ShowCount {
			t.Errorf("chip %q ShowCount = false, want true when summary loaded", c.Value)
		}
		if c.Value == "task" && c.Count != 1 {
			t.Errorf("task chip Count = %d, want 1", c.Count)
		}
		if c.Value == "issues" && !c.Active {
			t.Errorf("issues chip Active = false, want true (it is the resolved filter)")
		}
	}
	// Validity runs the canonical validator over the config bead's raw fields;
	// well-formed config → no findings (green badge).
	if !view.Validity.OK() {
		t.Errorf("Validity.OK() = false, want true for a well-formed config bead; checks=%+v", view.Validity.Checks)
	}
	// Sequencer epic is frozen.
	if view.SequencerEnabled {
		t.Errorf("SequencerEnabled = true, want false (ka-0000003558 frozen)")
	}
}

// TestBuildProjectHub_NoConfigBead verifies a label-only project (no
// type=project bead) still assembles: health renders, config is zero-valued,
// and validity reports no config rather than failing.
func TestBuildProjectHub_NoConfigBead(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{} // no entry for "orphan"
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-9", Type: "task", Status: "open", Priority: 2, CreatedAt: now, UpdatedAt: now},
		},
		Total: 1,
	}

	s := hubServer(f)
	view := s.buildProjectHub(context.Background(), hubReq(nil), "orphan", chipQuery{Active: "issues"}, now)

	if view.ConfigBeadID != "" {
		t.Errorf("ConfigBeadID = %q, want empty for label-only project", view.ConfigBeadID)
	}
	if view.Config.Name != "" {
		t.Errorf("Config should be zero-valued for label-only project, got Name=%q", view.Config.Name)
	}
	// No config bead → zero report (no findings).
	if len(view.Validity.Checks) != 0 {
		t.Errorf("Validity.Checks = %+v, want empty with no config bead", view.Validity.Checks)
	}
	if view.Summary == nil {
		t.Error("Summary is nil, health should still render for a label-only project")
	}
}

// TestBuildProjectHub_ConfigError verifies best-effort degradation: a daemon
// error on ListProjectBeads hides the config panel but health still renders
// and the assembler never returns nil.
func TestBuildProjectHub_ConfigError(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.projectsErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-1", Type: "task", Status: "open", Priority: 2, CreatedAt: now, UpdatedAt: now},
		},
		Total: 1,
	}

	s := hubServer(f)
	view := s.buildProjectHub(context.Background(), hubReq(nil), "gardenio", chipQuery{Active: "issues"}, now)

	if view == nil {
		t.Fatal("view is nil; config error must not fail the whole assembly")
	}
	if view.ConfigBeadID != "" {
		t.Errorf("ConfigBeadID = %q, want empty when config fetch errored", view.ConfigBeadID)
	}
	// Config fetch errored → no ConfigBeadID → zero report (no findings).
	if len(view.Validity.Checks) != 0 {
		t.Errorf("Validity.Checks = %+v, want empty when config fetch errored", view.Validity.Checks)
	}
	if view.Summary == nil {
		t.Error("Summary is nil; health should still render when config fetch errored")
	}
}

// TestBuildProjectHub_SummaryError verifies best-effort degradation in the
// other direction: a daemon error on the summary query hides the health panel
// and falls the chips back to label-only, but config still renders.
func TestBuildProjectHub_SummaryError(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-proj1", Status: "open"},
	}
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}

	s := hubServer(f)
	view := s.buildProjectHub(context.Background(), hubReq(nil), "gardenio", chipQuery{Active: "issues"}, now)

	if view.Summary != nil {
		t.Errorf("Summary = %+v, want nil when summary query errored", view.Summary)
	}
	if view.ConfigBeadID != "gd-proj1" {
		t.Errorf("ConfigBeadID = %q, want gd-proj1 (config still renders)", view.ConfigBeadID)
	}
	if len(view.Chips) != len(projectTypeChips) {
		t.Fatalf("len(Chips) = %d, want %d even on summary error", len(view.Chips), len(projectTypeChips))
	}
	for _, c := range view.Chips {
		if c.ShowCount {
			t.Errorf("chip %q ShowCount = true, want false (label-only fallback on summary error)", c.Value)
		}
		if c.Count != 0 {
			t.Errorf("chip %q Count = %d, want 0 on summary error", c.Value, c.Count)
		}
	}
}

// TestBuildProjectHub_IsAdminGate verifies the Config-editor gate mirrors
// viewerCanEditProjectConfig: admins and roleless sessions (static token, which
// the daemon allows update/project for) see the editor; an OIDC operator
// (lvl 2 < lvl 3) does not.
func TestBuildProjectHub_IsAdminGate(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio", BeadID: "gd-proj1"}}
	f.listResp = &beadsapi.ListBeadsResult{}
	s := hubServer(f)

	cases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"no roles (static token: daemon accepts update/project)", nil, true},
		{"admin", []string{oidcauth.RoleAdmin}, true},
		{"operator only", []string{oidcauth.RoleOperator}, false},
		{"viewer only", []string{oidcauth.RoleViewer}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := s.buildProjectHub(context.Background(), hubReq(tc.roles), "gardenio", chipQuery{Active: "issues"}, now)
			if view.IsAdmin != tc.want {
				t.Errorf("IsAdmin = %v, want %v for roles %v", view.IsAdmin, tc.want, tc.roles)
			}
		})
	}
}

// TestBuildProjectChipHrefFromQuery verifies the chip hrefs preserve the
// status/label/q/sort filters (so a type click only changes the type
// dimension) and route to /p/<project>.
func TestBuildProjectChipHrefFromQuery(t *testing.T) {
	href := buildProjectChipHrefFromQuery("", "gardenio", "bug", chipQuery{
		Status: "open,in_progress",
		Label:  "area:ci",
		Search: "flaky",
		Sort:   "-updated_at",
		Active: "issues",
	})
	if !strings.HasPrefix(href, "/p/gardenio?") {
		t.Fatalf("href = %q, want /p/gardenio? prefix", href)
	}
	for _, want := range []string{"type=bug", "status=open%2Cin_progress", "label=area%3Aci", "q=flaky", "sort=-updated_at"} {
		if !strings.Contains(href, want) {
			t.Errorf("href = %q, missing %q", href, want)
		}
	}
}

// TestViewerCanAdmin pins the admin gate semantics directly.
func TestViewerCanAdmin(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"no roles (fails closed)", nil, false},
		{"admin", []string{oidcauth.RoleAdmin}, true},
		{"operator", []string{oidcauth.RoleOperator}, false},
		{"viewer", []string{oidcauth.RoleViewer}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := viewerCanAdmin(hubReq(tc.roles)); got != tc.want {
				t.Errorf("viewerCanAdmin(%v) = %v, want %v", tc.roles, got, tc.want)
			}
		})
	}
}

// TestViewerCanAdmin_MatchesDaemonRoleCheck pins that the viewer asks the same
// question the daemon does: the roles lifted from the caller's own token
// (oidcauth.RolesFromContext, i.e. the realm_access.roles claim) against the
// gardenio-admin minimum, with NO special case for an absent roles claim —
// kbeads refuses a roleless (static-token) caller admin writes, so the page
// must not offer them (gd-pjKksLDLWM).
func TestViewerCanAdmin_MatchesDaemonRoleCheck(t *testing.T) {
	for _, roles := range [][]string{
		nil,
		{},
		{oidcauth.RoleViewer},
		{oidcauth.RoleOperator},
		{oidcauth.RoleAdmin},
		{oidcauth.RoleViewer, oidcauth.RoleAdmin},
		{"unrelated-realm-role"},
	} {
		r := hubReq(roles)
		want := oidcauth.HasMinimumRole(oidcauth.RolesFromContext(r.Context()), oidcauth.RoleAdmin)
		if got := viewerCanAdmin(r); got != want {
			t.Errorf("viewerCanAdmin(roles=%v) = %v, daemon-equivalent check = %v", roles, got, want)
		}
	}
}

// TestAdminSessionBanner covers the read-only marker: shown for a roleless
// session (naming the dev bypass when it is on), absent for a real OIDC session.
func TestAdminSessionBanner(t *testing.T) {
	t.Setenv(devAuthBypassEnv, "")
	if got := adminSessionBanner(hubReq(nil)); !strings.Contains(got, "daemon will refuse those writes") {
		t.Errorf("roleless banner = %q, want the daemon-will-refuse marker", got)
	}
	if got := adminSessionBanner(hubReq(nil)); strings.Contains(got, "admin writes") || strings.Contains(strings.ToLower(got), "config") {
		t.Errorf("roleless banner = %q, must name only taint creation / team membership", got)
	}
	if got := adminSessionBanner(hubReq(nil)); strings.Contains(got, devAuthBypassEnv) {
		t.Errorf("banner names the bypass although it is off: %q", got)
	}
	t.Setenv(devAuthBypassEnv, "1")
	if got := adminSessionBanner(hubReq(nil)); !strings.Contains(got, devAuthBypassEnv) {
		t.Errorf("bypass banner = %q, want it to name %s", got, devAuthBypassEnv)
	}
	for _, roles := range [][]string{{oidcauth.RoleViewer}, {oidcauth.RoleAdmin}} {
		if got := adminSessionBanner(hubReq(roles)); got != "" {
			t.Errorf("banner for roles %v = %q, want none for a real OIDC session", roles, got)
		}
	}
	if got := adminSessionBanner(nil); got != "" {
		t.Errorf("nil request banner = %q, want empty", got)
	}
}

// TestViewerCanEditProjectConfig pins that the project Config editor stays
// visible for a roleless session (the daemon accepts update/project from the
// viewer's static token) while taint creation / team membership stay hidden,
// and that an OIDC session is judged on its roles.
func TestViewerCanEditProjectConfig(t *testing.T) {
	cases := []struct {
		name         string
		roles        []string
		wantConfig   bool
		wantTaintsTm bool
	}{
		{"no roles", nil, true, false},
		{"viewer", []string{oidcauth.RoleViewer}, false, false},
		{"operator", []string{oidcauth.RoleOperator}, false, false},
		{"admin", []string{oidcauth.RoleAdmin}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := hubReq(tc.roles)
			if got := viewerCanEditProjectConfig(r); got != tc.wantConfig {
				t.Errorf("viewerCanEditProjectConfig(%v) = %v, want %v", tc.roles, got, tc.wantConfig)
			}
			if got := viewerCanAdmin(r); got != tc.wantTaintsTm {
				t.Errorf("viewerCanAdmin(%v) = %v, want %v", tc.roles, got, tc.wantTaintsTm)
			}
		})
	}
}
