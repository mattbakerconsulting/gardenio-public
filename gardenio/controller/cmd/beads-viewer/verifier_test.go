package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/oidcauth"
)

// verifierChain is the viewer's chain from verifier to the mux over the full
// route table (each route answers 200), as a direct call with no edge proof.
func verifierChain(t *testing.T, roles string, me *fakeMe, roleOf verifierAgentRole) http.Handler {
	t.Helper()
	m := registeredViewerRoutes(t)
	patternOf := func(r *http.Request) string { _, p := m.mux.Handler(r); return p }
	return verifierMiddleware(scopeMiddleware(m.mux, patternOf), patternOf, func() string { return roles }, me.who, roleOf, testLogger())
}

// directRequest is a call that reached the viewer without the edge secret,
// carrying bearer, as devBypassMiddleware and operatorMiddleware leave it.
func directRequest(method, path, bearer string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	ctx := withNoEdgeProof(r.Context())
	if bearer != "" {
		ctx = beadsapi.WithUserBearer(ctx, bearer)
	}
	return r.WithContext(ctx)
}

func agentRoles(roles map[string]string) verifierAgentRole {
	return func(_ context.Context, _ string, agentBead string) (string, error) {
		role, ok := roles[agentBead]
		if !ok {
			return "", &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
		}
		return role, nil
	}
}

var verifierMe = map[string]*beadsapi.MeResponse{
	"crew-token":  {OperatorID: "crew-1", Source: "token", TokenProject: "gardenio", Agent: "crew-1", AgentBead: "gd-crew", Unrestricted: true},
	"infra-token": {OperatorID: "infra-1", Source: "token", TokenProject: "gardenio", Agent: "infra-1", AgentBead: "gd-infra", Unrestricted: true},
	"gard-token":  {OperatorID: "gard-1", Source: "token", TokenProject: "gardenio", Agent: "gard-1", AgentBead: "gd-gard", Unrestricted: true},
	"land-agent":  {OperatorID: "tenant-1", Source: "token", TokenProject: "claimlayer", Agent: "tenant-1", AgentBead: "gd-tenant", Unrestricted: false},
	"fleet-token": {OperatorID: "gardenio", Source: "token", Unrestricted: true},
	"bridge-sa":   {OperatorID: "machine:gardenio-slack-bridge", Source: "token", Unrestricted: true},
}

var verifierAgentRoleTable = map[string]string{"gd-crew": "crew", "gd-infra": "infra", "gd-gard": "plan-gardener", "gd-tenant": "crew"}

// TestVerifier_AdmitsAllowedFleetAgentOnGET: a fleet agent whose role is on
// viewer.verifier_roles reads a page that a direct call is otherwise refused
// (gd-6ujfeTZfNL), admin-only and scoped routes alike.
func TestVerifier_AdmitsAllowedFleetAgentOnGET(t *testing.T) {
	h := verifierChain(t, "crew, infra", &fakeMe{answers: verifierMe}, agentRoles(verifierAgentRoleTable))
	for _, path := range []string{"/", "/decisions", "/escalations", "/inbox", "/inbox/gd-zTQIrRbcse", "/gd-zTQIrRbcse", "/garden", "/active", "/plans", "/plans/gd-p1", "/p/gardenio", "/releases", "/release/r1"} {
		for _, bearer := range []string{"crew-token", "infra-token"} {
			if code := serveReq(h, directRequest(http.MethodGet, path, bearer)); code != http.StatusOK {
				t.Errorf("GET %s as %s = %d, want 200", path, bearer, code)
			}
		}
	}
	if code := serveReq(h, directRequest(http.MethodHead, "/decisions", "crew-token")); code != http.StatusOK {
		t.Errorf("HEAD /decisions = %d, want 200", code)
	}
}

// TestVerifier_AdmitsTheFarm: the farm /garden draws, its stream and the shared
// connection, and the two POSTs that steer the caller's own stream, so a crew
// can shoot the live farm (gd-TRdxFvBjW2, owner grant gd-rO1s5UC5m2).
func TestVerifier_AdmitsTheFarm(t *testing.T) {
	h := verifierChain(t, "crew", &fakeMe{answers: verifierMe}, agentRoles(verifierAgentRoleTable))
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/garden/farm"},
		{http.MethodGet, "/garden/farm/events"},
		{http.MethodGet, "/garden/live/events"},
		{http.MethodPost, "/garden/farm/events/view"},
		{http.MethodPost, "/garden/live/sub"},
	} {
		if code := serveReq(h, directRequest(c.method, c.path, "crew-token")); code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200", c.method, c.path, code)
		}
		if code := serveReq(h, directRequest(c.method, c.path, "land-agent")); code != http.StatusForbidden {
			t.Errorf("%s %s as a land agent = %d, want 403", c.method, c.path, code)
		}
	}
}

