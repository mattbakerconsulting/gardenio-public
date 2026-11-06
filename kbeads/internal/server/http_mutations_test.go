package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/presence"
)

func TestCreateBeadRecordsEvent(t *testing.T) {
	_, ms, h := newTestServerAuth()
	// SECURITY (AUDIT §C3): even if a client supplies created_by, the server
	// must derive actor from the authenticated identity on the context.
	rec := doJSONAs(t, h, "POST", "/v1/beads", map[string]any{"title": "Test bead", "type": "task", "created_by": "evil-forged"}, "alice")
	requireStatus(t, rec, 201)
	requireEvent(t, ms, 1, "beads.bead.created")
	if ms.events[0].Actor != "alice" {
		t.Fatalf("expected actor=%q, got %q", "alice", ms.events[0].Actor)
	}
	// Regression: verify the forged client-supplied actor was ignored.
	if got := ms.beads[ms.events[0].BeadID].CreatedBy; got != "alice" {
		t.Fatalf("expected bead.CreatedBy=%q (from ctx), got %q", "alice", got)
	}
}

func TestDeleteBeadRecordsEvent(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-del1"] = &model.Bead{ID: "gd-del1", Title: "To delete", Status: model.StatusOpen}

	rec := doJSON(t, h, "DELETE", "/v1/beads/gd-del1", nil)
	requireStatus(t, rec, 204)
	requireEvent(t, ms, 1, "beads.bead.deleted")
	if ms.events[0].BeadID != "gd-del1" {
		t.Fatalf("expected bead_id=%q, got %q", "gd-del1", ms.events[0].BeadID)
	}
}

func TestAddCommentRecordsEvent(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-cmt1"] = &model.Bead{ID: "gd-cmt1", Title: "Bead with comment", Status: model.StatusOpen}

	// SECURITY (AUDIT §C3): client-supplied author must be ignored.
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-cmt1/comments", map[string]any{"author": "evil-forged", "text": "Hello world"}, "bob")
	requireStatus(t, rec, 201)
	requireEvent(t, ms, 1, "beads.comment.added")
	if ms.events[0].Actor != "bob" {
		t.Fatalf("expected actor=%q, got %q", "bob", ms.events[0].Actor)
	}
}

func TestAddLabelRecordsEvent(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-lbl1"] = &model.Bead{ID: "gd-lbl1", Title: "Bead with label", Status: model.StatusOpen}

	rec := doJSON(t, h, "POST", "/v1/beads/gd-lbl1/labels", map[string]any{"label": "urgent"})
	requireStatus(t, rec, 201)
	requireEvent(t, ms, 1, "beads.label.added")
}

func TestRemoveLabelRecordsEvent(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-lbl2"] = &model.Bead{ID: "gd-lbl2", Title: "Bead losing label", Status: model.StatusOpen}
	// Seed the label being removed. This test previously deleted a label that
	// was never added and still asserted an event — the phantom-event bug fixed
	// in ka-0000000424. Only a delete that matches a row is a mutation now.
	ms.labels["gd-lbl2"] = []string{"urgent"}

	rec := doJSON(t, h, "DELETE", "/v1/beads/gd-lbl2/labels/urgent", nil)
	requireStatus(t, rec, 200)
	requireEvent(t, ms, 1, "beads.label.removed")
	var labelEvt events.LabelRemoved
	if err := json.Unmarshal(ms.events[0].Payload, &labelEvt); err != nil {
		t.Fatalf("failed to unmarshal event payload: %v", err)
	}
	if labelEvt.Label != "urgent" {
		t.Fatalf("expected label=%q in payload, got %q", "urgent", labelEvt.Label)
	}
	// The rows-affected signal must ride in the body for the CLI to read it.
	var body struct {
		Removed *bool `json:"removed"`
	}
	decodeJSON(t, rec, &body)
	if body.Removed == nil || !*body.Removed {
		t.Errorf("body removed = %v; want an explicit true", body.Removed)
	}
}

// TestRemoveLabelNoMatchRecordsNothing pins the no-op path: removing a label
// the bead never carried matches no row, which must stay a 2xx (idempotent
// callers treat any non-2xx as fatal) while reporting removed=false and
// recording NO event. Before ka-0000000424 this fabricated a "label removed"
// history entry for a label that never existed.
func TestRemoveLabelNoMatchRecordsNothing(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-lbl3"] = &model.Bead{ID: "gd-lbl3", Title: "Bead", Status: model.StatusOpen}
	// A real `urgent` label exists; the request asks for one it never had.
	ms.labels["gd-lbl3"] = []string{"urgent"}

	rec := doJSON(t, h, "DELETE", "/v1/beads/gd-lbl3/labels/not-a-real-label", nil)
	requireStatus(t, rec, 200)

	var body struct {
		Removed *bool `json:"removed"`
	}
	decodeJSON(t, rec, &body)
	if body.Removed == nil {
		t.Fatal("removed absent; want an explicit false so a new client can tell it from an old daemon")
	}
	if *body.Removed {
		t.Error("removed = true for a label that never existed; want false")
	}
	if len(ms.events) != 0 {
		t.Errorf("no-op remove recorded %d event(s); want 0", len(ms.events))
	}
	// And the real label is untouched.
	if got := len(ms.labels["gd-lbl3"]); got != 1 {
		t.Errorf("len(labels) = %d; want the urgent label to survive", got)
	}
}

