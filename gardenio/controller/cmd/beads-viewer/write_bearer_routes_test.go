package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// Boundary step 1 (gd-eXNRKIIyzD, recommendation gd-uYNbyW4a9Z §6): with
// BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES on, no viewer write reaches the daemon
// unless it carries the signed-in user's own bearer, on every write route of
// every route class. The static BEADS_DAEMON_TOKEN is then a read credential
// only (viewer role in effect): it must never authenticate a mutation.
//
// Unlike route_classes_test.go, which stubs every handler, this walk runs the
// REAL handlers behind the production middleware order (operator -> scope ->
// mux) over a real beadsapi.Client pointed at a recording fake daemon, so the
// gate under test is the one in the wire path, not a stand-in. Reads stay on
// the static token (read enforcement off) so a handler gets as far as its
// write before the gate decides.

const (
	writeGateStaticToken = "static-daemon-token"
	writeGateUserBearer  = "user-jwt-writes"
)

// writeGateCall is one request the fake daemon saw.
type writeGateCall struct {
	method, path, auth string
}

// writeGateDaemon is an httptest daemon that answers every read with a minimal
// body and records every request with its Authorization header.
type writeGateDaemon struct {
	mu    sync.Mutex
	calls []writeGateCall
}

var writeGateBeadPath = regexp.MustCompile(`^/v1/beads/([^/]+)$`)

func (d *writeGateDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.calls = append(d.calls, writeGateCall{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if m := writeGateBeadPath.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet && m[1] == writeGatePlan {
		// The form's plan reads as an open plan, so meadow adoption (gardenhub adopt.go) gets as far as its write.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": m[1], "title": "write gate plan", "type": "epic", "status": "open",
			"labels": []string{"project:claimlayer", "plan"},
		})
		return
	}
	if m := writeGateBeadPath.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": m[1], "title": "write gate fixture", "type": "task", "status": "open",
			"labels": []string{"project:claimlayer"},
		})
		return
	}
	_, _ = w.Write([]byte(`{}`))
}

