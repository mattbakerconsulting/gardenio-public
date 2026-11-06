package gardenhub

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Costs panel's tests past 2.1's own (plan gd-gh5J0vVbJP step 2.4, gd-5HipEi6w2o): the costs kind's DOM half, the
// route's answers as costs.js reads them, the iframes' accessibility (titles, reduced motion, focus order) and the page's
// embedded-script budget. costs_test.go keeps 2.1's: the pure half, the card stack and the page wiring.

// The costs kind as workbench.js drives it, under node with a stub host (testdata/costs_kind_test.js): the loading paint
// and the answer, the chips, a range change holding the cards in place, the farm's figures going stale and back, a failed
// refresh and a failed first load, no charts, and closing.
func TestCostsKindUnderNode(t *testing.T) {
	r := nodeScript(t, "costs_kind_test.js")
	for _, k := range []string{"registered", "paint", "chips", "hold", "stale", "failed", "closed"} {
		if r[k] != true {
			t.Errorf("costs_kind_test.js did not reach %s: %v", k, r)
		}
	}
}

// The skeleton's shimmer is motion, so it lives only in the page's prefers-reduced-motion:no-preference block, on a box
// still waiting (neither loaded nor without Grafana): nowhere else does a costs rule animate or transition.
func TestCostsShimmerOnlyWithMotion(t *testing.T) {
	motion, rest := mediaBlock(pageCSS, "(prefers-reduced-motion:no-preference)")
	if !strings.Contains(motion, ".gp-box:not(.loaded):not(.unset){") || !strings.Contains(motion, "animation:gp-shimmer ") || !strings.Contains(motion, "@keyframes gp-shimmer{") {
		t.Fatalf("the no-preference block lost the waiting box's shimmer:\n%s", motion)
	}
	if strings.Contains(rest, "gp-shimmer") {
		t.Error("gp-shimmer appears outside the no-preference block: reduced motion would still see it")
	}
	rule := regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	for _, m := range rule.FindAllStringSubmatch(rest, -1) {
		if sel, body := m[1], m[2]; strings.Contains(sel, ".gp-") && (strings.Contains(body, "animation") || strings.Contains(body, "transition")) {
			t.Errorf("%s moves outside the no-preference block: %s", strings.TrimSpace(sel), body)
		}
	}
}

