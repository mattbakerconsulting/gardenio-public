package main

import (
	"encoding/json"
	"testing"
)

// The two verbatim fixtures captured from the live fleet (ka-0000008239):
//   - gd-iAQLfab3JC: the resumer-watchdog per-sweep status squawk (type=message,
//     label squawk) — a pure zero-delta "Resume sweep <ts>: 0 stuck … No re-arm/
//     escalate — dormant sweep, progress score 0" recap.
//   - gd-m5992Q4Esp: its paired "created" scorecard task (type=task) titled
//     exactly "Resume sweep <ts>" with the enumeration table in its description.
//
// Both are standing-state noise — the safety-net sweep ran and nothing changed —
// so both are dropped from /feed, the same structural treatment as gardener
// cycle-tick squawks and CDQR/GRADE scorecard steps.
const (
	resumeSweepSquawkTitle    = "Resume sweep 2026-06-19 07:44 UTC: 0 stuck gardeners. 20 triggers enumerated (10 LIVE+armed, gd-AaXgQcoFwX dormant-by-design [chains_to closed-epic], ka-0000007674 legit-disabled [live sibling], 8 closed-terminal), 0 gardener-stuck-escalate, 0 live agents. No re-arm/escalate — dormant sweep, progress score 0. Scorecard: gd-m5992Q4Esp."
	resumeSweepScorecardTitle = "Resume sweep 2026-06-19 07:44 UTC"
)

// TestFeedSquawkResumeSweepTick covers the resumer-watchdog "Resume sweep <ts>:
// …" zero-delta status squawk (ka-0000008239). A dormant sweep is dropped; a
// sweep naming a concrete action (re-arm / rescue / reopen / escalate) is
// retained.
func TestFeedSquawkResumeSweepTick(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		// --- DROP: the verbatim live zero-delta dormant sweep (gd-iAQLfab3JC) ---
		{"verbatim dormant sweep", resumeSweepSquawkTitle, true},
		// --- DROP: other zero-delta phrasings the sweep can take ---
		{"all-zero terse", "Resume sweep 2026-06-19 09:44 UTC: 0 stuck, 0 gardener-stuck-escalate, 0 live agents. No re-arm/escalate — dormant sweep, progress score 0.", true},
		// --- RETAIN: a sweep that actually took action this cycle ---
		{"re-armed a stuck gardener", "Resume sweep 2026-06-19 11:44 UTC: 1 stuck gardener re-armed (gd-abc123), progress score 1.", false},
		{"reopened beads", "Resume sweep 2026-06-19 11:44 UTC: reopened 3 abandoned children, rescued crew on gd-xyz.", false},
		{"escalated a bead", "Resume sweep 2026-06-19 11:44 UTC: 0 stuck but escalated ka-0000007674 to operator (dark > 2h).", false},
		{"respawned a gardener", "Resume sweep 2026-06-19 11:44 UTC: gardener gd-45aY6VxSch dark — respawned.", false},
		// --- IGNORE: not a resume-sweep squawk at all ---
		{"prose mentioning a sweep", "Resume-latency watchdog: all green this sweep, no stuck gardeners", false},
		{"unrelated squawk", "release-owner: cut 2026.169.11", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &feedEventBead{Type: "message", Title: c.title, Labels: []string{"squawk"}}
			if got := feedSquawkResumeSweepTick(b); got != c.want {
				t.Errorf("feedSquawkResumeSweepTick(%q) = %v, want %v", c.title, got, c.want)
			}
		})
	}
	if feedSquawkResumeSweepTick(nil) {
		t.Errorf("feedSquawkResumeSweepTick(nil) = true, want false")
	}
}

// TestFeedResumeSweepScorecard covers the paired scorecard task (gd-m5992Q4Esp):
// a type=task titled EXACTLY "Resume sweep <ts>" (no ": …" body) is dropped; the
// squawk recap (which carries the body) and unrelated tasks are not.
func TestFeedResumeSweepScorecard(t *testing.T) {
	cases := []struct {
		name string
		bead *feedEventBead
		want bool
	}{
		// DROP: the verbatim bare-header scorecard task (gd-m5992Q4Esp).
		{"verbatim scorecard task", &feedEventBead{ID: "gd-m5992Q4Esp", Type: "task", Title: resumeSweepScorecardTitle}, true},
		// IGNORE: the squawk recap is a message, not a task, and carries a body.
		{"squawk recap is not a task", &feedEventBead{ID: "gd-iAQLfab3JC", Type: "message", Title: resumeSweepSquawkTitle, Labels: []string{"squawk"}}, false},
		// IGNORE: a bare-header bead that is NOT a task (defensive).
		{"bare header but message type", &feedEventBead{Type: "message", Title: resumeSweepScorecardTitle}, false},
		// IGNORE: a genuine task that merely mentions a sweep.
		{"unrelated task", &feedEventBead{Type: "task", Title: "Fix the resume sweep classifier noise"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedResumeSweepScorecard(c.bead); got != c.want {
				t.Errorf("feedResumeSweepScorecard(%q) = %v, want %v", c.bead.Title, got, c.want)
			}
		})
	}
	if feedResumeSweepScorecard(nil) {
		t.Errorf("feedResumeSweepScorecard(nil) = true, want false")
	}
}

// TestFeedClassifyDropsResumeSweepPair is the end-to-end acceptance check: both
// verbatim fixtures, wrapped as their real SSE lifecycle events, are dropped by
// classifyFeedEvent (ok=false). The squawk arrives as a `created` message bead;
// the scorecard as a `created` task bead.
func TestFeedClassifyDropsResumeSweepPair(t *testing.T) {
	mk := func(b *feedEventBead) json.RawMessage {
		raw, err := json.Marshal(feedEventSSE{Bead: b})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}
	cases := []struct {
		name  string
		topic string
		bead  *feedEventBead
	}{
		{
			"squawk recap dropped",
			"beads.bead.created",
			&feedEventBead{ID: "gd-iAQLfab3JC", Type: "message", Title: resumeSweepSquawkTitle, Labels: []string{"squawk", "from:gardenio-resumer-watchdog-gardener-of-capt-9y4r"}},
		},
		{
			"scorecard task dropped",
			"beads.bead.created",
			&feedEventBead{ID: "gd-m5992Q4Esp", Type: "task", Title: resumeSweepScorecardTitle, Labels: []string{"needs-intake", "project:gardenio"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, _, ok := classifyFeedEvent(c.topic, mk(c.bead)); ok {
				t.Errorf("classifyFeedEvent(%s, %q) ok=true, want dropped (false)", c.topic, c.bead.Title)
			}
		})
	}

	// A substantive sweep squawk STILL renders as a card.
	substantive := &feedEventBead{ID: "gd-live", Type: "message", Title: "Resume sweep 2026-06-19 11:44 UTC: 1 stuck gardener re-armed (gd-45aY6VxSch), progress score 1.", Labels: []string{"squawk"}}
	if kind, _, _, ok := classifyFeedEvent("beads.bead.created", mk(substantive)); !ok || kind.Name != feedKindSquawk.Name {
		t.Errorf("substantive resume sweep: ok=%v kind=%q, want ok=true kind=%q", ok, kind.Name, feedKindSquawk.Name)
	}
}
