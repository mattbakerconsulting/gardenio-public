package main

// Render-level golden tests for the legible gardener rendering (task
// ka-0000005782 / I2.4). They drive the REAL /gardener/{id} detail page and the
// /gardeners list page against a gardener fixture carrying a firing config + stop
// spec, and assert the decoded wakes-when / chasing / stops-when prose appears
// on the surface — and that the raw stop-spec JSON is no longer the primary
// render (it stays behind the collapsed audit <details>). This locks the
// "replace JSON dumps" contract so a regression fails CI.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// legibleGardenerFixture builds a gardener trigger bead with a real cron cadence,
// a terminal goal, and a loop_end bead-query stop spec — the shape whose config
// used to render as opaque JSON.
func legibleGardenerFixture(t *testing.T) *beadsapi.BeadDetail {
	t.Helper()
	b := goldenGardenerBead(t, "gd-cap", "Delivery gardener", map[string]any{
		"name":          "delivery",
		"terminal_goal": "drive the release epic to done",
		"stop": map[string]any{
			"loop_end": map[string]any{
				"any": []map[string]any{
					{"kind": "bead_query", "op": "==", "value": "0", "parent": "gd-rel", "statuses": []string{"open"}},
				},
			},
			"hard_stop": map[string]any{
				"any": []map[string]any{
					{"kind": "consecutive_failures", "value": "3"},
				},
			},
			"on_loop_end": "notify_slack",
		},
	})
	b.Fields["cron"] = "0 * * * *" // hourly
	return b
}

// TestGardenerDetailGolden_Legible pins the decoded config on the detail page.
func TestGardenerDetailGolden_Legible(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = legibleGardenerFixture(t)
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}

	body := renderGardenerGolden(t, f, "gd-cap")

	wantPresent := []string{
		`class="gardener-legible"`,   // the decoded block container
		"Wakes",                      // wakes-when label
		"on a schedule",              // decoded cron cadence (not raw "0 * * * *" in prose)
		"Chasing",                    // chasing label
		"Stops",                      // stops-when label
		"ends the loop",              // loop_end slot label
		"no open beads under gd-rel", // decoded bead-query stop
		"kills the run",              // hard_stop slot label
		"3 consecutive failures",     // decoded hard_stop
		"notify Slack on loop end",   // decoded on_loop_end
	}
	for _, want := range wantPresent {
		if !strings.Contains(body, want) {
			t.Errorf("gardener detail page missing decoded config %q", want)
		}
	}

	// The raw stop JSON must not be the PRIMARY render. It stays behind the
	// collapsed audit <details> (gardener-spec-details), so a bare top-level
	// "kind": key must appear only inside that details block, never as the lead
	// config. Assert the decoded prose leads by checking the legible block sits
	// before the raw <pre> dump.
	legibleAt := strings.Index(body, `class="gardener-legible"`)
	rawAt := strings.Index(body, `<pre class="gardener-spec-json"`) // the rendered dump, not the CSS rule
	if legibleAt < 0 {
		t.Fatal("legible block absent")
	}
	if rawAt >= 0 && rawAt < legibleAt {
		t.Errorf("raw stop JSON (%d) renders before the decoded legible block (%d) — JSON should be demoted", rawAt, legibleAt)
	}
	// The raw dump must still be reachable as the audit escape-hatch.
	if rawAt < 0 {
		t.Error("raw stop-spec audit <details> should still be present as the escape-hatch")
	}
}

// TestGardenersList_Legible pins the decoded wakes/stops/chasing one-liners on
// the /gardeners index row.
func TestGardenersList_Legible(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = legibleGardenerFixture(t)
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}
	// /gardeners lists role:gardener trigger beads via the trigger+schedule list.
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-cap"]},
		Total: 1,
	}

	mux := mkServerWithNow(t, f, gardenerPageNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /gardeners: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	wantPresent := []string{
		`class="cap-behavior"`,       // the behavior strip container
		"cap-behavior-wakes",         // wakes one-liner
		"on a schedule",              // decoded cadence
		"cap-behavior-stops",         // stops one-liner
		"no open beads under gd-rel", // decoded loop_end
		"cap-behavior-chasing",       // chasing one-liner
	}
	for _, want := range wantPresent {
		if !strings.Contains(body, want) {
			t.Errorf("gardeners list row missing decoded config %q", want)
		}
	}
}

// TestGardenersList_PromptOnlyFallback pins the P2 index-row half of bug
// ka-0000003596: a gardener authored the sanctioned way — a per-run `prompt`,
// NO terminal goal — must not read as a silent/idle row. The index row surfaces
// a clamped prompt one-liner (full text in the title tooltip) in place of the
// Goal line, and the "chasing" one-liner leads with the prompt rather than a
// bare "idle".
func TestGardenersList_PromptOnlyFallback(t *testing.T) {
	f := newFake()
	// A gardener with a prompt and NO terminal goal.
	b := goldenGardenerBead(t, "gd-cap", "Prompt-only gardener", map[string]any{"name": "promptonly"})
	b.Fields["prompt"] = "Shepherd every open MR under gd-EPIC to green and merge it."
	b.Fields["cron"] = "0 * * * *"
	f.beads["gd-cap"] = b
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{b},
		Total: 1,
	}

	mux := mkServerWithNow(t, f, gardenerPageNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /gardeners: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, want := range []string{
		"cap-goal-prompt", // the prompt-fallback goal line
		">Prompt<",        // its label
		"Shepherd every open MR under gd-EPIC to green and merge", // the prompt text (row + tooltip)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt-only gardeners list row missing %q", want)
		}
	}
	// The chasing one-liner must lead with the prompt, never a bare "idle · " with
	// nothing after it.
	if strings.Contains(body, "🎯</span>idle</span>") {
		t.Errorf("prompt-only row read as a bare idle chasing line; expected a prompt-derived label")
	}
}
