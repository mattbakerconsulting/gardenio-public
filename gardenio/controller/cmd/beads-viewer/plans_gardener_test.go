package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/scheduler"
)

// planGardenerTestNow pins the clock so buildTriggerRow's enabled derivation and
// any recency-relative rendering is deterministic.
var planGardenerTestNow = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

// seedPlanGardenerBead seeds a gardener trigger bead bound to epicID via the
// gardener-epic:<id> label (the Step 1 binding), with the LEGACY plan-assistant
// spawn role (a pre-ka-0000000081 trigger — deliberately, so the alias path
// stays covered) and a minimal gardener spec. enabled/status/lastAgent are
// caller-controlled so each state (running / paused / retired / in-flight) is
// reachable.
func seedPlanGardenerBead(f *fakeDaemon, id, epicID, enabled, status, lastAgentID string) *beadsapi.BeadDetail {
	spec := map[string]any{
		"name":          "plan-cap",
		"terminal_goal": "All steps of plan " + epicID + " are closed.",
		"run_budget":    8,
		"max_runs":      8,
		"runs_spawned":  2,
		"stop":          map[string]any{"max_runs": 8},
	}
	if lastAgentID != "" {
		spec["last_agent_id"] = lastAgentID
		spec["one_spawn_at_a_time"] = true
	}
	raw, _ := json.Marshal(spec)
	fields := map[string]string{
		"role":     planGardenerRole, // the unified plan agent spawn role
		"action":   "spawn_agent",
		"cron":     "7,37 * * * *",
		"enabled":  enabled,
		"gardener": string(raw),
		"project":  "gardenio",
		"prompt":   "drive plan " + epicID,
	}
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Plan gardener for " + epicID,
		Type:   "trigger",
		Kind:   "data",
		Status: status,
		Labels: []string{"trigger:schedule", "role:gardener", gardenerEpicLabelFor(epicID), planGardenerMarkerLabel, "project:gardenio"},
		Fields: fields,
		// task_id binds the gardener's iterations to the plan (mirrors gb plan
		// gardener), so a Wake writes the assigned edge to the plan epic.
		UpdatedAt: planGardenerTestNow,
	}
	b.Fields["task_id"] = epicID
	f.beads[id] = b
	return b
}

// seedPlanGardenerEventBead seeds a mode-FLIPPED (event) plan gardener — the shape
// gardenermode.Set writes when a gardener is flipped timed→event: the trigger:multi
// label (NOT trigger:schedule), a cleared cron field, a timed_cron backup, and a
// triggers[] array with a bead_event entry (the firing authority) plus a slow
// fallback-cron watchdog. Used to assert the /plans chip renders the ⚡ event
// glyph rather than the ⏱ timed one (Task 8, epic ka-0000006020).
func seedPlanGardenerEventBead(f *fakeDaemon, id, epicID, enabled, status string) *beadsapi.BeadDetail {
	spec := map[string]any{
		"name":          "plan-cap",
		"terminal_goal": "All steps of plan " + epicID + " are closed.",
		"run_budget":    8,
		"max_runs":      8,
		"runs_spawned":  2,
		"stop":          map[string]any{"max_runs": 8},
	}
	raw, _ := json.Marshal(spec)
	fields := map[string]string{
		"role":       planGardenerRole, // the unified plan agent spawn role
		"action":     "spawn_agent",
		"cron":       "", // cleared by the flip — event mode has no cadence cron
		"timed_cron": "7,37 * * * *",
		"triggers":   `[{"kind":"bead_event","events":["created","updated","closed"]},{"kind":"cron","cron":"11 2,8,14,20 * * *"}]`,
		"enabled":    enabled,
		"gardener":   string(raw),
		"project":    "gardenio",
		"prompt":     "drive plan " + epicID,
		"task_id":    epicID,
	}
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Plan gardener for " + epicID,
		Type:   "trigger",
		Kind:   "data",
		Status: status,
		// trigger:multi (event mode), NOT trigger:schedule. role:gardener keeps
		// isGardenerBead resolving it once the schedule label is gone.
		Labels:    []string{"trigger:multi", "role:gardener", gardenerEpicLabelFor(epicID), planGardenerMarkerLabel, "project:gardenio"},
		Fields:    fields,
		UpdatedAt: planGardenerTestNow,
	}
	f.beads[id] = b
	return b
}

