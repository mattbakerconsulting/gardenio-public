package main

// plans_group_project_chip_test.go — iter-62 (ka-0000005352): suppress the
// redundant per-card `project` chip on a ?group=project /plans board — the group
// twin of iter-60's ?project= FILTER suppression.
//
// THE MEASURE (measure-first, live daemon 2026-08-12): the board carries 58 real
// (non-fixture) open plan epics, all project:gardenio, so the default board's
// project chips already carry ZERO discriminating power. iter-60 stripped the
// chip on a ?project=X FILTERED board; but a ?group=project board — which
// sections the cards into one <h2> heading per project (groupPlanRows) — still
// rendered the same chip on every card, even though the section heading already
// names the project. So the chip is the SAME redundant ink AND dead-end
// self-link (its href=/plans?project=X points back at a filter of the project
// the section already names), just reached via grouping instead of filtering.
//
// THE FIX: extend the "render only where the value discriminates" guard to also
// drop the chip when $root.GroupKey == "project". The group key is URL-derivable
// in EVERY render path so a swapped-in card stays byte-consistent with its
// neighbours: the page render reads it off the page dot ($root.GroupKey), and the
// single-row + bulk-OOB re-renders read it from the Referer (refererPlansGroup).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- unit: refererPlansGroup over synthetic Referers ---------------------------

func TestRefererPlansGroup(t *testing.T) {
	s := &Server{basePath: ""}
	cases := []struct {
		name    string
		referer string
		want    string
	}{
		{"grouped-by-project plans page", "http://viewer/plans?group=project", "project"},
		{"grouped plus other params", "http://viewer/plans?operated_by=&group=status&project=", "status"},
		{"group by user", "http://viewer/plans?group=user", "user"},
		{"ungrouped plans page", "http://viewer/plans", ""},
		{"plans page empty group", "http://viewer/plans?group=", ""},
		{"unrecognized group normalizes to none", "http://viewer/plans?group=bogus", ""},
		{"non-plans referer is ignored", "http://viewer/deck?group=project", ""},
		{"cockpit referer is ignored", "http://viewer/plans/gd-plan1?group=project", ""},
		{"no referer", "", ""},
		{"garbage referer", "://not a url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/gardener/pause-toggle", nil)
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if got := s.refererPlansGroup(r); got != tc.want {
				t.Errorf("refererPlansGroup(%q) = %q, want %q", tc.referer, got, tc.want)
			}
		})
	}
}

// TestRefererPlansGroup_HonoursBasePath proves the path match is anchored on the
// server's basePath, so a viewer mounted under /beads reads the group from
// /beads/plans and ignores a bare /plans referer (a different mount) — the same
// mount-safety refererPlansProject carries.
func TestRefererPlansGroup_HonoursBasePath(t *testing.T) {
	s := &Server{basePath: "/beads"}
	r := httptest.NewRequest(http.MethodPost, "/beads/plans/gd-plan1/gardener/pause-toggle", nil)
	r.Header.Set("Referer", "http://viewer/beads/plans?group=project")
	if got := s.refererPlansGroup(r); got != "project" {
		t.Errorf("basePath /beads: got %q, want project", got)
	}
	r.Header.Set("Referer", "http://viewer/plans?group=project") // wrong mount
	if got := s.refererPlansGroup(r); got != "" {
		t.Errorf("bare /plans under a /beads mount should not match; got %q", got)
	}
}

// --- page render: chip suppressed grouped-by-project, kept otherwise -----------

// TestPlansIndex_ProjectChipSuppressedWhenGroupedByProject is the headline: on a
// ?group=project board every section's <h2> already names the project, so the
// redundant per-card project chip is gone.
func TestPlansIndex_ProjectChipSuppressedWhenGroupedByProject(t *testing.T) {
	card := planCardFor(t, seedPlansFake(), "/plans?group=project")
	if projectChipRe.MatchString(card) {
		t.Fatalf("a ?group=project board should suppress the redundant project chip:\n%s", card)
	}
	// The card itself must still render — suppression removes ONLY the chip.
	if !strings.Contains(card, `data-plan-id="gd-plan1"`) {
		t.Fatalf("the plan card must still render, only the chip drops:\n%s", card)
	}
	// The grouping heading must still name the project (that is what makes the
	// per-card chip redundant in the first place).
	mux := mkServerWithNow(t, seedPlansFake(), func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?group=project", "matt@acme.example"))
	if !strings.Contains(w.Body.String(), "plan-group-head") {
		t.Fatalf("a ?group=project board should render group headings:\n%s", snippetN(w.Body.String(), 800))
	}
}

// TestPlansIndex_ProjectChipPresentWhenGroupedByStatus is the discriminating
// floor: a ?group=status board sections by status, NOT project — so within a
// status section the project label still discriminates a possibly-mixed set and
// the chip must render (proves the suppression keys on group=="project", not on
// "any group present").
func TestPlansIndex_ProjectChipPresentWhenGroupedByStatus(t *testing.T) {
	card := planCardFor(t, seedPlansFake(), "/plans?group=status")
	if !projectChipRe.MatchString(card) {
		t.Fatalf("a ?group=status board keeps the discriminating project chip:\n%s", card)
	}
}

// TestPlansIndex_ProjectChipPresentWhenGroupedByUser is the second discriminating
// floor: a ?group=user board sections by operator, so the project label still
// discriminates within a user section and the chip renders.
func TestPlansIndex_ProjectChipPresentWhenGroupedByUser(t *testing.T) {
	card := planCardFor(t, seedPlansFake(), "/plans?group=user")
	if !projectChipRe.MatchString(card) {
		t.Fatalf("a ?group=user board keeps the discriminating project chip:\n%s", card)
	}
}

// --- re-render consistency: a toggled row matches its unswapped neighbours -----

// TestPlanRowToggle_SuppressesProjectChipFromGroupReferer proves the single-row
// re-render (a gardener pause-toggle) reads the active ?group= key off the Referer
// and suppresses the project chip on a ?group=project board too — so a swapped-in
// row stays byte-consistent with the neighbours the page render already stripped,
// instead of re-growing a chip they dropped.
func TestPlanRowToggle_SuppressesProjectChipFromGroupReferer(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	req := operatorPost("/plans/gd-plan1/gardener/pause-toggle", "")
	req.Header.Set("Referer", "http://viewer/plans?group=project")
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
		t.Fatalf("a row re-rendered onto a ?group=project board should drop the project chip; body=%s", snippetN(body, 800))
	}
}

// TestPlanRowToggle_KeepsProjectChipUngroupedReferer is the symmetric floor: a
// toggle issued from an UNGROUPED /plans page (no ?group=) re-renders the row
// WITH the chip, matching the ungrouped neighbours — the Referer-derived group is
// empty, so the row reads exactly as a fresh ungrouped page render.
func TestPlanRowToggle_KeepsProjectChipUngroupedReferer(t *testing.T) {
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
		t.Fatalf("a row re-rendered onto an ungrouped board should keep the project chip; body=%s", snippetN(w.Body.String(), 800))
	}
}
