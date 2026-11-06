package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/golden"
)

// composerGoldenSurfaces is the tolerance of each surface testdata/composer_golden_browser.js shoots (golden.Tol says
// what the two numbers mean).
var composerGoldenSurfaces = map[string]golden.Tol{
	"composer": golden.Default, // the empty thread composer row: the house voice in the textarea, and Send
}

// TestComposerGoldens is the looking-glass gate for the house voice (gd-5e6xgIOH6B): an empty composer shows
// gardenhub.EmptyPrompt in the muted placeholder colour. Chromium opens /threads/{id} for a Slack-bound wish as an
// operator (so the composer renders), checks the placeholder is the constant and clears 4.5:1 in every skin, and shoots the
// composer row at a 390 px phone and a 1280 px desktop in the light (baseline), dark (dark-terminal), homestead (day) and
// dark-homestead (night) skins, the last two the viewer's default look (plan gd-JaMxRKsc8z, step 4.3). Each PNG is
// held against testdata/golden/<same name> by golden.Check; failing shots go to golden.ArtifactsDir("composer").
//
// The same switches as TestGardenGoldens: GARDENHUB_GOLDENS=1 to run (make test-garden-goldens), Linux only,
// GARDENHUB_GOLDEN_UPDATE=1 to rewrite goldens, GARDENHUB_BROWSER_REQUIRED=1 to fail instead of skip.
func TestComposerGoldens(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, mods, why := golden.Browser()
	if why != "" {
		skip("%s", why)
	}
	goldens, _ := filepath.Abs(filepath.Join("testdata", "golden"))
	script, _ := filepath.Abs(filepath.Join("testdata", "composer_golden_browser.js"))
	fonts, _ := filepath.Abs(filepath.Join("..", "..", "internal", "gardenhub", "testdata", "golden", "fonts"))

	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "wish", Status: "open",
			Labels: []string{threadHeadLabel},
			Fields: map[string]string{
				threadSlackChannelField: "C0AHE7C0E1G",
				threadSlackTSField:      "1782600000.000100",
			},
			CreatedAt: time.Unix(1782600000, 0),
		},
	}
	mux := mkServerFromFake(t, f)
	// Every request is the operator's, as the edge would stamp it, so CanCompose is true.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Auth-Request-Email", "matt@example.com")
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	shots := t.TempDir()
	cmd := exec.Command(node, script, srv.URL+"/threads/gd-wish", mods, shots, fonts, gardenhub.EmptyPrompt)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("composer capture failed: %v\n%s", err, out)
	}
	t.Logf("capture:\n%s", out)

	arts := golden.ArtifactsDir("composer")
	r, err := golden.Check(golden.Options{Shots: shots, Goldens: goldens, Artifacts: arts, Update: os.Getenv("GARDENHUB_GOLDEN_UPDATE") == "1", Surfaces: composerGoldenSurfaces})
	for _, l := range r.Logs {
		t.Log(l)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failed) > 0 {
		t.Fatalf("%d composer golden(s) differ; actual/expected/diff PNGs are in %s. An intentional change? Commit no golden: the window bless PR (bless-goldens.yml) re-shoots them on main.\n  %s",
			len(r.Failed), arts, strings.Join(r.Failed, "\n  "))
	}
	t.Logf("%d shots match their goldens", r.Shots)
}

// TestEmptyPrompt_Composers proves the boxes where a human types to the fleet carry the house voice as their placeholder
// and the empty-prompt class that gives it the muted colour, without a browser: the thread reply composer, the /threads
// wish box and the take-control input. html/template writes the apostrophe as &#39;.
func TestEmptyPrompt_Composers(t *testing.T) {
	want := `class="empty-prompt"`
	ph := `placeholder="` + strings.ReplaceAll(gardenhub.EmptyPrompt, "'", "&#39;") + `"`

	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "wish", Status: "open", Labels: []string{threadHeadLabel},
			Fields: map[string]string{threadSlackChannelField: "C0AHE7C0E1G", threadSlackTSField: "1782600000.000100"},
		},
	}
	mux := mkServerFromFake(t, f)
	for _, path := range []string{"/threads/gd-wish", "/threads"} {
		body, code := getBodyWithRoles(t, mux, path, "matt@example.com", nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
		if !strings.Contains(body, ph) || !strings.Contains(body, want) {
			t.Errorf("GET %s: the composer should carry %s and %s", path, ph, want)
		}
	}

	bar := renderAgentControlToolbar("", "gd-agent", "matt@example.com")
	if !strings.Contains(bar, `placeholder="`+strings.ReplaceAll(gardenhub.EmptyPrompt, "'", "&#39;")+`"`) || !strings.Contains(bar, `class="empty-prompt"`) {
		t.Errorf("take-control input should carry the house voice and the empty-prompt class:\n%s", bar)
	}
}
