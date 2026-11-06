package main

import (
	"crypto/rand"
	"crypto/rsa"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// The impersonation banner (plan gd-0SUIdKXWV3 step 4, gd-8HB0UjzwsL): /garden draws "You are <tenant>, impersonated by
// <admin>" with Stop only for a session whose verified edge bearer carries the impersonator claim. The request runs the
// whole way: the dev-bypass edge middleware, then the page, as main.go wires them.
func TestGardenImpersonationBannerOnlyFromTheVerifiedClaim(t *testing.T) {
	realm := newEdgeBearerRealm(t)
	v := realm.verifier(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	h := &gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v",
		Viewer:       func(r *http.Request) string { return strings.ToLower(canonicalOIDC(oidcauth.EmailFromContext(r.Context()))) },
		Impersonator: viewerImpersonator, StopURL: defaultStopBecomeURL}
	mux := http.NewServeMux()
	h.Register(mux)
	page := devBypassMiddleware(testLogger(), edgeTestSecret, v)(mux)

	get := func(r *http.Request, headers ...string) string {
		t.Helper()
		r.URL.Path = "/garden"
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		page.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /garden = %d", rec.Code)
		}
		return rec.Body.String()
	}
	noBanner := func(name, body string) {
		t.Helper()
		if strings.Contains(body, "data-impersonator") || strings.Contains(body, `class="imp"`) {
			t.Errorf("%s: the page draws the impersonation banner", name)
		}
	}
	// Every header a client might send to claim an impersonator; none of them is read.
	forged := []string{"impersonator", "admin@example.com", "X-Auth-Request-Impersonator", "admin@example.com", "X-Impersonator", "admin@example.com"}

	plain := realm.token(tokenOpts{email: edgeOwner})
	noBanner("a token without the claim", get(edgeRequest(edgeTestSecret, edgeOwner, plain)))
	noBanner("a token without the claim and impersonator headers", get(edgeRequest(edgeTestSecret, edgeOwner, plain), forged...))
	noBanner("impersonator headers and no token", get(edgeRequest(edgeTestSecret, edgeOwner, ""), forged...))
	noBanner("impersonator headers that skipped the edge", get(edgeRequest("", edgeOwner, ""), forged...))
	noBanner("a claim on a token the realm did not sign", get(edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner, impersonator: "admin", key: other}))))
	noBanner("a claim on another user's token", get(edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: "someone@example.com", impersonator: "admin"}))))
	// An unauthorised caller (no edge secret, or a wrong one) with a real impersonated token, and a static-token caller (an
	// opaque bearer, not a realm JWT) with the headers: neither is a verified claim.
	imp := realm.token(tokenOpts{email: edgeOwner, impersonator: "matt"})
	noBanner("an impersonated token that skipped the edge", get(edgeRequest("", edgeOwner, imp)))
	noBanner("an impersonated token with a wrong edge secret", get(edgeRequest("not-the-edge-secret", edgeOwner, imp)))
	noBanner("a static token and impersonator headers", get(edgeRequest(edgeTestSecret, edgeOwner, "kd-static-api-token-0123456789"), forged...))

	body := get(edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner, impersonator: "matt"})), "impersonator", "forged@example.com")
	if !strings.Contains(body, ` data-impersonator="matt"`) {
		t.Error("an impersonated session's body does not say data-impersonator from the token's claim")
	}
	if strings.Contains(body, "forged@example.com") {
		t.Error("the page names the admin from a header")
	}
	if !strings.Contains(body, `You are <b>`+edgeOwner+`</b>, impersonated by <b>matt</b>`) {
		t.Error("the banner does not name the tenant and the admin behind them")
	}
	if !strings.Contains(body, `<a class="btn small imp-stop" href="`+html.EscapeString(defaultStopBecomeURL)+`">Stop</a>`) {
		t.Error("the banner has no Stop to the sign-out")
	}
}

// Stop signs out of the edge, then ends the gardenio realm's Keycloak session for the forwardauth client, then returns to
// /garden.
func TestDefaultStopBecomeURL(t *testing.T) {
	u, err := url.Parse(defaultStopBecomeURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://gardenio.clonk.tv/oauth2/sign_out" {
		t.Errorf("Stop starts at %s, want the edge's sign-out", u.Scheme+"://"+u.Host+u.Path)
	}
	end, err := url.Parse(u.Query().Get("rd"))
	if err != nil {
		t.Fatal(err)
	}
	if got := end.Scheme + "://" + end.Host + end.Path; got != "https://auth.clonk.tv/auth/realms/gardenio/protocol/openid-connect/logout" {
		t.Errorf("rd = %s, want the gardenio realm's end-session endpoint", got)
	}
	if q := end.Query(); q.Get("client_id") != "gardenio-traefik-forwardauth" || q.Get("post_logout_redirect_uri") != "https://gardenio.clonk.tv/garden" {
		t.Errorf("end-session query = %v, want the forwardauth client and /garden", q)
	}
}
