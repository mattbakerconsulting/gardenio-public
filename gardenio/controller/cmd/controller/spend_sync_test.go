package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// serveSpendLogs serves the TWO SHAPES real LiteLLM exposes, from one set of
// per-request fixture rows. The shape difference is the whole of the
// boundary-day bug, so a fixture that collapses it cannot catch a regression:
//
//   - /spend/logs/v2 — paginated PER-REQUEST rows with true timestamps,
//     filtered server-side by api_key (verified live: a bogus api_key returns
//     zero rows; it is request_tags, not api_key, that the proxy ignores).
//   - /spend/logs — per-CALENDAR-DAY aggregates whose startTime is a bare date,
//     which parseSpendTime reads as that day's MIDNIGHT. These tests previously
//     fed per-request rows here, which is why a bug that only bites on
//     mid-day window boundaries survived a green suite.
//
// Returns true when it handled the request.
func serveSpendLogs(w http.ResponseWriter, r *http.Request, spendLogs map[string][]spendLogEntry) bool {
	rows := spendLogs[r.URL.Query().Get("api_key")]

	if strings.HasPrefix(r.URL.Path, "/spend/logs/v2") {
		from, errF := time.Parse(spendSliceTimeFormat, r.URL.Query().Get("start_date"))
		to, errT := time.Parse(spendSliceTimeFormat, r.URL.Query().Get("end_date"))
		data := []map[string]any{}
		for i, e := range rows {
			t, ok := parseSpendTime(e.StartTime)
			if !ok {
				continue
			}
			if errF == nil && t.Before(from) {
				continue
			}
			if errT == nil && t.After(to) {
				continue
			}
			data = append(data, map[string]any{
				"request_id": fmt.Sprintf("req-%d", i),
				"spend":      e.Spend,
				"startTime":  e.StartTime,
			})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": data, "total": len(data),
			"page": 1, "page_size": 100, "total_pages": 1,
		})
		return true
	}

	if strings.HasPrefix(r.URL.Path, "/spend/logs") {
		byDay := map[string]float64{}
		for _, e := range rows {
			if t, ok := parseSpendTime(e.StartTime); ok {
				byDay[t.UTC().Format("2006-01-02")] += e.Spend
			}
		}
		out := []spendLogEntry{}
		for day, sum := range byDay {
			out = append(out, spendLogEntry{Spend: sum, StartTime: day})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].StartTime < out[j].StartTime })
		json.NewEncoder(w).Encode(out)
		return true
	}
	return false
}

// hk is a test helper that returns the SHA256 hash of a key (same as hashKey in production).
func hk(virtualKey string) string { return hashKey(virtualKey) }

// fixedNow is the deterministic instant these tests anchor their seeded spend
// rows and the spendSync clock to. Wednesday 2026-04-15 12:00 UTC: its
// Sunday-anchored week (syncAll) opened 2026-04-12, so "today" and "yesterday"
// rows both fall inside the weekly window on EVERY run. Using the real wall
// clock instead made TestSpendSync_{BasicDailyAndWeeklyAggregation,
// RFC3339TimeFormat,DateOnlyStartTime} red every Sunday UTC — on a Sunday
// Weekday()==0 collapses weekStart onto today, dropping the yesterday rows out
// of the week (calendar tripwire ka-0000003815; prod code is correct, the tests
// were weekday-dependent). Pinning the clock via ss.now removes that seam.
var fixedNow = time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

// fixedClock returns a clock function that always reports fixedNow, for
// injection into spendSync.now.
func fixedClock() func() time.Time { return func() time.Time { return fixedNow } }

// daemonStub simulates the beads daemon HTTP API for spend sync tests.
type daemonStub struct {
	mu      sync.Mutex
	beads   []map[string]any // list response beads
	updates []daemonUpdate   // captured PATCH calls
	// onUpdate, when non-nil, is invoked (off the stub's lock) after each PATCH
	// is recorded. Tests that drive an async worker use it to synchronize on the
	// write actually landing rather than polling a wall-clock deadline.
	onUpdate func()
}

type daemonUpdate struct {
	BeadID string
	Fields map[string]any
}

func (d *daemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/") && !strings.Contains(r.URL.Path[len("/v1/beads/"):], "/"):
			// GET /v1/beads/:id — used by UpdateBeadFieldsTyped to read current fields.
			beadID := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			d.mu.Lock()
			defer d.mu.Unlock()
			for _, b := range d.beads {
				if b["id"] == beadID {
					json.NewEncoder(w).Encode(b)
					return
				}
			}
			http.NotFound(w, r)

		case r.Method == "GET" && r.URL.Path == "/v1/beads":
			// ListBeadsFiltered
			d.mu.Lock()
			resp := map[string]any{"beads": d.beads, "total": len(d.beads)}
			d.mu.Unlock()
			json.NewEncoder(w).Encode(resp)

		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// UpdateBeadFieldsTyped PATCH
			beadID := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var patch map[string]json.RawMessage
			json.Unmarshal(body, &patch)
			var fields map[string]any
			if raw, ok := patch["fields"]; ok {
				json.Unmarshal(raw, &fields)
			}
			d.mu.Lock()
			d.updates = append(d.updates, daemonUpdate{BeadID: beadID, Fields: fields})
			notify := d.onUpdate
			d.mu.Unlock()
			if notify != nil {
				notify()
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"ok": true})

		default:
			http.NotFound(w, r)
		}
	})
}

