package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planspend"
)

// planSpendListLimit bounds each plan-epic list call. Open plans number in the
// dozens; closed plans are read newest first and only those not yet rolled up
// since they closed are walked, so the limit only bounds the backlog a fresh
// rollout catches up on.
const planSpendListLimit = 500

// planSpendBatch is how many plans one planspend.Plans call walks. Each call
// makes at most three reverse-deps batches and two hydration batches, so a
// chunk bounds both the request fan-out and the agent beads held at once.
const planSpendBatch = 50

// planSpendRefusedBackoff is how long a plan whose stamp the daemon refused
// (403) is left alone before the sync tries it again. A refusal is the
// daemon's authz answer, not a transient fault: re-sending it every pass is
// what made this sync ~12.6k/day of gardenio_authz_denials_total while the
// controller held no grant on plan epics (gd-Hk70sS1XuS). The backoff, not a
// permanent skip, lets a grant that lands later take effect without a
// controller restart.
const planSpendRefusedBackoff = 6 * time.Hour

// planSpendOpenStatuses are the statuses of a plan that is still walked every
// pass (gardenhub's farmOpenStatuses).
var planSpendOpenStatuses = []string{"open", "in_progress", "blocked", "deferred", "partial"}

// planSpendListFields are the only plan-epic fields the sync reads.
var planSpendListFields = []string{
	"phase_order",
	planspend.FieldUSD, planspend.FieldSyncedAt, planspend.FieldCrew, planspend.FieldLive,
}

// planSpendDaemon is the slice of the daemon client the sync uses.
type planSpendDaemon interface {
	planspend.Source
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
}

// planSpendSync periodically rolls up each plan's crew spend onto the plan
// epic's plan_spend_* fields (gd-kcy4IFNzv5), the sibling of gardenerSpendSync.
// gardenhub reads the fields into FarmPlan.Cost, so /garden shows what a plan
// cost without walking agent beads; the walk and the per-agent figure are
// internal/planspend, shared with the /plans crew card's agreement test.
//
// Open plans are walked every pass. A closed plan is walked until one write
// lands after its closed_at, then left alone. A plan's epic is only written
// when its figure, crew count or live count changed, never to re-stamp an
// unchanged one: each write is a plan-activity event that wakes the plan's
// gardener (gd-vwMhcJgts2). Readers judge a moving figure fresh by the newest
// stamp on any plan instead (planspend.ReadWithPass).
type planSpendSync struct {
	daemon planSpendDaemon
	logger *slog.Logger
	now    func() time.Time
	// refused maps a plan whose last stamp was a 403 to when it was refused
	// (planSpendRefusedBackoff). Only RunLoop's goroutine touches it.
	refused map[string]time.Time
}

func newPlanSpendSync(daemon planSpendDaemon, logger *slog.Logger) *planSpendSync {
	return &planSpendSync{
		daemon:  daemon,
		logger:  logger.With("component", "plan-spend-sync"),
		now:     time.Now,
		refused: map[string]time.Time{},
	}
}

