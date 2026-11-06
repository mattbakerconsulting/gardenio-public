package server

import (
	"context"
	"strings"

	"gardenio/kbeads/internal/eventbus"
	"gardenio/kbeads/internal/model"
)

// sensitiveConfigPrefixes and sensitiveConfigKeys name config rows that are
// read back as authority by the daemon itself, so writing one is a privilege
// escalation rather than configuration, and reading one can be too (a
// bridge_token:/fleet_token: row holds a raw bearer; gd-zdo4DIeGWR):
//   - bus.handler.* — loaded at startup into external bus handlers that run
//     `sh -c <command>` inside the daemon, which holds the DB credentials.
//   - bridge_token:* / fleet_token:* — read back at startup as fleet tokens
//     (cmd/kd/serve.go), so a scoped token could mint an unscoped one.
//   - hook-execute-allowlist — widens who may fire /v1/hooks/execute.
//   - authz-denied-text — rewrites the denial text agents are shown.
//   - security.token-revocation-slo — the approved SLO the security panel shows.
//
// Compared case-insensitively so a differently-cased spelling cannot slip past
// a check that the loader would still honor.
var (
	sensitiveConfigPrefixes = []string{
		strings.ToLower(eventbus.HandlerConfigPrefix),
		"bridge_token:",
		"fleet_token:",
	}
	sensitiveConfigKeys = map[string]bool{
		hookExecuteAllowlistConfigKey: true,
		"authz-denied-text":           true,
		tokenRevocationSLOConfigKey:   true,
	}
)

// authorizeSetConfigKey is the key gate on PUT /v1/configs/{key}. The route is
// requireRoleMachineAllowed, so any non-OIDC caller (the shared static token,
// scoped fleet tokens, per-agent tokens, machine principals) reaches the
// handler; this gate decides which keys they may write:
//
//   - the token's type/op scopes (checkTokenScopes) apply, with the pseudo bead
//     type "config", so a scoped token restricted away from configs is refused;
//   - sensitiveConfigPrefixes / sensitiveConfigKeys are refused;
//
// type:* keys are not gated here: authorizeSetConfig (gd-32dXqWb95Y) owns them,
// refusing builtin types and any type carrying write_policy for non-OIDC
// callers while letting plain custom types register (the e2e seed's path).
//
// An OIDC caller is already role-gated (admin) by the route, and an
// auth-disabled deployment has no caller to gate. A v1 edge envelope is
// refused by the route at RoleAdmin; should it reach here it is gated like
// any other non-OIDC caller (gd-AvSKOUyqRz).
func authorizeSetConfigKey(ctx context.Context, key string) *AuthzDenial {
	switch authMethodFromContext(ctx) {
	case authMethodOIDC, authMethodNone:
		return nil
	}
	if d := checkTokenScopes(ctx, "update", "config"); d != nil {
		return d
	}
	if isSensitiveConfigKey(key) {
		return &AuthzDenial{
			Action:       "update",
			BeadType:     "config",
			RequiredRole: RoleAdmin,
			Message: "insufficient permissions: config key " + key +
				" is read back by the daemon as authority (bus handler, token, or allowlist) and requires " +
				RoleAdmin + " (OIDC)",
		}
	}
	return nil
}

// isSensitiveConfigKey reports whether key is one of sensitiveConfigKeys or
// under one of sensitiveConfigPrefixes.
func isSensitiveConfigKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if sensitiveConfigKeys[k] {
		return true
	}
	for _, p := range sensitiveConfigPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// isSensitiveConfigNamespace reports whether a list namespace names a
// sensitive prefix: "bridge_token" or "fleet_token" (the store lists
// namespace+":"), or "bus.handler" with or without its trailing dot.
func isSensitiveConfigNamespace(namespace string) bool {
	n := strings.ToLower(strings.TrimSpace(namespace))
	for _, p := range sensitiveConfigPrefixes {
		if strings.HasPrefix(n+":", p) || n == strings.TrimRight(p, ".:") {
			return true
		}
	}
	return false
}

// callerMayReadSensitiveConfig reports whether the caller may read a
// sensitive config row through the API: an OIDC admin, or an auth-disabled
// deployment (no caller to gate). Every other caller reaches the RoleViewer
// read routes (a static, fleet or per-agent token, a land principal, a
// scoped or viewer human) and is refused. The daemon's own token loader
// (cmd/kd resyncBridgeTokens) reads the store directly and is not affected.
func callerMayReadSensitiveConfig(ctx context.Context) bool {
	return authMethodFromContext(ctx) == authMethodNone || callerIsOIDCAdmin(ctx)
}

// authorizeGetConfigKey is the key gate on GET /v1/configs/{key} and gRPC
// GetConfig. It runs before the lookup, so a refused key answers the same
// whether or not the row exists.
func authorizeGetConfigKey(ctx context.Context, key string) *AuthzDenial {
	if !isSensitiveConfigKey(key) || callerMayReadSensitiveConfig(ctx) {
		return nil
	}
	return sensitiveConfigReadDenial("config key " + key)
}

// authorizeListConfigNamespace is the namespace gate on GET /v1/configs and
// gRPC ListConfigs. The store matches the namespace with LIKE, so a wildcard
// namespace can still reach sensitive rows; readableConfigs drops those.
func authorizeListConfigNamespace(ctx context.Context, namespace string) *AuthzDenial {
	if !isSensitiveConfigNamespace(namespace) || callerMayReadSensitiveConfig(ctx) {
		return nil
	}
	return sensitiveConfigReadDenial("config namespace " + namespace)
}

func sensitiveConfigReadDenial(what string) *AuthzDenial {
	return &AuthzDenial{
		Action:       "read",
		BeadType:     "config",
		RequiredRole: RoleAdmin,
		Message: "insufficient permissions: " + what +
			" holds daemon authority (bearer token, bus handler, or allowlist) and requires " +
			RoleAdmin + " (OIDC) to read",
	}
}

// readableConfigs drops the sensitive rows the caller may not read.
func readableConfigs(ctx context.Context, configs []*model.Config) []*model.Config {
	if callerMayReadSensitiveConfig(ctx) {
		return configs
	}
	out := configs[:0:0]
	for _, c := range configs {
		if !isSensitiveConfigKey(c.Key) {
			out = append(out, c)
		}
	}
	return out
}
