package main

// webhook_dark.go — the server-side read of the bridge webhook "dark-hook"
// deadman verdict, surfaced on the /health "Bridges & controller" tile
// (GAP-D of the non-AVA webhook carve-out, epic ka-0000000914 via
// ka-0000005632; gardenio/docs/security/non-ava-webhook-carveout.md).
//
// WHY. The bridges tile classifies each source by `last_event_at` freshness
// (health.go buildBridgesTileView), which cannot tell "healthy and quiet"
// apart from "silently dark" — the exact blind spot that let the Jira hook
// stay dark in prod for 8 days (bug ka-0000000426). The authoritative dark
// verdict is the Go-computed deadman gauge each bridge exports —
// gardenio_jira_webhook_dark / gardenio_gitlab_webhook_dark (0/1, set once per
// evaluation cycle in internal/bridge/*_webhook_deadman.go): 1 while no
// authenticated delivery arrived within the deadman window AND an independent
// upstream-activity scan still shows traffic. Reading that gauge lets the tile
// escalate a row to a loud "dark" state even when a stale last-event bead
// would otherwise read merely "slow"/"stale" (or, on a truly silent hook that
// never even ingested a bead, "unknown").
//
// HOW. One vmsingle instant query per dark-capable bridge, reusing the exact
// round-trip the /costs headline uses (costs_rate.go: vmsingleInstantQueryPath,
// vmInstantResponse, the metricsUpstream/metricsHTTP client). The query is a
// bare gauge selector, so parseWebhookDark reads the single scalar sample.
//
// GRACEFUL DEGRADATION is the whole point of the availability tri-state:
//   - no vmsingle upstream wired (local dev / tests) -> unknown -> tile
//     behavior is byte-identical to before this change.
//   - upstream error / non-200 / unparseable / absent metric (deadman monitor
//     not running in this env — it only runs where the group scope is set) ->
//     unknown -> age-based classification stands.
//   - a present 0 sample -> NOT dark (an explicit "monitor ran, hook is live"
//     signal we must not conflate with "metric absent").
//   - a present 1 sample -> dark -> row escalates regardless of last-event age.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// webhookDarkQuery maps a bridge source label to the PromQL selector for its
// dark-hook deadman gauge. Only the two non-AVA webhook doors (jira, gitlab)
// export a deadman verdict; slack-bridge and the controller have no such gauge
// and are absent from this map (their rows keep the age-only classification).
//
// The selectors are bare gauge names — the deadman sets a single unlabelled
// series per cycle (internal/bridge/*_webhook_deadman.go), so `max(<gauge>)`
// collapses any pod-churn duplicates to the one 0/1 verdict without needing a
// by-clause. See ka-0000000170 for the same max-dedup rationale on the cost
// gauges scraped across agent-pod churn.
var webhookDarkQuery = map[string]string{
	"source:jira":   "max(gardenio_jira_webhook_dark)",
	"source:gitlab": "max(gardenio_gitlab_webhook_dark)",
}

// webhookDarkState is the tri-state result of reading a bridge's deadman gauge.
// The middle state (Unknown) is load-bearing: it is what makes the tile
// degrade to its prior age-only behavior when no verdict is available, rather
// than defaulting a missing metric to either "dark" (false alarm) or "live"
// (false all-clear).
type webhookDarkState int

const (
	// webhookDarkUnknown — no verdict available (no upstream, error, or the
	// metric is absent because the deadman monitor isn't running here).
	webhookDarkUnknown webhookDarkState = iota
	// webhookDarkLive — the monitor ran and the hook is NOT dark (gauge 0).
	webhookDarkLive
	// webhookDarkDark — the monitor ran and the hook IS dark (gauge 1):
	// no authenticated delivery within the window while upstream is active.
	webhookDarkDark
)

// webhookDarkForSource returns the dark verdict for one bridge source label.
// Sources without a deadman gauge (anything not in webhookDarkQuery) return
// Unknown without a round-trip. Availability semantics mirror
// queryFleetRate (costs_rate.go): any missing-upstream / transport / shape
// failure collapses to Unknown so the caller falls back to age classification.
func (s *Server) webhookDarkForSource(ctx context.Context, sourceLabel string) webhookDarkState {
	query, ok := webhookDarkQuery[sourceLabel]
	if !ok {
		return webhookDarkUnknown
	}
	if s == nil || s.metricsUpstream == "" {
		return webhookDarkUnknown
	}
	client := s.metricsHTTP
	if client == nil {
		client = &http.Client{Timeout: metricsProxyTimeout}
	}

	u := s.metricsUpstream + vmsingleInstantQueryPath + "?query=" + url.QueryEscape(query)
	ctx, cancel := context.WithTimeout(ctx, metricsProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("webhook dark: build request", "source", sourceLabel, "error", err)
		}
		return webhookDarkUnknown
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("webhook dark: upstream error", "source", sourceLabel, "error", err)
		}
		return webhookDarkUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if s.logger != nil {
			s.logger.Warn("webhook dark: upstream status", "source", sourceLabel, "status", resp.StatusCode)
		}
		return webhookDarkUnknown
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("webhook dark: read body", "source", sourceLabel, "error", err)
		}
		return webhookDarkUnknown
	}
	return parseWebhookDark(body)
}

// parseWebhookDark extracts the 0/1 dark verdict from a vmsingle instant
// "vector" response. Split out so the JSON-shape handling is unit-testable
// without an HTTP round-trip (mirrors parseFleetHourlyRate). An empty result
// vector (metric absent) is Unknown; a present sample is Dark iff it rounds to
// a positive value, Live otherwise — the deadman only ever sets an exact 0 or
// 1, so the >= 0.5 threshold is just NaN/precision defence.
func parseWebhookDark(body []byte) webhookDarkState {
	var parsed vmInstantResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return webhookDarkUnknown
	}
	if parsed.Status != "success" || len(parsed.Data.Result) == 0 {
		return webhookDarkUnknown
	}
	raw := parsed.Data.Result[0].Value[1]
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return webhookDarkUnknown
	}
	str = strings.TrimSpace(str)
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return webhookDarkUnknown
	}
	// Reject NaN/Inf (vmsingle can emit "NaN"/"+Inf", which ParseFloat accepts)
	// as an unusable verdict rather than misreading it as live/dark.
	if v != v || v > 1e300 || v < -1e300 {
		return webhookDarkUnknown
	}
	if v >= 0.5 {
		return webhookDarkDark
	}
	return webhookDarkLive
}
