package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The demo land's door (lands_demo.go, gd-c6LzZkk0qy).

const demoBearer = "guest-access-token"

// demoGuest is a stranger signed in through GitHub, with no gardenio role.
func demoGuest() principal {
	p := applicantPrincipal()
	p.githubID, p.githubLogin = 4242, "Hubot"
	return p
}

type demoFixture struct {
	h     http.Handler
	srv   *Server
	d     *fakeIntake
	terms *fakeLandsTerms
}

// newDemoFixture is the viewer with a demo land "demo" that is open, and a
// caller who accepted the current terms.
func newDemoFixture(t *testing.T, env map[string]string) *demoFixture {
	t.Helper()
	d := newFakeIntake()
	d.beads["gd-demo"] = &beadsapi.BeadDetail{ID: "gd-demo", Type: "project", Title: "Project: demo", Status: "open", Fields: map[string]string{
		"land_kind": "tenant", "land_opened_at": "2026-10-20T18:00:00Z",
	}}
	d.order = append(d.order, "gd-demo")
	getenv := func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return map[string]string{landsDemoLandEnv: "demo", landsDemoDaemonURLEnv: "https://api.example.test/"}[k]
	}
	demo, err := landsDemoFromEnv(getenv, machineCheckingIntake{d}, d, staticTokenSource("machine-token"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || demo == nil {
		t.Fatalf("landsDemoFromEnv = %v, %v", demo, err)
	}
	demo.invites.now = func() time.Time { return ghNow }
	terms := &fakeLandsTerms{version: mustLandsLegal(t).Version, acceptedAt: "2026-10-19T15:20:00Z"}
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.landsTerms = terms
	srv.landsDemo = demo
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := operatorMiddleware(scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p }))
	return &demoFixture{h: h, srv: srv, d: d, terms: terms}
}

// demoAnswer is one response from the door.
type demoAnswer struct {
	Code     int
	Body     string
	Location string
}

func (f *demoFixture) do(method string, p principal) demoAnswer {
	var form url.Values
	if method == http.MethodPost {
		form = url.Values{}
	}
	rec := landsDo(f.h, method, "/lands/demo", p, demoBearer, form)
	return demoAnswer{Code: rec.Code, Body: rec.Body.String(), Location: rec.Header().Get("Location")}
}

func (f *demoFixture) passes() []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for _, id := range f.d.order {
		if b := f.d.beads[id]; b.Type == "land_invite" {
			out = append(out, b)
		}
	}
	return out
}

func TestLandsDemo_AskJoinedFlow(t *testing.T) {
	f := newDemoFixture(t, nil)
	guest := demoGuest()

	got := f.do(http.MethodGet, guest)
	if got.Code != http.StatusOK || !strings.Contains(got.Body, "Get a guest pass") || !strings.Contains(got.Body, "@Hubot") || !strings.Contains(got.Body, "24 hours") {
		t.Fatalf("GET before asking = %d:\n%s", got.Code, got.Body)
	}

	got = f.do(http.MethodPost, guest)
	if got.Code != http.StatusSeeOther || got.Location != "/lands/demo?asked=1" {
		t.Fatalf("POST = %d to %q:\n%s", got.Code, got.Location, got.Body)
	}
	passes := f.passes()
	if len(passes) != 1 {
		t.Fatalf("%d passes filed, want 1", len(passes))
	}
	b := passes[0]
	if len(b.Labels) != 1 || b.Labels[0] != "project:demo" {
		t.Errorf("labels = %v", b.Labels)
	}
	want := map[string]string{
		"land": "demo", "github_id": "4242", "github_login": "Hubot", "role": "guest", "state": "pending",
		"invited_by": "oidc:stranger@example.com", "expires_at": ghNow.Add(time.Hour).Format(time.RFC3339),
		"open_invite_cap": "50",
	}
	for k, v := range want {
		if b.Fields[k] != v {
			t.Errorf("%s = %q, want %q", k, b.Fields[k], v)
		}
	}

	// Asking again files nothing new and goes back to the page.
	if got = f.do(http.MethodPost, guest); got.Code != http.StatusSeeOther || len(f.passes()) != 1 {
		t.Fatalf("second POST = %d, %d passes", got.Code, len(f.passes()))
	}
	r := landsDo(f.h, http.MethodGet, "/lands/demo?asked=1", guest, demoBearer, nil)
	if body := r.Body.String(); !strings.Contains(body, "on its way") || !strings.Contains(body, "being set up") {
		t.Fatalf("GET while pending:\n%s", body)
	}

	// The provisioner joins them: the page gives the commands for the door.
	b.Fields["state"], b.Fields["joined_at"] = "joined", "2026-10-19T18:01:00Z"
	got = f.do(http.MethodGet, guest)
	for _, w := range []string{"You are a guest of <code>demo</code> until 20 October 2026, 18:01 UTC", "export BEADS_HTTP_URL=https://api.example.test/land/demo", "gb login", "gb ping", "kd list"} {
		if !strings.Contains(got.Body, w) {
			t.Errorf("GET when joined: missing %q", w)
		}
	}
}

