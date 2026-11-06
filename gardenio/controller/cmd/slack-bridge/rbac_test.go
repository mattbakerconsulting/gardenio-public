package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/oidcauth"
)

// newTestHandler returns a bridgeRBAC handler whose `next` records the path.
func newTestHandler(oidcMW func(http.Handler) http.Handler, agentToken string, oidcConfigured, insecureOpenAPI bool) (http.Handler, *bool) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	return bridgeRBAC(next, oidcMW, agentToken, nil, oidcConfigured, insecureOpenAPI, slog.New(slog.NewTextHandler(io.Discard, nil))), &reached
}

func TestBridgeRBAC_AgentEndpoints_RequireToken(t *testing.T) {
	// OIDC configured + agent token set: bearer token grants access.
	oidcMW := oidcauth.Middleware(context.Background(), oidcauth.Config{}, slog.Default()) // no-op, not configured
	h, reached := newTestHandler(oidcMW, "secret-token", true, false)

	paths := []string{"/api/slack/threads", "/api/permission/callback"}
	for _, p := range paths {
		t.Run("allow_with_bearer_"+p, func(t *testing.T) {
			*reached = false
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("Authorization", "Bearer secret-token")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 with valid bearer, got %d: %s", rr.Code, rr.Body.String())
			}
			if !*reached {
				t.Fatal("expected next handler to be invoked")
			}
		})

		t.Run("deny_without_bearer_"+p, func(t *testing.T) {
			*reached = false
			req := httptest.NewRequest(http.MethodGet, p, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without bearer, got %d: %s", rr.Code, rr.Body.String())
			}
			if *reached {
				t.Fatal("handler must not be invoked without bearer")
			}
		})

		t.Run("deny_wrong_bearer_"+p, func(t *testing.T) {
			*reached = false
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("Authorization", "Bearer wrong")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with wrong bearer, got %d", rr.Code)
			}
			if *reached {
				t.Fatal("handler must not be invoked with wrong bearer")
			}
		})
	}
}

func TestBridgeRBAC_AgentEndpoints_DenyWhenNoTokenAndOIDCConfigured(t *testing.T) {
	// Agent token unset + OIDC configured: agent endpoints require admin role.
	// Unauthenticated caller must be rejected (no fall-open).
	oidcMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate OIDC configured but no token on the request → 401.
			http.Error(w, "missing authorization token", http.StatusUnauthorized)
		})
	}
	h, reached := newTestHandler(oidcMW, "", true, false)

	req := httptest.NewRequest(http.MethodPost, "/api/permission/callback", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (fail-closed), got %d: %s", rr.Code, rr.Body.String())
	}
	if *reached {
		t.Fatal("handler must not be invoked without auth")
	}
}

// TestPermissionCallback_RejectsUnsignedRequest is the AUDIT-2026-04-18 §4.2
// regression for N3/C2 (ka-0000006036). An unauthenticated POST to
// /api/permission/callback must never reach the handler: the pre-fix path
// let anyone inject a forged permission_request with an attacker-controlled
// channel_url, leading to SSRF when an operator clicked Allow.
//
// The fix put the auth check at the bridgeRBAC layer (bearer
// SLACK_BRIDGE_AGENT_TOKEN) rather than HMAC on the handler (per §N3 fix
// item 1). This test locks in that a request without the bearer token —
// when the bridge is deployed with either OIDC or the agent token
// configured (i.e. not the insecure local-dev fallback) — is rejected 401
// and the callback handler is never invoked.
func TestPermissionCallback_RejectsUnsignedRequest(t *testing.T) {
	// Agent token configured on the server side; request arrives without one.
	oidcMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate OIDC configured but no token on the request → 401.
			http.Error(w, "missing authorization token", http.StatusUnauthorized)
		})
	}
	h, reached := newTestHandler(oidcMW, "agent-secret", true, false)

	body := `{"type":"permission_request","request_id":"PWN01","tool_name":"Bash",` +
		`"agent":"victim","channel_url":"http://attacker.invalid"}`
	req := httptest.NewRequest(http.MethodPost, "/api/permission/callback",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization / X-Auth-Request-Access-Token header.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unsigned callback, got %d: %s", rr.Code, rr.Body.String())
	}
	if *reached {
		t.Fatal("permission callback handler must not be invoked without auth — SSRF gate bypassed")
	}
}

