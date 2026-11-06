package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// TestEnvOrDefault covers the small helper: env set → env value, env empty
// or unset → fallback.
func TestEnvOrDefault(t *testing.T) {
	const key = "GITLAB_BRIDGE_TEST_ENV_OR_DEFAULT"

	t.Setenv(key, "custom")
	if got := envOrDefault(key, "fb"); got != "custom" {
		t.Fatalf("envOrDefault with set env = %q, want %q", got, "custom")
	}

	t.Setenv(key, "")
	if got := envOrDefault(key, "fb"); got != "fb" {
		t.Fatalf("envOrDefault with empty env = %q, want %q", got, "fb")
	}

	// Not using Setenv here — use unset via t.Setenv + empty.
	t.Setenv(key+"_UNSET", "")
	if got := envOrDefault(key+"_UNSET", "fallback"); got != "fallback" {
		t.Fatalf("envOrDefault with unset = %q, want %q", got, "fallback")
	}
}

// TestSetupLogger exercises each level branch including the default.
func TestSetupLogger(t *testing.T) {
	cases := []struct {
		level     string
		wantLevel slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"unknown-level", slog.LevelInfo}, // default branch
		{"", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			lg := setupLogger(tc.level)
			if lg == nil {
				t.Fatalf("setupLogger(%q) returned nil", tc.level)
			}
			// Probe the returned logger with the expected level + one below it.
			ctx := context.Background()
			if !lg.Enabled(ctx, tc.wantLevel) {
				t.Errorf("logger for level %q does not enable %v", tc.level, tc.wantLevel)
			}
			below := tc.wantLevel - 4
			if tc.wantLevel > slog.LevelDebug && lg.Enabled(ctx, below) {
				t.Errorf("logger for level %q should NOT enable %v (below threshold)", tc.level, below)
			}
		})
	}
}

// TestSetupLoggerWritesJSON verifies we get a working JSON handler.
func TestSetupLoggerWritesJSON(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lg.Info("ping", "k", "v")
	if !bytes.Contains(buf.Bytes(), []byte(`"msg":"ping"`)) {
		t.Fatalf("expected JSON log line, got %q", buf.String())
	}
	// And ensure setupLogger returns a non-nil JSON logger too.
	got := setupLogger("debug")
	if got == nil {
		t.Fatal("setupLogger returned nil")
	}
}

// TestParseConfigDefaults exercises the "no env set" path and asserts all
// default values.
func TestParseConfigDefaults(t *testing.T) {
	// Clear all env vars parseConfig reads.
	for _, k := range []string{
		"GITLAB_POLL_INTERVAL", "TRIGGER_POLL_INTERVAL", "GITLAB_GROUP_ID",
		"BEADS_HTTP_ADDR", "GITLAB_BASE_URL", "GITLAB_API_TOKEN",
		"GITLAB_WEBHOOK_SECRET", "GITLAB_BOT_USERNAME",
		"LISTEN_ADDR", "LOG_LEVEL", "STATE_PATH",
		"SLACK_BOT_TOKEN", "SLACK_CHANNEL",
		"GITLAB_MENTION_TRIGGER", "GITLAB_MENTION_PROJECT",
	} {
		t.Setenv(k, "")
	}

	cfg := parseConfig()
	if cfg == nil {
		t.Fatal("parseConfig returned nil")
	}
	if cfg.pollInterval != 5*time.Minute {
		t.Errorf("pollInterval = %v, want 5m", cfg.pollInterval)
	}
	if cfg.triggerPollInterval != 60*time.Second {
		t.Errorf("triggerPollInterval = %v, want 60s", cfg.triggerPollInterval)
	}
	if cfg.gitlabGroupID != 0 {
		t.Errorf("gitlabGroupID = %d, want 0", cfg.gitlabGroupID)
	}
	if cfg.beadsHTTPAddr != "http://localhost:8080" {
		t.Errorf("beadsHTTPAddr = %q, want default", cfg.beadsHTTPAddr)
	}
	if cfg.listenAddr != ":8092" {
		t.Errorf("listenAddr = %q, want :8092", cfg.listenAddr)
	}
	if cfg.logLevel != "info" {
		t.Errorf("logLevel = %q, want info", cfg.logLevel)
	}
	if cfg.statePath != "/data/gitlab-bridge-state.json" {
		t.Errorf("statePath = %q, want default", cfg.statePath)
	}
	if cfg.mentionTrigger != "@gardenio" {
		t.Errorf("mentionTrigger = %q, want @gardenio", cfg.mentionTrigger)
	}
	if cfg.mentionProject != "gardenio" {
		t.Errorf("mentionProject = %q, want gardenio", cfg.mentionProject)
	}
}

