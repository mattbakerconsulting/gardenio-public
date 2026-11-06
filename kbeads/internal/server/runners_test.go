package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Local-agents Task 7 (gd-zPtKJKzZhE): runner registration from an invite,
// heartbeat, get and revoke (runners.go).

func seedInvite(t *testing.T, ms *mockStore, id, code, invitee string, projects []string, expires time.Time) {
	t.Helper()
	fields, _ := json.Marshal(map[string]any{
		"code_hash":  runnerTokenHash(code),
		"invitee":    invitee,
		"projects":   projects,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
	ms.beadsMu.Lock()
	ms.beads[id] = &model.Bead{ID: id, Type: "runner_invite", Kind: model.KindData, Title: "invite", Status: model.StatusOpen, Fields: fields}
	ms.beadsMu.Unlock()
}

func registerBody(code, host string) map[string]any {
	return map[string]any{"invite_code": code, "hostname": host, "os_arch": "linux/amd64", "tags": []string{"browser"}}
}

func TestRegisterRunner_ValidInvite_MintsTokenOnce(t *testing.T) {
	_, ms, h := newTestServerNoAuth()
	seedInvite(t, ms, "gd-inv1", "GRI-TEST-0001", "oidc:owner@example.com", []string{"pet"}, time.Now().Add(time.Hour))

	rec := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-TEST-0001", "laptop"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp registerRunnerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.Token, runnerTokenPrefix) || resp.Owner != "oidc:owner@example.com" || len(resp.Projects) != 1 || resp.Projects[0] != "pet" {
		t.Fatalf("bad response: %+v", resp)
	}
	if rt := ms.runnerTokens[runnerTokenHash(resp.Token)]; rt == nil || rt.RunnerID != resp.RunnerID {
		t.Fatal("token hash row not stored for the runner")
	}
	for h, rt := range ms.runnerTokens {
		if strings.HasPrefix(h, runnerTokenPrefix) || strings.HasPrefix(rt.TokenHash, runnerTokenPrefix) {
			t.Fatal("the raw token must never be stored")
		}
	}

	ms.beadsMu.Lock()
	runner := ms.beads[resp.RunnerID]
	invite := ms.beads["gd-inv1"]
	ms.beadsMu.Unlock()
	if runner == nil || runner.Type != "runner" || runner.Owner != "oidc:owner@example.com" {
		t.Fatalf("runner bead not created as type=runner owned by the invitee: %+v", runner)
	}
	if strings.Contains(string(runner.Fields), resp.Token) {
		t.Fatal("the raw token must never be on the runner bead")
	}
	if f := beadFields(t, ms, resp.RunnerID); f["state"] != runnerStateOnline || f["hostname"] != "laptop" || f["credential_mode"] != "owner_login" {
		t.Fatalf("runner fields: %v", f)
	}
	if invite.Status != model.StatusClosed || beadFields(t, ms, "gd-inv1")["redeemed_by"] != resp.RunnerID {
		t.Fatalf("invite not redeemed and closed: status=%s fields=%s", invite.Status, invite.Fields)
	}

	// Single use: the same code again is a 404 indistinguishable from unknown.
	again := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-TEST-0001", "laptop2"))
	unknown := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-NOPE-9999", "laptop2"))
	if again.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound || again.Body.String() != unknown.Body.String() {
		t.Fatalf("redeemed and unknown codes must look identical: %d %q vs %d %q", again.Code, again.Body.String(), unknown.Code, unknown.Body.String())
	}
}

func TestRegisterRunner_ExpiredInvite404_NoProjects400(t *testing.T) {
	_, ms, h := newTestServerNoAuth()
	seedInvite(t, ms, "gd-inv-old", "GRI-OLD", "oidc:owner@example.com", []string{"pet"}, time.Now().Add(-time.Minute))
	expired := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-OLD", "l"))
	unknown := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-NOPE", "l"))
	if expired.Code != http.StatusNotFound || expired.Body.String() != unknown.Body.String() {
		t.Fatalf("expired invite must 404 like an unknown one, got %d %q", expired.Code, expired.Body.String())
	}

	seedInvite(t, ms, "gd-inv-empty", "GRI-EMPTY", "oidc:owner@example.com", nil, time.Now().Add(time.Hour))
	if rec := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-EMPTY", "l")); rec.Code != http.StatusBadRequest {
		t.Fatalf("invite without projects must 400 (D8 allowlist is required), got %d", rec.Code)
	}

	// Positive control: a live invite with projects on the same server registers.
	seedInvite(t, ms, "gd-inv-ok", "GRI-OK", "oidc:owner@example.com", []string{"pet"}, time.Now().Add(time.Hour))
	if rec := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-OK", "l")); rec.Code != http.StatusCreated {
		t.Fatalf("live invite must register, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterRunner_RateLimitedPerIP(t *testing.T) {
	_, _, h := newTestServerNoAuth()
	post := func(remote string) int {
		b, _ := json.Marshal(registerBody("GRI-X", "l"))
		req := httptest.NewRequest(http.MethodPost, "/v1/runners/register", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < runnerRegisterPerHour; i++ {
		if code := post("10.0.0.1:1234"); code != http.StatusNotFound {
			t.Fatalf("attempt %d: want 404 (unknown code), got %d", i+1, code)
		}
	}
	if code := post("10.0.0.1:1234"); code != http.StatusTooManyRequests {
		t.Fatalf("11th registration from one IP must be 429, got %d", code)
	}
	// Positive control: another address is still served.
	if code := post("10.0.0.2:1234"); code != http.StatusNotFound {
		t.Fatalf("a different IP must not be limited, got %d", code)
	}
}

func TestRegisterRunner_RequiresNoBearerButAuthIsOn(t *testing.T) {
	// With real auth configured, register is reachable with no Authorization
	// header (pre-auth exception) while a neighbouring runner route is not.
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	h := s.NewHTTPHandler(nil, withTestToken(nil, "primary-static-token"))
	seedInvite(t, ms, "gd-inv1", "GRI-AUTH", "oidc:owner@example.com", []string{"pet"}, time.Now().Add(time.Hour))

	if rec := doJSON(t, h, http.MethodPost, "/v1/runners/register", registerBody("GRI-AUTH", "laptop")); rec.Code != http.StatusCreated {
		t.Fatalf("register must not need a bearer, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodGet, "/v1/runners/gd-whatever", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/runners/{id} without a bearer must 401, got %d", rec.Code)
	}
}

func seedRunner(ms *mockStore, id, state string) {
	ms.beadsMu.Lock()
	ms.beads[id] = &model.Bead{ID: id, Type: "runner", Kind: model.KindConfig, Title: "laptop", Status: model.StatusOpen,
		Fields: []byte(`{"owner":"oidc:owner@example.com","projects":["pet"],"state":"` + state + `"}`)}
	ms.beadsMu.Unlock()
}

func TestRunnerHeartbeat_SetsOnlineThenAway(t *testing.T) {
	_, ms, h := newTestServer()
	seedRunner(ms, "gd-r1", runnerStateAway)

	rec := doJSON(t, h, http.MethodPost, "/v1/runners/gd-r1/heartbeat", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	if f := beadFields(t, ms, "gd-r1"); f["state"] != runnerStateOnline || f["last_seen_at"] == "" {
		t.Fatalf("heartbeat must set state=online and last_seen_at, got %v", f)
	}
	rec = doJSON(t, h, http.MethodPost, "/v1/runners/gd-r1/heartbeat", map[string]string{"state": "away"})
	if rec.Code != http.StatusOK || beadFields(t, ms, "gd-r1")["state"] != runnerStateAway {
		t.Fatalf("explicit away not applied: %d", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPost, "/v1/runners/gd-r1/heartbeat", map[string]string{"state": "revoked"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a heartbeat may not set state=revoked, got %d", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPost, "/v1/runners/gd-nope/heartbeat", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown runner must 404, got %d", rec.Code)
	}
}

func TestRunnerHeartbeat_RunnerMayOnlyHeartbeatItself(t *testing.T) {
	s, ms, _ := newTestServer()
	seedRunner(ms, "gd-r1", runnerStateOnline)
	seedRunner(ms, "gd-r2", runnerStateOnline)
	beat := func(runnerID, target string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/runners/"+target+"/heartbeat", nil)
		req.SetPathValue("id", target)
		req = req.WithContext(WithRunnerID(context.Background(), runnerID))
		rec := httptest.NewRecorder()
		s.handleRunnerHeartbeat(rec, req)
		return rec.Code
	}
	if code := beat("gd-r2", "gd-r1"); code != http.StatusForbidden {
		t.Fatalf("runner gd-r2 heartbeating gd-r1 must 403, got %d", code)
	}
	// Positive control: the same runner heartbeating itself succeeds.
	if code := beat("gd-r1", "gd-r1"); code != http.StatusOK {
		t.Fatalf("runner heartbeating itself must 200, got %d", code)
	}
}

func TestGetRunner(t *testing.T) {
	_, ms, h := newTestServer()
	seedRunner(ms, "gd-r1", runnerStateOnline)
	ms.beadsMu.Lock()
	ms.beads["gd-t1"] = &model.Bead{ID: "gd-t1", Type: "task", Title: "not a runner", Status: model.StatusOpen}
	ms.beadsMu.Unlock()

	rec := doJSON(t, h, http.MethodGet, "/v1/runners/gd-r1", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"gd-r1"`) {
		t.Fatalf("get runner: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodGet, "/v1/runners/gd-t1", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a non-runner bead must 404 on the runner route, got %d", rec.Code)
	}
}

func TestRunnerRevoke_RevokesTokensAndKillsAgents(t *testing.T) {
	_, ms, h := newTestServer()
	seedRunner(ms, "gd-r1", runnerStateOnline)
	ms.beadsMu.Lock()
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Title: "local-laptop", Assignee: "local-laptop", Status: model.StatusOpen, Fields: []byte(`{"runtime_id":"gd-r1","execution_target":"local"}`)}
	ms.beads["gd-a2"] = &model.Bead{ID: "gd-a2", Type: "agent", Title: "crew-cloud", Assignee: "crew-cloud", Status: model.StatusOpen, Fields: []byte(`{}`)}
	ms.beadsMu.Unlock()
	ms.runnerTokens = map[string]*model.RunnerToken{
		"h1": {TokenHash: "h1", RunnerID: "gd-r1", CreatedAt: time.Now()},
		"h2": {TokenHash: "h2", RunnerID: "gd-r2", CreatedAt: time.Now()},
	}

	rec := doJSON(t, h, http.MethodPost, "/v1/runners/gd-r1/revoke", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	var resp runnerRevokeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RevokedTokens != 1 || resp.AgentsKilled != 1 {
		t.Fatalf("want 1 token revoked and 1 agent killed, got %+v", resp)
	}
	if ms.runnerTokens["h1"].RevokedAt == nil {
		t.Fatal("token row not revoked")
	}
	if ms.runnerTokens["h2"].RevokedAt != nil {
		t.Fatal("another runner's token must not be revoked")
	}
	if beadFields(t, ms, "gd-r1")["state"] != runnerStateRevoked {
		t.Fatal("runner state not revoked")
	}

	killMailTo := map[string]bool{}
	ms.beadsMu.Lock()
	for id, b := range ms.beads {
		if b.Type != "mail" {
			continue
		}
		labels := append(append([]string{}, b.Labels...), ms.labels[id]...)
		for _, l := range labels {
			if l == "control:kill" {
				killMailTo[b.Assignee] = true
			}
		}
	}
	ms.beadsMu.Unlock()
	if !killMailTo["local-laptop"] {
		t.Fatal("expected a control:kill mail to the runner's agent")
	}
	if killMailTo["crew-cloud"] {
		t.Fatal("an agent not on the runner must not get a kill mail")
	}

	if rec := doJSON(t, h, http.MethodPost, "/v1/runners/gd-r1/heartbeat", nil); rec.Code != http.StatusConflict {
		t.Fatalf("heartbeat on a revoked runner must 409, got %d", rec.Code)
	}
}

func TestRunnerTypes_AdminOnlyThroughGenericAPI(t *testing.T) {
	// type=runner and type=runner_invite are admin-gated: a static-token caller
	// (every agent pod) cannot mint an invite or forge a runner through
	// POST /v1/beads, while the register route's own writes go through.
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	h := s.NewHTTPHandler(nil, withTestToken(nil, "primary-static-token"))
	post := func(body map[string]any) int {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/beads", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer primary-static-token")
		req.Header.Set("X-Operated-By", "oidc:someone@example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	invite := map[string]any{"title": "invite", "type": "runner_invite", "kind": "data", "fields": map[string]any{
		"code_hash": runnerTokenHash("GRI-FORGED"), "invitee": "oidc:attacker@example.com", "projects": []string{"pet"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	if code := post(invite); code != http.StatusForbidden {
		t.Fatalf("static-token create of a runner_invite must 403, got %d", code)
	}
	runner := map[string]any{"title": "laptop", "type": "runner", "kind": "config", "fields": map[string]any{
		"owner": "oidc:victim@example.com", "hostname": "laptop", "projects": []string{"pet"},
	}}
	if code := post(runner); code != http.StatusForbidden {
		t.Fatalf("static-token create of a runner must 403, got %d", code)
	}
	// Positive control: the same static token may create an ordinary bead.
	if code := post(map[string]any{"title": "plain", "type": "task", "kind": "issue", "short_title": "plain"}); code != http.StatusCreated {
		t.Fatalf("static-token create of a task must 201, got %d", code)
	}
}

func TestMintRunnerToken(t *testing.T) {
	tok, hash, err := mintRunnerToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, runnerTokenPrefix) || len(tok) != len(runnerTokenPrefix)+43 {
		t.Fatalf("token shape: %q", tok)
	}
	if hash != runnerTokenHash(tok) || len(hash) != 64 {
		t.Fatalf("hash must be hex sha256 of the token: %q", hash)
	}
	other, _, _ := mintRunnerToken()
	if other == tok {
		t.Fatal("two mints returned the same token")
	}
}

// beadFields decodes a mock bead's fields map (string values only).
func beadFields(t *testing.T, ms *mockStore, id string) map[string]string {
	t.Helper()
	ms.beadsMu.Lock()
	defer ms.beadsMu.Unlock()
	var raw map[string]any
	_ = json.Unmarshal(ms.beads[id].Fields, &raw)
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// revokeAs calls the revoke handler on gd-r1 as the caller ctx describes.
func revokeAs(s *BeadsServer, ctx context.Context) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/runners/gd-r1/revoke", nil).WithContext(ctx)
	req.SetPathValue("id", "gd-r1")
	rec := httptest.NewRecorder()
	s.handleRevokeRunner(rec, req)
	return rec
}

func seedRunnerWithAgent(ms *mockStore) {
	seedRunner(ms, "gd-r1", runnerStateOnline)
	ms.beadsMu.Lock()
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Title: "local-laptop", Assignee: "local-laptop", Status: model.StatusOpen, Fields: []byte(`{"runtime_id":"gd-r1"}`)}
	ms.beadsMu.Unlock()
	ms.runnerTokens = map[string]*model.RunnerToken{"h1": {TokenHash: "h1", RunnerID: "gd-r1", CreatedAt: time.Now()}}
}

// gd-FebppYflAV: requireRole(RoleOperator) lets every service token through,
// so revoke checks the caller itself. A non-owner, non-admin person and a
// per-agent token are refused and nothing is revoked.
func TestRunnerRevoke_RefusesNonOwnerAndAgentTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"operator who is not the owner", WithRoles(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "oidc", Subject: "mallory@example.com"}, ""), authMethodOIDC), []string{RoleOperator})},
		{"v2 envelope for another person", WithRoles(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "edge", Subject: "oidc:mallory@example.com"}, ""), authMethodOIDC), []string{RoleOperator})},
		{"per-agent fleet token", withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-attacker"}, "agents"), authMethodStatic)},
		{"machine principal", withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "machine", Subject: "machine:x"}, ""), authMethodMachine)},
		// The retired shared static token (gd-ke6PC7bRto) no longer revokes:
		// no request can carry this identity, and if one did it would be refused.
		{"retired static identity", WithOperatedByHeader(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "static", Subject: "static"}, ""), authMethodStatic), "oidc:owner@example.com")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			s.controlMailFleets = map[string]bool{"slack-bridge": true}
			seedRunnerWithAgent(ms)

			if rec := revokeAs(s, tc.ctx); rec.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
			}
			if ms.runnerTokens["h1"].RevokedAt != nil || beadFields(t, ms, "gd-r1")["state"] == runnerStateRevoked {
				t.Fatal("a refused revoke must change nothing")
			}
		})
	}
}

// The owner, an admin and the bridge fleet may revoke (the edge case carries
// the operated-by stamp its middleware sets on a real request), and each also passes the control: mail guard, so the agent on the
// runner gets its kill mail (gd-qoiurW3S7m).
func TestRunnerRevoke_OwnerAdminAndBridgeKillAgents(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"owner", WithRoles(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "oidc", Subject: "Owner@example.com"}, ""), authMethodOIDC), []string{RoleOperator})},
		{"owner via v2 envelope", WithOperatedByHeader(WithRoles(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "edge", Subject: "owner@example.com"}, ""), authMethodOIDC), []string{RoleOperator}), "oidc:owner@example.com")},
		{"admin", WithRoles(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "oidc", Subject: "admin@example.com"}, ""), authMethodOIDC), []string{RoleAdmin})},
		{"slack bridge fleet", withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "slack-bridge"}, "slack-bridge"), authMethodStatic)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			s.controlMailFleets = map[string]bool{"slack-bridge": true}
			seedRunnerWithAgent(ms)

			rec := revokeAs(s, tc.ctx)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var resp runnerRevokeResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp.RevokedTokens != 1 || resp.AgentsKilled != 1 {
				t.Fatalf("want 1 token revoked and 1 agent killed, got %+v", resp)
			}
		})
	}
}

// gd-qoiurW3S7m: a daemon with auth disabled (identity none) admits every
// revoke, but the control: mail guard refuses identity none. The kill mails are
// the daemon's once the revoke is authorized, so the agent is still killed.
func TestRunnerRevoke_KillMailPassesGuardForNonRoleBearingRevoker(t *testing.T) {
	s, ms, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}
	seedRunnerWithAgent(ms)
	// As AuthMiddleware stamps a no-auth request carrying X-Operated-By.
	ctx := WithOperatedByHeader(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "none"}, ""), authMethodNone), "oidc:owner@example.com")
	kill := &model.Bead{Type: "mail", Assignee: "local-laptop", Labels: []string{"control:kill"}}
	if err := s.applyMailControlGuard(ctx, kill); err == nil {
		t.Fatal("precondition: the guard must refuse this caller's own control: mail")
	}

	rec := revokeAs(s, ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp runnerRevokeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RevokedTokens != 1 || resp.AgentsKilled != 1 {
		t.Fatalf("want 1 token revoked and 1 agent killed, got %+v", resp)
	}
	found := false
	ms.beadsMu.Lock()
	for id, b := range ms.beads {
		if b.Type != "mail" || b.Assignee != "local-laptop" {
			continue
		}
		for _, l := range append(append([]string{}, b.Labels...), ms.labels[id]...) {
			found = found || l == "control:kill"
		}
	}
	ms.beadsMu.Unlock()
	if !found {
		t.Fatal("expected a control:kill mail to the runner's agent")
	}
}
