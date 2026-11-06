package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// EnsureConfigs upserts all gardenio-managed type, view, context, and config
// entries as config beads in the beads daemon. It is safe to call on every
// startup; existing beads with matching (title, labels) are updated in place.
func EnsureConfigs(ctx context.Context, setter ConfigSetter, logger *slog.Logger) error {
	all, err := loadConfigs()
	if err != nil {
		return err
	}
	return ensureConfigEntries(ctx, setter, all, logger)
}

// grafanaBridgeConfigKeys are the config entries grafana-bridge owns: the
// `dashboard` bead type, and no view (none of the view:* entries concern
// dashboards). The kbeads gardenio-machine-config-seed grant is exactly the
// titles of these keys (TestMachineConfigSeedTitles_MatchBridgeSeed), because
// config is admin-gated and the prompt/prerun/context/instructions entries
// reach every agent pod (prerun runs via `sh -c`); those stay with the bridges
// that seed them on their own tokens.
var grafanaBridgeConfigKeys = []string{"type:dashboard"}

// EnsureGrafanaBridgeConfigs is EnsureConfigs narrowed to the entries
// grafana-bridge owns (GrafanaBridgeConfigKeys). It exists so the bridge can run
// under a machine principal whose config grant is typed to those titles.
func EnsureGrafanaBridgeConfigs(ctx context.Context, setter ConfigSetter, logger *slog.Logger) error {
	all, err := loadConfigs()
	if err != nil {
		return err
	}
	owned := make(map[string]any, len(grafanaBridgeConfigKeys))
	for _, key := range grafanaBridgeConfigKeys {
		v, ok := all[key]
		if !ok {
			return fmt.Errorf("grafana-bridge config key %q is not defined in loadConfigs()", key)
		}
		owned[key] = v
	}
	return ensureConfigEntries(ctx, setter, owned, logger)
}

// slackBridgeConfigKeys are the config entries slack-bridge owns when it runs
// under its machine principal (gd-Hc34EENbO9): the bead types it delivers into
// (dm, message, mail, decision, escalation) and the three operator-queue views
// over them. No context:*, and none of the write-policy types (role, agent,
// project, user, account, trigger, schedule): the controller seeds the rest of
// configs() on its own token. The kbeads gardenio-machine-slack-config-seed
// grant is exactly the titles of these keys
// (TestSlackBridgeConfigSeedTitles_MatchBridgeSeed).
var slackBridgeConfigKeys = []string{
	"type:dm", "type:message", "type:mail", "type:decision", "type:escalation",
	"view:decisions:pending", "view:escalations:pending", "view:mail:inbox",
}

// SlackBridgeConfigKeys returns the keys EnsureSlackBridgeConfigs seeds.
func SlackBridgeConfigKeys() []string {
	return append([]string(nil), slackBridgeConfigKeys...)
}

// EnsureSlackBridgeConfigs is EnsureConfigs narrowed to the entries slack-bridge
// owns (SlackBridgeConfigKeys). The bridge runs it, instead of EnsureConfigs,
// only when its machine principal is enabled, because that principal may write
// no other config title.
func EnsureSlackBridgeConfigs(ctx context.Context, setter ConfigSetter, logger *slog.Logger) error {
	all, err := loadConfigs()
	if err != nil {
		return err
	}
	owned := make(map[string]any, len(slackBridgeConfigKeys))
	for _, key := range slackBridgeConfigKeys {
		v, ok := all[key]
		if !ok {
			return fmt.Errorf("slack-bridge config key %q is not defined in loadConfigs()", key)
		}
		owned[key] = v
	}
	return ensureConfigEntries(ctx, setter, owned, logger)
}

// ensureConfigEntries seeds every entry, best-effort: a key that fails is
// logged and the rest are still attempted, and the failures come back joined
// so the caller can tell a clean seed from a partial one. In particular a
// machine-principal client whose first token fetch failed
// (beadsapi.IsAuthUnavailable) sees that in the returned error and can retry
// the whole seed once the credential is there (gd-pj5hT3V1la); before this it
// returned nil with every key failed, so nothing could know to retry.
func ensureConfigEntries(ctx context.Context, setter ConfigSetter, entries map[string]any, logger *slog.Logger) error {
	var errs []error
	for key, value := range entries {
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshalling config %s: %w", key, err)
		}

		if err := ensureConfigBead(ctx, setter, key, valueJSON, logger); err != nil {
			logger.Warn("failed to ensure config bead", "key", key, "error", err)
			errs = append(errs, fmt.Errorf("config %s: %w", key, err))
			continue
		}

		logger.Info("ensured config bead", "key", key)
	}

	return errors.Join(errs...)
}

// roleKeyedNamespaces are key prefixes where the suffix represents a role
// (or "global"). Other namespaces (type, view) use the full key as a
// global-scoped title.
var roleKeyedNamespaces = map[string]bool{
	"context": true,
}

// parseEntryKey converts a config entry key into (title, labels) for
// config bead storage.
//
// Key patterns:
//
//	"config:nudge-prompts:global"            → ("nudge-prompts", ["global"])
//	"config:claude-instructions:role:thread" → ("claude-instructions", ["role:thread"])
//	"context:gardener"                        → ("context", ["role:gardener"])
//	"type:agent"                             → ("type:agent", ["global"])
//	"view:agents:active"                     → ("view:agents:active", ["global"])
func parseEntryKey(key string) (title string, labels []string) {
	// config:* keys: strip prefix, parse category:scope.
	if strings.HasPrefix(key, "config:") {
		rest := strings.TrimPrefix(key, "config:")
		idx := strings.Index(rest, ":")
		if idx < 0 {
			return rest, []string{"global"}
		}
		return rest[:idx], []string{rest[idx+1:]}
	}

	// Check for role-keyed namespaces (e.g. context:gardener).
	if idx := strings.Index(key, ":"); idx > 0 {
		namespace := key[:idx]
		if roleKeyedNamespaces[namespace] {
			suffix := key[idx+1:]
			if suffix == "global" {
				return namespace, []string{"global"}
			}
			return namespace, []string{"role:" + suffix}
		}
	}

	// Everything else (type:*, view:*): full key is title, always global.
	return key, []string{"global"}
}

// ensureConfigBead creates or updates a config bead for the given config key.
func ensureConfigBead(ctx context.Context, setter ConfigSetter, key string, valueJSON []byte, logger *slog.Logger) error {
	category, labels := parseEntryKey(key)

	// Search for an existing open config bead with matching title.
	result, err := setter.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return fmt.Errorf("listing config beads: %w", err)
	}

	// Find existing bead with same title and labels.
	description := string(valueJSON)
	for _, bead := range result.Beads {
		if bead.Title == category && labelsMatch(bead.Labels, labels) {
			// Update existing bead's description.
			if bead.Description != description {
				if err := setter.UpdateBeadDescription(ctx, bead.ID, description); err != nil {
					return fmt.Errorf("updating config bead %s: %w", bead.ID, err)
				}
				logger.Info("updated config bead", "key", key, "bead_id", bead.ID)
			}
			return nil
		}
	}

	// Create new config bead.
	id, err := setter.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       category,
		Type:        "config",
		Kind:        "config",
		Description: description,
		Labels:      labels,
		CreatedBy:   "gardenio",
	})
	if err != nil {
		return fmt.Errorf("creating config bead for %s: %w", key, err)
	}
	logger.Info("created config bead", "key", key, "bead_id", id)
	return nil
}

// labelsMatch checks if two label slices contain the same elements.
func labelsMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, l := range a {
		set[l] = true
	}
	for _, l := range b {
		if !set[l] {
			return false
		}
	}
	return true
}
