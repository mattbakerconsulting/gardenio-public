package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func (f *fakeDaemon) ListReleases(_ context.Context, project, _ string, limit int) ([]beadsapi.Release, error) {
	if f.releasesErr != nil {
		return nil, f.releasesErr
	}
	var out []beadsapi.Release
	for _, r := range f.releases {
		if project != "" && r.Project != project {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeDaemon) GetRelease(_ context.Context, ref, project string) (*beadsapi.Release, error) {
	if f.releasesErr != nil {
		return nil, f.releasesErr
	}
	for _, r := range f.releases {
		if r.ID == ref || (project != "" && r.Project == project && r.Tag == ref) {
			return &r, nil
		}
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "release not found"}
}

// releaseFixture is three gardenio releases, newest first, shaped like the
// backfilled ones on the daemon: 2026.304.2 cut (two plans and an unplanned
// PR), 2026.304.1 live on prod, 2026.303.2 superseded on prod.
func releaseFixture(t *testing.T) []beadsapi.Release {
	t.Helper()
	contents := func(rows ...map[string]any) json.RawMessage { return rawJSON(t, rows) }
	pr := func(n int, title, task, step, plan string) map[string]any {
		row := map[string]any{"pr": n, "title": title, "url": "https://github.com/acme/gardenio/pull/" + itoa(n),
			"merged_at": "2026-10-31T07:20:12Z"}
		for k, v := range map[string]string{"task": task, "step": step, "plan": plan} {
			if v != "" {
				row[k] = v
			}
		}
		return row
	}
	return []beadsapi.Release{
		{
			ID: "gd-rel3", Project: "gardenio", Tag: "2026.304.2", SHA: "9cf6aca202d993f56ae0a59a4b7bf4f444916536",
			PreviousTag: "2026.304.1", CutAt: "2026-10-31T12:19:35Z", CutBy: "release-captain", Status: "cut",
			Contents: contents(
				pr(1652, "docs(kbeads): map every ownership exception", "gd-step1", "gd-step1", "gd-planA"),
				pr(1651, "feat(kbeads): shadow ownership grant table", "gd-step2", "gd-step2", "gd-planA"),
				pr(1650, "docs(kbeads): record the owner's answers", "", "", ""),
				pr(1639, "fix(gardenio/helm): release-recorder only", "gd-step9", "gd-step9", "gd-planB"),
			),
		},
		{
			ID: "gd-rel2", Project: "gardenio", Tag: "2026.304.1", SHA: "1db2170dc55135d09cb8a0618ef8de3c3aca4726",
			PreviousTag: "2026.303.2", CutAt: "2026-10-31T04:02:00Z", CutBy: "release-captain", Status: "live",
			Contents: contents(pr(1618, "feat(gardenio/github-bridge): record releases", "gd-step8", "gd-step8", "gd-planB")),
			Deployments: []beadsapi.ReleaseDeployment{
				{Target: "prod", Kind: "k3s", Since: "2026-10-31T04:20:00Z", By: "gardenio-release-deployer", Revision: "413"},
			},
		},
		{
			ID: "gd-rel1", Project: "gardenio", Tag: "2026.303.2", CutAt: "2026-10-30T16:02:11Z", Status: "superseded",
			Contents: contents(),
			Deployments: []beadsapi.ReleaseDeployment{
				{Target: "prod", Kind: "k3s", Since: "2026-10-30T16:20:00Z", Until: "2026-10-31T04:20:00Z", Revision: "412"},
			},
		},
	}
}

func releaseProjects() map[string]beadsapi.ProjectInfo {
	return map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", GitURL: "git@github.com:acme/gardenio.git", ReleaseSource: &beadsapi.ReleaseSource{
			TagPattern: `^\d{4}\.\d{3}\.\d+$`, Targets: []beadsapi.ReleaseTarget{{Name: "prod", Kind: "k3s"}}}},
		"quiet": {Name: "quiet"},
	}
}

func releaseFake(t *testing.T) *fakeDaemon {
	f := newFake()
	f.releases = releaseFixture(t)
	f.projectsResp = releaseProjects()
	return f
}

func releaseGet(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code, w.Body.String()
}

