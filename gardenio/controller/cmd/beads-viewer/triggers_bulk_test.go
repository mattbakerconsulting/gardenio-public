package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// triggersBulkReq builds a POST /triggers/bulk/{action} request with the
// supplied bead_id form values and X-Auth-Request-Email set so
// EmailFromRequest sees the operator.
func triggersBulkReq(action string, ids ...string) *http.Request {
	form := url.Values{}
	for _, id := range ids {
		form.Add("bead_id", id)
	}
	req := httptest.NewRequest(http.MethodPost, "/triggers/bulk/"+action, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	return req
}

func TestTriggersBulkEnable_HappyPath(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {
			ID:        "gd-sch1",
			Title:     "Daily",
			Type:      "schedule",
			Status:    "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "false"},
		},
		"gd-trig1": {
			ID:        "gd-trig1",
			Title:     "Cycle",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule"},
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "*/5 * * * *", "enabled": "false"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-sch1", "gd-trig1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 2 {
		t.Fatalf("UpdateBeadFieldsTyped: want 2 calls, got %d", f.updateFieldsCalls)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != true {
		t.Errorf("enabled write: want \"true\", got %v", got)
	}
	if f.addCommentCalls != 2 {
		t.Errorf("AddComment: want 2 calls, got %d", f.addCommentCalls)
	}
	body := w.Body.String()
	// Re-rendered toolbar present.
	if !strings.Contains(body, `id="triggers-bulk-bar"`) {
		t.Errorf("toolbar missing from response\nbody=%s", body)
	}
	// OOB row swaps for both updated beads.
	for _, id := range []string{"gd-sch1", "gd-trig1"} {
		needle := `id="trigger-row-` + id + `"`
		if !strings.Contains(body, needle) {
			t.Errorf("missing OOB row %s\nbody=%s", id, body)
		}
	}
	if !strings.Contains(body, `hx-swap-oob="outerHTML"`) {
		t.Errorf("missing OOB swap attribute\nbody=%s", body)
	}
}

func TestTriggersBulkDisable_HappyPath(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {
			ID:        "gd-sch1",
			Type:      "schedule",
			Status:    "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("disable", "gd-sch1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := f.updateFieldsLastVal["enabled"]; got != false {
		t.Errorf("enabled write: want \"false\", got %v", got)
	}
	if want := "enabled → false by op@example.com"; f.addCommentLastText != want {
		t.Errorf("audit text: want %q, got %q", want, f.addCommentLastText)
	}
}

func TestTriggersBulkEnable_DedupesIDs(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {ID: "gd-sch1", Type: "schedule", Status: "open"},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-sch1", "gd-sch1", "gd-sch1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 1 {
		t.Errorf("dedupe: want 1 update call, got %d", f.updateFieldsCalls)
	}
}

func TestTriggersBulkEnable_EmptyIDs(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("no updates on empty ids, got %d", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "Select at least one trigger") {
		t.Errorf("missing error message\nbody=%s", w.Body.String())
	}
}

func TestTriggersBulkEnable_Unauthenticated(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {ID: "gd-sch1", Type: "schedule", Status: "open"},
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Add("bead_id", "gd-sch1")
	req := httptest.NewRequest(http.MethodPost, "/triggers/bulk/enable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// no X-Auth-Request-Email header

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("unauthenticated must not write, got %d calls", f.updateFieldsCalls)
	}
}

func TestTriggersBulkEnable_SkipsNonTriggerBead(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {
			ID: "gd-sch1", Type: "schedule", Status: "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"enabled": "false"},
		},
		"gd-task1": {ID: "gd-task1", Type: "task", Status: "open"},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-sch1", "gd-task1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	// Only the schedule bead is updated; the task is skipped with an inline error.
	if f.updateFieldsCalls != 1 {
		t.Errorf("UpdateBeadFieldsTyped: want 1 call (skipping task), got %d", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "is not a trigger") {
		t.Errorf("expected inline error about non-trigger bead\nbody=%s", w.Body.String())
	}
}

func TestTriggersBulkEnable_SkipsClosedBead(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {ID: "gd-sch1", Type: "schedule", Status: "closed"},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-sch1"))

	// No successful updates → 502 (only failures).
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status: want 502, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("closed bead must not be written, got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "is closed") {
		t.Errorf("expected inline error about closed bead\nbody=%s", w.Body.String())
	}
}

func TestTriggersBulkEnable_PartialFailure(t *testing.T) {
	// One bead succeeds, one's update fails. Response is 200 (some succeeded)
	// with an inline error message and an OOB row for the successful bead.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {
			ID: "gd-sch1", Type: "schedule", Status: "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"enabled": "false"},
		},
		"gd-task1": {ID: "gd-task1", Type: "task", Status: "open"},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-task1", "gd-sch1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "is not a trigger") {
		t.Errorf("expected inline error\nbody=%s", body)
	}
	// OOB row for the successful bead only.
	if !strings.Contains(body, `id="trigger-row-gd-sch1"`) {
		t.Errorf("expected OOB row for gd-sch1\nbody=%s", body)
	}
	if strings.Contains(body, `id="trigger-row-gd-task1"`) {
		t.Errorf("must not OOB-swap the failed bead's row\nbody=%s", body)
	}
}

func TestTriggersBulkEnable_AuditCommentFailureNonFatal(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {
			ID: "gd-sch1", Type: "schedule", Status: "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"enabled": "false"},
		},
	}
	f.addCommentErr = errors.New("daemon down")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-sch1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 (comment failure non-fatal), got %d", w.Code)
	}
	if f.updateFieldsCalls != 1 {
		t.Errorf("UpdateBeadFieldsTyped: want 1 call, got %d", f.updateFieldsCalls)
	}
}

func TestTriggersBulkEnable_FieldUpdateFailureSkipsAudit(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-sch1": {
			ID: "gd-sch1", Type: "schedule", Status: "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"enabled": "false"},
		},
	}
	f.updateFieldsErr = errors.New("daemon down")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-sch1"))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status: want 502, got %d", w.Code)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("audit must not post when field write failed, got %d", f.addCommentCalls)
	}
}

