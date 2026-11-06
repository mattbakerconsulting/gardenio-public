package pgbudget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// FieldReportDay marks the #machines post with the UTC day it reports, so a
// restarted controller does not post it twice.
const FieldReportDay = "pg_report_day"

// RunnerStore is what the runner reads and writes; *beadsapi.Client is one.
type RunnerStore interface {
	Store
	conversation.ChannelReader
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// Runner posts the report once a day at Hour UTC, filing breaches first so
// the post names them. It runs in the controller's leader-elected loop, so
// there is one writer.
type Runner struct {
	Query   Querier
	Store   RunnerStore
	Budgets Budgets
	// Land's #machines gets the post and its project the tasks; Epic, when
	// set, parents them.
	Land, Epic string
	Hour       int
	Actor      string
	Logger     *slog.Logger
	// Every is how often the loop checks the clock (default 5m).
	Every time.Duration
	now   func() time.Time
	done  string // the last day posted or found posted
}

// RunLoop checks the clock until ctx ends.
func (r *Runner) RunLoop(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = 5 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	t := now().UTC()
	if t.Hour() < r.Hour || t.Format(time.DateOnly) == r.done {
		return
	}
	if err := r.RunOnce(ctx, t); err != nil {
		r.logger().Warn("pgbudget: daily efficiency report failed; retrying next tick", "error", err)
		return
	}
	r.done = t.Format(time.DateOnly)
}

// RunOnce reports the day of now, as of Hour:00 UTC that day, unless that
// day's post is already in #machines.
func (r *Runner) RunOnce(ctx context.Context, now time.Time) error {
	now = now.UTC()
	day := now.Format(time.DateOnly)
	asOf := time.Date(now.Year(), now.Month(), now.Day(), r.Hour, 0, 0, 0, time.UTC)
	mc, err := conversation.MachinesChannel(ctx, r.Store, r.Land)
	if err != nil {
		return err
	}
	if mc == nil {
		return fmt.Errorf("pgbudget: land %s has no #machines channel", r.Land)
	}
	prior, err := r.Store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Labels: []string{"project:" + r.Land},
		Fields: map[string]string{conversation.BeadChannelID: mc.ID, FieldReportDay: day},
		Limit:  1, NoTotal: true,
	})
	if err != nil {
		return fmt.Errorf("pgbudget: look for %s's report: %w", day, err)
	}
	if len(prior.Beads) > 0 {
		return nil
	}

	rep, err := Gather(ctx, r.Query, r.Budgets, asOf)
	if err != nil {
		return err
	}
	f := &Filer{Store: r.Store, Land: r.Land, Epic: r.Epic, Actor: r.Actor, Budgets: r.Budgets}
	filed, capped, ferr := f.File(ctx, rep, now)
	text := rep.Text(filed, capped)
	router := conversation.NewRouter(r.Store, r.logger())
	if _, err := router.Post(ctx, conversation.ChannelFromBead(mc, r.Land), conversation.Message{
		Kind:   conversation.KindText,
		Text:   text,
		Author: conversation.Principal{ID: r.Actor},
		Fields: map[string]string{FieldReportDay: day},
	}); err != nil {
		return errors.Join(ferr, fmt.Errorf("pgbudget: post to #machines: %w", err))
	}
	r.logger().Info("pgbudget: posted daily efficiency report", "day", day, "statements", len(rep.Statements), "tables", len(rep.Tables), "fired", len(rep.Fired), "filed", len(filed), "capped", capped)
	if ferr != nil {
		// Posted without the failed filings; the next day's run files them.
		r.logger().Warn("pgbudget: filing breaches failed", "error", ferr)
	}
	return nil
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
