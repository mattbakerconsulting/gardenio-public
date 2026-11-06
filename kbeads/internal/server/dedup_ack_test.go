package server

// Acceptance tests for the decision/escalation dedup-ack gate (T3 of epic
// ka-0000004599). The gate rejects a decision/escalation create when open
// beads of the same type already exist in the project, unless the caller
// echoes the current open-list hash. Design-of-record:
// kbeads/docs/research-dedup-ack-gate.md.
//
// Scenarios (design §14):
//   - dedup-happy    — correct hash → create proceeds
//   - empty-list     — no ack required when 0 open
//   - race           — new decision appears mid-loop → hash mismatch → re-list
//   - type-filter    — task creates never require ack
//   - cross-type     — a decision is not gated on open escalations
//   - force-bypass   — Force=true skips the gate
//   - replay-bypass  — escalation replay context skips the gate
//   - no-project     — a project-less bead skips the gate
//   - flag-off       — gate disabled → no rejection regardless of open list
//   - list-cap       — >100 open → body capped, hash over the full set
//   - allowlist      — BEADS_DEDUP_GATE_PROJECTS scopes the gate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// enableDedupGate turns the gate on for the duration of a test.
func enableDedupGate(t *testing.T) {
	t.Helper()
	t.Setenv("BEADS_DEDUP_GATE_ENABLED", "1")
}

// gateServer builds a server + mock store for the gate tests.
func gateServer() (*BeadsServer, *mockStore) {
	return newAutoCloseServer()
}

// asDedupAckError type-asserts err to *dedupAckError, failing the test if it
// is nil or a different error.
func asDedupAckError(t *testing.T, err error) *dedupAckError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a dedup-ack rejection, got nil")
	}
	de, ok := err.(*dedupAckError)
	if !ok {
		t.Fatalf("expected *dedupAckError, got %T: %v", err, err)
	}
	return de
}

func newDecision(id, project string) *model.Bead {
	return &model.Bead{
		ID:     id,
		Kind:   model.KindData,
		Type:   model.TypeDecision,
		Title:  id,
		Status: model.StatusOpen,
		Labels: []string{"project:" + project},
	}
}

// TestDedupGate_EmptyList: no open beads → no ack required.
func TestDedupGate_EmptyList(t *testing.T) {
	enableDedupGate(t)
	s, _ := gateServer()
	err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision)
	if err != nil {
		t.Fatalf("empty open list must not reject, got: %v", err)
	}
}

// TestDedupGate_RejectsWithOpen: one open decision in the project, no ack →
// reject with the open list and a hash.
func TestDedupGate_RejectsWithOpen(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision)
	de := asDedupAckError(t, err)
	if de.Code() != dedupAckErrorCode {
		t.Errorf("code = %q, want %q", de.Code(), dedupAckErrorCode)
	}
	if de.TotalOpen != 1 || len(de.Open) != 1 {
		t.Errorf("expected 1 open entry, got total=%d len=%d", de.TotalOpen, len(de.Open))
	}
	if de.Open[0].ID != "gd-open1" {
		t.Errorf("open[0].id = %q, want gd-open1", de.Open[0].ID)
	}
	if de.Hash == "" {
		t.Error("expected a non-empty hash")
	}
	if de.RetryWith != "--saw-open-decisions="+de.Hash {
		t.Errorf("retry_with = %q, want --saw-open-decisions=<hash>", de.RetryWith)
	}
	if de.Project != "gardenio" {
		t.Errorf("project = %q, want gardenio", de.Project)
	}
}

// TestDedupGate_HappyPath: echoing the current hash lets the create proceed.
func TestDedupGate_HappyPath(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	// First attempt → reject, learn the hash.
	err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision)
	de := asDedupAckError(t, err)

	// Retry with the acknowledged hash → proceeds.
	if err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), de.Hash, model.TypeDecision); err != nil {
		t.Fatalf("correct hash must be accepted, got: %v", err)
	}
}

// TestDedupGate_Race: a new decision appearing after the hash was learned
// changes the snapshot, so the stale hash re-rejects with the fresh list.
func TestDedupGate_Race(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision)
	stale := asDedupAckError(t, err).Hash

	// A second decision appears mid-loop.
	seedBead(ms, "gd-open2", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	err2 := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), stale, model.TypeDecision)
	de2 := asDedupAckError(t, err2)
	if de2.Hash == stale {
		t.Error("hash must change when the open list changes")
	}
	if de2.TotalOpen != 2 {
		t.Errorf("expected fresh list of 2, got %d", de2.TotalOpen)
	}
	// The fresh hash is then acceptable.
	if err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), de2.Hash, model.TypeDecision); err != nil {
		t.Fatalf("fresh hash must be accepted, got: %v", err)
	}
}

