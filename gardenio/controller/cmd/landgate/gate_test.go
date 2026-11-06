package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/oidcauth"
)

// upstreamRecorder stands in for kbeads and keeps every request it receives.
// A land door's GET /v1/me carrying the admission check's marker (landgate's
// land token check, as kbeads isLandDoorAuthCheck tells it) is kept apart in
// checks and answered from landTokens as kbeads would: 200 with the land
// principal's body for a token of the door's land, 403 for another land's
// or a fleet token, 401 for one it does not know.
type upstreamRecorder struct {
	mu     sync.Mutex
	reqs   []*http.Request
	checks []*http.Request
}

// stubLandToken is how the stub kbeads answers one opaque token: the land it
// is a land principal for ("" for a fleet token), or a fixed reply.
type stubLandToken struct {
	land   string
	status int
	body   string
}

var landTokens = map[string]stubLandToken{
	"landtokacme0123456789abcdef": {land: "acme"},
	"landtokbeta0123456789abcdef": {land: "beta"},
	"fleettok0123456789abcdef":    {},
	"unrestricted0123456789":      {status: http.StatusOK, body: `{"operator_id":"land:acme","unrestricted":true,"lands":[{"name":"acme","tier":"operator"}]}`},
	"twolands0123456789":          {status: http.StatusOK, body: `{"operator_id":"land:acme","unrestricted":false,"lands":[{"name":"acme","tier":"land"},{"name":"beta","tier":"land"}]}`},
	"otheroperator0123456789":     {status: http.StatusOK, body: `{"operator_id":"ana@example.com","unrestricted":false,"lands":[{"name":"acme","tier":"land"}]}`},
	"otherlandbody0123456789":     {status: http.StatusOK, body: `{"operator_id":"land:beta","unrestricted":false,"lands":[{"name":"beta","tier":"land"}]}`},
	"nolands0123456789":           {status: http.StatusOK, body: `{"operator_id":"land:acme","unrestricted":false,"lands":[]}`},
	// Per-agent tokens (gd-mh9ANzCHIE): kbeads reports one unrestricted, in no
	// land, with its project binding and agent bead.
	"agenttokacme0123456789":    {status: http.StatusOK, body: `{"operator_id":"land-agent","source":"token","unrestricted":true,"token_project":"acme","agent":"land-agent","agent_bead":"acme-1","lands":[]}`},
	"agenttokbeta0123456789":    {status: http.StatusOK, body: `{"operator_id":"beta-agent","unrestricted":true,"token_project":"beta","agent":"beta-agent","agent_bead":"beta-1","lands":[]}`},
	"agenttokunbound0123456789": {status: http.StatusOK, body: `{"operator_id":"loose-agent","unrestricted":true,"agent":"loose-agent","agent_bead":"x-1","lands":[]}`},
	"agenttoklands0123456789":   {status: http.StatusOK, body: `{"operator_id":"odd-agent","unrestricted":true,"token_project":"acme","agent":"odd-agent","agent_bead":"acme-2","lands":[{"name":"acme","tier":"operator"}]}`},
	"projectfleet0123456789":    {status: http.StatusOK, body: `{"operator_id":"svc","unrestricted":true,"token_project":"acme","lands":[]}`},
	"notjson0123456789":         {status: http.StatusOK, body: `upstream:/v1/me`},
	"kbeads500x0123456789":      {status: http.StatusInternalServerError, body: `{"error":"boom"}`},
	"kbeads429x0123456789":      {status: http.StatusTooManyRequests, body: `{"error":"rate limit exceeded"}`},
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	door := r.Header.Get(landDoorHeader)
	if r.Method == http.MethodGet && r.URL.Path == "/v1/me" && door != "" && r.Header.Get(landDoorCheckHeader) == "1" {
		u.checks = append(u.checks, r.Clone(context.Background()))
		tok, ok := landTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		switch {
		case !ok:
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
		case tok.status != 0:
			w.WriteHeader(tok.status)
			_, _ = io.WriteString(w, tok.body)
		case tok.land == "":
			http.Error(w, `{"error":"land door: a land bearer token is required"}`, http.StatusForbidden)
		case tok.land != door:
			http.Error(w, `{"error":"land door: credential is for another land"}`, http.StatusForbidden)
		default:
			_, _ = io.WriteString(w, `{"operator_id":"land:`+tok.land+`","source":"token","unrestricted":false,"lands":[{"name":"`+tok.land+`","tier":"land"}]}`)
		}
		return
	}
	u.reqs = append(u.reqs, r.Clone(context.Background()))
	_, _ = io.WriteString(w, "upstream:"+r.URL.Path)
}

