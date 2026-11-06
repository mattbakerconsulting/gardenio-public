package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// badKeyCooldown is how long we skip a key after LiteLLM returns 404/403
// for it. This prevents hammering LiteLLM with requests for stale keys
// that generate ERROR-level tracebacks in its logs.
const badKeyCooldown = 1 * time.Hour

// subscription5hWindow / subscription7dWindow are the Anthropic subscription
// rate-limit window lengths. The current window opened at throttle_reset_*_at −
// window, so spend since that instant is what a subscription account is billed
// against for that window — on the subscription's own clock, not the 00:00-UTC
// calendar day the daily_cost_usd figure buckets to (ka-0000003343).
const (
	subscription5hWindow = 5 * time.Hour
	subscription7dWindow = 7 * 24 * time.Hour
)

// subscriptionWindowStart returns the instant the current subscription window
// opened: the probe-written reset instant (RFC3339) minus the window length.
// ok=false when the reset field is empty or unparseable — the caller then
// leaves the window-spend field unset rather than billing against a fabricated
// boundary. Pure so the windowing is unit-testable without a probe round-trip.
func subscriptionWindowStart(resetAt string, window time.Duration) (time.Time, bool) {
	reset, err := time.Parse(time.RFC3339, strings.TrimSpace(resetAt))
	if err != nil {
		return time.Time{}, false
	}
	return reset.Add(-window), true
}

// spendSync periodically queries LiteLLM /key/info for each account's virtual
// key and writes the cumulative spend to the account bead's total_cost_usd field.
// It also queries /spend/logs for daily and weekly breakdowns so the iOS app
// can display period costs.
type spendSync struct {
	daemon *beadsapi.Client
	// accounts is the client every account-bead WRITE goes through. In
	// production it authenticates as the controller's own machine principal
	// (gd-s7aPIf0Bgp) so kbeads admits update/account through the
	// gardenio-machine-accounts grant; when the machine principal is disabled
	// it is the same client as daemon. Reads and non-account writes stay on
	// daemon: a machine principal is ownership-gated for non-config beads.
	accounts   *beadsapi.Client
	litellmURL string
	masterKey  string
	httpClient *http.Client
	logger     *slog.Logger

	mu      sync.Mutex
	badKeys map[string]time.Time // virtual key → when it was marked bad
	// sliceCache memoizes settled boundary-day slices (windowSpendUSD). Keys are
	// closed past intervals, so an entry can never go stale — a window roll mints
	// a new key rather than invalidating an old one.
	sliceCache map[sliceKey]float64

	// now is the wall clock, injectable so tests can anchor the daily/weekly
	// calendar windows to a fixed instant instead of the real UTC weekday.
	// Defaults to time.Now. Without this seam the weekly-window assertions are
	// weekday-dependent: the Sunday-anchored week (syncAll) collapses to today
	// on Sundays, so "yesterday" rows fall out of the week and the sums drop
	// (calendar tripwire ka-0000003815, same class as ka-0000006078).
	now func() time.Time
}

// accounts is the client for account-bead writes (the machine principal when
// enabled); pass daemon itself to keep today's single-client behavior.
func newSpendSync(daemon, accounts *beadsapi.Client, litellmURL, masterKey string, logger *slog.Logger) *spendSync {
	if accounts == nil {
		accounts = daemon
	}
	return &spendSync{
		daemon:     daemon,
		accounts:   accounts,
		litellmURL: strings.TrimRight(litellmURL, "/"),
		masterKey:  masterKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger.With("component", "spend-sync"),
		badKeys:    make(map[string]time.Time),
		sliceCache: make(map[sliceKey]float64),
		now:        time.Now,
	}
}

