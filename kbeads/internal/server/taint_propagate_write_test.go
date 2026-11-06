package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// staticAgentCtx builds a context carrying a static AuthIdentity for
// agentBeadID plus an operated-by header value, satisfying both the taint
// context resolver (which requires a static-type identity) and the
// post-R2 fail-closed operator resolver (resolveAuditOperatorRequired),
// which every write path under test here consults.
func staticAgentCtx(agentBeadID string) context.Context {
	ctx := setBoth(context.Background(), "slack:U_TEST", "")
	return withTestPrivilegedAgent(ctx, agentBeadID)
}

// staticAgentReq builds an HTTP request carrying a static agent AuthIdentity
// plus an operated-by header, for handlers exercised through bareMux (which
// skips AuthMiddleware so tests can inject identity directly).
func staticAgentReq(method, path string, body any, agentBeadID string) *http.Request {
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	ctx := WithOperatedByHeader(req.Context(), "slack:U_TEST")
	ctx = withTestPrivilegedAgent(ctx, agentBeadID)
	return req.WithContext(ctx)
}

// TestUpdateBead_InheritsWriterTaint pins ADR §4.2 row 4: a tainted agent
// updating an existing (untainted) bead causes the bead to acquire the
// agent's taints, source=inherit:write.
func TestUpdateBead_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	ms.taints = map[string][]int64{"gd-agent1": {3}}
	title := "updated by tainted agent"
	if _, err := s.updateBead(staticAgentCtx("gd-agent1"), bead.ID, updateBeadInput{Title: &title}); err != nil {
		t.Fatalf("updateBead error: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 3)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 3, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
	if row.SourceBeadID != "gd-agent1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-agent1")
	}
}

// TestUpdateBead_UntaintedWriterAddsNoTaintRows pins the negative half of
// row 4: an untainted agent writing to a bead must add zero taint rows.
func TestUpdateBead_UntaintedWriterAddsNoTaintRows(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	title := "updated by untainted agent"
	if _, err := s.updateBead(staticAgentCtx("gd-agent-clean"), bead.ID, updateBeadInput{Title: &title}); err != nil {
		t.Fatalf("updateBead error: %v", err)
	}

	if rows := ms.beadTaints[bead.ID]; len(rows) != 0 {
		t.Fatalf("expected no bead_taints rows, got %v", rows)
	}
}

// TestHandleAddComment_InheritsWriterTaint covers the comment-add call site.
func TestHandleAddComment_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	h := bareMux(s)
	ms.beads["gd-cmt"] = &model.Bead{ID: "gd-cmt", Title: "x", Type: model.TypeTask, Status: model.StatusOpen}
	ms.taints = map[string][]int64{"gd-agent1": {4}}

	req := staticAgentReq("POST", "/v1/beads/gd-cmt/comments",
		map[string]any{"text": "hello"}, "gd-agent1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	rows := ms.beadTaints["gd-cmt"]
	row, ok := beadTaintByID(rows, 4)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 4, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
	if row.SourceBeadID != "gd-agent1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-agent1")
	}
}

// TestHandleAddLabel_InheritsWriterTaint covers the label-add call site.
func TestHandleAddLabel_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	h := bareMux(s)
	ms.beads["gd-lbl"] = &model.Bead{ID: "gd-lbl", Title: "x", Type: model.TypeTask, Status: model.StatusOpen}
	ms.taints = map[string][]int64{"gd-agent1": {5}}

	req := staticAgentReq("POST", "/v1/beads/gd-lbl/labels",
		map[string]any{"label": "some-label"}, "gd-agent1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	rows := ms.beadTaints["gd-lbl"]
	row, ok := beadTaintByID(rows, 5)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 5, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
}

// TestHandleRemoveLabel_InheritsWriterTaint covers the label-remove call site.
func TestHandleRemoveLabel_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	h := bareMux(s)
	ms.beads["gd-rml"] = &model.Bead{ID: "gd-rml", Title: "x", Type: model.TypeTask, Status: model.StatusOpen}
	ms.labels["gd-rml"] = []string{"doomed-label"}
	ms.taints = map[string][]int64{"gd-agent1": {6}}

	req := staticAgentReq("DELETE", "/v1/beads/gd-rml/labels/doomed-label", nil, "gd-agent1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	rows := ms.beadTaints["gd-rml"]
	row, ok := beadTaintByID(rows, 6)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 6, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
}