func (u *upstreamRecorder) takeChecks() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.checks
	u.checks = nil
	return out
}

func (u *upstreamRecorder) take() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.reqs
	u.reqs = nil
	return out
}

// newTestAuth returns oidcauth's real middleware against iss, once discovery
// has finished.
func newTestAuth(t *testing.T, iss *testIssuer) *oidcauth.Handle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := oidcauth.New(iss.clientContext(ctx), oidcauth.Config{IssuerURL: iss.url(), ClientID: testAudience}, slog.New(slog.DiscardHandler))
	deadline := time.Now().Add(10 * time.Second)
	for !h.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("oidcauth never became ready against the test issuer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h
}

func newTestGate(t *testing.T, land string) (*testIssuer, *upstreamRecorder, *httptest.Server) {
	t.Helper()
	iss := newTestIssuer(t)
	up := &upstreamRecorder{}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	u, _ := url.Parse(upSrv.URL)
	auth := newTestAuth(t, iss)
	gw := httptest.NewServer(newGate(land, u, auth.Middleware, slog.New(slog.DiscardHandler)).handler(auth.Ready))
	t.Cleanup(gw.Close)
	return iss, up, gw
}

// rawGet sends requestTarget byte for byte, so the gate sees encodings and
// dot segments a net/http client would normalise.
func rawGet(t *testing.T, srv *httptest.Server, requestTarget, bearer string) int {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "GET " + requestTarget + " HTTP/1.1\r\nHost: api.clonk.tv\r\nConnection: close\r\n"
	if bearer != "" {
		req += "Authorization: Bearer " + bearer + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func do(t *testing.T, srv *httptest.Server, method, path string, header http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func bearer(tok string) http.Header {
	return http.Header{"Authorization": {"Bearer " + tok}}
}

func TestGate_ForwardsLandMembersWithTheirOwnBearer(t *testing.T) {
	iss, up, gw := newTestGate(t, "acme")
	for _, group := range []string{"/project-acme", "/project-acme/owners", "/project-acme/guests"} {
		tok := iss.tenant(t, "acme", "ana@example.com", "/lands-tenants", group)
		code, body := do(t, gw, http.MethodGet, "/v1/beads?project=acme", bearer(tok))
		if code != http.StatusOK || body != "upstream:/v1/beads" {
			t.Fatalf("%s: got %d %q, want the upstream's 200", group, code, body)
		}
		reqs := up.take()
		if len(reqs) != 1 {
			t.Fatalf("%s: upstream saw %d requests, want 1", group, len(reqs))
		}
		r := reqs[0]
		if got := r.Header.Get("Authorization"); got != "Bearer "+tok {
			t.Errorf("%s: upstream Authorization = %q, want the caller's bearer untouched", group, got)
		}
		if r.URL.RawQuery != "project=acme" {
			t.Errorf("%s: query = %q", group, r.URL.RawQuery)
		}
		if got := r.Header.Values(landDoorHeader); len(got) != 1 || got[0] != "acme" {
			t.Errorf("%s: upstream %s = %q, want [acme]", group, landDoorHeader, got)
		}
	}
	// A user of two lands with a token minted for this one passes; kbeads
	// narrows it to this land (and X-Land-Door makes it check).
	tok := iss.tenant(t, "acme", "two@example.com", "/project-acme", "/project-beta")
	h := bearer(tok)
	h.Set(landDoorHeader, "beta") // a forged door is replaced, never trusted
	if code, _ := do(t, gw, http.MethodGet, "/v1/beads?project=beta", h); code != http.StatusOK {
		t.Fatalf("two-land user with an acme token: got %d, want 200", code)
	}
	if reqs := up.take(); len(reqs) != 1 || reqs[0].Header.Get(landDoorHeader) != "acme" {
		t.Errorf("two-land user: upstream %s not pinned to acme", landDoorHeader)
	}
}

// Landgate holds no credential: a request it forwards carries exactly the
// caller's Authorization and no other header that names an identity, however
// the caller dressed it.
func TestGate_ScrubsIdentityHeaders(t *testing.T) {
	iss, up, gw := newTestGate(t, "acme")
	tok := iss.tenant(t, "acme", "ana@example.com", "/project-acme")
	h := bearer(tok)
	for _, name := range []string{
		"Cookie", "X-Auth-Request-Access-Token", "X-Actor", "X-Fleet", "X-System-Operator",
		"X-Owner-Override", "X-Operated-By", "X-Edge-Auth-Ts", "X-Edge-Auth-Sig", "X-Edge-Auth-Version",
		"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups", "X-Auth-Request-Preferred-Username",
		// The agent-pod family kbeads honours for a per-agent token
		// (gd-Utbyy4xJRX).
		"X-Trigger-Bead", "X-User-Bead-Id", "X-No-Owner-Default", "X-No-Auto-Parent",
		"X-No-Auto-Inherit", "X-Force-Parent-Edge",
	} {
		h.Set(name, "forged")
	}
	h.Set(landDoorCheckHeader, "1") // a caller may not pass as the door's admission check
	h.Set("X-Forwarded-For", "203.0.113.7")
	if code, _ := do(t, gw, http.MethodPost, "/v1/beads", h); code != http.StatusOK {
		t.Fatalf("got %d, want 200", code)
	}
	reqs := up.take()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests", len(reqs))
	}
	for name, vs := range reqs[0].Header {
		for _, v := range vs {
			if v == "forged" {
				t.Errorf("upstream received %s: %s", name, v)
			}
		}
	}
	if got := reqs[0].Header.Values(landDoorCheckHeader); len(got) != 0 {
		t.Errorf("upstream received %s: %q", landDoorCheckHeader, got)
	}
	if got := reqs[0].Header.Get("X-Forwarded-For"); got != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want the edge's value kept", got)
	}
}

func TestGate_RefusesOtherCallers(t *testing.T) {
	iss, up, gw := newTestGate(t, "acme")
	other := newTestIssuer(t)
	cases := []struct {
		name   string
		header http.Header
		want   int
	}{
		{"no credential", nil, http.StatusUnauthorized},
		{"forwardauth header only", http.Header{"X-Auth-Request-Access-Token": {iss.tenant(t, "acme", "ana@example.com", "/project-acme")}}, http.StatusUnauthorized},
		{"basic auth", http.Header{"Authorization": {"Basic YTpi"}}, http.StatusUnauthorized},
		{"not a JWT (a fleet or static token)", bearer("kd_fleet_0123456789abcdef"), http.StatusUnauthorized},
		{"another issuer's key", bearer(other.mint(t, map[string]any{"iss": iss.url(), "groups": []string{"/project-acme"}, "realm_access": map[string]any{"roles": []string{"gardenio-project-member"}}})), http.StatusUnauthorized},
		{"expired", bearer(iss.mint(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix(), "groups": []string{"/project-acme"}, "realm_access": map[string]any{"roles": []string{"gardenio-project-member"}}})), http.StatusUnauthorized},
		{"wrong audience", bearer(iss.mint(t, map[string]any{"aud": "someone-else", "groups": []string{"/project-acme"}, "realm_access": map[string]any{"roles": []string{"gardenio-project-member"}}})), http.StatusUnauthorized},
		{"another land", bearer(iss.tenant(t, "beta", "bo@example.com", "/project-beta", "/project-beta/owners")), http.StatusForbidden},
		{"lands-tenants only", bearer(iss.tenant(t, "acme", "cy@example.com", "/lands-tenants")), http.StatusForbidden},
		{"lookalike groups", bearer(iss.tenant(t, "acme", "cy@example.com", "/project-acme-evil", "/project-acmex", "project-acme", "/project-acme/admins", "/project-acme/owners/x", "/PROJECT-acme")), http.StatusForbidden},
		{"member without the project-member role", bearer(iss.mint(t, map[string]any{"email": "dee@example.com", "groups": []string{"/project-acme"}, "realm_access": map[string]any{"roles": []string{"gardenio-viewer"}}, "aud": []string{testAudience, "land-acme"}})), http.StatusForbidden},
		{"member, token minted for no land", bearer(iss.tenant(t, "", "ana@example.com", "/project-acme")), http.StatusForbidden},
		{"member of two lands, token minted for the other", bearer(iss.tenant(t, "beta", "ana@example.com", "/project-acme", "/project-beta")), http.StatusForbidden},
		{"member of two lands, token minted for both", bearer(iss.mint(t, map[string]any{"email": "ana@example.com", "groups": []string{"/project-acme", "/project-beta"}, "realm_access": map[string]any{"roles": []string{"gardenio-project-member"}}, "aud": []string{testAudience, "land-acme", "land-beta"}})), http.StatusForbidden},
		{"land audience without the client audience", bearer(iss.mint(t, map[string]any{"email": "ana@example.com", "groups": []string{"/project-acme"}, "realm_access": map[string]any{"roles": []string{"gardenio-project-member"}}, "aud": []string{"land-acme"}})), http.StatusUnauthorized},
		{"admin who is also a member", bearer(iss.mint(t, map[string]any{"email": "op@example.com", "groups": []string{"/project-acme", "/gardenio-admins"}, "realm_access": map[string]any{"roles": []string{"gardenio-project-member", "gardenio-admin"}}, "aud": []string{testAudience, "land-acme"}})), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := do(t, gw, http.MethodGet, "/v1/beads", tc.header); code != tc.want {
				t.Errorf("got %d, want %d", code, tc.want)
			}
			if reqs := up.take(); len(reqs) != 0 {
				t.Errorf("forwarded %d requests, want none", len(reqs))
			}
		})
	}
}

