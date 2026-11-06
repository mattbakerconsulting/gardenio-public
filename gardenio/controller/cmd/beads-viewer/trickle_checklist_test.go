package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// boolPtr is a local helper for the tri-state Pass field.
func boolPtr(b bool) *bool { return &b }

// TestBuildTrickleChecklist_NilForNonMR pins AC-3: the section is
// merge-request-only. A nil view means the template {{with}} renders nothing,
// so no other bead-show view can break.
func TestBuildTrickleChecklist_NilForNonMR(t *testing.T) {
	for _, typ := range []string{"task", "epic", "bug", "agent", "advice", "bundle", ""} {
		bead := &beadsapi.BeadDetail{ID: "gd-x", Type: typ, Fields: map[string]string{}}
		if v := buildTrickleChecklist(bead, nil, "/b"); v != nil {
			t.Errorf("type %q: expected nil trickle view, got %+v", typ, v)
		}
	}
	if v := buildTrickleChecklist(nil, nil, "/b"); v != nil {
		t.Errorf("nil bead: expected nil trickle view, got %+v", v)
	}
}

// TestBuildTrickleChecklist_MRBaseBar asserts a bare MR bead (no gate output)
// renders the full canonical bar, all-pending, with the verdict suppressed.
func TestBuildTrickleChecklist_MRBaseBar(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-mr", Type: "merge-request", Fields: map[string]string{}}
	v := buildTrickleChecklist(bead, nil, "/b")
	if v == nil {
		t.Fatal("expected non-nil view for merge-request bead")
	}
	if v.GateRan {
		t.Error("GateRan should be false with no trickle_checklist field")
	}
	if v.HardTotal != 7 {
		t.Errorf("HardTotal = %d, want 7 (the canonical HARD affordances)", v.HardTotal)
	}
	if v.HardPass != 0 {
		t.Errorf("HardPass = %d, want 0 (no gate → all pending)", v.HardPass)
	}
	if len(v.Advisory) != 3 {
		t.Errorf("Advisory count = %d, want 3", len(v.Advisory))
	}
	for _, it := range v.HardItems {
		if it.Status != trickleStatusPending {
			t.Errorf("item %q status = %q, want pending", it.ID, it.Status)
		}
		if it.Glyph() != "☐" {
			t.Errorf("item %q glyph = %q, want ☐", it.ID, it.Glyph())
		}
	}
}

// TestBuildTrickleChecklist_GateApplied drives a full structured gate payload
// through the builder and asserts per-item status + the eligibility roll-up.
func TestBuildTrickleChecklist_GateApplied(t *testing.T) {
	gate := `{
	  "eligible": false,
	  "reason": "coverage bypass present; blocks eligibility",
	  "checklist": [
	    {"id": "code-review",  "kind": "HARD", "pass": true,  "evidence": "review-artifact gd-rev1"},
	    {"id": "coverage",     "kind": "HARD", "pass": false, "evidence": ".bypass set"},
	    {"id": "security",     "kind": "HARD", "pass": true},
	    {"id": "perf",         "kind": "HARD", "na": true},
	    {"id": "small-diff",   "kind": "HARD", "pass": true},
	    {"id": "rationale",    "kind": "HARD", "pass": true},
	    {"id": "provenance",   "kind": "HARD", "pass": true},
	    {"id": "perf-benchmark", "kind": "ADVISORY", "pass": true}
	  ]
	}`
	bead := &beadsapi.BeadDetail{
		ID:     "gd-mr",
		Type:   "merge-request",
		Fields: map[string]string{"trickle_checklist": gate},
	}
	v := buildTrickleChecklist(bead, nil, "/b")
	if v == nil {
		t.Fatal("expected non-nil view")
	}
	if !v.GateRan {
		t.Fatal("GateRan should be true when the field is present + parseable")
	}
	if v.Eligible {
		t.Error("Eligible should be false per the gate payload")
	}
	if v.Reason != "coverage bypass present; blocks eligibility" {
		t.Errorf("Reason = %q", v.Reason)
	}
	// 6 pass/na of 7 HARD (coverage failed).
	if v.HardPass != 6 || v.HardTotal != 7 {
		t.Errorf("HardPass/HardTotal = %d/%d, want 6/7", v.HardPass, v.HardTotal)
	}
	byID := map[string]trickleGateItem{}
	for _, it := range append(append([]trickleGateItem{}, v.HardItems...), v.Advisory...) {
		byID[it.ID] = it
	}
	if got := byID["coverage"].Status; got != trickleStatusFail {
		t.Errorf("coverage status = %q, want fail", got)
	}
	if got := byID["coverage"].Evidence; got != ".bypass set" {
		t.Errorf("coverage evidence = %q", got)
	}
	if got := byID["perf"].Status; got != trickleStatusNA {
		t.Errorf("perf status = %q, want na", got)
	}
	if got := byID["code-review"].Status; got != trickleStatusPass {
		t.Errorf("code-review status = %q, want pass", got)
	}
	if got := byID["perf-benchmark"].Status; got != trickleStatusPass {
		t.Errorf("perf-benchmark (advisory) status = %q, want pass", got)
	}
}

