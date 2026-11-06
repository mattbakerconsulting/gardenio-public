package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Server-side default Owner (ka-0000003507, decision ka-0000003484:
// keep-header-but-tighten; supersedes ka-0000000058 spawn-root-only, which
// in turn superseded ka-0000000269 verbatim-X-Operated-By):
//
// When a caller creates a bead without setting Owner, the server may
// default Owner to the X-Operated-By header value — but only after
// verifying the header is a human-with-email identity AND, for agent-
// driven writes, that the header matches the spawn-chain ROOT human of
// the calling agent. See ADR docs/adr/owner-default.md.
//
// Policy:
//
//   1. req.Owner != "" (and not the "-" sentinel) wins — caller-supplied
//      Owner is persisted verbatim.
//
//   2. "-" sentinel persists as "" — explicit per-bead opt-out.
//
//   3. X-Owner-Override header (sanitized into ctx by the static-token
//      interceptor) wins next — for bridges that genuinely know the right
//      human (Slack /spawn from a non-Matt user, jira-bridge attributing
//      to a ticket reporter, etc.). Honored even when the feature flag is
//      off because the override is an explicit bridge signal.
//
//   4. When BEADS_OWNER_DEFAULT_ENABLED is off OR the caller passed
//      X-No-Owner-Default: 1, no default fires and Owner stays empty.
//
//   5. Otherwise, read X-Operated-By from ctx. Default Owner to the header
//      value iff:
//        (a) it is a human-with-email shape (slack:U… or oidc:…@…) per
//            isHumanOperator (ka-0000006809 Invariant); AND
//        (b) for agent-driven writes (a per-agent fleet token, see
//            actingAgentRef), the header value EQUALS the spawn-chain ROOT
//            human identity computed by walking the calling agent's
//            fields.spawned_by chain.
//      For non-agent callers (a bridge on its fleet token), the chain check
//      is skipped: there is no agent chain to compare against.
//
//   6. Mismatch (agent caller, header is human-shape but disagrees with
//      the resolved chain root) emits a structured owner_default_mismatch
//      slog.Warn and persists Owner = "". This is the Matt-disaster shape
//      the tightening is designed to catch.
//
//   7. Otherwise Owner stays "". Beads-viewer renders "(unowned)" via the
//      system:unowned sentinel pattern from ka-0000004566.
//
// See audit.go resolveOperatedByPure for the audit-operator path (which is
// unaffected — Owner and OperatedBy are separate columns).

const noOwnerDefaultKey contextKey = "no_owner_default"

// WithNoOwnerDefault returns a new context that opts out of the server-side
// Owner default. Set by the auth interceptor when the caller passed
// X-No-Owner-Default: 1.
func WithNoOwnerDefault(ctx context.Context) context.Context {
	return context.WithValue(ctx, noOwnerDefaultKey, true)
}

// noOwnerDefaultFromContext reports whether the caller opted out of the
// server-side Owner default.
func noOwnerDefaultFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noOwnerDefaultKey).(bool)
	return v
}

// parseNoOwnerDefaultHeader reports whether a raw X-No-Owner-Default header
// value should be interpreted as an opt-out.
func parseNoOwnerDefaultHeader(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ownerOverrideKey holds the sanitized X-Owner-Override header value when
// the static-token interceptor accepted it.
const ownerOverrideKey contextKey = "owner_override"

// WithOwnerOverride returns a new context carrying the sanitized
// X-Owner-Override header value. The value MUST already be sanitized via
// sanitizeOwnerOverrideHeader.
func WithOwnerOverride(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, ownerOverrideKey, v)
}

// ownerOverrideFromContext returns the sanitized X-Owner-Override header
// value, or "" when absent.
func ownerOverrideFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ownerOverrideKey).(string)
	return v
}

