package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadShow_DecisionType verifies the decision per-type partial: prompt,
// options list, resolution banner with chosen-option badge.
func TestBeadShow_DecisionType(t *testing.T) {
	f := newFake()
	f.beads["gd-dec"] = &beadsapi.BeadDetail{
		ID:        "gd-dec",
		Title:     "JWT vs sessions",
		Type:      "decision",
		Status:    "closed",
		CreatedBy: "gardener-1",
		Fields: map[string]string{
			"prompt":       "Pick auth approach for the new service.",
			"requested_by": "gardener-1",
			"options": `[
				{"id":"jwt","short":"Use JWT","label":"Stateless JWT tokens","artifact_type":"plan"},
				{"id":"session","short":"Use sessions","label":"Server-side sessions","artifact_type":"plan"}
			]`,
			"resolution": `{"option_id":"jwt","resolved_by":"matt","resolved_at":"2026-04-20T12:00:00Z","response_text":"Go with JWT, refresh on rotation."}`,
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-dec", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h3>Decision</h3>",
		"Pick auth approach", // prompt
		"gardener-1",         // requested by
		"banner-resolved",    // resolution banner
		"<code>jwt</code>",   // chosen option id in banner
		"matt",               // resolved-by
		"Go with JWT",        // response text
		"opt-jwt",            // option list class
		"opt-session",
		"opt-chosen", // chosen badge class on jwt option
		"Stateless JWT tokens",
		"Server-side sessions",
		"plan", // artifact type
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decision render missing %q\nbody: %s", want, body)
		}
	}
	// Generic Fields dump must NOT appear for typed dispatch.
	if strings.Contains(body, `<h3 aria-level="2">Fields</h3>`) {
		t.Errorf("decision bead leaked into generic Fields dump\nbody: %s", body)
	}
}

