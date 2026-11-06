package main

// Mezmo live-data wiring for incident-intel widgets.
// Bead: ka-0000002519.
// Plan-of-record: docs/research/incident-intelligence/synthesis.md (Phase 7-8 follow-on).
//
// Widget-instance beads can carry a `mezmo_source` field shaped like:
//
//	{
//	  "query":  "<plain mezmo query>",
//	  "window": "5m"|"1h"|"24h"|...,
//	  "metric": "count"|"p95"|...,
//	  "field":  "<group-by field>"  // optional, only group_logs_by_field uses it
//	}
//
// On render, applyMezmoSource resolves a slug → MCP method, fetches fresh data
// through the per-process MezmoClient (LRU-cached on (slug,query,window,field)),
// and splices the result back into the raw widget payload before the renderer
// sees it. Any error path returns the original payload unchanged so the widget
// falls back to the baked-in fields the gardener emitted with `gb widget emit`.
//
// The MezmoClient interface keeps the caller-side mockable; the production
// implementation lives in widgets_mezmo_http.go (env-configured base URL +
// service-key bearer auth). When no client is wired (default), all calls are
// no-ops and the page degrades cleanly.

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MezmoSource is the parsed shape of the `mezmo_source` field on a widget-
// instance bead. Empty Query disables the lookup (treated as "no source").
type MezmoSource struct {
	Query  string `json:"query"`
	Window string `json:"window"`
	Metric string `json:"metric"`
	Field  string `json:"field"`
}

// parseMezmoSource decodes the field. Empty or malformed input returns the
// zero value with ok=false; callers treat that as "no live data path".
func parseMezmoSource(raw string) (MezmoSource, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return MezmoSource{}, false
	}
	var m MezmoSource
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return MezmoSource{}, false
	}
	if strings.TrimSpace(m.Query) == "" {
		return MezmoSource{}, false
	}
	return m, true
}

// HistogramPoint is one bucket from get_log_histogram. Time is unix seconds.
type HistogramPoint struct {
	Time  int64
	Value float64
}

// GroupBucket is one row from group_logs_by_field.
type GroupBucket struct {
	Key   string
	Count int64
}

// LogEntry is one deduplicated entry from deduplicate_logs_relative_time.
type LogEntry struct {
	When  time.Time
	Actor string
	Kind  string
	Text  string
}

// MezmoClient is the small surface widget rendering needs from Mezmo. The
// production implementation hits the Mezmo MCP HTTP server with a service key.
// The default (nil-safe through noopMezmoClient) returns ErrMezmoUnavailable
// so apply paths fall back to baked fields without a remote call.
type MezmoClient interface {
	Histogram(ctx context.Context, query, window string) ([]HistogramPoint, error)
	GroupBy(ctx context.Context, query, field, window string) ([]GroupBucket, error)
	Dedup(ctx context.Context, query, window string, limit int) ([]LogEntry, error)
}

// ErrMezmoUnavailable is returned by the no-op client and surfaces in tests
// so the apply pipeline can distinguish "not configured" from "transient
// failure" without crossing the network.
var ErrMezmoUnavailable = fmt.Errorf("mezmo client not configured")

// noopMezmoClient is the zero-value default. All methods return
// ErrMezmoUnavailable; the apply pipeline catches the error and leaves the
// payload alone, so the widget renders from baked fields.
type noopMezmoClient struct{}

func (noopMezmoClient) Histogram(context.Context, string, string) ([]HistogramPoint, error) {
	return nil, ErrMezmoUnavailable
}
func (noopMezmoClient) GroupBy(context.Context, string, string, string) ([]GroupBucket, error) {
	return nil, ErrMezmoUnavailable
}
func (noopMezmoClient) Dedup(context.Context, string, string, int) ([]LogEntry, error) {
	return nil, ErrMezmoUnavailable
}

// applyMezmoSource is the slug-aware splice. For a registered slug it calls
// the matching MezmoClient method, merges the fresh data into the payload,
// and returns the new JSON. On any error (including ErrMezmoUnavailable) it
// returns rawJSON unchanged. Unknown slugs are also a no-op so adding a new
// widget with a mezmo_source doesn't accidentally break the page.
func applyMezmoSource(ctx context.Context, rawJSON string, source MezmoSource, slug string, client MezmoClient, cache *mezmoCache) string {
	if client == nil || source.Query == "" {
		return rawJSON
	}
	window := source.Window
	if window == "" {
		window = "1h"
	}
	switch slug {
	case "vm-sparklines":
		points, err := mezmoFetch(ctx, cache, slug, source.Query, window, source.Field, func() ([]HistogramPoint, error) {
			return client.Histogram(ctx, source.Query, window)
		})
		if err != nil || len(points) == 0 {
			return rawJSON
		}
		return spliceSparklinePoints(rawJSON, points, source)
	case "event-timeline":
		points, err := mezmoFetch(ctx, cache, slug, source.Query, window, source.Field, func() ([]HistogramPoint, error) {
			return client.Histogram(ctx, source.Query, window)
		})
		if err != nil || len(points) == 0 {
			return rawJSON
		}
		return spliceTimelineEvents(rawJSON, points, source)
	case "service-health":
		field := source.Field
		if field == "" {
			field = "app"
		}
		buckets, err := mezmoFetch(ctx, cache, slug, source.Query, window, field, func() ([]GroupBucket, error) {
			return client.GroupBy(ctx, source.Query, field, window)
		})
		if err != nil || len(buckets) == 0 {
			return rawJSON
		}
		return spliceServiceHealth(rawJSON, buckets)
	case "activity-log":
		entries, err := mezmoFetch(ctx, cache, slug, source.Query, window, source.Field, func() ([]LogEntry, error) {
			return client.Dedup(ctx, source.Query, window, 50)
		})
		if err != nil || len(entries) == 0 {
			return rawJSON
		}
		return spliceActivityLog(rawJSON, entries)
	}
	return rawJSON
}

