package commons

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// A bead a card or message links to reads as its title, with the id in title=
// (gd-YvdhZD2UtL, rubric C12); one of another land reads as one the daemon
// does not have.
func TestBeadNames_TitleNotID(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	d.Put(&beadsapi.BeadDetail{ID: "cl-AbCdEfGhIj", Type: "task", Status: "open", Labels: []string{"project:claimlayer"}, Title: "Chart lints again"})
	_, body := get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, `<span>Blocks <a class="cm-bead" href="/base/garden?bead=cl-AbCdEfGhIj" data-bead="cl-AbCdEfGhIj" title="Chart lints again (cl-AbCdEfGhIj)">Chart lints again</a></span>`)
	_, body = get(t, h, "/garden/c/m/cl-dcard1/lightbox/other", "claimlayer")
	mustContain(t, body, `See <a class="cm-bead" href="/base/garden?bead=cl-AbCdEfGhIj" data-bead="cl-AbCdEfGhIj" title="Chart lints again (cl-AbCdEfGhIj)">Chart lints again</a>`)

	// the short title, when the bead has one, is what reads (gd-QTukIA8Nvv)
	d.Put(&beadsapi.BeadDetail{ID: "cl-AbCdEfGhIj", Type: "task", Status: "open", Labels: []string{"project:claimlayer"}, Title: "Chart lints again, in every chart job", ShortTitle: " chart lints again "})
	_, body = get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, `<span>Blocks <a class="cm-bead" href="/base/garden?bead=cl-AbCdEfGhIj" data-bead="cl-AbCdEfGhIj" title="chart lints again (cl-AbCdEfGhIj)">chart lints again</a></span>`)

	d.Put(&beadsapi.BeadDetail{ID: "cl-AbCdEfGhIj", Type: "task", Status: "open", Labels: []string{"project:gardenio"}, Title: "Gardenio secret task"})
	_, body = get(t, h, "/test/m/cl-ecard1", "claimlayer")
	mustContain(t, body, `title="cl-AbCdEfGhIj: not found" data-titles="off">Bead not found <span class="cm-bead-id">cl-AbCdEfGhIj</span></a>`)
	mustNotContain(t, body, "Gardenio secret task")
}

// A snapshot taken at a plan its framed list does not title is captioned with
// the plan's title as the daemon has it; the address stays in title=.
func TestBeadNames_SnapPlaceRead(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-AbCdEfGhIj", Type: "epic", Status: "open", Labels: []string{"project:claimlayer"}, Title: "Plot one"})
	d.Put(&beadsapi.BeadDetail{ID: "cl-snap1", Type: "message", Title: "Snapshot of the farm at claimlayer/cl-AbCdEfGhIj", Status: "closed",
		Labels: []string{"project:claimlayer"}, CreatedAt: t0, Fields: map[string]string{
			"channel_id": "cl-chgen", "author": "oidc:bo@claimlayer.example", "kind": "text", "origin": "commons",
			"files": `[{"name":"farm.png","mimetype":"image/png","size":48213,"url":"http://example.com/base/garden/c/f/claimlayer/cl-chgen/0123456789abcdef0123456789abcdef/farm.png"}]`,
			"snap":  `{"kind":"farm","at":"claimlayer/cl-AbCdEfGhIj","z":2,"who":"all"}`,
		}})
	h := commonsServer(t, d)
	_, body := get(t, h, "/test/m/cl-snap1", "claimlayer")
	mustContain(t, body, `<span class="cm-snap-cap" title="claimlayer/cl-AbCdEfGhIj">Farm at Plot one (z2, Everyone)</span>`)
}

func TestSnapPlaceID(t *testing.T) {
	for at, want := range map[string]string{
		"": "", "yard": "", "lab": "", "~/lab": "", "lab/gd-AbCdEfGhIj": "gd-AbCdEfGhIj", "gardenio/gd-1": "gd-1",
		"lab%2Fgd-AbCdEfGhIjKlMn": "gd-AbCdEfGhIjKlMn", "lab/in-flight": "", "lab/gd-A1/gd-A1y": "gd-A1y",
	} {
		if got := snapPlaceID(at); got != want {
			t.Errorf("snapPlaceID(%q) = %q, want %q", at, got, want)
		}
	}
}
