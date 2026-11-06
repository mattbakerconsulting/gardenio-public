package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/prhealth"
)

// Open-PR health gauges (plan gd-3F4W22h2nT step 10, gd-Pt8ztuasTE). The PR
// sweeper counts the open PRs into main once an hour and writes the figures to
// its run's agent bead (internal/prhealth); these gauges republish the last
// run's record so the Grafana panel and the vmalert rules
// (OpenPROrphansHigh, OpenPROldestOverdue) can read it:
//
//	gardenio_open_prs{state}                    state=open | desk_queue | orphaned | conflicting
//	gardenio_open_pr_oldest_age_hours{state}    state=open | orphaned | conflicting
//	gardenio_merge_train_state{state}           1 for the train's state, 0 for the rest
//	gardenio_open_pr_health_timestamp_seconds   when the sweeper counted
//
// The series are absent until a sweeper run has written a record. A run that
// has not written yet, or a failed read, keeps the last record: the timestamp
// gauge says how old it is.

// openPRHealthInterval is how often the record is re-read. The sweeper writes
// it once an hour.
const openPRHealthInterval = 5 * time.Minute

// beadGetter is the GetBead half of prhealth.Querier. The daemon client has it;
// a BeadQuerier without it publishes no open-PR series.
type beadGetter interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// The controller's querier must stay a beadGetter, or the series vanish.
var _ beadGetter = beadsapi.LeanAgentLister{}

type openPRGauges struct {
	count, oldest, train *prometheus.GaugeVec
	recordedAt           prometheus.Gauge
}

func newOpenPRGauges() openPRGauges {
	return openPRGauges{
		count: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "open_prs",
			Help:      "Open PRs into main at the PR sweeper's last run. state=open is every open PR; desk_queue the ones an open merge escalation names; orphaned the ones with no merge escalation and no living owner when the sweeper found them, before it escalated or closed them; conflicting the ones that conflict with main, from the PR beads' mr_merge_state.",
		}, []string{"state"}),
		oldest: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "open_pr_oldest_age_hours",
			Help:      "Age in hours of the oldest open PR (state=open, desk and human PRs excluded) of the oldest orphan (state=orphaned, 0 with none) and of the longest conflict with main, from mr_conflict_since (state=conflicting, 0 with none), at the PR sweeper's last run.",
		}, []string{"state"}),
		train: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "merge_train_state",
			Help:      "The merge desk's train at the PR sweeper's last run: 1 for its state (none, pending, green, red, conflicting, revert), 0 for the others.",
		}, []string{"state"}),
		recordedAt: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "open_pr_health_timestamp_seconds",
			Help:      "Unix time the PR sweeper counted the open PRs the open-PR gauges show.",
		}),
	}
}

func (g openPRGauges) collectors() []prometheus.Collector {
	return []prometheus.Collector{g.count, g.oldest, g.train, g.recordedAt}
}

func (g openPRGauges) set(h *prhealth.Health) {
	g.count.WithLabelValues("open").Set(float64(h.Open))
	g.count.WithLabelValues("desk_queue").Set(float64(h.DeskQueue))
	g.count.WithLabelValues("orphaned").Set(float64(h.Orphans))
	g.count.WithLabelValues("conflicting").Set(float64(h.Conflicting))
	g.oldest.WithLabelValues("open").Set(h.OldestOpenHours)
	g.oldest.WithLabelValues("orphaned").Set(h.OldestOrphanHours)
	g.oldest.WithLabelValues("conflicting").Set(h.OldestConflictHours)
	g.train.Reset()
	if h.Train != "" {
		for _, s := range prhealth.TrainStates {
			v := 0.0
			if s == h.Train {
				v = 1
			}
			g.train.WithLabelValues(s).Set(v)
		}
	}
	g.recordedAt.Set(float64(h.At.Unix()))
}

// collectOpenPRs refreshes the open-PR gauges at most every
// openPRHealthInterval.
func (c *Collector) collectOpenPRs(ctx context.Context) {
	now := time.Now()
	if !c.openPRsAt.IsZero() && now.Sub(c.openPRsAt) < openPRHealthInterval {
		return
	}
	getter, ok := c.querier.(beadGetter)
	if !ok {
		return
	}
	c.openPRsAt = now
	h, err := prhealth.Latest(ctx, struct {
		BeadQuerier
		beadGetter
	}{c.querier, getter})
	if err != nil {
		c.logger.Warn("failed to read the PR sweeper's open-PR figures", "error", err)
		c.scrapeErrors.Inc()
		return
	}
	if h != nil {
		c.openPRs.set(h)
	}
}