// RunLoop syncs spend every 5 minutes until ctx is cancelled.
func (s *spendSync) RunLoop(ctx context.Context) {
	s.syncAll(ctx)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.syncAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// isKeyBad returns true if the key was recently marked as invalid.
func (s *spendSync) isKeyBad(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.badKeys[key]
	if !ok {
		return false
	}
	if time.Since(t) > badKeyCooldown {
		delete(s.badKeys, key)
		return false
	}
	return true
}

// markKeyBad records that a key is invalid so we stop polling it.
func (s *spendSync) markKeyBad(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badKeys[key] = time.Now()
}

func (s *spendSync) syncAll(ctx context.Context) {
	// Fetch raw beads so we have both the name (Title) and ID.
	resp, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Limit:    50,
	})
	if err != nil {
		s.logger.Warn("spend sync: failed to list accounts", "error", err)
		return
	}

	now := s.now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := dayStart.AddDate(0, 0, -6)

	for _, bead := range resp.Beads {
		litellmKey := bead.Fields["litellm_key"]
		if litellmKey == "" {
			continue
		}

		if s.isKeyBad(litellmKey) {
			s.logger.Debug("spend sync: skipping bad key", "account", bead.Title)
			continue
		}

		// Per-account base_url override: a vkey minted on an in-cluster proxy is
		// only resolvable there, so an account carrying a non-empty base_url is
		// queried against that URL rather than the global LITELLM_URL. Empty
		// base_url falls back to the global URL (current behavior). Additive and
		// reversible (ka-0000003221).
		baseURL := s.accountBaseURL(bead.Fields["base_url"])

		// A tenant land's account (lands-<land>) also stamps when it was synced
		// and each running agent's live cost at that moment, which the land's
		// purse needs (internal/lands/purse, Lands Track B §3.4). The live costs
		// are read BEFORE the key, so the baseline never holds spend the key
		// figure does not: in-flight errs high, never low.
		land := purse.LandOfAccount(bead.Title)
		var landAgents []purse.AgentCost
		landSnap := false
		if land != "" {
			agents, err := purse.LiveCosts(ctx, s.daemon, land)
			if err != nil {
				s.logger.Warn("spend sync: land agents read failed; purse not stamped", "account", bead.Title, "error", err)
			} else {
				landAgents, landSnap = agents, true
			}
		}

		info, err := s.queryKeyInfo(ctx, baseURL, litellmKey)
		if err != nil {
			s.logger.Warn("spend sync: query failed", "account", bead.Title, "error", err)
			continue
		}
		spend := info.Spend

		// Subscription-window starts (ka-0000003343): the current 5h/7d windows
		// opened at throttle_reset_*_at − window, on the SUBSCRIPTION's own clock
		// (probe headers), not the 00:00-UTC calendar day. Empty/unparseable →
		// (zero,false), leaving the field unset so the viewer falls back rather
		// than billing against a fabricated window.
		win5hStart, has5h := subscriptionWindowStart(bead.Fields["throttle_reset_5h_at"], subscription5hWindow)
		win7dStart, has7d := subscriptionWindowStart(bead.Fields["throttle_reset_7d_at"], subscription7dWindow)

		// One fetch feeds every window sum: bound start_date by the earliest
		// window we need (usually weekStart, but a fresh 7d subscription window
		// can open earlier). start_date is day-granular, so flooring back a day is
		// harmless — the per-row stamp filter is what actually windows the sum.
		fetchFrom := weekStart
		if has5h && win5hStart.Before(fetchFrom) {
			fetchFrom = win5hStart
		}
		if has7d && win7dStart.Before(fetchFrom) {
			fetchFrom = win7dStart
		}

		fields := map[string]any{
			"total_cost_usd":        spend,
			"shadow_total_cost_usd": spend,
		}
		// LiteLLM-enforced budget (bug ka-0000001246): when the account's virtual
		// key carries a max_budget, persist it (and its reset instant) onto the
		// account bead so the dark-lane spend-exhaustion alert can name the real
		// spend_limit / days_remaining instead of guessing. Written only when
		// present — an unbudgeted key (max_budget=null, the common case today)
		// leaves both fields untouched so no fabricated limit ever appears.
		maps.Copy(fields, spendLimitFields(info))
		if logs, ok := s.fetchSpendLogsSince(ctx, baseURL, litellmKey, fetchFrom); ok {
			daily := sumSpendInWindow(logs, dayStart)
			weekly := sumSpendInWindow(logs, weekStart)
			// shadow_* fields mirror the canonical values for POC reconciliation
			// against the Anthropic Console; drift is recorded manually (D-h3).
			fields["daily_cost_usd"] = daily
			fields["weekly_cost_usd"] = weekly
			fields["shadow_daily_cost_usd"] = daily
			fields["shadow_weekly_cost_usd"] = weekly
			// Subscription windows open mid-day, and these `logs` rows are
			// day-AGGREGATES stamped at midnight — so an instant filter drops the
			// boundary day wholesale (see windowSpendUSD). Decompose instead.
			if has5h {
				if usd, ok := s.windowSpendUSD(ctx, baseURL, litellmKey, logs, win5hStart, now); ok {
					fields["spend_5h_window_usd"] = usd
				}
			}
			if has7d {
				if usd, ok := s.windowSpendUSD(ctx, baseURL, litellmKey, logs, win7dStart, now); ok {
					fields["spend_7d_window_usd"] = usd
				}
			}
		}
		if err := s.accounts.UpdateBeadFieldsTyped(ctx, bead.ID, fields); err != nil {
			s.logger.Warn("spend sync: update failed", "account", bead.Title, "error", err)
			continue
		}
		// The stamp is its own write, after the spend landed: a daemon whose
		// account schema predates these fields refuses only this write, never
		// the spend figures (ka-0000007493). An unstamped purse goes unknown.
		if landSnap {
			stamp := map[string]any{
				purse.FieldSpendSyncedAt:    now.Format(time.RFC3339),
				purse.FieldLiveCostBaseline: purse.BaselineJSON(landAgents),
			}
			if err := s.accounts.UpdateBeadFieldsTyped(ctx, bead.ID, stamp); err != nil {
				s.logger.Warn("spend sync: purse stamp failed", "account", bead.Title, "error", err)
			}
		}
	}
}