func planGardenerMux(t *testing.T, f *fakeDaemon) http.Handler {
	t.Helper()
	return mkServerWithNow(t, f, func() time.Time { return planGardenerTestNow })
}

// TestPlanGardener_LookupByEpicLabel resolves the plan's bound gardener via the
// gardener-epic:<plan-id> label.
func TestPlanGardener_LookupByEpicLabel(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	s := mkPlanGardenerServer(f)

	got := s.planGardener(context.Background(), "gd-plan1")
	if got == nil {
		t.Fatal("expected to find the bound gardener via the gardener-epic label")
	}
	if got.ID != "gd-cap1" {
		t.Errorf("found %q, want gd-cap1", got.ID)
	}
}

// TestPlanGardener_NoneWhenUnbound returns nil for a plan with no gardener.
func TestPlanGardener_NoneWhenUnbound(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	s := mkPlanGardenerServer(f)

	if got := s.planGardener(context.Background(), "gd-plan1"); got != nil {
		t.Errorf("expected no gardener, got %q", got.ID)
	}
}

// TestPlanGardener_PrefersOpenOverClosed picks the open gardener when a stale
// retired one lingers on the same epic.
func TestPlanGardener_PrefersOpenOverClosed(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-capOld", "gd-plan1", "true", "closed", "")
	seedPlanGardenerBead(f, "gd-capNew", "gd-plan1", "true", "open", "")
	s := mkPlanGardenerServer(f)

	got := s.planGardener(context.Background(), "gd-plan1")
	if got == nil || got.ID != "gd-capNew" {
		t.Fatalf("expected the open gardener gd-capNew, got %v", got)
	}
}

// TestPlanGardenerView_States maps enabled/status onto the four chip states.
func TestPlanGardenerView_States(t *testing.T) {
	cases := []struct {
		name    string
		enabled string
		status  string
		want    planGardenerState
	}{
		{"running", "true", "open", planGardenerStateRunning},
		{"paused", "false", "open", planGardenerStatePaused},
		{"retired", "true", "closed", planGardenerStateRetired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			seedPlanEpic(f, "gd-plan1")
			cap := seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", tc.enabled, tc.status, "")
			s := mkPlanGardenerServer(f)

			v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateUnset, true, "")
			if v.State != tc.want {
				t.Errorf("state = %q, want %q", v.State, tc.want)
			}
			if (v.State == planGardenerStatePaused) != v.Paused {
				t.Errorf("Paused=%v inconsistent with state %q", v.Paused, v.State)
			}
		})
	}
}

// TestPlanGardenerView_FiringMode maps the bound gardener's trigger shape onto the
// chip's firing-mode glyph facet (Task 8, epic ka-0000006020): a clock-fired
// gardener (trigger:schedule + cron) → "cron" (⏱), a mode-flipped one
// (trigger:multi + a bead_event triggers[] entry) → "event" (⚡), and an unbound
// plan → "" (no glyph). It reuses the SAME gardenerTriggerSubKind classifier the
// /gardeners list uses (Task 6), so the /plans chip glyph and the /gardeners node
// color can never disagree about a gardener's firing mode.
func TestPlanGardenerView_FiringMode(t *testing.T) {
	t.Run("cron gardener reads cron", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		cap := seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
		s := mkPlanGardenerServer(f)

		v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateUnset, true, "")
		if v.FiringMode != "cron" {
			t.Errorf("FiringMode = %q, want cron", v.FiringMode)
		}
	})
	t.Run("flipped event gardener reads event", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		cap := seedPlanGardenerEventBead(f, "gd-cap1", "gd-plan1", "true", "open")
		s := mkPlanGardenerServer(f)

		v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateUnset, true, "")
		if v.FiringMode != "event" {
			t.Errorf("FiringMode = %q, want event", v.FiringMode)
		}
	})
	t.Run("unbound plan has no firing mode", func(t *testing.T) {
		s := mkPlanGardenerServer(newFake())
		v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", nil, planStateUnset, true, "")
		if v.FiringMode != "" {
			t.Errorf("FiringMode = %q, want empty (no gardener bound)", v.FiringMode)
		}
	})
}

