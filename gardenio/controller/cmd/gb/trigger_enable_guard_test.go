package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
)

// Bug ka-0000005774: `gb trigger enable` on a gardener trigger whose hard_stop
// wall_clock window has ALREADY expired used to succeed straight into an
// immediate engine re-close with 0 runs — a silent no-op that reverted the
// operator's revive. These tests pin the guard: refuse by default, offer
// --rebaseline (reset the window, then enable) and --force (enable as-is).

// expiredGardenerTrigger builds a gardener trigger created before the grandfather
// epoch with a bounded 168h hard_stop wall_clock — the exact
// pre-epoch/short-window shape whose floored baseline is long expired.
func expiredGardenerTrigger(id string) map[string]any {
	return makeGardenerTriggerCreatedAt(id, "gd-EPIC1", "2026-05-31T00:00:00Z", map[string]any{
		"enabled":  "false", // open+disabled: the "one enable away" cohort
		"gardener": boundedGardenerBlob("168h"),
	})
}

func TestTriggerEnable_ExpiredWallClock_RefusedByDefault(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC))
	stub := newGardenerStub()
	stub.addBead(expiredGardenerTrigger("gd-CAP1"))
	setupGardenerStub(t, stub)

	err := setTriggerEnabled(context.Background(), "gd-CAP1", true, triggerEnableOptions{})
	if err == nil {
		t.Fatal("expected refusal enabling a gardener with an expired wall_clock window")
	}
	for _, want := range []string{"EXPIRED", "--rebaseline", "--force", "ka-0000005774"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message missing %q:\n%s", want, err.Error())
		}
	}
	// The trigger must NOT have been enabled (no PATCH setting enabled=true).
	for _, p := range stub.patches["gd-CAP1"] {
		if fields, ok := p["fields"].(map[string]any); ok {
			if fields["enabled"] == true || fields["enabled"] == "true" {
				t.Fatalf("refused enable must not flip enabled=true, but a PATCH did: %+v", p)
			}
		}
	}
}

func TestTriggerEnable_ExpiredWallClock_Rebaseline(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC))
	stub := newGardenerStub()
	stub.addBead(expiredGardenerTrigger("gd-CAP1"))
	setupGardenerStub(t, stub)

	before := time.Now()
	if err := setTriggerEnabled(context.Background(), "gd-CAP1", true, triggerEnableOptions{rebaseline: true}); err != nil {
		t.Fatalf("--rebaseline enable should succeed: %v", err)
	}

	// The gardener blob must now carry a wall_clock_since stamped to ~now, written
	// THROUGH the blob (not a flat field — ka-0000004562).
	var sawBlobWrite, sawEnable bool
	var since time.Time
	for _, p := range stub.patches["gd-CAP1"] {
		fields, _ := p["fields"].(map[string]any)
		if raw, ok := fields["gardener"].(string); ok {
			var spec bridge.GardenerSpec
			if err := json.Unmarshal([]byte(raw), &spec); err != nil {
				t.Fatalf("gardener blob write is not valid JSON: %v", err)
			}
			if spec.WallClockSince.IsZero() {
				t.Fatal("rebaseline must stamp wall_clock_since in the blob")
			}
			sawBlobWrite = true
			since = spec.WallClockSince
		}
		if fields["enabled"] == true || fields["enabled"] == "true" {
			sawEnable = true
		}
	}
	if !sawBlobWrite {
		t.Fatal("expected a gardener-blob PATCH carrying wall_clock_since")
	}
	if !sawEnable {
		t.Fatal("expected the trigger to be enabled after rebaselining")
	}
	if since.Before(before.Add(-time.Minute)) {
		t.Errorf("wall_clock_since %v is not ~now (%v)", since, before)
	}

	// A rebaselining comment must be left explaining the reset.
	var sawComment bool
	for _, c := range stub.comments["gd-CAP1"] {
		if txt, ok := c["text"].(string); ok && strings.Contains(txt, "wall_clock baseline reset") {
			sawComment = true
		}
	}
	if !sawComment {
		t.Error("expected a wall_clock-reset comment on the bead")
	}
}

