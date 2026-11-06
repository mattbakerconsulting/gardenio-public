package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleQuickSpawnSheet_RendersForm(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio":    {Name: "gardenio"},
		"kbeads":      {Name: "kbeads"},
		"site-deploy": {Name: "site-deploy"},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/quick-spawn", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-post="/quick-spawn"`,
		`name="project"`,
		`name="role"`,
		`name="prompt"`,
		`name="task_id"`,
		`>gardenio<`,
		`>kbeads<`,
		`>site-deploy<`,
		`Spawn Agent`,
		`>crew<`,
		`>gardener<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}
}

func TestHandleQuickSpawnSheet_PreselectsProjectQueryParam(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"kbeads":   {Name: "kbeads"},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/quick-spawn?project=kbeads&task=gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `value="kbeads" selected`) {
		t.Errorf("expected kbeads to be pre-selected; body=%s", body)
	}
	if !strings.Contains(body, `value="gd-aaa"`) {
		t.Errorf("expected task id to be pre-filled; body=%s", body)
	}
}

// TestHandleQuickSpawnSheet_AppliesUserPreferences verifies that the rendered
// modal pre-fills project/role/prompt from the operator's user-bead spawn
// prefs (gd-k4t3aid4QE).
func TestHandleQuickSpawnSheet_AppliesUserPreferences(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"foo":      {Name: "foo"},
	}
	prefsJSON := `{"default_spawn_project":"foo","default_spawn_role":"gardener",` +
		`"default_spawn_prompt_template":"do X"}`
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:    "gd-user-alice",
				Type:  "user",
				Kind:  "config",
				Title: "Alice",
				Fields: map[string]string{
					"email":       "alice@example.com",
					"preferences": prefsJSON,
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/quick-spawn", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `value="foo" selected`) {
		t.Errorf("expected project=foo to be pre-selected from prefs; body=%s", body)
	}
	if !strings.Contains(body, `value="gardener" selected`) {
		t.Errorf("expected role=gardener to be pre-selected from prefs; body=%s", body)
	}
	if !strings.Contains(body, `>do X</textarea>`) {
		t.Errorf("expected prompt textarea to be pre-filled with template; body=%s", body)
	}
}

// TestHandleQuickSpawnSheet_URLParamsOverridePrefs verifies that explicit
// ?project= URL params override the user-bead default_spawn_project pref.
func TestHandleQuickSpawnSheet_URLParamsOverridePrefs(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"foo":      {Name: "foo"},
		"kbeads":   {Name: "kbeads"},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:    "gd-user-alice",
				Type:  "user",
				Kind:  "config",
				Title: "Alice",
				Fields: map[string]string{
					"email":       "alice@example.com",
					"preferences": `{"default_spawn_project":"foo"}`,
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/quick-spawn?project=kbeads", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `value="kbeads" selected`) {
		t.Errorf("expected URL ?project=kbeads to override prefs; body=%s", body)
	}
	if strings.Contains(body, `value="foo" selected`) {
		t.Errorf("did not expect prefs project=foo to win over URL param; body=%s", body)
	}
}

// TestHandleQuickSpawnSheet_PromptTemplateRendersAgainstTask verifies that
// when ?task= and a {{title}}/{{id}} template pref are both present, the
// template is rendered against the selected bead before being placed in the
// textarea.
func TestHandleQuickSpawnSheet_PromptTemplateRendersAgainstTask(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:    "gd-user-alice",
				Type:  "user",
				Kind:  "config",
				Title: "Alice",
				Fields: map[string]string{
					"email":       "alice@example.com",
					"preferences": `{"default_spawn_prompt_template":"do X for {{title}} ({{id}})"}`,
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/quick-spawn?task=gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "do X for Example bead (gd-aaa)") {
		t.Errorf("expected rendered prompt template in textarea; body=%s", body)
	}
}

func TestHandleQuickSpawnSheet_Unauthenticated_Returns401(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/quick-spawn", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleQuickSpawn_HappyPath(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newagent"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("role", "crew")
	form.Set("prompt", "Investigate the noisy alert from yesterday")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", f.createCalls)
	}
	got := f.createLastReq
	if got.Type != "agent" {
		t.Errorf("Type = %q, want agent", got.Type)
	}
	if got.CreatedBy != "alice@example.com" {
		t.Errorf("CreatedBy = %q, want alice@example.com", got.CreatedBy)
	}
	// kbeads R2 (ka-0000005772) fail-closes write paths if the daemon ctx
	// doesn't carry an operator. The handler must wrap r.Context() with
	// beadsapi.WithOperator("oidc:<email>") before calling CreateBead;
	// otherwise the daemon returns HTTP 400 "operator could not be resolved"
	// and the spawn-agent button looks broken to the user.
	if got, want := f.createLastOpBy, "oidc:alice@example.com"; got != want {
		t.Errorf("CreateBead ctx operator = %q, want %q (kbeads R2 enforcement)", got, want)
	}
	if !strings.HasPrefix(got.Title, "gardenio-investigate-the-noisy-alert-") {
		t.Errorf("Title = %q, expected gardenio-{slug}-{hex} prefix", got.Title)
	}
	if !strings.Contains(got.Description, "Investigate the noisy alert") {
		t.Errorf("Description should contain prompt; got %q", got.Description)
	}

	var fields map[string]string
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("Fields unmarshal: %v", err)
	}
	if fields["project"] != "gardenio" || fields["role"] != "crew" || fields["mode"] != "crew" {
		t.Errorf("fields = %v, want project/role/mode set correctly", fields)
	}
	if fields["prompt"] != "Investigate the noisy alert from yesterday" {
		t.Errorf("fields[prompt] = %q", fields["prompt"])
	}
	if fields["agent"] != got.Title {
		t.Errorf("fields[agent] %q must equal Title %q", fields["agent"], got.Title)
	}

	wantLabels := []string{"project:gardenio", "role:crew"}
	if len(f.addLabelAll) != len(wantLabels) {
		t.Fatalf("AddLabel calls = %v, want %v", f.addLabelAll, wantLabels)
	}
	for i, l := range wantLabels {
		if f.addLabelAll[i] != l {
			t.Errorf("AddLabel[%d] = %q, want %q", i, f.addLabelAll[i], l)
		}
	}
	if f.addLabelLastBead != "gd-newagent" {
		t.Errorf("AddLabel target = %q, want gd-newagent", f.addLabelLastBead)
	}

	if f.addDepCalls != 0 {
		t.Errorf("expected no AddDependency call when task_id empty, got %d", f.addDepCalls)
	}

	body := w.Body.String()
	if !strings.Contains(body, "gd-newagent") {
		t.Errorf("result body missing bead id; body=%s", body)
	}
	if !strings.Contains(body, `href="/gd-newagent"`) {
		t.Errorf("result body missing link to new bead; body=%s", body)
	}
}

func TestHandleQuickSpawn_WithTaskID_AddsAssignedDependency(t *testing.T) {
	f := newFake()
	f.createResp = "gd-agent2"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("role", "gardener")
	form.Set("prompt", "drive epic to land")
	form.Set("task_id", "gd-aaa")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "bob@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addDepCalls != 1 {
		t.Fatalf("expected 1 AddDependency call, got %d", f.addDepCalls)
	}
	if f.addDepLastBead != "gd-agent2" || f.addDepLastDep != "gd-aaa" || f.addDepLastType != "assigned" {
		t.Errorf("AddDependency = (%q, %q, %q), want (gd-agent2, gd-aaa, assigned)",
			f.addDepLastBead, f.addDepLastDep, f.addDepLastType)
	}

	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["task_id"] != "gd-aaa" {
		t.Errorf("fields[task_id] = %q, want gd-aaa", fields["task_id"])
	}
	if fields["role"] != "gardener" {
		t.Errorf("fields[role] = %q, want gardener", fields["role"])
	}
}

func TestHandleQuickSpawn_DefaultsRoleToCrew(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("prompt", "do a thing")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["role"] != "crew" {
		t.Errorf("fields[role] = %q, want crew", fields["role"])
	}
}

func TestHandleQuickSpawn_MissingProject_RendersError(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("prompt", "do something")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Project is required") {
		t.Errorf("expected error message; body=%s", w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead call, got %d", f.createCalls)
	}
}

func TestHandleQuickSpawn_MissingPrompt_RendersError(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Prompt is required") {
		t.Errorf("expected error message; body=%s", w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead call, got %d", f.createCalls)
	}
}

func TestHandleQuickSpawn_Unauthenticated_Returns401(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("prompt", "do something")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
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

func TestHandleQuickSpawn_DaemonError_RendersFailure(t *testing.T) {
	f := newFake()
	f.createErr = &beadsapi.APIError{StatusCode: 500, Message: "boom"}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("prompt", "do something")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to spawn agent") {
		t.Errorf("expected failure message; body=%s", w.Body.String())
	}
}

// TestBeadShow_RendersInlineSpawnForm verifies that the bead detail page
// renders the inline "Spawn agent" form at the bottom, with the bead's id
// pre-filled in the task field.
func TestBeadShow_RendersInlineSpawnForm(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="quick-spawn-inline"`) {
		t.Errorf("expected inline spawn form section; body=%s", body)
	}
	if !strings.Contains(body, `name="display" value="inline"`) {
		t.Errorf("expected hidden display=inline field on inline form; body=%s", body)
	}
	if !strings.Contains(body, `value="gd-aaa"`) {
		t.Errorf("expected bead id pre-filled in inline task field; body=%s", body)
	}
}