// spliceSparklinePoints rewrites the first series' `points` array in a
// vm-sparklines payload with the histogram. Other series are left alone, so a
// payload that already carries multiple bake-in series degrades to the live
// one only swapping out. If there are no series, one is synthesized from the
// source metadata.
func spliceSparklinePoints(rawJSON string, points []HistogramPoint, source MezmoSource) string {
	var p map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &p); err != nil {
		return rawJSON
	}
	pts := make([]map[string]any, 0, len(points))
	for _, h := range points {
		pts = append(pts, map[string]any{
			"when":  time.Unix(h.Time, 0).UTC().Format(time.RFC3339),
			"value": h.Value,
		})
	}
	series, _ := p["series"].([]any)
	if len(series) == 0 {
		series = []any{map[string]any{
			"service": "mezmo",
			"metric":  source.Metric,
		}}
	}
	if first, ok := series[0].(map[string]any); ok {
		first["points"] = pts
		series[0] = first
	}
	p["series"] = series
	out, err := json.Marshal(p)
	if err != nil {
		return rawJSON
	}
	return string(out)
}

// spliceTimelineEvents synthesizes one event per histogram bucket for the
// event-timeline widget. Each event lands on the "mezmo" lane so the renderer
// preserves baked events from other lanes.
func spliceTimelineEvents(rawJSON string, points []HistogramPoint, source MezmoSource) string {
	var p map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &p); err != nil {
		return rawJSON
	}
	existing, _ := p["events"].([]any)
	for _, h := range points {
		if h.Value <= 0 {
			continue
		}
		existing = append(existing, map[string]any{
			"when":   time.Unix(h.Time, 0).UTC().Format(time.RFC3339),
			"kind":   "mezmo",
			"title":  fmt.Sprintf("%s: %s", source.Metric, formatFloat(h.Value)),
			"detail": source.Query,
		})
	}
	p["events"] = existing
	out, err := json.Marshal(p)
	if err != nil {
		return rawJSON
	}
	return string(out)
}

// spliceServiceHealth replaces the `services` array with one row per group
// bucket. State is derived from count: 0 → healthy, top-bucket → critical,
// others → warn. Caller can override by emitting a richer payload from the
// gardener (the splice only fires when there's no payload already).
func spliceServiceHealth(rawJSON string, buckets []GroupBucket) string {
	var p map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &p); err != nil {
		return rawJSON
	}
	rows := make([]map[string]any, 0, len(buckets))
	var top int64
	for _, b := range buckets {
		if b.Count > top {
			top = b.Count
		}
	}
	for _, b := range buckets {
		state := "healthy"
		switch {
		case b.Count == 0:
			state = "healthy"
		case b.Count == top && top > 0:
			state = "critical"
		case b.Count > 0:
			state = "warn"
		}
		rows = append(rows, map[string]any{
			"service": b.Key,
			"state":   state,
			"detail":  fmt.Sprintf("%d events", b.Count),
		})
	}
	p["services"] = rows
	out, err := json.Marshal(p)
	if err != nil {
		return rawJSON
	}
	return string(out)
}

// spliceActivityLog replaces the `entries` array with the deduplicated logs.
// Schema matches the activity-log widget's shape (when, actor, kind, text).
func spliceActivityLog(rawJSON string, entries []LogEntry) string {
	var p map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &p); err != nil {
		return rawJSON
	}
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, map[string]any{
			"when":  e.When.UTC().Format(time.RFC3339),
			"actor": e.Actor,
			"kind":  e.Kind,
			"text":  e.Text,
		})
	}
	p["entries"] = rows
	out, err := json.Marshal(p)
	if err != nil {
		return rawJSON
	}
	return string(out)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ---- LRU cache -------------------------------------------------------------
//
// One process-wide cache, sized by entry count, keyed by (slug,query,window,
// field). Entries hold the typed result + an absolute expiry; expired entries
// are evicted on access. Window-derived TTL means a 5m window refreshes much
// more aggressively than a 24h window — bounded staleness either way.

