package main

// decision_context_golden_test.go — the decision context block on every viewer
// surface that resolves a decision (epic gd-hmwSI9lwKe step 2, gd-PNtilz1bgs).
// Each surface is driven through its real route with one long-context
// decision, the Gate's plot-application approval (landsGate.decisionContext,
// the production writer), and pinned two ways:
//
//   - the context block renders ABOVE the surface's first resolve control, and
//   - the excerpt from the block through the options is byte-stable against a
//     golden under testdata/decision_context/.
//
// Regenerate after an intended change with
// `GB_UPDATE_GOLDEN=1 go test ./cmd/beads-viewer/ -run TestDecisionContext_Golden`
// and read the diff.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// gateDecisionFields is the field set the Gate files for a plot application
// (lands_apply.go fileDecision), with a description on each option so the
// option-description render is pinned too.
func gateDecisionFields(t *testing.T) map[string]string {
	t.Helper()
	g := &landsGate{defaultCredits: 25}
	ctx := g.decisionContext(&landsApplication{
		ID:               "gd-app1",
		ApplicantEmail:   "ada@example.com",
		GitHubLogin:      "ada-l",
		LandName:         "analytical-engine",
		Repo:             "ada-l/analytical-engine",
		AccountAgeDays:   "2114",
		PublicRepos:      "37",
		CreditsRequested: "40",
		Purpose: "Port the note G Bernoulli program to a modern runtime and keep a crew of agents " +
			"triaging the issue tracker while I write the paper. I expect two or three agents working " +
			"most weekdays, mostly on test coverage and docs, with the occasional refactor of the card " +
			"reader module, which is old and has no tests at all.",
	})
	opts, err := json.Marshal([]map[string]string{
		{"id": "approve", "short": "Approve", "artifact_type": "report",
			"label":       "Approve: provision the land with 25 credits on Sonnet 5.5. To change either, reply credits=<n> and/or model=opus.",
			"description": "The land is created and the applicant is emailed a sign-in link."},
		{"id": "deny", "short": "Deny", "artifact_type": "report",
			"label":       "Deny: your reply is the reason the applicant reads.",
			"description": "Closes the application; no land is created."},
		{"id": "ask", "short": "Ask", "artifact_type": "report",
			"label":       "Ask: your reply is emailed to the applicant as a question, and the application stays open.",
			"description": "The decision is filed again when they reply."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx) <= decisionContextClampChars {
		t.Fatalf("fixture context is %d chars; it must exceed the %d-char clamp to pin the show-more", len(ctx), decisionContextClampChars)
	}
	return map[string]string{
		"prompt":       "Plot application: analytical-engine for ada-l/analytical-engine from ada-l",
		"options":      string(opts),
		"context":      ctx,
		"requested_by": "lands-gate",
	}
}

func gateDecisionBead(t *testing.T, id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Title: "Plot application: analytical-engine", Kind: "issue", Type: "decision",
		Status: "open", Labels: []string{"project:gardenio"}, Fields: gateDecisionFields(t),
	}
}

func decisionCtxOperator(req *http.Request) *http.Request {
	ctx := oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator})
	return req.WithContext(oidcauth.ContextWithEmail(ctx, "op@example.com"))
}

func decisionCtxBody(t *testing.T, f *fakeDaemon, target string, operator bool) string {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	if operator {
		req = decisionCtxOperator(req)
	}
	w := httptest.NewRecorder()
	mkServer(t, f).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", target, w.Code)
	}
	return w.Body.String()
}

// contextExcerpt returns the surface's markup from start (the context block,
// or the disclosure holding it) through the first end after the options open
// marker, with each line trimmed and blank lines dropped so indentation churn
// does not force a regen. It fails unless the block precedes the options.
func contextExcerpt(t *testing.T, body, start, options, end string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("no %q in the render", start)
	}
	o := strings.Index(body, options)
	if o < 0 {
		t.Fatalf("no options marker %q in the render", options)
	}
	if o < i {
		t.Fatalf("options (%q) render above the context block (%q): context must come first", options, start)
	}
	e := strings.Index(body[o:], end)
	if e < 0 {
		t.Fatalf("no %q after the options", end)
	}
	var lines []string
	for _, l := range strings.Split(body[i:o+e+len(end)], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func decisionCtxGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "decision_context", name+".golden.html")
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

// mustHaveContext asserts the long-context markers every surface shares: the
// clamp, the show-more toggle, a line from deep in the context (so the whole
// text is sent, not a clip) and an option description.
func mustHaveContext(t *testing.T, surface, excerpt, id string) {
	t.Helper()
	for _, want := range []string{
		`class="decision-ctx-block decision-ctx-block-long"`,
		`id="decision-ctx-more-` + id + `"`,
		`for="decision-ctx-more-` + id + `"`,
		"Show more",
		"ada-l/analytical-engine",
		"Deny's reply is the reason the applicant reads",
		`<span class="opt-desc">Closes the application; no land is created.</span>`,
	} {
		if !strings.Contains(excerpt, want) {
			t.Errorf("%s: context excerpt missing %q\n%s", surface, want, excerpt)
		}
	}
}

