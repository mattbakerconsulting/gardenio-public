package main

// Golden-render test for the gardener_detail page — the trajectory strip and the
// iteration cards (task gd-qtrGoeCNbR, parent ka-0000004480).
//
// This is a table-driven render-and-assert test (the codebase's golden idiom:
// render the real template against fixture GardenerSpec/ProgressState blobs and
// assert on the produced HTML, rather than diffing a byte-for-byte fixture
// file). It locks the !3317 (cross-run trajectory strip) and !3318
// (outcome-first iteration cards) invariants so a regression fails CI:
//
//   - Trajectory strip: rendered (<section class="gardener-trajectory">) when the
//     gardener's ProgressState carries scored runs and/or credited outcome events,
//     and SUPPRESSED entirely when there is nothing to narrate (HasData=false —
//     no ProgressState and no spec counters).
//   - Iteration cards: every card leads with an OUTCOME line in the headline
//     anchor, following the precedence wrap-up tldr → accomplishments → derived
//     outcome → task title — never a bare agent name as the headline.
//   - Counter rows: the authoritative Iterations row plus the engine-snapshot
//     Runs-spawned / Runs-completed rows are always present in the header.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// renderGardenerGolden renders GET /gardener/{id} against the supplied fakeDaemon
// at the pinned gardenerPageNow clock and returns the response body, failing the
// test on any non-200 so each case asserts against a known-good render.
func renderGardenerGolden(t *testing.T, f *fakeDaemon, id string) string {
	t.Helper()
	mux := mkServerWithNow(t, f, gardenerPageNow)
	req := httptest.NewRequest("GET", "/gardener/"+id, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /gardener/%s: expected 200, got %d body=%s", id, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// goldenGardenerFields encodes a GardenerSpec map into the trigger bead's `gardener`
// field, matching mkGardenerBead's shape.
func goldenGardenerBead(t *testing.T, id, title string, spec map[string]any) *beadsapi.BeadDetail {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal gardener spec: %v", err)
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     title,
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
}

// TestGardenerDetailGolden_Trajectory pins the trajectory-strip invariant from
// !3317: the strip renders with its verdict, sparkline bars, and aggregate stat
// rows when ProgressState carries data, and is suppressed entirely when there is
// nothing to narrate. Table-driven so the present/absent forks share one render
// harness and read as a single contract.
func TestGardenerDetailGolden_Trajectory(t *testing.T) {
	cases := []struct {
		name        string
		spec        map[string]any
		wantPresent []string // substrings that MUST appear
		wantAbsent  []string // substrings that MUST NOT appear
	}{
		{
			name: "progress_state_with_runs_and_events_renders_strip",
			spec: map[string]any{
				"name":           "delivery",
				"terminal_goal":  "drain the ready queue",
				"runs_spawned":   3,
				"runs_completed": 3,
				"progress_state": map[string]any{
					"runs": []map[string]any{
						{"agent_id": "a1", "progress_score": 2, "agent_state": "done"},
						{"agent_id": "a2", "progress_score": 0, "agent_state": "done"},
						{"agent_id": "a3", "progress_score": 5, "agent_state": "done"},
					},
					"events": []map[string]any{
						{"kind": "mr_merged", "target_id": "gd-mr1", "occurred_at": "2026-06-01T10:00:00Z"},
						{"kind": "mr_merged", "target_id": "gd-mr2", "occurred_at": "2026-06-01T10:01:00Z"},
						{"kind": "child_closed", "target_id": "gd-c1", "occurred_at": "2026-06-01T10:05:00Z"},
					},
				},
			},
			wantPresent: []string{
				`<section class="gardener-trajectory"`,   // the strip container
				"gardener-verdict-progress",              // verdict styling hook
				"making progress",                        // verdict text
				`class="gardener-spark"`,                 // sparkline row
				"gardener-spark-bar gardener-spark-ok",   // a scored (ok) bar
				"gardener-spark-bar gardener-spark-flat", // the zero-score (flat) bar
				"Outcome rate",                           // aggregate stat label
				"MRs merged",
				"Beads closed",
			},
			wantAbsent: nil,
		},
		{
			name: "stuck_verdict_renders_failure_streak",
			spec: map[string]any{
				"name":                 "delivery",
				"consecutive_failures": 4,
				"runs_completed":       2,
				"progress_state": map[string]any{
					"runs": []map[string]any{
						{"agent_id": "a1", "progress_score": 3, "agent_state": "done"},
						{"agent_id": "a2", "progress_score": 0, "agent_state": "failed"},
					},
				},
			},
			wantPresent: []string{
				`<section class="gardener-trajectory"`,
				"gardener-verdict-stuck",
				"stuck",
				"gardener-spark-bar gardener-spark-fail", // the failed-run bar
				"Consecutive failures",
			},
			wantAbsent: []string{"making progress"},
		},
		{
			name: "no_progress_no_counters_suppresses_strip",
			spec: map[string]any{
				"name":          "fresh",
				"terminal_goal": "warm up",
			},
			wantPresent: nil,
			wantAbsent: []string{
				`<section class="gardener-trajectory"`,
				`class="gardener-spark"`, // the rendered sparkline row (CSS uses .gardener-spark)
				"Outcome rate",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.beads["gd-cap"] = goldenGardenerBead(t, "gd-cap", "Delivery gardener", tc.spec)
			f.liveAgentsResp = &beadsapi.ListBeadsResult{}

			body := renderGardenerGolden(t, f, "gd-cap")
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("rendered gardener page missing %q", want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(body, bad) {
					t.Errorf("rendered gardener page unexpectedly contains %q (strip should be suppressed)", bad)
				}
			}
		})
	}
}

// TestGardenerDetailGolden_BudgetBurn pins the run-budget burn meter invariant
// (task ka-0000007105): the operator's #1 question — "how close is this gardener
// to stopping?" — must be answered by a first-class progress bar, not a pair of
// plain numbers. The meter renders spawned/budget as a filled track colour-graded
// by burn level (ok < 75% < warn < 100% <= over), and is SUPPRESSED entirely for
// a gardener with no run_budget ceiling (nothing to burn against).
func TestGardenerDetailGolden_BudgetBurn(t *testing.T) {
	cases := []struct {
		name        string
		spec        map[string]any
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "under_budget_renders_ok_meter",
			spec: map[string]any{
				"name":         "delivery",
				"run_budget":   200,
				"runs_spawned": 77,
			},
			wantPresent: []string{
				`class="gardener-budget gardener-budget-ok"`,
				`role="progressbar"`,
				`aria-valuenow="38"`, // 77/200 = 38%
				"77 / 200",
				"(123 left)",
				`style="width: 38%"`,
			},
			wantAbsent: []string{"at budget", `gardener-budget gardener-budget-over"`, `gardener-budget gardener-budget-warn"`},
		},
		{
			name: "near_budget_renders_warn_meter",
			spec: map[string]any{
				"name":         "delivery",
				"run_budget":   100,
				"runs_spawned": 90,
			},
			wantPresent: []string{
				`class="gardener-budget gardener-budget-warn"`,
				`aria-valuenow="90"`,
				"90 / 100",
				"(10 left)",
			},
			wantAbsent: []string{"at budget", `gardener-budget gardener-budget-over"`},
		},
		{
			name: "at_budget_renders_over_meter",
			spec: map[string]any{
				"name":         "delivery",
				"run_budget":   50,
				"runs_spawned": 50,
			},
			wantPresent: []string{
				`class="gardener-budget gardener-budget-over"`,
				`aria-valuenow="100"`,
				"50 / 50",
				"at budget",
			},
			wantAbsent: []string{"left)"},
		},
		{
			name: "over_budget_clamps_bar_to_100",
			spec: map[string]any{
				"name":         "delivery",
				"run_budget":   40,
				"runs_spawned": 55, // resumed gardener, briefly over before refill
			},
			wantPresent: []string{
				`class="gardener-budget gardener-budget-over"`,
				`aria-valuenow="100"`,
				`style="width: 100%"`,
				"55 / 40",
				"at budget",
			},
		},
		{
			name: "no_run_budget_suppresses_meter",
			spec: map[string]any{
				"name":         "standing",
				"runs_spawned": 12,
			},
			wantAbsent: []string{
				`class="gardener-budget`,
				"Budget burn",
				`role="progressbar"`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.beads["gd-cap"] = goldenGardenerBead(t, "gd-cap", "Budget gardener", tc.spec)
			f.liveAgentsResp = &beadsapi.ListBeadsResult{}

			body := renderGardenerGolden(t, f, "gd-cap")
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("rendered gardener page missing %q", want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(body, bad) {
					t.Errorf("rendered gardener page unexpectedly contains %q", bad)
				}
			}
		})
	}
}

// TestGardenerDetailGolden_StandingSuppressesBudgetMeter is the end-to-end
// regression for bug ka-0000006238, exercising the LABEL path (perpetual:true)
// that the live gardener-delivery gardener actually uses. A standing gardener that
// carries a refilled run_budget (RunsSpawned >> RunBudget) must NOT render the
// red "at budget / over" burn meter — that reads as a false runaway signal —
// and must instead render the calm "standing · unbounded by design" chip so the
// operator gets the affirmative answer.
func TestGardenerDetailGolden_StandingSuppressesBudgetMeter(t *testing.T) {
	// Standing recognized via the perpetual:true LABEL (blob does NOT set
	// standing:true), mirroring the live delivery gardener ka-0000001510.
	spec := map[string]any{
		"name":         "delivery",
		"run_budget":   1672,
		"runs_spawned": 2093, // >> budget: pre-fix this rendered "over" + "at budget"
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal gardener spec: %v", err)
	}
	bead := &beadsapi.BeadDetail{
		ID:        "gd-cap",
		Title:     "Standing delivery gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener-delivery", "project:gardenio", "perpetual:true"},
		Fields:    map[string]string{"role": "gardener-delivery", "gardener": string(raw)},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}

	f := newFake()
	f.beads["gd-cap"] = bead
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}

	body := renderGardenerGolden(t, f, "gd-cap")

	// The misleading burn meter must be gone.
	for _, bad := range []string{
		`class="gardener-budget gardener-budget-over"`,
		`class="gardener-budget gardener-budget-warn"`,
		"at budget",
		`role="progressbar"`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("standing gardener page unexpectedly contains burn-meter fragment %q", bad)
		}
	}
	// The affirmative standing chip must render in its place.
	for _, want := range []string{
		`class="gardener-budget gardener-budget-standing"`,
		"standing",
		"unbounded by design",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("standing gardener page missing %q", want)
		}
	}
}