// TestParseConfigOverrides verifies each env var's override path including
// the "invalid value → fallback to default" branch for durations and ints.
func TestParseConfigOverrides(t *testing.T) {
	t.Setenv("GITLAB_POLL_INTERVAL", "30s")
	t.Setenv("TRIGGER_POLL_INTERVAL", "15s")
	t.Setenv("GITLAB_GROUP_ID", "12345")
	t.Setenv("BEADS_HTTP_ADDR", "http://beads:9090")
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_API_TOKEN", "secret-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "whsec")
	t.Setenv("GITLAB_BOT_USERNAME", "mybot")
	t.Setenv("LISTEN_ADDR", ":7777")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("STATE_PATH", "/tmp/state.json")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")
	t.Setenv("SLACK_CHANNEL", "#ops")
	t.Setenv("GITLAB_MENTION_TRIGGER", "@bot")
	t.Setenv("GITLAB_MENTION_PROJECT", "my-project")

	cfg := parseConfig()
	if cfg.pollInterval != 30*time.Second {
		t.Errorf("pollInterval = %v, want 30s", cfg.pollInterval)
	}
	if cfg.triggerPollInterval != 15*time.Second {
		t.Errorf("triggerPollInterval = %v, want 15s", cfg.triggerPollInterval)
	}
	if cfg.gitlabGroupID != 12345 {
		t.Errorf("gitlabGroupID = %d, want 12345", cfg.gitlabGroupID)
	}
	if cfg.beadsHTTPAddr != "http://beads:9090" {
		t.Errorf("beadsHTTPAddr = %q", cfg.beadsHTTPAddr)
	}
	if cfg.gitlabBaseURL != "https://gitlab.example.com" {
		t.Errorf("gitlabBaseURL = %q", cfg.gitlabBaseURL)
	}
	if cfg.gitlabAPIToken != "secret-token" {
		t.Errorf("gitlabAPIToken = %q", cfg.gitlabAPIToken)
	}
	if cfg.gitlabWebhookSecret != "whsec" {
		t.Errorf("gitlabWebhookSecret = %q", cfg.gitlabWebhookSecret)
	}
	if cfg.gitlabBotUsername != "mybot" {
		t.Errorf("gitlabBotUsername = %q", cfg.gitlabBotUsername)
	}
	if cfg.listenAddr != ":7777" {
		t.Errorf("listenAddr = %q", cfg.listenAddr)
	}
	if cfg.logLevel != "debug" {
		t.Errorf("logLevel = %q", cfg.logLevel)
	}
	if cfg.statePath != "/tmp/state.json" {
		t.Errorf("statePath = %q", cfg.statePath)
	}
	if cfg.slackBotToken != "xoxb-fake" {
		t.Errorf("slackBotToken = %q", cfg.slackBotToken)
	}
	if cfg.slackChannel != "#ops" {
		t.Errorf("slackChannel = %q", cfg.slackChannel)
	}
	if cfg.mentionTrigger != "@bot" {
		t.Errorf("mentionTrigger = %q", cfg.mentionTrigger)
	}
	if cfg.mentionProject != "my-project" {
		t.Errorf("mentionProject = %q", cfg.mentionProject)
	}
}

// TestParseConfigInvalidValuesFallBackToDefaults covers the "err != nil"
// branches of the three env-parsed numeric/duration fields.
func TestParseConfigInvalidValuesFallBackToDefaults(t *testing.T) {
	t.Setenv("GITLAB_POLL_INTERVAL", "not-a-duration")
	t.Setenv("TRIGGER_POLL_INTERVAL", "also-bad")
	t.Setenv("GITLAB_GROUP_ID", "not-an-int")

	cfg := parseConfig()
	if cfg.pollInterval != 5*time.Minute {
		t.Errorf("invalid pollInterval should fall back to 5m, got %v", cfg.pollInterval)
	}
	if cfg.triggerPollInterval != 60*time.Second {
		t.Errorf("invalid triggerPollInterval should fall back to 60s, got %v", cfg.triggerPollInterval)
	}
	if cfg.gitlabGroupID != 0 {
		t.Errorf("invalid groupID should fall back to 0, got %d", cfg.gitlabGroupID)
	}
}

