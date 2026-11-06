package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

// permissionButtonValue is the Allow button's value on the newest message in
// channel of twin.
func permissionButtonValue(t *testing.T, twin *surfacetest.SlackTwin, channel string) string {
	t.Helper()
	msgs := twin.Messages(channel)
	if len(msgs) == 0 {
		t.Fatalf("no message in %s", channel)
	}
	var blocks []struct {
		Elements []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(msgs[len(msgs)-1][2]), &blocks); err != nil {
		t.Fatalf("blocks of the newest message in %s: %v", channel, err)
	}
	for _, bl := range blocks {
		for _, el := range bl.Elements {
			if el.ActionID == "permission_allow" {
				return el.Value
			}
		}
	}
	t.Fatalf("no Allow button on the newest message in %s", channel)
	return ""
}

// permFixture is crew-x of the gardenio land (newDecisionCardsBot) with an
// open permission request req-1 and a coop endpoint counting verdicts.
type permFixture struct {
	b        *Bot
	daemon   *mockDaemon
	id       string // the permission bead
	verdicts *atomic.Int32
}

func newPermFixture(t *testing.T) *permFixture {
	t.Helper()
	b, daemon := newDecisionCardsBot(t, surfacetest.NewSlackTwin())
	var verdicts atomic.Int32
	coop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		verdicts.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
	}))
	t.Cleanup(coop.Close)
	daemon.mu.Lock()
	daemon.beads["gd-agent-x"].Notes = "coop_url: " + coop.URL
	daemon.mu.Unlock()
	id, land := b.writePermissionBead(context.Background(), channelPermissionCallback{
		Type: "permission_request", RequestID: "req-1", ToolName: "Bash", Agent: "crew-x",
	})
	if id == "" || land != "gardenio" {
		t.Fatalf("writePermissionBead = (%q, %q)", id, land)
	}
	return &permFixture{b: b, daemon: daemon, id: id, verdicts: &verdicts}
}

func (f *permFixture) click(slackUserID, value string) {
	var cb slack.InteractionCallback
	cb.Channel.ID, cb.Message.Timestamp, cb.User.ID, cb.User.Name = "C0CREW", "1690000000.000001", slackUserID, "renamed-at-will"
	f.b.resolvePermission(context.Background(), value, "allow", cb)
}

func (f *permFixture) closes(id string) []closeCall {
	var out []closeCall
	for _, c := range f.daemon.getClosed() {
		if c.BeadID == id {
			out = append(out, c)
		}
	}
	return out
}

func (f *permFixture) tenantLand() {
	f.daemon.mu.Lock()
	f.daemon.beads["proj-gardenio"] = &beadsapi.BeadDetail{ID: "proj-gardenio", Type: "project", Title: "gardenio", Fields: map[string]string{"land_kind": "tenant"}}
	f.daemon.mu.Unlock()
}

// The answer records the Slack user id and the person it is linked to, never
// the username the clicker can change (gd-eemeNhUt5I).
func TestResolvePermission_RecordsSlackIDAndPrincipal(t *testing.T) {
	f := newPermFixture(t)
	f.click("UMATT", "req-1|crew-x|"+f.id)
	got := f.closes(f.id)
	if len(got) != 1 {
		t.Fatalf("closes of %s = %+v, want one", f.id, f.daemon.getClosed())
	}
	if got[0].Fields["answered_by"] != "oidc:matt@example.com" || got[0].Fields["answered_by_slack_id"] != "UMATT" || got[0].Fields["behavior"] != "allow" {
		t.Errorf("close fields = %v", got[0].Fields)
	}
	if n := f.verdicts.Load(); n != 1 {
		t.Errorf("verdicts delivered = %d, want 1", n)
	}
}

// The click closes the bead its button names, not the first open bead of the
// same request id and agent name (names are not unique across lands).
func TestResolvePermission_ClosesTheBeadTheButtonNames(t *testing.T) {
	f := newPermFixture(t)
	f.daemon.mu.Lock()
	f.daemon.beads["gd-perm-other"] = &beadsapi.BeadDetail{
		ID: "gd-perm-other", Type: "permission", Status: "open", Labels: []string{"project:acme"},
		Fields: map[string]string{"agent": "crew-x", "request_id": "req-1", "tool_name": "Bash"},
	}
	f.daemon.mu.Unlock()

	f.click("UMATT", "req-1|crew-x|"+f.id)
	if len(f.closes(f.id)) != 1 || len(f.closes("gd-perm-other")) != 0 {
		t.Fatalf("closed = %+v, want only %s", f.daemon.getClosed(), f.id)
	}
}