// TestAddLabel_InheritsWriterTaint covers the gRPC label-add call site
// (server.go AddLabel), which duplicates the HTTP handleAddLabel
// implementation rather than sharing it.
func TestAddLabel_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["gd-lbl-grpc"] = &model.Bead{ID: "gd-lbl-grpc", Title: "x", Type: model.TypeTask, Status: model.StatusOpen}
	ms.taints = map[string][]int64{"gd-agent1": {12}}

	if _, err := s.AddLabel(staticAgentCtx("gd-agent1"), &beadsv1.AddLabelRequest{
		BeadId: "gd-lbl-grpc", Label: "some-label",
	}); err != nil {
		t.Fatalf("AddLabel error: %v", err)
	}

	rows := ms.beadTaints["gd-lbl-grpc"]
	row, ok := beadTaintByID(rows, 12)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 12, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
	if row.SourceBeadID != "gd-agent1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-agent1")
	}
}

// TestRemoveLabel_InheritsWriterTaint covers the gRPC label-remove call site
// (server.go RemoveLabel).
func TestRemoveLabel_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["gd-rml-grpc"] = &model.Bead{ID: "gd-rml-grpc", Title: "x", Type: model.TypeTask, Status: model.StatusOpen}
	ms.labels["gd-rml-grpc"] = []string{"doomed-label"}
	ms.taints = map[string][]int64{"gd-agent1": {13}}

	resp, err := s.RemoveLabel(staticAgentCtx("gd-agent1"), &beadsv1.RemoveLabelRequest{
		BeadId: "gd-rml-grpc", Label: "doomed-label",
	})
	if err != nil {
		t.Fatalf("RemoveLabel error: %v", err)
	}
	if resp.GetRemoved() != true {
		t.Fatalf("expected removed=true, got %v", resp.GetRemoved())
	}

	rows := ms.beadTaints["gd-rml-grpc"]
	row, ok := beadTaintByID(rows, 13)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 13, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
}

// TestGRPCAddComment_InheritsWriterTaint covers the gRPC comment-add call
// site (server.go AddComment), which duplicates the HTTP handleAddComment
// implementation rather than sharing it.
func TestGRPCAddComment_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["gd-cmt-grpc"] = &model.Bead{ID: "gd-cmt-grpc", Title: "x", Type: model.TypeTask, Status: model.StatusOpen}
	ms.taints = map[string][]int64{"gd-agent1": {14}}

	if _, err := s.AddComment(staticAgentCtx("gd-agent1"), &beadsv1.AddCommentRequest{
		BeadId: "gd-cmt-grpc", Text: "hello",
	}); err != nil {
		t.Fatalf("AddComment error: %v", err)
	}

	rows := ms.beadTaints["gd-cmt-grpc"]
	row, ok := beadTaintByID(rows, 14)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 14, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
	if row.SourceBeadID != "gd-agent1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-agent1")
	}
}

// TestAddDependency_InheritsWriterTaint covers the dep-add call site, using
// a "related" edge (not triggered-by/derived-from) to isolate the writer's
// own inherit:write union from the separate target-taint inherit:create
// union that only fires for triggered-by/derived-from edges (see
// TestAddDependency_TriggeredByInheritsTargetTaint below).
func TestAddDependency_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["gd-dep-a"] = &model.Bead{ID: "gd-dep-a", Title: "a", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-dep-b"] = &model.Bead{ID: "gd-dep-b", Title: "b", Type: model.TypeTask, Status: model.StatusOpen}
	ms.taints = map[string][]int64{"gd-agent1": {7}}

	_, err := s.AddDependency(staticAgentCtx("gd-agent1"), &beadsv1.AddDependencyRequest{
		BeadId: "gd-dep-a", DependsOnId: "gd-dep-b", Type: string(model.DepRelated),
	})
	if err != nil {
		t.Fatalf("AddDependency error: %v", err)
	}

	rows := ms.beadTaints["gd-dep-a"]
	row, ok := beadTaintByID(rows, 7)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 7 on dependent bead, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
	if row.SourceBeadID != "gd-agent1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-agent1")
	}
	// The target bead must NOT be touched by the writer's own inherit:write
	// union — only the dependent bead is.
	if rows := ms.beadTaints["gd-dep-b"]; len(rows) != 0 {
		t.Fatalf("target bead gd-dep-b unexpectedly tainted: %v", rows)
	}
}

