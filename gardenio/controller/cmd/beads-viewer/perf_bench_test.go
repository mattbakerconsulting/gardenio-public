package main

// perf_bench_test.go holds the package-level benchmarks (run them with
// `go test -bench`; no CI job runs them since the RWX bench task was deleted). Each benchmark targets a
// handler or hot-path that an earlier perf audit identified as a serial
// chain or N+1 — so a regression that re-introduces the chain shows up as
// a multi-x slowdown in benchstat output.
//
// Benchmarks deliberately use the in-package fakeDaemon (no network, no
// daemon) so the numbers reflect Go-side concurrency overhead and template
// rendering only. Real-world wins from removing N+1s are larger than the
// bench numbers suggest because each fake-daemon call returns instantly;
// the bench is sensitive to *additional* serial dispatch in the handler,
// which is what matters for regression detection.

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// benchFakeForRichReport seeds n widget-instance children referenced by a
// single rich-report parent. The bench scales linearly in n so a regression
// that drops fan-out shows up as ns/op growing roughly n× rather than
// n/cap×.
func benchFakeForRichReport(n int) (*beadsapi.BeadDetail, []beadsapi.BeadReferenceSummary, *fakeDaemon) {
	parentChild := "parent-child"
	parent := &beadsapi.BeadDetail{
		ID:     "gd-bench-rr",
		Type:   "rich-report",
		Fields: map[string]string{"headline": "bench"},
	}
	mentionedBy := make([]beadsapi.BeadReferenceSummary, 0, n)
	f := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}}
	for i := 0; i < n; i++ {
		id := "gd-w" + strconv.Itoa(i)
		mentionedBy = append(mentionedBy, beadsapi.BeadReferenceSummary{
			ID: id, Type: "widget-instance", RelationshipType: &parentChild,
		})
		f.beads[id] = &beadsapi.BeadDetail{
			ID:   id,
			Type: "widget-instance",
			Fields: map[string]string{
				"widget_type": "alert-summary",
				"position":    strconv.Itoa(i),
				"data":        `{"i":` + strconv.Itoa(i) + `}`,
			},
		}
	}
	return parent, mentionedBy, f
}

func BenchmarkBuildRichReportView_16Widgets(b *testing.B) {
	parent, mentionedBy, f := benchFakeForRichReport(16)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildRichReportView(ctx, parent, nil, mentionedBy, f, nil)
	}
}

func BenchmarkBuildRichReportView_64Widgets(b *testing.B) {
	parent, mentionedBy, f := benchFakeForRichReport(64)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildRichReportView(ctx, parent, nil, mentionedBy, f, nil)
	}
}

// BenchmarkListGardenerMentors targets the create-gardener modal hydration —
// 50 gardener rows each with a TaskID. Pre-fan-out this was 50 serial RTTs;
// post-fan-out it's bounded by gardenerMentorTaskFanout=8.
func BenchmarkListGardenerMentors(b *testing.B) {
	const n = 50
	f := newFake()
	beads := make([]*beadsapi.BeadDetail, 0, n)
	for i := 0; i < n; i++ {
		capID := "gd-cap" + strconv.Itoa(i)
		taskID := "gd-task" + strconv.Itoa(i)
		beads = append(beads, &beadsapi.BeadDetail{
			ID:     capID,
			Title:  "gardener-" + strconv.Itoa(i),
			Type:   "agent",
			Kind:   "config",
			Status: "in_progress",
			Labels: []string{"role:gardener"},
			Fields: map[string]string{"task_id": taskID},
		})
		f.beads[taskID] = &beadsapi.BeadDetail{ID: taskID, Title: "task-" + strconv.Itoa(i)}
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: beads, Total: n}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = srv.listGardenerMentors(ctx)
	}
}

// BenchmarkHandleGardeners covers the /gardeners index — the page flagged for a
// perf regression (ka-0000003665). Each row resolves its owner; before the
// gardenerOwnerCache fix a Slack-owned row triggered an uncached alias lookup
// plus a FULL operator-directory scan, so the page was O(gardeners) serial RTTs
// + O(gardeners) directory fetches. The bench seeds a large synthetic gardener
// set so a re-introduced N+1 shows up as ns/op multiplying by the row count
// rather than staying flat. distinctOwners < gardeners exercises the per-owner
// memo; the shared-directory memo bounds the worst case regardless.
func benchHandleGardeners(b *testing.B, gardeners, distinctOwners int) {
	b.Helper()
	f := newFakeSlackOwnedGardeners(gardeners, distinctOwners)
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) })
	mux := edgeAuthMux(srv)
	b.ResetTimer()
	for range b.N {
		req := httptest.NewRequest("GET", "/gardeners", nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
	}
}

func BenchmarkHandleGardeners_50(b *testing.B)  { benchHandleGardeners(b, 50, 5) }
func BenchmarkHandleGardeners_300(b *testing.B) { benchHandleGardeners(b, 300, 8) }

// BenchmarkHandleAgents covers the /agents page which fans active +
// recent-closed ListBeads in parallel under sync.WaitGroup. Regression
// guard against re-serializing the two calls.
func BenchmarkHandleAgents(b *testing.B) {
	f := newFake()
	// Active agents.
	active := make([]*beadsapi.BeadDetail, 0, 10)
	for i := 0; i < 10; i++ {
		active = append(active, &beadsapi.BeadDetail{
			ID: "gd-act" + strconv.Itoa(i), Type: "agent", Status: "in_progress",
			Title: "agent-" + strconv.Itoa(i),
		})
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: active, Total: 10}
	// Recently-closed agents — separate bucket the fake routes by Statuses.
	closed := make([]*beadsapi.BeadDetail, 0, 10)
	for i := 0; i < 10; i++ {
		closed = append(closed, &beadsapi.BeadDetail{
			ID: "gd-cls" + strconv.Itoa(i), Type: "agent", Status: "closed",
			Title: "agent-closed-" + strconv.Itoa(i),
		})
	}
	f.agentRecentResp = &beadsapi.ListBeadsResult{Beads: closed, Total: 10}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	mux := edgeAuthMux(srv)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/agents", nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
	}
}
