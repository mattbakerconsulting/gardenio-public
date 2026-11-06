package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleCreateGardenerSheet_RendersForm(t *testing.T) {
	f := newFake()
	f.beads["gd-epic1"] = &beadsapi.BeadDetail{
		ID:     "gd-epic1",
		Title:  "Build the rocket",
		Type:   "epic",
		Status: "open",
		Labels: []string{"project:gardenio"},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/create-gardener?epic=gd-epic1", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-post="/create-gardener"`,
		`name="epic_id"`,
		`value="gd-epic1"`,
		`name="project"`,
		`name="mentor_id"`,
		`name="prompt"`,
		`Author gardener`,
		`Build the rocket`,
		`Drive epic gd-epic1`,
		// Rich-form knobs (I2.1): preset picker, schedule, terminal goal,
		// budget/bounds.
		`name="preset"`,
		`name="terminal_goal"`,
		`name="cron"`,
		`name="max_runs"`,
		`name="cooldown_minutes"`,
		`name="loop_wall_clock"`,
		// Preset + schedule catalogue entries.
		`Epic driver`,
		`Ready-queue drainer`,
		`Every 20 minutes`,
		`Daily at 09:00`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleCreateGardenerSheet_MissingEpic_Returns400(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/create-gardener", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleCreateGardenerSheet_Unauthenticated_Returns401(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/create-gardener?epic=gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateGardenerSheet_RendersExistingGardenersAsMentors(t *testing.T) {
	f := newFake()
	f.beads["gd-epic1"] = &beadsapi.BeadDetail{
		ID:     "gd-epic1",
		Title:  "Build the rocket",
		Type:   "epic",
		Status: "open",
		Labels: []string{"project:gardenio"},
	}
	// listGardenerMentors queries Types=[agent] across ALL statuses —
	// gardener agent beads close themselves at gb stop, so excluding closed
	// would empty the dropdown in production. The picker shows in_progress
	// and open gardeners first; closed gardeners follow as dormant mentors.
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-cap1",
				Title:  "gardenio-prior-gardener-aaaa",
				Type:   "agent",
				Kind:   "config",
				Status: "in_progress",
				Labels: []string{"role:gardener", "project:gardenio"},
				Fields: map[string]string{"task_id": "gd-prior-epic"},
			},
			{
				ID:     "gd-cap2",
				Title:  "gardenio-dormant-gardener-bbbb",
				Type:   "agent",
				Kind:   "config",
				Status: "closed",
				Labels: []string{"role:gardener", "project:gardenio"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/create-gardener?epic=gd-epic1", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`value="gd-cap1"`,
		`gardenio-prior-gardener-aaaa`,
		`value="gd-cap2"`,
		`gardenio-dormant-gardener-bbbb`,
		`— no mentor —`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Verify the mentor query does NOT restrict by status — closed gardeners
	// must be selectable as mentors so the dropdown is populated in practice.
	var mentorQ *beadsapi.ListBeadsQuery
	for i := range f.listAllQ {
		q := &f.listAllQ[i]
		if slices.Contains(q.Types, "agent") && slices.Contains(q.Labels, "role:gardener") {
			mentorQ = q
			break
		}
	}
	if mentorQ == nil {
		t.Fatalf("expected a ListBeadsFiltered call with Types=[agent], Labels=[role:gardener]; got %+v", f.listAllQ)
	}
	if len(mentorQ.Statuses) != 0 {
		t.Errorf("mentor query must not filter by status (closed gardeners are valid mentors); got Statuses=%v", mentorQ.Statuses)
	}

	// Live mentors must come before dormant mentors in the rendered options.
	cap1 := strings.Index(body, `value="gd-cap1"`)
	cap2 := strings.Index(body, `value="gd-cap2"`)
	if cap1 < 0 || cap2 < 0 {
		t.Fatalf("expected both gd-cap1 and gd-cap2 in body")
	}
	if cap1 > cap2 {
		t.Errorf("expected in_progress mentor (gd-cap1) before closed mentor (gd-cap2); cap1=%d cap2=%d", cap1, cap2)
	}
}

func TestHandleCreateGardener_HappyPath_NoMentor(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newcap"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("epic_id", "gd-epic1")
	form.Set("prompt", "Drive the rocket epic to land")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", f.createCalls)
	}
	got := f.createLastReq
	// I2.1: the create form now authors a real type=trigger gardener LOOP, not a
	// one-shot type=agent spawn.
	if got.Type != "trigger" {
		t.Errorf("Type = %q, want trigger", got.Type)
	}
	if !strings.HasPrefix(got.Title, "gardenio-drive-the-rocket-") {
		t.Errorf("Title = %q, want gardenio-{slug}-{hex} prefix", got.Title)
	}
	if got.CreatedBy != "alice@example.com" {
		t.Errorf("CreatedBy = %q", got.CreatedBy)
	}
	if !strings.Contains(got.Description, "Gardener for epic: gd-epic1") {
		t.Errorf("description missing epic header; got %q", got.Description)
	}
	if strings.Contains(got.Description, "mentor:") {
		t.Errorf("description should not have mentor line when none chosen; got %q", got.Description)
	}
	if strings.Contains(got.Description, "mentor(") {
		t.Errorf("description should not have mentor mention when none chosen; got %q", got.Description)
	}

	// Labels are set on the create request (not via per-label AddLabel calls).
	wantLabels := []string{"trigger:schedule", "role:gardener", "project:gardenio", "gardener-epic:gd-epic1"}
	for _, want := range wantLabels {
		if !slices.Contains(got.Labels, want) {
			t.Errorf("create labels %v missing %q", got.Labels, want)
		}
	}

	// Fields carry mixed types (bools/ints/strings), so decode into RawMessage.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("Fields unmarshal: %v", err)
	}
	fieldStr := func(k string) string {
		var s string
		_ = json.Unmarshal(fields[k], &s)
		return s
	}
	if fieldStr("role") != "gardener" {
		t.Errorf("fields[role] = %q, want gardener", fieldStr("role"))
	}
	if fieldStr("action") != "spawn_agent" {
		t.Errorf("fields[action] = %q, want spawn_agent", fieldStr("action"))
	}
	if fieldStr("project") != "gardenio" {
		t.Errorf("fields[project] = %q, want gardenio", fieldStr("project"))
	}
	if string(fields["enabled"]) != "true" {
		t.Errorf("fields[enabled] = %s, want true", fields["enabled"])
	}
	if fieldStr("cron") == "" {
		t.Errorf("fields[cron] should be non-empty (staggered default)")
	}
	if _, ok := fields["mentor_bead_id"]; ok {
		t.Errorf("fields[mentor_bead_id] should be absent when no mentor; got %s", fields["mentor_bead_id"])
	}
	// The gardener loop blob must be present and bounded (run_budget + wall_clock).
	capBlob := fieldStr("gardener")
	if capBlob == "" {
		t.Fatalf("fields[gardener] loop blob must be present")
	}
	var spec struct {
		AssignBead   string          `json:"assign_bead"`
		TerminalGoal string          `json:"terminal_goal"`
		RunBudget    int             `json:"run_budget"`
		MaxRuns      int             `json:"max_runs"`
		Stop         json.RawMessage `json:"stop"`
	}
	if err := json.Unmarshal([]byte(capBlob), &spec); err != nil {
		t.Fatalf("gardener blob unmarshal: %v", err)
	}
	if spec.AssignBead != "gd-epic1" {
		t.Errorf("gardener assign_bead = %q, want gd-epic1", spec.AssignBead)
	}
	if spec.RunBudget <= 0 || spec.MaxRuns <= 0 {
		t.Errorf("gardener must be bounded: run_budget=%d max_runs=%d", spec.RunBudget, spec.MaxRuns)
	}
	if len(spec.Stop) == 0 {
		t.Errorf("gardener must carry a stop spec")
	}
	if spec.TerminalGoal == "" {
		t.Errorf("gardener must carry a terminal goal (defaulted from preset)")
	}

	// One dep: epic (assigned).
	if f.addDepCalls != 1 {
		t.Fatalf("expected 1 AddDependency (epic), got %d", f.addDepCalls)
	}
	if f.addDepLastBead != "gd-newcap" || f.addDepLastDep != "gd-epic1" || f.addDepLastType != "assigned" {
		t.Errorf("dep = (%q,%q,%q), want (gd-newcap, gd-epic1, assigned)",
			f.addDepLastBead, f.addDepLastDep, f.addDepLastType)
	}
}

// TestHandleCreateGardener_WritesNoLegacyCaptainTwins: /gardener/new stamps only
// the role:gardener / gardener-epic:<id> / `gardener` blob markers; the
// pre-rename role:captain / captain-epic:<id> / `captain` twins are retired
// (gd-5O9QUlA8zr phase 5).
func TestHandleCreateGardener_WritesNoLegacyCaptainTwins(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newcap"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("epic_id", "gd-epic1")
	form.Set("prompt", "Drive the rocket epic to land")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", f.createCalls)
	}
	got := f.createLastReq
	for _, want := range []string{"role:gardener", "gardener-epic:gd-epic1"} {
		if !slices.Contains(got.Labels, want) {
			t.Errorf("create labels %v missing %q", got.Labels, want)
		}
	}
	for _, l := range got.Labels {
		if l == "role:captain" || strings.HasPrefix(l, "captain-epic:") {
			t.Errorf("create labels %v carry retired legacy label %q", got.Labels, l)
		}
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("Fields unmarshal: %v", err)
	}
	var gardener string
	_ = json.Unmarshal(fields["gardener"], &gardener)
	if strings.TrimSpace(gardener) == "" {
		t.Fatal("fields[gardener] loop blob absent")
	}
	if _, ok := fields["captain"]; ok {
		t.Error("fields[captain] legacy twin must no longer be written")
	}
}

