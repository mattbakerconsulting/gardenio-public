package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// outageFixture is the REAL /api/v2/summary.json payload captured from
// status.claude.com at 2026-07-29T21:51Z, during the incident that motivated
// this detector (epic ka-0000000487). Trimmed to the fields the poller reads.
//
// Recording the genuine payload matters: a hand-written fixture would encode
// whatever shape the author assumed, and the whole failure mode being guarded
// against is "the poller silently reads nothing useful".
const outageFixture = `{
  "page": {"id": "tymt9n04zgry", "name": "Claude", "url": "https://status.claude.com"},
  "status": {"indicator": "minor", "description": "Minor Service Outage"},
  "components": [
    {"id": "rwppv331jlwc", "name": "claude.ai", "status": "partial_outage"},
    {"id": "0qbwn08sd68x", "name": "Claude Console (platform.claude.com)", "status": "operational"},
    {"id": "k8w3r06qmzrp", "name": "Claude API (api.anthropic.com)", "status": "partial_outage"},
    {"id": "yyzkbfz2thpt", "name": "Claude Code", "status": "partial_outage"},
    {"id": "bpp5gb3hpjcl", "name": "Claude Cowork", "status": "partial_outage"},
    {"id": "0scnb50nvy53", "name": "Claude for Government", "status": "operational"}
  ],
  "incidents": [
    {"id": "q2kg8n613kr3", "name": "Elevated errors across all models",
     "status": "identified", "impact": "critical"}
  ],
  "scheduled_maintenances": []
}`

const healthyFixture = `{
  "page": {"id": "tymt9n04zgry", "name": "Claude"},
  "status": {"indicator": "none", "description": "All Systems Operational"},
  "components": [
    {"name": "claude.ai", "status": "operational"},
    {"name": "Claude API (api.anthropic.com)", "status": "operational"},
    {"name": "Claude Code", "status": "operational"}
  ],
  "incidents": [],
  "scheduled_maintenances": []
}`

// discardStatusLogger keeps poll-failure warnings out of the test output; the
// poller logs on every deliberate failure case below.
func discardStatusLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newStatusServer serves body from /api/v2/summary.json and returns a poller
// wired to it with a fixed clock.
func newStatusPoller(t *testing.T, body *string, code *int) (*ClaudeStatusPoller, *atomic.Int64) {
	t.Helper()
	// hits is written by the httptest handler goroutine and read by the test
	// body (e.g. TestClaudeStatusPoller_RunChecksImmediately's poll loop), so it
	// must be accessed atomically or -race flags the concurrent access.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if code != nil && *code != http.StatusOK {
			w.WriteHeader(*code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(*body))
	}))
	t.Cleanup(srv.Close)

	p := NewClaudeStatusPoller(ClaudeStatusConfig{
		URL:    srv.URL + "/api/v2/summary.json",
		Logger: discardStatusLogger(),
		Now:    func() time.Time { return time.Unix(1785362000, 0) },
	})
	return p, &hits
}

// TestClaudeStatusPoller_ReproducesRealOutage is the acceptance criterion from
// the task bead: the poller run against the recorded outage payload must
// reproduce that exact reading — indicator=minor(1), claude_api and claude_code
// at partial_outage(2), one critical open incident.
func TestClaudeStatusPoller_ReproducesRealOutage(t *testing.T) {
	body := outageFixture
	p, _ := newStatusPoller(t, &body, nil)

	p.Check(context.Background())

	if got := testutil.ToFloat64(p.metrics.indicator); got != claudeIndicatorMinor {
		t.Errorf("indicator = %v, want %d (minor)", got, claudeIndicatorMinor)
	}
	for _, slug := range []string{"claude_api", "claude_code", "claude_ai", "claude_cowork"} {
		if got := testutil.ToFloat64(p.metrics.component.WithLabelValues(slug)); got != claudeComponentPartialOutage {
			t.Errorf("component %s = %v, want %d (partial_outage)", slug, got, claudeComponentPartialOutage)
		}
	}
	for _, slug := range []string{"claude_console", "claude_gov"} {
		if got := testutil.ToFloat64(p.metrics.component.WithLabelValues(slug)); got != claudeComponentOperational {
			t.Errorf("component %s = %v, want operational", slug, got)
		}
	}
	if got := testutil.ToFloat64(p.metrics.openIncidents.WithLabelValues("critical")); got != 1 {
		t.Errorf("open critical incidents = %v, want 1", got)
	}
	if got := testutil.ToFloat64(p.metrics.lastCheckTS); got != 1785362000 {
		t.Errorf("last check ts = %v, want the fixed clock value", got)
	}

	v := p.Verdict()
	if !v.Suspect() {
		t.Error("Suspect() = false during a real API partial_outage with a critical incident")
	}
	summary := v.Summary()
	for _, want := range []string{"Minor Service Outage", "claude_api=partial_outage", "q2kg8n613kr3", "critical"} {
		if !strings.Contains(summary, want) {
			t.Errorf("Summary() = %q, missing %q", summary, want)
		}
	}
}