// liteLLMKeyInfo is the relevant subset of a /key/info `info` object.
//
// MaxBudget / BudgetResetAt are the LiteLLM-enforced budget the account's
// virtual key is billed against — when a budget IS configured on the key, this
// is the authoritative source for the account monthly spend limit and its reset
// instant (bug ka-0000001246). Both are pointer/omitempty: LiteLLM omits
// max_budget (null) for an unbudgeted key, in which case spendLimitFields leaves
// the derived account fields unset rather than fabricating a limit.
type liteLLMKeyInfo struct {
	Spend         float64  `json:"spend"`
	MaxBudget     *float64 `json:"max_budget"`
	BudgetResetAt string   `json:"budget_reset_at"`
}

// liteLLMKeyInfoResponse is the /key/info response shape.
type liteLLMKeyInfoResponse struct {
	Info liteLLMKeyInfo `json:"info"`
}

// spendLimitFields derives the account-bead budget fields from a /key/info info
// object (bug ka-0000001246). It returns entries ONLY for the values LiteLLM
// actually reported:
//
//   - spend_limit_usd    ← max_budget, when a positive budget is configured.
//   - spend_reset_at     ← budget_reset_at (RFC3339), when present.
//
// An unbudgeted key (max_budget null/≤0, the common case today) yields no
// spend_limit_usd entry, and a key with no budget_reset_at yields no
// spend_reset_at entry — so the alert reads the field as honestly-unset rather
// than reporting a fabricated $0 limit or a zero-value reset instant. Returns an
// empty (non-nil) map when nothing is reported, so callers can range over it
// unconditionally.
func spendLimitFields(info liteLLMKeyInfo) map[string]any {
	out := make(map[string]any, 2)
	if info.MaxBudget != nil && *info.MaxBudget > 0 {
		out["spend_limit_usd"] = *info.MaxBudget
	}
	if reset := strings.TrimSpace(info.BudgetResetAt); reset != "" {
		out["spend_reset_at"] = reset
	}
	return out
}

