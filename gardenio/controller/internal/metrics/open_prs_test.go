package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/prhealth"
)

// sweeperBeads serves the PR sweeper trigger from landBeads and its runs'
// agent beads from GetBead.
type sweeperBeads struct {
	landBeads
	agents map[string]*beadsapi.BeadDetail
}

func (s *sweeperBeads) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := s.agents[id]; ok {
		return b, nil
	}
	return nil, errors.New("bead not found")
}

func (s *sweeperBeads) run(agentID, record string) {
	s.list = nil
	s.add("trigger", "PR sweeper", []string{prhealth.TriggerLabel}, map[string]string{"last_agent_id": agentID})
	fields := map[string]string{}
	if record != "" {
		fields[prhealth.Field] = record
	}
	s.agents[agentID] = &beadsapi.BeadDetail{ID: agentID, Type: "agent", Fields: fields}
}

func TestCollectOpenPRs(t *testing.T) {
	q := &sweeperBeads{agents: map[string]*beadsapi.BeadDetail{}}
	q.run("gd-run1", `{"at":"2026-10-30T14:17:00Z","open":22,"desk_queue":3,"orphans":7,`+
		`"oldest_orphan_hours":124.5,"oldest_open_hours":130,"conflicting":4,"oldest_conflict_hours":7.5,`+
		`"train":"red","train_pr":1570}`)
	c := newLandCreditsCollector(q)
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatal(err)
	}
	c.collectOpenPRs(context.Background())

	want := `
# HELP gardenio_open_prs Open PRs into main at the PR sweeper's last run. state=open is every open PR; desk_queue the ones an open merge escalation names; orphaned the ones with no merge escalation and no living owner when the sweeper found them, before it escalated or closed them; conflicting the ones that conflict with main, from the PR beads' mr_merge_state.
# TYPE gardenio_open_prs gauge
gardenio_open_prs{state="conflicting"} 4
gardenio_open_prs{state="desk_queue"} 3
gardenio_open_prs{state="open"} 22
gardenio_open_prs{state="orphaned"} 7
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "gardenio_open_prs"); err != nil {
		t.Error(err)
	}
	if got := testutil.ToFloat64(c.openPRs.oldest.WithLabelValues("open")); got != 130 {
		t.Errorf("oldest open = %v, want 130", got)
	}
	if got := testutil.ToFloat64(c.openPRs.oldest.WithLabelValues("orphaned")); got != 124.5 {
		t.Errorf("oldest orphan = %v, want 124.5", got)
	}
	if got := testutil.ToFloat64(c.openPRs.oldest.WithLabelValues("conflicting")); got != 7.5 {
		t.Errorf("oldest conflict = %v, want 7.5", got)
	}
	if got := testutil.ToFloat64(c.openPRs.train.WithLabelValues("red")); got != 1 {
		t.Errorf("train red = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.openPRs.train.WithLabelValues("green")); got != 0 {
		t.Errorf("train green = %v, want 0", got)
	}
	if n := testutil.CollectAndCount(c.openPRs.train); n != len(prhealth.TrainStates) {
		t.Errorf("train series = %d, want one per state (%d)", n, len(prhealth.TrainStates))
	}
	at := time.Date(2026, 10, 30, 14, 17, 0, 0, time.UTC)
	if got := testutil.ToFloat64(c.openPRs.recordedAt); got != float64(at.Unix()) {
		t.Errorf("recorded at = %v, want %v", got, at.Unix())
	}

	// The next run has not written yet: the last record stays.
	q.run("gd-run2", "")
	c.openPRsAt = time.Time{}
	c.collectOpenPRs(context.Background())
	if got := testutil.ToFloat64(c.openPRs.count.WithLabelValues("orphaned")); got != 7 {
		t.Errorf("orphaned after an unwritten run = %v, want the last record's 7", got)
	}

	// Within the refresh interval the record is not re-read.
	q.run("gd-run3", `{"at":"2026-10-30T15:17:00Z","open":20,"orphans":1,"train":"none"}`)
	c.collectOpenPRs(context.Background())
	if got := testutil.ToFloat64(c.openPRs.count.WithLabelValues("orphaned")); got != 7 {
		t.Errorf("orphaned within the interval = %v, want 7", got)
	}
	c.openPRsAt = time.Time{}
	c.collectOpenPRs(context.Background())
	if got := testutil.ToFloat64(c.openPRs.count.WithLabelValues("orphaned")); got != 1 {
		t.Errorf("orphaned after the next run = %v, want 1", got)
	}
}

func TestCollectOpenPRsAbsentWithoutARecord(t *testing.T) {
	q := &sweeperBeads{agents: map[string]*beadsapi.BeadDetail{}}
	c := newLandCreditsCollector(q)
	c.collectOpenPRs(context.Background())
	if n := testutil.CollectAndCount(c.openPRs.count); n != 0 {
		t.Errorf("open_prs series with no sweeper trigger = %d, want 0", n)
	}
}