// TestBuildTrickleChecklist_UnparseableGate: a garbage field is treated as
// "gate did not run", never as a verdict — no phantom eligibility from noise.
func TestBuildTrickleChecklist_UnparseableGate(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-mr",
		Type:   "merge-request",
		Fields: map[string]string{"trickle_checklist": "not json {{{"},
	}
	v := buildTrickleChecklist(bead, nil, "/b")
	if v == nil {
		t.Fatal("expected non-nil view")
	}
	if v.GateRan {
		t.Error("GateRan should be false for an unparseable field")
	}
}

// TestApplyTrickleGate_UnknownItemSurfaced: a gate item whose id is not in the
// canonical bar is appended (never silently dropped) so a growing gate stays
// visible.
func TestApplyTrickleGate_UnknownItemSurfaced(t *testing.T) {
	items := trickleBarSpec()
	g := trickleGateJSON{Checklist: []trickleGateJSONItem{
		{ID: "brand-new-check", Kind: "HARD", Pass: boolPtr(false), Evidence: "e"},
	}}
	out := applyTrickleGate(items, g)
	var found bool
	for _, it := range out {
		if it.ID == "brand-new-check" {
			found = true
			if it.Status != trickleStatusFail {
				t.Errorf("unknown-item status = %q, want fail", it.Status)
			}
		}
	}
	if !found {
		t.Error("unknown gate item was dropped; it must be surfaced")
	}
}

// TestBuildTrickleChecklist_UnknownItemThroughBuilder pins that an unknown gate
// item survives the full builder path (not just applyTrickleGate in isolation)
// — guarding the load-bearing reassignment of applyTrickleGate's return.
func TestBuildTrickleChecklist_UnknownItemThroughBuilder(t *testing.T) {
	gate := `{"eligible":false,"reason":"r","checklist":[
	  {"id":"brand-new-check","kind":"HARD","pass":false,"evidence":"e"}
	]}`
	bead := &beadsapi.BeadDetail{
		ID: "gd-mr", Type: "merge-request",
		Fields: map[string]string{"trickle_checklist": gate},
	}
	v := buildTrickleChecklist(bead, nil, "/b")
	if v == nil {
		t.Fatal("expected non-nil view")
	}
	var found bool
	for _, it := range v.HardItems {
		if it.ID == "brand-new-check" {
			found = true
		}
	}
	if !found {
		t.Error("unknown gate item did not survive buildTrickleChecklist; the applyTrickleGate return was dropped")
	}
	// It counts toward HardTotal: 7 canonical + 1 unknown.
	if v.HardTotal != 8 {
		t.Errorf("HardTotal = %d, want 8 (7 canonical + 1 surfaced unknown)", v.HardTotal)
	}
}

