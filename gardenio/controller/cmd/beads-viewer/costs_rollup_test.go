package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --- pure-function unit tests -------------------------------------------------

func TestValidRollupDim(t *testing.T) {
	for _, d := range costsRollupDims {
		if got := validRollupDim(d); got != d {
			t.Errorf("validRollupDim(%q) = %q, want %q", d, got, d)
		}
	}
	for _, bad := range []string{"", "agentt", "drop table", "by", "operator"} {
		if got := validRollupDim(bad); got != costsRollupDimDefault {
			t.Errorf("validRollupDim(%q) = %q, want default %q", bad, got, costsRollupDimDefault)
		}
	}
}

func TestValidRollupRange(t *testing.T) {
	for _, r := range costsRollupRanges {
		if got := validRollupRange(r.Key); got != r.Key {
			t.Errorf("validRollupRange(%q) = %q, want %q", r.Key, got, r.Key)
		}
	}
	for _, bad := range []string{"", "5y", "1h]) or drop", "0"} {
		if got := validRollupRange(bad); got != costsRollupRangeDefault {
			t.Errorf("validRollupRange(%q) = %q, want default %q", bad, got, costsRollupRangeDefault)
		}
	}
}

func TestSanitizeFilterValue(t *testing.T) {
	cases := []struct {
		in       string
		wantVal  string
		wantOk   bool
		whatItIs string
	}{
		{"", "", false, "empty = All"},
		{"  ", "", false, "blank = All"},
		{"monorepo", "monorepo", true, "plain"},
		{"  gardener  ", "gardener", true, "trimmed"},
		{"claude-opus-4-8", "claude-opus-4-8", true, "model name"},
		{"thread-1782329116-766089", "thread-1782329116-766089", true, "agent name"},
		{"litellm-prod", "litellm-prod", true, "account"},
		// Injection-shaped values must be rejected (treated as All).
		{`a"} or up{`, "", false, "quote+brace injection rejected"},
		{`x[5m])`, "", false, "range-vector injection rejected"},
		{"a\nb", "", false, "newline rejected"},
		{"a*b", "", false, "regex metachar rejected"},
	}
	for _, c := range cases {
		gotVal, gotOk := sanitizeFilterValue(c.in)
		if gotVal != c.wantVal || gotOk != c.wantOk {
			t.Errorf("sanitizeFilterValue(%q) [%s] = (%q,%v), want (%q,%v)",
				c.in, c.whatItIs, gotVal, gotOk, c.wantVal, c.wantOk)
		}
	}
}