// TestPlanGardenerBarData_CarriesFiringMode: the gardener bar's OWN data bag (the
// standalone fragment + POST + SSE re-render path) carries the firing mode on the
// chip view, so a gardener-bar re-render renders the glyph without a page reload.
func TestPlanGardenerBarData_CarriesFiringMode(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerEventBead(f, "gd-cap1", "gd-plan1", "true", "open")
	s := mkPlanGardenerServer(f)

	data := s.planGardenerBarData(context.Background(), "gd-plan1", true, "")
	chip, ok := data["Chip"].(planAgentChipView)
	if !ok {
		t.Fatalf("gardener bar Chip missing or wrong type: %T", data["Chip"])
	}
	if chip.FiringMode != "event" {
		t.Errorf("gardener-bar chip FiringMode = %q, want event", chip.FiringMode)
	}
}

// TestPlanGardenerBar_RendersFiringModeGlyph: the plan cockpit renders the ⚡ event
// glyph for a mode-flipped gardener and, for an operator, a flip affordance that
// posts to the canonical /gardener/{id}/mode verb (Task 7). A viewer sees the
// glyph but no flip control.
func TestPlanGardenerBar_RendersFiringModeGlyph(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerEventBead(f, "gd-cap1", "gd-plan1", "true", "open")
	mux := planGardenerMux(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"⚡",                      // the event firing-mode glyph
		"/gardener/gd-cap1/mode", // the flip affordance posts to the canonical verb
	} {
		if !strings.Contains(body, want) {
			t.Errorf("event-mode plan chip missing %q; body=%s", want, body)
		}
	}
}

// TestPlanGardenerBar_RendersCadenceEditor: the cockpit renders the cadence
// editor form (posting to the plan-scoped cadence route) for an operator on a
// bound gardener, pre-filled with the effective cadence.
func TestPlanGardenerBar_RendersCadenceEditor(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerEventBead(f, "gd-cap1", "gd-plan1", "true", "open")
	mux := planGardenerMux(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"/plans/gd-plan1/gardener/cadence", // the cadence form posts to the plan-scoped route
		"11 2,8,14,20 * * *",               // the effective (fallback watchdog) cadence, pre-filled
		"Save cadence",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cadence editor missing %q; body=%s", want, body)
		}
	}
}

// TestPlanGardenerView_NoneWhenNil renders the "none" state for an unbound plan.
func TestPlanGardenerView_NoneWhenNil(t *testing.T) {
	s := mkPlanGardenerServer(newFake())
	v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", nil, planStateUnset, true, "")
	if v.State != planGardenerStateNone {
		t.Errorf("state = %q, want none", v.State)
	}
	if v.GardenerID != "" {
		t.Errorf("GardenerID should be empty for an unbound plan, got %q", v.GardenerID)
	}
}

// TestPlanGardenerView_InflightRun sets HasInflightRun when the last iteration
// is still live.
func TestPlanGardenerView_InflightRun(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	cap := seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "gd-iter")
	f.beads["gd-iter"] = &beadsapi.BeadDetail{ID: "gd-iter", Type: "agent", Status: "in_progress"}
	s := mkPlanGardenerServer(f)

	v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateUnset, true, "")
	if !v.HasInflightRun {
		t.Error("expected HasInflightRun=true when the last iteration is in_progress")
	}
}

// TestPlanGardenerBar_RendersOnPlanDetail: the compact gardener bar renders on the
// full plan cockpit, with the status chip and the operator levers pointed at the
// plan-scoped routes.
func TestPlanGardenerBar_RendersOnPlanDetail(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-agent"`,
		// Unified plan-agent chip (step 5, ka-0000001332). Enabled gardener, no
		// live iteration → the calm "idle" tone, NOT green "running" (Matt's ask,
		// thread ka-0000001290); "running" is now reserved for a live agent.
		`plan-agent-chip-idle`,
		"/plans/gd-plan1/gardener/toggle",
		"/plans/gd-plan1/gardener/wake",
		"/gardener/gd-cap1", // deep link to the full gardener page
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan detail missing %q", want)
		}
	}
}