// TestVerifier_LiveCarriesOnlyAdmittedStreams: a stream a verifier's shared
// connection opens comes back through scopeMiddleware on the connection's own
// request, past verifierMiddleware (main.go gardenPage.Live), so scope admits it
// only when verifierAdmits the stream's route.
func TestVerifier_LiveCarriesOnlyAdmittedStreams(t *testing.T) {
	m := registeredViewerRoutes(t)
	patternOf := func(r *http.Request) string { _, p := m.mux.Handler(r); return p }
	live := scopeMiddleware(m.mux, patternOf)
	stream := func(path string) int {
		r := directRequest(http.MethodGet, path, "crew-token")
		return serveReq(live, r.WithContext(context.WithValue(r.Context(), verifierKey{}, true)))
	}
	if code := stream("/garden/farm/events"); code != http.StatusOK {
		t.Errorf("the farm stream = %d, want 200", code)
	}
	for _, path := range []string{"/garden/events", "/garden/glass/wall", "/garden/glass/crew-1/stream", "/garden/c/events", "/garden/bead/gd-a1/events", "/garden/plan/gd-p1/events"} {
		if code := stream(path); code != http.StatusForbidden {
			t.Errorf("stream %s = %d, want 403", path, code)
		}
	}
}

// TestVerifier_RefusesEverythingElse: what a verifier may not do stays the 403
// a direct call gets today.
func TestVerifier_RefusesEverythingElse(t *testing.T) {
	h := verifierChain(t, "crew,infra", &fakeMe{answers: verifierMe}, agentRoles(verifierAgentRoleTable))
	cases := []struct {
		name, method, path, bearer string
	}{
		{"a write", http.MethodPost, "/decisions/gd-x/resolve", "crew-token"},
		{"a /garden write", http.MethodPost, "/garden/welcome", "crew-token"},
		{"a steer route as a GET", http.MethodGet, "/garden/live/sub", "crew-token"},
		{"the garden event stream", http.MethodGet, "/garden/events", "crew-token"},
		{"a role not on the list", http.MethodGet, "/decisions", "gard-token"},
		{"a land agent's restricted token", http.MethodGet, "/decisions", "land-agent"},
		{"a shared fleet token", http.MethodGet, "/decisions", "fleet-token"},
		{"a service account", http.MethodGet, "/decisions", "bridge-sa"},
		{"a token the daemon refuses", http.MethodGet, "/decisions", "forged"},
		{"no bearer", http.MethodGet, "/decisions", ""},
		{"an agent terminal", http.MethodGet, "/agents/gd-a1/screen", "crew-token"},
		{"an agent prime", http.MethodGet, "/agents/gd-a1/prime", "crew-token"},
		{"a chat-named agent's terminal", http.MethodGet, "/agents/chatter-ab12cd/screen", "crew-token"},
		{"a chat- agent's terminal", http.MethodGet, "/agents/chat-a1/screen", "crew-token"},
		{"a chat-named agent's prime", http.MethodGet, "/agents/chatter-x/prime", "crew-token"},
		{"the agents event stream", http.MethodGet, "/agents/events", "crew-token"},
		{"telepathy", http.MethodGet, "/telepathy", "crew-token"},
		{"the glass", http.MethodGet, "/garden/glass/crew-1", "crew-token"},
		{"the farm activity feed", http.MethodGet, "/garden/farm/activity", "crew-token"},
		{"one farm activity transcript", http.MethodGet, "/garden/farm/activity/gd-a1", "crew-token"},
		{"the Grafana proxy", http.MethodGet, "/grafana", "crew-token"},
		{"a chart set", http.MethodGet, "/garden/charts/fleet", "crew-token"},
		{"the GitHub install callback", http.MethodGet, "/lands/github/setup", "crew-token"},
		{"the cassette store", http.MethodGet, "/cassettes", "crew-token"},
		{"an image from the store", http.MethodGet, "/image/cassettes/a1/shot.png", "crew-token"},
		{"a widget", http.MethodGet, "/widgets/burndown/gd-a1", "crew-token"},
		{"the agent list", http.MethodGet, "/agents", "crew-token"},
		{"the costs page", http.MethodGet, "/costs", "crew-token"},
	}
	for _, c := range cases {
		if code := serveReq(h, directRequest(c.method, c.path, c.bearer)); code != http.StatusForbidden {
			t.Errorf("%s: %s %s = %d, want 403", c.name, c.method, c.path, code)
		}
	}
}

