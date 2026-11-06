package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// fakeReaperDaemon answers the reaper's three list queries from fixed rows and
// records closes.
type fakeReaperDaemon struct {
	sessions []*beadsapi.BeadDetail
	live     []*beadsapi.BeadDetail
	recent   []*beadsapi.BeadDetail
	listErr  error
	closeErr map[string]error

	closes map[string]map[string]string
	order  []string
}

func (f *fakeReaperDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var rows []*beadsapi.BeadDetail
	switch {
	case slices.Equal(q.Types, []string{"session"}):
		rows = f.sessions
	case slices.Equal(q.Statuses, []string{"closed"}):
		if f.listErr != nil {
			return nil, f.listErr
		}
		rows = f.recent
	default:
		rows = f.live
	}
	end := min(q.Offset+q.Limit, len(rows))
	if q.Offset >= end {
		return &beadsapi.ListBeadsResult{}, nil
	}
	return &beadsapi.ListBeadsResult{Beads: rows[q.Offset:end]}, nil
}

func (f *fakeReaperDaemon) CloseBead(_ context.Context, id string, fields map[string]string) error {
	if err := f.closeErr[id]; err != nil {
		return err
	}
	if f.closes == nil {
		f.closes = map[string]map[string]string{}
	}
	f.closes[id] = fields
	f.order = append(f.order, id)
	return nil
}

func reaperSession(id, agentID string, created time.Time, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{"agent_bead_id": agentID, "status": "active"}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Type: "session", Status: "open", CreatedAt: created, Fields: f}
}

func reaperAgent(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "open"}
}

func newTestReaper(d sessionReaperDaemon) (*sessionReaper, *gardeniometrics.EventMetrics) {
	em := gardeniometrics.NewEventMetrics("test")
	return &sessionReaper{daemon: d, metrics: em, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), interval: time.Minute}, em
}

func orphanGauge(t *testing.T, em *gardeniometrics.EventMetrics, phase string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := em.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_orphan_open_sessions" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "phase" && lp.GetValue() == phase {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("no gardenio_orphan_open_sessions{phase=%q}", phase)
	return 0
}

func TestSessionReaper_ClosesSessionsOfEndedAgents(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-3 * time.Hour)
	d := &fakeReaperDaemon{
		sessions: []*beadsapi.BeadDetail{
			reaperSession("s-live", "a-live", old, nil),                    // agent live: keep
			reaperSession("s-closed", "a-closed", old, nil),                // agent closed long ago: close
			reaperSession("s-gone", "a-gone", old, nil),                    // agent missing: close
			reaperSession("s-recent", "a-recent", old, nil),                // agent closed inside grace: keep
			reaperSession("s-fresh", "a-gone", now.Add(-time.Minute), nil), // session inside grace: keep
			reaperSession("s-noagent", "", old, nil),                       // no agent id: keep
			// gb stamped the exit but the close leg failed; agent still live.
			reaperSession("s-stamped", "a-live", old, map[string]string{"status": "completed", "ended_at": old.Add(time.Hour).Format(time.RFC3339)}),
		},
		live:   []*beadsapi.BeadDetail{reaperAgent("a-live")},
		recent: []*beadsapi.BeadDetail{{ID: "a-recent", Type: "agent", Status: "closed"}},
	}
	r, em := newTestReaper(d)
	r.reap(context.Background(), now)

	want := []string{"s-closed", "s-gone", "s-stamped"}
	if !slices.Equal(d.order, want) {
		t.Fatalf("closed %v, want %v", d.order, want)
	}
	stamp := now.Format(time.RFC3339)
	if got := d.closes["s-closed"]; got["status"] != "retired" || got["ended_at"] != stamp {
		t.Errorf("s-closed close fields = %v, want status=retired ended_at=%s", got, stamp)
	}
	if got := d.closes["s-stamped"]; len(got) != 0 {
		t.Errorf("s-stamped close fields = %v, want none (gb's exit record kept)", got)
	}
	if got := orphanGauge(t, em, "found"); got != 3 {
		t.Errorf("found gauge = %v, want 3", got)
	}
	if got := orphanGauge(t, em, "left"); got != 0 {
		t.Errorf("left gauge = %v, want 0", got)
	}
}

func TestSessionReaper_FailedCloseCountsAsLeft(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	d := &fakeReaperDaemon{
		sessions: []*beadsapi.BeadDetail{
			reaperSession("s-1", "a-gone", old, nil),
			reaperSession("s-2", "a-gone", old, nil),
		},
		closeErr: map[string]error{"s-1": errors.New("enum rejected")},
	}
	r, em := newTestReaper(d)
	r.reap(context.Background(), now)

	if got := orphanGauge(t, em, "found"); got != 2 {
		t.Errorf("found gauge = %v, want 2", got)
	}
	if got := orphanGauge(t, em, "left"); got != 1 {
		t.Errorf("left gauge = %v, want 1", got)
	}
}

// A failed agent list must skip the cycle: closing against a partial live set
// would end the sessions of live agents.
func TestSessionReaper_AgentListErrorSkipsCycle(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	d := &fakeReaperDaemon{
		sessions: []*beadsapi.BeadDetail{reaperSession("s-1", "a-1", now.Add(-time.Hour), nil)},
		listErr:  errors.New("daemon down"),
	}
	r, _ := newTestReaper(d)
	r.reap(context.Background(), now)
	if len(d.order) != 0 {
		t.Fatalf("closed %v on a failed agent list, want none", d.order)
	}
}

// A live-agent list that hits the page cap is truncated and must skip the
// cycle for the same reason.
func TestSessionReaper_TruncatedLiveListSkipsCycle(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	live := make([]*beadsapi.BeadDetail, sessionReaperPageSize*sessionReaperMaxPages)
	for i := range live {
		live[i] = reaperAgent("a-filler")
	}
	d := &fakeReaperDaemon{
		sessions: []*beadsapi.BeadDetail{reaperSession("s-1", "a-unlisted", now.Add(-time.Hour), nil)},
		live:     live,
	}
	r, _ := newTestReaper(d)
	r.reap(context.Background(), now)
	if len(d.order) != 0 {
		t.Fatalf("closed %v on a truncated live list, want none", d.order)
	}
}

func TestSessionReaper_CapsClosesPerCycle(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	n := sessionReaperMaxCloses + 7
	sessions := make([]*beadsapi.BeadDetail, n)
	for i := range sessions {
		sessions[i] = reaperSession(fmt.Sprintf("s-%d", i), "a-gone", now.Add(-time.Hour), nil)
	}
	d := &fakeReaperDaemon{sessions: sessions}
	r, em := newTestReaper(d)
	r.reap(context.Background(), now)

	if len(d.order) != sessionReaperMaxCloses {
		t.Fatalf("closed %d, want cap %d", len(d.order), sessionReaperMaxCloses)
	}
	if got := orphanGauge(t, em, "left"); got != 7 {
		t.Errorf("left gauge = %v, want 7", got)
	}
}