// TestTrickleMergeStatusFromFields covers the live merge-status projection,
// including the draft override and the tri-state approved flag.
func TestTrickleMergeStatusFromFields(t *testing.T) {
	t.Run("draft override", func(t *testing.T) {
		m := trickleMergeStatusFromFields(map[string]string{
			"mr_state": "opened", "mr_draft": "true",
		})
		if m.State != "draft" {
			t.Errorf("State = %q, want draft", m.State)
		}
	})
	t.Run("approved known", func(t *testing.T) {
		m := trickleMergeStatusFromFields(map[string]string{"mr_approved": "true"})
		if !m.ApprovedKnown || !m.Approved {
			t.Errorf("ApprovedKnown/Approved = %v/%v, want true/true", m.ApprovedKnown, m.Approved)
		}
	})
	t.Run("approved absent", func(t *testing.T) {
		m := trickleMergeStatusFromFields(map[string]string{"mr_state": "opened"})
		if m.ApprovedKnown {
			t.Error("ApprovedKnown should be false when the field is absent")
		}
		if !m.Any() {
			t.Error("Any() should be true when state is set")
		}
	})
	t.Run("pipeline + conflicts", func(t *testing.T) {
		m := trickleMergeStatusFromFields(map[string]string{
			"mr_pipeline_status": "failed", "mr_has_conflicts": "true",
		})
		if m.Pipeline != "failed" || !m.HasConflicts {
			t.Errorf("Pipeline/HasConflicts = %q/%v", m.Pipeline, m.HasConflicts)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if trickleMergeStatusFromFields(map[string]string{}).Any() {
			t.Error("Any() should be false for an empty field map")
		}
	})
}

// TestTrickleAncestry_TicketFromParentDep: with no explicit field, the
// originating ticket falls back to the parent-child forward dependency.
func TestTrickleAncestry_TicketFromParentDep(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-mr", Type: "merge-request", Fields: map[string]string{}}
	deps := []beadsapi.Dependency{
		{BeadID: "gd-mr", DependsOnID: "gd-other", Type: "related"},
		{BeadID: "gd-mr", DependsOnID: "gd-ticket", Type: "parent-child"},
	}
	links := trickleAncestryFromBead(bead, deps, "/b")
	if len(links) == 0 {
		t.Fatal("expected an ancestry link from the parent-child dep")
	}
	if links[0].Text != "gd-ticket" || links[0].Href != "/b/gd-ticket" {
		t.Errorf("ticket link = %+v, want gd-ticket → /b/gd-ticket", links[0])
	}
}

// TestTrickleAncestry_ExplicitFieldsWin: explicit fields override the dep
// fallback and an http dark_mr_url renders as an external link.
func TestTrickleAncestry_ExplicitFieldsWin(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:   "gd-mr",
		Type: "merge-request",
		Fields: map[string]string{
			"originating_ticket": "gd-ticket9",
			"dark_mr_url":        "https://gitlab-dark/monorepo-dark/monorepo/-/merge_requests/42",
		},
	}
	// A parent dep exists but must be overridden by the explicit field.
	deps := []beadsapi.Dependency{{BeadID: "gd-mr", DependsOnID: "gd-wrong", Type: "parent-child"}}
	links := trickleAncestryFromBead(bead, deps, "/b")
	var ticket, dark *trickleAncestryLink
	for i := range links {
		switch links[i].Label {
		case "originating ticket":
			ticket = &links[i]
		case "dark MR":
			dark = &links[i]
		}
	}
	if ticket == nil || ticket.Text != "gd-ticket9" {
		t.Errorf("ticket link = %+v, want explicit gd-ticket9", ticket)
	}
	if dark == nil || !dark.External {
		t.Fatalf("dark MR link missing or not external: %+v", dark)
	}
	if dark.Text != "monorepo-dark/monorepo!42" {
		t.Errorf("dark MR text = %q, want monorepo-dark/monorepo!42", dark.Text)
	}
}

