package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleTriggers_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No triggers") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	q := f.listAllQ[0]
	if len(q.Types) != 1 || q.Types[0] != "schedule" {
		t.Errorf("query types = %v, want [schedule]", q.Types)
	}
}

func TestHandleTriggers_NonEmpty_RendersRows(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-sch1",
				Title:     "Daily report",
				Type:      "schedule",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-3 * time.Minute),
				Fields: map[string]string{
					"cron":         "0 9 * * *",
					"enabled":      "true",
					"timezone":     "UTC",
					"project":      "gardenio",
					"role":         "researcher",
					"runs_spawned": "12",
				},
			},
			{
				ID:        "gd-sch2",
				Title:     "Weekly cleanup",
				Type:      "schedule",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-1 * time.Hour),
				Fields: map[string]string{
					"cron":     "0 0 * * 0",
					"enabled":  "false",
					"timezone": "UTC",
					"project":  "gardenio",
				},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Daily report",
		"Weekly cleanup",
		"0 9 * * *",
		"0 0 * * 0",
		"enabled",
		"disabled",
		`href="/gd-sch1"`,
		`href="/gd-sch2"`,
		// Filter chips with counts
		"All ",
		"Enabled ",
		"Disabled ",
		// ka-0000005685 (audit F4): the page-level filter menus are <nav>
		// wrappers, standardized to match project.html / project_hub.html.
		`<nav class="chip-strip filter-strip" aria-label="Trigger filter">`,
		`<nav class="chip-strip filter-strip" aria-label="Trigger source filter">`,
		`<nav class="chip-strip filter-strip" aria-label="Trigger action filter">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("triggers page missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "No triggers.") {
		t.Errorf("unexpected empty-state when rows present")
	}
}

func TestHandleTriggers_FilterEnabled(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-on",
				Title:     "Active job",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
			{
				ID:        "gd-off",
				Title:     "Inactive job",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "false"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers?filter=enabled", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Active job") {
		t.Errorf("expected enabled row visible, body=%s", body)
	}
	// Inactive job should be filtered out of the list, but counts (in chip
	// strip) are computed before filtering, so its title may still appear in
	// the chip count badges. Check the row HTML class as the signal.
	if strings.Contains(body, ">Inactive job<") {
		t.Errorf("disabled row leaked through ?filter=enabled, body=%s", body)
	}
	if !strings.Contains(body, "chip-active") {
		t.Errorf("expected an active chip, body=%s", body)
	}
}

func TestHandleTriggers_FilterDisabled(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-on",
				Title:     "Active job",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
			{
				ID:        "gd-off",
				Title:     "Inactive job",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "false"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers?filter=disabled", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Inactive job") {
		t.Errorf("expected disabled row visible, body=%s", body)
	}
	if strings.Contains(body, ">Active job<") {
		t.Errorf("enabled row leaked through ?filter=disabled, body=%s", body)
	}
}

func TestHandleTriggers_SortCron(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-z",
				Title:     "Zeta",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "9 9 * * *", "enabled": "true"},
			},
			{
				ID:        "gd-a",
				Title:     "Alpha",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 0 * * *", "enabled": "true"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers?sort=cron", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// Alpha (0 0 * * *) should appear before Zeta (9 9 * * *) when sorted by cron string
	iAlpha := strings.Index(body, "Alpha")
	iZeta := strings.Index(body, "Zeta")
	if iAlpha < 0 || iZeta < 0 {
		t.Fatalf("expected both rows present, body=%s", body)
	}
	if iAlpha >= iZeta {
		t.Errorf("?sort=cron: expected Alpha (cron 0 0 * * *) before Zeta (9 9 * * *)")
	}
	// Sort link state should reflect ?sort=cron.
	if !strings.Contains(body, `href="/triggers`) {
		t.Errorf("expected sort links to /triggers, body=%s", body)
	}
}

func TestHandleTriggers_DaemonError_RendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load triggers") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