// TestPlanGardenerBar_NoneState renders the not-started prompt when the plan has
// no gardener, WITHOUT the toggle/wake levers.
func TestPlanGardenerBar_NoneState(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	mux := planGardenerMux(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1/agent", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-agent-chip-none") {
		t.Errorf("expected the unified none chip; body=%s", body)
	}
	if strings.Contains(body, "/plans/gd-plan1/gardener/wake") {
		t.Errorf("an unbound plan must not render the Wake lever; body=%s", body)
	}
	// An OPERATOR gets the one-click "Enable gardener" button (thread
	// ka-0000004696) POSTing to /gardener/enable — not the copy-paste CLI hint.
	if !strings.Contains(body, "/plans/gd-plan1/gardener/enable") {
		t.Errorf("an unbound plan must render the Enable-gardener button for an operator; body=%s", body)
	}
	if !strings.Contains(body, "Enable gardener") {
		t.Errorf("expected the Enable-gardener button label; body=%s", body)
	}
}

// TestPlanGardenerBar_NoneStateViewer: a non-operator viewing an unbound plan
// sees the read-only CLI hint (no bind button), so the button never
// over-advertises a write the viewer can't perform.
func TestPlanGardenerBar_NoneStateViewer(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	mux := planGardenerMux(t, f)

	// An authenticated identity carrying only the read-scoped viewer role — a
	// genuine non-operator (no roles at all is the dev-bypass "operator" case).
	req := httptest.NewRequest("GET", "/plans/gd-plan1/agent", nil)
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "/plans/gd-plan1/gardener/enable") {
		t.Errorf("a viewer must not see the operator-only Enable-gardener button; body=%s", body)
	}
	if !strings.Contains(body, "gb plan gardener gd-plan1") {
		t.Errorf("expected the CLI bind hint for a viewer; body=%s", body)
	}
}

// TestPlanGardenerEnable_BindsGardener: the one-click Enable button binds a
// gardener via the shared plangardener.Bind core and re-renders the card with the
// pause/resume + Wake levers — the CLI hint is gone.
func TestPlanGardenerEnable_BindsGardener(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	storeCreatedBeads(f) // so the re-render resolves the just-bound gardener
	f.createResp = "gd-newcap"
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/enable", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	// Exactly one plan-gardener trigger minted through the shared core.
	caps := gardenerCreates(f)
	if len(caps) != 1 {
		t.Fatalf("expected exactly one gardener trigger minted, got %d", len(caps))
	}
	// It must carry the marker + epic-pin labels the shared core stamps, so the
	// resolver adopts it on re-render.
	if !slices.Contains(caps[0].Labels, gardenerEpicLabelFor("gd-plan1")) || !slices.Contains(caps[0].Labels, planGardenerMarkerLabel) {
		t.Errorf("bound gardener missing plan-gardener marker/epic labels; got %v", caps[0].Labels)
	}
	// The re-rendered card no longer shows the bind hint or the Enable button —
	// a gardener is now bound, so the pause/resume levers render instead.
	body := w.Body.String()
	if strings.Contains(body, "gb plan gardener gd-plan1") {
		t.Errorf("after binding, the CLI hint must be gone; body=%s", body)
	}
	if strings.Contains(body, "/plans/gd-plan1/gardener/enable") {
		t.Errorf("after binding, the Enable button must be gone; body=%s", body)
	}
}

// TestPlanGardenerEnable_Idempotent: clicking Enable when a gardener is already
// bound (e.g. bound via the CLI between page load and click) adopts it —
// no duplicate bead is minted.
func TestPlanGardenerEnable_Idempotent(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/enable", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if caps := gardenerCreates(f); len(caps) != 0 {
		t.Errorf("an already-bound plan must adopt, not mint a duplicate; minted %d", len(caps))
	}
}