// A minted land token (D.18a) is opaque: landgate asks kbeads GET /v1/me
// about it, with the bearer, this land's door and the admission check's
// marker and nothing else, and forwards only this land's land principal. The
// request it forwards, the caller's own GET /v1/me included, never carries the
// marker, so kbeads counts it against the rate limit (gd-Utbyy4xJRX).
func TestGate_LandTokenForwardedAfterKbeadsCheck(t *testing.T) {
	_, up, gw := newTestGate(t, "acme")
	const tok = "landtokacme0123456789abcdef"
	h := bearer(tok)
	for _, name := range []string{"Cookie", "X-Actor", "X-Fleet", "X-Operated-By", "X-Auth-Request-Email", "X-Edge-Auth-Sig", "X-Trigger-Bead", "X-Force-Parent-Edge"} {
		h.Set(name, "forged")
	}
	h.Set(landDoorHeader, "beta")   // a forged door is replaced, never trusted
	h.Set(landDoorCheckHeader, "1") // as is a forged admission check
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/beads?project=acme"}, {http.MethodPost, "/v1/beads"}, {http.MethodGet, "/v1/me"},
	} {
		code, body := do(t, gw, c.method, c.path, h)
		if code != http.StatusOK {
			t.Fatalf("%s %s: got %d %q, want the upstream's 200", c.method, c.path, code, body)
		}
		checks := up.takeChecks()
		if len(checks) == 0 {
			t.Fatalf("%s %s: forwarded without asking kbeads", c.method, c.path)
		}
		chk := checks[0]
		if chk.Method != http.MethodGet || chk.URL.Path != "/v1/me" {
			t.Errorf("check = %s %s, want GET /v1/me", chk.Method, chk.URL.Path)
		}
		if got := chk.Header.Get("Authorization"); got != "Bearer "+tok {
			t.Errorf("check Authorization = %q", got)
		}
		if got := chk.Header.Values(landDoorHeader); len(got) != 1 || got[0] != "acme" {
			t.Errorf("check %s = %q, want [acme]", landDoorHeader, got)
		}
		for name := range chk.Header {
			switch name {
			case "Authorization", landDoorHeader, landDoorCheckHeader, "User-Agent", "Accept-Encoding":
			default:
				t.Errorf("check carried %s; want only Authorization, %s and %s", name, landDoorHeader, landDoorCheckHeader)
			}
		}
		if len(checks) != 1 {
			t.Errorf("%s %s: kbeads saw %d admission checks, want 1: the caller's own request passed as one", c.method, c.path, len(checks))
		}
		reqs := up.take()
		if len(reqs) != 1 {
			t.Fatalf("%s %s: upstream saw %d requests, want 1", c.method, c.path, len(reqs))
		}
		r := reqs[0]
		if got := r.Header.Get("Authorization"); got != "Bearer "+tok {
			t.Errorf("forwarded Authorization = %q, want the caller's bearer untouched", got)
		}
		if got := r.Header.Values(landDoorHeader); len(got) != 1 || got[0] != "acme" {
			t.Errorf("forwarded %s = %q, want [acme]", landDoorHeader, got)
		}
		if got := r.Header.Values(landDoorCheckHeader); len(got) != 0 {
			t.Errorf("forwarded %s = %q, want none", landDoorCheckHeader, got)
		}
		for name, vs := range r.Header {
			for _, v := range vs {
				if v == "forged" {
					t.Errorf("upstream received %s: %s", name, v)
				}
			}
		}
	}
	// Nothing is cached: every request asks again, so a revoke bites at once.
	for range 3 {
		do(t, gw, http.MethodGet, "/v1/beads", bearer(tok))
	}
	if n := len(up.takeChecks()); n != 3 {
		t.Errorf("kbeads was asked %d times for 3 requests, want 3", n)
	}
	up.take()
}

