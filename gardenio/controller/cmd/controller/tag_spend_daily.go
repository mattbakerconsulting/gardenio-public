package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Lifetime per-tag totals from LiteLLM's LiteLLM_DailyTagSpend aggregate
// (gd-Um9gnz9FuW).
//
// GET /spend/tags sums LiteLLM_SpendLogs, so spend-log retention would shrink
// every total it returns: gardenio_tag_cost_usd is read through increase(),
// which counts a drop as a counter reset, and the session reconciler books
// closeTotal - firstBaseline, which a drop floors at 0. LiteLLM_DailyTagSpend
// holds one row per (tag, date, api_key, model, ...) that the spend-log
// cleanup never deletes, and LiteLLM serves it, indexed on date, as
// GET /tag/daily/activity (LiteLLM v1.95.0 tag_management_endpoints.py ->
// common_daily_activity.get_daily_activity).
//
// That endpoint pages over rows (offset pagination, date desc then id) and
// answers per date with each tag's sum under breakdown.entities. A tag's
// lifetime total is the sum over every page of every day, which re-read every
// five minutes would cost about as much as the scan it replaces. So the reader
// keeps the totals of settled days per proxy and re-reads only the trailing
// tagDailyTrailingDays (today, and yesterday, whose row still takes late
// writes after midnight):
//
//   - The first read of a proxy rebuilds the settled days in one paged read
//     from tagDailyEpoch. No rows are added to settled days, so the offsets
//     cannot shift under it.
//   - Every read re-reads the trailing window, usually in one page.
//   - When the date rolls over, the day that left the window is read once more
//     and folded into the settled totals.
//
// The cache lives in memory, so a controller restart rebuilds it.

const (
	// tagSourceSpendTags reads GET /spend/tags (a full LiteLLM_SpendLogs scan).
	tagSourceSpendTags tagSpendSource = "spend-tags"
	// tagSourceDaily reads LiteLLM_DailyTagSpend through /tag/daily/activity.
	tagSourceDaily tagSpendSource = "daily"
	// tagSourceAuto reads both and serves /spend/tags until every tag the
	// controller uses agrees (tagSpendParity), then serves the aggregate only.
	tagSourceAuto tagSpendSource = "auto"

	// tagDailyTrailingDays is how many days, today included, every read
	// re-reads.
	tagDailyTrailingDays = 2
	// tagDailyEpoch is the start_date of a rebuild. The date column is a
	// YYYY-MM-DD string, so any date before LiteLLM existed reads every row.
	tagDailyEpoch = "2000-01-01"
	// tagDailyPageSize is the rows per /tag/daily/activity page. The response
	// is aggregated per date, so its size follows the distinct tags, not the
	// rows; a large page keeps the trailing window to one request, where rows
	// added while it pages could shift the offsets.
	tagDailyPageSize = 5000
	// tagDailyMaxPages bounds one paged read.
	tagDailyMaxPages = 1000

	// tagParityAbsUSD and tagParityRel are the per-tag tolerance of the auto
	// cutover: a tag agrees when |spendTags - daily| <= max(abs, rel * the
	// larger). The two reads are seconds apart and LiteLLM flushes the two
	// tables in batches, so an active agent's tag can differ by a little.
	// VictoriaMetrics' increase() absorbs a drop under 1/8 of the value, so a
	// cutover inside this tolerance adds no false spend.
	tagParityAbsUSD = 0.50
	tagParityRel    = 0.005

	// tagStaticGapRel bounds a static gap (tagSpendParity): a mismatched tag
	// whose two totals are both unchanged since the previous comparison does
	// not hold the cutover back when its gap is at most this share of the
	// larger total. No write landed between the reads, so a flush race cannot
	// explain the gap: SpendLogs rows that never reached DailyTagSpend do,
	// and no read will close it (gd-Jp8wOpNeh8). The 1/8 matches increase()
	// above; a larger gap is a hole in the aggregate and still blocks.
	tagStaticGapRel = 0.125
	// tagStaticGapEps is how close a total must be to its previous read to
	// count as unchanged; the two sides re-sum floats on every read.
	tagStaticGapEps = 1e-6
)