func TestBuildCostsRollupQuery_NoFilters(t *testing.T) {
	// by-agent with no filters rides the per-agent REAL-BILL spend-logs gauge
	// (ka-0000002699). It is a Reset+Set gauge, so the query is a RAW deduped INSTANT
	// sum — NOT increase(), which would trip counter-reset detection and read garbage
	// — and takes no range window (the gauge's ~48h sweep population IS the window).
	q := buildCostsRollupQuery("agent", map[string]string{}, "24h")
	for _, want := range []string{
		"topk(50,",
		"sum by(agent)(",
		// Raw deduped instant on the gauge, with only the E2E exclusion in the
		// selector (ka-0000005167) since no user filter is set.
		"max by (agent, project, role, model, account) (" + costsRollupMetric + "{" + costsRollupE2EAccountExclusion + "})",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
	// The whole point of the fix: it must NOT ride the dead at-close counter, and
	// must NOT use increase() (meaningless on a Reset+Set gauge) or a range window.
	if strings.Contains(q, "gardenio_agent_spend_usd_total") {
		t.Errorf("by-agent query must not ride the dead at-close counter: %q", q)
	}
	if strings.Contains(q, "increase(") {
		t.Errorf("by-agent query must not use increase() on the Reset+Set gauge: %q", q)
	}
	if strings.Contains(q, "[24h") {
		t.Errorf("by-agent gauge query must not carry a range window: %q", q)
	}
}

// TestBuildCostsRollupQuery_RoleUsesTagGauge is the ka-0000000478 fix: by-role
// (and by-project/by-model) with no cross-dimension filter must source the
// REAL-BILL per-dimension tag gauge with the headline's dedup + aligned-subquery
// shape, NOT the at-close counter that read low.
func TestBuildCostsRollupQuery_RoleUsesTagGauge(t *testing.T) {
	for _, dim := range []string{"role", "project", "model"} {
		q := buildCostsRollupQuery(dim, map[string]string{}, "24h")
		for _, want := range []string{
			"topk(50,",
			"sum by(value)(",
			`dim="` + dim + `"`,
			"increase((max by (dim, value, env) (" + costsRollupTagGaugeMetric + "{",
			"[24h:5m])",
		} {
			if !strings.Contains(q, want) {
				t.Errorf("by-%s query %q missing %q", dim, q, want)
			}
		}
		// Must NOT ride the at-close counter — the whole point of the fix.
		if strings.Contains(q, costsRollupMetric) {
			t.Errorf("by-%s query must not reference the at-close counter: %q", dim, q)
		}
	}
}

// TestBuildCostsRollupQuery_RoleSameDimFilterOnTagGauge: a same-dimension value
// filter (e.g. by-role + role=gardener) IS honorable on the tag gauge via its
// shared `value` label, and does not force the counter fallback.
func TestBuildCostsRollupQuery_RoleSameDimFilterOnTagGauge(t *testing.T) {
	q := buildCostsRollupQuery("role", map[string]string{"role": "gardener"}, "24h")
	if !strings.Contains(q, costsRollupTagGaugeMetric) {
		t.Errorf("same-dim filter should stay on the tag gauge: %q", q)
	}
	if !strings.Contains(q, `value="gardener"`) {
		t.Errorf("same-dim filter should scope via the value label: %q", q)
	}
}

// TestBuildCostsRollupQuery_RoleCrossFilterFallsBackToAgentGauge: a cross-dimension
// filter (by-role + project=monorepo) cannot be expressed on the single-dimension
// tag gauge, so the rollup falls back to the cross-filterable per-agent real-bill
// spend-logs gauge (which carries every label) — NOT the retired at-close counter.
func TestBuildCostsRollupQuery_RoleCrossFilterFallsBackToAgentGauge(t *testing.T) {
	q := buildCostsRollupQuery("role", map[string]string{"project": "monorepo"}, "24h")
	if !strings.Contains(q, costsRollupMetric) {
		t.Errorf("cross-dim filter should fall back to the per-agent gauge: %q", q)
	}
	if strings.Contains(q, "gardenio_agent_spend_usd_total") {
		t.Errorf("cross-dim filter must not ride the retired at-close counter: %q", q)
	}
	if strings.Contains(q, costsRollupTagGaugeMetric) {
		t.Errorf("cross-dim filter must not use the tag gauge: %q", q)
	}
	if !strings.Contains(q, `project="monorepo"`) || !strings.Contains(q, "sum by(role)(") {
		t.Errorf("per-agent-gauge fallback should keep the cross filter + group-by-dim: %q", q)
	}
	// Raw deduped instant on the gauge — no increase(), no range window.
	if strings.Contains(q, "increase(") || strings.Contains(q, "[24h") {
		t.Errorf("per-agent-gauge fallback must be a raw instant, not a windowed increase: %q", q)
	}
}

func TestBuildCostsRollupQuery_WithFilters(t *testing.T) {
	q := buildCostsRollupQuery("agent", map[string]string{
		"role":    "gardener",
		"project": "monorepo",
	}, "7d")
	// The operator's worked example: role=gardener + project=monorepo, by agent —
	// now on the per-agent real-bill spend-logs gauge, which carries every label so
	// the cross-dimension filter applies directly.
	for _, want := range []string{
		"sum by(agent)(",
		`role="gardener"`,
		`project="monorepo"`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
	// Filters are emitted in costsRollupFilterLabels order (project before role).
	if strings.Index(q, "project=") > strings.Index(q, "role=") {
		t.Errorf("filters out of canonical order: %q", q)
	}
	// Raw deduped instant on the gauge — no increase(), no range window.
	if strings.Contains(q, "increase(") || strings.Contains(q, "[7d") {
		t.Errorf("by-agent gauge query must be a raw instant, not a windowed increase: %q", q)
	}
}

// TestBuildCostsRollupQuery_AccountUsesRealBillGauge is the core ka-0000006213
// fix: the by-account roll-up must source the REAL-BILL account-cost gauge with
// the same dedup + aligned-subquery shape the /costs headline uses
// (ka-0000004779), NOT the at-close counter that read ~3.5x low. Mirrors
// costs_rate_test.go's query-shape assertion.
func TestBuildCostsRollupQuery_AccountUsesRealBillGauge(t *testing.T) {
	q := buildCostsRollupQuery("account", map[string]string{}, "24h")
	for _, want := range []string{
		"topk(50,",
		"sum by(account)(",
		// The headline's dedup + aligned-subquery shape. The selector braces always
		// carry the E2E-account exclusion (ka-0000005167), even with no filter set.
		"increase((max by (account, env) (" + costsRollupGaugeMetric + "{" + costsRollupE2EAccountExclusion + "}))[24h:5m])",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("by-account query %q missing %q", q, want)
		}
	}
	// It must NOT ride the at-close counter (the whole point of the fix), and
	// must not use a bare (un-deduped, un-subqueried) increase.
	if strings.Contains(q, costsRollupMetric) {
		t.Errorf("by-account query must not reference the at-close counter: %q", q)
	}
	if !strings.Contains(q, "max by (account, env)") {
		t.Errorf("by-account query must dedup per-pod gauge dups: %q", q)
	}
}

// TestBuildCostsRollupQuery_AccountAlwaysExcludesE2E pins the PromQL-side layer of
// the ka-0000005167 fix: the by-account gauge query ALWAYS carries the E2E
// test-account exclusion matcher — the gauge series outlive the bead, so the
// producer-side skip cannot clean the retention tail — whether or not an account
// filter is set. The per-agent real-bill gauge (ka-0000002699) carries the SAME
// exclusion, because it too has an {account} label and outlives the bead, so the
// by-account and by-agent totals stay reconciled. The tag-gauge dims (role/project/
// model with no cross filter) have no account label and must NOT carry it.
func TestBuildCostsRollupQuery_AccountAlwaysExcludesE2E(t *testing.T) {
	for _, filters := range []map[string]string{
		{},
		{"account": "gardenio3"},
		{"account": "gardenio3", "role": "gardener"},
	} {
		q := buildCostsRollupQuery("account", filters, "24h")
		if !strings.Contains(q, costsRollupE2EAccountExclusion) {
			t.Errorf("by-account query %q missing the E2E exclusion %q", q, costsRollupE2EAccountExclusion)
		}
	}
	// A by-agent rollup rides the per-agent real-bill gauge, which carries an
	// {account} label — so it MUST apply the same exclusion (ka-0000002699).
	if q := buildCostsRollupQuery("agent", map[string]string{}, "24h"); !strings.Contains(q, costsRollupE2EAccountExclusion) {
		t.Errorf("by-agent gauge query must carry the E2E exclusion: %q", q)
	}
	// A by-role rollup with NO cross filter rides the tag gauge (no account label),
	// so it must NOT carry the account exclusion.
	if q := buildCostsRollupQuery("role", map[string]string{}, "24h"); strings.Contains(q, "probe-acct") {
		t.Errorf("by-role tag-gauge query must not carry the account exclusion: %q", q)
	}
}

// TestCostsRollupE2EAccountExclusion_MatchesRealAccounts is the no-false-positive
// guard for the PromQL layer (ka-0000005167 acceptance criterion 4). PromQL regex
// matchers are FULLY ANCHORED, so the exclusion alternation must match every e2e
// harness account name and no real account — including one that legitimately
// begins with the letters "e2e" but no hyphen. Compiled with a Go ^…$ anchor to
// replicate PromQL anchoring semantics.
func TestCostsRollupE2EAccountExclusion_MatchesRealAccounts(t *testing.T) {
	// Extract the alternation from the `account!~"…"` matcher so the test exercises
	// the SHIPPED pattern rather than a copy that could drift.
	const prefix = `account!~"`
	pat := strings.TrimSuffix(strings.TrimPrefix(costsRollupE2EAccountExclusion, prefix), `"`)
	re := regexp.MustCompile("^(?:" + pat + ")$")

	// Every E2E harness naming convention must be EXCLUDED (i.e. the regex matches,
	// so the !~ drops the series).
	for _, name := range []string{
		"e2e-acct-primary-abc", "e2e-acct-backup-abc", "e2e-cred-a-tag", "e2e-rot-resume-a-tag",
		"e2e-chain-b-tag", "e2e-bulk-a-tag", "e2e-drain-b-tag", "e2e-cost-tracking-tag",
		"e2e-account-tag", "probe-acct-12345",
		// Case variants: the Go predicate lowercases the title before matching, but
		// PromQL matchers are case-SENSITIVE, so the pattern must carry (?i:…) or the
		// two layers disagree and an uppercase-titled account keeps its retention-tail
		// series in the rollup/dropdown despite being skipped by the producer.
		"E2E-acct-primary-abc", "PROBE-ACCT-12345", "E2e-Cred-A-Tag",
	} {
		if !re.MatchString(name) {
			t.Errorf("E2E account %q must be excluded by %q", name, pat)
		}
	}
	// Real accounts (live fleet names + the e2e-ish edge cases) must SURVIVE.
	for _, name := range []string{
		"gardenio2", "gardenio3", "shannon", "fable", "glm-bedrock", "first-mate-haiku",
		"matthewcbaker0122", "gvisor-canary-acct-96d", "litellm-prod",
		"e2eflow-prod", // legitimately starts with "e2e", no hyphen — must NOT be dropped
		"e2e",          // bare, no hyphen
		"my-e2e-lab",   // e2e- appears mid-name, not as a prefix
		"probe-acct",   // no trailing hyphen
	} {
		if re.MatchString(name) {
			t.Errorf("REAL account %q must NOT be excluded by %q (over-filtering real spend)", name, pat)
		}
	}
}

// TestBuildCostsRollupQuery_AccountFilterOnlyOnGauge proves the by-account gauge
// query applies ONLY the account filter (the gauge has no project/role/model
// labels), so those filters are dropped from the selector rather than producing
// a malformed/empty query (ka-0000006213).
func TestBuildCostsRollupQuery_AccountFilterOnlyOnGauge(t *testing.T) {
	q := buildCostsRollupQuery("account", map[string]string{
		"account": "gardenio3",
		"role":    "gardener", // not a gauge label → must be dropped
		"project": "monorepo",
	}, "7d")
	if !strings.Contains(q, `account="gardenio3"`) {
		t.Errorf("account filter should scope the gauge query: %q", q)
	}
	for _, mustNot := range []string{`role="gardener"`, `project="monorepo"`, costsRollupMetric} {
		if strings.Contains(q, mustNot) {
			t.Errorf("gauge query must not carry non-account selector/counter %q: %q", mustNot, q)
		}
	}
	if !strings.Contains(q, "[7d:5m]") {
		t.Errorf("gauge query should use the [<range>:5m] aligned subquery: %q", q)
	}
}

// TestCostsRollupGaugeIgnoredFilters checks the "filters the gauge can't apply"
// summary used to tell an operator why a non-account filter had no effect on the
// by-account view (ka-0000006213).
func TestCostsRollupGaugeIgnoredFilters(t *testing.T) {
	if got := costsRollupGaugeIgnoredFilters(map[string]string{"account": "gardenio3"}); got != "" {
		t.Errorf("account-only filter set should drop nothing, got %q", got)
	}
	if got := costsRollupGaugeIgnoredFilters(map[string]string{}); got != "" {
		t.Errorf("empty filter set should drop nothing, got %q", got)
	}
	got := costsRollupGaugeIgnoredFilters(map[string]string{
		"account": "gardenio3", "role": "gardener", "project": "monorepo",
	})
	// project before role (costsRollupFilterLabels order); account excluded.
	if got != "project=monorepo, role=gardener" {
		t.Errorf("ignored filters = %q, want %q", got, "project=monorepo, role=gardener")
	}
}

// TestApplyCostsRollupCaption proves the source-provenance captions across the
// three roll-up sources (ka-0000006213 + ka-0000000478).
func TestApplyCostsRollupCaption(t *testing.T) {
	// by-{role,project,model} with NO cross filter → tag gauge: flagged real-bill,
	// carries the per-dimension real-bill note (not the settled warning).
	for _, dim := range []string{"role", "project", "model"} {
		v := costsRollupView{RollupBy: dim}
		applyCostsRollupCaption(&v, map[string]string{})
		if !v.SourceIsRealBill {
			t.Errorf("%s rollup (no cross filter) should be flagged real-bill", dim)
		}
		if v.Caption != costsRollupTagRealBillCaption {
			t.Errorf("%s rollup caption = %q, want tag real-bill caption", dim, v.Caption)
		}
	}
	// by-{role,project,model} WITH a cross filter → per-agent-gauge fallback: still
	// real-bill (ka-0000002699), carries the cross-filter coverage note.
	for _, dim := range []string{"role", "project", "model"} {
		v := costsRollupView{RollupBy: dim}
		// pick a cross-dimension filter (account is always cross for these dims).
		applyCostsRollupCaption(&v, map[string]string{"account": "gardenio3"})
		if !v.SourceIsRealBill {
			t.Errorf("%s rollup (cross-filtered) falls back to the per-agent REAL-BILL gauge, should be flagged real-bill", dim)
		}
		if v.Caption != costsRollupCrossFilterSettledCaption {
			t.Errorf("%s rollup (cross-filtered) caption = %q, want cross-filter coverage caption", dim, v.Caption)
		}
	}
	// by-agent → per-agent real-bill gauge: flagged real-bill, coverage note, and it
	// must NEVER carry the old "settled" warning (that was the 1000x-low counter).
	v := costsRollupView{RollupBy: "agent"}
	applyCostsRollupCaption(&v, map[string]string{"role": "gardener"})
	if !v.SourceIsRealBill {
		t.Error("agent rollup now reads the per-agent real-bill gauge; should be flagged real-bill")
	}
	if v.Caption != costsRollupAgentRealBillCaption {
		t.Errorf("agent caption = %q, want per-agent real-bill caption", v.Caption)
	}
	if strings.Contains(v.Caption, "Settled spend") {
		t.Errorf("agent caption must not carry the old settled warning: %q", v.Caption)
	}
	// Account dim → real-bill note, flagged, with the non-account filters listed.
	va := costsRollupView{RollupBy: "account"}
	applyCostsRollupCaption(&va, map[string]string{"account": "gardenio3", "role": "gardener"})
	if !va.SourceIsRealBill {
		t.Error("account rollup should be flagged real-bill")
	}
	if va.Caption != costsRollupRealBillCaption {
		t.Errorf("account caption = %q, want real-bill caption", va.Caption)
	}
	if va.IgnoredFilters != "role=gardener" {
		t.Errorf("account ignored filters = %q, want %q", va.IgnoredFilters, "role=gardener")
	}
}

// TestResolveRollupSource exercises the routing matrix directly. Post-ka-0000002699
// the by-agent dim and every cross-dimension-filtered role/project/model rollup route
// to the per-agent real-bill spend-logs gauge (rollupSourceAgentGauge), which
// replaced the dead at-close counter.
func TestResolveRollupSource(t *testing.T) {
	cases := []struct {
		dim     string
		filters map[string]string
		want    rollupSource
	}{
		{"account", map[string]string{}, rollupSourceAccountGauge},
		{"account", map[string]string{"role": "gardener"}, rollupSourceAccountGauge},
		{"agent", map[string]string{}, rollupSourceAgentGauge},
		{"agent", map[string]string{"role": "gardener"}, rollupSourceAgentGauge},
		{"role", map[string]string{}, rollupSourceTagGauge},
		{"role", map[string]string{"role": "gardener"}, rollupSourceTagGauge},      // same-dim ok
		{"role", map[string]string{"project": "monorepo"}, rollupSourceAgentGauge}, // cross → per-agent gauge
		{"project", map[string]string{}, rollupSourceTagGauge},
		{"project", map[string]string{"model": "claude-opus-4-8"}, rollupSourceAgentGauge},
		{"model", map[string]string{}, rollupSourceTagGauge},
		{"model", map[string]string{"account": "gardenio3"}, rollupSourceAgentGauge},
	}
	for _, c := range cases {
		if got := resolveRollupSource(c.dim, c.filters); got != c.want {
			t.Errorf("resolveRollupSource(%q, %v) = %d, want %d", c.dim, c.filters, got, c.want)
		}
	}
}

// TestCostsRollup_ByAgentReadsRealBillNotDeadCounter is the direct regression for
// the /costs "Settled cost" 1000x-low P0 (ka-0000002699). It reproduces the prod
// topology at the time of the bug — the at-close counter gardenio_agent_spend_usd_total
// DEAD (0 series, would render ~$0/$36) while the per-agent real-bill spend-logs
// gauge holds the real per-agent bill — and proves the by-agent explorer now reads
// the real bill and reconciles with the attributed-cost total within tolerance,
// instead of the ~1000x-low figure the dead counter produced.
//
// The vmsingle stub answers each query FROM ITS SHAPE, so the assertion pins the
// magnitude end-to-end: if a future edit re-points by-agent back at the counter (or
// switches to increase() on the Reset+Set gauge), the rendered total collapses to
// ~$0 and this test fails.
func TestCostsRollup_ByAgentReadsRealBillNotDeadCounter(t *testing.T) {
	// A realistic per-agent real-bill population: three agents summing to $2745.69,
	// the same order of magnitude as the live 48h-active-agent slice. The attributed
	// total over the same window is $2944.95 (a superset — it includes agents that
	// aged out of the gauge's lookback), so the two reconcile within a structural
	// gap, NOT by 1000x.
	const attributed = 2944.95
	agentGaugeBody := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"agent":"daily-bundle-editor-2rli"},"value":[1700000000,"1286.86"]},` +
		`{"metric":{"agent":"gardenio-3-more-rwx-s0a"},"value":[1700000000,"958.83"]},` +
		`{"metric":{"agent":"monorepo-fsm-triage-kfiy"},"value":[1700000000,"500.00"]}` +
		`]}}`

	mux, lastQuery := mkRollupServer(t, func(q string) string {
		// The DEAD at-close counter: any query touching it returns an empty vector,
		// exactly as prod did (the counter has 0 live series).
		if strings.Contains(q, "gardenio_agent_spend_usd_total") {
			return `{"status":"success","data":{"resultType":"vector","result":[]}}`
		}
		// The by-agent rollup must hit the per-agent real-bill gauge (costsRollupMetric
		// == gardenio_agent_spend_logs_usd) as a raw deduped instant.
		if strings.Contains(q, costsRollupMetric) {
			return agentGaugeBody
		}
		return `{"status":"success","data":{"resultType":"vector","result":[]}}`
	})

	req := httptest.NewRequest("GET", "/costs/rollup?part=table&rollup=agent&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	// 1. The query the handler actually issued rides the real-bill gauge, NOT the
	//    dead counter, and is a raw deduped instant (no increase(), no range window).
	if strings.Contains(*lastQuery, "gardenio_agent_spend_usd_total") {
		t.Fatalf("by-agent rollup still rides the DEAD at-close counter: %q", *lastQuery)
	}
	if !strings.Contains(*lastQuery, costsRollupMetric) {
		t.Fatalf("by-agent rollup must ride the per-agent real-bill gauge: %q", *lastQuery)
	}
	if strings.Contains(*lastQuery, "increase(") {
		t.Fatalf("by-agent rollup must not use increase() on the Reset+Set gauge: %q", *lastQuery)
	}

	// 2. The MAGNITUDE: the rendered total must reconcile with the attributed total,
	//    not read ~1000x low. Pin it as: within 25% of attributed, and — the core of
	//    the bug — at least 10% of it (the dead counter produced <1%).
	body := w.Body.String()
	total := parseRenderedRollupTotal(t, body)
	if total < 0.10*attributed {
		t.Fatalf("by-agent total $%.2f is <10%% of the $%.2f attributed bill — the 1000x-low regression is back", total, attributed)
	}
	if total > 1.25*attributed {
		t.Fatalf("by-agent total $%.2f exceeds 125%% of the $%.2f attributed bill — over-counting", total, attributed)
	}
	// Sanity: the exact sum of the three gauge rows.
	if total < 2745.0 || total > 2746.5 {
		t.Errorf("by-agent total $%.2f, want ~$2745.69 (sum of the gauge rows)", total)
	}
}

// parseRenderedRollupTotal extracts the "total <strong>$N</strong>" figure the
// costs_rollup_table fragment renders, so a test can assert on the operator-visible
// magnitude rather than an internal field.
func parseRenderedRollupTotal(t *testing.T, body string) float64 {
	t.Helper()
	// The template renders: total <strong>$2,745.69</strong>
	const anchor = "total <strong>$"
	i := strings.Index(body, anchor)
	if i < 0 {
		t.Fatalf("rendered rollup table has no total figure:\n%s", body)
	}
	rest := body[i+len(anchor):]
	end := strings.Index(rest, "</strong>")
	if end < 0 {
		t.Fatalf("malformed total figure in rollup table:\n%s", body)
	}
	num := strings.ReplaceAll(rest[:end], ",", "")
	v, ok := parseFloat(num)
	if !ok {
		t.Fatalf("could not parse rollup total %q", rest[:end])
	}
	return v
}

func TestEscapePromQLLabelValue(t *testing.T) {
	// Even though sanitize rejects these upstream, the escaper must be correct.
	if got := escapePromQLLabelValue(`a"b`); got != `a\"b` {
		t.Errorf("escape quote: got %q", got)
	}
	if got := escapePromQLLabelValue(`a\b`); got != `a\\b` {
		t.Errorf("escape backslash: got %q", got)
	}
}