// An agent's own per-agent token bound to this land (gd-mh9ANzCHIE) is
// forwarded after the same kbeads check, so gb stop reaches its own bead.
func TestGate_AgentTokenForwardedAfterKbeadsCheck(t *testing.T) {
	_, up, gw := newTestGate(t, "acme")
	const tok = "agenttokacme0123456789"
	if code, body := do(t, gw, http.MethodPatch, "/v1/beads/acme-1", bearer(tok)); code != http.StatusOK {
		t.Fatalf("PATCH own agent bead: got %d %q, want the upstream's 200", code, body)
	}
	if n := len(up.takeChecks()); n != 1 {
		t.Errorf("kbeads was asked %d times, want 1", n)
	}
	reqs := up.take()
	if len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+tok || reqs[0].Header.Get(landDoorHeader) != "acme" {
		t.Fatalf("upstream saw %d requests, want 1 with the caller's bearer and door acme", len(reqs))
	}
}

func TestGate_LandTokenRefusals(t *testing.T) {
	_, up, gw := newTestGate(t, "acme")
	cases := []struct {
		name, token string
		want        int
	}{
		{"another land's land token", "landtokbeta0123456789abcdef", http.StatusForbidden},
		{"a fleet or bridge token", "fleettok0123456789abcdef", http.StatusForbidden},
		{"an unknown token", "nosuchtoken0123456789", http.StatusUnauthorized},
		{"kbeads says unrestricted", "unrestricted0123456789", http.StatusForbidden},
		{"kbeads names two lands", "twolands0123456789", http.StatusForbidden},
		{"kbeads names no land", "nolands0123456789", http.StatusForbidden},
		{"kbeads names another operator", "otheroperator0123456789", http.StatusForbidden},
		{"kbeads names another land", "otherlandbody0123456789", http.StatusForbidden},
		{"another land's agent token", "agenttokbeta0123456789", http.StatusForbidden},
		{"an agent token bound to no project", "agenttokunbound0123456789", http.StatusForbidden},
		{"an agent token kbeads lists in a land", "agenttoklands0123456789", http.StatusForbidden},
		{"a project-bound token that is no agent's", "projectfleet0123456789", http.StatusForbidden},
		{"kbeads answers 200 with no JSON", "notjson0123456789", http.StatusBadGateway},
		{"kbeads answers 500", "kbeads500x0123456789", http.StatusBadGateway},
		{"kbeads answers 429", "kbeads429x0123456789", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := do(t, gw, http.MethodGet, "/v1/beads", bearer(tc.token)); code != tc.want {
				t.Errorf("got %d, want %d", code, tc.want)
			}
			if n := len(up.takeChecks()); n != 1 {
				t.Errorf("kbeads was asked %d times, want 1", n)
			}
			if reqs := up.take(); len(reqs) != 0 {
				t.Errorf("forwarded %d requests, want none", len(reqs))
			}
		})
	}
	// A JWT-shaped bearer never takes the land token path: oidcauth refuses it.
	if code, _ := do(t, gw, http.MethodGet, "/v1/beads", bearer("aaa.bbb.ccc")); code != http.StatusUnauthorized {
		t.Errorf("garbage JWT: got %d, want 401", code)
	}
	if n := len(up.takeChecks()); n != 0 {
		t.Errorf("a JWT-shaped bearer reached the land token check %d times", n)
	}
	// Anything else that is not one base64url segment takes the JWT path too.
	for _, raw := range []string{"bearer landtokacme0123456789abcdef", "Bearer  landtokacme0123456789abcdef", "Bearer landtok+acme/0123=", "Bearer a.b"} {
		if code, _ := do(t, gw, http.MethodGet, "/v1/beads", http.Header{"Authorization": {raw}}); code != http.StatusUnauthorized {
			t.Errorf("%q: got %d, want 401", raw, code)
		}
	}
	if n := len(up.takeChecks()); n != 0 {
		t.Errorf("a malformed bearer reached the land token check %d times", n)
	}
	if reqs := up.take(); len(reqs) != 0 {
		t.Errorf("forwarded %d requests, want none", len(reqs))
	}
}

