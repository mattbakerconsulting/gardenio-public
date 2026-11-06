package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// badge_image_test.go — badges plan gd-pQZO9ie11D, step 9 (gd-5qEMPflr4z): a
// bead's badge is drawn large where there is room (the bead page and plan
// detail headers at 64 px, og:image on those pages, the deck card cover), and
// nowhere when the bead has none.
//
// The fixture URLs are what `gb badge --upload` writes for the round-1 poster
// sample of plan gd-N6I3Oa7uDg (gardenio/docs/heraldry/samples/badges-round-1/
// opus/poster): <viewer>/image/<prefix>/images/badges/<id>/<sha8>-<tier>.<ext>.
// Regenerate the goldens after an intentional template change with
// `GB_UPDATE_GOLDEN=1 go test ./cmd/beads-viewer/ -run TestBadge_Golden`.

const (
	testBadgeSVGURL = "https://gardenio.clonk.tv/beads/image/coop/sessions/images/badges/gd-plan1/cd3190aa-model.svg"
	testBadgePNGURL = "https://gardenio.clonk.tv/beads/image/coop/sessions/images/badges/gd-plan1/544a0dc9-model-512.png"
	testBadgeSVG    = "/image/coop/sessions/images/badges/gd-plan1/cd3190aa-model.svg"
	testBadgePNG    = "/image/coop/sessions/images/badges/gd-plan1/544a0dc9-model-512.png"
)

// giveBadge writes the uploaded badge's two URLs onto b, as gb badge --upload
// does.
func giveBadge(b *beadsapi.BeadDetail) {
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	b.Fields["icon_svg_url"] = testBadgeSVGURL
	b.Fields["icon_png_url"] = testBadgePNGURL
}

// badgeBeads gives every epic in f a badge, the way armBeads gives them arms.
func badgeBeads(f *fakeDaemon) *fakeDaemon {
	for _, b := range f.beads {
		if b != nil && b.Type == "epic" {
			giveBadge(b)
		}
	}
	for _, r := range []*beadsapi.ListBeadsResult{f.hotEpicsResp, f.listResp} {
		if r == nil {
			continue
		}
		for _, b := range r.Beads {
			if b != nil && b.Type == "epic" {
				giveBadge(b)
			}
		}
	}
	return f
}

func TestBadgeProxyPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{testBadgeSVGURL, testBadgeSVG},
		{"http://localhost:8080/image/coop/sessions/images/x.png", "/image/coop/sessions/images/x.png"},
		{"/image/plans/badge-abc123.svg", "/image/plans/badge-abc123.svg"},
		{"/beads/image/coop/sessions/images/a%20b.svg?v=2#top", "/image/coop/sessions/images/a%20b.svg"},
		{"https://h/a/image/b/image/coop/k.svg", "/image/coop/k.svg"}, // the last /image/ wins
		{"", ""},
		{"https://elsewhere.example/badge.svg", ""}, // not through the proxy
		{"javascript:alert(1)//image/x", ""},
		{"data:image/svg+xml,/image/x", ""},
		{"https://h/image/", ""},
		{"https://h/image/coop/../secrets", ""},
	} {
		if got := badgeProxyPath(tc.in); got != tc.want {
			t.Errorf("badgeProxyPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBeadBadge(t *testing.T) {
	if beadBadge(nil) != nil || beadBadge(&beadsapi.BeadDetail{}) != nil {
		t.Errorf("a bead with no icon fields has no badge")
	}
	png := beadBadge(&beadsapi.BeadDetail{Fields: map[string]string{"icon_png_url": testBadgePNGURL}})
	if png == nil || png.Src() != testBadgePNG {
		t.Errorf("a PNG-only badge draws the PNG, got %+v", png)
	}
	b := &beadsapi.BeadDetail{}
	giveBadge(b)
	if got := beadBadge(b); got == nil || got.Src() != testBadgeSVG || got.PNG != testBadgePNG {
		t.Errorf("a full badge draws the SVG and keeps the PNG for og:image, got %+v", got)
	}
}

// badgeFragments are the regions of each page the badge touches; the goldens
// pin exactly these, so unrelated page churn does not move them.
var (
	ogImageRE      = regexp.MustCompile(`<meta property="og:image"[^>]*>`)
	titleBlockRE   = regexp.MustCompile(`(?s)<header id="title-block".*?</header>`)
	planTitleRowRE = regexp.MustCompile(`(?s)<div class="plan-title-row">.*?</h1>\s*</div>`)
	deckCardHeadRE = regexp.MustCompile(`(?s)<article class="deck-card[^"]*"[^>]*>.*?</h2>`)
)

func mustFind(t *testing.T, re *regexp.Regexp, body, where string) string {
	t.Helper()
	m := re.FindString(body)
	if m == "" {
		t.Fatalf("%s: no match for %s", where, re)
	}
	return m
}

// TestBadge_Golden pins the plan detail header, the bead page header (with
// og:image) and the deck card heads, with and without a badge.
func TestBadge_Golden(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
	type surface struct {
		name, target string
		seed         func() *fakeDaemon
		regions      []*regexp.Regexp
	}
	for _, golden := range []struct {
		path     string
		surfaces []surface
	}{
		{"testdata/badge_plan_header.golden.html", []surface{
			{"plan detail", "/plans/gd-plan1", seedPlansFake, []*regexp.Regexp{ogImageRE, planTitleRowRE}},
		}},
		{"testdata/badge_bead_header.golden.html", []surface{
			{"bead page", "/gd-plan1?bead=1", seedPlansFake, []*regexp.Regexp{ogImageRE, titleBlockRE}},
		}},
		{"testdata/badge_deck_card.golden.html", []surface{
			{"deck card fragment", "/plans/gd-plan1/card", seedDeckCardFatFace, []*regexp.Regexp{deckCardHeadRE}},
			{"deck page", "/deck", seedDeck, []*regexp.Regexp{deckCardHeadRE}},
		}},
	} {
		t.Run(golden.path, func(t *testing.T) {
			var got strings.Builder
			for _, s := range golden.surfaces {
				for _, variant := range []struct {
					label string
					seed  func() *fakeDaemon
				}{
					{"armed, with badge", func() *fakeDaemon { return badgeBeads(armBeads(s.seed())) }},
					{"armed, no badge", func() *fakeDaemon { return armBeads(s.seed()) }},
				} {
					body := getWith(t, mkServerWithNow(t, variant.seed(), now), s.target)
					got.WriteString("<!-- " + s.name + ": " + variant.label + " -->\n")
					for _, re := range s.regions {
						if re == ogImageRE {
							got.WriteString(re.FindString(body) + "\n") // absent without a badge
							continue
						}
						got.WriteString(mustFind(t, re, body, s.name) + "\n")
					}
				}
			}
			compareGolden(t, golden.path, got.String())
		})
	}
}

func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("GB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("updated golden %s (%d bytes)", path, len(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s drifted (regenerate with GB_UPDATE_GOLDEN=1 if intended)\n--- got ---\n%s", path, got)
	}
}

// The badge sits beside the arms run at 64 px in both headers, loads through
// this viewer's /image proxy, and og:image is the absolute PNG on the host the
// page was asked for.
func TestBadge_HeadersAndOGImage(t *testing.T) {
	f := badgeBeads(armBeads(seedPlansFake()))
	mux := mkServer(t, f)
	for _, target := range []string{"/plans/gd-plan1", "/gd-plan1?bead=1"} {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "viewer.example")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", target, w.Code)
		}
		body := w.Body.String()
		img := `<img class="badge-64" src="` + testBadgeSVG + `" alt="" width="64" height="64">`
		if strings.Count(body, img) != 1 {
			t.Errorf("%s: want one 64 px badge through /image, got %d", target, strings.Count(body, img))
		}
		if i, j := strings.Index(body, img), strings.Index(body, testArmsSpan(t, "64")); i < 0 || j < i {
			t.Errorf("%s: the badge must come before (left of) the 64 px arms run", target)
		}
		og := `<meta property="og:image" content="https://viewer.example` + testBadgePNG + `">`
		if !strings.Contains(body, og) {
			t.Errorf("%s: want og:image %s, got %q", target, og, ogImageRE.FindString(body))
		}
	}
}