// TestGardenerDetailGolden_Schedule pins the detail-header schedule strip
// invariant (task ka-0000002425): an operator must be able to read a gardener's
// next-fire and cadence (cron) from the detail page without bouncing back to
// the /gardeners index. The strip renders next-fire + cron when the gardener
// carries a parseable cron, surfaces a disabled/closed "not scheduled" note
// when the gardener won't fire, and is suppressed entirely for bead-trigger
// gardeners with no cron. gardenerPageNow is pinned to 2026-06-01 12:00 UTC.
func TestGardenerDetailGolden_Schedule(t *testing.T) {
	cases := []struct {
		name        string
		cron        string
		timezone    string
		enabled     string // "" leaves the field unset (default-on)
		closed      bool
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "enabled_cron_renders_next_fire_and_cadence",
			cron: "0 * * * *", // hourly: next fire is 13:00 UTC, i.e. "fires in 1h"
			wantPresent: []string{
				`class="gardener-schedule"`,
				"fires in",
				"gardener-cadence",
				"cadence:",
				"0 * * * *",
			},
			wantAbsent: []string{"not scheduled", "cron error"},
		},
		{
			name:     "cron_with_timezone_surfaces_zone",
			cron:     "0 9 * * *",
			timezone: "America/New_York",
			wantPresent: []string{
				`class="gardener-schedule"`,
				"fires in",
				"0 9 * * *",
				"America/New_York",
			},
		},
		{
			name:    "disabled_gardener_shows_cadence_but_not_scheduled",
			cron:    "0 * * * *",
			enabled: "false",
			wantPresent: []string{
				`class="gardener-schedule"`,
				"not scheduled (disabled)",
				"cadence:",
				"0 * * * *",
			},
			wantAbsent: []string{"fires in"},
		},
		{
			name:   "closed_gardener_shows_cadence_but_not_scheduled",
			cron:   "0 * * * *",
			closed: true,
			wantPresent: []string{
				`class="gardener-schedule"`,
				"not scheduled (closed)",
				"0 * * * *",
			},
			wantAbsent: []string{"fires in"},
		},
		{
			name: "unparseable_cron_surfaces_error",
			cron: "not-a-cron",
			wantPresent: []string{
				`class="gardener-schedule"`,
				"cron error",
				"not-a-cron",
			},
			wantAbsent: []string{"fires in"},
		},
		{
			name: "no_cron_suppresses_strip",
			cron: "",
			wantAbsent: []string{
				`class="gardener-schedule"`,
				"cadence:",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			bead := goldenGardenerBead(t, "gd-cap", "Schedule gardener", map[string]any{"name": "sched"})
			if tc.cron != "" {
				bead.Fields["cron"] = tc.cron
			}
			if tc.timezone != "" {
				bead.Fields["timezone"] = tc.timezone
			}
			if tc.enabled != "" {
				bead.Fields["enabled"] = tc.enabled
			}
			if tc.closed {
				bead.Status = "closed"
			}
			f.beads["gd-cap"] = bead
			f.liveAgentsResp = &beadsapi.ListBeadsResult{}

			body := renderGardenerGolden(t, f, "gd-cap")
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("rendered gardener page missing %q", want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(body, bad) {
					t.Errorf("rendered gardener page unexpectedly contains %q", bad)
				}
			}
		})
	}
}