// TestBeadShow_ScheduleType verifies cron, enabled flag, and next-fire surfacing.
func TestBeadShow_ScheduleType(t *testing.T) {
	f := newFake()
	f.beads["gd-sch"] = &beadsapi.BeadDetail{
		ID:     "gd-sch",
		Title:  "nightly sync",
		Type:   "schedule",
		Status: "open",
		Fields: map[string]string{
			"cron":           "0 9 * * *",
			"enabled":        "true",
			"timezone":       "UTC",
			"project":        "gardenio",
			"role":           "crew",
			"prompt":         "Run nightly sync.",
			"runs_spawned":   "12",
			"runs_completed": "11",
			"last_agent_id":  "gd-agent-99",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-sch", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h3>Schedule</h3>",
		"<code>0 9 * * *</code>", // cron expression
		"Yes",                    // enabled
		"UTC",                    // timezone
		"Next fire",              // next-fire label
		"gardenio",               // project
		"crew",                   // role
		"12",                     // runs spawned
		"11",                     // runs completed
		"gd-agent-99",            // last agent
		"Run nightly sync.",      // prompt body inside <details>
	} {
		if !strings.Contains(body, want) {
			t.Errorf("schedule render missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_ScheduleType_BadCron verifies the NextFireError path: a
// malformed cron expression must surface the parse error inline rather than
// crashing the page.
func TestBeadShow_ScheduleType_BadCron(t *testing.T) {
	f := newFake()
	f.beads["gd-sch-bad"] = &beadsapi.BeadDetail{
		ID:     "gd-sch-bad",
		Title:  "broken cron",
		Type:   "schedule",
		Status: "open",
		Fields: map[string]string{
			"cron":    "not a cron",
			"enabled": "false",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-sch-bad", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No") {
		t.Errorf("expected enabled=No for false-flag\nbody: %s", body)
	}
	// Some error string from the cron parser must surface (we don't pin the
	// exact text since it's third-party).
	if !strings.Contains(body, "Next fire") {
		t.Errorf("expected Next fire row\nbody: %s", body)
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("expected em-dash placeholder for failed cron parse\nbody: %s", body)
	}
}

// TestBeadShow_GardenerType is the convergence guard (task ka-0000006542): a
// gardener bead (type=trigger + role:gardener with a gardener spec) must render
// the rich gardener summary inline on /beads/<id> — the same goal / cadence /
// counters / config the dedicated /gardener/<id> page shows — plus a link into
// the full gardener page, instead of falling through to the bare type_generic
// field dump.
func TestBeadShow_GardenerType(t *testing.T) {
	f := newFake()
	spec := map[string]any{
		"name":           "DEVOPS triage gardener",
		"terminal_goal":  "drain the generated-DEVOPS triage queue to zero",
		"exit_condition": "no open generated DEVOPS tickets remain",
		"max_runs":       50,
	}
	f.beads["gd-cap"] = goldenGardenerBead(t, "gd-cap", "Gardener: DEVOPS triage", spec)
	f.beads["gd-cap"].Fields["cron"] = "0 9 * * *"
	f.beads["gd-cap"].Fields["timezone"] = "UTC"

	mux := mkServerWithNow(t, f, func() time.Time {
		return time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	})
	req := httptest.NewRequest("GET", "/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="bead-gardener"`,                           // the rich gardener block rendered
		"drain the generated-DEVOPS triage queue to zero", // terminal goal from the spec
		"Terminal goal",                                   // goal label from gardener_summary
		"<code>0 9 * * *</code>",                          // cadence
		"Iterations",                                      // authoritative run-count row
		"Recent runs",                                     // run preview section
		"/gardener/gd-cap",                                // CTA into the full gardener page
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gardener bead view missing %q\nbody: %s", want, body)
		}
	}
	// The bare generic field-dump heading must NOT be the primary content for a
	// gardener — that was the "wasted opportunity" this convergence replaces.
	if strings.Contains(body, "<h3>Schedule</h3>") {
		t.Errorf("gardener bead view should not fall back to the schedule field-list\nbody: %s", body)
	}
}

// TestBeadShow_PlainScheduleNotGardener guards the narrowed predicate: an
// ordinary type=schedule bead with NO gardener spec (e.g. a nightly-sync crew
// job) must keep its plain schedule field-list and must NOT render the rich
// gardener block. Pairs with TestBeadShow_GardenerType so the boundary is pinned.
func TestBeadShow_PlainScheduleNotGardener(t *testing.T) {
	f := newFake()
	f.beads["gd-plain"] = &beadsapi.BeadDetail{
		ID:     "gd-plain",
		Title:  "nightly sync",
		Type:   "schedule",
		Status: "open",
		Fields: map[string]string{
			"cron":    "0 3 * * *",
			"enabled": "true",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-plain", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<h3>Schedule</h3>") {
		t.Errorf("plain schedule bead should render the schedule field-list\nbody: %s", body)
	}
	if strings.Contains(body, `class="bead-gardener"`) {
		t.Errorf("plain schedule bead should NOT render the gardener block\nbody: %s", body)
	}
}

// TestBeadShow_FormulaType verifies step progress, current step, and vars.
func TestBeadShow_FormulaType(t *testing.T) {
	f := newFake()
	f.beads["gd-frm"] = &beadsapi.BeadDetail{
		ID:     "gd-frm",
		Title:  "release pipeline",
		Type:   "formula",
		Status: "in_progress",
		Fields: map[string]string{
			"pour_status": "running",
			"vars":        `[{"name":"version","description":"calver tag"}]`,
			"var_version": "2026.114.1",
			"steps": `[
				{"id":"build","type":"build","title":"Build images"},
				{"id":"deploy","type":"deploy","title":"Deploy to staging"},
				{"id":"e2e","type":"test","title":"E2E smoke"}
			]`,
			"step_status_build":  "completed",
			"step_status_deploy": "in_progress",
			"step_bead_deploy":   "gd-deploy-99",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-frm", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h3>Formula</h3>",
		"1 / 3 completed",         // step progress count
		"<code>deploy</code>",     // current step id
		"Deploy to staging",       // current step title
		"running",                 // pour status
		"<code>version</code>",    // var name
		"<code>2026.114.1</code>", // var value
		"Build images",
		"E2E smoke",
		"step-completed", // status class
		"step-in_progress",
		`href="/gd-deploy-99"`, // step bead link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("formula render missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_FormulaRecipe_NoExecutionChrome verifies that a PRISTINE recipe
// (a formula bead whose steps carry NO run status) reads as a template, not as
// a stalled molecule run: no "X / N completed" progress, no "Current step" row,
// no per-step run-status badges, and no leaked `{{var}}` placeholders in the
// page chrome. Instead it shows a definition count ("N steps") and renders the
// recipe's own variable placeholders as styled var tokens (bug ka-0000003537).
func TestBeadShow_FormulaRecipe_NoExecutionChrome(t *testing.T) {
	f := newFake()
	f.beads["gd-recipe"] = &beadsapi.BeadDetail{
		ID:     "gd-recipe",
		Title:  "release pipeline",
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			"vars": `[{"name":"version","description":"calver tag"}]`,
			"steps": `[
				{"id":"release-tag","type":"task","title":"Tag release {{version}}"},
				{"id":"build","type":"build","title":"Build images for {{version}}"},
				{"id":"deploy","type":"deploy","title":"Deploy to staging"}
			]`,
			// No step_status_* fields — this is a template, not a run.
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-recipe", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The "View raw steps JSON" reveal legitimately echoes the recipe's own
	// `{{version}}` placeholder inside a code block — that is valid raw content,
	// not a chrome leak. Scope the leak/execution-chrome assertions to the chrome
	// BEFORE that reveal so we test only human-readable prose, not the raw dump.
	chrome := body
	if before, _, ok := strings.Cut(body, `<details class="formula-raw-json">`); ok {
		chrome = before
	}

	// A pristine recipe must NOT surface any execution-shaped chrome.
	for _, forbidden := range []string{
		"/ 3 completed", // no "0 / 3 completed" run progress
		"Current step",  // no frontier step on a template
		"step-status",   // no per-step run-status badge
		">pending<",     // no "pending" execution badge text
	} {
		if strings.Contains(chrome, forbidden) {
			t.Errorf("pristine recipe chrome must not contain %q\nchrome: %s", forbidden, chrome)
		}
	}

	// The recipe's own `{{version}}` placeholder must NOT leak as a raw Go-template
	// action into visible chrome — it is rendered as a styled var token instead.
	if strings.Contains(chrome, "{{version}}") {
		t.Errorf("recipe chrome leaked raw {{version}} placeholder\nchrome: %s", chrome)
	}

	// It SHOULD show a definition count and the var-token styling.
	for _, want := range []string{
		"<h3>Formula</h3>",
		"3 steps",                   // definition count, not run progress
		`class="formula-var-token"`, // {{version}} rendered as a var token
		">version<",                 // the token text
		"Deploy to staging",         // a non-templated title still renders
	} {
		if !strings.Contains(chrome, want) {
			t.Errorf("recipe render missing %q\nchrome: %s", want, chrome)
		}
	}
}

// TestBeadShow_FormulaRecipe_StepListVisible pins the ka-0000003420 whitespace
// fix: a recipe's step list is the pane's primary content, so it renders as an
// OPEN <details> with visible step rows rather than a collapsed one-line
// disclosure that left a tall white void under the thin DAG strip. Also guards
// the test-collision constraint from TestBeadShow_FormulaRecipe_NoExecutionChrome:
// a pristine recipe must not surface any "step-status" substring, so the step
// list's run badge is class "formula-step-run", not "step-status".
func TestBeadShow_FormulaRecipe_StepListVisible(t *testing.T) {
	f := newFake()
	f.beads["gd-recipe2"] = &beadsapi.BeadDetail{
		ID:     "gd-recipe2",
		Title:  "Maps Beauty CLUB Audit",
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			"vars": `[{"name":"iteration","description":"audit iteration"}]`,
			"steps": `[
				{"id":"boot-and-warmup","type":"task","title":"Boot and warm up the viewer"},
				{"id":"capture","type":"task","title":"Capture screenshots + DOM","depends_on":["boot-and-warmup"]},
				{"id":"measure","type":"task","title":"Compute objective metrics","depends_on":["capture"]},
				{"id":"report","type":"task","title":"Publish roll-up report","depends_on":["measure"]}
			]`,
			// No step_status_* fields — pristine recipe, no run chrome.
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-recipe2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The step list must render OPEN (visible content), not a collapsed
	// disclosure — that is what fills the whitespace band.
	if !strings.Contains(body, `<details class="formula-step-list" open>`) {
		t.Errorf("step list should render as an OPEN <details> to fill the pane\nbody: %s", body)
	}
	// The step ids + titles must actually appear as content rows.
	for _, want := range []string{
		"<summary>Steps (4)</summary>",
		"boot-and-warmup",
		"Boot and warm up the viewer",
		"report",
		"Publish roll-up report",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("step list missing %q\nbody: %s", want, body)
		}
	}

	// Guard the run-badge rename: a pristine recipe must carry no "step-status"
	// substring anywhere in its chrome (shared constraint with the
	// NoExecutionChrome test), and must use the "formula-step-run" class for
	// the (absent-here) run badge instead.
	chrome := body
	if before, _, ok := strings.Cut(body, `<details class="formula-raw-json">`); ok {
		chrome = before
	}
	if strings.Contains(chrome, "step-status") {
		t.Errorf("pristine recipe chrome must not contain \"step-status\"\nchrome: %s", chrome)
	}
}

// longRecipeDescription is a stand-in for a golden-recipe bead body: the real
// ones (ka-0000001189 etc.) carry 15-20k chars of env/tooling reference and
// phase instructions in the description field.
func longRecipeDescription() string {
	var b strings.Builder
	b.WriteString("## Golden recipe\n\n")
	for range 400 {
		b.WriteString("This is a long recipe instruction paragraph that fills the bead body. ")
	}
	return b.String()
}

// TestBeadShow_RecipeDescriptionBounded pins the ka-0000005286 fix: a formula's
// (or molecule's) huge golden-recipe description — 15-20k chars living in the
// bead body — renders inline BELOW the DAG/steps/exec and made the recipe pages
// tens of screens tall (the narrow formula-dag page measured ~26000px). The
// description block on these two page types must be capped to a scrollable box
// (max-height + overflow) so the page stays a few screens tall and the DAG stays
// the visual centre of gravity, with the full text still reachable by scrolling
// inside the box. Ordinary bead pages must NOT get the clamp.
func TestBeadShow_RecipeDescriptionBounded(t *testing.T) {
	desc := longRecipeDescription()
	f := newFake()
	f.beads["gd-recipe-long"] = &beadsapi.BeadDetail{
		ID:     "gd-recipe-long",
		Title:  "Golden recipe with a long body",
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			"steps": `[{"id":"one","type":"task","title":"Step one"}]`,
		},
		Description: desc,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/gd-recipe-long", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The clamp is scoped to recipe pages via :has(.formula-card)/(.molecule-card)
	// on the description block, and uses a scrollable max-height box.
	for _, want := range []string{
		"body:has(.formula-card) #description-block .description",
		"body:has(.molecule-card) #description-block .description",
		"max-height: min(75vh, 44rem);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recipe page missing description-clamp rule %q", want)
		}
	}
	// The full description text is still SERVED (only visually boxed, not
	// truncated) — a chunk of the long body must be present in the HTML.
	if !strings.Contains(body, "long recipe instruction paragraph") {
		t.Errorf("recipe description body should still render in full inside the box")
	}

	// A plain (non-recipe) bead with the same long body must NOT get the clamp
	// scoped to it — the :has() selectors only fire on formula/molecule cards,
	// and a task page renders neither, so the description stays unbounded.
	f.beads["gd-plain-long"] = &beadsapi.BeadDetail{
		ID:          "gd-plain-long",
		Title:       "Plain task with a long body",
		Type:        "task",
		Status:      "open",
		Description: desc,
	}
	req2 := httptest.NewRequest("GET", "/gd-plain-long", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	body2 := w2.Body.String()
	// The layout <style> block (with the :has(.formula-card) clamp rule) is
	// inlined into every page, so assert on the rendered CARD element, not the
	// bare class substring: a plain task must render neither recipe card, so the
	// :has() clamp never matches it.
	if strings.Contains(body2, `class="formula-card"`) || strings.Contains(body2, `class="molecule-card"`) {
		t.Fatalf("a plain task page unexpectedly rendered a recipe card")
	}
}

// TestBeadShow_MRType verifies the merge-request partial reads from raw
// fields with sensible fallbacks.
func TestBeadShow_MRType(t *testing.T) {
	f := newFake()
	f.beads["gd-mr"] = &beadsapi.BeadDetail{
		ID:        "gd-mr",
		Title:     "feat: foo",
		Type:      "merge-request",
		Status:    "open",
		CreatedBy: "alice",
		Fields: map[string]string{
			"mr_url":              "https://gitlab.com/x/y/-/merge_requests/42",
			"mr_state":            "opened",
			"pipeline_status":     "success",
			"source_branch":       "feat/foo",
			"target_branch":       "main",
			"approvals_required":  "1",
			"approvals_given":     "0",
			"last_commit_sha":     "abc123def",
			"last_commit_message": "feat: foo bar",
			"unresolved_threads":  "2",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-mr", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h3>Merge Request</h3>",
		"mr-state-opened",
		"opened",
		`href="https://gitlab.com/x/y/-/merge_requests/42"`,
		"pipe-success",
		"success",
		"<code>feat/foo</code>",
		"<code>main</code>",
		"alice",
		"0 / 1", // approvals
		"<code>abc123def</code>",
		"feat: foo bar",
		"2", // unresolved threads
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mr render missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_AdviceType verifies scope chips (with grouping prefix stripped)
// and hook configuration.
func TestBeadShow_AdviceType(t *testing.T) {
	f := newFake()
	f.beads["gd-adv"] = &beadsapi.BeadDetail{
		ID:     "gd-adv",
		Title:  "Use trace-id everywhere",
		Type:   "advice",
		Status: "open",
		Labels: []string{"g0:project:gardenio", "g1:role:crew", "inline:true"},
		Fields: map[string]string{
			"hook_trigger":  "PreToolUse",
			"hook_command":  "echo hi",
			"subscriptions": "Bash,Write",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-adv", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h3>Advice</h3>",
		"<code>project:gardenio</code>", // grouping prefix stripped
		"<code>role:crew</code>",
		"<code>PreToolUse</code>",
		"<code>echo hi</code>",
		"<code>Bash,Write</code>",
		"yes", // inline:true rendered
	} {
		if !strings.Contains(body, want) {
			t.Errorf("advice render missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_AgentType verifies the agent-card uses raw Fields lookups.
func TestBeadShow_AgentType(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       "crew-1",
			"role":        "crew",
			"agent_state": "running",
			"pod_phase":   "Running",
			"task_id":     "gd-task-9",
			"spawned_by":  "gardener-1",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h3>Agent</h3>",
		"<code>crew-1</code>",
		"agent-state-running",
		"Running",
		`href="/gd-task-9"`,
		"<code>gardener-1</code>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agent render missing %q\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_GenericFallback verifies that an unknown type falls through
// to the generic Fields dump without panic.
func TestBeadShow_GenericFallback(t *testing.T) {
	f := newFake()
	f.beads["gd-unk"] = &beadsapi.BeadDetail{
		ID:     "gd-unk",
		Title:  "unknown shape",
		Type:   "novel-type-not-handled",
		Status: "open",
		Fields: map[string]string{
			"foo": "bar",
			"baz": "qux",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-unk", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<h3 aria-level="2">Fields</h3>`,
		"foo",
		"bar",
		"baz",
		"qux",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generic fallback missing %q\nbody: %s", want, body)
		}
	}
	// Per-type sections should not appear.
	for _, bad := range []string{"<h3>Decision</h3>", "<h3>Schedule</h3>", "<h3>Formula</h3>", "<h3>Merge Request</h3>", "<h3>Agent</h3>", "<h3>Advice</h3>"} {
		if strings.Contains(body, bad) {
			t.Errorf("generic fallback unexpectedly rendered %q\nbody: %s", bad, body)
		}
	}
}

// TestBeadShow_DecisionType_NoFieldsDoesNotPanic verifies a decision bead with
// an empty Fields map renders the placeholder without panicking.
func TestBeadShow_DecisionType_NoFieldsDoesNotPanic(t *testing.T) {
	f := newFake()
	f.beads["gd-dec-bare"] = &beadsapi.BeadDetail{
		ID:     "gd-dec-bare",
		Title:  "bare decision",
		Type:   "decision",
		Status: "open",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-dec-bare", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<h3>Decision</h3>") {
		t.Errorf("expected decision header even on bare bead\nbody: %s", body)
	}
}

// TestBuildScheduleView_PrefersGardenerSpecLastAgent verifies the gardener
// spec's engine-owned run bookkeeping wins over the stale top-level
// last_agent_id / runs_spawned / runs_completed fields (bug ka-0000002119).
// The gardener evaluator stamps the latest run onto the embedded `gardener`
// spec each fire, while the top-level fields are a legacy snapshot that lags
// behind — so reading the top-level last_agent_id showed an old iteration as
// the "last agent" while the spec carried the true latest run.
func TestBuildScheduleView_PrefersGardenerSpecLastAgent(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:   "gd-cap",
		Type: "trigger",
		Fields: map[string]string{
			"cron":           "0 * * * *",
			"enabled":        "true",
			"last_agent_id":  "gd-STALE",
			"runs_spawned":   "10",
			"runs_completed": "9",
			"gardener": `{"name":"cap","last_agent_id":"gd-TRUE",` +
				`"runs_spawned":94,"runs_completed":90}`,
		},
	}
	v := buildScheduleView(bead, time.Now())
	if v == nil {
		t.Fatal("expected non-nil scheduleView")
	}
	if v.LastAgentID != "gd-TRUE" {
		t.Errorf("LastAgentID = %q, want spec value gd-TRUE (not stale top-level gd-STALE)", v.LastAgentID)
	}
	if v.RunsSpawned != "94" {
		t.Errorf("RunsSpawned = %q, want spec value 94", v.RunsSpawned)
	}
	if v.RunsCompleted != "90" {
		t.Errorf("RunsCompleted = %q, want spec value 90", v.RunsCompleted)
	}
}

// TestBuildScheduleView_FallsBackToTopLevelWithoutSpec verifies a plain
// legacy-scheduler type=schedule bead (no embedded gardener spec) still reads
// its run bookkeeping from the top-level fields.
func TestBuildScheduleView_FallsBackToTopLevelWithoutSpec(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:   "gd-sched",
		Type: "schedule",
		Fields: map[string]string{
			"cron":           "0 * * * *",
			"enabled":        "true",
			"last_agent_id":  "gd-TOP",
			"runs_spawned":   "3",
			"runs_completed": "2",
		},
	}
	v := buildScheduleView(bead, time.Now())
	if v == nil {
		t.Fatal("expected non-nil scheduleView")
	}
	if v.LastAgentID != "gd-TOP" {
		t.Errorf("LastAgentID = %q, want top-level gd-TOP", v.LastAgentID)
	}
	if v.RunsSpawned != "3" {
		t.Errorf("RunsSpawned = %q, want top-level 3", v.RunsSpawned)
	}
	if v.RunsCompleted != "2" {
		t.Errorf("RunsCompleted = %q, want top-level 2", v.RunsCompleted)
	}
}

// TestBuildScheduleView_SpecEmptyLastAgentKeepsTopLevel verifies a gardener
// spec with an empty LastAgentID does not blank out a non-empty top-level
// fallback — the spec only OVERRIDES when it carries a real value.
func TestBuildScheduleView_SpecEmptyLastAgentKeepsTopLevel(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:   "gd-cap2",
		Type: "trigger",
		Fields: map[string]string{
			"cron":          "0 * * * *",
			"enabled":       "true",
			"last_agent_id": "gd-TOP",
			"gardener":      `{"name":"cap"}`,
		},
	}
	v := buildScheduleView(bead, time.Now())
	if v == nil {
		t.Fatal("expected non-nil scheduleView")
	}
	if v.LastAgentID != "gd-TOP" {
		t.Errorf("LastAgentID = %q, want top-level gd-TOP (spec last_agent_id empty)", v.LastAgentID)
	}
}

// TestBeadShow_AgentType_ContextGauge verifies the always-visible context-window
// gauge (ka-0000006399): a type=agent bead reporting context_tokens renders the
// labeled progress bar in the card header (and again beside the Compact button),
// colored by the shared ContextLevel band. Operator asked to front context "in
// an obvious way", replacing the small chip with a gauge.
func TestBeadShow_AgentType_ContextGauge(t *testing.T) {
	f := newFake()
	f.beads["gd-agtg"] = &beadsapi.BeadDetail{
		ID:     "gd-agtg",
		Title:  "crew agent",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{
			"agent":          "crew-alpha",
			"agent_state":    "working",
			"role":           "crew",
			"project":        "gardenio",
			"context_tokens": "128000", // → 76% of the ~167k threshold (warn band)
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agtg", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"agent-ctx-gauge",      // gauge container
		"agent-ctx-gauge-warn", // 76% → warn band
		`aria-valuenow="76"`,   // clamped bar percent
		"width:76%",            // fill width
		"128.0k · 76%",         // humanized label + percent in the gauge head
		`role="meter"`,         // accessible meter role
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agent context gauge render missing %q\nbody: %s", want, body)
		}
	}
	// The gauge should render twice: once in the header, once beside Compact.
	if n := strings.Count(body, "agent-ctx-gauge-warn"); n < 2 {
		t.Errorf("expected the gauge to render in both the header and the Compact block, got %d", n)
	}
}
