// Unit tests for the trigger-engine binary entrypoint. Exercise env-parsing
// helpers, logger setup, spawn-guard construction, and the runTriggerEngine
// goroutine loop (immediate eval, ticker, notify channel, ctx cancel).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
)

// emptyDaemon implements bridge.TriggerBeadClient with no-op responses. It is
// enough to let a TriggerEngine run through an Evaluate tick — no triggers are
// loaded, so no matching, bead creation, or spawns happen.
type emptyDaemon struct {
	listCalls atomic.Int32
}

func (d *emptyDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.listCalls.Add(1)
	return &beadsapi.ListBeadsResult{}, nil
}

func (d *emptyDaemon) CreateBead(_ context.Context, _ beadsapi.CreateBeadRequest) (string, error) {
	return "", nil
}

func (d *emptyDaemon) UpdateBeadFields(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (d *emptyDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

func (d *emptyDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return nil
}

func (d *emptyDaemon) SpawnAgent(_ context.Context, _, _, _, _, _ string, _ ...map[string]string) (string, error) {
	return "", nil
}

func (d *emptyDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return map[string]beadsapi.ProjectInfo{}, nil
}

func (d *emptyDaemon) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (d *emptyDaemon) AddDependency(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (d *emptyDaemon) AddComment(_ context.Context, _, _, _ string) error {
	return nil
}

func (d *emptyDaemon) AddLabel(_ context.Context, _, _ string) error {
	return nil
}

func (d *emptyDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (d *emptyDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseConfig_Defaults(t *testing.T) {
	// Clear all env vars the parser consults.
	for _, k := range []string{
		"BEADS_HTTP_ADDR", "TRIGGER_POLL_INTERVAL", "LISTEN_ADDR", "METRICS_LISTEN_ADDR", "LOG_LEVEL",
		"NATS_URL", "NATS_TOKEN",
	} {
		t.Setenv(k, "")
	}

	cfg := parseConfig()

	if cfg.beadsHTTPAddr != "http://localhost:8080" {
		t.Errorf("beadsHTTPAddr=%q, want http://localhost:8080", cfg.beadsHTTPAddr)
	}
	if cfg.triggerPollInterval != 30*time.Second {
		t.Errorf("triggerPollInterval=%s, want 30s", cfg.triggerPollInterval)
	}
	if cfg.listenAddr != ":8094" {
		t.Errorf("listenAddr=%q, want :8094", cfg.listenAddr)
	}
	if cfg.metricsAddr != ":9092" {
		t.Errorf("metricsAddr=%q, want :9092", cfg.metricsAddr)
	}
	if cfg.logLevel != "info" {
		t.Errorf("logLevel=%q, want info", cfg.logLevel)
	}
	if cfg.natsURL != "" {
		t.Errorf("natsURL=%q, want empty by default", cfg.natsURL)
	}
	if cfg.natsToken != "" {
		t.Errorf("natsToken=%q, want empty by default", cfg.natsToken)
	}
}

func TestParseConfig_FromEnv(t *testing.T) {
	t.Setenv("BEADS_HTTP_ADDR", "http://beads:9090")
	t.Setenv("TRIGGER_POLL_INTERVAL", "5s")
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("METRICS_LISTEN_ADDR", ":9191")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_TOKEN", "sekret")

	cfg := parseConfig()

	if cfg.beadsHTTPAddr != "http://beads:9090" {
		t.Errorf("beadsHTTPAddr=%q", cfg.beadsHTTPAddr)
	}
	if cfg.triggerPollInterval != 5*time.Second {
		t.Errorf("triggerPollInterval=%s, want 5s", cfg.triggerPollInterval)
	}
	if cfg.listenAddr != ":9000" {
		t.Errorf("listenAddr=%q", cfg.listenAddr)
	}
	if cfg.metricsAddr != ":9191" {
		t.Errorf("metricsAddr=%q, want :9191", cfg.metricsAddr)
	}
	if cfg.logLevel != "debug" {
		t.Errorf("logLevel=%q", cfg.logLevel)
	}
	if cfg.natsURL != "nats://nats:4222" {
		t.Errorf("natsURL=%q", cfg.natsURL)
	}
	if cfg.natsToken != "sekret" {
		t.Errorf("natsToken=%q", cfg.natsToken)
	}
}

func TestParseConfig_InvalidPollIntervalFallsBackToDefault(t *testing.T) {
	t.Setenv("TRIGGER_POLL_INTERVAL", "not-a-duration")
	cfg := parseConfig()
	if cfg.triggerPollInterval != 30*time.Second {
		t.Errorf("triggerPollInterval=%s, want default 30s on invalid input", cfg.triggerPollInterval)
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("GARDENIO_TEST_KEY", "")
	if got := envOrDefault("GARDENIO_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("envOrDefault on unset=%q, want fallback", got)
	}
	t.Setenv("GARDENIO_TEST_KEY", "set-value")
	if got := envOrDefault("GARDENIO_TEST_KEY", "fallback"); got != "set-value" {
		t.Errorf("envOrDefault on set=%q, want set-value", got)
	}
}

func TestSetupLogger(t *testing.T) {
	// Verify all level branches return a non-nil logger. Invalid values fall
	// through to the info-level default.
	cases := []string{"debug", "info", "warn", "error", "", "gibberish"}
	for _, lvl := range cases {
		l := setupLogger(lvl)
		if l == nil {
			t.Errorf("setupLogger(%q) returned nil", lvl)
		}
	}
}

// TestRunTriggerEngine_LoopsAndStops drives runTriggerEngine with a very short
// interval and verifies (a) the immediate first Evaluate runs, (b) the ticker
// keeps firing, and (c) the goroutine exits promptly when ctx is cancelled.
func TestRunTriggerEngine_LoopsAndStops(t *testing.T) {
	daemon := &emptyDaemon{}
	src := bridge.NewWebhookTriggerSource(discardLogger())
	beadSrc := bridge.NewBeadStateTriggerSource(discardLogger())
	engine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon: daemon,
		Logger: discardLogger(),
	})
	multiSrc := bridge.NewMultiTriggerSource(discardLogger(), nil)
	engine.RegisterSource(src)
	engine.RegisterSource(beadSrc)
	engine.RegisterSource(multiSrc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTriggerEngine(ctx, engine, src, beadSrc, multiSrc, 10*time.Millisecond, discardLogger())
		close(done)
	}()

	// Wait for at least two list calls: the immediate Evaluate (1) plus at
	// least one ticker-driven Evaluate (2).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.listCalls.Load() >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := daemon.listCalls.Load(); got < 2 {
		cancel()
		<-done
		t.Fatalf("runTriggerEngine made only %d ListBeadsFiltered calls, want >=2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTriggerEngine did not stop within 1s of ctx cancel")
	}
}

// TestAccountTriggerSource_RegistersAndEvaluates is the registration smoke for
// C0 (ka-0000004826): AccountTriggerSource is now wired into the prod
// trigger-engine alongside webhook/schedule/bead/multi/mirror. Constructing it
// with the exported constructor, registering it under its "account" name, and
// driving EvaluateSource must reach the daemon — proving the source is no
// longer a silent no-op. Mirrors how main() builds accountSrc.
func TestAccountTriggerSource_RegistersAndEvaluates(t *testing.T) {
	daemon := &emptyDaemon{}
	engine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon: daemon,
		Logger: discardLogger(),
	})
	accountSrc := bridge.NewAccountTriggerSource(discardLogger())
	if got := accountSrc.Name(); got != "account" {
		t.Fatalf("AccountTriggerSource.Name() = %q, want %q", got, "account")
	}
	engine.RegisterSource(accountSrc)

	// Driving the registered source by name must load its triggers from the
	// daemon — an unregistered source would warn-and-return without ever
	// touching ListBeadsFiltered.
	engine.EvaluateSource(context.Background(), "account")

	if got := daemon.listCalls.Load(); got < 1 {
		t.Fatalf("registered account source made %d ListBeadsFiltered calls, want >=1", got)
	}
}

// startEmbeddedNATS spins up an in-process NATS server on a random port and
// returns its ClientURL. The server is shut down via t.Cleanup.
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host: "127.0.0.1",
		Port: -1,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded NATS not ready within 5s")
	}
	t.Cleanup(func() { ns.Shutdown() })
	return ns.ClientURL()
}

