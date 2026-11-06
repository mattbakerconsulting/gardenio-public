package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/server"
)

// persistedBridgeToken is one bridge/fleet token row decoded from the config
// store (bridge_token:<name>, or the legacy fleet_token:<name>).
type persistedBridgeToken struct {
	Name         string
	Token        string
	Project      string
	AllowedTypes []string
	AllowedOps   []string
	ExpiresAt    time.Time
	ExpiresAtRaw string
	Subject      string
	Privileged   bool
	// Legacy marks the bare-JSON-string shape, which carries no scopes or expiry.
	Legacy bool
}

// decodePersistedBridgeTokens picks the bridge/fleet token rows out of configs
// and decodes them. Two persisted shapes are supported:
//   - legacy: a bare JSON string holding the token value (no scopes/expiry).
//   - current: a JSON object {"token","project?","allowed_types?","allowed_ops?","expires_at?","subject?"}.
//
// The legacy `cross_fleet_read` field is silently ignored (epic ka-0000000871).
// A legacy fleet_token:* row is skipped when a canonical bridge_token:* row
// exists for the same name (ka-0000000310). Rows whose expires_at is before now
// are returned separately in expired, so the caller can log them.
func decodePersistedBridgeTokens(configs []*model.Config, now time.Time) (live, expired []persistedBridgeToken) {
	// First pass: index which names have a canonical bridge_token:* row so the
	// legacy fallback can skip them.
	hasCanonical := make(map[string]bool)
	for _, c := range configs {
		if name, ok := strings.CutPrefix(c.Key, "bridge_token:"); ok && name != "" {
			hasCanonical[name] = true
		}
	}
	for _, c := range configs {
		name, ok := bridgeTokenName(c.Key, hasCanonical)
		if !ok {
			continue
		}
		var asString string
		if err := json.Unmarshal(c.Value, &asString); err == nil && asString != "" {
			live = append(live, persistedBridgeToken{Name: name, Token: asString, Legacy: true})
			continue
		}
		var entry struct {
			Token        string   `json:"token"`
			Project      string   `json:"project,omitempty"`
			AllowedTypes []string `json:"allowed_types,omitempty"`
			AllowedOps   []string `json:"allowed_ops,omitempty"`
			ExpiresAt    string   `json:"expires_at,omitempty"`
			Subject      string   `json:"subject,omitempty"`
			Privileged   bool     `json:"privileged,omitempty"`
			// CrossFleetRead is intentionally not parsed — silently ignored.
		}
		if err := json.Unmarshal(c.Value, &entry); err != nil || entry.Token == "" {
			continue
		}
		var expiresAt time.Time
		if entry.ExpiresAt != "" {
			if t, perr := time.Parse(time.RFC3339Nano, entry.ExpiresAt); perr == nil {
				expiresAt = t
			} else if t, perr := time.Parse(time.RFC3339, entry.ExpiresAt); perr == nil {
				expiresAt = t
			}
		}
		tok := persistedBridgeToken{
			Name:         name,
			Token:        entry.Token,
			Project:      entry.Project,
			AllowedTypes: entry.AllowedTypes,
			AllowedOps:   entry.AllowedOps,
			ExpiresAt:    expiresAt,
			ExpiresAtRaw: entry.ExpiresAt,
			Subject:      entry.Subject,
			Privileged:   entry.Privileged,
		}
		if !expiresAt.IsZero() && now.After(expiresAt) {
			expired = append(expired, tok)
			continue
		}
		live = append(live, tok)
	}
	return live, expired
}

// configLister is the slice of the store the bridge-token re-sync reads.
type configLister interface {
	ListConfigs(ctx context.Context, namespace string) ([]*model.Config, error)
}

// resyncBridgeTokens re-reads the persisted bridge/fleet token rows and
// registers every live one this daemon does not hold yet, returning how many it
// added.
//
// Why (gd-PPzLrS2k8e): persisted tokens used to be read only at startup. During
// a rolling update the old daemon pod keeps serving for a few seconds after the
// new one has loaded the store, and a per-agent token the controller registers
// on the old pod in that window is persisted but never reaches the new pod's
// memory. The agent pod holding that token then got `HTTP 401: invalid token`
// on every call for the rest of its life (gb ready, kd claim, the nudge-prompt
// lookup), which looked like a credential-switch fault because a coop switch
// was often the first thing to make a daemon call after the roll.
//
// Tokens already held are left alone, so an in-memory token keeps the scopes
// it was registered with, and tokens this daemon revoked are never re-added
// (FleetConfig.RegisterPersistedToken).
func resyncBridgeTokens(ctx context.Context, store configLister, fleetCfg *server.FleetConfig, logger *slog.Logger, now time.Time) (int, error) {
	var configs []*model.Config
	for _, ns := range []string{"bridge_token", "fleet_token"} {
		rows, err := store.ListConfigs(ctx, ns)
		if err != nil {
			return 0, err
		}
		configs = append(configs, rows...)
	}
	live, _ := decodePersistedBridgeTokens(configs, now)
	added := 0
	for _, t := range live {
		if fleetCfg.RegisterPersistedToken(t.Token, t.Name, t.Project, t.AllowedTypes, t.AllowedOps, t.ExpiresAt, t.Subject, t.Privileged) {
			added++
			logger.Warn("registered persisted fleet token this daemon was missing",
				"fleet", t.Name, "project", t.Project, "has_expiry", !t.ExpiresAt.IsZero())
		}
	}
	return added, nil
}

// bridgeTokenResyncLoop runs resyncBridgeTokens every interval until ctx is
// cancelled. A failed pass is logged and retried on the next tick.
func bridgeTokenResyncLoop(ctx context.Context, store configLister, fleetCfg *server.FleetConfig, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := resyncBridgeTokens(ctx, store, fleetCfg, logger, time.Now()); err != nil && ctx.Err() == nil {
				logger.Warn("bridge token re-sync failed", "err", err)
			}
		}
	}
}
