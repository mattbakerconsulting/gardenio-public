package gardenhub

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The release truck (gd-yOOVahJMCh, step 7 of plan gd-UZKZ9H25UD; releases-design.md section 3.5) is pure where it decides
// anything, so its contract runs under node (testdata/truck_test.js): TRUCK.norm reads the farm document's releases blob in
// both shapes the plan fixes, bayOf puts the loading bay in the project's home strip under its Barn's keep, clear of the sign
// and the steward, place gives one truck per project on the map indexed as release:<tag>, load is a crate per plan, depart drives a
// loading truck out when its release goes live (and says so in a toast), pose drives it out, away and back, and under reduced
// motion stands it still, its board over the cab saying where it runs or its status and since when (gd-rSz81Me1Pe) and its door
// its PR count, shippedOf gives a plot's "shipped in <tag> · <targets>", the card says "No deployments yet" when nothing runs
// the release and links its changelog, and web/release.js groups that changelog by plan, then step. The releases come from testdata/releases_fixture.json until the farm
// document carries them (step 5, gd-90IZtpGyR4).
func TestTruckUnderNode(t *testing.T) {
	r := nodeScript(t, "truck_test.js")
	t.Logf("bay %v; painted tag %v", r["bay"], r["tags"])
}

// The release truck on the lanes in a real Chromium (testdata/truck_browser.js; homestead-world-design.md 29.3, X-D11): a
// release gone live drives its truck west out of the bay onto the west lane, north up it and east along the north lane across
// the east lane's crossroads and off the region, drawn on lane tiles the whole way; away it is drawn nowhere, and it comes
// home the same way and parks in its bay facing east.
func TestTruckUnderChromium(t *testing.T) {
	underChromium(t, "truck_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}

// The page loads truck.js after world.js, art.js and goats.js and before scene.js and garden.js, draws the trucks after the
// plots and before the goats, stamps a shipped plot, makes each truck a link with the steward's tip, says its words on the
// signs, toasts a release gone live and lists it in the legend. A click opens the release's changelog: release.js loads after
// workbench.js, registers the Workbench's release kind and reads the viewer's /api/releases/{id}.
func TestTruckWired(t *testing.T) {
	at := func(name string) int { return strings.Index(pageHTML, `{{asset "`+name+`"}}`) }
	tr := at("truck.js")
	for _, before := range []string{"world.js", "art.js", "goats.js"} {
		if b := at(before); b < 0 || tr < b {
			t.Errorf("garden.html: %s at %d, truck.js at %d; truck.js must load after it", before, b, tr)
		}
	}
	for _, after := range []string{"scene.js", "garden.js"} {
		if a := at(after); a < tr {
			t.Errorf("garden.html: %s at %d, truck.js at %d; truck.js must load before it", after, a, tr)
		}
	}
	if pageSources["truck.js"] == "" || pageSources["release.js"] == "" {
		t.Error("page.go: truck.js or release.js is not in pageSources")
	}
	if w, r := at("workbench.js"), at("release.js"); w < 0 || r < w {
		t.Errorf("garden.html: workbench.js at %d, release.js at %d; release.js must load after it", w, r)
	}
	for _, want := range []string{"W.register('release',", "'/api/releases/' + encodeURIComponent(id)", "TRUCK.release(id)"} {
		if !strings.Contains(pageReleaseJS, want) {
			t.Errorf("release.js does not contain %q", want)
		}
	}
	if !strings.Contains(pageTruckJS, `"/garden?release=" + encodeURIComponent(id)`) {
		t.Error("truck.js: the truck's link does not open the release panel")
	}
	scene := pageSceneJS
	if d, g := strings.Index(scene, "TRUCK.draw(ctx,"), strings.Index(scene, "GOATS.draw(ctx,"); d < 0 || g < d {
		t.Errorf("scene.js: TRUCK.draw at %d, GOATS.draw at %d; the trucks are drawn before the goats", d, g)
	}
	for _, want := range []string{"ART.drawShippedMark(ctx,pal,", "TRUCK.shipped(p.plan)"} {
		if !strings.Contains(scene, want) {
			t.Errorf("scene.js does not contain %q", want)
		}
	}
	for _, want := range []string{
		"truck:1}", "h.kind==='truck')STIP.wire(node)", "world?TRUCK.hots(world):[]", "TRUCK.href(h.tr,base)", "TRUCK.card(h.tr,base,ago)",
		"ex=n?TRUCK.line(n,level,0):''", "if(ex)b.status=b.status?b.status+' · '+ex:ex;", "TRUCK.stale()", "TRUCK.wire({toast:toast})", "['release','",
	} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js does not contain %q", want)
		}
	}
	for _, want := range []string{"h.kind === 'truck' ? TRUCK.card(h.tr, s.base, s.ago)", "h.kind === 'truck')", ".w-truck"} {
		if !strings.Contains(pageStipJS, want) {
			t.Errorf("stip.js does not contain %q", want)
		}
	}
}
