package main

import (
	"testing"
	"time"
)

// feed_daily_bundle_cluster_test.go — ka-0000005042. ONE Daily Bundle
// publication (Fleet Vol. I No. 6, Fri Jun 19) fanned into 8 cards in /feed:
// the bundle bead + a "published and merged" decision + a :newspaper: message +
// 5 section/index docs. None share a bead id (so collapseGlobalDuplicates /
// collapseBeadLifecycle miss them), the docs carry no bundle id in their titles
// and one parents a DIFFERENT (archive) bundle (so a parent-key alone misses
// them), and they span three bead TYPES (so the type-specific passes miss them).
// collapseDailyBundleClusters keys on the bundle anchor and folds the whole
// publication to ONE card exemplified by the bundle, with a count badge.

// The 9 live bead ids from the offending publication, used verbatim as
// fixtures (8 surface as feed cards; the 9th, Infrastructure & Markets, is one
// of the 5 section docs).
const (
	dbFixtureBundle   = "gd-Ednz25SRTC" // bundle
	dbFixtureDecision = "gd-pebcZo1AG9" // "published and merged" decision
	dbFixtureMessage  = "gd-DLYh5WMuIK" // :newspaper: announcement message
	dbFixtureFront    = "gd-mCTttTxziT" // doc: News Archive — Front Index
	dbFixtureWire     = "gd-9Okxayw6Qo" // doc: 🔌 The Wire
	dbFixtureInside   = "gd-a7K6sxFRZX" // doc: 📖 Inside This Issue
	dbFixtureInfra    = "gd-xcw6BPkIBa" // doc: 📈 Infrastructure & Markets
	dbFixturePageOne  = "gd-CSbjTKF3tH" // doc: 📰 Page One
)

// dbFixtureEditor is the verbatim author of every artifact in the publication
// (with a different 4-char pod suffix per artifact in real life; the
// normalizer strips it, so the fixtures share the suffix-free stem here and a
// distinct suffix where it matters below).
const dbFixtureEditor = "daily-bundle-editor-in-chief-the-daily-bundle-eh9p"

