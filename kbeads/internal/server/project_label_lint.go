package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// project_label_lint.go: the lint for the project-label invariant
// (project_label_invariant.go, gd-YdsEpnN3Qo). gardenio_beads_project_label_missing{type}
// counts the live beads that break it. createBead keeps new beads compliant
// and the admin backfill (POST /v1/admin/backfill-project-labels) fixes the
// old ones, so the gauge should read zero for
// every type; a non-zero value means a write path that bypasses createBead
// (an update dropping the label, a raw SQL fix) or a project-bearing type the
// backfill did not know about.
//
// The count scans every project-bearing bead and every message, so it runs on
// its own hourly ticker, never on the 30s fleet refresh, and Collect serves
// only the cached snapshot.

const (
	projectLabelLintInterval = time.Hour
	projectLabelLintTimeout  = 2 * time.Minute
)

// projectBearingBuiltinTypes returns, sorted, the builtin types whose config
// declares a `project` field naming the bead's own project. The lint and the
// backfill both take their type list from here.
func projectBearingBuiltinTypes() []string {
	var out []string
	for key, cfg := range builtinConfigs {
		name, ok := strings.CutPrefix(key, "type:")
		if !ok {
			continue
		}
		var tc model.TypeConfig
		if json.Unmarshal(cfg.Value, &tc) != nil {
			continue
		}
		if isProjectBearing(model.BeadType(name), &tc) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

type projectLabelLintCollector struct {
	store    store.Store
	types    []string
	interval time.Duration

	mu     sync.RWMutex
	counts map[string]int // nil until the first successful refresh

	missingDesc *prometheus.Desc
}

func newProjectLabelLintCollector(s store.Store, interval time.Duration) *projectLabelLintCollector {
	return &projectLabelLintCollector{
		store:    s,
		types:    projectBearingBuiltinTypes(),
		interval: interval,
		missingDesc: prometheus.NewDesc(
			"gardenio_beads_project_label_missing",
			"Live beads that break the project-label invariant (gd-YdsEpnN3Qo), by type: a project-bearing bead with fields.project and no project: label, or a message with no project: label whose thread head carries one. Such a bead is invisible to its own project's agent tokens (403). Should be 0; refreshed hourly.",
			[]string{"type"}, nil,
		),
	}
}

func (c *projectLabelLintCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.missingDesc
}

// Collect emits every linted type, zero included, so "zero" is a reading
// rather than an absent series. Nothing is emitted before the first refresh.
func (c *projectLabelLintCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	counts := c.counts
	c.mu.RUnlock()
	if counts == nil {
		return
	}
	for _, t := range append(slices.Clone(c.types), "message") {
		ch <- prometheus.MustNewConstMetric(c.missingDesc, prometheus.GaugeValue, float64(counts[t]), t)
	}
}

// start refreshes once in the background, then on every interval tick until
// ctx is cancelled. Unlike the fleet collector the first refresh is not
// synchronous: the scan is too slow to hold up daemon start.
func (c *projectLabelLintCollector) start(ctx context.Context) {
	go func() {
		c.refresh(ctx)
		if c.interval <= 0 {
			return
		}
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.refresh(ctx)
			}
		}
	}()
}

// refresh swaps in new counts; on error the previous ones are kept.
func (c *projectLabelLintCollector) refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, projectLabelLintTimeout)
	defer cancel()
	counts, err := c.store.ProjectLabelMissingCounts(fetchCtx, c.types)
	if err != nil {
		slog.Warn("project label lint refresh failed; keeping the last counts", "error", err)
		return
	}
	if counts == nil {
		counts = map[string]int{}
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total > 0 {
		slog.Warn("beads without the project label their type requires", "counts", counts)
	}
	c.mu.Lock()
	c.counts = counts
	c.mu.Unlock()
}

// BackfillProjectLabelsRequest is the JSON body for
// POST /v1/admin/backfill-project-labels. DryRun reports the counts without
// writing.
type BackfillProjectLabelsRequest struct {
	BatchSize int  `json:"batch_size"`
	DryRun    bool `json:"dry_run"`
}

// BackfillProjectLabelsResponse: Labelled is what this page wrote, by type;
// Missing is the lint's count (before the page, on a dry run). The caller
// repeats until Done.
type BackfillProjectLabelsResponse struct {
	Labelled map[string]int `json:"labelled"`
	Missing  map[string]int `json:"missing,omitempty"`
	Done     bool           `json:"done"`
}

// handleBackfillProjectLabels handles POST /v1/admin/backfill-project-labels:
// one page of the one-off backfill that gives the beads written before the
// invariant the project: label it requires (gd-YdsEpnN3Qo). Admin-only; an
// agent token cannot even see these beads.
func (s *BeadsServer) handleBackfillProjectLabels(w http.ResponseWriter, r *http.Request) {
	var req BackfillProjectLabelsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	types := projectBearingBuiltinTypes()
	if req.DryRun {
		missing, err := s.store.ProjectLabelMissingCounts(r.Context(), types)
		if err != nil {
			writeServerError(r.Context(), w, "handleBackfillProjectLabels", err, "failed to count beads missing a project label")
			return
		}
		writeJSON(w, http.StatusOK, &BackfillProjectLabelsResponse{Labelled: map[string]int{}, Missing: missing, Done: true})
		return
	}
	batch := req.BatchSize
	if batch <= 0 || batch > 1000 {
		batch = 500
	}
	stats, err := s.store.BackfillProjectLabelsPage(r.Context(), types, batch)
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillProjectLabels", err, "failed to backfill project labels")
		return
	}
	slog.Info("project label backfill page", "labelled", stats.Labelled, "done", stats.Done, "actor", actorFromContext(r.Context()))
	writeJSON(w, http.StatusOK, &BackfillProjectLabelsResponse{Labelled: stats.Labelled, Done: stats.Done})
}