func TestCostsRollupRowsFromVector(t *testing.T) {
	resp := &vmVectorResponse{}
	resp.Status = "success"
	resp.Data.ResultType = "vector"
	add := func(role, val string) {
		resp.Data.Result = append(resp.Data.Result, struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		}{
			Metric: map[string]string{"role": role},
			Value:  [2]json.RawMessage{json.RawMessage("1700000000"), json.RawMessage(`"` + val + `"`)},
		})
	}
	add("gardener", "10")
	add("crew", "30")
	add("thread", "0")
	add("broken", "NaN")  // dropped — not a meaningful spend
	add("negative", "-5") // floored to 0

	rows, total := costsRollupRowsFromVector(resp, "role")
	if total != 40 { // 10 + 30 + 0 + 0 (negative floored); NaN dropped
		t.Fatalf("total = %v, want 40", total)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (NaN dropped)", len(rows))
	}
	// Sorted descending: crew(30) first, then gardener(10).
	if rows[0].Key != "crew" || rows[0].USD != 30 {
		t.Errorf("rows[0] = %+v, want crew/30", rows[0])
	}
	if rows[0].Rank != 1 || rows[1].Rank != 2 {
		t.Errorf("ranks not 1-based sequential: %d,%d", rows[0].Rank, rows[1].Rank)
	}
	// Top row's bar is full width; share is of the total.
	if rows[0].BarPct != 100 {
		t.Errorf("top BarPct = %d, want 100", rows[0].BarPct)
	}
	if rows[0].SharePct != 75 { // 30/40
		t.Errorf("top SharePct = %d, want 75", rows[0].SharePct)
	}
}