// sanitizeOwnerOverrideHeader validates an X-Owner-Override header value
// supplied by a static-primary-token caller. Returns the cleaned value and
// true when acceptable; "" and false otherwise.
//
// Rules mirror sanitizeOperatedByHeader: non-empty after trimming, length
// ≤ 128, no control characters. The bridge layer is the trusted caller and
// is expected to push an already-canonical operator id (slack:U..., oidc:
// email, etc.); we do not enforce the prefix shape here so future operator
// kinds don't require parser churn.
func sanitizeOwnerOverrideHeader(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 128 {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return s, true
}

// ownerDefaultEnabled reports whether BEADS_OWNER_DEFAULT_ENABLED is set to
// a truthy value. Default off per ADR §6 — re-enable only after the
// spawn-chain-root walk has baked and the C4 Matt-rebind backfill has run.
func ownerDefaultEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_OWNER_DEFAULT_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// spawnChainWalkCap bounds the agent.spawned_by walk so a misconfigured
// chain can't pin a goroutine on store lookups. 16 hops is well above the
// observed real-world maximum (Matt → gardener → crew is 2).
const spawnChainWalkCap = 16

// applyOwnerDefault returns the Owner value to persist on a new bead. See
// the policy notes at the top of this file for the precedence rules.
func (s *BeadsServer) applyOwnerDefault(ctx context.Context, owner string) string {
	if owner == "-" {
		return ""
	}
	if owner != "" {
		return owner
	}
	if v := ownerOverrideFromContext(ctx); v != "" {
		return v
	}
	if !ownerDefaultEnabled() {
		return ""
	}
	if noOwnerDefaultFromContext(ctx) {
		return ""
	}
	header := operatedByHeaderFromContext(ctx)
	if header == "" || !isHumanOperator(header) {
		return ""
	}
	if agentRef, isAgent := actingAgentRef(ctx); isAgent {
		root, ok := s.resolveSpawnChainRoot(ctx)
		if !ok {
			slog.Warn("owner_default_mismatch",
				"reason", "spawn_chain_root_unresolved",
				"agent", agentRef,
				"header", header,
			)
			return ""
		}
		if root != header {
			slog.Warn("owner_default_mismatch",
				"reason", "header_disagrees_with_spawn_chain_root",
				"agent", agentRef,
				"header", header,
				"spawn_chain_root", root,
			)
			return ""
		}
	}
	return header
}

// resolveSpawnChainRoot walks the calling agent's spawned_by chain to the
// root agent bead and returns the canonical human identity reachable from
// its fields. Returns ("", false) when:
//
//   - the caller is not static-token authenticated (OIDC has its own owner
//     story, fleet/anonymous don't have a chain root),
//   - the agent bead isn't reachable,
//   - the chain ends at a non-human sentinel (scheduler:/trigger:/
//     escalation-bridge/system:/fleet:) with no human fields,
//   - the root agent bead carries no user_bead_id / slack_user_id /
//     non-sentinel operated_by we can resolve to a human.
//
// The walk only counts a match when we land on the chain ROOT — relay
// agents in the middle don't get their header forwarded into Owner.
//
// A resolved root is cached per agent bead id for spawnChainRootCacheTTL
// (spawned_by never changes); an unresolved one is walked again next time,
// since the walk does not tell a missing bead from a store error.
func (s *BeadsServer) resolveSpawnChainRoot(ctx context.Context) (string, bool) {
	agentRef, isAgent := actingAgentRef(ctx)
	if !isAgent {
		return "", false
	}
	return s.resolveSpawnChainRootOf(ctx, agentRef)
}

// resolveSpawnChainRootOf is resolveSpawnChainRoot for a named agent bead
// rather than the caller, cached the same way.
func (s *BeadsServer) resolveSpawnChainRootOf(ctx context.Context, agentRef string) (string, bool) {
	if s == nil || s.store == nil || agentRef == "" {
		return "", false
	}
	root, _ := s.spawnChainRootCache.load(agentRef, func() (string, error) {
		root, _ := s.walkSpawnChainRoot(ctx, agentRef)
		return root, nil
	}, func(root string) bool { return root != "" })
	return root, root != ""
}

// walkSpawnChainRoot is resolveSpawnChainRoot's uncached walk from agentRef,
// one GetBead per hop.
func (s *BeadsServer) walkSpawnChainRoot(ctx context.Context, agentRef string) (string, bool) {
	visited := map[string]bool{}
	cur := agentRef
	var rootBead *model.Bead
	for i := 0; i < spawnChainWalkCap; i++ {
		if visited[cur] {
			return "", false
		}
		visited[cur] = true
		bead, err := s.store.GetBead(ctx, cur)
		if err != nil || bead == nil {
			return "", false
		}
		rootBead = bead
		spawner := agentSpawnedByField(bead)
		if spawner == "" {
			break
		}
		if isSentinelSpawner(spawner) {
			// Sentinel spawner: the current bead is the chain root (no
			// human spawner above it). Keep rootBead set to this bead so
			// we can still try fields.user_bead_id / slack_user_id below
			// — long-running scheduled gardeners often carry the human who
			// authored the schedule in those fields.
			break
		}
		cur = spawner
	}
	if rootBead == nil {
		return "", false
	}
	return humanIdentityFromAgentBead(ctx, s, rootBead)
}

// agentSpawnedByField extracts the .fields.spawned_by string from an agent
// bead, returning "" when absent or malformed.
func agentSpawnedByField(b *model.Bead) string {
	if b == nil || len(b.Fields) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		return ""
	}
	v, _ := fields["spawned_by"].(string)
	return strings.TrimSpace(v)
}

