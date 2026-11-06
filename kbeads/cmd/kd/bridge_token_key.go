package main

import "strings"

// bridgeTokenName resolves a persisted config key to the bridge-credential
// identity name it carries, implementing the dual-READ precedence for the
// fleet→bridge token rename (ka-0000000310).
//
// The canonical key is bridge_token:<name>. The legacy fleet_token:<name> key
// is accepted as a fallback ONLY when no canonical row exists for the same
// name — so when both shapes are present for one identity the canonical row
// wins and the legacy duplicate is ignored. hasCanonical must be the set of
// names that have a bridge_token:* row (built in a prior pass over all keys).
//
// Returns (name, true) when the key is a bridge/fleet token row to load, or
// ("", false) for any other key (or a shadowed legacy duplicate).
func bridgeTokenName(key string, hasCanonical map[string]bool) (string, bool) {
	if name, ok := strings.CutPrefix(key, "bridge_token:"); ok && name != "" {
		return name, true
	}
	if name, ok := strings.CutPrefix(key, "fleet_token:"); ok && name != "" && !hasCanonical[name] {
		return name, true
	}
	return "", false
}
