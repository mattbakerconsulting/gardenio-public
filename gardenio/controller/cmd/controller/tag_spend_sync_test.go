package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// tagsHandler serves GET /spend/tags returning the supplied entries and GET
// /global/spend returning the supplied bill (defaulting to the sum of the
// entries when unset, so pre-existing tests that only exercise the tag gauge
// stay bill-consistent). A flip to failing (or failGlobal) lets a test
// simulate a down proxy / a half-broken one.
type tagsHandler struct {
	entries    []tagSpendEntry
	bill       float64 // /global/spend total; 0 → derived from entries
	fail       bool    // fail every endpoint
	failGlobal bool    // fail only /global/spend (snapshot must still be all-or-nothing)
}

func (h *tagsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	switch r.URL.Path {
	case "/spend/tags":
		_ = json.NewEncoder(w).Encode(h.entries)
	case "/global/spend":
		if h.failGlobal {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		bill := h.bill
		if bill == 0 {
			// LiteLLM books the full request cost under EVERY tag; the test
			// fixtures use one tag per "request", so the untagged-free default
			// bill is the agent:* sum (or, lacking agent tags, the max dim sum —
			// close enough for fixtures that don't assert the split).
			for _, e := range h.entries {
				bill += e.TotalSpend
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]float64{"spend": bill})
	default:
		http.NotFound(w, r)
	}
}

// accountBead builds an account bead JSON object carrying a base_url field.
func accountBead(id, title, baseURL string) map[string]any {
	return accountBeadWithProxy(id, title, baseURL, "")
}

// accountBeadWithProxy builds an account bead carrying base_url and (optionally)
// proxy_url, for exercising the claudemore-fronting case.
func accountBeadWithProxy(id, title, baseURL, proxyURL string) map[string]any {
	fields := map[string]any{}
	if baseURL != "" {
		fields["base_url"] = baseURL
	}
	if proxyURL != "" {
		fields["proxy_url"] = proxyURL
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

func newTestTagSync(t *testing.T, daemon *daemonStub, globalURL string) (*tagSpendSync, *gardeniometrics.SpendMetrics) {
	t.Helper()
	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	sm := gardeniometrics.NewSpendMetrics()
	ts := newTagSpendSync(client, globalURL, "test-master-key", "prod", sm, slog.Default())
	return ts, sm
}

func TestParseDimTag(t *testing.T) {
	cases := []struct {
		tag       string
		wantDim   string
		wantValue string
		wantOK    bool
	}{
		{"role:gardener", "role", "gardener", true},
		{"project:monorepo", "project", "monorepo", true},
		{"model:claude-opus-4-8", "model", "claude-opus-4-8", true},
		{"role:crew-thread", "role", "crew-thread", true}, // sanitized multi-role
		{"agent:gardenio-foo-1", "", "", false},           // agent excluded
		{"User-Agent: claude-cli", "", "", false},         // litellm auto-tag excluded
		{"untagged", "", "", false},                       // no colon
		{"role:", "", "", false},                          // empty value
		{":gardener", "", "", false},                      // empty dim
		{"unknownDim:x", "", "", false},                   // not an attribution dim
	}
	for _, c := range cases {
		dim, value, ok := parseDimTag(c.tag)
		if dim != c.wantDim || value != c.wantValue || ok != c.wantOK {
			t.Errorf("parseDimTag(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.tag, dim, value, ok, c.wantDim, c.wantValue, c.wantOK)
		}
	}
}

func TestTagSpendSync_AggregatesAcrossProxies(t *testing.T) {
	// Two proxies with SEPARATE spend DBs. role:gardener appears on BOTH and must
	// SUM (10 + 7 = 17); project:monorepo only on proxy A; model only on proxy B.
	// agent: and User-Agent: rows are ignored.
	proxyA := httptest.NewServer(&tagsHandler{entries: []tagSpendEntry{
		{IndividualRequestTag: "role:gardener", TotalSpend: 10.0},
		{IndividualRequestTag: "project:monorepo", TotalSpend: 4.0},
		{IndividualRequestTag: "agent:foo", TotalSpend: 99.0},
		{IndividualRequestTag: "User-Agent: claude-cli", TotalSpend: 99.0},
	}})
	t.Cleanup(proxyA.Close)
	proxyB := httptest.NewServer(&tagsHandler{entries: []tagSpendEntry{
		{IndividualRequestTag: "role:gardener", TotalSpend: 7.0},
		{IndividualRequestTag: "model:claude-opus-4-8", TotalSpend: 5.0},
	}})
	t.Cleanup(proxyB.Close)

	// proxyA is the global URL; proxyB is reached via an account bead's base_url.
	daemon := &daemonStub{beads: []map[string]any{
		accountBead("bd-a", "Acct A", proxyA.URL), // duplicate of global → deduped
		accountBead("bd-b", "Acct B", proxyB.URL),
	}}
	ts, sm := newTestTagSync(t, daemon, proxyA.URL)

	ts.syncAll(context.Background())

	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "gardener", "prod")); got != 17.0 {
		t.Errorf("role:gardener = %v, want 17.0 (summed across proxies)", got)
	}
	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("project", "monorepo", "prod")); got != 4.0 {
		t.Errorf("project:monorepo = %v, want 4.0", got)
	}
	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("model", "claude-opus-4-8", "prod")); got != 5.0 {
		t.Errorf("model:claude-opus-4-8 = %v, want 5.0", got)
	}
	// agent:/User-Agent: never become series.
	if got := testutil.CollectAndCount(sm.TagCostGauge()); got != 3 {
		t.Errorf("expected 3 tag-cost series (role/project/model), got %d", got)
	}
}

