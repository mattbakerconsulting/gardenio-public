package tests

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The viewer front door, in real nginx (gd-BQExi3UZrc, the spoofing check of
// gd-epq9jTtIxV): public-edge.yaml's gardenio.clonk.tv server block, then
// viewer-authproxy.yaml's 8097 listener, then a stub viewer. The identity the
// viewer gets must be the one forwardauth verified, never one the client sent,
// and a request with no session must not reach the viewer at all.
//
// nginx comes from $EDGE_NGINX or PATH, as for the land door tests.

const (
	frontDoorSession     = "_oauth2_proxy=signed-in-owner"
	frontDoorEmail       = "owner@example.com"
	frontDoorToken       = "owner-access-token"
	frontDoorProxySecret = "edge-proxy-secret"
	// A GitHub sign-in with no invite and no group, and one whose invite was
	// claimed (/invited only; gd-ycE484kLyk).
	frontDoorStranger = "_oauth2_proxy=signed-in-stranger"
	frontDoorInvitee  = "_oauth2_proxy=signed-in-invitee"
)

// frontDoorForwardauth is oauth2-proxy's /oauth2/auth with a cookie session
// and --set-xauthrequest --pass-access-token: 202 plus the verified identity
// for the signed-in cookie, 401 for anything else (a bearer never signs in:
// the edge blanks Authorization).
func frontDoorForwardauth(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// oauth2-proxy answers 403 to a session in none of allowed_groups.
		allowed := strings.Split(r.URL.Query().Get("allowed_groups"), ",")
		cookie := r.Header.Get("Cookie")
		switch {
		case r.Header.Get("Authorization") != "":
			w.WriteHeader(http.StatusUnauthorized)
			return
		case strings.Contains(cookie, frontDoorStranger) && allowed[0] != "",
			strings.Contains(cookie, frontDoorInvitee) && allowed[0] != "" && !slices.Contains(allowed, "/invited"):
			w.WriteHeader(http.StatusForbidden)
			return
		case strings.Contains(cookie, frontDoorStranger), strings.Contains(cookie, frontDoorInvitee):
		case !strings.Contains(cookie, frontDoorSession):
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Auth-Request-Email", frontDoorEmail)
		w.Header().Set("X-Auth-Request-Access-Token", frontDoorToken)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// frontDoorForged is every identity header a client might dress a request in.
var frontDoorForged = []string{
	"Authorization", "X-Auth-Request-Email", "X-Auth-Request-Access-Token", "X-Auth-Request-User",
	"X-Auth-Request-Preferred-Username", "X-Forwarded-Email", "X-Authproxy-Secret",
}

// httpLevelOf returns an nginx conf's http-level directives: everything but
// its server blocks, less TLS and the cluster resolver.
func httpLevelOf(conf string) string {
	for {
		loc := regexp.MustCompile(`(?m)^    server \{`).FindStringIndex(conf)
		if loc == nil {
			break
		}
		body := nginxBlock(conf[loc[0]:], "server")
		conf = conf[:loc[0]] + conf[loc[0]+len("    server {")+len(body)+1:]
	}
	var kept []string
	for line := range strings.SplitSeq(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ssl_") || strings.HasPrefix(trimmed, "resolver ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// replaceAll applies each old->new pair to s, failing when old is absent.
func replaceAll(t *testing.T, what, s string, pairs ...string) string {
	t.Helper()
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			t.Fatalf("%s has no %q", what, pairs[i])
		}
		s = strings.ReplaceAll(s, pairs[i], pairs[i+1])
	}
	return s
}

type frontDoorHarness struct {
	url    string
	viewer *edgeStub
	// The edge's and viewer-authproxy's access logs (each "redacted").
	edgeLog, proxyLog string
}

func startFrontDoor(t *testing.T) *frontDoorHarness {
	t.Helper()
	bin := edgeNginxBinary(t)
	h := &frontDoorHarness{
		viewer: newEdgeStub(t, func(r *http.Request) (int, string) { return http.StatusOK, "viewer:" + r.URL.Path }),
	}
	// viewer-authproxy's 8097 listener, envsubst done by hand.
	proxyDoc := readEdgeFile(t, "viewer-authproxy.yaml")
	_, tmpl, ok := strings.Cut(proxyDoc, "  default.conf.template: |\n")
	if !ok {
		t.Fatal("viewer-authproxy.yaml: no default.conf.template")
	}
	tmpl, _, _ = strings.Cut(tmpl, "\n---")
	proxyPort := freePort(t)
	var viewerServer string
	for _, m := range regexp.MustCompile(`(?m)^    server \{`).FindAllStringIndex(tmpl, -1) {
		if body := nginxBlock(tmpl[m[0]:], "server"); strings.Contains(body, "listen 8097;") {
			viewerServer = body
		}
	}
	if viewerServer == "" {
		t.Fatal("viewer-authproxy.yaml: no listen 8097 server")
	}
	logs := t.TempDir()
	h.edgeLog, h.proxyLog = filepath.Join(logs, "edge.log"), filepath.Join(logs, "proxy.log")
	viewerServer = replaceAll(t, "viewer-authproxy 8097", viewerServer,
		"listen 8097;", fmt.Sprintf("listen 127.0.0.1:%d;", proxyPort),
		"access_log /dev/stdout redacted;", "access_log "+h.proxyLog+" redacted;",
		"${VIEWER_PROXY_SECRET}", frontDoorProxySecret,
		"gardenio-beads-viewer.gardenio.svc.cluster.local:8095", h.viewer.addr(),
	)
	runNginx(t, bin, httpLevelOf(tmpl), viewerServer, proxyPort)

	// The edge's gardenio.clonk.tv block in front of it.
	edgeDoc := readEdgeFile(t, "public-edge.yaml")
	_, conf, ok := strings.Cut(edgeDoc, "  default.conf: |\n")
	if !ok {
		t.Fatal("public-edge.yaml: no default.conf")
	}
	conf, _, _ = strings.Cut(conf, "\n---")
	edgeHTTP := replaceAll(t, "public-edge http level", httpLevelOf(conf),
		// The harness sends far more than 5 requests a second from one address.
		"zone=front_door_per_ip:10m rate=5r/s", "zone=front_door_per_ip:10m rate=10000r/s",
	)
	edgePort := freePort(t)
	edge := replaceAll(t, "gardenio.clonk.tv block", edgeServer(t, edgeDoc, "gardenio.clonk.tv"),
		"listen 8443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d;", edgePort),
		"http2 on;", "",
		"access_log /dev/stdout redacted;", "access_log "+h.edgeLog+" redacted;",
		"gardenio-forwardauth.gardenio.svc.cluster.local:4180", frontDoorForwardauth(t),
		"viewer-authproxy.gardenio.svc.cluster.local:8097", fmt.Sprintf("127.0.0.1:%d", proxyPort),
	)
	h.url = runNginx(t, bin, edgeHTTP, edge, edgePort)
	return h
}

// do sends one request to the edge as gardenio.clonk.tv, signed in or not,
// with every identity header forged, and returns the response.
func (h *frontDoorHarness) do(t *testing.T, method, path string, signedIn bool, extra ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.url+path, strings.NewReader("labels=project:x"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "gardenio.clonk.tv"
	for _, name := range frontDoorForged {
		req.Header.Set(name, "forged")
	}
	req.Header.Set("X-Auth-Request-Email", "attacker@example.com")
	req.Header.Set("Authorization", "Bearer forged")
	if signedIn {
		req.Header.Set("Cookie", frontDoorSession)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// noForgedValue fails if any header the viewer got carries a forged value.
func noForgedValue(t *testing.T, r *http.Request) {
	t.Helper()
	for name, vals := range r.Header {
		for _, v := range vals {
			if strings.Contains(v, "forged") || strings.Contains(v, "attacker@") {
				t.Errorf("%s %s: viewer got client-forged %s: %q", r.Method, r.URL.Path, name, v)
			}
		}
	}
}

func TestViewerFrontDoorNginx_SignedInForgedIdentityIsOverwritten(t *testing.T) {
	t.Parallel()
	h := startFrontDoor(t)
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodPost, "/b/gd-XOnJ3PSDsn/labels"},
		{http.MethodPost, "/b/gd-XOnJ3PSDsn/escalation/approve"},
		{http.MethodGet, "/lands/accept"},
	} {
		if resp := h.do(t, p.method, p.path, true); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s signed in: %d, want 200 from the viewer", p.method, p.path, resp.StatusCode)
		}
		reqs := h.viewer.take()
		if len(reqs) != 1 {
			t.Fatalf("%s %s: viewer got %d requests, want 1", p.method, p.path, len(reqs))
		}
		r := reqs[0]
		for name, want := range map[string]string{
			"X-Auth-Request-Email":              frontDoorEmail,
			"X-Auth-Request-Access-Token":       frontDoorToken,
			"Authorization":                     "Bearer " + frontDoorToken,
			"X-Authproxy-Secret":                frontDoorProxySecret,
			"X-Auth-Request-User":               "",
			"X-Auth-Request-Preferred-Username": "",
			"X-Forwarded-Email":                 "",
		} {
			if got := r.Header.Values(name); (want == "" && len(got) != 0) || (want != "" && (len(got) != 1 || got[0] != want)) {
				t.Errorf("%s %s: viewer got %s=%q, want %q", p.method, p.path, name, got, want)
			}
		}
		noForgedValue(t, r)
	}
}

func TestViewerFrontDoorNginx_NoSessionNeverReachesTheViewer(t *testing.T) {
	t.Parallel()
	h := startFrontDoor(t)
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/me"},
		{http.MethodPost, "/b/gd-XOnJ3PSDsn/labels"},
		{http.MethodPost, "/b/gd-XOnJ3PSDsn/escalation/approve"},
		{http.MethodGet, "/lands/accept"},
	} {
		// A navigation is sent to sign in.
		resp := h.do(t, p.method, p.path, false)
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "https://gardenio.clonk.tv/oauth2/start?") {
			t.Errorf("%s %s no session: %d Location %q, want 302 to /oauth2/start", p.method, p.path, resp.StatusCode, loc)
		}
		// A script gets a bare 401.
		if resp := h.do(t, p.method, p.path, false, "HX-Request", "true"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s no session, htmx: %d, want 401", p.method, p.path, resp.StatusCode)
		}
		if reqs := h.viewer.take(); len(reqs) != 0 {
			t.Errorf("%s %s no session: viewer got %d requests, want 0", p.method, p.path, len(reqs))
		}
	}
}

