package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const edgeTestSecret = "edge-s3cret"

// edgeChain is the live viewer chain behind the forwardauth edge: dev bypass
// with proxySecret, then operator, machine and scope, over the full route
// table, with who standing in for the daemon's /v1/me.
func edgeChain(t *testing.T, proxySecret string, who machineWho) http.Handler {
	t.Helper()
	m := registeredViewerRoutes(t)
	patternOf := func(r *http.Request) string { _, p := m.mux.Handler(r); return p }
	var h http.Handler = scopeMiddleware(m.mux, patternOf)
	h = machineMiddleware(h, patternOf, who, testLogger())
	h = operatorMiddleware(h)
	return devBypassMiddleware(testLogger(), proxySecret, nil)(h)
}

// TestEdgeProof_DirectCallReachesOnlyPublicAndMachine (gd-ozc7DSwCXO): with
// the edge secret set, a pod that dials the viewer Service directly, without
// the secret, gets 403 on every route but the public and machine ones. With
// no identity its scope is unrestricted, so a hub route such as
// /garden/events would otherwise stream it every land's beads.
func TestEdgeProof_DirectCallReachesOnlyPublicAndMachine(t *testing.T) {
	me := &fakeMe{answers: map[string]*beadsapi.MeResponse{
		"bridge-sa":    {OperatorID: "machine:gardenio-slack-bridge", Source: "token"},
		"land-b-agent": {OperatorID: "land:landb", Source: "token", TokenProject: "landb"},
	}}
	h := edgeChain(t, edgeTestSecret, me.who)

	direct := func(method, path string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	refused := []string{"/garden", "/garden/events", "/garden/farm/events", "/garden/state", "/index", "/inbox", "/feed", "/feed/events", "/active", "/garden/c", "/lands/accept"}
	callers := map[string]map[string]string{
		"no credential":      nil,
		"own bearer":         {"Authorization": "Bearer land-b-agent"},
		"forged email":       {"X-Auth-Request-Email": "operator@acme.example"},
		"wrong edge secret":  {devAuthProxySecretHeader: "nope", "X-Auth-Request-Email": "operator@acme.example"},
		"machine bearer":     {"Authorization": "Bearer bridge-sa"},
		"bearer and forgery": {"Authorization": "Bearer land-b-agent", "X-Auth-Request-Email": "operator@acme.example"},
	}
	for name, hdr := range callers {
		for _, path := range refused {
			if code := serveReq(h, direct(http.MethodGet, path, hdr)); code != http.StatusForbidden {
				t.Errorf("%s: GET %s = %d, want 403", name, path, code)
			}
		}
		if code := serveReq(h, direct(http.MethodPost, "/b/gd-1/close", hdr)); code != http.StatusForbidden {
			t.Errorf("%s: POST /b/gd-1/close = %d, want 403", name, code)
		}
	}

	// The public routes stay open, and the machine route still serves a
	// machine principal on its own bearer (the slack-bridge, 3.14).
	for _, path := range []string{"/assets/static/app.css", "/manifest.webmanifest", "/lands/terms"} {
		if code := serveReq(h, direct(http.MethodGet, path, nil)); code != http.StatusOK {
			t.Errorf("public GET %s = %d, want 200", path, code)
		}
	}
	if code := serveReq(h, direct(http.MethodGet, machineFilePath, map[string]string{"Authorization": "Bearer bridge-sa"})); code != http.StatusOK {
		t.Errorf("machine principal on the machine route = %d, want 200", code)
	}
	if code := serveReq(h, direct(http.MethodGet, machineFilePath, map[string]string{"Authorization": "Bearer land-b-agent"})); code != http.StatusForbidden {
		t.Errorf("land token on the machine route = %d, want 403", code)
	}
}

// Through the edge (valid secret) nothing changes: the stamped human reaches
// the admin routes as before.
func TestEdgeProof_EdgeRequestUnchanged(t *testing.T) {
	h := edgeChain(t, edgeTestSecret, (&fakeMe{}).who)
	for _, path := range []string{"/garden", "/garden/state", "/index", "/active"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(devAuthProxySecretHeader, edgeTestSecret)
		r.Header.Set("X-Auth-Request-Email", "operator@acme.example")
		if code := serveReq(h, r); code != http.StatusOK {
			t.Errorf("edge GET %s = %d, want 200", path, code)
		}
	}
}

// With no edge secret configured (local dev, where the operator's own machine
// is the only route to the pod) a request is not marked and keeps today's
// behaviour.
func TestEdgeProof_NoSecretConfiguredUnchanged(t *testing.T) {
	h := edgeChain(t, "", (&fakeMe{}).who)
	for _, path := range []string{"/garden", "/index", "/active"} {
		if code := serveReq(h, httptest.NewRequest(http.MethodGet, path, nil)); code != http.StatusOK {
			t.Errorf("local dev GET %s = %d, want 200", path, code)
		}
	}
}