func TestDecisionContext_Golden(t *testing.T) {
	t.Run("bead_page", func(t *testing.T) {
		f := newFake()
		f.beads["gd-gate1"] = gateDecisionBead(t, "gd-gate1")
		body := decisionCtxBody(t, f, "/gd-gate1", true)
		got := contextExcerpt(t, body, `<section class="decision-ctx-block`, `<ul class="decision-options">`, "</ul>")
		mustHaveContext(t, "bead page", got, "gd-gate1")
		if i, c := strings.Index(body, `<section class="decision-ctx-block`), strings.Index(body, "decision-resolve-chip"); c >= 0 && c < i {
			t.Error("bead page: a resolve chip renders above the context")
		}
		decisionCtxGolden(t, "bead_page", got)
	})

	t.Run("up", func(t *testing.T) {
		f := newFake()
		f.beads["gd-gate1"] = gateDecisionBead(t, "gd-gate1")
		body := decisionCtxBody(t, f, "/up/gd-gate1", true)
		got := contextExcerpt(t, body, `<section class="decision-ctx-block`, `<div class="up-decision-chips"`, "</div>")
		mustHaveContext(t, "up", got, "gd-gate1")
		if strings.Contains(body, "No description.") {
			t.Error("up: a decision card must show its context, not \"No description.\"")
		}
		if !strings.Contains(body, `data-up-key="Enter" data-up-kind="post"`) || !strings.Contains(body, `data-up-endpoint="/b/{id}/decision/resolve"`) {
			t.Error("up: with context shown, Enter should resolve with the recommended option")
		}
		decisionCtxGolden(t, "up", got)
	})

	t.Run("plan_cockpit", func(t *testing.T) {
		f := seedPlanInlineDecisionFake()
		dec := f.beads["gd-dec1"]
		dec.Fields = gateDecisionFields(t)
		got := contextExcerpt(t, decisionCtxBody(t, f, "/plans/gd-plan1", true),
			`<section class="decision-ctx-block`, `<ul class="plan-decision-options"`, "</ul>")
		mustHaveContext(t, "plan cockpit", got, "gd-dec1")
		decisionCtxGolden(t, "plan_cockpit", got)
	})

	t.Run("deck_card", func(t *testing.T) {
		f := seedDeckCardFatFace()
		f.beads["gd-dec1"].Fields = gateDecisionFields(t)
		body := decisionCtxBody(t, f, "/plans/gd-plan1/card", true)
		if len(body) > deckCardByteBudget {
			t.Errorf("deck card with a long context is %d bytes, over the %d budget", len(body), deckCardByteBudget)
		}
		got := contextExcerpt(t, body, `<section class="decision-ctx-block`, `<ul class="deck-card-options"`, "</ul>")
		mustHaveContext(t, "deck card", got, "gd-dec1")
		decisionCtxGolden(t, "deck_card", got)
	})

	t.Run("deck", func(t *testing.T) {
		f := seedDeck()
		f.beads["gd-q1"].Fields = gateDecisionFields(t)
		got := contextExcerpt(t, decisionCtxBody(t, f, "/deck", true),
			`<section class="decision-ctx-block`, `<ul class="deck-options"`, "</ul>")
		mustHaveContext(t, "deck", got, "gd-q1")
		decisionCtxGolden(t, "deck", got)
	})

	t.Run("decisions_list", func(t *testing.T) {
		f := newFake()
		f.decisionResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{gateDecisionBead(t, "gd-gate1")}, Total: 1}
		got := contextExcerpt(t, decisionCtxBody(t, f, "/decisions", true),
			`<details class="bead-row-details" open>`, `aria-label="Decision options"`, "</div>")
		for _, want := range []string{`class="decision-ctx-block decision-ctx-block-long"`, "ada-l/analytical-engine",
			"Closes the application; no land is created."} {
			if !strings.Contains(got, want) {
				t.Errorf("decisions list: excerpt missing %q\n%s", want, got)
			}
		}
		decisionCtxGolden(t, "decisions_list", got)
	})
}

// TestDecisionContext_UpNoContextDisarmsEnter pins the /up rule: a decision
// with neither a context field nor a description says so, and Enter opens the
// bead instead of resolving with the first option.
func TestDecisionContext_UpNoContextDisarmsEnter(t *testing.T) {
	f := newFake()
	b := gateDecisionBead(t, "gd-bare1")
	delete(b.Fields, "context")
	f.beads["gd-bare1"] = b
	body := decisionCtxBody(t, f, "/up/gd-bare1", true)
	if !strings.Contains(body, `decision-ctx-block-empty`) || !strings.Contains(body, "No context was given for this decision.") {
		t.Error("up: a decision without context must say so")
	}
	if strings.Contains(body, "No description.") {
		t.Error("up: a decision card must not fall back to \"No description.\"")
	}
	if strings.Contains(body, `data-up-key="Enter" data-up-kind="post"`) {
		t.Error("up: Enter must not resolve a decision whose context is not shown")
	}
	if !strings.Contains(body, `data-up-key="Enter" data-up-kind="open"`) {
		t.Error("up: Enter should open the bead to read first")
	}
}

// TestDecisionContext_DescriptionFallback pins the read model's fallback on a
// surface: an older decision that carries its context in the description
// shows it, unclamped when short.
func TestDecisionContext_DescriptionFallback(t *testing.T) {
	f := newFake()
	b := gateDecisionBead(t, "gd-old1")
	delete(b.Fields, "context")
	b.Description = "Grafana conflict on **dashboard-42**: the file and the live copy differ."
	f.beads["gd-old1"] = b
	body := decisionCtxBody(t, f, "/gd-old1", true)
	for _, want := range []string{`<section class="decision-ctx-block" `, "<strong>dashboard-42</strong>"} {
		if !strings.Contains(body, want) {
			t.Errorf("bead page: missing %q", want)
		}
	}
	if strings.Contains(body, `id="decision-ctx-more-gd-old1"`) {
		t.Error("a short context must not get a show-more toggle")
	}
}