func TestTagSpendSync_RetainsLastGoodOnProxyFailure(t *testing.T) {
	hA := &tagsHandler{entries: []tagSpendEntry{{IndividualRequestTag: "role:gardener", TotalSpend: 10.0}}}
	proxyA := httptest.NewServer(hA)
	t.Cleanup(proxyA.Close)
	hB := &tagsHandler{entries: []tagSpendEntry{{IndividualRequestTag: "role:thread", TotalSpend: 3.0}}}
	proxyB := httptest.NewServer(hB)
	t.Cleanup(proxyB.Close)

	daemon := &daemonStub{beads: []map[string]any{
		accountBead("bd-b", "Acct B", proxyB.URL),
	}}
	ts, sm := newTestTagSync(t, daemon, proxyA.URL)

	// First poll: both proxies healthy.
	ts.syncAll(context.Background())
	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "thread", "prod")); got != 3.0 {
		t.Fatalf("setup: role:thread = %v, want 3.0", got)
	}

	// Proxy B goes down; its last-good contribution (thread=3) must be retained,
	// not zeroed — while proxy A's grown total (gardener=12) updates.
	hB.fail = true
	hA.entries = []tagSpendEntry{{IndividualRequestTag: "role:gardener", TotalSpend: 12.0}}
	ts.syncAll(context.Background())

	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "gardener", "prod")); got != 12.0 {
		t.Errorf("after B-fail: role:gardener = %v, want 12.0 (A updated)", got)
	}
	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "thread", "prod")); got != 3.0 {
		t.Errorf("after B-fail: role:thread = %v, want 3.0 (B last-good retained)", got)
	}
}

func TestTagSpendSync_DistinctProxyURLs_Dedup(t *testing.T) {
	daemon := &daemonStub{beads: []map[string]any{
		accountBead("bd-a", "A", "http://litellm-x:4000"),
		accountBead("bd-b", "B", "http://litellm-x:4000/"), // trailing slash → same
		accountBead("bd-c", "C", "http://litellm-y:4000"),
		accountBead("bd-d", "D", ""), // empty → falls back to global, not a new URL
	}}
	ts, _ := newTestTagSync(t, daemon, "http://litellm-x:4000")

	urls := ts.distinctProxyURLs(context.Background())
	want := map[string]bool{"http://litellm-x:4000": true, "http://litellm-y:4000": true}
	if len(urls) != len(want) {
		t.Fatalf("distinctProxyURLs = %v, want %d distinct", urls, len(want))
	}
	for _, u := range urls {
		if !want[u] {
			t.Errorf("unexpected proxy URL %q", u)
		}
	}
}