// A healthy page must read as healthy AND clear the incident gauge, so an alert
// that fired during an outage can resolve.
// TestClaudeStatusPoller_HealthyPageStillExportsIncidentBuckets pins the fix for
// ka-0000003771: a GaugeVec exports NO series for a label until it is first set,
// so the old reset-then-set-only-nonzero shape left a clean page (the steady
// state) exporting zero incident series. The leg-B "open incidents by impact"
// System Health panel then queried absent series and rendered an empty green — a
// silently-dead detector. The poller must now seed all four impact buckets at 0
// even for an all-clear page so the panel has a flat-zero baseline to plot.
func TestClaudeStatusPoller_HealthyPageStillExportsIncidentBuckets(t *testing.T) {
	body := healthyFixture
	p, _ := newStatusPoller(t, &body, nil)

	p.Check(context.Background())

	if got := testutil.CollectAndCount(p.metrics.openIncidents); got != len(claudeImpactLevels) {
		t.Fatalf("open-incident series on a clean page = %d, want %d (every impact bucket seeded at 0 so the leg-B panel is not empty-green)",
			got, len(claudeImpactLevels))
	}
	for _, impact := range claudeImpactLevels {
		if got := testutil.ToFloat64(p.metrics.openIncidents.WithLabelValues(impact)); got != 0 {
			t.Errorf("open %s on a clean page = %v, want 0", impact, got)
		}
	}
}

func TestClaudeStatusPoller_HealthyPageClearsIncidents(t *testing.T) {
	body := outageFixture
	p, _ := newStatusPoller(t, &body, nil)
	p.Check(context.Background())
	if got := testutil.ToFloat64(p.metrics.openIncidents.WithLabelValues("critical")); got != 1 {
		t.Fatalf("setup: expected 1 critical incident, got %v", got)
	}

	body = healthyFixture
	p.Check(context.Background())

	if got := testutil.ToFloat64(p.metrics.indicator); got != claudeIndicatorNone {
		t.Errorf("indicator = %v, want 0 (none)", got)
	}
	// The four impact buckets stay present (seeded each scrape, ka-0000003771) but
	// must all read 0 so the alert resolves and the leg-B panel shows a flat-zero
	// baseline rather than an empty (absent-series) green.
	if got := testutil.CollectAndCount(p.metrics.openIncidents); got != len(claudeImpactLevels) {
		t.Errorf("incident series = %d, want %d (all impact buckets present, seeded at 0)", got, len(claudeImpactLevels))
	}
	for _, impact := range claudeImpactLevels {
		if got := testutil.ToFloat64(p.metrics.openIncidents.WithLabelValues(impact)); got != 0 {
			t.Errorf("open %s = %v, want 0 (must clear so the alert resolves)", impact, got)
		}
	}
	if got := testutil.ToFloat64(p.metrics.component.WithLabelValues("claude_api")); got != claudeComponentOperational {
		t.Errorf("claude_api = %v, want operational", got)
	}
	if v := p.Verdict(); v.Suspect() {
		t.Error("Suspect() = true on an all-operational page")
	}
}

// TestClaudeStatusPoller_FollowsRedirect guards the exact footgun in the task
// bead: status.anthropic.com 302s to status.claude.com, so a client that does
// not follow redirects gets a 302 body, fails to parse, and the detector is
// silently blind during the outage it exists to catch.
func TestClaudeStatusPoller_FollowsRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outageFixture))
	}))
	t.Cleanup(target.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v2/summary.json", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	p := NewClaudeStatusPoller(ClaudeStatusConfig{
		URL:    redirector.URL + "/api/v2/summary.json",
		Logger: discardStatusLogger(),
		Now:    func() time.Time { return time.Unix(1785362000, 0) },
	})
	p.Check(context.Background())

	if v := p.Verdict(); v.Indicator != "minor" {
		t.Errorf("indicator through a 302 = %q, want minor — the default client must follow redirects", v.Indicator)
	}
}