func TestTriggersBulkBar_RendersInTriggersPage(t *testing.T) {
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
		`data-bulk-scope="triggers"`,
		`id="triggers-bulk-bar"`,
		`hx-post="/triggers/bulk/enable"`,
		`hx-post="/triggers/bulk/disable"`,
		`class="trigger-row-select"`,
		`data-bulk-confirm-over="5"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/triggers page missing %q", want)
		}
	}
}

func TestTriggersBulkBar_OpenRowsHaveCheckboxClosedDoNot(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-open", Title: "Open one", Type: "schedule", Status: "open",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
			{
				ID: "gd-closed", Title: "Closed one", Type: "schedule", Status: "closed",
				UpdatedAt: time.Now(),
				Fields:    map[string]string{"cron": "0 9 * * *", "enabled": "true"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest(http.MethodGet, "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Open row has a checkbox.
	if !strings.Contains(body, `value="gd-open"`) {
		t.Errorf("open row should have a checkbox with value=gd-open\nbody=%s", body)
	}
	// Closed row's checkbox must be absent.
	if strings.Contains(body, `value="gd-closed"`) {
		t.Errorf("closed row should NOT have a checkbox\nbody=%s", body)
	}
}

func TestTriggersBulkEnable_EventModeGardener(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-evg1": eventGardenerBead("gd-evg1", "gd-plan1", map[string]string{"enabled": "false"}),
		"gd-evg2": eventGardenerBead("gd-evg2", "gd-plan2", map[string]string{"enabled": "false"}),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-evg1", "gd-evg2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 2 {
		t.Fatalf("UpdateBeadFieldsTyped: want 2 calls, got %d", f.updateFieldsCalls)
	}
	if len(f.updateFieldsLastVal) != 1 || f.updateFieldsLastVal["enabled"] != true {
		t.Errorf("patch: want exactly {enabled:true}, got %v", f.updateFieldsLastVal)
	}
	body := w.Body.String()
	if strings.Contains(body, "is not a trigger") {
		t.Errorf("event-mode gardener rejected as non-trigger\nbody=%s", body)
	}
	for _, id := range []string{"gd-evg1", "gd-evg2"} {
		if !strings.Contains(body, `id="trigger-row-`+id+`"`) {
			t.Errorf("missing OOB row %s\nbody=%s", id, body)
		}
	}
}

func TestTriggersBulkDisable_SkipsSteward(t *testing.T) {
	// A land steward is single-row toggleable but never swept by a bulk
	// action; the gardener next to it in the selection still flips.
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-stew": stewardBead("gd-stew", nil),
		"gd-evg1": eventGardenerBead("gd-evg1", "gd-plan1", nil),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("disable", "gd-stew", "gd-evg1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 || f.updateFieldsLastID != "gd-evg1" {
		t.Errorf("UpdateBeadFieldsTyped: want 1 call on gd-evg1, got %d (last %q)", f.updateFieldsCalls, f.updateFieldsLastID)
	}
	if !strings.Contains(w.Body.String(), "gd-stew is a land steward") {
		t.Errorf("expected inline steward-skip error\nbody=%s", w.Body.String())
	}
}

func TestTriggersBulkEnable_SkipsTriggerMultiNonGardener(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-multi": {ID: "gd-multi", Type: "trigger", Status: "open", Labels: []string{"trigger:multi"}},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, triggersBulkReq("enable", "gd-multi"))

	if f.updateFieldsCalls != 0 {
		t.Errorf("non-gardener trigger:multi must not be written, got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "is not a trigger") {
		t.Errorf("expected inline non-trigger error\nbody=%s", w.Body.String())
	}
}