// A prompt posted before the button carried the bead closes the one open bead
// of its request, and none when two lands share it; the verdict goes either way.
func TestResolvePermission_LegacyButtonValue(t *testing.T) {
	f := newPermFixture(t)
	f.click("UMATT", "req-1|crew-x")
	if len(f.closes(f.id)) != 1 {
		t.Fatalf("legacy value, one open bead: closed = %+v", f.daemon.getClosed())
	}

	f = newPermFixture(t)
	f.daemon.mu.Lock()
	f.daemon.beads["gd-perm-other"] = &beadsapi.BeadDetail{
		ID: "gd-perm-other", Type: "permission", Status: "open", Labels: []string{"project:acme"},
		Fields: map[string]string{"agent": "crew-x", "request_id": "req-1", "tool_name": "Bash"},
	}
	f.daemon.mu.Unlock()
	f.click("UMATT", "req-1|crew-x")
	if c := f.daemon.getClosed(); len(c) != 0 {
		t.Fatalf("legacy value, ambiguous: closed = %+v, want none", c)
	}
	if n := f.verdicts.Load(); n != 1 {
		t.Errorf("verdicts delivered = %d, want 1", n)
	}
}

// A button that names another request's bead, or an answered one, sends no
// verdict and closes nothing.
func TestResolvePermission_RefusesAMismatchedOrAnsweredBead(t *testing.T) {
	f := newPermFixture(t)
	f.click("UMATT", "req-2|crew-x|"+f.id)
	f.click("UMATT", "req-1|crew-t|"+f.id)
	if c := f.daemon.getClosed(); len(c) != 0 || f.verdicts.Load() != 0 {
		t.Fatalf("mismatched button: closed %+v, verdicts %d", c, f.verdicts.Load())
	}

	f.click("UMATT", "req-1|crew-x|"+f.id)
	f.click("UMATT", "req-1|crew-x|"+f.id)
	if len(f.closes(f.id)) != 1 || f.verdicts.Load() != 1 {
		t.Fatalf("second click on an answered request: closes %d, verdicts %d, want 1 and 1", len(f.closes(f.id)), f.verdicts.Load())
	}
}

// Without Keycloak a person mapped by slack_id answers in an operator land; an
// unmapped or unidentified clicker, or any clicker in a tenant land, is refused.
func TestResolvePermission_RefusesClickersWithoutKeycloak(t *testing.T) {
	for _, c := range []struct {
		name, user string
		tenant     bool
	}{
		{"no Slack user", "", false},
		{"unmapped Slack user", "UNOBODY", false},
		{"mapped person in a tenant land", "UMATT", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPermFixture(t)
			if c.tenant {
				f.tenantLand()
			}
			f.click(c.user, "req-1|crew-x|"+f.id)
			if got := f.daemon.getClosed(); len(got) != 0 || f.verdicts.Load() != 0 {
				t.Fatalf("closed %+v, verdicts %d, want neither", got, f.verdicts.Load())
			}
		})
	}
}

// stubLandAccess is a PersonRoleSource that also answers LandAccess.
type stubLandAccess struct {
	roles, groups []string
	err           error
}

func (s stubLandAccess) GardenioRoles(context.Context, string) ([]string, error) {
	return s.roles, s.err
}

func (s stubLandAccess) LandAccess(context.Context, string) ([]string, []string, error) {
	return s.roles, s.groups, s.err
}

// With Keycloak the clicker must be an operator or a member of the request's
// land, read as kbeads reads a token.
func TestResolvePermission_LandRoleFromKeycloak(t *testing.T) {
	for _, c := range []struct {
		name   string
		src    stubLandAccess
		tenant bool
		want   bool
	}{
		{"admin", stubLandAccess{roles: []string{"gardenio-admin"}}, true, true},
		{"admin who is also a project member", stubLandAccess{roles: []string{"gardenio-admin", "gardenio-project-member"}}, true, true},
		{"operator", stubLandAccess{roles: []string{"gardenio-operator"}}, false, true},
		{"operator in a tenant land", stubLandAccess{roles: []string{"gardenio-operator"}}, true, true},
		{"member of the land", stubLandAccess{roles: []string{"gardenio-project-member"}, groups: []string{"/project-gardenio"}}, true, true},
		{"owner of the land", stubLandAccess{roles: []string{"gardenio-project-member"}, groups: []string{"/project-gardenio/owners"}}, true, true},
		{"guest of the land", stubLandAccess{roles: []string{"gardenio-project-member"}, groups: []string{"/project-gardenio/guests"}}, true, false},
		{"member of another land", stubLandAccess{roles: []string{"gardenio-project-member"}, groups: []string{"/project-acme"}}, true, false},
		{"project member who is also an operator", stubLandAccess{roles: []string{"gardenio-operator", "gardenio-project-member"}, groups: []string{"/project-acme"}}, false, false},
		{"viewer", stubLandAccess{roles: []string{"gardenio-viewer"}}, false, false},
		{"no roles", stubLandAccess{}, false, false},
		{"Keycloak down", stubLandAccess{err: errors.New("keycloak down")}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPermFixture(t)
			if c.tenant {
				f.tenantLand()
			}
			f.b.SetPersonRoleSource(c.src)
			f.click("UMATT", "req-1|crew-x|"+f.id)
			if answered := len(f.closes(f.id)) == 1 && f.verdicts.Load() == 1; answered != c.want {
				t.Fatalf("answered = %v (closed %+v, verdicts %d), want %v", answered, f.daemon.getClosed(), f.verdicts.Load(), c.want)
			}
		})
	}
}