// accountBaseURL returns the LiteLLM base URL to use for an account: the
// per-account base_url when non-empty, else the global litellmURL. The result
// is trimmed of a trailing slash so callers can concatenate "/key/info" etc.
//
// It resolves via resolveSpendReadBaseURL, which deliberately IGNORES the
// account's proxy_url (claudemore): spend is read from litellm directly so
// metering stays byte-identical whether or not claudemore fronts the write path
// (transparency A1, ka-0000007533). The caller passes only base_url here — never
// proxy_url — which keeps the invariant true by construction.
func (s *spendSync) accountBaseURL(accountBaseURL string) string {
	return resolveSpendReadBaseURL(accountBaseURL, s.litellmURL)
}

// queryKeyInfo fetches the account's /key/info object: cumulative spend plus the
// LiteLLM-enforced budget (max_budget / budget_reset_at) when the key carries one
// (bug ka-0000001246). A 403/404 marks the key bad so it is not polled again.
func (s *spendSync) queryKeyInfo(ctx context.Context, baseURL, virtualKey string) (liteLLMKeyInfo, error) {
	u := baseURL + "/key/info?key=" + virtualKey
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return liteLLMKeyInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.masterKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return liteLLMKeyInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// Mark key as bad on 403/404 to stop hammering LiteLLM with
		// requests for stale keys (which generate ERROR tracebacks).
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			s.markKeyBad(virtualKey)
		}
		return liteLLMKeyInfo{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 120))
	}

	var result liteLLMKeyInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return liteLLMKeyInfo{}, fmt.Errorf("decoding response: %w", err)
	}
	return result.Info, nil
}

// spendLogEntry is a single entry from LiteLLM /spend/logs.
type spendLogEntry struct {
	Spend     float64 `json:"spend"`
	StartTime string  `json:"startTime"`
}

// hashKey returns the SHA256 hex digest of a LiteLLM virtual key.
// The /spend/logs endpoint indexes spend by the hashed key, not the raw key.
func hashKey(virtualKey string) string {
	h := sha256.Sum256([]byte(virtualKey))
	return hex.EncodeToString(h[:])
}

// fetchSpendLogsSince fetches the raw /spend/logs request rows from `since`
// (inclusive, day-granular start_date) through tomorrow. Returns the parsed
// rows and true on a clean 200; (nil, false) on any transport / status / parse
// failure so callers can distinguish "no data available" from "genuinely zero
// spend". Splitting the fetch from the windowing lets one round-trip feed every
// window sum (daily, weekly, and the subscription 5h/7d windows) instead of one
// HTTP call per window (ka-0000003343).
func (s *spendSync) fetchSpendLogsSince(ctx context.Context, baseURL, virtualKey string, since time.Time) ([]spendLogEntry, bool) {
	u := fmt.Sprintf("%s/spend/logs?api_key=%s&start_date=%s&end_date=%s",
		baseURL,
		hashKey(virtualKey),
		since.Format("2006-01-02"),
		s.now().UTC().AddDate(0, 0, 1).Format("2006-01-02"),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+s.masterKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}

	var logs []spendLogEntry
	if err := json.Unmarshal(body, &logs); err != nil {
		return nil, false
	}
	return logs, true
}

// sumSpendInWindow sums the spend of every log row whose parseable startTime is
// at or after `start`. Rows with an unparseable stamp are skipped (never
// silently counted at the epoch). Pure — the windowing basis is the caller's
// `start`, so the SAME fetched rows can be summed against a calendar boundary
// (dayStart/weekStart) or a subscription reset instant (throttle_reset − window)
// and the two figures share a source of truth (advice ka-0000005924).
func sumSpendInWindow(logs []spendLogEntry, start time.Time) float64 {
	var total float64
	for _, entry := range logs {
		if t, ok := parseSpendTime(entry.StartTime); ok && !t.Before(start) {
			total += entry.Spend
		}
	}
	return total
}