// tagSpendSource names where tagSpendReader reads lifetime tag totals.
type tagSpendSource string

// parseTagSpendSource parses LITELLM_TAG_SPEND_SOURCE. Empty is auto.
func parseTagSpendSource(s string) (tagSpendSource, error) {
	switch src := tagSpendSource(strings.TrimSpace(s)); src {
	case "":
		return tagSourceAuto, nil
	case tagSourceSpendTags, tagSourceDaily, tagSourceAuto:
		return src, nil
	default:
		return "", fmt.Errorf("unknown tag spend source %q (want %s, %s or %s)", s,
			tagSourceAuto, tagSourceDaily, tagSourceSpendTags)
	}
}

// tagSum is one tag's spend and request count over some days.
type tagSum struct {
	spend    float64
	requests int64
}

// dailyTagTotals is one proxy's DailyTagSpend totals over the settled days,
// every date before through. It is never modified once built, so a read that
// fails halfway leaves the previous one intact.
type dailyTagTotals struct {
	settled map[string]tagSum
	through time.Time // first unsettled date (UTC midnight)
}

// tagDailyActivityPage mirrors the fields of LiteLLM's
// SpendAnalyticsPaginatedResponse that the reader uses.
type tagDailyActivityPage struct {
	Results []struct {
		Breakdown struct {
			Entities map[string]struct {
				Metrics struct {
					Spend       float64 `json:"spend"`
					APIRequests int64   `json:"api_requests"`
				} `json:"metrics"`
			} `json:"entities"`
		} `json:"breakdown"`
	} `json:"results"`
	Metadata struct {
		HasMore bool `json:"has_more"`
	} `json:"metadata"`
}

// tagDailyUnassigned is the entity LiteLLM reports for a row with no tag.
const tagDailyUnassigned = "Unassigned"

// readDaily returns lifetime per-tag totals from LiteLLM_DailyTagSpend,
// rebuilding or advancing p.daily as needed (see the file doc). p.daily is
// replaced only when every request succeeds.
func (r *tagSpendReader) readDaily(ctx context.Context, baseURL string, p *tagSpendProxy) ([]tagSpendEntry, error) {
	today := utcDate(r.now())
	trailStart := today.AddDate(0, 0, -(tagDailyTrailingDays - 1))
	lastSettled := trailStart.AddDate(0, 0, -1)

	totals := p.daily
	switch {
	case totals == nil:
		settled, pages, err := r.getDaily(ctx, baseURL, tagDailyEpoch, dateString(lastSettled))
		if err != nil {
			return nil, fmt.Errorf("DailyTagSpend rebuild: %w", err)
		}
		r.logger.Info("DailyTagSpend totals rebuilt", "proxy", baseURL, "through", dateString(lastSettled),
			"tags", len(settled), "pages", pages)
		totals = &dailyTagTotals{settled: settled, through: trailStart}
	case trailStart.After(totals.through):
		days, _, err := r.getDaily(ctx, baseURL, dateString(totals.through), dateString(lastSettled))
		if err != nil {
			return nil, fmt.Errorf("DailyTagSpend settle: %w", err)
		}
		settled := make(map[string]tagSum, len(totals.settled)+len(days))
		for tag, s := range totals.settled {
			settled[tag] = s
		}
		addTagSums(settled, days)
		totals = &dailyTagTotals{settled: settled, through: trailStart}
	}

	trail, pages, err := r.getDaily(ctx, baseURL, dateString(trailStart), dateString(today))
	if err != nil {
		return nil, fmt.Errorf("DailyTagSpend trailing days: %w", err)
	}
	if pages > 1 {
		// Rows added to today while this paged may have shifted a row from one
		// page onto the next and counted it twice. Only this read is off: a
		// day is settled from a read of its own once it leaves the window.
		// Raise tagDailyPageSize if this shows up.
		r.logger.Warn("DailyTagSpend trailing window spans several pages", "proxy", baseURL, "pages", pages)
	}
	p.daily = totals

	sums := make(map[string]tagSum, len(totals.settled)+len(trail))
	addTagSums(sums, totals.settled)
	addTagSums(sums, trail)
	entries := make([]tagSpendEntry, 0, len(sums))
	for tag, s := range sums {
		entries = append(entries, tagSpendEntry{IndividualRequestTag: tag, LogCount: s.requests, TotalSpend: s.spend})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].IndividualRequestTag < entries[j].IndividualRequestTag })
	return entries, nil
}