func TestCostsRollupRowsFromVector_MissingDimLabel(t *testing.T) {
	resp := &vmVectorResponse{}
	resp.Status = "success"
	resp.Data.Result = append(resp.Data.Result, struct {
		Metric map[string]string  `json:"metric"`
		Value  [2]json.RawMessage `json:"value"`
	}{
		Metric: map[string]string{}, // no "agent" label
		Value:  [2]json.RawMessage{json.RawMessage("1700000000"), json.RawMessage(`"5"`)},
	})
	rows, _ := costsRollupRowsFromVector(resp, "agent")
	if len(rows) != 1 || rows[0].Key != "(none)" {
		t.Errorf("missing-dim sample should bucket under (none): %+v", rows)
	}
}

// TestCostsRollupDimDefault_IsAgent guards the headline default: the cost
// explorer opens on the per-agent view (ka-0000001479), because "who spent the
// most" is the first question operators ask and agent rows link to beads.
func TestCostsRollupDimDefault_IsAgent(t *testing.T) {
	if costsRollupDimDefault != "agent" {
		t.Errorf("costsRollupDimDefault = %q, want %q", costsRollupDimDefault, "agent")
	}
	// The default must be a member of the closed dim allowlist, else
	// validRollupDim would loop the default back to itself indefinitely or
	// produce a malformed by-clause.
	if !slices.Contains(costsRollupDims, costsRollupDimDefault) {
		t.Errorf("default dim %q not in allowlist %v", costsRollupDimDefault, costsRollupDims)
	}
}