// TestGardenerDetailGolden_RunPrompt pins the per-run prompt surfacing (bug
// ka-0000003596): the gardener's operative instruction (the top-level `prompt`
// field, sent verbatim to every run it spawns) must render in a collapsible
// "Run prompt" block on the detail page. It was previously invisible on every
// modern gardener surface. The block is default-open for a prompt-only gardener
// (no terminal goal) so the sanctioned prompt-only shape is not silent about
// what it does, and closed (present but collapsed) when a terminal goal already
// leads. gardenerPageNow is pinned to 2026-06-01 12:00 UTC.
func TestGardenerDetailGolden_RunPrompt(t *testing.T) {
	cases := []struct {
		name         string
		spec         map[string]any
		prompt       string
		wantPresent  []string
		wantAbsent   []string
		wantOpenBloc bool // "Run prompt" <details> carries the `open` attr
		wantClosed   bool // block present but NOT open
	}{
		{
			name:   "prompt_only_gardener_renders_open_block_and_prompt_text",
			spec:   map[string]any{"name": "promptonly"},
			prompt: "Drive epic gd-EPIC to closure. Spawn crew for each open child.",
			wantPresent: []string{
				`class="gardener-prompt"`,
				"Run prompt",
				"Drive epic gd-EPIC to closure",
			},
			wantOpenBloc: true,
		},
		{
			name:   "prompt_with_goal_renders_collapsed_block",
			spec:   map[string]any{"name": "withgoal", "terminal_goal": "All children of gd-EPIC closed."},
			prompt: "Spawn crew for each open child of gd-EPIC.",
			wantPresent: []string{
				`class="gardener-prompt"`,
				"Run prompt",
				"Spawn crew for each open child",
				"All children of gd-EPIC closed.",
			},
			wantClosed: true,
		},
		{
			name:       "no_prompt_suppresses_block",
			spec:       map[string]any{"name": "noprompt", "terminal_goal": "Some goal."},
			prompt:     "",
			wantAbsent: []string{`class="gardener-prompt"`, "Run prompt"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			bead := goldenGardenerBead(t, "gd-cap", "Prompt gardener", tc.spec)
			if tc.prompt != "" {
				bead.Fields["prompt"] = tc.prompt
			}
			f.beads["gd-cap"] = bead
			f.liveAgentsResp = &beadsapi.ListBeadsResult{}

			body := renderGardenerGolden(t, f, "gd-cap")
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("rendered gardener page missing %q", want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(body, bad) {
					t.Errorf("rendered gardener page unexpectedly contains %q", bad)
				}
			}
			if tc.wantOpenBloc && !strings.Contains(body, `<details class="gardener-prompt" open>`) {
				t.Errorf("expected the Run prompt block to be default-open for a prompt-only gardener")
			}
			if tc.wantClosed {
				if !strings.Contains(body, `<details class="gardener-prompt">`) {
					t.Errorf("expected the Run prompt block present-but-collapsed when a goal leads")
				}
				if strings.Contains(body, `<details class="gardener-prompt" open>`) {
					t.Errorf("Run prompt block should NOT be open when a terminal goal already leads")
				}
			}
		})
	}
}

// TestGardenerDetailGolden_EventModeCadence pins the mode-flip cadence
// projection (Task 6, spec 2026-07-13-gardener-mode-flip-design.md): a gardener
// flipped to event mode (trigger:multi label, cleared `cron` field, triggers[]
// with a bead_event entry + a cron fallback-watchdog entry) must still surface
// its FALLBACK cron in the cadence slot — an operator reads the guaranteed
// minimum tempo — but must NOT show a clock next-fire ("fires in …"), because
// an event gardener wakes on subtree activity, not the clock, and the chip must
// read as a fallback watchdog rather than a firing schedule. gardenerPageNow is
// pinned to 2026-06-01 12:00 UTC.
func TestGardenerDetailGolden_EventModeCadence(t *testing.T) {
	f := newFake()
	bead := goldenGardenerBead(t, "gd-cap", "Event gardener", map[string]any{"name": "evt"})
	// gardenermode.Set → event: label swapped to trigger:multi, cron cleared,
	// triggers[] holds the plan-activity bead_event entry + the fallback cron.
	bead.Labels = []string{"trigger:multi", "role:gardener", "project:gardenio", "gardener-epic:gd-EPIC"}
	bead.Fields["cron"] = ""
	bead.Fields["triggers"] = `[{"kind":"bead_event","label":"plan-activity","events":["created","updated","closed"]},{"kind":"cron","label":"fallback-watchdog","cron":"11 2,8,14,20 * * *","timezone":"America/Los_Angeles"}]`
	f.beads["gd-cap"] = bead
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}

	body := renderGardenerGolden(t, f, "gd-cap")

	wantPresent := []string{
		`class="gardener-schedule"`,
		"cadence:",
		"11 2,8,14,20 * * *", // the fallback-watchdog cron surfaces as cadence
	}
	for _, want := range wantPresent {
		if !strings.Contains(body, want) {
			t.Errorf("rendered event-mode gardener page missing %q", want)
		}
	}
	wantAbsent := []string{
		"fires in",   // no clock countdown for an event gardener (v1)
		"cron error", // the fallback cron is valid
	}
	for _, bad := range wantAbsent {
		if strings.Contains(body, bad) {
			t.Errorf("rendered event-mode gardener page unexpectedly contains %q", bad)
		}
	}
}