func TestAddDependencyRecordsEvent(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-dep1"] = &model.Bead{ID: "gd-dep1", Title: "A", Status: model.StatusOpen}
	ms.beads["gd-dep2"] = &model.Bead{ID: "gd-dep2", Title: "B", Status: model.StatusOpen}

	// SECURITY (AUDIT §C3): client-supplied created_by must be ignored.
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-dep1/dependencies", map[string]any{
		"depends_on_id": "gd-dep2", "type": "blocks", "created_by": "evil-forged",
	}, "carol")
	requireStatus(t, rec, 201)
	requireEvent(t, ms, 1, "beads.dependency.added")
	if ms.events[0].Actor != "carol" {
		t.Fatalf("expected actor=%q, got %q", "carol", ms.events[0].Actor)
	}
}

// TestAddDependency_RejectsParentChildCycle verifies the dep-add cycle guard
// (ka-0000006899): a parent-child edge that would close a cycle is rejected
// with 409 and no dependency-added event is recorded.
func TestAddDependency_RejectsParentChildCycle(t *testing.T) {
	_, ms, h := newTestServerAuth()
	// Plain tasks so the reversed-epic canonicalizer (ka-0000006880) does not
	// fire and swap the edge before the cycle check runs.
	ms.beads["gd-cyc-a"] = &model.Bead{ID: "gd-cyc-a", Title: "A", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-cyc-b"] = &model.Bead{ID: "gd-cyc-b", Title: "B", Type: model.TypeTask, Status: model.StatusOpen}
	// Existing: gd-cyc-a is a child of gd-cyc-b.
	addPC(ms, "gd-cyc-a", "gd-cyc-b")

	// New edge would make gd-cyc-b a child of gd-cyc-a — closing a 2-cycle.
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-cyc-b/dependencies", map[string]any{
		"depends_on_id": "gd-cyc-a", "type": "parent-child",
	}, "carol")
	requireStatus(t, rec, 409)
	if len(ms.events) != 0 {
		t.Fatalf("expected no events on rejected cycle, got %d", len(ms.events))
	}
	if deps := ms.deps["gd-cyc-b"]; len(deps) != 0 {
		t.Fatalf("expected no dep written on rejected cycle, got %d", len(deps))
	}
}

