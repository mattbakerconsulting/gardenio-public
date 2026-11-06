package gardenhub

// farm_gardener_cost.go — gardeners[].cost on the farm document
// (gd-0VeG81uW3w): what each gardener has spent, so the Porch and the
// gardener card can show it and the per-viewer cut can keep it to the
// viewer's projects (by FarmGardener.Project).
//
// The agent's own figure is beads-viewer's agentSpendUSD over the agent bead
// the farm already read: total_cost_usd plus, while alive, live_cost_usd.
// The live part is a lower bound: on the LiteLLM path live_cost_usd stays 0
// until the session closes (gd-UIlh0DR2VG), so the words say "at least" and
// a 0 there is not sent as a figure. It is floored to gardenerLiveStepUSD so
// a turn does not re-send the gardener.
//
// A gardener's run (an agent a trigger spawned) also carries its trigger's
// gardener_spend_usd, the controller's rollup over every run of that gardener
// and the crew they sent (cmd/controller/gardener_spend_sync.go). That is the
// one read here the agent list does not answer: one BatchGetBeads of the
// triggers named, re-read at most every farmRunSpendEvery.

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// gardenerLiveStepUSD is the step the running session's cost is floored
	// to: a working agent spends about this much a minute, so its gardener
	// upserts about once a minute, as it does for since.
	gardenerLiveStepUSD = 0.50
	// farmRunSpendEvery bounds how often the triggers' rollup is re-read. The
	// controller rewrites it every 10 minutes.
	farmRunSpendEvery = 5 * time.Minute
	// maxRunSpendReads bounds the triggers one read asks for.
	maxRunSpendReads = 50
)

// runSpendState is what the last trigger read saw. Only refresh touches it.
type runSpendState struct {
	at  time.Time           // last full read
	usd map[string]*float64 // by trigger id; nil = read, no figure
}

// gardenerRun is the trigger whose run an agent bead is: trigger_id, else the
// legacy schedule_id. "" for any other agent.
func gardenerRun(b *beadsapi.BeadDetail) string {
	return strings.TrimSpace(firstNonEmpty(b.Fields["trigger_id"], b.Fields["schedule_id"]))
}

// usdField parses a dollar field. A blank, unparseable, negative or zero one
// is not known.
func usdField(fields map[string]string, k string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(fields[k]), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, false
	}
	return v, true
}

// gardenerCost is a gardener's cost from its agent bead's fields, its bucket,
// and its trigger's rollup (nil when it is no gardener's run or the rollup is
// not known). It is nil when nothing is known.
func gardenerCost(fields map[string]string, bucket string, run *float64) *FarmGardenerCost {
	c := &FarmGardenerCost{AtLeast: bucket != GardenerDormant, GardenerUSD: run}
	spent, known := usdField(fields, "total_cost_usd")
	if live, ok := usdField(fields, "live_cost_usd"); ok && bucket == GardenerAlive {
		if step := math.Floor(live/gardenerLiveStepUSD) * gardenerLiveStepUSD; step > 0 {
			c.LiveUSD = cents(step)
			spent, known = spent+step, true
		}
	}
	if known {
		c.SpentUSD = cents(spent)
	} else if !c.AtLeast && run == nil {
		return nil
	}
	c.Text = gardenerCostText(c)
	return c
}

// gardenerCostText is the cost's words.
func gardenerCostText(c *FarmGardenerCost) string {
	var s string
	switch {
	case c.SpentUSD != nil && c.AtLeast:
		s = "Spent at least " + usd(*c.SpentUSD) + "; this session is counted in full when it ends."
	case c.SpentUSD != nil:
		s = "Spent " + usd(*c.SpentUSD) + "."
	case c.AtLeast:
		s = "Nothing counted yet; this session is counted when it ends."
	}
	if c.GardenerUSD != nil {
		lead := "Its gardener has spent "
		if c.AtLeast {
			lead += "at least "
		}
		s = strings.TrimSpace(s + " " + lead + usd(*c.GardenerUSD) + " over all its runs and their crew.")
	}
	return s
}

// usd is "$12.34", as fmtUSD in beads-viewer.
func usd(v float64) string { return "$" + strconv.FormatFloat(v, 'f', 2, 64) }

// readRunSpend reads the spend rollup of the triggers the gardeners among
// agents are runs of: every one named, at most every farmRunSpendEvery, and a
// trigger not yet read at once. Triggers no gardener names any more are
// dropped. A failed read keeps what was read before; it never fails the
// refresh.
func (f *Farm) readRunSpend(ctx context.Context, agents []*beadsapi.BeadDetail) {
	want := map[string]bool{}
	for _, b := range agents {
		if isGardener(b) {
			if run := gardenerRun(b); run != "" {
				want[run] = true
			}
		}
	}
	for id := range f.runSpend.usd {
		if !want[id] {
			delete(f.runSpend.usd, id)
		}
	}
	full := f.runSpend.at.IsZero() || f.now().Sub(f.runSpend.at) >= farmRunSpendEvery
	var ask []string
	for id := range want {
		if _, ok := f.runSpend.usd[id]; full || !ok {
			ask = append(ask, id)
		}
	}
	if len(ask) == 0 {
		return
	}
	sort.Strings(ask)
	if len(ask) > maxRunSpendReads {
		ask = ask[:maxRunSpendReads]
	}
	got, err := f.src.BatchGetBeads(ctx, ask)
	if err != nil {
		f.logger.Warn("garden farm: reading the gardeners' spend failed", "error", err)
		return
	}
	if full {
		f.runSpend.at = f.now()
	}
	for _, id := range ask {
		var v *float64
		if b := got[id]; b != nil {
			if spent, ok := usdField(b.Fields, "gardener_spend_usd"); ok {
				v = cents(spent)
			}
		}
		f.runSpend.usd[id] = v
	}
}

// runSpendOf is a trigger's rollup as last read, nil when not known.
func (f *Farm) runSpendOf(id string) *float64 {
	if id == "" {
		return nil
	}
	return f.runSpend.usd[id]
}