// TestClaudeStatusPoller_FetchFailureLeavesGaugesStale is the "do not suppress
// the alert" property. A failed fetch must NOT publish "all operational": if it
// did, a network partition — or the status page going down, which correlates
// with an outage — would actively mask the incident.
func TestClaudeStatusPoller_FetchFailureLeavesGaugesStale(t *testing.T) {
	body := outageFixture
	code := http.StatusOK
	p, _ := newStatusPoller(t, &body, &code)

	p.Check(context.Background())
	before := testutil.ToFloat64(p.metrics.indicator)
	if before != claudeIndicatorMinor {
		t.Fatalf("setup: indicator = %v, want minor", before)
	}
	errsBefore := testutil.ToFloat64(p.metrics.checkErrors)

	code = http.StatusInternalServerError
	p.Check(context.Background())

	if got := testutil.ToFloat64(p.metrics.indicator); got != before {
		t.Errorf("indicator = %v after a failed fetch, want it left stale at %v", got, before)
	}
	if got := testutil.ToFloat64(p.metrics.checkErrors); got != errsBefore+1 {
		t.Errorf("check errors = %v, want %v", got, errsBefore+1)
	}
	// The deadman must NOT advance on a failed poll — that is what makes a
	// wedged poller detectable via staleness.
	if got := testutil.ToFloat64(p.metrics.lastCheckTS); got != 1785362000 {
		t.Errorf("last check ts advanced on a failed poll: %v", got)
	}
}

// A 200 carrying HTML (an unfollowed-redirect landing page, or an error shell)
// parses as JSON-empty. Treating that as a reading would set every gauge to
// healthy-ish garbage, so it must be a hard failure.
func TestClaudeStatusPoller_EmptyIndicatorIsAFailure(t *testing.T) {
	body := `{"page":{"name":"Claude"},"components":[],"incidents":[]}`
	p, _ := newStatusPoller(t, &body, nil)
	errsBefore := testutil.ToFloat64(p.metrics.checkErrors)

	p.Check(context.Background())

	if got := testutil.ToFloat64(p.metrics.checkErrors); got != errsBefore+1 {
		t.Errorf("check errors = %v, want %v (missing indicator must fail)", got, errsBefore+1)
	}
	if v := p.Verdict(); !v.CheckedAt.IsZero() {
		t.Error("a response with no indicator must not produce a verdict")
	}
}

// Unknown components and statuses must not panic, and must not mint new series —
// upstream can add a component at any time.
func TestClaudeStatusPoller_UnknownComponentsAndStatuses(t *testing.T) {
	body := `{
      "status": {"indicator": "minor", "description": "Partial"},
      "components": [
        {"name": "Claude Quantum Mesh", "status": "on_fire"},
        {"name": "Claude API (api.anthropic.com)", "status": "some_new_status"}
      ],
      "incidents": []
    }`
	p, _ := newStatusPoller(t, &body, nil)

	p.Check(context.Background())

	if got := testutil.CollectAndCount(p.metrics.component); got != 1 {
		t.Errorf("component series = %d, want 1 (unknown NAME must not create a series)", got)
	}
	// An unknown STATUS on a known component maps below operational, so it can
	// never trip a `>= partial_outage` alert on its own — an unparseable status
	// is a poller bug, not evidence of an outage.
	got := testutil.ToFloat64(p.metrics.component.WithLabelValues("claude_api"))
	if got != claudeComponentUnknownFallback {
		t.Errorf("unknown status = %v, want %d (below operational)", got, claudeComponentUnknownFallback)
	}
	if v := p.Verdict(); v.Suspect() {
		t.Error("Suspect() = true from an unparseable component status alone")
	}
}