func TestHandleCreateGardener_WithMentor_AddsMentorMentionAndDep(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newcap"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("epic_id", "gd-epic1")
	form.Set("mentor_id", "gd-mentor")
	form.Set("prompt", "Drive the rocket epic to land")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got := f.createLastReq
	if !strings.Contains(got.Description, "mentor: gd-mentor") {
		t.Errorf("description should include 'mentor: gd-mentor' typed colon-line form; got %q", got.Description)
	}
	if !strings.Contains(got.Description, "mentor(gd-mentor)") {
		t.Errorf("description should include 'mentor(gd-mentor)' typed call form for constellation edge; got %q", got.Description)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("Fields unmarshal: %v", err)
	}
	fieldStr := func(k string) string {
		var s string
		_ = json.Unmarshal(fields[k], &s)
		return s
	}
	if fieldStr("mentor_bead_id") != "gd-mentor" {
		t.Errorf("fields[mentor_bead_id] = %q, want gd-mentor", fieldStr("mentor_bead_id"))
	}
	// The mentor reference must also appear in the prompt so each iteration's
	// spawn nudge instructs the gardener to read its mentor's history.
	if !strings.Contains(fieldStr("prompt"), "mentor(gd-mentor)") {
		t.Errorf("fields[prompt] should embed mentor(gd-mentor) so spawn nudge carries it; got %q", fieldStr("prompt"))
	}

	// Two dep calls: epic (assigned) + mentor (related).
	if f.addDepCalls != 2 {
		t.Fatalf("expected 2 AddDependency calls (epic+mentor), got %d", f.addDepCalls)
	}
}