// TestShortenMRURL table-tests the URL shortener.
func TestShortenMRURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/2185", "Acme/Platform/gardenio!2185"},
		{"https://gitlab-dark/monorepo-dark/monorepo/-/merge_requests/42/", "monorepo-dark/monorepo!42"},
		{"https://example.com/no-mr-here", "https://example.com/no-mr-here"},
		{"garbage", "garbage"},
	}
	for _, c := range cases {
		if got := shortenMRURL(c.in); got != c.want {
			t.Errorf("shortenMRURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// parseTrickleTemplate parses the partial standalone (mirroring the
// constellation-test parse path) so the template render can be exercised
// without an HTTP round-trip.
func parseTrickleTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.New("").ParseFS(templateFS, "templates/partials/trickle_checklist.html")
	if err != nil {
		t.Fatalf("parse trickle_checklist template: %v", err)
	}
	return tmpl
}

// TestTrickleTemplate_RendersGrid renders the partial against a populated view
// and asserts the key rows/verdict/ancestry appear.
func TestTrickleTemplate_RendersGrid(t *testing.T) {
	tmpl := parseTrickleTemplate(t)
	gate := `{"eligible":true,"reason":"all HARD checks green","checklist":[
	  {"id":"code-review","kind":"HARD","pass":true},
	  {"id":"coverage","kind":"HARD","pass":true},
	  {"id":"security","kind":"HARD","pass":true},
	  {"id":"perf","kind":"HARD","na":true},
	  {"id":"small-diff","kind":"HARD","pass":true},
	  {"id":"rationale","kind":"HARD","pass":true},
	  {"id":"provenance","kind":"HARD","pass":true}
	]}`
	bead := &beadsapi.BeadDetail{
		ID:   "gd-mr",
		Type: "merge-request",
		Fields: map[string]string{
			"trickle_checklist":  gate,
			"mr_state":           "opened",
			"mr_pipeline_status": "success",
			"originating_ticket": "gd-ticket9",
		},
	}
	v := buildTrickleChecklist(bead, nil, "/b")
	page := struct{ TrickleChecklist *trickleChecklistView }{v}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "trickle_checklist", page); err != nil {
		t.Fatalf("execute trickle_checklist: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Trickle-eligibility bar",
		"Eligible to trickle",
		"Automated Claude review",
		"Provenance / linked beads",
		"Merge status",
		"Ancestry",
		"gd-ticket9",
		"HARD",
		"ADVISORY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n---\n%s", want, out)
		}
	}
}

// TestTrickleTemplate_NilViewNoOps confirms a nil view renders nothing — the
// AC-3 guarantee at the template layer.
func TestTrickleTemplate_NilViewNoOps(t *testing.T) {
	tmpl := parseTrickleTemplate(t)
	page := struct{ TrickleChecklist *trickleChecklistView }{nil}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "trickle_checklist", page); err != nil {
		t.Fatalf("execute trickle_checklist with nil view: %v", err)
	}
	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("nil view should render empty, got: %q", buf.String())
	}
}

// TestBeadShow_MRRendersTrickleChecklist is the end-to-end assertion: an MR
// bead served through the real /{id} route renders the trickle checklist
// section, and a non-MR bead does NOT (AC-1 + AC-3, full-page HTTP style).
func TestBeadShow_MRRendersTrickleChecklist(t *testing.T) {
	fake := newFake()
	fake.beads["gd-mr1"] = &beadsapi.BeadDetail{
		ID:     "gd-mr1",
		Title:  "Dark MR: peel of gd-parent",
		Kind:   "issue",
		Type:   "merge-request",
		Status: "open",
		Fields: map[string]string{
			"mr_url":             "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/9",
			"mr_state":           "opened",
			"mr_pipeline_status": "success",
		},
	}
	fake.deps["gd-mr1"] = []beadsapi.Dependency{
		{BeadID: "gd-mr1", DependsOnID: "gd-parent", Type: "parent-child"},
	}
	mux := mkServer(t, fake)

	// MR bead → section present.
	req := httptest.NewRequest("GET", "/gd-mr1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("MR bead: expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The <section> markup only appears when the view renders (the class name
	// alone lives in the always-present inline <style>, so assert on the opening
	// tag + section-only content).
	for _, want := range []string{`<section class="trickle-checklist"`, "Trickle-eligibility bar", "Automated Claude review", "Ancestry"} {
		if !strings.Contains(body, want) {
			t.Errorf("MR bead-show missing %q", want)
		}
	}

	// Non-MR bead → section absent (AC-3): the plain gd-aaa task must not grow
	// the checklist. Assert on the opening tag (the class name alone lives in the
	// shared <style> block on every page).
	req2 := httptest.NewRequest("GET", "/gd-aaa", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("task bead: expected 200, got %d", w2.Code)
	}
	if strings.Contains(w2.Body.String(), `<section class="trickle-checklist"`) {
		t.Error("non-MR bead should NOT render the trickle checklist section")
	}
}

// TestTrickleTemplate_PendingVerdict: no gate → the neutral "not yet evaluated"
// note, never a false "blocked".
func TestTrickleTemplate_PendingVerdict(t *testing.T) {
	tmpl := parseTrickleTemplate(t)
	bead := &beadsapi.BeadDetail{ID: "gd-mr", Type: "merge-request", Fields: map[string]string{}}
	v := buildTrickleChecklist(bead, nil, "/b")
	page := struct{ TrickleChecklist *trickleChecklistView }{v}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "trickle_checklist", page); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Not yet evaluated") {
		t.Errorf("expected pending verdict, got:\n%s", out)
	}
	if strings.Contains(out, "Not yet eligible") {
		t.Error("must not render a 'blocked' verdict when the gate never ran")
	}
}

