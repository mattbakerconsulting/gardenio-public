package main

import (
	"log/slog"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/toollatency"
)

// startToolLatencyConsumer builds and starts the always-on P2 HOOK_EVENTS
// consumer (plan gardenio/plans/agent-tool-latency-attribution.md, bead
// gd-sWwPnwkZtw): a durable JetStream consumer that turns every agent
// PostToolUse hook event into bounded per-tool/per-cmd-class Prometheus
// histogram samples (the metric plane) plus, for calls over
// cfg.ToolLatencySlowThreshold, a redacted full-argv slow_tool_calls ledger
// row (the record plane).
//
// Nil-safe / best-effort, mirroring startAssistantTurnMinter:
//   - Returns a nil consumer (tool-latency attribution disabled) when NATS is
//     unconfigured.
//   - A connect/subscribe failure is logged and swallowed — the controller
//     must keep running every other loop even when NATS is down.
//
// The returned consumer's Close should be deferred by the caller so the
// JetStream subscription and NATS connection unwind on shutdown.
func startToolLatencyConsumer(cfg *config.Config, daemon *beadsapi.Client, toolMetrics *gardeniometrics.ToolLatencyMetrics, logger *slog.Logger) *toollatency.Consumer {
	log := logger.With("component", "tool-latency-consumer")
	if cfg.NatsURL == "" {
		log.Info("tool-latency-consumer: NATS_URL unset — tool latency will not be attributed")
		return nil
	}

	// Same token/user resolution as the assistant-turn minter and the
	// rotation notifier: prefer COOP_NATS_TOKEN, else the controller's own
	// infra NATS_TOKEN/NATS_USER (the chart sets NATS_USER=infra + the infra
	// token; it never sets COOP_NATS_TOKEN on the controller pod).
	natsToken := os.Getenv("COOP_NATS_TOKEN")
	natsUser := os.Getenv("COOP_NATS_USER")
	if strings.TrimSpace(natsToken) == "" {
		natsToken = cfg.NatsToken
		natsUser = cfg.NatsUser
	}

	consumer := toollatency.New(cfg.NatsURL, natsToken, natsUser, daemon, toolMetrics, cfg.ToolLatencySlowThreshold, log)
	if consumer == nil {
		log.Info("tool-latency-consumer: disabled")
		return nil
	}
	if err := consumer.Start(); err != nil {
		log.Warn("tool-latency-consumer: failed to start — tool latency will not be attributed", "error", err)
		return nil
	}
	log.Info("tool-latency-consumer: started — attributing agent tool-call latency")
	return consumer
}