// TestHandleTriggers_SortNextFire is the regression test for the operator-
// reported bug ("sort by next fire isn't working"). Two flavors of the bug
// converged: (1) the cron parser rejected "@hourly" descriptors so any such
// schedule sank to the bottom of the list with a NextFireError, and (2)
// timezones in the rendered absolute time differed across rows, making a
// correctly-sorted list look broken.
//
// The default ?sort= (no query string) must order rows by the absolute
// next-fire instant ascending — independent of the timezone the row was
// computed in, and independent of whether the cron field uses 5-field or
// descriptor form.
func TestHandleTriggers_SortNextFire(t *testing.T) {
	// Pin "now" to a wall-clock instant so the cron schedules below have
	// deterministic next-fire ordering. Choose 12:00 UTC so all "today" times
	// are still ahead. The handler reads its clock from s.now (injected via
	// SetNow) — without this override the asserted Hourly < Late UTC <
	// Evening PDT order only holds while real now is between ~04:00 UTC and
	// ~23:00 UTC (ka-0000003725).
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// fires next at the top of the next hour (e.g. 13:00 UTC) — the
			// EARLIEST upcoming. Uses descriptor form, which the original
			// parser rejected.
			{
				ID:        "gd-hourly",
				Title:     "Hourly job",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "@hourly", "enabled": "true", "timezone": "UTC"},
			},
			// fires next at 21:00 PDT (= 04:00 UTC the next day). LATER than
			// the hourly job. Different timezone — caught the second bug
			// flavor where mixed-tz absolute strings hid the true order.
			{
				ID:        "gd-evening-pdt",
				Title:     "Evening job PDT",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 21 * * *", "enabled": "true", "timezone": "America/Los_Angeles"},
			},
			// fires next at 23:00 UTC TODAY — between the two above.
			{
				ID:        "gd-late-utc",
				Title:     "Late job UTC",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 23 * * *", "enabled": "true", "timezone": "UTC"},
			},
		},
		Total: 3,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	// Default sort is now source+title (b-density spec). Explicitly request
	// next-fire to exercise the ordering this test guards.
	req := httptest.NewRequest("GET", "/triggers?sort=next-fire", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Hourly should appear FIRST despite using a @hourly descriptor —
	// regression on the parser flag fix. Then Late UTC, then Evening PDT.
	iHourly := strings.Index(body, "Hourly job")
	iLate := strings.Index(body, "Late job UTC")
	iEvening := strings.Index(body, "Evening job PDT")
	if iHourly < 0 || iLate < 0 || iEvening < 0 {
		t.Fatalf("expected all three rows present\nbody=%s", body)
	}
	if !(iHourly < iLate && iLate < iEvening) {
		t.Errorf("?sort=next-fire: expected order Hourly < Late UTC < Evening PDT, got hourly=%d late=%d evening=%d\nbody=%s",
			iHourly, iLate, iEvening, body)
	}

	// No row should carry a NextFireError — descriptor form must parse.
	if strings.Contains(body, "next: failed") || strings.Contains(body, "expected exactly 5 fields") {
		t.Errorf("descriptor cron failed to parse — saw next-fire error in body=%s", body)
	}
}

// TestHandleTriggers_SortNextFire_InstantTie pins "now" inside the
// 03:00–04:00 UTC window where an @hourly UTC schedule and a `0 21 PDT`
// daily schedule both next-fire at the SAME instant (04:00 UTC). Before the
// nextFireAt2 tiebreaker the equal next-fire instants fell through to ID
// order, so the hourly row's position was wall-clock-dependent and sank
// below the coincident daily row — the root cause of schedules.spec.js:88
// (ka-0000007664). The tiebreaker ranks the schedule whose *subsequent* fire
// is sooner (the more frequent one) first, so hourly must lead.
func TestHandleTriggers_SortNextFire_InstantTie(t *testing.T) {
	// 03:30 UTC: @hourly fires next at 04:00 UTC (subsequent 05:00 UTC);
	// 0 21 America/Los_Angeles (PDT, UTC-7) fires next at 04:00 UTC too
	// (subsequent next-day 04:00 UTC). Identical next-fire instant → the
	// tie must be broken by the sooner subsequent fire (hourly).
	now := time.Date(2026, 5, 8, 3, 30, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// ID sorts AFTER the daily row, so a fall-through to ID order
			// would (wrongly) place the daily row first — this ordering is
			// what the pre-fix bug produced.
			{
				ID:        "gd-zzz-hourly",
				Title:     "Hourly tie job",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "@hourly", "enabled": "true", "timezone": "UTC"},
			},
			{
				ID:        "gd-aaa-evening-pdt",
				Title:     "Evening tie job PDT",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 21 * * *", "enabled": "true", "timezone": "America/Los_Angeles"},
			},
		},
		Total: 2,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers?sort=next-fire", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	iHourly := strings.Index(body, "Hourly tie job")
	iEvening := strings.Index(body, "Evening tie job PDT")
	if iHourly < 0 || iEvening < 0 {
		t.Fatalf("expected both rows present\nbody=%s", body)
	}
	if iHourly >= iEvening {
		t.Errorf("instant-tie next-fire: expected Hourly before Evening PDT "+
			"(more-frequent tiebreaker), got hourly=%d evening=%d\nbody=%s",
			iHourly, iEvening, body)
	}
}

// TestComputeNextFire_Descriptor exercises the parser configuration directly:
// the kbeads scheduler accepts cron descriptors (@hourly, @daily, …), so the
// viewer must accept them too. Without the cron.Descriptor flag this returns
// an error and the row sinks in the next-fire sort.
func TestComputeNextFire_Descriptor(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	cases := []struct {
		spec string
		tz   string
	}{
		{"@hourly", "UTC"},
		{"@daily", "UTC"},
		{"@weekly", "UTC"},
		{"@every 15m", ""},
		{"0 9 * * *", "America/Los_Angeles"},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			next, err := computeNextFire(c.spec, c.tz, now)
			if err != nil {
				t.Fatalf("computeNextFire(%q,%q) = err %v", c.spec, c.tz, err)
			}
			if !next.After(now) {
				t.Errorf("computeNextFire(%q,%q) = %v, want > %v", c.spec, c.tz, next, now)
			}
		})
	}
}

