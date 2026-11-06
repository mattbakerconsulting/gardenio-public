package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// plans_next_fire_test.go — iter-56 (ka-0000005352): the forward-looking
// "· fires in 12m" next-fire facet on an ARMED-IDLE cron gardener's plan-agent
// chip. It is the temporal INVERSE of the whole backward "how long since"
// wait-age family every other card signal already carries (decision "· 32d",
// escalation "· 40d", stall "· 3d", stuck/queued crew "· 6h"/"· 12m", MR "· 8d",
// now-step "· 5d", ready "· 4d"): those all answer "how long has this waited",
// while an armed-idle cron gardener's card answered NEITHER "how long idle" nor
// "when does the schedule next move the plan".
//
// The chip reads "idle" when a running loop has no live iteration (planAgentChip
// re-labels a running·parked/none loop to the calm "idle" so green never
// over-claims activity). That is honest but silent about WHEN the cron next
// fires. setNextFire folds the countdown on, gated on Tone=="idle" &&
// FiringMode=="cron", computed off the SAME computeNextFire2 + fmtTimeUntil the
// /triggers next-fire column uses. Display-only, zero extra daemon call (the
// cron/tz ride the gardener bead the firing-mode glyph already read).

// --- unit truth-table: planAgentChipView.setNextFire -------------------------

// TestPlanAgentChip_SetNextFire is the pure crux: the facet is populated iff the
// chip is idle-tone AND cron-fired AND the cron parses; every other combination
// leaves NextFire empty (the chip reads exactly as before). The clock is pinned
// so the countdown is deterministic — cron "7,37 * * * *" from 12:00:00 UTC next
// fires at 12:07, i.e. "in 7m".
func TestPlanAgentChip_SetNextFire(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	const cron = "7,37 * * * *"
	cases := []struct {
		name       string
		tone       string
		firingMode string
		cron       string
		tz         string
		wantNext   string
	}{
		// The reconcile: idle + cron + a parseable spec → the countdown.
		{"idle cron → facet", "idle", "cron", cron, "", "in 7m"},
		// A timezone is honoured (America/Los_Angeles is UTC-7 in July, but the
		// cron fires every hour at :07/:37 in LOCAL time, which for a minute-only
		// cron lands at the same wall-minute → still "in 7m" from :00).
		{"idle cron with tz → facet", "idle", "cron", cron, "America/Los_Angeles", "in 7m"},
		// Not idle → no facet (a running·live loop is already working; paused /
		// retired / none loops are not scheduled to fire).
		{"running·live cron → none", "running", "cron", cron, "", ""},
		{"paused cron → none", "paused", "cron", cron, "", ""},
		{"retired cron → none", "retired", "cron", cron, "", ""},
		{"none cron → none", "none", "cron", cron, "", ""},
		// Idle but NOT cron-fired → no facet (an event gardener fires on subtree
		// activity, not a clock — no meaningful countdown).
		{"idle event → none", "idle", "event", "", "", ""},
		{"idle no-mode → none", "idle", "", "", "", ""},
		// Idle + cron but an EMPTY / UNPARSEABLE spec → no facet (reads as before).
		{"idle cron empty spec → none", "idle", "cron", "", "", ""},
		{"idle cron bad spec → none", "idle", "cron", "not a cron", "", ""},
		{"idle cron bad tz → none", "idle", "cron", cron, "Mars/Phobos", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := planAgentChipView{Label: "idle", Tone: c.tone, FiringMode: c.firingMode, Title: "t"}
			got := in.setNextFire(c.cron, c.tz, now)
			if got.NextFire != c.wantNext {
				t.Errorf("setNextFire(tone=%s mode=%s cron=%q).NextFire = %q, want %q",
					c.tone, c.firingMode, c.cron, got.NextFire, c.wantNext)
			}
			// Everything else must survive — setNextFire touches only NextFire.
			if got.Label != "idle" || got.Tone != c.tone || got.FiringMode != c.firingMode || got.Title != "t" {
				t.Errorf("setNextFire mutated a non-NextFire field: %+v", got)
			}
		})
	}
}

// --- enrich: the facet appears on the real row (index seam) -------------------

// TestPlanIndexEnrich_IdleCronGardenerNextFire proves the index enrich pass folds
// the countdown onto a running-but-idle cron gardener's chip. seedPlanGardener
// binds an ENABLED gardener with no live assistant → the chip re-labels to "idle";
// giving it the staggered default cron makes it clock-fired, so setNextFire fires.
// The enrich clock is planAssistantTestNow (2026-07-05 12:00 UTC); cron
// "7,37 * * * *" next fires 12:07 → "in 7m".
func TestPlanIndexEnrich_IdleCronGardenerNextFire(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true").Fields["cron"] = "7,37 * * * *"

	row := enrichRow(t, f)
	// Precondition: the chip is armed-idle cron.
	if row.Chip.Tone != "idle" {
		t.Fatalf("precondition: Chip.Tone = %q, want idle (a running gardener with no live iteration)", row.Chip.Tone)
	}
	if row.Chip.FiringMode != "cron" {
		t.Fatalf("precondition: Chip.FiringMode = %q, want cron", row.Chip.FiringMode)
	}
	if row.Chip.NextFire != "in 7m" {
		t.Errorf("Chip.NextFire = %q, want %q", row.Chip.NextFire, "in 7m")
	}
}

