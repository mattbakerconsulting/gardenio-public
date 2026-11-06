package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
	"gardenio/controller/internal/gzipmw"
)

// TestLandsPerfBaseline is not a check: it is how the Lands UX performance pass (plan gd-MEGB3dEaHu, step P0.1
// gd-j8jXq1TC1m) measures /garden and its surfaces. It serves the goldens' world (cardWorld at goldenNow with goldenPorch)
// and runs testdata/perf_baseline.js against it, which loads each surface cold and warm in Chromium on a throttled phone
// (4x CPU, 4G) and a desktop and writes results.json and summary.md into LANDS_PERF_OUT: page weight, requests, TTFB,
// FCP, LCP, TTI, the first interaction's INP, long tasks, and the JS heap after an idle spell.
//
// Runs only when LANDS_PERF_OUT is set (`make lands-perf-baseline OUT=<dir>`); LANDS_PERF_SURFACES, LANDS_PERF_PROFILES,
// LANDS_PERF_RUNS, LANDS_PERF_IDLE and LANDS_PERF_PAN narrow it (perf_baseline.js's header). LANDS_PERF_SERVE=<seconds>
// only serves the fixture farm that long and logs its URL, to poke at it or point perf_baseline.js at it by hand. Same
// browser needs as TestCommonsUXShoot. LANDS_PERF_LATENCY=1 (the script's latency pass) serves perfWorld instead, whose
// Commons is live and which has the routes the pass drives. Either is served through beads-viewer's gzip (gzipmw), which
// production puts outermost, so the byte budgets grade what a phone is sent, the fetches' JSON and the Glass's xterm
// bundle included (gd-GO1ba3rosB).
func TestLandsPerfBaseline(t *testing.T) {
	out, serve := os.Getenv("LANDS_PERF_OUT"), os.Getenv("LANDS_PERF_SERVE")
	if out == "" && serve == "" {
		t.Skip("measures /garden's load and response for the Lands performance pass: `make lands-perf-baseline OUT=<dir>` (LANDS_PERF_OUT)")
	}
	var srv *httptest.Server
	if os.Getenv("LANDS_PERF_LATENCY") == "1" {
		srv = perfWorld(t)
	} else {
		var rig *cardRig
		srv, rig = cardWorldRig(t, func() time.Time { return goldenNow }, "", nil, nil, false, goldenPorch()...)
		busyChannel(rig.commons, goldenNow)
	}
	srv = httptest.NewServer(gzipmw.Middleware(srv.Config.Handler))
	t.Cleanup(srv.Close)
	if serve != "" {
		s, err := strconv.Atoi(serve)
		if err != nil {
			t.Fatalf("LANDS_PERF_SERVE=%q: want seconds", serve)
		}
		t.Logf("fixture farm at %s for %ds", srv.URL, s)
		time.Sleep(time.Duration(s) * time.Second)
		return
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
	script, _ := filepath.Abs(filepath.Join("testdata", "perf_baseline.js"))
	out, _ = filepath.Abs(out)
	cmd := exec.Command(node, script, srv.URL, mods, out)
	cmd.Env = append(os.Environ(), "LANDS_PERF_TARGET=fixture")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // a full run takes tens of minutes: show progress as it goes
	if err := cmd.Run(); err != nil {
		t.Fatalf("perf_baseline.js: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	t.Logf("%s", strings.TrimSpace(string(b)))
}

// perfWorld is the goldens' world (cardWorld at goldenNow with goldenPorch) with its Commons live, for perf_baseline.js's
// latency pass (the Commons v2 budgets B12-B15, plan gd-lRaCxsiLvC step 1 gd-J3LZYVcuBK), and the two routes the pass
// drives it with:
//
//	POST /test/perf/say?ch=&thread=&text=&mark=  says text in channel ch (in thread, when set) the way the daemon does:
//	     stored, logged for replay and put on the Commons stream; it answers {"at": <unix ms>}, the moment it was put
//	     there, which the pass times the paint from (the page and the farm share the host's clock)
//	POST /test/perf/forget?mark=                 forgets every message whose text carries mark: the pass's sends and
//	     says, so the next load reads the farm it opened on
//
// A pass writes into the farm, so TestCommonsUXShoot measures on one of these and shoots on cardWorld, which stays the
// goldens' world.
func perfWorld(t *testing.T) *httptest.Server {
	t.Helper()
	srv, rig := cardWorldRig(t, func() time.Time { return goldenNow }, "", nil, nil, true, goldenPorch()...)
	busyChannel(rig.commons, goldenNow)
	var n atomic.Int64
	rig.mux.HandleFunc("POST /test/perf/say", func(w http.ResponseWriter, r *http.Request) {
		d, q := rig.commons, r.URL.Query()
		at := goldenNow
		hello, _ := d.GetBead(beadsapi.WithUserBearer(context.Background(), commonsViewer), "gd-msgHello")
		b := &beadsapi.BeadDetail{ID: "gd-msgPerf" + strconv.FormatInt(n.Add(1), 10), Type: "message", Status: "closed", Title: q.Get("text"), CreatedAt: at, UpdatedAt: at,
			Fields: map[string]string{"channel_id": q.Get("ch"), "author": "oidc:flo@example.com", "text": q.Get("text")}}
		if th := q.Get("thread"); th != "" {
			b.Fields["thread_id"] = th
		}
		if hello != nil {
			b.Labels = hello.Labels
		}
		d.Put(b)
		rig.beads.put(b) // the bead view reads it too (cardWorldRig)
		d.Log("beads.bead.created", b, at)
		pushed := time.Now()
		select {
		case rig.commonsEvents <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(b)}:
		default:
			http.Error(w, "the Commons stream is full", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]float64{"at": float64(pushed.UnixMicro()) / 1000})
	})
	rig.mux.HandleFunc("POST /test/perf/forget", func(w http.ResponseWriter, r *http.Request) {
		mark := r.URL.Query().Get("mark")
		if mark == "" {
			http.Error(w, "mark is required", http.StatusBadRequest)
			return
		}
		marked := func(b *beadsapi.BeadDetail) bool {
			return b.Type == "message" && strings.Contains(b.Fields["text"], mark)
		}
		got := rig.commons.Forget(marked)
		rig.beads.mu.Lock() // and the bead view's copies (cardWorldRig)
		maps.DeleteFunc(rig.beads.beads, func(_ string, b *beadsapi.BeadDetail) bool { return marked(b) })
		rig.beads.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"forgot": got})
	})
	return srv
}

