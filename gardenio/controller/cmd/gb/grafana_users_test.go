package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGrafanaOrg is an httptest server standing in for the Grafana org-users
// API. It serves GET /api/org/users and applies PATCH /api/org/users/<id> to an
// in-memory user table, recording each PATCH so tests can assert on the writes.
type fakeGrafanaOrg struct {
	mu      sync.Mutex
	users   map[int]*grafanaOrgUser
	patches []patchRecord
	// forbid, when set, makes every request 403 (auth failure simulation).
	forbid bool
}

type patchRecord struct {
	userID int
	role   string
}

func newFakeGrafanaOrg(users ...grafanaOrgUser) *fakeGrafanaOrg {
	f := &fakeGrafanaOrg{users: map[int]*grafanaOrgUser{}}
	for i := range users {
		u := users[i]
		f.users[u.UserID] = &u
	}
	return f
}

func (f *fakeGrafanaOrg) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)

	// Route the users client at this server and neutralise auth env so
	// applyGrafanaAuth adds nothing unexpected.
	oldClient := grafanaUsersHTTPClient
	grafanaUsersHTTPClient = func() *http.Client { return srv.Client() }
	t.Cleanup(func() { grafanaUsersHTTPClient = oldClient })
	t.Setenv("GRAFANA_TOKEN", "")
	t.Setenv("GRAFANA_AUTH_PROXY_HEADER", "")
	t.Setenv("GRAFANA_USER", "")

	return srv.URL
}