func TestLandsDemo_Full(t *testing.T) {
	f := newDemoFixture(t, map[string]string{landsDemoMaxGuestsEnv: "1"})
	f.d.beads["gd-other"] = &beadsapi.BeadDetail{ID: "gd-other", Type: "land_invite", Status: "open", Labels: []string{"project:demo"},
		Fields: map[string]string{"land": "demo", "github_id": "7", "github_login": "someone", "role": "guest", "state": "joined"}}
	f.d.order = append(f.d.order, "gd-other")
	if got := f.do(http.MethodGet, demoGuest()); !strings.Contains(got.Body, "Every guest pass") {
		t.Fatalf("GET when full:\n%s", got.Body)
	}
	if got := f.do(http.MethodPost, demoGuest()); got.Code != http.StatusConflict || len(f.passes()) != 1 {
		t.Fatalf("POST when full = %d, %d passes", got.Code, len(f.passes()))
	}
	// A pass being revoked does not count.
	f.d.beads["gd-other"].Fields["revoke_requested_at"] = "2026-10-19T17:00:00Z"
	if got := f.do(http.MethodPost, demoGuest()); got.Code != http.StatusSeeOther || len(f.passes()) != 2 {
		t.Fatalf("POST after a revoke = %d, %d passes", got.Code, len(f.passes()))
	}
}

// otherReplica is the daemon as a second viewer replica meets it: on the
// create, a pass is filed first (as the other replica's press), and the daemon
// then answers as kbeads admitLandInvite does: the account's open pass, or a
// refusal at open_invite_cap (gd-GPvddfQY6y).
type otherReplica struct {
	machineCheckingIntake
	first func()
}

func (o otherReplica) CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	if o.first != nil {
		o.first()
	}
	var f struct {
		GitHubID string `json:"github_id"`
		Cap      int    `json:"open_invite_cap"`
	}
	_ = json.Unmarshal(req.Fields, &f)
	n := 0
	for _, id := range o.order {
		b := o.beads[id]
		if b.Type != "land_invite" || b.Fields["revoke_requested_at"] != "" {
			continue
		}
		if b.Fields["github_id"] == f.GitHubID {
			return b, nil
		}
		n++
	}
	if n >= f.Cap {
		return nil, &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "land demo holds open invites, the open_invite_cap this invite carries"}
	}
	return o.machineCheckingIntake.CreateBeadDetail(ctx, req)
}

func TestLandsDemo_AnotherReplicaFiledFirst(t *testing.T) {
	file := func(f *demoFixture, id, githubID string) func() {
		return func() {
			f.d.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "land_invite", Status: "open", Labels: []string{"project:demo"}, CreatedAt: ghNow,
				Fields: map[string]string{"land": "demo", "github_id": githubID, "github_login": "someone", "role": "guest", "state": "pending"}}
			f.d.order = append(f.d.order, id)
		}
	}
	ask := func(f *demoFixture, first func()) (*openInvite, error) {
		f.srv.landsDemo.invites.daemon = otherReplica{machineCheckingIntake{f.d}, first}
		return f.srv.landsDemo.ask(context.Background(), "oidc:stranger@example.com", 4242, "Hubot")
	}

	// The same person pressed on the other replica: one pass, theirs.
	f := newDemoFixture(t, nil)
	inv, err := ask(f, file(f, "gd-theirs", "4242"))
	if err != nil || inv == nil || inv.ID != "gd-theirs" || len(f.passes()) != 1 {
		t.Fatalf("same account: %+v, %v, %d passes", inv, err, len(f.passes()))
	}

	// Someone else took the last place there: full, and nothing filed.
	f = newDemoFixture(t, map[string]string{landsDemoMaxGuestsEnv: "1"})
	if inv, err := ask(f, file(f, "gd-last", "7")); !errors.Is(err, errLandsDemoFull) || inv != nil || len(f.passes()) != 1 {
		t.Fatalf("last place taken: %+v, %v, %d passes", inv, err, len(f.passes()))
	}

	// With room, the pass is filed and read from the daemon's answer.
	f = newDemoFixture(t, map[string]string{landsDemoMaxGuestsEnv: "2"})
	inv, err = ask(f, file(f, "gd-other", "7"))
	if err != nil || inv == nil || inv.ID == "gd-other" || inv.State != "pending" || inv.Login != "Hubot" || len(f.passes()) != 2 {
		t.Fatalf("with room: %+v, %v, %d passes", inv, err, len(f.passes()))
	}
}

