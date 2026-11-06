package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeDailyLiteLLM serves GET /spend/tags from legacy and GET
// /tag/daily/activity from days, in LiteLLM v1.95.0's response shape. It pages
// one date per page (newest first), whatever page_size asks, so every read of
// more than one day exercises paging.
type fakeDailyLiteLLM struct {
	mu          sync.Mutex
	days        map[string]map[string]float64 // date -> tag -> spend
	legacy      []tagSpendEntry
	dailyReqs   []string // "start..end p<page>"
	legacyReads int
	failDaily   bool
}

func (f *fakeDailyLiteLLM) set(date, tag string, spend float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.days == nil {
		f.days = map[string]map[string]float64{}
	}
	if f.days[date] == nil {
		f.days[date] = map[string]float64{}
	}
	f.days[date][tag] = spend
}

func (f *fakeDailyLiteLLM) takeDailyReqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	reqs := f.dailyReqs
	f.dailyReqs = nil
	return reqs
}

func (f *fakeDailyLiteLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/spend/tags":
		f.legacyReads++
		_ = json.NewEncoder(w).Encode(f.legacy)
	case "/tag/daily/activity":
		q := r.URL.Query()
		start, end := q.Get("start_date"), q.Get("end_date")
		page, _ := strconv.Atoi(q.Get("page"))
		f.dailyReqs = append(f.dailyReqs, fmt.Sprintf("%s..%s p%d", start, end, page))
		if start == "" || end == "" {
			http.Error(w, `{"error":"Please provide start_date and end_date"}`, http.StatusBadRequest)
			return
		}
		if f.failDaily {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var dates []string
		for d := range f.days {
			if d >= start && d <= end {
				dates = append(dates, d)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dates)))
		type metrics struct {
			Spend       float64 `json:"spend"`
			APIRequests int64   `json:"api_requests"`
		}
		type entity struct {
			Metrics metrics `json:"metrics"`
		}
		type day struct {
			Date      string `json:"date"`
			Breakdown struct {
				Entities map[string]entity `json:"entities"`
			} `json:"breakdown"`
		}
		resp := struct {
			Results  []day `json:"results"`
			Metadata struct {
				Page    int  `json:"page"`
				HasMore bool `json:"has_more"`
			} `json:"metadata"`
		}{Results: []day{}}
		resp.Metadata.Page = page
		if page >= 1 && page <= len(dates) {
			d := day{Date: dates[page-1]}
			d.Breakdown.Entities = map[string]entity{}
			for tag, spend := range f.days[dates[page-1]] {
				d.Breakdown.Entities[tag] = entity{Metrics: metrics{Spend: spend, APIRequests: 1}}
			}
			resp.Results = append(resp.Results, d)
			resp.Metadata.HasMore = page < len(dates)
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		http.NotFound(w, r)
	}
}

func newDailyTestReader(source tagSpendSource, clock *fakeClock) *tagSpendReader {
	r := newTagSpendReader("k", 0, quietLogger())
	r.source = source
	r.now = clock.now
	return r
}