func TestBridgeRBAC_AgentEndpoints_AllowInsecureDev(t *testing.T) {
	// No OIDC, no agent token, explicit insecure flag — local dev fallback still allows.
	oidcMW := func(next http.Handler) http.Handler { return next }
	h, reached := newTestHandler(oidcMW, "", false, true)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 in local-dev fallback, got %d", rr.Code)
	}
	if !*reached {
		t.Fatal("expected passthrough in dev fallback")
	}
}

func TestBridgeRBAC_RegularRoutes_NoFallOpen(t *testing.T) {
	// OIDC configured, token unlocks middleware but claims have no roles.
	// The pre-fix code allowed this through ("len(roles)>0 && !HasMinimumRole").
	// The fix must deny.
	oidcMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Inject empty role slice into ctx via the production key by
			// using oidcauth.Middleware wrapping — we mimic it here by
			// routing through a handler that sees zero roles.
			next.ServeHTTP(w, r)
		})
	}
	h, reached := newTestHandler(oidcMW, "", true, false)

	req := httptest.NewRequest(http.MethodGet, "/api/decisions", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on no-roles (pre-fix bypass), got %d: %s", rr.Code, rr.Body.String())
	}
	if *reached {
		t.Fatal("handler must not be invoked when no roles present under OIDC")
	}
}

func TestBridgeRBAC_NonAPI_Passthrough(t *testing.T) {
	// Non-/api/ paths bypass auth entirely (health, slack webhooks).
	oidcMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("oidcMW must not run for non-/api/ path")
		})
	}
	h, reached := newTestHandler(oidcMW, "secret", true, false)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !*reached {
		t.Fatalf("expected passthrough, got %d reached=%v", rr.Code, *reached)
	}
}

