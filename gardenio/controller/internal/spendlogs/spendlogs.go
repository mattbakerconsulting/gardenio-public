// Package spendlogs recomputes per-agent LiteLLM cost for an arbitrary time
// window directly from GET /spend/logs/v2 — statelessly. Unlike the settlement
// path (cmd/controller/reconcile_session_spend.go), which anchors lifetime
// /spend/tags totals against baselines and watermarks persisted on agent
// beads, a projection here needs NO stored state: given (window, proxies) it
// pages the raw request rows and re-derives per-agent spend plus
// per-{role,project,model} rollups from the request tags the podspec stamps on
// every agent request (x-litellm-tags — see cmd/controller/podspec.go). The
// same answer is re-derivable after the fact, which is what makes it usable as
// an audit surface against the stateful path (plan ka-0000003747, C1/C2/C3).
//
// Endpoint facts (probed live against LiteLLM v1.84.8, ka-0000001899):
//
//   - /spend/logs/v2 is the paginated per-request endpoint. The legacy
//     /spend/logs is deprecated (unpaginated) and, with start/end dates,
//     returns per-DAY aggregates that carry no request tags.
//   - page_size is server-capped at 100; the response envelope is
//     {data, page, page_size, total, total_pages}.
//   - start_date/end_date accept ONLY "YYYY-MM-DD" or "YYYY-MM-DD HH:MM:SS"
//     (naive, interpreted as UTC). RFC3339 ("...T...Z") is rejected with 400.
//   - There is NO server-side tag filter: /spend/logs/ui advertises a
//     request_tags param but ignores it (a bogus tag returns the full,
//     unchanged total). Per-agent filtering is therefore client-side, which
//     means a single-agent query costs the same as a whole-fleet sweep of the
//     window — callers wanting several agents should Project() once and read
//     all of them from the result.
//   - Rows are heavy (~6.3 KB each; a page_size=100 page is ~640 KB, served in
//     ~0.2-0.6 s). Fleet volume at probe time was ~16k rows/day, so a full-day
//     sweep is ~160 pages / ~100 MB transfer. The Throttle knob spaces page
//     fetches to keep sustained sweeps polite.
//   - Row retention is bounded: the oldest retained row at probe time was
//     2026-06-26, while /spend/tags aggregates are lifetime-cumulative. A
//     projection over a window that predates retention UNDER-reads relative to
//     /spend/tags — that is a property of the source, not a bug here.
package spendlogs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tag prefixes stamped by the podspec (x-litellm-tags) on every agent request.
// The agent: prefix must match agentTag in cmd/controller/
// reconcile_session_spend.go; the dim prefixes mirror
// metrics.TagCostDims{role,project,model}. Kept as local constants so the
// library has no dependency on cmd/controller.
const (
	agentTagPrefix   = "agent:"
	roleTagPrefix    = "role:"
	projectTagPrefix = "project:"
	modelTagPrefix   = "model:"
)

// Defaults for the Client knobs. defaultMaxPages bounds a runaway window at
// ~200k rows (~12 days of fleet traffic at probe-time volume) — hitting it is
// an ERROR, never a silent truncation.
const (
	defaultPageSize = 100 // server cap; larger values 422
	defaultMaxPages = 2000
	defaultThrottle = 100 * time.Millisecond
)

// spendLogTimeFormat is the only datetime format /spend/logs/v2 accepts for
// start_date/end_date (naive UTC; RFC3339 is rejected with HTTP 400).
const spendLogTimeFormat = "2006-01-02 15:04:05"

