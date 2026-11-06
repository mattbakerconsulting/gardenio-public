package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gardenerPauseReq builds a POST /gardener/{id}/pause-toggle request with the
// operator email header set so EmailFromRequest sees the operator (mirrors
// triggerToggleReq).
func gardenerPauseReq(id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/gardener/"+id+"/pause-toggle", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	return req
}

func gardenerTriggerBead(id string, fields map[string]string) *beadsapi.BeadDetail {
	if fields == nil {
		fields = map[string]string{}
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     "Epic drainer",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"role:gardener", "trigger:schedule"},
		UpdatedAt: time.Now(),
		Fields:    fields,
	}
}

// A running (enabled-by-default) trigger:schedule gardener pauses → enabled:false.
func TestGardenerPauseToggle_RunningToPaused(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap1": gardenerTriggerBead("gd-cap1", map[string]string{"cron": "0 9 * * *"}),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, gardenerPauseReq("gd-cap1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("UpdateBeadFieldsTyped: want 1 call, got %d", f.updateFieldsCalls)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != false {
		t.Errorf("enabled write: want \"false\", got %v", got)
	}
	// Response is the re-rendered toggle partial (the htmx swap target).
	if body := w.Body.String(); !strings.Contains(body, "gardener-pause-gd-cap1") {
		t.Errorf("response should render the gardener pause toggle, got %s", body)
	}
}

// A paused gardener (enabled:false) resumes → enabled:true.
func TestGardenerPauseToggle_PausedToRunning(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap2": gardenerTriggerBead("gd-cap2", map[string]string{"cron": "0 9 * * *", "enabled": "false"}),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, gardenerPauseReq("gd-cap2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := f.updateFieldsLastVal["enabled"]; got != true {
		t.Errorf("enabled write: want \"true\", got %v", got)
	}
	if body := w.Body.String(); !strings.Contains(body, "gardener-pause-gd-cap2") {
		t.Errorf("response should render the gardener pause toggle, got %s", body)
	}
}

// Legacy type=schedule gardener (default disabled) resumes → enabled:true.
func TestGardenerPauseToggle_LegacySchedule_MissingToTrue(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap3": {
			ID:        "gd-cap3",
			Type:      "schedule",
			Status:    "open",
			Labels:    []string{"role:gardener"},
			UpdatedAt: time.Now(),
			Fields:    map[string]string{"cron": "0 9 * * *"},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, gardenerPauseReq("gd-cap3"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := f.updateFieldsLastVal["enabled"]; got != true {
		t.Errorf("enabled write: want \"true\" (legacy default disabled), got %v", got)
	}
}

// The flip posts a one-line audit comment phrased in pause vocabulary.
func TestGardenerPauseToggle_PostsAuditComment(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap4": gardenerTriggerBead("gd-cap4", map[string]string{"cron": "0 9 * * *"}),
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, gardenerPauseReq("gd-cap4"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if f.addCommentCalls != 1 {
		t.Fatalf("AddComment: want 1 call, got %d", f.addCommentCalls)
	}
	if f.addCommentLastID != "gd-cap4" {
		t.Errorf("audit comment bead: want gd-cap4, got %q", f.addCommentLastID)
	}
	if !strings.Contains(f.addCommentLastText, "paused") {
		t.Errorf("audit comment should use pause vocabulary, got %q", f.addCommentLastText)
	}
}

// A closed (retired) gardener rejects a stray pause POST.
func TestGardenerPauseToggle_ClosedRejected(t *testing.T) {
	f := newFake()
	b := gardenerTriggerBead("gd-cap5", map[string]string{"cron": "0 9 * * *"})
	b.Status = "closed"
	f.beads = map[string]*beadsapi.BeadDetail{"gd-cap5": b}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, gardenerPauseReq("gd-cap5"))

	if w.Code != http.StatusConflict {
		t.Fatalf("status: want 409, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("closed gardener must not be mutated, got %d update calls", f.updateFieldsCalls)
	}
}

// A non-gardener bead (plain task) is rejected.
func TestGardenerPauseToggle_NonGardenerRejected(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-task1": {
			ID:        "gd-task1",
			Type:      "task",
			Status:    "open",
			UpdatedAt: time.Now(),
			Fields:    map[string]string{},
		},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, gardenerPauseReq("gd-task1"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("non-gardener must not be mutated, got %d update calls", f.updateFieldsCalls)
	}
}

// An unauthenticated request is rejected and posts no audit comment.
func TestGardenerPauseToggle_Unauthenticated(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap6": gardenerTriggerBead("gd-cap6", map[string]string{"cron": "0 9 * * *"}),
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/gardener/gd-cap6/pause-toggle", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("unauthenticated request must not succeed, got 200")
	}
	if f.addCommentCalls != 0 {
		t.Errorf("unauthenticated request must not post audit comment, got %d calls", f.addCommentCalls)
	}
}
