package bridge

// End-to-end legs for the slack-bridge cold-respawn PHASE-BREAKDOWN metrics
// (source: respawn_phase_metrics.go; feature merge 8edcbe2e1 / task
// ka-0000001457). Wrapped into the e2e gate by
// gardenio/tests/e2e/scripts/test-slackbridge-respawn-phase-metrics.sh — a
// `go test -run` module in the blessed hermetic-probe style (backfill bead
// ka-0000003422, epic ka-0000000958).
//
// ── Why a from-source go-test module, NOT a live-bridge /metrics port-forward ──
//
// The task's original feasibility note proposed scraping BRIDGE_HTTP_URL/metrics
// (run-shard.sh's 19091->8090 tunnel). That is not reachable, for two reasons
// both verified against the live gardenio-system bridge at authoring time:
//
//   1. WRONG PORT. run-shard.sh forwards 19091 -> the bridge's :8090 escalation
//      API + /healthz mux. /metrics is served by a SEPARATE http.Server on :9093
//      (cmd/slack-bridge/main.go:312, resolveMetricsAddr default ":9093"). Proven:
//      :8090/metrics => HTTP 404; :9093/metrics => 200 with gardenio_bridge_*. No
//      shard forwards :9093 for the bridge (run-shard.sh:2499 documents that the
//      only :9093 in the suite is coop's AGENT-pod metrics port, not the bridge).
//
//   2. EMPTY UNTIL A REAL RESPAWN. respawnPhaseSeconds/respawnPhaseSlowTotal are a
//      promauto HistogramVec/CounterVec with NO zero-initialization. A *Vec with
//      zero child series marshals NOTHING to /metrics — no HELP/TYPE, no samples —
//      until the first Observe/Inc. The only producer is a real cold thread/DM
//      respawn completing (agentstream.go:1211), which no e2e module drives
//      (test-thread-respawn-resume.sh asserts bead-level revive, never scrapes
//      /metrics). So on any freshly-booted bridge the series are simply absent.
//
// This is the identical constraint that made test-gitlab-webhook-deadman.sh /
// test-session-spend-attribution.sh wrap a targeted `go test -run` against the
// REAL production surface instead of scraping a live pod. We do the same: drive
// the REAL recordRespawnPhase (the exact call agentstream.go makes) and scrape a
// REAL promhttp endpoint over the DEFAULT gatherer — the same
// promhttp.HandlerFor(prometheus.DefaultGatherer) buildMetricsServer mounts
// (cmd/slack-bridge/helpers.go:341) — then parse the served TEXT exposition, so
// the legs prove the series reach the wire in the format Prometheus scrapes.
//
// ── What these legs prove OVER the in-process unit test (respawn_phase_metrics_test.go) ──
//
// The unit test drives recordRespawnPhase and reads children with
// testutil.CollectAndCount / testutil.ToFloat64 — it proves the RECORD LOGIC. It
// reads the in-process registry directly and stays green even if the series never
// marshal to the scrape format. These legs add the four things an operator's page
// actually depends on end-to-end:
//
//   Leg exposure   — the two series are ABSENT from the served exposition before
//                    any observation (the anti-vacuity control — a "present"
//                    assertion is meaningless without a proven "absent" on the
//                    SAME endpoint; crew-e2e advice ka-0000001521) and PRESENT,
//                    with HELP/TYPE, after one real observation per phase. This is
//                    the registry-regression guard the task asks for: it fails if
//                    the respawn_phase series ever stop reaching the default
//                    gatherer's wire format. (That buildMetricsServer serves the
//                    default gatherer — not a bespoke private registry that would
//                    expose none of the bridge series — is itself guarded by
//                    cmd/slack-bridge TestBuildMetricsServer.)
//   Leg bounding   — every served gardenio_slackbridge_respawn_phase_seconds sample
//                    carries a phase label in {dispatch,boot,total}, an empty role
//                    collapses to role="unknown", and an out-of-enum phase driven
//                    through the real entry point NEVER appears on the wire. This
//                    is the cardinality guard.
//   Leg slow       — an under-threshold phase does NOT bump the served
//                    gardenio_slackbridge_respawn_phase_slow_total; an over-
//                    threshold phase bumps it by exactly 1 and the counter is
//                    monotonic — asserted BY VALUE on the served wire.
//   Leg name-match — the metric names on the served endpoint are exactly the
//                    strings the SHIPPED vmalert rule (SlackBridgeRespawnPhaseSlow,
//                    helm/.../vmalert/configmap.yaml) keys off. A one-sided rename
//                    of either the Go metric name OR the template expr silently
//                    un-fires the page while the unit test AND the helm rule test
//                    both stay green — the exact unfireable-alert failure. This is
//                    the "their vmalert page path" half of the task title.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The contract strings. These are the two series names an operator's dashboard
// and page depend on; the legs below assert them against the served wire and the
// shipped alert rule so a rename on either side reddens.
const (
	respawnPhaseSecondsSeries = "gardenio_slackbridge_respawn_phase_seconds"
	respawnPhaseSlowSeries    = "gardenio_slackbridge_respawn_phase_slow_total"
)

