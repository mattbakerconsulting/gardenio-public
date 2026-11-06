package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// tagSpendSync populates the real-bill per-dimension gauge gardenio_tag_cost_usd
// (ka-0000000478). It is the dimensional counterpart of spendSync: where spendSync
// polls LiteLLM GET /key/info per account and writes the per-account total onto
// the account bead (which the metrics Collector then exposes as
// gardenio_account_cost_usd{account,env}), tagSpendSync polls GET /spend/tags and
// exposes the SAME real bill sliced by {role,project,model} via the tag the
// podspec sets on every request (x-litellm-tags).
//
// Why a gauge poller and not the at-close counter: gardenio_agent_spend_usd_total
// only ticks once per session at close, so it lags ~1d and reads multiples LOW
// versus the live bill — the exact reason the /costs headline was re-pointed onto
// the account-cost gauge (ka-0000004779). This poller gives the by-role/by-project/
// by-model rollups the same real-bill treatment the by-account rollup already got.
//
// Multi-proxy: the fleet runs more than one LiteLLM proxy (e.g. an external proxy
// and an in-cluster one), each with its OWN spend database, and a given request
// hits exactly one of them. So the true spend for, say, role:gardener is the SUM of
// role:gardener across every proxy. tagSpendSync enumerates the distinct LiteLLM
// base URLs from the account beads (plus the controller-global LITELLM_URL), polls
// each, and sums per (dim,value) before setting the gauge.
//
// It polls the LiteLLM endpoint (account base_url, else the global URL) — NOT an
// account's proxy_url. proxy_url (claudemore) is a transparent record/replay
// fronting proxy that forwards to the SAME litellm and never meters spend itself
// ("litellm still sees every request, so spend tagging is unchanged",
// podspec.go); the spend lives in litellm. This mirrors spend_sync.go and
// reconcile_session_spend.go, which also resolve spend against base_url only.
// Polling proxy_url too would double-count the same bill (or just fail — claudemore
// has no /spend/tags endpoint).
//
// Resilience: a single proxy being unreachable for one tick must not dent the
// gauge (which would dip increase() over that window). The poller therefore keeps
// the LAST GOOD per-proxy snapshot and only overwrites a proxy's contribution on a
// successful read; a failed proxy carries its previous snapshot forward. The gauge
// is set from the merged (summed) view every tick, so the snapshot it publishes is
// always complete.
//
// It additionally publishes the attribution split of the SAME bill (Approach B,
// ka-0000003988): gardenio_agent_attributed_cost_usd (the agent:*-tagged part —
// what per-agent settlement can ever book) and gardenio_unattributed_cost_usd
// (each proxy's /global/spend bill minus its agent-tagged total — the residual
// no settlement can attribute). attributed + unattributed ≡ the full bill by
// construction every tick, which is what makes the residual an alertable
// quality signal rather than a vibe (UnattributedSpendHigh gap-alert).
type tagSpendSync struct {
	daemon     *beadsapi.Client
	litellmURL string
	masterKey  string
	env        string
	httpClient *http.Client
	logger     *slog.Logger
	metrics    *gardeniometrics.SpendMetrics

	// tags reads GET /spend/tags; main shares it with the session reconciler
	// (see tagSpendReader). The constructor's default is uncached.
	tags *tagSpendReader

	// lastGood holds, per proxy base URL, the most recent successful spend
	// snapshot read from that proxy (/spend/tags + /global/spend). Carried
	// forward across a failed poll so one flaky proxy never zeroes its
	// contribution. Only ever touched from the single RunLoop goroutine, so it
	// needs no mutex.
	lastGood map[string]proxySpendSnapshot
}

// tagDimValue keys a (dimension, value) cell of the real-bill breakdown, e.g.
// {dim:"role", value:"gardener"}.
type tagDimValue struct {
	dim   string
	value string
}

// proxySpendSnapshot is one proxy's complete spend read for a tick: the
// per-(dim,value) breakdown for the tag gauge, plus the two sides of the
// attribution split (ka-0000003988) — the agent:*-tagged cumulative total and
// the proxy's whole bill from /global/spend. Captured atomically (a tick that
// fails EITHER read carries the previous snapshot forward whole) so the split
// published from it is internally consistent: attributed + unattributed always
// reconstructs this proxy's bill as-of one instant, never a blend of two reads.
type proxySpendSnapshot struct {
	dims        map[tagDimValue]float64
	agentTagged float64 // Σ TotalSpend of agent:* tags — the attributable bill
	bill        float64 // /global/spend total — the WHOLE proxy bill, tagged or not
}

// tagSpendSyncInterval matches spendSync's 5-minute cadence and the account-cost
// gauge refresh, so the by-role/by-project/by-model gauge moves in lockstep with
// the by-account gauge and the /costs headline (ka-0000004779).
const tagSpendSyncInterval = 5 * time.Minute