// TestGardenerDetailGolden_LifecycleChip pins the header lifecycle chip
// (enabled / disabled / closed) against the closed-but-enabled inconsistency
// (ka-0000008246): the misleading "enabled" chip must NEVER render for a
// gardener whose lifecycle is terminal — closed OR goal-achieved — even when
// the underlying fields.enabled is still "true" (the bit is never flipped off
// on close). Open/active and genuinely disabled gardeners must still render
// their accurate chip — no regression.
func TestGardenerDetailGolden_LifecycleChip(t *testing.T) {
	cases := []struct {
		name         string
		status       string // gardener trigger-bead status
		enabled      string // "" leaves the field unset (default-on for triggers)
		targetStatus string // "" => no live goal target; else the target epic's status
		wantPresent  []string
		wantAbsent   []string
	}{
		{
			// Open + enabled + no terminal goal target: the ordinary live gardener.
			name:        "open_enabled_shows_enabled",
			status:      "open",
			wantPresent: []string{`gardener-badge-enabled`},
			wantAbsent:  []string{`gardener-badge-closed`, `gardener-badge-disabled`},
		},
		{
			// Explicitly paused: accurate, not misleading — keep the disabled chip.
			name:        "open_disabled_shows_disabled",
			status:      "open",
			enabled:     "false",
			wantPresent: []string{`gardener-badge-disabled`},
			wantAbsent:  []string{`gardener-badge-enabled`, `gardener-badge-closed`},
		},
		{
			// The headline bug: closed trigger bead whose enabled bit was never
			// flipped off. The closed chip wins; no "enabled" chip leaks.
			name:        "closed_but_enabled_shows_closed_not_enabled",
			status:      "closed",
			enabled:     "true",
			wantPresent: []string{`gardener-badge-closed`},
			wantAbsent:  []string{`gardener-badge-enabled`},
		},
		{
			// Goal reached while the trigger bead is still open (engine has not
			// flipped it closed). Live goal-state derives "achieved" from the
			// closed target epic — the loud GOAL ACHIEVED badge is authoritative,
			// so the contradictory "enabled" chip must be suppressed.
			name:         "open_goal_achieved_suppresses_enabled",
			status:       "open",
			enabled:      "true",
			targetStatus: "closed",
			wantPresent:  []string{"🏁 GOAL ACHIEVED"},
			wantAbsent:   []string{`gardener-badge-enabled`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			spec := map[string]any{"name": "lifecycle", "terminal_goal": "do the thing"}
			if tc.targetStatus != "" {
				// A loop_end bead_query whose parent is the goal-target epic gives
				// gardenerGoalTargetID a single authoritative target to fetch live.
				spec["stop"] = map[string]any{
					"loop_end": map[string]any{
						"any": []any{
							map[string]any{"kind": "bead_query", "parent": "gd-epic"},
						},
					},
				}
				f.beads["gd-epic"] = &beadsapi.BeadDetail{ID: "gd-epic", Title: "goal epic", Status: tc.targetStatus}
			}
			bead := goldenGardenerBead(t, "gd-cap", "Lifecycle gardener", spec)
			bead.Status = tc.status
			if tc.enabled != "" {
				bead.Fields["enabled"] = tc.enabled
			}
			f.beads["gd-cap"] = bead
			f.liveAgentsResp = &beadsapi.ListBeadsResult{}

			body := renderGardenerGolden(t, f, "gd-cap")
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("rendered gardener page missing %q", want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(body, bad) {
					t.Errorf("rendered gardener page unexpectedly contains %q", bad)
				}
			}
		})
	}
}

// TestGardenerDetailGolden_IterationCards pins the outcome-first card invariant
// from !3318: each iteration card's headline anchor carries the run's outcome
// line, following the precedence tldr → accomplishments → derived → task title,
// and never renders a bare agent name as the headline. The agent name is always
// demoted to the meta row.
func TestGardenerDetailGolden_IterationCards(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = goldenGardenerBead(t, "gd-cap", "Outcome gardener", map[string]any{
		"name":     "outcome",
		"max_runs": 10,
	})

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	// Run #1: wrap-up with a tldr → tldr is the headline (precedence step 1),
	// plus a beads-closed chip and a clean-exit chip.
	r1 := mkIteration("gd-agent-alpha", "gd-cap", "", 1, "closed", t0)
	r1.Fields["wrapup"] = `{"tldr":"Merged MR 7; drained 2 ready beads","accomplishments":"long form","beads_closed":["gd-x","gd-y"]}`

	// Run #2: wrap-up with NO tldr but accomplishments present → accomplishments
	// is the headline (precedence step 2).
	r2 := mkIteration("gd-agent-bravo", "gd-cap", "gd-agent-alpha", 2, "closed", t0.Add(time.Hour))
	r2.Fields["wrapup"] = `{"accomplishments":"Refactored the trajectory parser"}`

	// Run #3: closed with NO prose wrap-up but a beads_closed signal → the
	// derived outcome line is the headline (precedence step 3), and a clean-exit
	// chip plus the "1 closed" chip render.
	r3 := mkIteration("gd-agent-charlie", "gd-cap", "gd-agent-bravo", 3, "closed", t0.Add(2*time.Hour))
	r3.Fields["wrapup"] = `{"beads_closed":["gd-z"]}`

	// Run #4: closed with NO wrap-up at all → no prose, no derived signal; the
	// card falls back to the task title for its headline and shows the "no
	// wrap-up" exit chip (the no-outcome edge of the precedence ladder).
	r4 := mkIteration("gd-agent-delta", "gd-cap", "gd-agent-charlie", 4, "closed", t0.Add(3*time.Hour))

	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{r1, r2, r3, r4},
		Total: 4,
	}

	body := renderGardenerGolden(t, f, "gd-cap")

	// Exactly four iteration cards rendered.
	if cards := renderedIterationCards(body); cards != 4 {
		t.Fatalf("rendered %d iteration cards, want 4", cards)
	}

	// Precedence step 1 — tldr is the headline of run #1.
	if !strings.Contains(body, "Merged MR 7; drained 2 ready beads") {
		t.Errorf("run #1 card missing tldr headline; body=%s", body)
	}
	// Precedence step 2 — accomplishments is the headline of run #2 (no tldr).
	if !strings.Contains(body, "Refactored the trajectory parser") {
		t.Errorf("run #2 card missing accomplishments headline; body=%s", body)
	}
	// Precedence step 3 — derived outcome line is the headline of run #3
	// (no prose wrap-up; derived from the closed bead signal).
	if !strings.Contains(body, "1 bead closed") {
		t.Errorf("run #3 card missing derived outcome headline; body=%s", body)
	}

	// The agent names are demoted to the meta row — never the headline.
	for _, name := range []string{"gd-agent-alpha", "gd-agent-bravo", "gd-agent-charlie", "gd-agent-delta"} {
		if !strings.Contains(body, name) {
			t.Errorf("agent name %q not rendered at all", name)
		}
	}
	if !strings.Contains(body, `class="meta agent-name-meta"`) {
		t.Errorf("agent name not demoted to a meta row")
	}

	// Outcome chips: beads-closed count + exit-reason chips are present.
	for _, want := range []string{"2 closed", "1 closed", "clean exit", "no wrap-up"} {
		if !strings.Contains(body, want) {
			t.Errorf("iteration cards missing outcome chip %q", want)
		}
	}

	// Outcome-first layout: the FIRST card's headline anchor (bead-row-title)
	// precedes the FIRST demoted agent-name meta span. Newest-first ordering
	// puts run #3 at the top, so its derived headline must lead.
	firstTitle := strings.Index(body, `class="bead-row-title`)
	firstNameMeta := strings.Index(body, `class="meta agent-name-meta"`)
	if firstTitle < 0 || firstNameMeta < 0 || firstTitle > firstNameMeta {
		t.Errorf("headline title (idx %d) should precede demoted agent name (idx %d)", firstTitle, firstNameMeta)
	}
}