// The no-session front-of-house pages reach the viewer with no identity at
// all: the client's forged headers are blanked, not passed through.
func TestViewerFrontDoorNginx_PublicPagesCarryNoIdentity(t *testing.T) {
	t.Parallel()
	h := startFrontDoor(t)
	for _, path := range []string{"/lands/", "/lands/terms"} {
		if resp := h.do(t, http.MethodGet, path, false); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d, want 200", path, resp.StatusCode)
		}
		reqs := h.viewer.take()
		if len(reqs) != 1 {
			t.Fatalf("GET %s: viewer got %d requests, want 1", path, len(reqs))
		}
		for _, name := range []string{"X-Auth-Request-Email", "X-Auth-Request-Access-Token", "Authorization", "Cookie"} {
			if got := reqs[0].Header.Values(name); len(got) != 0 {
				t.Errorf("GET %s: viewer got %s=%q, want none", path, name, got)
			}
		}
		noForgedValue(t, reqs[0])
	}
}

// The Window (plan gd-pz87vf3xsB, PW.5): /w/<token> reaches the viewer signed
// in or not, with the edge secret and no identity of any kind; a POST is
// refused at the edge; every answer says no-referrer and noindex; and neither
// access log holds the token, however the path is spelled.
func TestViewerFrontDoorNginx_WindowCarriesNoIdentityAndLogsNoToken(t *testing.T) {
	t.Parallel()
	h := startFrontDoor(t)
	const secret = "Zm9yZ2VkLXdpbmRvdy10b2tlbg" // gitleaks:allow: forged test token
	token := "gw_" + secret

	for _, signedIn := range []bool{false, true} {
		for _, path := range []string{"/w/" + token, "/w/" + token + "/plan.json?v=" + token} {
			resp := h.do(t, http.MethodGet, path, signedIn, "Referer", "https://gardenio.clonk.tv/w/"+token)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s signed in %v: %d, want 200 from the viewer", path, signedIn, resp.StatusCode)
			}
			for name, want := range map[string]string{"Referrer-Policy": "no-referrer", "X-Robots-Tag": "noindex, nofollow"} {
				if got := resp.Header.Values(name); len(got) != 1 || got[0] != want {
					t.Errorf("GET %s: %s=%q, want %q", path, name, got, want)
				}
			}
			reqs := h.viewer.take()
			if len(reqs) != 1 {
				t.Fatalf("GET %s: viewer got %d requests, want 1", path, len(reqs))
			}
			r := reqs[0]
			if got := r.Header.Values("X-Authproxy-Secret"); len(got) != 1 || got[0] != frontDoorProxySecret {
				t.Errorf("GET %s: viewer got X-Authproxy-Secret=%q, want the proxy secret", path, got)
			}
			for _, name := range []string{"X-Auth-Request-Email", "X-Auth-Request-Access-Token", "Authorization", "Cookie", "X-Auth-Request-User", "X-Auth-Request-Preferred-Username", "X-Forwarded-Email"} {
				if got := r.Header.Values(name); len(got) != 0 {
					t.Errorf("GET %s signed in %v: viewer got %s=%q, want none", path, signedIn, name, got)
				}
			}
			noForgedValue(t, r)
		}
	}

	if resp := h.do(t, http.MethodPost, "/w/"+token, false); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /w/<token>: %d, want 403 at the edge", resp.StatusCode)
	}
	if reqs := h.viewer.take(); len(reqs) != 0 {
		t.Errorf("POST /w/<token>: viewer got %d requests, want 0", len(reqs))
	}

	// Spellings nginx normalises onto ^~ /w/, sent byte for byte (a Go
	// client would rewrite them).
	for _, raw := range []string{"//w/" + token, "/%77/" + token + "/plan.json"} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(h.url, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: gardenio.clonk.tv\r\nConnection: close\r\n\r\n", raw)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		conn.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", raw, resp.StatusCode)
		}
	}
	h.viewer.take()

	// nginx writes the access log after the reply, so give it a moment.
	// Seven requests reached the edge, six of them viewer-authproxy.
	for _, l := range []struct {
		name, path string
		lines      int
	}{{"edge", h.edgeLog, 7}, {"viewer-authproxy", h.proxyLog, 6}} {
		name := l.name
		var log string
		for range 50 {
			raw, _ := os.ReadFile(l.path)
			if log = string(raw); strings.Count(log, "\n") >= l.lines {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if strings.Contains(log, secret) {
			t.Errorf("%s access log holds the window token:\n%s", name, log)
		}
		for _, want := range []string{`"GET /w/- HTTP/1.1"`, `"GET /w/-/plan.json HTTP/1.1"`, `"POST /w/- HTTP/1.1" 403`, `"/w/-"`} {
			if name == "viewer-authproxy" && strings.HasPrefix(want, `"POST`) {
				continue // refused at the edge
			}
			if !strings.Contains(log, want) {
				t.Errorf("%s access log has no %s:\n%s", name, want, log)
			}
		}
	}
}

// GitHub sign-in is invite-only (gd-ycE484kLyk). A signed-in stranger asking
// for /lands/* gets the "You need an invite" page with a 403, never a
// redirect, and never reaches the viewer; a script gets the bare 403. The
// demo land's door, = /lands/demo and = /lands/accept, is the exception
// (gd-T37nCeWMhq = open): a stranger reaches the viewer there. The public
// landing page still answers, so the sign-in round trip has somewhere to end.
// Once the invite is claimed (/invited), /lands/* answers; the viewer root
// still sends them to /lands/. A member the viewer admits is unaffected.
func TestViewerFrontDoorNginx_LandsIsInviteOnly(t *testing.T) {
	t.Parallel()
	h := startFrontDoor(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path, cookie string, extra ...string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, h.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "gardenio.clonk.tv"
		req.Header.Set("Cookie", cookie)
		for i := 0; i+1 < len(extra); i += 2 {
			req.Header.Set(extra[i], extra[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	for _, path := range []string{"/lands/apply", "/lands/demo/x", "/lands/accepted"} {
		resp, body := get(path, frontDoorStranger, "Sec-Fetch-Mode", "navigate")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "You need an invite") ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("stranger %s: %d %q %v\n%s", path, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header, body)
		}
		resp, body = get(path, frontDoorStranger, "HX-Request", "true", "Sec-Fetch-Mode", "cors")
		if resp.StatusCode != http.StatusForbidden || strings.Contains(body, "You need an invite") {
			t.Errorf("stranger script %s: %d\n%s", path, resp.StatusCode, body)
		}
	}
	if got := h.viewer.take(); len(got) != 0 {
		t.Fatalf("an uninvited sign-in reached the viewer: %v", got)
	}
	for _, path := range []string{"/lands/demo", "/lands/accept"} {
		if resp, body := get(path, frontDoorStranger, "Sec-Fetch-Mode", "navigate"); resp.StatusCode != http.StatusOK || body != "viewer:"+path {
			t.Errorf("stranger %s (demo door): %d %s", path, resp.StatusCode, body)
		}
	}
	h.viewer.take()
	if resp, _ := get("/", frontDoorStranger, "Sec-Fetch-Mode", "navigate"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://gardenio.clonk.tv/lands/" {
		t.Errorf("stranger /: %d %s, want 302 to /lands/", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := get("/lands/", frontDoorStranger, "Sec-Fetch-Mode", "navigate"); resp.StatusCode != http.StatusOK || body != "viewer:/lands/" {
		t.Errorf("stranger /lands/ (public): %d %s", resp.StatusCode, body)
	}
	h.viewer.take()

	if resp, body := get("/lands/accept", frontDoorInvitee, "Sec-Fetch-Mode", "navigate"); resp.StatusCode != http.StatusOK || body != "viewer:/lands/accept" {
		t.Errorf("invitee /lands/accept: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get("/", frontDoorInvitee, "Sec-Fetch-Mode", "navigate"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://gardenio.clonk.tv/lands/" {
		t.Errorf("invitee /: %d %s, want 302 to /lands/ (the viewer root does not admit /invited)", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, path := range []string{"/", "/lands/accept"} {
		if resp, body := get(path, frontDoorSession, "Sec-Fetch-Mode", "navigate"); resp.StatusCode != http.StatusOK || body != "viewer:"+path {
			t.Errorf("member %s: %d %s", path, resp.StatusCode, body)
		}
	}
}