func (d *daemonStub) getUpdates() []daemonUpdate {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]daemonUpdate, len(d.updates))
	copy(out, d.updates)
	return out
}

// makeDaemonBead builds a bead JSON object as the daemon would return it.
func makeDaemonBead(id, title, litellmKey string) map[string]any {
	fields := map[string]any{}
	if litellmKey != "" {
		fields["litellm_key"] = litellmKey
	}
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":     id,
		"title":  title,
		"type":   "account",
		"status": "open",
		"fields": json.RawMessage(fieldsRaw),
	}
}

// makeDaemonBeadWithFields is makeDaemonBead plus arbitrary extra bead fields
// (e.g. throttle_reset_5h_at), for exercising the subscription-window path.
func makeDaemonBeadWithFields(id, title, litellmKey string, extra map[string]any) map[string]any {
	fields := map[string]any{}
	if litellmKey != "" {
		fields["litellm_key"] = litellmKey
	}
	for k, v := range extra {
		fields[k] = v
	}
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":     id,
		"title":  title,
		"type":   "account",
		"status": "open",
		"fields": json.RawMessage(fieldsRaw),
	}
}

// litellmRouter builds an HTTP handler that simulates LiteLLM endpoints.
func litellmRouter(keySpend map[string]float64, spendLogs map[string][]spendLogEntry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/key/info"):
			key := r.URL.Query().Get("key")
			spend, ok := keySpend[key]
			if !ok {
				http.Error(w, "key not found", http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{"spend": spend},
			})

		case serveSpendLogs(w, r, spendLogs):
			// handled

		default:
			http.NotFound(w, r)
		}
	})
}

func newTestSpendSync(t *testing.T, daemon *daemonStub, litellmHandler http.Handler) (*spendSync, *httptest.Server, *httptest.Server) {
	t.Helper()

	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)

	litellmSrv := httptest.NewServer(litellmHandler)
	t.Cleanup(litellmSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	ss := newSpendSync(client, nil, litellmSrv.URL, "test-master-key", slog.Default())
	// Pin the clock so daily/weekly windowing is weekday-independent (fixedNow is
	// a Wednesday); see fixedNow doc and calendar tripwire ka-0000003815.
	ss.now = fixedClock()
	return ss, daemonSrv, litellmSrv
}

func TestSpendSync_BasicDailyAndWeeklyAggregation(t *testing.T) {
	now := fixedNow
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-acct1", "Acct One", "vk-111"),
		},
	}

	// Two entries today, one from yesterday.
	// The /spend/logs endpoint indexes by SHA256-hashed key.
	spendLogs := map[string][]spendLogEntry{
		hk("vk-111"): {
			{Spend: 1.50, StartTime: dayStart.Add(2 * time.Hour).Format("2006-01-02T15:04:05")},
			{Spend: 0.75, StartTime: dayStart.Add(5 * time.Hour).Format("2006-01-02T15:04:05")},
			{Spend: 3.00, StartTime: dayStart.AddDate(0, 0, -1).Format("2006-01-02T15:04:05")},
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-111": 42.50},
		spendLogs,
	))

	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected at least one update")
	}

	u := updates[len(updates)-1]
	if u.BeadID != "bd-acct1" {
		t.Errorf("expected update for bd-acct1, got %s", u.BeadID)
	}

	// total_cost_usd should be the /key/info spend value.
	total, ok := u.Fields["total_cost_usd"].(float64)
	if !ok {
		t.Fatalf("total_cost_usd should be float64, got %T: %v", u.Fields["total_cost_usd"], u.Fields["total_cost_usd"])
	}
	if total != 42.50 {
		t.Errorf("total_cost_usd = %v, want 42.50", total)
	}

	// daily = 1.50 + 0.75 = 2.25
	daily, ok := u.Fields["daily_cost_usd"].(float64)
	if !ok {
		t.Fatalf("daily_cost_usd should be float64, got %T", u.Fields["daily_cost_usd"])
	}
	if math.Abs(daily-2.25) > 0.001 {
		t.Errorf("daily_cost_usd = %v, want 2.25", daily)
	}

	// weekly = 1.50 + 0.75 + 3.00 = 5.25
	weekly, ok := u.Fields["weekly_cost_usd"].(float64)
	if !ok {
		t.Fatalf("weekly_cost_usd should be float64, got %T", u.Fields["weekly_cost_usd"])
	}
	if math.Abs(weekly-5.25) > 0.001 {
		t.Errorf("weekly_cost_usd = %v, want 5.25", weekly)
	}

	// shadow_* fields mirror the canonical values for POC reconciliation (D-h3).
	for field, want := range map[string]float64{
		"shadow_total_cost_usd":  42.50,
		"shadow_daily_cost_usd":  2.25,
		"shadow_weekly_cost_usd": 5.25,
	} {
		got, ok := u.Fields[field].(float64)
		if !ok {
			t.Fatalf("%s should be float64, got %T: %v", field, u.Fields[field], u.Fields[field])
		}
		if math.Abs(got-want) > 0.001 {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
}

func TestSpendSync_CostFieldsAreNumericNotStrings(t *testing.T) {
	// Regression test for fix 1888f2a6: total_cost_usd must be written as a
	// JSON number (float64), not a string.
	now := fixedNow
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-acct1", "Acct One", "vk-111"),
		},
	}

	spendLogs := map[string][]spendLogEntry{
		hk("vk-111"): {
			{Spend: 10.0, StartTime: dayStart.Add(time.Hour).Format("2006-01-02T15:04:05")},
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-111": 99.99},
		spendLogs,
	))

	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected update")
	}

	u := updates[len(updates)-1]
	for _, field := range []string{"total_cost_usd", "daily_cost_usd", "weekly_cost_usd"} {
		v := u.Fields[field]
		if _, ok := v.(float64); !ok {
			t.Errorf("%s should be float64 (JSON number), got %T: %v", field, v, v)
		}
		if _, ok := v.(string); ok {
			t.Errorf("%s was written as a string — regression of fix 1888f2a6", field)
		}
	}
}