func TestApplyRollupRowLinks(t *testing.T) {
	base := "/bp"
	// Role + project rows link deterministically with no daemon round-trip.
	roleRows := []costsRollupRow{{Key: "gardener"}, {Key: "(none)"}}
	applyRollupRowLinks(roleRows, "role", base, nil)
	if roleRows[0].Href != "/bp/role/gardener" {
		t.Errorf("role href = %q, want /bp/role/gardener", roleRows[0].Href)
	}
	if roleRows[1].Href != "" {
		t.Errorf("(none) role row should stay unlinked, got %q", roleRows[1].Href)
	}

	projRows := []costsRollupRow{{Key: "gardenio"}}
	applyRollupRowLinks(projRows, "project", base, nil)
	if projRows[0].Href != "/bp/p/gardenio" {
		t.Errorf("project href = %q, want /bp/p/gardenio", projRows[0].Href)
	}

	// Model / account have no per-row page, so they stay unlinked.
	for _, dim := range []string{"model", "account"} {
		rows := []costsRollupRow{{Key: "claude-opus-4-8"}}
		applyRollupRowLinks(rows, dim, base, nil)
		if rows[0].Href != "" {
			t.Errorf("%s row should stay unlinked, got %q", dim, rows[0].Href)
		}
	}

	// Agent rows resolve through the resolver: a match links to its bead, a
	// miss stays unlinked, and a non-agent-shaped key is skipped before any
	// lookup. agentLinkFake resolves only "thread-bot-1".
	agentRows := []costsRollupRow{
		{Key: "thread-bot-1"},     // resolves → gd-agent1
		{Key: "unknown-agent-99"}, // agent-shaped but unresolved → unlinked
		{Key: "(none)"},           // sentinel → skipped
	}
	applyRollupRowLinks(agentRows, "agent", base, resolverFor(agentLinkFake()))
	if agentRows[0].Href != "/bp/gd-agent1" {
		t.Errorf("agent href = %q, want /bp/gd-agent1", agentRows[0].Href)
	}
	if agentRows[1].Href != "" {
		t.Errorf("unresolved agent should stay unlinked, got %q", agentRows[1].Href)
	}
	if agentRows[2].Href != "" {
		t.Errorf("(none) agent row should stay unlinked, got %q", agentRows[2].Href)
	}

	// Nil resolver must not panic and leaves agent rows unlinked.
	nilRows := []costsRollupRow{{Key: "thread-bot-1"}}
	applyRollupRowLinks(nilRows, "agent", base, nil)
	if nilRows[0].Href != "" {
		t.Errorf("nil resolver should leave agent rows unlinked, got %q", nilRows[0].Href)
	}
}

// --- handler integration tests ------------------------------------------------

// mkRollupServer builds a viewer wired to a fake vmsingle that echoes the query
// and returns the supplied JSON body. Returns the mux + a pointer to the last
// query string vmsingle received (for assertion).
func mkRollupServer(t *testing.T, vmBody func(query string) string) (*http.ServeMux, *string) {
	t.Helper()
	var lastQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vmBody(lastQuery)))
	}))
	t.Cleanup(upstream.Close)

	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux, &lastQuery
}

// rankedBody returns a ranked-cost vector for the "role" dimension. Each sample
// carries BOTH the `role` label (the counter/account-gauge group key) and the
// `value` label (the tag-gauge group key) with the same value, so the row
// extractor finds the key whichever source backs the roll-up (ka-0000000478).
func rankedBody() string {
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"role":"gardener","value":"gardener"},"value":[1700000000,"42.50"]},` +
		`{"metric":{"role":"crew","value":"crew"},"value":[1700000000,"12.00"]}` +
		`]}}`
}

// TestHandleCostsRollup_AccountsDropdownExcludesE2E pins the dropdown half of the
// PromQL-side fix (ka-0000005167): the Accounts <select> is populated from a
// `count by(account)(...)` label-values query, and its account series outlive the
// bead just like the gauge's — so that query must carry the E2E exclusion. The
// project/role/model dropdowns must NOT (an account matcher there is meaningless).
func TestHandleCostsRollup_AccountsDropdownExcludesE2E(t *testing.T) {
	var queries []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(q, "count by(") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"account":"gardenio3"}}]}}`))
			return
		}
		_, _ = w.Write([]byte(rankedBody()))
	}))
	t.Cleanup(upstream.Close)

	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Full fragment (not part=table) is what populates the dropdowns.
	req := httptest.NewRequest("GET", "/costs/rollup?rollup=account&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var sawAccountDropdown bool
	for _, q := range queries {
		if !strings.HasPrefix(q, "count by(") {
			continue
		}
		isAccount := strings.HasPrefix(q, "count by(account)")
		if isAccount {
			sawAccountDropdown = true
			if !strings.Contains(q, costsRollupE2EAccountExclusion) {
				t.Errorf("Accounts dropdown query %q missing the E2E exclusion %q", q, costsRollupE2EAccountExclusion)
			}
			continue
		}
		if strings.Contains(q, "probe-acct") {
			t.Errorf("non-account dropdown query must not carry the account exclusion: %q", q)
		}
	}
	if !sawAccountDropdown {
		t.Fatalf("expected a count by(account) dropdown query; got %v", queries)
	}
}

