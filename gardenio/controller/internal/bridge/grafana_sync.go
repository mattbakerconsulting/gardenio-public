// Package bridge — grafana-bridge sync handler (beads → Grafana).
//
// GrafanaSync subscribes to kbeads SSE events for type=dashboard beads, fetches
// their JSON model from the attached bundle doc, and writes it to Grafana via
// the ConfigMap provisioning path. The dashboard_hash + dashboard_source
// fields form the loop-breaker so a write does not bounce back as an edit.
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md (epic ka-0000006776),
// build plan §8 steps 1–3 (W2-impl-a). The Grafana→beads read-back poll +
// conflict policy (W2-impl-b) live in grafana_readback.go.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// grafanaSyncTTL is the dedup window for beads→Grafana writes keyed on
// bead+hash, so an SSE replay after restart does not re-write an unchanged
// model.
const grafanaSyncTTL = 10 * time.Minute

// DashboardWriter is the write surface GrafanaSync targets. The default
// implementation is the ConfigMap writer; an HTTP-API writer can be swapped in
// later (W2 design doc path A) behind the same interface.
type DashboardWriter interface {
	// Upsert writes the model for a dashboard uid on behalf of beadID and
	// returns the target identifier (ConfigMap name) and data key for
	// logging/provenance. It refuses with ErrDashboardUIDOwned when the uid's
	// target is already provisioned from a different bead, or is not the
	// bridge's own.
	Upsert(ctx context.Context, uid, beadID string, model []byte) (target, key string, err error)
	// ChartOwnsDashboard reports whether a chart-shipped (non-bridge)
	// grafana_dashboard ConfigMap already provisions this uid's dashboard file.
	// When true the bridge defers to the chart rather than writing a competing
	// ConfigMap (see grafana_configmap.go for the sidecar-collision rationale).
	ChartOwnsDashboard(ctx context.Context, uid string) (owned bool, err error)
	// RemoveOwn deletes the bridge's own ConfigMap for this uid if present, so a
	// shadow written before the dashboard became chart-owned stops competing.
	RemoveOwn(ctx context.Context, uid string) error
	// Exists reports whether the bridge's own ConfigMap for this uid is present
	// with a non-empty model under the expected data key. It lets SyncDashboard
	// verify the artifact the hash loop-breaker short-circuits on still exists,
	// so a ConfigMap deleted out-of-band self-heals on the next sync/catch-up
	// even when the bead's stamped dashboard_hash is unchanged (ka-0000002299).
	Exists(ctx context.Context, uid string) (present bool, err error)
}

// ErrDashboardUIDOwned is returned by DashboardWriter.Upsert when the uid is
// already provisioned from another bead, so a bead cannot take over a board by
// claiming its uid (gd-rbEoXGobIg).
var ErrDashboardUIDOwned = errors.New("dashboard uid is owned by another bead")

// GrafanaSync watches dashboard-bead SSE events and provisions Grafana.
type GrafanaSync struct {
	store    *DashboardStore
	writer   DashboardWriter
	logger   *slog.Logger
	projects []string

	mu   sync.Mutex
	seen map[string]time.Time // dedup key (bead:hash) → last write time
}

// GrafanaSyncConfig configures a GrafanaSync.
type GrafanaSyncConfig struct {
	Store  *DashboardStore
	Writer DashboardWriter
	Logger *slog.Logger
	// Projects is the fleet allow-list: a dashboard bead syncs only when it
	// carries a project: label and every project: label it carries is listed
	// here. Empty syncs nothing.
	Projects []string
}

// NewGrafanaSync constructs a GrafanaSync.
func NewGrafanaSync(cfg GrafanaSyncConfig) *GrafanaSync {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &GrafanaSync{
		store:    cfg.Store,
		writer:   cfg.Writer,
		logger:   logger,
		projects: cfg.Projects,
		seen:     make(map[string]time.Time),
	}
}

// RegisterHandlers wires SSE handlers for dashboard bead create/update events.
func (s *GrafanaSync) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", s.handleEvent)
	stream.On("beads.bead.updated", s.handleEvent)
	s.logger.Info("grafana-sync registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.updated"})
}

// handleEvent processes a dashboard bead event: fetch model, hash, skip if
// unchanged, else write to Grafana and stamp provenance fields.
func (s *GrafanaSync) handleEvent(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "dashboard" {
		return
	}
	s.SyncDashboard(ctx, bead.ID)
}