// Subscription-window decomposition (the boundary-day fix).
const (
	// sliceMaxPages bounds one boundary-slice sweep. A slice spans at most 24h
	// of ONE key's rows (api_key IS filtered server-side on /spend/logs/v2 —
	// only request_tags is ignored), so this is generous; exceeding it fails the
	// slice to (0,false) and leaves the field unset rather than writing a
	// silently truncated sum.
	sliceMaxPages = 400
	// slicePageSize is the server cap for /spend/logs/v2.
	slicePageSize = 100
	// sliceSettleLag is how far a slice's upper edge must trail now before the
	// result is cached. LiteLLM's spend writer flushes in batches with up to
	// ~a minute of lag, so a slice closed moments ago can still gain rows;
	// caching it then would freeze an undercount for the life of the window.
	sliceSettleLag = 2 * time.Minute
	// sliceCacheMax bounds the cache; entries are keyed by (key,from,to) and a
	// window roll simply mints a new key, so this only needs to cover the few
	// live windows per account.
	sliceCacheMax = 64
)

// sliceKey identifies one immutable, closed, past interval of one virtual key.
type sliceKey struct {
	virtualKey string
	from, to   time.Time
}

// windowSpendUSD returns the spend inside the subscription window
// [winStart, now] for one virtual key.
//
// THE BUG THIS REPLACES: `logs` are per-CALENDAR-DAY aggregates whose startTime
// is that day's midnight. Filtering them by an instant (`!t.Before(winStart)`)
// is all-or-nothing on the boundary day, and midnight always loses to a window
// that opened later that day — so the day a window opens contributes ZERO. For
// the 5h window, which essentially always opens mid-day, that meant the figure
// could never be anything but $0.00 for any account; for the 7d window it zeroed
// any account young enough that its spend lived on the boundary day (observed on
// `fimbaz`: $0.00 against a real $206 of in-window spend).
//
// The decomposition, with D = the calendar day containing winStart:
//
//	D is today:   today's aggregate − slice[midnight(D), winStart]
//	D is earlier: slice[winStart, end(D)] + Σ aggregates for days in (D, today]
//
// Each branch needs exactly ONE per-request query, always over a CLOSED interval
// in the PAST — so it is immutable and cached, recomputed only when the window
// rolls (every 5h / 7d) rather than every sync cycle. The complete-day terms keep
// using the cheap day aggregates, which are already correct on day boundaries.
//
// Returns ok=false if the slice could not be fetched, leaving the field unset
// rather than writing a figure that is wrong in the same direction as the bug.
func (s *spendSync) windowSpendUSD(ctx context.Context, baseURL, virtualKey string, logs []spendLogEntry, winStart, now time.Time) (float64, bool) {
	winStart, now = winStart.UTC(), now.UTC()
	if !winStart.Before(now) {
		return 0, true // window opens in the future: nothing billed to it yet
	}
	boundaryDay := time.Date(winStart.Year(), winStart.Month(), winStart.Day(), 0, 0, 0, 0, time.UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	if boundaryDay.Equal(today) {
		// The window opened today, so today's aggregate covers [midnight, now]
		// and overshoots by exactly the leading sliver [midnight, winStart].
		// Subtracting is cheaper than summing the trailing remainder: the sliver
		// is bounded by winStart−midnight and, unlike the remainder, never grows.
		sliver, ok := s.sliceSpendUSD(ctx, baseURL, virtualKey, boundaryDay, winStart, now)
		if !ok {
			return 0, false
		}
		return maxFloat(sumSpendInWindow(logs, today)-sliver, 0), true
	}

	// The window opened on an earlier day: take that day's trailing slice, then
	// every complete day after it (today's aggregate included — it covers
	// [midnight, now], which is exactly the window's live tail).
	slice, ok := s.sliceSpendUSD(ctx, baseURL, virtualKey, winStart, boundaryDay.AddDate(0, 0, 1), now)
	if !ok {
		return 0, false
	}
	return slice + sumSpendInWindow(logs, boundaryDay.AddDate(0, 0, 1)), true
}

// sliceSpendUSD sums one key's per-request spend over the closed past interval
// [from,to) via /spend/logs/v2, memoized. Only settled intervals (upper edge at
// least sliceSettleLag behind now) are cached, so a slice read moments after its
// edge closed is not frozen mid-flush.
func (s *spendSync) sliceSpendUSD(ctx context.Context, baseURL, virtualKey string, from, to, now time.Time) (float64, bool) {
	if !from.Before(to) {
		return 0, true
	}
	key := sliceKey{virtualKey: virtualKey, from: from, to: to}
	s.mu.Lock()
	cached, hit := s.sliceCache[key]
	s.mu.Unlock()
	if hit {
		return cached, true
	}

	usd, err := s.fetchSliceSpend(ctx, baseURL, virtualKey, from, to)
	if err != nil {
		s.logger.Warn("spend sync: window slice failed",
			"from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339), "error", err)
		return 0, false
	}
	if to.Before(now.Add(-sliceSettleLag)) {
		s.mu.Lock()
		if len(s.sliceCache) >= sliceCacheMax {
			s.sliceCache = make(map[sliceKey]float64, sliceCacheMax)
		}
		s.sliceCache[key] = usd
		s.mu.Unlock()
	}
	return usd, nil == err
}

// fetchSliceSpend pages /spend/logs/v2 over [from,to) for ONE virtual key and
// sums the per-request spend. Unlike the legacy /spend/logs, this endpoint is
// paginated, carries true per-request timestamps, and DOES honor the api_key
// filter server-side (verified against LiteLLM: a bogus api_key returns 0 rows
// while the fleet-wide query returns all of them — it is request_tags, not
// api_key, that the proxy ignores). Rows are deduped by request_id and filtered
// client-side to [from,to) because the server window is widened a second on each
// side, its boundary inclusivity being undocumented.
func (s *spendSync) fetchSliceSpend(ctx context.Context, baseURL, virtualKey string, from, to time.Time) (float64, error) {
	var total float64
	seen := make(map[string]bool)
	for pageNum := 1; ; pageNum++ {
		if pageNum > sliceMaxPages {
			return 0, fmt.Errorf("slice exceeds %d pages", sliceMaxPages)
		}
		q := url.Values{}
		q.Set("api_key", hashKey(virtualKey))
		q.Set("start_date", from.UTC().Add(-time.Second).Format(spendSliceTimeFormat))
		q.Set("end_date", to.UTC().Add(time.Second).Format(spendSliceTimeFormat))
		q.Set("page", strconv.Itoa(pageNum))
		q.Set("page_size", strconv.Itoa(slicePageSize))
		q.Set("sort_by", "startTime")
		q.Set("sort_order", "asc")

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/spend/logs/v2?"+q.Encode(), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+s.masterKey)

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return 0, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 120))
		}
		if readErr != nil {
			return 0, readErr
		}
		var pg spendSlicePage
		if err := json.Unmarshal(body, &pg); err != nil {
			return 0, fmt.Errorf("decoding page %d: %w", pageNum, err)
		}
		for _, row := range pg.Data {
			if row.RequestID != "" {
				if seen[row.RequestID] {
					continue
				}
				seen[row.RequestID] = true
			}
			t, ok := parseSpendTime(row.StartTime)
			if !ok || t.Before(from) || !t.Before(to) {
				continue
			}
			total += row.Spend
		}
		if pageNum >= pg.TotalPages || len(pg.Data) == 0 {
			break
		}
	}
	return total, nil
}

// spendSliceTimeFormat is the only datetime format /spend/logs/v2 accepts for
// start_date/end_date (naive UTC; RFC3339 is rejected with HTTP 400).
const spendSliceTimeFormat = "2006-01-02 15:04:05"

// spendSliceRow / spendSlicePage mirror the /spend/logs/v2 per-request envelope.
type spendSliceRow struct {
	RequestID string  `json:"request_id"`
	Spend     float64 `json:"spend"`
	StartTime string  `json:"startTime"`
}

type spendSlicePage struct {
	Data       []spendSliceRow `json:"data"`
	TotalPages int             `json:"total_pages"`
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// parseSpendTime parses a startTime string from LiteLLM /spend/logs.
// The API may return date-only ("2006-01-02"), datetime ("2006-01-02T15:04:05"),
// or RFC3339 ("2006-01-02T15:04:05Z") formats.
func parseSpendTime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