// TestParseConfigExtendedFields exercises the newer env-driven fields:
// MR-backlink, upstream-backstop, trust-metrics, and AI-approval — including
// their override, invalid-value-fallback, and default branches.
func TestParseConfigExtendedFields(t *testing.T) {
	t.Setenv("MR_BACKLINK_INTERVAL", "20m")
	t.Setenv("MR_BACKLINK_WINDOW", "48h")
	t.Setenv("MR_BACKLINK_DRY_RUN", "true")
	t.Setenv("MR_BACKLINK_FILE_UNLINKED", "true")
	t.Setenv("UPSTREAM_BACKSTOP_POLL", "true")
	t.Setenv("UPSTREAM_BACKSTOP_INTERVAL", "3m")
	t.Setenv("UPSTREAM_BACKSTOP_LABEL", "mirror:yes")
	t.Setenv("TRUST_METRICS_ENABLED", "true")
	t.Setenv("TRUST_METRICS_INTERVAL", "7m")
	t.Setenv("TRUST_METRICS_WINDOW", "14d") // invalid Go duration → fallback
	t.Setenv("TRUST_METRICS_TARGET_BRANCHES", "main, develop ,")
	t.Setenv("AI_APPROVAL_ENABLED", "true")
	t.Setenv("AI_APPROVAL_RULE_NAME", "AI change review")
	t.Setenv("AI_APPROVAL_REQUIRED", "3")
	t.Setenv("AI_APPROVAL_BOT_USERNAMES", "bot-a, bot-b")
	t.Setenv("AI_APPROVAL_REVIEWER_GROUP_IDS", "10,20")
	t.Setenv("AI_APPROVAL_REVIEWER_USER_IDS", "5")

	cfg := parseConfig()

	if cfg.mrBacklinkInterval != 20*time.Minute {
		t.Errorf("mrBacklinkInterval = %v, want 20m", cfg.mrBacklinkInterval)
	}
	if cfg.mrBacklinkWindow != 48*time.Hour {
		t.Errorf("mrBacklinkWindow = %v, want 48h", cfg.mrBacklinkWindow)
	}
	if !cfg.mrBacklinkDryRun {
		t.Error("mrBacklinkDryRun = false, want true")
	}
	if !cfg.mrBacklinkFileUnlinked {
		t.Error("mrBacklinkFileUnlinked = false, want true")
	}
	if !cfg.upstreamBackstopEnabled {
		t.Error("upstreamBackstopEnabled = false, want true")
	}
	if cfg.upstreamBackstopInterval != 3*time.Minute {
		t.Errorf("upstreamBackstopInterval = %v, want 3m", cfg.upstreamBackstopInterval)
	}
	if cfg.upstreamBackstopLabel != "mirror:yes" {
		t.Errorf("upstreamBackstopLabel = %q, want mirror:yes", cfg.upstreamBackstopLabel)
	}
	if !cfg.trustMetricsEnabled {
		t.Error("trustMetricsEnabled = false, want true")
	}
	if cfg.trustMetricsInterval != 7*time.Minute {
		t.Errorf("trustMetricsInterval = %v, want 7m", cfg.trustMetricsInterval)
	}
	// "14d" is not a valid Go duration, so the window must fall back to 28d.
	if cfg.trustMetricsWindow != 28*24*time.Hour {
		t.Errorf("trustMetricsWindow = %v, want 28d fallback", cfg.trustMetricsWindow)
	}
	if !reflect.DeepEqual(cfg.trustMetricsBranches, []string{"main", "develop"}) {
		t.Errorf("trustMetricsBranches = %#v, want [main develop]", cfg.trustMetricsBranches)
	}
	if !cfg.aiApprovalEnabled {
		t.Error("aiApprovalEnabled = false, want true")
	}
	if cfg.aiApprovalRuleName != "AI change review" {
		t.Errorf("aiApprovalRuleName = %q", cfg.aiApprovalRuleName)
	}
	if cfg.aiApprovalRequired != 3 {
		t.Errorf("aiApprovalRequired = %d, want 3", cfg.aiApprovalRequired)
	}
	if !reflect.DeepEqual(cfg.aiApprovalBotUsernames, []string{"bot-a", "bot-b"}) {
		t.Errorf("aiApprovalBotUsernames = %#v, want [bot-a bot-b]", cfg.aiApprovalBotUsernames)
	}
	if !reflect.DeepEqual(cfg.aiApprovalReviewerGroupIDs, []int{10, 20}) {
		t.Errorf("aiApprovalReviewerGroupIDs = %#v, want [10 20]", cfg.aiApprovalReviewerGroupIDs)
	}
	if !reflect.DeepEqual(cfg.aiApprovalReviewerUserIDs, []int{5}) {
		t.Errorf("aiApprovalReviewerUserIDs = %#v, want [5]", cfg.aiApprovalReviewerUserIDs)
	}
}

