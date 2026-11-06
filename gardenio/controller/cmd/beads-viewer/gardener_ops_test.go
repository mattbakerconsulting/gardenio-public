package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedGardenerOpsFake builds a fakeDaemon with one open gardener trigger bead
// (gd-cap) carrying a spec with a still-running last iteration (gd-r3). It
// reuses the gardener-page seed shape so the ops handlers exercise the same
// fixture the read page does.
func seedGardenerOpsFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()
	spec := map[string]any{
		"name":           "delivery",
		"terminal_goal":  "drain the ready queue",
		"run_budget":     100,
		"max_runs":       10,
		"runs_spawned":   3,
		"runs_completed": 2,
		"last_agent_id":  "gd-r3",
		"stop":           map[string]any{"max_runs": 10},
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Delivery gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"gardener": string(raw),
			// Runtime backend opt-in: run-now must stamp this onto the
			// iteration exactly like a scheduled fire (ka-0000003026).
			"execution_target": "lambda_microvm",
		},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	// The in-flight iteration the cancel-run lever targets.
	f.beads["gd-r3"] = &beadsapi.BeadDetail{
		ID:     "gd-r3",
		Title:  "iteration 3",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"schedule_id": "gd-cap"},
	}
	return f
}

func opsReq(method, path string, form url.Values) *http.Request {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	return req
}

// adminOpsReq is opsReq carrying a gardenio-admin OIDC role: run-now / Wake spawn
// and then write the run counter back, which kbeads takes only from an admin
// session or the trigger engine (gd-uzCT6oXur9).
func adminOpsReq(method, path string, form url.Values) *http.Request {
	req := opsReq(method, path, form)
	return req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleAdmin}))
}

