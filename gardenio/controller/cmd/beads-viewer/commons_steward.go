package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/steward"
	"gardenio/controller/internal/steward/gardenerwatch"
)

// commonsStewards is the Commons' steward node read (commons.Stewards, Commons v2 steward view S1, gd-FsDEc7ZXGl): the
// land's steward trigger and steward.LoadLand on the viewer's bearer (the request context), its live run, and the
// head's day (runs, cap, spend, reason) from the farm's cost row, which costs no read. Nothing new is stored.
type commonsStewards struct {
	src  purse.Source
	farm *gardenhub.Farm // nil: the head has no spend and no reason
	now  func() time.Time
}

// stewardRunStates are the agent states of a run at work (steward's running).
var stewardRunStates = []string{"spawning", "assigning", "idle", "working", "yielding", "rate_limited", "model_rate_limited", "resuming"}

// Steward is land's steward, the home garden's for land "". nil when the land has none.
func (s commonsStewards) Steward(ctx context.Context, land string) (*commons.StewardLand, error) {
	now := s.now()
	if land == "" {
		home, err := s.home(ctx)
		if err != nil || home == "" {
			return nil, err
		}
		land = home
	}
	trig, err := steward.FindSteward(ctx, s.src, land)
	if err != nil || trig == nil {
		return nil, err
	}
	st, err := steward.LoadLand(ctx, s.src, land, now, time.Time{})
	if err != nil {
		return nil, err
	}
	l := st.Land
	out := &commons.StewardLand{ID: trig.ID, Name: land + "-" + steward.Role, Land: land, Home: l.Home, General: st.Channels.General,
		NotesID: l.NotesID, Notes: l.Notes}
	if out.Run, err = s.run(ctx, land); err != nil {
		return nil, err
	}
	day, _ := gardenhub.StewardRunCap(trig.Fields["gardener"], now)
	out.RunsToday, out.MaxRunsPerDay = day.RunsToday, day.MaxRunsPerDay
	if s.farm != nil {
		if row := s.farm.ProjectCost(land); row != nil {
			out.Spend = stewardSpend(row)
			if row.Steward != nil && row.Steward.Reason != nil {
				out.Reason = row.Steward.Reason.Text
			}
		}
	}
	switch {
	case out.Run != "":
		out.State = commons.StewardWorking
	case day.Capped:
		out.State = commons.StewardResting
	case l.Purse.State == purse.StateWall:
		out.State = commons.StewardParked
	default:
		out.State = commons.StewardIdle
	}
	stalled := map[string]gardenerwatch.StalledPlan{}
	for _, p := range l.Stalled {
		stalled[p.ID] = p
	}
	for _, p := range l.OpenPlans {
		sp, isStalled := stalled[p.ID]
		out.Plans = append(out.Plans, commons.StewardPlan{ID: p.ID, Title: p.Title, State: p.State, Stalled: isStalled, Why: stalledWhy(sp)})
	}
	for _, p := range l.ParkedSteps {
		out.Parked = append(out.Parked, commons.StewardStep{ID: p.ID, Title: p.Title, Agent: p.Agent})
	}
	return out, nil
}

// run is the steward's live run in land, its agent bead, "" between runs.
func (s commonsStewards) run(ctx context.Context, land string) (string, error) {
	res, err := s.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land}, Statuses: []string{"open", "in_progress", "blocked"},
		Sort: "-updated_at", Limit: 200,
	})
	if err != nil {
		return "", fmt.Errorf("reading %s's steward run: %w", land, err)
	}
	for _, b := range res.Beads {
		if b != nil && wall.IsSteward(b.Fields["role"]) && slices.Contains(stewardRunStates, b.Fields["agent_state"]) {
			return b.ID, nil
		}
	}
	return "", nil
}

// home is the home garden: the land of a steward trigger that is not a tenant land and whose project bead is the home
// garden's (steward.HomeOwner). "" when there is none.
func (s commonsStewards) home(ctx context.Context) (string, error) {
	res, err := s.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"trigger"}, Labels: []string{steward.MarkerLabel}, Statuses: []string{"open", "in_progress", "blocked"},
	})
	if err != nil {
		return "", fmt.Errorf("looking for the home garden's steward: %w", err)
	}
	for _, b := range res.Beads {
		land := ""
		for _, l := range b.Labels {
			if v, ok := strings.CutPrefix(l, steward.LandLabelPrefix); ok {
				land = v
			}
		}
		if land == "" || (s.farm != nil && s.farm.IsTenantLand(land)) {
			continue
		}
		if proj, tenant, err := purse.Project(ctx, s.src, land); err == nil && !tenant && steward.HomeOwner(proj) != "" {
			return land, nil
		}
	}
	return "", nil
}

// stewardSpend is the land's day as its header says it: a tenant land's purse in credits (never at cost), else the
// day's spend.
func stewardSpend(row *gardenhub.FarmCostProject) string {
	if p := row.Purse; p != nil {
		if p.RemainingCredits != nil {
			return fmt.Sprintf("%.0f credits left", *p.RemainingCredits)
		}
		return ""
	}
	if row.TodayUSD != nil {
		return fmt.Sprintf("$%.2f spent today", *row.TodayUSD)
	}
	return ""
}

// stalledWhy is how a stalled plan's gardener stopped, in a few words.
func stalledWhy(p gardenerwatch.StalledPlan) string {
	switch {
	case p.ID == "":
		return ""
	case p.Gardener == "":
		return "no gardener"
	case p.Kind == gardenerwatch.KindPaused:
		return "gardener paused"
	case p.Kind != "":
		return "gardener stopped: " + strings.ReplaceAll(p.Kind, "_", " ")
	}
	return "gardener closed"
}