// TestGardenerDetailGolden_CounterRows pins that the header's counter rows always
// render: the PRIMARY Iterations row (== rendered card count) plus the
// always-shown, labeled SECONDARY engine lifetime-tally line. Locks the
// counter-row surface so a template regression that drops a <dt> fails CI.
func TestGardenerDetailGolden_CounterRows(t *testing.T) {
	f := seedGardenerPageFake(t)

	body := renderGardenerGolden(t, f, "gd-cap")

	// Authoritative PRIMARY Iterations row, equal to the three rendered cards.
	if cards := renderedIterationCards(body); cards != 3 {
		t.Fatalf("rendered %d iteration cards, want 3", cards)
	}
	if !strings.Contains(body, "<dt>Iterations</dt><dd>3") {
		t.Errorf("header missing authoritative Iterations row (3); body=%s", body)
	}

	// The engine lifetime tally is present as a labeled SECONDARY line (not a
	// co-equal "Runs" authority). The seeded fixture (3 spawned / 2 completed,
	// 3 live) is within the prune gap, so no "pruned" framing here.
	for _, want := range []string{
		"Engine count",
		"lifetime tally",
		"3 spawned / 2 completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("header missing counter surface %q; body=%s", want, body)
		}
	}

	// The life-thread heading echoes the same authoritative iteration count,
	// labeled "iterations" (not "runs") so it never conflates rendered cards
	// with the engine's lifetime spawn tally (bead ka-0000005522).
	if !strings.Contains(body, "(3 iterations)") {
		t.Errorf("life-thread heading missing matching iteration count; body=%s", body)
	}
}

// ===========================================================================
// Prod-shaped golden-render invariants (task gd-8qWNw5WPoV, design doc §3/§8,
// CDQR bug ka-0000005522 / audit lane ka-0000001553).
//
// The CDQR Correctness audit on /gardener/{id} flagged a family of counter /
// tail failures: a gardener whose engine snapshot advertised hundreds of
// spawned runs while the life-thread rendered ~13 cards and the lazy /runs tail
// returned a literal "No older runs." dead-end. The four invariants below pin
// the contract that the rendered page can never reproduce that shape again,
// each exercised against three prod-shaped fixtures:
//
//	(a) a 167-head no-chain gardener — 167 iteration beads, none carrying a
//	    schedule_prior_agent_id chain, engine snapshot in agreement (167). The
//	    at-scale "consistent" case: the page must render ALL 167 across the eager
//	    head + lazy tail and the counts must agree with no drift annotation.
//	(b) a 60-iter partial-chain gardener with a 379 pruned gap — 60 retained
//	    iteration beads whose chain root points at a pruned ancestor (the gap),
//	    while the engine snapshot still claims 379 spawned. THE counter-drift
//	    case: the authoritative Iterations count is the live 60, the inflated 379
//	    surfaces ONLY as a framed engine snapshot, and the drift note appears.
//	(c) a clean single-node gardener — one iteration, an intact single-node chain,
//	    engine snapshot 1. The minimal case: no tail, no drift, singular wording.
//
// These render the REAL gardener_detail template + /runs partial through the
// handler (mkServerWithNow), so a template or handler regression — not just a
// pure-helper regression — fails CI.
// ===========================================================================

// goldenGardenerFixture wires a gardener trigger bead (spec) plus its iteration
// beads into a fresh fakeDaemon under id "gd-cap". The iterations are served
// verbatim from liveAgentsResp; gardenerIterations' dual-key client filter
// matches them by schedule_id (stamped by mkIteration).
func goldenGardenerFixture(t *testing.T, spec map[string]any, iters []*beadsapi.BeadDetail) *fakeDaemon {
	t.Helper()
	f := newFake()
	f.beads["gd-cap"] = goldenGardenerBead(t, "gd-cap", "Fixture gardener", spec)
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: iters, Total: len(iters)}
	return f
}

// fixture167HeadNoChain builds prod-shaped fixture (a): 167 iteration beads with
// NO schedule_prior_agent_id back-pointer (every run is a root, so the chain
// cannot be reconstructed and ordering falls to created_at "by time"), with an
// engine snapshot that agrees (RunsSpawned == RunsCompleted == 167).
func fixture167HeadNoChain() (spec map[string]any, iters []*beadsapi.BeadDetail) {
	const n = 167
	spec = map[string]any{
		"name":           "wide",
		"terminal_goal":  "fan out broadly",
		"runs_spawned":   n,
		"runs_completed": n,
	}
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	iters = make([]*beadsapi.BeadDetail, 0, n)
	for i := range n {
		// priorID "" => no chain back-pointer => a root; 167 roots => no unique
		// chain => created_at fallback (every card ByTime).
		iters = append(iters, mkIteration("gd-w"+itoa(i), "gd-cap", "", i+1, "closed", t0.Add(time.Duration(i)*time.Minute)))
	}
	return spec, iters
}

// fixture60PartialChainPrunedGap builds prod-shaped fixture (b): 60 retained
// iteration beads forming an internally-intact chain whose ROOT points back at a
// pruned ancestor ("gd-pruned-ancestor", not in the set) — the dangling
// predecessor models the 379-spawned / 60-retained pruned gap. The engine
// snapshot still advertises 379 spawned, so the live count (60) and the snapshot
// (379) diverge far beyond tolerance.
func fixture60PartialChainPrunedGap() (spec map[string]any, iters []*beadsapi.BeadDetail) {
	const live = 60
	const spawned = 379
	spec = map[string]any{
		"name":           "pruned",
		"terminal_goal":  "long-haul drainer",
		"runs_spawned":   spawned,
		"runs_completed": spawned - 9,
	}
	t0 := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	iters = make([]*beadsapi.BeadDetail, 0, live)
	prior := "gd-pruned-ancestor" // dangles outside the set => the pruned gap
	for i := range live {
		id := "gd-p" + itoa(i)
		iters = append(iters, mkIteration(id, "gd-cap", prior, i+1, "closed", t0.Add(time.Duration(i)*time.Minute)))
		prior = id
	}
	return spec, iters
}