// addDepStatic POSTs a parent-child dep-add through the STATIC primary-token
// interceptor path (the path `kd dep add` uses in agent pods / operator CLIs),
// optionally setting X-Force-Parent-Edge. Returns the recorder.
func addDepStatic(t *testing.T, h http.Handler, childID, parentID string, force bool) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"depends_on_id": parentID, "type": "parent-child"})
	req := httptest.NewRequest("POST", "/v1/beads/"+childID+"/dependencies", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer primary-token")
	// X-Operated-By makes the static-token resolver attributable (fail-closed
	// otherwise), matching a real agent-pod dep-add.
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	if force {
		req.Header.Set("X-Force-Parent-Edge", "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAddDependency_RejectsClosedParent verifies the closed-parent guard
// (ka-0000005756): an explicit parent-child edge onto an already-closed epic
// is rejected with 409 and no dependency-added event/edge is written.
func TestAddDependency_RejectsClosedParent(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
	ms.beads["gd-cp-child"] = &model.Bead{ID: "gd-cp-child", Title: "child", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-cp-dead"] = &model.Bead{ID: "gd-cp-dead", Title: "dead epic", Type: model.TypeEpic, Status: model.StatusClosed}

	rec := addDepStatic(t, h, "gd-cp-child", "gd-cp-dead", false)
	requireStatus(t, rec, 409)
	if len(ms.events) != 0 {
		t.Fatalf("expected no events on rejected closed-parent edge, got %d", len(ms.events))
	}
	if deps := ms.deps["gd-cp-child"]; len(deps) != 0 {
		t.Fatalf("expected no dep written on rejected closed-parent edge, got %d", len(deps))
	}
}

// TestAddDependency_ClosedParent_ForceOverride verifies X-Force-Parent-Edge
// (`kd dep add --force`) overrides the closed-parent guard so a deliberate
// re-parent onto a recently-closed epic still lands (ka-0000005756).
func TestAddDependency_ClosedParent_ForceOverride(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
	ms.beads["gd-cp-child"] = &model.Bead{ID: "gd-cp-child", Title: "child", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-cp-dead"] = &model.Bead{ID: "gd-cp-dead", Title: "dead epic", Type: model.TypeEpic, Status: model.StatusClosed}

	rec := addDepStatic(t, h, "gd-cp-child", "gd-cp-dead", true)
	requireStatus(t, rec, 201)
	if deps := ms.deps["gd-cp-child"]; len(deps) != 1 {
		t.Fatalf("expected 1 dep written with --force override, got %d", len(deps))
	}
}

// TestAddDependency_OpenParent_Accepted is the positive control: a parent-child
// edge onto an OPEN epic is unaffected by the closed-parent guard.
func TestAddDependency_OpenParent_Accepted(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
	ms.beads["gd-cp-child"] = &model.Bead{ID: "gd-cp-child", Title: "child", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-cp-live"] = &model.Bead{ID: "gd-cp-live", Title: "live epic", Type: model.TypeEpic, Status: model.StatusOpen}

	rec := addDepStatic(t, h, "gd-cp-child", "gd-cp-live", false)
	requireStatus(t, rec, 201)
	if deps := ms.deps["gd-cp-child"]; len(deps) != 1 {
		t.Fatalf("expected 1 dep written onto open parent, got %d", len(deps))
	}
}

func TestRemoveDependencyRecordsEvent(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-rdep1"] = &model.Bead{ID: "gd-rdep1", Title: "A", Status: model.StatusOpen}
	// Seed the edge being removed. This test previously deleted an edge that was
	// never added and still asserted an event — the phantom-event bug fixed in
	// ka-0000001966. Only a delete that matches a row is a mutation now.
	ms.deps["gd-rdep1"] = []*model.Dependency{
		{BeadID: "gd-rdep1", DependsOnID: "gd-rdep2", Type: model.DepBlocks},
	}

	rec := doJSON(t, h, "DELETE", "/v1/beads/gd-rdep1/dependencies?depends_on_id=gd-rdep2&type=blocks", nil)
	requireStatus(t, rec, 200)
	requireEvent(t, ms, 1, "beads.dependency.removed")
	var depEvt events.DependencyRemoved
	if err := json.Unmarshal(ms.events[0].Payload, &depEvt); err != nil {
		t.Fatalf("failed to unmarshal event payload: %v", err)
	}
	if depEvt.DependsOnID != "gd-rdep2" || depEvt.Type != "blocks" {
		t.Fatalf("expected depends_on_id=%q type=%q, got %q %q", "gd-rdep2", "blocks", depEvt.DependsOnID, depEvt.Type)
	}
	// The rows-affected signal must ride in the body for the CLI to read it.
	var body struct {
		Removed *bool `json:"removed"`
	}
	decodeJSON(t, rec, &body)
	if body.Removed == nil || !*body.Removed {
		t.Errorf("body removed = %v; want an explicit true", body.Removed)
	}
}

// TestRemoveDependencyNoMatchRecordsNothing pins the no-op path: the wrong
// --type matches no row, which must stay a 2xx (idempotent callers treat any
// non-2xx as fatal) while reporting removed=false and recording NEITHER an
// event nor an audit row. Before ka-0000001966 this fabricated a
// "dependency removed" history entry for an edge that never existed.
func TestRemoveDependencyNoMatchRecordsNothing(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-rdep1"] = &model.Bead{ID: "gd-rdep1", Title: "A", Status: model.StatusOpen}
	// A real `related` edge exists; the request asks for parent-child.
	ms.deps["gd-rdep1"] = []*model.Dependency{
		{BeadID: "gd-rdep1", DependsOnID: "gd-rdep2", Type: model.DepRelated},
	}

	rec := doJSON(t, h, "DELETE", "/v1/beads/gd-rdep1/dependencies?depends_on_id=gd-rdep2&type=parent-child", nil)
	requireStatus(t, rec, 200)

	var body struct {
		Removed *bool `json:"removed"`
	}
	decodeJSON(t, rec, &body)
	if body.Removed == nil {
		t.Fatal("removed absent; want an explicit false so a new client can tell it from an old daemon")
	}
	if *body.Removed {
		t.Error("removed = true for an edge that never existed; want false")
	}
	if len(ms.events) != 0 {
		t.Errorf("no-op remove recorded %d event(s); want 0", len(ms.events))
	}
	if len(ms.mutations) != 0 {
		t.Errorf("no-op remove recorded %d audit row(s); want 0", len(ms.mutations))
	}
	// And the real edge is untouched.
	if got := len(ms.deps["gd-rdep1"]); got != 1 {
		t.Errorf("len(deps) = %d; want the related edge to survive", got)
	}
}

func TestHandleUpdateBead(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-upd1"] = &model.Bead{ID: "gd-upd1", Title: "Original", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-upd1", map[string]any{"title": "Updated"})
	requireStatus(t, rec, 200)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.Title != "Updated" {
		t.Fatalf("expected title=%q, got %q", "Updated", bead.Title)
	}
}

func TestHandleUpdateBead_LabelsReconciled(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-upd2"] = &model.Bead{ID: "gd-upd2", Title: "Bead", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.labels["gd-upd2"] = []string{"a", "b"}

	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-upd2", map[string]any{"labels": []string{"b", "c"}})
	requireStatus(t, rec, 200)

	// Verify via GET that labels were reconciled in the store.
	rec = doJSON(t, h, "GET", "/v1/beads/gd-upd2", nil)
	requireStatus(t, rec, 200)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if len(bead.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(bead.Labels), bead.Labels)
	}
	labelSet := map[string]bool{}
	for _, l := range bead.Labels {
		labelSet[l] = true
	}
	if !labelSet["b"] || !labelSet["c"] {
		t.Fatalf("expected labels [b, c], got %v", bead.Labels)
	}
}

func TestHandleUpdateBead_NotFound(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "PATCH", "/v1/beads/nonexistent", map[string]any{"title": "x"})
	requireStatus(t, rec, 404)
}

func TestHandleUpdateBead_InvalidJSON(t *testing.T) {
	_, _, h := newTestServer()
	req := httptest.NewRequest("PATCH", "/v1/beads/gd-x", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 400)
}

func TestHandleCloseBead(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-cls1"] = &model.Bead{ID: "gd-cls1", Title: "To close", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	rec := doJSON(t, h, "POST", "/v1/beads/gd-cls1/close", nil)
	requireStatus(t, rec, 200)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.Status != model.StatusClosed {
		t.Fatalf("expected status=%q, got %q", model.StatusClosed, bead.Status)
	}
}

func TestHandleCloseBead_WithClosedBy(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-cls2"] = &model.Bead{ID: "gd-cls2", Title: "To close", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	// SECURITY (AUDIT §C3): client-supplied closed_by must be ignored; server
	// derives ClosedBy from the authenticated identity.
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-cls2/close", map[string]any{"closed_by": "evil-forged"}, "alice")
	requireStatus(t, rec, 200)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.ClosedBy != "alice" {
		t.Fatalf("expected closed_by=%q, got %q", "alice", bead.ClosedBy)
	}
}

func TestHandleCloseBead_RecordsEvent(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-cls3"] = &model.Bead{ID: "gd-cls3", Title: "To close", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	// SECURITY (AUDIT §C3): client-supplied closed_by must be ignored.
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-cls3/close", map[string]any{"closed_by": "evil-forged"}, "bob")
	requireStatus(t, rec, 200)
	requireEvent(t, ms, 1, "beads.bead.closed")
	if ms.events[0].Actor != "bob" {
		t.Fatalf("expected actor=%q, got %q", "bob", ms.events[0].Actor)
	}
}

func TestHandleCloseBead_NotFound(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads/nonexistent/close", nil)
	requireStatus(t, rec, 404)
}

func TestHandleListBeads_FilterByLabels(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-l1"] = &model.Bead{ID: "gd-l1", Title: "Has urgent", Status: model.StatusOpen}
	ms.labels["gd-l1"] = []string{"urgent"}
	ms.beads["gd-l2"] = &model.Bead{ID: "gd-l2", Title: "Has frontend", Status: model.StatusOpen}
	ms.labels["gd-l2"] = []string{"frontend"}

	rec := doJSON(t, h, "GET", "/v1/beads?labels=urgent", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1, got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-l1" {
		t.Fatalf("expected bead gd-l1, got %q", result.Beads[0].ID)
	}
}

func TestHandleListBeads_FilterByPriority(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-p1"] = &model.Bead{ID: "gd-p1", Title: "High", Status: model.StatusOpen, Priority: 3}
	ms.beads["gd-p2"] = &model.Bead{ID: "gd-p2", Title: "Low", Status: model.StatusOpen, Priority: 1}

	rec := doJSON(t, h, "GET", "/v1/beads?priority=3", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1, got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-p1" {
		t.Fatalf("expected bead gd-p1, got %q", result.Beads[0].ID)
	}
}

func TestHandleListBeads_FilterByFields(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-f1"] = &model.Bead{
		ID: "gd-f1", Title: "Has images", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"jira_has_images":"true"}`),
	}
	ms.beads["gd-f2"] = &model.Bead{
		ID: "gd-f2", Title: "No images", Status: model.StatusOpen,
		Fields: json.RawMessage(`{}`),
	}
	ms.beads["gd-f3"] = &model.Bead{
		ID: "gd-f3", Title: "No fields", Status: model.StatusOpen,
	}

	rec := doJSON(t, h, "GET", "/v1/beads?field=jira_has_images%3Dtrue", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1, got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-f1" {
		t.Fatalf("expected bead gd-f1, got %q", result.Beads[0].ID)
	}
}

func TestHandleCreateBead_LabelFailure_ReturnsError(t *testing.T) {
	_, ms, h := newTestServer()
	ms.addLabelErr = fmt.Errorf("label store down")

	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "With labels", "type": "task", "labels": []string{"x"},
	})
	requireStatus(t, rec, 500)
}

func TestHandleCreateBead_WithLabels_AllPersisted(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "With labels", "type": "task", "labels": []string{"a", "b", "c"},
	})
	requireStatus(t, rec, 201)
	var bead model.Bead
	decodeJSON(t, rec, &bead)

	// Verify all 3 labels are in the store.
	if len(ms.labels[bead.ID]) != 3 {
		t.Fatalf("expected 3 labels, got %d", len(ms.labels[bead.ID]))
	}

	// Verify GET returns them.
	rec = doJSON(t, h, "GET", "/v1/beads/"+bead.ID, nil)
	requireStatus(t, rec, 200)
	var got model.Bead
	decodeJSON(t, rec, &got)
	if len(got.Labels) != 3 {
		t.Fatalf("expected 3 labels on GET, got %d", len(got.Labels))
	}
}

func TestHandleUpdateBead_LabelsPreservedWhenNotSpecified(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-upd3"] = &model.Bead{ID: "gd-upd3", Title: "Original", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.labels["gd-upd3"] = []string{"keep-me"}

	// Update only title, don't send labels.
	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-upd3", map[string]any{"title": "New title"})
	requireStatus(t, rec, 200)

	// Labels should be unchanged.
	if len(ms.labels["gd-upd3"]) != 1 || ms.labels["gd-upd3"][0] != "keep-me" {
		t.Fatalf("expected labels to be preserved, got %v", ms.labels["gd-upd3"])
	}
}

func TestHandleUpdateBead_ClearLabels(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-upd4"] = &model.Bead{ID: "gd-upd4", Title: "Has labels", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.labels["gd-upd4"] = []string{"a", "b"}

	// Update with empty labels array to clear them.
	rec := doJSON(t, h, "PATCH", "/v1/beads/gd-upd4", map[string]any{"labels": []string{}})
	requireStatus(t, rec, 200)

	if len(ms.labels["gd-upd4"]) != 0 {
		t.Fatalf("expected 0 labels, got %d: %v", len(ms.labels["gd-upd4"]), ms.labels["gd-upd4"])
	}
}

func TestHandleGraph_Empty(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/graph", nil)
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)
	if len(result.Nodes) != 0 || len(result.Edges) != 0 {
		t.Fatalf("expected empty graph, got %d nodes, %d edges", len(result.Nodes), len(result.Edges))
	}
	if result.Stats == nil {
		t.Fatal("expected stats to be non-nil")
	}
}

func TestHandleGraph_WithData(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-g1"] = &model.Bead{ID: "gd-g1", Title: "Open bead", Status: model.StatusOpen, Kind: model.KindIssue, Type: model.TypeTask}
	ms.beads["gd-g2"] = &model.Bead{ID: "gd-g2", Title: "In progress", Status: model.StatusInProgress, Kind: model.KindIssue, Type: model.TypeTask}
	ms.beads["gd-g3"] = &model.Bead{ID: "gd-g3", Title: "Blocked", Status: model.StatusBlocked, Kind: model.KindIssue, Type: model.TypeBug}
	ms.deps["gd-g2"] = []*model.Dependency{
		{BeadID: "gd-g2", DependsOnID: "gd-g1", Type: model.DepBlocks},
	}
	ms.labels["gd-g1"] = []string{"urgent"}

	rec := doJSON(t, h, "GET", "/v1/graph", nil)
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)

	if len(result.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(result.Nodes))
	}
	if len(result.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(result.Edges))
	}
	if result.Stats.TotalOpen != 1 || result.Stats.TotalInProgress != 1 || result.Stats.TotalBlocked != 1 {
		t.Fatalf("unexpected stats: %+v", result.Stats)
	}

	// Verify node fields are properly projected.
	nodeMap := make(map[string]model.GraphNode)
	for _, n := range result.Nodes {
		nodeMap[n.ID] = n
	}
	if nodeMap["gd-g1"].IssueType != "task" {
		t.Fatalf("expected issue_type=task, got %q", nodeMap["gd-g1"].IssueType)
	}
}

func TestHandleGraph_POST(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-p1"] = &model.Bead{ID: "gd-p1", Title: "Task", Status: model.StatusOpen, Type: model.TypeTask}
	ms.beads["gd-p2"] = &model.Bead{ID: "gd-p2", Title: "Bug", Status: model.StatusInProgress, Type: model.TypeBug}

	rec := doJSON(t, h, "POST", "/v1/graph", map[string]any{
		"status": []string{"open", "in_progress"},
		"limit":  100,
	})
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)
	if len(result.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(result.Nodes))
	}
}

func TestHandleGraph_WithLimit(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now()
	ms.beads["gd-gl1"] = &model.Bead{ID: "gd-gl1", Title: "A", Status: model.StatusOpen, UpdatedAt: now, Type: model.TypeTask}
	ms.beads["gd-gl2"] = &model.Bead{ID: "gd-gl2", Title: "B", Status: model.StatusOpen, UpdatedAt: now, Type: model.TypeTask}
	ms.beads["gd-gl3"] = &model.Bead{ID: "gd-gl3", Title: "C", Status: model.StatusOpen, UpdatedAt: now, Type: model.TypeTask}

	rec := doJSON(t, h, "GET", "/v1/graph?limit=2", nil)
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)
	// Stats should still count all 3 beads.
	if result.Stats.TotalOpen != 3 {
		t.Fatalf("expected total_open=3, got %d", result.Stats.TotalOpen)
	}
}

func TestHandleGraph_AgentNodes(t *testing.T) {
	srv, ms, h := newTestServer()
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Title: "WIP task", Status: model.StatusInProgress, Type: model.TypeTask, Assignee: "agent-1"}

	// Record presence so the agent shows up.
	srv.Presence.RecordHookEvent(presence.HookEvent{Actor: "agent-1", HookType: "PreToolUse"})

	rec := doJSON(t, h, "POST", "/v1/graph", map[string]any{
		"include_agents": true,
	})
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)

	// Should have the bead node + agent node.
	if len(result.Nodes) < 2 {
		t.Fatalf("expected at least 2 nodes (bead + agent), got %d", len(result.Nodes))
	}

	// Should have an assigned_to edge.
	hasAssignedTo := false
	for _, e := range result.Edges {
		if e.Type == "assigned_to" && e.Source == "agent:agent-1" && e.Target == "gd-a1" {
			hasAssignedTo = true
		}
	}
	if !hasAssignedTo {
		t.Fatal("expected assigned_to edge from agent:agent-1 to gd-a1")
	}
}

func TestHandleGraph_ParentChildInference(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-epic1"] = &model.Bead{ID: "gd-epic1", Title: "Epic", Status: model.StatusOpen, Type: model.TypeEpic}
	ms.beads["gd-task1"] = &model.Bead{ID: "gd-task1", Title: "Task under epic", Status: model.StatusOpen, Type: model.TypeTask}
	ms.deps["gd-task1"] = []*model.Dependency{
		{BeadID: "gd-task1", DependsOnID: "gd-epic1", Type: model.DepBlocks},
	}

	rec := doJSON(t, h, "GET", "/v1/graph", nil)
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)

	// The blocks edge should be promoted to parent-child since target is an epic.
	for _, e := range result.Edges {
		if e.Source == "gd-task1" && e.Target == "gd-epic1" {
			if e.Type != "parent-child" {
				t.Fatalf("expected parent-child edge, got %q", e.Type)
			}
			return
		}
	}
	t.Fatal("expected edge from gd-task1 to gd-epic1")
}

func TestHandleGraph_ClosedPruning(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-open1"] = &model.Bead{ID: "gd-open1", Title: "Open", Status: model.StatusOpen, Type: model.TypeTask}
	ms.beads["gd-closed1"] = &model.Bead{ID: "gd-closed1", Title: "Closed connected", Status: model.StatusClosed, Type: model.TypeTask}
	ms.beads["gd-closed2"] = &model.Bead{ID: "gd-closed2", Title: "Closed disconnected", Status: model.StatusClosed, Type: model.TypeTask}
	ms.deps["gd-open1"] = []*model.Dependency{
		{BeadID: "gd-open1", DependsOnID: "gd-closed1", Type: model.DepBlocks},
	}

	// Include closed in status filter so they show up.
	rec := doJSON(t, h, "POST", "/v1/graph", map[string]any{
		"status": []string{"open", "in_progress", "blocked", "deferred", "closed"},
	})
	requireStatus(t, rec, 200)
	var result model.GraphResponse
	decodeJSON(t, rec, &result)

	// gd-closed1 should be kept (connected to open), gd-closed2 should be pruned.
	nodeIDs := make(map[string]bool)
	for _, n := range result.Nodes {
		nodeIDs[n.ID] = true
	}
	if !nodeIDs["gd-open1"] {
		t.Fatal("expected gd-open1 in nodes")
	}
	if !nodeIDs["gd-closed1"] {
		t.Fatal("expected gd-closed1 in nodes (connected to open)")
	}
	if nodeIDs["gd-closed2"] {
		t.Fatal("expected gd-closed2 to be pruned (disconnected closed)")
	}
}

func TestHandleGetStats_Empty(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/stats", nil)
	requireStatus(t, rec, 200)

	var stats model.GraphStats
	decodeJSON(t, rec, &stats)
	if stats.TotalOpen != 0 || stats.TotalInProgress != 0 || stats.TotalBlocked != 0 {
		t.Fatalf("expected all zeros, got %+v", stats)
	}
}

func TestHandleGetStats_WithData(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-s1"] = &model.Bead{ID: "gd-s1", Status: model.StatusOpen}
	ms.beads["gd-s2"] = &model.Bead{ID: "gd-s2", Status: model.StatusOpen}
	ms.beads["gd-s3"] = &model.Bead{ID: "gd-s3", Status: model.StatusInProgress}
	ms.beads["gd-s4"] = &model.Bead{ID: "gd-s4", Status: model.StatusBlocked}
	ms.beads["gd-s5"] = &model.Bead{ID: "gd-s5", Status: model.StatusClosed}

	rec := doJSON(t, h, "GET", "/v1/stats", nil)
	requireStatus(t, rec, 200)

	var stats model.GraphStats
	decodeJSON(t, rec, &stats)
	if stats.TotalOpen != 2 {
		t.Fatalf("expected total_open=2, got %d", stats.TotalOpen)
	}
	if stats.TotalInProgress != 1 {
		t.Fatalf("expected total_in_progress=1, got %d", stats.TotalInProgress)
	}
	if stats.TotalBlocked != 1 {
		t.Fatalf("expected total_blocked=1, got %d", stats.TotalBlocked)
	}
	if stats.TotalClosed != 1 {
		t.Fatalf("expected total_closed=1, got %d", stats.TotalClosed)
	}
}

func TestHandleGetReady_Empty(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/ready", nil)
	requireStatus(t, rec, 200)

	var result struct {
		Beads []*model.Bead `json:"beads"`
		Total int           `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Beads) != 0 {
		t.Fatalf("expected 0 beads, got %d", len(result.Beads))
	}
}

func TestHandleGetReady_FiltersBlockedBeads(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now()
	ms.beads["gd-r1"] = &model.Bead{ID: "gd-r1", Title: "Ready", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now}
	ms.beads["gd-r2"] = &model.Bead{ID: "gd-r2", Title: "Blocked by r3", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now}
	ms.beads["gd-r3"] = &model.Bead{ID: "gd-r3", Title: "Blocker (open)", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now}
	// r2 depends on r3 (r3 blocks r2)
	ms.deps["gd-r2"] = []*model.Dependency{
		{BeadID: "gd-r2", DependsOnID: "gd-r3", Type: model.DepBlocks},
	}

	rec := doJSON(t, h, "GET", "/v1/ready", nil)
	requireStatus(t, rec, 200)

	var result struct {
		Beads []*model.Bead `json:"beads"`
		Total int           `json:"total"`
	}
	decodeJSON(t, rec, &result)
	// r2 should be filtered out (blocked by open r3), r1 and r3 should remain
	if result.Total != 2 {
		t.Fatalf("expected 2 ready beads, got %d", result.Total)
	}
	ids := make(map[string]bool)
	for _, b := range result.Beads {
		ids[b.ID] = true
	}
	if !ids["gd-r1"] || !ids["gd-r3"] {
		t.Fatalf("expected gd-r1 and gd-r3, got %v", ids)
	}
	if ids["gd-r2"] {
		t.Fatal("gd-r2 should be filtered out (blocked)")
	}
}

func TestHandleGetDecision(t *testing.T) {
	_, ms, h := newTestServer()

	// Add a decision bead with fields.
	fields, _ := json.Marshal(map[string]any{
		"prompt":       "Deploy to prod?",
		"options":      json.RawMessage(`[{"id":"y","label":"Yes"},{"id":"n","label":"No"}]`),
		"context":      "Release v1.0 is ready",
		"requested_by": "alice",
	})
	ms.beads["gd-d1"] = &model.Bead{
		ID:     "gd-d1",
		Kind:   model.KindData,
		Type:   model.TypeDecision,
		Title:  "Deploy to prod?",
		Status: model.StatusOpen,
		Fields: fields,
	}

	rec := doJSON(t, h, "GET", "/v1/decisions/gd-d1", nil)
	requireStatus(t, rec, 200)

	var result struct {
		Decision map[string]any `json:"decision"`
		Issue    *model.Bead    `json:"issue"`
	}
	decodeJSON(t, rec, &result)

	if result.Decision["prompt"] != "Deploy to prod?" {
		t.Fatalf("expected prompt 'Deploy to prod?', got %v", result.Decision["prompt"])
	}
	if result.Decision["requested_by"] != "alice" {
		t.Fatalf("expected requested_by 'alice', got %v", result.Decision["requested_by"])
	}
	if result.Issue.ID != "gd-d1" {
		t.Fatalf("expected issue id gd-d1, got %s", result.Issue.ID)
	}
}

func TestHandleGetDecision_NotFound(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, "GET", "/v1/decisions/gd-nope", nil)
	requireStatus(t, rec, 404)
}

