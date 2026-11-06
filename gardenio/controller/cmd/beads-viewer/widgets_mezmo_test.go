package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeMezmoClient is a deterministic in-memory MezmoClient. Each method
// returns the canned response unless ErrSet is set, in which case the error
// is returned. Call counts let tests assert cache hit/miss behaviour.
type fakeMezmoClient struct {
	HistogramFn func() ([]HistogramPoint, error)
	GroupByFn   func() ([]GroupBucket, error)
	DedupFn     func() ([]LogEntry, error)
	HistCalls   int
	GroupCalls  int
	DedupCalls  int
}

func (f *fakeMezmoClient) Histogram(ctx context.Context, query, window string) ([]HistogramPoint, error) {
	f.HistCalls++
	if f.HistogramFn != nil {
		return f.HistogramFn()
	}
	return nil, nil
}
func (f *fakeMezmoClient) GroupBy(ctx context.Context, query, field, window string) ([]GroupBucket, error) {
	f.GroupCalls++
	if f.GroupByFn != nil {
		return f.GroupByFn()
	}
	return nil, nil
}
func (f *fakeMezmoClient) Dedup(ctx context.Context, query, window string, limit int) ([]LogEntry, error) {
	f.DedupCalls++
	if f.DedupFn != nil {
		return f.DedupFn()
	}
	return nil, nil
}