// fixtureCleanSingleNode builds prod-shaped fixture (c): a single iteration that
// is its own intact chain (no prior, nothing points at it). Engine snapshot in
// agreement (1). No tail, no drift, singular "run" wording.
func fixtureCleanSingleNode() (spec map[string]any, iters []*beadsapi.BeadDetail) {
	spec = map[string]any{
		"name":           "solo",
		"terminal_goal":  "one and done",
		"runs_spawned":   1,
		"runs_completed": 1,
		"last_agent_id":  "gd-s1",
	}
	t0 := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	r1 := mkIteration("gd-s1", "gd-cap", "", 1, "closed", t0)
	r1.Fields["wrapup"] = `{"tldr":"shipped it"}`
	return spec, []*beadsapi.BeadDetail{r1}
}

// renderGardenerRunsTail renders GET /gardener/{id}/runs (the lazy older-runs
// partial) and returns the body, failing on any non-200.
// renderGardenerRunsTail renders the ENTIRE lazy older-runs tail by walking the
// stateless cursor chain: the /runs section is now paged (gardenerRunsPageSize
// cards per request, design doc §7/§8), each page emitting a revealed-once shell
// whose hx-get carries the next page's ?cursor=. This helper follows that chain
// from the first page to exhaustion and concatenates every page's HTML, so the
// invariant tests can still assert against the full rendered tail.
func renderGardenerRunsTail(t *testing.T, f *fakeDaemon, id string) string {
	t.Helper()
	mux := mkServerWithNow(t, f, gardenerPageNow)
	var combined strings.Builder
	// Start at the first tail page (no cursor); follow each page's next-page URL.
	url := "/gardener/" + id + "/runs"
	for hops := 0; ; hops++ {
		if hops > 1000 {
			t.Fatalf("gardener runs tail: cursor chain did not terminate after %d hops (id=%s)", hops, id)
		}
		req := httptest.NewRequest("GET", url, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d body=%s", url, w.Code, w.Body.String())
		}
		body := w.Body.String()
		combined.WriteString(body)
		next, ok := nextGardenerRunsURL(body)
		if !ok {
			break
		}
		url = next
	}
	return combined.String()
}

// nextGardenerRunsURL extracts the next-page hx-get URL from a rendered
// gardener_runs_section page, returning ok=false when the page emits no
// continuation shell (the tail is exhausted). It scopes the search to the
// gardener-runs-lazy shell so it never trips on an unrelated hx-get.
func nextGardenerRunsURL(body string) (string, bool) {
	_, rest, found := strings.Cut(body, `class="gardener-runs-lazy"`)
	if !found {
		return "", false
	}
	_, rest, found = strings.Cut(rest, `hx-get="`)
	if !found {
		return "", false
	}
	url, _, found := strings.Cut(rest, `"`)
	if !found {
		return "", false
	}
	return url, true
}

// goldenInvariantCase is one prod-shaped fixture, with the derived expectations
// the four invariants assert against.
type goldenInvariantCase struct {
	name          string
	spec          map[string]any
	iters         []*beadsapi.BeadDetail
	wantLive      int  // authoritative Iterations count == rendered cards (eager+tail)
	wantSpawned   int  // engine snapshot RunsSpawned (lifetime tally)
	wantCompleted int  // engine snapshot RunsCompleted (lifetime tally)
	wantPruned    bool // "older runs pruned" framing expected (spawned-live > gardenerPrunedGap)
}

// goldenInvariantCases assembles the three prod-shaped fixtures with their
// expected counts, shared by all four invariant tests.
func goldenInvariantCases() []goldenInvariantCase {
	aSpec, aIters := fixture167HeadNoChain()
	bSpec, bIters := fixture60PartialChainPrunedGap()
	cSpec, cIters := fixtureCleanSingleNode()
	return []goldenInvariantCase{
		{"167_head_no_chain", aSpec, aIters, len(aIters), 167, 167, false},
		{"60_iter_partial_chain_379_pruned_gap", bSpec, bIters, len(bIters), 379, 370, true},
		{"clean_single_node", cSpec, cIters, len(cIters), 1, 1, false},
	}
}

// renderedCardTotal renders the eager page AND the lazy /runs tail and returns
// the combined iteration-card count — the true number of run cards the page
// surfaces across both fragments. The eager page caps at gardenerEagerRuns and
// defers the rest to /runs, so neither fragment alone is the full thread.
func renderedCardTotal(t *testing.T, f *fakeDaemon, id string) int {
	t.Helper()
	eager := renderGardenerGolden(t, f, id)
	tail := renderGardenerRunsTail(t, f, id)
	return renderedIterationCards(eager) + renderedIterationCards(tail)
}

// TestGardenerGoldenInvariant_CounterConsistency pins counter_consistency: the
// authoritative Iterations count, the life-thread heading count, and the actual
// number of rendered run cards (eager head + lazy tail) all agree for every
// fixture; and the engine-snapshot drift annotation surfaces IFF the snapshot
// diverges from the live count beyond tolerance. This is the direct guard for
// the CDQR bug where the header counters were unreconcilable with the rendered
// thread (ka-0000005522).
func TestGardenerGoldenInvariant_CounterConsistency(t *testing.T) {
	for _, tc := range goldenInvariantCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := goldenGardenerFixture(t, tc.spec, tc.iters)
			eager := renderGardenerGolden(t, f, "gd-cap")

			// The actual rendered card count across both fragments equals the
			// authoritative live count — no card is silently dropped at scale.
			if total := renderedCardTotal(t, f, "gd-cap"); total != tc.wantLive {
				t.Errorf("rendered %d cards across eager+tail, want %d (live count)", total, tc.wantLive)
			}

			// The Iterations counter row shows the live count, full stop.
			if want := fmt.Sprintf("<dt>Iterations</dt><dd>%d", tc.wantLive); !strings.Contains(eager, want) {
				t.Errorf("Iterations row missing authoritative count %q", want)
			}

			// The life-thread heading echoes the same authoritative count,
			// labeled "iteration"/"iterations" — NEVER "runs". Labeling the
			// rendered iteration cards "runs" re-conflates the per-iteration
			// index with the engine's lifetime spawn tally, the exact
			// contradiction this page's Iterations-vs-Engine-count split kills
			// (bead ka-0000005522).
			suffix := "iterations)"
			if tc.wantLive == 1 {
				suffix = "iteration)"
			}
			if want := fmt.Sprintf("(%d %s", tc.wantLive, suffix); !strings.Contains(eager, want) {
				t.Errorf("life-thread heading missing %q", want)
			}

			// The engine lifetime tally is ALWAYS rendered as a framed secondary
			// line carrying both numbers verbatim — never folded into the live
			// count above. When the snapshot outruns the live count beyond the
			// prune gap, the line additionally carries the "(older runs pruned)"
			// explanation (the re-expressed counter-drift signal: design doc
			// ka-0000005648 §3/§8 — the legacy "counter drift" warning was
			// replaced by this pruned-aware engine-count framing).
			engineLine := fmt.Sprintf("%d spawned / %d completed", tc.wantSpawned, tc.wantCompleted)
			if tc.wantPruned {
				engineLine += " (older runs pruned)"
			}
			if !strings.Contains(eager, engineLine) {
				t.Errorf("engine-count line missing honest both-numbers text %q", engineLine)
			}
			gotPruned := strings.Contains(eager, "(older runs pruned)")
			if gotPruned != tc.wantPruned {
				t.Errorf("pruned framing present = %v, want %v (spawned=%d live=%d)",
					gotPruned, tc.wantPruned, tc.wantSpawned, tc.wantLive)
			}
		})
	}
}

