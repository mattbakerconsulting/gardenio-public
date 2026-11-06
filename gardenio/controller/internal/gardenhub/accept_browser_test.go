package gardenhub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// TestCommonsUXShoot is not a check: it is how the commons-ux-acceptance skill (.claude/skills/commons-ux-acceptance,
// gd-vzsNYKoGNX) gets a /garden surface on screen to grade. It serves the goldens' world (cardWorld at goldenNow with
// goldenPorch) and runs the skill's shoot.js against it, which captures each surface in every cell of the Commons UX
// rubric's capture matrix (gardenio/docs/commons-ux-rubric.md §1), runs the §2 DOM probes, and leaves PNGs, probe JSON and
// a draft COMMONS-UX-GRADE.md per surface in COMMONS_UX_OUT. A grader then reads the PNGs and finishes the grade. With
// perf on (COMMONS_UX_PERF; by default when COMMONS_UX_SURFACES names surfaces) it also measures each surface against the
// rubric's §7 budgets with testdata/perf_baseline.js, grades it with testdata/perf_gate.js, and writes a perf JSON per
// phone-light and desktop-light cell and the draft's perf rows (gd-qeaDwFSDqq). The perf pass measures on a second farm,
// perfWorld (COMMONS_UX_PERF_URL to shoot.js): its latency pass (B12-B15) sends, says and opens for real, with the
// Commons stream live, and so must not write into the world the shots are taken of.
//
// Runs only when COMMONS_UX_OUT is set (`make commons-ux-shoot OUT=<dir>`); COMMONS_UX_SURFACES and COMMONS_UX_CELLS
// narrow it, COMMONS_UX_PERF_RUNS and COMMONS_UX_PERF_IDLE shorten the perf pass, and COMMONS_UX_SCRIPT points at another
// shoot.js (a branch's copy, say). Same browser needs as TestGardenGoldens.
func TestCommonsUXShoot(t *testing.T) {
	out := os.Getenv("COMMONS_UX_OUT")
	if out == "" {
		t.Skip("shoots for the commons-ux-acceptance skill: `make commons-ux-shoot OUT=<dir>` (COMMONS_UX_OUT)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		t.Fatalf("playwright-core not installed under %s (make gardenhub-browser-deps)", mods)
	}
	script := os.Getenv("COMMONS_UX_SCRIPT")
	if script == "" {
		script = filepath.Join("..", "..", "..", "..", ".claude", "skills", "commons-ux-acceptance", "scripts", "shoot.js")
	}
	script, _ = filepath.Abs(script)
	out, _ = filepath.Abs(out)
	golden, _ := filepath.Abs(filepath.Join("testdata", "golden_browser.js"))
	fonts, _ := filepath.Abs(filepath.Join("testdata", "golden", "fonts"))

	srv := cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	cmd := exec.Command(node, script, srv.URL, mods, out, golden, fonts, strconv.FormatInt(goldenNow.UnixMilli(), 10))
	cmd.Env = append(os.Environ(), "COMMONS_UX_PERF_URL="+perfWorld(t).URL)
	b, err := cmd.CombinedOutput()
	t.Logf("%s", strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("shoot.js: %v", err)
	}
}

// A bead's Commons node and its bead view are one document (plan gd-gO1fTT7oGC, step gd-0GnBi7SRfK): the fixture wish
// opened as /garden?bead= and as /garden/c/n/ draws byte-identical face and Conversation sections, and the node keeps its
// own tree and rails around them. A DOM probe and not a Go test: what is compared is two pages' rendering of one bead.
// What is asserted lives in testdata/onebead_browser.js.
func TestOneBeadViewUnderChromium(t *testing.T) {
	underChromium(t, "onebead_browser.js", func() *httptest.Server {
		srv, _ := cardWorldRig(t, time.Now, "", nil, nil, false)
		return srv
	}, wishSlack)
}

// The open bead panel's conversation is live (plan gd-gO1fTT7oGC, step gd-MpqfrPhLZP): with the fixture wish open as
// /garden?bead=, a squawk its crew writes to the farm appears in the panel's Conversation with its counts bumped, and its
// task closing greys its Work row, neither by a reload nor a read of the bead's document. What is asserted lives in
// testdata/convlive_browser.js; the two routes below write as kbeads would, the bead in the daemon and its event on the
// Commons stream:
//
//	POST /test/squawk?thread=&text=  the thread's crew squawks text
//	POST /test/close?id=             closes a bead
func TestConversationLiveUnderChromium(t *testing.T) {
	underChromium(t, "convlive_browser.js", func() *httptest.Server {
		srv, rig := cardWorldRig(t, time.Now, "", nil, nil, true)
		squawkRoute(rig)
		rig.mux.HandleFunc("POST /test/close", func(w http.ResponseWriter, r *http.Request) {
			b, _ := rig.commons.GetBead(r.Context(), r.URL.Query().Get("id"))
			if b == nil {
				http.NotFound(w, r)
				return
			}
			c := *b
			c.Status, c.UpdatedAt = "closed", time.Now()
			rig.commons.Put(&c)
			rig.commonsEvents <- beadsapi.SSEEvent{Event: "beads.bead.closed", Data: commonstest.Payload(&c)}
		})
		return srv
	}, wishSlack, wishSlack+commonstest.WishTask)
}

// squawkRoute adds POST /test/squawk?thread=&text= to rig: the thread's crew squawks text, written as kbeads would, the
// bead in the daemon and its event on the Commons stream. It answers the squawk's id.
func squawkRoute(rig *cardRig) {
	var n atomic.Int64
	rig.mux.HandleFunc("POST /test/squawk", func(w http.ResponseWriter, r *http.Request) {
		head, agent, now := r.URL.Query().Get("thread"), "gardenio-one-bead-view-k2p", time.Now()
		b := &beadsapi.BeadDetail{ID: fmt.Sprintf("gd-sqLive%d", n.Add(1)), Type: "message", Kind: "data", Status: "closed", CreatedBy: agent,
			CreatedAt: now, UpdatedAt: now, Title: r.URL.Query().Get("text"), Labels: []string{"from:" + agent, "project:gardenio", "squawk"},
			Fields: map[string]string{"text": r.URL.Query().Get("text"), "thread_id": head, "source_agent": agent}}
		rig.commons.Put(b)
		rig.beads.put(b)
		rig.commons.Log("beads.bead.created", b, now)
		rig.commonsEvents <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(b)}
		fmt.Fprint(w, b.ID)
	})
}

// A cut text opens in place and never navigates (gd-Fmq3fgqFTm): on the fixture farm, the stress steward's #general line
// past the transcript's cut, a squawk's face past the bead view's fold (in the panel) and its line in the wish's
// Conversation (on the wish's node) each open whole and shut again from their Read it all / Show all button, with the
// address and the scroll as they were, and each view's Expand all opens every cut in it and is then Collapse all. A DOM
// probe and not a Go test: what is asserted is what a press does to the page. What is asserted lives in
// testdata/cuts_browser.js.
func TestCutsOpenInPlaceUnderChromium(t *testing.T) {
	underChromium(t, "cuts_browser.js", func() *httptest.Server {
		srv, rig := cardWorldRig(t, time.Now, "", nil, nil, true)
		squawkRoute(rig)
		return srv
	}, wishSlack, "/garden/c/s/"+swLand)
}