// servedRespawnPhaseMetrics scrapes a REAL promhttp endpoint backed by the
// DEFAULT gatherer — the exact promhttp.HandlerFor(prometheus.DefaultGatherer)
// buildMetricsServer mounts on the bridge's :9093 (cmd/slack-bridge) — and
// returns the raw Prometheus text exposition. Parsing the served text (rather
// than reg.Gather()/testutil) is the point of the e2e: it proves the series reach
// the wire in the format Prometheus scrapes.
func servedRespawnPhaseMetrics(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	return string(body)
}

// sampleLinesFor returns the non-comment sample lines whose series name is
// metric or metric with a Prometheus suffix (_bucket/_sum/_count for a
// histogram, "" for a counter). It rejects a metric whose name is a strict
// prefix of a longer, unrelated family by requiring the next char to be '{', ' ',
// or '_' (a suffix separator).
func sampleLinesFor(text, metric string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rest, ok := strings.CutPrefix(line, metric)
		if !ok {
			continue
		}
		if rest == "" {
			continue
		}
		switch rest[0] {
		case '{', ' ', '_':
			out = append(out, line)
		}
	}
	return out
}

// respawnPhaseLabelValue extracts the value of label key from a single sample
// line of the form `name{k1="v1",k2="v2"} value`. Returns ("", false) when the
// label is absent.
func respawnPhaseLabelValue(line, key string) (string, bool) {
	open := strings.IndexByte(line, '{')
	shut := strings.LastIndexByte(line, '}')
	if open < 0 || shut < open {
		return "", false
	}
	labels := line[open+1 : shut]
	_, rest, ok := strings.Cut(labels, key+"=\"")
	if !ok {
		return "", false
	}
	val, _, ok := strings.Cut(rest, "\"")
	if !ok {
		return "", false
	}
	return val, true
}