func TestLandsDemo_Refusals(t *testing.T) {
	noGitHub := applicantPrincipal()
	for _, c := range []struct {
		name  string
		setup func(*demoFixture)
		p     principal
		code  int
		want  string
	}{
		{"terms not accepted", func(f *demoFixture) { f.terms.version = "" }, demoGuest(), http.StatusForbidden, "Read and accept the terms"},
		{"older terms", func(f *demoFixture) { f.terms.version = "2026-01-01-old" }, demoGuest(), http.StatusForbidden, "Read and accept the terms"},
		{"no GitHub account", nil, noGitHub, http.StatusForbidden, "did not come through GitHub"},
		{"land not open", func(f *demoFixture) { delete(f.d.beads["gd-demo"].Fields, "land_opened_at") }, demoGuest(), http.StatusServiceUnavailable, "no demo land on this server"},
		{"land closed", func(f *demoFixture) { f.d.beads["gd-demo"].Fields["land_closed_at"] = "2026-10-21T00:00:00Z" }, demoGuest(), http.StatusServiceUnavailable, "no demo land on this server"},
		{"no demo configured", func(f *demoFixture) { f.srv.landsDemo = nil }, demoGuest(), http.StatusServiceUnavailable, "no demo land on this server"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDemoFixture(t, nil)
			if c.setup != nil {
				c.setup(f)
			}
			if got := f.do(http.MethodGet, c.p); got.Code != http.StatusOK || !strings.Contains(got.Body, c.want) {
				t.Errorf("GET = %d, want 200 with %q:\n%s", got.Code, c.want, got.Body)
			}
			if got := f.do(http.MethodPost, c.p); got.Code != c.code || !strings.Contains(got.Body, c.want) {
				t.Errorf("POST = %d, want %d with %q", got.Code, c.code, c.want)
			}
			if n := len(f.passes()); n != 0 {
				t.Errorf("a refused ask filed %d passes", n)
			}
		})
	}

	// A form that says it is cross-site is refused before anything is read.
	f := newDemoFixture(t, nil)
	r := requestAs(http.MethodPost, "/lands/demo", demoGuest())
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("X-Auth-Request-Access-Token", demoBearer)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || len(f.passes()) != 0 {
		t.Fatalf("cross-site POST = %d, %d passes", rec.Code, len(f.passes()))
	}
}

func TestLandsDemo_HomeOffersItOnlyWhenConfigured(t *testing.T) {
	f := newDemoFixture(t, nil)
	rec := landsDo(f.h, http.MethodGet, "/lands/", principal{}, "", nil)
	if !strings.Contains(rec.Body.String(), `href="/lands/demo"`) {
		t.Errorf("home with a demo land does not link it")
	}
	if rec := landsDo(landsViewer(t, nil), http.MethodGet, "/lands/", principal{}, "", nil); strings.Contains(rec.Body.String(), "/lands/demo") {
		t.Errorf("home without a demo land links it")
	}
}

func TestLandsDemoFromEnv(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	if d, err := landsDemoFromEnv(env(), nil, nil, nil, nil); d != nil || err != nil {
		t.Fatalf("unset = %v, %v; want the door off", d, err)
	}
	d, err := landsDemoFromEnv(env(landsDemoLandEnv, "demo", landsDemoDaemonURLEnv, "https://api.example.test", landsDemoMaxGuestsEnv, "5", landsDemoGuestTTLEnv, "72h"), nil, nil, nil, nil)
	if err != nil || d.land != "demo" || d.daemonURL != "https://api.example.test" || d.maxGuests != 5 || d.guestTTL != 72*time.Hour {
		t.Fatalf("configured = %+v, %v", d, err)
	}
	if got := landsDemoDuration(d.guestTTL); got != "3 days" {
		t.Errorf("72h reads %q", got)
	}
	for name, kv := range map[string][]string{
		"bad land":    {landsDemoLandEnv, "Demo!", landsDemoDaemonURLEnv, "https://api.example.test"},
		"reserved":    {landsDemoLandEnv, "claimlayer", landsDemoDaemonURLEnv, "https://api.example.test"},
		"no url":      {landsDemoLandEnv, "demo"},
		"http url":    {landsDemoLandEnv, "demo", landsDemoDaemonURLEnv, "http://api.example.test"},
		"zero guests": {landsDemoLandEnv, "demo", landsDemoDaemonURLEnv, "https://api.example.test", landsDemoMaxGuestsEnv, "0"},
		"short ttl":   {landsDemoLandEnv, "demo", landsDemoDaemonURLEnv, "https://api.example.test", landsDemoGuestTTLEnv, "10m"},
	} {
		if _, err := landsDemoFromEnv(env(kv...), nil, nil, nil, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every state renders, inside the page.
func TestLandsDemo_EveryStateRenders(t *testing.T) {
	f := newDemoFixture(t, nil)
	for _, st := range landsDemoStates {
		v, ok := f.srv.landsBase(requestAs(http.MethodGet, "/lands/demo", demoGuest()), "Try the demo")
		if !ok {
			t.Fatal("landsBase failed")
		}
		v.Demo = landsDemoView{State: st, Land: "demo", DaemonURL: "https://api.example.test", GuestTTL: "24 hours", Login: "Hubot"}
		rec := httptest.NewRecorder()
		f.srv.renderLands(rec, http.StatusOK, "lands_demo", v)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, "<h1>Try the demo</h1>") || !strings.Contains(body, "</main>") {
			t.Errorf("state %s: %d", st, rec.Code)
		}
		if strings.Count(body, `class="lands-`) < 3 {
			t.Errorf("state %s rendered nearly nothing", st)
		}
	}
}