// TestGardenerGoldenInvariant_BoundedClaim pins bounded_claim: the page's
// AUTHORITATIVE run count (the Iterations counter row + the life-thread heading)
// never exceeds the number of run cards the thread can actually render — it is
// always exactly the live count, never the (potentially inflated) engine
// snapshot. For the pruned-gap fixture this means the page shows 60, not 379,
// as the primary count. Guards the "header advertises 439, thread shows 13"
// over-claim from the CDQR audit.
func TestGardenerGoldenInvariant_BoundedClaim(t *testing.T) {
	for _, tc := range goldenInvariantCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := goldenGardenerFixture(t, tc.spec, tc.iters)
			eager := renderGardenerGolden(t, f, "gd-cap")

			// The authoritative count is bounded by what is actually rendered.
			total := renderedCardTotal(t, f, "gd-cap")
			if total != tc.wantLive {
				t.Fatalf("rendered card total %d != live count %d", total, tc.wantLive)
			}

			// The Iterations row and life-thread heading both show the live
			// (bounded) count, not an inflated engine number.
			if want := fmt.Sprintf("<dt>Iterations</dt><dd>%d", tc.wantLive); !strings.Contains(eager, want) {
				t.Errorf("Iterations row should show bounded live count %q", want)
			}

			// When the engine snapshot is inflated (drift case), that number must
			// NOT appear as the authoritative Iterations count or the heading run
			// count — it is allowed ONLY in the framed engine-snapshot row.
			if tc.wantSpawned != tc.wantLive {
				if bad := fmt.Sprintf("<dt>Iterations</dt><dd>%d", tc.wantSpawned); strings.Contains(eager, bad) {
					t.Errorf("inflated engine count %d leaked into the authoritative Iterations row", tc.wantSpawned)
				}
				if bad := fmt.Sprintf("(%d runs)", tc.wantSpawned); strings.Contains(eager, bad) {
					t.Errorf("inflated engine count %d leaked into the life-thread heading", tc.wantSpawned)
				}
			}
		})
	}
}

// TestGardenerGoldenInvariant_NoEmptyTail pins no_empty_tail: the lazy /runs
// section never renders a literal "No older runs." dead-end (which reads like an
// error). When a tail exists it renders the older-runs list; when none exists it
// renders nothing, so the htmx swap simply removes the "Loading older runs…"
// shell. Guards the CDQR finding that /runs returned "No older runs." (35 bytes)
// despite hundreds of spawned runs (ka-0000005522, gd-5nBd9Vukd6).
func TestGardenerGoldenInvariant_NoEmptyTail(t *testing.T) {
	for _, tc := range goldenInvariantCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := goldenGardenerFixture(t, tc.spec, tc.iters)
			tail := renderGardenerRunsTail(t, f, "gd-cap")

			// The dead-end string never appears, tail or no tail.
			if strings.Contains(tail, "No older runs") {
				t.Errorf("lazy /runs rendered a 'No older runs.' dead-end: %q", tail)
			}

			hasTail := tc.wantLive > gardenerEagerRuns
			gotList := strings.Contains(tail, "gardener-thread-older")
			if gotList != hasTail {
				t.Errorf("older-runs list present = %v, want %v (live=%d eager-cap=%d)",
					gotList, hasTail, tc.wantLive, gardenerEagerRuns)
			}
		})
	}
}

// TestGardenerGoldenInvariant_EngineCountFramed pins engine_count_framed: the
// engine-owned lifetime tally (RunsSpawned / RunsCompleted) is ALWAYS rendered
// as a single secondary "Engine count" line, explicitly framed as a "lifetime
// tally" and styled with gardener-stat-secondary — never as a bare number
// co-equal with the live Iterations count. This is what lets the inflated 379
// coexist honestly with the authoritative live 60 instead of contradicting it.
func TestGardenerGoldenInvariant_EngineCountFramed(t *testing.T) {
	for _, tc := range goldenInvariantCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := goldenGardenerFixture(t, tc.spec, tc.iters)
			eager := renderGardenerGolden(t, f, "gd-cap")

			// The engine line carries the "lifetime tally" framing label and the
			// gardener-stat-secondary styling that demotes it below the live count.
			if want := `Engine count <span class="meta gardener-stat-source">lifetime tally</span>`; !strings.Contains(eager, want) {
				t.Errorf("engine counter not framed as lifetime tally: missing %q", want)
			}
			if !strings.Contains(eager, "gardener-stat-secondary") {
				t.Errorf("engine count not styled as secondary (missing gardener-stat-secondary)")
			}

			// The framed line carries BOTH lifetime numbers verbatim, confirming
			// the engine tally has a home that is NOT the authoritative Iterations
			// count. When the snapshot outruns the live count beyond the prune gap
			// it is additionally framed "(older runs pruned)" and styled pruned.
			engineLine := fmt.Sprintf("%d spawned / %d completed", tc.wantSpawned, tc.wantCompleted)
			if tc.wantPruned {
				engineLine += " (older runs pruned)"
				if !strings.Contains(eager, "gardener-stat-pruned") {
					t.Errorf("pruned engine count not styled as pruned (missing gardener-stat-pruned)")
				}
			}
			if !strings.Contains(eager, engineLine) {
				t.Errorf("engine snapshot line %q not rendered", engineLine)
			}
		})
	}
}

