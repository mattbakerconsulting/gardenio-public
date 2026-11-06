package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeSweeperClient captures ListAllBeadsFiltered + CloseBead calls and returns
// preconfigured responses. Satisfies adviceSweeperClient.
type fakeSweeperClient struct {
	mu         sync.Mutex
	listResult *beadsapi.ListBeadsResult
	listErr    error
	listCalls  int

	closeErrFor map[string]error
	closed      map[string]map[string]string
	closeCalls  []string
}

func newFakeSweeperClient() *fakeSweeperClient {
	return &fakeSweeperClient{
		closed:      make(map[string]map[string]string),
		closeErrFor: make(map[string]error),
	}
}

func (f *fakeSweeperClient) ListAllBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResult == nil {
		return &beadsapi.ListBeadsResult{}, nil
	}
	return f.listResult, nil
}

func (f *fakeSweeperClient) CloseBead(_ context.Context, beadID string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls = append(f.closeCalls, beadID)
	if err, ok := f.closeErrFor[beadID]; ok {
		return err
	}
	f.closed[beadID] = fields
	return nil
}

func (f *fakeSweeperClient) closedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.closed))
	for id := range f.closed {
		out = append(out, id)
	}
	return out
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mkBead(id, typ string, expiresAt string) *beadsapi.BeadDetail {
	fields := map[string]string{}
	if expiresAt != "" {
		fields["expires_at"] = expiresAt
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   typ,
		Status: "open",
		Fields: fields,
	}
}

func TestAdviceSweep_ClosesExpiredAdvice(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	fake := newFakeSweeperClient()
	fake.listResult = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkBead("gd-expired1", "advice", past),
			mkBead("gd-expired2", "advice", past),
		},
	}

	sw := newAdviceSweeper(fake, time.Minute, true, silentLogger())
	sw.sweep(context.Background())

	closed := fake.closedIDs()
	if len(closed) != 2 {
		t.Fatalf("expected 2 closed beads, got %d: %v", len(closed), closed)
	}
	if _, ok := fake.closed["gd-expired1"]; !ok {
		t.Error("gd-expired1 was not closed")
	}
	fields := fake.closed["gd-expired1"]
	if got := fields["close_reason"]; got == "" || !strings.Contains(got, "auto-closed") || !strings.Contains(got, past) {
		t.Errorf("close_reason missing expected markers: %q", got)
	}
}

func mkAdvice(id, desc string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "advice", Status: "open", Description: desc}
}

func TestAdviceSweep_ClosesSupersededAdvice(t *testing.T) {
	fake := newFakeSweeperClient()
	fake.listResult = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkAdvice("gd-new", "Better rule. supersedes(gd-old)"),
			mkAdvice("gd-old", "Old rule."),
			mkAdvice("gd-keep", "Unrelated rule."),
		},
	}

	sw := newAdviceSweeper(fake, time.Minute, true, silentLogger())
	sw.sweep(context.Background())

	closed := fake.closedIDs()
	if len(closed) != 1 || closed[0] != "gd-old" {
		t.Fatalf("expected only gd-old closed, got %v", closed)
	}
	// The superseder and unrelated advice stay open.
	if _, ok := fake.closed["gd-new"]; ok {
		t.Error("superseder gd-new should not be closed")
	}
	if _, ok := fake.closed["gd-keep"]; ok {
		t.Error("unrelated gd-keep should not be closed")
	}
	reason := fake.closed["gd-old"]["close_reason"]
	if !strings.Contains(reason, "superseded by gd-new") {
		t.Errorf("close_reason missing supersession link: %q", reason)
	}
}

func TestAdviceSweep_CompactionDisabledSkipsSupersession(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	expired := mkBead("gd-expired", "advice", past)
	fake := newFakeSweeperClient()
	fake.listResult = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkAdvice("gd-new", "supersedes(gd-old)"),
			mkAdvice("gd-old", "Old rule."),
			expired,
		},
	}

	// compaction=false: only the expiry pass runs.
	sw := newAdviceSweeper(fake, time.Minute, false, silentLogger())
	sw.sweep(context.Background())

	closed := fake.closedIDs()
	if len(closed) != 1 || closed[0] != "gd-expired" {
		t.Fatalf("expected only gd-expired closed when compaction disabled, got %v", closed)
	}
}