// TestCostsRollupLabelValues_AccountSourcedFromGauge is the core of the
// ka-0000007866 fix: the Accounts dropdown must be sourced from the SAME real-bill
// account-cost gauge that backs the by-account rollup, NOT the per-agent spend-logs
// gauge. Sourcing it from costsRollupMetric let the table rank accounts the picker
// could never offer, because the spend-logs gauge only carries accounts with an
// agent active in the sweep's ~48h window while the account gauge carries the full
// retention tail. This pins the exact metric each axis queries.
func TestCostsRollupLabelValues_AccountSourcedFromGauge(t *testing.T) {
	byLabel := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		// Record the query per axis (count by(<label>)(<metric>{...})).
		for _, label := range costsRollupFilterLabels {
			if strings.HasPrefix(q, "count by("+label+")") {
				byLabel[label] = q
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(upstream.Close)

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)
	ctx := context.Background()
	for _, label := range costsRollupFilterLabels {
		s.costsRollupLabelValues(ctx, label)
	}

	// account → the real-bill account-cost gauge (same as the by-account rollup),
	// carrying the E2E exclusion (which is load-bearing on the gauge's retention
	// tail), and never the per-agent spend-logs gauge.
	accQ := byLabel[costsRollupGaugeDim]
	if accQ == "" {
		t.Fatalf("no account dropdown query captured; saw %v", byLabel)
	}
	wantAcc := "count by(account)(" + costsRollupGaugeMetric + "{" + costsRollupE2EAccountExclusion + "})"
	if accQ != wantAcc {
		t.Errorf("account dropdown query = %q, want %q", accQ, wantAcc)
	}
	if strings.Contains(accQ, costsRollupMetric) {
		t.Errorf("account dropdown must NOT read the per-agent spend-logs gauge %q: %q", costsRollupMetric, accQ)
	}

	// project/role/model → costsRollupMetric, no account matcher.
	for _, label := range []string{"project", "role", "model"} {
		q := byLabel[label]
		if q == "" {
			t.Fatalf("no %s dropdown query captured; saw %v", label, byLabel)
		}
		want := "count by(" + label + ")(" + costsRollupMetric + ")"
		if q != want {
			t.Errorf("%s dropdown query = %q, want %q", label, q, want)
		}
		if strings.Contains(q, "account") {
			t.Errorf("%s dropdown must not carry an account matcher/gauge: %q", label, q)
		}
	}
}

func TestHandleCostsRollup_FullFragment(t *testing.T) {
	mux, _ := mkRollupServer(t, func(q string) string {
		// label_values lookups use count by(...); ranked query uses topk(...).
		if strings.HasPrefix(q, "count by(") {
			return `{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"role":"gardener"}},{"metric":{"role":"crew"}}]}}`
		}
		return rankedBody()
	})

	req := httptest.NewRequest("GET", "/costs/rollup?rollup=role&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"costs-rollup-controls", // the form rendered (full fragment)
		`name="rollup"`,
		`name="project"`,
		"Roll up by",
		"gardener",           // ranked row
		"$42.50",             // formatted spend
		"costs-rollup-table", // the swap target id exists
	} {
		if !strings.Contains(body, want) {
			t.Errorf("full fragment missing %q\n---\n%s", want, body)
		}
	}
}

func TestHandleCostsRollup_TableOnly(t *testing.T) {
	mux, _ := mkRollupServer(t, func(q string) string { return rankedBody() })

	req := httptest.NewRequest("GET", "/costs/rollup?part=table&rollup=role&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	// Table-only fragment must NOT carry the controls form (those keep state).
	// Assert on the form control + the always-present <style> isn't shipped here.
	if strings.Contains(body, `name="rollup"`) || strings.Contains(body, "<form") {
		t.Errorf("table-only fragment should not re-render the controls form")
	}
	for _, want := range []string{"gardener", "$42.50", "crew", "$12.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("table fragment missing %q", want)
		}
	}
}

func TestHandleCostsRollup_PassesFiltersToQuery(t *testing.T) {
	mux, lastQuery := mkRollupServer(t, func(q string) string { return rankedBody() })

	// The operator's worked example.
	u := "/costs/rollup?part=table&rollup=agent&range=7d&" +
		url.Values{"role": {"gardener"}, "project": {"monorepo"}}.Encode()
	req := httptest.NewRequest("GET", u, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, want := range []string{"sum by(agent)(", `role="gardener"`, `project="monorepo"`} {
		if !strings.Contains(*lastQuery, want) {
			t.Errorf("vmsingle query %q missing %q", *lastQuery, want)
		}
	}
	// by-agent is a raw deduped instant on the per-agent real-bill gauge — no
	// increase(), no range window (ka-0000002699).
	if strings.Contains(*lastQuery, "increase(") || strings.Contains(*lastQuery, "[7d") {
		t.Errorf("by-agent query must be a raw gauge instant, not a windowed increase: %q", *lastQuery)
	}
}

// TestHandleCostsRollup_AccountSendsGaugeQueryAndRealBillCaption is the
// end-to-end proof of the ka-0000006213 fix at the handler layer: selecting the
// by-account roll-up sends the REAL-BILL gauge query (deduped windowed increase
// of gardenio_account_cost_usd) to vmsingle — not the at-close counter — and the
// rendered table carries the neutral "real bill" caption (NOT the settled
// warning).
func TestHandleCostsRollup_AccountSendsGaugeQueryAndRealBillCaption(t *testing.T) {
	mux, lastQuery := mkRollupServer(t, func(q string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"account":"gardenio3"},"value":[1700000000,"232.61"]}` +
			`]}}`
	})

	req := httptest.NewRequest("GET", "/costs/rollup?part=table&rollup=account&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// The query must hit the real-bill gauge with the headline's dedup+subquery
	// shape, and must NOT reference the at-close counter.
	for _, want := range []string{
		"sum by(account)(",
		"increase((max by (account, env) (" + costsRollupGaugeMetric + "{" + costsRollupE2EAccountExclusion + "}))[24h:5m])",
	} {
		if !strings.Contains(*lastQuery, want) {
			t.Errorf("by-account vmsingle query %q missing %q", *lastQuery, want)
		}
	}
	if strings.Contains(*lastQuery, costsRollupMetric) {
		t.Errorf("by-account query must not ride the at-close counter: %q", *lastQuery)
	}
	body := w.Body.String()
	if !strings.Contains(body, "real bill") {
		t.Errorf("by-account table should carry the real-bill caption; body:\n%s", body)
	}
	if strings.Contains(body, "Settled spend") {
		t.Errorf("by-account table must NOT carry the settled warning; body:\n%s", body)
	}
}

// TestHandleCostsRollup_RoleSendsTagGaugeAndRealBillCaption is the end-to-end
// proof of the ka-0000000478 fix at the handler layer: by-role (no cross filter)
// sends the REAL-BILL per-dimension tag-gauge query to vmsingle — not the at-close
// counter — and the rendered table carries the real-bill caption, NOT the settled
// warning.
func TestHandleCostsRollup_RoleSendsTagGaugeAndRealBillCaption(t *testing.T) {
	mux, lastQuery := mkRollupServer(t, func(q string) string { return rankedBody() })

	req := httptest.NewRequest("GET", "/costs/rollup?part=table&rollup=role&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// Tag-gauge path: real-bill gauge with the headline's dedup + subquery shape.
	if !strings.Contains(*lastQuery, "increase((max by (dim, value, env) ("+costsRollupTagGaugeMetric+"{dim=\"role\"}))[24h:5m])") {
		t.Errorf("role rollup should ride the real-bill tag gauge: %q", *lastQuery)
	}
	if strings.Contains(*lastQuery, costsRollupMetric) {
		t.Errorf("role rollup (no cross filter) must ride the tag gauge, not the per-agent gauge: %q", *lastQuery)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Real LiteLLM bill") {
		t.Errorf("by-role table should carry the real-bill caption; body:\n%s", body)
	}
	if strings.Contains(body, "Settled spend") {
		t.Errorf("by-role table (no cross filter) must NOT carry the settled warning; body:\n%s", body)
	}
	// The ranked rows are still keyed correctly (via the gauge's `value` label).
	for _, want := range []string{"gardener", "$42.50", "crew"} {
		if !strings.Contains(body, want) {
			t.Errorf("by-role table missing %q; body:\n%s", want, body)
		}
	}
}

// TestHandleCostsRollup_RoleCrossFilterFallsBackToAgentGauge proves the fallback: a
// by-role rollup WITH a cross-dimension filter (project) can't be expressed on the
// single-dim tag gauge, so it rides the cross-filterable per-agent REAL-BILL
// spend-logs gauge (a raw deduped instant) and carries the cross-filter coverage
// caption — never the retired at-close counter (ka-0000000478 / ka-0000002699).
func TestHandleCostsRollup_RoleCrossFilterFallsBackToAgentGauge(t *testing.T) {
	mux, lastQuery := mkRollupServer(t, func(q string) string { return rankedBody() })

	u := "/costs/rollup?part=table&rollup=role&range=24h&" + url.Values{"project": {"monorepo"}}.Encode()
	req := httptest.NewRequest("GET", u, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// Per-agent-gauge path: raw deduped instant, cross filter applied, group-by-dim.
	// The selector carries the E2E exclusion (ka-0000005167) AND the project filter.
	if !strings.Contains(*lastQuery, "sum by(role)(max by (agent, project, role, model, account) ("+costsRollupMetric+"{") {
		t.Errorf("cross-filtered role rollup should ride the per-agent gauge (deduped raw instant): %q", *lastQuery)
	}
	if !strings.Contains(*lastQuery, `project="monorepo"`) {
		t.Errorf("cross-filtered role rollup should keep the project filter: %q", *lastQuery)
	}
	if !strings.Contains(*lastQuery, costsRollupE2EAccountExclusion) {
		t.Errorf("cross-filtered role rollup should carry the E2E account exclusion: %q", *lastQuery)
	}
	if strings.Contains(*lastQuery, "gardenio_agent_spend_usd_total") {
		t.Errorf("cross-filtered role rollup must not ride the retired at-close counter: %q", *lastQuery)
	}
	if strings.Contains(*lastQuery, costsRollupTagGaugeMetric) {
		t.Errorf("cross-filtered role rollup must not use the tag gauge: %q", *lastQuery)
	}
	if strings.Contains(*lastQuery, "increase(") {
		t.Errorf("cross-filtered role rollup must be a raw gauge instant, not an increase: %q", *lastQuery)
	}
	body := w.Body.String()
	// Still real-bill (per-agent gauge), so it must NOT carry the old settled warning.
	if strings.Contains(body, "Settled spend") {
		t.Errorf("cross-filtered by-role table must not carry the old settled warning; body:\n%s", body)
	}
	if !strings.Contains(body, "Real LiteLLM bill (per-agent gauge)") {
		t.Errorf("cross-filtered by-role table should carry the per-agent-gauge real-bill caption; body:\n%s", body)
	}
}

// TestHandleCostsRollup_RoleBackfillFallback proves the self-healing backfill: a
// by-role rollup tries the per-dimension tag gauge first, and when it returns NO
// rows (empty right after the tag-spend producer deploys) falls back to the
// per-agent REAL-BILL spend-logs gauge so the table is never blank — and carries
// the warming-up caption (ka-0000000478 / ka-0000002699).
func TestHandleCostsRollup_RoleBackfillFallback(t *testing.T) {
	var queries []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		w.Header().Set("Content-Type", "application/json")
		// The tag-gauge query returns an EMPTY vector; the per-agent-gauge fallback
		// (which references costsRollupMetric) returns rows.
		if strings.Contains(q, costsRollupTagGaugeMetric) {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		_, _ = w.Write([]byte(rankedBody()))
	}))
	t.Cleanup(upstream.Close)

	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/costs/rollup?part=table&rollup=role&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// Both queries ran: tag gauge first, then the per-agent-gauge fallback (a raw
	// deduped instant on costsRollupMetric, never increase()).
	var sawGauge, sawAgentGauge bool
	for _, q := range queries {
		if strings.Contains(q, costsRollupTagGaugeMetric) {
			sawGauge = true
		}
		if strings.Contains(q, "max by (agent, project, role, model, account) ("+costsRollupMetric+"{") {
			sawAgentGauge = true
		}
	}
	if !sawGauge || !sawAgentGauge {
		t.Errorf("expected tag-gauge then per-agent-gauge fallback queries; got %v", queries)
	}
	body := w.Body.String()
	// Rows rendered from the per-agent-gauge fallback, and the warming-up caption shown.
	if !strings.Contains(body, "gardener") || !strings.Contains(body, "$42.50") {
		t.Errorf("fallback table should render per-agent-gauge rows; body:\n%s", body)
	}
	if !strings.Contains(body, "warming up") {
		t.Errorf("backfill fallback should carry the warming-up caption; body:\n%s", body)
	}
	// The backfill IS the real bill (per-agent gauge), so it must NOT carry the old
	// settled warning — but it should still read as a real-bill source.
	if strings.Contains(body, "Settled spend") {
		t.Errorf("backfill fallback must not carry the old settled warning; body:\n%s", body)
	}
	if !strings.Contains(body, "Real LiteLLM bill (per-agent gauge)") {
		t.Errorf("backfill fallback should carry the per-agent-gauge real-bill caption; body:\n%s", body)
	}
}

// TestRunCostsRollupQuery_NoFallbackWhenGaugeHasData proves the happy path does
// NOT issue a second query: when the tag gauge returns rows, the counter is never
// hit (ka-0000000478).
func TestRunCostsRollupQuery_NoFallbackWhenGaugeHasData(t *testing.T) {
	var queries []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("query"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rankedBody())) // gauge query returns rows
	}))
	t.Cleanup(upstream.Close)

	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)

	resp, keyLabel, usedFallback, ok := srv.runCostsRollupQuery(context.Background(), "role", map[string]string{}, "24h")
	if !ok || resp == nil {
		t.Fatalf("expected a usable response, ok=%v", ok)
	}
	if usedFallback {
		t.Error("should not fall back when the tag gauge has data")
	}
	if keyLabel != costsRollupTagGaugeKeyLabel {
		t.Errorf("key label = %q, want %q", keyLabel, costsRollupTagGaugeKeyLabel)
	}
	if len(queries) != 1 {
		t.Errorf("expected exactly 1 upstream query, got %d: %v", len(queries), queries)
	}
}

func TestHandleCostsRollup_RejectsInjectionFilter(t *testing.T) {
	mux, lastQuery := mkRollupServer(t, func(q string) string { return rankedBody() })

	// A crafted role value must be dropped (treated as All), never reach vmsingle.
	u := "/costs/rollup?part=table&rollup=role&range=24h&role=" + url.QueryEscape(`x"} or up{`)
	req := httptest.NewRequest("GET", u, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(*lastQuery, "or up{") || strings.Contains(*lastQuery, `role="x`) {
		t.Errorf("injection leaked into query: %q", *lastQuery)
	}
	// With the bad filter dropped, the selector is empty (no role= matcher).
	if strings.Contains(*lastQuery, "role=") {
		t.Errorf("rejected filter should produce no role selector: %q", *lastQuery)
	}
}