// The page's embedded scripts stay plain, hand-written and small (garden-grafana-panels.md, "Not taken: Grafana
// Scenes"): the Costs panel is one kind of under 24 KiB that builds no Grafana URL and loads nothing, and all of web/*.js
// stays under the page budget. A budget that trips is a decision, not a number to bump in passing: say in the PR what
// grew and why, and raise it there with an entry in testdata/jsbudget (see jsBudgetCaps).
func TestEmbeddedJSBudget(t *testing.T) {
	const (
		costsMax = 26 << 10 // costs.js, raw (2.1 shipped it at ~18 KiB; 24 until the tenant view, step 3.3 gd-kSG5BqJMRo, took it to ~24.8: its land: id, route and openers)
	)
	if n := len(pageCostsJS); n > costsMax {
		t.Errorf("web/costs.js is %d bytes, budget %d: the Costs panel is one Workbench kind", n, costsMax)
	}
	for _, bad := range []string{"https://", "http://", "import(", "<script", "document.write", "innerHTML", "insertAdjacentHTML", "embed_html"} {
		if strings.Contains(stripJSComments(pageCostsJS), bad) {
			t.Errorf("web/costs.js carries %q: the server builds every frame URL and the page sets them with setAttribute", bad)
		}
	}
	files, err := filepath.Glob(filepath.Join("web", "*.js"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no web/*.js: %v", err)
	}
	raw, gz := 0, 0
	var each []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var z bytes.Buffer
		w, _ := gzip.NewWriterLevel(&z, gzip.BestCompression)
		_, _ = w.Write(b)
		_ = w.Close()
		raw, gz = raw+len(b), gz+z.Len()
		each = append(each, fmt.Sprintf("%s %d", filepath.Base(f), len(b)))
	}
	pageMax, pageGzMax := jsBudgetCaps(t)
	t.Logf("web/*.js: %d files, %d bytes raw (budget %d), %d gzipped (budget %d)", len(files), raw, pageMax, gz, pageGzMax)
	if raw > pageMax || gz > pageGzMax {
		t.Errorf("web/*.js is %d bytes raw (budget %d) and %d gzipped (budget %d): %s", raw, pageMax, gz, pageGzMax, strings.Join(each, ", "))
	}
}

// jsBudgetCaps returns the web/*.js caps in bytes: the base below plus every entry in testdata/jsbudget. A PR that
// raises the budget adds its own entry file, <bead>.txt, holding one line "+<raw KiB> +<gz KiB> <reason>" sized to its
// own growth, so two such PRs never edit the same line, and because entries add, two that each count their own growth
// cannot together red main (gd-2ZqzHAxHIt; the ledger before it is in git history). A negative entry pays budget back.
func jsBudgetCaps(t *testing.T) (raw, gz int) {
	const (
		baseKiB   = 1591 // every web/*.js, raw: the cap on main when the ledger moved to entries
		baseGzKiB = 554  // the same, each gzipped at -9
	)
	rawKiB, gzKiB := baseKiB, baseGzKiB
	entries, err := filepath.Glob(filepath.Join("testdata", "jsbudget", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			t.Fatal(err)
		}
		line := strings.TrimSpace(string(b))
		f := strings.Fields(line)
		if strings.Contains(line, "\n") || len(f) < 3 {
			t.Fatalf(`%s: want one line "+<raw KiB> +<gz KiB> <reason>", got %q`, e, line)
		}
		r, rErr := strconv.Atoi(f[0])
		g, gErr := strconv.Atoi(f[1])
		if rErr != nil || gErr != nil {
			t.Fatalf(`%s: want whole KiB as "+<raw KiB> +<gz KiB> <reason>", got %q`, e, line)
		}
		rawKiB, gzKiB = rawKiB+r, gzKiB+g
	}
	return rawKiB << 10, gzKiB << 10
}

// stripJSComments drops /* */ and // comments, so a URL in a doc comment is not code.
func stripJSComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	return regexp.MustCompile(`(?m)(^|[^:\\])//.*$`).ReplaceAllString(s, "$1")
}

// The route answers what costs.js reads, for both sets over every chip: the range it asked for, the dashboard link, and
// per card an id the src's panelId names, the words, a src on the solo path and an Open on the pop-out path, and with no
// Grafana base neither (so paintCostCards paints the words and a range change sees the base gone).
func TestChartsAnswerWhatCostsReads(t *testing.T) {
	farm := &Farm{cost: []byte(`{"projects":[{"project":"gardenio"}]}`)}
	for _, base := range []string{"https://grafana.example", ""} {
		h := &Handler{GrafanaBaseURL: base, Farm: farm}
		for _, path := range []string{"/garden/charts/costs-fleet", "/garden/charts/costs-project?project=gardenio"} {
			for _, rng := range []string{"today", "7d", "30d"} {
				sep := "?"
				if strings.Contains(path, "?") {
					sep = "&"
				}
				p := path + sep + "range=" + rng + "&theme=dark"
				code, doc := getQueue(t, h, p)
				if code != http.StatusOK {
					t.Errorf("%s (base %q): %d", p, base, code)
					continue
				}
				if doc["range"] != rng {
					t.Errorf("%s: range %v", p, doc["range"])
				}
				if d, _ := doc["open_dashboard_url"].(string); (base == "") != (d == "") {
					t.Errorf("%s (base %q): open_dashboard_url %q", p, base, d)
				}
				panels, _ := doc["panels"].([]any)
				seen := map[float64]bool{}
				for i, x := range panels {
					c := x.(map[string]any)
					id, _ := c["id"].(float64)
					title, _ := c["title"].(string)
					src, _ := c["src"].(string)
					open, _ := c["open_url"].(string)
					if id <= 0 || seen[id] || title == "" || c["scope"] == "" {
						t.Errorf("%s card %d: id %v title %q scope %v", p, i, c["id"], title, c["scope"])
					}
					seen[id] = true
					if _, ok := c["note"].(string); !ok {
						t.Errorf("%s card %d: note is not a string: %v", p, i, c["note"])
					}
					if base == "" {
						if src != "" || open != "" {
							t.Errorf("%s card %d: no base, yet src %q open %q", p, i, src, open)
						}
						continue
					}
					if !strings.HasPrefix(src, base+"/d-solo/") || !strings.Contains(src, fmt.Sprintf("?panelId=%d&", int(id))) {
						t.Errorf("%s card %d: src %q does not name panel %v", p, i, src, id)
					}
					if !strings.HasPrefix(open, base+"/d/") || !strings.Contains(open, fmt.Sprintf("?viewPanel=%d&", int(id))) {
						t.Errorf("%s card %d: open_url %q does not name panel %v", p, i, open, id)
					}
				}
				if len(panels) == 0 {
					t.Errorf("%s: no cards", p)
				}
			}
		}
	}
}

// Near misses at the route: a set name that is almost one is a 404, any input outside its closed set a 400 (with no
// Grafana base too: nothing to build is no excuse for a fallback), a project param the fleet set does not take a 400, a
// land never read from the request, and only GET is served.
func TestChartsRejectNearMisses(t *testing.T) {
	farm := &Farm{cost: []byte(`{"projects":[{"project":"gardenio"}]}`)}
	for _, base := range []string{"https://grafana.example", ""} {
		h := &Handler{GrafanaBaseURL: base, Farm: farm}
		for path, want := range map[string]int{
			"/garden/charts/Costs-Fleet":                               http.StatusNotFound,
			"/garden/charts/costs-fleet.json":                          http.StatusNotFound,
			"/garden/charts/costs":                                     http.StatusNotFound,
			"/garden/charts/costs-fleet%2F..":                          http.StatusNotFound,
			"/garden/charts/costs-fleet?range=Today":                   http.StatusBadRequest,
			"/garden/charts/costs-fleet?range=%207d":                   http.StatusBadRequest,
			"/garden/charts/costs-fleet?range=1y&range=7d":             http.StatusBadRequest, // the first value is the one read
			"/garden/charts/costs-fleet?theme=Dark":                    http.StatusBadRequest,
			"/garden/charts/costs-fleet?project=fleet":                 http.StatusBadRequest,
			"/garden/charts/costs-project?project=fleet":               http.StatusBadRequest, // not a project the farm knows
			"/garden/charts/costs-project?project=gardenio%0A":         http.StatusBadRequest,
			"/garden/charts/costs-project?project=g%C3%A4rdenio":       http.StatusBadRequest,
			"/garden/charts/costs-project?project=gardenio&range=90d":  http.StatusBadRequest,
			"/garden/charts/costs-project?project=gardenio&theme=auto": http.StatusBadRequest,
		} {
			if got := chartsStatus(t, h, path); got != want {
				t.Errorf("base %q: %s = %d, want %d", base, path, got, want)
			}
		}
	}
	h := &Handler{GrafanaBaseURL: "https://grafana.example", Farm: farm}
	code, doc := getQueue(t, h, "/garden/charts/costs-project?project=gardenio&land=true")
	_, op := getQueue(t, h, "/garden/charts/costs-project?project=gardenio")
	if code != http.StatusOK || len(doc["panels"].([]any)) != len(op["panels"].([]any)) {
		t.Errorf("land=true from the request changed the cards: %d, %d vs %d", code, len(doc["panels"].([]any)), len(op["panels"].([]any)))
	}
	if code, doc := getQueue(t, h, "/garden/charts/costs-fleet?range=7d&range=1y"); code != http.StatusOK || doc["range"] != "7d" {
		t.Errorf("a second range value was read: %d %v", code, doc["range"])
	}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(m, "/garden/charts/costs-fleet", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /garden/charts/costs-fleet = %d, want 405", m, rec.Code)
		}
	}
}

// The Costs panel's frames in a real Chromium (testdata/costs_browser.js): every frame titled by its card, a waiting box
// shimmering only when motion is allowed, keyboard focus reaching each card's Open link and its frame in order and leaving
// again, and the cards fitting a phone's sheet. The page is cardWorld's; its charts route answers on the same server with
// a stub Grafana under /grafana, so the frames load and nothing leaves the test.
func TestCostsUnderChromium(t *testing.T) {
	underChromium(t, "costs_browser.js", func() *httptest.Server {
		world := cardWorld(t, time.Now)
		charts := &Handler{}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /garden/charts/{set}", charts.chartsPanel)
		mux.HandleFunc("GET /grafana/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>stub panel</title><body style="margin:0;background:#111;color:#eee"><button type="button">Panel menu</button> %s</body>`,
				strings.ReplaceAll(r.URL.Query().Get("panelId"), "<", ""))
		})
		mux.Handle("/", world.Config.Handler)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		charts.GrafanaBaseURL = srv.URL + "/grafana"
		return srv
	})
}