func TestReleasesPage_OneSectionPerProjectWithAReleaseSource(t *testing.T) {
	code, body := releaseGet(t, mkServer(t, releaseFake(t)), "/releases")
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	for _, want := range []string{
		`<h1>Releases</h1>`,
		`data-project="gardenio"`,
		`runs on prod`,
		`id="rel-row-tpl"`,
		`data-word="no-deployments">No deployments yet.`,
		`/assets/static/releases.js`,
		`/assets/static/releases.css`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `data-project="quiet"`) {
		t.Error("a project with no release source has a section")
	}
}

func TestReleasesPage_NoReleaseSourceSaysHowToSwitchOn(t *testing.T) {
	f := releaseFake(t)
	f.projectsResp = map[string]beadsapi.ProjectInfo{"quiet": {Name: "quiet"}}
	_, body := releaseGet(t, mkServer(t, f), "/releases")
	if !strings.Contains(body, "No project has a release source yet") {
		t.Errorf("no empty state:\n%s", body)
	}
}

func TestReleasePage_FrameByIDAndByTag(t *testing.T) {
	h := mkServer(t, releaseFake(t))
	for _, path := range []string{"/release/gd-rel2", "/release/2026.304.1?project=gardenio"} {
		code, body := releaseGet(t, h, path)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d\n%s", path, code, body)
		}
		for _, want := range []string{
			`<title>2026.304.1 · gardenio release · Beads Viewer</title>`,
			`data-release="gd-rel2"`,
			`data-project="gardenio"`,
			`data-repo="https://github.com/acme/gardenio"`,
			`<h2 id="rel-contents-h">Contents</h2>`,
			`<h2 id="rel-deploys-h">Deployments</h2>`,
			`href="/releases?project=gardenio"`,
			`id="rel-plan-tpl"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", path, want)
			}
		}
	}
}

func TestReleasePage_UnknownIs404(t *testing.T) {
	h := mkServer(t, releaseFake(t))
	for _, path := range []string{"/release/gd-nope", "/release/2026.304.1"} {
		if code, _ := releaseGet(t, h, path); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, code)
		}
	}
}

func TestGithubRepoURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/gardenio.git":     "https://github.com/acme/gardenio",
		"https://github.com/acme/gardenio.git": "https://github.com/acme/gardenio",
		"https://github.com/acme/gardenio/":    "https://github.com/acme/gardenio",
		"https://gitlab.com/acme/gardenio.git": "",
		"":                                     "",
	} {
		if got := githubRepoURL(in); got != want {
			t.Errorf("githubRepoURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBeadShow_ShippedInLine(t *testing.T) {
	f := releaseFake(t)
	f.beads["gd-step1"] = &beadsapi.BeadDetail{ID: "gd-step1", Title: "Map the exceptions", Type: "task", Kind: "issue",
		Status: "closed", Labels: []string{"project:gardenio"}}
	_, body := releaseGet(t, mkServer(t, f), "/gd-step1")
	for _, want := range []string{`data-testid="release-shipped"`, `Shipped in`, `<a href="/release/gd-rel3">2026.304.2</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestBeadShow_NoShippedInLineWhenNotShipped(t *testing.T) {
	f := releaseFake(t)
	// gd-aaa is open and in no release; an open non-plan bead is not looked up.
	_, body := releaseGet(t, mkServer(t, f), "/gd-aaa")
	if strings.Contains(body, `data-testid="release-shipped"`) {
		t.Error("an unshipped bead says Shipped in")
	}
	f.beads["gd-other"] = &beadsapi.BeadDetail{ID: "gd-other", Title: "x", Type: "task", Status: "closed", Labels: []string{"project:gardenio"}}
	f.releasesErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	code, body := releaseGet(t, mkServer(t, f), "/gd-other")
	if code != http.StatusOK || strings.Contains(body, `data-testid="release-shipped"`) {
		t.Errorf("a failed release read broke the bead view: %d", code)
	}
}

func TestReleaseShippedIn_PlanAcrossReleasesOldestFirst(t *testing.T) {
	srv := testServerWithDaemon(releaseFake(t))
	plan := &beadsapi.BeadDetail{ID: "gd-planB", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}}
	got := srv.releaseShippedIn(context.Background(), plan)
	want := []releaseShippedView{
		{Tag: "2026.304.1", Href: "/release/gd-rel2", Count: 1},
		{Tag: "2026.304.2", Href: "/release/gd-rel3", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	mr := &beadsapi.BeadDetail{ID: "gd-mr", Type: "merge-request", Status: "closed", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"mr_url": "https://github.com/acme/gardenio/pull/1650"}}
	if got := srv.releaseShippedIn(context.Background(), mr); len(got) != 1 || got[0].Tag != "2026.304.2" {
		t.Errorf("merged-MR bead by its PR url: %+v", got)
	}
}

// A merged PR boards the project's open release before the cut (plan
// gd-lgN4U7X5Tb): its task's line says "on the next release (open)", and a
// plan's line names it last, after the releases its steps shipped in.
func TestReleaseShippedIn_OpenReleaseIsTheNextRelease(t *testing.T) {
	f := releaseFake(t)
	f.releases = append([]beadsapi.Release{{ID: "gd-next", Project: "gardenio", Status: beadsapi.ReleaseStatusOpen,
		Contents: json.RawMessage(`[{"pr":1700,"merge_sha":"abc","task":"gd-step7","step":"gd-step7","plan":"gd-planB"}]`)}}, f.releases...)
	f.beads["gd-step7"] = &beadsapi.BeadDetail{ID: "gd-step7", Title: "Board the open release", Type: "task", Kind: "issue",
		Status: "closed", Labels: []string{"project:gardenio"}}
	_, body := releaseGet(t, mkServer(t, f), "/gd-step7")
	if !strings.Contains(body, `On the <a href="/release/gd-next">next release</a> (open)`) || strings.Contains(body, "Shipped in") {
		t.Errorf("a boarded task's line does not read on the next release (open):\n%s", body)
	}
	plan := &beadsapi.BeadDetail{ID: "gd-planB", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}}
	got := testServerWithDaemon(f).releaseShippedIn(context.Background(), plan)
	if len(got) != 3 || got[0].Open || !got[2].Open || got[2].Href != "/release/gd-next" {
		t.Errorf("plan line = %+v, want two cut releases then the open one", got)
	}
}

func TestIndex_LatestReleaseTile(t *testing.T) {
	srv := testServerWithDaemon(releaseFake(t))
	now := time.Date(2026, 10, 31, 15, 19, 35, 0, time.UTC)
	tiles := srv.latestReleaseTiles(context.Background(), releaseProjects(), now)
	if len(tiles) != 1 {
		t.Fatalf("tiles = %+v, want one (gardenio)", tiles)
	}
	got := tiles[0]
	if got.Project != "gardenio" || got.Tag != "2026.304.2" || got.Status != "cut" || got.Href != "/release/gd-rel3" || got.PRs != 4 || got.CutAgo == "" {
		t.Errorf("tile = %+v", got)
	}
	_, body := releaseGet(t, mkServer(t, releaseFake(t)), "/index")
	for _, want := range []string{`data-testid="index-latest-releases"`, `<h2>Latest release</h2>`, `href="/release/gd-rel3">2026.304.2</a>`, `release-status-cut`} {
		if !strings.Contains(body, want) {
			t.Errorf("front page missing %q", want)
		}
	}
}

func TestReleaseStatusWord_UnknownReadsAsCut(t *testing.T) {
	for in, want := range map[string]string{"live": "live", "built": "built", "superseded": "superseded", "cut": "cut", "": "cut", "rolled": "cut"} {
		if got := releaseStatusWord(in); got != want {
			t.Errorf("releaseStatusWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHealthBuildTile_LinksTheRunningRelease(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	srv := testServerWithDaemon(releaseFake(t))
	version = "2026.304.1"
	if got := srv.runningReleaseHref(context.Background()); got != "/release/gd-rel2" {
		t.Errorf("runningReleaseHref = %q, want /release/gd-rel2", got)
	}
	version = "2026.299.9"
	if got := srv.runningReleaseHref(context.Background()); got != "" {
		t.Errorf("a version with no release links %q", got)
	}
	version = "dev"
	if got := srv.runningReleaseHref(context.Background()); got != "" {
		t.Errorf("a dev build links %q", got)
	}
}

// releaseAPIFixture answers step 6's JSON routes (gd-DiI90CU9Fy) with the
// shape agreed on the plan: {items, next_cursor?} for the list, one Release
// for the single read, the viewer extras included.
func releaseAPIFixture(t *testing.T) http.Handler {
	t.Helper()
	row := func(pr int, title, step, stepTitle, plan, planTitle, planStatus string) map[string]any {
		m := map[string]any{"pr": pr, "title": title, "url": "https://github.com/acme/gardenio/pull/" + itoa(pr), "merged_at": "2026-10-31T07:20:12Z"}
		for k, v := range map[string]string{"step": step, "task": step, "step_title": stepTitle, "plan": plan, "plan_title": planTitle, "plan_status": planStatus} {
			if v != "" {
				m[k] = v
			}
		}
		return m
	}
	rel3 := map[string]any{
		"id": "gd-rel3", "project": "gardenio", "tag": "2026.304.2", "status": "live", "sha": "9cf6aca202d993f56ae0a59a4b7bf4f444916536",
		"cut_at": "2026-10-31T12:19:35Z", "cut_by": "release-captain", "previous": "gd-rel2", "previous_tag": "2026.304.1",
		"contents": []map[string]any{
			row(1652, "docs(kbeads): map every ownership exception to its grant row", "gd-step1", "Map every exception", "gd-planA", "Ownership grant table", "open"),
			row(1650, "docs(kbeads): record the owner's answers", "", "", "", "", ""),
			row(1639, "fix(gardenio/helm): github-bridge machine client holds release-recorder only", "gd-step9", "Machine client", "gd-planB", "Releases of a project", "closed"),
			row(1651, "feat(kbeads): shadow ownership grant table beside authorizeOwnership", "gd-step2", "Build the table", "gd-planA", "Ownership grant table", "open"),
		},
		"deployments": []map[string]any{{"target": "prod", "kind": "k3s", "since": "2026-10-31T16:20:00Z", "by": "gardenio-release-deployer", "revision": "414", "via": "script"}},
	}
	rel2 := map[string]any{"id": "gd-rel2", "project": "gardenio", "tag": "2026.304.1", "status": "superseded", "cut_at": "2026-10-31T04:02:00Z",
		"cut_by": "release-captain", "previous_tag": "2026.303.2", "next_tag": "2026.304.2",
		"contents":    []map[string]any{row(1618, "feat(gardenio/github-bridge): record releases", "gd-step8", "", "gd-planB", "Releases of a project", "closed")},
		"deployments": []map[string]any{{"target": "prod", "kind": "k3s", "since": "2026-10-31T04:20:00Z", "until": "2026-10-31T16:20:00Z", "revision": "413"}}}
	rel1 := map[string]any{"id": "gd-rel1", "project": "gardenio", "tag": "2026.304.0", "status": "built", "cut_at": "2026-10-30T22:00:00Z",
		"contents": []map[string]any{}, "deployments": []map[string]any{}}
	byID := map[string]map[string]any{"gd-rel3": rel3, "gd-rel2": rel2, "gd-rel1": rel1}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/releases", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{rel3, rel2, rel1}})
	})
	mux.HandleFunc("GET /api/releases/{id}", func(w http.ResponseWriter, r *http.Request) {
		rel, ok := byID[r.PathValue("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	})
	return mux
}

// TestReleasePagesUnderChromium opens /releases and /release/{id} in a real
// Chromium (testdata/release_page_browser.js) at a phone's size and a
// desktop's, light and dark: releases.js fills both from the JSON routes
// (a fixture of step 6's shape until it merges), the contents group by plan
// with the finished plan first and unplanned last, the timeline shows prod,
// nothing scrolls sideways, and every link is reachable by Tab.
// RELEASE_PAGE_SHOTS=<dir> keeps a screenshot per cell.
func TestReleasePagesUnderChromium(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skip("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		skip("playwright-core not installed under %s (make test-gardenhub-browser)", mods)
	}
	app := mkServer(t, releaseFake(t))
	api := releaseAPIFixture(t)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/releases") {
			api.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	script, _ := filepath.Abs(filepath.Join("testdata", "release_page_browser.js"))
	cmd := exec.Command(node, script, hs.URL, mods)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("release page checks failed: %v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// The daemon lists a project's open release (no tag yet, plan gd-lgN4U7X5Tb)
// first: the Latest release tile skips it for the newest cut one.
func TestIndex_LatestReleaseTileSkipsTheOpenRelease(t *testing.T) {
	f := releaseFake(t)
	f.releases = append([]beadsapi.Release{{ID: "gd-next", Project: "gardenio", Status: beadsapi.ReleaseStatusOpen}}, f.releases...)
	tiles := testServerWithDaemon(f).latestReleaseTiles(context.Background(), releaseProjects(), time.Now())
	if len(tiles) != 1 || tiles[0].Tag != "2026.304.2" || tiles[0].Href != "/release/gd-rel3" {
		t.Errorf("tiles = %+v, want gardenio's 2026.304.2", tiles)
	}
}