// getDaily sums every tag's DailyTagSpend rows dated start..end (inclusive,
// YYYY-MM-DD) from GET /tag/daily/activity, paging until has_more is false.
func (r *tagSpendReader) getDaily(ctx context.Context, baseURL, start, end string) (map[string]tagSum, int, error) {
	sums := make(map[string]tagSum)
	for page := 1; ; page++ {
		if page > tagDailyMaxPages {
			return nil, page - 1, fmt.Errorf("more than %d pages", tagDailyMaxPages)
		}
		q := url.Values{}
		q.Set("start_date", start)
		q.Set("end_date", end)
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", strconv.Itoa(tagDailyPageSize))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/tag/daily/activity?"+q.Encode(), nil)
		if err != nil {
			return nil, page, err
		}
		req.Header.Set("Authorization", "Bearer "+r.masterKey)
		resp, err := r.httpClient.Do(req)
		if err != nil {
			return nil, page, err
		}
		var body tagDailyActivityPage
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, page, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(msg), 120))
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, page, fmt.Errorf("decoding /tag/daily/activity response: %w", err)
		}
		for _, day := range body.Results {
			for tag, e := range day.Breakdown.Entities {
				if tag == tagDailyUnassigned {
					continue
				}
				s := sums[tag]
				s.spend += e.Metrics.Spend
				s.requests += e.Metrics.APIRequests
				sums[tag] = s
			}
		}
		if !body.Metadata.HasMore {
			return sums, page, nil
		}
	}
}