// writes returns the mutating calls recorded since index from, excluding the
// read-only POST endpoints the client deliberately does not gate.
func (d *writeGateDaemon) writes(from int) []writeGateCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []writeGateCall
	for _, c := range d.calls[from:] {
		switch c.method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		if c.path == "/v1/search/semantic" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (d *writeGateDaemon) mark() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// writeRoutePatterns returns every registered pattern that accepts a write:
// POST/PUT/PATCH/DELETE, plus method-less patterns (they accept POST too).
func writeRoutePatterns(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range registeredViewerRoutes(t).patterns {
		method, _, hasMethod := strings.Cut(p, " ")
		if !hasMethod {
			out = append(out, p)
			continue
		}
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

var writeGateWildcard = regexp.MustCompile(`\{([^}]*)\}`)

// writeRouteRequest turns a mux pattern into a concrete method and path. A
// project wildcard names the scoped principal's own project, so a scoped write
// route (4.2 land roles moves some there) reaches its handler too.
func writeRouteRequest(pattern string) (string, string) {
	method, path, hasMethod := strings.Cut(pattern, " ")
	if !hasMethod {
		method, path = http.MethodPost, pattern
	}
	path = writeGateWildcard.ReplaceAllStringFunc(path, func(seg string) string {
		switch name := strings.Trim(seg, "{}"); {
		case name == "$":
			return ""
		case name == "project" || name == "name":
			return "claimlayer"
		case strings.HasSuffix(name, "..."):
			return "x/y"
		case name == "id" || name == "decision" || name == "step" || name == "doc":
			return "gd-wg" + name
		default:
			return "x"
		}
	})
	return method, path
}

// writeGateForm is a body generous enough that most handlers pass their input
// validation and reach the daemon write the gate is meant to stop.
var writeGateForm = url.Values{
	"text": {"x"}, "body": {"x"}, "comment": {"x"}, "message": {"x"}, "title": {"x"},
	"description": {"x"}, "reason": {"x"}, "status": {"closed"}, "priority": {"2"},
	"assignee": {"someone"}, "owner": {"someone"}, "label": {"x"}, "labels": {"x"},
	"chosen": {"1"}, "choice": {"1"}, "option": {"1"}, "rating": {"4"}, "name": {"x"},
	"project": {"claimlayer"}, "parent": {"gd-wgparent"}, "value": {"x"}, "plan": {writeGatePlan},
}.Encode()

// writeGatePlan is the form's plan, the one bead the fake daemon reads as a plan.
const writeGatePlan = "gd-wgplan"

// newWriteGateViewer wires the viewer the way main.go does with write-bearer
// enforcement on and a static token present.
func newWriteGateViewer(t *testing.T) (http.Handler, *writeGateDaemon) {
	t.Helper()
	d := &writeGateDaemon{}
	ts := httptest.NewServer(d)
	t.Cleanup(ts.Close)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: ts.URL, Token: writeGateStaticToken})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	client.SetRequireUserBearerForWrites(true)
	srv := NewServer(client, testLogger(), "", ts.URL, writeGateStaticToken, &fakeScreenClient{})
	// main.go wires the garden handler with the daemon client as its reader and linker, which registers its write
	// (POST /garden/bead/{id}/adopt); without it the walk would 404 that route and prove nothing about it.
	srv.SetGardenHandler(&gardenhub.Handler{Beads: client, Links: client})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	var h http.Handler = mux
	h = scopeMiddleware(h, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	h = operatorMiddleware(h)
	return h, d
}

// serveWrite sends one write as p, with bearer as the forwarded access token
// ("" forwards none, like the plain ziti listener or an AVA edge). A handler
// that panics on the fixture is reported, not fatal: the walk is about what
// reached the daemon.
func serveWrite(t *testing.T, h http.Handler, pattern string, p principal, bearer string) {
	t.Helper()
	method, path := writeRouteRequest(pattern)
	r := httptest.NewRequest(method, path, strings.NewReader(writeGateForm))
	r = r.WithContext(requestAs(method, path, p).Context())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	if bearer != "" {
		r.Header.Set("X-Auth-Request-Access-Token", bearer)
	}
	defer func() {
		if v := recover(); v != nil {
			t.Logf("%s (bearer=%t): handler panicked on the fixture: %v", pattern, bearer != "", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), r)
}

// TestWriteBearer_EveryWriteRouteRefusedWithoutUserBearer is the refusal half:
// every write route, as each principal that can reach its class, with no
// forwarded bearer, sends NO mutation to the daemon.
func TestWriteBearer_EveryWriteRouteRefusedWithoutUserBearer(t *testing.T) {
	h, d := newWriteGateViewer(t)
	patterns := writeRoutePatterns(t)
	classes := map[routeClass]int{}
	for _, pattern := range patterns {
		classes[viewerRouteClasses[pattern]]++
		for _, p := range []principal{adminPrincipal(), scopedPrincipal()} {
			from := d.mark()
			serveWrite(t, h, pattern, p, "")
			for _, c := range d.writes(from) {
				t.Errorf("%s as %s with no user bearer reached the daemon: %s %s (Authorization %q)",
					pattern, p.email, c.method, c.path, c.auth)
			}
		}
	}
	if len(patterns) < 50 {
		t.Fatalf("walked only %d write routes; the walk is not seeing the route table", len(patterns))
	}
	t.Logf("walked %d write routes by class: %v", len(patterns), classes)
}

// TestWriteBearer_WritesGoOutAsTheUserNeverTheStaticToken is the positive
// control that keeps the refusal walk honest, and the downscope proof: with a
// bearer forwarded, the same walk does reach the daemon, and every mutation
// carries the user's bearer, never the static token.
func TestWriteBearer_WritesGoOutAsTheUserNeverTheStaticToken(t *testing.T) {
	h, d := newWriteGateViewer(t)
	reached := 0
	for _, pattern := range writeRoutePatterns(t) {
		from := d.mark()
		serveWrite(t, h, pattern, adminPrincipal(), writeGateUserBearer)
		writes := d.writes(from)
		if len(writes) > 0 {
			reached++
		}
		for _, c := range writes {
			if c.auth != "Bearer "+writeGateUserBearer {
				t.Errorf("%s: daemon write %s %s authenticated as %q, want the user's bearer",
					pattern, c.method, c.path, c.auth)
			}
		}
	}
	// Enough routes must actually write for the refusal walk to mean anything.
	if reached < 20 {
		t.Errorf("only %d write routes reached a daemon write with a bearer; the fixture is too thin for the refusal walk to prove anything", reached)
	}
	t.Logf("%d write routes reached a daemon write with the user's bearer", reached)
}

// The walk reaches meadow adoption's write (gd-6HQyB0jgmO): with the user's bearer, POST /garden/bead/{id}/adopt
// writes its one dependency as the user; with none, nothing reaches the daemon. Without the garden handler wired in
// newWriteGateViewer the route would 404 and both walks above would pass it vacuously.
func TestWriteBearer_GardenAdoptIsWalked(t *testing.T) {
	const pattern = "POST /garden/bead/{id}/adopt"
	h, d := newWriteGateViewer(t)
	from := d.mark()
	serveWrite(t, h, pattern, adminPrincipal(), writeGateUserBearer)
	writes := d.writes(from)
	if len(writes) != 1 || !strings.HasSuffix(writes[0].path, "/dependencies") || writes[0].auth != "Bearer "+writeGateUserBearer {
		t.Fatalf("%s with the user's bearer: daemon writes %+v, want one dependency write as the user", pattern, writes)
	}
	from = d.mark()
	serveWrite(t, h, pattern, adminPrincipal(), "")
	if writes := d.writes(from); len(writes) != 0 {
		t.Fatalf("%s with no user bearer reached the daemon: %+v", pattern, writes)
	}
}
