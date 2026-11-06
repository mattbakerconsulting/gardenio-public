package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// mockDaemon is an in-process stand-in for the beads daemon that RECORDS every
// write the escalation reaper makes instead of persisting it. It satisfies
// bridge.ReaperBeadClient in full:
//
//	ListBeadsFiltered / AddComment / CaptureEscalationResult / GetComments /
//	UpdateBeadFields / AddLabel
//
// Modelled on cmd/gitlab-prov-probe/mockdaemon.go, trimmed to the reaper's
// surface and extended with the two methods that mock lacks (GetComments,
// AddLabel) — the orchestration detector's comment witness and the review
// flag's needs-review label both need them.
//
// Because every write is a recorded call against an in-memory store and no real
// bead is ever created, nothing outside this process observes the probe: ZERO
// pods, ZERO shared-daemon state. That is what lets the module be a HARD gate in
// a namespace where gitlab-bridge (the reaper's host) is not even deployed.
type mockDaemon struct {
	mu sync.Mutex

	// beads is the escalation store keyed by ID. ListBeadsFiltered scans it and
	// applies the reaper's type+status query.
	beads map[string]*beadsapi.BeadDetail

	// existingComments are the comments GetComments serves per bead — the
	// operator/agent resolve-comment witnesses the orchestration detector reads.
	existingComments map[string][]beadsapi.Comment
	// getCommentsErr forces GetComments to fail for a bead, exercising the
	// "comment fetch failed ⇒ no witness this sweep, leave open" path.
	getCommentsErr map[string]error
	// getCommentsCalls counts GetComments per bead, so a leg can prove the field
	// witness SHORT-CIRCUITS and never pays for the comment round-trip.
	getCommentsCalls map[string]int

	// closeErr forces CaptureEscalationResult to fail for a bead
	// (audit-comment-then-close ordering assertion).
	closeErr map[string]error

	// listCalls counts ListBeadsFiltered calls — one per reaper sweep, since
	// sweepOnce opens with it. armed makes the FIRST list of a runSweep serve the
	// real open-set and every later one serve an EMPTY set, so exactly one
	// effective sweep happens per runSweep and observing call #2 proves call #1's
	// sweep finished. See runSweep for why this must be exact rather than a
	// quiescence heuristic.
	listCalls int
	armed     bool

	// comments / fieldUpdates / labels / closed record what the reaper wrote.
	comments     map[string][]commentRecord
	fieldUpdates map[string][]map[string]string
	labels       map[string][]string
	closed       map[string]map[string]string
	closeOrder   []string
}

type commentRecord struct {
	Author string
	Text   string
}

func newMockDaemon() *mockDaemon {
	return &mockDaemon{
		beads:            map[string]*beadsapi.BeadDetail{},
		existingComments: map[string][]beadsapi.Comment{},
		getCommentsErr:   map[string]error{},
		getCommentsCalls: map[string]int{},
		closeErr:         map[string]error{},
		comments:         map[string][]commentRecord{},
		fieldUpdates:     map[string][]map[string]string{},
		labels:           map[string][]string{},
		closed:           map[string]map[string]string{},
	}
}

// seedEscalation adds an OPEN type=escalation bead to the store.
func (m *mockDaemon) seedEscalation(b *beadsapi.BeadDetail) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.Type == "" {
		b.Type = "escalation"
	}
	if b.Status == "" {
		b.Status = "open"
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	m.beads[b.ID] = b
}

// seedComments sets the comments GetComments will serve for a bead.
func (m *mockDaemon) seedComments(beadID string, cs ...beadsapi.Comment) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.existingComments[beadID] = cs
}

// ── read-back accessors (probe assertions) ────────────────────────

// wasClosed reports whether the reaper closed the bead.
func (m *mockDaemon) wasClosed(beadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.closed[beadID]
	return ok
}

// closeField returns a field stamped on the close, or "".
func (m *mockDaemon) closeField(beadID, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed[beadID][key]
}

// commentsFor returns the bodies the reaper commented on a bead.
func (m *mockDaemon) commentsFor(beadID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.comments[beadID] {
		out = append(out, c.Text)
	}
	return out
}

// commentAuthors returns the authors the reaper commented as.
func (m *mockDaemon) commentAuthors(beadID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.comments[beadID] {
		out = append(out, c.Author)
	}
	return out
}