// TestStartSchedulerNATSListeners_NoURLNoop verifies the helper is a quiet
// no-op when NATS_URL is unset, so dev/test runs do not spuriously dial.
func TestStartSchedulerNATSListeners_NoURLNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	multiSrc := bridge.NewMultiTriggerSource(discardLogger(), nil)

	startSchedulerNATSListeners(ctx, multiSrc.Engine(), "", "", "", discardLogger())

	if got := multiSrc.PendingCount(); got != 0 {
		t.Fatalf("PendingCount=%d, want 0 (listeners should be no-op without NATS_URL)", got)
	}
}

// TestStartSchedulerNATSListeners_DecisionEventFiresTrigger boots an embedded
// NATS server, starts the three scheduler listeners against multiSrc.Engine(),
// publishes a decisions.X.DecisionResponded event, and asserts the trigger
// engine observes it in its pending buffer. This is the integration-level
// proof that gardeners with non-cron trigger_mode fire from trigger-engine
// alone, per Phase 3b/B7-c3.
func TestStartSchedulerNATSListeners_DecisionEventFiresTrigger(t *testing.T) {
	natsURL := startEmbeddedNATS(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	multiSrc := bridge.NewMultiTriggerSource(discardLogger(), nil)
	startSchedulerNATSListeners(ctx, multiSrc.Engine(), natsURL, "", "", discardLogger())

	// Use a separate publisher connection — the listeners run their own.
	pub, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats.Connect publisher: %v", err)
	}
	defer pub.Close()

	// Listener subscriptions are registered from goroutines, so we briefly
	// flush + sleep to absorb the publish-before-subscribe race. The real
	// assertion below polls PendingCount with its own timeout, so a missed
	// publish still fails with a useful message rather than hanging.
	if err := pub.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	decisionPayload, _ := json.Marshal(map[string]any{
		"decision_id":  "gd-dec-1",
		"question":     "Ship it?",
		"chosen_label": "yes",
		"project":      "gardenio",
		"role":         "crew",
		"bead_id":      "gd-bead-7",
		"labels":       []string{"project:gardenio", "role:crew"},
	})
	if err := pub.Publish("decisions.gardenio.DecisionResponded", decisionPayload); err != nil {
		t.Fatalf("publish decision: %v", err)
	}

	beadClosedPayload, _ := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":     "gd-bead-2",
			"type":   "task",
			"title":  "demo",
			"status": "closed",
			"labels": []string{"project:gardenio"},
		},
		"closed_by": "tester",
	})
	if err := pub.Publish("beads.bead.closed", beadClosedPayload); err != nil {
		t.Fatalf("publish bead-closed: %v", err)
	}

	mailPayload, _ := json.Marshal(map[string]any{
		"bead_id": "gd-mail-3",
		"channel": "alerts",
		"from":    "agent",
		"to":      "gardener",
		"subject": "ping",
	})
	if err := pub.Publish("beads.mail.sent", mailPayload); err != nil {
		t.Fatalf("publish mail: %v", err)
	}

	// All three listeners feed multiSrc.Engine() via OnDecisionResolved /
	// OnBeadEvent; PendingCount should reach 3 once the messages are
	// delivered and decoded.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if multiSrc.PendingCount() >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := multiSrc.PendingCount(); got < 3 {
		t.Fatalf("PendingCount=%d, want >=3 (decision + bead-closed + mail)", got)
	}
}

// TestRunTriggerEngine_ZeroIntervalFallback covers the `interval <= 0`
// fallback branch (defaults to 30s). We cancel the context immediately after
// the first Evaluate so the test does not wait the full 30s for a tick.
func TestRunTriggerEngine_ZeroIntervalFallback(t *testing.T) {
	daemon := &emptyDaemon{}
	src := bridge.NewWebhookTriggerSource(discardLogger())
	beadSrc := bridge.NewBeadStateTriggerSource(discardLogger())
	engine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon: daemon,
		Logger: discardLogger(),
	})
	multiSrc := bridge.NewMultiTriggerSource(discardLogger(), nil)
	engine.RegisterSource(src)
	engine.RegisterSource(beadSrc)
	engine.RegisterSource(multiSrc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTriggerEngine(ctx, engine, src, beadSrc, multiSrc, 0, discardLogger())
		close(done)
	}()

	// Wait for the immediate Evaluate to complete.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if daemon.listCalls.Load() >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTriggerEngine with zero interval did not stop on ctx cancel")
	}
	if daemon.listCalls.Load() < 1 {
		t.Fatalf("immediate Evaluate did not run with zero interval")
	}
}