func TestGate_LandTokenKbeadsDownIs502(t *testing.T) {
	iss := newTestIssuer(t)
	down := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(down.URL)
	down.Close()
	auth := newTestAuth(t, iss)
	gw := httptest.NewServer(newGate("acme", u, auth.Middleware, slog.New(slog.DiscardHandler)).handler(auth.Ready))
	t.Cleanup(gw.Close)
	if code, _ := do(t, gw, http.MethodGet, "/v1/beads", bearer("landtokacme0123456789abcdef")); code != http.StatusBadGateway {
		t.Errorf("got %d, want 502", code)
	}
}

func TestGate_DeniedRoutesAre404ForEveryone(t *testing.T) {
	iss, up, gw := newTestGate(t, "acme")
	tok := iss.tenant(t, "acme", "ana@example.com", "/project-acme")
	for _, p := range []string{
		"/v1/events/stream", "/v1/events/stream/", "/v1/bus/events", "/v1/bus/events/x",
		"/v1/bridge-tokens", "/v1/bridge-tokens/x", "/v1/fleet-tokens", "/v1/fleet-tokens/x",
		"/v1/auth/refresh", "/v1/auth",
		"/v1/admin/revoke-token", "/v1/admin/", "/v1/admin",
		"/v1/hooks/execute", "/v1/hooks/emit", "/v1/embeddings/upsert",
		"/", "/v1", "/v2/beads", "/metrics", "/land/acme/v1/beads",
	} {
		for _, h := range []http.Header{nil, bearer(tok)} {
			if code, _ := do(t, gw, http.MethodGet, p, h); code != http.StatusNotFound {
				t.Errorf("%s (bearer=%v): got %d, want 404", p, h != nil, code)
			}
		}
	}
	// Routes the land door does not list (land_door_routes.txt) are 404 too:
	// they are not the agent loop's or the Lands API's.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/events"}, {http.MethodGet, "/v1/authz"}, {http.MethodGet, "/v1/administer"},
		{http.MethodGet, "/v1/beads/gd-1/events"}, {http.MethodPost, "/v1/decisions/gd-1/resolve"},
		{http.MethodGet, "/v1/lands/acme/members"}, {http.MethodGet, "/v1/configs"},
		{http.MethodPost, "/v1/agents/gd-1/gates/decision/satisfy"}, {http.MethodGet, "/v1/users/me/terms"},
		{http.MethodDelete, "/v1/beads/gd-1"}, {http.MethodPut, "/v1/beads/gd-1"},
		{http.MethodPost, "/v1/beads/gd-1/transfer"}, {http.MethodGet, "/v1/beads/gd-1/"},
		{http.MethodGet, "/v1/beads/gd-1/."}, {http.MethodGet, "/v1/lands/v1/lands/acme/nope"},
		{http.MethodGet, "/v1/windows/self/view"}, {http.MethodGet, "/v1/windows/self/live"}, {http.MethodGet, "/v1/windows/gd-1"},
		{http.MethodPost, "/v1/windows/preview"}, {http.MethodGet, "/v1/windows/preview/x"},
	} {
		if code, _ := do(t, gw, c.method, c.path, bearer(tok)); code != http.StatusNotFound {
			t.Errorf("%s %s: got %d, want 404", c.method, c.path, code)
		}
	}
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/v1/beads"}, {http.MethodGet, "/v1/beads/gd-1"}, {http.MethodGet, "/v1/me"},
		{http.MethodPost, "/v1/beads"}, {http.MethodPatch, "/v1/beads/gd-1"},
		{http.MethodPost, "/v1/beads/gd-1/close"}, {http.MethodPost, "/v1/beads/gd-1/comments"},
		{http.MethodDelete, "/v1/beads/gd-1/labels/x"}, {http.MethodGet, "/v1/agents-by-name/a"},
		{http.MethodGet, "/v1/lands/v1/lands/acme/whoami"}, {http.MethodPost, "/v1/lands/v1/lands/acme/tickets"},
		{http.MethodGet, "/v1/lands/v1/lands/acme/events"},
		{http.MethodPost, "/v1/windows"}, {http.MethodGet, "/v1/windows"}, {http.MethodDelete, "/v1/windows/gd-1"},
		{http.MethodGet, "/v1/windows/preview"},
	}
	for _, c := range allowed {
		if code, _ := do(t, gw, c.method, c.path, bearer(tok)); code != http.StatusOK {
			t.Errorf("%s %s: got %d, want 200", c.method, c.path, code)
		}
	}
	if n := len(up.take()); n != len(allowed) {
		t.Errorf("upstream saw %d requests, want %d (only the allowed routes)", n, len(allowed))
	}
}