// TestFmtTimeUntil exercises the relative-time helper rendered alongside the
// absolute next-fire string. The relative chip is the operator-visible signal
// that the list is sorted correctly even when timezones differ row to row.
func TestFmtTimeUntil(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, ""},
		{"past", now.Add(-1 * time.Minute), "now"},
		{"30s", now.Add(30 * time.Second), "in 30s"},
		{"5m", now.Add(5 * time.Minute), "in 5m"},
		{"3h", now.Add(3 * time.Hour), "in 3h"},
		{"2d", now.Add(48 * time.Hour), "in 2d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fmtTimeUntil(c.t, now)
			if got != c.want {
				t.Errorf("fmtTimeUntil(%v) = %q, want %q", c.t, got, c.want)
			}
		})
	}
}

// TestHandleTriggers_BothPrimitives verifies /triggers surfaces both
// type=trigger label=trigger:schedule beads and legacy type=schedule beads,
// and labels each row with its kind so operators can tell them apart.
func TestHandleTriggers_BothPrimitives(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-legacy",
				Title:     "Legacy daily",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
		},
		Total: 1,
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-trig",
				Title:     "Cron trigger",
				Type:      "trigger",
				Status:    "open",
				Labels:    []string{"trigger:schedule", "project:gardenio"},
				UpdatedAt: now,
				// enabled missing == enabled-by-default for triggers
				Fields: map[string]string{"cron": "*/15 * * * *"},
			},
			{
				ID:        "gd-trig-off",
				Title:     "Cron trigger off",
				Type:      "trigger",
				Status:    "open",
				Labels:    []string{"trigger:schedule"},
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 * * * *", "enabled": "false"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"Legacy daily", "Cron trigger", "Cron trigger off",
		"legacy schedule", // chip on the legacy row
		"chip-kind-trigger",
		"chip-kind-schedule",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("triggers page missing %q\nbody=%s", want, body)
		}
	}

	// Trigger:schedule with no `enabled` field must render as enabled (default
	// for triggers), so gd-trig should NOT be marked disabled.
	rowTrig := extractRow(body, "gd-trig")
	if strings.Contains(rowTrig, "sched-disabled") {
		t.Errorf("trigger with implicit enabled rendered as disabled\nrow=%s", rowTrig)
	}
	if !strings.Contains(rowTrig, ">enabled<") {
		t.Errorf("expected enabled chip on default-enabled trigger\nrow=%s", rowTrig)
	}

	// Trigger:schedule with enabled=false should be disabled.
	rowOff := extractRow(body, "gd-trig-off")
	if !strings.Contains(rowOff, ">disabled<") {
		t.Errorf("expected disabled chip on enabled=false trigger\nrow=%s", rowOff)
	}

	// Two list calls: one for legacy schedules, one for type=trigger beads
	// across every source label.
	if len(f.listAllQ) < 2 {
		t.Fatalf("expected at least 2 ListBeadsFiltered calls, got %d", len(f.listAllQ))
	}
	sawTriggerQuery := false
	for _, q := range f.listAllQ {
		for _, ty := range q.Types {
			if ty == "trigger" {
				sawTriggerQuery = true
			}
		}
	}
	if !sawTriggerQuery {
		t.Errorf("expected a type=trigger list query, got %+v", f.listAllQ)
	}
}

