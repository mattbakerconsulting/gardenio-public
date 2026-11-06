package main

import (
	"log/slog"
	"os"
	"strings"

	"gardenio/controller/internal/assistantturnminter"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/coopmuxapi"
)

// startAssistantTurnMinter builds and starts the always-on plan-transcript
// assistant-turn minter in the controller process (task ka-0000006891, P3 of
// plan ka-0000000187). It was rehomed here off the beads-viewer so plan
// transcripts persist for the whole cluster lifetime — independent of whether an
// operator has a viewer/cockpit open, and surviving viewer restarts.
//
// It subscribes to coop.events.messages.*.assistant_text (live) plus a
// full-tail JetStream replay of MESSAGE_EVENTS, and mints a durable type=message
// turn bead for each plan-assistant reply. Nil-safe / best-effort throughout:
//   - Returns a nil minter (persistence disabled) when NATS is unconfigured.
//   - A connect failure is logged and swallowed — the controller must keep
//     running every other loop even when NATS is down.
//   - coopmux is optional: without it a >256KB reply keeps its clipped wire copy
//     (marked truncated) instead of hydrating the full body.
//
// The returned minter's Close should be deferred by the caller so the background
// replay goroutine and NATS connection unwind on shutdown.
func startAssistantTurnMinter(cfg *config.Config, daemon *beadsapi.Client, logger *slog.Logger) *assistantturnminter.Minter {
	log := logger.With("component", "assistant-turn-minter")
	if cfg.NatsURL == "" {
		log.Info("assistant-turn-minter: NATS_URL unset — plan assistant turns will not persist")
		return nil
	}

	// coopmux client for the >256KB full-text hydration fallback. Optional: an
	// empty COOPMUX_URL yields a disabled client, which the minter tolerates
	// (keeps the clipped copy, marks it truncated).
	coopmuxClient := coopmuxapi.New(coopmuxapi.Config{
		BaseURL: cfg.CoopmuxURL,
		Token:   os.Getenv("COOP_MUX_AUTH_TOKEN"),
	})

	// The minter subscribes to and JetStream-consumes coop.events.messages.>.
	// The controller runs under the NATS `infra` user (subscribe: ">"), so it can
	// read the fleet-wide MESSAGE_EVENTS stream. Mirror the rotation/credsync
	// notifiers' token resolution: prefer COOP_NATS_TOKEN, else the controller's
	// own infra NATS_TOKEN/NATS_USER (the chart sets NATS_USER=infra + the infra
	// token; it never sets COOP_NATS_TOKEN on the controller pod).
	natsToken := os.Getenv("COOP_NATS_TOKEN")
	natsUser := os.Getenv("COOP_NATS_USER")
	if strings.TrimSpace(natsToken) == "" {
		natsToken = cfg.NatsToken
		natsUser = cfg.NatsUser
	}

	minter := assistantturnminter.New(cfg.NatsURL, natsToken, natsUser, daemon, coopmuxClient, log)
	if minter == nil {
		log.Info("assistant-turn-minter: disabled")
		return nil
	}
	if err := minter.Start(); err != nil {
		log.Warn("assistant-turn-minter: failed to start — plan assistant turns will not persist", "error", err)
		return nil
	}
	log.Info("assistant-turn-minter: started — persisting plan assistant turns")
	return minter
}
