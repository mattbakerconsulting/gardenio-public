package main

// plans_project_chip_test.go — iter-60 (ka-0000005352): suppress the redundant
// per-card `project` chip on a ?project= FILTERED /plans board.
//
// THE MEASURE (measure-first, live board 2026-08-12): all 8 active plans are
// project:gardenio, so the default board renders 8 identical "gardenio" chips — a
// facet with ZERO discriminating power. On a board narrowed to one project via
// ?project=X the chip is strictly worse: every rendered card shares project X
// (handlePlans appends the project:X label to the list query), the page H1 reads
// "Plans · X", the filter dropdown shows X selected — AND the chip's own
// click-through href=/plans?project=X points right back at the same filtered
// page, a dead-end link. So on a filtered board the chip is pure redundant ink.
//
// THE FIX: the priority-pill's "render only where the value discriminates"
// discipline, applied to the project chip — suppress it when .Project equals the
// active ?project= filter ($root.ProjectFilter). "More info in LESS space": on a
// filtered board every card reclaims the chip's width from the meta row (which
// the latest audit flagged as near its wrapping ceiling), with zero info loss.
// The filter is URL-derivable in EVERY render path so a swapped-in row stays
// byte-consistent with its neighbours: the page render reads it off the page dot,
// and the single-row + bulk-OOB re-renders read it from the Referer
// (refererPlansProject).

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// projectChipRe matches the per-card project chip an unfiltered card renders.
var projectChipRe = regexp.MustCompile(`<a class="chip chip-type" href="[^"]*/plans\?project=[^"]*">`)

// planCardFor renders the served /plans index for the given target URL (so a
// ?project= filter can be exercised end to end) and returns the single
// <li class="plan-card"> block for the seed plan.
func planCardFor(t *testing.T, f *fakeDaemon, target string) string {
	t.Helper()
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq(target, "matt@acme.example"))
	if w.Code != 200 {
		t.Fatalf("GET %s: expected 200, got %d", target, w.Code)
	}
	re := regexp.MustCompile(`(?s)<li class="plan-card".*?</li>`)
	m := re.FindString(w.Body.String())
	if m == "" {
		t.Fatalf("no plan-card in body:\n%s", w.Body.String())
	}
	return m
}

// --- unit: refererPlansProject over synthetic Referers -------------------------

func TestRefererPlansProject(t *testing.T) {
	s := &Server{basePath: ""}
	cases := []struct {
		name    string
		referer string
		want    string
	}{
		{"filtered plans page", "http://viewer/plans?project=gardenio", "gardenio"},
		{"filtered plus other params", "http://viewer/plans?operated_by=&project=monorepo&status=open", "monorepo"},
		{"unfiltered plans page", "http://viewer/plans", ""},
		{"plans page empty project", "http://viewer/plans?project=", ""},
		{"non-plans referer is ignored", "http://viewer/deck?project=gardenio", ""},
		{"cockpit referer is ignored", "http://viewer/plans/gd-plan1?project=gardenio", ""},
		{"no referer", "", ""},
		{"garbage referer", "://not a url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/gardener/pause-toggle", nil)
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if got := s.refererPlansProject(r); got != tc.want {
				t.Errorf("refererPlansProject(%q) = %q, want %q", tc.referer, got, tc.want)
			}
		})
	}
}

// TestRefererPlansProject_HonoursBasePath proves the path match is anchored on
// the server's basePath, so a viewer mounted under /beads reads the filter from
// /beads/plans and ignores a bare /plans referer (a different mount).
func TestRefererPlansProject_HonoursBasePath(t *testing.T) {
	s := &Server{basePath: "/beads"}
	r := httptest.NewRequest(http.MethodPost, "/beads/plans/gd-plan1/gardener/pause-toggle", nil)
	r.Header.Set("Referer", "http://viewer/beads/plans?project=gardenio")
	if got := s.refererPlansProject(r); got != "gardenio" {
		t.Errorf("basePath /beads: got %q, want gardenio", got)
	}
	r.Header.Set("Referer", "http://viewer/plans?project=gardenio") // wrong mount
	if got := s.refererPlansProject(r); got != "" {
		t.Errorf("bare /plans under a /beads mount should not match; got %q", got)
	}
}

