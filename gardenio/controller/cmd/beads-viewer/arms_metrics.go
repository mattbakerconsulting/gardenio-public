package main

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/prometheus/client_golang/prometheus"
)

// arms_metrics.go — how many epics bear arms (badges-from-arms plan
// gd-pQZO9ie11D, step 3 gd-YvWv4mR4fu). Arms are chosen by the agent that holds
// an epic, so the gap closes only as gardeners and crews get to it; this gauge
// shows that gap per project on /metrics:
//
//	gardenio_epics_arms{project="gardenio",arms="with"}    41
//	gardenio_epics_arms{project="gardenio",arms="without"} 17
//
// It counts not-closed epics. A scrape never calls the daemon: a background
// loop recounts every armsGaugeInterval and Collect reports the last count.

// armsGaugeInterval is how often the gauge recounts. Arms change at the pace
// of gardener runs (half-hourly), so five minutes is plenty fresh.
const armsGaugeInterval = 5 * time.Minute

// armsGaugeNoProject is the project label for an epic with no project:<name>.
const armsGaugeNoProject = "none"

// armsLister is the daemon surface the gauge needs.
type armsLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// armsGauge is a prometheus.Collector over the last epic count.
type armsGauge struct {
	api    armsLister
	desc   *prometheus.Desc
	mu     sync.Mutex
	counts map[string][2]int // project → {with, without}
}

func newArmsGauge(api armsLister) *armsGauge {
	return &armsGauge{
		api: api,
		desc: prometheus.NewDesc("gardenio_epics_arms",
			"Not-closed epics per project, with and without arms (heraldry).",
			[]string{"project", "arms"}, nil),
	}
}

// refresh recounts not-closed epics by project and arms. On error the last
// count stands. The count is the viewer's own, read with no user behind it, so
// it is a system read: with read-bearer enforcement on, an unmarked read is
// refused before it is sent (gd-WUojuuXdbc).
func (g *armsGauge) refresh(ctx context.Context) error {
	res, err := g.api.ListBeadsFiltered(beadsapi.WithSystemRead(ctx), beadsapi.ListBeadsQuery{
		Types:    []string{"epic"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    10000,
	})
	if err != nil {
		return err
	}
	counts := map[string][2]int{}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		p := projectFromLabels(b.Labels)
		if p == "" {
			p = armsGaugeNoProject
		}
		c := counts[p]
		if b.Heraldry != "" {
			c[0]++
		} else {
			c[1]++
		}
		counts[p] = c
	}
	g.mu.Lock()
	g.counts = counts
	g.mu.Unlock()
	return nil
}

// run recounts now and then every interval until ctx ends.
func (g *armsGauge) run(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := g.refresh(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("arms gauge: counting epics failed; keeping the last count", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (g *armsGauge) Describe(ch chan<- *prometheus.Desc) { ch <- g.desc }

func (g *armsGauge) Collect(ch chan<- prometheus.Metric) {
	g.mu.Lock()
	defer g.mu.Unlock()
	projects := make([]string, 0, len(g.counts))
	for p := range g.counts {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		c := g.counts[p]
		ch <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, float64(c[0]), p, "with")
		ch <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, float64(c[1]), p, "without")
	}
}