func newTagSpendSync(daemon *beadsapi.Client, litellmURL, masterKey, env string, metrics *gardeniometrics.SpendMetrics, logger *slog.Logger) *tagSpendSync {
	return &tagSpendSync{
		daemon:     daemon,
		litellmURL: strings.TrimRight(litellmURL, "/"),
		masterKey:  masterKey,
		env:        env,
		// /global/spend reads LiteLLM's MonthlyGlobalSpend view, itself a scan
		// of LiteLLM_SpendLogs: allow it the same time as a /spend/tags read so
		// the controller does not hang up on a query Postgres is still running.
		httpClient: &http.Client{Timeout: tagSpendReadTimeout},
		logger:     logger.With("component", "tag-spend-sync"),
		metrics:    metrics,
		tags:       newTagSpendReader(masterKey, 0, logger),
		lastGood:   make(map[string]proxySpendSnapshot),
	}
}

// RunLoop refreshes the gauge every tagSpendSyncInterval until ctx is cancelled.
func (t *tagSpendSync) RunLoop(ctx context.Context) {
	t.syncAll(ctx)

	ticker := time.NewTicker(tagSpendSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.syncAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// syncAll polls /spend/tags from every distinct LiteLLM proxy, merges the
// per-(dim,value) spend across proxies, and republishes gardenio_tag_cost_usd.
func (t *tagSpendSync) syncAll(ctx context.Context) {
	proxies := t.distinctProxyURLs(ctx)
	if len(proxies) == 0 {
		t.logger.Debug("tag spend sync: no LiteLLM proxies resolved; skipping")
		return
	}

	// Prune last-good snapshots for proxies no longer in the active set, so a
	// proxy that left the account list (account closed/exhausted, base_url edited)
	// stops contributing its stale spend to the merged total forever. Only proxies
	// in the CURRENT poll set are merged below.
	current := make(map[string]bool, len(proxies))
	for _, p := range proxies {
		current[p] = true
	}
	for url := range t.lastGood {
		if !current[url] {
			delete(t.lastGood, url)
		}
	}

	for _, baseURL := range proxies {
		snapshot, err := t.queryProxySnapshot(ctx, baseURL)
		if err != nil {
			// Carry the proxy's last good read forward (if any) so its dimensions
			// don't dip to zero this tick. A proxy we've never read stays absent.
			if _, seen := t.lastGood[baseURL]; seen {
				t.logger.Warn("tag spend sync: proxy query failed, retaining last good snapshot",
					"proxy", baseURL, "error", err)
			} else {
				t.logger.Warn("tag spend sync: proxy query failed, no prior snapshot",
					"proxy", baseURL, "error", err)
			}
			continue
		}
		t.lastGood[baseURL] = snapshot
	}

	// Merge (sum) the active proxies' last-good snapshots. lastGood now contains
	// only current proxies (pruned above), so a departed proxy can never leak
	// into the totals. Alongside the per-(dim,value) tag merge, fold each
	// proxy's attribution split (ka-0000003988): the bill − agent-tagged
	// residual is computed PER PROXY, clamped at 0, so one proxy whose tag
	// indexing momentarily runs ahead of its bill cannot mask another proxy's
	// genuine residual.
	merged := make(map[tagDimValue]float64)
	var attributed, unattributed float64
	for proxy, snapshot := range t.lastGood {
		for k, v := range snapshot.dims {
			merged[k] += v
		}
		attributed += snapshot.agentTagged
		residual := snapshot.bill - snapshot.agentTagged
		if residual < 0 {
			// /spend/tags and /global/spend aggregate the same spend logs, so the
			// tagged total exceeding the bill beyond rounding means the two reads
			// diverged (e.g. mid-flight indexing) — visible, not booked.
			t.logger.Debug("tag spend sync: agent-tagged total exceeds proxy bill, clamping residual to 0",
				"proxy", proxy, "bill", snapshot.bill, "agent_tagged", snapshot.agentTagged)
			residual = 0
		}
		unattributed += residual
	}

	costs := make([]gardeniometrics.TagCost, 0, len(merged))
	for k, v := range merged {
		costs = append(costs, gardeniometrics.TagCost{Dim: k.dim, Value: k.value, USD: v})
	}
	// Stable order so logs/tests are deterministic (Set itself is order-independent).
	sort.Slice(costs, func(i, j int) bool {
		if costs[i].Dim != costs[j].Dim {
			return costs[i].Dim < costs[j].Dim
		}
		return costs[i].Value < costs[j].Value
	})

	t.metrics.SetTagCosts(costs, t.env)
	t.metrics.SetAttributionSplit(attributed, unattributed, t.env)
	t.logger.Debug("tag spend sync: gauges updated",
		"proxies", len(proxies), "series", len(costs),
		"attributed_usd", attributed, "unattributed_usd", unattributed)
}

// distinctProxyURLs enumerates the LiteLLM base URLs to poll: every non-empty
// account base_url, plus the controller-global LITELLM_URL, de-duplicated. This
// mirrors spendSync's per-account base_url resolution (ka-0000003221) but collapses
// to the DISTINCT set, since /spend/tags is a per-proxy global query (not
// per-account). On a daemon error it falls back to just the global URL so the
// gauge still tracks the primary proxy.
//
// It deliberately does NOT poll account proxy_url values: claudemore (proxy_url)
// is a transparent fronting proxy that forwards to the same litellm and never
// meters spend, so the spend always lives at base_url/the global URL (see the
// type doc). Polling proxy_url would double-count or fail.
func (t *tagSpendSync) distinctProxyURLs(ctx context.Context) []string {
	seen := make(map[string]bool)
	var urls []string
	add := func(raw string) {
		u := strings.TrimRight(strings.TrimSpace(raw), "/")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		urls = append(urls, u)
	}

	add(t.litellmURL)

	resp, err := t.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Limit:    100,
	})
	if err != nil {
		t.logger.Warn("tag spend sync: failed to list accounts for proxy enumeration; using global URL only", "error", err)
		return urls
	}
	for _, bead := range resp.Beads {
		add(bead.Fields["base_url"])
	}
	// Deterministic order so the poll sequence (and tests) are stable.
	sort.Strings(urls)
	return urls
}