// busyChannel puts #busy in gardenio's land with as many messages as production's #general held on 10-23 (2,621,
// gd-h2GZsjqwA7), for perf_baseline.js's opt-in surface busy (gd-PXnAoQle99): one every minute or so over the two days
// before now, nearly all machine-written, one in eight a reply to a recent root. Put, not logged: the stream replays none.
func busyChannel(d *commonstest.Daemon, now time.Time) {
	land := []string{"project:gardenio"}
	d.Put(&beadsapi.BeadDetail{ID: "gd-chBusy", Type: "channel", Title: "Busy", Description: "As busy as #general", Status: "open", Labels: land,
		CreatedAt: now.Add(-72 * time.Hour), Fields: map[string]string{"name": "busy", "channel_kind": "user"}})
	authors := []string{"agent:slack-bridge", "agent:slack-bridge", "agent:gardenio-controller", "agent:gardenio-plan-gardener", "agent:gardenio-crew-busy", "oidc:flo@example.com"}
	const n = 2621
	var root string
	for i := range n {
		id := fmt.Sprintf("gd-busy%05d", i)
		f := map[string]string{"channel_id": "gd-chBusy", "author": authors[i%len(authors)],
			"text": fmt.Sprintf("Message %d: crew gardenio-crew-%d squawked progress on its task; the merge desk filed an escalation for the PR and the gardener woke the next step.", i, i%97)}
		if i%8 == 7 && root != "" {
			f["thread_id"] = root
		} else if i%3 == 0 {
			root = id
		}
		d.Put(&beadsapi.BeadDetail{ID: id, Type: "message", Title: f["text"], Status: "closed", Labels: land,
			CreatedAt: now.Add(-time.Duration(n-i) * 66 * time.Second), Fields: f})
	}
}
