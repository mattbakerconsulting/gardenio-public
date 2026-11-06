package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Auto-link M3 (ka-0000003164): a PATCH that transitions status to
// in_progress and carries X-Trigger-Bead must wire a triggered-by dep
// from the claimed bead to the trigger bead — but only if no
// parent-child or triggered-by dep already exists.

// seedClaimableTask installs a task bead that can be claimed (open, no deps).
func seedClaimableTask(ms *mockStore, id string) {
	ms.beads[id] = &model.Bead{
		ID:     id,
		Kind:   model.KindIssue,
		Type:   model.TypeTask,
		Title:  "claimable " + id,
		Status: model.StatusOpen,
	}
}

func claimPatch(t *testing.T, h http.Handler, taskID, trigger string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"status":   "in_progress",
		"assignee": "gardenio-test-claimer",
	})
	req := httptest.NewRequest(http.MethodPatch, "/v1/beads/"+taskID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "gardenio-test-claimer")
	if trigger != "" {
		req.Header.Set("X-Trigger-Bead", trigger)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func findDep(deps []*model.Dependency, depType model.DependencyType, target string) *model.Dependency {
	for _, d := range deps {
		if d.Type == depType && d.DependsOnID == target {
			return d
		}
	}
	return nil
}

func TestClaim_TriggeredBy_FiresWhenNoParentDep(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const taskID = "gd-task-tb1"
	const triggerID = "gd-trig-tb1"
	seedClaimableTask(ms, taskID)

	rec := claimPatch(t, h, taskID, triggerID)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if dep := findDep(ms.deps[taskID], model.DepTriggeredBy, triggerID); dep == nil {
		t.Fatalf("expected triggered-by dep to %s, got %+v", triggerID, ms.deps[taskID])
	}
}

func TestClaim_TriggeredBy_SkipsWhenParentChildExists(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const taskID = "gd-task-tb2"
	const triggerID = "gd-trig-tb2"
	const parentID = "gd-parent-tb2"
	seedClaimableTask(ms, taskID)
	ms.deps[taskID] = []*model.Dependency{{
		BeadID: taskID, DependsOnID: parentID, Type: model.DepParentChild,
	}}

	rec := claimPatch(t, h, taskID, triggerID)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if dep := findDep(ms.deps[taskID], model.DepTriggeredBy, triggerID); dep != nil {
		t.Fatalf("expected NO triggered-by dep (parent-child already present); got %+v", dep)
	}
}

func TestClaim_TriggeredBy_SkipsWhenTriggeredByDepExists(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const taskID = "gd-task-tb3"
	const triggerID = "gd-trig-tb3"
	const otherTrigger = "gd-other-tb3"
	seedClaimableTask(ms, taskID)
	ms.deps[taskID] = []*model.Dependency{{
		BeadID: taskID, DependsOnID: otherTrigger, Type: model.DepTriggeredBy,
	}}

	rec := claimPatch(t, h, taskID, triggerID)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if dep := findDep(ms.deps[taskID], model.DepTriggeredBy, triggerID); dep != nil {
		t.Fatalf("expected NO new triggered-by dep (one already present); got %+v", dep)
	}
	// existing dep untouched
	if dep := findDep(ms.deps[taskID], model.DepTriggeredBy, otherTrigger); dep == nil {
		t.Fatalf("expected pre-existing triggered-by dep to remain")
	}
}

func TestClaim_TriggeredBy_NoHeaderIsNoOp(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const taskID = "gd-task-tb4"
	seedClaimableTask(ms, taskID)

	rec := claimPatch(t, h, taskID, "") // no header
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if len(ms.deps[taskID]) != 0 {
		t.Fatalf("expected no deps, got %+v", ms.deps[taskID])
	}
}

func TestClaim_TriggeredBy_SelfReferenceIsNoOp(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const taskID = "gd-task-tb5"
	seedClaimableTask(ms, taskID)

	rec := claimPatch(t, h, taskID, taskID) // self
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if len(ms.deps[taskID]) != 0 {
		t.Fatalf("expected no self-referential dep, got %+v", ms.deps[taskID])
	}
}

func TestClaim_TriggeredBy_InvalidHeaderIsNoOp(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const taskID = "gd-task-tb6"
	seedClaimableTask(ms, taskID)

	// "not-a-bead-id" lacks the gd- prefix; sanitizer rejects, no header is
	// propagated, and no dep is written.
	rec := claimPatch(t, h, taskID, "not-a-bead-id")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if len(ms.deps[taskID]) != 0 {
		t.Fatalf("expected no deps from invalid header, got %+v", ms.deps[taskID])
	}
}

func TestClaim_TriggeredBy_NonClaimUpdateIsNoOp(t *testing.T) {
	// A non-status update (e.g. labels-only) should not trigger the dep
	// insert even if the header is present — the claim semantics hinge on
	// the status transition to in_progress.
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const taskID = "gd-task-tb7"
	const triggerID = "gd-trig-tb7"
	seedClaimableTask(ms, taskID)

	body, _ := json.Marshal(map[string]any{"title": "renamed"})
	req := httptest.NewRequest(http.MethodPatch, "/v1/beads/"+taskID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Trigger-Bead", triggerID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	if dep := findDep(ms.deps[taskID], model.DepTriggeredBy, triggerID); dep != nil {
		t.Fatalf("expected NO triggered-by dep on non-claim update; got %+v", dep)
	}
}

func TestSanitizeTriggerBeadHeader(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOk bool
	}{
		{"gd-ABC123xyz", "gd-ABC123xyz", true},
		{"gd-task-tb1", "gd-task-tb1", true},
		{"  gd-XYZ  ", "gd-XYZ", true},
		{"", "", false},
		{"gd-", "", false},
		{"abc-XYZ", "", false},
		{"gd-XYZ!", "", false},
		{"gd-XYZ\nINJECT", "", false},
		{"gd-with space", "", false},
		// 64-char overall length cap
		{"gd-" + repeatChar('a', 65), "", false},
	}
	for _, tc := range cases {
		got, ok := sanitizeTriggerBeadHeader(tc.in)
		if ok != tc.wantOk || got != tc.want {
			t.Errorf("sanitizeTriggerBeadHeader(%q) = (%q, %v); want (%q, %v)",
				tc.in, got, ok, tc.want, tc.wantOk)
		}
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