// extractRow returns the substring of the triggers HTML for the row with the
// given data-bead-id, or "" if not found. Used to isolate per-row assertions
// from sibling rows on the same page.
func extractRow(body, beadID string) string {
	start := strings.Index(body, `data-bead-id="`+beadID+`"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</li>")
	if end < 0 {
		return body[start:]
	}
	return body[start : start+end]
}

func TestHandleTriggers_NavLink(t *testing.T) {
	// Triggers is a vibe-only nav link after gb-ux #3 (ka-0000003340); render as
	// a vibe-mode viewer so the gated link is present.
	f := newFake()
	f.userResp = vibeUserResp()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `href="/triggers"`) {
		t.Errorf("expected /triggers nav link in layout, body=%s", body)
	}
}

// TestHandleSchedules_RedirectsToTriggers verifies the legacy /schedules path
// 301-redirects to /triggers (bookmark/external-link compatibility).
func TestHandleSchedules_RedirectsToTriggers(t *testing.T) {
	mux := mkServer(t, newFake())

	cases := []struct {
		name string
		path string
		want string
	}{
		{"bare", "/schedules", "/triggers"},
		{"with_query", "/schedules?filter=enabled&sort=cron", "/triggers?filter=enabled&sort=cron"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", c.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusMovedPermanently {
				t.Errorf("expected 301, got %d", w.Code)
			}
			if got := w.Header().Get("Location"); got != c.want {
				t.Errorf("Location = %q, want %q", got, c.want)
			}
		})
	}
}

// TestHandleTriggers_PerSourceRowShape exercises the universal row template
// across every (source, exemplar action) pair the trigger engine supports.
// Each case seeds a single trigger bead and asserts that the row carries the
// per-source pill, the action pill, the match summary string, and the
// destination summary string. Together these snapshots are the contract that
// the row shape is legible for every source — not just cron schedules
// (gd-n1II3dPby7, design bundle ka-0000003886).
func TestHandleTriggers_PerSourceRowShape(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		bead         *beadsapi.BeadDetail
		wantSrcCls   string // CSS class on the source pill (full color)
		wantSrcText  string // human label inside the pill
		wantSrcGlyph string // 2-letter glyph for compact density
		wantActCls   string // CSS class on the action pill
		wantActFull  string // full action label
		wantMatch    string // match summary line copy
		wantDest     string // destination summary line copy
	}{
		{
			name: "jira_spawn_agent",
			bead: &beadsapi.BeadDetail{
				ID: "gd-jira1", Title: "Jira watch", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":  "spawn_agent",
					"jql":     "project = AION AND status = 'In Review'",
					"project": "aion",
					"role":    "reviewer",
				},
			},
			wantSrcCls:   "trig-src-jira",
			wantSrcText:  "jira",
			wantSrcGlyph: "ji",
			wantActCls:   "trig-action-spawn_agent",
			wantActFull:  "spawn_agent",
			wantMatch:    "JQL: project = AION AND status = &#39;In Review&#39;",
			wantDest:     "→ spawn role=reviewer (aion)",
		},
		{
			name: "gitlab_apply_formula",
			bead: &beadsapi.BeadDetail{
				ID: "gd-gl1", Title: "Pipeline failed", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:gitlab"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":          "apply_formula",
					"project_path":    "acme/platform",
					"event_type":      "pipeline",
					"pipeline_status": "failed",
					"pipeline_ref":    "main",
					"formula_id":      "gd-FORMULA",
				},
			},
			wantSrcCls:   "trig-src-gitlab",
			wantSrcText:  "gitlab",
			wantSrcGlyph: "gl",
			wantActCls:   "trig-action-apply_formula",
			wantActFull:  "apply_formula",
			wantMatch:    "acme/platform · pipeline=failed@main",
			wantDest:     "→ formula gd-FORMULA",
		},
		{
			name: "gitlab_pipeline_ref_pattern",
			bead: &beadsapi.BeadDetail{
				ID: "gd-gl2", Title: "Train failures", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:gitlab"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":               "apply_formula",
					"project_path":         "acme/platform",
					"event_type":           "pipeline",
					"pipeline_status":      "failed",
					"pipeline_ref_pattern": `^(main|refs/merge-requests/[0-9]+/train)$`,
					"formula_id":           "gd-FORMULA",
				},
			},
			wantSrcCls:   "trig-src-gitlab",
			wantSrcText:  "gitlab",
			wantSrcGlyph: "gl",
			wantActCls:   "trig-action-apply_formula",
			wantActFull:  "apply_formula",
			wantMatch:    "acme/platform · pipeline=failed ~/^(main|refs/merge-requests/[0-9]&#43;/train)$/",
			wantDest:     "→ formula gd-FORMULA",
		},
		{
			name: "gitlab_pipeline_min_duration",
			bead: &beadsapi.BeadDetail{
				ID: "gd-gl3", Title: "Slow green trains", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:gitlab"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":               "notify",
					"project_path":         "acme/platform",
					"event_type":           "pipeline",
					"pipeline_status":      "success",
					"pipeline_ref_pattern": `^refs/merge-requests/[0-9]+/train$`,
					"min_duration":         "45m",
				},
			},
			wantSrcCls:   "trig-src-gitlab",
			wantSrcText:  "gitlab",
			wantSrcGlyph: "gl",
			wantActCls:   "trig-action-notify",
			wantActFull:  "notify",
			wantMatch:    "acme/platform · pipeline=success ~/^refs/merge-requests/[0-9]&#43;/train$/ ≥45m",
			wantDest:     "→ notify",
		},
		{
			name: "webhook_create_bead",
			bead: &beadsapi.BeadDetail{
				ID: "gd-wh1", Title: "Pagerduty webhook", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:webhook"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":        "create_bead",
					"webhook_token": "pd-secret-abc",
					"conditions":    `[{"path":"event.kind","equals":"incident"}]`,
					"bead_type":     "bug",
					"bead_priority": "1",
				},
			},
			wantSrcCls:   "trig-src-webhook",
			wantSrcText:  "webhook",
			wantSrcGlyph: "wh",
			wantActCls:   "trig-action-create_bead",
			wantActFull:  "create_bead",
			wantMatch:    "POST /webhooks/pd-secret-abc · 1 conditions",
			wantDest:     "→ create type=bug priority=1",
		},
		{
			name: "bead_notify",
			bead: &beadsapi.BeadDetail{
				ID: "gd-bd1", Title: "Bead-event notifier", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:bead"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":      "notify",
					"event_types": "claimed,closed",
					"conditions":  `[{"label":"priority:0"}]`,
				},
			},
			wantSrcCls:   "trig-src-bead",
			wantSrcText:  "bead",
			wantSrcGlyph: "bd",
			wantActCls:   "trig-action-notify",
			wantActFull:  "notify",
			wantMatch:    "claimed,closed · 1 conditions",
			wantDest:     "→ notify Slack",
		},
		{
			name: "account_spawn_agent",
			bead: &beadsapi.BeadDetail{
				ID: "gd-ac1", Title: "Account health watch", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:account"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":         "spawn_agent",
					"condition_type": "credit",
					"field":          "balance",
					"op":             "<",
					"threshold":      "100",
					"role":           "ops",
				},
			},
			wantSrcCls:   "trig-src-account",
			wantSrcText:  "account",
			wantSrcGlyph: "ac",
			wantActCls:   "trig-action-spawn_agent",
			wantActFull:  "spawn_agent",
			wantMatch:    "credit balance &lt; 100",
			wantDest:     "→ spawn role=ops",
		},
		{
			name: "schedule_spawn_agent",
			bead: &beadsapi.BeadDetail{
				ID: "gd-sc1", Title: "Hourly gardener", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:schedule"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":   "spawn_agent",
					"cron":     "0 * * * *",
					"timezone": "UTC",
					"role":     "gardener",
					"project":  "gardenio",
				},
			},
			wantSrcCls:   "trig-src-schedule",
			wantSrcText:  "schedule",
			wantSrcGlyph: "sc",
			wantActCls:   "trig-action-spawn_agent",
			wantActFull:  "spawn_agent",
			// next-fire countdown folded in to the schedule match line; the
			// exact suffix depends on now vs cron, so assert the stable prefix.
			wantMatch: "cron: 0 * * * * (UTC) · next ",
			wantDest:  "→ spawn role=gardener (gardenio)",
		},
		{
			name: "slack_reaction_create_bead",
			bead: &beadsapi.BeadDetail{
				ID: "gd-sl1", Title: "Eyes-react", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:slack-reaction"},
				UpdatedAt: now,
				Fields: map[string]string{
					"action":         "create_bead",
					"emoji":          "eyes",
					"channel_filter": "ops",
					"match":          "ALERT",
					"bead_type":      "task",
				},
			},
			wantSrcCls:   "trig-src-slack-reaction",
			wantSrcText:  "slack-reaction",
			wantSrcGlyph: "sl",
			wantActCls:   "trig-action-create_bead",
			wantActFull:  "create_bead",
			wantMatch:    `:eyes: in #ops (substring &#34;ALERT&#34;)`,
			wantDest:     "→ create type=task",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			f.listResp = &beadsapi.ListBeadsResult{}
			f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{c.bead}, Total: 1}
			mux := mkServerWithNow(t, f, func() time.Time { return now })

			req := httptest.NewRequest("GET", "/triggers", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			row := extractRow(w.Body.String(), c.bead.ID)
			if row == "" {
				t.Fatalf("row for %s not rendered\nbody=%s", c.bead.ID, w.Body.String())
			}
			for _, want := range []string{
				c.wantSrcCls,
				`<span class="trig-src-full">` + c.wantSrcText + `</span>`,
				`<span class="trig-src-glyph">` + c.wantSrcGlyph + `</span>`,
				c.wantActCls,
				`<span class="trig-action-full">` + c.wantActFull + `</span>`,
				c.wantMatch,
				c.wantDest,
			} {
				if !strings.Contains(row, want) {
					t.Errorf("row %s missing %q\nrow=%s", c.bead.ID, want, row)
				}
			}
			// Non-schedule rows must not leak a `next: <error>` slot — that
			// noise was the original bug behind the row redesign.
			if c.bead.Labels[0] != "trigger:schedule" {
				if strings.Contains(row, "next: ") || strings.Contains(row, "expected exactly 5 fields") {
					t.Errorf("non-schedule row leaked next-fire error\nrow=%s", row)
				}
			}
		})
	}
}