// An unrecognised page INDICATOR fails loud (maps to critical) — the opposite
// choice from component status, because the indicator is the top-level "is
// something wrong" signal and under-reporting it is the dangerous direction.
func TestClaudeIndicatorSeverity_UnknownFailsLoud(t *testing.T) {
	cases := map[string]int{
		"none":         claudeIndicatorNone,
		"minor":        claudeIndicatorMinor,
		"major":        claudeIndicatorMajor,
		"critical":     claudeIndicatorCritical,
		"MINOR":        claudeIndicatorMinor,
		"  major  ":    claudeIndicatorMajor,
		"catastrophic": claudeIndicatorCritical,
		"":             claudeIndicatorCritical,
	}
	for in, want := range cases {
		if got := claudeIndicatorSeverity(in); got != want {
			t.Errorf("claudeIndicatorSeverity(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestClaudeComponentSeverity_Ladder(t *testing.T) {
	cases := map[string]int{
		"operational":          claudeComponentOperational,
		"degraded_performance": claudeComponentDegraded,
		"partial_outage":       claudeComponentPartialOutage,
		"major_outage":         claudeComponentMajorOutage,
		"under_maintenance":    claudeComponentUnderMaintence,
		"OPERATIONAL":          claudeComponentOperational,
		"nonsense":             claudeComponentUnknownFallback,
	}
	for in, want := range cases {
		if got := claudeComponentSeverity(in); got != want {
			t.Errorf("claudeComponentSeverity(%q) = %d, want %d", in, got, want)
		}
	}
	// The ladder must be monotonic — the alert rules compare with `>=`.
	if !(claudeComponentOperational < claudeComponentDegraded &&
		claudeComponentDegraded < claudeComponentPartialOutage &&
		claudeComponentPartialOutage < claudeComponentMajorOutage) {
		t.Error("component severity ladder is not monotonic; `>= 2` rules would be wrong")
	}
}

// Resolved incidents must not count as open, or the incident gauge would never
// return to zero after an outage.
func TestClaudeStatusPoller_ResolvedIncidentsExcluded(t *testing.T) {
	body := `{
      "status": {"indicator": "none", "description": "All Systems Operational"},
      "components": [{"name": "Claude API (api.anthropic.com)", "status": "operational"}],
      "incidents": [
        {"id": "old1", "name": "Fixed", "status": "resolved", "impact": "critical"},
        {"id": "old2", "name": "Written up", "status": "postmortem", "impact": "major"},
        {"id": "live", "name": "Ongoing", "status": "monitoring", "impact": "minor"}
      ]
    }`
	p, _ := newStatusPoller(t, &body, nil)

	p.Check(context.Background())

	if got := testutil.ToFloat64(p.metrics.openIncidents.WithLabelValues("minor")); got != 1 {
		t.Errorf("open minor = %v, want 1", got)
	}
	// All four impact buckets are present (seeded, ka-0000003771); only minor is
	// nonzero because the resolved/postmortem incidents are excluded.
	if got := testutil.CollectAndCount(p.metrics.openIncidents); got != len(claudeImpactLevels) {
		t.Errorf("incident series = %d, want %d (buckets seeded; resolved/postmortem excluded from counts)", got, len(claudeImpactLevels))
	}
	for _, impact := range []string{"none", "major", "critical"} {
		if got := testutil.ToFloat64(p.metrics.openIncidents.WithLabelValues(impact)); got != 0 {
			t.Errorf("open %s = %v, want 0 (resolved/postmortem excluded)", impact, got)
		}
	}
	v := p.Verdict()
	if len(v.Incidents) != 1 || v.Incidents[0].ID != "live" {
		t.Errorf("verdict incidents = %+v, want only the live one", v.Incidents)
	}
	// A minor incident on an operational API is NOT suspect — this is precisely
	// the blip that must not page anyone.
	if v.Suspect() {
		t.Error("Suspect() = true for a minor incident with an operational API")
	}
}

// TestClaudeStatusPoller_RecoveredButIncidentStillOpen is a state observed LIVE
// while building this (2026-07-29 22:2x UTC): the 2h outage had recovered —
// indicator back to `none`, every component `operational` — but incident
// q2kg8n613kr3 was still open at impact=critical with status=monitoring, because
// Statuspage keeps an incident open through the monitoring window after service
// is restored.
//
// This is the tail state that would make a naive leg-B alert stick: keying on
// "any open critical incident" would hold the alert firing long after agents
// recovered. Suspect() reports true here (correct — the provider has not yet
// declared the incident over, and it is genuinely useful context on a leg-A
// alert), which is exactly why leg B is annotation/heads-up only and the PAGE is
// gated on leg A. Pinned so a future change that promotes leg B to a pager has
// to confront this case.
func TestClaudeStatusPoller_RecoveredButIncidentStillOpen(t *testing.T) {
	body := `{
      "status": {"indicator": "none", "description": "All Systems Operational"},
      "components": [
        {"name": "claude.ai", "status": "operational"},
        {"name": "Claude API (api.anthropic.com)", "status": "operational"},
        {"name": "Claude Code", "status": "operational"}
      ],
      "incidents": [
        {"id": "q2kg8n613kr3", "name": "Elevated errors across all models",
         "status": "monitoring", "impact": "critical"}
      ]
    }`
	p, _ := newStatusPoller(t, &body, nil)

	p.Check(context.Background())

	// Components read healthy, so the component-threshold rule does NOT fire.
	if got := testutil.ToFloat64(p.metrics.component.WithLabelValues("claude_api")); got != claudeComponentOperational {
		t.Errorf("claude_api = %v, want operational", got)
	}
	if got := testutil.ToFloat64(p.metrics.indicator); got != claudeIndicatorNone {
		t.Errorf("indicator = %v, want none", got)
	}
	// The incident is still counted while unresolved — that is the provider's
	// own position and belongs in the annotation.
	if got := testutil.ToFloat64(p.metrics.openIncidents.WithLabelValues("critical")); got != 1 {
		t.Errorf("open critical = %v, want 1 (monitoring is not resolved)", got)
	}
	v := p.Verdict()
	if !v.Suspect() {
		t.Error("Suspect() = false with an unresolved critical incident")
	}
	if s := v.Summary(); !strings.Contains(s, "All Systems Operational") || !strings.Contains(s, "q2kg8n613kr3") {
		t.Errorf("Summary() = %q, want it to report BOTH the recovered page and the open incident", s)
	}
}

// Summary() quotes the WORST incident, so the annotation leads with the one the
// oncall cares about.
func TestClaudeStatusVerdict_SummaryQuotesWorstIncident(t *testing.T) {
	body := `{
      "status": {"indicator": "major", "description": "Major Service Outage"},
      "components": [{"name": "Claude API (api.anthropic.com)", "status": "major_outage"}],
      "incidents": [
        {"id": "minor1", "name": "Small thing", "status": "investigating", "impact": "minor"},
        {"id": "crit1", "name": "Big thing", "status": "identified", "impact": "critical"}
      ]
    }`
	p, _ := newStatusPoller(t, &body, nil)
	p.Check(context.Background())

	v := p.Verdict()
	if len(v.Incidents) == 0 || v.Incidents[0].ID != "crit1" {
		t.Fatalf("incidents not sorted worst-first: %+v", v.Incidents)
	}
	if s := v.Summary(); !strings.Contains(s, "crit1") || strings.Contains(s, "minor1") {
		t.Errorf("Summary() = %q, want it to lead with the critical incident", s)
	}
}

// A zero verdict must render as "" so a caller omits the annotation rather than
// presenting "unknown" as a reading. And it must never look healthy.
func TestClaudeStatusVerdict_ZeroValueIsUnknownNotHealthy(t *testing.T) {
	var v ClaudeStatusVerdict
	if v.Summary() != "" {
		t.Errorf("zero Summary() = %q, want empty", v.Summary())
	}
	if v.Suspect() {
		t.Error("zero Suspect() = true; a never-polled verdict must not claim an outage")
	}
	// The poller has published nothing, so the deadman gauge is what tells the
	// operator the detector is blind — asserted by the alert, not here.
	var p *ClaudeStatusPoller
	if got := p.Verdict(); !got.CheckedAt.IsZero() {
		t.Error("nil poller must return a zero verdict")
	}
	p.Check(context.Background()) // must not panic
	if err := p.Run(context.Background()); err != nil {
		t.Errorf("nil poller Run = %v, want nil", err)
	}
}

// Run polls immediately (does not wait for the first tick) and then honours ctx
// cancellation — a detector that waited a full interval before its first read
// would be blind for that window after every controller restart.
func TestClaudeStatusPoller_RunChecksImmediately(t *testing.T) {
	body := healthyFixture
	p, hits := newStatusPoller(t, &body, nil)
	p.interval = time.Hour // guarantee only the immediate check runs

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for {
		if hits.Load() > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run did not poll within 2s — it must check before the first tick")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil on cancellation, want ctx.Err()")
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after cancellation")
	}
}

func TestNewClaudeStatusPoller_Defaults(t *testing.T) {
	p := NewClaudeStatusPoller(ClaudeStatusConfig{})
	if p.url != claudeStatusDefaultURL {
		t.Errorf("url = %q, want the status.claude.com summary endpoint", p.url)
	}
	if !strings.Contains(p.url, "status.claude.com") {
		t.Error("default URL must target status.claude.com (status.anthropic.com now 302s)")
	}
	if !strings.HasSuffix(p.url, "/api/v2/summary.json") {
		t.Error("default URL must be summary.json — status.json lacks components/incidents")
	}
	if p.interval != claudeStatusDefaultInterval {
		t.Errorf("interval = %v, want %v", p.interval, claudeStatusDefaultInterval)
	}
	if p.client == nil || p.client.Timeout != claudeStatusDefaultTimeout {
		t.Errorf("client timeout not defaulted: %+v", p.client)
	}
	if p.logger == nil || p.now == nil {
		t.Error("logger/now not defaulted")
	}
	if p.metrics == nil {
		t.Error("metrics not defaulted; a poller must always have a series set to publish to")
	}
}

// TestClaudeStatusMetrics_RegisterOnServedRegistryNotDefault is the regression
// test for ka-0000008266. The poller's series were built with promauto, which
// registered them on prometheus.DefaultRegisterer at package var-init — BEFORE
// the controller swaps DefaultRegisterer to the registry its :9092 /metrics
// endpoint serves. So the controller's series landed on a registry :9092 never
// served (leg B dark), while the four bridge binaries that merely IMPORT this
// package exported phantom zero series (last_check=0), making
// ClaudeStatusPollerStale fire forever.
//
// The fix moved registration off promauto into ClaudeStatusMetrics.Register, so
// the series appear on a registry ONLY when a caller explicitly registers them.
// This test pins both halves of that contract:
//
//   - the registry the controller serves (the one it passes to Register + runs
//     the poller against) DOES contain gardenio_claude_status_last_check_timestamp_seconds;
//   - a bridge's registry (which imports the package but never registers these
//     metrics) does NOT.
func TestClaudeStatusMetrics_RegisterOnServedRegistryNotDefault(t *testing.T) {
	const deadmanSeries = "gardenio_claude_status_last_check_timestamp_seconds"

	// The controller path: build the metrics, register them on the SAME registry
	// its /metrics endpoint serves, and wire the poller to that instance. A poll
	// then advances the deadman on the served registry.
	controllerReg := prometheus.NewRegistry()
	metrics := NewClaudeStatusMetrics()
	if err := metrics.Register(controllerReg); err != nil {
		t.Fatalf("Register on controller registry: %v", err)
	}
	body := healthyFixture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewClaudeStatusPoller(ClaudeStatusConfig{
		URL:     srv.URL + "/api/v2/summary.json",
		Logger:  discardStatusLogger(),
		Now:     func() time.Time { return time.Unix(1785362000, 0) },
		Metrics: metrics,
	})
	p.Check(context.Background())

	if got := testutil.CollectAndCount(metrics.lastCheckTS); got != 1 {
		t.Fatalf("deadman gauge not collectable after a poll: got %d series, want 1", got)
	}
	if n, err := testutil.GatherAndCount(controllerReg, deadmanSeries); err != nil {
		t.Fatalf("gather controller registry: %v", err)
	} else if n != 1 {
		t.Errorf("controller-served registry has %d %s series, want 1 — the series the controller serves on :9092 MUST carry the poller's deadman (ka-0000008266)", n, deadmanSeries)
	}

	// The bridge path: a bridge binary imports this package (so the metric
	// CONSTRUCTORS run) but never calls Register. Its served registry — modeled
	// here by a fresh registry that the bridge would hand to promhttp — must
	// therefore carry NONE of the leg-B series. Under the old promauto code the
	// var-init side effect put them on DefaultRegisterer, which each bridge DOES
	// serve, exactly the phantom-zero-series bug.
	bridgeReg := prometheus.NewRegistry()
	if n, err := testutil.GatherAndCount(bridgeReg, deadmanSeries); err != nil {
		t.Fatalf("gather bridge registry: %v", err)
	} else if n != 0 {
		t.Errorf("a bridge's registry exports %d %s series, want 0 — a bare import must not register leg-B metrics or the bridges emit phantom zeros and ClaudeStatusPollerStale fires forever (ka-0000008266)", n, deadmanSeries)
	}

	// And the strongest form of the bridge guarantee: merely importing the
	// package (which this test does) must not have registered these series on
	// the global default registry either — that global is what the bridges
	// serve. Assert the default registry is clean of the deadman series.
	if n, err := testutil.GatherAndCount(prometheus.DefaultGatherer, deadmanSeries); err != nil {
		t.Fatalf("gather default registry: %v", err)
	} else if n != 0 {
		t.Errorf("the global default registry exports %d %s series, want 0 — importing the bridge package must have NO registration side effect (the promauto var-init bug, ka-0000008266)", n, deadmanSeries)
	}
}

// --- Fallback + failure-classification coverage (ka-0000007040) ------------
//
// Leg B has now gone dark twice (ka-0000008266 registry init-order; this bug, a
// provider custom-domain TLS cert with no SAN for status.claude.com). The
// hardening: an ordered fallback-URL list so a single dead endpoint cannot
// blind the detector, plus a {reason} breakout on the failure counter so the
// deadman routes the oncall to "provider cert" instead of "our pod egress".

// newStatusPollerURLs wires a poller to an explicit primary URL and fallback
// list with the fixed test clock, so a test can point each at its own server.
func newStatusPollerURLs(t *testing.T, primary string, fallbacks []string) *ClaudeStatusPoller {
	t.Helper()
	return NewClaudeStatusPoller(ClaudeStatusConfig{
		URL:          primary,
		FallbackURLs: fallbacks,
		Logger:       discardStatusLogger(),
		Now:          func() time.Time { return time.Unix(1785362000, 0) },
	})
}

// jsonStatusServer serves the healthy fixture at /api/v2/summary.json and counts
// hits, so a test can prove a fallback was (or was not) reached.
func jsonStatusServer(t *testing.T, body string) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/v2/summary.json", &hits
}

// deadStatusServer serves a non-200 at every path and counts hits.
func deadStatusServer(t *testing.T, code int) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/v2/summary.json", &hits
}

