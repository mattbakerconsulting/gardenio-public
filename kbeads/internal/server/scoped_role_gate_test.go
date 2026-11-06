package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// A Lands tenant's token carries gardenio-project-member and its
// /project-<land> groups, nothing else: the lands-tenants and project-<name>
// Keycloak groups grant that one role (helm values keycloak groups,
// templates/keycloak/groups.yaml). gd-35s9RXARRB: requireRole refused every
// land-door read and write for that shape, because project-member has no rank
// and the scoped fixture's tokens also carried viewer and operator.

const tokScopedMemberOnly = "scoped-claimlayer-member-only-tok"

func newMemberOnlyFixture(t *testing.T) *scopedFixture {
	t.Helper()
	f := newScopedFixture(t)
	f.keycloak.tokens[tokScopedMemberOnly] = tokenInfo{
		email: "tenant@example.com", roles: []string{RoleProjectMember},
		groups: []string{"/project-claimlayer"}, azp: testBrowserClient,
	}
	return f
}

// TestScopedRoleGate_MemberOnlyTenant drives the real tenant shape through the
// full stack: it reads and writes in its own land and nowhere else.
func TestScopedRoleGate_MemberOnlyTenant(t *testing.T) {
	f := newMemberOnlyFixture(t)
	tok := tokScopedMemberOnly

	// (a) its own land: list, get, comment, create.
	if ids := f.listIDs(t, tok); !ids[beadClaimlayer] || len(ids) != 1 {
		t.Errorf("list: saw %v, want only %s", ids, beadClaimlayer)
	}
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{http.MethodGet, "/v1/beads/" + beadClaimlayer, nil, http.StatusOK},
		{http.MethodGet, "/v1/beads/" + beadClaimlayer + "/comments", nil, http.StatusOK},
		{http.MethodPost, "/v1/beads/" + beadClaimlayer + "/comments", map[string]any{"text": "hi"}, http.StatusCreated},
		{http.MethodPost, "/v1/beads", map[string]any{"title": "t", "type": "task", "labels": []string{"project:claimlayer"}}, http.StatusCreated},
		{http.MethodGet, "/v1/whoami", nil, http.StatusOK},
	} {
		if code, body := f.do(t, tok, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, code, body, c.want)
		}
	}

	// (b) another land's bead: never served, never written.
	for _, id := range []string{beadGardenio, beadOther, beadNoLabel} {
		if code, _ := f.do(t, tok, http.MethodGet, "/v1/beads/"+id, nil); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 403 or 404", id, code)
		}
		if code, _ := f.do(t, tok, http.MethodPost, "/v1/beads/"+id+"/comments", map[string]any{"text": "pwned"}); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("comment on %s: %d, want 403 or 404", id, code)
		}
	}
	if code, body := f.do(t, tok, http.MethodPost, "/v1/beads", map[string]any{"title": "t", "type": "task", "labels": []string{"project:gardenio"}}); code != http.StatusForbidden {
		t.Errorf("create in another land: %d %s, want 403", code, body)
	}

	// (c) global routes stay closed.
	for _, p := range []string{"/v1/stats", "/v1/configs", "/v1/taints"} {
		if code, _ := f.do(t, tok, http.MethodGet, p, nil); code != http.StatusForbidden {
			t.Errorf("GET %s: %d, want 403", p, code)
		}
	}
}

// TestScopedRoleGate_NonScopedUnchanged pins that the waiver is for scoped
// principals only: an OIDC caller without project-member is still ranked.
func TestScopedRoleGate_NonScopedUnchanged(t *testing.T) {
	f := newScopedFixture(t)
	f.keycloak.tokens["viewer-only-tok"] = tokenInfo{email: "v@example.com", roles: []string{RoleViewer}, azp: testBrowserClient}
	f.keycloak.tokens["no-role-tok"] = tokenInfo{email: "n@example.com", groups: []string{"/project-claimlayer"}, azp: testBrowserClient}
	create := map[string]any{"title": "t", "type": "task", "labels": []string{"project:claimlayer"}}

	if code, _ := f.do(t, "no-role-tok", http.MethodGet, "/v1/beads", nil); code != http.StatusForbidden {
		t.Errorf("no-role list: %d, want 403", code)
	}
	if code, _ := f.do(t, "viewer-only-tok", http.MethodGet, "/v1/beads", nil); code != http.StatusOK {
		t.Errorf("viewer list: %d, want 200", code)
	}
	if code, _ := f.do(t, "viewer-only-tok", http.MethodPost, "/v1/beads", create); code != http.StatusForbidden {
		t.Errorf("viewer create: %d, want 403", code)
	}
	for _, tok := range []string{tokOperator, tokOwnerAdmin} {
		if code, body := f.do(t, tok, http.MethodPost, "/v1/beads", create); code != http.StatusCreated {
			t.Errorf("%s create: %d %s, want 201", tok, code, body)
		}
	}
	if code, _ := f.do(t, tokOperator, http.MethodGet, "/v1/stats", nil); code != http.StatusOK {
		t.Errorf("operator stats: %d, want 200", code)
	}
}