// TestHandleQuickSpawn_InlineDisplayUsesInlineResultTemplate verifies that a
// POST /quick-spawn with display=inline returns the inline result section
// rather than the modal dialog markup.
func TestHandleQuickSpawn_InlineDisplayUsesInlineResultTemplate(t *testing.T) {
	f := newFake()
	f.createResp = "gd-inline-agent"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("role", "crew")
	form.Set("prompt", "Investigate inline render path")
	form.Set("display", "inline")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="quick-spawn-inline"`) {
		t.Errorf("expected inline result section; body=%s", body)
	}
	if strings.Contains(body, `class="quick-spawn-sheet"`) {
		t.Errorf("inline result should not render modal sheet; body=%s", body)
	}
	if !strings.Contains(body, "gd-inline-agent") {
		t.Errorf("expected new bead id in body; body=%s", body)
	}
}

// TestHandleQuickSpawnInline_GET_RendersForm verifies the standalone
// /quick-spawn-inline GET route returns just the inline form fragment.
func TestHandleQuickSpawnInline_GET_RendersForm(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/quick-spawn-inline?task=gd-bbb", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="quick-spawn-inline"`) {
		t.Errorf("expected inline form section; body=%s", body)
	}
	if !strings.Contains(body, `value="gd-bbb"`) {
		t.Errorf("expected task pre-filled from query; body=%s", body)
	}
	if !strings.Contains(body, `name="display" value="inline"`) {
		t.Errorf("expected hidden display=inline field; body=%s", body)
	}
}