func TestParseMezmoSource(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
		want MezmoSource
	}{
		{"empty", "", false, MezmoSource{}},
		{"whitespace", "   ", false, MezmoSource{}},
		{"malformed", "not-json", false, MezmoSource{}},
		{"missing query", `{"window":"5m"}`, false, MezmoSource{}},
		{"valid", `{"query":"app:auth","window":"5m","metric":"count","field":"app"}`, true,
			MezmoSource{Query: "app:auth", Window: "5m", Metric: "count", Field: "app"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseMezmoSource(c.raw)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestApplyMezmoSource_NoOpClient(t *testing.T) {
	raw := `{"series":[]}`
	out := applyMezmoSource(context.Background(), raw, MezmoSource{Query: "x"}, "vm-sparklines", noopMezmoClient{}, newMezmoCache(8))
	if out != raw {
		t.Fatalf("noop client should preserve payload, got %s", out)
	}
}

func TestApplyMezmoSource_NilClient(t *testing.T) {
	raw := `{"series":[]}`
	out := applyMezmoSource(context.Background(), raw, MezmoSource{Query: "x"}, "vm-sparklines", nil, nil)
	if out != raw {
		t.Fatalf("nil client should preserve payload, got %s", out)
	}
}

func TestApplyMezmoSource_Sparklines(t *testing.T) {
	now := time.Now().Unix()
	fc := &fakeMezmoClient{
		HistogramFn: func() ([]HistogramPoint, error) {
			return []HistogramPoint{
				{Time: now - 60, Value: 10},
				{Time: now, Value: 20},
			}, nil
		},
	}
	src := MezmoSource{Query: "app:auth", Window: "5m", Metric: "count"}
	raw := `{"series":[{"service":"auth","metric":"count"}]}`
	out := applyMezmoSource(context.Background(), raw, src, "vm-sparklines", fc, newMezmoCache(8))
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid output JSON: %v", err)
	}
	series, _ := parsed["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	first := series[0].(map[string]any)
	pts, _ := first["points"].([]any)
	if len(pts) != 2 {
		t.Fatalf("expected 2 points, got %d", len(pts))
	}
}

func TestApplyMezmoSource_ServiceHealth(t *testing.T) {
	fc := &fakeMezmoClient{
		GroupByFn: func() ([]GroupBucket, error) {
			return []GroupBucket{{Key: "auth", Count: 10}, {Key: "billing", Count: 0}}, nil
		},
	}
	raw := `{"services":[]}`
	out := applyMezmoSource(context.Background(), raw, MezmoSource{Query: "level:error", Window: "5m", Field: "app"}, "service-health", fc, newMezmoCache(8))
	if !strings.Contains(out, `"service":"auth"`) {
		t.Fatalf("expected auth row in output: %s", out)
	}
	if !strings.Contains(out, `"state":"critical"`) {
		t.Fatalf("expected critical state for top bucket: %s", out)
	}
}

func TestApplyMezmoSource_ActivityLog(t *testing.T) {
	when := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	fc := &fakeMezmoClient{
		DedupFn: func() ([]LogEntry, error) {
			return []LogEntry{{When: when, Actor: "auth", Kind: "error", Text: "boom"}}, nil
		},
	}
	raw := `{"entries":[]}`
	out := applyMezmoSource(context.Background(), raw, MezmoSource{Query: "level:error", Window: "5m"}, "activity-log", fc, newMezmoCache(8))
	if !strings.Contains(out, `"text":"boom"`) {
		t.Fatalf("missing text in output: %s", out)
	}
	if !strings.Contains(out, `"actor":"auth"`) {
		t.Fatalf("missing actor in output: %s", out)
	}
}

func TestApplyMezmoSource_ErrorFallsBack(t *testing.T) {
	fc := &fakeMezmoClient{
		HistogramFn: func() ([]HistogramPoint, error) {
			return nil, errors.New("boom")
		},
	}
	raw := `{"series":[{"service":"auth","metric":"count","points":[{"when":"2026-05-07T12:00:00Z","value":1}]}]}`
	out := applyMezmoSource(context.Background(), raw, MezmoSource{Query: "x", Window: "5m"}, "vm-sparklines", fc, newMezmoCache(8))
	if out != raw {
		t.Fatalf("error path should preserve baked payload, got %s", out)
	}
}

func TestApplyMezmoSource_UnknownSlug(t *testing.T) {
	fc := &fakeMezmoClient{
		HistogramFn: func() ([]HistogramPoint, error) { return []HistogramPoint{{Time: 1, Value: 1}}, nil },
	}
	raw := `{"foo":"bar"}`
	out := applyMezmoSource(context.Background(), raw, MezmoSource{Query: "x", Window: "5m"}, "not-a-real-widget", fc, newMezmoCache(8))
	if out != raw {
		t.Fatalf("unknown slug should be a no-op, got %s", out)
	}
	if fc.HistCalls != 0 {
		t.Fatalf("should not call client for unknown slug")
	}
}

func TestMezmoCache_HitMiss(t *testing.T) {
	cache := newMezmoCache(4)
	calls := 0
	fc := &fakeMezmoClient{
		HistogramFn: func() ([]HistogramPoint, error) {
			calls++
			return []HistogramPoint{{Time: 1, Value: float64(calls)}}, nil
		},
	}
	src := MezmoSource{Query: "q", Window: "1h"}
	raw := `{"series":[{"service":"s","metric":"count"}]}`
	for range 3 {
		_ = applyMezmoSource(context.Background(), raw, src, "vm-sparklines", fc, cache)
	}
	if calls != 1 {
		t.Fatalf("expected 1 client call (cache hit), got %d", calls)
	}
}

func TestMezmoCache_Eviction(t *testing.T) {
	c := newMezmoCache(2)
	c.set("a", 1, time.Minute)
	c.set("b", 2, time.Minute)
	c.set("c", 3, time.Minute)
	if _, ok := c.get("a"); ok {
		t.Fatalf("expected a to be evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Fatalf("expected b to be retained")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatalf("expected c to be retained")
	}
}

func TestMezmoCache_Expiry(t *testing.T) {
	c := newMezmoCache(4)
	c.set("k", 42, 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.get("k"); ok {
		t.Fatalf("expected expired entry to be evicted")
	}
}

func TestWindowTTL(t *testing.T) {
	cases := []struct {
		w    string
		want time.Duration
	}{
		{"5m", 60 * time.Second}, // capped
		{"1m", 15 * time.Second}, // quarter-window
		{"1h", 60 * time.Second},
		{"1d", 5 * time.Minute},
		{"", 30 * time.Second},
		{"garbage", 30 * time.Second},
	}
	for _, c := range cases {
		got := windowTTL(c.w)
		if got != c.want {
			t.Errorf("windowTTL(%q)=%v want %v", c.w, got, c.want)
		}
	}
}

func TestMezmoLiveIntervalSecs(t *testing.T) {
	if mezmoLiveIntervalSecs(mezmoLiveActive) != 30 {
		t.Fatal("active default should be 30s")
	}
	if mezmoLiveIntervalSecs(mezmoLiveArchived) != 0 {
		t.Fatal("archived default should be 0 (no polling)")
	}
}

func TestRefreshWidgetAction_Shape(t *testing.T) {
	a := RefreshWidgetAction("service-health", "gd-abc", "#widget-body-gd-abc")
	if a.Kind != ActionHtmxGet {
		t.Fatalf("expected htmx-get kind, got %s", a.Kind)
	}
	if a.Target != "/widgets/service-health/gd-abc" {
		t.Fatalf("unexpected target: %s", a.Target)
	}
	js, err := a.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !strings.Contains(js, `"kind":"htmx-get"`) {
		t.Fatalf("kind not in JSON: %s", js)
	}
}

func TestLiveIntervalForCard(t *testing.T) {
	cases := []struct {
		name     string
		slug     string
		hasMezmo bool
		want     int
	}{
		{"mezmo overrides allowlist", "service-health", true, 30},
		{"allowlisted slug only", "service-health", false, 5},
		{"unlisted slug, no mezmo", "fix-available-pending-branch", false, 0},
		{"mezmo on unlisted slug", "fix-available-pending-branch", true, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := liveIntervalForCard(c.slug, c.hasMezmo, mezmoLiveActive)
			if got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}
}

func TestLiveAttrsFor_EmptyInputs(t *testing.T) {
	if got := liveAttrsFor("", "gd-x", "/", 5); got != "" {
		t.Fatalf("empty slug should return empty attrs, got %q", got)
	}
	if got := liveAttrsFor("s", "", "/", 5); got != "" {
		t.Fatalf("empty bead should return empty attrs, got %q", got)
	}
	if got := liveAttrsFor("s", "gd-x", "/", 0); got != "" {
		t.Fatalf("0 interval should return empty attrs, got %q", got)
	}
}

func TestLiveAttrsFor_Renders(t *testing.T) {
	attrs := liveAttrsFor("vm-sparklines", "gd-x", "/base", 30)
	if !strings.Contains(attrs, `hx-get="/base/widgets/vm-sparklines/gd-x"`) {
		t.Fatalf("missing hx-get: %s", attrs)
	}
	if !strings.Contains(attrs, `hx-trigger="every 30s"`) {
		t.Fatalf("missing hx-trigger: %s", attrs)
	}
}