// Any bead can carry a badge: a task (which bears no arms) shows its badge in
// the bead page header without arms.
func TestBadge_TaskBeadPage(t *testing.T) {
	f := seedPlansFake()
	task := &beadsapi.BeadDetail{ID: "gd-badgetask", Title: "Badged step", Type: "task", Status: "open", UpdatedAt: time.Now()}
	giveBadge(task)
	f.beads[task.ID] = task
	body := getWith(t, mkServer(t, f), "/gd-badgetask?bead=1")
	head := mustFind(t, titleBlockRE, body, "task page")
	if !strings.Contains(head, `class="title-block title-block-badged"`) || !strings.Contains(head, `src="`+testBadgeSVG+`"`) {
		t.Errorf("task page header lacks its badge:\n%s", head)
	}
	if strings.Contains(head, `class="arms `) {
		t.Errorf("a task drew arms beside its badge")
	}
}

// A bead with no badge shows nothing extra: no picture, no og:image, no
// cover, no badged header class, on any of the four surfaces.
func TestBadge_NoneShowsNothing(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		target string
		seed   func() *fakeDaemon
	}{
		{"/plans/gd-plan1", seedPlansFake},
		{"/gd-plan1?bead=1", seedPlansFake},
		{"/plans/gd-plan1/card", seedDeckCardFatFace},
		{"/deck", seedDeck},
	} {
		body := getWith(t, mkServerWithNow(t, armBeads(tc.seed()), now), tc.target)
		for _, mark := range []string{`<img class="badge-64"`, `title-block-badged"`, `<img class="deck-card-cover"`, `property="og:image"`, `src="/image/`} {
			if strings.Contains(body, mark) {
				t.Errorf("%s without a badge drew %q", tc.target, mark)
			}
		}
	}
}

// The arms run stays the mark everywhere small (R9): the plan card, the peek
// and cmd-k never draw the badge picture, even when the plan has one.
func TestBadge_NotOnSmallSurfaces(t *testing.T) {
	for _, target := range []string{"/plans", "/plans/gd-plan1/peek"} {
		body := getWith(t, mkServer(t, badgeBeads(armBeads(seedPlansFake()))), target)
		assertArms(t, target, body, "40")
		if strings.Contains(body, "/image/coop/sessions/images/badges/") {
			t.Errorf("%s drew the badge picture; small slots carry only the arms run", target)
		}
	}
	f := newFake()
	plan := &beadsapi.BeadDetail{ID: "gd-arms", Title: "Armed plan", Type: "epic", Status: "open", Labels: []string{"plan"}, Heraldry: testArms}
	giveBadge(plan)
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	if body := getWith(t, mkServer(t, f), "/cmdk/suggest?q=armed"); strings.Contains(body, "/image/") {
		t.Errorf("cmd-k drew the badge picture")
	}
}

// `gb badge --upload` writes icon_svg_url and icon_png_url onto the bead; the
// same running viewer shows the picture on the next page load, with no
// restart, on the bead page, the plan detail and the deck card.
func TestBadge_UploadShowsWithoutRestart(t *testing.T) {
	f := armBeads(seedDeckCardFatFace())
	mux := mkServer(t, f)
	targets := []string{"/gd-plan1?bead=1", "/plans/gd-plan1", "/plans/gd-plan1/card"}
	for _, target := range targets {
		if body := getWith(t, mux, target); strings.Contains(body, testBadgeSVG) {
			t.Fatalf("%s: badge drawn before the upload", target)
		}
	}
	giveBadge(f.beads["gd-plan1"]) // the upload's field write
	for _, target := range targets {
		if body := getWith(t, mux, target); !strings.Contains(body, `src="`+testBadgeSVG+`"`) {
			t.Errorf("%s: badge not drawn after the upload on the same viewer", target)
		}
	}
}
