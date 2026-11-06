package server

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestSweepSlowToolCalls_DeletesExpired(t *testing.T) {
	srv, ms, _ := newTestServer()
	now := time.Now().UTC()
	for _, age := range []time.Duration{100 * 24 * time.Hour, 61 * 24 * time.Hour, 10 * 24 * time.Hour, time.Hour} {
		_ = ms.InsertSlowToolCall(context.Background(), &model.SlowToolCallRow{
			TS: now.Add(-age), ToolName: "Bash", DurationMs: 3000, Source: "claude",
		})
	}

	srv.sweepSlowToolCalls(context.Background(), 60)

	if got := len(ms.slowToolCalls); got != 2 {
		t.Fatalf("remaining rows = %d, want 2 (the 10d and 1h rows)", got)
	}
	for _, r := range ms.slowToolCalls {
		if r.TS.Before(now.Add(-60 * 24 * time.Hour)) {
			t.Errorf("row with ts %s survived a 60-day retention", r.TS)
		}
	}
}

func TestSweepSlowToolCalls_DrainsMultipleBatches(t *testing.T) {
	srv, ms, _ := newTestServer()
	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	n := slowToolCallsRetentionBatch*2 + 5
	for i := 0; i < n; i++ {
		_ = ms.InsertSlowToolCall(context.Background(), &model.SlowToolCallRow{
			TS: old, ToolName: "Bash", DurationMs: 3000, Source: "claude",
		})
	}

	srv.sweepSlowToolCalls(context.Background(), 60)

	if got := len(ms.slowToolCalls); got != 0 {
		t.Fatalf("remaining rows = %d, want 0 after multi-batch drain", got)
	}
}

func TestRunSlowToolCallsRetention_StopsOnCancel(t *testing.T) {
	srv, _, _ := newTestServer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.runSlowToolCallsRetention(ctx, 60, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSlowToolCallsRetention did not stop after context cancellation")
	}
}