// TestHandleCreateGardener_StandingPreset asserts the standing-quality preset
// path: the perpetual marker (perpetual:true label + gardener.standing) is
// applied so the engine treats the gardener as standing, and a custom cron the
// operator typed overrides the preset default.
func TestHandleCreateGardener_StandingPreset(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newcap"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("epic_id", "gd-epic1")
	form.Set("prompt", "Keep the home page beautiful forever")
	form.Set("preset", "standing-quality")
	form.Set("cron", "7,37 * * * *") // operator override of the preset cron
	form.Set("max_runs", "15")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got := f.createLastReq
	if !slices.Contains(got.Labels, "perpetual:true") {
		t.Errorf("standing preset must add perpetual:true label; got %v", got.Labels)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("Fields unmarshal: %v", err)
	}
	var cron string
	_ = json.Unmarshal(fields["cron"], &cron)
	if cron != "7,37 * * * *" {
		t.Errorf("operator cron override not honored; got %q", cron)
	}
	var capBlob string
	_ = json.Unmarshal(fields["gardener"], &capBlob)
	var spec struct {
		Standing  bool `json:"standing"`
		MaxRuns   int  `json:"max_runs"`
		RunBudget int  `json:"run_budget"`
	}
	if err := json.Unmarshal([]byte(capBlob), &spec); err != nil {
		t.Fatalf("gardener blob unmarshal: %v", err)
	}
	if !spec.Standing {
		t.Errorf("gardener.standing must be true for the standing preset")
	}
	if spec.MaxRuns != 15 || spec.RunBudget != 15 {
		t.Errorf("max_runs/run_budget = %d/%d, want 15/15 (operator override)", spec.MaxRuns, spec.RunBudget)
	}
}