// TestDedupGate_CrossType: a decision create is gated only on open decisions,
// never on open escalations (§6, independent queues).
func TestDedupGate_CrossType(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	// Only an open ESCALATION exists in the project.
	seedBead(ms, "gd-esc1", model.TypeEscalation, model.StatusOpen, []string{"project:gardenio"}, nil)

	// Creating a DECISION must not be gated on the escalation.
	if err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision); err != nil {
		t.Fatalf("decision must not be gated on an open escalation, got: %v", err)
	}
	// And creating an ESCALATION is gated on the open escalation.
	esc := &model.Bead{ID: "gd-newesc", Kind: model.KindData, Type: model.TypeEscalation, Title: "gd-newesc", Status: model.StatusOpen, Labels: []string{"project:gardenio"}}
	if err := s.applyDedupAckGate(context.Background(), esc, "", model.TypeEscalation); err == nil {
		t.Fatal("escalation must be gated on the open escalation")
	}
}

// TestDedupGate_ProjectScope: an open decision in ANOTHER project does not
// gate a create in this project.
func TestDedupGate_ProjectScope(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-other", model.TypeDecision, model.StatusOpen, []string{"project:otherproj"}, nil)

	if err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision); err != nil {
		t.Fatalf("a decision in another project must not gate this create, got: %v", err)
	}
}

// TestDedupGate_NoProject: a project-less bead skips the gate (§7).
func TestDedupGate_NoProject(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	noProj := &model.Bead{ID: "gd-new", Kind: model.KindData, Type: model.TypeDecision, Title: "gd-new", Status: model.StatusOpen}
	if err := s.applyDedupAckGate(context.Background(), noProj, "", model.TypeDecision); err != nil {
		t.Fatalf("a project-less bead must skip the gate, got: %v", err)
	}
}

// TestDedupGate_FlagOff: with the gate disabled, no rejection regardless of
// how many open decisions exist.
func TestDedupGate_FlagOff(t *testing.T) {
	// Note: BEADS_DEDUP_GATE_ENABLED intentionally NOT set.
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)
	seedBead(ms, "gd-open2", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	if err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision); err != nil {
		t.Fatalf("gate off must never reject, got: %v", err)
	}
}

// TestDedupGate_Allowlist: BEADS_DEDUP_GATE_PROJECTS scopes the gate to the
// listed projects even when globally enabled.
func TestDedupGate_Allowlist(t *testing.T) {
	enableDedupGate(t)
	t.Setenv("BEADS_DEDUP_GATE_PROJECTS", "otherproj")
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	// gardenio is not on the allowlist → skip.
	if err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision); err != nil {
		t.Fatalf("project not on allowlist must skip the gate, got: %v", err)
	}
}

// TestDedupGate_ForceBypass: runDedupAckGate with Force=true skips the gate.
func TestDedupGate_ForceBypass(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	in := createBeadInput{Force: true}
	if err := s.runDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), in, model.TypeDecision); err != nil {
		t.Fatalf("Force=true must bypass the gate, got: %v", err)
	}
}

// TestDedupGate_ReplayBypass: an escalation-replay context skips the gate (§9).
func TestDedupGate_ReplayBypass(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-esc1", model.TypeEscalation, model.StatusOpen, []string{"project:gardenio"}, nil)

	esc := &model.Bead{ID: "gd-newesc", Kind: model.KindData, Type: model.TypeEscalation, Title: "gd-newesc", Status: model.StatusOpen, Labels: []string{"project:gardenio"}}
	ctx := withDedupAckReplayBypass(context.Background())
	if err := s.runDedupAckGate(ctx, esc, createBeadInput{}, model.TypeEscalation); err != nil {
		t.Fatalf("escalation replay context must bypass the gate, got: %v", err)
	}
}

// TestDedupGate_InputFieldAck: the dedicated SawOpenHash input field is the
// acknowledgement channel (it is exempt from the strict type-schema
// unknown-field validation that a fields.saw_open_* entry would trip).
func TestDedupGate_InputFieldAck(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)

	// Learn the hash.
	err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision)
	hash := asDedupAckError(t, err).Hash

	// Retry via the input field on createBeadInput.
	in := createBeadInput{SawOpenHash: hash}
	if err := s.runDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), in, model.TypeDecision); err != nil {
		t.Fatalf("hash in SawOpenHash must be accepted, got: %v", err)
	}
}

// TestDedupGate_ListCap: with more than the display cap open, the body is
// capped and marked truncated, but the hash covers the FULL set.
func TestDedupGate_ListCap(t *testing.T) {
	enableDedupGate(t)
	s, ms := gateServer()
	total := dedupAckListCap + 25
	for i := 0; i < total; i++ {
		id := "gd-open" + pad(i)
		seedBead(ms, id, model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, nil)
	}

	err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), "", model.TypeDecision)
	de := asDedupAckError(t, err)
	if !de.Truncated {
		t.Error("expected truncated=true when open count exceeds the cap")
	}
	if len(de.Open) != dedupAckListCap {
		t.Errorf("display list = %d, want %d", len(de.Open), dedupAckListCap)
	}
	if de.TotalOpen != total {
		t.Errorf("total_open = %d, want %d", de.TotalOpen, total)
	}
	// The hash must be over the full set: echoing it is accepted.
	if err := s.applyDedupAckGate(context.Background(), newDecision("gd-new", "gardenio"), de.Hash, model.TypeDecision); err != nil {
		t.Fatalf("hash over the full set must be accepted, got: %v", err)
	}
}