// TestHandleCostsRollup_AgentRowsLinkToBeads is the end-to-end proof that an
// agent rollup renders clickable rows: the ranked vmsingle vector keys agents
// by name, the handler prewarms the resolver against the daemon, and the table
// emits an <a href="/{gd-id}"> for the resolved agent (ka-0000001479).
func TestHandleCostsRollup_AgentRowsLinkToBeads(t *testing.T) {
	// vmsingle returns a one-row agent vector for "thread-bot-1".
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.HasPrefix(q, "count by(") {
			// dropdown label-values lookup — empty is fine for this test.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"agent":"thread-bot-1"},"value":[1700000000,"42.50"]}` +
			`]}}`))
	}))
	t.Cleanup(upstream.Close)

	// agentLinkFake resolves "thread-bot-1" → gd-agent1.
	srv := NewServer(agentLinkFake(), slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/costs/rollup?part=table&rollup=agent&range=24h", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/gd-agent1"`) {
		t.Errorf("agent rollup row should link to the resolved bead; body:\n%s", body)
	}
	// The "biggest agent" lead callout must also render and link.
	if !strings.Contains(body, "costs-rollup-lead") {
		t.Errorf("agent rollup should render the biggest-spender lead callout; body:\n%s", body)
	}
}

func TestHandleCostsRollup_Unavailable(t *testing.T) {
	// No metrics upstream wired → degrade to the honest note, status still 200.
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/costs/rollup", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Live rollup unavailable") {
		t.Errorf("expected unavailable note, got:\n%s", body)
	}
	// The roll-up <select> (name="rollup") only renders in the available branch;
	// its absence proves the unavailable branch skipped the live controls. (The
	// CSS class name appears in the always-present <style> block, so assert on
	// the form control, not the class.)
	if strings.Contains(body, `name="rollup"`) {
		t.Errorf("unavailable fragment should not render live controls")
	}
}

func TestHandleCostsRollup_InvalidDimFallsBackToDefault(t *testing.T) {
	mux, lastQuery := mkRollupServer(t, func(q string) string { return rankedBody() })
	req := httptest.NewRequest("GET", "/costs/rollup?part=table&rollup=DROP", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(*lastQuery, "sum by("+costsRollupDimDefault+")(") {
		t.Errorf("invalid dim should fall back to default %q: %q", costsRollupDimDefault, *lastQuery)
	}
}

// TestCostsPage_EmbedsRollupShell verifies the /costs page itself carries the
// lazy-load shell that fetches the rollup fragment — the operator-visible entry
// point for the interactive surface (decision ka-0000000742).
func TestCostsPage_EmbedsRollupShell(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="costs-rollup-host"`,
		`hx-get="/costs/rollup"`,
		`hx-trigger="intersect once"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/costs missing rollup shell marker %q", want)
		}
	}
	// The interactive explorer must come BEFORE the Grafana time-series charts:
	// it is the headline per-agent surface and works with no Grafana session,
	// so it leads (ka-0000001479). Guard the ordering so a future edit can't
	// silently bury it back below the embeds.
	explorerAt := strings.Index(body, `id="costs-rollup-host"`)
	chartsAt := strings.Index(body, `class="costs-chart-cards"`)
	if explorerAt < 0 || chartsAt < 0 {
		t.Fatalf("missing explorer (%d) or charts (%d) section", explorerAt, chartsAt)
	}
	if explorerAt > chartsAt {
		t.Errorf("interactive explorer (at %d) must render before the Grafana charts (at %d)", explorerAt, chartsAt)
	}
}

func TestInstantVectorQuery_NoUpstream(t *testing.T) {
	s := &Server{logger: slog.Default()}
	if _, ok := s.instantVectorQuery(context.Background(), "up"); ok {
		t.Error("expected unavailable with no upstream wired")
	}
}