// TestRunStepDispatchBackfill_LoopsAndStops covers the backfill-sweep driver
// (ka-0000006285): the durable retry the SSE close-event path cannot provide.
// It must sweep immediately on startup and then on the ticker, and stop
// promptly on ctx cancel. The sweep issues one ListBeadsFiltered per pass
// (emptyDaemon returns no candidates), so listCalls is the pass counter.
func TestRunStepDispatchBackfill_LoopsAndStops(t *testing.T) {
	daemon := &emptyDaemon{}
	dispatcher := bridge.NewFormulaStepDispatcher(daemon, nil, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runStepDispatchBackfill(ctx, dispatcher, 10*time.Millisecond, discardLogger())
		close(done)
	}()

	// Wait for at least two sweeps: the immediate startup sweep (1) plus at
	// least one ticker-driven sweep (2).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.listCalls.Load() >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := daemon.listCalls.Load(); got < 2 {
		cancel()
		<-done
		t.Fatalf("runStepDispatchBackfill made only %d ListBeadsFiltered calls, want >=2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runStepDispatchBackfill did not stop within 1s of ctx cancel")
	}
}

// TestRunStepDispatchBackfill_ZeroIntervalFallback: a non-positive interval must
// fall back to the 2m default rather than panicking on time.NewTicker(<=0). The
// immediate startup sweep still runs, then ctx cancel stops the loop before the
// (2m) ticker ever fires.
func TestRunStepDispatchBackfill_ZeroIntervalFallback(t *testing.T) {
	daemon := &emptyDaemon{}
	dispatcher := bridge.NewFormulaStepDispatcher(daemon, nil, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runStepDispatchBackfill(ctx, dispatcher, 0, discardLogger())
		close(done)
	}()

	// Wait for the immediate startup sweep.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if daemon.listCalls.Load() >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runStepDispatchBackfill with zero interval did not stop on ctx cancel")
	}
	if daemon.listCalls.Load() < 1 {
		t.Fatalf("immediate startup sweep did not run with zero interval")
	}
}

// TestBuildMetricsServer_ServesDefaultGatherer is the regression guard for the
// scrape half of the gardener-resume observability layer (bead ka-0000000971,
// decision ka-0000006918). trigger-engine fires gardeners, so the gardener-loop
// resume metrics (gardener_resume_total / gardener_resumes_without_progress) are
// incremented in THIS process via promauto against the DEFAULT registry. Before
// this binary served /metrics they were registered but never scraped. The
// handler must return 200 on GET /metrics and serve the default gatherer —
// proven by the auto-registered Go runtime collectors. (That the gardener-resume
// families live on this same default registry is asserted in the bridge package
// where they can be incremented: TestGardenerResumeMetricsOnDefaultRegistry.
// Prometheus *Vec metrics emit no exposition lines until their first observed
// label set, so an un-incremented family cannot be asserted here.)
func TestBuildMetricsServer_ServesDefaultGatherer(t *testing.T) {
	srv := buildMetricsServer(":0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Default gatherer proof: client_golang's Go collector is auto-registered
	// there at init, so its series prove buildMetricsServer serves the same
	// registry the bridge's promauto metrics land on.
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics did not serve the default gatherer (missing go_goroutines)")
	}
}

// TestLiveHealthzHandler covers the LIVE /healthz engine-loop deadman
// (ka-0000005325). A fresh Evaluate() stamp reads healthy (200); a stale one
// past the staleness window reports unhealthy (503) so the k8s liveness probe
// restarts a wedged pod. This is the primitive whose ABSENCE let the 30h
// schedule blackout stay invisible — the old handler was static and always ok.
func TestLiveHealthzHandler(t *testing.T) {
	staleness := 5 * time.Minute
	// ready=true: the bootstrapping grace (gd-niQPoeR7ja) is exercised
	// separately by TestLiveHealthzHandler_BootstrappingSkipsStaleness.
	ready := &atomic.Bool{}
	ready.Store(true)
	h := liveHealthzHandler(staleness, ready)

	// Fresh: a just-completed Evaluate() → healthy 200.
	recordEvaluateCompleted(time.Now())
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("fresh Evaluate() → /healthz = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("fresh /healthz body = %s, want status ok", rec.Body.String())
	}

	// Stale: last Evaluate() well past the staleness window → 503 so liveness
	// restarts the wedged pod.
	lastEvaluateNano.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("stalled Evaluate() → /healthz = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"unhealthy"`) {
		t.Errorf("stalled /healthz body = %s, want status unhealthy", rec.Body.String())
	}
}

// TestRecordEvaluateCompleted verifies the heartbeat stamps the /healthz clock
// forward: a completed cycle after a stale window flips the handler back to
// healthy. This is the write side of the deadman the metric + /healthz share.
func TestRecordEvaluateCompleted_RefreshesHealth(t *testing.T) {
	ready := &atomic.Bool{}
	ready.Store(true)
	h := liveHealthzHandler(time.Minute, ready)

	// Force stale, confirm 503.
	lastEvaluateNano.Store(time.Now().Add(-time.Hour).UnixNano())
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("precondition: stale /healthz = %d, want 503", rec.Code)
	}

	// A completed cycle beats the heartbeat → healthy again.
	recordEvaluateCompleted(time.Now())
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("after recordEvaluateCompleted, /healthz = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestBuildMetricsServer_ExposesEngineHeartbeat proves the engine-loop deadman
// counter is scrapeable on the internal /metrics endpoint after at least one
// completed cycle — the series the TriggerEngineScheduleDark alert reads.
func TestBuildMetricsServer_ExposesEngineHeartbeat(t *testing.T) {
	recordEvaluateCompleted(time.Now())
	srv := buildMetricsServer(":0")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gardenio_trigger_engine_evaluations_total") {
		t.Errorf("/metrics did not expose the engine-loop heartbeat counter, got:\n%s", rec.Body.String())
	}
}

// TestParseConfig_HealthStalenessMultiple covers the deadman /healthz staleness
// multiple: default 10, a valid override, and the guard that a non-positive or
// unparseable value falls back to the default rather than 503-ing every scrape.
func TestParseConfig_HealthStalenessMultiple(t *testing.T) {
	t.Setenv("HEALTH_STALENESS_MULTIPLE", "")
	if got := parseConfig().healthStalenessMultiple; got != 10 {
		t.Errorf("default healthStalenessMultiple = %d, want 10", got)
	}

	t.Setenv("HEALTH_STALENESS_MULTIPLE", "20")
	if got := parseConfig().healthStalenessMultiple; got != 20 {
		t.Errorf("override healthStalenessMultiple = %d, want 20", got)
	}

	for _, bad := range []string{"0", "-3", "not-a-number"} {
		t.Setenv("HEALTH_STALENESS_MULTIPLE", bad)
		if got := parseConfig().healthStalenessMultiple; got != 10 {
			t.Errorf("HEALTH_STALENESS_MULTIPLE=%q → healthStalenessMultiple = %d, want fallback 10", bad, got)
		}
	}
}