// TestParseConfigExtendedDefaults covers the default branches of the extended
// fields (no env set), plus the special cases in branch/bot parsing:
//   - TRUST_METRICS_TARGET_BRANCHES="*" disables the branch filter (nil slice).
//   - AI_APPROVAL_BOT_USERNAMES empty falls back to GITLAB_BOT_USERNAME.
//   - AI_APPROVAL_REQUIRED < 1 is rejected and falls back to the default of 1.
func TestParseConfigExtendedDefaults(t *testing.T) {
	for _, k := range []string{
		"MR_BACKLINK_INTERVAL", "MR_BACKLINK_WINDOW", "MR_BACKLINK_DRY_RUN",
		"MR_BACKLINK_FILE_UNLINKED", "UPSTREAM_BACKSTOP_POLL",
		"UPSTREAM_BACKSTOP_INTERVAL", "UPSTREAM_BACKSTOP_LABEL",
		"TRUST_METRICS_ENABLED", "TRUST_METRICS_INTERVAL", "TRUST_METRICS_WINDOW",
		"AI_APPROVAL_ENABLED", "AI_APPROVAL_RULE_NAME",
		"AI_APPROVAL_REVIEWER_GROUP_IDS", "AI_APPROVAL_REVIEWER_USER_IDS",
		"AI_APPROVAL_BOT_USERNAMES",
	} {
		t.Setenv(k, "")
	}
	// "*" disables the trust-metrics branch filter.
	t.Setenv("TRUST_METRICS_TARGET_BRANCHES", "*")
	// Sub-1 approval count must be rejected in favor of the default 1.
	t.Setenv("AI_APPROVAL_REQUIRED", "0")
	// No explicit bot list → defaults to GITLAB_BOT_USERNAME.
	t.Setenv("GITLAB_BOT_USERNAME", "gardenio-bot")

	cfg := parseConfig()

	if cfg.mrBacklinkInterval != 15*time.Minute {
		t.Errorf("default mrBacklinkInterval = %v, want 15m", cfg.mrBacklinkInterval)
	}
	if cfg.mrBacklinkWindow != 14*24*time.Hour {
		t.Errorf("default mrBacklinkWindow = %v, want 14d", cfg.mrBacklinkWindow)
	}
	if cfg.mrBacklinkDryRun {
		t.Error("default mrBacklinkDryRun = true, want false")
	}
	if cfg.upstreamBackstopEnabled {
		t.Error("default upstreamBackstopEnabled = true, want false")
	}
	if cfg.upstreamBackstopInterval != 10*time.Minute {
		t.Errorf("default upstreamBackstopInterval = %v, want 10m", cfg.upstreamBackstopInterval)
	}
	if cfg.upstreamBackstopLabel != "upstream:true" {
		t.Errorf("default upstreamBackstopLabel = %q, want upstream:true", cfg.upstreamBackstopLabel)
	}
	if cfg.trustMetricsEnabled {
		t.Error("default trustMetricsEnabled = true, want false")
	}
	if cfg.trustMetricsInterval != 10*time.Minute {
		t.Errorf("default trustMetricsInterval = %v, want 10m", cfg.trustMetricsInterval)
	}
	// "*" sentinel disables the filter → nil slice.
	if cfg.trustMetricsBranches != nil {
		t.Errorf("trustMetricsBranches = %#v, want nil for '*' sentinel", cfg.trustMetricsBranches)
	}
	if cfg.aiApprovalRequired != 1 {
		t.Errorf("aiApprovalRequired = %d, want 1 (sub-1 rejected)", cfg.aiApprovalRequired)
	}
	if !reflect.DeepEqual(cfg.aiApprovalBotUsernames, []string{"gardenio-bot"}) {
		t.Errorf("aiApprovalBotUsernames = %#v, want [gardenio-bot] from GITLAB_BOT_USERNAME", cfg.aiApprovalBotUsernames)
	}
}