func TestSpendSync_SkipsAccountsWithoutLitellmKey(t *testing.T) {
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-nokey", "No Key Account", ""),
			makeDaemonBead("bd-haskey", "Has Key", "vk-222"),
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-222": 5.0},
		map[string][]spendLogEntry{hk("vk-222"): {}},
	))

	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	for _, u := range updates {
		if u.BeadID == "bd-nokey" {
			t.Error("should not update account without litellm_key")
		}
	}
	// bd-haskey should have been updated.
	found := false
	for _, u := range updates {
		if u.BeadID == "bd-haskey" {
			found = true
		}
	}
	if !found {
		t.Error("expected update for bd-haskey")
	}
}

func TestSpendSync_LitellmAPIError(t *testing.T) {
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-acct1", "Acct One", "vk-err"),
		},
	}

	// Return 500 for /key/info, spend/logs.
	errHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})

	ss, _, _ := newTestSpendSync(t, daemon, errHandler)
	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) != 0 {
		t.Errorf("expected no updates when LiteLLM returns errors, got %d", len(updates))
	}
}

func TestSpendSync_LitellmUnreachable(t *testing.T) {
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-acct1", "Acct One", "vk-111"),
		},
	}

	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	// Point to a closed server.
	ss := newSpendSync(client, nil, "http://127.0.0.1:1", "key", slog.Default())

	// Should not panic or hang.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ss.syncAll(ctx)

	updates := daemon.getUpdates()
	if len(updates) != 0 {
		t.Errorf("expected no updates when LiteLLM is unreachable, got %d", len(updates))
	}
}

func TestSpendSync_MultipleAccounts(t *testing.T) {
	now := fixedNow
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-a", "Alpha", "vk-a"),
			makeDaemonBead("bd-b", "Beta", "vk-b"),
		},
	}

	spendLogs := map[string][]spendLogEntry{
		hk("vk-a"): {{Spend: 1.0, StartTime: dayStart.Add(time.Hour).Format("2006-01-02T15:04:05")}},
		hk("vk-b"): {{Spend: 2.0, StartTime: dayStart.Add(time.Hour).Format("2006-01-02T15:04:05")}},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-a": 10.0, "vk-b": 20.0},
		spendLogs,
	))

	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	seen := map[string]bool{}
	for _, u := range updates {
		seen[u.BeadID] = true
	}
	if !seen["bd-a"] {
		t.Error("expected update for bd-a")
	}
	if !seen["bd-b"] {
		t.Error("expected update for bd-b")
	}
}

func TestSpendSync_RFC3339TimeFormat(t *testing.T) {
	// The daily/weekly spend windows should parse both bare datetime and RFC3339 formats.
	now := fixedNow
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-rfc", "RFC Acct", "vk-rfc"),
		},
	}

	spendLogs := map[string][]spendLogEntry{
		hk("vk-rfc"): {
			{Spend: 5.0, StartTime: dayStart.Add(3 * time.Hour).Format(time.RFC3339)},
			{Spend: 2.0, StartTime: dayStart.Add(-time.Hour).Format(time.RFC3339)},
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-rfc": 100.0},
		spendLogs,
	))

	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected update")
	}

	u := updates[len(updates)-1]
	daily := u.Fields["daily_cost_usd"].(float64)
	weekly := u.Fields["weekly_cost_usd"].(float64)

	// Only the 5.0 entry is today.
	if math.Abs(daily-5.0) > 0.001 {
		t.Errorf("daily_cost_usd = %v, want 5.0", daily)
	}
	// Both entries are this week.
	if math.Abs(weekly-7.0) > 0.001 {
		t.Errorf("weekly_cost_usd = %v, want 7.0", weekly)
	}
}