// TestHandleTriggers_DensityRoundTrip checks that the server-side default
// `?density=` knob renders the density into the wrapper attribute. The inline
// localStorage script overrides the default at the client; this assertion
// covers the no-script path (and the SSR fallback that prevents FOUC).
func TestHandleTriggers_DensityRoundTrip(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-d1", Title: "Density test", Type: "schedule", Status: "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	cases := []struct {
		query string
		want  string
	}{
		{"", "comfortable"}, // default
		{"?density=comfortable", "comfortable"},
		{"?density=compact", "compact"},
		{"?density=bogus", "comfortable"}, // unknown values fall back to default
	}
	for _, c := range cases {
		t.Run("density="+c.query, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/triggers"+c.query, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			wantAttr := `data-density="` + c.want + `"`
			if !strings.Contains(body, wantAttr) {
				t.Errorf("expected wrapper %s, body=%s", wantAttr, body)
			}
			// Density toggle must be present with an `active` link for the
			// current selection so the segmented control reflects state.
			if !strings.Contains(body, "triggers-density-toggle") {
				t.Errorf("expected density toggle in body=%s", body)
			}
		})
	}
}

// TestHandleTriggers_LocalStorageScriptInert verifies the FOUC-avoidance
// inline script is present in the rendered HTML but doesn't break server-side
// rendering (it's wrapped in try/catch and inspects window.localStorage). The
// SSR output must contain the wrapper attribute regardless.
func TestHandleTriggers_LocalStorageScriptInert(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`localStorage.getItem('triggers.density')`,
		`data-triggers-density`,
		`triggers.density`,
		`data-density=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body, body=%s", want, body)
		}
	}
}

// TestHandleTriggers_DefaultSortSourceTitle verifies the b-density default
// sort order: rows group by source (alphabetical), then by title. The old
// default was next-fire which sank non-schedule sources to the bottom.
func TestHandleTriggers_DefaultSortSourceTitle(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// Intentionally out-of-order: source=schedule comes alphabetically
			// after gitlab and jira; within each source we want title order.
			{
				ID: "gd-sc-z", Title: "Zeta schedule", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:schedule"},
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "action": "spawn_agent"},
			},
			{
				ID: "gd-jr-a", Title: "Alpha jira", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"jql": "x", "action": "spawn_agent"},
			},
			{
				ID: "gd-gl-b", Title: "Beta gitlab", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:gitlab"},
				UpdatedAt: now,
				Fields:    map[string]string{"project_path": "p/q", "action": "spawn_agent"},
			},
			{
				ID: "gd-jr-c", Title: "Charlie jira", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"jql": "y", "action": "spawn_agent"},
			},
		},
		Total: 4,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	iBeta := strings.Index(body, "Beta gitlab")
	iAlpha := strings.Index(body, "Alpha jira")
	iCharlie := strings.Index(body, "Charlie jira")
	iZeta := strings.Index(body, "Zeta schedule")
	if iBeta < 0 || iAlpha < 0 || iCharlie < 0 || iZeta < 0 {
		t.Fatalf("expected all four rows present\nbody=%s", body)
	}
	// gitlab < jira < schedule alphabetically; within jira, Alpha < Charlie.
	if !(iBeta < iAlpha && iAlpha < iCharlie && iCharlie < iZeta) {
		t.Errorf("default sort wrong: got beta=%d alpha=%d charlie=%d zeta=%d (want beta<alpha<charlie<zeta)",
			iBeta, iAlpha, iCharlie, iZeta)
	}
}

// TestHandleTriggers_SourceFilter exercises the ?source= chip-strip filter.
// jira is selected; gitlab and slack-reaction rows must drop out of the list.
// All three chips remain in the strip (with counts), and the jira chip is
// rendered as the active span. The href on the gitlab chip must preserve any
// other filter dimensions (sort/density/action) the operator has set.
func TestHandleTriggers_SourceFilter(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-jr-1", Title: "Jira row", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"jql": "x", "action": "spawn_agent"},
			},
			{
				ID: "gd-gl-1", Title: "Gitlab row", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:gitlab"},
				UpdatedAt: now,
				Fields:    map[string]string{"project_path": "p/q", "action": "spawn_agent"},
			},
			{
				ID: "gd-sl-1", Title: "Slack row", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:slack-reaction"},
				UpdatedAt: now,
				Fields:    map[string]string{"emoji": "eyes", "action": "create_bead"},
			},
		},
		Total: 3,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers?source=jira", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Jira row") {
		t.Errorf("expected jira row visible; body=%s", body)
	}
	for _, dropped := range []string{">Gitlab row<", ">Slack row<"} {
		if strings.Contains(body, dropped) {
			t.Errorf("expected %s filtered out by ?source=jira; body=%s", dropped, body)
		}
	}
	// Active jira chip must render as a span (not a link).
	if !strings.Contains(body, `chip-active" data-source="jira"`) {
		t.Errorf("expected active jira chip span; body=%s", body)
	}
	// Inactive chips remain as links and carry the ?source= query.
	if !strings.Contains(body, `href="/triggers?source=gitlab"`) {
		t.Errorf("expected gitlab chip link; body=%s", body)
	}
	// "All sources" chip clears the filter.
	if !strings.Contains(body, `href="/triggers"`) {
		t.Errorf("expected 'All sources' clear-link; body=%s", body)
	}
}

