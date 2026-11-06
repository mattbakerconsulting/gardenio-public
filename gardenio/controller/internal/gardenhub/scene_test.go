package gardenhub

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The World draws with none of the signed-in page (LW.3 gd-VEbAvsTNDe), as a land window will draw it: the page-scene.js
// bundle exactly as /garden serves it runs in a bare context (testdata/scene_test.js) with no me, PLANS, WORKBENCH, BEAD,
// CHAT or fetch, lays out the fixture farm and a land window's scene (internal/planwindow's mixed fixture, LW.2) with
// SCENE.lay and draws each at the four levels with SCENE.scene(cv).draw. garden.js draws through the same function, so the
// /garden goldens hold what it paints. At World and Project the plots with gardeners carry the gardener badge and the rest
// none (gd-vi77cq1oYY); scene_test.js asserts it plot by plot, on a crowded World too. At Plan a planted plot's baked ring is
// hedge and a staked one's string (homestead-world-design.md 29.4).
func TestSceneDrawsAlone(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	a := pageAssets.byName["page-scene.js"]
	if a == nil {
		t.Fatal("no page-scene.js bundle")
	}
	if !strings.Contains(a.src, "var SCENE = (function () {") {
		t.Fatal("the page-scene.js bundle does not carry web/scene.js")
	}
	bundle := filepath.Join(t.TempDir(), "page-scene.js")
	if err := os.WriteFile(bundle, []byte(a.body), 0o644); err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, f := range []string{filepath.Join("testdata", "scene_test.js"), filepath.Join("testdata", "farm_fixture.json"), filepath.Join("..", "planwindow", "testdata", "scene", "mixed.scene.json")} {
		p, err := filepath.Abs(f)
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{args[0], bundle}, args[1:]...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("scene_test.js failed: %v\n%s", err, out)
	}
	var r struct {
		Docs []struct {
			File   string       `json:"file"`
			Hedged int          `json:"hedged"`
			Strung int          `json:"strung"`
			Levels []sceneLevel `json:"levels"`
		} `json:"docs"`
		Fetched []string `json:"fetched"`
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		t.Fatalf("scene_test.js printed no report: %v\n%s", err, out)
	}
	if len(r.Fetched) > 0 {
		t.Errorf("drawing the scene fetched %v", r.Fetched)
	}
	if len(r.Docs) != 2 {
		t.Fatalf("drew %d documents, want 2: %s", len(r.Docs), out)
	}
	for _, d := range r.Docs {
		// scene_test.js checks each plot's ring tile by tile (29.4); the fixture farm has a plot of each kind for it to check
		if d.File == "farm_fixture.json" && (d.Hedged == 0 || d.Strung == 0) {
			t.Errorf("%s: want a hedged (planted) and a strung (staked) plot in the baked region, got %d and %d", d.File, d.Hedged, d.Strung)
		}
		for _, l := range d.Levels {
			checkSceneLevel(t, d.File, l)
		}
	}
}

// sceneLevel is what scene_test.js reports for one document drawn at one level.
type sceneLevel struct {
	Level, LOD, Fills, Blits, Frames int
	Settled                          bool
	Plate                            int `json:"drawRollupPlate"`
	Flat                             int `json:"drawPlotFlat"`
	Bead                             int `json:"drawBead"`
	Ground                           int `json:"drawBiomeGround"`
	Sign                             int `json:"drawSign"`
	Badges                           int `json:"badges"`
}

// checkSceneLevel asserts what TestSceneDrawsAlone wants painted at one level: the camera at that level, the scene settled,
// and the regions, plates, badges, chunks, signs, ground, beds and step sprites that level draws.
func checkSceneLevel(t *testing.T, file string, l sceneLevel) {
	t.Helper()
	t.Logf("%s level %d: %+v", file, l.Level, l)
	if l.LOD != l.Level {
		t.Errorf("%s: the camera for level %d is at level %d", file, l.Level, l.LOD)
	}
	if !l.Settled {
		t.Errorf("%s level %d: still asking for frames after %d", file, l.Level, l.Frames)
	}
	if l.Level <= 1 && (l.Fills == 0 || l.Plate == 0) {
		t.Errorf("%s level %d: want the regions as blobs and a rollup plate per plot, got %d fills and %d plates", file, l.Level, l.Fills, l.Plate)
	}
	if l.Level <= 1 && l.Badges == 0 {
		t.Errorf("%s level %d: no plot carries the gardener badge (gd-vi77cq1oYY)", file, l.Level)
	}
	if l.Level >= 2 && l.Blits == 0 {
		t.Errorf("%s level %d: no chunk blitted", file, l.Level)
	}
	if l.Level >= 1 && l.Sign == 0 {
		t.Errorf("%s level %d: no region sign", file, l.Level)
	}
	if l.Level == 2 && (l.Ground == 0 || l.Flat == 0) {
		t.Errorf("%s level 2: want a region's ground baked into its chunks and flat beds, got %d grounds and %d beds", file, l.Ground, l.Flat)
	}
	if l.Level == 3 && l.Bead == 0 {
		t.Errorf("%s level 3: no step drawn as a sprite", file)
	}
}

// The same in a real page (testdata/scene_browser.js): a canvas, the page-scene.js bundle as served and a land window's scene
// inline, and a fetch that only records. The scene, laid out and drawn at each level by day and by night, paints the region's
// ground in many colours and fetches nothing.
func TestSceneDrawsAloneUnderChromium(t *testing.T) {
	scene, err := os.ReadFile(filepath.Join("..", "planwindow", "testdata", "scene", "mixed.scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	page := template.Must(template.New("scene").Parse(`<!doctype html><meta charset="utf-8"><title>scene</title>
<style>body{margin:0}canvas{display:block;width:390px;height:844px}</style>
<canvas id="cv" width="780" height="1688"></canvas>
<script>window.FETCHED=[];window.fetch=function(u){FETCHED.push(String(u));return new Promise(function(){});};</script>
<script src="/page-scene.js"></script>
<script type="application/json" id="doc">{{.}}</script>`))
	underChromium(t, "scene_browser.js", func() *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = page.Execute(w, template.JS(scene)) //nolint:gosec // a test fixture
		})
		mux.HandleFunc("GET /page-scene.js", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = w.Write([]byte(pageAssets.byName["page-scene.js"].body))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	})
}
