package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// A member's own-machine agent through the whole HTTP chain (D.17
// gd-foQLqIRXDq): who may mint, refresh and revoke, what the bead and token
// are, and the land's live cap.

type landAgentGrant struct {
	AgentID   string `json:"agent_id"`
	Bearer    string `json:"bearer"`
	Refresh   string `json:"refresh"`
	ExpiresAt string `json:"expires_at"`
	DoorURL   string `json:"door_url"`
}

// newLandAgentFixture is the scoped fixture with claimlayer a tenant land
// capped at limit, the lands-gardener role bead and a refresh manager.
func newLandAgentFixture(t *testing.T, limit int) *scopedFixture {
	t.Helper()
	f := newScopedFixture(t)
	f.srv.SetRefreshTokenManager(NewRefreshTokenManager(f.ms, f.fc))
	f.ms.beads["cl-proj"] = &model.Bead{ID: "cl-proj", Title: "claimlayer", Type: "project", Kind: "config", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"tenant","max_live_agents":` + strconv.Itoa(limit) + `}`)}
	makeTierRoleBead(f.ms, "gd-rlg", landsGardenerRole, `{"capability_tiers":["lands-tenant"],"privileged":false}`)
	return f
}

func (f *scopedFixture) mintLandAgent(t *testing.T, token string, body any) (int, landAgentGrant, string) {
	t.Helper()
	code, _, raw := f.landsDo(t, token, http.MethodPost, "/claimlayer/agents", body)
	var g landAgentGrant
	if code == http.StatusCreated {
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			t.Fatalf("grant: %v\n%s", err, raw)
		}
	}
	return code, g, raw
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestLandAgents_MintRefreshRevoke(t *testing.T) {
	f := newLandAgentFixture(t, 5)

	code, g, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{"name": "studio"})
	if code != http.StatusCreated {
		t.Fatalf("member mint: %d %s", code, raw)
	}
	if !hex64.MatchString(g.Bearer) || !hex64.MatchString(g.Refresh) || g.ExpiresAt == "" {
		t.Errorf("grant = %+v", g)
	}
	if !strings.HasSuffix(g.DoorURL, "/land/claimlayer") || !strings.HasPrefix(g.DoorURL, "https://") {
		t.Errorf("door_url = %q", g.DoorURL)
	}

	// The bead: a laptop lands-gardener on the land, acting for the member.
	b := f.ms.beads[g.AgentID]
	if b == nil {
		t.Fatalf("no agent bead %s", g.AgentID)
	}
	fields := parseFieldsToStringMap(b.Fields)
	if b.Type != typeAgent || fields["role"] != landsGardenerRole || fields["project"] != "claimlayer" ||
		fields["execution_target"] != "laptop" || fields["operated_by"] != "oidc:clw@example.com" ||
		!strings.HasPrefix(fields["agent"], "studio-") {
		t.Errorf("agent bead = %+v fields %v", b, fields)
	}
	if b.OperatedBy != "oidc:clw@example.com" {
		t.Errorf("operated_by column = %q", b.OperatedBy)
	}

	// The token: the agent's own, bound to the land, never privileged.
	if fleet := f.fc.ResolveFleet(g.Bearer); fleet != agentFleetName(g.AgentID) {
		t.Errorf("bearer resolves to fleet %q", fleet)
	}
	if p := f.fc.ResolveProject(g.Bearer); p != "claimlayer" {
		t.Errorf("bearer bound to %q", p)
	}
	if f.fc.ResolvePrivileged(g.Bearer) {
		t.Error("bearer is privileged")
	}
	if _, ok := f.ms.configs["bridge_token:"+agentFleetName(g.AgentID)]; !ok {
		t.Error("the bearer is not persisted")
	}

	// Anyone in the land may list it; the starter reads as a display name.
	code, _, raw = f.landsDo(t, tokScopedGuest, http.MethodGet, "/claimlayer/agents", nil)
	if code != http.StatusOK || !strings.Contains(raw, g.AgentID) || !strings.Contains(raw, `"started_by":"clw"`) || strings.Contains(raw, g.Bearer) {
		t.Errorf("list: %d %s", code, raw)
	}

	// The agent refreshes on its own bearer; the refresh is spent.
	code, _, raw = f.landsDo(t, g.Bearer, http.MethodPost, "/claimlayer/agents/"+g.AgentID+"/refresh", map[string]any{"refresh": g.Refresh})
	if code != http.StatusOK {
		t.Fatalf("agent refresh: %d %s", code, raw)
	}
	var g2 landAgentGrant
	_ = json.Unmarshal([]byte(raw), &g2)
	if g2.Bearer == "" || g2.Bearer == g.Bearer || g2.Refresh == "" || g2.Refresh == g.Refresh {
		t.Errorf("refreshed grant = %+v", g2)
	}
	if f.fc.ResolveFleet(g2.Bearer) != agentFleetName(g.AgentID) {
		t.Error("the refreshed bearer is not the agent's")
	}
	code, _, raw = f.landsDo(t, g2.Bearer, http.MethodPost, "/claimlayer/agents/"+g.AgentID+"/refresh", map[string]any{"refresh": g.Refresh})
	assertLandsProblem(t, "spent refresh", code, raw, http.StatusNotFound, "not_found")

	// Another member may not stop it; the owner may.
	code, _, raw = f.landsDo(t, tokScopedClaimlayer, http.MethodPost, "/claimlayer/agents/"+g.AgentID+"/revoke", nil)
	assertLandsProblem(t, "another member revokes", code, raw, http.StatusForbidden, "forbidden")
	code, _, raw = f.landsDo(t, tokScopedOwner, http.MethodPost, "/claimlayer/agents/"+g.AgentID+"/revoke", nil)
	if code != http.StatusOK || !strings.Contains(raw, `"status":"stopped"`) {
		t.Fatalf("owner revoke: %d %s", code, raw)
	}
	if f.fc.HasToken(g.Bearer) || f.fc.HasToken(g2.Bearer) {
		t.Error("a revoked agent's bearer still signs in")
	}
	if b := f.ms.beads[g.AgentID]; b.Status != model.StatusClosed {
		t.Errorf("revoked agent bead status %s", b.Status)
	}
	code, _, raw = f.landsDo(t, tokScopedWriter, http.MethodPost, "/claimlayer/agents/"+g.AgentID+"/refresh", map[string]any{"refresh": g2.Refresh})
	assertLandsProblem(t, "refresh a stopped agent", code, raw, http.StatusConflict, "conflict")
}

// Freezing the land revokes its own-machine agents (gd-nHh7M0k3ps): their
// bearers and refreshes stop working and their beads close, and no new one
// is minted until it thaws. An agent of another land is untouched.
func TestLandAgents_FreezeRevokes(t *testing.T) {
	f := newLandAgentFixture(t, 5)
	code, g, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{"name": "studio"})
	if code != http.StatusCreated {
		t.Fatalf("member mint: %d %s", code, raw)
	}
	other := &model.Bead{ID: "gd-other", Type: typeAgent, Status: model.StatusOpen, Labels: []string{"project:elsewhere", "execution_target:laptop"},
		Fields: json.RawMessage(`{"project":"elsewhere","execution_target":"laptop","role":"` + landsGardenerRole + `","spawn_source":"` + landAgentSpawnSource + `"}`)}
	f.ms.beads[other.ID] = other
	f.fc.RegisterTokenFull("other-agent-bearer", agentFleetName(other.ID), "elsewhere", nil, nil, time.Now().Add(time.Hour), "x", false)

	if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/cl-proj", map[string]any{"fields": map[string]any{"land_frozen": true}}); code != http.StatusOK {
		t.Fatalf("freeze: %d %s", code, body)
	}
	if f.fc.HasToken(g.Bearer) {
		t.Error("a frozen land's agent bearer still signs in")
	}
	b := f.ms.beads[g.AgentID]
	if b.Status != model.StatusClosed || parseFieldsToStringMap(b.Fields)["agent_state"] != "killed" {
		t.Errorf("agent bead after the freeze: status %s fields %s", b.Status, b.Fields)
	}
	code, _, raw = f.landsDo(t, tokScopedWriter, http.MethodPost, "/claimlayer/agents/"+g.AgentID+"/refresh", map[string]any{"refresh": g.Refresh})
	if code < 400 {
		t.Errorf("refresh after the freeze: %d %s", code, raw)
	}
	code, _, raw = f.mintLandAgent(t, tokScopedWriter, map[string]any{})
	if code < 400 {
		t.Errorf("mint in a frozen land: %d %s", code, raw)
	}
	if !f.fc.HasToken("other-agent-bearer") || f.ms.beads[other.ID].Status == model.StatusClosed {
		t.Error("freezing claimlayer revoked another land's agent")
	}
}

// Only an owner or member mints, on their own sign-in: never a guest, the
// land's own token, or an agent's.
func TestLandAgents_WhoMints(t *testing.T) {
	f := newLandAgentFixture(t, 5)
	if code, _, raw := f.mintLandAgent(t, tokScopedOwner, map[string]any{}); code != http.StatusCreated {
		t.Fatalf("owner mint: %d %s", code, raw)
	}
	_, g, _ := f.mintLandAgent(t, tokScopedWriter, map[string]any{})
	for name, tok := range map[string]string{"guest": tokScopedGuest, "land principal": tokLandClaimlayer, "agent": g.Bearer} {
		code, _, raw := f.mintLandAgent(t, tok, map[string]any{})
		assertLandsProblem(t, name+" mints", code, raw, http.StatusForbidden, "forbidden")
	}
	// Another land's member does not see the land at all.
	code, _, raw := f.landsDo(t, tokScopedNoGroup, http.MethodPost, "/claimlayer/agents", map[string]any{})
	if code != http.StatusNotFound && code != http.StatusForbidden {
		t.Errorf("stranger mint: %d %s", code, raw)
	}
}

// The land's max_live_agents holds a member's agents like pod gardeners, the
// steward's slot kept.
func TestLandAgents_LiveCap(t *testing.T) {
	f := newLandAgentFixture(t, 2)
	if code, _, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{}); code != http.StatusCreated {
		t.Fatalf("first mint: %d %s", code, raw)
	}
	code, _, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{})
	assertLandsProblem(t, "mint past the cap", code, raw, http.StatusConflict, "conflict")
}

// A role bead that would hand the agent more than the lands-tenant tier
// refuses the mint.
func TestLandAgents_RoleMustBeTenantTier(t *testing.T) {
	f := newLandAgentFixture(t, 5)
	tokens := len(f.fc.ListBridgeTokens())
	makeTierRoleBead(f.ms, "gd-rlg", landsGardenerRole, `{"capability_tiers":["lands-tenant","privileged"]}`)
	code, _, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{})
	assertLandsProblem(t, "privileged role", code, raw, http.StatusConflict, "conflict")
	if len(f.fc.ListBridgeTokens()) != tokens {
		t.Errorf("tokens minted: %v", f.fc.ListBridgeTokens())
	}
}

// landsDoOperated calls the Lands API as a service token (the static token or
// a fleet token) speaking for a human through X-Operated-By, so a refusal is
// the agent checks' and not the audit-operator requirement's.
func (f *scopedFixture) landsDoOperated(t *testing.T, token, operator, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = ioReader(b)
	}
	req, err := http.NewRequest(method, f.ts.URL+"/v1/lands/v1/lands"+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Operated-By", operator)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// landAgentBeads counts the agent beads the Lands API wrote.
func (f *scopedFixture) landAgentBeads() int {
	n := 0
	for _, b := range f.ms.beads {
		if b.Type == typeAgent && parseFieldsToStringMap(b.Fields)["spawn_source"] == landAgentSpawnSource {
			n++
		}
	}
	return n
}

// No service token mints, admin roles or not: the static token every pod
// holds and a bridge's fleet token are not a member on their own sign-in.
func TestLandAgents_ServiceTokensNeverMint(t *testing.T) {
	admin := []string{RoleViewer, RoleOperator, RoleAdmin}
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		ctx = WithRoles(ctx, admin)
		if landAgentMayStart(ctx, "claimlayer") {
			t.Errorf("%s token with admin roles may start an agent", name)
		}
		mine := &model.Bead{Type: typeAgent, Fields: json.RawMessage(`{"operated_by":"slack:U0TESTER1"}`)}
		if landAgentMayRevoke(ctx, "claimlayer", "slack:U0TESTER1", mine) {
			t.Errorf("%s token with admin roles may revoke an agent", name)
		}
	}

	f := newLandAgentFixture(t, 5)
	tokens := len(f.fc.ListBridgeTokens())
	for name, tok := range map[string]string{"static token": scopedAdminToken, "fleet token": tokFleetBridge} {
		code, raw := f.landsDoOperated(t, tok, "oidc:clw@example.com", http.MethodPost, "/claimlayer/agents", map[string]any{})
		assertLandsProblem(t, name+" mints", code, raw, http.StatusForbidden, "forbidden")
	}
	if n := f.landAgentBeads(); n != 0 {
		t.Errorf("%d agent beads written", n)
	}
	if n := len(f.fc.ListBridgeTokens()); n != tokens {
		t.Errorf("tokens %d, want %d: %v", n, tokens, f.fc.ListBridgeTokens())
	}
}

// Revoke admits the member who started the agent, the land's owner or an
// operator: never a guest, the land principal, the agent itself or a service
// token. A refused revoke leaves the token signing in and the bead open.
func TestLandAgents_WhoRevokes(t *testing.T) {
	f := newLandAgentFixture(t, 5)
	code, g, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("member mint: %d %s", code, raw)
	}
	path := "/claimlayer/agents/" + g.AgentID + "/revoke"
	still := func(what string) {
		t.Helper()
		if !f.fc.HasToken(g.Bearer) {
			t.Errorf("%s: the agent's bearer no longer signs in", what)
		}
		if b := f.ms.beads[g.AgentID]; b.Status == model.StatusClosed {
			t.Errorf("%s: the agent bead was closed", what)
		}
	}
	for name, tok := range map[string]string{"guest": tokScopedGuest, "land principal": tokLandClaimlayer, "the agent": g.Bearer} {
		code, _, raw := f.landsDo(t, tok, http.MethodPost, path, nil)
		assertLandsProblem(t, name+" revokes", code, raw, http.StatusForbidden, "forbidden")
		still(name)
	}
	// The static token, even speaking for the member who started it.
	code, raw = f.landsDoOperated(t, scopedAdminToken, "oidc:clw@example.com", http.MethodPost, path, nil)
	assertLandsProblem(t, "static token revokes", code, raw, http.StatusForbidden, "forbidden")
	still("static token")

	code, _, raw = f.landsDo(t, tokScopedWriter, http.MethodPost, path, nil)
	if code != http.StatusOK {
		t.Fatalf("starter revoke: %d %s", code, raw)
	}
	if f.fc.HasToken(g.Bearer) {
		t.Error("the starter's revoke left the bearer signing in")
	}
}

// Refresh admits the agent on its own bearer, or an owner or member: never
// another agent, nor a guest holding the agent's refresh. A refused refresh
// does not spend it.
func TestLandAgents_WhoRefreshes(t *testing.T) {
	f := newLandAgentFixture(t, 5)
	code, a, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{"name": "a"})
	if code != http.StatusCreated {
		t.Fatalf("mint a: %d %s", code, raw)
	}
	code, b, raw := f.mintLandAgent(t, tokScopedWriter, map[string]any{"name": "b"})
	if code != http.StatusCreated {
		t.Fatalf("mint b: %d %s", code, raw)
	}
	path := "/claimlayer/agents/" + a.AgentID + "/refresh"
	body := map[string]any{"refresh": a.Refresh}
	for name, tok := range map[string]string{"another agent": b.Bearer, "guest": tokScopedGuest} {
		code, _, raw := f.landsDo(t, tok, http.MethodPost, path, body)
		assertLandsProblem(t, name+" refreshes", code, raw, http.StatusForbidden, "forbidden")
		if !f.fc.HasToken(a.Bearer) {
			t.Fatalf("%s: a's bearer no longer signs in", name)
		}
	}

	// The refresh was not spent: a still rotates on it.
	code, _, raw = f.landsDo(t, a.Bearer, http.MethodPost, path, body)
	if code != http.StatusOK {
		t.Fatalf("a refreshes after the refusals: %d %s", code, raw)
	}
	var g2 landAgentGrant
	_ = json.Unmarshal([]byte(raw), &g2)
	if f.fc.ResolveFleet(g2.Bearer) != agentFleetName(a.AgentID) {
		t.Errorf("a's refreshed bearer resolves to %q", f.fc.ResolveFleet(g2.Bearer))
	}
}

// A member's mint may not take a wish head's identity as its Idempotency-Key
// (gd-3HAfTiByJK): the mint creates as a machine write, and a bead holding
// the key would be returned to the bridge as the head. A key of its own mints.
func TestLandAgents_MintRefusesWishHeadKey(t *testing.T) {
	f := newLandAgentFixture(t, 5)
	mint := func(key string) (int, string) {
		b, _ := json.Marshal(map[string]any{"name": "studio"})
		req, err := http.NewRequest(http.MethodPost, f.ts.URL+"/v1/lands/v1/lands/claimlayer/agents", ioReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tokScopedWriter)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	for _, key := range []string{"commons-promote:gd-root", "slack-thread:C0A:1700000000.000100"} {
		code, raw := mint(key)
		assertLandsProblem(t, "mint keyed "+key, code, raw, http.StatusForbidden, "forbidden")
	}
	if n := f.landAgentBeads(); n != 0 {
		t.Fatalf("a refused mint wrote %d agent beads", n)
	}
	if code, raw := mint("studio-retry-1"); code != http.StatusCreated {
		t.Fatalf("mint with its own key: %d %s", code, raw)
	}
}
