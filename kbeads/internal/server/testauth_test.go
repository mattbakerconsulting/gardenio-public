package server

import (
	"context"
	"net/http"
	"time"
)

// testBearerSubject is the fleet subject withTestToken registers. It is not
// "static": the shared static token is gone (gd-ke6PC7bRto), and tests that
// used it as a convenient all-powerful bearer now hold a privileged fleet
// token instead.
const testBearerSubject = "test-admin"

// withTestToken registers token on fc as a privileged fleet token and returns
// fc, creating a default FleetConfig when fc is nil. An empty token returns fc
// unchanged. Tests use it where they used to pass the daemon a primary static
// token: the daemon now authenticates only fleet tokens and JWTs.
func withTestToken(fc *FleetConfig, token string) *FleetConfig {
	if fc == nil {
		fc = NewFleetConfig("default", nil)
	}
	if token == "" {
		return fc
	}
	fc.RegisterTokenFull(token, fc.DefaultFleet, "", nil, nil, time.Time{}, testBearerSubject, true)
	return fc
}

// asTokenMinter gives r the identity of the one caller allowed to use the
// bridge/fleet-token routes: a machine principal holding the token-mint
// capability (the controller). Handler-level tests call the handlers directly,
// so the auth middleware never attaches an identity unless they do.
func asTokenMinter(r *http.Request) *http.Request {
	return r.WithContext(machineCtx("gardenio-controller", "gardenio-machine-token-mint"))
}

// asOIDCAdmin gives r an OIDC admin identity, the caller the embeddings admin
// routes accept (isAdminRequest).
func asOIDCAdmin(r *http.Request) *http.Request {
	ctx := withAuthMethod(r.Context(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleAdmin})
	return r.WithContext(ctx)
}

// withMinterRoutes serves the bridge/fleet-token routes for requests that
// carry adminToken as the token-mint machine principal would, and everything
// else through next. The routes now admit only a machine principal holding the
// token-mint capability, which needs a Keycloak harness; the e2e tests that
// only need to register tokens before exercising something else use this.
func withMinterRoutes(srv *BeadsServer, adminToken string, next http.Handler) http.Handler {
	mux := http.NewServeMux()
	for _, noun := range []string{"fleet", "bridge"} {
		mux.HandleFunc("POST /v1/"+noun+"-tokens", srv.handleRegisterBridgeToken)
		mux.HandleFunc("GET /v1/"+noun+"-tokens", srv.handleListBridgeTokens)
		mux.HandleFunc("DELETE /v1/"+noun+"-tokens/{"+noun+"}", srv.handleRevokeBridgeToken)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+adminToken {
			if _, pattern := mux.Handler(r); pattern != "" {
				mux.ServeHTTP(w, asTokenMinter(r))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authDisabledCtx is the context of a request served by a daemon with no auth
// configured (dev/test): authMethodNone.
func authDisabledCtx() context.Context {
	return withAuthMethod(context.Background(), authMethodNone)
}

// agentBearer registers a per-agent fleet token for the agent bead named name
// (fleet "agent-<name>", subject name) on fc and returns the bearer. It is
// what an agent pod authenticates with now that it cannot self-attribute via
// X-Actor on a shared token.
func agentBearer(fc *FleetConfig, name string) string {
	tok := "agent-tok-" + name
	fc.RegisterTokenFull(tok, agentFleetPrefix+name, "", nil, nil, time.Time{}, name, false)
	return tok
}

// xactorAgentHandler is NewHTTPHandler for tests that predate the retirement
// of the shared static token: they authenticate with `primary` and name the
// acting agent in X-Actor. The daemon no longer honors X-Actor, so this wrapper
// translates that pair into what a pod really presents now, its own per-agent
// fleet token (agentBearer), before the request reaches the real handler.
// Requests without X-Actor keep `primary`, a privileged non-agent fleet token.
func xactorAgentHandler(s *BeadsServer, primary string, verifier TokenVerifier) http.Handler {
	fc := withTestToken(nil, primary)
	inner := s.NewHTTPHandler(verifier, fc)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actor := r.Header.Get("X-Actor"); actor != "" && r.Header.Get("Authorization") == "Bearer "+primary {
			if name, ok := sanitizeActorHeader(actor); ok {
				r.Header.Set("Authorization", "Bearer "+agentBearer(fc, name))
				r.Header.Del("X-Actor")
			}
		}
		inner.ServeHTTP(w, r)
	})
}

// withTestAgent gives ctx the identity of agent name authenticating with its
// own per-agent fleet token (fleet "agent-<name>", subject name): what a
// Type:"static" identity with Subject name stood for before the shared token
// was retired.
func withTestAgent(ctx context.Context, name string) context.Context {
	return WithFleet(WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: name}), agentFleetPrefix+name)
}

// withTestPrivilegedAgent is withTestAgent for tests of a mechanism other than
// ownership (taint propagation): a privileged agent fleet token bypasses the
// per-bead ownership gate, as the shared static token used to.
func withTestPrivilegedAgent(ctx context.Context, name string) context.Context {
	return WithFleet(WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: name, Privileged: true}), agentFleetPrefix+name)
}

// withTestService is the context of a non-agent service caller (a bridge or
// admin tool on a privileged fleet token): what a Type:"static" identity with
// Subject "static" stood for before the shared token was retired.
func withTestService(ctx context.Context) context.Context {
	ctx = withAuthMethod(ctx, authMethodStatic)
	return WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: testBearerSubject, Privileged: true})
}