// TestPlanGardenerEnable_RequiresOperator: the enable route is operator-gated —
// a plain viewer POST is refused, so a gardener can't be bound without operate
// rights.
func TestPlanGardenerEnable_RequiresOperator(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	mux := planGardenerMux(t, f)

	// operatorOnly rejects a POST with no operator identity.
	req := httptest.NewRequest("POST", "/plans/gd-plan1/gardener/enable", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("expected the operator gate to reject an unauthenticated enable, got 200")
	}
	if caps := gardenerCreates(f); len(caps) != 0 {
		t.Errorf("no gardener must be bound without operator rights; minted %d", len(caps))
	}
}

// TestPlanGardenerToggle_FlipsEnabled: the plan-scoped toggle flips the bound
// gardener's enabled field via the shared core — pausing a running gardener
// writes enabled=false.
func TestPlanGardenerToggle_FlipsEnabled(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastID != "gd-cap1" {
		t.Errorf("toggle wrote to %q, want gd-cap1", f.updateFieldsLastID)
	}
	if f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("pausing a running gardener should write enabled=false, got %v", f.updateFieldsLastVal["enabled"])
	}
	// The response is the gardener bar fragment (HTMX outerHTML swap target).
	if !strings.Contains(w.Body.String(), `id="plan-agent"`) {
		t.Errorf("toggle response should be the gardener bar fragment; body=%s", w.Body.String())
	}
}

// TestPlanGardenerToggle_ResumesPaused: toggling a PAUSED gardener writes
// enabled=true (resume).
func TestPlanGardenerToggle_ResumesPaused(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal["enabled"] != true {
		t.Errorf("resuming a paused gardener should write enabled=true, got %v", f.updateFieldsLastVal["enabled"])
	}
}

// TestPlanGardenerToggle_NoGardenerConflicts: toggling a plan with no bound
// gardener re-renders the bar with an inline note rather than mutating anything.
func TestPlanGardenerToggle_NoGardener(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("toggle with no gardener must not write; got %d UpdateBeadFieldsTyped calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "no bound gardener") {
		t.Errorf("expected an inline 'no bound gardener' note; body=%s", w.Body.String())
	}
}

// TestPlanGardenerWake_SpawnsIteration: the Wake button spawns one iteration via
// the shared run-now core — carrying the trigger's OWN spawn role (plan-gardener)
// and the assigned edge to the plan, so it resolves as the plan's bound agent.
// adminPost is operatorPost upgraded to a gardenio-admin session: Wake counts the
// run against the gardener, a write kbeads takes only from an admin
// (gd-uzCT6oXur9).
func adminPost(target, body string) *http.Request {
	req := operatorPost(target, body)
	return req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleAdmin}))
}

// TestPlanGardenerWake_NonAdminRefusedWithoutSpawn: an operator session would
// spawn an iteration whose counter write 403s at the daemon, leaving the run
// uncounted against run_budget, so Wake refuses it before spawning anything.
func TestPlanGardenerWake_NonAdminRefusedWithoutSpawn(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPost("/plans/gd-plan1/gardener/wake", ""))
	if f.createCalls != 0 {
		t.Errorf("operator Wake must not spawn; got %d creates", f.createCalls)
	}
	if !strings.Contains(w.Body.String(), "gardenio-admin") {
		t.Errorf("expected the admin-required note inline; body=%s", w.Body.String())
	}
}

func TestPlanGardenerWake_SpawnsIteration(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	f.createResp = "gd-iter1"
	mux := planGardenerMux(t, f)

	req := adminPost("/plans/gd-plan1/gardener/wake", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("wake: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("wake: expected 1 CreateBead, got %d", f.createCalls)
	}
	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatalf("wake: agent fields not JSON: %v", err)
	}
	// The spawned iteration takes the trigger's OWN spawn role verbatim
	// (plan-gardener) — run-now must never rewrite the trigger's role.
	if fields["role"] != planGardenerRole {
		t.Errorf("wake: role = %q, want %q (run-now honours the trigger's spawn role)", fields["role"], planGardenerRole)
	}
	if fields["schedule_id"] != "gd-cap1" {
		t.Errorf("wake: schedule_id = %q, want gd-cap1", fields["schedule_id"])
	}
	if fields["task_id"] != "gd-plan1" {
		t.Errorf("wake: task_id = %q, want gd-plan1 (pins the iteration to the plan)", fields["task_id"])
	}
	// The assigned edge to the plan is what makes the iteration resolve as the
	// plan's assistant (planAssistant reads it).
	if f.addDepLastBead != "gd-iter1" || f.addDepLastDep != "gd-plan1" || f.addDepLastType != "assigned" {
		t.Errorf("wake: expected assigned edge gd-iter1 -> gd-plan1, got %s -> %s (%s)", f.addDepLastBead, f.addDepLastDep, f.addDepLastType)
	}
}