func TestSpendSync_EmptySpendLogs(t *testing.T) {
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-empty", "Empty", "vk-empty"),
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-empty": 50.0},
		map[string][]spendLogEntry{hk("vk-empty"): {}},
	))

	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected update")
	}

	u := updates[len(updates)-1]
	if u.Fields["total_cost_usd"].(float64) != 50.0 {
		t.Errorf("total_cost_usd = %v, want 50.0", u.Fields["total_cost_usd"])
	}
	if u.Fields["daily_cost_usd"].(float64) != 0.0 {
		t.Errorf("daily_cost_usd = %v, want 0.0", u.Fields["daily_cost_usd"])
	}
	if u.Fields["weekly_cost_usd"].(float64) != 0.0 {
		t.Errorf("weekly_cost_usd = %v, want 0.0", u.Fields["weekly_cost_usd"])
	}
}

func TestQuerySpend_AuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{
			"info": map[string]any{"spend": 1.0},
		})
	}))
	defer srv.Close()

	ss := &spendSync{
		litellmURL: srv.URL,
		masterKey:  "sk-master-123",
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     slog.Default(),
		badKeys:    make(map[string]time.Time),
		now:        fixedClock(),
	}

	_, err := ss.queryKeyInfo(context.Background(), ss.litellmURL, "vk-test")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-master-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-master-123")
	}
}

func TestSumSpendInWindow_DayBoundary(t *testing.T) {
	// Entries exactly at dayStart should count as today.
	dayStart := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	weekStart := dayStart.AddDate(0, 0, -int(dayStart.Weekday()))

	logs := []spendLogEntry{
		{Spend: 1.0, StartTime: dayStart.Format("2006-01-02T15:04:05")},                     // exactly midnight today
		{Spend: 2.0, StartTime: dayStart.Add(-time.Second).Format("2006-01-02T15:04:05")},   // 1s before today
		{Spend: 4.0, StartTime: dayStart.Add(12 * time.Hour).Format("2006-01-02T15:04:05")}, // midday today
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(logs)
	}))
	defer srv.Close()

	ss := &spendSync{
		litellmURL: srv.URL,
		masterKey:  "key",
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     slog.Default(),
		badKeys:    make(map[string]time.Time),
		now:        fixedClock(),
	}

	logs, ok := ss.fetchSpendLogsSince(context.Background(), ss.litellmURL, "vk-x", weekStart)
	if !ok {
		t.Fatal("fetchSpendLogsSince failed")
	}
	daily, weekly := sumSpendInWindow(logs, dayStart), sumSpendInWindow(logs, weekStart)

	// Daily: midnight (1.0) + midday (4.0) = 5.0. The 23:59:59 entry (2.0) is yesterday.
	if math.Abs(daily-5.0) > 0.001 {
		t.Errorf("daily = %v, want 5.0", daily)
	}

	// Weekly: all three entries (1.0 + 2.0 + 4.0 = 7.0).
	if math.Abs(weekly-7.0) > 0.001 {
		t.Errorf("weekly = %v, want 7.0", weekly)
	}
}

func TestSumSpendInWindow_MalformedTimeSkipped(t *testing.T) {
	logs := []spendLogEntry{
		{Spend: 1.0, StartTime: "not-a-timestamp"},
		{Spend: 2.0, StartTime: "2026-04-10T12:00:00"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(logs)
	}))
	defer srv.Close()

	ss := &spendSync{
		litellmURL: srv.URL,
		masterKey:  "key",
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     slog.Default(),
		badKeys:    make(map[string]time.Time),
		now:        fixedClock(),
	}

	dayStart := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	weekStart := dayStart.AddDate(0, 0, -int(dayStart.Weekday()))

	logs, ok := ss.fetchSpendLogsSince(context.Background(), ss.litellmURL, "vk-x", weekStart)
	if !ok {
		t.Fatal("fetchSpendLogsSince failed")
	}
	daily, weekly := sumSpendInWindow(logs, dayStart), sumSpendInWindow(logs, weekStart)

	// A row with an unparseable startTime is skipped from EVERY window
	// (ka-0000003343): weekly is now a client-side windowed sum on the row's own
	// stamp, not a sum-of-all-returned-rows, because the shared fetch can reach
	// back before weekStart to cover a subscription window. So a malformed stamp
	// (which can't be placed in any window) contributes to neither daily nor
	// weekly — only the parseable 2.0 row counts.
	if math.Abs(weekly-2.0) > 0.001 {
		t.Errorf("weekly = %v, want 2.0 (malformed-stamp row skipped from weekly too)", weekly)
	}
	if math.Abs(daily-2.0) > 0.001 {
		t.Errorf("daily = %v, want 2.0 (only parseable today entry)", daily)
	}
}

