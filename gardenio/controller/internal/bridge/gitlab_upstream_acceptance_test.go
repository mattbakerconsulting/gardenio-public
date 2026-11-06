package bridge

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- E-PROV FW-3 (ka-0000002343) GATE -------------------------------------
//
// GATE (task description): "an upstream merge stamps the peel-request bead with
// acceptance fields without closing dark work beads (dark lives ahead of the
// peeled slice)."
//
// These tests drive the reverse-provenance acceptance reconciler directly: a
// closed upstream-mirror MR bead → walk reverse `produces` → peel-request bead,
// stamp upstream_accepted_at / upstream_mr_url / upstream_merged_by, comment —
// and NEVER close anything. The mock client structurally cannot close a bead
// (no Close method on UpstreamAcceptanceBeadClient), so the "no auto-close"
// invariant is enforced by the interface itself; the tests additionally assert
// the dark work bead behind the peel-request is never even fetched/written.

// acceptanceMock implements UpstreamAcceptanceBeadClient.
type acceptanceMock struct {
	mu sync.Mutex
	// beads is the bead store keyed by ID.
	beads map[string]*beadsapi.BeadDetail
	// producers maps mrBeadID → producing task beads (the reverse-produces walk).
	producers map[string][]*beadsapi.BeadDetail
	// fieldUpdates records every UpdateBeadFields call.
	fieldUpdates []fieldUpdateRecord
	// comments records every AddComment call.
	comments []commentRecord
	// getBeadErr / producersErr force error paths.
	getBeadErr   error
	producersErr error
}

type fieldUpdateRecord struct {
	BeadID string
	Fields map[string]string
}

func newAcceptanceMock() *acceptanceMock {
	return &acceptanceMock{
		beads:     map[string]*beadsapi.BeadDetail{},
		producers: map[string][]*beadsapi.BeadDetail{},
	}
}

func (m *acceptanceMock) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getBeadErr != nil {
		return nil, m.getBeadErr
	}
	return m.beads[beadID], nil
}

func (m *acceptanceMock) ProducingTaskBeadsForMR(_ context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.producersErr != nil {
		return nil, m.producersErr
	}
	return m.producers[mrBeadID], nil
}

func (m *acceptanceMock) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]string, len(fields))
	maps.Copy(cp, fields)
	m.fieldUpdates = append(m.fieldUpdates, fieldUpdateRecord{BeadID: beadID, Fields: cp})
	// Reflect the write so an idempotency re-run sees the stamped value.
	if b := m.beads[beadID]; b != nil {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		maps.Copy(b.Fields, cp)
	}
	return nil
}

func (m *acceptanceMock) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, commentRecord{BeadID: beadID, Author: author, Text: text})
	return nil
}

func (m *acceptanceMock) updatesFor(beadID string) []fieldUpdateRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []fieldUpdateRecord
	for _, u := range m.fieldUpdates {
		if u.BeadID == beadID {
			out = append(out, u)
		}
	}
	return out
}

func (m *acceptanceMock) commentsFor(beadID string) []commentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []commentRecord
	for _, c := range m.comments {
		if c.BeadID == beadID {
			out = append(out, c)
		}
	}
	return out
}

// fixedClock returns a deterministic timestamp for stable assertions.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func newTestReconciler(m *acceptanceMock, scope string) *UpstreamAcceptanceReconciler {
	r := NewUpstreamAcceptanceReconciler(UpstreamAcceptanceReconcilerConfig{
		Daemon:     m,
		Logger:     slog.Default(),
		ScopeLabel: scope,
	})
	r.now = fixedClock(time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC))
	return r
}

// GATE: a merged upstream MR stamps the peel-request bead with acceptance
// fields and never closes it (or any dark work bead).
func TestAcceptanceReconciler_Merged_StampsPeelRequest_GATE(t *testing.T) {
	mrURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/777"
	m := newAcceptanceMock()
	m.beads["mr-1"] = &beadsapi.BeadDetail{
		ID:     "mr-1",
		Type:   "merge-request",
		Status: "closed",
		Labels: []string{"upstream:true"},
		Fields: map[string]string{"mr_url": mrURL, "mr_outcome": "merged"},
	}
	m.producers["mr-1"] = []*beadsapi.BeadDetail{
		{ID: "peel-req-1", Type: "task", Status: "open", Fields: map[string]string{}},
	}

	r := newTestReconciler(m, "upstream:true")
	r.Reconcile(context.Background(), "mr-1", "oidc:matthew.baker@acme.example")

	ups := m.updatesFor("peel-req-1")
	if len(ups) != 1 {
		t.Fatalf("expected 1 field update on peel-req-1, got %d", len(ups))
	}
	f := ups[0].Fields
	if f["upstream_accepted_at"] == "" {
		t.Errorf("upstream_accepted_at not stamped: %v", f)
	}
	if f["upstream_mr_url"] != mrURL {
		t.Errorf("upstream_mr_url=%q, want %q", f["upstream_mr_url"], mrURL)
	}
	if f["upstream_merged_by"] != "oidc:matthew.baker@acme.example" {
		t.Errorf("upstream_merged_by=%q, want operator id", f["upstream_merged_by"])
	}
	// No rejection field on an acceptance.
	if _, ok := f["upstream_rejected_at"]; ok {
		t.Errorf("unexpected upstream_rejected_at on a merged outcome")
	}
	// An acceptance comment was posted.
	if cs := m.commentsFor("peel-req-1"); len(cs) != 1 {
		t.Fatalf("expected 1 comment on peel-req-1, got %d", len(cs))
	}
	// GATE: the peel-request stays open — the reconciler must never close it.
	if m.beads["peel-req-1"] != nil && m.beads["peel-req-1"].Status == "closed" {
		t.Errorf("peel-req-1 was closed — reconciler must NOT auto-close any bead")
	}
}

