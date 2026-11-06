package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeArmsLister serves one listing (or an error) and records the query.
type fakeArmsLister struct {
	beads []*beadsapi.BeadDetail
	err   error
	q     beadsapi.ListBeadsQuery
	ctx   context.Context
}

func (f *fakeArmsLister) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.q, f.ctx = q, ctx
	if f.err != nil {
		return nil, f.err
	}
	return &beadsapi.ListBeadsResult{Beads: f.beads, Total: len(f.beads)}, nil
}

// TestArmsGauge_CountsPerProject: the gauge reports not-closed epics with and
// without arms per project on /metrics (gd-YvWv4mR4fu), and keeps the last
// count when a recount fails.
func TestArmsGauge_CountsPerProject(t *testing.T) {
	f := &fakeArmsLister{beads: []*beadsapi.BeadDetail{
		{ID: "gd-1", Type: "epic", Labels: []string{"project:gardenio", "plan"}, Heraldry: testArms},
		{ID: "gd-2", Type: "epic", Labels: []string{"project:gardenio"}},
		{ID: "gd-3", Type: "epic", Labels: []string{"project:gardenio"}},
		{ID: "gd-4", Type: "epic", Labels: []string{"project:other"}, Heraldry: testArms},
		{ID: "gd-5", Type: "epic"},
	}}
	g := newArmsGauge(f)
	if err := g.refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !beadsapi.IsSystemRead(f.ctx) {
		t.Error("the gauge's count must be a system read: read-bearer enforcement refuses an unmarked one (gd-WUojuuXdbc)")
	}
	if len(f.q.Types) != 1 || f.q.Types[0] != "epic" {
		t.Errorf("the gauge must count epics only; query types %v", f.q.Types)
	}
	for _, s := range f.q.Statuses {
		if s == "closed" {
			t.Errorf("the gauge must count not-closed epics; query statuses %v", f.q.Statuses)
		}
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(g)
	want := `
# HELP gardenio_epics_arms Not-closed epics per project, with and without arms (heraldry).
# TYPE gardenio_epics_arms gauge
gardenio_epics_arms{arms="with",project="gardenio"} 1
gardenio_epics_arms{arms="without",project="gardenio"} 2
gardenio_epics_arms{arms="with",project="none"} 0
gardenio_epics_arms{arms="without",project="none"} 1
gardenio_epics_arms{arms="with",project="other"} 1
gardenio_epics_arms{arms="without",project="other"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "gardenio_epics_arms"); err != nil {
		t.Error(err)
	}

	f.err = errors.New("daemon down")
	if err := g.refresh(t.Context()); err == nil {
		t.Fatal("refresh must report the daemon error")
	}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "gardenio_epics_arms"); err != nil {
		t.Errorf("a failed recount must keep the last count: %v", err)
	}
}
