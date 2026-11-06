package steward

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// The step cost estimate (spec §4.2 item 3, step 9.5): what one gardener
// step is expected to cost, in credits, before the steward spawns it. It is
// the mean settled cost of the land's last EstimateSessions gardener
// sessions, or DefaultStepEstimateCredits before the land has that many.
//
// A session here is one lands-gardener agent bead: one spawn on one step,
// which is what the steward weighs against the purse. Its settled cost is the
// controller's spend settlement on the closed agent bead: total_cost_usd,
// written with the litellm_spend_close watermark (cmd/controller
// reconcile_session_spend.go). A closed bead without the watermark has not
// settled yet and is not counted, so a figure the in-pod estimate wrote never
// moves the mean.

const (
	// EstimateSessions is how many settled gardener sessions the mean takes.
	EstimateSessions = 10
	// settledField marks an agent bead whose spend the controller settled.
	settledField = "litellm_spend_close"
	// estimateScan caps the land's closed gardener beads read, newest first.
	estimateScan = 200
)

// Estimate is a step cost estimate and where it came from.
type Estimate struct {
	// Credits is what one step is expected to cost.
	Credits float64 `json:"step_estimate_credits"`
	// Sessions is how many settled gardener sessions the mean took; 0 when
	// Credits is the default.
	Sessions int `json:"step_estimate_sessions"`
}

// DefaultEstimate is the estimate before a land has EstimateSessions settled
// gardener sessions.
func DefaultEstimate() Estimate { return Estimate{Credits: DefaultStepEstimateCredits} }

// StepEstimate reads a land's step cost estimate. multiplier is the
// llm_multiplier of the rate in force (credits per US$1 of settled spend);
// without one (no rate in force, or a purse that could not be read) the mean
// cannot be priced, so the default stands.
func StepEstimate(ctx context.Context, src purse.Source, land string, multiplier float64) (Estimate, error) {
	if multiplier <= 0 {
		return DefaultEstimate(), nil
	}
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land, "role:" + GardenerRole},
		Statuses: []string{"closed"}, Sort: "-closed_at", Limit: estimateScan,
	})
	if err != nil {
		return DefaultEstimate(), fmt.Errorf("reading %s's settled gardeners: %w", land, err)
	}
	var settled []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.Type != "agent" || b.Status != "closed" || strings.TrimSpace(b.Fields[settledField]) == "" {
			continue
		}
		if p := strings.TrimSpace(b.Fields["project"]); p != "" && p != land {
			continue
		}
		if !slices.Contains(parseRoles(b.Fields["role"]), GardenerRole) {
			continue
		}
		settled = append(settled, b)
	}
	// Newest settled first, whatever order the daemon returned.
	slices.SortStableFunc(settled, func(a, b *beadsapi.BeadDetail) int {
		return closedAt(b).Compare(closedAt(a))
	})
	if len(settled) < EstimateSessions {
		return DefaultEstimate(), nil
	}
	var sum float64
	for _, b := range settled[:EstimateSessions] {
		sum += math.Max(0, purse.FieldFloat(b.Fields, "total_cost_usd"))
	}
	credits := math.Round(sum/EstimateSessions*multiplier*100) / 100
	return Estimate{Credits: credits, Sessions: EstimateSessions}, nil
}

// parseRoles splits a comma-joined agent role.
func parseRoles(role string) []string {
	var out []string
	for r := range strings.SplitSeq(role, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// LandStepEstimate is StepEstimate priced at the rate the land's purse reads
// now. A purse or history that cannot be read gives the default; an unmetered
// land (the home garden) has no estimate, zero.
func LandStepEstimate(ctx context.Context, src purse.Source, land string, now time.Time) Estimate {
	p := ReadPurse(ctx, src, land, now)
	if p.IsUnmetered() {
		return Estimate{}
	}
	e, _ := StepEstimate(ctx, src, land, p.LLMMultiplier)
	return e
}