// SyncDashboard performs one beads→Grafana sync for a dashboard bead. Exposed
// (not just via SSE) so backfill and catch-up can drive it directly.
func (s *GrafanaSync) SyncDashboard(ctx context.Context, dashboardID string) {
	detail, err := s.store.daemon.GetBead(ctx, dashboardID)
	if err != nil || detail == nil {
		s.logger.Warn("grafana-sync: get dashboard bead failed", "bead", dashboardID, "error", err)
		return
	}
	// Project fence (gd-rbEoXGobIg): the bridge writes fleet Grafana, so only a
	// bead from an allowed project may reach it. A refused bead writes nothing
	// and removes nothing, so it cannot delete a fleet board either.
	if perr := dashboardProjectError(detail.Labels, s.projects); perr != nil {
		s.logger.Warn("grafana-sync: dashboard bead refused", "bead", dashboardID,
			"labels", detail.Labels, "allowed_projects", s.projects, "error", perr)
		return
	}
	uid := detail.Fields["dashboard_uid"]
	if uid == "" {
		s.logger.Warn("grafana-sync: dashboard bead has no dashboard_uid; skipping", "bead", dashboardID)
		return
	}

	if s.deferToChart(ctx, dashboardID, uid) {
		return
	}

	_, model, err := s.store.ModelDoc(ctx, dashboardID)
	if err != nil {
		s.logger.Warn("grafana-sync: fetch model doc failed", "bead", dashboardID, "error", err)
		return
	}
	if len(model) == 0 {
		s.logger.Info("grafana-sync: dashboard bead has no model doc yet; skipping", "bead", dashboardID, "uid", uid)
		return
	}

	hash, err := HashDashboardModel(model)
	if err != nil {
		s.logger.Warn("grafana-sync: hashing model failed", "bead", dashboardID, "error", err)
		return
	}

	dedupKey := dashboardID + ":" + hash
	if s.inSync(ctx, dashboardID, uid, hash, detail.Fields["dashboard_hash"], dedupKey) {
		return
	}

	target, key, err := s.writer.Upsert(ctx, uid, dashboardID, model)
	if errors.Is(err, ErrDashboardUIDOwned) {
		s.logger.Warn("grafana-sync: dashboard uid belongs to another bead; refusing write",
			"bead", dashboardID, "uid", uid, "error", err)
		return
	}
	if err != nil {
		s.logger.Error("grafana-sync: write to Grafana failed", "bead", dashboardID, "uid", uid, "error", err)
		return
	}

	// Stamp provenance: hash + source=beads so the next read-back is a no-op.
	fields := map[string]string{
		"dashboard_hash":          hash,
		"dashboard_source":        "beads",
		"dashboard_configmap_key": key,
	}
	if err := s.store.daemon.UpdateBeadFields(ctx, dashboardID, fields); err != nil {
		s.logger.Warn("grafana-sync: stamping provenance fields failed", "bead", dashboardID, "error", err)
	}
	s.markSeen(dedupKey)
	s.logger.Info("grafana-sync: dashboard synced to Grafana",
		"bead", dashboardID, "uid", uid, "configmap", target, "key", key, "hash", hash[:12])
}

// deferToChart reports whether a chart-shipped ConfigMap owns uid, in which case
// the bridge must not write and has removed its own shadow.
func (s *GrafanaSync) deferToChart(ctx context.Context, dashboardID, uid string) bool {
	// Defer to chart-shipped dashboards (ka-0000002774). If a chart bundle
	// ConfigMap already provisions this uid's dashboard file, a bridge-written
	// ConfigMap for the same uid would carry the same "<uid>.json" data key and
	// collide with it in the Grafana sidecar's flat provisioning folder
	// (last-writer-wins), able to pin the live board to this bead's — often
	// stale — model. Skip the write and remove any shadow we previously wrote so
	// the maintained chart bundle ConfigMap is the sole source. Run before the
	// hash loop-breaker so a startup catch-up cleans up a pre-existing shadow
	// even when the bead model is unchanged. Fail open (log + proceed) if the
	// ownership probe errors, so a transient list failure never strands a
	// legitimately bead-only dashboard.
	owned, oerr := s.writer.ChartOwnsDashboard(ctx, uid)
	if oerr != nil {
		s.logger.Warn("grafana-sync: chart-ownership probe failed; proceeding with bridge provisioning",
			"bead", dashboardID, "uid", uid, "error", oerr)
	} else if owned {
		if rmErr := s.writer.RemoveOwn(ctx, uid); rmErr != nil {
			s.logger.Warn("grafana-sync: removing shadow ConfigMap failed", "bead", dashboardID, "uid", uid, "error", rmErr)
		}
		s.logger.Info("grafana-sync: dashboard is chart-shipped; deferring to chart ConfigMap (skipping bridge write)",
			"bead", dashboardID, "uid", uid)
		return true
	}
	return false
}