// RunLoop rolls up plan spend every planspend.SyncInterval until ctx is
// cancelled, with one immediate pass on start.
func (c *planSpendSync) RunLoop(ctx context.Context) {
	c.syncAll(ctx)

	ticker := time.NewTicker(planspend.SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.syncAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// syncAll walks every plan that needs it and writes the figures that moved.
func (c *planSpendSync) syncAll(ctx context.Context) {
	open, closed, err := c.listPlans(ctx)
	if err != nil {
		c.logger.Warn("plan spend sync: failed to list plans", "error", err)
		return
	}
	now := c.now().UTC()
	final := make(map[string]bool, len(closed))
	epics := make([]*beadsapi.BeadDetail, 0, len(open)+len(closed))
	backedOff := 0
	for _, b := range append(append([]*beadsapi.BeadDetail{}, open...), closed...) {
		if at, ok := c.refused[b.ID]; ok {
			if now.Sub(at) < planSpendRefusedBackoff {
				backedOff++
				continue
			}
			delete(c.refused, b.ID)
		}
		epics = append(epics, b)
	}
	for _, b := range closed {
		final[b.ID] = true
	}

	written, failed := 0, 0
	for start := 0; start < len(epics); start += planSpendBatch {
		chunk := epics[start:min(start+planSpendBatch, len(epics))]
		rolls, err := planspend.Plans(ctx, c.daemon, chunk, now)
		if err != nil {
			// Nothing is written for the chunk rather than a partial figure.
			c.logger.Warn("plan spend sync: rollup failed", "plans", len(chunk), "error", err)
			failed += len(chunk)
			continue
		}
		for _, b := range chunk {
			r := rolls[b.ID]
			if !planSpendNeedsWrite(planspend.Read(b.Fields, now), r, final[b.ID]) {
				continue
			}
			fields := map[string]any{
				planspend.FieldUSD:      planspend.FormatUSD(r.USD),
				planspend.FieldSyncedAt: now.Format(time.RFC3339),
				planspend.FieldCrew:     r.Crew,
				planspend.FieldLive:     r.Live,
			}
			if err := c.daemon.UpdateBeadFieldsTyped(ctx, b.ID, fields); err != nil {
				if planSpendRefused(err) {
					c.refused[b.ID] = now
					c.logger.Warn("plan spend sync: daemon refused the stamp; not retrying this plan until the backoff passes",
						"plan", b.ID, "backoff", planSpendRefusedBackoff, "error", err)
				} else {
					c.logger.Warn("plan spend sync: update failed", "plan", b.ID, "error", err)
				}
				failed++
				continue
			}
			written++
			c.logger.Debug("plan spend rolled up", "plan", b.ID, "spend_usd", r.USD, "crew", r.Crew, "live", r.Live, "closed", final[b.ID])
		}
	}
	c.logger.Info("plan spend sync pass", "open_plans", len(open), "closed_plans", len(closed), "written", written, "failed", failed, "refused_backoff", backedOff)
}

// planSpendRefused reports whether err is the daemon refusing the write (403),
// which a retry cannot change.
func planSpendRefused(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden
}

// listPlans returns the open plan epics and the closed ones still owed their
// final rollup (planSpendOwedFinal).
func (c *planSpendSync) listPlans(ctx context.Context) (open, closed []*beadsapi.BeadDetail, err error) {
	res, err := c.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"epic"},
		Labels:   []string{"plan"},
		Statuses: planSpendOpenStatuses,
		Limit:    planSpendListLimit,
	}.OnlyFields(planSpendListFields...))
	if err != nil {
		return nil, nil, err
	}
	if res != nil {
		for _, b := range res.Beads {
			if b != nil && b.ID != "" {
				open = append(open, b)
			}
		}
	}
	res, err = c.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"epic"},
		Labels:   []string{"plan"},
		Statuses: []string{"closed"},
		Sort:     "-updated_at",
		Limit:    planSpendListLimit,
	}.OnlyFields(planSpendListFields...))
	if err != nil {
		return nil, nil, err
	}
	if res != nil {
		for _, b := range res.Beads {
			if b != nil && b.ID != "" && planSpendOwedFinal(b) {
				closed = append(closed, b)
			}
		}
	}
	return open, closed, nil
}

// planSpendOwedFinal reports whether a closed plan still needs its rollup at
// close: it has never been rolled up, or its last rollup predates closed_at
// (a plan reopened and closed again is owed another). A plan with a stamp but
// no readable closed_at is left alone.
func planSpendOwedFinal(b *beadsapi.BeadDetail) bool {
	synced, err := time.Parse(time.RFC3339, strings.TrimSpace(b.Fields[planspend.FieldSyncedAt]))
	if err != nil {
		return true
	}
	closedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ClosedAt))
	if err != nil {
		return false
	}
	return synced.Before(closedAt)
}

// planSpendNeedsWrite decides whether a fresh rollup is written over what the
// epic carries. A final (closed-plan) rollup is always written, so the plan
// stops being owed one.
func planSpendNeedsWrite(old planspend.Reading, r planspend.Rollup, final bool) bool {
	switch {
	case final, !old.Has:
		return true
	case math.Round(old.USD*100) != math.Round(r.USD*100), old.Crew != r.Crew, old.Live != r.Live:
		return true
	}
	return false
}