func TestQueryKeyInfo_HTTP404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	ss := &spendSync{
		litellmURL: srv.URL,
		masterKey:  "key",
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     slog.Default(),
		badKeys:    make(map[string]time.Time),
		now:        fixedClock(),
	}

	_, err := ss.queryKeyInfo(context.Background(), ss.litellmURL, "vk-bad")
	if err == nil {
		t.Error("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404: %v", err)
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is way too long", 10, "this is..."},
		{"abc", 3, "abc"},
		{"abcd", 3, "..."},
	}
	for _, tt := range tests {
		got := truncateStr(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
	}
}

func TestHashKey(t *testing.T) {
	// hashKey must produce a stable SHA256 hex digest.
	got := hashKey("sk-OfWyjlevpZxctj7jx_p5XA")
	want := "f7b848ae06aa694b4cc6c606223a7c896183ee43804f5a1e29a6a5cffbfc37d7"
	if got != want {
		t.Errorf("hashKey() = %q, want %q", got, want)
	}
}

func TestParseSpendTime(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
		day   int // expected day-of-month
	}{
		{"2026-04-11", true, 11},
		{"2026-04-11T14:30:00", true, 11},
		{"2026-04-11T14:30:00Z", true, 11},
		{"not-a-date", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := parseSpendTime(tt.input)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got.Day() != tt.day {
				t.Errorf("day = %d, want %d", got.Day(), tt.day)
			}
		})
	}
}

func TestSpendSync_DateOnlyStartTime(t *testing.T) {
	// LiteLLM /spend/logs returns startTime as "2026-04-11" (date only).
	// This test verifies the date-only format is correctly parsed for daily aggregation.
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-dateonly", "DateOnly", "vk-do"),
		},
	}

	today := fixedNow.Format("2006-01-02")
	yesterday := fixedNow.AddDate(0, 0, -1).Format("2006-01-02")

	spendLogs := map[string][]spendLogEntry{
		hk("vk-do"): {
			{Spend: 10.0, StartTime: today},
			{Spend: 5.0, StartTime: yesterday},
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-do": 100.0},
		spendLogs,
	))

	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected update")
	}

	u := updates[len(updates)-1]
	daily := u.Fields["daily_cost_usd"].(float64)
	weekly := u.Fields["weekly_cost_usd"].(float64)

	if math.Abs(daily-10.0) > 0.001 {
		t.Errorf("daily_cost_usd = %v, want 10.0 (only today's date-only entry)", daily)
	}
	if math.Abs(weekly-15.0) > 0.001 {
		t.Errorf("weekly_cost_usd = %v, want 15.0 (both entries)", weekly)
	}
}

func TestSpendSync_RunLoopStopsOnCancel(t *testing.T) {
	daemon := &daemonStub{beads: []map[string]any{}}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ss.RunLoop(ctx)
		close(done)
	}()

	// Cancel should stop the loop promptly.
	cancel()
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Error("RunLoop did not stop after context cancellation")
	}
}

// makeDaemonBeadWithBaseURL is makeDaemonBead with a per-account base_url field.
func makeDaemonBeadWithBaseURL(id, title, litellmKey, baseURL string) map[string]any {
	fields := map[string]any{}
	if litellmKey != "" {
		fields["litellm_key"] = litellmKey
	}
	if baseURL != "" {
		fields["base_url"] = baseURL
	}
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":     id,
		"title":  title,
		"type":   "account",
		"status": "open",
		"fields": json.RawMessage(fieldsRaw),
	}
}

func TestAccountBaseURL(t *testing.T) {
	// Constructed directly (not via newSpendSync, which already trims), so the
	// global carries a trailing slash to exercise the fallback-trim path. In
	// production s.litellmURL is pre-trimmed, making the trim a no-op there.
	ss := &spendSync{litellmURL: "https://global.example/"}
	// Empty/whitespace base_url falls back to the global URL, trimmed of its
	// trailing slash (resolveSpendReadBaseURL, the single-sourced read-path
	// contract — ka-0000007533).
	if got := ss.accountBaseURL(""); got != "https://global.example" {
		t.Errorf("empty base_url should fall back to trimmed global, got %q", got)
	}
	if got := ss.accountBaseURL("  "); got != "https://global.example" {
		t.Errorf("whitespace base_url should fall back to trimmed global, got %q", got)
	}
	if got := ss.accountBaseURL("http://gardenio-e2e-litellm:4000/"); got != "http://gardenio-e2e-litellm:4000" {
		t.Errorf("per-account base_url should win and be trimmed, got %q", got)
	}
}