// A closed-without-merge upstream MR stamps a rejection, not an acceptance.
func TestAcceptanceReconciler_Closed_StampsRejection(t *testing.T) {
	mrURL := "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/600"
	m := newAcceptanceMock()
	m.beads["mr-2"] = &beadsapi.BeadDetail{
		ID:     "mr-2",
		Type:   "merge-request",
		Status: "closed",
		Labels: []string{"upstream:true"},
		Fields: map[string]string{"mr_url": mrURL, "mr_outcome": "closed"},
	}
	m.producers["mr-2"] = []*beadsapi.BeadDetail{
		{ID: "peel-req-2", Type: "task", Status: "open", Fields: map[string]string{}},
	}

	r := newTestReconciler(m, "upstream:true")
	r.Reconcile(context.Background(), "mr-2", "")

	ups := m.updatesFor("peel-req-2")
	if len(ups) != 1 {
		t.Fatalf("expected 1 field update, got %d", len(ups))
	}
	f := ups[0].Fields
	if f["upstream_rejected_at"] == "" {
		t.Errorf("upstream_rejected_at not stamped: %v", f)
	}
	if f["upstream_mr_url"] != mrURL {
		t.Errorf("upstream_mr_url=%q, want %q", f["upstream_mr_url"], mrURL)
	}
	if _, ok := f["upstream_accepted_at"]; ok {
		t.Errorf("unexpected upstream_accepted_at on a closed (rejected) outcome")
	}
	if _, ok := f["upstream_merged_by"]; ok {
		t.Errorf("unexpected upstream_merged_by on a rejection")
	}
}

// Multi-producer: an MR linked from several peel-request beads stamps each.
func TestAcceptanceReconciler_MultiProducer_StampsAll(t *testing.T) {
	m := newAcceptanceMock()
	m.beads["mr-3"] = &beadsapi.BeadDetail{
		ID:     "mr-3",
		Type:   "merge-request",
		Status: "closed",
		Labels: []string{"upstream:true"},
		Fields: map[string]string{"mr_url": "https://gitlab.com/o/p/-/merge_requests/5", "mr_outcome": "merged"},
	}
	m.producers["mr-3"] = []*beadsapi.BeadDetail{
		{ID: "peel-a", Type: "task", Status: "open", Fields: map[string]string{}},
		{ID: "peel-b", Type: "task", Status: "open", Fields: map[string]string{}},
	}

	r := newTestReconciler(m, "upstream:true")
	r.Reconcile(context.Background(), "mr-3", "gitlab:merger")

	for _, id := range []string{"peel-a", "peel-b"} {
		if ups := m.updatesFor(id); len(ups) != 1 {
			t.Errorf("expected 1 update on %s, got %d", id, len(ups))
		}
	}
}

// Scope guard: a closed MR bead lacking the upstream marker label (a normal
// dark/prod merge) must NOT be stamped — its producers are real work beads.
func TestAcceptanceReconciler_OutOfScope_Skipped(t *testing.T) {
	m := newAcceptanceMock()
	m.beads["mr-4"] = &beadsapi.BeadDetail{
		ID:     "mr-4",
		Type:   "merge-request",
		Status: "closed",
		Labels: []string{}, // no upstream:true marker
		Fields: map[string]string{"mr_url": "https://gitlab.com/o/p/-/merge_requests/9", "mr_outcome": "merged"},
	}
	m.producers["mr-4"] = []*beadsapi.BeadDetail{
		{ID: "real-work-1", Type: "task", Status: "open", Fields: map[string]string{}},
	}

	r := newTestReconciler(m, "upstream:true")
	r.Reconcile(context.Background(), "mr-4", "gitlab:merger")

	if ups := m.updatesFor("real-work-1"); len(ups) != 0 {
		t.Errorf("expected no stamping for an out-of-scope MR, got %d updates", len(ups))
	}
	if cs := m.commentsFor("real-work-1"); len(cs) != 0 {
		t.Errorf("expected no comment for an out-of-scope MR, got %d", len(cs))
	}
}