// TestPlanGardenerWake_OneSpawnBlocks: Wake respects one_spawn_at_a_time — a live
// prior iteration makes it re-render the bar with the rejection note instead of
// spawning a second run.
func TestPlanGardenerWake_OneSpawnBlocks(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "gd-iterlive")
	f.beads["gd-iterlive"] = &beadsapi.BeadDetail{ID: "gd-iterlive", Type: "agent", Status: "in_progress"}
	mux := planGardenerMux(t, f)

	req := adminPost("/plans/gd-plan1/gardener/wake", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("wake: expected 200 (inline note), got %d", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("wake must not spawn while a run is in flight; got %d creates", f.createCalls)
	}
	if !strings.Contains(w.Body.String(), "in flight") {
		t.Errorf("expected the one-spawn rejection note; body=%s", w.Body.String())
	}
}

// TestPlanGardenerBar_RoleScoped locks the operator gate: an operator-scoped
// viewer sees the levers, a viewer-scoped one does not (parity with the
// operatorOnly POST routes, mirroring TestGardenerPage_RoleScopedControlBar).
func TestPlanGardenerBar_RoleScoped(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	render := func(roles []string) string {
		req := httptest.NewRequest("GET", "/plans/gd-plan1/agent", nil)
		req.Header.Set("X-Auth-Request-Email", "u@acme.example")
		if roles != nil {
			req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), roles))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		return w.Body.String()
	}

	opBody := render([]string{oidcauth.RoleOperator})
	if !strings.Contains(opBody, "/plans/gd-plan1/gardener/wake") {
		t.Errorf("operator should see the Wake lever; body=%s", opBody)
	}

	viewerBody := render([]string{oidcauth.RoleViewer})
	if strings.Contains(viewerBody, "/plans/gd-plan1/gardener/wake") {
		t.Errorf("viewer must NOT see the Wake lever; body=%s", viewerBody)
	}
	// The chip renders for both — read-scoped viewers see state, just no levers.
	if !strings.Contains(viewerBody, "plan-agent-chip") {
		t.Errorf("viewer should still see the unified plan-agent status chip; body=%s", viewerBody)
	}
}

// TestHandlePlanEvents_GardenerFrameOnGardenerChange: a lifecycle event on the
// plan's bound gardener (a pause/resume enable flip) emits a plan-agent-updated
// frame carrying the re-rendered bar — and does NOT flash the step timeline (the
// gardener is in the steps interest set but is not a step).
func TestHandlePlanEvents_GardenerFrameOnGardenerChange(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServerWithNow(t, f, func() time.Time { return planGardenerTestNow })

	// A pause/resume flip on the gardener → a gardener-bar frame, not a step frame.
	enqueuePlanEvent(t, ch, "beads.bead.updated", "gd-cap1")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-agent-updated") {
		t.Errorf("expected a plan-agent-updated frame, got: %s", got)
	}
	if !strings.Contains(got, `id="plan-agent"`) {
		t.Errorf("expected the re-rendered gardener bar, got: %s", got)
	}
	if strings.Contains(got, "event: plan-updated") {
		t.Errorf("a gardener change must NOT flash the step timeline, got: %s", got)
	}
}

// TestHandlePlanEvents_StepChangeStillWorksWithGardenerBound: binding a gardener
// must not swallow ordinary step-timeline frames — a step close still emits
// plan-updated (and no gardener frame).
func TestHandlePlanEvents_StepChangeStillWorksWithGardenerBound(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServerWithNow(t, f, func() time.Time { return planGardenerTestNow })

	enqueuePlanEvent(t, ch, "beads.bead.closed", "gd-step1")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-updated") {
		t.Errorf("expected a plan-updated frame for the step close, got: %s", got)
	}
	if strings.Contains(got, "event: plan-agent-updated") {
		t.Errorf("a step close must NOT flash the gardener bar, got: %s", got)
	}
}

