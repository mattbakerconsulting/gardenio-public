package main

// decision_acceptance_golden_test.go — the acceptance decision on every viewer
// surface that resolves a decision (epic gd-hmwSI9lwKe step 6, gd-DiU8SB6TAM).
// The decision is decisiontest's, the one the /garden, Commons and Slack
// goldens use too: the Gate's plot-application approval with five options, a
// context past the fold, and --evidence naming a task whose comments carry two
// `gb image upload` links. Each surface is driven through its real route and
// pinned two ways:
//
//   - the context block, then the evidence strip, then the options, in that
//     order, with all five options and both pictures through the proxy, and
//   - the excerpt from the context block through the options is byte-stable
//     against a golden under testdata/decision_acceptance/.
//
// Regenerate after an intended change with
// `GB_UPDATE_GOLDEN=1 go test ./cmd/beads-viewer/ -run TestDecisionAcceptance_Golden`
// and read the diff.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview/decisiontest"
)

const acceptEvidenceID = "gd-GateEvid01"

// withAcceptance makes dec the acceptance decision and puts its evidence
// task, with the task's comments, on the fake daemon.
func withAcceptance(f *fakeDaemon, dec *beadsapi.BeadDetail) {
	dec.Title, dec.Fields = decisiontest.Title, decisiontest.Fields(acceptEvidenceID)
	f.beads[acceptEvidenceID] = decisiontest.Evidence(acceptEvidenceID)
	if f.comments == nil {
		f.comments = map[string][]beadsapi.Comment{}
	}
	f.comments[acceptEvidenceID] = decisiontest.Comments(time.Unix(1_793_000_000, 0))
}

// acceptanceExcerpt is contextExcerpt with the evidence strip asserted
// between the context block and the options, every option's button text and
// description in it, and the pictures loading only through /image/.
func acceptanceExcerpt(t *testing.T, surface, body, options, end string, descriptions bool) string {
	t.Helper()
	got := contextExcerpt(t, body, `<section class="decision-ctx-block`, options, end)
	c, e, o := strings.Index(got, `<section class="decision-ctx-block`), strings.Index(got, `<section class="dec-ev"`), strings.Index(got, options)
	if e < c || o < e {
		t.Fatalf("%s: context %d, evidence %d, options %d: the strip goes under the context and above the options\n%s", surface, c, e, o, got)
	}
	want := []string{
		`class="decision-ctx-block decision-ctx-block-long"`, "Show more",
		"ada-l/analytical-engine", "Dismissing this files it again: deny instead.",
		`src="/image/` + decisiontest.ImageKeys[0] + `"`, `src="/image/` + decisiontest.ImageKeys[1] + `"`,
		`alt="` + decisiontest.ImageAlts[0] + `"`,
		`from <a href="/` + acceptEvidenceID + `">` + strings.ReplaceAll(decisiontest.EvidenceTitle, "'", "&#39;") + `</a>`,
	}
	want = append(want, decisiontest.OptionShorts...) // each surface words the button its own way (/up stars the recommended)
	if descriptions {
		for _, d := range decisiontest.OptionDescriptions {
			want = append(want, strings.ReplaceAll(d, "'", "&#39;"))
		}
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s: excerpt missing %q\n%s", surface, w, got)
		}
	}
	if strings.Contains(body, "beads.example") {
		t.Errorf("%s: a picture must load through the proxy, never from the uploader's host", surface)
	}
	return got
}

func acceptanceGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "decision_acceptance", name+".golden.html")
	if os.Getenv("GB_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s drifted (regenerate with GB_UPDATE_GOLDEN=1 if intended)\n%s", path, firstDiff(string(want), got))
	}
}

