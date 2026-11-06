package gardenhub

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	"gardenio/controller/internal/grafanaembed"
	"gardenio/controller/internal/lands/purse"
)

// A tenant land's credit charts (plan gd-gh5J0vVbJP step 3.3, gd-kSG5BqJMRo; gardenio/docs/garden-grafana-panels.md,
// "Tenant isolation (3.1)"): GET /garden/charts/costs-land?land=<name>&range= answers one land's credits as JSON, drawn
// by the Costs panel as SVG (web/credits.js). There is no Grafana in this path. The viewer classes the route
// routeScoped; its cut is InLand, checked before anything is read, so a land outside the viewer's scope is a 404, the
// same as a land that does not exist (the BeadFeedHub.eventVisible and Glass InScope pattern). An operator land has no
// purse and is a 404 too.
//
// Credits only, by construction: no type below has a dollar or a URL field. The tiles and the meter take the purse the
// farm read (Purse.CreditsOnly, the same figures as the land header); the history comes from the 3.2 recording rules
// (gardenio:land_spend_credits:*, gardenio:land_credits:*) through Credits, which only the land's validated name reaches.

// LandCreditSource reads one land's recorded credit series (the viewer's landCreditSource, over vmsingle). land is a
// DNS label the scope admits; w is one of the chips. It answers ErrCreditsUnavailable when there is no metrics backend
// or a query fails; a land with no series yet answers a history with Recorded false.
type LandCreditSource interface {
	LandCredits(ctx context.Context, land string, w CreditWindow) (CreditHistory, error)
}

// ErrCreditsUnavailable: the credit history cannot be read right now. The figures are unknown, never 0.
var ErrCreditsUnavailable = errors.New("credit history unavailable")

// CreditWindow is one range chip as a range query: [Start, End] every Step.
type CreditWindow struct {
	Name       string
	Start, End time.Time
	Step       time.Duration
}

// creditWindow is the chip's window ending at now: today is since 00:00 UTC every 5 minutes (the :today rule's own
// step), a week hourly and 30 days (the retention) every 6 hours, so no chip draws more than 288 points.
func creditWindow(name string, now time.Time) CreditWindow {
	now = now.UTC()
	switch name {
	case "today":
		start := now.Truncate(24 * time.Hour)
		if !start.Before(now) {
			start = now.Add(-5 * time.Minute)
		}
		return CreditWindow{Name: name, Start: start, End: now, Step: 5 * time.Minute}
	case "30d":
		return CreditWindow{Name: name, Start: now.Add(-30 * 24 * time.Hour), End: now, Step: 6 * time.Hour}
	}
	return CreditWindow{Name: "7d", Start: now.Add(-7 * 24 * time.Hour), End: now, Step: time.Hour}
}

// CreditHistory is what the recorded series say about one land, every figure in credits.
type CreditHistory struct {
	// Recorded is false when the land has no credit series at all (no rate in force yet): "No credit history yet".
	Recorded bool
	// Today, Week and Month are :today, :increase7d and :increase30d; nil when that series has no sample.
	Today, Week, Month *float64
	// RatePerHour is :rate1d, the trailing day's mean burn.
	RatePerHour *float64
	// Remaining is :remaining (granted − spent, settled) over the window, oldest first.
	Remaining []CreditPoint
	// Agents is :by_agent, one row per agent, in any order.
	Agents []AgentCredits
}

// CreditPoint is one sample: Unix seconds and credits.
type CreditPoint [2]float64

// AgentCredits is one agent's credits in the spend-logs gauge's ~48 h slice.
type AgentCredits struct {
	Agent   string  `json:"agent"`
	Credits float64 `json:"credits"`
}

// Credit history states (landCosts.History).
const (
	creditHistoryOK          = "ok"
	creditHistoryNone        = "none"
	creditHistoryUnavailable = "unavailable"
)

const (
	// landAgentsTop is how many agents the per-agent card names; the rest are one row.
	landAgentsTop = 10
	// runOutHorizon is how far ahead a run-out is told; further than that it is "not in sight".
	runOutHorizon = 365 * 24 * time.Hour
	// landCreditsTimeout bounds the history read, as the farm's spend read is bounded.
	landCreditsTimeout = 5 * time.Second
)

// landCosts is the route's document.
type landCosts struct {
	Land  string `json:"land"`
	Range string `json:"range"`
	// History is ok, none (no series yet) or unavailable (the read failed); only ok carries Spent, Burn's line and Agents.
	History string         `json:"history"`
	Purse   landPurse      `json:"purse"`
	Spent   *landSpent     `json:"spent"`
	Burn    landBurn       `json:"burn"`
	Agents  []AgentCredits `json:"agents"`
	// Rest is the agents after the top landAgentsTop, summed; nil when there are none.
	Rest *landAgentsRest `json:"rest,omitempty"`
}

// landPurse is the purse as the land header tells it: Purse.CreditsOnly, field by field, so nothing at cost can ride
// along.
type landPurse struct {
	State     string   `json:"state"`
	Reason    string   `json:"reason,omitempty"`
	Granted   float64  `json:"granted_credits"`
	Spent     *float64 `json:"spent_credits"`
	InFlight  *float64 `json:"in_flight_credits"`
	Remaining *float64 `json:"remaining_credits"`
	Margin    float64  `json:"margin_credits"`
	UsedPct   *float64 `json:"used_pct"`
	ReadAt    string   `json:"read_at"`
}