// mkPlanGardenerServer builds a *Server (not a mux) with the clock pinned, for
// unit-testing the lookup + view helpers directly.
func mkPlanGardenerServer(f *fakeDaemon) *Server {
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return planGardenerTestNow })
	return s
}

// --- Cadence editor (task ka-0000008237) ---

// TestPlanGardenerView_Cadence: the cockpit view surfaces the effective cadence
// per firing mode — the flat cron in timed mode, the fallback-watchdog cron in
// event mode — and marks it editable for both, but not for a retired gardener.
func TestPlanGardenerView_Cadence(t *testing.T) {
	t.Run("timed gardener exposes flat cron", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		cap := seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
		s := mkPlanGardenerServer(f)
		v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateUnset, true, "")
		if v.Cadence != cap.Fields["cron"] || v.Cadence == "" {
			t.Errorf("timed Cadence = %q, want the flat cron %q", v.Cadence, cap.Fields["cron"])
		}
		if !v.CadenceEditable {
			t.Errorf("timed gardener cadence should be editable")
		}
	})
	t.Run("event gardener exposes the fallback watchdog cron", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		cap := seedPlanGardenerEventBead(f, "gd-cap1", "gd-plan1", "true", "open")
		s := mkPlanGardenerServer(f)
		v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateUnset, true, "")
		if v.Cadence != "11 2,8,14,20 * * *" {
			t.Errorf("event Cadence = %q, want the fallback watchdog cron", v.Cadence)
		}
		if !v.CadenceEditable {
			t.Errorf("event gardener cadence should be editable")
		}
	})
	t.Run("retired gardener hides the editor", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		cap := seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "closed", "")
		s := mkPlanGardenerServer(f)
		v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateUnset, true, "")
		if v.CadenceEditable {
			t.Errorf("a retired gardener must not expose a cadence editor")
		}
	})
}

// TestPlanGardenerCadence_TimedSetsCron: posting a valid cron to the cadence route
// writes it to the timed gardener's flat cron field.
func TestPlanGardenerCadence_TimedSetsCron(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/cadence", "cadence=15+*+*+*+*")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cadence: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal["cron"] != "15 * * * *" {
		t.Errorf("cadence should write cron=15 * * * *, got %v", f.updateFieldsLastVal["cron"])
	}
	if !strings.Contains(w.Body.String(), `id="plan-agent"`) {
		t.Errorf("cadence response should be the plan-agent fragment; body=%s", w.Body.String())
	}
}

// TestPlanGardenerCadence_EventClearsWatchdog: posting an EMPTY cadence to an
// event-mode gardener drops the fallback watchdog entry entirely (purely
// event-driven), preserving the bead_event source.
func TestPlanGardenerCadence_EventClearsWatchdog(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerEventBead(f, "gd-cap1", "gd-plan1", "true", "open")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/cadence", "cadence=")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cadence: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	raw, _ := f.updateFieldsLastVal["triggers"].(string)
	var entries []scheduler.TriggerEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("triggers not valid JSON: %v (raw=%q)", err, raw)
	}
	if len(entries) != 1 || entries[0].Kind != "bead_event" {
		t.Errorf("clearing the event cadence must drop the watchdog, got %+v", entries)
	}
}

// TestPlanGardenerCadence_RejectsBadCron: an invalid cron re-renders the card with
// an inline error and writes nothing.
func TestPlanGardenerCadence_RejectsBadCron(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/cadence", "cadence=not-a-cron")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("a bad cron must not write; got %d UpdateBeadFieldsTyped calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "Invalid cron") {
		t.Errorf("expected an inline invalid-cron message, got: %s", w.Body.String())
	}
}

// TestPlanGardenerCadence_TimedRejectsEmpty: clearing a TIMED gardener's cron would
// stop it firing, so it is rejected with an inline note and no write.
func TestPlanGardenerCadence_TimedRejectsEmpty(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/cadence", "cadence=")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("an empty timed cadence must not write; got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "needs a cron") {
		t.Errorf("expected a timed-needs-cron message, got: %s", w.Body.String())
	}
}