// TestAddDependency_TriggeredByInheritsTargetTaint is the ADR §4.2 row 1
// scope addition: bead A is tainted; bead B is created untainted; adding a
// "triggered-by" edge B -> A must union A's taints onto B, source
// inherit:create, source_bead_id=A.
func TestAddDependency_TriggeredByInheritsTargetTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	untaintedCtx := ctxAsTestSubject()

	beadA, err := s.createBead(untaintedCtx, createBeadInput{Title: "A", Type: "task"})
	if err != nil {
		t.Fatalf("creating A: %v", err)
	}
	ms.taints = map[string][]int64{beadA.ID: {11}}

	beadB, err := s.createBead(untaintedCtx, createBeadInput{Title: "B", Type: "task"})
	if err != nil {
		t.Fatalf("creating B: %v", err)
	}
	if rows := ms.beadTaints[beadB.ID]; len(rows) != 0 {
		t.Fatalf("B should start untainted, got %v", rows)
	}

	_, err = s.AddDependency(untaintedCtx, &beadsv1.AddDependencyRequest{
		BeadId: beadB.ID, DependsOnId: beadA.ID, Type: string(model.DepTriggeredBy),
	})
	if err != nil {
		t.Fatalf("AddDependency error: %v", err)
	}

	rows := ms.beadTaints[beadB.ID]
	row, ok := beadTaintByID(rows, 11)
	if !ok {
		t.Fatalf("expected B to inherit A's taint 11, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritCreate {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritCreate)
	}
	if row.SourceBeadID != beadA.ID {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, beadA.ID)
	}
}

// TestAddDependency_TriggeredByInheritsNonGatingTargetTaint is the §15.2
// companion to the test above, and the regression test for a real asymmetry
// this task introduced.
//
// §15.2 narrows THREE consumers to gate-effect taints: the read gate, the
// write gate, and the list predicate. Inheritance is not one of them — a
// trace-only dye ({}-effect, or any effect set without "gate") is still a
// taint, and it must still travel. The create path already gets this right:
// taint_propagate_create.go calls GetAllTaintsForBead for the identical
// TaintSourceInheritCreate union. The write path called the narrowed
// GetTaintsForBead, so the same dye propagated when a bead was born under a
// tainted parent but vanished across a triggered-by edge. Two paths, one
// semantic, two answers.
//
// This is invisible to TestAddDependency_TriggeredByInheritsTargetTaint above
// because an unregistered taint id gates by default (taintHasGateEffect
// returns true when it finds no registry row), so that test passes either
// way. Registering the taint with a non-gating effect set is what makes the
// two calls distinguishable.
func TestAddDependency_TriggeredByInheritsNonGatingTargetTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	untaintedCtx := ctxAsTestSubject()

	// A dye: carries provenance, gates nothing.
	if err := ms.SetTaint(untaintedCtx, &model.Taint{
		ID: 77, Name: "trace-only-dye", Effects: []string{},
	}); err != nil {
		t.Fatalf("registering dye taint: %v", err)
	}
	if ms.taintHasGateEffect(77) {
		t.Fatalf("fixture is wrong: taint 77 must NOT gate, or this test cannot fail")
	}

	beadA, err := s.createBead(untaintedCtx, createBeadInput{Title: "A", Type: "task"})
	if err != nil {
		t.Fatalf("creating A: %v", err)
	}
	ms.taints = map[string][]int64{beadA.ID: {77}}

	beadB, err := s.createBead(untaintedCtx, createBeadInput{Title: "B", Type: "task"})
	if err != nil {
		t.Fatalf("creating B: %v", err)
	}

	if _, err := s.AddDependency(untaintedCtx, &beadsv1.AddDependencyRequest{
		BeadId: beadB.ID, DependsOnId: beadA.ID, Type: string(model.DepTriggeredBy),
	}); err != nil {
		t.Fatalf("AddDependency error: %v", err)
	}

	rows := ms.beadTaints[beadB.ID]
	row, ok := beadTaintByID(rows, 77)
	if !ok {
		t.Fatalf("B did not inherit A's non-gating taint 77 across the triggered-by edge, got %v; "+
			"propagation must use GetAllTaintsForBead, not the gate-filtered GetTaintsForBead", rows)
	}
	if row.Source != model.TaintSourceInheritCreate {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritCreate)
	}
	if row.SourceBeadID != beadA.ID {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, beadA.ID)
	}
}