// TestScopedRouteAdmits_Matrix walks every registered route at every rank: a
// member-only scoped principal clears requireRole exactly on the routes
// scopedRouteGate lets it reach, below RoleAdmin; a member who is also an
// admin and an unscoped OIDC caller get no waiver.
func TestScopedRouteAdmits_Matrix(t *testing.T) {
	oidc := func(roles []string, groups ...string) context.Context {
		ctx := withAuthMethod(context.Background(), authMethodOIDC)
		return WithGroups(WithRoles(ctx, roles), groups)
	}
	member := oidc([]string{RoleProjectMember}, "/project-claimlayer")
	callers := map[string]struct {
		ctx    context.Context
		waived bool // gets the scoped waiver at all
	}{
		"member-only":      {member, true},
		"member+viewer":    {oidc([]string{RoleViewer, RoleProjectMember}, "/project-claimlayer"), true},
		"member+admin":     {oidc([]string{RoleAdmin, RoleProjectMember}, "/project-claimlayer"), false},
		"no role":          {oidc(nil, "/project-claimlayer"), false},
		"viewer, unscoped": {oidc([]string{RoleViewer}), false},
	}

	patterns := registeredRoutePatterns()
	if len(patterns) < 100 {
		t.Fatalf("only %d routes recorded; the registration walk is broken", len(patterns))
	}
	for name, c := range callers {
		for _, p := range patterns {
			class := scopedRouteClasses[p]
			reachable := class == classFiltered || class == classScopedSafe
			for _, role := range []string{RoleViewer, RoleOperator, RoleAdmin} {
				r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(c.ctx)
				r.Pattern = p
				want := c.waived && reachable && role != RoleAdmin
				if got := scopedRouteAdmits(r, role); got != want {
					t.Errorf("%s on %q at %s: waived=%v, want %v", name, p, role, got, want)
				}
			}
		}
	}

	// A land principal is scoped too (it already holds viewer and operator,
	// so the waiver changes nothing for it); never at admin.
	land := landPrincipalContext(context.Background(), nil, "claimlayer")
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(land)
	r.Pattern = "POST /v1/beads"
	if !scopedRouteAdmits(r, RoleOperator) || scopedRouteAdmits(r, RoleAdmin) {
		t.Errorf("land principal: want waived below admin only")
	}

	// An unmatched pattern (a request requireRole sees outside the mux) is
	// never waived.
	r = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(member)
	if scopedRouteAdmits(r, RoleViewer) {
		t.Errorf("empty pattern waived")
	}

	// requireRole itself: the member-only tenant reaches the handler on a
	// scoped operator route and is refused on an admin-only one.
	served := false
	h := requireRole(RoleOperator, func(http.ResponseWriter, *http.Request) { served = true })
	for _, c := range []struct {
		pattern string
		want    bool
	}{{"POST /v1/beads/{id}/comments", true}, {"POST /v1/beads/{id}/transfer", false}, {"POST /v1/taints", false}} {
		served = false
		r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(member)
		r.Pattern = c.pattern
		w := httptest.NewRecorder()
		h(w, r)
		if served != c.want {
			t.Errorf("requireRole(operator) on %s: served=%v (%d), want %v", c.pattern, served, w.Code, c.want)
		}
	}
}