// TestVerifier_AgentPageHasNoTerminal: a verifier reading an agent bead page
// gets no coopmux snapshot or terminal link; coopmux answers on the viewer's
// token, not the verifier's bearer.
func TestVerifier_AgentPageHasNoTerminal(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{ID: "gd-agt", Title: "crew-1", Type: "agent", Status: "open",
		Fields: map[string]string{"agent": "crew-1", "role": "crew", "agent_state": "running", "last_session_id": "sess-abc"},
		Notes:  "pod_name: crew-1-pod"}
	cm := &fakeScreenClient{screen: &coopmuxapi.Screen{Lines: []string{"secret screen"}}}
	srv := NewServer(f, slog.Default(), "", "", "", cm)
	srv.coopmuxPublicURL = "https://mux.example/dash"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	r := httptest.NewRequest(http.MethodGet, "/gd-agt", nil)
	r = r.WithContext(context.WithValue(r.Context(), verifierKey{}, true))
	w := httptest.NewRecorder()
	edgeAuth(mux).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /gd-agt as a verifier = %d, want 200", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "secret screen") || strings.Contains(body, "mux.example") {
		t.Errorf("verifier's agent page carries the coopmux snapshot or link: %s", snippetN(body, 600))
	}
	if cm.lastID != "" {
		t.Errorf("coopmux asked for %q on a verifier's request, want no call", cm.lastID)
	}
}

// TestVerifier_OffByDefault: with viewer.verifier_roles empty no one is
// admitted and the daemon is never asked.
func TestVerifier_OffByDefault(t *testing.T) {
	me := &fakeMe{answers: verifierMe}
	h := verifierChain(t, " ", me, agentRoles(verifierAgentRoleTable))
	if code := serveReq(h, directRequest(http.MethodGet, "/decisions", "crew-token")); code != http.StatusForbidden {
		t.Errorf("GET /decisions with no verifier roles = %d, want 403", code)
	}
	if me.asked != 0 {
		t.Errorf("daemon asked %d times with the verifier off, want 0", me.asked)
	}
}

// TestVerifier_OnlyDirectCalls: a request that came through the edge (a human
// with an identity) is not the verifier's business; the daemon is not asked.
func TestVerifier_OnlyDirectCalls(t *testing.T) {
	me := &fakeMe{answers: verifierMe}
	h := verifierChain(t, "crew", me, agentRoles(verifierAgentRoleTable))
	r := httptest.NewRequest(http.MethodGet, "/decisions", nil)
	r = r.WithContext(oidcauth.ContextWithEmail(beadsapi.WithUserBearer(r.Context(), "crew-token"), "matt@example.com"))
	if code := serveReq(h, r); code != http.StatusOK {
		t.Errorf("edge request = %d, want 200", code)
	}
	if me.asked != 0 {
		t.Errorf("daemon asked %d times for an edge request, want 0", me.asked)
	}
}

// TestVerifier_DaemonDownIs502: a daemon that cannot answer is not a refusal.
func TestVerifier_DaemonDownIs502(t *testing.T) {
	h := verifierChain(t, "crew", &fakeMe{err: errors.New("connection refused")}, agentRoles(verifierAgentRoleTable))
	if code := serveReq(h, directRequest(http.MethodGet, "/decisions", "crew-token")); code != http.StatusBadGateway {
		t.Errorf("daemon down = %d, want 502", code)
	}
	failingRole := func(context.Context, string, string) (string, error) { return "", errors.New("timeout") }
	h = verifierChain(t, "crew", &fakeMe{answers: verifierMe}, failingRole)
	if code := serveReq(h, directRequest(http.MethodGet, "/decisions", "crew-token")); code != http.StatusBadGateway {
		t.Errorf("role read failed = %d, want 502", code)
	}
}

// TestVerifierRoutes_AreRegisteredGETs: every allow-listed pattern is a GET (a
// POST on verifierSteerRoutes) the viewer registers, so a typo or a renamed
// route cannot silently widen or drop the list.
func TestVerifierRoutes_AreRegisteredGETs(t *testing.T) {
	for pattern := range verifierRoutes {
		if !strings.HasPrefix(pattern, "GET /") {
			t.Errorf("verifierRoutes has %q, want only GET patterns", pattern)
		}
		if _, ok := viewerRouteClasses[pattern]; !ok {
			t.Errorf("verifierRoutes has %q, which the viewer does not register", pattern)
		}
	}
	for pattern := range verifierSteerRoutes {
		if !strings.HasPrefix(pattern, "POST /") {
			t.Errorf("verifierSteerRoutes has %q, want only POST patterns", pattern)
		}
		if _, ok := viewerRouteClasses[pattern]; !ok {
			t.Errorf("verifierSteerRoutes has %q, which the viewer does not register", pattern)
		}
	}
}