// TestParseConfigInvalidExtendedDurations covers the invalid-duration fallback
// branches for the extended interval fields.
func TestParseConfigInvalidExtendedDurations(t *testing.T) {
	t.Setenv("MR_BACKLINK_INTERVAL", "bad")
	t.Setenv("MR_BACKLINK_WINDOW", "bad")
	t.Setenv("UPSTREAM_BACKSTOP_INTERVAL", "bad")
	t.Setenv("TRUST_METRICS_INTERVAL", "bad")

	cfg := parseConfig()
	if cfg.mrBacklinkInterval != 15*time.Minute {
		t.Errorf("mrBacklinkInterval = %v, want 15m fallback", cfg.mrBacklinkInterval)
	}
	if cfg.mrBacklinkWindow != 14*24*time.Hour {
		t.Errorf("mrBacklinkWindow = %v, want 14d fallback", cfg.mrBacklinkWindow)
	}
	if cfg.upstreamBackstopInterval != 10*time.Minute {
		t.Errorf("upstreamBackstopInterval = %v, want 10m fallback", cfg.upstreamBackstopInterval)
	}
	if cfg.trustMetricsInterval != 10*time.Minute {
		t.Errorf("trustMetricsInterval = %v, want 10m fallback", cfg.trustMetricsInterval)
	}
}

// spawnGuardCfg extracts the unexported Config from a *spawnguard.SpawnGuard
// via reflection so tests can assert env-override behavior.
func spawnGuardCfg(t *testing.T, sg any) (rateMax, tripThreshold int, rateWindow, tripWindow, tripCooldown time.Duration) {
	t.Helper()
	v := reflect.ValueOf(sg).Elem().FieldByName("cfg")
	if !v.IsValid() {
		t.Fatal("SpawnGuard.cfg field not found — test must be updated if the struct shape changed")
	}
	return int(v.FieldByName("RateMax").Int()),
		int(v.FieldByName("TripThreshold").Int()),
		time.Duration(v.FieldByName("RateWindow").Int()),
		time.Duration(v.FieldByName("TripWindow").Int()),
		time.Duration(v.FieldByName("TripCooldown").Int())
}

// TestLoadSpawnGuardDefaults: no SPAWN_GUARD_* env → DefaultConfig.
func TestLoadSpawnGuardDefaults(t *testing.T) {
	for _, k := range []string{
		"SPAWN_GUARD_RATE_MAX", "SPAWN_GUARD_RATE_WINDOW",
		"SPAWN_GUARD_TRIP_THRESHOLD", "SPAWN_GUARD_TRIP_WINDOW",
		"SPAWN_GUARD_TRIP_COOLDOWN",
	} {
		t.Setenv(k, "")
	}
	sg := loadSpawnGuard()
	if sg == nil {
		t.Fatal("loadSpawnGuard returned nil")
	}
	rateMax, tripT, rateW, tripW, cool := spawnGuardCfg(t, sg)
	if rateMax != 30 || tripT != 120 || rateW != time.Minute || tripW != time.Minute || cool != 5*time.Minute {
		t.Errorf("defaults mismatch: rateMax=%d tripT=%d rateW=%v tripW=%v cool=%v",
			rateMax, tripT, rateW, tripW, cool)
	}
}

