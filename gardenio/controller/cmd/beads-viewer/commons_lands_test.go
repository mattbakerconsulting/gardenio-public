package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

type landSwitchSource struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (s *landSwitchSource) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []*beadsapi.BeadDetail
	for _, b := range s.beads {
		if slices.Contains(q.Types, b.Type) && q.Offset == 0 {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func TestLandSwitchesReadTheFreezeAndKeepTheLastGoodRead(t *testing.T) {
	src := &landSwitchSource{beads: []*beadsapi.BeadDetail{
		{ID: "kd-1", Type: "project", Title: "tomato-patch", Status: "open", Fields: map[string]string{"land_kind": "tenant", "land_frozen": "true"}},
		{ID: "kd-2", Type: "project", Title: "pea-row", Status: "open", Fields: map[string]string{"land_kind": "tenant"}},
		{ID: "kd-3", Type: "project", Title: "gardenio", Status: "open", Fields: map[string]string{"land_frozen": "true"}},
	}}
	l := newLandSwitches(src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	// Before any read nothing is frozen (kbeads still refuses the write).
	if tenant, frozen := l.state(ctx, "tomato-patch"); tenant || frozen {
		t.Fatalf("before a read: tenant %v frozen %v", tenant, frozen)
	}
	l.refresh()
	for _, tc := range []struct {
		land           string
		tenant, frozen bool
	}{
		{"tomato-patch", true, true},
		{"pea-row", true, false},
		{"gardenio", false, false}, // land_frozen means nothing on an operator land
		{"nowhere", false, false},
	} {
		if tenant, frozen := l.state(ctx, tc.land); tenant != tc.tenant || frozen != tc.frozen {
			t.Errorf("%s: tenant %v frozen %v, want %v %v", tc.land, tenant, frozen, tc.tenant, tc.frozen)
		}
	}
	src.err = errors.New("daemon down")
	l.refresh()
	if _, frozen := l.state(ctx, "tomato-patch"); !frozen {
		t.Error("a failed read must keep the last good one")
	}
}
