package main

// Tests for the C6 /costs trust strip (ka-0000008212): score extraction from
// the vmsingle vector, badge-level rules (absent → unverified, never green),
// the ratio-tracked P7 carve-out, and the strip's render on /costs.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func trustVectorBody(samples map[string]string) string {
	type sample struct {
		Metric map[string]string `json:"metric"`
		Value  [2]any            `json:"value"`
	}
	res := make([]sample, 0, len(samples))
	for pair, val := range samples {
		res = append(res, sample{
			Metric: map[string]string{"__name__": "gardenio_xcheck_agreement", "pair": pair, "env": "prod"},
			Value:  [2]any{1700000000, val},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": res},
	})
	return string(body)
}

func TestCostsTrustScoresFromVector(t *testing.T) {
	var resp vmVectorResponse
	body := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"pair":"bill-conservation","env":"prod"},"value":[1700000000,"0.98"]},
		{"metric":{"pair":"bill-conservation","env":"e2e"},"value":[1700000000,"0.40"]},
		{"metric":{"pair":"token-parity","env":"prod"},"value":[1700000000,"1"]},
		{"metric":{"env":"prod"},"value":[1700000000,"0.5"]},
		{"metric":{"pair":"tag-conservation","env":"prod"},"value":[1700000000,"NaN"]},
		{"metric":{"pair":"rollup-vs-tree","env":"prod"},"value":[1700000000,"1.73"]}
	]}}`
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	scores := costsTrustScoresFromVector(&resp)

	// Multi-env duplicate: the LOWEST score wins so a healthy estate cannot
	// mask a diverging one — and the worst env is named so the badge can point
	// at the estate at fault.
	if got := scores["bill-conservation"]; got.Score != 0.40 {
		t.Errorf("bill-conservation = %v, want 0.40 (lowest across envs)", got.Score)
	}
	if got := scores["bill-conservation"]; got.WorstEnv != "e2e" || got.Envs != 2 {
		t.Errorf("bill-conservation worst env/count = %q/%d, want e2e/2", got.WorstEnv, got.Envs)
	}
	if got := scores["token-parity"]; got.Score != 1 {
		t.Errorf("token-parity = %v, want 1", got.Score)
	}
	if got := scores["token-parity"]; got.Envs != 1 {
		t.Errorf("token-parity env count = %d, want 1 (single env)", got.Envs)
	}
	// No pair label / NaN sample → dropped.
	if _, ok := scores[""]; ok {
		t.Error("sample without a pair label must be dropped")
	}
	if _, ok := scores["tag-conservation"]; ok {
		t.Error("NaN sample must be dropped, not scored")
	}
	// Out-of-range sample clamps to [0,1].
	if got := scores["rollup-vs-tree"]; got.Score != 1 {
		t.Errorf("rollup-vs-tree = %v, want clamp to 1", got.Score)
	}

	if got := costsTrustScoresFromVector(nil); len(got) != 0 {
		t.Errorf("nil vector should yield no scores, got %v", got)
	}
}

func score(s float64) costsTrustScore { return costsTrustScore{Score: s, Envs: 1} }

func TestBuildCostsTrust_LevelRules(t *testing.T) {
	scores := map[string]costsTrustScore{
		costsTrustPairBillConservation: score(0.98), // ≥ its 0.85 floor → corroborated
		costsTrustPairTagConservation:  score(0.80), // < default 0.95 floor → drifting
		// agent-spend-parity (P10) intentionally absent → unverified.
		// P3 settled-vs-bill retired (ka-0000000543) and P7 estimate-vs-settled
		// retired (ka-0000007296) — neither is a registered pair any longer.
	}
	v := buildCostsTrust(scores, true, "https://grafana.example")

	if !v.Queried || !v.AnyPresent {
		t.Fatalf("Queried/AnyPresent = %v/%v, want true/true", v.Queried, v.AnyPresent)
	}
	if v.DashboardURL != "https://grafana.example/d/"+costsTrustDashboardUID {
		t.Errorf("DashboardURL = %q", v.DashboardURL)
	}

	cases := []struct {
		pair, level, word string
		hasScore          bool
	}{
		{costsTrustPairBillConservation, costsTrustLevelOK, "corroborated", true},
		{costsTrustPairTagConservation, costsTrustLevelDrift, "drifting", true},
		{costsTrustPairAgentSpendParity, costsTrustLevelUnverified, "unverified", false},
		{costsTrustPairTokenParity, costsTrustLevelUnverified, "unverified", false},
	}
	for _, tc := range cases {
		b := v.Badge(tc.pair)
		if b == nil {
			t.Errorf("Badge(%q) = nil, want a badge", tc.pair)
			continue
		}
		if b.Level != tc.level || b.LevelWord != tc.word || b.HasScore != tc.hasScore {
			t.Errorf("%s: level/word/hasScore = %s/%s/%v, want %s/%s/%v",
				tc.pair, b.Level, b.LevelWord, b.HasScore, tc.level, tc.word, tc.hasScore)
		}
	}

	// Every registered pair renders a badge even with no score.
	if len(v.Badges) != len(costsTrustPairSpecs()) {
		t.Errorf("badges = %d, want %d (one per registered pair)", len(v.Badges), len(costsTrustPairSpecs()))
	}
	// Unknown pair lookups are nil (template omits them), not a panic.
	if v.Badge("no-such-pair") != nil {
		t.Error("unknown pair should have no badge")
	}
	var nilView *costsTrustView
	if nilView.Badge("bill-conservation") != nil {
		t.Error("nil view Badge must be nil-safe")
	}
}

// TestBuildCostsTrust_PerPairFloors exercises the ka-0000007295 acceptance
// criteria: a pair is judged against its OWN floor (from values.yaml
// pairThresholds, mirrored in costsTrustPairFloors), not a stale 0.95 constant.
func TestBuildCostsTrust_PerPairFloors(t *testing.T) {
	cases := []struct {
		name      string
		pair      string
		score     float64
		wantLevel string
		wantWord  string
		titleHas  string // substring the tooltip must contain
	}{
		// AC: bill-conservation at 0.91 (measured steady state) renders corroborated (above 0.85).
		// (P3 settled-vs-bill was retired in ka-0000000543 — bill-conservation is now
		// the only tuned-floor-below-default pair, and it exercises the same paths.)
		{"bill-conservation 0.91 above 0.85 floor", costsTrustPairBillConservation, 0.91, costsTrustLevelOK, "corroborated", "structurally low but within this pair's expected band"},
		// AC: a pair genuinely below its OWN floor still renders drifting — the
		// tripwire is not muted.
		{"bill-conservation 0.80 below 0.85 floor", costsTrustPairBillConservation, 0.80, costsTrustLevelDrift, "drifting", "below this pair's 0.85 floor"},
		// A pair with NO override still uses the 0.95 default both ways.
		{"tag-conservation 0.90 below default 0.95", costsTrustPairTagConservation, 0.90, costsTrustLevelDrift, "drifting", "below this pair's 0.95 floor"},
		{"tag-conservation 0.96 above default 0.95", costsTrustPairTagConservation, 0.96, costsTrustLevelOK, "corroborated", "1.00 = two independent derivations agree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := buildCostsTrust(map[string]costsTrustScore{tc.pair: score(tc.score)}, true, "")
			b := v.Badge(tc.pair)
			if b == nil {
				t.Fatalf("Badge(%q) = nil", tc.pair)
			}
			if b.Level != tc.wantLevel || b.LevelWord != tc.wantWord {
				t.Errorf("level/word = %s/%s, want %s/%s", b.Level, b.LevelWord, tc.wantLevel, tc.wantWord)
			}
			if !strings.Contains(b.Title, tc.titleHas) {
				t.Errorf("title %q missing %q", b.Title, tc.titleHas)
			}
		})
	}
}

// TestBuildCostsTrust_EnvNoLongerMasks proves the env-masking fix: a divergent
// env drags the badge red (lowest wins), but the badge now NAMES the worst env
// instead of silently masking which estate is at fault.
func TestBuildCostsTrust_EnvNoLongerMasks(t *testing.T) {
	// prod healthy (1.0), e2e diverged (0.30) on a default-floor pair.
	scores := map[string]costsTrustScore{
		costsTrustPairTagConservation: {Score: 0.30, WorstEnv: "e2e", Envs: 2},
	}
	v := buildCostsTrust(scores, true, "")
	b := v.Badge(costsTrustPairTagConservation)
	if b == nil {
		t.Fatal("badge nil")
	}
	if b.Level != costsTrustLevelDrift {
		t.Errorf("level = %s, want drift (lowest env wins)", b.Level)
	}
	if !strings.Contains(b.Title, "worst: e2e") {
		t.Errorf("title %q must name the worst env", b.Title)
	}
	// Single-env pair carries no env note.
	single := buildCostsTrust(map[string]costsTrustScore{
		costsTrustPairTagConservation: {Score: 0.30, WorstEnv: "prod", Envs: 1},
	}, true, "")
	if strings.Contains(single.Badge(costsTrustPairTagConservation).Title, "worst:") {
		t.Error("single-env badge must not carry a worst-env note")
	}
}

func TestBuildCostsTrust_NoUpstreamAllUnverified(t *testing.T) {
	v := buildCostsTrust(nil, false, "")
	if v.Queried || v.AnyPresent {
		t.Fatalf("Queried/AnyPresent = %v/%v, want false/false", v.Queried, v.AnyPresent)
	}
	if v.DashboardURL != "" {
		t.Errorf("DashboardURL = %q, want empty without Grafana", v.DashboardURL)
	}
	for _, b := range v.Badges {
		if b.Level != costsTrustLevelUnverified {
			t.Errorf("%s: level = %s, want unverified — no data must never read green", b.Pair, b.Level)
		}
	}
}

func TestBuildCostsTrust_UnregisteredPairStillRenders(t *testing.T) {
	v := buildCostsTrust(map[string]costsTrustScore{"mystery-pair": score(0.99)}, true, "")
	b := v.Badge("mystery-pair")
	if b == nil {
		t.Fatal("published-but-unregistered pair must still render a badge")
	}
	if b.Level != costsTrustLevelOK || b.Score != "0.99" {
		t.Errorf("mystery-pair badge = %s/%s, want ok/0.99", b.Level, b.Score)
	}
	// Appended after the registered set.
	if got := v.Badges[len(v.Badges)-1].Pair; got != "mystery-pair" {
		t.Errorf("unregistered pair position = %q, want last", got)
	}
}

// mkCostsTrustServer wires a viewer whose vmsingle upstream serves the given
// agreement vector for the trust query (and an empty vector for any other
// query /costs makes, e.g. the rate headline).
func mkCostsTrustServer(t *testing.T, agreementBody string) *http.ServeMux {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Query().Get("query"), "gardenio_xcheck_agreement") {
			_, _ = w.Write([]byte(agreementBody))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(upstream.Close)

	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux
}

func TestHandleCosts_TrustStripRenders(t *testing.T) {
	mux := mkCostsTrustServer(t, trustVectorBody(map[string]string{
		"bill-conservation": "0.99",
		"tag-conservation":  "0.72",
	}))

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="costs-trust-strip"`,
		"x-check corroboration",
		// Present pairs badge with their scores + level words.
		"bill conservation: 0.99 corroborated",
		"tag conservation: 0.72 drifting",
		// Absent pairs (incl. the pre-registered P10) read unverified, not green.
		"agent attribution: unverified",
		"token parity: unverified",
		// Rendered chips (not just the CSS defs) at each level.
		`class="chip costs-trust-chip costs-trust-ok"`,
		`class="chip costs-trust-chip costs-trust-drift"`,
		`class="chip costs-trust-chip costs-trust-unverified"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/costs trust strip missing %q", want)
		}
	}
	// Inline badge next to the summary card qualifies the headline numbers.
	if !strings.Contains(body, `class="costs-trust-inline"`) {
		t.Error("/costs missing the inline summary trust badge")
	}
	// No-data note must NOT show when scores are present.
	if strings.Contains(body, "single-source and unverified") {
		t.Error("no-data note rendered despite present scores")
	}
}

func TestHandleCosts_TrustStripNoUpstream(t *testing.T) {
	// mkServer wires no metrics upstream — the strip must render its honest
	// all-unverified state with the unavailable note, and never a green chip.
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="costs-trust-strip"`,
		"Trust scores unavailable",
		"single-source and unverified",
		"docs/cost-xcheck-audit-sweep.md",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/costs degraded trust strip missing %q", want)
		}
	}
	if strings.Contains(body, `class="chip costs-trust-chip costs-trust-ok"`) {
		t.Error("no-upstream strip rendered a green badge — absence must not read healthy")
	}
}