func TestFeedDailyBundleBundleRef(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bundle word form", "bundle gd-Ednz25SRTC is published", "gd-ednz25srtc"},
		{"slash path form", "see /bundle/gd-Ednz25SRTC for sections", "gd-ednz25srtc"},
		{"decision verbatim", "The Daily Bundle Fleet Vol. I No. 6 (Fri Jun 19) is published and merged: bundle gd-Ednz25SRTC (/bundle/gd-Ednz25SRTC), MR !32 merged", "gd-ednz25srtc"},
		{"no bundle ref", "release-owner: cut 2026.169.11, merged MR !4625", ""},
		{"bare kd id is not a bundle ref", "crew reporting in (ka-0000003233)", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedDailyBundleBundleRef(c.in); got != c.want {
				t.Errorf("feedDailyBundleBundleRef(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCollapseDailyBundleClusters_FleetVolINo6 uses the VERBATIM ids + titles +
// author of the live offending publication. The 8 cards MUST fold to a single
// card, exemplified by the bundle (so it titles "The Daily Bundle …" and links
// through to the bundle), carrying the count of all folded artifacts.
func TestCollapseDailyBundleClusters_FleetVolINo6(t *testing.T) {
	t0 := mustTime(t, "2026-06-19T13:18:31Z")
	// One publication is authored by ONE editor pod, so every artifact carries
	// the IDENTICAL created_by (verbatim from the live beads: all eight share
	// daily-bundle-editor-in-chief-the-daily-bundle-eh9p). A NEXT edition is a
	// different pod AND hours away, so the window guard — not author skew —
	// separates editions.
	card := func(id, beadType, title string, k feedKind, off time.Duration) feedCardItem {
		return feedCardItem{
			kind:  k,
			bead:  &feedEventBead{ID: id, Type: beadType, Title: title, Status: "closed", CreatedBy: dbFixtureEditor},
			ts:    t0.Add(off),
			count: 1,
		}
	}

	// Verbatim from the live publication (oldest-first, the order the collapse
	// pass receives). The bundle anchor is created first, sections follow within
	// minutes, the announcement + decision close it out.
	in := []feedCardItem{
		card(dbFixtureBundle, "bundle",
			"The Daily Bundle — Friday, June 19, 2026 (Fleet Vol. I, No. 6)", feedKindCreated, 0),
		card(dbFixtureWire, "doc",
			"🔌 The Wire", feedKindCreated, 8*time.Second),
		card(dbFixtureInside, "doc",
			"📖 Inside This Issue", feedKindCreated, 10*time.Second),
		card(dbFixtureInfra, "doc",
			"📈 Infrastructure & Markets", feedKindCreated, 12*time.Second),
		card(dbFixturePageOne, "doc",
			"📰 Page One", feedKindCreated, 14*time.Second),
		card(dbFixtureMessage, "message",
			":newspaper: *The Daily Bundle — Fleet Vol. I, No. 6* · Fri, June 19, 2026 (Morning Edition)\nhttps://beads-viewer.app.e2e.dev.acme.example/bundle/gd-Ednz25SRTC", feedKindSquawk, 15*time.Second),
		card(dbFixtureFront, "doc",
			"The News Archive — Front Index", feedKindCreated, 117*time.Second),
		card(dbFixtureDecision, "decision",
			"The Daily Bundle Fleet Vol. I No. 6 (Fri Jun 19) is published and merged: bundle gd-Ednz25SRTC (/bundle/gd-Ednz25SRTC), MR !32 merged to main, posted to #the-daily-bundle. How should I close out?", feedKindDecision, 4*time.Minute),
	}

	out := collapseDailyBundleClusters(in)

	if len(out) != 1 {
		var got []string
		for _, it := range out {
			got = append(got, it.bead.ID+"/"+it.bead.Type)
		}
		t.Fatalf("Daily Bundle fan-out should fold to 1 card, got %d: %v", len(out), got)
	}
	// Representative exemplifies the BUNDLE (title + id + kind) so the card links
	// through to /bundle/<id> and titles cleanly.
	if out[0].bead.ID != dbFixtureBundle {
		t.Errorf("exemplar id = %q, want bundle %q", out[0].bead.ID, dbFixtureBundle)
	}
	if out[0].bead.Type != "bundle" {
		t.Errorf("exemplar type = %q, want bundle", out[0].bead.Type)
	}
	if out[0].count != len(in) {
		t.Errorf("folded count = %d, want %d (all artifacts)", out[0].count, len(in))
	}
}

// TestCollapseDailyBundleClusters_UnrelatedStaysDistinct proves the pass is
// narrow: an unrelated doc (not in the section allowlist) authored by the SAME
// editor in the SAME window stays its own card, and an unrelated bundle by a
// different author is untouched. Only genuine daily-bundle sub-artifacts fold.
func TestCollapseDailyBundleClusters_UnrelatedStaysDistinct(t *testing.T) {
	t0 := mustTime(t, "2026-06-19T13:18:31Z")
	bead := func(id, beadType, title, author string) *feedEventBead {
		return &feedEventBead{ID: id, Type: beadType, Title: title, Status: "open", CreatedBy: author}
	}
	item := func(b *feedEventBead, k feedKind, off time.Duration) feedCardItem {
		return feedCardItem{kind: k, bead: b, ts: t0.Add(off), count: 1}
	}

	in := []feedCardItem{
		// The daily-bundle cluster (bundle + 2 sections) — folds to 1. All share
		// the editor's verbatim author (one publication = one editor pod).
		item(bead(dbFixtureBundle, "bundle",
			"The Daily Bundle — Friday, June 19, 2026 (Fleet Vol. I, No. 6)",
			dbFixtureEditor), feedKindCreated, 0),
		item(bead(dbFixtureWire, "doc", "🔌 The Wire", dbFixtureEditor), feedKindCreated, 9*time.Second),
		item(bead(dbFixturePageOne, "doc", "📰 Page One", dbFixtureEditor), feedKindCreated, 14*time.Second),

		// An UNRELATED doc by the same editor in the same window — NOT in the
		// section allowlist, so it must stay distinct.
		item(bead("gd-p30qb6aedw", "doc", "Editor scratch notes — TODO cleanup",
			dbFixtureEditor), feedKindCreated, 20*time.Second),

		// An UNRELATED non-daily bundle by a DIFFERENT author — never an anchor,
		// never folded.
		item(bead("gd-otherbundle", "bundle", "Agent artifact bundle — incident intel",
			"some-other-agent-xyz9"), feedKindCreated, 30*time.Second),

		// An unrelated work card — untouched.
		item(bead("gd-work1", "task", "Widen modules/network default vpc_endpoint_services",
			"gardenio-widen-modulesnetwork-default-ort"), feedKindCreated, 40*time.Second),
	}

	out := collapseDailyBundleClusters(in)

	// 1 daily-bundle card + scratch doc + other bundle + work = 4.
	if len(out) != 4 {
		var got []string
		for _, it := range out {
			got = append(got, it.bead.ID)
		}
		t.Fatalf("expected 4 cards (1 folded bundle + 3 distinct), got %d: %v", len(out), got)
	}

	var foldedCount int
	ids := map[string]bool{}
	for _, it := range out {
		ids[it.bead.ID] = true
		if it.bead.ID == dbFixtureBundle {
			foldedCount = it.count
		}
	}
	if foldedCount != 3 {
		t.Errorf("daily-bundle card count = %d, want 3 (bundle + 2 sections)", foldedCount)
	}
	for _, want := range []string{dbFixtureBundle, "gd-p30qb6aedw", "gd-otherbundle", "gd-work1"} {
		if !ids[want] {
			t.Errorf("expected card %q to survive distinct, missing", want)
		}
	}
	// The unrelated bundle must NOT have absorbed anything.
	for _, it := range out {
		if it.bead.ID == "gd-otherbundle" && it.count != 1 {
			t.Errorf("unrelated bundle count = %d, want 1 (no fold)", it.count)
		}
	}
}

// TestCollapseDailyBundleClusters_NoAnchorLeavesSectionsAlone proves that when
// the bundle anchor is absent from the window (scrolled out of the seed), the
// section docs are left untouched rather than folded among themselves — the
// pass refuses to invent an anchor.
func TestCollapseDailyBundleClusters_NoAnchorLeavesSectionsAlone(t *testing.T) {
	t0 := mustTime(t, "2026-06-19T13:18:31Z")
	item := func(id, title string, off time.Duration) feedCardItem {
		return feedCardItem{
			kind:  feedKindCreated,
			bead:  &feedEventBead{ID: id, Type: "doc", Title: title, Status: "open", CreatedBy: dbFixtureEditor},
			ts:    t0.Add(off),
			count: 1,
		}
	}
	in := []feedCardItem{
		item(dbFixtureWire, "🔌 The Wire", 0),
		item(dbFixturePageOne, "📰 Page One", 2*time.Second),
	}
	out := collapseDailyBundleClusters(in)
	if len(out) != 2 {
		t.Fatalf("with no anchor, sections must stay distinct; got %d cards", len(out))
	}
}