func TestHandleListDecisions(t *testing.T) {
	_, ms, h := newTestServer()

	fields1, _ := json.Marshal(map[string]any{"prompt": "Q1"})
	fields2, _ := json.Marshal(map[string]any{"prompt": "Q2"})
	ms.beads["gd-d1"] = &model.Bead{
		ID: "gd-d1", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Q1", Status: model.StatusOpen, Fields: fields1,
	}
	ms.beads["gd-d2"] = &model.Bead{
		ID: "gd-d2", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Q2", Status: model.StatusOpen, Fields: fields2,
	}
	// Non-decision bead — should not appear.
	ms.beads["gd-t1"] = &model.Bead{
		ID: "gd-t1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Task", Status: model.StatusOpen,
	}

	rec := doJSON(t, h, "GET", "/v1/decisions", nil)
	requireStatus(t, rec, 200)

	var result struct {
		Decisions []map[string]any `json:"decisions"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(result.Decisions))
	}
}

func TestHandleListDecisions_Empty(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, "GET", "/v1/decisions", nil)
	requireStatus(t, rec, 200)

	var result struct {
		Decisions []map[string]any `json:"decisions"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Decisions) != 0 {
		t.Fatalf("expected 0 decisions, got %d", len(result.Decisions))
	}
}

func TestHandleResolveDecision(t *testing.T) {
	_, ms, h := newTestServer()

	fields, _ := json.Marshal(map[string]any{
		"prompt":  "Deploy?",
		"options": json.RawMessage(`[{"id":"y","label":"Yes"},{"id":"n","label":"No"}]`),
	})
	ms.beads["gd-d1"] = &model.Bead{
		ID: "gd-d1", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Deploy?", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/decisions/gd-d1/resolve", map[string]any{
		"selected_option": "y",
		"responded_by":    "bob",
	})
	requireStatus(t, rec, 200)

	var result struct {
		Decision map[string]any `json:"decision"`
		Issue    *model.Bead    `json:"issue"`
	}
	decodeJSON(t, rec, &result)

	if result.Issue.Status != model.StatusClosed {
		t.Fatalf("expected closed status, got %s", result.Issue.Status)
	}
	if result.Decision["selected_option"] != "y" {
		t.Fatalf("expected selected_option 'y', got %v", result.Decision["selected_option"])
	}
}