// A per-agent token names the asking agent, its bead and its land; the body
// may only repeat it.
func TestPermissionCallback_TakesTheAgentFromItsToken(t *testing.T) {
	b, daemon := newDecisionCardsBot(t, surfacetest.NewSlackTwin())
	h := NewPermissionCallbackHandler(b, b.logger)
	tok := WithAgentCaller(context.Background(), AgentCaller{Agent: "crew-x", BeadID: "gd-agent-x2", Land: "acme"})

	cb := channelPermissionCallback{RequestID: "req-1", ToolName: "Bash"}
	if err := h.askerOf(tok, &cb); err != nil {
		t.Fatalf("token caller, no body agent: %v", err)
	}
	if cb.Agent != "crew-x" || cb.agentBeadID != "gd-agent-x2" || cb.land != "acme" {
		t.Fatalf("asker = %+v", cb)
	}
	// The bead is written in the token's land with the token's agent bead,
	// although crew-x by name is gardenio's.
	id, land := b.writePermissionBead(context.Background(), cb)
	if id == "" || land != "acme" || daemon.beads[id].Fields["agent_bead_id"] != "gd-agent-x2" {
		t.Fatalf("writePermissionBead = (%q, %q), bead %+v", id, land, daemon.beads[id])
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleCallback(w, r.WithContext(WithAgentCaller(r.Context(), AgentCaller{Agent: "crew-x", Land: "gardenio"})))
	}))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"request_id":"req-2","tool_name":"Bash","agent":"crew-t"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token for crew-x naming crew-t: %d, want 403", resp.StatusCode)
	}
}

// The shared bearer still names the agent, in operator lands only, and not at
// all once retired; an OIDC admin names it anywhere.
func TestPermissionCallback_SharedBearerAndOperator(t *testing.T) {
	b, daemon := newDecisionCardsBot(t, surfacetest.NewSlackTwin())
	h := NewPermissionCallbackHandler(b, b.logger)
	ctx := context.Background()
	status := func(err error) int {
		var se *sayError
		if errors.As(err, &se) {
			return se.status
		}
		if err != nil {
			return -1
		}
		return http.StatusOK
	}
	ask := func(ctx context.Context, agent string) int {
		cb := channelPermissionCallback{RequestID: "req-1", ToolName: "Bash", Agent: agent}
		return status(h.askerOf(ctx, &cb))
	}

	if got := ask(ctx, "crew-x"); got != http.StatusOK {
		t.Errorf("bearer, operator land: %d, want 200", got)
	}
	if got := ask(ctx, ""); got != http.StatusBadRequest {
		t.Errorf("bearer, no agent: %d, want 400", got)
	}
	daemon.mu.Lock()
	daemon.beads["proj-gardenio"] = &beadsapi.BeadDetail{ID: "proj-gardenio", Type: "project", Title: "gardenio", Fields: map[string]string{"land_kind": "tenant"}}
	daemon.mu.Unlock()
	if got := ask(ctx, "crew-x"); got != http.StatusForbidden {
		t.Errorf("bearer, tenant land: %d, want 403", got)
	}
	if got := ask(WithOperatorCaller(ctx), "crew-x"); got != http.StatusOK {
		t.Errorf("OIDC admin, tenant land: %d, want 200", got)
	}
	h.SetSharedBearerAgents(false)
	if got := ask(ctx, "crew-x"); got != http.StatusUnauthorized {
		t.Errorf("retired bearer: %d, want 401", got)
	}
}

// LandAccess reads every effective realm role and the user's group paths.
func TestKeycloakRoleChecker_LandAccess(t *testing.T) {
	var groupCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/gardenio/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
	})
	mux.HandleFunc("/admin/realms/gardenio/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("email") == "ann@example.com" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "kc-ann", "email": "Ann@example.com", "enabled": true}})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("/admin/realms/gardenio/users/kc-ann/role-mappings/realm/composite", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "gardenio-project-member"}, {"name": "offline_access"}, {"name": "gardenio-project-member"}})
	})
	mux.HandleFunc("/admin/realms/gardenio/users/kc-ann/groups", func(w http.ResponseWriter, _ *http.Request) {
		groupCalls.Add(1)
		_ = json.NewEncoder(w).Encode([]map[string]string{{"path": "/project-acme/owners"}, {"path": "/project-beta"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	k := NewKeycloakRoleChecker(KeycloakConfig{BaseURL: srv.URL, Realm: "gardenio", ClientID: "c", ClientSecret: "s"}, slog.Default())

	for range 2 {
		roles, groups, err := k.LandAccess(context.Background(), " ANN@example.com ")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(roles, ",") != "gardenio-project-member,offline_access" || strings.Join(groups, ",") != "/project-acme/owners,/project-beta" {
			t.Fatalf("LandAccess = %v %v", roles, groups)
		}
	}
	if n := groupCalls.Load(); n != 1 {
		t.Errorf("group lookups = %d, want 1 (cached)", n)
	}
	roles, groups, err := k.LandAccess(context.Background(), "nobody@example.com")
	if err != nil || roles != nil || groups != nil {
		t.Errorf("unknown user: %v %v %v, want nothing", roles, groups, err)
	}
}