// --- page render: chip present unfiltered, suppressed when filtered ------------

// TestPlansIndex_ProjectChipPresentUnfiltered pins that an UNFILTERED board still
// renders the per-card project chip — there the project label genuinely
// discriminates a possibly-mixed board, so the facet stays (no regression).
func TestPlansIndex_ProjectChipPresentUnfiltered(t *testing.T) {
	card := planCardFor(t, seedPlansFake(), "/plans")
	if !projectChipRe.MatchString(card) {
		t.Fatalf("unfiltered card should render the project chip:\n%s", card)
	}
}

// TestPlansIndex_ProjectChipSuppressedWhenFiltered is the headline: on a board
// scoped to the card's own project (?project=gardenio), the redundant project
// chip is gone — the chip's own project matches the active filter, the H1
// already names it, and the chip link would only point back at the same page.
func TestPlansIndex_ProjectChipSuppressedWhenFiltered(t *testing.T) {
	card := planCardFor(t, seedPlansFake(), "/plans?project=gardenio")
	if projectChipRe.MatchString(card) {
		t.Fatalf("a ?project=gardenio board should suppress the redundant project chip:\n%s", card)
	}
	// The plan itself must still be on the page — suppression removes ONLY the
	// chip, never the card.
	if !strings.Contains(card, `data-plan-id="gd-plan1"`) {
		t.Fatalf("the plan card must still render, only the chip drops:\n%s", card)
	}
}

// TestPlansIndex_ProjectChipPresentUnderDifferentFilter is the defensive guard:
// if the active filter names a DIFFERENT project than the card's own (a shape a
// scoped list query never actually produces, since it appends project:X), the
// card's still-discriminating chip renders. Proves the suppression keys on
// equality, not on "any filter present".
func TestPlansIndex_ProjectChipPresentUnderDifferentFilter(t *testing.T) {
	// The list query is label-scoped to project:gardenio, so the seed plan (also
	// project:gardenio) is returned; but we request ?project=monorepo to drive the
	// ProjectFilter="monorepo" != card.Project="gardenio" branch. The chip must
	// render because gardenio != the monorepo the board claims to be filtered to.
	card := planCardFor(t, seedPlansFake(), "/plans?project=monorepo")
	if !projectChipRe.MatchString(card) {
		t.Fatalf("a card whose project differs from the active filter keeps its discriminating chip:\n%s", card)
	}
}

// --- re-render consistency: a toggled row matches its unswapped neighbours -----

// TestPlanRowToggle_SuppressesProjectChipFromReferer proves the single-row
// re-render (a gardener pause-toggle) reads the active ?project= filter off the
// Referer and suppresses the project chip too — so a swapped-in row on a filtered
// board stays byte-consistent with the neighbours the page render already
// stripped, instead of re-growing a chip they dropped.
func TestPlanRowToggle_SuppressesProjectChipFromReferer(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := operatorPost("/plans/gd-plan1/gardener/pause-toggle", "")
	req.Header.Set("Referer", "http://viewer/plans?project=gardenio")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-plan-id="gd-plan1"`) {
		t.Fatalf("response should be the re-rendered plan row; body=%s", snippetN(body, 800))
	}
	if projectChipRe.MatchString(body) {
		t.Fatalf("a row re-rendered onto a ?project=gardenio board should drop the project chip; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowToggle_KeepsProjectChipUnfilteredReferer is the symmetric floor: a
// toggle issued from an UNFILTERED /plans page (no ?project=) re-renders the row
// WITH the chip, matching the unfiltered neighbours — the Referer-derived filter
// is empty, so the row reads exactly as a fresh unfiltered page render.
func TestPlanRowToggle_KeepsProjectChipUnfilteredReferer(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := operatorPost("/plans/gd-plan1/gardener/pause-toggle", "")
	req.Header.Set("Referer", "http://viewer/plans")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if !projectChipRe.MatchString(w.Body.String()) {
		t.Fatalf("a row re-rendered onto an unfiltered board should keep the project chip; body=%s", snippetN(w.Body.String(), 800))
	}
}
