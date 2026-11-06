package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// triggerToggleReq builds a POST /triggers/{id}/toggle request with
// X-Auth-Request-Email set so EmailFromRequest sees the operator. Roles are
// left empty so RequireRole(operator) passes through (matches every other
// inline-edit test in this package).
func triggerToggleReq(id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/triggers/"+id+"/toggle", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	return req
}

func TestToggleTriggerEnabled_LegacySchedule_FlipsTrueToFalse(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {
			ID:        "gd-sch1",
			Title:     "Daily report",
			Type:      "schedule",
			Status:    "open",
			UpdatedAt: time.Now(),
			Fields: map[string]string{
				"cron":    "0 9 * * *",
				"enabled": "true",
			},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-sch1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("UpdateBeadFieldsTyped: want 1 call, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-sch1" {
		t.Errorf("UpdateBeadFieldsTyped id: want gd-sch1, got %q", f.updateFieldsLastID)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != false {
		t.Errorf("enabled write: want \"false\", got %v", got)
	}
}

func TestToggleTriggerEnabled_LegacySchedule_FlipsMissingToTrue(t *testing.T) {
	// Legacy schedule defaults to disabled when enabled field is absent.
	// Toggling should write "true".
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch2": {
			ID:        "gd-sch2",
			Type:      "schedule",
			Status:    "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "0 9 * * *"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-sch2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != true {
		t.Errorf("enabled write: want \"true\" (legacy default disabled), got %v", got)
	}
}

func TestToggleTriggerEnabled_TriggerSchedule_DefaultEnabled_FlipsToFalse(t *testing.T) {
	// type=trigger label=trigger:schedule defaults *enabled* when the
	// enabled field is absent or anything other than "false". The first
	// toggle must write "false".
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-trig1": {
			ID:        "gd-trig1",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule"},
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "*/5 * * * *"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-trig1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != false {
		t.Errorf("enabled write: want \"false\" (trigger default enabled), got %v", got)
	}
}

func TestToggleTriggerEnabled_TriggerSchedule_FalseFlipsToTrue(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-trig2": {
			ID:        "gd-trig2",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule"},
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "*/5 * * * *", "enabled": "false"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-trig2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != true {
		t.Errorf("enabled write: want \"true\", got %v", got)
	}
}

func TestToggleTriggerEnabled_RendersTogglePartial(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch3": {
			ID:        "gd-sch3",
			Type:      "schedule",
			Status:    "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-sch3"))

	body := w.Body.String()
	// The response is a fragment, not a full page — it should contain the
	// toggle form and the row id, and NOT contain the layout chrome.
	for _, want := range []string{
		`id="trigger-toggle-gd-sch3"`,
		`hx-post="/triggers/gd-sch3/toggle"`,
		`role="switch"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("toggle partial missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "<html") {
		t.Errorf("toggle response should be a fragment, not a full page")
	}
}

func TestToggleTriggerEnabled_RejectsUnauthenticated(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch4": {ID: "gd-sch4", Type: "schedule", Status: "open"},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/triggers/gd-sch4/toggle", nil)
	// no X-Auth-Request-Email header set
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("unauthenticated request must not write fields, got %d calls", f.updateFieldsCalls)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("unauthenticated request must not post audit comment, got %d calls", f.addCommentCalls)
	}
}

func TestToggleTriggerEnabled_NotFound(t *testing.T) {
	f := newFake()
	// Force the daemon to return a 404-shaped APIError for the missing id so
	// the handler maps it to a 404 response.
	f.beads = map[string]*beadsapi.BeadDetail{}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-missing"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestToggleTriggerEnabled_RejectsNonScheduleBead(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-task1": {ID: "gd-task1", Type: "task", Status: "open"},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-task1"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("non-schedule bead must not be written, got %d calls", f.updateFieldsCalls)
	}
}

func TestToggleTriggerEnabled_RejectsTriggerWithoutScheduleLabel(t *testing.T) {
	// type=trigger beads carrying a non-schedule source (e.g. trigger:jira)
	// must not be flipped from /triggers — the per-row toggle is wired to
	// the schedule/trigger:schedule kinds. Engine-only trigger kinds get
	// their own affordance later.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-trig-j": {
			ID:     "gd-trig-j",
			Type:   "trigger",
			Status: "open",
			Labels: []string{"trigger:jira"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-trig-j"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("non-trigger:schedule bead must not be written, got %d calls", f.updateFieldsCalls)
	}
}

func TestToggleTriggerEnabled_RejectsClosedBead(t *testing.T) {
	// Closed beads do not render the toggle in the /triggers list; a stray
	// POST should be rejected with 409 instead of silently flipping a
	// parked bead.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch-closed": {
			ID:     "gd-sch-closed",
			Type:   "schedule",
			Status: "closed",
			Fields: map[string]string{"cron": "0 9 * * *", "enabled": "true"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-sch-closed"))

	if w.Code != http.StatusConflict {
		t.Fatalf("status: want 409, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("closed bead must not be written, got %d calls", f.updateFieldsCalls)
	}
}

func TestToggleTriggerEnabled_PostsAuditComment(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch-aud": {
			ID:     "gd-sch-aud",
			Type:   "schedule",
			Status: "open",
			Fields: map[string]string{"cron": "0 9 * * *", "enabled": "true"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-sch-aud"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addCommentCalls != 1 {
		t.Fatalf("AddComment: want 1 call, got %d", f.addCommentCalls)
	}
	if f.addCommentLastID != "gd-sch-aud" {
		t.Errorf("audit comment bead: want gd-sch-aud, got %q", f.addCommentLastID)
	}
	if f.addCommentLastAuth != "op@example.com" {
		t.Errorf("audit comment author: want op@example.com, got %q", f.addCommentLastAuth)
	}
	if want := "enabled → false by op@example.com"; f.addCommentLastText != want {
		t.Errorf("audit comment text: want %q, got %q", want, f.addCommentLastText)
	}
}

func TestToggleTriggerEnabled_AuditCommentFailureDoesNotFailToggle(t *testing.T) {
	// Audit comment is best-effort — mutation_history is the canonical
	// record. A daemon error on the comment write should not block the
	// re-rendered toggle.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch-aud2": {
			ID:     "gd-sch-aud2",
			Type:   "schedule",
			Status: "open",
			Fields: map[string]string{"cron": "0 9 * * *", "enabled": "true"},
		},
	}
	f.addCommentErr = errors.New("daemon down")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-sch-aud2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 (comment error is non-fatal), got %d", w.Code)
	}
	if f.updateFieldsCalls != 1 {
		t.Errorf("UpdateBeadFieldsTyped: want 1 call, got %d", f.updateFieldsCalls)
	}
}

func TestToggleTriggerEnabled_FieldsUpdateFailure(t *testing.T) {
	// A daemon failure on the field write must propagate — without the
	// flip, posting an audit comment would lie about state.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch-err": {
			ID:     "gd-sch-err",
			Type:   "schedule",
			Status: "open",
			Fields: map[string]string{"enabled": "true"},
		},
	}
	f.updateFieldsErr = errors.New("daemon down")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-sch-err"))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status: want 502, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addCommentCalls != 0 {
		t.Errorf("audit comment must not be posted when field write failed, got %d calls", f.addCommentCalls)
	}
}

func TestHandleTriggers_RendersInlineToggle(t *testing.T) {
	// Integration smoke test: the /triggers list page should render the
	// toggle partial inside each row so a click flips state in place.
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-sch5",
				Title:     "Daily",
				Type:      "schedule",
				Status:    "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest(http.MethodGet, "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="trigger-toggle-gd-sch5"`,
		`hx-post="/triggers/gd-sch5/toggle"`,
		`role="switch"`,
		`aria-checked="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/triggers row missing %q", want)
		}
	}
}

func TestHandleTriggers_HidesToggleOnClosedBead(t *testing.T) {
	// Closed schedule rows show the "closed" chip and do NOT render the
	// inline toggle — flipping a closed bead is not a legitimate operator
	// affordance.
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-sch-c",
				Title:     "Old daily",
				Type:      "schedule",
				Status:    "closed",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest(http.MethodGet, "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `id="trigger-toggle-gd-sch-c"`) {
		t.Errorf("closed row must not render the toggle\nbody=%s", body)
	}
	if !strings.Contains(body, `trig-state-closed`) {
		t.Errorf("closed row must render the closed chip")
	}
}

// eventGardenerBead builds an event-mode plan gardener the way gardenermode.Set
// leaves one (gd-fL4PM5z8kc): type=trigger, trigger:multi (no trigger:schedule),
// role:gardener + a gardener blob, no `cron` field, and a triggers array with a
// plan-activity bead_event entry plus a daily fallback-watchdog cron entry.
func eventGardenerBead(id, epic string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"role":     "plan-gardener",
		"gardener": `{"terminal_goal":"build the plan"}`,
		"triggers": `[{"kind":"bead_event","label":"plan-activity","events":["created","updated","closed"]},` +
			`{"kind":"cron","label":"fallback-watchdog","cron":"17 4 * * *","timezone":"UTC"}]`,
	}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     "Gardener for " + epic,
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:multi", "role:gardener", "plan-gardener", "gardener-epic:" + epic},
		UpdatedAt: time.Now(),
		Fields:    f,
	}
}

// stewardBead builds a land steward trigger the way steward.Bind mints one.
func stewardBead(id string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"role":     "steward",
		"gardener": `{"standing":true}`,
		"triggers": `[{"kind":"cron","label":"digest","cron":"0 7 * * *"}]`,
	}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:multi", "role:gardener", "steward", "land:home", "project:home", "perpetual:true"},
		Fields: f,
	}
}

func TestToggleTriggerEnabled_EventModeGardener_FlipsToFalse(t *testing.T) {
	// An event-mode gardener carries trigger:multi, not trigger:schedule. It
	// defaults enabled (type=trigger), so the first toggle writes false.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-evg1": eventGardenerBead("gd-evg1", "gd-plan1", nil),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-evg1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 || f.updateFieldsLastID != "gd-evg1" {
		t.Fatalf("UpdateBeadFieldsTyped: want 1 call on gd-evg1, got %d on %q", f.updateFieldsCalls, f.updateFieldsLastID)
	}
	// The patch is the lone `enabled` bool setGardenerEnabledCore sends: no
	// digest key, no reseal control key, so the spawn seal is untouched.
	if len(f.updateFieldsLastVal) != 1 || f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("patch: want exactly {enabled:false}, got %v", f.updateFieldsLastVal)
	}
	if want := "enabled → false by op@example.com"; f.addCommentLastText != want {
		t.Errorf("audit text: want %q, got %q", want, f.addCommentLastText)
	}
}

func TestToggleTriggerEnabled_EventModeGardener_FalseFlipsToTrue(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-evg2": eventGardenerBead("gd-evg2", "gd-plan2", map[string]string{"enabled": "false"}),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-evg2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := f.updateFieldsLastVal["enabled"]; got != true {
		t.Errorf("enabled write: want true, got %v", got)
	}
	// A re-enable keeps the spawn seal only when it is the operator's own
	// OIDC write; a machine-principal write would void it
	// (voidSpawnLineageOnReactivation). The toggle must not switch principals.
	if f.updateFieldsLastMachine {
		t.Error("re-enable went out on the machine principal; it must ride the operator's request")
	}
}

func TestToggleTriggerEnabled_RejectsTriggerMultiNonGardener(t *testing.T) {
	// A trigger:multi bead that is NOT a gardener (no role:gardener label, no
	// gardener blob) stays refused: the widening is to gardeners only, the
	// same set the /gardener/{id} bar and /gardeners pause toggle accept.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-multi": {
			ID:     "gd-multi",
			Type:   "trigger",
			Status: "open",
			Labels: []string{"trigger:multi"},
			Fields: map[string]string{"triggers": `[{"kind":"cron","cron":"0 * * * *"}]`},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-multi"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("non-gardener trigger:multi must not be written, got %d calls", f.updateFieldsCalls)
	}
}

func TestToggleTriggerEnabled_StewardSingleRowAllowed(t *testing.T) {
	// The steward is a gardener (role:gardener + blob), which the /gardeners
	// pause toggle and /gardener/{id} bar already accept; its own /triggers row
	// toggle matches that. Only the bulk path skips it.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-stew": stewardBead("gd-stew", nil),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggerToggleReq("gd-stew"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := f.updateFieldsLastVal["enabled"]; got != false {
		t.Errorf("enabled write: want false, got %v", got)
	}
}

func TestTriggerToggleTarget_MatchesGardenerPredicate(t *testing.T) {
	// For every trigger:multi shape, the /triggers toggle accepts exactly
	// what loadGardenerTrigger (isGardenerBead) accepts.
	cases := []*beadsapi.BeadDetail{
		eventGardenerBead("gd-a", "gd-p", nil),
		stewardBead("gd-b", nil),
		{ID: "gd-c", Type: "trigger", Labels: []string{"trigger:multi"}},
		{ID: "gd-d", Type: "trigger", Labels: []string{"trigger:multi", "role:gardener"}},
		{ID: "gd-e", Type: "trigger", Labels: []string{"trigger:multi"}, Fields: map[string]string{"gardener": `{"x":1}`}},
		{ID: "gd-f", Type: "trigger", Labels: []string{"trigger:jira", "role:gardener"}},
	}
	for _, b := range cases {
		want := hasLabel(b, "trigger:multi") && isGardenerBead(b)
		if got := triggerToggleTarget(b); got != want {
			t.Errorf("%s: triggerToggleTarget=%v, want %v", b.ID, got, want)
		}
	}
	if triggerToggleTarget(nil) {
		t.Error("nil bead must not be a toggle target")
	}
}
