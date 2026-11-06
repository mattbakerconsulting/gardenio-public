package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gardenio/clicreds/oidclogin"
)

func TestReleaseList(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /releases", 200, "ReleasePage/first.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "release", "list", "--limit", "2")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if q := f.requests()[0].query; q != "limit=2" {
		t.Errorf("query = %q", q)
	}
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "TAG") {
		t.Fatalf("output:\n%s", r.out)
	}
	for i, want := range []string{"v1.2.0  cut  2026-10-25 08:00 UTC  2    -", "v1.1.0  live  2026-10-24 09:00 UTC  1    prod"} {
		if strings.Join(strings.Fields(lines[i+1]), " ") != strings.Join(strings.Fields(want), " ") {
			t.Errorf("line %d = %q, want %q", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(r.err, "(more: --cursor ") {
		t.Errorf("no more hint: %q", r.err)
	}
}

func TestReleaseShow(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /releases/v1.0.0", 200, "Release/superseded.json")
	f.answer("GET /releases/v1.2.1", 200, "Release/built_empty.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "release", "show", "v1.0.0")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	for _, want := range []string{
		"v1.0.0  or-Rel1aB2cDe", "Status:  superseded", "Carries 1 pull request:",
		"#41", "Show the autumn banner", "plan or-Pln6vB7nMq, step or-Stp1000002",
		"Deployments:", "prod", "2026-10-20 10:00 UTC", "to 2026-10-24 09:30 UTC", "revision 7, by orchard-owner",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	r = run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "release", "show", "v1.2.1")
	if r.e != nil || !strings.Contains(r.out, "No deployments yet.") || !strings.Contains(r.out, "Carries 0 pull requests:") {
		t.Errorf("an empty release: %v\n%s", r.e, r.out)
	}
}

func TestReleaseDeployed_PostsTheReportWithAnIdempotencyKey(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("POST /releases/v1.1.0/deployments", 200, "Release/live.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "release", "deployed", "v1.1.0", "--revision", "8", "--by", "shop-deploy")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	req := f.requests()[0]
	var body map[string]any
	if err := json.Unmarshal([]byte(req.body), &body); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(body) != fmt.Sprint(map[string]any{"by": "shop-deploy", "revision": "8", "target": "prod"}) {
		t.Errorf("body = %v", body)
	}
	if req.header.Get("Idempotency-Key") == "" {
		t.Error("no Idempotency-Key")
	}
	if !strings.Contains(r.out, "Status:  live") {
		t.Errorf("output:\n%s", r.out)
	}

	f.on("POST /releases/v1.1.0/deployments", func(w http.ResponseWriter, _ *http.Request) { problem(w, t, 403, "Problem/forbidden.json") })
	if r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "release", "deployed", "v1.1.0", "--target", "prod"); r.e == nil {
		t.Errorf("a refused report succeeded:\n%s", r.out)
	}
}

func TestWatch_PrintsReleases(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	_, cut := frame(t, "release.cut.json")
	_, deployed := frame(t, "release.deployed.json")
	f.on("GET /events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, cut+"\n"+deployed+"\n")
		hold(w, r)
	})
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{stopAfter: 2, stream: f.srv.Client()}, "--edge", f.srv.URL, "--land", "acme", "watch")
	if r.e != nil {
		t.Fatalf("gl watch: %v\n%s", r.e, r.err)
	}
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "release.cut  v1.2.0 cut, 2 pull requests, runs on -") ||
		!strings.HasPrefix(lines[1], "release.deployed  v1.1.0 live, 1 pull request, runs on prod") {
		t.Errorf("output:\n%s", r.out)
	}
}