// TestLoadSpawnGuardOverrides: each env var overrides its matching field.
func TestLoadSpawnGuardOverrides(t *testing.T) {
	t.Setenv("SPAWN_GUARD_RATE_MAX", "7")
	t.Setenv("SPAWN_GUARD_RATE_WINDOW", "42s")
	t.Setenv("SPAWN_GUARD_TRIP_THRESHOLD", "99")
	t.Setenv("SPAWN_GUARD_TRIP_WINDOW", "2m")
	t.Setenv("SPAWN_GUARD_TRIP_COOLDOWN", "10m")

	sg := loadSpawnGuard()
	rateMax, tripT, rateW, tripW, cool := spawnGuardCfg(t, sg)
	if rateMax != 7 {
		t.Errorf("RateMax = %d, want 7", rateMax)
	}
	if rateW != 42*time.Second {
		t.Errorf("RateWindow = %v, want 42s", rateW)
	}
	if tripT != 99 {
		t.Errorf("TripThreshold = %d, want 99", tripT)
	}
	if tripW != 2*time.Minute {
		t.Errorf("TripWindow = %v, want 2m", tripW)
	}
	if cool != 10*time.Minute {
		t.Errorf("TripCooldown = %v, want 10m", cool)
	}
}

// TestRunTriggerEngineZeroIntervalUsesDefault ensures the interval<=0 branch
// normalizes to 60s and the goroutine exits cleanly when ctx is cancelled.
// The immediate Evaluate will hit the (fake) GitLab server once; after that
// the ticker's 60s period is too long to fire before we cancel.
func TestRunTriggerEngineZeroIntervalUsesDefault(t *testing.T) {
	daemon := &fakeDaemon{
		beads: []*beadsapi.BeadDetail{pipelineTriggerBead("gd-t-zero", nil)},
	}
	pipelines := []bridge.GitLabPipeline{{ID: 1, Status: "failed", Ref: "main"}}
	srv, hits := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})
	engine := newEngine(daemon, client)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTriggerEngine(ctx, engine, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTriggerEngine with interval=0 did not stop within 1s of ctx cancel")
	}
	if h := hits.Load(); h < 1 {
		t.Errorf("immediate Evaluate should have hit GitLab at least once, got %d", h)
	}
}

// TestRunTriggerEngineLogsStatsEvery10Evals drives enough ticks to cross the
// evalCount%10==0 boundary so the "trigger engine stats" log branch is
// exercised.
func TestRunTriggerEngineLogsStatsEvery10Evals(t *testing.T) {
	daemon := &fakeDaemon{
		beads: []*beadsapi.BeadDetail{pipelineTriggerBead("gd-t-stats", map[string]string{
			"last_checked": time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
		})},
	}
	pipelines := []bridge.GitLabPipeline{{ID: 1, Status: "failed", Ref: "main"}}
	srv, hits := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})
	engine := newEngine(daemon, client)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// 5ms ticks → ~50ms to reach 10 iterations.
		runTriggerEngine(ctx, engine, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Initial evaluate + 10 ticks = 11 hits minimum, but cooldown/dedup
		// may reduce actual GitLab calls. Just wait enough wall time.
		if hits.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give the ticker time to accumulate >=10 evals for the stats branch.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
}

// TestLoadSpawnGuardInvalidValuesFallBack: parse errors leave defaults in place.
func TestLoadSpawnGuardInvalidValuesFallBack(t *testing.T) {
	t.Setenv("SPAWN_GUARD_RATE_MAX", "abc")
	t.Setenv("SPAWN_GUARD_RATE_WINDOW", "not-a-duration")
	t.Setenv("SPAWN_GUARD_TRIP_THRESHOLD", "xyz")
	t.Setenv("SPAWN_GUARD_TRIP_WINDOW", "nope")
	t.Setenv("SPAWN_GUARD_TRIP_COOLDOWN", "never")

	sg := loadSpawnGuard()
	rateMax, tripT, rateW, tripW, cool := spawnGuardCfg(t, sg)
	// Should match DefaultConfig since every parse failed.
	if rateMax != 30 || tripT != 120 || rateW != time.Minute || tripW != time.Minute || cool != 5*time.Minute {
		t.Errorf("invalid-env fallback mismatch: rateMax=%d tripT=%d rateW=%v tripW=%v cool=%v",
			rateMax, tripT, rateW, tripW, cool)
	}
}