// When the primary fetch fails, the poll must fall through to the fallback and
// produce a valid reading from it — the whole point of the hardening.
func TestClaudeStatusPoller_FallbackServesWhenPrimaryDead(t *testing.T) {
	primary, primHits := deadStatusServer(t, http.StatusServiceUnavailable)
	fallback, fbHits := jsonStatusServer(t, healthyFixture)
	p := newStatusPollerURLs(t, primary, []string{fallback})

	p.Check(context.Background())

	if primHits.Load() == 0 {
		t.Error("primary was never tried")
	}
	if fbHits.Load() == 0 {
		t.Error("fallback was never reached after the primary failed")
	}
	// A healthy reading from the fallback advances the deadman and produces a
	// verdict — leg B is NOT blind.
	if got := testutil.ToFloat64(p.metrics.lastCheckTS); got != 1785362000 {
		t.Errorf("last check ts = %v, want the fixed clock (fallback should have produced a reading)", got)
	}
	if got := testutil.ToFloat64(p.metrics.checkErrors); got != 0 {
		t.Errorf("check errors = %v, want 0 (a fallback succeeded, so the poll succeeded)", got)
	}
	if v := p.Verdict(); v.CheckedAt.IsZero() {
		t.Error("no verdict produced despite a healthy fallback reading")
	}
}