func TestLayout_NavHasNewAgentButton(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "New Agent") {
		t.Errorf("expected New Agent button in nav; body=%s", body)
	}
	if !strings.Contains(body, `hx-get="/quick-spawn"`) {
		t.Errorf("expected nav button to hx-get /quick-spawn; body=%s", body)
	}
	if !strings.Contains(body, `id="quick-spawn-host"`) {
		t.Errorf("expected #quick-spawn-host container in layout; body=%s", body)
	}
}

// TestLayout_NewAgentButtonPrefillsTaskFromBeadShow verifies that when the
// operator is viewing a bead, the nav "+ New Agent" button's hx-get URL
// includes ?task=<bead-id> so the spawn modal pre-fills the task field
// (gd-vGECU4PMv4).
func TestLayout_NewAgentButtonPrefillsTaskFromBeadShow(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-get="/quick-spawn?task=gd-aaa&project=gardenio"`) {
		t.Errorf("expected nav button to prefill task and project from bead show; body=%s", body)
	}
}

// TestHandleQuickSpawn_MultipleTaskIDs_SingleMode_AddsDepPerTask verifies the
// spawn-from-selection happy path with `spawn_per_selection=single`: one agent
// bead, N `assigned` deps, one per selected task.
func TestHandleQuickSpawn_MultipleTaskIDs_SingleMode_AddsDepPerTask(t *testing.T) {
	f := newFake()
	f.createResp = "gd-multi-agent"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("role", "crew")
	form.Set("prompt", "Triage a batch of related bugs")
	form.Set("spawn_per_selection", "single")
	form["task_ids[]"] = []string{"gd-aaa", "gd-bbb", "gd-ccc"}
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", f.createCalls)
	}
	if f.addDepCalls != 3 {
		t.Errorf("expected 3 AddDependency calls (one per selected task), got %d", f.addDepCalls)
	}
	// Per-call dep type/source must be (agent_id, task, "assigned").
	if f.addDepLastBead != "gd-multi-agent" || f.addDepLastType != "assigned" {
		t.Errorf("AddDependency target/type = (%q, %q), want (gd-multi-agent, assigned)",
			f.addDepLastBead, f.addDepLastType)
	}

	// task_id field is omitted when multiple selected — dep edges are
	// canonical for multi-task assignment.
	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if _, has := fields["task_id"]; has {
		t.Errorf("fields[task_id] should be unset for multi-task spawn; got %q", fields["task_id"])
	}
	if !strings.Contains(f.createLastReq.Description, "gd-aaa") ||
		!strings.Contains(f.createLastReq.Description, "gd-bbb") ||
		!strings.Contains(f.createLastReq.Description, "gd-ccc") {
		t.Errorf("description should list all selected tasks; got %q", f.createLastReq.Description)
	}
}

// TestHandleQuickSpawn_MultipleTaskIDs_PerBeadMode_SpawnsNAgents verifies that
// `spawn_per_selection=per-bead` produces N agent beads, each with one
// `assigned` dep.
func TestHandleQuickSpawn_MultipleTaskIDs_PerBeadMode_SpawnsNAgents(t *testing.T) {
	f := newFake()
	f.createResp = "gd-per-bead-agent"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("role", "crew")
	form.Set("prompt", "Investigate selected bug")
	form.Set("spawn_per_selection", "per-bead")
	form["task_ids[]"] = []string{"gd-aaa", "gd-bbb"}
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 2 {
		t.Fatalf("expected 2 CreateBead calls (one per selected task), got %d", f.createCalls)
	}
	if f.addDepCalls != 2 {
		t.Errorf("expected 2 AddDependency calls (one per agent), got %d", f.addDepCalls)
	}
	// Each per-bead agent gets project: + role: labels — 2 agents × 2 labels.
	if len(f.addLabelAll) != 4 {
		t.Errorf("expected 4 AddLabel calls (project+role per agent, 2 agents); got %d (%v)",
			len(f.addLabelAll), f.addLabelAll)
	}

	// Result body should mention both agents (multi-render path).
	body := w.Body.String()
	if !strings.Contains(body, "Spawned <strong>2</strong> agents") {
		t.Errorf("expected multi-spawn result body; body=%s", body)
	}
}

// TestHandleQuickSpawn_PreferencesFallback_FillsMissingFields verifies that
// when project / role / prompt / spawn_per_selection are missing on the form,
// values are pulled from the operator's user-bead preferences.
func TestHandleQuickSpawn_PreferencesFallback_FillsMissingFields(t *testing.T) {
	f := newFake()
	f.createResp = "gd-prefs-agent"
	// User bead with preferences for the operator's email.
	prefsJSON := `{"default_spawn_project":"kbeads","default_spawn_role":"gardener",` +
		`"default_spawn_prompt_template":"Please look at {{title}} ({{id}})",` +
		`"spawn_per_selection":"single"}`
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:    "gd-user-alice",
				Type:  "user",
				Kind:  "config",
				Title: "Alice",
				Fields: map[string]string{
					"email":       "alice@example.com",
					"preferences": prefsJSON,
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	form := url.Values{}
	// No project / role / prompt / spawn_per_selection — all from prefs.
	form["task_ids[]"] = []string{"gd-aaa"}
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", f.createCalls)
	}

	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["project"] != "kbeads" {
		t.Errorf("fields[project] = %q, want kbeads (from prefs)", fields["project"])
	}
	if fields["role"] != "gardener" {
		t.Errorf("fields[role] = %q, want gardener (from prefs)", fields["role"])
	}
	// Template substitution: "{{title}}" → "Example bead", "{{id}}" → "gd-aaa".
	wantPrompt := "Please look at Example bead (gd-aaa)"
	if fields["prompt"] != wantPrompt {
		t.Errorf("fields[prompt] = %q, want %q", fields["prompt"], wantPrompt)
	}

	// project: + role: labels reflect prefs.
	wantLabels := []string{"project:kbeads", "role:gardener"}
	if len(f.addLabelAll) != len(wantLabels) {
		t.Fatalf("AddLabel calls = %v, want %v", f.addLabelAll, wantLabels)
	}
	for i, l := range wantLabels {
		if f.addLabelAll[i] != l {
			t.Errorf("AddLabel[%d] = %q, want %q", i, f.addLabelAll[i], l)
		}
	}

	// Title prefix must reflect the prefs-derived project.
	if !strings.HasPrefix(f.createLastReq.Title, "kbeads-") {
		t.Errorf("agent name should start with kbeads-, got %q", f.createLastReq.Title)
	}
}

// TestHandleQuickSpawn_PreferencesFallback_PerBeadMode verifies that
// spawn_per_selection=per-bead pulled from prefs produces N agents.
func TestHandleQuickSpawn_PreferencesFallback_PerBeadMode(t *testing.T) {
	f := newFake()
	f.createResp = "gd-pf-agent"
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:    "gd-user-bob",
				Type:  "user",
				Kind:  "config",
				Title: "Bob",
				Fields: map[string]string{
					"email":       "bob@example.com",
					"preferences": `{"spawn_per_selection":"per-bead"}`,
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("project", "gardenio")
	form.Set("role", "crew")
	form.Set("prompt", "Look at this batch")
	form["task_ids[]"] = []string{"gd-aaa", "gd-bbb"}
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "bob@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 2 {
		t.Errorf("expected 2 CreateBead calls (per-bead from prefs), got %d", f.createCalls)
	}
}

// TestHandleQuickSpawn_PreferencesFallback_MissingFieldStillErrors verifies
// that when no user bead matches and required fields are missing, the
// existing error path still fires.
func TestHandleQuickSpawn_PreferencesFallback_MissingFieldStillErrors(t *testing.T) {
	f := newFake()
	// Empty user list — lookup yields no prefs.
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	form := url.Values{}
	// project missing AND no prefs → 400.
	form.Set("prompt", "do something")
	req := httptest.NewRequest("POST", "/quick-spawn", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "stranger@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead, got %d", f.createCalls)
	}
}

// TestCollectTaskIDs_DedupsAndPreservesOrder verifies the helper that merges
// repeated `task_ids[]` and the legacy `task_id` form param.
func TestCollectTaskIDs_DedupsAndPreservesOrder(t *testing.T) {
	cases := []struct {
		name   string
		multi  []string
		single string
		want   []string
	}{
		{"empty", nil, "", nil},
		{"only legacy", nil, "gd-aaa", []string{"gd-aaa"}},
		{"only multi", []string{"gd-aaa", "gd-bbb"}, "", []string{"gd-aaa", "gd-bbb"}},
		{"both, legacy distinct", []string{"gd-aaa"}, "gd-bbb", []string{"gd-aaa", "gd-bbb"}},
		{"both, legacy duplicate of multi", []string{"gd-aaa"}, "gd-aaa", []string{"gd-aaa"}},
		{"trims and ignores empties", []string{"  gd-aaa ", "", " "}, "  ", []string{"gd-aaa"}},
		{"dedups across multi", []string{"gd-aaa", "gd-aaa", "gd-bbb"}, "", []string{"gd-aaa", "gd-bbb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectTaskIDs(tc.multi, tc.single)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Errorf("got[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

// TestRenderPromptTemplate_SubstitutesTitleAndID verifies the simple
// {{title}} / {{id}} substitution path.
func TestRenderPromptTemplate_SubstitutesTitleAndID(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-aaa", Title: "Hello world"}
	cases := []struct {
		in, want string
	}{
		{"investigate {{title}}", "investigate Hello world"},
		{"link to {{id}}", "link to gd-aaa"},
		{"{{id}}: {{title}}", "gd-aaa: Hello world"},
		{"no placeholders", "no placeholders"},
		{"{{title}} and {{title}} again", "Hello world and Hello world again"},
	}
	for _, c := range cases {
		got := renderPromptTemplate(c.in, bead)
		if got != c.want {
			t.Errorf("renderPromptTemplate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello, World!", "hello-world"},
		{"  multiple   spaces  ", "multiple-spaces"},
		{"already-slug", "already-slug"},
		{"!!!", ""},
		{"UPPER_under-mixed/123", "upper-under-mixed-123"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGenerateAgentName_FormatAndLength(t *testing.T) {
	name, err := generateAgentName("gardenio", "this is a fairly long prompt that should be truncated to thirty chars")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "gardenio-") {
		t.Errorf("expected gardenio- prefix, got %q", name)
	}
	parts := strings.Split(name, "-")
	suffix := parts[len(parts)-1]
	if len(suffix) != 4 {
		t.Errorf("expected 4-hex suffix, got %q (full=%q)", suffix, name)
	}

	short, err := generateAgentName("kbeads", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(short, "kbeads-agent-") {
		t.Errorf("expected fallback slug 'agent', got %q", short)
	}
}
