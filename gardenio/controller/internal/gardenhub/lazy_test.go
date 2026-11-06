package gardenhub

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// web/lazy.js (P1.3, gd-p02ZelpMxk), pure half, under node: what an address needs at once (a panel or nothing),
// which links the Workbench opens in place, its panel kinds, which are WORKBENCH.KINDS, and the kinds and pages whose code
// is in page-glass.js, page-land.js (gd-yKSXtthbwB) or page-panels.js (gd-yOMRlm7kKF).
func TestLazyLogicUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var L = require(process.argv[1]), W = require(process.argv[2]).host, assert = require("assert");
assert.deepStrictEqual(L.KINDS, W.KINDS, "lazy.js's panel kinds are the Workbench's");
assert.strictEqual(L.needOf(""), "", "the World alone");
assert.strictEqual(L.needOf("?at=gardenio&z=2"), "");
assert.strictEqual(L.needOf("?bead=gd-1"), "workbench");
assert.strictEqual(L.needOf("?transcript=gardenio-x"), "workbench", "a gardener's transcript");
assert.strictEqual(L.needOf("?release=2026.11.01-1600"), "workbench", "a release's changelog");
assert.strictEqual(L.needOf("?at=x&glass="), "workbench", "?glass= alone is the Wall");
assert.strictEqual(L.needOf("?be%61d=gd-1"), "workbench", "a key is decoded");
assert.strictEqual(L.needOf("?beadx=1&xbead=2"), "", "only whole keys");
assert.strictEqual(L.bundleOf("glass", true, "gd-A1a"), "glass", "an operator's Glass");
assert.strictEqual(L.bundleOf("glass", true, "wall"), "wall", "the Wall draws no xterm, nudge or attach");
assert.strictEqual(L.bundleOf("glass", true, ""), "wall", "?glass= alone is the Wall");
assert.strictEqual(L.bundleOf("glass", true), "wall");
assert.strictEqual(L.bundleOf("glass", false, "gd-A1a"), "", "no Glass for a viewer who may not watch");
assert.strictEqual(L.bundleOf("glass", false, "wall"), "", "nor a Wall");
assert.strictEqual(L.bundleOf("creds", false), "glass", "the credential pool");
assert.strictEqual(L.bundleOf("bead", true), "");
["gardener", "bundle", "settings", "release"].forEach(function (k) { assert.strictEqual(L.bundleOf(k, false, "x"), "panels", k + " is page-panels.js's"); });
assert.deepStrictEqual(L.PANELS.filter(function (k) { return L.KINDS.indexOf(k) < 0; }), [], "page-panels.js draws Workbench kinds");
assert.strictEqual(L.landIn("?at=x&land=acme"), true);
assert.strictEqual(L.landIn("?land=acme"), true);
assert.strictEqual(L.landIn("?land="), false, "no land named");
assert.strictEqual(L.landIn("?island=acme"), false, "only the whole key");
assert.strictEqual(L.needOf("?land=acme"), "", "a land's header is not a panel");
assert.strictEqual(L.panelLink("/v", "https://h/v/garden?bead=gd-1", "https://h"), true);
assert.strictEqual(L.panelLink("/v", "/v/garden?at=x&plan=gd-1", "https://h"), true, "relative");
assert.strictEqual(L.panelLink("/v", "https://h/v/garden?at=x", "https://h"), false, "no panel");
assert.strictEqual(L.panelLink("/v", "https://h/garden?bead=gd-1", "https://h"), false, "another base");
assert.strictEqual(L.panelLink("/v", "https://o/v/garden?bead=gd-1", "https://h"), false, "another origin");
assert.strictEqual(L.panelLink("/v", "https://h/v/b/gd-1", "https://h"), false, "the viewer's own page");
`
	src, _ := filepath.Abs(filepath.Join("web", "lazy.js"))
	wb, _ := filepath.Abs(filepath.Join("web", "workbench.js"))
	if out, err := exec.Command(node, "-e", script, src, wb).CombinedOutput(); err != nil {
		t.Fatalf("node lazy test failed: %v\n%s", err, out)
	}
}

// The World paints first (P1.3): the page loads page-scene.js and page-farm.js by tag and names page-workbench.js,
// page-wall.js, page-glass.js, page-land.js and page-panels.js only for web/lazy.js, which comes after garden.js in page-farm.js. Its <head>
// preload names the same panel kinds as lazy.js, and page-panels.js's, so a deep link starts the bundles it opens.
func TestPageLoadsTheWorldFirst(t *testing.T) {
	_, rec := servedPage(t)
	page := rec.Body.String()
	tags := regexp.MustCompile(`<script src="/v/garden/assets/([a-z-]+)\.[0-9a-f]{12}\.js"`).FindAllStringSubmatch(page, -1)
	var got []string
	for _, m := range tags {
		got = append(got, m[1])
	}
	if strings.Join(got, " ") != "sky page-scene page-farm" {
		t.Errorf("the page's script tags load %v, want sky, page-scene and page-farm", got)
	}
	for _, name := range []string{"page-workbench.js", "page-wall.js", "page-glass.js", "page-land.js", "page-panels.js"} {
		u, err := pageAssets.url(name)
		if err != nil {
			t.Fatal(err)
		}
		attr := "data-" + strings.TrimSuffix(strings.TrimPrefix(name, "page-"), ".js") + "-js=\"/v" + u + "\""
		if !strings.Contains(page, attr) {
			t.Errorf("the body does not name %s for lazy.js (%s)", name, attr)
		}
	}
	// the Glass and a land's code are bundles of their own, loaded only by the viewers who use them (B1, gd-yKSXtthbwB)
	wb := pageAssets.byName["page-workbench.js"].src
	for name, src := range map[string]string{"glass.js": pageGlassJS, "wall.js": pageWallJS, "credits.js": pageCreditsJS, "landhd.js": pageLandHdJS, "become.js": pageBecomeJS} {
		if strings.Contains(wb, src) {
			t.Errorf("%s is in page-workbench.js, which every Commons deep link loads", name)
		}
	}
	// the panels no other script calls into are a bundle of their own, which a Commons thread and the Wall do not load
	// (B1, gd-yOMRlm7kKF)
	for name, src := range map[string]string{"gardener.js": pageGardenerJS, "bundle.js": pageBundleJS, "release.js": pageReleaseJS, "settings.js": pageSettingsJS} {
		if strings.Contains(wb, src) || !strings.Contains(pageAssets.byName["page-panels.js"].src, src) {
			t.Errorf("%s is not page-panels.js's alone", name)
		}
	}
	// the Wall is its own bundle (B1, gd-BC3yZxTQ65): one agent's Glass, its nudge and attach are not in it
	wall := pageAssets.byName["page-wall.js"].src
	for name, src := range map[string]string{"glass.js": pageGlassJS, "glassact.js": pageGlassActJS, "glasskey.js": pageGlassKeyJS, "creds.js": pageCredsJS} {
		if strings.Contains(wall, src) {
			t.Errorf("%s is in page-wall.js, which the Wall and a figure's card load", name)
		}
	}
	farm := pageAssets.byName["page-farm.js"].src
	if g, l := strings.Index(farm, pageAppJS), strings.Index(farm, pageLazyJS); g < 0 || l < g {
		t.Error("lazy.js is not in page-farm.js after garden.js, whose click handlers it must follow")
	}
	if p, g := strings.Index(farm, pagePlansJS), strings.Index(farm, pageAppJS); p != 0 || g < p {
		t.Error("plans.js, the farm's feed, does not open page-farm.js")
	}
	// a goat's tip draws its kin's arms the first time it opens (STIP.goat does not redraw a tip already up), and a bead's
	// label names its arms (garden.js fillHot): ARMS is the World's, not the Workbench's
	if a, s := strings.Index(farm, pageArmsJS), strings.Index(farm, pageStipJS); a < 0 || s < a {
		t.Error("arms.js is not in page-farm.js before stip.js")
	}
	m := regexp.MustCompile(`\(\^\\\?\|&\)\(([a-z|]+)\)`).FindStringSubmatch(pageHTML)
	if m == nil {
		t.Fatal("no panel kinds in garden.html's lazy preload")
	}
	kinds := regexp.MustCompile(`var KINDS = \[([^\]]+)\]`).FindStringSubmatch(pageLazyJS)
	if kinds == nil || strings.Join(strings.Split(strings.NewReplacer("'", "", " ", "").Replace(kinds[1]), ","), "|") != m[1] {
		t.Errorf("the preload's kinds %q are not lazy.js's %v", m[1], kinds)
	}
	pm := regexp.MustCompile(`u\.push\(f\[1\]\)`).MatchString(pageHTML)
	pk := regexp.MustCompile(`if\(/\(\^\\\?\|&\)\(([a-z|]+)\)\(=\|&\|\$\)/\.test\(q\)\)u\.push`).FindStringSubmatch(pageHTML)
	panels := regexp.MustCompile(`var PANELS = \[([^\]]+)\]`).FindStringSubmatch(pageLazyJS)
	if !pm || pk == nil || panels == nil || strings.Join(strings.Split(strings.NewReplacer("'", "", " ", "").Replace(panels[1]), ","), "|") != pk[1] {
		t.Errorf("the preload's page-panels.js kinds %v are not lazy.js's PANELS %v", pk, panels)
	}
}