// TestEnvBoolDefault exercises every branch of envBoolDefault: unset → default,
// parseable true/false forms override, and an unparseable value falls back to
// the default rather than panicking. The default branch (env unset) and the
// parse-error branch were previously uncovered.
func TestEnvBoolDefault(t *testing.T) {
	const key = "GARDENIO_TEST_BOOL"

	// Unset → returns default (both polarities).
	t.Setenv(key, "")
	if got := envBoolDefault(key, true); got != true {
		t.Errorf("envBoolDefault unset want default true, got %v", got)
	}
	if got := envBoolDefault(key, false); got != false {
		t.Errorf("envBoolDefault unset want default false, got %v", got)
	}

	// Parseable forms override the default regardless of its polarity.
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"true", true}, {"1", true}, {"t", true}, {"TRUE", true},
		{"false", false}, {"0", false}, {"f", false}, {"FALSE", false},
	} {
		t.Setenv(key, tc.val)
		if got := envBoolDefault(key, !tc.want); got != tc.want {
			t.Errorf("envBoolDefault(%q)=%v, want %v", tc.val, got, tc.want)
		}
	}

	// Unparseable → falls back to the supplied default, not a panic.
	t.Setenv(key, "not-a-bool")
	if got := envBoolDefault(key, true); got != true {
		t.Errorf("envBoolDefault unparseable want default true, got %v", got)
	}
	if got := envBoolDefault(key, false); got != false {
		t.Errorf("envBoolDefault unparseable want default false, got %v", got)
	}
}

// TestParseConfig_OrphanAndLivenessDefaults pins the orphan-sweep and
// gardener-liveness defaults parseConfig produces when their env vars are unset:
// the sweep and liveness alerting are on by default, durable bead creation is
// off, and the intake label is the scheduler default. These branches of
// parseConfig were not previously asserted.
func TestParseConfig_OrphanAndLivenessDefaults(t *testing.T) {
	for _, k := range []string{
		"ORPHAN_SWEEP_ENABLED", "ORPHAN_INTAKE_LABEL", "ORPHAN_SWEEP_MIN_AGE",
		"ORPHAN_SWEEP_INTERVAL", "ORPHAN_DELANE_COOLDOWN", "RECONCILE_INTERVAL", "STATE_FILE",
		"GARDENER_LIVENESS_ENABLED", "GARDENER_LIVENESS_NOTIFY", "GARDENER_LIVENESS_CREATE_BEAD",
	} {
		t.Setenv(k, "")
	}

	cfg := parseConfig()

	if !cfg.orphanSweepEnabled {
		t.Errorf("orphanSweepEnabled=false, want true by default")
	}
	if cfg.orphanIntakeLabel != scheduler.DefaultIntakeLabel {
		t.Errorf("orphanIntakeLabel=%q, want %q", cfg.orphanIntakeLabel, scheduler.DefaultIntakeLabel)
	}
	if cfg.orphanMinAge != 0 {
		t.Errorf("orphanMinAge=%s, want 0 (scheduler default sentinel)", cfg.orphanMinAge)
	}
	if cfg.orphanSweepInterval != 0 {
		t.Errorf("orphanSweepInterval=%s, want 0 (scheduler default sentinel)", cfg.orphanSweepInterval)
	}
	if cfg.orphanDeLaneCooldown != 0 {
		t.Errorf("orphanDeLaneCooldown=%s, want 0 (scheduler default sentinel)", cfg.orphanDeLaneCooldown)
	}
	if cfg.reconcileInterval != 60*time.Second {
		t.Errorf("reconcileInterval=%s, want 60s", cfg.reconcileInterval)
	}
	if cfg.stateFile != "" {
		t.Errorf("stateFile=%q, want empty (reconciler disabled)", cfg.stateFile)
	}
	if !cfg.livenessEnabled {
		t.Errorf("livenessEnabled=false, want true by default")
	}
	if !cfg.livenessNotify {
		t.Errorf("livenessNotify=false, want true by default")
	}
	if cfg.livenessCreateBead {
		t.Errorf("livenessCreateBead=true, want false by default")
	}
}

// TestParseConfig_OrphanAndLivenessFromEnv drives the orphan-sweep and
// liveness config through their env-var overrides, including the
// duration-parsing branches for ORPHAN_SWEEP_MIN_AGE / ORPHAN_SWEEP_INTERVAL /
// RECONCILE_INTERVAL.
func TestParseConfig_OrphanAndLivenessFromEnv(t *testing.T) {
	t.Setenv("ORPHAN_SWEEP_ENABLED", "false")
	t.Setenv("ORPHAN_INTAKE_LABEL", "custom-intake")
	t.Setenv("ORPHAN_SWEEP_MIN_AGE", "10m")
	t.Setenv("ORPHAN_SWEEP_INTERVAL", "2m")
	t.Setenv("ORPHAN_DELANE_COOLDOWN", "20m")
	t.Setenv("RECONCILE_INTERVAL", "45s")
	t.Setenv("STATE_FILE", "/tmp/state.json")
	t.Setenv("GARDENER_LIVENESS_ENABLED", "false")
	t.Setenv("GARDENER_LIVENESS_NOTIFY", "false")
	t.Setenv("GARDENER_LIVENESS_CREATE_BEAD", "true")

	cfg := parseConfig()

	if cfg.orphanSweepEnabled {
		t.Errorf("orphanSweepEnabled=true, want false from env")
	}
	if cfg.orphanIntakeLabel != "custom-intake" {
		t.Errorf("orphanIntakeLabel=%q, want custom-intake", cfg.orphanIntakeLabel)
	}
	if cfg.orphanMinAge != 10*time.Minute {
		t.Errorf("orphanMinAge=%s, want 10m", cfg.orphanMinAge)
	}
	if cfg.orphanSweepInterval != 2*time.Minute {
		t.Errorf("orphanSweepInterval=%s, want 2m", cfg.orphanSweepInterval)
	}
	if cfg.orphanDeLaneCooldown != 20*time.Minute {
		t.Errorf("orphanDeLaneCooldown=%s, want 20m", cfg.orphanDeLaneCooldown)
	}
	if cfg.reconcileInterval != 45*time.Second {
		t.Errorf("reconcileInterval=%s, want 45s", cfg.reconcileInterval)
	}
	if cfg.stateFile != "/tmp/state.json" {
		t.Errorf("stateFile=%q, want /tmp/state.json", cfg.stateFile)
	}
	if cfg.livenessEnabled {
		t.Errorf("livenessEnabled=true, want false from env")
	}
	if cfg.livenessNotify {
		t.Errorf("livenessNotify=true, want false from env")
	}
	if !cfg.livenessCreateBead {
		t.Errorf("livenessCreateBead=false, want true from env")
	}
}