// The fallback must be tried ONLY when the primary fails — a healthy primary
// must never touch the fallback (avoids doubling load on Anthropic's infra).
func TestClaudeStatusPoller_FallbackNotTriedWhenPrimaryHealthy(t *testing.T) {
	primary, primHits := jsonStatusServer(t, healthyFixture)
	fallback, fbHits := jsonStatusServer(t, healthyFixture)
	p := newStatusPollerURLs(t, primary, []string{fallback})

	p.Check(context.Background())

	if primHits.Load() == 0 {
		t.Error("primary was never tried")
	}
	if fbHits.Load() != 0 {
		t.Errorf("fallback was hit %d times despite a healthy primary, want 0", fbHits.Load())
	}
}

// When BOTH primary and every fallback fail, the poll fails: the deadman stays
// stale and the error counter increments exactly once — the failure the deadman
// alert is meant to catch.
func TestClaudeStatusPoller_AllEndpointsDeadFailsClosed(t *testing.T) {
	primary, primHits := deadStatusServer(t, http.StatusBadGateway)
	fallback, fbHits := deadStatusServer(t, http.StatusServiceUnavailable)
	p := newStatusPollerURLs(t, primary, []string{fallback})

	p.Check(context.Background())

	if primHits.Load() == 0 || fbHits.Load() == 0 {
		t.Errorf("expected both endpoints tried, primary=%d fallback=%d", primHits.Load(), fbHits.Load())
	}
	if got := testutil.ToFloat64(p.metrics.checkErrors); got != 1 {
		t.Errorf("check errors = %v, want 1 (all endpoints dead = one failed poll)", got)
	}
	if got := testutil.ToFloat64(p.metrics.lastCheckTS); got != 0 {
		t.Errorf("last check ts = %v, want 0 (no endpoint produced a reading)", got)
	}
	// The reason must reflect the PRIMARY failure (http_status here), not the
	// last fallback tried — the deadman describes the endpoint we are pinned to.
	if got := testutil.ToFloat64(p.metrics.checkErrorsReason.WithLabelValues("http_status")); got != 1 {
		t.Errorf("reason http_status = %v, want 1", got)
	}
}