// TestTagSpendSync_IgnoresProxyURL proves proxy_url (claudemore) is NOT polled —
// it's a transparent fronting proxy that forwards to the same litellm and never
// meters spend, so polling it would double-count the bill. Only base_url (and the
// global URL) are enumerated.
func TestTagSpendSync_IgnoresProxyURL(t *testing.T) {
	daemon := &daemonStub{beads: []map[string]any{
		accountBeadWithProxy("bd-a", "A", "http://litellm:4000", "http://claudemore:9000"),
	}}
	ts, _ := newTestTagSync(t, daemon, "http://litellm:4000")

	urls := ts.distinctProxyURLs(context.Background())
	for _, u := range urls {
		if u == "http://claudemore:9000" {
			t.Errorf("proxy_url (claudemore) must NOT be polled; got %v", urls)
		}
	}
	if len(urls) != 1 || urls[0] != "http://litellm:4000" {
		t.Errorf("expected only the litellm base_url, got %v", urls)
	}
}

// TestTagSpendSync_AttributionSplit proves the bill split (ka-0000003988):
// attributed = Σ agent:* tag totals across proxies; unattributed = Σ per-proxy
// (/global/spend bill − agent-tagged). Their sum reconstructs the full bill.
func TestTagSpendSync_AttributionSplit(t *testing.T) {
	// Proxy A: $100 bill, $90 agent-tagged → $10 residual.
	proxyA := httptest.NewServer(&tagsHandler{
		entries: []tagSpendEntry{
			{IndividualRequestTag: "agent:foo", TotalSpend: 60.0},
			{IndividualRequestTag: "agent:bar", TotalSpend: 30.0},
			{IndividualRequestTag: "role:gardener", TotalSpend: 90.0},
			{IndividualRequestTag: "User-Agent: claude-cli", TotalSpend: 90.0},
		},
		bill: 100.0,
	})
	t.Cleanup(proxyA.Close)
	// Proxy B: $50 bill, $45 agent-tagged → $5 residual.
	proxyB := httptest.NewServer(&tagsHandler{
		entries: []tagSpendEntry{
			{IndividualRequestTag: "agent:baz", TotalSpend: 45.0},
		},
		bill: 50.0,
	})
	t.Cleanup(proxyB.Close)

	daemon := &daemonStub{beads: []map[string]any{
		accountBead("bd-b", "Acct B", proxyB.URL),
	}}
	ts, sm := newTestTagSync(t, daemon, proxyA.URL)

	ts.syncAll(context.Background())

	if got := testutil.ToFloat64(sm.AttributedCostGauge().WithLabelValues("prod")); got != 135.0 {
		t.Errorf("attributed = %v, want 135.0 (90 + 45 across proxies)", got)
	}
	if got := testutil.ToFloat64(sm.UnattributedCostGauge().WithLabelValues("prod")); got != 15.0 {
		t.Errorf("unattributed = %v, want 15.0 (10 + 5 per-proxy residuals)", got)
	}
	// agent: tags must feed the split, not become tag-cost series.
	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "gardener", "prod")); got != 90.0 {
		t.Errorf("role:gardener = %v, want 90.0", got)
	}
	if got := testutil.CollectAndCount(sm.TagCostGauge()); got != 1 {
		t.Errorf("expected 1 tag-cost series, got %d", got)
	}
}

// TestTagSpendSync_AttributionSplit_ClampsNegativeResidual proves a proxy whose
// tag indexing momentarily runs ahead of its /global/spend bill contributes 0
// residual — not a negative that would mask another proxy's genuine residual.
func TestTagSpendSync_AttributionSplit_ClampsNegativeResidual(t *testing.T) {
	proxyA := httptest.NewServer(&tagsHandler{
		entries: []tagSpendEntry{{IndividualRequestTag: "agent:foo", TotalSpend: 105.0}},
		bill:    100.0, // tagged > bill → residual clamps to 0
	})
	t.Cleanup(proxyA.Close)

	ts, sm := newTestTagSync(t, &daemonStub{}, proxyA.URL)
	ts.syncAll(context.Background())

	if got := testutil.ToFloat64(sm.AttributedCostGauge().WithLabelValues("prod")); got != 105.0 {
		t.Errorf("attributed = %v, want 105.0", got)
	}
	if got := testutil.ToFloat64(sm.UnattributedCostGauge().WithLabelValues("prod")); got != 0.0 {
		t.Errorf("unattributed = %v, want 0.0 (negative residual clamped)", got)
	}
}

