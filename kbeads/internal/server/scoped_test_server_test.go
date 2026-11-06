package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Shared fixture for the scoped-principal tests: a full-stack HTTP server
// (AuthMiddleware → UserProvisioner → requireRole → handler) behind an OIDC
// mock whose tokens carry a verified groups claim.
const (
	scopedAdminToken = "scoped-test-static-admin"

	tokScopedClaimlayer = "scoped-claimlayer-tok"
	tokScopedNoGroup    = "scoped-nogroup-tok"
	tokScopedTwo        = "scoped-two-tok"
	// tokScopedWriter is a scoped principal whose token names the browser
	// client (azp), so the write tests reach the handlers' scoped-principal
	// checks.
	tokScopedWriter = "scoped-claimlayer-writer-tok"
	// Land roles (land_role.go): the same writer, as owner or guest of
	// claimlayer; tokScopedGuestOther is a guest in claimlayer and a member
	// of other.
	tokScopedOwner      = "scoped-claimlayer-owner-tok"
	tokScopedOwnerCLI   = "scoped-claimlayer-owner-cli-tok" // the same owner on gl / kd login
	tokScopedGuest      = "scoped-claimlayer-guest-tok"
	tokScopedGuestOther = "scoped-claimlayer-guest-other-member-tok"
	tokOwnerAdmin       = "owner-admin-tok"
	tokOperator         = "operator-tok"
	// tokLandClaimlayer is the land principal for claimlayer
	// (land_principal.go): a land-claimlayer bridge token, not an OIDC token.
	tokLandClaimlayer = "land-claimlayer-tok"
	// tokFleetBridge is an unscoped fleet token (a bridge's), not OIDC.
	tokFleetBridge = "fleet-bridge-tok"
)

// Seeded beads. gd-1 and cl-1 are in different projects, nl-1 has no project
// label, other-1 is in a third project.
const (
	beadGardenio   = "gd-1"
	beadClaimlayer = "cl-1"
	beadNoLabel    = "nl-1"
	beadOther      = "other-1"
)

type scopedFixture struct {
	ts *httptest.Server
	ms *mockStore
	// keycloak is the OIDC mock behind the server; tests that model a token
	// whose groups change edit its tokens between requests.
	keycloak *oidcIssuerMock
	// fc is the fleet auth behind the server; tests register extra fleet
	// tokens (an agent's per-agent token) on it.
	fc *FleetConfig
	// filed numbers fileEscalation's default operation_spec, so repeat
	// filings are not refused as duplicates (create_dedupe.go).
	filed int
	// srv is the server itself, for tests that set a policy on it.
	srv *BeadsServer
}

func newScopedFixture(t *testing.T) *scopedFixture {
	t.Helper()
	// A Lands tenant's realm roles: the lands-tenants and project-<land>
	// Keycloak groups grant gardenio-project-member and nothing else, so no
	// scoped token here carries viewer or operator (gd-35s9RXARRB).
	member := []string{RoleProjectMember}
	keycloak := &oidcIssuerMock{tokens: map[string]tokenInfo{
		tokScopedClaimlayer: {email: "cl@example.com", roles: member, groups: []string{"/project-claimlayer"}},
		tokScopedNoGroup:    {email: "nogroup@example.com", roles: member},
		tokScopedTwo:        {email: "two@example.com", roles: member, groups: []string{"/project-claimlayer", "/project-other"}},
		tokScopedWriter:     {email: "clw@example.com", roles: member, groups: []string{"/project-claimlayer"}, azp: testBrowserClient},
		tokScopedOwner:      {email: "clo@example.com", roles: member, groups: []string{"/project-claimlayer/owners"}, azp: testBrowserClient},
		tokScopedOwnerCLI:   {email: "clo@example.com", roles: member, groups: []string{"/project-claimlayer/owners"}, azp: DeviceGrantClientID},
		tokScopedGuest:      {email: "clg@example.com", roles: member, groups: []string{"/project-claimlayer/guests"}, azp: testBrowserClient},
		tokScopedGuestOther: {email: "clgo@example.com", roles: member, groups: []string{"/project-claimlayer/guests", "/project-other"}, azp: testBrowserClient},
		tokOwnerAdmin:       {email: "owner@example.com", roles: []string{RoleViewer, RoleOperator, RoleAdmin}, azp: testBrowserClient},
		tokOperator:         {email: "op@example.com", roles: []string{RoleViewer, RoleOperator}, azp: testBrowserClient},
	}}
	verifier := &multiIssuerMock{issuers: []*oidcIssuerMock{keycloak}}
	// Built by hand (not newMultiIssuerE2EServer) so the per-identity rate
	// limiter can be switched off: the write matrix issues hundreds of requests.
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("gardenio", nil)
	registerLandPrincipal(fc, tokLandClaimlayer, "claimlayer")
	fc.RegisterToken(tokFleetBridge, "slack-bridge")
	srv.SetFleetAuth(fc)
	srv.SetRateLimit(RateLimitConfig{})
	srv.SetForwardAuthClients([]string{testBrowserClient})
	ts := httptest.NewServer(srv.NewHTTPHandler(verifier, withTestToken(fc, scopedAdminToken)))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { stopWindowViewTimers(srv) })

	for id, project := range map[string]string{
		beadGardenio:   "gardenio",
		beadClaimlayer: "claimlayer",
		beadNoLabel:    "",
		beadOther:      "other",
	} {
		ms.beads[id] = &model.Bead{ID: id, Title: id, Type: "task", Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
		if project != "" {
			ms.labels[id] = []string{"project:" + project}
		}
	}
	return &scopedFixture{ts: ts, ms: ms, keycloak: keycloak, fc: fc, srv: srv}
}

// stopWindowViewTimers stops every armed window-view flush, so a held tally
// cannot fire into this fixture's store after its test ends (gd-AHNBxcUXAB).
// A flush already running is not waited for; the mock's locks cover it.
func stopWindowViewTimers(srv *BeadsServer) {
	windowViewsInitMu.Lock()
	t := srv.windowViews
	windowViewsInitMu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, v := range t.windows {
		if v.timer != nil {
			v.timer.Stop()
		}
	}
}

func (f *scopedFixture) get(t *testing.T, token, path string) *http.Response {
	t.Helper()
	return oidcE2ERequest(t, f.ts, http.MethodGet, path, token, nil)
}

// listIDs lists beads as token and returns the ids it saw.
func (f *scopedFixture) listIDs(t *testing.T, token string) map[string]bool {
	t.Helper()
	resp := f.get(t, token, "/v1/beads?limit=100")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list as %s: status %d", token, resp.StatusCode)
	}
	var body struct {
		Beads []*model.Bead `json:"beads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	ids := map[string]bool{}
	for _, b := range body.Beads {
		// The provisioned user beads are not part of the seeded set.
		if b.Type == "user" {
			continue
		}
		ids[b.ID] = true
	}
	return ids
}
