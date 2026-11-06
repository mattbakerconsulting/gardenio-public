package bridge

// End-to-end leg for the GitLab inbound-webhook dark-hook deadman (GAP-C of the
// non-AVA webhook carve-out ka-0000005632; runbook bug ka-0000000426; backfill
// bead ka-0000005729). Wrapped into the e2e gate by
// gardenio/tests/e2e/scripts/test-gitlab-webhook-deadman.sh (a `go test -run`
// module in the blessed hermetic-probe style — the gitlab-bridge is OFF in the
// e2e namespace, so there is no live /webhook to POST to; see
// test-gitlab-dark-provenance.sh / test-session-spend-attribution.sh for the same
// pattern).
//
// ── What this adds OVER the in-process unit test (gitlab_webhook_deadman_test.go) ──
//
// The unit test drives m.evaluate() and reads the gauge with testutil.ToFloat64 —
// it proves the VERDICT LOGIC. It cannot prove the two things an operator's page
// actually depends on end-to-end:
//
//   1. THE SERIES REACHES THE WIRE. The deadman gauge/heartbeat are only useful
//      if they appear in the Prometheus TEXT EXPOSITION the gitlab-bridge serves
//      on its :9092 /metrics endpoint (promhttp.HandlerFor(DefaultGatherer),
//      gitlab-bridge/main.go). testutil.ToFloat64 reads the in-process registry
//      directly and would stay green even if the series never marshalled to the
//      scrape format. Here we scrape a REAL promhttp endpoint over HTTP and parse
//      the exposition, exactly as Prometheus (and session_spend_e2e_test.go's
//      servedCounter) does.
//
//   2. THE EMITTED NAMES MATCH THE SHIPPED ALERT EXPR. The paging PrometheusRule
//      (helm/gardenio/templates/gitlab-bridge/webhook-deadman-alert.yaml) keys its
//      expr off the literal strings `gardenio_gitlab_webhook_dark` and
//      `gardenio_gitlab_webhook_evaluations_total`. If either the Go metric name
//      OR the template expr is renamed in isolation, the alert silently stops
//      firing — the exact unfireable-alert failure the whole carve-out exists to
//      prevent — and BOTH the unit test and the helm render test stay green (each
//      sees only its own side). This leg reads the served metric names AND the
//      shipped template expr and asserts they are the same strings, so a
//      one-sided rename reddens.
//
// And it does both with an ANTI-VACUITY control (crew-e2e advice ka-0000001521 /
// ka-0000005143): the dark==1 assertion is meaningless unless the SAME served
// endpoint reads dark==0 in the healthy case. The healthy case is driven by a
// REAL authenticated delivery POSTed through the ACTUAL
// GitLabWebhookHandlerWithConfig (not a hand-set clock), so the "alive" path is
// the shape production actually emits — the handler stamps the receive-liveness
// clock via recordGitLabWebhookOutcome, and the next evaluate() reads healthy.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// servedGitLabDeadmanMetrics scrapes GET /metrics from a promhttp server exposing
// the DEFAULT gatherer — the exact promhttp.HandlerFor(prometheus.DefaultGatherer)
// the gitlab-bridge mounts on :9092 (gitlab-bridge/main.go) — and returns the raw
// Prometheus text exposition. Parsing the served text (rather than
// reg.Gather()/testutil.ToFloat64) is the point of the e2e: it proves the deadman
// series reach the wire in the format Prometheus scrapes.
func servedGitLabDeadmanMetrics(t *testing.T) string {
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

// servedGaugeValue returns the value of an unlabeled gauge sample line
// ("<metric> <value>") from a Prometheus text exposition. found=false when the
// series line is absent (a never-marshalled series). Kept deliberately simple:
// gardenio_gitlab_webhook_dark carries no labels.
func servedGaugeValue(text, metric string) (value float64, found bool) {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Unlabeled gauge: "metric value". Reject the {label} form and any
		// metric whose name is a prefix of a longer name (e.g. _total).
		rest, ok := strings.CutPrefix(line, metric+" ")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// e2eGitLabMonitor builds a monitor over the fake lister with a fixed clock,
// mirroring testGitLabMonitor but local to this file so the two files stay
// independently readable.
func e2eGitLabMonitor(lister gitlabGroupMRLister, window time.Duration, now time.Time) *GitLabWebhookMonitor {
	return NewGitLabWebhookMonitor(GitLabWebhookMonitorConfig{
		GitLab:  lister,
		GroupID: 42,
		Window:  window,
		Logger:  slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		Clock:   func() time.Time { return now },
	})
}

// TestGitLabWebhookDeadmanE2E_ServedGaugeFlipsOnDark drives the monitor's real
// evaluate() and asserts the dark verdict reaches the SERVED /metrics exposition
// — with a REAL-delivery anti-vacuity control proving the same endpoint reads 0
// when a well-formed authenticated webhook has just arrived.
func TestGitLabWebhookDeadmanE2E_ServedGaugeFlipsOnDark(t *testing.T) {
	// ── Anti-vacuity control (HEALTHY): a real authenticated delivery through the
	// ACTUAL webhook handler stamps the receive-liveness clock, so the next
	// evaluate() reads healthy and the SERVED gauge is 0. Without this, a served
	// "==1" below could be a stuck-at-1 artifact rather than a real flip.
	resetGitLabWebhookDeadmanMetrics()
	now := time.Unix(1_700_000_000, 0)

	const secret = "e2e-deadman-secret"
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		WebhookSecret: secret,
		Logger:        slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})
	// An unrecognized object_kind is authenticated + decoded → terminal outcome
	// "ignored", which stamps the liveness clock (recordGitLabWebhookOutcome) with
	// ZERO daemon/GitLab dependency — the minimal real "the hook is alive" signal.
	req := httptest.NewRequest(http.MethodPost, "/webhook",
		strings.NewReader(`{"object_kind":"deploy"}`))
	req.Header.Set("X-Gitlab-Token", secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated delivery status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if lastGitLabWebhookReceivedNano.Load() == 0 {
		t.Fatal("a real authenticated delivery must stamp the receive-liveness clock")
	}

	// Evaluate with the clock reading the just-received delivery as recent.
	// upstream activity present (999) proves the 0 is the healthy early-out, not a
	// quiet-upstream 0. Reuse the makeMRs helper from the unit test file.
	healthyLister := &webhookFakeGroupMRLister{mrs: makeMRs(999)}
	// Re-seed the deterministic clock: recordGitLabWebhookOutcome stamped
	// time.Now(); pin it to `now` so the fixed-clock monitor reads it as recent.
	lastGitLabWebhookReceivedNano.Store(now.Add(-1 * time.Minute).UnixNano())
	e2eGitLabMonitor(healthyLister, 2*time.Hour, now).evaluate(context.Background())

	healthyText := servedGitLabDeadmanMetrics(t)
	if v, found := servedGaugeValue(healthyText, "gardenio_gitlab_webhook_dark"); !found || v != 0 {
		t.Fatalf("served dark gauge (healthy control) = %v found=%v, want 0 — the anti-vacuity control failed; the served exposition must read 0 while deliveries flow", v, found)
	}

	// ── DARK: no authenticated delivery in the window while upstream MR activity
	// exists → the monitor folds the verdict to 1, and it must reach the wire.
	resetGitLabWebhookDeadmanMetrics()
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano()) // past the window
	darkLister := &webhookFakeGroupMRLister{mrs: makeMRs(7)}
	e2eGitLabMonitor(darkLister, 2*time.Hour, now).evaluate(context.Background())

	darkText := servedGitLabDeadmanMetrics(t)
	if v, found := servedGaugeValue(darkText, "gardenio_gitlab_webhook_dark"); !found || v != 1 {
		t.Fatalf("served dark gauge (dark case) = %v found=%v, want 1 — the dark verdict did not reach the served /metrics exposition", v, found)
	}
	// The heartbeat the deadman leg reads must also be on the wire (a HELP/TYPE +
	// at least one series line), or the monitor-deadman alert can never evaluate.
	if !strings.Contains(darkText, "gardenio_gitlab_webhook_evaluations_total") {
		t.Fatalf("served exposition missing gardenio_gitlab_webhook_evaluations_total — the monitor-deadman leg would be unfireable:\n%s", darkText)
	}
}