// totalsOf reads baseURL and returns every tag's total, failing on error.
func totalsOf(t *testing.T, r *tagSpendReader, baseURL string) map[string]float64 {
	t.Helper()
	entries, err := r.Read(context.Background(), baseURL, true)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	m := map[string]float64{}
	for _, e := range entries {
		m[e.IndividualRequestTag] = e.TotalSpend
	}
	return m
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The first read rebuilds the settled days, later reads re-read only today and
// yesterday, and a late write to yesterday is picked up (gd-Um9gnz9FuW
// constraint 3: no full re-read every poll).
func TestTagSpendDaily_RebuildThenTrailingOnly(t *testing.T) {
	f := &fakeDailyLiteLLM{}
	f.set("2026-10-01", "agent:a", 1)
	f.set("2026-10-01", "project:p", 2)
	f.set("2026-10-01", "Unassigned", 50)
	f.set("2026-10-29", "agent:a", 3)
	f.set("2026-10-30", "agent:a", 4)
	f.set("2026-10-31", "agent:a", 5)
	srv := httptest.NewServer(f)
	defer srv.Close()
	clock := &fakeClock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	r := newDailyTestReader(tagSourceDaily, clock)

	got := totalsOf(t, r, srv.URL)
	if got["agent:a"] != 13 || got["project:p"] != 2 || len(got) != 2 {
		t.Fatalf("rebuild totals = %v, want agent:a 13, project:p 2 (Unassigned dropped)", got)
	}
	wantReqs := []string{
		"2000-01-01..2026-10-29 p1", "2000-01-01..2026-10-29 p2",
		"2026-10-30..2026-10-31 p1", "2026-10-30..2026-10-31 p2",
	}
	if reqs := f.takeDailyReqs(); !equalStrings(reqs, wantReqs) {
		t.Errorf("rebuild requests = %v, want %v", reqs, wantReqs)
	}

	f.set("2026-10-30", "agent:a", 4.5)
	f.set("2026-10-31", "agent:a", 6)
	clock.advance(5 * time.Minute)
	if got := totalsOf(t, r, srv.URL); got["agent:a"] != 14.5 {
		t.Errorf("trailing re-read agent:a = %v, want 14.5", got["agent:a"])
	}
	wantReqs = []string{"2026-10-30..2026-10-31 p1", "2026-10-30..2026-10-31 p2"}
	if reqs := f.takeDailyReqs(); !equalStrings(reqs, wantReqs) {
		t.Errorf("steady-state requests = %v, want only the trailing window %v", reqs, wantReqs)
	}
	if f.legacyReads != 0 {
		t.Errorf("/spend/tags reads = %d, want 0 in daily mode", f.legacyReads)
	}
}

// At the date rollover the day that leaves the trailing window is read once
// more and settled; a failed read leaves the previous cache intact.
func TestTagSpendDaily_RolloverSettlesAndFailureKeepsCache(t *testing.T) {
	f := &fakeDailyLiteLLM{}
	f.set("2026-10-29", "agent:a", 3)
	f.set("2026-10-30", "agent:a", 4)
	f.set("2026-10-31", "agent:a", 5)
	srv := httptest.NewServer(f)
	defer srv.Close()
	clock := &fakeClock{t: time.Date(2026, 10, 31, 23, 58, 0, 0, time.UTC)}
	r := newDailyTestReader(tagSourceDaily, clock)

	if got := totalsOf(t, r, srv.URL); got["agent:a"] != 12 {
		t.Fatalf("first read = %v, want 12", got["agent:a"])
	}
	f.takeDailyReqs()

	clock.advance(5 * time.Minute) // 2026-11-01 00:03
	f.set("2026-10-31", "agent:a", 5.25)
	f.set("2026-11-01", "agent:a", 1)
	f.mu.Lock()
	f.failDaily = true
	f.mu.Unlock()
	if got := totalsOf(t, r, srv.URL); got["agent:a"] != 12 {
		t.Fatalf("failed read = %v, want the last good snapshot 12", got["agent:a"])
	}
	f.mu.Lock()
	f.failDaily = false
	f.mu.Unlock()
	f.takeDailyReqs()

	if got := totalsOf(t, r, srv.URL); got["agent:a"] != 13.25 {
		t.Errorf("after rollover = %v, want 3+4+5.25+1 = 13.25", got["agent:a"])
	}
	wantReqs := []string{
		"2026-10-30..2026-10-30 p1",
		"2026-10-31..2026-11-01 p1", "2026-10-31..2026-11-01 p2",
	}
	if reqs := f.takeDailyReqs(); !equalStrings(reqs, wantReqs) {
		t.Errorf("rollover requests = %v, want settle 10-30 then trailing %v", reqs, wantReqs)
	}
}

// auto serves /spend/tags while the sources disagree, and cuts over to
// DailyTagSpend for good once they agree (constraint 1: the gauge never steps
// at cutover; gd-Um9gnz9FuW).
func TestTagSpendDaily_AutoCutsOverOnlyOnParity(t *testing.T) {
	f := &fakeDailyLiteLLM{
		legacy: []tagSpendEntry{
			{IndividualRequestTag: "agent:a", TotalSpend: 10},
			{IndividualRequestTag: "project:p", TotalSpend: 40},
			{IndividualRequestTag: "User-Agent: claude-cli", TotalSpend: 99},
		},
	}
	// DailyTagSpend is missing project:p's oldest day.
	f.set("2026-10-30", "agent:a", 6)
	f.set("2026-10-31", "agent:a", 4)
	f.set("2026-10-31", "project:p", 30)
	srv := httptest.NewServer(f)
	defer srv.Close()
	clock := &fakeClock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	r := newDailyTestReader(tagSourceAuto, clock)

	if got := totalsOf(t, r, srv.URL); got["project:p"] != 40 || got["User-Agent: claude-cli"] != 99 {
		t.Fatalf("disagreeing sources: served %v, want the /spend/tags totals", got)
	}
	clock.advance(5 * time.Minute)
	if got := totalsOf(t, r, srv.URL); got["project:p"] != 40 {
		t.Fatalf("still disagreeing: project:p = %v, want 40", got["project:p"])
	}
	if f.legacyReads != 2 {
		t.Errorf("/spend/tags reads = %d, want 2 before cutover", f.legacyReads)
	}

	// Within tolerance (0.5% of 40 = 0.20; $0.50 floor): cut over. The
	// User-Agent tag, absent from the aggregate here, does not gate it.
	f.set("2026-10-31", "project:p", 39.9)
	clock.advance(5 * time.Minute)
	if got := totalsOf(t, r, srv.URL); got["project:p"] != 39.9 {
		t.Fatalf("at cutover project:p = %v, want the DailyTagSpend 39.9", got["project:p"])
	}
	f.set("2026-10-31", "project:p", 40.9)
	clock.advance(5 * time.Minute)
	if got := totalsOf(t, r, srv.URL); got["project:p"] != 40.9 {
		t.Errorf("after cutover project:p = %v, want 40.9", got["project:p"])
	}
	if f.legacyReads != 3 {
		t.Errorf("/spend/tags reads = %d, want none after the cutover read (3)", f.legacyReads)
	}
}

// TestTagSpendParity: the cutover parity check also compares the agent:* sum,
// so per-tag drift inside tolerance cannot add up unseen (gd-Um9gnz9FuW).
func TestTagSpendParity(t *testing.T) {
	legacy := []tagSpendEntry{
		{IndividualRequestTag: "agent:a", TotalSpend: 1.00},
		{IndividualRequestTag: "agent:b", TotalSpend: 2.00},
		{IndividualRequestTag: "model:opus", TotalSpend: 1000},
		{IndividualRequestTag: "User-Agent: x", TotalSpend: 5},
	}
	daily := []tagSpendEntry{
		{IndividualRequestTag: "agent:a", TotalSpend: 1.40},
		{IndividualRequestTag: "agent:b", TotalSpend: 2.40},
		{IndividualRequestTag: "model:opus", TotalSpend: 996},
	}
	rep := tagSpendParity(legacy, daily, nil)
	// agent:a and agent:b are each within $0.50, model:opus within 0.5%, but
	// the agent:* sum is off by $0.80.
	if rep.mismatched != 1 || rep.compared != 4 {
		t.Fatalf("report = %+v, want 4 compared, 1 mismatched (agent:*)", rep)
	}
	if len(rep.worst) != 1 || rep.worst[0][:7] != "agent:*" {
		t.Errorf("worst = %v, want the agent:* sum", rep.worst)
	}
	if rep.maxTag != "model:opus" || rep.maxDelta != 4 {
		t.Errorf("max delta = %v on %q, want 4 on model:opus", rep.maxDelta, rep.maxTag)
	}

	// A tag missing from one side compares against 0.
	rep = tagSpendParity(legacy[:1], append(daily[:1:1], tagSpendEntry{IndividualRequestTag: "role:crew", TotalSpend: 3}), nil)
	if rep.mismatched != 1 || rep.worst[0][:9] != "role:crew" {
		t.Errorf("one-sided tag: report = %+v, want role:crew mismatched", rep)
	}
}

// TestTagSpendParity_StaticGap: a mismatched tag whose two totals have not
// moved since the previous comparison is a static gap and does not block the
// cutover (gd-Jp8wOpNeh8: two dead agent tags, about 5% short in
// DailyTagSpend, held every comparison for 72h). A gap that moves, a tag still
// taking spend, a first comparison and a gap over 1/8 still block.
func TestTagSpendParity_StaticGap(t *testing.T) {
	legacy := []tagSpendEntry{
		{IndividualRequestTag: "agent:dead", TotalSpend: 11.4974},
		{IndividualRequestTag: "agent:hole", TotalSpend: 10},
		{IndividualRequestTag: "agent:big", TotalSpend: 1000}, // keeps agent:* inside 0.5%
		{IndividualRequestTag: "project:p", TotalSpend: 10000},
	}
	daily := []tagSpendEntry{
		{IndividualRequestTag: "agent:dead", TotalSpend: 10.8967},
		{IndividualRequestTag: "agent:hole", TotalSpend: 8}, // 20% short
		{IndividualRequestTag: "agent:big", TotalSpend: 1000},
		{IndividualRequestTag: "project:p", TotalSpend: 10000},
	}

	first := tagSpendParity(legacy, daily, nil)
	if first.mismatched != 2 || len(first.static) != 0 {
		t.Fatalf("first comparison = %+v, want agent:dead and agent:hole blocking", first)
	}
	if g := first.gaps["agent:dead"]; g.spendTags != 11.4974 || g.daily != 10.8967 {
		t.Fatalf("gaps[agent:dead] = %+v, want both totals", g)
	}

	again := tagSpendParity(legacy, daily, first.gaps)
	if again.mismatched != 1 || len(again.worst) != 1 || again.worst[0][:10] != "agent:hole" {
		t.Fatalf("unchanged read = %+v, want only agent:hole (over 1/8) blocking", again)
	}
	if len(again.static) != 1 || again.static[0][:10] != "agent:dead" {
		t.Errorf("static = %v, want agent:dead", again.static)
	}
	if _, ok := again.gaps["agent:dead"]; !ok {
		t.Errorf("gaps dropped the static tag; it must stay static on the next read")
	}

	// A live tag: both sides grew by the same amount, so the delta is the
	// same, but spend landed between the reads and a flush race is possible.
	live := []tagSpendEntry{{IndividualRequestTag: "agent:dead", TotalSpend: 12.4974}}
	liveDaily := []tagSpendEntry{{IndividualRequestTag: "agent:dead", TotalSpend: 11.8967}}
	if rep := tagSpendParity(live, liveDaily, first.gaps); len(rep.static) != 0 || rep.mismatched == 0 {
		t.Errorf("live tag with the same delta = %+v, want it blocking", rep)
	}
	// The aggregate caught up a little: the gap moved.
	moved := []tagSpendEntry{{IndividualRequestTag: "agent:dead", TotalSpend: 11.4974}}
	movedDaily := []tagSpendEntry{{IndividualRequestTag: "agent:dead", TotalSpend: 10.95}}
	if rep := tagSpendParity(moved, movedDaily, first.gaps); len(rep.static) != 0 || rep.mismatched == 0 {
		t.Errorf("moving gap = %+v, want it blocking", rep)
	}
}

// A dead tag's static gap lets auto cut over on the second comparison; a live
// tag's gap keeps it on /spend/tags (gd-Jp8wOpNeh8).
func TestTagSpendDaily_AutoCutsOverPastStaticGap(t *testing.T) {
	f := &fakeDailyLiteLLM{
		legacy: []tagSpendEntry{
			{IndividualRequestTag: "agent:dead", TotalSpend: 26.4622},
			{IndividualRequestTag: "agent:live", TotalSpend: 3},
			{IndividualRequestTag: "agent:big", TotalSpend: 1000}, // keeps agent:* inside 0.5%
		},
	}
	f.set("2026-10-24", "agent:dead", 25.9070)
	f.set("2026-10-24", "agent:big", 1000)
	f.set("2026-10-31", "agent:live", 2) // short by $1 while live
	srv := httptest.NewServer(f)
	defer srv.Close()
	clock := &fakeClock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	r := newDailyTestReader(tagSourceAuto, clock)

	if got := totalsOf(t, r, srv.URL); got["agent:dead"] != 26.4622 {
		t.Fatalf("first read: agent:dead = %v, want /spend/tags 26.4622", got["agent:dead"])
	}
	// agent:live takes spend on both sides: its gap is not static.
	f.legacy[1].TotalSpend = 4
	f.set("2026-10-31", "agent:live", 3)
	clock.advance(5 * time.Minute)
	if got := totalsOf(t, r, srv.URL); got["agent:dead"] != 26.4622 {
		t.Fatalf("live gap: agent:dead = %v, want /spend/tags still served", got["agent:dead"])
	}
	// agent:live catches up; agent:dead is unchanged on both sides.
	f.set("2026-10-31", "agent:live", 4)
	clock.advance(5 * time.Minute)
	if got := totalsOf(t, r, srv.URL); got["agent:dead"] != 25.9070 {
		t.Fatalf("static gap only: agent:dead = %v, want the DailyTagSpend 25.9070", got["agent:dead"])
	}
	if f.legacyReads != 3 {
		t.Errorf("/spend/tags reads = %d, want 3 (cut over on the third)", f.legacyReads)
	}
}

func TestParseTagSpendSource(t *testing.T) {
	for in, want := range map[string]tagSpendSource{"": tagSourceAuto, " daily ": tagSourceDaily, "spend-tags": tagSourceSpendTags, "auto": tagSourceAuto} {
		got, err := parseTagSpendSource(in)
		if err != nil || got != want {
			t.Errorf("parseTagSpendSource(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseTagSpendSource("logs"); err == nil {
		t.Error("parseTagSpendSource(logs): want an error")
	}
}

// The reconciler's baselines came from /spend/tags. While the sources disagree
// on an agent's tag, auto keeps closing on /spend/tags, so the session cost is
// measured against the same source as its baseline (constraint 2). Closing on
// the lower DailyTagSpend total would book 0.75, not 3.50.
func TestReconcileAtClose_SourcesDisagreeKeepsBaselineSource(t *testing.T) {
	f := &fakeDailyLiteLLM{legacy: []tagSpendEntry{{IndividualRequestTag: "agent:alpha", TotalSpend: 7.75}}}
	f.set("2026-10-31", "agent:alpha", 5.00)
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{
			spendBaselineField: "4.25", spendFirstBaselineField: "4.25",
		})},
	}
	r, _ := newTestReconciler(t, daemon, f)
	clock := &fakeClock{t: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)}
	r.tags = newDailyTestReader(tagSourceAuto, clock)

	bead := &beadsapi.BeadDetail{
		ID:     "crew-acme-crew-alpha",
		Fields: map[string]string{spendBaselineField: "4.25", spendFirstBaselineField: "4.25"},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("no settlement written")
	}
	if cost, _ := updates[0].Fields["total_cost_usd"].(float64); cost != 3.50 {
		t.Errorf("total_cost_usd = %v, want 3.50 (7.75 from /spend/tags - 4.25)", cost)
	}
	if f.legacyReads != 1 {
		t.Errorf("/spend/tags reads = %d, want 1", f.legacyReads)
	}
}
