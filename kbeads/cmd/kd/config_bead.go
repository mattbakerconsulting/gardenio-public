package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// roleKeyedNamespaces are key prefixes where the suffix represents a role.
var roleKeyedNamespaces = map[string]bool{
	"context": true,
}

// parseConfigEntryKey converts a KV-style key to (title, labels) for
// looking up config beads.
//
//	"context:gardener"     → ("context", ["role:gardener"])
//	"view:agents:active"  → ("view:agents:active", ["global"])
//	"type:agent"          → ("type:agent", ["global"])
func parseConfigEntryKey(key string) (title string, labels []string) {
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
	return key, []string{"global"}
}

// resolveConfigBead looks up a config value from config beads instead of
// the legacy KV store. Returns a model.Config with the bead's description
// as the value, matching the same return shape as GetConfig.
func resolveConfigBead(ctx context.Context, key string) (*model.Config, error) {
	title, labels := parseConfigEntryKey(key)

	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:   []string{"config"},
		Status: []string{"open"},
		Limit:  500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing config beads: %w", err)
	}

	for _, b := range resp.Beads {
		if b.Title == title && labelsMatchConfig(b.Labels, labels) {
			return &model.Config{
				Key:   key,
				Value: json.RawMessage(b.Description),
			}, nil
		}
	}

	return nil, fmt.Errorf("config bead not found: %s (title=%q labels=%v)", key, title, labels)
}

// labelsMatchConfig checks if bead labels (a) contain all required labels (b).
// Uses superset matching: the bead may have extra labels (e.g. auto-added
// project labels) and still match a search for fewer labels.
func labelsMatchConfig(a, b []string) bool {
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