// TestSpendSync_PerAccountBaseURLRouting verifies that an account carrying a
// non-empty base_url is queried against THAT proxy (not the global LITELLM_URL),
// while an account with no base_url falls back to the global proxy. This is the
// in-cluster migration fix (ka-0000003221): the global proxy 404s an in-cluster
// vkey, so spend must route per-account.
func TestSpendSync_PerAccountBaseURLRouting(t *testing.T) {
	// Global proxy knows only vk-global; an in-cluster vkey 404s here (like prod).
	globalSrv := httptest.NewServer(litellmRouter(
		map[string]float64{"vk-global": 10.0},
		nil,
	))
	t.Cleanup(globalSrv.Close)

	// In-cluster proxy knows only the in-cluster vkey.
	inclusterSrv := httptest.NewServer(litellmRouter(
		map[string]float64{"vk-incluster": 0.000034},
		nil,
	))
	t.Cleanup(inclusterSrv.Close)

	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBeadWithBaseURL("bd-incluster", "in-cluster acct", "vk-incluster", inclusterSrv.URL),
			makeDaemonBeadWithBaseURL("bd-global", "global acct", "vk-global", ""),
		},
	}
	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// Global URL is the EXTERNAL proxy — the in-cluster account must NOT use it.
	ss := newSpendSync(client, nil, globalSrv.URL, "test-master-key", slog.Default())
	ss.syncAll(context.Background())

	got := map[string]float64{}
	for _, u := range daemon.getUpdates() {
		if v, ok := u.Fields["total_cost_usd"].(float64); ok {
			got[u.BeadID] = v
		}
	}

	// In-cluster account resolved spend from the in-cluster proxy (would have
	// 404'd on the global proxy before this fix).
	if math.Abs(got["bd-incluster"]-0.000034) > 1e-9 {
		t.Errorf("in-cluster account total_cost_usd = %v, want 0.000034 (queried in-cluster proxy)", got["bd-incluster"])
	}
	// Account with no base_url still uses the global proxy.
	if math.Abs(got["bd-global"]-10.0) > 1e-9 {
		t.Errorf("global account total_cost_usd = %v, want 10.0 (queried global proxy)", got["bd-global"])
	}
}

func TestSpendSync_DaemonListError(t *testing.T) {
	// Daemon returns error for list — syncAll should not panic.
	errHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon down", http.StatusInternalServerError)
	})
	daemonSrv := httptest.NewServer(errHandler)
	t.Cleanup(daemonSrv.Close)

	litellmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"info": map[string]any{"spend": 0.0}})
	}))
	t.Cleanup(litellmSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	ss := newSpendSync(client, nil, litellmSrv.URL, "key", slog.Default())

	// Should not panic.
	ss.syncAll(context.Background())
}

// --- LiteLLM-enforced budget sync (bug ka-0000001246) ---

// litellmRouterWithBudget is litellmRouter whose /key/info also returns
// max_budget / budget_reset_at for keys present in keyBudget, so the budget-sync
// path can be exercised. A key absent from keyBudget returns spend only (an
// unbudgeted key), mirroring LiteLLM omitting max_budget.
func litellmRouterWithBudget(keySpend map[string]float64, keyBudget map[string]float64, keyReset map[string]string, spendLogs map[string][]spendLogEntry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/key/info"):
			key := r.URL.Query().Get("key")
			spend, ok := keySpend[key]
			if !ok {
				http.Error(w, "key not found", http.StatusNotFound)
				return
			}
			info := map[string]any{"spend": spend}
			if b, ok := keyBudget[key]; ok {
				info["max_budget"] = b
			}
			if rs, ok := keyReset[key]; ok {
				info["budget_reset_at"] = rs
			}
			json.NewEncoder(w).Encode(map[string]any{"info": info})
		case serveSpendLogs(w, r, spendLogs):
			// handled
		default:
			http.NotFound(w, r)
		}
	})
}

func TestSpendLimitFields(t *testing.T) {
	budget := 500.0
	zero := 0.0
	neg := -5.0

	cases := []struct {
		name      string
		info      liteLLMKeyInfo
		wantLimit any // nil = must be absent
		wantReset any // nil = must be absent
	}{
		{
			name:      "budgeted key with reset",
			info:      liteLLMKeyInfo{Spend: 120, MaxBudget: &budget, BudgetResetAt: "2026-08-01T00:00:00Z"},
			wantLimit: 500.0,
			wantReset: "2026-08-01T00:00:00Z",
		},
		{
			name:      "unbudgeted key (max_budget null) — both absent",
			info:      liteLLMKeyInfo{Spend: 120},
			wantLimit: nil,
			wantReset: nil,
		},
		{
			name:      "zero budget is not a real limit — absent",
			info:      liteLLMKeyInfo{Spend: 1, MaxBudget: &zero},
			wantLimit: nil,
			wantReset: nil,
		},
		{
			name:      "negative budget is not a real limit — absent",
			info:      liteLLMKeyInfo{Spend: 1, MaxBudget: &neg},
			wantLimit: nil,
			wantReset: nil,
		},
		{
			name:      "reset present without budget — only reset written",
			info:      liteLLMKeyInfo{Spend: 1, BudgetResetAt: "2026-09-01T00:00:00Z"},
			wantLimit: nil,
			wantReset: "2026-09-01T00:00:00Z",
		},
		{
			name:      "whitespace reset trimmed to absent",
			info:      liteLLMKeyInfo{Spend: 1, MaxBudget: &budget, BudgetResetAt: "   "},
			wantLimit: 500.0,
			wantReset: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := spendLimitFields(tc.info)
			if tc.wantLimit == nil {
				if _, present := got["spend_limit_usd"]; present {
					t.Errorf("spend_limit_usd must be absent, got %v", got["spend_limit_usd"])
				}
			} else if got["spend_limit_usd"] != tc.wantLimit {
				t.Errorf("spend_limit_usd = %v, want %v", got["spend_limit_usd"], tc.wantLimit)
			}
			if tc.wantReset == nil {
				if _, present := got["spend_reset_at"]; present {
					t.Errorf("spend_reset_at must be absent, got %v", got["spend_reset_at"])
				}
			} else if got["spend_reset_at"] != tc.wantReset {
				t.Errorf("spend_reset_at = %v, want %v", got["spend_reset_at"], tc.wantReset)
			}
		})
	}
}