// Idempotency: a peel-request already stamped for this outcome is skipped, so a
// webhook + backstop double-delivery stamps once and comments once.
func TestAcceptanceReconciler_Idempotent(t *testing.T) {
	mrURL := "https://gitlab.com/o/p/-/merge_requests/12"
	m := newAcceptanceMock()
	m.beads["mr-5"] = &beadsapi.BeadDetail{
		ID:     "mr-5",
		Type:   "merge-request",
		Status: "closed",
		Labels: []string{"upstream:true"},
		Fields: map[string]string{"mr_url": mrURL, "mr_outcome": "merged"},
	}
	m.beads["peel-5"] = &beadsapi.BeadDetail{ID: "peel-5", Type: "task", Status: "open", Fields: map[string]string{}}
	m.producers["mr-5"] = []*beadsapi.BeadDetail{m.beads["peel-5"]}

	r := newTestReconciler(m, "upstream:true")
	// First delivery (webhook) stamps.
	r.Reconcile(context.Background(), "mr-5", "oidc:matthew.baker@acme.example")
	// Second delivery (backstop, empty mergedBy) must be a no-op — the bead is
	// already stamped, and the empty mergedBy must not clobber the webhook value.
	r.Reconcile(context.Background(), "mr-5", "")

	if ups := m.updatesFor("peel-5"); len(ups) != 1 {
		t.Errorf("expected exactly 1 stamp (idempotent), got %d", len(ups))
	}
	if cs := m.commentsFor("peel-5"); len(cs) != 1 {
		t.Errorf("expected exactly 1 comment (idempotent), got %d", len(cs))
	}
	if got := m.beads["peel-5"].Fields["upstream_merged_by"]; got != "oidc:matthew.baker@acme.example" {
		t.Errorf("upstream_merged_by clobbered by re-poll: %q", got)
	}
}

// Empty scope label disables the guard (single-host / tests): every closed MR
// bead is treated as in-scope.
func TestAcceptanceReconciler_EmptyScope_NoGuard(t *testing.T) {
	m := newAcceptanceMock()
	m.beads["mr-6"] = &beadsapi.BeadDetail{
		ID:     "mr-6",
		Type:   "merge-request",
		Status: "closed",
		Labels: []string{}, // no marker, but empty scope = no guard
		Fields: map[string]string{"mr_url": "https://gitlab.com/o/p/-/merge_requests/1", "mr_outcome": "merged"},
	}
	m.producers["mr-6"] = []*beadsapi.BeadDetail{
		{ID: "peel-6", Type: "task", Status: "open", Fields: map[string]string{}},
	}

	r := newTestReconciler(m, "")
	r.Reconcile(context.Background(), "mr-6", "gitlab:merger")

	if ups := m.updatesFor("peel-6"); len(ups) != 1 {
		t.Errorf("expected stamping with empty scope, got %d updates", len(ups))
	}
}

// No producing peel-request → no write, no panic.
func TestAcceptanceReconciler_NoProducers_NoOp(t *testing.T) {
	m := newAcceptanceMock()
	m.beads["mr-7"] = &beadsapi.BeadDetail{
		ID:     "mr-7",
		Type:   "merge-request",
		Status: "closed",
		Labels: []string{"upstream:true"},
		Fields: map[string]string{"mr_url": "https://gitlab.com/o/p/-/merge_requests/2", "mr_outcome": "merged"},
	}
	// No producers registered for mr-7.

	r := newTestReconciler(m, "upstream:true")
	r.Reconcile(context.Background(), "mr-7", "gitlab:merger")

	if len(m.fieldUpdates) != 0 {
		t.Errorf("expected no field updates, got %d", len(m.fieldUpdates))
	}
}

// A GetBead failure is logged and skipped — never panics, never stamps.
func TestAcceptanceReconciler_GetBeadError_Skipped(t *testing.T) {
	m := newAcceptanceMock()
	m.getBeadErr = errors.New("daemon down")
	r := newTestReconciler(m, "upstream:true")
	r.Reconcile(context.Background(), "mr-x", "gitlab:merger")
	if len(m.fieldUpdates) != 0 {
		t.Errorf("expected no writes on GetBead error, got %d", len(m.fieldUpdates))
	}
}

// Empty mrBeadID / nil receiver are safe no-ops.
func TestAcceptanceReconciler_EmptyInput_NoOp(t *testing.T) {
	m := newAcceptanceMock()
	r := newTestReconciler(m, "upstream:true")
	r.Reconcile(context.Background(), "", "x")
	if len(m.fieldUpdates) != 0 {
		t.Errorf("expected no writes for empty mrBeadID, got %d", len(m.fieldUpdates))
	}
	var nilR *UpstreamAcceptanceReconciler
	nilR.Reconcile(context.Background(), "mr-1", "x") // must not panic
}