// fieldsFor returns the merged field-update patch applied to a bead.
func (m *mockDaemon) fieldsFor(beadID string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for _, patch := range m.fieldUpdates[beadID] {
		maps.Copy(out, patch)
	}
	return out
}

// updateCount returns how many UpdateBeadFields patches a bead received — the
// review flag's one-shot latch is proven by this staying at 1 across sweeps.
func (m *mockDaemon) updateCount(beadID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.fieldUpdates[beadID])
}

func (m *mockDaemon) labelsFor(beadID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.labels[beadID])
}

func (m *mockDaemon) commentCount(beadID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.comments[beadID])
}

func (m *mockDaemon) commentFetches(beadID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCommentsCalls[beadID]
}

// ── bridge.ReaperBeadClient ───────────────────────────────────────

// armSweep resets the per-runSweep list counter so the next list call serves
// the real open-set and subsequent ones serve empty.
func (m *mockDaemon) armSweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls = 0
	m.armed = true
}

// listCallCount reports how many ListBeadsFiltered calls the current runSweep
// has seen. Reaching 2 proves the first (real) sweep ran to completion.
func (m *mockDaemon) listCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalls
}

// ListBeadsFiltered serves the reaper's open-escalation sweep query. It honors
// Types + Statuses so a bead the reaper CLOSED drops out of the next sweep —
// the property the reaper's idempotency rests on.
//
// While armed, only the FIRST call of a runSweep serves the store; later calls
// serve an empty set so the extra ticks that prove sweep 1 finished cannot
// themselves mutate anything.
func (m *mockDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	if m.armed && m.listCalls > 1 {
		return &beadsapi.ListBeadsResult{}, nil
	}
	var out []*beadsapi.BeadDetail
	for _, id := range slices.Sorted(maps.Keys(m.beads)) {
		b := m.beads[id]
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out}, nil
}

func (m *mockDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments[beadID] = append(m.comments[beadID], commentRecord{Author: author, Text: text})
	return nil
}

// CaptureEscalationResult models POST /v1/escalations/{id}/result — the dedicated
// verb the reaper closes through (the daemon 409s a generic close of an open
// escalation since ka-0000005516). Like the daemon's handleCaptureEscalationResult
// (P7 Task 10) it refuses any Fields key outside the render/witness allowlist and
// ignores the body executor, merges Fields with result_status, records the close,
// AND flips the stored bead to closed so the next sweep's ListBeadsFiltered drops
// it (idempotency).
func (m *mockDaemon) CaptureEscalationResult(_ context.Context, escalationID string, req beadsapi.CaptureEscalationResultRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.closeErr[escalationID]; err != nil {
		return err
	}
	merged := map[string]string{}
	for k, v := range req.Fields {
		if k != "reaped" && !strings.HasPrefix(k, "reaped_") && !strings.HasPrefix(k, "mr_") && !strings.HasPrefix(k, "approvals_") {
			return fmt.Errorf("400: result field %s is not allowed", k)
		}
		if v != "" {
			merged[k] = v
		}
	}
	merged["result_status"] = req.ResultStatus
	m.closed[escalationID] = merged
	m.closeOrder = append(m.closeOrder, escalationID)
	if b, ok := m.beads[escalationID]; ok {
		b.Status = "closed"
		maps.Copy(b.Fields, merged)
	}
	return nil
}

func (m *mockDaemon) GetComments(_ context.Context, beadID string) ([]beadsapi.Comment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCommentsCalls[beadID]++
	if err := m.getCommentsErr[beadID]; err != nil {
		return nil, err
	}
	return slices.Clone(m.existingComments[beadID]), nil
}

// UpdateBeadFields records the patch and applies it to the stored bead, so the
// review flag's mr_landed latch is visible to the NEXT sweep.
func (m *mockDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fieldUpdates[beadID] = append(m.fieldUpdates[beadID], maps.Clone(fields))
	b, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("no such bead %s", beadID)
	}
	maps.Copy(b.Fields, fields)
	return nil
}

func (m *mockDaemon) AddLabel(_ context.Context, beadID, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Contains(m.labels[beadID], label) {
		m.labels[beadID] = append(m.labels[beadID], label)
	}
	if b, ok := m.beads[beadID]; ok && !slices.Contains(b.Labels, label) {
		b.Labels = append(b.Labels, label)
	}
	return nil
}