// readAuto serves /spend/tags until the DailyTagSpend totals agree with it for
// every tag the controller uses, then cuts the proxy over to DailyTagSpend for
// the life of the process. Cutting over inside tolerance keeps
// gardenio_tag_cost_usd from stepping, and keeps the session reconciler's
// baselines, taken from /spend/tags, comparable with the totals it closes on.
// A DailyTagSpend failure before cutover is logged and /spend/tags is served.
func (r *tagSpendReader) readAuto(ctx context.Context, baseURL string, p *tagSpendProxy) ([]tagSpendEntry, error) {
	legacy, err := r.get(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	daily, err := r.readDaily(ctx, baseURL, p)
	if err != nil {
		r.logger.Warn("DailyTagSpend read failed; serving /spend/tags", "proxy", baseURL, "error", err)
		return legacy, nil
	}
	par := tagSpendParity(legacy, daily, p.gaps)
	p.gaps = par.gaps
	if par.mismatched == 0 {
		p.cutOver = true
		r.logger.Info("tag spend totals agree; reading DailyTagSpend from now on", "proxy", baseURL,
			"tags_compared", par.compared, "max_delta_usd", par.maxDelta, "max_delta_tag", par.maxTag,
			"static_gaps", par.static)
		return daily, nil
	}
	r.logger.Info("tag spend totals disagree; serving /spend/tags", "proxy", baseURL,
		"tags_compared", par.compared, "tags_mismatched", par.mismatched,
		"max_delta_usd", par.maxDelta, "max_delta_tag", par.maxTag, "worst", par.worst,
		"static_gaps", par.static)
	return legacy, nil
}

// tagParityReport is the outcome of tagSpendParity.
type tagParityReport struct {
	compared   int
	mismatched int     // mismatches that block the cutover; static gaps excluded
	maxDelta   float64 // largest |spendTags - daily| over the compared tags
	maxTag     string
	worst      []string          // up to five blocking mismatches, "tag spend-tags=X daily=Y"
	static     []string          // up to five static gaps, same format
	gaps       map[string]tagGap // every mismatched tag, static or not, for the next comparison
}

// tagGap is one mismatched tag's two totals at a comparison.
type tagGap struct{ spendTags, daily float64 }

// tagSpendParity compares the /spend/tags and DailyTagSpend totals of every
// tag the controller reads: agent:* (one by one and summed, as the reconciler
// and gardenio_agent_attributed_cost_usd read them) and the dimension tags of
// gardenio_tag_cost_usd. A tag absent from one side counts as 0 there. Other
// tags (LiteLLM's User-Agent ones) are ignored.
//
// prev is the previous comparison's gaps (nil on the first). A mismatch that
// is a static gap (see tagStaticGapRel) is reported but does not count in
// mismatched. A tag that is still taking spend never has a static gap, so a
// flush race on a live tag always blocks.
func tagSpendParity(legacy, daily []tagSpendEntry, prev map[string]tagGap) tagParityReport {
	const agentSum = "agent:*"
	totals := func(entries []tagSpendEntry) map[string]float64 {
		m := make(map[string]float64)
		for _, e := range entries {
			tag := e.IndividualRequestTag
			if strings.HasPrefix(tag, agentTagPrefix) {
				m[agentSum] += e.TotalSpend
			} else if _, _, ok := parseDimTag(tag); !ok {
				continue
			}
			m[tag] += e.TotalSpend
		}
		return m
	}
	a, b := totals(legacy), totals(daily)
	tags := make([]string, 0, len(a)+len(b))
	for tag := range a {
		tags = append(tags, tag)
	}
	for tag := range b {
		if _, ok := a[tag]; !ok {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)

	rep := tagParityReport{gaps: make(map[string]tagGap)}
	type miss struct {
		tag   string
		delta float64
	}
	var misses, statics []miss
	for _, tag := range tags {
		x, y := a[tag], b[tag]
		delta := math.Abs(x - y)
		rep.compared++
		if delta > rep.maxDelta {
			rep.maxDelta, rep.maxTag = delta, tag
		}
		if delta <= math.Max(tagParityAbsUSD, tagParityRel*math.Max(x, y)) {
			continue
		}
		rep.gaps[tag] = tagGap{spendTags: x, daily: y}
		m := miss{tag: fmt.Sprintf("%s spend-tags=%.4f daily=%.4f", tag, x, y), delta: delta}
		if g, ok := prev[tag]; ok && math.Abs(g.spendTags-x) <= tagStaticGapEps &&
			math.Abs(g.daily-y) <= tagStaticGapEps && delta <= tagStaticGapRel*math.Max(x, y) {
			statics = append(statics, m)
			continue
		}
		rep.mismatched++
		misses = append(misses, m)
	}
	top := func(ms []miss) []string {
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].delta > ms[j].delta })
		var out []string
		for i := 0; i < len(ms) && i < 5; i++ {
			out = append(out, ms[i].tag)
		}
		return out
	}
	rep.worst, rep.static = top(misses), top(statics)
	return rep
}

// addTagSums adds src into dst.
func addTagSums(dst, src map[string]tagSum) {
	for tag, s := range src {
		d := dst[tag]
		d.spend += s.spend
		d.requests += s.requests
		dst[tag] = d
	}
}

// utcDate is t's UTC date at midnight. LiteLLM dates DailyTagSpend rows by
// the proxy's clock; the trailing window keeps a day open until a full day
// after it ends, which covers a proxy clock that is not UTC.
func utcDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func dateString(t time.Time) string { return t.Format("2006-01-02") }