// TestHandleTriggers_ActionFilter exercises the ?action= chip-strip filter.
// spawn_agent is selected; create_bead/notify rows drop out.
func TestHandleTriggers_ActionFilter(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-act-spawn", Title: "Spawner", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"action": "spawn_agent", "jql": "x"},
			},
			{
				ID: "gd-act-bead", Title: "Bead-creator", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"action": "create_bead", "jql": "y"},
			},
			{
				ID: "gd-act-notify", Title: "Notifier", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"action": "notify", "jql": "z"},
			},
		},
		Total: 3,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers?action=spawn_agent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Spawner") {
		t.Errorf("expected spawn row visible; body=%s", body)
	}
	for _, dropped := range []string{">Bead-creator<", ">Notifier<"} {
		if strings.Contains(body, dropped) {
			t.Errorf("expected %s filtered out by ?action=spawn_agent; body=%s", dropped, body)
		}
	}
	if !strings.Contains(body, `chip-active" data-action="spawn_agent"`) {
		t.Errorf("expected active spawn_agent chip; body=%s", body)
	}
	if !strings.Contains(body, `href="/triggers?action=create_bead"`) {
		t.Errorf("expected create_bead chip link; body=%s", body)
	}
}

// TestHandleTriggers_SourceAndActionFilterCompose verifies that source and
// action filters combine (intersection) and that chip hrefs preserve the
// other dimension when the operator has one active.
func TestHandleTriggers_SourceAndActionFilterCompose(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-jr-spawn", Title: "Jira spawn", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"action": "spawn_agent", "jql": "x"},
			},
			{
				ID: "gd-jr-bead", Title: "Jira bead", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"action": "create_bead", "jql": "y"},
			},
			{
				ID: "gd-gl-spawn", Title: "Gitlab spawn", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:gitlab"},
				UpdatedAt: now,
				Fields:    map[string]string{"action": "spawn_agent", "project_path": "p/q"},
			},
		},
		Total: 3,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers?source=jira&action=spawn_agent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "Jira spawn") {
		t.Errorf("intersection should keep jira+spawn; body=%s", body)
	}
	for _, dropped := range []string{">Jira bead<", ">Gitlab spawn<"} {
		if strings.Contains(body, dropped) {
			t.Errorf("expected %s filtered out by intersection; body=%s", dropped, body)
		}
	}
	// gitlab chip-link must preserve the action filter so clicking it only
	// changes the source dimension.
	if !strings.Contains(body, `href="/triggers?action=spawn_agent&amp;source=gitlab"`) &&
		!strings.Contains(body, `href="/triggers?source=gitlab&amp;action=spawn_agent"`) {
		t.Errorf("expected gitlab chip href to preserve ?action=spawn_agent; body=%s", body)
	}
}

