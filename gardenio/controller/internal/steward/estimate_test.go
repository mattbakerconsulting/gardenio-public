package steward

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// settledGardener is a closed lands-gardener of the land whose spend the
// controller settled at cost usd, closed n minutes after the base time.
func settledGardener(n int, usd string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: fmt.Sprintf("tp-g%02d", n), Type: "agent", Status: "closed",
		Labels:   []string{"project:" + land, "role:" + GardenerRole},
		ClosedAt: time.Date(2026, 10, 20, 0, n, 0, 0, time.UTC).Format(time.RFC3339),
		Fields:   map[string]string{"project": land, "role": GardenerRole, "total_cost_usd": usd, settledField: "12.5"},
	}
}

func TestStepEstimate(t *testing.T) {
	ctx := context.Background()
	d := newFarm()
	// Nine settled sessions: still the default.
	for i := 1; i <= 9; i++ {
		d.beads = append(d.beads, settledGardener(i, "100"))
	}
	if e, err := StepEstimate(ctx, d, land, 1.5); err != nil || e != DefaultEstimate() || !e.Default() {
		t.Fatalf("nine settled = %+v, %v; want the default", e, err)
	}
	// Ones that do not count: unsettled, another land, another role.
	unsettled := settledGardener(30, "1000")
	delete(unsettled.Fields, settledField)
	other := settledGardener(31, "1000")
	other.Fields["project"] = "other-land"
	crew := settledGardener(32, "1000")
	crew.Fields["role"] = "steward"
	d.beads = append(d.beads, unsettled, other, crew)
	if e, _ := StepEstimate(ctx, d, land, 1.5); !e.Default() {
		t.Fatalf("unsettled, foreign and steward beads counted: %+v", e)
	}
	// The tenth, and two newer: the mean of the newest ten (eight at $100 and
	// two at $2 → $80.40), at x1.5.
	d.beads = append(d.beads, settledGardener(10, "100"), settledGardener(11, "2"), settledGardener(12, "2"))
	e, err := StepEstimate(ctx, d, land, 1.5)
	if err != nil || e.Sessions != EstimateSessions || e.Credits != 120.6 {
		t.Fatalf("estimate = %+v, %v; want 120.60 credits over 10 sessions", e, err)
	}
	// With no rate in force the mean cannot be priced.
	if e, _ := StepEstimate(ctx, d, land, 0); !e.Default() {
		t.Errorf("no multiplier = %+v, want the default", e)
	}
}

type failingSource struct{}

func (failingSource) ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return nil, errors.New("daemon down")
}

func TestStepEstimate_UnreadableIsTheDefault(t *testing.T) {
	e, err := StepEstimate(context.Background(), failingSource{}, land, 1.5)
	if err == nil || !e.Default() {
		t.Errorf("unreadable = %+v, %v; want the default and an error", e, err)
	}
}
