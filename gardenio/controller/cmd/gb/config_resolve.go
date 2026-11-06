package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"gardenio/controller/internal/claudeconfig"

	"gardenio/clicreds/avareauth"
)

// configBeadLister is the interface for querying config beads.
// Satisfied by *beadsapi.Client. Alias of claudeconfig.BeadLister.
type configBeadLister = claudeconfig.BeadLister

// resolvedConfig is an extra (inline) config layer contributed outside the
// type=config bead resolution — e.g. a role/project bead's inline claude_config.
// setup.go builds these and hands them to ResolveConfigBeads. The full resolve +
// merge now lives in internal/claudeconfig (task ka-0000006351); this struct is
// the thin adapter for the extra-layer inputs the shared resolver accepts.
type resolvedConfig struct {
	value       json.RawMessage
	specificity string
}

// projectInlineSpecificity is the sort key for project bead inline config.
const projectInlineSpecificity = claudeconfig.ProjectInlineSpecificity

// roleInlineSpecificity returns the sort key for role bead inline config.
func roleInlineSpecificity(roleName string) string {
	return claudeconfig.RoleInlineSpecificity(roleName)
}

// ResolveConfigBeads queries config beads matching the given category and agent
// subscriptions, sorts by label specificity, and merges according to the
// category's merge strategy. Thin wrapper over claudeconfig.ResolveConfigBeads
// that adapts cmd/gb's resolvedConfig extra layers.
func ResolveConfigBeads(ctx context.Context, lister configBeadLister, category string, subscriptions []string, extraLayers ...resolvedConfig) (map[string]any, int, error) {
	return claudeconfig.ResolveConfigBeads(ctx, lister, category, subscriptions, toClaudeLayers(extraLayers)...)
}

// resolveConfigBackoffs is the retry schedule for resolveConfigBeadsWithRetry.
// Tight and short because "gb setup claude" runs synchronously on the SessionStart
// boot path — a pod cannot wait minutes for its settings. Total added latency on
// the (rare) all-attempts-fail path is ~1.75s, negligible against a boot that is
// already several seconds. Overridable in tests.
var resolveConfigBackoffs = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}

// resolveConfigBeadsWithRetry wraps ResolveConfigBeads with bounded backoff on
// transient daemon-listing failures.
//
// Why (ka-0000006617 / long-lived flake ka-0000003923, ka-0000001102): "gb setup
// claude" makes FOUR separate daemon round-trips (claude-settings, claude-mcp,
// claude-hooks, claude-instructions), each a single HTTP GET with no transport-
// level retry (beadsapi.doJSON retries only a 401 token-refresh). On the busy
// shared kbeads instance backing the E2E shards, one of the LATER reads
// occasionally blips — a connection reset or a brief 5xx — AFTER settings.json is
// already written. The result is the exact documented signature: correct merged
// output on disk, then a non-zero exit (ka-0000003391: "gb setup wrote correct
// merged settings then exited 1"). ~1/60 E2E runs, blocking golden promotion for
// months. A bounded retry absorbs the transient without masking a real regression:
// a PERSISTENT failure still exhausts every attempt and errors, and every retry is
// logged to stderr so a genuine outage stays visible.
//
// A re-auth bounce (expired AVA cookie → 3xx to the Cloudflare-walled IdP) is
// deterministic, not transient, so it bails immediately rather than burning the
// full schedule against a sign-in wall — mirroring emitHookWithRetry in hook.go.
func resolveConfigBeadsWithRetry(ctx context.Context, lister configBeadLister, category string, subscriptions []string, extraLayers ...resolvedConfig) (map[string]any, int, error) {
	var lastErr error
	for attempt := 0; attempt <= len(resolveConfigBackoffs); attempt++ {
		cfg, n, err := ResolveConfigBeads(ctx, lister, category, subscriptions, extraLayers...)
		if err == nil {
			return cfg, n, nil
		}
		lastErr = err
		if errors.Is(err, avareauth.ErrReauthRequired) {
			return nil, 0, err
		}
		if attempt < len(resolveConfigBackoffs) {
			fmt.Fprintf(os.Stderr, "[setup] resolving %s failed (attempt %d/%d), retrying in %s: %v\n",
				category, attempt+1, len(resolveConfigBackoffs)+1, resolveConfigBackoffs[attempt], err)
			select {
			case <-time.After(resolveConfigBackoffs[attempt]):
			case <-ctx.Done():
				return nil, 0, fmt.Errorf("resolving %s: context cancelled during retry: %w", category, ctx.Err())
			}
		}
	}
	return nil, 0, lastErr
}

// toClaudeLayers adapts cmd/gb inline extra layers to shared claudeconfig.Layer.
func toClaudeLayers(extras []resolvedConfig) []claudeconfig.Layer {
	if len(extras) == 0 {
		return nil
	}
	out := make([]claudeconfig.Layer, len(extras))
	for i, e := range extras {
		out[i] = claudeconfig.NewLayer(e.value, e.specificity)
	}
	return out
}