// TestHandleTriggers_SourceFilterRejectsBogus verifies that unknown ?source=
// values are silently dropped — bogus inputs should not cause a 500 or render
// an active chip that isn't in the enum.
func TestHandleTriggers_SourceFilterRejectsBogus(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/triggers?source=not-a-real-source", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// "All sources" should be the active chip when the input is rejected. The
	// active toggle now also carries the standardized aria-current="true" cue
	// (ka-0000002684 / audit F3).
	if !strings.Contains(body, `chip-active" data-source="" aria-current="true">All sources`) {
		t.Errorf("expected 'All sources' active when source query is bogus; body=%s", body)
	}
}

// TestHandleTriggers_SortFireCount verifies ?sort=fire-count orders rows by
// the fire_count field descending.
func TestHandleTriggers_SortFireCount(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-low", Title: "Low fires", Type: "schedule", Status: "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true", "fire_count": "3"},
			},
			{
				ID: "gd-high", Title: "High fires", Type: "schedule", Status: "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true", "fire_count": "99"},
			},
			{
				ID: "gd-mid", Title: "Mid fires", Type: "schedule", Status: "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true", "fire_count": "20"},
			},
		},
		Total: 3,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers?sort=fire-count", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	iHigh := strings.Index(body, "High fires")
	iMid := strings.Index(body, "Mid fires")
	iLow := strings.Index(body, "Low fires")
	if iHigh < 0 || iMid < 0 || iLow < 0 {
		t.Fatalf("expected all three rows present\nbody=%s", body)
	}
	if !(iHigh < iMid && iMid < iLow) {
		t.Errorf("?sort=fire-count: expected High < Mid < Low, got high=%d mid=%d low=%d",
			iHigh, iMid, iLow)
	}
	// Sort link must render as active.
	if !strings.Contains(body, `<a href="/triggers?sort=fire-count" class="is-active">fire count</a>`) {
		t.Errorf("expected fire-count sort link active; body=%s", body)
	}
}