// TestHandleCreateGardener_InvalidDuration rejects an unparseable wall-clock at
// 200 with an inline error (HTMX-swap-friendly) and never creates a bead.
func TestHandleCreateGardener_InvalidDuration(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("epic_id", "gd-epic1")
	form.Set("prompt", "drive it")
	form.Set("loop_wall_clock", "forever") // not a Go duration
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Loop wall-clock must be a Go duration") {
		t.Errorf("expected wall-clock validation error; body=%s", w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead on invalid duration, got %d", f.createCalls)
	}
}

func TestHandleCreateGardener_DerivesProjectFromEpic(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newcap"
	f.beads["gd-epic1"] = &beadsapi.BeadDetail{
		ID:     "gd-epic1",
		Title:  "Build the rocket",
		Type:   "epic",
		Status: "open",
		Labels: []string{"project:kbeads"},
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("epic_id", "gd-epic1")
	form.Set("prompt", "Drive the rocket epic to land")
	// Note: project deliberately omitted.
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatalf("Fields unmarshal: %v", err)
	}
	var project string
	_ = json.Unmarshal(fields["project"], &project)
	if project != "kbeads" {
		t.Errorf("fields[project] = %q, want kbeads (derived from epic label)", project)
	}
	// The project: label must also be applied.
	if !slices.Contains(f.createLastReq.Labels, "project:kbeads") {
		t.Errorf("create labels %v missing project:kbeads", f.createLastReq.Labels)
	}
}

func TestHandleCreateGardener_MissingEpic_RendersError(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("prompt", "do a thing")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Validation errors render at 200 so HTMX swaps the inline error UI;
	// HTMX 2 default config does not swap on 4xx/5xx responses.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Epic id is required") {
		t.Errorf("expected error message; body=%s", w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead call, got %d", f.createCalls)
	}
}

func TestHandleCreateGardener_MissingPrompt_RendersError(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("epic_id", "gd-epic1")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Validation errors render at 200 so HTMX swaps the inline error UI.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Prompt is required") {
		t.Errorf("expected error message; body=%s", w.Body.String())
	}
}

func TestHandleCreateGardener_SpawnFailure_RendersErrorAt200(t *testing.T) {
	// When the upstream daemon refuses the create, the form must still render
	// an inline error to the operator. HTMX 2 silently drops 4xx/5xx swaps,
	// so the handler returns the error partial at 200 — the user sees the
	// failure instead of the modal silently appearing to "do nothing".
	f := newFake()
	f.createErr = errors.New("daemon exploded")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("epic_id", "gd-epic1")
	form.Set("prompt", "Drive the rocket epic to land")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-Groups", "operator")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (HTMX-swap-friendly), got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to author gardener") {
		t.Errorf("expected author-failure message; body=%s", w.Body.String())
	}
}

func TestHandleCreateGardener_Unauthenticated_Returns401(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	form := url.Values{}
	form.Set("epic_id", "gd-epic1")
	form.Set("prompt", "do something")
	req := httptest.NewRequest("POST", "/create-gardener", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead, got %d", f.createCalls)
	}
}

func TestBeadShow_EpicHasCreateGardenerButton(t *testing.T) {
	f := newFake()
	f.beads["gd-epic1"] = &beadsapi.BeadDetail{
		ID:     "gd-epic1",
		Title:  "Build the rocket",
		Type:   "epic",
		Status: "open",
		Labels: []string{"project:gardenio"},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-epic1", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `+ Create gardener`) {
		t.Errorf("expected '+ Create gardener' button on epic show page; body=%s", body)
	}
	if !strings.Contains(body, `hx-get="/create-gardener?epic=gd-epic1"`) {
		t.Errorf("expected button to hx-get /create-gardener?epic=gd-epic1; body=%s", body)
	}
}

