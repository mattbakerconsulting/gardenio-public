package server

import (
	"context"

	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/store"
)

// storeEmbeddingStatsProvider adapts the store's GetEmbeddingStats aggregate
// onto the metrics.StatsProvider interface consumed by the embedding coverage/
// stale collector. The store query aggregates over the whole beads table
// (there is no per-fleet split in the embedding schema), so the snapshot is
// reported under a single fleet label — the daemon's default fleet.
type storeEmbeddingStatsProvider struct {
	store store.Store
	fleet string
}

// NewStoreEmbeddingStatsProvider returns a metrics.StatsProvider backed by the
// store. fleet is the label applied to the single aggregate snapshot (pass the
// daemon's default fleet). Wire the result with
// BeadsServer.SetEmbeddingStatsProvider so gardenio_beads_embedding_coverage_ratio
// and gardenio_beads_embedding_stale_count emit live series.
func NewStoreEmbeddingStatsProvider(s store.Store, fleet string) metrics.StatsProvider {
	if fleet == "" {
		fleet = "default"
	}
	return &storeEmbeddingStatsProvider{store: s, fleet: fleet}
}

func (p *storeEmbeddingStatsProvider) EmbeddingStats(ctx context.Context) ([]metrics.EmbeddingStats, error) {
	stats, err := p.store.GetEmbeddingStats(ctx)
	if err != nil {
		return nil, err
	}
	if stats == nil {
		return nil, nil
	}
	return []metrics.EmbeddingStats{{
		Fleet:         p.fleet,
		TotalBeads:    stats.TotalBeads,
		EmbeddedBeads: stats.EmbeddedBeads,
		StaleCount:    stats.StaleCount,
	}}, nil
}
