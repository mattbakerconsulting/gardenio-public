package main

// decision_evidence_test.go — the decision evidence strip on every viewer
// surface that resolves a decision (gd-Pi7C83262N): the screenshots on the
// bead a decision is about (its requesting task) render under the context
// block, a failed read renders the decision without them, and the deck card
// reads them through the cache.

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// withEvidence points the decision at a task whose comment carries a viewer
// image link and a picture from a host the proxy does not serve.
func withEvidence(f *fakeDaemon, dec *beadsapi.BeadDetail) {
	dec.Fields["requesting_task"] = "gd-evtask1"
	f.beads["gd-evtask1"] = &beadsapi.BeadDetail{ID: "gd-evtask1", Type: "task", Status: "in_progress", Title: "Grade the Porch"}
	if f.comments == nil {
		f.comments = map[string][]beadsapi.Comment{}
	}
	f.comments["gd-evtask1"] = []beadsapi.Comment{{Text: "![porch, phone](https://viewer.example/image/ui/porch-phone.png) https://tracker.example/pixel.png"}}
}

// mustHaveEvidence asserts the strip renders after the context block and
// before options (the first resolve marker), and loads only through the proxy.
func mustHaveEvidence(t *testing.T, surface, body, options string) {
	t.Helper()
	c, e, o := strings.Index(body, `<section class="decision-ctx-block`), strings.Index(body, `<section class="dec-ev"`), strings.Index(body, options)
	if c < 0 || e < c || (options != "" && (o < 0 || o < e)) {
		t.Fatalf("%s: the evidence strip renders under the context and above the options (context %d, strip %d, options %d)", surface, c, e, o)
	}
	for _, want := range []string{`src="/image/ui/porch-phone.png"`, `alt="porch, phone"`, `from <a href="/gd-evtask1">Grade the Porch</a>`, `popover`} {
		if !strings.Contains(body[e:], want) {
			t.Errorf("%s: strip missing %q", surface, want)
		}
	}
	if strings.Contains(body, "tracker.example") {
		t.Errorf("%s: a picture the proxy does not serve must never load from its host", surface)
	}
}

func TestDecisionEvidence_EverySurface(t *testing.T) {
	t.Run("bead_page", func(t *testing.T) {
		f := newFake()
		f.beads["gd-gate1"] = gateDecisionBead(t, "gd-gate1")
		withEvidence(f, f.beads["gd-gate1"])
		mustHaveEvidence(t, "bead page", decisionCtxBody(t, f, "/gd-gate1", true), `<ul class="decision-options">`)
	})
	t.Run("decision_detail", func(t *testing.T) {
		f := newFake()
		f.beads["gd-gate1"] = gateDecisionBead(t, "gd-gate1")
		withEvidence(f, f.beads["gd-gate1"])
		mustHaveEvidence(t, "decision page", decisionCtxBody(t, f, "/decisions/gd-gate1", true), `<ul class="decision-options">`)
	})
	t.Run("up", func(t *testing.T) {
		f := newFake()
		f.beads["gd-gate1"] = gateDecisionBead(t, "gd-gate1")
		withEvidence(f, f.beads["gd-gate1"])
		mustHaveEvidence(t, "up", decisionCtxBody(t, f, "/up/gd-gate1", true), `<div class="up-decision-chips"`)
	})
	t.Run("plan_cockpit", func(t *testing.T) {
		f := seedPlanInlineDecisionFake()
		f.beads["gd-dec1"].Fields = gateDecisionFields(t)
		withEvidence(f, f.beads["gd-dec1"])
		mustHaveEvidence(t, "plan cockpit", decisionCtxBody(t, f, "/plans/gd-plan1", true), `<ul class="plan-decision-options"`)
	})
	t.Run("deck_card", func(t *testing.T) {
		f := seedDeckCardFatFace()
		f.beads["gd-dec1"].Fields = gateDecisionFields(t)
		withEvidence(f, f.beads["gd-dec1"])
		srv := mkServer(t, f)
		body, code := getBody(t, srv, "/plans/gd-plan1/card")
		if code != 200 {
			t.Fatalf("GET card: %d", code)
		}
		if len(body) > deckCardByteBudget {
			t.Errorf("deck card with evidence is %d bytes, over the %d budget", len(body), deckCardByteBudget)
		}
		mustHaveEvidence(t, "deck card", body, `<ul class="deck-card-options"`)
		f.getBeadMu.Lock()
		before := f.getBeadCalls
		f.getBeadMu.Unlock()
		getBody(t, srv, "/plans/gd-plan1/card")
		f.getBeadMu.Lock()
		again := f.getBeadCalls - before
		f.getBeadMu.Unlock()
		if again > deckCardRoundTripBudget {
			t.Errorf("a prefetched card again reads its evidence through the cache: %d GetBead, budget %d", again, deckCardRoundTripBudget)
		}
	})
	t.Run("deck", func(t *testing.T) {
		f := seedDeck()
		f.beads["gd-q1"].Fields = gateDecisionFields(t)
		withEvidence(f, f.beads["gd-q1"])
		mustHaveEvidence(t, "deck", decisionCtxBody(t, f, "/deck", true), `<ul class="deck-options"`)
	})
	t.Run("decisions_list", func(t *testing.T) {
		f := newFake()
		b := gateDecisionBead(t, "gd-gate1")
		withEvidence(f, b)
		f.decisionResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{b}, Total: 1}
		mustHaveEvidence(t, "decisions list", decisionCtxBody(t, f, "/decisions", true), `aria-label="Decision options"`)
	})
}

// TestDecisionEvidence_FailedReadRendersWithout: an evidence bead the daemon
// will not return leaves the strip out and the decision whole.
func TestDecisionEvidence_FailedReadRendersWithout(t *testing.T) {
	f := newFake()
	b := gateDecisionBead(t, "gd-gate1")
	b.Fields["requesting_task"] = "gd-missing1"
	f.beads["gd-gate1"] = b
	body := decisionCtxBody(t, f, "/gd-gate1", true)
	if strings.Contains(body, `class="dec-ev"`) {
		t.Error("no readable evidence: no strip")
	}
	if !strings.Contains(body, `<section class="decision-ctx-block`) || !strings.Contains(body, `<ul class="decision-options">`) {
		t.Error("the decision renders whole without its evidence")
	}
}
