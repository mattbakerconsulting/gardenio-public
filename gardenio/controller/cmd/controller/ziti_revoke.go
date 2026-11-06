package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/config"
)

// OpenZiti lease revoke-on-exit (epic ka-0000000990).
//
// When an agent reaches a terminal state, the controller releases the pod's
// task-scoped OpenZiti lease early by calling DELETE /lease?task_id=<beadID> on
// the ziti-access-broker. The broker's contract makes task_id the durable
// correlation key, so the controller does NOT need to persist the opaque
// server-minted lease_id — it revokes by the agent bead ID it already owns.
//
// The controller authenticates with its OWN projected broker-audience token
// (ZitiAccessControllerTokenFile), and must be listed in the broker's
// REVOKER_SUBJECTS to revoke leases minted by agent service accounts. This is
// best-effort: a failure (broker down, token missing) is logged but never
// blocks teardown — the lease TTL is the hard backstop that bounds overlay
// access even if this revoke never lands.

// zitiRevokeTimeout bounds the revoke HTTP call so a slow/hung broker cannot
// stall the teardown path.
const zitiRevokeTimeout = 10 * time.Second

// revokeZitiLease releases the OpenZiti lease(s) for taskID by calling
// DELETE /lease?task_id=<taskID> on the broker. It is a no-op (returns nil)
// when revoke-on-exit is not configured, so callers can invoke it
// unconditionally on every terminal transition.
//
// Best-effort by design: it logs and returns an error rather than propagating
// it into teardown control flow. The caller should not fail teardown on a
// revoke error.
func revokeZitiLease(ctx context.Context, logger *slog.Logger, cfg *config.Config, taskID string) error {
	// Master switch + opt-out: no broker, revoke disabled, or no controller
	// token → nothing to do. A cluster without a broker never reaches here with
	// a populated config, so this also makes the call safe on every namespace.
	if cfg.ZitiAccessBrokerURL == "" || !cfg.ZitiAccessRevokeOnExit || cfg.ZitiAccessControllerTokenFile == "" {
		return nil
	}
	if strings.TrimSpace(taskID) == "" {
		return nil
	}

	token, err := os.ReadFile(cfg.ZitiAccessControllerTokenFile)
	if err != nil {
		logger.Warn("ziti lease revoke: controller broker token unreadable; skipping revoke (TTL will bound access)",
			"task_id", taskID, "token_file", cfg.ZitiAccessControllerTokenFile, "error", err)
		return fmt.Errorf("reading controller broker token: %w", err)
	}

	endpoint := strings.TrimRight(cfg.ZitiAccessBrokerURL, "/") + "/lease?task_id=" + url.QueryEscape(taskID)

	reqCtx, cancel := context.WithTimeout(ctx, zitiRevokeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building ziti revoke request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Warn("ziti lease revoke: broker unreachable; lease will expire via TTL",
			"task_id", taskID, "broker", cfg.ZitiAccessBrokerURL, "error", err)
		return fmt.Errorf("ziti revoke request: %w", err)
	}
	defer resp.Body.Close()

	// 204 = revoked (or idempotent no-op if already gone). Anything else is a
	// real failure worth surfacing: 401/403 means the controller is not a
	// configured revoker subject or its token audience is wrong.
	if resp.StatusCode != http.StatusNoContent {
		logger.Warn("ziti lease revoke: broker returned non-204; lease will expire via TTL",
			"task_id", taskID, "http", resp.StatusCode,
			"hint", "401/403 => controller not in broker REVOKER_SUBJECTS or wrong token audience")
		return fmt.Errorf("ziti revoke: broker returned http %d", resp.StatusCode)
	}

	logger.Info("ziti lease revoked on agent exit", "task_id", taskID)
	return nil
}