func TestHandleResolveDecision_RequiresInput(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-d1"] = &model.Bead{
		ID: "gd-d1", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Q?", Status: model.StatusOpen,
	}

	rec := doJSON(t, h, "POST", "/v1/decisions/gd-d1/resolve", map[string]any{})
	requireStatus(t, rec, 400)
}

func TestHandleCancelDecision(t *testing.T) {
	_, ms, h := newTestServer()

	fields, _ := json.Marshal(map[string]any{"prompt": "Deploy?"})
	ms.beads["gd-d1"] = &model.Bead{
		ID: "gd-d1", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Deploy?", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/decisions/gd-d1/cancel", map[string]any{
		"reason":      "no longer needed",
		"canceled_by": "carol",
	})
	requireStatus(t, rec, 200)

	var result struct {
		Decision map[string]any `json:"decision"`
		Issue    *model.Bead    `json:"issue"`
	}
	decodeJSON(t, rec, &result)

	if result.Issue.Status != model.StatusClosed {
		t.Fatalf("expected closed status, got %s", result.Issue.Status)
	}
}

func TestHandleCreateBead_DueAtOnNonIssue(t *testing.T) {
	_, ms, h := newTestServer()
	ms.configs["type:note"] = &model.Config{Key: "type:note", Value: json.RawMessage(`{"kind":"data"}`)}

	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "x", "type": "note", "due_at": "2026-03-01T00:00:00Z",
	})
	requireStatus(t, rec, 201)
}

