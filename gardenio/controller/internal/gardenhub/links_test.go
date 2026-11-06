package gardenhub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Owner feedback on the Homestead World plan (gd-oi3liYjNRA, phase 1c, gd-qie6tPtdpi): every link on
// /garden must stay inside the garden. Nothing may send the viewer to a beads-viewer page (the bead
// page, the Plans page, the agent chat, the viewer root). External links (GitHub/GitLab PRs, commits)
// are not the viewer and stay, opening in a new tab.
//
// TestGardenLinksStayInside walks the rendered page and the embedded sources and fails on any href or navigation
// under Handler.BasePath that is not /garden*. It began as an expected-failure inventory (gd-qie6tPtdpi, 109 live
// bead-page links); gd-BdhucNqv0i moved every bead link into the garden's own bead view (/garden?bead=<id>,
// web/workbench.js) and the overflow crate onto the Plans drawer, and gd-AXfec5afZo brought the header Beads chip and
// the Porch's agent chat inside (web/chat.js). Nothing is left, so any hit is a new leak.
//
// Keys are file + the navigation expression, not line numbers. <id> stands for any encodeURIComponent(x.id) /
// {{.ID}} value. Counts are occurrences in the source.

// gardenSources are the embedded page sources the static scan walks, by name.
var gardenSources = map[string]string{
	"garden.html": pageHTML, "garden.js": pageAppJS, "plans.js": pagePlansJS, "world.js": pageWorldJS, "people.js": pageSources["people.js"], "cursor.js": pageCursorJS, "weeds.js": pageWeedsJS, "art.js": pageArtJS,
	"chat.js": pageChatJS, "workbench.js": pageWorkbenchJS, "beacons.js": pageBeaconsJS, "transitions.js": pageTransitionsJS, "birds.js": pageBirdsJS, "goats.js": pageGoatsJS, "truck.js": pageTruckJS, "pictures.js": pagePicturesJS, "porch.js": pagePorchJS, "reason.js": pageReasonJS,
	"snap.js": pageSnapJS, "neighbours.js": pageNeighboursJS, "edges.js": pageEdgesJS, "answer.js": pageAnswerJS, "titles.js": pageTitlesJS, "stip.js": pageStipJS, "signfit.js": pageSignFitJS,
}

var (
	// base + '/path' in JS, capturing the encodeURIComponent(arg) that may follow the literal.
	jsBaseLink = regexp.MustCompile(`\bbase\s*\+\s*['"](/[^'"]*)['"](?:\s*\+\s*encodeURIComponent\(([^)]*)\))?`)
	// {{.Base}}/path in the template.
	tmplBaseLink = regexp.MustCompile(`href="\{\{\.Base\}\}(/[^"]*)"`)
	// every href in the rendered page.
	anyHref = regexp.MustCompile(`href="([^"]*)"`)
)

// insideGarden reports whether a path under BasePath stays on the garden page (its document with any query,
// such as the bead view's ?bead=, its events, farm document, bead reads and any future /garden/* route).
// Everything else under BasePath is the beads viewer.
func insideGarden(path string) bool {
	return path == "/garden" || strings.HasPrefix(path, "/garden?") || strings.HasPrefix(path, "/garden/")
}

// oldViewerDoor is the one link that leaves on purpose: the Menu's Old viewer item, to the viewer's list of the pages
// the garden does not open (Lands workbench-glass spec §2.3, W-D7 and §7 Q5, step 5.13): the administrative interface,
// one click away for an admin. It is not a leak of something the garden does.
const oldViewerDoor = "/old-viewer"

