package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Short titles in the viewer (gd-V5nGseRS5f, task gd-1DvGBBCE31): list rows,
// cards and plan steps show a bead's short_title with the full title on hover,
// and fall back to the full title when the bead carries none.

func TestDisplayTitle_ShortTitle(t *testing.T) {
	cases := []struct {
		name, short, full, want string
	}{
		{"no short title", "", "Wire the route", "Wire the route"},
		{"blank short title", "  ", "Wire the route", "Wire the route"},
		{"short title", "Route", "Wire the route", `<span class="short-title" title="Wire the route">Route</span>`},
		{"escapes both", `a<b`, `x "y" <z>`, `<span class="short-title" title="x &#34;y&#34; &lt;z&gt;">a&lt;b</span>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(displayTitle(tc.short, tc.full)); got != tc.want {
				t.Errorf("displayTitle(%q, %q) = %q, want %q", tc.short, tc.full, got, tc.want)
			}
		})
	}
}

func TestActiveRowPartial_ShortTitle(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-short": {
				ID: "gd-short", Title: "A long full title for the short row", ShortTitle: "Short row",
				Type: "task", Status: "open", Priority: 2, Labels: []string{"project:gardenio"},
			},
			"gd-plain": {
				ID: "gd-plain", Title: "Plain full title", Type: "task", Status: "open",
				Priority: 2, Labels: []string{"project:gardenio"},
			},
		},
	}
	mux := mkServer(t, f)
	render := func(id string) string {
		t.Helper()
		req := httptest.NewRequest("POST", "/b/"+id+"/claim?return=row", nil)
		req.Header.Set("X-Auth-Request-Email", "matt@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d body=%s", id, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	body := render("gd-short")
	want := `<td><span class="short-title" title="A long full title for the short row">Short row</span></td>`
	if !strings.Contains(body, want) {
		t.Errorf("short-title row should show the short title with the full title on hover; want %s in:\n%s", want, body)
	}

	body = render("gd-plain")
	if !strings.Contains(body, `<td>Plain full title</td>`) {
		t.Errorf("row without a short title should show the full title, body:\n%s", body)
	}
	if strings.Contains(body, "short-title") {
		t.Errorf("row without a short title must not render the short-title span, body:\n%s", body)
	}
}

func TestPlanSteps_ShortTitle(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].ShortTitle = "Route"
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Second step, no short title", Kind: "issue", Type: "task",
		Status: "open", CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if want := `href="/gd-step1"><span class="short-title" title="Wire the route">Route</span></a>`; !strings.Contains(body, want) {
		t.Errorf("plan step with a short title should show it, full title on hover; want %s", want)
	}
	if want := `href="/gd-step2">Second step, no short title</a>`; !strings.Contains(body, want) {
		t.Errorf("plan step without a short title should show the full title; want %s", want)
	}
}

func TestPlanCard_ShortTitle(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].ShortTitle = "Plans page"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The card head already carries the full title in its own title=, so the
	// face swaps in the short title without a nested span.
	if want := `title="Plan: ship the /plans page">Plans page</a>`; !strings.Contains(body, want) {
		t.Errorf("plan card should show the short title with the full title on hover; want %s", want)
	}
}

func TestBeadShow_ShortTitleLine(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].ShortTitle = "Route"
	mux := mkServer(t, f)

	get := func(id string) string {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/"+id, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", id, w.Code)
		}
		return w.Body.String()
	}

	body := get("gd-step1")
	if !strings.Contains(body, "Wire the route</h1>") {
		t.Errorf("bead page header must keep the full title")
	}
	if !strings.Contains(body, `<p class="bead-short-title">Short title: Route</p>`) {
		t.Errorf("bead page should show a Short title line under the title")
	}
	if strings.Contains(get("gd-dec1"), `<p class="bead-short-title">`) {
		t.Errorf("bead without a short title must not render the Short title line")
	}
}
