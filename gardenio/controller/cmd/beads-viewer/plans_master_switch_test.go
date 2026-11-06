package main

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// masterSwitchFake seeds a plans-index-capable fake with no plans (the index
// renders the master switch regardless) and optionally the two pause gates.
func masterSwitchFake(t *testing.T, gardenersPaused, schedulerPaused bool) *fakeDaemon {
	t.Helper()
	f := newFake()
	// The plans index lists Types=[epic],Labels=[plan]; leave it empty so the
	// test isolates the master switch. planProjectOptions calls ListProjectBeads,
	// which newFake handles.
	f.listResp = &beadsapi.ListBeadsResult{}
	if gardenersPaused {
		f.beads["gate-cap"] = &beadsapi.BeadDetail{
			ID: "gate-cap", Title: "gardeners-pause", Type: "gate", Status: "open",
			CreatedBy: "oidc:matt@x", UpdatedAt: time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC),
		}
	}
	if schedulerPaused {
		f.beads["gate-sched"] = &beadsapi.BeadDetail{
			ID: "gate-sched", Title: "scheduler-pause", Type: "gate", Status: "open",
			CreatedBy: "oidc:matt@x", UpdatedAt: time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC),
		}
	}
	return f
}

func TestPlansMasterSwitch_RunningStateNoBanner(t *testing.T) {
	mux := mkServer(t, masterSwitchFake(t, false, false))
	req := httptest.NewRequest("GET", "/plans", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /plans = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The switch renders in the "all running" state...
	if !strings.Contains(body, `id="plans-master-switch"`) {
		t.Fatal("master switch region missing")
	}
	if !strings.Contains(body, "all running") {
		t.Errorf("expected 'all running' switch label, body=%s", firstN(body, 400))
	}
	// ...and NO global-pause banner is present.
	if strings.Contains(body, "banner-warn plans-master-banner") {
		t.Error("banner rendered when no gate is engaged")
	}
	// Operator sees the toggle form/lever.
	if !strings.Contains(body, "/plans/master-switch/toggle") {
		t.Error("operator did not see the master switch toggle form")
	}
}

func TestPlansMasterSwitch_GardenersPausedShowsBanner(t *testing.T) {
	mux := mkServer(t, masterSwitchFake(t, true, false))
	req := httptest.NewRequest("GET", "/plans", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /plans = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "banner-warn plans-master-banner") {
		t.Fatal("global-pause banner missing while gardeners-pause is engaged")
	}
	if !strings.Contains(body, "All plan gardeners are paused") {
		t.Errorf("banner did not name the gardeners pause, body=%s", firstN(body, 600))
	}
	// The banner must state the pause SUPERSEDES the row/bulk toggles.
	if !strings.Contains(body, "supersede") {
		t.Error("banner did not warn that the master pause supersedes per-row toggles")
	}
	// Switch reflects the paused state.
	if !strings.Contains(body, "all paused") {
		t.Error("switch label did not reflect the paused state")
	}
}

func TestPlansMasterSwitch_SchedulerPausedShowsBanner(t *testing.T) {
	mux := mkServer(t, masterSwitchFake(t, false, true))
	req := httptest.NewRequest("GET", "/plans", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "banner-warn plans-master-banner") {
		t.Fatal("banner missing while scheduler-pause is engaged")
	}
	if !strings.Contains(body, "Scheduled firing is paused") {
		t.Errorf("banner did not name the scheduler pause, body=%s", firstN(body, 600))
	}
	// Event-mode plan gardeners (trigger:multi) are silenced by the same gate
	// (gd-gCN4m4DYxc), so the banner must not say only scheduled ones are.
	if !strings.Contains(body, "every plan gardener, scheduled or event-mode, are silenced") {
		t.Errorf("banner does not say event-mode plan gardeners are silenced, body=%s", firstN(body, 600))
	}
	// Scheduler-pause alone must NOT flip the master switch to paused — the
	// switch tracks gardeners-pause only.
	if !strings.Contains(body, "all running") {
		t.Error("switch flipped to paused on scheduler-pause alone; it must track gardeners-pause only")
	}
}

// TestPlansMasterSwitch_ViewerHidesLever pins RBAC parity: a read-only viewer
// sees the banner + state but never the toggle form (mirrors the operatorOnly
// POST route).
func TestPlansMasterSwitch_ViewerHidesLever(t *testing.T) {
	mux := mkServer(t, masterSwitchFake(t, true, false))
	req := httptest.NewRequest("GET", "/plans", nil)
	req.Header.Set("X-Auth-Request-Email", "viewer@example.com")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	// State + banner still render.
	if !strings.Contains(body, "banner-warn plans-master-banner") {
		t.Error("viewer did not see the global-pause banner")
	}
	if !strings.Contains(body, "all paused") {
		t.Error("viewer did not see the paused state chip")
	}
	// But the operator lever is withheld.
	if strings.Contains(body, "/plans/master-switch/toggle") {
		t.Error("viewer leaked the operator master switch toggle form (RBAC parity broken)")
	}
}

// TestPlansMasterSwitchToggle_PausesWhenRunning opens the gardeners-pause gate
// when none is engaged.
func TestPlansMasterSwitchToggle_PausesWhenRunning(t *testing.T) {
	f := masterSwitchFake(t, false, false)
	// Persist the minted gate into f.beads so the handler's post-write re-read
	// (pausegate.Find) sees it — exercising the full open → re-render-shows-paused
	// round-trip rather than just the write.
	f.createResp = "gate-cap"
	f.createOnSuccess = func(fd *fakeDaemon, id string, req beadsapi.CreateBeadRequest) {
		fd.beads[id] = &beadsapi.BeadDetail{ID: id, Title: req.Title, Type: req.Type, Status: "open", CreatedBy: req.CreatedBy}
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/master-switch/toggle", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("toggle = %d, body=%s", w.Code, w.Body.String())
	}
	// A type=gate gardeners-pause bead was created, attributed to the operator.
	var found bool
	for _, r := range f.createReqs {
		if r.Type == "gate" && r.Title == "gardeners-pause" {
			found = true
			if r.CreatedBy != "oidc:op@example.com" {
				t.Errorf("gate CreatedBy = %q, want canonical oidc operator id", r.CreatedBy)
			}
		}
	}
	if !found {
		t.Fatalf("toggle did not open a gardeners-pause gate; creates=%+v", f.createReqs)
	}
	// The re-rendered fragment reflects the now-paused state + shows the banner.
	body := w.Body.String()
	if !strings.Contains(body, `id="plans-master-switch"`) {
		t.Fatal("toggle response was not the master switch fragment")
	}
	if !strings.Contains(body, "all paused") {
		t.Errorf("pause re-render did not show the paused state, body=%s", firstN(body, 500))
	}
	if !strings.Contains(body, "banner-warn plans-master-banner") {
		t.Error("pause re-render did not surface the global-pause banner")
	}
}

// TestPlansMasterSwitchToggle_ResumesWhenPaused closes the open gardeners-pause
// gate, attributed to the operator.
func TestPlansMasterSwitchToggle_ResumesWhenPaused(t *testing.T) {
	f := masterSwitchFake(t, true, false)
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/master-switch/toggle", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("toggle = %d, body=%s", w.Code, w.Body.String())
	}
	if f.closeCalls != 1 || f.closeLastID != "gate-cap" {
		t.Fatalf("toggle did not close the gardeners-pause gate: closeCalls=%d lastID=%q", f.closeCalls, f.closeLastID)
	}
	if f.closeLastBy != "oidc:op@example.com" {
		t.Errorf("gate closed by %q, want canonical oidc operator id", f.closeLastBy)
	}
	// No new gate is opened on a resume.
	for _, r := range f.createReqs {
		if r.Type == "gate" {
			t.Errorf("resume unexpectedly created a gate: %+v", r)
		}
	}
	// After close, the seeded gate is closed in the fake, so the re-render reads
	// "all running" (no banner).
	body := w.Body.String()
	if !strings.Contains(body, "all running") {
		t.Errorf("resume re-render did not show the running state, body=%s", firstN(body, 400))
	}
	if strings.Contains(body, "banner-warn plans-master-banner") {
		t.Error("banner still rendered after resume")
	}
}

// TestPlansMasterSwitchToggle_RequiresEmail rejects an unauthenticated POST.
func TestPlansMasterSwitchToggle_RequiresEmail(t *testing.T) {
	f := masterSwitchFake(t, false, false)
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	// Call the handler directly (bypassing operatorOnly) with no email to pin the
	// handler's own auth guard.
	req := httptest.NewRequest("POST", "/plans/master-switch/toggle", nil)
	w := httptest.NewRecorder()
	srv.handlePlansMasterSwitchToggle(w, req)
	if w.Code != 401 {
		t.Fatalf("unauthenticated toggle = %d, want 401", w.Code)
	}
}

// firstN truncates a string for readable failure messages.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
