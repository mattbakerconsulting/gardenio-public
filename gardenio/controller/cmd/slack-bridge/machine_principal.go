package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// Machine principal for slack-bridge (gd-Hc34EENbO9 component 4, decision
// gd-ZsfxvXRiyv "tight, amended").
//
// The bridge keeps its fleet token (bridge-tokens/slack-bridge) for everything
// it does today, and holds NO operated-by relay capability: a person's Slack
// writes ride that person's own Keycloak token through account linking
// (gd-OZJ6H2S3xx), so the machine principal never acts for a human. The
// machine principal (BEADS_OIDC_*, rendered by templates/slack-bridge/
// deployment.yaml when slackBridge.machinePrincipal.enabled) carries exactly
// three kinds of background write, per call (beadsapi.WithMachinePrincipal):
//
//   - the startup config seed (EnsureSlackBridgeConfigs, role
//     gardenio-machine-slack-config-seed: the dm/message/mail/decision/
//     escalation types and their operator-queue views);
//   - the dm delivery writes (DMDelivery, role gardenio-machine-slack-dm-delivery:
//     the type=dm ownership carve-out a fleet token has, typed to dm);
//   - the permission bead create and verdict close (Bot.SetMachinePrincipal,
//     the same role: type:permission is admin-gated, gd-RuI4Ze6vAg).
//
// Everything else (thread bookkeeping, decisions, escalations, agents, admin
// routes) and every read stays on the fleet token. A token fetch failure fails
// the machine write closed (auth_unavailable); the fleet token is never
// substituted for it. With BEADS_OIDC_* unset the bridge behaves as before.

// seedBackoffMin/Max bound the delay between seed retries (auth_unavailable or 403).
const (
	seedBackoffMin = time.Second
	seedBackoffMax = 30 * time.Second
)

// seedSleep waits d or until ctx ends. Tests replace it.
var seedSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// seedConfigs registers the bridge's config entries.
//
// Without a machine principal it is today's behavior: EnsureConfigs on the
// daemon's own token, once, synchronously, failures logged and non-fatal.
//
// With one, it runs EnsureSlackBridgeConfigs as the machine principal in a
// goroutine, so a Keycloak that is still reconciling the new client on the
// enabling roll (gd-pj5hT3V1la: invalid_client two seconds before the client
// existed) never delays the listener: it waits for the first token, then
// retries the seed with backoff while the failure is auth_unavailable or a 403 (the
// first-roll role race). Any other daemon 4xx is a real answer and is logged, not retried. The goroutine ends when ctx
// does.
func seedConfigs(ctx context.Context, daemon bridge.ConfigSetter, machine beadsapi.TokenSource, logger *slog.Logger) {
	if machine == nil {
		if err := bridge.EnsureConfigs(ctx, daemon, logger); err != nil {
			logger.Warn("failed to ensure beads configs (non-fatal)", "error", err)
		}
		return
	}
	go func() {
		if err := seedConfigsAsMachine(ctx, daemon, machine, logger); err != nil {
			logger.Warn("beads config seed abandoned", "error", err)
		}
	}()
}

// seedRetryable reports whether a seed failure is the credential not being
// ready yet, so the next attempt may succeed: no token (auth_unavailable), or a
// 403 because Keycloak had not yet attached the config-seed role to the service
// account when the first token was minted (first-roll role race; the client
// re-acquires its token on a 403, so the retry carries the fresh one). Any
// other daemon 4xx is a real answer and is not retried.
func seedRetryable(err error) bool {
	if beadsapi.IsAuthUnavailable(err) {
		return true
	}
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden
}

// seedConfigsAsMachine is the blocking body of the machine-principal seed.
func seedConfigsAsMachine(ctx context.Context, daemon bridge.ConfigSetter, machine beadsapi.TokenSource, logger *slog.Logger) error {
	logger.Info("waiting for the first machine token before seeding")
	if err := beadsapi.WarmMachineToken(ctx, machine); err != nil {
		return err
	}
	mctx := beadsapi.WithMachinePrincipal(ctx, machine)
	delay := seedBackoffMin
	for attempt := 1; ; attempt++ {
		err := bridge.EnsureSlackBridgeConfigs(mctx, daemon, logger)
		if err == nil {
			if attempt > 1 {
				logger.Info("ensure beads configs succeeded after retry", "attempts", attempt)
			}
			return nil
		}
		if !seedRetryable(err) {
			logger.Warn("failed to ensure beads configs (non-fatal)", "error", err)
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("ensure beads configs abandoned after %d attempt(s): %w", attempt, err)
		}
		logger.Warn("ensure beads configs failed: machine credential unavailable or not yet authorized (403); retrying",
			"attempt", attempt, "retry_in", delay, "error", err)
		if serr := seedSleep(ctx, delay); serr != nil {
			return fmt.Errorf("ensure beads configs abandoned after %d attempt(s): %w", attempt, err)
		}
		if delay *= 2; delay > seedBackoffMax {
			delay = seedBackoffMax
		}
	}
}