// servedCounterValueFor returns the value of a single labelled counter sample
// `name{...} value` whose label set contains want (e.g. `phase="boot"`).
// found=false when no such line exists (an unbumped / never-marshalled child).
func servedCounterValueFor(text, metric, wantLabel string) (float64, bool) {
	for _, line := range sampleLinesFor(text, metric) {
		if !strings.Contains(line, wantLabel) {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// TestSlackBridgeRespawnPhaseE2E_ExposureAndAntiVacuity proves the two series
// reach the served exposition — with the load-bearing anti-vacuity control that
// the SAME endpoint serves NEITHER series before any observation. This is the
// registry-regression guard: it fails if the respawn_phase families ever stop
// marshalling to the default gatherer's wire format.
func TestSlackBridgeRespawnPhaseE2E_ExposureAndAntiVacuity(t *testing.T) {
	respawnPhaseSeconds.Reset()
	respawnPhaseSlowTotal.Reset()

	// Anti-vacuity: a zero-child *Vec marshals nothing, so both series must be
	// absent from the served text. A "present" assertion after this is only
	// meaningful because we proved "absent" on the same endpoint first.
	before := servedRespawnPhaseMetrics(t)
	if strings.Contains(before, respawnPhaseSecondsSeries) {
		t.Errorf("anti-vacuity: %s present on the served exposition BEFORE any observation — the presence assertion would be vacuous", respawnPhaseSecondsSeries)
	}
	if strings.Contains(before, respawnPhaseSlowSeries) {
		t.Errorf("anti-vacuity: %s present on the served exposition BEFORE any observation — the presence assertion would be vacuous", respawnPhaseSlowSeries)
	}

	// Drive the REAL recording entry point once per phase; the over-threshold
	// total also creates the slow counter's first child so BOTH families marshal.
	recordRespawnPhase("crew", respawnPhaseDispatch, 1)
	recordRespawnPhase("crew", respawnPhaseBoot, 40)
	recordRespawnPhase("crew", respawnPhaseTotal, 220) // > 180s total threshold

	after := servedRespawnPhaseMetrics(t)

	// Histogram family: HELP + TYPE + samples must be on the wire.
	if !strings.Contains(after, "# HELP "+respawnPhaseSecondsSeries) {
		t.Errorf("%s HELP line absent from served exposition after observation — histogram did not reach the wire", respawnPhaseSecondsSeries)
	}
	if !strings.Contains(after, "# TYPE "+respawnPhaseSecondsSeries+" histogram") {
		t.Errorf("%s TYPE line absent/not a histogram on served exposition", respawnPhaseSecondsSeries)
	}
	if len(sampleLinesFor(after, respawnPhaseSecondsSeries)) == 0 {
		t.Errorf("%s emitted no sample lines on served exposition", respawnPhaseSecondsSeries)
	}

	// Counter family: HELP + TYPE + at least one sample.
	if !strings.Contains(after, "# HELP "+respawnPhaseSlowSeries) {
		t.Errorf("%s HELP line absent from served exposition after over-threshold observation", respawnPhaseSlowSeries)
	}
	if !strings.Contains(after, "# TYPE "+respawnPhaseSlowSeries+" counter") {
		t.Errorf("%s TYPE line absent/not a counter on served exposition", respawnPhaseSlowSeries)
	}
	if len(sampleLinesFor(after, respawnPhaseSlowSeries)) == 0 {
		t.Errorf("%s emitted no sample lines on served exposition", respawnPhaseSlowSeries)
	}
}

// TestSlackBridgeRespawnPhaseE2E_LabelBounding proves the served histogram is
// cardinality-bounded: every sample carries a phase in the enum, an empty role
// collapses to "unknown", and an out-of-enum phase driven through the REAL entry
// point never reaches the wire.
func TestSlackBridgeRespawnPhaseE2E_LabelBounding(t *testing.T) {
	respawnPhaseSeconds.Reset()
	respawnPhaseSlowTotal.Reset()

	recordRespawnPhase("crew", respawnPhaseDispatch, 1)
	recordRespawnPhase("crew", respawnPhaseBoot, 40)
	recordRespawnPhase("crew", respawnPhaseTotal, 41)
	recordRespawnPhase("", respawnPhaseDispatch, 2) // empty role -> "unknown"
	// Out-of-enum phase: recordRespawnPhase drops it, so it must NOT create a
	// child. Driving the real entry point (not WithLabelValues) is the point —
	// this is what agentstream.go calls.
	if recordRespawnPhase("crew", "init", 1) {
		t.Fatal(`recordRespawnPhase("crew","init",1) returned true — out-of-enum phase was not dropped`)
	}

	served := servedRespawnPhaseMetrics(t)
	lines := sampleLinesFor(served, respawnPhaseSecondsSeries)
	if len(lines) == 0 {
		t.Fatalf("no %s samples on served exposition", respawnPhaseSecondsSeries)
	}

	validPhase := map[string]bool{
		respawnPhaseDispatch: true,
		respawnPhaseBoot:     true,
		respawnPhaseTotal:    true,
	}
	sawUnknownRole := false
	for _, line := range lines {
		phase, ok := respawnPhaseLabelValue(line, "phase")
		if !ok {
			t.Errorf("served %s sample carries no phase label: %s", respawnPhaseSecondsSeries, line)
			continue
		}
		if !validPhase[phase] {
			t.Errorf("served %s sample carries out-of-enum phase %q (cardinality unbounded): %s", respawnPhaseSecondsSeries, phase, line)
		}
		role, ok := respawnPhaseLabelValue(line, "role")
		if !ok || role == "" {
			t.Errorf("served %s sample carries empty/absent role label: %s", respawnPhaseSecondsSeries, line)
		}
		if role == "unknown" {
			sawUnknownRole = true
		}
	}
	if !sawUnknownRole {
		t.Errorf("empty role did not collapse to role=\"unknown\" on the served exposition")
	}
	// The dropped out-of-enum phase must be nowhere on the wire.
	if strings.Contains(served, `phase="init"`) {
		t.Errorf(`out-of-enum phase="init" reached the served exposition — recordRespawnPhase did not bound the series`)
	}
}

// TestSlackBridgeRespawnPhaseE2E_SlowCounterSemantics proves the slow counter's
// served value: an under-threshold phase does not bump it; an over-threshold
// phase bumps it by exactly 1 and it is monotonic.
func TestSlackBridgeRespawnPhaseE2E_SlowCounterSemantics(t *testing.T) {
	respawnPhaseSeconds.Reset()
	respawnPhaseSlowTotal.Reset()

	// A healthy boot (40s < 150s threshold) must not create/bump the boot slow
	// child — an unbumped child does not marshal, so it is absent from the wire.
	recordRespawnPhase("crew", respawnPhaseBoot, 40)
	served := servedRespawnPhaseMetrics(t)
	if _, found := servedCounterValueFor(served, respawnPhaseSlowSeries, `phase="boot"`); found {
		t.Errorf("under-threshold boot bumped %s{phase=\"boot\"} — a healthy respawn must not page", respawnPhaseSlowSeries)
	}

	// An anomalous boot (200s > 150s) bumps the boot slow child to exactly 1.
	recordRespawnPhase("crew", respawnPhaseBoot, 200)
	served = servedRespawnPhaseMetrics(t)
	v, found := servedCounterValueFor(served, respawnPhaseSlowSeries, `phase="boot"`)
	if !found {
		t.Fatalf("%s{phase=\"boot\"} absent from served exposition after over-threshold boot", respawnPhaseSlowSeries)
	}
	if v != 1 {
		t.Errorf("%s{phase=\"boot\"} = %v on the wire, want 1", respawnPhaseSlowSeries, v)
	}

	// Monotonic: a second anomalous boot advances it to 2.
	recordRespawnPhase("crew", respawnPhaseBoot, 300)
	served = servedRespawnPhaseMetrics(t)
	v, found = servedCounterValueFor(served, respawnPhaseSlowSeries, `phase="boot"`)
	if !found {
		t.Fatalf("%s{phase=\"boot\"} absent after second over-threshold boot", respawnPhaseSlowSeries)
	}
	if v != 2 {
		t.Errorf("%s{phase=\"boot\"} = %v on the wire after two over-threshold boots, want 2 (monotonic)", respawnPhaseSlowSeries, v)
	}
}

// TestSlackBridgeRespawnPhaseE2E_ServedNamesMatchShippedAlertExpr couples the
// series names on the served endpoint to the strings the SHIPPED vmalert rule
// keys off, so a one-sided rename that would silently un-fire the page reddens.
func TestSlackBridgeRespawnPhaseE2E_ServedNamesMatchShippedAlertExpr(t *testing.T) {
	respawnPhaseSeconds.Reset()
	respawnPhaseSlowTotal.Reset()
	// Ensure both families marshal so the served-name half is a real assertion.
	recordRespawnPhase("crew", respawnPhaseTotal, 220) // over threshold: both series
	served := servedRespawnPhaseMetrics(t)

	if !strings.Contains(served, respawnPhaseSlowSeries) {
		t.Errorf("%s absent from the served exposition — the alert expr would key off a series the bridge never emits", respawnPhaseSlowSeries)
	}
	if !strings.Contains(served, respawnPhaseSecondsSeries) {
		t.Errorf("%s absent from the served exposition — the alert's triage graph would reference a series the bridge never emits", respawnPhaseSecondsSeries)
	}

	// Read the SHIPPED vmalert template's raw text (not a helm render, so no helm
	// binary is needed). The metric-name literals in the expr/annotation are
	// OUTSIDE any {{ }} action, so the raw template contains them verbatim.
	//
	// CROSS-TREE READ, GUARDED (mirrors gitlab_webhook_deadman_e2e_test.go). The
	// template lives in a SIBLING tree (helm/), so a filtered CI layer that
	// materializes only gardenio/controller/** would leave it absent. We SKIP
	// LOUDLY when the file is missing rather than fail — the coupling is a HARD
	// gate in the layers that carry the helm tree (ci.yml/coverage.yml test-gardenio
	// and the e2e module, which run against the full worktree); the skip is only a
	// safety net for a layer that legitimately does not carry helm/, never a way to
	// pass a real rename. The served-name half above still hard-asserts everywhere.
	tmplPath := filepath.Join("..", "..", "..", "helm", "gardenio", "templates", "vmalert", "configmap.yaml")
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("SKIP (LOUD): shipped vmalert template not materialized in this layer (%s) — the served-name↔alert-expr coupling is hard-gated in ci.yml/coverage.yml test-gardenio and the e2e module, which carry the helm tree; this layer only omits it. NOT a pass for a rename.", tmplPath)
		}
		t.Fatalf("read shipped vmalert template %s: %v", tmplPath, err)
	}
	tmpl := string(raw)

	// LOAD-BEARING: the paging rule's expr fires on
	// increase(gardenio_slackbridge_respawn_phase_slow_total[...]). If the Go name
	// or the template expr is renamed in isolation, the alert stops firing.
	if !strings.Contains(tmpl, "increase("+respawnPhaseSlowSeries+"[") {
		t.Errorf("shipped vmalert expr does not key off the emitted slow-counter series %q — a one-sided rename has silently un-fired SlackBridgeRespawnPhaseSlow", respawnPhaseSlowSeries)
	}
	// The histogram is the rule's documented triage surface (annotation: "graph
	// gardenio_slackbridge_respawn_phase_seconds by phase"). Weaker than the expr
	// coupling, but the task calls out the histogram as the operator's page-path
	// triage series, so a rename that orphans the triage pointer should redden.
	if !strings.Contains(tmpl, respawnPhaseSecondsSeries) {
		t.Errorf("shipped vmalert rule does not reference the emitted histogram series %q — the alert's triage pointer is orphaned", respawnPhaseSecondsSeries)
	}
	// The alert this coupling protects must actually exist in the shipped rule.
	if !strings.Contains(tmpl, "SlackBridgeRespawnPhaseSlow") {
		t.Errorf("shipped vmalert template no longer defines the SlackBridgeRespawnPhaseSlow alert — the respawn-phase page path is gone")
	}
}
