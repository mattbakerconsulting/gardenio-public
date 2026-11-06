package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tagSpendReader is the controller's only reader of LiteLLM GET /spend/tags
// (gd-oih6JxjJMM). LiteLLM serves that endpoint with one GROUP BY over every
// row of LiteLLM_SpendLogs: there is no index for it and it ignores
// start_date/end_date, so each call is a full-table scan. On 2026-10-31 a scan
// took 7-16 s on the platform node, and the callers (the tag gauge, the spawn
// baseline, the close-time reconcile, its retry worker and the settlement
// sweep) each issued their own with a 10 s timeout. Postgres kept running
// every scan the controller hung up on, so they piled up (5 at once) and held
// the node at 100% CPU.
//
// Every caller now shares one snapshot per proxy:
//
//   - At most one read per proxy per minInterval, whether it succeeds or fails.
//   - At most one read per proxy in flight; a caller that needs one while it
//     runs joins it instead of starting another.
//   - A read is detached from the caller's context and allowed
//     tagSpendReadTimeout, which is longer than the LiteLLM database's
//     statement_timeout (chart litellm.dbInit.statementTimeout). Postgres
//     cancels a slow scan and LiteLLM answers with an error before the
//     controller would hang up, so the controller never leaves a scan running.
//
// The snapshot can be up to minInterval old. Lifetime totals only grow, so a
// stale total under-reads, and every settlement is idempotent by watermark:
// the settlement sweep (spend_settlement_sweep.go, every 30 min) books what a
// stale close-time read missed.
//
// source picks where the totals come from (gd-Um9gnz9FuW): /spend/tags, the
// LiteLLM_DailyTagSpend aggregate (tag_spend_daily.go), or auto, which reads
// both until every tag the controller uses agrees and then reads only the
// aggregate. Spend-log retention needs the aggregate: /spend/tags sums the
// LiteLLM_SpendLogs rows that are left, so a cleanup would shrink every total.
type tagSpendReader struct {
	masterKey string
	// source is where Read's totals come from. Constructors default to
	// tagSourceSpendTags; main sets the configured one before the first Read.
	source tagSpendSource
	// minInterval is the least time between two reads of one proxy. 0 makes
	// the reader uncached: every Read waits for a read of its own (or joins
	// the one in flight). Constructors default to that so tests see each
	// server change; main wires one shared reader at tagSpendReadInterval.
	minInterval time.Duration
	httpClient  *http.Client
	logger      *slog.Logger
	now         func() time.Time

	mu      sync.Mutex
	proxies map[string]*tagSpendProxy
}

// tagSpendProxy is one proxy's shared /spend/tags state. All fields are
// guarded by tagSpendReader.mu.
type tagSpendProxy struct {
	entries   []tagSpendEntry // last good read; nil until one succeeds
	attempted time.Time       // start of the last read, good or bad
	lastErr   error           // error of the last read; nil after a good one
	flight    chan struct{}   // non-nil while a read runs; closed when it ends

	// The fields below belong to the read in flight: only fetch touches
	// them, and at most one fetch per proxy runs at a time.
	daily   *dailyTagTotals   // DailyTagSpend totals of settled days; nil until a rebuild succeeds
	cutOver bool              // auto: parity shown, so reads skip /spend/tags
	gaps    map[string]tagGap // auto: the previous comparison's mismatched tags
}

const (
	// tagSpendReadInterval is the shared reader's minInterval. It matches
	// tagSpendSyncInterval, so the gauge poller is the usual reader and the
	// reconciler paths ride its snapshot.
	tagSpendReadInterval = 5 * time.Minute
	// tagSpendReadTimeout bounds one /spend/tags read. Keep it above the
	// LiteLLM database statement_timeout (chart default 20s; see the type doc).
	tagSpendReadTimeout = 60 * time.Second
	// tagSpendFetchTimeout bounds one whole read, which may be several
	// requests (see fetch).
	tagSpendFetchTimeout = 3 * time.Minute
)