// scanGardenLeaks returns every link that leaves the garden for the viewer, keyed by file and expression, from
// (a) the page as served and (b) the embedded sources. Fragment links (#main) and absolute URLs are not
// under BasePath and are not counted; the page has no absolute URLs at all (TestPageHasNoExternalRequests).
func scanGardenLeaks(t *testing.T, base, body string) map[string]int {
	t.Helper()
	got := map[string]int{}
	// (a) the rendered page: any href under base that is not /garden*. Item hrefs are base+"/"+id, so
	// collapse them onto one key (the live page has one per bead).
	seen := map[string]bool{}
	for _, m := range anyHref.FindAllStringSubmatch(body, -1) {
		h := m[1]
		if strings.Contains(h, "{{") {
			t.Fatalf("unrendered template in served page: %q", h)
		}
		if !strings.HasPrefix(h, base+"/") && h != base {
			continue
		}
		p := strings.TrimPrefix(h, base)
		if insideGarden(p) || p == oldViewerDoor {
			continue
		}
		key := `rendered href="{base}` + p + `"`
		switch {
		case strings.Contains(body, `<a class="t" href="`+h+`"`):
			key = `rendered a.t href="{base}/<id>"`
		case strings.Contains(pageHTML, `href="{{.Base}}`+p+`"`):
			continue // template-static: counted once under its garden.html key below
		}
		if !seen[key] {
			got[key]++
			seen[key] = true
		}
	}
	// (b) the sources: every JS-built or template-static href/navigation under base.
	for _, m := range tmplBaseLink.FindAllStringSubmatch(pageHTML, -1) {
		if !insideGarden(m[1]) && m[1] != oldViewerDoor {
			got[`garden.html href="{{.Base}}`+m[1]+`"`]++
		}
	}
	for _, name := range []string{"garden.js", "plans.js", "world.js", "people.js", "weeds.js", "art.js", "chat.js", "workbench.js", "beacons.js", "transitions.js", "birds.js", "goats.js", "truck.js", "pictures.js", "porch.js", "reason.js", "snap.js", "neighbours.js", "edges.js"} {
		for _, m := range jsBaseLink.FindAllStringSubmatch(gardenSources[name], -1) {
			p := m[1]
			if insideGarden(p) {
				continue
			}
			// base+'/'+encodeURIComponent(x.id) -> <id>; any other argument is named, e.g. <h.biome.key>.
			tail := ""
			if arg := strings.TrimSpace(m[2]); strings.HasSuffix(arg, ".id") {
				tail = "+<id>"
			} else if arg != "" {
				tail = "+<" + arg + ">"
			}
			got[name+" base+'"+p+"'"+tail]++
		}
	}
	return got
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestGardenLinksStayInside: no href or navigation on /garden may point at the beads viewer.
func TestGardenLinksStayInside(t *testing.T) {
	f := newFake()
	b := bead("gd-w1", "bug", 5)
	b.HasReport = true
	a := bead("a-1", "agent", 6)
	a.Fields = map[string]string{"agent_state": "working"}
	f.set([]*beadsapi.BeadDetail{b, a}, []*beadsapi.BeadDetail{bead("d-1", "decision", 7)})
	h := New(f, "/v", nil)
	_ = h.refresh(context.Background())
	mux := http.NewServeMux()
	(&Handler{Hub: h, BasePath: "/v", Viewer: func(*http.Request) string { return "oidc:me@example.com" }}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /garden: %d", rec.Code)
	}
	body := rec.Body.String()
	// The fixture must exercise every list, or an empty Ledger would hide the item links.
	for _, id := range []string{"gd-w1", "a-1", "d-1"} {
		if !strings.Contains(body, `href="/v/garden?bead=`+id+`"`) && !strings.Contains(body, `id="i-`+id+`"`) {
			t.Fatalf("fixture item %s not on the page", id)
		}
	}

	got := scanGardenLeaks(t, "/v", body)

	if len(got) == 0 {
		return
	}
	var report []string
	for _, k := range sortedKeys(got) {
		report = append(report, fmt.Sprintf("  %s x%d", k, got[k]))
	}
	t.Errorf("link(s) leave the garden for the beads viewer; links must stay inside /garden (gd-qie6tPtdpi):\n%s", strings.Join(report, "\n"))
}

// insideGarden must admit the garden's own routes and nothing else under BasePath.
func TestInsideGarden(t *testing.T) {
	for p, want := range map[string]bool{
		"/garden": true, "/garden/events": true, "/garden/farm": true, "/garden/farm/events?since=1": true, "/garden?bead=": true,
		"/garden?bead=gd-abc": true, "/garden?at=gardenio&bead=gd-abc": true, "/garden/bead/gd-abc": true,
		"/": false, "": false, "/gd-abc": false, "/plans?project=x": false, "/agents/chat": false, "/gardeners": false, "/gardenx": false,
	} {
		if insideGarden(p) != want {
			t.Errorf("insideGarden(%q) = %v, want %v", p, !want, want)
		}
	}
}