func TestHandleCosts_TrustStripEmptyVector(t *testing.T) {
	// Upstream answers but no pair is published (sweep not running here yet):
	// distinct note from the no-upstream case, still no green.
	mux := mkCostsTrustServer(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		"No x-check scores published yet",
		"GARDENIO_XCHECK_SWEEP_INTERVAL",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/costs empty-vector trust strip missing %q", want)
		}
	}
	if strings.Contains(body, `class="chip costs-trust-chip costs-trust-ok"`) {
		t.Error("empty-vector strip rendered a green badge")
	}
}

func TestCostsTrustPanelsCarryTrustPairs(t *testing.T) {
	// The chart cards must associate the pair that actually qualifies their
	// numbers (badges rendered next to the numbers they qualify — the C6
	// contract). Guard the mapping so a panel reshuffle can't silently drop it.
	wantByTitle := map[string]string{
		"Fleet spend rate":           costsTrustPairBillConservation,
		"Per-agent cost over time":   costsTrustPairAgentSpendParity,
		"Spend by account over time": costsTrustPairBillConservation,
		// "Spend by model over time" panel removed (ka-0000006501).
		// "Settled cost — rollup by role" panel removed (ka-0000002699); its
		// settled-vs-bill trust pair was retired (ka-0000000543).
	}
	for _, p := range costsGrafanaPanels() {
		if want, ok := wantByTitle[p.Title]; ok && p.TrustPair != want {
			t.Errorf("panel %q TrustPair = %q, want %q", p.Title, p.TrustPair, want)
		}
	}
}