// TestComputeOpenListHash_OrderIndependent: the hash is deterministic
// regardless of the store's return order (sorted by id first).
func TestComputeOpenListHash_OrderIndependent(t *testing.T) {
	a := &model.Bead{ID: "gd-aaa", Title: "first"}
	b := &model.Bead{ID: "gd-bbb", Title: "second"}
	h1 := computeOpenListHash([]*model.Bead{a, b})
	h2 := computeOpenListHash([]*model.Bead{b, a})
	if h1 != h2 {
		t.Errorf("hash must be order-independent: %q vs %q", h1, h2)
	}
	// A title change flips the hash (§2: title stability is the signal).
	b2 := &model.Bead{ID: "gd-bbb", Title: "second (edited)"}
	if computeOpenListHash([]*model.Bead{a, b2}) == h1 {
		t.Error("a title change must change the hash")
	}
}

// TestDedupGate_EndToEnd_GRPCReject drives the full CreateBead gRPC path with
// the gate enabled: a decision create with an open decision already in the
// project is rejected with FailedPrecondition carrying the dedup_ack_required
// payload. Confirms the createBead switch wiring maps the gate error onto the
// gRPC status, not just the gate function in isolation.
func TestDedupGate_EndToEnd_GRPCReject(t *testing.T) {
	enableDedupGate(t)
	srv, ms, ctx := testCtx(t)

	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, map[string]any{"prompt": "existing"})

	validOptions, _ := json.Marshal([]map[string]string{
		{"id": "a", "short": "A", "label": "do A"},
		{"id": "b", "short": "B", "label": "do B"},
	})
	fields, _ := json.Marshal(map[string]any{"prompt": "new decision", "options": json.RawMessage(validOptions)})

	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "new decision", Type: "decision", Kind: "data",
		Labels: []string{"project:gardenio"}, Fields: fields,
	})
	if err == nil {
		t.Fatal("expected the dedup gate to reject the create")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", st.Code())
	}
	if !strings.Contains(st.Message(), dedupAckErrorCode) {
		t.Errorf("message should embed %q, got: %s", dedupAckErrorCode, st.Message())
	}
}

// TestDedupGate_EndToEnd_AckProceeds drives s.createBead with the dedicated
// SawOpenHash input field (the primary retry mechanism — it is NOT part of the
// type schema, so it survives ValidateTypeFields where a fields.saw_open_*
// value would be rejected as an unknown field). With the current hash acked,
// the create proceeds AND the ack-only decision guard still runs independently.
func TestDedupGate_EndToEnd_AckProceeds(t *testing.T) {
	enableDedupGate(t)
	srv, ms, ctx := testCtx(t)

	seedBead(ms, "gd-open1", model.TypeDecision, model.StatusOpen, []string{"project:gardenio"}, map[string]any{"prompt": "existing"})
	hash := computeOpenListHash([]*model.Bead{ms.beads["gd-open1"]})

	validOptions, _ := json.Marshal([]map[string]string{
		{"id": "a", "short": "A", "label": "do A"},
		{"id": "b", "short": "B", "label": "do B"},
	})
	fields, _ := json.Marshal(map[string]any{"prompt": "new decision", "options": json.RawMessage(validOptions)})

	bead, err := srv.createBead(ctx, createBeadInput{
		Title: "new decision", Type: "decision", Kind: "data",
		Labels: []string{"project:gardenio"}, Fields: fields, SawOpenHash: hash,
	})
	if err != nil {
		t.Fatalf("create with acked hash should succeed, got: %v", err)
	}
	if bead.ID == "" {
		t.Fatal("expected a created bead id")
	}

	// The dedup gate does not shadow the ack-only decision guard: a zero-option
	// decision that passes the dedup ack still fails the content guard. The
	// first create added a second open decision, so recompute the fresh ack.
	freshHash := computeOpenListHash([]*model.Bead{ms.beads["gd-open1"], ms.beads[bead.ID]})
	_, err = srv.createBead(ctx, createBeadInput{
		Title: "noop decision", Type: "decision", Kind: "data",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"prompt":"noop"}`), SawOpenHash: freshHash,
	})
	if err == nil {
		t.Fatal("expected the ack-only decision guard to reject a zero-option decision even with a valid dedup ack")
	}
	if !strings.Contains(err.Error(), "no options") {
		t.Errorf("expected decision-guard 'no options' rejection, got: %v", err)
	}
}

// pad renders i as a fixed-width 4-digit string so seeded ids sort stably.
func pad(i int) string {
	const digits = "0123456789"
	out := []byte{'0', '0', '0', '0'}
	for p := 3; p >= 0 && i > 0; p-- {
		out[p] = digits[i%10]
		i /= 10
	}
	return string(out)
}