func TestDecisionAcceptance_Golden(t *testing.T) {
	const id = "gd-GateDec001"
	single := func(t *testing.T) *fakeDaemon {
		f := newFake()
		f.beads[id] = decisiontest.Decision(id, acceptEvidenceID)
		withAcceptance(f, f.beads[id])
		return f
	}

	t.Run("bead_page", func(t *testing.T) {
		body := decisionCtxBody(t, single(t), "/"+id, true)
		got := acceptanceExcerpt(t, "bead page", body, `<ul class="decision-options">`, "</ul>", true)
		if !strings.Contains(body, `decision-other-chip`) {
			t.Error("bead page: the free-text Other is offered beside the five options")
		}
		acceptanceGolden(t, "bead_page", got)
	})

	t.Run("decision_detail", func(t *testing.T) {
		got := acceptanceExcerpt(t, "decision page", decisionCtxBody(t, single(t), "/decisions/"+id, true), `<ul class="decision-options">`, "</ul>", true)
		acceptanceGolden(t, "decision_detail", got)
	})

	t.Run("up", func(t *testing.T) {
		body := decisionCtxBody(t, single(t), "/up/"+id, true)
		got := acceptanceExcerpt(t, "up", body, `<div class="up-decision-chips"`, "</div>", true)
		if !strings.Contains(body, `data-up-key="Enter" data-up-kind="post"`) {
			t.Error("up: with context and evidence shown, Enter resolves with the recommended option")
		}
		acceptanceGolden(t, "up", got)
	})

	t.Run("plan_cockpit", func(t *testing.T) {
		f := seedPlanInlineDecisionFake()
		withAcceptance(f, f.beads["gd-dec1"])
		body := decisionCtxBody(t, f, "/plans/gd-plan1", true)
		got := acceptanceExcerpt(t, "plan cockpit", body, `<ul class="plan-decision-options"`, "</ul>", true)
		if !strings.Contains(body, "Other answer or dismiss") {
			t.Error("plan cockpit: the free-text Other is offered beside the five options")
		}
		acceptanceGolden(t, "plan_cockpit", got)
	})

	t.Run("deck_card", func(t *testing.T) {
		f := seedDeckCardFatFace()
		withAcceptance(f, f.beads["gd-dec1"])
		body := decisionCtxBody(t, f, "/plans/gd-plan1/card", true)
		if len(body) > deckCardByteBudget {
			t.Errorf("deck card with the acceptance decision is %d bytes, over the %d budget", len(body), deckCardByteBudget)
		}
		acceptanceGolden(t, "deck_card", acceptanceExcerpt(t, "deck card", body, `<ul class="deck-card-options"`, "</ul>", true))
	})

	t.Run("deck", func(t *testing.T) {
		f := seedDeck()
		withAcceptance(f, f.beads["gd-q1"])
		acceptanceGolden(t, "deck", acceptanceExcerpt(t, "deck", decisionCtxBody(t, f, "/deck", true), `<ul class="deck-options"`, "</ul>", true))
	})

	t.Run("decisions_list", func(t *testing.T) {
		f := single(t)
		f.decisionResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{f.beads[id]}, Total: 1}
		body := decisionCtxBody(t, f, "/decisions", true)
		if !strings.Contains(body, `<details class="bead-row-details" open>`) {
			t.Error("decisions list: the row's context opens by default")
		}
		acceptanceGolden(t, "decisions_list", acceptanceExcerpt(t, "decisions list", body, `aria-label="Decision options"`, "</div>", true))
	})
}

// TestGateFixtureIsTheGatesOwn keeps decisiontest the realistic fixture the
// step asks for: its prompt and context are what the Gate writes for its
// application, so a change to the Gate's wording reaches every surface's
// golden through the fixture, not past it.
func TestGateFixtureIsTheGatesOwn(t *testing.T) {
	a := decisiontest.Application
	app := &landsApplication{
		ID: a["id"], ApplicantEmail: a["applicant_email"], GitHubLogin: a["github_login"], LandName: a["land_name"],
		Repo: a["repo"], AccountAgeDays: a["account_age_days"], PublicRepos: a["public_repos"],
		CreditsRequested: a["credits_requested"], Purpose: a["purpose"],
	}
	g := &landsGate{defaultCredits: decisiontest.DefaultCredits}
	if got := g.decisionContext(app); got != decisiontest.Context {
		t.Errorf("decisiontest.Context is not the Gate's context for decisiontest.Application:\n%s", firstDiff(decisiontest.Context, got))
	}
	if want := "Plot application: " + app.LandName + " for " + app.Repo + " from " + app.GitHubLogin; decisiontest.Prompt != want {
		t.Errorf("decisiontest.Prompt = %q, the Gate asks %q", decisiontest.Prompt, want)
	}
	if len(decisiontest.Context) <= decisionContextClampChars {
		t.Errorf("the fixture context is %d chars, inside the %d-char fold", len(decisiontest.Context), decisionContextClampChars)
	}
}