func TestGardenerPage_OperatorSeesControls(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := opsReq("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The operator control bar + each lever must render, all pointed at the
	// operator-gated routes.
	for _, want := range []string{
		`id="gardener-controls"`,
		"/gardener/gd-cap/disable",
		"/gardener/gd-cap/run-now",
		"/gardener/gd-cap/cancel-run",
		"/gardener/gd-cap/edit",
		"/gardener/gd-cap/retire",
		`id="gardener-config-host"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operator page missing %q", want)
		}
	}
}

// TestGardenerPage_RoleScopedControlBar locks the operator-gated contract that
// the gardener-page CAMP audit (metric control_bar_present, bug ka-0000007161)
// measures: an OPERATOR-scoped viewer sees the lifecycle control bar, a
// VIEWER-scoped one does NOT. The 2026-06-06 CAMP audits reported
// control_bar_present=0 even though the bar shipped (d99aeff1d) and deployed —
// the root cause was that the audit captured the page with a gardenio-viewer
// client_credentials token, so the operator-gated bar was correctly hidden and
// thus structurally unmeasurable. This test pins both directions so a future
// refactor of viewerCanOperate / the {{if .IsOperator}} gate cannot silently
// drop the bar for operators (re-introducing the original bug) or leak it to
// read-scoped viewers (breaking RBAC parity with the operatorOnly routes).
func TestGardenerPage_RoleScopedControlBar(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServerWithNow(t, f, gardenerPageNow)

	// Markers that exist ONLY inside the operator control bar partial.
	barMarkers := []string{
		`id="gardener-controls"`,
		"/gardener/gd-cap/disable",
		"/gardener/gd-cap/run-now",
		"/gardener/gd-cap/retire",
	}

	render := func(roles []string) string {
		t.Helper()
		req := opsReq("GET", "/gardener/gd-cap", nil)
		if roles != nil {
			req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), roles))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("roles=%v: expected 200, got %d body=%s", roles, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	// Operator (and admin, which outranks operator) MUST see the bar.
	for _, roles := range [][]string{
		{oidcauth.RoleOperator},
		{oidcauth.RoleAdmin},
		{oidcauth.RoleViewer, oidcauth.RoleOperator},
	} {
		body := render(roles)
		for _, want := range barMarkers {
			if !strings.Contains(body, want) {
				t.Errorf("roles=%v: operator control bar missing %q", roles, want)
			}
		}
	}

	// Viewer-scoped request (exactly the gardenio-info-density-audit capture
	// token shape) MUST NOT see the bar — this is the case the CAMP audit hits.
	viewerBody := render([]string{oidcauth.RoleViewer})
	for _, leaked := range barMarkers {
		if strings.Contains(viewerBody, leaked) {
			t.Errorf("viewer-scoped page leaked operator control %q (RBAC parity broken)", leaked)
		}
	}
	// The life-thread + unconditional surface must still render for the viewer;
	// only the operator levers are withheld.
	if !strings.Contains(viewerBody, "gd-cap") {
		t.Errorf("viewer-scoped page should still render the gardener body")
	}
}

func TestGardenerEnableDisable_FlipsField(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	// Disable → writes enabled=false.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/disable", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("disable: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("disable: expected enabled=false, got %v", f.updateFieldsLastVal["enabled"])
	}

	// Enable → writes enabled=true.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/enable", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: expected 200, got %d", w.Code)
	}
	if f.updateFieldsLastVal["enabled"] != true {
		t.Errorf("enable: expected enabled=true, got %v", f.updateFieldsLastVal["enabled"])
	}
}

func TestGardenerRunNow_SpawnsIterationAndBumpsSpec(t *testing.T) {
	f := seedGardenerOpsFake(t)
	// run-budget=100, runs_spawned=3 so the budget gate passes. The seeded
	// gardener has no one_spawn_at_a_time, so run-now should spawn even with a
	// live last iteration.
	f.createResp = "gd-r4"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminOpsReq("POST", "/gardener/gd-cap/run-now", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("run-now: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("run-now: expected 1 create, got %d", f.createCalls)
	}
	// The spawned iteration must carry the gardener's schedule_id (FK) and the
	// loop-context fields gb prime reads.
	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatalf("run-now: agent fields not JSON: %v", err)
	}
	if fields["schedule_id"] != "gd-cap" {
		t.Errorf("run-now: schedule_id = %q, want gd-cap", fields["schedule_id"])
	}
	if fields["role"] != "gardener" {
		t.Errorf("run-now: role = %q, want gardener", fields["role"])
	}
	if fields["spawn_source"] != "schedule" {
		t.Errorf("run-now: spawn_source = %q, want schedule", fields["spawn_source"])
	}
	if fields["schedule_prior_agent_id"] != "gd-r3" {
		t.Errorf("run-now: schedule_prior_agent_id = %q, want gd-r3", fields["schedule_prior_agent_id"])
	}
	// Neutral succession back-pointer, written ADDITIVELY alongside the untouched
	// schedule_prior_agent_id (task ka-0000006931). Mirrors the prior iteration.
	if fields["prior_agent_id"] != "gd-r3" {
		t.Errorf("run-now: prior_agent_id = %q, want gd-r3 (additive to schedule_prior_agent_id)", fields["prior_agent_id"])
	}
	// The seeded gardener has no LineageRoot yet, so this first run-now stamps no
	// lineage_root_id on the iteration; the post-fire block below seeds it to the
	// new agent id for the NEXT iteration.
	if _, ok := fields["lineage_root_id"]; ok {
		t.Errorf("run-now: lineage_root_id should be absent on the first fire (no root yet), got %q", fields["lineage_root_id"])
	}
	if fields["schedule_runs_spawned"] != "4" {
		t.Errorf("run-now: schedule_runs_spawned = %q, want 4", fields["schedule_runs_spawned"])
	}
	// Runtime backend opt-in: the trigger's execution_target rides onto the
	// iteration so a Wake/run-now lands on the same backend as scheduled fires.
	if fields["execution_target"] != "lambda_microvm" {
		t.Errorf("run-now: execution_target = %q, want lambda_microvm", fields["execution_target"])
	}

	// Post-fire bookkeeping: spec persisted with RunsSpawned bumped to 4 and
	// LastAgentID stamped to the new agent.
	persisted, ok := f.updateFieldsLastVal["gardener"].(string)
	if !ok {
		t.Fatalf("run-now: gardener spec not persisted; got %v", f.updateFieldsLastVal)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(persisted), &got); err != nil {
		t.Fatalf("run-now: persisted spec not JSON: %v", err)
	}
	if got["runs_spawned"] != float64(4) {
		t.Errorf("run-now: persisted runs_spawned = %v, want 4", got["runs_spawned"])
	}
	if got["last_agent_id"] != "gd-r4" {
		t.Errorf("run-now: persisted last_agent_id = %v, want gd-r4", got["last_agent_id"])
	}
	// The first run-now with no prior LineageRoot seeds it to the newly-minted
	// agent, so every SUBSEQUENT iteration stamps lineage_root_id = gd-r4 (task
	// ka-0000006931).
	if got["lineage_root"] != "gd-r4" {
		t.Errorf("run-now: persisted lineage_root = %v, want gd-r4 (seeded from first agent)", got["lineage_root"])
	}
}

// TestGardenerRunNow_HonoursTriggerSpawnRole locks the run-now / scheduled-fire
// parity on the spawn role (epic ka-0000002022 Step 1): the engine's
// actionSpawnAgent spawns trig.Role, so run-now must too. A plan gardener sets
// role=plan-gardener on the trigger so its iterations resolve as the plan's
// bound agent (planAssistant / isPlanAssistantBead) — hard-coding "gardener"
// broke that for Wake-button iterations. Also locks the `assigned` edge on the
// assign bead, the load-bearing edge the plan-agent resolver and constellation
// read (SpawnAgent writes it on the scheduled path).
func TestGardenerRunNow_HonoursTriggerSpawnRole(t *testing.T) {
	f := seedGardenerOpsFake(t)
	f.beads["gd-cap"].Fields["role"] = "plan-gardener"
	f.beads["gd-cap"].Fields["task_id"] = "gd-plan"
	f.createResp = "gd-r4"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminOpsReq("POST", "/gardener/gd-cap/run-now", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("run-now: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatalf("run-now: agent fields not JSON: %v", err)
	}
	if fields["role"] != "plan-gardener" {
		t.Errorf("run-now: role = %q, want plan-gardener (the trigger's spawn role)", fields["role"])
	}
	roleLabelled := false
	for _, l := range f.addLabelAll {
		if l == "role:plan-gardener" {
			roleLabelled = true
		}
		if l == "role:gardener" {
			t.Error("run-now: iteration labelled role:gardener despite the trigger's plan-gardener spawn role")
		}
	}
	if !roleLabelled {
		t.Errorf("run-now: iteration missing role:plan-gardener label (labels added: %v)", f.addLabelAll)
	}
	if f.addDepLastBead != "gd-r4" || f.addDepLastDep != "gd-plan" || f.addDepLastType != "assigned" {
		t.Errorf("run-now: assigned edge = (%s -> %s, %s), want (gd-r4 -> gd-plan, assigned)",
			f.addDepLastBead, f.addDepLastDep, f.addDepLastType)
	}
}

// TestGardenerRunNow_DefaultsRoleToGardener pins the fallback: a trigger with no
// role field keeps spawning gardener-role iterations (the pre-existing shape).
func TestGardenerRunNow_DefaultsRoleToGardener(t *testing.T) {
	f := seedGardenerOpsFake(t)
	delete(f.beads["gd-cap"].Fields, "role")
	f.createResp = "gd-r4"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminOpsReq("POST", "/gardener/gd-cap/run-now", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("run-now: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatalf("run-now: agent fields not JSON: %v", err)
	}
	if fields["role"] != "gardener" {
		t.Errorf("run-now: role = %q, want gardener fallback", fields["role"])
	}
}

func TestGardenerRunNow_OneSpawnAtATimeBlocksWhileLive(t *testing.T) {
	f := seedGardenerOpsFake(t)
	// Flip on one_spawn_at_a_time; the seeded last iteration (gd-r3) is live.
	spec := map[string]any{
		"name":                "delivery",
		"terminal_goal":       "drain",
		"run_budget":          100,
		"runs_spawned":        3,
		"last_agent_id":       "gd-r3",
		"one_spawn_at_a_time": true,
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-cap"].Fields["gardener"] = string(raw)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminOpsReq("POST", "/gardener/gd-cap/run-now", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("run-now (one-spawn, live): expected 409, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("run-now (one-spawn, live): should not spawn, got %d creates", f.createCalls)
	}
}

func TestGardenerCancelRun_ClosesInflightIteration(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/cancel-run", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("cancel-run: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.closeLastID != "gd-r3" {
		t.Errorf("cancel-run: closed %q, want gd-r3", f.closeLastID)
	}
	// The gardener trigger bead itself must NOT be closed by a cancel-run.
	if f.beads["gd-cap"].Status == "closed" {
		t.Errorf("cancel-run: must not close the gardener trigger bead")
	}
}

func TestGardenerCancelRun_NoInflightConflicts(t *testing.T) {
	f := seedGardenerOpsFake(t)
	// Mark the last iteration terminal — there is nothing to cancel.
	f.beads["gd-r3"].Status = "closed"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/cancel-run", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("cancel-run (no in-flight): expected 409, got %d", w.Code)
	}
	if f.closeCalls != 0 {
		t.Errorf("cancel-run (no in-flight): should not close anything, got %d", f.closeCalls)
	}
}

func TestGardenerRetire_ClosesGardenerBead(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/retire", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("retire: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.closeLastID != "gd-cap" {
		t.Errorf("retire: closed %q, want gd-cap", f.closeLastID)
	}
	if f.beads["gd-cap"].Status != "closed" {
		t.Errorf("retire: gardener bead should be closed")
	}
	// Re-rendered bar should show the retired (closed) state with no levers.
	if !strings.Contains(w.Body.String(), "retired") {
		t.Errorf("retire: re-rendered bar should note retired state; body=%s", w.Body.String())
	}
}

func TestGardenerEditForm_RendersCurrentSpec(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("GET", "/gardener/gd-cap/edit", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("edit form: expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="terminal_goal"`,
		"drain the ready queue", // current goal pre-filled
		`name="run_budget"`,
		`name="stop_json"`,
		"/gardener/gd-cap/config", // posts back to save
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}
}

func TestGardenerConfigSave_PersistsKnobsAndPreservesRuntime(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("terminal_goal", "ship the new goal")
	form.Set("run_budget", "42")
	form.Set("max_runs", "7")
	form.Set("retry_on_failure", "1")
	form.Set("max_retries", "3")
	form.Set("one_spawn_at_a_time", "1")
	form.Set("stop_json", `{"max_runs":7}`)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/config", form))
	if w.Code != http.StatusOK {
		t.Fatalf("config save: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	persisted, ok := f.updateFieldsLastVal["gardener"].(string)
	if !ok {
		t.Fatalf("config save: gardener spec not persisted; got %v", f.updateFieldsLastVal)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(persisted), &got); err != nil {
		t.Fatalf("config save: persisted spec not JSON: %v", err)
	}
	// Edited knobs landed.
	if got["terminal_goal"] != "ship the new goal" {
		t.Errorf("config save: terminal_goal = %v", got["terminal_goal"])
	}
	if got["run_budget"] != float64(42) {
		t.Errorf("config save: run_budget = %v, want 42", got["run_budget"])
	}
	if got["retry_on_failure"] != true {
		t.Errorf("config save: retry_on_failure = %v, want true", got["retry_on_failure"])
	}
	if got["one_spawn_at_a_time"] != true {
		t.Errorf("config save: one_spawn_at_a_time = %v, want true", got["one_spawn_at_a_time"])
	}
	// Engine-owned runtime counters preserved from the original spec (the form
	// never sends them).
	if got["runs_spawned"] != float64(3) {
		t.Errorf("config save: runs_spawned clobbered = %v, want 3", got["runs_spawned"])
	}
	if got["last_agent_id"] != "gd-r3" {
		t.Errorf("config save: last_agent_id clobbered = %v, want gd-r3", got["last_agent_id"])
	}
}

func TestGardenerConfigSave_RejectsBadStopJSON(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("terminal_goal", "x")
	form.Set("stop_json", "{not json")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/config", form))
	// Rendered at 200 (htmx swap) but with an inline error and NO persist.
	if w.Code != http.StatusOK {
		t.Fatalf("config save (bad json): expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("config save (bad json): must not persist; got %d updates", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "valid JSON") {
		t.Errorf("config save (bad json): expected inline error; body=%s", w.Body.String())
	}
}

func TestGardenerConfigSave_RejectsNegativeBudget(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("run_budget", "-1")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/config", form))
	if w.Code != http.StatusOK {
		t.Fatalf("config save (neg budget): expected 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("config save (neg budget): must not persist; got %d updates", f.updateFieldsCalls)
	}
}

func TestGardenerOps_RejectsNonGardenerBead(t *testing.T) {
	f := newFake()
	f.beads["gd-task"] = &beadsapi.BeadDetail{
		ID: "gd-task", Title: "a task", Type: "task", Status: "open",
		Fields: map[string]string{},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-task/disable", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("non-gardener disable: expected 400, got %d", w.Code)
	}
}

// seedGardenerModeFake builds a fakeDaemon with one open, timed (trigger:schedule)
// gardener trigger bead scoped to an epic (gardener-epic:gd-E) with a cron set —
// the exact shape gardenermode.Set flips to event mode.
func seedGardenerModeFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()
	spec := map[string]any{"name": "delivery", "terminal_goal": "drain the ready queue"}
	raw, _ := json.Marshal(spec)
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:        "gd-cap",
		Title:     "Delivery gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio", "gardener-epic:gd-E"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw), "cron": "7,37 * * * *"},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	return f
}

// TestGardenerModeToggle_TimedToEvent drives the viewer verb end to end: an
// operator POSTs mode=event to a timed gardener and the daemon receives exactly
// the ordered writes gardenermode.Set emits — cron cleared, timed_cron backed up,
// a triggers[] array written, then the label swap trigger:schedule → multi.
func TestGardenerModeToggle_TimedToEvent(t *testing.T) {
	f := seedGardenerModeFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("mode", "event")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/mode", form))
	if w.Code != http.StatusOK {
		t.Fatalf("mode=event: expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	// Field write: cron cleared, prior cron backed up to timed_cron, a triggers
	// array written.
	if got := f.updateFieldsLastVal["cron"]; got != "" {
		t.Errorf("mode=event: cron = %v, want cleared", got)
	}
	if got := f.updateFieldsLastVal["timed_cron"]; got != "7,37 * * * *" {
		t.Errorf("mode=event: timed_cron = %v, want the backed-up prior cron", got)
	}
	triggers, ok := f.updateFieldsLastVal["triggers"].(string)
	if !ok || !strings.Contains(triggers, "bead_event") {
		t.Errorf("mode=event: triggers = %v, want a JSON array with a bead_event entry", f.updateFieldsLastVal["triggers"])
	}

	// Label swap: claim trigger:multi, release trigger:schedule.
	if !containsStr(f.addLabelAll, "trigger:multi") {
		t.Errorf("mode=event: expected AddLabel trigger:multi, got %v", f.addLabelAll)
	}
	if !containsStr(f.removeLabelAll, "trigger:schedule") {
		t.Errorf("mode=event: expected RemoveLabel trigger:schedule, got %v", f.removeLabelAll)
	}
}

// TestGardenerModeToggle_EventToTimed flips the other direction: an event gardener
// (trigger:multi) POSTed mode=timed restores its backed-up cron and clears the
// event-mode fields, swapping the label back to trigger:schedule.
func TestGardenerModeToggle_EventToTimed(t *testing.T) {
	f := seedGardenerModeFake(t)
	// Re-shape the seed into an event-mode gardener.
	f.beads["gd-cap"].Labels = []string{"trigger:multi", "role:gardener", "project:gardenio", "gardener-epic:gd-E"}
	f.beads["gd-cap"].Fields["cron"] = ""
	f.beads["gd-cap"].Fields["timed_cron"] = "9,39 * * * *"
	f.beads["gd-cap"].Fields["triggers"] = `[{"kind":"bead_event"},{"kind":"cron"}]`
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("mode", "timed")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/mode", form))
	if w.Code != http.StatusOK {
		t.Fatalf("mode=timed: expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	if got := f.updateFieldsLastVal["cron"]; got != "9,39 * * * *" {
		t.Errorf("mode=timed: cron = %v, want restored from timed_cron", got)
	}
	if got := f.updateFieldsLastVal["triggers"]; got != "" {
		t.Errorf("mode=timed: triggers = %v, want cleared", got)
	}
	if !containsStr(f.removeLabelAll, "trigger:multi") {
		t.Errorf("mode=timed: expected RemoveLabel trigger:multi, got %v", f.removeLabelAll)
	}
	if !containsStr(f.addLabelAll, "trigger:schedule") {
		t.Errorf("mode=timed: expected AddLabel trigger:schedule, got %v", f.addLabelAll)
	}
}

// TestGardenerModeToggle_RejectsNonOperator locks the operator gate: a
// viewer-scoped request (RBAC roles present but below operator) is refused at
// the route layer with 403 and writes nothing — parity with the other
// operator-gated gardener levers.
func TestGardenerModeToggle_RejectsNonOperator(t *testing.T) {
	f := seedGardenerModeFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("mode", "event")
	req := opsReq("POST", "/gardener/gd-cap/mode", form)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer-scoped mode flip: expected 403, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 || f.addLabelCalls != 0 || f.removeLabelCalls != 0 {
		t.Errorf("viewer-scoped mode flip must not write: updates=%d addLabels=%d rmLabels=%d",
			f.updateFieldsCalls, f.addLabelCalls, f.removeLabelCalls)
	}
}

// TestGardenerModeToggle_AmbiguousBead409 pins the ambiguous-label guard: a bead
// carrying BOTH trigger:schedule and trigger:multi has no well-defined current
// mode, so the flip is a 409 and nothing is written.
func TestGardenerModeToggle_AmbiguousBead409(t *testing.T) {
	f := seedGardenerModeFake(t)
	f.beads["gd-cap"].Labels = []string{"trigger:schedule", "trigger:multi", "role:gardener", "gardener-epic:gd-E"}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("mode", "event")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/mode", form))
	if w.Code != http.StatusConflict {
		t.Fatalf("ambiguous mode flip: expected 409, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 0 || f.addLabelCalls != 0 || f.removeLabelCalls != 0 {
		t.Errorf("ambiguous mode flip must not write: updates=%d addLabels=%d rmLabels=%d",
			f.updateFieldsCalls, f.addLabelCalls, f.removeLabelCalls)
	}
}

// TestGardenerModeToggle_RejectsBadMode pins the input guard: a mode value that
// is neither "timed" nor "event" is a 400 with no writes.
func TestGardenerModeToggle_RejectsBadMode(t *testing.T) {
	f := seedGardenerModeFake(t)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("mode", "sideways")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/mode", form))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: expected 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("bad mode must not write; got %d updates", f.updateFieldsCalls)
	}
}

// TestGardenerRunNow_NonAdminRefusedWithoutSpawn: run-now spawns and then writes
// the run counter, which kbeads accepts only from an admin OIDC session or the
// trigger engine. An operator (or roleless) session therefore must be refused
// BEFORE the spawn, not left with a live but uncounted iteration (gd-uzCT6oXur9).
func TestGardenerRunNow_NonAdminRefusedWithoutSpawn(t *testing.T) {
	for name, roles := range map[string][]string{
		"operator": {oidcauth.RoleOperator},
		"roleless": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := seedGardenerOpsFake(t)
			f.createResp = "gd-r4"
			mux := mkServer(t, f)

			req := opsReq("POST", "/gardener/gd-cap/run-now", nil)
			if roles != nil {
				req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), roles))
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
			}
			if f.createCalls != 0 {
				t.Errorf("a refused run-now must not spawn; got %d creates", f.createCalls)
			}
			if f.updateFieldsLastID != "" {
				t.Errorf("a refused run-now must not write the counter; wrote %s", f.updateFieldsLastID)
			}
		})
	}
}

// TestGardenerRunNow_CounterWriteFailureRetractsIteration: when the counter
// write fails the action fails and the just-spawned iteration is closed, rather
// than logging a warning and leaving an uncounted run live.
func TestGardenerRunNow_CounterWriteFailureRetractsIteration(t *testing.T) {
	f := seedGardenerOpsFake(t)
	f.createResp = "gd-r4"
	f.updateFieldsErr = errors.New("403 forbidden: run counters are written only by the trigger engine")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminOpsReq("POST", "/gardener/gd-cap/run-now", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the counter write fails, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("expected the iteration to have been created once, got %d", f.createCalls)
	}
	if f.closeCalls != 1 || f.closeLastID != "gd-r4" {
		t.Errorf("expected the uncounted iteration gd-r4 to be closed, got %d closes (last %q)", f.closeCalls, f.closeLastID)
	}
}