// TestHandleTriggers_SortLastFired verifies ?sort=last-fired orders rows by
// the freshest activity timestamp (last_fired / last_run / last_checked).
func TestHandleTriggers_SortLastFired(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-stale", Title: "Stale", Type: "schedule", Status: "open",
				UpdatedAt: now,
				Fields: map[string]string{
					"cron": "0 9 * * *", "enabled": "true",
					"last_fired": "2026-05-01T00:00:00Z",
				},
			},
			{
				ID: "gd-fresh", Title: "Fresh", Type: "schedule", Status: "open",
				UpdatedAt: now,
				Fields: map[string]string{
					"cron": "0 9 * * *", "enabled": "true",
					"last_fired": "2026-05-08T11:00:00Z",
				},
			},
			{
				ID: "gd-mid", Title: "Midfire", Type: "schedule", Status: "open",
				UpdatedAt: now,
				Fields: map[string]string{
					"cron": "0 9 * * *", "enabled": "true",
					"last_fired": "2026-05-06T00:00:00Z",
				},
			},
		},
		Total: 3,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers?sort=last-fired", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	iFresh := strings.Index(body, "Fresh")
	iMid := strings.Index(body, "Midfire")
	iStale := strings.Index(body, "Stale")
	if iFresh < 0 || iMid < 0 || iStale < 0 {
		t.Fatalf("expected all three rows; body=%s", body)
	}
	if !(iFresh < iMid && iMid < iStale) {
		t.Errorf("?sort=last-fired: expected Fresh < Midfire < Stale, got fresh=%d mid=%d stale=%d",
			iFresh, iMid, iStale)
	}
}

// TestHandleTriggers_BlockedOnDecisionBadge verifies the row_badges partial
// renders a clickable ⏸ badge on a trigger row that the daemon has flagged
// blocked_on_decision. The badge links to the specific decision bead and the
// toggle remains live (ka-0000001354 policy: surface, do not block).
func TestHandleTriggers_BlockedOnDecisionBadge(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                      "gd-blocked",
				Title:                   "Blocked trigger",
				Type:                    "schedule",
				Status:                  "open",
				UpdatedAt:               now,
				BlockedOnDecision:       true,
				BlockedOnDecisionBeadID: "gd-dec-42",
				Fields:                  map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
			{
				ID:        "gd-fine",
				Title:     "Fine trigger",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	rowBlocked := extractRow(body, "gd-blocked")
	if rowBlocked == "" {
		t.Fatalf("blocked row not rendered; body=%s", body)
	}
	if !strings.Contains(rowBlocked, "bead-row-badge-blocked") {
		t.Errorf("expected blocked badge on row; row=%s", rowBlocked)
	}
	if !strings.Contains(rowBlocked, `href="/gd-dec-42"`) {
		t.Errorf("expected badge to link to specific decision bead; row=%s", rowBlocked)
	}
	// The toggle must still render (policy: surface, do not block).
	if !strings.Contains(rowBlocked, "trig-toggle") {
		t.Errorf("expected toggle to remain live on blocked-by row; row=%s", rowBlocked)
	}

	// The unblocked row should NOT carry the badge.
	rowFine := extractRow(body, "gd-fine")
	if strings.Contains(rowFine, "bead-row-badge-blocked") {
		t.Errorf("unblocked row should not carry the blocked badge; row=%s", rowFine)
	}
}

// TestHandleTriggers_HideToggleOnClosed verifies that a closed trigger bead
// renders the inline "closed" chip but NOT the per-row toggle or bulk-select
// checkbox — flipping enabled on a closed bead is not a legitimate operator
// affordance.
func TestHandleTriggers_HideToggleOnClosed(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-closed",
				Title:     "Closed trigger",
				Type:      "schedule",
				Status:    "closed",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
			{
				ID:        "gd-open",
				Title:     "Open trigger",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: now,
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	rowClosed := extractRow(body, "gd-closed")
	if rowClosed == "" {
		t.Fatalf("closed row not rendered; body=%s", body)
	}
	if !strings.Contains(rowClosed, "trig-state-closed") {
		t.Errorf("expected closed chip on closed row; row=%s", rowClosed)
	}
	if strings.Contains(rowClosed, "trig-toggle-btn") {
		t.Errorf("closed row must NOT render the inline toggle; row=%s", rowClosed)
	}
	if strings.Contains(rowClosed, "trigger-row-select") {
		t.Errorf("closed row must NOT render the bulk-select checkbox; row=%s", rowClosed)
	}

	// The open row should still carry both.
	rowOpen := extractRow(body, "gd-open")
	if !strings.Contains(rowOpen, "trig-toggle-btn") {
		t.Errorf("open row should still render the toggle; row=%s", rowOpen)
	}
	if !strings.Contains(rowOpen, "trigger-row-select") {
		t.Errorf("open row should still render the bulk-select checkbox; row=%s", rowOpen)
	}
}

// TestHandleTriggers_NoNextFireErrorOnNonCron verifies the row for a jira
// trigger does NOT render a next-fire span — that slot is gone for every
// non-schedule source.
func TestHandleTriggers_NoNextFireErrorOnNonCron(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-jirax", Title: "Jira no cron", Type: "trigger", Status: "open",
				Labels:    []string{"trigger:jira"},
				UpdatedAt: now,
				Fields:    map[string]string{"jql": "project = X", "action": "spawn_agent"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	row := extractRow(body, "gd-jirax")
	if row == "" {
		t.Fatalf("row not rendered, body=%s", body)
	}
	for _, bad := range []string{"next: ", "expected exactly 5 fields", "next-fire-err"} {
		if strings.Contains(row, bad) {
			t.Errorf("non-cron row contains %q\nrow=%s", bad, row)
		}
	}
}