// After a rebaseline, the SAME expiry check the guard runs must report the
// window as OPEN (not expired) — otherwise the guard would refuse the revive it
// just performed. This is the WallClockSince-awareness added to
// gardenerWallClockExpiryStatus (ka-0000005774 / ka-0000006465).
func TestTriggerEnable_RebaselinedGardener_NotExpired(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC))
	var spec bridge.GardenerSpec
	if err := json.Unmarshal([]byte(boundedGardenerBlob("168h")), &spec); err != nil {
		t.Fatal(err)
	}
	spec.WallClockSince = time.Now()
	raw, _ := json.Marshal(&spec)

	bead := makeGardenerTriggerCreatedAt("gd-CAP1", "gd-EPIC1", "2026-05-31T00:00:00Z", map[string]any{
		"gardener": string(raw),
	})
	// Round-trip through the daemon so CreatedAt is parsed like production.
	stub := newGardenerStub()
	stub.addBead(bead)
	setupGardenerStub(t, stub)
	got, err := daemon.GetBead(context.Background(), "gd-CAP1")
	if err != nil {
		t.Fatal(err)
	}
	view := gardenerWallClockExpiryStatus(got, time.Now())
	if !view.present {
		t.Fatal("expected a present expiry readout")
	}
	if view.expired {
		t.Errorf("a gardener rebaselined to now must NOT read expired (expiry=%v)", view.expiry)
	}
	if view.grandfathered {
		t.Errorf("a post-epoch wall_clock_since must not be flagged grandfathered (baseline=%v)", view.baseline)
	}
	if !view.expiry.After(time.Now()) {
		t.Errorf("rebaselined expiry %v must be in the future", view.expiry)
	}
}

func TestTriggerEnable_ExpiredWallClock_Force(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC))
	stub := newGardenerStub()
	stub.addBead(expiredGardenerTrigger("gd-CAP1"))
	setupGardenerStub(t, stub)

	if err := setTriggerEnabled(context.Background(), "gd-CAP1", true, triggerEnableOptions{force: true}); err != nil {
		t.Fatalf("--force enable should succeed (as-is): %v", err)
	}
	// --force must NOT rewrite the blob (no rebaseline), only flip enabled.
	var sawEnable, sawBlobWrite bool
	for _, p := range stub.patches["gd-CAP1"] {
		fields, _ := p["fields"].(map[string]any)
		if _, ok := fields["gardener"]; ok {
			sawBlobWrite = true
		}
		if fields["enabled"] == true || fields["enabled"] == "true" {
			sawEnable = true
		}
	}
	if sawBlobWrite {
		t.Error("--force must not rewrite the gardener blob (that is --rebaseline's job)")
	}
	if !sawEnable {
		t.Error("--force must still enable the trigger")
	}
}

func TestTriggerEnable_NonGardener_PassesThrough(t *testing.T) {
	stub := newGardenerStub()
	// A plain (non-gardener) trigger bead — no `gardener` blob field.
	stub.addBead(map[string]any{
		"id": "gd-T1", "title": "webhook trigger", "type": "trigger", "kind": "data",
		"status": "open", "fields": map[string]any{"enabled": "false", "source": "webhook"},
		"labels": []any{"trigger:webhook"},
	})
	setupGardenerStub(t, stub)

	if err := setTriggerEnabled(context.Background(), "gd-T1", true, triggerEnableOptions{}); err != nil {
		t.Fatalf("non-gardener enable must pass straight through: %v", err)
	}
	var sawEnable bool
	for _, p := range stub.patches["gd-T1"] {
		if fields, ok := p["fields"].(map[string]any); ok && (fields["enabled"] == true || fields["enabled"] == "true") {
			sawEnable = true
		}
	}
	if !sawEnable {
		t.Error("non-gardener trigger must be enabled")
	}
}

func TestTriggerEnable_LiveWindow_PassesThrough(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC))
	stub := newGardenerStub()
	// Created recently with a long (720h) window → still open, must enable clean.
	created := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	stub.addBead(makeGardenerTriggerCreatedAt("gd-CAP1", "gd-EPIC1", created, map[string]any{
		"enabled":  "false",
		"gardener": boundedGardenerBlob("720h"),
	}))
	setupGardenerStub(t, stub)

	if err := setTriggerEnabled(context.Background(), "gd-CAP1", true, triggerEnableOptions{}); err != nil {
		t.Fatalf("a gardener with a still-open window must enable without the guard tripping: %v", err)
	}
	var sawEnable bool
	for _, p := range stub.patches["gd-CAP1"] {
		if fields, ok := p["fields"].(map[string]any); ok && (fields["enabled"] == true || fields["enabled"] == "true") {
			sawEnable = true
		}
	}
	if !sawEnable {
		t.Error("live-window gardener must be enabled")
	}
}

// Disable is never guarded — an expired gardener must still be disable-able.
func TestTriggerDisable_ExpiredWallClock_Unguarded(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC))
	stub := newGardenerStub()
	trig := expiredGardenerTrigger("gd-CAP1")
	trig["fields"].(map[string]any)["enabled"] = "true"
	stub.addBead(trig)
	setupGardenerStub(t, stub)

	if err := setTriggerEnabled(context.Background(), "gd-CAP1", false, triggerEnableOptions{}); err != nil {
		t.Fatalf("disable must never be blocked by the expired-wall_clock guard: %v", err)
	}
}
