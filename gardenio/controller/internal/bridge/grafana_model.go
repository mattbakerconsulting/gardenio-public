// Package bridge — Grafana dashboard model helpers.
//
// A dashboard is authored/versioned as a bead (type=dashboard) whose JSON model
// lives in an attached bundle doc; grafana-bridge syncs that model to Grafana
// via the grafana_dashboard="1" ConfigMap sidecar. These helpers normalize a
// dashboard JSON model and compute a stable content hash so the bridge can
// detect real changes and break the beads↔Grafana sync loop.
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md (epic ka-0000006776).
package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// volatileDashboardKeys are top-level Grafana-assigned fields that change on
// every save without representing a meaningful model change. They are stripped
// before hashing so that a version bump alone does not look like an edit.
var volatileDashboardKeys = []string{"id", "version"}

// NormalizeDashboardJSON parses a Grafana dashboard JSON model, strips the
// volatile Grafana-assigned fields (id, version), and re-encodes it
// canonically (sorted keys, no insignificant whitespace). The result is stable
// across re-serializations so it can be hashed for change detection.
//
// It returns an error if the input is not a JSON object.
func NormalizeDashboardJSON(model []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(model, &obj); err != nil {
		return nil, fmt.Errorf("dashboard model is not valid JSON object: %w", err)
	}
	for _, k := range volatileDashboardKeys {
		delete(obj, k)
	}
	// encoding/json marshals map keys in sorted order, giving a canonical form.
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("re-encoding normalized dashboard: %w", err)
	}
	return out, nil
}

// HashDashboardModel returns the hex sha256 of the normalized dashboard model.
// Two models that differ only in volatile fields (id/version) or key ordering
// hash identically. This is the loop-breaker token stored in the bead's
// dashboard_hash field.
func HashDashboardModel(model []byte) (string, error) {
	norm, err := NormalizeDashboardJSON(model)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(norm)
	return hex.EncodeToString(sum[:]), nil
}

// DashboardMeta holds the small metadata extracted from a dashboard JSON model.
type DashboardMeta struct {
	UID     string
	Title   string
	Version int
}

// ParseDashboardMeta extracts uid, title, and version from a dashboard JSON
// model. Missing fields come back as zero values; it only errors when the
// model is not a JSON object.
func ParseDashboardMeta(model []byte) (DashboardMeta, error) {
	var raw struct {
		UID     string `json:"uid"`
		Title   string `json:"title"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(model, &raw); err != nil {
		return DashboardMeta{}, fmt.Errorf("dashboard model is not valid JSON object: %w", err)
	}
	return DashboardMeta{UID: raw.UID, Title: raw.Title, Version: raw.Version}, nil
}