// TestPlanIndexEnrich_NextFireOnlyIdleCron is the anti-over-reach guard: the facet
// is absent for every chip that is not armed-idle cron — a running·live loop
// (already working), a paused loop (not scheduled), and an event gardener (no
// clock). Each must leave NextFire empty.
func TestPlanIndexEnrich_NextFireOnlyIdleCron(t *testing.T) {
	t.Run("running·live cron → no facet", func(t *testing.T) {
		f := seedPlansFake()
		seedPlanGardener(f, "gd-cap1", "gd-plan1", "true").Fields["cron"] = "7,37 * * * *"
		// A live assistant flips the chip off "idle" to "running · live".
		seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
		row := enrichRow(t, f)
		if row.Chip.Tone != "running" {
			t.Fatalf("precondition: Chip.Tone = %q, want running (live assistant)", row.Chip.Tone)
		}
		if row.Chip.NextFire != "" {
			t.Errorf("a working (running·live) gardener must carry NO next-fire facet; got %q", row.Chip.NextFire)
		}
	})
	t.Run("paused cron → no facet", func(t *testing.T) {
		f := seedPlansFake()
		seedPlanGardener(f, "gd-cap1", "gd-plan1", "false").Fields["cron"] = "7,37 * * * *"
		row := enrichRow(t, f)
		if row.Chip.Tone != "paused" {
			t.Fatalf("precondition: Chip.Tone = %q, want paused", row.Chip.Tone)
		}
		if row.Chip.NextFire != "" {
			t.Errorf("a paused gardener is not scheduled to fire — no facet; got %q", row.Chip.NextFire)
		}
	})
	t.Run("idle event → no facet", func(t *testing.T) {
		f := seedPlansFake()
		seedPlanGardenerEvent(f, "gd-cap1", "gd-plan1", "true")
		row := enrichRow(t, f)
		if row.Chip.FiringMode != "event" {
			t.Fatalf("precondition: Chip.FiringMode = %q, want event", row.Chip.FiringMode)
		}
		if row.Chip.NextFire != "" {
			t.Errorf("an event gardener fires on activity, not a clock — no countdown; got %q", row.Chip.NextFire)
		}
	})
	t.Run("gardener-less plan → no facet", func(t *testing.T) {
		f := seedPlansFake()
		row := enrichRow(t, f)
		if row.Chip.NextFire != "" {
			t.Errorf("a gardener-less plan has no schedule — no facet; got %q", row.Chip.NextFire)
		}
	})
}

// --- HTTP render: fail-without / pass-with -----------------------------------

// TestPlansIndex_IdleCronGardenerRendersNextFire is the render regression: the
// armed-idle cron gardener's card renders the "· fires in Xm" span. Reverting the
// enrich seam (dropping .setNextFire) or the template gate reds this. The seed
// plan is in_progress; binding an enabled cron gardener with no live assistant
// yields the armed-idle chip.
func TestPlansIndex_IdleCronGardenerRendersNextFire(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true").Fields["cron"] = "7,37 * * * *"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-agent-chip-next"`) {
		t.Errorf("armed-idle cron card must render the next-fire span; body=%s", snippetN(body, 900))
	}
	if !strings.Contains(body, "next scheduled run is in 7m") {
		t.Errorf("next-fire span must carry the countdown tooltip 'next scheduled run is in 7m'; body=%s", snippetN(body, 900))
	}
	if !strings.Contains(body, " · in 7m<") {
		t.Errorf("next-fire face must read ' · in 7m'; body=%s", snippetN(body, 900))
	}
}

// TestPlansIndex_PausedGardenerNoNextFire is the render anti-over-reach: a paused
// gardener's card must render NO next-fire span (it is not scheduled to fire).
func TestPlansIndex_PausedGardenerNoNextFire(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false").Fields["cron"] = "7,37 * * * *"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `class="plan-agent-chip-next"`) {
		t.Errorf("a paused gardener's card must render no next-fire span")
	}
}

// --- index ↔ cockpit parity --------------------------------------------------

// TestPlanNextFire_IndexCockpitAgree proves the SAME facet renders on both the
// /plans index enrich seam AND the /plans/{id} cockpit chip-build seam — the chip
// is shared, so the two must carry the identical NextFire. Both are computed off
// the gardener bead's cron/tz + the pinned clock, so they agree by construction.
func TestPlanNextFire_IndexCockpitAgree(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true").Fields["cron"] = "7,37 * * * *"

	// Index side.
	indexNext := enrichRow(t, f).Chip.NextFire
	if indexNext != "in 7m" {
		t.Fatalf("index Chip.NextFire = %q, want %q", indexNext, "in 7m")
	}

	// Cockpit side: planAgentPanelData builds the header chip off the same clock.
	s := planAssistantTestServer(f)
	panel := s.planAgentPanelData(context.Background(), "gd-plan1", true, "", planDeliveryNone, "")
	chip, ok := panel["Chip"].(planAgentChipView)
	if !ok {
		t.Fatalf("cockpit panel Chip missing or wrong type: %T", panel["Chip"])
	}
	if chip.NextFire != indexNext {
		t.Errorf("cockpit Chip.NextFire = %q, want %q (index↔cockpit must agree)", chip.NextFire, indexNext)
	}
}