// Client fetches and projects /spend/logs/v2 rows. Zero stored state between
// calls — every Project is a fresh read of the source of truth.
type Client struct {
	// MasterKey authenticates as the LiteLLM proxy admin; /spend/logs/v2
	// rejects virtual keys with 401.
	MasterKey string
	// HTTPClient is the transport. New sets a 30 s timeout — pages are ~640 KB
	// and served well under a second, so anything slower is a stuck proxy.
	HTTPClient *http.Client
	Logger     *slog.Logger
	// PageSize per fetch, clamped to the server cap of 100.
	PageSize int
	// MaxPages bounds one proxy's sweep. Exceeding it fails the Project with
	// ErrTooManyPages rather than silently truncating the window.
	MaxPages int
	// Throttle is the pause between successive page fetches on one proxy,
	// keeping sustained sweeps polite to the proxy's DB.
	Throttle time.Duration

	// sleep is the throttle sleeper, injectable so tests don't wall-clock
	// wait. Returns false if ctx was cancelled during the wait.
	sleep func(ctx context.Context, d time.Duration) bool
}

// New returns a Client with the probed-safe defaults documented on the
// constants above.
func New(masterKey string, logger *slog.Logger) *Client {
	return &Client{
		MasterKey:  masterKey,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Logger:     logger.With("component", "spendlogs"),
		PageSize:   defaultPageSize,
		MaxPages:   defaultMaxPages,
		Throttle:   defaultThrottle,
		sleep:      sleepCtx,
	}
}

// ErrTooManyPages is returned when one proxy's window exceeds MaxPages pages.
// The window must be narrowed (or MaxPages raised) — the alternative would be
// a silently partial sum presented as complete.
var ErrTooManyPages = fmt.Errorf("spendlogs: window exceeds MaxPages; narrow the window or raise MaxPages")

// Row is the subset of one /spend/logs/v2 entry the projection consumes. The
// full row carries ~30 more fields (metadata, token counts, session ids) that
// are decoded-and-dropped here.
type Row struct {
	RequestID   string   `json:"request_id"`
	APIKey      string   `json:"api_key"` // sha256 of the virtual key
	Model       string   `json:"model"`   // provider model, e.g. anthropic/claude-opus-4-8
	ModelGroup  string   `json:"model_group"`
	Spend       float64  `json:"spend"`
	StartTime   string   `json:"startTime"` // RFC3339 with offset, e.g. 2026-07-19T00:59:54.825+00:00
	Status      string   `json:"status"`
	RequestTags []string `json:"request_tags"`
}

// page mirrors the /spend/logs/v2 response envelope.
type page struct {
	Data       []Row `json:"data"`
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	Total      int   `json:"total"`
	TotalPages int   `json:"total_pages"`
}

// AgentSpend is one agent's share of a projection window.
type AgentSpend struct {
	USD      float64   `json:"usd"`
	Requests int       `json:"requests"`
	First    time.Time `json:"first"` // startTime of the agent's earliest in-window row
	Last     time.Time `json:"last"`  // startTime of the agent's latest in-window row
}

// ProxyStats records what one proxy's sweep actually fetched, so callers can
// audit coverage (e.g. total drift from rows landing mid-sweep).
type ProxyStats struct {
	BaseURL string `json:"base_url"`
	Pages   int    `json:"pages"`
	// ReportedTotal is the server's row count for the window as of the FIRST
	// page; RowsFetched is what the sweep actually saw. They differ when rows
	// land mid-sweep (live traffic) — the asc-sort order below keeps such rows
	// from shifting already-read pages, and Duplicates counts request_ids seen
	// twice (deduped, first occurrence kept).
	ReportedTotal int `json:"reported_total"`
	RowsFetched   int `json:"rows_fetched"`
	Duplicates    int `json:"duplicates"`
	// OutOfWindow counts fetched rows the strict client-side [From,To) filter
	// rejected (the server window is requested one second wide on each side
	// because its boundary inclusivity is undocumented).
	OutOfWindow int `json:"out_of_window"`
}