// isSentinelSpawner reports whether s is a non-agent spawner sentinel
// (scheduler/trigger/bridge/system/fleet). These end the walk: there is no
// further agent bead to look up, but the current bead is still the chain
// root and may carry the originating human in its own fields.
func isSentinelSpawner(s string) bool {
	if strings.HasPrefix(s, "scheduler:") {
		return true
	}
	if strings.HasPrefix(s, "trigger:") {
		return true
	}
	if strings.HasPrefix(s, "escalation-bridge") {
		return true
	}
	if strings.HasPrefix(s, "system:") {
		return true
	}
	if strings.HasPrefix(s, "fleet:") {
		return true
	}
	return false
}

// humanIdentityFromAgentBead extracts the canonical human operator id
// (slack:U..., oidc:email, etc.) from the root agent bead's fields. Tries
// user_bead_id first (dereferences to the user bead and prefers email),
// then slack_user_id, then operated_by (rejecting sentinel actors).
func humanIdentityFromAgentBead(ctx context.Context, s *BeadsServer, b *model.Bead) (string, bool) {
	if b == nil {
		return "", false
	}
	var fields map[string]any
	if len(b.Fields) > 0 {
		if err := json.Unmarshal(b.Fields, &fields); err != nil {
			return "", false
		}
	}

	if ubID, _ := fields["user_bead_id"].(string); strings.TrimSpace(ubID) != "" {
		if v, ok := canonicalIdentityFromUserBead(ctx, s, strings.TrimSpace(ubID)); ok {
			return v, true
		}
	}
	if v, _ := fields["slack_user_id"].(string); strings.TrimSpace(v) != "" {
		return "slack:" + strings.TrimSpace(v), true
	}
	if v, _ := fields["operated_by"].(string); strings.TrimSpace(v) != "" {
		v = strings.TrimSpace(v)
		if !isSentinelOperator(v) {
			return v, true
		}
	}
	return "", false
}

// canonicalIdentityFromUserBead looks up a user bead by id and returns the
// best canonical human identity from its fields. Prefers oidc:<email>,
// falling back to slack:<slack_user_id>.
func canonicalIdentityFromUserBead(ctx context.Context, s *BeadsServer, userBeadID string) (string, bool) {
	if s == nil || s.store == nil {
		return "", false
	}
	bead, err := s.store.GetBead(ctx, userBeadID)
	if err != nil || bead == nil {
		return "", false
	}
	var fields map[string]any
	if len(bead.Fields) > 0 {
		if err := json.Unmarshal(bead.Fields, &fields); err != nil {
			return "", false
		}
	}
	if email, _ := fields["email"].(string); strings.TrimSpace(email) != "" {
		return "oidc:" + strings.TrimSpace(email), true
	}
	if sl, _ := fields["slack_user_id"].(string); strings.TrimSpace(sl) != "" {
		return "slack:" + strings.TrimSpace(sl), true
	}
	return "", false
}