type mezmoCacheEntry struct {
	value  any
	expiry time.Time
	key    string
	elem   *list.Element
}

// mezmoCache is a tiny size-bounded LRU. The zero value is unusable; build
// with newMezmoCache(n).
type mezmoCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	by    map[string]*mezmoCacheEntry
}

func newMezmoCache(max int) *mezmoCache {
	if max <= 0 {
		max = 64
	}
	return &mezmoCache{max: max, order: list.New(), by: make(map[string]*mezmoCacheEntry, max)}
}

func mezmoCacheKey(slug, query, window, field string) string {
	return slug + "|" + query + "|" + window + "|" + field
}

// windowTTL maps a window string to a cache TTL. Fall-through default is 30s
// so an unparseable window still caches briefly (avoids hammering Mezmo from a
// page-render storm). Caller-passed values are minute/hour units only.
func windowTTL(window string) time.Duration {
	w := strings.TrimSpace(strings.ToLower(window))
	if w == "" {
		return 30 * time.Second
	}
	unit := w[len(w)-1]
	num, err := strconv.Atoi(w[:len(w)-1])
	if err != nil || num <= 0 {
		return 30 * time.Second
	}
	switch unit {
	case 's':
		return time.Duration(num) * time.Second
	case 'm':
		// quarter-window TTL caps at 60s so a 24h window with metric=count
		// still updates frequently enough to feel live.
		ttl := min(time.Duration(num)*time.Minute/4, 60*time.Second)
		ttl = max(ttl, 5*time.Second)
		return ttl
	case 'h':
		return 60 * time.Second
	case 'd':
		return 5 * time.Minute
	}
	return 30 * time.Second
}

// mezmoFetch is the cache-aware fetch. Hits return the cached value typed
// back via the generic parameter; misses run fetch(), store the result, and
// return it. Errors from fetch are not cached — the next render will retry.
func mezmoFetch[T any](ctx context.Context, c *mezmoCache, slug, query, window, field string, fetch func() (T, error)) (T, error) {
	var zero T
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	// The cache is process-wide and keyed by query, not by viewer, so a scoped
	// principal neither reads what another viewer cached nor fills it: it goes
	// to Mezmo live every time.
	if isScopedCtx(ctx) {
		c = nil
	}
	key := mezmoCacheKey(slug, query, window, field)
	if c != nil {
		if v, ok := c.get(key); ok {
			if t, ok2 := v.(T); ok2 {
				return t, nil
			}
		}
	}
	v, err := fetch()
	if err != nil {
		return zero, err
	}
	if c != nil {
		c.set(key, v, windowTTL(window))
	}
	return v, nil
}

func (c *mezmoCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.by[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiry) {
		c.order.Remove(e.elem)
		delete(c.by, key)
		return nil, false
	}
	c.order.MoveToFront(e.elem)
	return e.value, true
}

func (c *mezmoCache) set(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.by[key]; ok {
		e.value = value
		e.expiry = time.Now().Add(ttl)
		c.order.MoveToFront(e.elem)
		return
	}
	for c.order.Len() >= c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		k := oldest.Value.(string)
		c.order.Remove(oldest)
		delete(c.by, k)
	}
	elem := c.order.PushFront(key)
	c.by[key] = &mezmoCacheEntry{value: value, expiry: time.Now().Add(ttl), key: key, elem: elem}
}

// ---- Refresh / live-poll wiring -------------------------------------------

// mezmoLiveInterval is the default polling cadence for mezmo-sourced widgets.
// A value of 0 disables polling (historical reports, archived incidents).
// Callers pass the report's "active"/"archived" mode and we pick from there.
type mezmoLiveMode string

const (
	mezmoLiveActive   mezmoLiveMode = "active"
	mezmoLiveArchived mezmoLiveMode = "archived"
)

// mezmoLiveIntervalSecs returns the per-widget polling cadence in seconds for
// mezmo-sourced widgets. Active reports refresh every 30s; archived reports
// don't poll. Window-aligned refresh would be nicer but 30s is the synthesis
// doc's default and matches the operator-visible "live" feel.
func mezmoLiveIntervalSecs(mode mezmoLiveMode) int {
	switch mode {
	case mezmoLiveArchived:
		return 0
	case mezmoLiveActive:
		return 30
	}
	return 30
}

// RefreshWidgetAction returns an htmx-get descriptor that re-fetches a single
// widget fragment from the live route. SwapInto is the CSS selector of the
// widget body; the dispatcher swaps innerHTML there.
func RefreshWidgetAction(slug, beadID, swapInto string) ActionDescriptor {
	return ActionDescriptor{
		ID:       "refresh-widget",
		Kind:     ActionHtmxGet,
		Label:    "Refresh",
		Target:   "/widgets/" + slug + "/" + beadID,
		SwapInto: swapInto,
		Hint:     "Re-fetch live data for this widget",
	}
}