// TestParseConfig_InvalidDurationsFallBack confirms that malformed
// ORPHAN_SWEEP_MIN_AGE / ORPHAN_SWEEP_INTERVAL / RECONCILE_INTERVAL values are
// silently ignored (matching the lenient parsing the parser documents) rather
// than failing startup. The min-age/interval sentinels stay 0 (scheduler
// default) and reconcileInterval keeps its 60s default.
func TestParseConfig_InvalidDurationsFallBack(t *testing.T) {
	t.Setenv("ORPHAN_SWEEP_MIN_AGE", "nope")
	t.Setenv("ORPHAN_SWEEP_INTERVAL", "nope")
	t.Setenv("ORPHAN_DELANE_COOLDOWN", "nope")
	t.Setenv("RECONCILE_INTERVAL", "nope")

	cfg := parseConfig()

	if cfg.orphanMinAge != 0 {
		t.Errorf("orphanMinAge=%s, want 0 on invalid input", cfg.orphanMinAge)
	}
	if cfg.orphanSweepInterval != 0 {
		t.Errorf("orphanSweepInterval=%s, want 0 on invalid input", cfg.orphanSweepInterval)
	}
	if cfg.orphanDeLaneCooldown != 0 {
		t.Errorf("orphanDeLaneCooldown=%s, want 0 on invalid input", cfg.orphanDeLaneCooldown)
	}
	if cfg.reconcileInterval != 60*time.Second {
		t.Errorf("reconcileInterval=%s, want default 60s on invalid input", cfg.reconcileInterval)
	}
}

// TestParseConfig_StepBackfillInterval exercises the STEP_BACKFILL_INTERVAL
// parse glue added for the formula step-dispatch backfill sweep (ka-0000006285):
// the 2m default when unset, an explicit override, and a fall-back to 2m on a
// malformed value (matching the lenient parsing every other duration field uses).
func TestParseConfig_StepBackfillInterval(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("STEP_BACKFILL_INTERVAL", "")
		if got := parseConfig().stepBackfillInterval; got != 2*time.Minute {
			t.Errorf("stepBackfillInterval=%s, want default 2m", got)
		}
	})
	t.Run("from env", func(t *testing.T) {
		t.Setenv("STEP_BACKFILL_INTERVAL", "30s")
		if got := parseConfig().stepBackfillInterval; got != 30*time.Second {
			t.Errorf("stepBackfillInterval=%s, want 30s", got)
		}
	})
	t.Run("invalid falls back", func(t *testing.T) {
		t.Setenv("STEP_BACKFILL_INTERVAL", "not-a-duration")
		if got := parseConfig().stepBackfillInterval; got != 2*time.Minute {
			t.Errorf("stepBackfillInterval=%s, want default 2m on invalid input", got)
		}
	})
}

// TestPrintUsage writes the usage text to a real *os.File (a temp file, since
// printUsage takes *os.File not io.Writer) and asserts the key sections are
// present: the version interpolation and a couple of documented env vars and
// endpoints. Guards against accidentally dropping the help output build
// verifications (CI, smoke) rely on (ka-0000006573).
func TestPrintUsage(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "usage-*.txt")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	printUsage(f)

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	body := string(out)
	for _, want := range []string{
		"trigger-engine", "BEADS_HTTP_ADDR", "TRIGGER_POLL_INTERVAL",
		"/webhooks/{token}", "Version:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("printUsage output missing %q\n---\n%s", want, body)
		}
	}
}