// TestCloseBead_InheritsWriterTaint covers the gRPC close call site.
func TestCloseBead_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{Title: "to close", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	ms.taints = map[string][]int64{"gd-agent1": {8}}

	if _, err := s.CloseBead(staticAgentCtx("gd-agent1"), &beadsv1.CloseBeadRequest{Id: bead.ID}); err != nil {
		t.Fatalf("CloseBead error: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 8)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 8, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
}

// TestHandleCloseBead_InheritsWriterTaint covers the independent HTTP close
// implementation, which duplicates the gRPC one rather than sharing it. Uses
// a bare mux (bypassing AuthMiddleware, like bareMux) so the injected static
// agent identity is not clobbered by defaultBearerMiddleware/OIDC resolution.
func TestHandleCloseBead_InheritsWriterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/beads/{id}/close", s.handleCloseBead)

	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{Title: "to close via http", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	ms.taints = map[string][]int64{"gd-agent1": {9}}

	req := staticAgentReq("POST", "/v1/beads/"+bead.ID+"/close", nil, "gd-agent1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 9)
	if !ok {
		t.Fatalf("expected bead_taints row for taint 9, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
}

// TestReplayEscalation_UpdateInheritsRequesterTaint pins ADR §4.2 row 7's
// positive case: requester tainted, approver untainted -> the replayed
// update's target inherits the REQUESTER's taints.
func TestReplayEscalation_UpdateInheritsRequesterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	approverCtx := ctxAsTestSubject() // untainted OIDC approver

	target, err := s.createBead(approverCtx, createBeadInput{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("creating target: %v", err)
	}
	ms.taints = map[string][]int64{"gd-requester": {21}}

	fields, _ := json.Marshal(map[string]any{
		"operation_type":   "update",
		"target_bead_id":   target.ID,
		"operation_spec":   map[string]any{"title": "replayed"},
		"requesting_agent": "gd-requester",
	})
	escalation := &model.Bead{ID: "gd-esc-1", Fields: fields}
	// ClaimEscalationReplay requires the escalation bead to exist in the
	// store (mirrors the real guarded-UPDATE's WHERE id = $1), matching how
	// replayEscalation is always invoked in production — the escalation was
	// just fetched from the store by the caller.
	ms.beads[escalation.ID] = escalation

	result := s.replayEscalation(approverCtx, escalation, "operator@example.com", nil)
	if result == nil || result["status"] != "success" {
		t.Fatalf("expected successful replay, got %v", result)
	}

	rows := ms.beadTaints[target.ID]
	row, ok := beadTaintByID(rows, 21)
	if !ok {
		t.Fatalf("expected target to inherit requester's taint 21, got %v", rows)
	}
	if row.SourceBeadID != "gd-requester" {
		t.Errorf("SourceBeadID = %q; want %q (the requester, not the approver)", row.SourceBeadID, "gd-requester")
	}
}

// TestReplayEscalation_UpdateDoesNotInheritApproverTaint pins ADR §4.2 row
// 7's negative case: requester untainted, approver tainted -> the replayed
// update must NOT pick up the approver's taints.
func TestReplayEscalation_UpdateDoesNotInheritApproverTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	untaintedCtx := ctxAsTestSubject()

	target, err := s.createBead(untaintedCtx, createBeadInput{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("creating target: %v", err)
	}
	// The approver identity itself carries a taint...
	ms.taints = map[string][]int64{"gd-approver": {22}}
	approverCtx := withTestPrivilegedAgent(untaintedCtx, "gd-approver")

	fields, _ := json.Marshal(map[string]any{
		"operation_type":   "update",
		"target_bead_id":   target.ID,
		"operation_spec":   map[string]any{"title": "replayed"},
		"requesting_agent": "gd-requester-untainted",
	})
	escalation := &model.Bead{ID: "gd-esc-2", Fields: fields}
	ms.beads[escalation.ID] = escalation

	result := s.replayEscalation(approverCtx, escalation, "gd-approver", nil)
	if result == nil || result["status"] != "success" {
		t.Fatalf("expected successful replay, got %v", result)
	}

	if rows := ms.beadTaints[target.ID]; len(rows) != 0 {
		t.Fatalf("replayed update must not inherit the approver's taint, got %v", rows)
	}
}

// TestReplayEscalation_CreateInheritsRequesterTaint covers the create-replay
// half of row 7 for parity with the update-replay tests above.
func TestReplayEscalation_CreateInheritsRequesterTaint(t *testing.T) {
	s, ms, _ := newTestServer()
	approverCtx := ctxAsTestSubject()
	ms.taints = map[string][]int64{"gd-requester": {23}}

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "create",
		"operation_spec": map[string]any{
			"title": "replayed create",
			"type":  "task",
		},
		"requesting_agent": "gd-requester",
	})
	escalation := &model.Bead{ID: "gd-esc-3", Fields: fields}
	ms.beads[escalation.ID] = escalation

	result := s.replayEscalation(approverCtx, escalation, "operator@example.com", nil)
	if result == nil || result["status"] != "success" {
		t.Fatalf("expected successful replay, got %v", result)
	}
	beadID, _ := result["bead_id"].(string)
	if beadID == "" {
		t.Fatalf("expected replay bead_id to be set, got %v", result)
	}

	rows := ms.beadTaints[beadID]
	row, ok := beadTaintByID(rows, 23)
	if !ok {
		t.Fatalf("expected replayed bead to inherit requester's taint 23, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritCreate {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritCreate)
	}
	if row.SourceBeadID != "gd-requester" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-requester")
	}
}