func TestGate_RefusesAmbiguousPaths(t *testing.T) {
	iss, up, gw := newTestGate(t, "acme")
	tok := iss.tenant(t, "acme", "ana@example.com", "/project-acme")
	for _, target := range []string{
		"/v1/beads/../admin/revoke-token", "/v1/%2e%2e/admin", "/v1/admin%2frevoke-token",
		"/v1//admin/revoke-token", "/v1/beads%5c..", "/v1/beads;x=1", "/v1/beads/%2E%2E/auth",
	} {
		if code := rawGet(t, gw, target, tok); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", target, code)
		}
	}
	if n := len(up.take()); n != 0 {
		t.Errorf("forwarded %d ambiguous requests", n)
	}
	// A percent-encoded query is fine: only the path is checked.
	if code := rawGet(t, gw, "/v1/beads?q=a%2Fb", tok); code != http.StatusOK {
		t.Errorf("encoded query: got %d, want 200", code)
	}
}

func TestGate_Probes(t *testing.T) {
	_, up, gw := newTestGate(t, "acme")
	for _, p := range []string{"/healthz", "/readyz"} {
		if code, _ := do(t, gw, http.MethodGet, p, nil); code != http.StatusOK {
			t.Errorf("%s: got %d", p, code)
		}
	}
	if n := len(up.take()); n != 0 {
		t.Errorf("probes reached the upstream %d times", n)
	}
}