func TestBridgeRouteRole(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/api/slack/threads", bridgeRoleAgent},
		{http.MethodPost, "/api/slack/messages/delete", bridgeRoleAgent},
		{http.MethodPost, "/api/permission/callback", bridgeRoleAgent},
		{http.MethodGet, "/api/decisions", oidcauth.RoleViewer},
		{http.MethodPost, "/api/decisions/abc", oidcauth.RoleOperator},
		{http.MethodGet, "/api/unknown", oidcauth.RoleViewer},
		{http.MethodPost, "/api/conversation/inbound", bridgeRolePerson},
		{http.MethodPost, "/api/conversation/say", bridgeRoleAgent},
		{http.MethodGet, "/api/conversation/thread", bridgeRoleAgent},
		{http.MethodPost, "/api/conversation/sayx", oidcauth.RoleViewer},
	}
	for _, c := range cases {
		got := bridgeRouteRole(c.method, c.path)
		if got != c.want {
			t.Errorf("bridgeRouteRole(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestBearerTokenMatches(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if bearerTokenMatches(req, "x") {
		t.Fatal("empty request must not match")
	}
	req.Header.Set("Authorization", "Bearer x")
	if !bearerTokenMatches(req, "x") {
		t.Fatal("authorization header must match")
	}
	if bearerTokenMatches(req, "y") {
		t.Fatal("must not match different token")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("X-Auth-Request-Access-Token", "z")
	if !bearerTokenMatches(req2, "z") {
		t.Fatal("X-Auth-Request-Access-Token must match")
	}
	if bearerTokenMatches(req2, "") {
		t.Fatal("empty expected must not match anything")
	}
}

// P7 Task 4: with no OIDC and no explicit insecure flag, regular /api/* routes
// fail closed, even with a forged X-Auth-Request-Email. Positive control: the
// agent-facing route with the agent token still works, and the explicit dev
// flag restores the old open behaviour.
func TestBridgeRBAC_NoOIDC_FailsClosed(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	noop := func(h http.Handler) http.Handler { return h }
	h := bridgeRBAC(ok, noop, "agent-secret", nil, false, false, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/api/escalations/gd-x/approve", nil)
	req.Header.Set("X-Auth-Request-Email", "owner@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forged-header approve without OIDC: %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("positive control: agent route with agent token: %d, want 200", rec.Code)
	}

	dev := bridgeRBAC(ok, noop, "", nil, false, true, slog.Default())
	req = httptest.NewRequest(http.MethodGet, "/api/escalations", nil)
	rec = httptest.NewRecorder()
	dev.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("positive control: explicit insecure dev mode: %d, want 200", rec.Code)
	}
}

// fakeAgentTokens verifies tokens by value.
type fakeAgentTokens map[string]error

func (f fakeAgentTokens) Verify(_ context.Context, tok string) (bridge.AgentCaller, error) {
	err, ok := f[tok]
	if !ok {
		return bridge.AgentCaller{}, bridge.ErrAgentTokenRejected
	}
	if err != nil {
		return bridge.AgentCaller{}, err
	}
	return bridge.AgentCaller{Agent: "steward", BeadID: "gd-steward1", Land: "acre"}, nil
}

// gd-P5wjHDweih: the conversation routes admit an agent's own daemon token
// with no bridge bearer (a tenant pod holds none) and hand the route the
// token's agent. A token the daemon refuses is refused whatever bearer rides
// with it; a token that names no agent falls through to the bearer; the
// header is ignored on every other agent route.
func TestBridgeRBAC_AgentTokenOnConversationRoutes(t *testing.T) {
	var got *bridge.AgentCaller
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		if c, ok := bridge.AgentCallerFromContext(r.Context()); ok {
			got = &c
		}
		w.WriteHeader(http.StatusOK)
	})
	noop := func(h http.Handler) http.Handler { return h }
	tokens := fakeAgentTokens{"agent-tok": nil, "static-tok": bridge.ErrNotAgentToken, "flaky-tok": errors.New("daemon down")}
	h := bridgeRBAC(next, noop, "shared-secret", tokens, true, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	do := func(method, path, agentTok, bearer string) int {
		got = nil
		req := httptest.NewRequest(method, path, nil)
		if agentTok != "" {
			req.Header.Set(bridge.AgentTokenHeader, agentTok)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/api/conversation/say"},
		{http.MethodGet, "/api/conversation/thread"},
		{http.MethodGet, "/api/slack/threads"},
		{http.MethodPost, "/api/permission/callback"},
	} {
		if code := do(rt.method, rt.path, "agent-tok", ""); code != http.StatusOK || got == nil || got.Agent != "steward" || got.Land != "acre" {
			t.Errorf("%s %s, agent token alone: %d caller %+v, want 200 as steward of acre", rt.method, rt.path, code, got)
		}
		if code := do(rt.method, rt.path, "agent-tok", "shared-secret"); code != http.StatusOK || got == nil {
			t.Errorf("%s %s, agent token and bearer: %d caller %+v, want the token's caller", rt.method, rt.path, code, got)
		}
		if code := do(rt.method, rt.path, "revoked-tok", "shared-secret"); code != http.StatusUnauthorized {
			t.Errorf("%s %s, refused token with a bearer: %d, want 401", rt.method, rt.path, code)
		}
		if code := do(rt.method, rt.path, "flaky-tok", "shared-secret"); code != http.StatusServiceUnavailable {
			t.Errorf("%s %s, unverifiable token: %d, want 503", rt.method, rt.path, code)
		}
		if code := do(rt.method, rt.path, "static-tok", "shared-secret"); code != http.StatusOK || got != nil {
			t.Errorf("%s %s, non-agent token with a bearer: %d caller %+v, want 200 with no caller", rt.method, rt.path, code, got)
		}
		if code := do(rt.method, rt.path, "static-tok", ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s, non-agent token alone: %d, want 401", rt.method, rt.path, code)
		}
		if code := do(rt.method, rt.path, "", "shared-secret"); code != http.StatusOK || got != nil {
			t.Errorf("%s %s, bearer alone: %d caller %+v, want 200 with no caller", rt.method, rt.path, code, got)
		}
	}
	if code := do(http.MethodGet, "/api/slack/channels", "agent-tok", ""); code != http.StatusUnauthorized {
		t.Errorf("agent token on another agent route: %d, want 401", code)
	}
	if code := do(http.MethodPost, "/api/slack/threads/reply", "agent-tok", ""); code != http.StatusUnauthorized {
		t.Errorf("agent token on the legacy reply route: %d, want 401", code)
	}
}

// TestBridgeRBAC_InboundAdmitsAnyPerson checks the Commons inbound call admits
// a project member, who holds no rank in the gardenio hierarchy (the handler
// gates them by land role), and still refuses a caller with no identity.
func TestBridgeRBAC_InboundAdmitsAnyPerson(t *testing.T) {
	for _, c := range []struct {
		name  string
		email string
		roles []string
		want  int
	}{
		{"project member", "mo@example.com", []string{oidcauth.RoleProjectMember}, http.StatusOK},
		{"admin", "op@example.com", []string{oidcauth.RoleAdmin}, http.StatusOK},
		{"roles but no email", "", []string{oidcauth.RoleProjectMember}, http.StatusForbidden},
		{"no roles", "mo@example.com", nil, http.StatusForbidden},
	} {
		oidcMW := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := oidcauth.ContextWithRoles(r.Context(), c.roles)
				if c.email != "" {
					ctx = oidcauth.ContextWithEmail(ctx, c.email)
				}
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
		h, reached := newTestHandler(oidcMW, "", true, false)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/conversation/inbound", nil))
		if rr.Code != c.want || *reached != (c.want == http.StatusOK) {
			t.Errorf("%s: %d (reached %v), want %d", c.name, rr.Code, *reached, c.want)
		}
	}
}

// TestBridgeRBAC_OIDCCommonsOnly checks SLACK_BRIDGE_OIDC_COMMONS_ONLY: an
// admin bearer the OIDC middleware would verify reaches the Commons inbound
// route and nothing else. The role-gated web routes and the agent routes'
// OIDC-admin fallback refuse it, while an agent route's own bearer still works.
func TestBridgeRBAC_OIDCCommonsOnly(t *testing.T) {
	verifying := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := oidcauth.ContextWithRoles(r.Context(), []string{oidcauth.RoleAdmin})
			ctx = oidcauth.ContextWithEmail(ctx, "op@example.com")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	for _, c := range []struct {
		method, path, bearer string
		want                 int
	}{
		{http.MethodPost, "/api/conversation/inbound", "", http.StatusOK},
		{http.MethodGet, "/api/conversation/inbound", "", http.StatusForbidden},
		{http.MethodPost, "/api/conversation/inbound/x", "", http.StatusForbidden},
		{http.MethodGet, "/api/decisions", "", http.StatusForbidden},
		{http.MethodPost, "/api/decisions/gd-x/resolve", "", http.StatusForbidden},
		{http.MethodPost, "/api/escalations/gd-x/approve", "", http.StatusForbidden},
		{http.MethodGet, "/api/jira/issues", "", http.StatusForbidden},
		{http.MethodGet, "/api/slack/channels", "", http.StatusForbidden},
		{http.MethodPost, "/api/permission/callback", "", http.StatusForbidden},
		{http.MethodGet, "/api/slack/channels", "agent-secret", http.StatusOK},
	} {
		h, reached := newTestHandler(oidcOnlyOnCommonsInbound(verifying), "agent-secret", true, false)
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != c.want || *reached != (c.want == http.StatusOK) {
			t.Errorf("%s %s: %d (reached %v), want %d", c.method, c.path, rr.Code, *reached, c.want)
		}
	}

	// Mutation control: unscoped, the same admin bearer opens a web route.
	h, reached := newTestHandler(verifying, "agent-secret", true, false)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/decisions", nil))
	if rr.Code != http.StatusOK || !*reached {
		t.Fatalf("unscoped control: GET /api/decisions %d, want 200", rr.Code)
	}
}

// gd-pXik57wZA9: the land listener is the only bridge port a tenant pod
// reaches. It serves the agent-token routes to a verified per-agent token and
// nothing else: not the shared bearer, not the UI, not the decision streams.
func TestLandListener_OnlyAgentTokenRoutes(t *testing.T) {
	var got *bridge.AgentCaller
	var sawAuth string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		if c, ok := bridge.AgentCallerFromContext(r.Context()); ok {
			got = &c
		}
		sawAuth = r.Header.Get("Authorization") + r.Header.Get("X-Auth-Request-Email")
		w.WriteHeader(http.StatusOK)
	})
	tokens := fakeAgentTokens{"agent-tok": nil, "static-tok": bridge.ErrNotAgentToken, "flaky-tok": errors.New("daemon down")}
	h := landListener(next, tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	do := func(method, path, agentTok, bearer string) int {
		got, sawAuth = nil, ""
		req := httptest.NewRequest(method, path, nil)
		if agentTok != "" {
			req.Header.Set(bridge.AgentTokenHeader, agentTok)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("X-Auth-Request-Email", "op@example.com")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/api/conversation/say"},
		{http.MethodGet, "/api/conversation/thread"},
		{http.MethodGet, "/api/slack/threads"},
		{http.MethodPost, "/api/permission/callback"},
	} {
		if code := do(rt.method, rt.path, "agent-tok", "shared-secret"); code != http.StatusOK || got == nil || got.Land != "acre" || sawAuth != "" {
			t.Errorf("%s %s, agent token: %d caller %+v auth %q, want 200 as the token's agent with no other credential", rt.method, rt.path, code, got, sawAuth)
		}
		for _, c := range []struct {
			name, tok, bearer string
			want              int
		}{
			{"bearer alone", "", "shared-secret", http.StatusUnauthorized},
			{"refused token", "revoked-tok", "", http.StatusUnauthorized},
			{"non-agent token with a bearer", "static-tok", "shared-secret", http.StatusUnauthorized},
			{"unverifiable token", "flaky-tok", "", http.StatusServiceUnavailable},
		} {
			if code := do(rt.method, rt.path, c.tok, c.bearer); code != c.want {
				t.Errorf("%s %s, %s: %d, want %d", rt.method, rt.path, c.name, code, c.want)
			}
		}
	}
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/conversation/say"},
		{http.MethodPost, "/api/conversation/inbound"},
		{http.MethodGet, "/api/decisions/events"},
		{http.MethodGet, "/api/escalations/events"},
		{http.MethodGet, "/api/unreleased"},
		{http.MethodGet, "/api/slack/channels"},
		{http.MethodPost, "/api/slack/threads/reply"},
		{http.MethodGet, "/ui/"},
		{http.MethodGet, "/slack/link/start"},
		{http.MethodPost, "/slack/interactions"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/"},
	} {
		if code := do(rt.method, rt.path, "agent-tok", "shared-secret"); code != http.StatusNotFound {
			t.Errorf("%s %s on the land listener: %d, want 404", rt.method, rt.path, code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/conversation/say", nil)
	req.Header.Set(bridge.AgentTokenHeader, "agent-tok")
	rr := httptest.NewRecorder()
	landListener(next, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no verifier: %d, want 401", rr.Code)
	}
}
