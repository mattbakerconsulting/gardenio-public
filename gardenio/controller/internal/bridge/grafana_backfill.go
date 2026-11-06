// Package bridge — one-time backfill of shipped Grafana dashboards as beads.
//
// The three chart-shipped dashboards (cost-overview, kbeads-embedding,
// operated-by) ship today as JSON files baked into the grafana_dashboard="1"
// bundle ConfigMap. Backfill creates a type=dashboard bead per file (idempotent
// on dashboard_uid) so those dashboards become editable-as-beads.
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md build plan §8 step 5.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// BackfillDashboard is one shipped dashboard to import: a human label and the
// raw JSON model bytes.
type BackfillDashboard struct {
	// Name is a short identifier for logging (e.g. the source filename stem).
	Name  string
	Model []byte
}

// BackfillResult reports what a single backfill entry did.
type BackfillResult struct {
	Name     string
	UID      string
	BeadID   string
	Created  bool // false = already existed (idempotent skip of bead creation)
	ModelDoc string
	Err      error
}

// BackfillDashboards imports each shipped dashboard as a type=dashboard bead
// with its JSON model stored in an attached bundle doc. It is idempotent: a
// dashboard whose uid already has a bead is left in place (the model doc is
// still refreshed so re-running picks up chart updates).
//
// project labels the created beads (e.g. "gardenio") so they appear in
// project-scoped listings.
func BackfillDashboards(ctx context.Context, store *DashboardStore, dashboards []BackfillDashboard, project string, logger *slog.Logger) []BackfillResult {
	if logger == nil {
		logger = slog.Default()
	}
	results := make([]BackfillResult, 0, len(dashboards))
	for _, d := range dashboards {
		res := BackfillResult{Name: d.Name}
		meta, err := ParseDashboardMeta(d.Model)
		if err != nil {
			res.Err = fmt.Errorf("parse meta: %w", err)
			results = append(results, res)
			logger.Warn("backfill: parse meta failed", "name", d.Name, "error", err)
			continue
		}
		if meta.UID == "" {
			res.Err = fmt.Errorf("dashboard %q has no uid", d.Name)
			results = append(results, res)
			logger.Warn("backfill: dashboard has no uid; skipping", "name", d.Name)
			continue
		}
		res.UID = meta.UID

		existing, err := store.FindByUID(ctx, meta.UID)
		if err != nil {
			res.Err = fmt.Errorf("find by uid: %w", err)
			results = append(results, res)
			logger.Warn("backfill: find-by-uid failed", "name", d.Name, "uid", meta.UID, "error", err)
			continue
		}

		beadID := existing
		if beadID == "" {
			desc := "Grafana dashboard (backfilled from chart-shipped JSON). uid=" + meta.UID
			beadID, err = store.CreateDashboardBead(ctx, meta, project, desc)
			if err != nil {
				res.Err = fmt.Errorf("create dashboard bead: %w", err)
				results = append(results, res)
				logger.Warn("backfill: create bead failed", "name", d.Name, "uid", meta.UID, "error", err)
				continue
			}
			res.Created = true
			logger.Info("backfill: created dashboard bead", "name", d.Name, "uid", meta.UID, "bead", beadID)
		} else {
			logger.Info("backfill: dashboard bead already exists; refreshing model", "name", d.Name, "uid", meta.UID, "bead", beadID)
		}
		res.BeadID = beadID

		docID, err := store.PutModel(ctx, beadID, d.Model)
		if err != nil {
			res.Err = fmt.Errorf("put model: %w", err)
			results = append(results, res)
			logger.Warn("backfill: put model failed", "name", d.Name, "bead", beadID, "error", err)
			continue
		}
		res.ModelDoc = docID
		results = append(results, res)
	}
	return results
}

// CreateDashboardBead creates a type=dashboard bead carrying the small metadata
// fields extracted from a dashboard model (uid/title/version) and the
// grafana:dashboard label. The JSON model itself is attached separately via
// PutModel. project (when non-empty) adds a project:<project> label; desc
// (when non-empty) overrides the default description. Returns the bead ID.
//
// This is the single place dashboard beads are minted, shared by backfill and
// the `gb grafana put` authoring path so both produce identical beads.
func (s *DashboardStore) CreateDashboardBead(ctx context.Context, meta DashboardMeta, project, desc string) (string, error) {
	title := meta.Title
	if title == "" {
		title = meta.UID
	}
	fields := map[string]string{
		"dashboard_uid":    meta.UID,
		"dashboard_source": "beads",
	}
	if meta.Version > 0 {
		fields["dashboard_version"] = fmt.Sprintf("%d", meta.Version)
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshal fields: %w", err)
	}
	labels := []string{"grafana:dashboard"}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	if desc == "" {
		desc = "Grafana dashboard authored as a bead. uid=" + meta.UID
	}
	beadID, err := s.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "dashboard",
		Kind:        "issue",
		Priority:    2,
		CreatedBy:   s.actor,
		Description: desc,
		Labels:      labels,
		Fields:      fieldsJSON,
	})
	if err != nil {
		return "", err
	}
	return beadID, nil
}