// TestListGardenerMentors_BatchesTaskTitleFetch asserts the N+1 refactor
// (gd-2f68XoUksO / MR C of perf epic ka-0000003045): listGardenerMentors must
// fetch all mentor task titles in a single BatchGetBeads round-trip rather
// than per-mentor GetBeads, and must preserve the "missing task -> empty
// TaskTitle" semantic of the old code.
func TestListGardenerMentors_BatchesTaskTitleFetch(t *testing.T) {
	f := newFake()
	// 10 mentors, each pointing at a distinct task_id; one task is missing
	// from the daemon to lock in the "missing task -> empty TaskTitle"
	// semantic.
	mentors := make([]*beadsapi.BeadDetail, 0, 10)
	for i := range 10 {
		taskID := fmt.Sprintf("gd-task%02d", i)
		mentors = append(mentors, &beadsapi.BeadDetail{
			ID:     fmt.Sprintf("gd-cap%02d", i),
			Title:  fmt.Sprintf("gardenio-gardener-%02d", i),
			Type:   "agent",
			Kind:   "config",
			Status: "in_progress",
			Labels: []string{"role:gardener", "project:gardenio"},
			Fields: map[string]string{"task_id": taskID},
		})
		if i == 7 {
			// Skip gd-task07 → "missing task -> empty TaskTitle".
			continue
		}
		f.beads[taskID] = &beadsapi.BeadDetail{
			ID:    taskID,
			Title: fmt.Sprintf("Drive epic %02d", i),
		}
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: mentors, Total: len(mentors)}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	out := srv.listGardenerMentors(context.Background())

	if len(out) != 10 {
		t.Fatalf("expected 10 mentor options, got %d", len(out))
	}
	if f.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1 (N+1 must be gone)", f.batchGetBeadsCalls)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("GetBead calls=%d, want 0 (per-mentor GetBead loop must be gone)", f.getBeadCalls)
	}

	// "Missing task -> empty TaskTitle" semantic preserved.
	// Find the mentor with TaskID=gd-task07 in the (status-sorted) output.
	var missing, present *gardenerMentorOption
	for i := range out {
		switch out[i].TaskID {
		case "gd-task07":
			missing = &out[i]
		case "gd-task00":
			present = &out[i]
		}
	}
	if missing == nil {
		t.Fatalf("expected mentor pointing at gd-task07 (missing) in output")
	}
	if missing.TaskTitle != "" {
		t.Errorf("missing task should leave TaskTitle empty; got %q", missing.TaskTitle)
	}
	if present == nil || present.TaskTitle != "Drive epic 00" {
		t.Errorf("present task should populate TaskTitle; got %+v", present)
	}
}

// TestListGardenerMentors_OneCallAtFullMentorCap asserts that even at the
// mentor-list ceiling (Limit=100), listGardenerMentors makes exactly ONE call to
// the daemon interface's BatchGetBeads method — the client-side sharding lives
// inside beadsapi.BatchGetBeads, which the fake stands in for, so the
// listGardenerMentors call site itself must not loop. Zero per-mentor GetBead
// calls either way.
//
// This asserts the CALL SITE does not loop, which holds at any cap. It used to
// double as "and sharding still collapses to one call site call" back when 100
// exceeded BatchGetBeadsMaxIDs=50; since the cap moved to 500 that second
// property is no longer exercised here — beadsapi's own shard tests own it.
func TestListGardenerMentors_OneCallAtFullMentorCap(t *testing.T) {
	f := newFake()
	const n = 100 // mentor query Limit
	mentors := make([]*beadsapi.BeadDetail, 0, n)
	for i := range n {
		taskID := fmt.Sprintf("gd-task%03d", i)
		mentors = append(mentors, &beadsapi.BeadDetail{
			ID:     fmt.Sprintf("gd-cap%03d", i),
			Title:  fmt.Sprintf("gardenio-gardener-%03d", i),
			Type:   "agent",
			Kind:   "config",
			Status: "in_progress",
			Labels: []string{"role:gardener", "project:gardenio"},
			Fields: map[string]string{"task_id": taskID},
		})
		f.beads[taskID] = &beadsapi.BeadDetail{ID: taskID, Title: fmt.Sprintf("epic %03d", i)}
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: mentors, Total: n}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	out := srv.listGardenerMentors(context.Background())

	if len(out) != n {
		t.Fatalf("expected %d mentor options, got %d", n, len(out))
	}
	// The fake represents the daemon interface (beadsapi.Client.BatchGetBeads);
	// sharding happens inside the real client, below this seam. From the call
	// site's view, it's exactly one batched call per listGardenerMentors call.
	if f.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1 (call site must not loop)", f.batchGetBeadsCalls)
	}
	if f.batchGetBeadsLastN != n {
		t.Errorf("BatchGetBeads last shard size=%d, want %d (all task_ids in one call)", f.batchGetBeadsLastN, n)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("GetBead calls=%d, want 0", f.getBeadCalls)
	}
	// Spot-check a title got hydrated.
	if out[0].TaskTitle == "" {
		t.Errorf("expected TaskTitle to be hydrated for first mentor; got empty")
	}
}

func TestBeadShow_NonEpicHasNoCreateGardenerButton(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `+ Create gardener`) {
		t.Errorf("did not expect '+ Create gardener' on non-epic bead; body=%s", body)
	}
}