// --- Agent Roster ---

func TestHandleAgentRoster_WithPresenceAndTasks(t *testing.T) {
	srv, ms, h := newTestServer()

	// Seed presence data.
	srv.Presence.RecordHookEvent(presence.HookEvent{
		Actor:     "wise-newt",
		HookType:  "PostToolUse",
		ToolName:  "Bash",
		SessionID: "sess-1",
		CWD:       "/home/agent/beads",
	})
	srv.Presence.RecordHookEvent(presence.HookEvent{
		Actor:     "ripe-elk",
		HookType:  "SessionStart",
		SessionID: "sess-2",
	})

	// Seed an in_progress bead assigned to wise-newt.
	ms.beads["bd-task1"] = &model.Bead{
		ID:       "bd-task1",
		Title:    "Fix login bug",
		Status:   model.StatusInProgress,
		Assignee: "wise-newt",
	}

	// Seed an unclaimed in_progress bead (no assignee).
	ms.beads["bd-orphan"] = &model.Bead{
		ID:       "bd-orphan",
		Title:    "Orphan task",
		Status:   model.StatusInProgress,
		Priority: 1,
	}

	rec := doJSON(t, h, "GET", "/v1/agents/roster?stale_threshold_secs=600", nil)
	requireStatus(t, rec, 200)

	var resp struct {
		Actors []struct {
			Actor     string  `json:"actor"`
			TaskID    string  `json:"task_id"`
			TaskTitle string  `json:"task_title"`
			IdleSecs  float64 `json:"idle_secs"`
			LastEvent string  `json:"last_event"`
			ToolName  string  `json:"tool_name"`
			SessionID string  `json:"session_id"`
			CWD       string  `json:"cwd"`
		} `json:"actors"`
		UnclaimedTasks []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Priority int    `json:"priority"`
		} `json:"unclaimed_tasks"`
	}
	decodeJSON(t, rec, &resp)

	if len(resp.Actors) != 2 {
		t.Fatalf("len(actors) = %d, want 2", len(resp.Actors))
	}

	// Find wise-newt in the roster (order depends on LastSeen, both should be present).
	var found bool
	for _, a := range resp.Actors {
		if a.Actor == "wise-newt" {
			found = true
			if a.TaskID != "bd-task1" {
				t.Errorf("wise-newt.task_id = %q, want bd-task1", a.TaskID)
			}
			if a.TaskTitle != "Fix login bug" {
				t.Errorf("wise-newt.task_title = %q, want Fix login bug", a.TaskTitle)
			}
			if a.LastEvent != "PostToolUse" {
				t.Errorf("wise-newt.last_event = %q, want PostToolUse", a.LastEvent)
			}
			if a.ToolName != "Bash" {
				t.Errorf("wise-newt.tool_name = %q, want Bash", a.ToolName)
			}
			if a.SessionID != "sess-1" {
				t.Errorf("wise-newt.session_id = %q, want sess-1", a.SessionID)
			}
			if a.CWD != "/home/agent/beads" {
				t.Errorf("wise-newt.cwd = %q, want /home/agent/beads", a.CWD)
			}
		}
	}
	if !found {
		t.Error("wise-newt not found in roster")
	}

	// Verify unclaimed tasks.
	if len(resp.UnclaimedTasks) != 1 {
		t.Fatalf("len(unclaimed_tasks) = %d, want 1", len(resp.UnclaimedTasks))
	}
	if resp.UnclaimedTasks[0].ID != "bd-orphan" {
		t.Errorf("unclaimed[0].id = %q, want bd-orphan", resp.UnclaimedTasks[0].ID)
	}
	if resp.UnclaimedTasks[0].Priority != 1 {
		t.Errorf("unclaimed[0].priority = %d, want 1", resp.UnclaimedTasks[0].Priority)
	}
}

