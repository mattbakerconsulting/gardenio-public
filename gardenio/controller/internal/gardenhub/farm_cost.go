package gardenhub

// farm_cost.go — the farm's cost blob: the purse's own figures (gd-594kYy1IYo)
// and its per-project rows (gd-T6zwDjWLBI).
//
// The purse says what the fleet spent today and over 7 days, summed over every
// project the costs rollup reports, and how much of the account budgets is
// left, read from a BudgetSource the viewer wires to the same account rollup
// /costs shows. A viewer scoped to some projects gets ForProjects' cut instead:
// its projects' rows, summed, and no account budgets.
//
// For every type:project bead the farm reports spend today and over 7 days,
// and, when the bead carries a budget (budget_usd + budget_period, gd-9Tlqjf49Sm),
// how much of it is left: remaining = budget_usd - spend over the period,
// clamped at 0. Spend comes from a SpendSource the viewer wires to the same
// costs rollup /costs shows by project, so /garden and /costs agree.
//
// The read rides the slow tier at most every farmCostEvery. A failed spend read
// is shown, never hidden: every row goes has=false with null figures and the
// time of the last good read, so a down rollup cannot look like "$0 spent".
//
// A tenant land's row also carries its purse (Lands 8.2, internal/lands/purse):
// credits granted, spent, in flight and left, and whether it is at the wall or
// unknown. A failed purse read is an unknown purse, never a full one. A land
// with a steward carries its daily run cap too (farm_steward.go).