// TestTagSpendSync_SnapshotAtomicOnGlobalSpendFailure proves a tick where
// /spend/tags succeeds but /global/spend fails carries the WHOLE previous
// snapshot forward — a half-updated snapshot would publish an attribution split
// blending two instants.
func TestTagSpendSync_SnapshotAtomicOnGlobalSpendFailure(t *testing.T) {
	h := &tagsHandler{
		entries: []tagSpendEntry{
			{IndividualRequestTag: "agent:foo", TotalSpend: 40.0},
			{IndividualRequestTag: "role:gardener", TotalSpend: 40.0},
		},
		bill: 50.0,
	}
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)

	ts, sm := newTestTagSync(t, &daemonStub{}, proxy.URL)
	ts.syncAll(context.Background())
	if got := testutil.ToFloat64(sm.UnattributedCostGauge().WithLabelValues("prod")); got != 10.0 {
		t.Fatalf("setup: unattributed = %v, want 10.0", got)
	}

	// Tags grow but the bill read breaks: everything must hold at the last
	// consistent snapshot (attributed 40, unattributed 10, gardener 40).
	h.entries = []tagSpendEntry{
		{IndividualRequestTag: "agent:foo", TotalSpend: 70.0},
		{IndividualRequestTag: "role:gardener", TotalSpend: 70.0},
	}
	h.failGlobal = true
	ts.syncAll(context.Background())

	if got := testutil.ToFloat64(sm.AttributedCostGauge().WithLabelValues("prod")); got != 40.0 {
		t.Errorf("after global-spend failure: attributed = %v, want 40.0 (snapshot carried forward whole)", got)
	}
	if got := testutil.ToFloat64(sm.UnattributedCostGauge().WithLabelValues("prod")); got != 10.0 {
		t.Errorf("after global-spend failure: unattributed = %v, want 10.0 (snapshot carried forward whole)", got)
	}
	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "gardener", "prod")); got != 40.0 {
		t.Errorf("after global-spend failure: role:gardener = %v, want 40.0 (dims from the SAME snapshot)", got)
	}
}

// TestTagSpendSync_PrunesDepartedProxy proves a proxy that leaves the account list
// stops contributing its stale spend: its last-good snapshot is dropped, not
// summed forever.
func TestTagSpendSync_PrunesDepartedProxy(t *testing.T) {
	hA := &tagsHandler{entries: []tagSpendEntry{{IndividualRequestTag: "role:gardener", TotalSpend: 10.0}}}
	proxyA := httptest.NewServer(hA)
	t.Cleanup(proxyA.Close)
	hB := &tagsHandler{entries: []tagSpendEntry{{IndividualRequestTag: "role:thread", TotalSpend: 3.0}}}
	proxyB := httptest.NewServer(hB)
	t.Cleanup(proxyB.Close)

	// Both accounts present initially (proxyA is global, proxyB via account).
	daemon := &daemonStub{beads: []map[string]any{
		accountBead("bd-b", "Acct B", proxyB.URL),
	}}
	ts, sm := newTestTagSync(t, daemon, proxyA.URL)

	ts.syncAll(context.Background())
	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "thread", "prod")); got != 3.0 {
		t.Fatalf("setup: role:thread = %v, want 3.0", got)
	}

	// Account B leaves the list (closed / base_url removed). Its proxy drops out of
	// distinctProxyURLs; its role:thread spend must NOT linger in the gauge.
	daemon.mu.Lock()
	daemon.beads = nil
	daemon.mu.Unlock()
	ts.syncAll(context.Background())

	if got := testutil.ToFloat64(sm.TagCostGauge().WithLabelValues("role", "gardener", "prod")); got != 10.0 {
		t.Errorf("after B departs: role:gardener = %v, want 10.0 (A still polled)", got)
	}
	if got := testutil.CollectAndCount(sm.TagCostGauge()); got != 1 {
		t.Errorf("after B departs: expected only proxy A's 1 series, got %d (stale B not pruned)", got)
	}
}