// inSync reports whether the uid's ConfigMap already carries the model with
// this hash, so SyncDashboard can skip the write.
func (s *GrafanaSync) inSync(ctx context.Context, dashboardID, uid, hash, stampedHash, dedupKey string) bool {
	// Loop-breaker: if the bead's stored hash already matches the model we're
	// about to write, the model is in sync — nothing to do. This also stops a
	// Grafana-side read-back (W2-impl-b) from ping-ponging back through here.
	//
	// But the loop-breaker assumes stamped-hash ⟺ ConfigMap-present-and-correct.
	// That invariant breaks when the ConfigMap is deleted out-of-band (e.g. a
	// manual delete to clear a dup-uid sidecar lockout) while the bead keeps its
	// stamped hash: catch-up/SSE then hit this short-circuit and the dashboard
	// stays permanently dark with no self-healing path (ka-0000002299). So before
	// honoring the hash match (and the restart dedup below, which is likewise
	// keyed on the hash), verify the artifact we would short-circuit on actually
	// exists. If the probe says the ConfigMap is absent, fall through and Upsert
	// regardless of the hash match. Fail closed toward writing: if the probe
	// errors we cannot prove the CM exists, so we proceed with the write rather
	// than risk leaving a dashboard stranded on an unverifiable assumption — the
	// Upsert is idempotent, so a redundant write is cheap and safe.
	if stampedHash == hash {
		present, perr := s.writer.Exists(ctx, uid)
		if perr != nil {
			s.logger.Warn("grafana-sync: ConfigMap presence probe failed; proceeding with write to be safe",
				"bead", dashboardID, "uid", uid, "error", perr)
		} else if present {
			s.logger.Debug("grafana-sync: hash unchanged and ConfigMap present; skipping write", "bead", dashboardID, "uid", uid)
			return true
		} else {
			s.logger.Info("grafana-sync: hash unchanged but ConfigMap missing; forcing re-provision",
				"bead", dashboardID, "uid", uid)
		}
	}

	// Restart dedup: avoid re-writing the same (bead, hash) within the TTL. This
	// only applies once we know a write is warranted — a missing-CM force-write
	// above must not be suppressed by an in-memory dedup entry from a prior write
	// whose ConfigMap was since deleted, so re-check presence when the dedup key
	// is a hit.
	if s.isDuplicate(dedupKey) {
		if present, perr := s.writer.Exists(ctx, uid); perr == nil && present {
			s.logger.Debug("grafana-sync: duplicate write suppressed", "bead", dashboardID, "uid", uid)
			return true
		}
		s.logger.Info("grafana-sync: dedup hit but ConfigMap missing/unverifiable; forcing re-provision",
			"bead", dashboardID, "uid", uid)
	}
	return false
}

// dashboardProjectError refuses a dashboard bead with no project: label, or
// with any project: label not in allowed.
func dashboardProjectError(labels, allowed []string) error {
	labelled := false
	for _, l := range labels {
		p, ok := strings.CutPrefix(l, "project:")
		if !ok {
			continue
		}
		if !slices.Contains(allowed, p) {
			return fmt.Errorf("project %q is not a fleet dashboard project", p)
		}
		labelled = true
	}
	if !labelled {
		return errors.New("no project: label")
	}
	return nil
}

func (s *GrafanaSync) isDuplicate(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.seen[key]
	if ok && time.Since(last) < grafanaSyncTTL {
		return true
	}
	return false
}

func (s *GrafanaSync) markSeen(key string) {
	s.mu.Lock()
	s.seen[key] = time.Now()
	s.mu.Unlock()
}