// A budgeted key's max_budget / budget_reset_at land on the account bead so the
// dark-lane spend alert can read the real limit.
func TestSpendSync_WritesBudgetFields(t *testing.T) {
	daemon := &daemonStub{
		beads: []map[string]any{makeDaemonBead("bd-budgeted", "Budgeted", "vk-b")},
	}
	ss, _, _ := newTestSpendSync(t, daemon, litellmRouterWithBudget(
		map[string]float64{"vk-b": 320.0},
		map[string]float64{"vk-b": 500.0},
		map[string]string{"vk-b": "2026-08-15T00:00:00Z"},
		nil,
	))

	ss.syncAll(context.Background())

	u := lastUpdate(t, daemon).Fields
	if got, ok := u["spend_limit_usd"].(float64); !ok || got != 500.0 {
		t.Errorf("spend_limit_usd = %v (%T), want 500.0", u["spend_limit_usd"], u["spend_limit_usd"])
	}
	if got, _ := u["spend_reset_at"].(string); got != "2026-08-15T00:00:00Z" {
		t.Errorf("spend_reset_at = %v, want 2026-08-15T00:00:00Z", u["spend_reset_at"])
	}
	// total_cost_usd still written as before.
	if got, ok := u["total_cost_usd"].(float64); !ok || got != 320.0 {
		t.Errorf("total_cost_usd = %v, want 320.0", u["total_cost_usd"])
	}
}

// An unbudgeted key (the common case) must not write a fabricated limit — the
// account bead's budget fields stay absent.
func TestSpendSync_UnbudgetedKey_NoBudgetFields(t *testing.T) {
	daemon := &daemonStub{
		beads: []map[string]any{makeDaemonBead("bd-plain", "Plain", "vk-p")},
	}
	ss, _, _ := newTestSpendSync(t, daemon, litellmRouterWithBudget(
		map[string]float64{"vk-p": 99.0},
		nil, // no budget
		nil,
		nil,
	))

	ss.syncAll(context.Background())

	u := lastUpdate(t, daemon).Fields
	if _, present := u["spend_limit_usd"]; present {
		t.Errorf("spend_limit_usd must be absent for an unbudgeted key, got %v", u["spend_limit_usd"])
	}
	if _, present := u["spend_reset_at"]; present {
		t.Errorf("spend_reset_at must be absent for an unbudgeted key, got %v", u["spend_reset_at"])
	}
	if got, ok := u["total_cost_usd"].(float64); !ok || got != 99.0 {
		t.Errorf("total_cost_usd = %v, want 99.0", u["total_cost_usd"])
	}
}

// lastUpdate returns the most recent captured PATCH, failing if none.
func lastUpdate(t *testing.T, d *daemonStub) daemonUpdate {
	t.Helper()
	updates := d.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected at least one update")
	}
	return updates[len(updates)-1]
}