// Projection is the stateless spend picture of one window across the swept
// proxies: the full bill of every row in-window, its per-agent split, and
// per-{role,project,model} rollups from the same rows. Rows carrying no tag of
// a given dimension simply don't contribute to that dimension's map (their
// spend is still in TotalUSD and UntaggedAgentUSD tracks the agent: gap).
type Projection struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// TotalUSD sums EVERY in-window row, tagged or not — the window's whole
	// bill across the swept proxies (the /global/spend analogue).
	TotalUSD float64 `json:"total_usd"`
	Requests int     `json:"requests"`
	// Agents is keyed by agent name (the agent: tag value).
	Agents map[string]AgentSpend `json:"agents"`
	// Roles/Projects/Models are keyed by the role:/project:/model: tag values.
	// model: is the model GROUP alias the podspec stamps; Row.Model has the
	// finer provider-model resolution when needed.
	Roles    map[string]float64 `json:"roles"`
	Projects map[string]float64 `json:"projects"`
	Models   map[string]float64 `json:"models"`
	// UntaggedAgentUSD is the in-window spend carrying NO agent: tag — the
	// per-agent-unattributable residual (ka-0000003988's split, per-window).
	UntaggedAgentUSD float64      `json:"untagged_agent_usd"`
	Proxies          []ProxyStats `json:"proxies"`
}

// AgentUSD returns the window spend for one agent name (0 when absent).
func (p *Projection) AgentUSD(agentName string) float64 {
	return p.Agents[agentName].USD
}

// Project sweeps [from,to) over every proxy base URL and returns the merged
// projection. The window is half-open on row startTime, enforced client-side.
// Any page fetch error fails the whole call — a partial sum silently presented
// as complete would defeat the audit purpose. proxyURLs are deduplicated after
// trailing-slash trimming; the fleet's true spend for a tag is the sum across
// proxies, since each request is metered by exactly one proxy (see
// cmd/controller/tag_spend_sync.go).
func (c *Client) Project(ctx context.Context, proxyURLs []string, from, to time.Time) (*Projection, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("spendlogs: empty window: from %s is not before to %s", from, to)
	}
	proj := &Projection{
		From:     from,
		To:       to,
		Agents:   make(map[string]AgentSpend),
		Roles:    make(map[string]float64),
		Projects: make(map[string]float64),
		Models:   make(map[string]float64),
	}
	seen := make(map[string]bool) // proxy URL dedup
	for _, raw := range proxyURLs {
		baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
		if baseURL == "" || seen[baseURL] {
			continue
		}
		seen[baseURL] = true
		stats, err := c.sweepProxy(ctx, baseURL, from, to, proj)
		if err != nil {
			return nil, fmt.Errorf("spendlogs: proxy %s: %w", baseURL, err)
		}
		proj.Proxies = append(proj.Proxies, stats)
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("spendlogs: no proxy URLs given")
	}
	return proj, nil
}

// sweepProxy pages one proxy's window into proj, returning the fetch stats.
//
// Pagination stability: pages are requested sorted by startTime ASCENDING so
// rows landing mid-sweep (live traffic has monotonically increasing
// startTime) append past the cursor instead of shifting every page under it —
// the desc default would re-serve already-read rows on every insert. LiteLLM's
// spend writer flushes in batches with up to ~a minute of lag, so a row CAN
// still land behind the cursor; request_id dedup absorbs the re-read case, and
// windows should end a couple of minutes before now when exactness matters
// (the flush-lag tail is not yet queryable at all — no read strategy fixes
// that).
func (c *Client) sweepProxy(ctx context.Context, baseURL string, from, to time.Time, proj *Projection) (ProxyStats, error) {
	stats := ProxyStats{BaseURL: baseURL}
	seenReq := make(map[string]bool)
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}
	for pageNum := 1; ; pageNum++ {
		if pageNum > maxPages {
			return stats, fmt.Errorf("%w (fetched %d pages of %d reported rows)", ErrTooManyPages, stats.Pages, stats.ReportedTotal)
		}
		if pageNum > 1 && c.Throttle > 0 {
			if !c.sleep(ctx, c.Throttle) {
				return stats, ctx.Err()
			}
		}
		pg, err := c.fetchPage(ctx, baseURL, from, to, pageNum)
		if err != nil {
			return stats, err
		}
		stats.Pages++
		if pageNum == 1 {
			stats.ReportedTotal = pg.Total
		}
		for _, row := range pg.Data {
			stats.RowsFetched++
			if row.RequestID != "" && seenReq[row.RequestID] {
				stats.Duplicates++
				continue
			}
			if row.RequestID != "" {
				seenReq[row.RequestID] = true
			}
			ts, ok := parseRowTime(row.StartTime)
			if !ok || ts.Before(from) || !ts.Before(to) {
				stats.OutOfWindow++
				continue
			}
			foldRow(proj, row, ts)
		}
		if pageNum >= pg.TotalPages || len(pg.Data) == 0 {
			break
		}
	}
	return stats, nil
}