// TestGardenerDetailGolden_ConnectedBeadsAndRailNoOverflow is the static
// guardrail for the narrow-viewport / rail-clipping fix (bug ka-0000006828). It
// asserts on the rendered <style>, so an edit that drops any of these rules —
// re-introducing the 430px→706px horizontal scroll or the rail chip clipping —
// fails the build.
//
// Roots the fix guards:
//  1. The grouped Connected-beads list (.cap-beads-node-link) previously carried
//     NO CSS, so one bead title/id/branch-ref with an unbreakable token forced
//     the <a> past the viewport and the page overflowed horizontally on mobile.
//     The link must be a shrinkable, truncating flex child (min-width:0 +
//     overflow:hidden + text-overflow:ellipsis), like the dense view already is.
//  2. The right-rail cadence chip and Trajectory verdict pill clipped at the
//     narrow rail column's right edge because .gardener-verdict is nowrap and
//     .gardener-cadence is a flex-shrink:0 chip. Both must be allowed to wrap /
//     shrink within the column.
func TestGardenerDetailGolden_ConnectedBeadsAndRailNoOverflow(t *testing.T) {
	f := newFake()
	// A scheduled gardener with a timezone (so the cadence chip renders the long
	// "America/Los_Angeles" token) and progress data (so the Trajectory verdict
	// pill renders). The connected-beads panel itself lazy-loads via htmx, but
	// its grouped-list CSS lives in the same page <style> and is asserted here.
	cap := goldenGardenerBead(t, "gd-cap", "Delivery gardener", map[string]any{
		"name":           "delivery",
		"terminal_goal":  "drain the ready queue",
		"runs_spawned":   3,
		"runs_completed": 3,
		"progress_state": map[string]any{
			"runs": []map[string]any{
				{"agent_id": "a1", "progress_score": 2, "agent_state": "done"},
			},
			"events": []map[string]any{
				{"kind": "child_closed", "target_id": "gd-c1", "occurred_at": "2026-06-01T10:05:00Z"},
			},
		},
	})
	cap.Fields["cron"] = "0 */2 * * *"
	cap.Fields["timezone"] = "America/Los_Angeles"
	f.beads["gd-cap"] = cap
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}

	body := renderGardenerGolden(t, f, "gd-cap")

	// (1) Grouped connected-beads link must be a truncating (ellipsis) flex child.
	// The unbreakable-token overflow is killed by overflow:hidden + ellipsis on
	// the link, combined with the shrinkable row/list track asserted below.
	linkRule := sliceAfter(body, ".cap-beads-node-link {")
	if i := strings.Index(linkRule, "}"); i >= 0 {
		linkRule = linkRule[:i]
	}
	if linkRule == "" {
		t.Fatal(".cap-beads-node-link rule not found in rendered gardener page — the " +
			"grouped Connected-beads list is unstyled again and will overflow narrow " +
			"viewports (bug ka-0000006828)")
	}
	for _, want := range []string{"text-overflow: ellipsis;", "overflow: hidden;"} {
		if !strings.Contains(linkRule, want) {
			t.Errorf(".cap-beads-node-link rule missing %q — an unbreakable bead "+
				"title/id/branch-ref will force 430px→706px horizontal page scroll "+
				"(bug ka-0000006828)", want)
		}
	}
	// The list container and the row must let the track floor collapse
	// (min-width:0) so the truncating link actually has room to shrink into. The
	// row also wraps so the fixed-width id/status pill never crush the title to
	// zero (or clip inside the card) on a very narrow column / larger-font skin.
	listRule := sliceAfter(body, ".cap-beads-group-list {")
	if i := strings.Index(listRule, "}"); i >= 0 {
		listRule = listRule[:i]
	}
	if !strings.Contains(listRule, "min-width: 0;") {
		t.Error(".cap-beads-group-list is missing min-width: 0; — the flex/grid track " +
			"floors at content width and the ellipsis child cannot shrink (bug ka-0000006828)")
	}
	nodeRule := sliceAfter(body, ".cap-beads-node {")
	if i := strings.Index(nodeRule, "}"); i >= 0 {
		nodeRule = nodeRule[:i]
	}
	for _, want := range []string{"flex-wrap: wrap;", "min-width: 0;"} {
		if !strings.Contains(nodeRule, want) {
			t.Errorf(".cap-beads-node rule missing %q — the fixed-width id/status pill "+
				"crush the title to zero (or clip inside the card) instead of wrapping "+
				"(bug ka-0000006828)", want)
		}
	}

	// (2) Rail chip clipping: the cadence chip and Trajectory verdict pill must be
	// allowed to wrap/shrink within the narrow rail column.
	cadenceRule := sliceAfter(body, ".gardener-schedule .gardener-cadence {")
	if i := strings.Index(cadenceRule, "}"); i >= 0 {
		cadenceRule = cadenceRule[:i]
	}
	if cadenceRule == "" {
		t.Fatal(".gardener-schedule .gardener-cadence rule not found — the cadence chip " +
			"will clip its timezone at the rail column right edge (bug ka-0000006828)")
	}
	for _, want := range []string{"min-width: 0;", "overflow-wrap: anywhere;"} {
		if !strings.Contains(cadenceRule, want) {
			t.Errorf(".gardener-cadence rail rule missing %q — the cadence chip clips its "+
				"timezone at the column edge (bug ka-0000006828)", want)
		}
	}
	verdictRule := sliceAfter(body, ".gardener-trajectory-head .gardener-verdict {")
	if i := strings.Index(verdictRule, "}"); i >= 0 {
		verdictRule = verdictRule[:i]
	}
	if !strings.Contains(verdictRule, "white-space: normal;") {
		t.Error(".gardener-trajectory-head .gardener-verdict is missing white-space: normal; — " +
			"the verdict pill stays nowrap and clips its detail tail in the narrow rail " +
			"(bug ka-0000006828)")
	}
}

// TestGardenerDetailGolden_ConnectedBeadsBelowLifeThread pins the density fix from
// bug ka-0000007518: the Connected-beads panel must render BELOW the Life-thread
// heading, not above it. The panel was stacked above the runs, pushing the
// gardener-health content (goal, counters, run history — the page's actual
// purpose) down a 33k-39k px page and scoring CDQR Density 6/10 for three
// iterations. Gardener-health leads; the exploratory graph is reference material
// that follows.
func TestGardenerDetailGolden_ConnectedBeadsBelowLifeThread(t *testing.T) {
	f := seedGardenerPageFake(t)

	body := renderGardenerGolden(t, f, "gd-cap")

	threadIdx := strings.Index(body, `class="gardener-thread-heading"`)
	connectedIdx := strings.Index(body, `class="gardener-connected-heading"`)
	if threadIdx < 0 {
		t.Fatal("Life-thread heading not rendered")
	}
	if connectedIdx < 0 {
		t.Fatal("Connected-beads heading not rendered")
	}
	if threadIdx > connectedIdx {
		t.Errorf("Life-thread heading (idx %d) must precede the Connected-beads heading (idx %d) — "+
			"the exploratory graph was stacked above the gardener's own run history again (bug ka-0000007518)",
			threadIdx, connectedIdx)
	}
}