// TestScopedRouteAdmits_WriteRoutesPinned pins the non-GET routes a
// member-only tenant clears at the role gate. Each handler confines a scoped
// principal (scopedWriteDenial and the actionTable, a bespoke scoped check, or
// the caller's own rows only); adding a route here is a review of that handler.
//
// The Lands map's write routes (/v1/lands/v1/..., lands_api.go) carry no role
// wrapper of their own: each one only calls this daemon's own routes in-process
// with the caller's context, so the inner route's role gate, scope filter and
// actionTable confine it. They are pinned here so a new map verb is reviewed
// the same way.
func TestScopedRouteAdmits_WriteRoutesPinned(t *testing.T) {
	want := []string{
		"DELETE /v1/beads/{id}/dependencies",
		"DELETE /v1/beads/{id}/labels/{label}",
		"DELETE /v1/devices/{token}",
		"DELETE /v1/windows/{id}", // windowManageDenial: owner tier only
		"PATCH /v1/beads/{id}",
		"PATCH /v1/lands/v1/lands/{land}/tickets/{ticket}",
		"POST /v1/auth/refresh",
		"POST /v1/beads",
		"POST /v1/beads/{id}/close",
		"POST /v1/beads/{id}/comments",
		"POST /v1/beads/{id}/dependencies",
		"POST /v1/beads/{id}/fields/{field}/append",
		"POST /v1/beads/{id}/fields/{field}/edit",
		"POST /v1/beads/{id}/fields/{field}/reorder",
		"POST /v1/beads/{id}/increment",
		"POST /v1/beads/{id}/labels",
		"POST /v1/decisions/{id}/cancel",
		"POST /v1/decisions/{id}/resolve",
		"POST /v1/devices",
		"POST /v1/escalations/{id}/approve",
		"POST /v1/escalations/{id}/audience",
		"POST /v1/escalations/{id}/can-approve",
		"POST /v1/escalations/{id}/claim",
		"POST /v1/escalations/{id}/hand-up",
		"POST /v1/escalations/{id}/reject",
		"POST /v1/escalations/{id}/result",
		"POST /v1/graph",
		// A member's own-machine agents: landAgentMayStart and
		// landAgentMayRevoke judge the caller after the map's admit.
		"POST /v1/lands/v1/lands/{land}/agents",
		"POST /v1/lands/v1/lands/{land}/agents/{agent}/refresh",
		"POST /v1/lands/v1/lands/{land}/agents/{agent}/revoke",
		"POST /v1/lands/v1/lands/{land}/decisions/{decision}/cancel",
		"POST /v1/lands/v1/lands/{land}/decisions/{decision}/comments",
		"POST /v1/lands/v1/lands/{land}/decisions/{decision}/resolve",
		"POST /v1/lands/v1/lands/{land}/escalations",
		"POST /v1/lands/v1/lands/{land}/plans/{plan}/answer",
		// Releases: releaseDeployDenial admits only the land's owner, for its
		// own project's releases. A land's CD credential is not admitted yet.
		"POST /v1/lands/v1/lands/{land}/releases/{release}/deployments",
		"POST /v1/lands/v1/lands/{land}/threads/{thread}/messages",
		"POST /v1/lands/v1/lands/{land}/tickets",
		"POST /v1/lands/v1/lands/{land}/tickets/{ticket}/close",
		"POST /v1/lands/v1/lands/{land}/tickets/{ticket}/comments",
		"POST /v1/lands/v1/lands/{land}/tickets/{ticket}/reopen",
		"POST /v1/lands/{land}/steward-delegation",
		// The daemon route the contract's reportDeployment rides; a member,
		// and an owner of another land, get releaseDeployDenial's 403.
		"POST /v1/releases/{id}/deployments",
		"POST /v1/runners/register",
		"POST /v1/scrub-requests",
		"POST /v1/scrub-requests/{id}/approve",
		"POST /v1/scrub-requests/{id}/attest",
		"POST /v1/scrub-requests/{id}/reject",
		"POST /v1/search/semantic",
		"POST /v1/users/me/consent-push",
		"POST /v1/users/me/read-cursors",
		"POST /v1/users/me/terms",
		"POST /v1/users/me/welcome-tour",
		// Windows: windowManageDenial refuses a member; only the land's owner
		// tier (or an operator) mints and revokes.
		"POST /v1/windows",
	}
	member := WithGroups(WithRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{RoleProjectMember}), []string{"/project-claimlayer"})
	var got []string
	for _, p := range registeredRoutePatterns() {
		if strings.HasPrefix(p, "GET ") {
			continue
		}
		r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(member)
		r.Pattern = p
		if scopedRouteAdmits(r, RoleOperator) {
			got = append(got, p)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("non-GET routes waived for a member-only tenant changed:\n got  %q\n want %q", got, want)
	}
}
