package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/events"
)

// registerLandPrincipal registers token as the land principal for land, the
// way POST /v1/bridge-tokens does for {"bridge":"land-<land>","project":<land>}.
func registerLandPrincipal(fc *FleetConfig, token, land string) {
	fc.RegisterTokenFull(token, landFleetPrefix+land, land, nil, nil, time.Time{}, "", false)
}

// GET /v1/me and /v1/whoami: one land at tierLand, a token-sourced
// land:<land> identity, never a fleet.
func TestLandPrincipal_MeAndWhoami(t *testing.T) {
	f := newScopedFixture(t)

	code, body := f.do(t, tokLandClaimlayer, http.MethodGet, "/v1/me", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/me: %d %s", code, body)
	}
	var me meJSON
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatal(err)
	}
	if me.Unrestricted || len(me.Lands) != 1 || me.Lands[0] != (meLand{Name: "claimlayer", Tier: tierLand}) {
		t.Errorf("me = %+v, want restricted to claimlayer at tier land", me)
	}
	if me.OperatorID != "land:claimlayer" || me.Source != whoamiSourceToken {
		t.Errorf("me identity = %q/%q, want land:claimlayer/%s", me.OperatorID, me.Source, whoamiSourceToken)
	}
}

// The land principal's context: a scoped principal on its one land, no fleet,
// no admin, no human identity.
func TestLandPrincipal_Context(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	ctx := landPrincipalContext(context.Background(), fc, "claimlayer")
	if m := authMethodFromContext(ctx); m != authMethodLand {
		t.Fatalf("auth method = %q, want %q: a land principal is not an OIDC human", m, authMethodLand)
	}
	if !isScopedPrincipal(ctx) {
		t.Fatal("land principal is not a scoped principal")
	}
	d := scopeFromContext(ctx)
	if d.Unrestricted || len(d.Projects) != 1 || d.Projects[0] != "claimlayer" || d.Tiers["claimlayer"] != tierLand {
		t.Errorf("scope = %+v, want claimlayer at tier land", d)
	}
	id := AuthIdentityFromContext(ctx)
	if id.Type != "land" || id.Subject != "land:claimlayer" || id.Privileged {
		t.Errorf("identity = %+v, want type land, subject land:claimlayer", id)
	}
	if _, isAgent := agentBeadIDFromContext(ctx); isAgent {
		t.Error("land principal resolves as an agent")
	}
	if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
		t.Error("land principal holds admin")
	}
	if got := FleetFromContext(ctx); got != "gardenio" {
		t.Errorf("fleet = %q, want the default fleet", got)
	}
	if got := TokenProjectFromContext(ctx); got != "" {
		t.Errorf("token project = %q, want none (no fleet binding)", got)
	}
}

// A land-* registration that is not bound to its own land, or is privileged,
// is refused outright: it never falls back to being served as a fleet token.
func TestLandPrincipal_MisboundTokenRefused(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("unbound", "land-claimlayer", "", nil, nil, time.Time{}, "", false)
	fc.RegisterTokenFull("other-land", "land-claimlayer", "gardenio", nil, nil, time.Time{}, "", false)
	fc.RegisterTokenFull("privileged", "land-claimlayer", "claimlayer", nil, nil, time.Time{}, "", true)
	registerLandPrincipal(fc, "good", "claimlayer")
	srv.SetFleetAuth(fc)
	srv.SetRateLimit(RateLimitConfig{})
	ts := httptest.NewServer(srv.NewHTTPHandler(nil, withTestToken(fc, scopedAdminToken)))
	t.Cleanup(ts.Close)
	f := &scopedFixture{ts: ts, ms: ms}

	for _, tok := range []string{"unbound", "other-land", "privileged"} {
		if code, body := f.do(t, tok, http.MethodGet, "/v1/beads", nil); code != http.StatusUnauthorized {
			t.Errorf("%s: GET /v1/beads = %d %s, want 401", tok, code, body)
		}
	}
	if code, body := f.do(t, "good", http.MethodGet, "/v1/me", nil); code != http.StatusOK || !strings.Contains(body, `"tier":"land"`) {
		t.Errorf("good: GET /v1/me = %d %s, want 200 at tier land", code, body)
	}
}

// Land principals are HTTP-only: gRPC refuses them instead of serving them as
// a fleet token.
func TestLandPrincipal_GRPCRefused(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	registerLandPrincipal(fc, "good", "claimlayer")
	fc.RegisterTokenFull("unbound", "land-claimlayer", "", nil, nil, time.Time{}, "", false)
	handler := func(ctx context.Context, req any) (any, error) {
		t.Error("handler reached")
		return nil, nil
	}
	for tok, want := range map[string]codes.Code{"good": codes.PermissionDenied, "unbound": codes.Unauthenticated} {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
		_, err := AuthInterceptor(withTestToken(fc, "secret"), nil)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
		if status.Code(err) != want {
			t.Errorf("%s: gRPC err = %v, want %s", tok, err, want)
		}
	}
}