// foldRow books one in-window row into the projection maps.
func foldRow(proj *Projection, row Row, ts time.Time) {
	proj.TotalUSD += row.Spend
	proj.Requests++
	hasAgent := false
	for _, tag := range row.RequestTags {
		switch {
		case strings.HasPrefix(tag, agentTagPrefix):
			// The podspec stamps at most one agent: tag per request; if more
			// ever appear the row books to each (visible as sum(Agents) >
			// TotalUSD − UntaggedAgentUSD rather than silently dropped).
			hasAgent = true
			name := tag[len(agentTagPrefix):]
			a := proj.Agents[name]
			a.USD += row.Spend
			a.Requests++
			if a.First.IsZero() || ts.Before(a.First) {
				a.First = ts
			}
			if ts.After(a.Last) {
				a.Last = ts
			}
			proj.Agents[name] = a
		case strings.HasPrefix(tag, roleTagPrefix):
			proj.Roles[tag[len(roleTagPrefix):]] += row.Spend
		case strings.HasPrefix(tag, projectTagPrefix):
			proj.Projects[tag[len(projectTagPrefix):]] += row.Spend
		case strings.HasPrefix(tag, modelTagPrefix):
			proj.Models[tag[len(modelTagPrefix):]] += row.Spend
		}
	}
	if !hasAgent {
		proj.UntaggedAgentUSD += row.Spend
	}
}

// fetchPage GETs one /spend/logs/v2 page. The server window is widened by one
// second on each side because its boundary inclusivity is undocumented; the
// caller's strict [from,to) filter runs client-side on row startTime.
func (c *Client) fetchPage(ctx context.Context, baseURL string, from, to time.Time, pageNum int) (*page, error) {
	pageSize := c.PageSize
	if pageSize <= 0 || pageSize > defaultPageSize {
		pageSize = defaultPageSize
	}
	q := url.Values{}
	q.Set("start_date", from.UTC().Add(-time.Second).Format(spendLogTimeFormat))
	q.Set("end_date", to.UTC().Add(time.Second).Format(spendLogTimeFormat))
	q.Set("page", strconv.Itoa(pageNum))
	q.Set("page_size", strconv.Itoa(pageSize))
	q.Set("sort_by", "startTime")
	q.Set("sort_order", "asc")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/spend/logs/v2?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET /spend/logs/v2 page %d: HTTP %d: %s", pageNum, resp.StatusCode, truncate(string(body), 200))
	}
	var pg page
	if err := json.NewDecoder(resp.Body).Decode(&pg); err != nil {
		return nil, fmt.Errorf("decoding /spend/logs/v2 page %d: %w", pageNum, err)
	}
	return &pg, nil
}

// parseRowTime parses a /spend/logs/v2 startTime. Live rows come back RFC3339
// with an explicit offset ("2026-07-19T00:59:54.825+00:00"); the bare
// datetime and date-only fallbacks mirror parseSpendTime in
// cmd/controller/spend_sync.go for older/edge encodings.
func parseRowTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// sleepCtx sleeps for d or until ctx is cancelled, returning false on
// cancellation.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