// TestTrickleGate_CrossPackageRoundTrip pins the cross-package contract that the
// whole design hinges on: the pure gate (internal/bridge.EvaluateTrickleEligibility)
// serializes to exactly the JSON this viewer's parseTrickleGate/applyTrickleGate
// consume. It marshals a real decision, feeds it through the viewer field path,
// and asserts the per-item statuses land on the canonical bar rows. If the two
// halves ever drift (a renamed field, a changed pass tri-state), this fails.
func TestTrickleGate_CrossPackageRoundTrip(t *testing.T) {
	in := bridge.TrickleEligibilityInput{
		Provenance: bridge.TrickleProvenanceInput{
			Evaluated: true, MRURLSet: true, LinkedIssueBeadSet: true,
			OperatorTrailerSet: true, DarkMRAncestrySet: true,
		},
		SmallDiff: bridge.TrickleSmallDiffInput{
			Evaluated: true, NetLOC: 100, FilesChanged: 3, SingleConcern: true,
		},
		Dossier: bridge.TrickleDossierInput{
			Evaluated: true, Summary: "s", Why: "distinct why",
		},
		Coverage:   bridge.TrickleCoverageInput{Evaluated: true, ResultPresent: true, Pass: true},
		CodeReview: bridge.TrickleCodeReviewInput{Evaluated: true, Ran: true, ArtifactAttached: true},
		Security: bridge.SecurityGateInput{
			StatusChecks: map[string]string{
				"CI / Gitleaks": "success", "CI / Go-Vuln": "success",
				"CI / Cargo-Audit": "success", "CI / Cargo-Deny": "success",
				"CI / OSV-Scan": "success",
			},
			ScanBlock: bridge.SecurityScanBlock{Present: true, Entries: map[string]bridge.SecurityScanEntry{
				"gitleaks": {Scanner: "gitleaks", Version: "8.21.2", Result: "clean"},
			}},
			LockPins: map[string]string{"gitleaks": "8.21.2"},
			Tier:     bridge.SecurityReviewTier{Effort: "low"},
		},
		SecurityEvaluated: true,
		Perf:              bridge.TricklePerfInput{Evaluated: true, PerfSensitivePathsTouched: false},
	}
	dec := bridge.EvaluateTrickleEligibility(in)
	if !dec.Eligible {
		t.Fatalf("fixture should be eligible: %s", dec.Reason)
	}
	raw, err := json.Marshal(dec)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}

	// Feed the serialized decision through the viewer's on-bead field parser.
	gate, ran := parseTrickleGate(string(raw))
	if !ran {
		t.Fatal("viewer parseTrickleGate rejected the gate's own JSON output")
	}
	items := applyTrickleGate(trickleBarSpec(), gate)

	// Every canonical bar row must have a resolved (non-pending) status, proving
	// the IDs matched 1:1 — no drift between bridge item ids and trickleBarSpec.
	byID := map[string]string{}
	for _, it := range items {
		byID[it.ID] = it.Status
	}
	for _, id := range []string{"provenance", "small-diff", "coverage", "code-review", "security"} {
		if byID[id] != trickleStatusPass {
			t.Errorf("item %q status = %q, want pass (id mismatch or contract drift?)", id, byID[id])
		}
	}
	if byID["perf"] != trickleStatusNA {
		t.Errorf("perf status = %q, want na", byID["perf"])
	}
	// No unknown item was appended (every gate id is in the canonical bar).
	if len(items) != len(trickleBarSpec()) {
		t.Errorf("checklist grew to %d rows; a gate id is not in trickleBarSpec (drift)", len(items))
	}
}