// queryProxySnapshot fetches one proxy's complete spend view for a tick: the
// shared GET /spend/tags snapshot (refreshed first when it is due) folded into
// the per-(dim,value) breakdown plus the agent:*-tagged total, and GET
// /global/spend for the proxy's whole bill. An error from EITHER
// endpoint fails the whole snapshot so the caller carries the previous one
// forward intact — a half-updated snapshot would publish an attribution split
// blending two instants (see proxySpendSnapshot).
//
// Tag folding: a tag whose prefix is a TagCostDims dimension (role/project/
// model) lands in dims; an agent: tag lands in agentTagged (ka-0000003988) —
// /spend/tags books the FULL request cost under every one of its tags, and
// agent: appears at most once per request, so the agent:* sum is exactly the
// bill attributable to per-agent settlement. Everything else (User-Agent:
// auto-tags, untagged) is ignored.
func (t *tagSpendSync) queryProxySnapshot(ctx context.Context, baseURL string) (proxySpendSnapshot, error) {
	entries, err := t.tags.Read(ctx, baseURL, true)
	if err != nil {
		return proxySpendSnapshot{}, err
	}

	snapshot := proxySpendSnapshot{dims: make(map[tagDimValue]float64)}
	for _, e := range entries {
		if strings.HasPrefix(e.IndividualRequestTag, agentTagPrefix) {
			snapshot.agentTagged += e.TotalSpend
			continue
		}
		dim, value, ok := parseDimTag(e.IndividualRequestTag)
		if !ok {
			continue
		}
		// A given (dim,value) appears at most once per proxy, but sum defensively
		// in case LiteLLM ever returns duplicate rows.
		snapshot.dims[tagDimValue{dim: dim, value: value}] += e.TotalSpend
	}

	snapshot.bill, err = t.queryGlobalSpend(ctx, baseURL)
	if err != nil {
		return proxySpendSnapshot{}, err
	}
	return snapshot, nil
}

// agentTagPrefix marks the per-agent request tag the podspec sets on every
// agent request (agentTag in reconcile_session_spend.go). Matched as a prefix
// here: LiteLLM's auto "User-Agent: ..." tags do NOT share it.
const agentTagPrefix = "agent:"

// globalSpendResponse mirrors GET /global/spend: the proxy's lifetime
// cumulative bill across ALL keys and requests, tagged or not.
type globalSpendResponse struct {
	Spend float64 `json:"spend"`
}

// queryGlobalSpend fetches one proxy's whole lifetime bill from GET
// /global/spend — the denominator side of the attribution split. Unlike
// /spend/tags it also covers requests carrying no tags at all, which is
// exactly the traffic the unattributed residual exists to surface.
func (t *tagSpendSync) queryGlobalSpend(ctx context.Context, baseURL string) (float64, error) {
	u := baseURL + "/global/spend"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+t.masterKey)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 120))
	}

	var result globalSpendResponse
	if derr := json.NewDecoder(resp.Body).Decode(&result); derr != nil {
		return 0, fmt.Errorf("decoding /global/spend response: %w", derr)
	}
	return result.Spend, nil
}

// parseDimTag splits a LiteLLM individual_request_tag of the form "<dim>:<value>"
// and returns (dim, value, true) only when dim is one of the attribution
// dimensions we expose on gardenio_tag_cost_usd (role/project/model). Everything
// else — agent:, User-Agent:, or an untagged row — returns ok=false. The value
// keeps any embedded colons (split on the FIRST colon only); none of our values
// contain colons in practice.
func parseDimTag(tag string) (dim, value string, ok bool) {
	idx := strings.IndexByte(tag, ':')
	if idx <= 0 || idx == len(tag)-1 {
		return "", "", false
	}
	dim = tag[:idx]
	if !slices.Contains(gardeniometrics.TagCostDims, dim) {
		return "", "", false
	}
	return dim, tag[idx+1:], true
}