// beadJSONForTest mirrors the daemon's bead JSON shape closely enough for the
// orphanStore projection (id/type/status/assignee/labels/created_at).
type beadJSONForTest struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	Assignee  string   `json:"assignee"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
}

// newOrphanDaemon spins up a stub daemon HTTP server and returns a beadsapi
// client pointed at it. The server is torn down via t.Cleanup.
func newOrphanDaemon(t *testing.T, beads []beadJSONForTest) (*beadsapi.Client, *atomic.Int32, *atomic.Value) {
	t.Helper()
	labelPosts := &atomic.Int32{}
	gotLabelID := &atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": beads,
				"total": len(beads),
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/beads/") && strings.HasSuffix(r.URL.Path, "/labels"):
			labelPosts.Add(1)
			// /v1/beads/{id}/labels — extract id.
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/labels")
			gotLabelID.Store(id)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/batch/dependencies":
			// Sweep enrichment: report no parent edges for any candidate, so the
			// seeded orphan is treated as unparented and still surfaces.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/batch/children-by-status":
			// Sweep enrichment: report no children for any candidate, so the seeded
			// orphan is treated as a childless leaf (not a tree-root) and surfaces.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"histograms": map[string]any{}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			// Sweep enrichment: no label.removed events ⇒ no de-lane cooldown.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			// Sweep enrichment: no intake-groomed comments ⇒ no de-lane cooldown.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, labelPosts, gotLabelID
}

// TestOrphanStore_ListReady drives the orphanStore.ListReady adapter against a
// stub daemon and asserts the daemon rows are projected into scheduler
// OrphanBead values (id/type/status/assignee/labels/created_at). Previously 0%.
func TestOrphanStore_ListReady(t *testing.T) {
	created := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	client, _, _ := newOrphanDaemon(t, []beadJSONForTest{
		{ID: "gd-a", Type: "task", Status: "open", Assignee: "", Labels: []string{"project:gardenio"}, CreatedAt: created},
		{ID: "gd-b", Type: "bug", Status: "open", Assignee: "someone", Labels: nil, CreatedAt: created},
	})

	store := orphanStore{daemon: client}
	got, err := store.ListReady(context.Background())
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListReady returned %d beads, want 2", len(got))
	}
	if got[0].ID != "gd-a" || got[0].Type != "task" || got[0].Status != "open" {
		t.Errorf("bead[0] = %+v, want id=gd-a type=task status=open", got[0])
	}
	if got[0].Assignee != "" || len(got[0].Labels) != 1 || got[0].Labels[0] != "project:gardenio" {
		t.Errorf("bead[0] assignee/labels = %q/%v, want unassigned with project:gardenio", got[0].Assignee, got[0].Labels)
	}
	if got[0].CreatedAt.IsZero() {
		t.Errorf("bead[0] CreatedAt not parsed from %q", created)
	}
	if got[1].ID != "gd-b" || got[1].Assignee != "someone" {
		t.Errorf("bead[1] = %+v, want id=gd-b assignee=someone", got[1])
	}
}

// TestOrphanStore_ListReady_Paginates is the regression for bug ka-0000000077:
// the sweep used to pull a single Limit-capped page newest-first, so any bead
// past the cap (the oldest ~79% of the fleet-wide candidate set) was invisible
// to it forever. ListReady now walks every page via ListAllBeadsFiltered. This
// seeds a candidate population larger than one page (the daemon honors
// offset/limit) and asserts EVERY bead — including the ones only reachable on a
// later page — is returned.
func TestOrphanStore_ListReady_Paginates(t *testing.T) {
	created := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	const total = 10001 // > listAllPagesPageSize (10000) ⇒ forces a second page
	all := make([]beadJSONForTest, total)
	for i := range all {
		all[i] = beadJSONForTest{
			ID:        fmt.Sprintf("gd-%05d", i),
			Type:      "task",
			Status:    "open",
			CreatedAt: created,
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/beads" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = len(all)
		}
		end := offset + limit
		if offset > len(all) {
			offset = len(all)
		}
		if end > len(all) {
			end = len(all)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": all[offset:end],
			"total": len(all),
		})
	}))
	t.Cleanup(srv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	store := orphanStore{daemon: client}
	got, err := store.ListReady(context.Background())
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(got) != total {
		t.Fatalf("ListReady returned %d beads, want %d — a later page was dropped (the ka-0000000077 truncation)", len(got), total)
	}
	// The last bead is only reachable on the second page; assert it made it.
	if got[len(got)-1].ID != fmt.Sprintf("gd-%05d", total-1) {
		t.Errorf("last bead = %q, want the final page's bead %q", got[len(got)-1].ID, fmt.Sprintf("gd-%05d", total-1))
	}
}

// TestOrphanStore_ListReady_DaemonError surfaces the error path: a daemon that
// returns 500 must propagate an error rather than an empty slice.
func TestOrphanStore_ListReady_DaemonError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer client.Close()

	store := orphanStore{daemon: client}
	if _, err := store.ListReady(context.Background()); err == nil {
		t.Fatal("ListReady returned nil error on daemon 500, want error")
	}
}

// TestOrphanStore_AddLabel verifies the AddLabel adapter forwards to the
// daemon's POST /v1/beads/{id}/labels endpoint with the right bead id.
// Previously 0%.
func TestOrphanStore_AddLabel(t *testing.T) {
	client, labelPosts, gotLabelID := newOrphanDaemon(t, nil)
	store := orphanStore{daemon: client}

	if err := store.AddLabel(context.Background(), "gd-xyz", "needs-intake"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if labelPosts.Load() != 1 {
		t.Errorf("label POST count = %d, want 1", labelPosts.Load())
	}
	if id, _ := gotLabelID.Load().(string); id != "gd-xyz" {
		t.Errorf("daemon received label for id %q, want gd-xyz", id)
	}
}

// TestRunOrphanSweep_SurfacesAndStops drives runOrphanSweep end-to-end: it
// builds a real OrphanSweeper over the orphanStore adapter against a stub
// daemon seeded with one orphaned bead (old, unassigned, eligible type, no
// intake label), runs the loop with a short interval, and asserts the sweep's
// initial Tick labelled the orphan via the daemon AND that the goroutine exits
// promptly on ctx cancel. Previously 0%.
func TestRunOrphanSweep_SurfacesAndStops(t *testing.T) {
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	client, labelPosts, gotLabelID := newOrphanDaemon(t, []beadJSONForTest{
		{ID: "gd-orphan", Type: "task", Status: "open", Assignee: "", Labels: nil, CreatedAt: old},
	})

	sweeper := scheduler.NewOrphanSweeper(
		orphanStore{daemon: client},
		discardLogger(),
		scheduler.OrphanSweepConfig{IntakeLabel: scheduler.DefaultIntakeLabel, MinAge: time.Minute},
		10*time.Millisecond,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runOrphanSweep(ctx, sweeper, 10*time.Millisecond, discardLogger())
		close(done)
	}()

	// The initial sweep on startup should label the orphan promptly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if labelPosts.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if labelPosts.Load() < 1 {
		cancel()
		<-done
		t.Fatalf("runOrphanSweep did not label the orphan (label POSTs=%d)", labelPosts.Load())
	}
	if id, _ := gotLabelID.Load().(string); id != "gd-orphan" {
		cancel()
		<-done
		t.Fatalf("orphan sweep labelled %q, want gd-orphan", id)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runOrphanSweep did not stop within 1s of ctx cancel")
	}
}

// TestOrphanStore_ParentedBeads drives the ParentedBeads adapter against a stub
// daemon serving GET /v1/batch/dependencies. A forward parent-child dep means
// the bead HAS a parent (edges are stored child-first), so only gd-child is
// reported parented; gd-orphan (blocks edge only) and gd-lonely (no edges) are
// not. (ka-0000007370)
func TestOrphanStore_ParentedBeads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/batch/dependencies" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": "gd-child", "deps": []map[string]string{{"bead_id": "gd-child", "depends_on_id": "gd-epic", "type": "parent-child"}}},
				{"id": "gd-orphan", "deps": []map[string]string{{"bead_id": "gd-orphan", "depends_on_id": "gd-other", "type": "blocks"}}},
				{"id": "gd-lonely", "deps": []map[string]string{}},
			},
		})
	}))
	defer srv.Close()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer client.Close()

	store := orphanStore{daemon: client}
	got, err := store.ParentedBeads(context.Background(), []string{"gd-child", "gd-orphan", "gd-lonely"})
	if err != nil {
		t.Fatalf("ParentedBeads: %v", err)
	}
	if !got["gd-child"] {
		t.Errorf("gd-child should be reported as parented")
	}
	if got["gd-orphan"] {
		t.Errorf("gd-orphan (blocks edge only) must not be parented")
	}
	if got["gd-lonely"] {
		t.Errorf("gd-lonely (no edges) must not be parented")
	}
}

// TestOrphanStore_ParentedBeads_Empty confirms the no-ids fast path returns an
// empty map without a daemon round-trip.
func TestOrphanStore_ParentedBeads_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("daemon must not be called for an empty id list")
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer client.Close()

	got, err := (orphanStore{daemon: client}).ParentedBeads(context.Background(), nil)
	if err != nil {
		t.Fatalf("ParentedBeads: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ParentedBeads(nil) = %v, want empty", got)
	}
}

// TestOrphanStore_HasOpenChildren drives the HasOpenChildren adapter against a
// stub daemon serving GET /v1/batch/children-by-status. A bead is a tree-root
// only when at least one child bucket is a NON-terminal status with a positive
// count. All-closed / partial-only subtrees and childless beads are NOT
// tree-roots — closing a child never removes the parent-child edge, so the
// status histogram (not a bare edge check) is the discriminator. (ka-0000003548)
func TestOrphanStore_HasOpenChildren(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/batch/children-by-status" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"histograms": map[string]map[string]int{
				"gd-root":       {"open": 2, "closed": 3},    // has open children → tree-root
				"gd-miomufvsis": {"in_progress": 1},          // in_progress is live → tree-root
				"gd-drained":    {"closed": 5},               // all closed → NOT a tree-root
				"gd-partial":    {"closed": 2, "partial": 1}, // terminal-only → NOT a tree-root
				"gd-childless":  {},                          // no children → NOT a tree-root
			},
		})
	}))
	defer srv.Close()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer client.Close()

	store := orphanStore{daemon: client}
	got, err := store.HasOpenChildren(context.Background(),
		[]string{"gd-root", "gd-miomufvsis", "gd-drained", "gd-partial", "gd-childless"})
	if err != nil {
		t.Fatalf("HasOpenChildren: %v", err)
	}
	if !got["gd-root"] {
		t.Errorf("gd-root (open children) should be a tree-root")
	}
	if !got["gd-miomufvsis"] {
		t.Errorf("gd-miomufvsis (in_progress child) should be a tree-root")
	}
	if got["gd-drained"] {
		t.Errorf("gd-drained (all children closed) must NOT be a tree-root")
	}
	if got["gd-partial"] {
		t.Errorf("gd-partial (closed+partial only) must NOT be a tree-root")
	}
	if got["gd-childless"] {
		t.Errorf("gd-childless (no children) must NOT be a tree-root")
	}
}

// TestOrphanStore_HasOpenChildren_Empty confirms the no-ids fast path returns an
// empty map without a daemon round-trip.
func TestOrphanStore_HasOpenChildren_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("daemon must not be called for an empty id list")
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer client.Close()

	got, err := (orphanStore{daemon: client}).HasOpenChildren(context.Background(), nil)
	if err != nil {
		t.Fatalf("HasOpenChildren: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("HasOpenChildren(nil) = %v, want empty", got)
	}
}

// TestOrphanStore_LastDeLane drives the LastDeLane adapter against a stub daemon
// serving per-bead events (beads.label.removed) AND comments. It must key the
// cooldown off the LATER of the authoritative removal event and the legacy
// intake-groomed comment, match the intake label case-insensitively, ignore
// removals of OTHER labels, and omit beads that were never de-laned.
// (ka-0000007370, ka-0000007706)
func TestOrphanStore_LastDeLane(t *testing.T) {
	early := time.Date(2026, 7, 10, 20, 15, 0, 0, time.UTC)
	late := time.Date(2026, 7, 10, 21, 45, 0, 0, time.UTC)
	later := time.Date(2026, 7, 10, 22, 30, 0, 0, time.UTC)

	// eventsByID: beads.label.removed (and noise) events per bead.
	eventsByID := map[string][]map[string]any{
		// gd-comment-newer: event de-lane early, comment de-lane late → comment wins.
		"gd-comment-newer": {
			{"id": 10, "topic": "beads.label.removed", "created_at": early.Format(time.RFC3339),
				"payload": json.RawMessage(`{"label":"needs-intake"}`)},
		},
		// gd-event-only: de-laned via a bare label remove with NO comment. This is
		// the ka-0000007706 regression case — the cooldown must still fire.
		"gd-event-only": {
			{"id": 11, "topic": "beads.label.added", "created_at": early.Format(time.RFC3339),
				"payload": json.RawMessage(`{"label":"needs-intake"}`)},
			{"id": 12, "topic": "beads.label.removed", "created_at": later.Format(time.RFC3339),
				"payload": json.RawMessage(`{"label":"NEEDS-INTAKE"}`)}, // case-insensitive
		},
		// gd-other-label: only OTHER labels were removed → not a de-lane.
		"gd-other-label": {
			{"id": 13, "topic": "beads.label.removed", "created_at": later.Format(time.RFC3339),
				"payload": json.RawMessage(`{"label":"blocked"}`)},
		},
	}
	commentsByID := map[string][]map[string]any{
		"gd-comment-newer": {
			{"id": 1, "author": "groomer", "text": "intake-groomed: releasing to queue", "created_at": late.Format(time.RFC3339)},
		},
		"gd-plain": {
			{"id": 4, "author": "someone", "text": "just a comment", "created_at": late.Format(time.RFC3339)},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/events")
			_ = json.NewEncoder(w).Encode(map[string]any{"events": eventsByID[id]})
		case strings.HasSuffix(r.URL.Path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/comments")
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": commentsByID[id]})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer client.Close()

	store := orphanStore{daemon: client, intakeLabel: "needs-intake"}
	got, err := store.LastDeLane(context.Background(),
		[]string{"gd-comment-newer", "gd-event-only", "gd-other-label", "gd-plain", "gd-missing"})
	if err != nil {
		t.Fatalf("LastDeLane: %v", err)
	}
	// comment (late) is newer than the removal event (early) → comment wins.
	if !got["gd-comment-newer"].Equal(late) {
		t.Errorf("gd-comment-newer de-lane = %s, want later signal %s", got["gd-comment-newer"], late)
	}
	// event-only de-lane (no comment) must still be detected — the regression fix.
	if !got["gd-event-only"].Equal(later) {
		t.Errorf("gd-event-only de-lane = %s, want removal event %s (no comment must still count)", got["gd-event-only"], later)
	}
	// a removal of some OTHER label is not a de-lane of the intake label.
	if _, ok := got["gd-other-label"]; ok {
		t.Errorf("gd-other-label removed a different label; must be absent from map")
	}
	if _, ok := got["gd-plain"]; ok {
		t.Errorf("gd-plain has no intake-groomed comment and no removal event; must be absent from map")
	}
	if _, ok := got["gd-missing"]; ok {
		t.Errorf("gd-missing (no events/comments) must be absent from map")
	}
}

// TestStartSchedulerNATSListeners_NilEngineNoop covers the nil-engine guard:
// when the trigger engine is nil the helper must warn-and-return without
// dialing NATS, even if a URL is supplied. Previously this branch was
// uncovered.
func TestStartSchedulerNATSListeners_NilEngineNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A non-empty URL ensures we exercise the engine==nil guard specifically
	// (not the empty-URL early return). No NATS server is running on this port;
	// the helper must not attempt to dial it.
	startSchedulerNATSListeners(ctx, nil, "nats://127.0.0.1:1", "", "", discardLogger())

	// Nothing to assert beyond "did not block or panic"; the nil-engine branch
	// returns synchronously before spawning any listener goroutine.
}

// TestSetupLogger_LevelGating verifies setupLogger maps level strings to the
// slog level that actually gates output: a "warn" logger must drop Info
// records, and a "debug" logger must keep them. This goes beyond the existing
// non-nil smoke test by asserting the configured level takes effect.
func TestSetupLogger_LevelGating(t *testing.T) {
	// We cannot easily redirect the logger's writer (it hardcodes os.Stdout),
	// so assert on the handler's Enabled gate directly via a parallel handler
	// built with the same level mapping. Instead, exercise the public behavior:
	// the returned logger's handler reports the expected Enabled decisions.
	warn := setupLogger("warn")
	if warn.Handler().Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("warn logger should not enable Info level")
	}
	if !warn.Handler().Enabled(context.Background(), slog.LevelError) {
		t.Errorf("warn logger should enable Error level")
	}
	debug := setupLogger("debug")
	if !debug.Handler().Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("debug logger should enable Debug level")
	}
	// Unknown level falls through to Info: Info enabled, Debug not.
	def := setupLogger("gibberish")
	if !def.Handler().Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("default logger should enable Info level")
	}
	if def.Handler().Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("default (info) logger should not enable Debug level")
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957).
func (d *emptyDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}

// GetDependencies rounds out the stepDispatchDaemon surface so emptyDaemon can
// back a FormulaStepDispatcher in the backfill-sweep loop test (ka-0000006285).
func (d *emptyDaemon) GetDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *emptyDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *emptyDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}

// --- ka-0000001444: SSE Last-Event-ID durability wiring ---

// TestNewReconcilerState_EmptyPathNoState verifies that an unset STATE_FILE
// yields a nil StateManager and no error — the "no persistence configured"
// case, where the SSE stream stays volatile and the reconciler stays off.
func TestNewReconcilerState_EmptyPathNoState(t *testing.T) {
	sm, err := newReconcilerState("")
	if err != nil {
		t.Fatalf("newReconcilerState(\"\") returned error: %v", err)
	}
	if sm != nil {
		t.Fatalf("newReconcilerState(\"\") = %v, want nil", sm)
	}
}

// TestNewReconcilerState_LoadsAndRoundTrips verifies that a non-empty path
// returns a live StateManager that persists across a reopen — the property the
// durability fix depends on: both LastEventID (cause #1) and LastReconcileAt
// (cause #3) survive a process restart via the same file.
func TestNewReconcilerState_LoadsAndRoundTrips(t *testing.T) {
	path := t.TempDir() + "/trigger-engine-state.json"

	sm, err := newReconcilerState(path)
	if err != nil {
		t.Fatalf("newReconcilerState(%q) returned error: %v", path, err)
	}
	if sm == nil {
		t.Fatal("newReconcilerState(non-empty) = nil, want a StateManager")
	}
	if err := sm.SetLastEventID("evt-42"); err != nil {
		t.Fatalf("SetLastEventID: %v", err)
	}

	// Reopen from the same path — a fresh process reading the persisted file.
	reopened, err := newReconcilerState(path)
	if err != nil {
		t.Fatalf("reopen newReconcilerState(%q): %v", path, err)
	}
	if got := reopened.GetLastEventID(); got != "evt-42" {
		t.Errorf("GetLastEventID after reopen = %q, want %q", got, "evt-42")
	}
}

// TestNewReconcilerState_BadPathErrors verifies that a non-empty path that
// cannot be loaded is a hard error (not a silent nil) — booting with an empty
// state would defeat the durability guarantee, so main() os.Exit(1)s on it.
func TestNewReconcilerState_BadPathErrors(t *testing.T) {
	// A path whose parent is a regular file (not a dir) cannot be written and
	// its contents cannot be loaded as JSON — either way NewStateManager errors.
	f := t.TempDir() + "/afile"
	if err := os.WriteFile(f, []byte("not json"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := newReconcilerState(f + "/state.json"); err == nil {
		t.Fatal("newReconcilerState(path under a regular file) = nil error, want error")
	}
}

// TestBuildSSEStreamConfig_WiresState verifies buildSSEStreamConfig threads the
// shared StateManager onto SSEStreamConfig.State (cause #1) and keeps the
// lifecycle + edge topics both trigger sources drain. A non-nil State is what
// makes NewSSEStream restore + persist lastID across restarts.
func TestBuildSSEStreamConfig_WiresState(t *testing.T) {
	t.Setenv("BEADS_DAEMON_TOKEN", "tok-xyz")
	cfg := &config{beadsHTTPAddr: "http://beads:8080"}
	sm, err := newReconcilerState(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("newReconcilerState: %v", err)
	}

	got := buildSSEStreamConfig(cfg, sm, nil, discardLogger())

	if got.State != sm {
		t.Errorf("SSEStreamConfig.State = %v, want the shared StateManager %v", got.State, sm)
	}
	if got.BeadsHTTPAddr != "http://beads:8080" {
		t.Errorf("BeadsHTTPAddr = %q, want %q", got.BeadsHTTPAddr, "http://beads:8080")
	}
	if got.Token != "tok-xyz" {
		t.Errorf("Token = %q, want %q", got.Token, "tok-xyz")
	}
	// Must subscribe to the three bead lifecycle topics the bead-state +
	// multi sources drain; a dropped topic would silently stop firing triggers.
	for _, want := range []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"} {
		found := false
		for _, tp := range got.Topics {
			if tp == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Topics missing %q; got %v", want, got.Topics)
		}
	}
}

// TestBuildSSEStreamConfig_NilStateVolatile verifies that with no persistence
// configured (nil state) the config carries a nil State — preserving the
// pre-existing volatile SSE behaviour, so the fix is strictly opt-in.
func TestBuildSSEStreamConfig_NilStateVolatile(t *testing.T) {
	cfg := &config{beadsHTTPAddr: "http://beads:8080"}
	got := buildSSEStreamConfig(cfg, nil, nil, discardLogger())
	if got.State != nil {
		t.Errorf("SSEStreamConfig.State = %v with nil state, want nil", got.State)
	}
}
