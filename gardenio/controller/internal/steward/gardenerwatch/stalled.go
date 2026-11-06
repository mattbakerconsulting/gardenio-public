package gardenerwatch

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler/conditions"
)

// Gardener-less plans (gd-hVTkwcDwxK). In the home garden each building plan
// is run by its own plan gardener, a trigger labelled gardener-epic:<plan>.
// When that trigger stops (a budget stop, a run of refused spawns) or was
// never bound, nothing runs the plan's steps and nothing says so: on 10-31
// 18 of 26 building plans had no live gardener, unnoticed for hours to days.
// Every wake the home steward is shown each one, with how its last gardener
// stopped and what is left to do, and the ask that follows from it. The home
// steward acts on it itself (gb plan rearm <plan>, else gb plan gardener
// <plan>; kbeads exempts the home garden's steward from the lands-tenant
// table, 11-02); it tells the owner only what it cannot do.

// StalledAsk is what a gardener-less plan calls for.
type StalledAsk string

const (
	// AskComplete: no open step is left; the owner completes the plan.
	AskComplete StalledAsk = "complete"
	// AskRebind: steps are ready and the gardener stopped on a budget, was
	// refused its spawns, was paused, or was never bound; the owner rebinds.
	AskRebind StalledAsk = "rebind"
	// AskWaits: every open step waits on something (a decision, a task);
	// a gardener would have nothing to do, so the owner is told what.
	AskWaits StalledAsk = "waits"
	// AskTell: the gardener was stopped some other way (by hand, a failure
	// that is not a refusal); the owner is told, not asked to rebind.
	AskTell StalledAsk = "tell"
)

// KindPaused is StalledPlan.Kind for an open gardener that is not enabled.
const KindPaused = conditions.GardenerlessPaused

// StalledPlan is a building plan of the home garden with no live gardener.
type StalledPlan struct {
	ID    string
	Title string
	// Gardener is its newest gardener trigger, "" when it never had one.
	Gardener string
	// Kind is how that gardener stopped (its gardener_terminated_kind),
	// KindPaused for an open one that is not enabled, "" when not recorded.
	Kind string
	// Error is the first line of the gardener's last_fire_error, cut.
	Error string
	// OpenSteps are its open steps; ReadySteps those waiting on nothing.
	OpenSteps  int
	ReadySteps int
	Ask        StalledAsk
}

const (
	// StalledShown caps the gardener-less plans in the prompt; the step
	// counts are read for these only.
	StalledShown = 20
	// stalledErrorChars cuts a gardener's fire error in the prompt.
	stalledErrorChars = 160
	// gardenerScan caps the open gardener triggers read.
	gardenerScan = 1000
	// openStepsScan caps a plan's open steps read.
	openStepsScan = 1000
)

// BudgetStopKinds are the gardener stops that end a loop on its budget, not on
// a fault: a fresh gardener picks the plan up where the last one stopped.
// The quiet_gardeners leaf classifies with the same list.
var BudgetStopKinds = conditions.GardenerlessBudgetStops

// Stalled lists the land's building plans with no open, enabled gardener,
// by plan id; more counts those past StalledShown.
func Stalled(ctx context.Context, src conditions.BeadClient, plans []conditions.QuietPlanRef) (out []StalledPlan, more int, err error) {
	building := slices.Clone(plans)
	if len(building) == 0 {
		return nil, 0, nil
	}
	open, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"trigger", "schedule"}, Statuses: []string{"open", "in_progress", "blocked"}, Limit: gardenerScan,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("reading the plan gardeners: %w", err)
	}
	slices.SortFunc(building, func(a, b conditions.QuietPlanRef) int { return strings.Compare(a.ID, b.ID) })
	for _, p := range building {
		var paused *beadsapi.BeadDetail
		live := false
		for _, b := range open.Beads {
			if b == nil || b.Status == "closed" || !plangardener.IsGardenerForEpic(b, p.ID) {
				continue
			}
			if strings.TrimSpace(b.Fields["enabled"]) != "false" {
				live = true
				break
			}
			paused = b
		}
		if live {
			continue
		}
		if len(out) == StalledShown {
			more++
			continue
		}
		s := StalledPlan{ID: p.ID, Title: p.Title}
		g := paused
		if g != nil {
			s.Kind = KindPaused
		} else if g, err = LastGardener(ctx, src, p.ID); err != nil {
			return nil, 0, err
		} else if g != nil {
			s.Kind = strings.TrimSpace(g.Fields["gardener_terminated_kind"])
		}
		if g != nil {
			s.Gardener = g.ID
			s.Error = firstLine(g.Fields["last_fire_error"], stalledErrorChars)
		}
		if s.OpenSteps, s.ReadySteps, err = planStepCounts(ctx, src, p.ID); err != nil {
			return nil, 0, err
		}
		s.Ask = stalledAsk(s)
		out = append(out, s)
	}
	return out, more, nil
}