import (
	"context"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

const (
	// farmCostEvery bounds how often project budgets and spend are re-read.
	// The spend gauge itself is refreshed about every 5 minutes.
	farmCostEvery = 60 * time.Second
	// farmCostTimeout bounds the whole cost read (one daemon list and up to
	// three rollup queries). It runs inside the farm refresh's refreshTimeout,
	// so it is kept short: a hung rollup must leave the rest of the refresh
	// (gardeners, exits) its time. A healthy read takes well under a second.
	farmCostTimeout = 3 * time.Second
)

// SpendSource reads spend per project from the costs rollup.
type SpendSource interface {
	// ProjectSpendUSD returns spend in US dollars per project over the window
	// ending now. A project with no spend in the window may be absent. An error
	// means the figures are unknown, never that nothing was spent.
	ProjectSpendUSD(ctx context.Context, window time.Duration) (map[string]float64, error)
}

// SetSpendSource wires the costs rollup. Without one the farm document has no
// cost blob and the farm makes no cost reads. A source that is also a
// BudgetSource gives the purse its account budgets. Call it before Start.
func (f *Farm) SetSpendSource(s SpendSource) { f.spend = s }

// BudgetSource reads the fleet's account budgets.
type BudgetSource interface {
	// FleetBudgetUSD sums the budgeted accounts (FarmCost.BudgetUSD says how).
	// An error means the budgets are unknown.
	FleetBudgetUSD(ctx context.Context) (FleetBudget, error)
}

// FleetBudget is the account budgets summed over the accounts that have one,
// in US dollars. Accounts is how many have one; with none the purse has no
// budget, never a $0 one.
type FleetBudget struct {
	Accounts                          int
	BudgetUSD, SpentUSD, RemainingUSD float64
}

// projectBudget is one type:project bead as the cost rows need it.
type projectBudget struct {
	name   string
	budget float64 // US dollars; 0 when period is ""
	period string  // Budget* constants; "" = no budget
	tenant bool    // land_kind=tenant: the row carries the land's purse
}

// projectSpend is one read of the rollup. ok is false when any window failed.
type projectSpend struct {
	ok                 bool
	today, week, month map[string]float64 // month is read only when a month budget needs it
}

// costState is what the last cost read saw. Only refresh touches it.
type costState struct {
	at       time.Time // last attempt
	good     time.Time // last read whose spend figures were all known
	projects []projectBudget
	budget   *FleetBudget // the last account budgets read well; nil before the first
	cost     FarmCost
	stewards map[string]*beadsapi.BeadDetail // the steward triggers last read, by land (farm_steward.go)
	notes    map[string]string               // the stewards' memory last read, by land (farm_steward.go)
	notesID  map[string]string               // the stewards' memory beads last read, by land: the land header's notes link
}

// budgetOf reads a project bead's budget. A blank, unparseable, zero or
// negative budget_usd is no budget (projectValidityReport flags the bad ones
// on the project page), as is a budget_period outside month|7d. An unset
// period is month.
func budgetOf(fields map[string]string) (float64, string) {
	raw := strings.TrimSpace(fields["budget_usd"])
	if raw == "" {
		return 0, ""
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, ""
	}
	switch strings.TrimSpace(fields["budget_period"]) {
	case "", BudgetMonth:
		return v, BudgetMonth
	case Budget7d:
		return v, Budget7d
	}
	return 0, ""
}

// readProjectBudgets lists the live type:project beads, by name. Two beads
// with one name resolve to the lowest bead id, as beadsapi.ListProjectBeads does.
func (f *Farm) readProjectBudgets(ctx context.Context) ([]projectBudget, error) {
	res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"project"}, Statuses: farmOpenStatuses, Limit: listLimit,
	})
	if err != nil {
		return nil, err
	}
	byName := map[string]*beadsapi.BeadDetail{}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(b.Title, "Project: "))
		if name == "" {
			continue
		}
		if prev, ok := byName[name]; ok && prev.ID < b.ID {
			continue
		}
		byName[name] = b
	}
	out := make([]projectBudget, 0, len(byName))
	for name, b := range byName {
		pb := projectBudget{name: name}
		pb.budget, pb.period = budgetOf(b.Fields)
		kind := strings.TrimSpace(b.Fields["land_kind"])
		pb.tenant = kind != "" && kind != beadsapi.LandKindOperator
		out = append(out, pb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// costWindows returns the trailing windows ending at now that "today" and a
// month budget cover: since 00:00 UTC, and since 00:00 UTC on the 1st.
func costWindows(now time.Time) (today, month time.Duration) {
	u := now.UTC()
	day := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	first := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return u.Sub(day), u.Sub(first)
}

// readSpend reads the rollup for every window the projects need. A zero
// window (exactly midnight) spent nothing and is not queried.
func (f *Farm) readSpend(ctx context.Context, projects []projectBudget) projectSpend {
	todayWin, monthWin := costWindows(f.now())
	read := func(w time.Duration) (map[string]float64, bool) {
		if w <= 0 {
			return map[string]float64{}, true
		}
		m, err := f.spend.ProjectSpendUSD(ctx, w)
		if err != nil {
			f.logger.Warn("garden farm: costs rollup read failed", "window", w.String(), "error", err)
			return nil, false
		}
		return m, true
	}
	sp := projectSpend{ok: true}
	var ok bool
	if sp.today, ok = read(todayWin); !ok {
		sp.ok = false
	}
	if sp.week, ok = read(7 * 24 * time.Hour); !ok {
		sp.ok = false
	}
	for _, p := range projects {
		if p.period == BudgetMonth {
			if sp.month, ok = read(monthWin); !ok {
				sp.ok = false
			}
			break
		}
	}
	return sp
}

// cents rounds a dollar amount to the cent, so float noise in the rollup does
// not send a patch.
func cents(v float64) *float64 {
	r := math.Round(v*100) / 100
	return &r
}

// sumSpend adds up a window's spend over every project the rollup reported.
func sumSpend(m map[string]float64) float64 {
	var t float64
	for _, v := range m {
		t += v
	}
	return t
}

// BudgetLevel buckets spend against a budget: BudgetLevelOK under 75%,
// BudgetLevelWarn from 75%, BudgetLevelOver at 100% or more. It is "" when
// there is no budget (budget not above 0).
func BudgetLevel(spent, budget float64) string {
	switch {
	case !(budget > 0):
		return ""
	case spent >= budget:
		return BudgetLevelOver
	case spent*100 >= 75*budget:
		return BudgetLevelWarn
	}
	return BudgetLevelOK
}

// buildFarmCost composes the fleet's figures and the per-project rows. good is
// the last time the spend was read well; it becomes the stale marker of
// figures read badly. budget is the last account budgets read, nil if none.
func buildFarmCost(projects []projectBudget, sp projectSpend, good time.Time, budget *FleetBudget) FarmCost {
	out := FarmCost{Scope: CostScopeFleet, Has: sp.ok, Projects: make([]FarmCostProject, 0, len(projects))}
	if sp.ok {
		out.TodayUSD, out.WeekUSD = cents(sumSpend(sp.today)), cents(sumSpend(sp.week))
	} else if !good.IsZero() {
		out.AsOf = good.Unix()
	}
	if budget != nil && budget.Accounts > 0 {
		out.HasBudget = true
		out.BudgetUSD, out.SpentUSD, out.RemainingUSD = cents(budget.BudgetUSD), cents(budget.SpentUSD), cents(budget.RemainingUSD)
		// The level reads the money spent out of the remaining figure, so an
		// overspent account that left the others' money untouched does not
		// say over budget beside dollars still left.
		out.Level = BudgetLevel(budget.BudgetUSD-budget.RemainingUSD, budget.BudgetUSD)
	}
	for _, p := range projects {
		e := FarmCostProject{Project: p.name, Has: sp.ok, HasBudget: p.period != ""}
		if e.HasBudget {
			e.BudgetUSD, e.Period = cents(p.budget), p.period
		}
		if !sp.ok {
			if !good.IsZero() {
				e.AsOf = good.Unix()
			}
			out.Projects = append(out.Projects, e)
			continue
		}
		e.TodayUSD, e.WeekUSD = cents(sp.today[p.name]), cents(sp.week[p.name])
		if e.HasBudget {
			spent := sp.month[p.name]
			if p.period == Budget7d {
				spent = sp.week[p.name]
			}
			e.SpentUSD, e.RemainingUSD = cents(spent), cents(math.Max(0, p.budget-spent))
			// Bucketed on the rounded figures, so the level never disagrees
			// with the amounts shown beside it.
			e.Level = BudgetLevel(*e.SpentUSD, *e.BudgetUSD)
		}
		out.Projects = append(out.Projects, e)
	}
	return out
}

// refreshCost re-reads the project budgets and their spend. A failed project
// list keeps the last projects read; a failed spend read marks every row
// has=false. rows are the farm's open work rows, for the stewards' reasons.
// It never fails the farm refresh.
func (f *Farm) refreshCost(ctx context.Context, rows []*beadsapi.BeadDetail) {
	ctx, cancel := context.WithTimeout(ctx, farmCostTimeout)
	defer cancel()
	f.costSt.at = f.now()
	if projects, err := f.readProjectBudgets(ctx); err != nil {
		f.logger.Warn("garden farm: project budget read failed", "error", err)
	} else {
		f.costSt.projects = projects
	}
	sp := f.readSpend(ctx, f.costSt.projects)
	if sp.ok {
		f.costSt.good = f.now()
	}
	f.readBudget(ctx)
	f.costSt.cost = buildFarmCost(f.costSt.projects, sp, f.costSt.good, f.costSt.budget)
	f.attachPurses(ctx, f.costSt.projects, &f.costSt.cost)
	f.attachStewards(ctx, &f.costSt.cost, rows)
}

// readBudget re-reads the account budgets when the spend source can. A failed
// read keeps the last budgets read: they come from the account beads, which
// spend_sync stamps every few minutes, not from the rollup.
func (f *Farm) readBudget(ctx context.Context) {
	bs, ok := f.spend.(BudgetSource)
	if !ok {
		return
	}
	b, err := bs.FleetBudgetUSD(ctx)
	if err != nil {
		f.logger.Warn("garden farm: account budget read failed", "error", err)
		return
	}
	f.costSt.budget = &b
}

// ForProjects is the cut of the purse a viewer scoped to the projects allow
// admits sees (decision gd-QPIHOFzCXc): only their rows, in the blob's order,
// with today and week summed over them, and no account budgets, which are the
// fleet's. Each row keeps its own budget and remaining; those are never summed.
//
// A tenant land's row is cut to credits (Purse.CreditsOnly, every at-cost
// dollar figure and the level bucketed on them dropped) and is left out of
// the sums, so a tenant never sees
// the fleet's dollars through its own land. has is false when a row in dollars
// has no figures, as_of then the oldest stale marker among them, or when no
// row is in dollars. A cut with no rows at all is the bare {"projects":[]}.
func (c FarmCost) ForProjects(allow func(string) bool) FarmCost {
	out := FarmCost{Projects: []FarmCostProject{}}
	for _, p := range c.Projects {
		if !allow(p.Project) {
			continue
		}
		if p.Purse != nil {
			credits := p.Purse.CreditsOnly()
			p.Purse = &credits
			p.HasBudget, p.BudgetUSD, p.Period, p.Level = false, nil, "", ""
			p.TodayUSD, p.WeekUSD, p.SpentUSD, p.RemainingUSD = nil, nil, nil, nil
		}
		out.Projects = append(out.Projects, p)
	}
	if len(out.Projects) == 0 {
		return out
	}
	out.Scope = CostScopeProject
	var today, week float64
	dollars := 0
	ok := true
	for _, p := range out.Projects {
		if p.Purse != nil {
			continue
		}
		dollars++
		if !p.Has || p.TodayUSD == nil || p.WeekUSD == nil {
			ok = false
			if p.AsOf != 0 && (out.AsOf == 0 || p.AsOf < out.AsOf) {
				out.AsOf = p.AsOf
			}
			continue
		}
		today += *p.TodayUSD
		week += *p.WeekUSD
	}
	if out.Has = ok && dollars > 0; out.Has {
		out.TodayUSD, out.WeekUSD = cents(today), cents(week)
	}
	return out
}

// attachPurses reads every tenant land's purse onto its row. Operator lands
// get none. A failed read makes each tenant purse unknown, which blocks like
// the wall; it never fails the farm refresh.
func (f *Farm) attachPurses(ctx context.Context, projects []projectBudget, cost *FarmCost) {
	var lands []string
	for _, p := range projects {
		if p.tenant {
			lands = append(lands, p.name)
		}
	}
	if len(lands) == 0 {
		return
	}
	now := f.now()
	got, err := purse.ReadMany(ctx, f.src, lands, now)
	if err != nil {
		f.logger.Warn("garden farm: land purse read failed", "error", err)
	}
	for i := range cost.Projects {
		row := &cost.Projects[i]
		if !slices.Contains(lands, row.Project) {
			continue
		}
		p, ok := got[row.Project]
		if !ok {
			// Not err: it is the batch's, and can name another land and the daemon ("reading <land>'s credit ledger:
			// ..."), while this reason reaches the land's tenant (CreditsOnly keeps it). It is logged above.
			p = purse.Unreadable(row.Project, nil, now)
		}
		row.Purse = &p
	}
}