func TestAdviceSweep_ExpiryWinsOverSupersession(t *testing.T) {
	// A bead that is both expired and superseded is closed via the expiry path
	// (expiry is checked first); the reason must reflect expiry, not supersession.
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	expiredSuperseded := mkAdvice("gd-both", "Old rule.")
	expiredSuperseded.Fields = map[string]string{"expires_at": past}
	fake := newFakeSweeperClient()
	fake.listResult = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkAdvice("gd-new", "supersedes(gd-both)"),
			expiredSuperseded,
		},
	}

	sw := newAdviceSweeper(fake, time.Minute, true, silentLogger())
	sw.sweep(context.Background())

	if _, ok := fake.closed["gd-both"]; !ok {
		t.Fatal("gd-both should have been closed")
	}
	reason := fake.closed["gd-both"]["close_reason"]
	if !strings.Contains(reason, "expiration") || strings.Contains(reason, "superseded") {
		t.Errorf("expected expiry reason, got %q", reason)
	}
}

func TestAdviceSweep_LeavesUnexpiredAdvice(t *testing.T) {
	future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	fake := newFakeSweeperClient()
	fake.listResult = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkBead("gd-future", "advice", future),
			mkBead("gd-noexpiry", "advice", ""),
		},
	}

	sw := newAdviceSweeper(fake, time.Minute, true, silentLogger())
	sw.sweep(context.Background())

	if got := len(fake.closedIDs()); got != 0 {
		t.Fatalf("expected 0 closed beads, got %d: %v", got, fake.closedIDs())
	}
}

func TestAdviceSweep_ContinuesOnCloseError(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	fake := newFakeSweeperClient()
	fake.closeErrFor["gd-expired1"] = errors.New("daemon unavailable")
	fake.listResult = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkBead("gd-expired1", "advice", past),
			mkBead("gd-expired2", "advice", past),
		},
	}

	sw := newAdviceSweeper(fake, time.Minute, true, silentLogger())
	sw.sweep(context.Background())

	// gd-expired2 should still have been closed.
	if _, ok := fake.closed["gd-expired2"]; !ok {
		t.Error("sweep aborted after first close error; gd-expired2 not closed")
	}
}

func TestAdviceSweep_NoBeads(t *testing.T) {
	fake := newFakeSweeperClient()
	sw := newAdviceSweeper(fake, time.Minute, true, silentLogger())
	sw.sweep(context.Background())
	if len(fake.closedIDs()) != 0 {
		t.Fatal("no beads should have been closed")
	}
}

func TestAdviceSweep_ListError(t *testing.T) {
	fake := newFakeSweeperClient()
	fake.listErr = errors.New("boom")

	sw := newAdviceSweeper(fake, time.Minute, true, silentLogger())
	// Must not panic; must not close anything.
	sw.sweep(context.Background())
	if len(fake.closedIDs()) != 0 {
		t.Fatal("no beads should have been closed on list error")
	}
}

func TestAdviceSweep_QueryFiltersAdviceOpen(t *testing.T) {
	fake := newFakeSweeperClient()
	// Intercept ListAllBeadsFiltered to assert query params.
	var gotQuery beadsapi.ListBeadsQuery
	intercepted := &queryCapturingClient{fakeSweeperClient: fake, captured: &gotQuery}

	sw := newAdviceSweeper(intercepted, time.Minute, true, silentLogger())
	sw.sweep(context.Background())

	if len(gotQuery.Types) != 1 || gotQuery.Types[0] != "advice" {
		t.Errorf("expected Types=[advice], got %v", gotQuery.Types)
	}
	if len(gotQuery.Statuses) != 1 || gotQuery.Statuses[0] != "open" {
		t.Errorf("expected Statuses=[open], got %v", gotQuery.Statuses)
	}
}

type queryCapturingClient struct {
	*fakeSweeperClient
	captured *beadsapi.ListBeadsQuery
}

func (q *queryCapturingClient) ListAllBeadsFiltered(ctx context.Context, query beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	*q.captured = query
	return q.fakeSweeperClient.ListAllBeadsFiltered(ctx, query)
}

func TestAdviceSweep_RunLoop_DisabledWhenIntervalZero(t *testing.T) {
	fake := newFakeSweeperClient()
	sw := newAdviceSweeper(fake, 0, true, silentLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		sw.RunLoop(ctx)
		close(done)
	}()

	select {
	case <-done:
		// RunLoop returned immediately, as expected.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("RunLoop did not return when interval=0")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listCalls != 0 {
		t.Errorf("expected no list calls when disabled, got %d", fake.listCalls)
	}
}

func TestAdviceSweep_RunLoop_ImmediateSweepThenTick(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	fake := newFakeSweeperClient()
	fake.listResult = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{mkBead("gd-expired", "advice", past)},
	}
	sw := newAdviceSweeper(fake, 20*time.Millisecond, true, silentLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	sw.RunLoop(ctx)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listCalls < 2 {
		t.Errorf("expected at least 2 list calls (immediate + tick), got %d", fake.listCalls)
	}
}