// TestSpendSync_TransparencyReadsLiteLLMNotProxy is the integration-level A1
// transparency proof (ka-0000007533): an account fronted by claudemore
// (proxy_url set) must have its spend read from litellm DIRECTLY (base_url), so
// total_cost_usd is byte-identical to the same account with no proxy. It stands
// up a real litellm stub AND a claudemore stub that FAILS the test if it ever
// receives a spend read, then drives syncAll for an account whose base_url is
// litellm — proving the read bypasses the proxy.
func TestSpendSync_TransparencyReadsLiteLLMNotProxy(t *testing.T) {
	const spend = 42.50

	// A claudemore stub that must NEVER be queried for spend. Any hit here means
	// the read path leaked onto the proxy — a transparency (A1) violation.
	var proxyHits int
	var proxyMu sync.Mutex
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyMu.Lock()
		proxyHits++
		proxyMu.Unlock()
		t.Errorf("claudemore proxy received a spend read (%s %s) — the read path must bypass the proxy (A1)", r.Method, r.URL.Path)
		http.Error(w, "claudemore does not serve spend endpoints", http.StatusNotFound)
	}))
	t.Cleanup(proxySrv.Close)

	// The real litellm the reader must query. base_url points here; proxy_url
	// points at the claudemore stub above.
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBeadWithFields("bd-proxied", "Proxied Acct", "vk-proxied", map[string]any{
				"base_url":  "", // filled after litellm server URL is known
				"proxy_url": proxySrv.URL,
			}),
		},
	}
	ss, _, litellmSrv := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-proxied": spend}, nil,
	))
	// Now that the litellm server URL is known, set the account's base_url to it.
	// (The reader resolves base_url; proxy_url must be ignored.)
	setBeadField(daemon, "bd-proxied", "base_url", litellmSrv.URL)

	ss.syncAll(context.Background())

	u := lastUpdate(t, daemon)
	if u.BeadID != "bd-proxied" {
		t.Fatalf("update bead = %s, want bd-proxied", u.BeadID)
	}
	got, ok := u.Fields["total_cost_usd"].(float64)
	if !ok || got != spend {
		t.Fatalf("total_cost_usd = %v (%T), want %v — spend must be read from litellm directly", u.Fields["total_cost_usd"], u.Fields["total_cost_usd"], spend)
	}

	proxyMu.Lock()
	hits := proxyHits
	proxyMu.Unlock()
	if hits != 0 {
		t.Fatalf("claudemore proxy was queried %d time(s) for spend — must be 0 (A1 transparency)", hits)
	}

	// Baseline equivalence: the SAME account with proxy_url absent produces the
	// identical total. This is the "identical vs litellm-direct baseline" claim.
	baselineDaemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBeadWithFields("bd-direct", "Direct Acct", "vk-proxied", map[string]any{
				"base_url": litellmSrv.URL,
				// proxy_url intentionally absent — the litellm-direct baseline.
			}),
		},
	}
	baselineClient, err := beadsapi.New(beadsapi.Config{HTTPAddr: httptest.NewServer(baselineDaemon.handler()).URL})
	if err != nil {
		t.Fatal(err)
	}
	baseline := newSpendSync(baselineClient, nil, ss.litellmURL, "test-master-key", slog.Default())
	baseline.now = fixedClock()
	baseline.syncAll(context.Background())

	bu := lastUpdate(t, baselineDaemon)
	baseTotal, ok := bu.Fields["total_cost_usd"].(float64)
	if !ok {
		t.Fatalf("baseline total_cost_usd not a float64: %v", bu.Fields["total_cost_usd"])
	}
	if baseTotal != got {
		t.Fatalf("proxied total_cost_usd = %v, litellm-direct baseline = %v — must be identical (A1)", got, baseTotal)
	}
}

// setBeadField mutates a seeded daemonStub bead's fields in place (test-only).
func setBeadField(d *daemonStub, beadID, key, val string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.beads {
		if b["id"] != beadID {
			continue
		}
		var fields map[string]any
		if raw, ok := b["fields"].(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &fields)
		}
		if fields == nil {
			fields = map[string]any{}
		}
		fields[key] = val
		raw, _ := json.Marshal(fields)
		b["fields"] = json.RawMessage(raw)
		return
	}
}

// A tenant land's account (lands-<land>) gets the purse stamp in a second
// write after the spend: spend_synced_at and each running agent's live cost.
// Every other account is written exactly as before.
func TestSpendSync_StampsLandPurse(t *testing.T) {
	agent := func(id, project, state string, live float64) map[string]any {
		raw, _ := json.Marshal(map[string]any{"project": project, "agent_state": state, "live_cost_usd": live})
		return map[string]any{
			"id": id, "title": id, "type": "agent", "status": "open",
			"labels": []string{"project:" + project}, "fields": json.RawMessage(raw),
		}
	}
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-land", "lands-tomato", "vk-land"),
			makeDaemonBead("bd-fleet", "fleet", "vk-fleet"),
			agent("ag-1", "tomato", "working", 1.25),
			agent("ag-2", "tomato", "done", 9),
			agent("ag-3", "gardenio", "working", 7),
		},
	}
	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-land": 3, "vk-fleet": 40},
		map[string][]spendLogEntry{hk("vk-land"): {}, hk("vk-fleet"): {}},
	))

	ss.syncAll(context.Background())

	var land, fleet []daemonUpdate
	for _, u := range daemon.getUpdates() {
		switch u.BeadID {
		case "bd-land":
			land = append(land, u)
		case "bd-fleet":
			fleet = append(fleet, u)
		}
	}
	if len(land) != 2 {
		t.Fatalf("lands account updates = %d, want spend then stamp", len(land))
	}
	if land[0].Fields["total_cost_usd"] != 3.0 {
		t.Errorf("first write total_cost_usd = %v, want 3", land[0].Fields["total_cost_usd"])
	}
	if _, ok := land[0].Fields[purse.FieldSpendSyncedAt]; ok {
		t.Error("the stamp must not ride the spend write")
	}
	stamp := land[1].Fields
	if stamp[purse.FieldSpendSyncedAt] != fixedNow.Format(time.RFC3339) {
		t.Errorf("spend_synced_at = %v, want %s", stamp[purse.FieldSpendSyncedAt], fixedNow.Format(time.RFC3339))
	}
	if stamp[purse.FieldLiveCostBaseline] != `{"ag-1":1.25}` {
		t.Errorf("live_cost_baseline = %v, want only the land's running agent", stamp[purse.FieldLiveCostBaseline])
	}
	if len(fleet) != 1 {
		t.Errorf("fleet account updates = %d, want 1 (no stamp)", len(fleet))
	}
}