// classifyClaudeStatusError is the routing brain of the deadman annotation: the
// motivating failure (a provider custom-domain cert with no SAN) must read as
// `tls`, never as `connect` — sending the oncall to chase pod egress on a
// provider cert bug is the exact mis-route this fixes.
func TestClassifyClaudeStatusError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"build_request", fmt.Errorf("%w: bad url", errClaudeStatusBuildRequest), "build_request"},
		{"http_status", fmt.Errorf("get x: %w %d", errClaudeStatusHTTPStatus, 503), "http_status"},
		{"read_body", fmt.Errorf("%w: eof", errClaudeStatusReadBody), "read_body"},
		{"parse", fmt.Errorf("%w: invalid character", errClaudeStatusParse), "parse"},
		{"empty", fmt.Errorf("%w: no indicator", errClaudeStatusEmpty), "empty"},
		{
			"tls_cert_verification",
			fmt.Errorf("get x: %w", &tls.CertificateVerificationError{Err: x509.HostnameError{Host: "status.claude.com"}}),
			"tls",
		},
		{
			"tls_hostname_mismatch",
			fmt.Errorf("get x: %w", x509.HostnameError{Host: "status.claude.com"}),
			"tls",
		},
		{
			"tls_unknown_authority",
			fmt.Errorf("get x: %w", x509.UnknownAuthorityError{}),
			"tls",
		},
		{
			"dns",
			fmt.Errorf("get x: %w", &net.DNSError{Err: "no such host", Name: "status.claude.com", IsNotFound: true}),
			"dns",
		},
		{
			"connect_opError",
			fmt.Errorf("get x: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}),
			"connect",
		},
		{"connect_deadline", fmt.Errorf("get x: %w", context.DeadlineExceeded), "connect"},
		{"other", errors.New("something unclassifiable"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyClaudeStatusError(tc.err); got != tc.want {
				t.Errorf("classifyClaudeStatusError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// End-to-end: a TLS-verification failure on a real https handshake must be
// counted under reason=tls on the by-reason series — the whole reason the
// classifier exists. Uses an httptest TLS server whose cert the poller's client
// does NOT trust, reproducing the x509 verification failure shape of the bug.
func TestClaudeStatusPoller_TLSFailureCountedAsReasonTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(healthyFixture))
	}))
	t.Cleanup(srv.Close)

	// A default client (no srv.Client()) does not trust the test server's cert,
	// so the handshake fails x509 verification — the same class of failure as the
	// provider's *.statuspage.io cert with no SAN for status.claude.com.
	p := NewClaudeStatusPoller(ClaudeStatusConfig{
		URL:    srv.URL + "/api/v2/summary.json",
		Client: &http.Client{Timeout: claudeStatusDefaultTimeout},
		Logger: discardStatusLogger(),
		Now:    func() time.Time { return time.Unix(1785362000, 0) },
	})

	p.Check(context.Background())

	if got := testutil.ToFloat64(p.metrics.checkErrors); got != 1 {
		t.Fatalf("check errors = %v, want 1 (TLS failure is a failed poll)", got)
	}
	if got := testutil.ToFloat64(p.metrics.checkErrorsReason.WithLabelValues("tls")); got != 1 {
		t.Errorf("reason tls = %v, want 1 — a cert failure must route to `tls`, not pod egress", got)
	}
	if got := testutil.ToFloat64(p.metrics.lastCheckTS); got != 0 {
		t.Errorf("last check ts = %v, want 0 (a TLS failure produces no reading)", got)
	}
}
