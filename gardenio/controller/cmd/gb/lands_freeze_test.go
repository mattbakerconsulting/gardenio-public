package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/freeze"
	"gardenio/controller/internal/pausegate"
)

// gateStore is a landsGateStore over gate beads.
type gateStore struct {
	gates    []*beadsapi.BeadDetail
	comments []string
	listErr  error
}

func (g *gateStore) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if g.listErr != nil {
		return nil, g.listErr
	}
	var out []*beadsapi.BeadDetail
	for _, b := range g.gates {
		if slices.Contains(q.Types, b.Type) && slices.Contains(q.Statuses, b.Status) {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (g *gateStore) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	id := "kd-g" + string(rune('1'+len(g.gates)))
	g.gates = append(g.gates, &beadsapi.BeadDetail{ID: id, Type: req.Type, Title: req.Title, Status: "open"})
	return id, nil
}

func (g *gateStore) CloseBead(_ context.Context, id string, _ map[string]string) error {
	for _, b := range g.gates {
		if b.ID == id {
			b.Status = "closed"
		}
	}
	return nil
}

func (g *gateStore) AddComment(_ context.Context, _, _, text string) error {
	g.comments = append(g.comments, text)
	return nil
}

func (g *gateStore) open() int {
	n := 0
	for _, b := range g.gates {
		if b.Status == "open" && b.Title == pausegate.LandsPauseTitle {
			n++
		}
	}
	return n
}

func TestLandsPauseResume(t *testing.T) {
	ctx := context.Background()
	st := &gateStore{}
	if _, err := pauseLands(ctx, st, "", "oidc:op"); err == nil {
		t.Fatal("a pause needs a reason")
	}
	g, err := pauseLands(ctx, st, "incident gd-abc", "oidc:op")
	if err != nil || !g.Changed || !g.Paused || st.open() != 1 {
		t.Fatalf("pause = %+v %v (open gates %d)", g, err, st.open())
	}
	if len(st.comments) != 1 || !strings.Contains(st.comments[0], "incident gd-abc") {
		t.Fatalf("the reason is commented on the gate: %q", st.comments)
	}
	// Pausing again opens no second gate.
	if g, err := pauseLands(ctx, st, "again", "oidc:op"); err != nil || g.Changed || st.open() != 1 {
		t.Fatalf("second pause = %+v %v (open gates %d)", g, err, st.open())
	}
	// Two gates (two operators at once) are both closed by one resume.
	st.gates = append(st.gates, &beadsapi.BeadDetail{ID: "kd-x", Type: "gate", Title: pausegate.LandsPauseTitle, Status: "open"})
	g, err = resumeLands(ctx, st, "oidc:op")
	if err != nil || !g.Changed || st.open() != 0 {
		t.Fatalf("resume = %+v %v (open gates %d)", g, err, st.open())
	}
	if g, err := resumeLands(ctx, st, "oidc:op"); err != nil || g.Changed {
		t.Fatalf("resuming unpaused lands = %+v %v", g, err)
	}
	st.listErr = errors.New("daemon down")
	if _, err := pauseLands(ctx, st, "x", "op"); err == nil {
		t.Error("an unreadable gate is an error")
	}
}

func TestPrintFreezeState(t *testing.T) {
	for _, tc := range []struct {
		s    freeze.State
		want string
	}{
		{freeze.State{Land: "tp", ProjectID: "kd-1", Frozen: true, Changed: true}, "tp frozen (kd-1)"},
		{freeze.State{Land: "tp", Frozen: true, By: "op", At: "t", Reason: "spam"}, "already frozen (by op at t: spam)"},
		{freeze.State{Land: "tp", ProjectID: "kd-1", Changed: true}, "stay parked until resumed"},
		{freeze.State{Land: "tp"}, "tp is not frozen"},
		{freeze.State{Land: "tp", Frozen: true, Changed: true, Warning: "unknown field"}, "warning: tp is frozen, but unknown field"},
	} {
		var b bytes.Buffer
		printFreezeState(&b, tc.s, map[bool]string{true: "is frozen", false: "is thawed"}[tc.s.Frozen])
		if !strings.Contains(b.String(), tc.want) {
			t.Errorf("%+v printed %q, want %q", tc.s, b.String(), tc.want)
		}
	}
}