func (f *fakeGrafanaOrg) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.forbid {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Forbidden"}`)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/org/users":
		out := make([]grafanaOrgUser, 0, len(f.users))
		for _, u := range f.users {
			out = append(out, *u)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPatch && len(r.URL.Path) > len("/api/org/users/"):
		var id int
		if _, err := fmt.Sscanf(r.URL.Path, "/api/org/users/%d", &id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			Role string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		u, ok := f.users[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		u.Role = body.Role
		f.patches = append(f.patches, patchRecord{userID: id, role: body.Role})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":"Organization user updated"}`)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCanonicalOrgRole(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"viewer":  {"Viewer", true},
		"Viewer":  {"Viewer", true},
		" ADMIN ": {"Admin", true},
		"Editor":  {"Editor", true},
		"root":    {"", false},
		"":        {"", false},
	}
	for in, exp := range cases {
		got, ok := canonicalOrgRole(in)
		if got != exp.want || ok != exp.ok {
			t.Errorf("canonicalOrgRole(%q) = (%q,%v), want (%q,%v)", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestListGrafanaOrgUsers(t *testing.T) {
	f := newFakeGrafanaOrg(
		grafanaOrgUser{UserID: 1, Login: "admin", Role: "Admin"},
		grafanaOrgUser{UserID: 2, Login: "matthew.baker", Email: "matthew.baker@acme.example", Role: "Viewer"},
	)
	base := f.start(t)

	users, err := listGrafanaOrgUsers(context.Background(), base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("want 2 users, got %d", len(users))
	}
}

func TestListGrafanaOrgUsers_Forbidden(t *testing.T) {
	f := newFakeGrafanaOrg()
	f.forbid = true
	base := f.start(t)

	if _, err := listGrafanaOrgUsers(context.Background(), base); err == nil {
		t.Fatal("403 should surface as an error")
	}
}

func TestSetGrafanaOrgUserRole(t *testing.T) {
	f := newFakeGrafanaOrg(grafanaOrgUser{UserID: 7, Login: "u7", Role: "Viewer"})
	base := f.start(t)

	if err := setGrafanaOrgUserRole(context.Background(), base, 7, "Admin"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.patches) != 1 || f.patches[0] != (patchRecord{userID: 7, role: "Admin"}) {
		t.Fatalf("unexpected patches: %+v", f.patches)
	}
	if f.users[7].Role != "Admin" {
		t.Errorf("role not applied: %q", f.users[7].Role)
	}
}

func TestSelectBumpTargets_ViewerToAdmin(t *testing.T) {
	users := []grafanaOrgUser{
		{UserID: 1, Login: "admin", Role: "Admin"},    // already Admin → skip
		{UserID: 2, Login: "viewer1", Role: "Viewer"}, // bump
		{UserID: 3, Login: "viewer2", Role: "Viewer"}, // bump
		{UserID: 4, Login: "editor1", Role: "Editor"}, // not from=Viewer → skip
	}
	sel := newUserSelector(nil, nil)
	got := selectBumpTargets(users, "Viewer", "Admin", sel)
	if len(got) != 2 {
		t.Fatalf("want 2 targets, got %d: %+v", len(got), got)
	}
	for _, u := range got {
		if u.Role != "Viewer" {
			t.Errorf("only Viewers should be targeted, got %q", u.Role)
		}
	}
}

func TestSelectBumpTargets_Idempotent(t *testing.T) {
	// Everyone already Admin → nothing to bump even with from="".
	users := []grafanaOrgUser{
		{UserID: 1, Login: "a", Role: "Admin"},
		{UserID: 2, Login: "b", Role: "Admin"},
	}
	got := selectBumpTargets(users, "", "Admin", newUserSelector(nil, nil))
	if len(got) != 0 {
		t.Fatalf("idempotent: want 0 targets, got %d", len(got))
	}
}

func TestSelectBumpTargets_FromAnyRole(t *testing.T) {
	users := []grafanaOrgUser{
		{UserID: 1, Login: "v", Role: "Viewer"},
		{UserID: 2, Login: "e", Role: "Editor"},
		{UserID: 3, Login: "a", Role: "Admin"}, // already target → skip
	}
	got := selectBumpTargets(users, "", "Admin", newUserSelector(nil, nil))
	if len(got) != 2 {
		t.Fatalf("from-any: want 2 targets (Viewer+Editor), got %d", len(got))
	}
}

func TestSelectBumpTargets_ByLogin(t *testing.T) {
	users := []grafanaOrgUser{
		{UserID: 1, Login: "matthew.baker", Email: "matthew.baker@acme.example", Role: "Viewer"},
		{UserID: 2, Login: "someone.else", Role: "Viewer"},
	}
	sel := newUserSelector([]string{"matthew.baker"}, nil)
	got := selectBumpTargets(users, "Viewer", "Admin", sel)
	if len(got) != 1 || got[0].UserID != 1 {
		t.Fatalf("by-login: want just userId=1, got %+v", got)
	}
	if err := sel.unmatchedErr(); err != nil {
		t.Errorf("all names matched, want nil err, got %v", err)
	}
}

func TestSelectBumpTargets_ByEmailCaseInsensitive(t *testing.T) {
	users := []grafanaOrgUser{
		{UserID: 1, Login: "mb", Email: "Matthew.Baker@acme.example", Role: "Viewer"},
	}
	sel := newUserSelector(nil, []string{"matthew.baker@acme.example"})
	got := selectBumpTargets(users, "Viewer", "Admin", sel)
	if len(got) != 1 {
		t.Fatalf("email match should be case-insensitive, got %d", len(got))
	}
}

func TestUserSelector_UnmatchedErr(t *testing.T) {
	users := []grafanaOrgUser{{UserID: 1, Login: "present", Role: "Viewer"}}
	sel := newUserSelector([]string{"present", "typo-user"}, nil)
	selectBumpTargets(users, "Viewer", "Admin", sel)
	err := sel.unmatchedErr()
	if err == nil {
		t.Fatal("a --login that matched nobody should error")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "typo-user") {
		t.Errorf("error should name the unmatched login, got %q", got)
	}
	// The matched login must NOT appear in the error.
	if strings.Contains(err.Error(), "login=present") {
		t.Errorf("matched login should not be reported unmatched: %q", err.Error())
	}
}

func TestFilterUsersByRole(t *testing.T) {
	users := []grafanaOrgUser{
		{UserID: 1, Role: "Viewer"},
		{UserID: 2, Role: "admin"}, // lower-case from a hypothetical source
		{UserID: 3, Role: "Viewer"},
	}
	if got := filterUsersByRole(users, "Viewer"); len(got) != 2 {
		t.Errorf("want 2 Viewers, got %d", len(got))
	}
	if got := filterUsersByRole(users, "Admin"); len(got) != 1 {
		t.Errorf("case-insensitive Admin match: want 1, got %d", len(got))
	}
}

func TestUserLabel(t *testing.T) {
	if got := userLabel(grafanaOrgUser{UserID: 5, Login: "x", Email: "y@z"}); got != "x" {
		t.Errorf("login should win: %q", got)
	}
	if got := userLabel(grafanaOrgUser{UserID: 5, Email: "y@z"}); got != "y@z" {
		t.Errorf("email fallback: %q", got)
	}
	if got := userLabel(grafanaOrgUser{UserID: 5}); got != "userId=5" {
		t.Errorf("id fallback: %q", got)
	}
}