func TestHandleAgentRoster_NilPresence(t *testing.T) {
	// When Presence is nil, should return empty arrays (not error).
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.Presence = nil
	h := s.NewHTTPHandler(nil)

	rec := doJSON(t, h, "GET", "/v1/agents/roster", nil)
	requireStatus(t, rec, 200)

	var resp struct {
		Actors         []any `json:"actors"`
		UnclaimedTasks []any `json:"unclaimed_tasks"`
	}
	decodeJSON(t, rec, &resp)

	if len(resp.Actors) != 0 {
		t.Errorf("actors should be empty, got %d", len(resp.Actors))
	}
	if len(resp.UnclaimedTasks) != 0 {
		t.Errorf("unclaimed_tasks should be empty, got %d", len(resp.UnclaimedTasks))
	}
}

func TestHandleAgentRoster_EmptyPresence(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, "GET", "/v1/agents/roster", nil)
	requireStatus(t, rec, 200)

	var resp struct {
		Actors         []any `json:"actors"`
		UnclaimedTasks []any `json:"unclaimed_tasks"`
	}
	decodeJSON(t, rec, &resp)

	if len(resp.Actors) != 0 {
		t.Errorf("actors should be empty, got %d", len(resp.Actors))
	}
}

func TestHandleAgentRoster_NoAssignedTask(t *testing.T) {
	srv, _, h := newTestServer()

	// Agent in presence but no matching in_progress bead.
	srv.Presence.RecordHookEvent(presence.HookEvent{
		Actor:    "idle-agent",
		HookType: "SessionStart",
	})

	rec := doJSON(t, h, "GET", "/v1/agents/roster?stale_threshold_secs=600", nil)
	requireStatus(t, rec, 200)

	var resp struct {
		Actors []struct {
			Actor  string `json:"actor"`
			TaskID string `json:"task_id"`
		} `json:"actors"`
	}
	decodeJSON(t, rec, &resp)

	if len(resp.Actors) != 1 {
		t.Fatalf("len(actors) = %d, want 1", len(resp.Actors))
	}
	if resp.Actors[0].TaskID != "" {
		t.Errorf("task_id should be empty for agent with no task, got %q", resp.Actors[0].TaskID)
	}
}