func TestParseConfig(t *testing.T) {
	base := map[string]string{
		"LANDGATE_LAND":     "acme",
		"LANDGATE_UPSTREAM": "http://gardenio-beads:8080",
		"OIDC_ISSUER_URL":   "https://auth.clonk.tv/realms/gardenio",
		"OIDC_CLIENT_ID":    "gardenio",
	}
	env := func(over map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := over[k]; ok {
				return v
			}
			return base[k]
		}
	}
	cfg, err := parseConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.land != "acme" || cfg.listenAddr != ":8080" || cfg.upstream.Host != "gardenio-beads:8080" {
		t.Errorf("cfg = %+v", cfg)
	}
	for name, over := range map[string]map[string]string{
		"no land":           {"LANDGATE_LAND": ""},
		"uppercase land":    {"LANDGATE_LAND": "Acme"},
		"land with a slash": {"LANDGATE_LAND": "acme/owners"},
		"64-char land":      {"LANDGATE_LAND": strings.Repeat("a", 64)},
		"no upstream":       {"LANDGATE_UPSTREAM": ""},
		"upstream not url":  {"LANDGATE_UPSTREAM": "gardenio-beads:8080"},
		"no issuer":         {"OIDC_ISSUER_URL": ""},
		"no client id":      {"OIDC_CLIENT_ID": ""},
		"http issuer":       {"OIDC_ISSUER_URL": "http://auth.clonk.tv/realms/gardenio"},
	} {
		if _, err := parseConfig(env(over)); err == nil {
			t.Errorf("%s: parseConfig accepted it", name)
		}
	}
}