type landSpent struct {
	Today *float64 `json:"today"`
	Week  *float64 `json:"week"`
	Month *float64 `json:"month"`
}

// landBurn is the burn-to-the-wall card: the recorded remaining over the window, the burn rate, the wall (the margin)
// and when the land reaches it at that rate.
type landBurn struct {
	Start  int64         `json:"start"`
	End    int64         `json:"end"`
	Points []CreditPoint `json:"points"`
	Rate   *float64      `json:"rate_per_hour"`
	Wall   float64       `json:"wall"`
	// RunOut is when the remaining reaches the wall at Rate (Unix seconds; End when it is there already), nil with no
	// burn or none within runOutHorizon.
	RunOut *int64 `json:"run_out"`
}

type landAgentsRest struct {
	Count   int     `json:"count"`
	Credits float64 `json:"credits"`
}

// landCostsPanel serves GET /garden/charts/costs-land. The order is the isolation: the land's shape, then the scope,
// then the farm's word that it is a tenant land, and only then any read. A range outside the chips is a 400.
func (h *Handler) landCostsPanel(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	land := q.Get("land")
	if !grafanaembed.ValidProject(land) || h.InLand == nil || !h.InLand(r, land) {
		http.Error(w, "no such land", http.StatusNotFound)
		return
	}
	var row *FarmCostProject
	if h.Farm != nil {
		row = h.Farm.ProjectCost(land)
	}
	if row == nil || row.Purse == nil || row.Purse.IsUnmetered() {
		http.Error(w, "no such land", http.StatusNotFound)
		return
	}
	rng, err := grafanaembed.ParseRange(q.Get("range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	win := creditWindow(rng.Name, time.Now())
	var got CreditHistory
	err = ErrCreditsUnavailable
	if h.Credits != nil {
		ctx, cancel := context.WithTimeout(r.Context(), landCreditsTimeout)
		got, err = h.Credits.LandCredits(ctx, land, win)
		cancel()
	}
	writeDoc(w, "charts", buildLandCosts(land, win, row.Purse.CreditsOnly(), got, err))
}

// buildLandCosts composes the document from the purse and what the history read answered (err non-nil: unavailable).
func buildLandCosts(land string, win CreditWindow, p purse.Purse, h CreditHistory, err error) landCosts {
	d := landCosts{
		Land: land, Range: win.Name, History: creditHistoryOK, Agents: []AgentCredits{},
		Purse: landPurse{
			State: p.State, Reason: p.Reason, Granted: p.GrantedCredits, Spent: p.SpentCredits, InFlight: p.InFlightCredits,
			Remaining: p.RemainingCredits, Margin: p.MarginCredits, UsedPct: p.UsedPct, ReadAt: p.ReadAt,
		},
		Burn: landBurn{Start: win.Start.Unix(), End: win.End.Unix(), Points: []CreditPoint{}, Wall: p.MarginCredits},
	}
	switch {
	case err != nil:
		d.History = creditHistoryUnavailable
		return d
	case !h.Recorded:
		d.History = creditHistoryNone
		return d
	}
	d.Spent = &landSpent{Today: h.Today, Week: h.Week, Month: h.Month}
	if h.Remaining != nil {
		d.Burn.Points = h.Remaining
	}
	d.Burn.Rate = h.RatePerHour
	// The projection starts from the live remaining (the purse also takes off what is in flight), else the last point.
	from := p.RemainingCredits
	if from == nil && len(h.Remaining) > 0 {
		from = &h.Remaining[len(h.Remaining)-1][1]
	}
	d.Burn.RunOut = runOut(from, p.MarginCredits, h.RatePerHour, win.End)
	d.Agents, d.Rest = rankAgents(h.Agents)
	return d
}

// runOut is when remaining reaches the wall at ratePerHour from now: now itself when it is at the wall already, nil
// with nothing to project from, no burn, or a run-out past runOutHorizon.
func runOut(remaining *float64, wall float64, ratePerHour *float64, now time.Time) *int64 {
	if remaining == nil || ratePerHour == nil || *ratePerHour <= 0 {
		return nil
	}
	t := now.Unix()
	if *remaining > wall {
		hours := (*remaining - wall) / *ratePerHour
		if math.IsNaN(hours) || hours*float64(time.Hour) > float64(runOutHorizon) {
			return nil
		}
		t += int64(math.Ceil(hours * 3600))
	}
	return &t
}

// rankAgents is the per-agent card's rows: the top landAgentsTop by credits (a tie by name), and the rest summed.
// An agent with no credits is left out.
func rankAgents(in []AgentCredits) ([]AgentCredits, *landAgentsRest) {
	rows := make([]AgentCredits, 0, len(in))
	for _, a := range in {
		if a.Agent != "" && a.Credits > 0 {
			rows = append(rows, a)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Credits != rows[j].Credits {
			return rows[i].Credits > rows[j].Credits
		}
		return rows[i].Agent < rows[j].Agent
	})
	if len(rows) <= landAgentsTop {
		return rows, nil
	}
	rest := &landAgentsRest{Count: len(rows) - landAgentsTop}
	for _, a := range rows[landAgentsTop:] {
		rest.Credits += a.Credits
	}
	return rows[:landAgentsTop], rest
}
