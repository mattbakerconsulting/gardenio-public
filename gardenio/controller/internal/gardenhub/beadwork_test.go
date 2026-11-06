package gardenhub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The bead panel's Work section (web/beadwork.js, gd-efoAiLlzRC): which relation rows a drawn node leaves, the heading
// of a list left with fewer rows, and when a fragment is read again.
func TestBeadWorkLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var V = require(process.argv[1]), assert = require("assert"), drawn = { c1: 1, r1: 1, p: 1 };
assert.ok(V.keeps("parents", "p", drawn), "Part of stays though the tree draws it");
assert.ok(!V.keeps("children", "c1", drawn) && V.keeps("children", "c2", drawn), "a child the tree draws goes, one it does not stays");
assert.ok(!V.keeps("related", "r1", drawn) && !V.keeps("blocked_by", "c1", drawn) && !V.keeps("blocks", "c1", drawn));
assert.strictEqual(V.heading("children", "Children (3)", 1, 3), "Other children (1)");
assert.strictEqual(V.heading("children", "Other children (2)", 2, 2), "Other children (2)", "nothing drawn: as it was");
assert.strictEqual(V.heading("blocked_by", "Waits on (2)", 1, 2), "Waits on (1)");
assert.ok(V.stale(null, 0), "never read: read it");
assert.ok(!V.stale({ f: {}, at: 1000 }, 5000) && V.stale({ f: {}, at: 1000 }, 7000), "a fragment is fresh 5 s");
assert.ok(!V.stale({ f: null, at: 1000 }, 20000) && V.stale({ f: null, at: 1000 }, 32000), "a failed read waits 30 s");
`
	js, _ := filepath.Abs(filepath.Join("web", "beadwork.js"))
	if out, err := exec.Command(node, "-e", script, js).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// Every bead of the fixture farm with children, in a real Chromium, lists each child once: in the Work tree when the
// Commons draws its node, in the panel's own lists when it does not (gd-efoAiLlzRC). Here the Commons daemon holds the bead
// view's beads and edges too, as one daemon does in production, so every bead has a node; GET /test/beadwork/kids names
// each bead's children. What is asserted lives in testdata/beadwork_browser.js.
func TestBeadWorkUnderChromium(t *testing.T) {
	underChromium(t, "beadwork_browser.js", func() *httptest.Server {
		srv, rig := cardWorldRig(t, time.Now, "", nil, nil, false)
		rig.beads.mu.Lock()
		defer rig.beads.mu.Unlock()
		had := map[string]bool{}
		for _, b := range rig.beads.beads {
			if got, _ := rig.commons.BatchGetBeads(t.Context(), []string{b.ID}); got[b.ID] != nil {
				had[b.ID] = true
			} else {
				rig.commons.Put(b)
			}
		}
		of := map[string][]string{}
		for _, d := range rig.beads.deps {
			if !had[d.BeadID] {
				rig.commons.Link(d.BeadID, d.DependsOnID, d.Type)
			}
			if d.Type == "parent-child" && rig.beads.beads[d.BeadID] != nil && rig.beads.beads[d.DependsOnID] != nil && !slices.Contains(of[d.DependsOnID], d.BeadID) {
				of[d.DependsOnID] = append(of[d.DependsOnID], d.BeadID)
			}
		}
		kids, _ := json.Marshal(of)
		rig.mux.HandleFunc("GET /test/beadwork/kids", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(kids)
		})
		return srv
	})
}