// No request header widens or re-attributes a land principal.
func TestLandPrincipal_HeadersIgnored(t *testing.T) {
	f := newScopedFixture(t)
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+"/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokLandClaimlayer)
	req.Header.Set("X-Actor", "someone-else")
	req.Header.Set("X-Operated-By", "oidc:boss@example.com")
	req.Header.Set("X-Fleet", "agent-x")
	req.Header.Set("X-Auth-Request-Email", "boss@example.com")
	req.Header.Set("X-Auth-Request-Groups", "/project-gardenio")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who whoamiJSON
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	if who.OperatorID != "land:claimlayer" {
		t.Errorf("whoami = %+v, want land:claimlayer", who)
	}
	if ids := f.listIDsWithHeader(t, tokLandClaimlayer, "X-Auth-Request-Groups", "/project-gardenio"); ids[beadGardenio] || !ids[beadClaimlayer] {
		t.Errorf("list with a groups header = %v, want claimlayer only", ids)
	}
}

func (f *scopedFixture) listIDsWithHeader(t *testing.T, token, header, value string) map[string]bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+"/v1/beads?limit=100", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(header, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Beads []struct{ ID string } `json:"beads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, b := range body.Beads {
		ids[b.ID] = true
	}
	return ids
}

// A land principal is no human: it gets no user bead, cannot approve an
// escalation, answer a decision or spawn, even in its own land.
func TestLandPrincipal_NoHumanActs(t *testing.T) {
	f := newScopedFixture(t)

	f.seedEscalation("esc-own", "project:claimlayer", "x", map[string]any{"a": "b"}, nil)
	code, body := f.approve(t, tokLandClaimlayer, "esc-own")
	f.wantStatus(t, "approve own-land escalation", code, body, http.StatusForbidden)

	f.seedTyped("dec-own", "decision", "project:claimlayer")
	code, body = f.do(t, tokLandClaimlayer, http.MethodPost, "/v1/decisions/dec-own/resolve", map[string]any{"selected_option": "a"})
	f.wantStatus(t, "resolve own-land decision", code, body, http.StatusForbidden)

	code, body = f.do(t, tokLandClaimlayer, http.MethodPost, "/v1/beads", map[string]any{
		"title": "spawned", "type": "agent", "labels": []string{"project:claimlayer", "role:crew"},
		"fields": map[string]any{"project": "claimlayer", "role": "crew"},
	})
	f.wantStatus(t, "spawn in own land", code, body, http.StatusForbidden)

	audited := false
	for _, b := range f.ms.beads {
		if b.Type == "user" && strings.Contains(b.Title, "land:") {
			t.Errorf("land principal provisioned a user bead: %+v", b)
		}
		if b.Type == "audit" && strings.Contains(string(b.Fields), `"actor":"land:claimlayer"`) {
			audited = true
		}
	}
	// The denials are audited under the land principal's own identity.
	if !audited {
		t.Error("no audit bead records a land:claimlayer denial")
	}
}

// The mint refuses a land-* registration that would not resolve as a land
// principal, and accepts one that does.
func TestLandPrincipal_MintValidation(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	srv, _ := newTestServerWithFleetAuth(t, "admin-secret", fc)
	// Only the token-mint machine principal may mint (gd-ke6PC7bRto), so the
	// handler is driven directly as that caller.
	mint := func(body map[string]any) (int, string) {
		raw, _ := json.Marshal(body)
		req := asTokenMinter(httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", strings.NewReader(string(raw))))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleRegisterBridgeToken(rec, req)
		return rec.Code, rec.Body.String()
	}
	for name, body := range map[string]map[string]any{
		"no project":      {"token": "t1", "bridge": "land-claimlayer"},
		"other project":   {"token": "t2", "bridge": "land-claimlayer", "project": "gardenio"},
		"subject":         {"token": "t3", "bridge": "land-claimlayer", "project": "claimlayer", "subject": "landgate"},
		"type scope":      {"token": "t4", "bridge": "land-claimlayer", "project": "claimlayer", "allowed_types": []string{"task"}},
		"no land":         {"token": "t5", "bridge": "land-"},
		"land with slash": {"token": "t6", "bridge": "land-a/owners", "project": "a/owners"},
	} {
		if code, resp := mint(body); code != http.StatusBadRequest {
			t.Errorf("%s: mint = %d %s, want 400", name, code, resp)
		}
	}
	if code, resp := mint(map[string]any{"token": "ok", "bridge": "land-claimlayer", "project": "claimlayer", "ttl": "1h"}); code != http.StatusCreated {
		t.Fatalf("valid land mint = %d %s, want 201", code, resp)
	}
	if land, ok, err := landPrincipalForToken(fc, "ok"); err != nil || !ok || land != "claimlayer" {
		t.Errorf("minted token resolves as (%q, %v, %v), want a claimlayer land principal", land, ok, err)
	}
}
