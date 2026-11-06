package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Markup follow-ups from the Homestead 2.4 audit (gd-ddw0WBzm9S). axe flagged
// these in every skin: no main landmark, an img-role constellation holding
// links, <li>s whose parent was a log rather than a list, a bead page with no
// heading one, and heading and landmark slips on /plans and the cockpit.

// The page's content is the main landmark. The shelf nav stays the only nav
// straight under body, labelled so a page's own nav is not its twin, and the
// version stamp is the footer.
func TestLayout_MainLandmark(t *testing.T) {
	body := renderThemedPage(t, "", "/agents")
	nav := strings.Index(body, `<nav aria-label="Site">`)
	open := strings.Index(body, "\n<main>\n")
	end := strings.Index(body, "\n</main>\n<footer class=\"app-version\"")
	if nav < 0 || open < nav || end < open {
		t.Fatalf("want <nav aria-label=\"Site\">, then <main> around the content, then the version footer (nav=%d main=%d /main=%d)", nav, open, end)
	}
	if strings.Count(body, "<main") != 1 {
		t.Errorf("want exactly one <main>, got %d", strings.Count(body, "<main"))
	}
}

// The constellations' nodes are links, so the svg is a named group, not an
// img (whose children are presentational).
func TestConstellation_SvgIsAGroup(t *testing.T) {
	body := getWith(t, mkServer(t, newFake()), "/gd-aaa")
	svg := regexp.MustCompile(`<svg class="constellation-svg"[^>]*>`).FindString(body)
	if svg == "" {
		t.Fatal("bead show rendered no constellation svg")
	}
	if !strings.Contains(svg, `role="group"`) || strings.Contains(svg, `role="img"`) {
		t.Errorf("constellation svg must be role=group, got %s", svg)
	}
}

// The editable title is still the page's heading one: no role on the H1.
func TestBeadShow_EditableTitleStaysAHeading(t *testing.T) {
	body, code := getBodyWithRoles(t, mkServer(t, newFake()), "/gd-aaa", "matt@example.com", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /gd-aaa = %d", code)
	}
	h1 := regexp.MustCompile(`(?s)<h1 class="bead-title bead-title-editable"[^>]*>`).FindString(body)
	if h1 == "" {
		t.Fatal("no editable title H1 for an operator")
	}
	if strings.Contains(h1, "role=") || !strings.Contains(h1, `tabindex="0"`) {
		t.Errorf("the editable H1 must stay a focusable heading with no role, got %s", h1)
	}
}

// The conversation's live region wraps the <ul>, so its items stay list items.
func TestThreadTimeline_LogWrapsTheList(t *testing.T) {
	f := newFake()
	f.beads["gd-wish"] = &beadsapi.BeadDetail{
		ID: "gd-wish", Title: "wish", Status: "open",
		Labels:    []string{threadHeadLabel},
		CreatedAt: time.Now().Add(-time.Hour),
	}
	f.comments["gd-wish"] = []beadsapi.Comment{{ID: 1, Author: "matt", Text: "hello", CreatedAt: time.Now()}}
	body := getWith(t, mkServerFromFake(t, f), "/threads/gd-wish")
	if !strings.Contains(body, `role="log" aria-live="polite" aria-relevant="additions text" aria-label="Conversation messages">`+"\n    "+`<ul class="thread2-convo" id="thread2-convo">`) {
		t.Error("the conversation <ul> must sit inside the role=log wrapper and keep its list role")
	}
}

// /plans lanes are list items without a sectioning element under them, and
// the cockpit's section heads read as level 2 under the plan's h1, while the
// peek drawer's copies stay h3 under its h2.
func TestPlans_ListItemsAndHeadingLevels(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	if body := getWith(t, mux, "/plans"); regexp.MustCompile(`<section class="plans-lane[^>]*role="listitem"`).MatchString(body) {
		t.Error("a /plans lane is a <section role=listitem>; it must be a <div>")
	}
	cockpit := getWith(t, mux, "/plans/gd-plan1")
	for _, want := range []string{`<h3 aria-level="2">Build steps`, `<h3 aria-level="2">Crew`, `<nav class="plan-breadcrumb" aria-label="Breadcrumb">`} {
		if !strings.Contains(cockpit, want) {
			t.Errorf("cockpit missing %q", want)
		}
	}
	peek := getWith(t, mux, "/plans/gd-plan1/peek")
	if !strings.Contains(peek, "<h3>Build steps") || strings.Contains(peek, `aria-level="2"`) {
		t.Error("the peek drawer's step and crew heads must stay plain h3 under its h2")
	}
}