func newTagSpendReader(masterKey string, minInterval time.Duration, logger *slog.Logger) *tagSpendReader {
	return &tagSpendReader{
		masterKey:   masterKey,
		minInterval: minInterval,
		source:      tagSourceSpendTags,
		httpClient:  &http.Client{Timeout: tagSpendReadTimeout},
		logger:      logger.With("component", "tag-spend-reader"),
		now:         time.Now,
		proxies:     make(map[string]*tagSpendProxy),
	}
}

// Read returns the proxy's /spend/tags entries. The returned slice is shared
// with other callers and must not be modified.
//
// It starts a read when the last one began at least minInterval ago and none
// is in flight. With wait=false a caller that already has a snapshot gets it
// at once, and the read refreshes it in the background; this is for the
// spawn and close paths, which must not stall on a scan. With wait=true, or
// when there is no snapshot yet, the caller waits for the read in flight.
// When the last read failed and the next is not due, Read returns the last
// good snapshot, or the last error if there is none.
func (r *tagSpendReader) Read(ctx context.Context, baseURL string, wait bool) ([]tagSpendEntry, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	r.mu.Lock()
	p := r.proxies[baseURL]
	if p == nil {
		p = &tagSpendProxy{}
		r.proxies[baseURL] = p
	}
	now := r.now()
	if p.flight == nil && (p.attempted.IsZero() || now.Sub(p.attempted) >= r.minInterval) {
		p.flight = make(chan struct{})
		p.attempted = now
		go r.fetch(ctx, baseURL, p, p.flight)
	}
	if r.minInterval <= 0 {
		wait = true
	}
	flight := p.flight
	if flight == nil || (!wait && p.entries != nil) {
		entries, err := p.entries, p.lastErr
		r.mu.Unlock()
		if entries != nil {
			return entries, nil
		}
		return nil, err
	}
	r.mu.Unlock()

	select {
	case <-flight:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.entries != nil {
		return p.entries, nil
	}
	return nil, p.lastErr
}

// fetch runs one read of baseURL, stores the outcome on p and ends the flight.
// It ignores the caller's cancellation: other callers may be waiting on it,
// and hanging up would leave LiteLLM's scan running.
func (r *tagSpendReader) fetch(ctx context.Context, baseURL string, p *tagSpendProxy, flight chan struct{}) {
	// One read can be several requests (a DailyTagSpend rebuild pages through
	// every settled day), so the whole read gets tagSpendFetchTimeout and each
	// request the client's tagSpendReadTimeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tagSpendFetchTimeout)
	defer cancel()
	start := r.now()
	var entries []tagSpendEntry
	var err error
	switch {
	case r.source == tagSourceDaily, r.source == tagSourceAuto && p.cutOver:
		entries, err = r.readDaily(ctx, baseURL, p)
	case r.source == tagSourceAuto:
		entries, err = r.readAuto(ctx, baseURL, p)
	default:
		entries, err = r.get(ctx, baseURL)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		p.lastErr = err
		r.logger.Warn("tag spend read failed", "proxy", baseURL, "source", r.source, "error", err,
			"duration", r.now().Sub(start), "have_snapshot", p.entries != nil)
	} else {
		if entries == nil {
			entries = []tagSpendEntry{}
		}
		p.entries, p.lastErr = entries, nil
		r.logger.Debug("tag spend read", "proxy", baseURL, "source", r.source, "tags", len(entries),
			"duration", r.now().Sub(start))
	}
	p.flight = nil
	close(flight)
}

// get issues GET /spend/tags against one proxy.
func (r *tagSpendReader) get(ctx context.Context, baseURL string) ([]tagSpendEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/spend/tags", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.masterKey)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 120))
	}

	var entries []tagSpendEntry
	if derr := json.NewDecoder(resp.Body).Decode(&entries); derr != nil {
		return nil, fmt.Errorf("decoding /spend/tags response: %w", derr)
	}
	return entries, nil
}