// LastGardener is the plan's gardener trigger that closed last, or nil.
func LastGardener(ctx context.Context, src conditions.BeadClient, planID string) (*beadsapi.BeadDetail, error) {
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"trigger", "schedule"}, Labels: []string{plangardener.GardenerEpicLabel(planID)}, Statuses: []string{"closed"},
	})
	if err != nil {
		return nil, fmt.Errorf("reading plan %s's last gardener: %w", planID, err)
	}
	var last *beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.Status != "closed" || !plangardener.IsGardenerForEpic(b, planID) {
			continue
		}
		if last == nil || closedAt(b).After(closedAt(last)) {
			last = b
		}
	}
	return last, nil
}

// planStepCounts counts a plan's open steps, and those of them ready: not
// blocked, and waiting on no open bead or decision.
func planStepCounts(ctx context.Context, src conditions.BeadClient, planID string) (open, ready int, err error) {
	q := beadsapi.ListBeadsQuery{
		Types: []string{"task", "bug"}, Parent: planID, Statuses: []string{"open", "in_progress", "blocked"}, Limit: openStepsScan,
	}
	all, err := src.ListBeadsFiltered(ctx, q)
	if err != nil {
		return 0, 0, fmt.Errorf("reading plan %s's open steps: %w", planID, err)
	}
	q.Statuses, q.NoOpenDeps = []string{"open", "in_progress"}, true
	unblocked, err := src.ListBeadsFiltered(ctx, q)
	if err != nil {
		return 0, 0, fmt.Errorf("reading plan %s's ready steps: %w", planID, err)
	}
	for _, b := range all.Beads {
		if b != nil && b.Status != "closed" {
			open++
		}
	}
	for _, b := range unblocked.Beads {
		if b != nil && b.Status != "closed" && b.Status != "blocked" && !b.BlockedOnDecision {
			ready++
		}
	}
	return open, ready, nil
}

// stalledAsk is the ask a gardener-less plan calls for.
func stalledAsk(s StalledPlan) StalledAsk {
	switch {
	case s.OpenSteps == 0:
		return AskComplete
	case s.ReadySteps == 0:
		return AskWaits
	case conditions.GardenerlessRebinds(s.Gardener, s.Kind, s.Error):
		// Never bound, paused, a budget stop, or spawns refused (a 403), which
		// a rebind as the owner clears; a run of other failures is a fault a
		// rebind would only repeat.
		return AskRebind
	}
	return AskTell
}

// ErrorLine is err's first line, cut as a gardener's fire error is.
func ErrorLine(err error) string { return firstLine(err.Error(), stalledErrorChars) }

// closedAt is when b closed: closed_at, else its last update.
func closedAt(b *beadsapi.BeadDetail) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ClosedAt)); err == nil {
		return t
	}
	return b.UpdatedAt
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// firstLine is s's first non-empty line, spaces collapsed, cut to n runes.
func firstLine(s string, n int) string {
	for _, l := range strings.Split(s, "\n") {
		if l = oneLine(l); l != "" {
			if r := []rune(l); len(r) > n {
				l = string(r[:n-1]) + "…"
			}
			return l
		}
	}
	return ""
}

// DescribeStalled is one gardener-less plan's line in the prompt.
func DescribeStalled(s StalledPlan) string {
	line := s.ID + ": " + s.Title + ": "
	switch {
	case s.Gardener == "":
		line += "no gardener was ever bound"
	case s.Kind == KindPaused:
		line += "gardener " + s.Gardener + " is paused"
	case s.Kind == "":
		line += "gardener " + s.Gardener + " closed, stop not recorded"
	default:
		line += "gardener " + s.Gardener + " stopped on " + s.Kind
	}
	if s.Error != "" {
		line += ": " + strconv.Quote(s.Error)
	}
	return line + fmt.Sprintf("; steps: %d open, %d ready. Ask: %s", s.OpenSteps, s.ReadySteps, s.Ask)
}