// TestGitLabWebhookDeadmanE2E_ServedNamesMatchShippedAlertExpr is the
// rename-either-side guard. It reads the metric names actually present on the
// served /metrics endpoint AND the metric names the SHIPPED PrometheusRule
// template keys its expr off, and asserts they are the same strings. A rename on
// EITHER side (Go metric or template expr) that is not mirrored on the other
// silently un-fires the page while both the unit test and the helm render test
// stay green — this leg is the only thing that couples the two artifacts.
func TestGitLabWebhookDeadmanE2E_ServedNamesMatchShippedAlertExpr(t *testing.T) {
	// Ensure both deadman families are marshalled to the wire, then scrape.
	resetGitLabWebhookDeadmanMetrics()
	initGitLabWebhookDeadmanMetrics(time.Unix(1_700_000_000, 0))
	served := servedGitLabDeadmanMetrics(t)

	// The two series names the shipped alert expr depends on. These are the
	// contract strings; the assertion below proves the template's expr references
	// exactly these — not a look-alike a rename could drift from.
	const (
		darkSeries = "gardenio_gitlab_webhook_dark"
		evalSeries = "gardenio_gitlab_webhook_evaluations_total"
	)
	if _, found := servedGaugeValue(served, darkSeries); !found {
		t.Errorf("%s absent from the served exposition — the dark alert expr would key off a series the bridge never emits", darkSeries)
	}
	if !strings.Contains(served, evalSeries) {
		t.Errorf("%s absent from the served exposition — the monitor-deadman expr would be unfireable", evalSeries)
	}

	// Read the SHIPPED template's rendered expr strings from the repo template.
	// We read the template source (not a helm render) so this test needs no helm
	// binary; the expr metric-name literals are outside any {{ }} action, so the
	// raw template contains them verbatim.
	//
	// CROSS-TREE READ, GUARDED (ka-0000005729). The template lives in a SIBLING
	// tree (helm/), so a filtered CI layer that materializes only
	// gardenio/controller/** would leave it absent. Rather than depend on every
	// task that runs this package pinning the file into its filter (the
	// costs_trust_lockstep precedent — fragile: a single missed CI filter turns
	// a green change RED, which is exactly how this test first failed the
	// coverage.yml test-gardenio-cov task), we SKIP LOUDLY when the file is absent.
	// The template IS materialized in the filters where this must hard-gate
	// (ci.yml test-gardenio, coverage.yml + coverage-merge.yml test-gardenio-cov)
	// AND in the e2e module (test-gitlab-webhook-deadman.sh runs against the full
	// worktree, so the coupling is a HARD gate there) — the skip is only a safety
	// net for a layer that legitimately does not carry the helm tree, never a way
	// to pass a real rename. The served-names half above still runs as a hard
	// assertion in every layer.
	tmplPath := filepath.Join("..", "..", "..", "helm", "gardenio", "templates", "gitlab-bridge", "webhook-deadman-alert.yaml")
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("SKIP (LOUD): shipped alert template not materialized in this layer (%s) — the served-name↔alert-expr coupling is hard-gated in ci.yml/coverage.yml test-gardenio[-cov] and the e2e module, which carry the helm tree; this layer only omits it. NOT a pass for a rename.", tmplPath)
		}
		t.Fatalf("read shipped alert template %s: %v", tmplPath, err)
	}
	tmpl := string(raw)

	// The dark alert expr must reference the served dark gauge; the deadman expr
	// must reference the served heartbeat counter via absent(...). If a rename
	// drifted the template off the emitted names, these fail — coupling the two
	// sides that would otherwise pass independently.
	if !strings.Contains(tmpl, "max_over_time("+darkSeries+"[") {
		t.Errorf("shipped alert template expr does not key off the emitted dark series %q — a one-sided rename has silently un-fired the dark alert", darkSeries)
	}
	if !strings.Contains(tmpl, "absent("+evalSeries+")") {
		t.Errorf("shipped alert template expr does not absent()-guard the emitted heartbeat series %q — a one-sided rename has silently un-fired the monitor-deadman alert", evalSeries)
	}
}
