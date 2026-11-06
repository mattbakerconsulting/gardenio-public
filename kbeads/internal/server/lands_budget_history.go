package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/landmap"
)

// A tenant land's daily credit history, the Lands contract's getBudgetHistory
// (step 3.4, gd-rEwCINaT8d; gardenio/docs/garden-grafana-panels.md "Tenant
// isolation (3.1)").
//
// The source is the credits-only recording rules (3.2, gd-DxFIK7tips), read
// with an exact land matcher, so no figure at cost ever reaches the answer.
// When VictoriaMetrics is not wired, fails, or has no series for the land, the
// answer comes from the purse snapshots the watcher recorded on the land's
// lands-<land> account bead, one update event per write, and is marked stale.

// The 3.2 series this reads, both in credits with a land label:
// creditsTodaySeries is what the land has spent so far in the current UTC day
// (it starts again at 00:00Z), and creditsRemainingSeries is granted − spent.
const (
	creditsTodaySeries     = "gardenio:land_spend_credits:today"
	creditsRemainingSeries = "gardenio:land_credits:remaining"
)

// purseLedgerEvents caps the account bead's update events the stale answer
// reads. The watcher writes the snapshot at most once a minute and at least
// every five (watcher.SnapshotHeartbeat), so this covers two to ten days; the
// days before it have no point.
const purseLedgerEvents = 3000

var historyLand = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// utcDay is one UTC day; Truncate(utcDay) is its 00:00Z.
const utcDay = 24 * time.Hour

// BudgetHistory answers getBudgetHistory: the credit series, or the purse
// ledger, stale, when the series cannot answer. ok is false as for Budget.
func (l landsForMap) BudgetHistory(ctx context.Context, land string, days int) (landmap.BudgetHistoryRecord, bool, error) {
	acct, ok, err := l.purseAccount(ctx, land)
	if err != nil || !ok {
		return landmap.BudgetHistoryRecord{}, ok, err
	}
	now := time.Now().UTC()
	if l.s.vmClient != nil && historyLand.MatchString(land) {
		got, err := creditSeriesHistory(ctx, l.s.vmClient, land, days, now)
		switch {
		case err != nil:
			slog.Warn("lands budget history: credit series unavailable, reading the purse ledger", "land", land, "error", err)
		case len(got) > 0:
			return landmap.BudgetHistoryRecord{Days: got}, true, nil
		}
	}
	if acct == nil {
		// No account bead yet: nothing has been spent or recorded.
		return landmap.BudgetHistoryRecord{Stale: true}, true, nil
	}
	evs, err := l.s.store.GetEventsByTopic(ctx, acct.ID, events.TopicBeadUpdated, purseLedgerEvents)
	if err != nil {
		return landmap.BudgetHistoryRecord{}, false, fmt.Errorf("account %s: purse ledger: %w", acct.ID, err)
	}
	snaps := make([]landmap.BudgetRecord, 0, len(evs))
	for _, e := range evs {
		var p struct {
			Bead *struct {
				Fields json.RawMessage `json:"fields"`
			} `json:"bead"`
		}
		var f map[string]any
		if json.Unmarshal(e.Payload, &p) != nil || p.Bead == nil || json.Unmarshal(p.Bead.Fields, &f) != nil {
			continue
		}
		snaps = append(snaps, purseSnapshot(f))
	}
	return landmap.BudgetHistoryRecord{Days: ledgerDays(snaps, now.Truncate(utcDay).AddDate(0, 0, 1-days)), Stale: true}, true, nil
}

// creditSeriesHistory reads the land's days from the 3.2 series: each past
// day from a range query stepped at 00:00Z, whose [1d] window is the day
// before (the day's highest spent-today, its last remaining), and today from
// the series as they are now. A day with only one of the two has no point.
func creditSeriesHistory(ctx context.Context, vm *VMClient, land string, days int, now time.Time) ([]landmap.BudgetDayRecord, error) {
	sel := `{land="` + land + `"}`
	today := now.Truncate(utcDay)
	spent := map[time.Time]float64{}
	remaining := map[time.Time]float64{}
	if days > 1 {
		start := today.AddDate(0, 0, 2-days)
		for q, into := range map[string]map[time.Time]float64{
			"max_over_time(" + creditsTodaySeries + sel + "[1d])":      spent,
			"last_over_time(" + creditsRemainingSeries + sel + "[1d])": remaining,
		} {
			series, err := vm.QueryRange(ctx, q, start, today, utcDay)
			if err != nil {
				return nil, err
			}
			for _, s := range series {
				for _, p := range s.Points {
					into[p.T.UTC().Truncate(utcDay).Add(-utcDay)] = p.Value
				}
			}
		}
	}
	for q, into := range map[string]map[time.Time]float64{
		creditsTodaySeries + sel:     spent,
		creditsRemainingSeries + sel: remaining,
	} {
		samples, err := vm.QueryVector(ctx, q, now)
		if err != nil {
			return nil, err
		}
		for _, s := range samples {
			into[today] = s.Value
		}
	}
	var out []landmap.BudgetDayRecord
	for d := today.AddDate(0, 0, 1-days); !d.After(today); d = d.Add(utcDay) {
		s, okS := spent[d]
		r, okR := remaining[d]
		if okS && okR {
			out = append(out, landmap.BudgetDayRecord{Day: d, Spent: s, Remaining: r})
		}
	}
	return out, nil
}

// ledgerDays folds purse snapshots, in the order they were written, into days
// from first on: a day's remaining is its last snapshot's granted − used, and
// its spent the rise in used from the last snapshot before the day (or, with
// none, from its own first). Used counts credits in flight, so this is the
// purse's coarse view, and a fall in used reads as nothing spent. A snapshot
// with no used figure or no read time, or read before the one ahead of it, is
// skipped.
func ledgerDays(snaps []landmap.BudgetRecord, first time.Time) []landmap.BudgetDayRecord {
	var out []landmap.BudgetDayRecord
	var open, last float64
	for _, s := range snaps {
		if s.Granted == nil || s.Used == nil || s.ReadAt.IsZero() {
			continue
		}
		d, used := s.ReadAt.UTC().Truncate(utcDay), *s.Used
		switch n := len(out); {
		case n == 0:
			open = used
			out = append(out, landmap.BudgetDayRecord{Day: d})
		case d.Before(out[n-1].Day):
			continue
		case d.After(out[n-1].Day):
			open = last
			out = append(out, landmap.BudgetDayRecord{Day: d})
		}
		last = used
		cur := &out[len(out)-1]
		cur.Spent, cur.Remaining = max(0, used-open), *s.Granted-used
	}
	for i, d := range out {
		if !d.Day.Before(first) {
			return out[i:]
		}
	}
	return nil
}
