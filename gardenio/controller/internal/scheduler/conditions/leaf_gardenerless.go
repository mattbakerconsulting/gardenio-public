package conditions

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Gardener-less plans in the quiet_gardeners leaf (gd-DivW9ZOyL2). A building
// plan whose gardener the engine closed (a budget stop) has no live gardener,
// so FindQuietGardeners leaves it to the steward's gardener-less list
// (gardenerwatch.Stalled). That list is read only in the steward's prompt,
// after a wake, so the leaf must count those plans too or nothing wakes the
// steward to rebind them. It counts only the ones the steward acts on itself
// (the rebind ask), and only once per QuietGardenerAskWindow.

const (
	// GardenerlessPaused is how a gardener-less plan's open gardener that is
	// not enabled stopped (gardenerwatch.KindPaused).
	GardenerlessPaused = "paused"
	// gardenerlessConsecutiveFailures mirrors
	// scheduler.StopKindConsecutiveFailures.
	gardenerlessConsecutiveFailures = "consecutive_failures"
	// GardenerlessSeenMarker ends every comment the steward posts on a
	// gardener-less plan it has seen to (re-armed, rebound, or told the owner
	// about). A steward comment carrying it inside QuietGardenerAskWindow
	// keeps the plan out of the leaf's count.
	GardenerlessSeenMarker = "; seen to"
)

// GardenerlessBudgetStops are the gardener stops that end a loop on its
// budget, not on a fault: a fresh gardener picks the plan up where the last
// one stopped. They mirror scheduler's StopKindMaxRuns, StopKindWallClock,
// StopKindNoProgressFor and StopKindBeadQuery (scheduler imports this
// package).
var GardenerlessBudgetStops = []string{"max_runs", "wall_clock", "no_progress_for", "bead_query"}

// GardenerlessRebinds reports whether a plan with no live gardener, whose
// newest gardener is gardener ("" when none was ever bound) and stopped on
// kind with fireError, is one the steward rebinds itself: never bound,
// paused, stopped on its budget, or closed on spawns that were refused. It
// says nothing of the plan's steps.
func GardenerlessRebinds(gardener, kind, fireError string) bool {
	switch {
	case gardener == "", kind == GardenerlessPaused, slices.Contains(GardenerlessBudgetStops, kind):
		return true
	case kind == gardenerlessConsecutiveFailures:
		e := strings.ToLower(fireError)
		return strings.Contains(e, "403") || strings.Contains(e, "denied") || strings.Contains(e, "not authorized")
	}
	return false
}

// countGardenerless counts land's building plans with no open, enabled
// gardener that the steward would rebind (GardenerlessRebinds), that have a
// ready step, and that the steward has not seen to in the last
// QuietGardenerAskWindow: no GardenerlessSeenMarker comment of a steward run
// on the plan, nor a steward decision about its gardener. A plan the steward
// cannot fix so wakes it at most once a window. Any read error fails the
// count, as FindQuietGardeners does.
func countGardenerless(ctx context.Context, client BeadClient, land string, plans []QuietPlanRef, now time.Time) (int, error) {
	if len(plans) == 0 {
		return 0, nil
	}
	// Every open trigger, as gardenerwatch.Stalled reads them: one bound to
	// the plan by label or assign_bead is its gardener, so a plan the prompt
	// calls live is never counted here.
	res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger", "schedule"},
		Statuses: []string{"open", "in_progress", "blocked"},
		Limit:    quietGardenerScan,
	})
	if err != nil {
		return 0, fmt.Errorf("quiet_gardeners: reading the plan gardeners: %w", err)
	}
	live, paused := map[string]bool{}, map[string]*beadsapi.BeadDetail{}
	for _, b := range res.Beads {
		if b == nil || b.Status == "closed" {
			continue
		}
		var bound []string
		if a := strings.TrimSpace(b.Fields["assign_bead"]); a != "" {
			bound = append(bound, a)
		}
		for _, l := range b.Labels {
			if plan, ok := strings.CutPrefix(l, quietEpicLabelPrefix); ok && plan != "" {
				bound = append(bound, plan)
			}
		}
		for _, plan := range bound {
			if strings.TrimSpace(b.Fields["enabled"]) != "false" {
				live[plan] = true
			} else {
				paused[plan] = b
			}
		}
	}
	n := 0
	for _, p := range plans {
		if live[p.ID] {
			continue
		}
		g, kind := paused[p.ID], GardenerlessPaused
		if g == nil {
			if g, err = lastClosedGardener(ctx, client, p.ID); err != nil {
				return 0, err
			}
			if g != nil {
				kind = strings.TrimSpace(g.Fields["gardener_terminated_kind"])
			}
		}
		gardener, fireError := "", ""
		if g != nil {
			gardener, fireError = g.ID, g.Fields["last_fire_error"]
		}
		if !GardenerlessRebinds(gardener, kind, fireError) {
			continue
		}
		ready, _, _, err := readySteps(ctx, client, p.ID)
		if err != nil {
			return 0, err
		}
		if ready == 0 {
			continue
		}
		seen, err := stewardSeen(ctx, client, land, p.ID, now)
		if err != nil {
			return 0, err
		}
		if seen {
			continue
		}
		if gardener != "" {
			asked, err := stewardDecisions(ctx, client, land, gardener, now)
			if err != nil {
				return 0, err
			}
			if len(asked) > 0 {
				continue
			}
		}
		n++
	}
	return n, nil
}

// lastClosedGardener is the plan's gardener-epic:<plan> trigger that closed
// last, or nil: gardenerwatch.LastGardener's pick (closed_at, else the last
// update).
func lastClosedGardener(ctx context.Context, client BeadClient, plan string) (*beadsapi.BeadDetail, error) {
	res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"trigger", "schedule"}, Labels: []string{quietEpicLabelPrefix + plan}, Statuses: []string{"closed"},
	})
	if err != nil {
		return nil, fmt.Errorf("quiet_gardeners: reading plan %s's last gardener: %w", plan, err)
	}
	closedAt := func(b *beadsapi.BeadDetail) time.Time {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ClosedAt)); err == nil {
			return t
		}
		return b.UpdatedAt
	}
	var last *beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.Status != "closed" {
			continue
		}
		if last == nil || closedAt(b).After(closedAt(last)) {
			last = b
		}
	}
	return last, nil
}

// stewardSeen reports whether a steward run of land commented on plan with
// GardenerlessSeenMarker in the last QuietGardenerAskWindow.
func stewardSeen(ctx context.Context, client BeadClient, land, plan string, now time.Time) (bool, error) {
	cc, ok := client.(CommentClient)
	if !ok {
		return false, fmt.Errorf("quiet_gardeners: the daemon client cannot read comments")
	}
	comments, err := cc.GetComments(ctx, plan)
	if err != nil {
		return false, fmt.Errorf("quiet_gardeners: reading plan %s's comments: %w", plan, err)
	}
	run := StewardRunPrefix(land)
	return slices.ContainsFunc(comments, func(c beadsapi.Comment) bool {
		return strings.Contains(c.Author, run) && strings.Contains(c.Text, GardenerlessSeenMarker) &&
			c.CreatedAt.After(now.Add(-QuietGardenerAskWindow)) && !c.CreatedAt.After(now)
	}), nil
}
