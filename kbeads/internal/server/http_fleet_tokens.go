package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// registerBridgeTokenRequest is the JSON body for POST /v1/bridge-tokens
// (canonical) and its dual-accept alias POST /v1/fleet-tokens.
//
// The bridge-credential identity noun is being renamed fleet→bridge
// (ka-0000000310). During the compat window the handler accepts BOTH the
// canonical `bridge` field and the legacy `fleet` field, preferring `bridge`
// when both are present. The internal identity value and audit stamp are
// UNCHANGED — only the API noun differs. resolveName() reconciles the two.
type registerBridgeTokenRequest struct {
	Token string `json:"token"`
	// Bridge is the canonical identity name (ka-0000000310).
	Bridge string `json:"bridge,omitempty"`
	// Fleet is the legacy alias for Bridge. Kept for dual-accept during the
	// compat window; removed in the cleanup bead (ka-0000004906).
	Fleet        string   `json:"fleet,omitempty"`
	Project      string   `json:"project,omitempty"`       // optional project scope
	AllowedTypes []string `json:"allowed_types,omitempty"` // optional bead type scope
	AllowedOps   []string `json:"allowed_ops,omitempty"`   // optional operation scope
	// TTL is a Go duration string (e.g. "24h", "30m"). When set, the token
	// expires at registration_time + TTL. Mutually exclusive with ExpiresAt.
	TTL string `json:"ttl,omitempty"`
	// ExpiresAt is an RFC3339 absolute expiration. Mutually exclusive with TTL.
	// Empty/zero means the token never expires (back-compat default).
	ExpiresAt string `json:"expires_at,omitempty"`
	// WithRefresh, when true, also mints a refresh token and returns it in
	// the response body. Requires SetRefreshTokenManager wiring on the
	// server. The refresh token's TTL is RefreshTTL when set, otherwise
	// DefaultRefreshTTL (30d).
	WithRefresh bool   `json:"with_refresh,omitempty"`
	RefreshTTL  string `json:"refresh_ttl,omitempty"` // optional duration for refresh
	// Subject, when set, scopes audit attribution for the access token
	// (and any refresh-rotated descendants). Sanitised like X-Actor:
	// length ≤ 128, no control characters. Empty falls back to the fleet
	// name for audit attribution (C1.4 back-compat).
	Subject string `json:"subject,omitempty"`
	// CrossFleetRead is accepted for backward compatibility with existing
	// operator runbooks but is silently ignored — all tokens now read across
	// all fleets. Removed under epic ka-0000000871 / ka-0000003285.
	CrossFleetRead bool `json:"cross_fleet_read,omitempty"`
	// Privileged marks a designated service-fleet identity (e.g. the
	// gitlab-bridge) that may perform system-managed graph writes —
	// produces/triggered-by deps and mr_url back-fill — without tripping
	// per-bead ownership. It relaxes ONLY the ownership gate; type/project
	// write-policy and token scopes still apply. Only an OIDC admin or a token-mint
	// machine principal can set this (gd-ua4DGNtZdG); the static token cannot. See ka-0000001946.
	Privileged bool `json:"privileged,omitempty"`
}

// resolveName returns the bridge-credential identity name from the request,
// preferring the canonical `bridge` field and falling back to the legacy
// `fleet` alias (ka-0000000310 dual-accept). Empty when neither is set.
func (req registerBridgeTokenRequest) resolveName() string {
	if req.Bridge != "" {
		return req.Bridge
	}
	return req.Fleet
}

// handleRegisterBridgeToken handles POST /v1/bridge-tokens (canonical) and the
// dual-accept alias POST /v1/fleet-tokens.
// Registers a new fleet token mapping at runtime and persists it to the config
// store. Only a machine principal with the token-mint capability can call it.
func (s *BeadsServer) handleRegisterBridgeToken(w http.ResponseWriter, r *http.Request) {
	if !s.isTokenMintRequest(r) {
		writeError(w, http.StatusForbidden, "fleet token management requires the token-mint capability")
		return
	}

	var req registerBridgeTokenRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	// Accept either the canonical `bridge` field or the legacy `fleet`
	// alias (ka-0000000310). `name` is the single internal identity value
	// from here on; it is what gets registered, persisted, and audit-stamped
	// — unchanged from before, only the wire noun differs.
	name := req.resolveName()
	if name == "" {
		writeError(w, http.StatusBadRequest, "bridge is required")
		return
	}
	req.Fleet = name

	if req.TTL != "" && req.ExpiresAt != "" {
		writeError(w, http.StatusBadRequest, "ttl and expires_at are mutually exclusive")
		return
	}

	// Sanitise subject the same way the X-Actor header is validated. An
	// empty subject is allowed (back-compat fall-through to fleet name).
	if req.Subject != "" {
		s, ok := sanitizeActorHeader(req.Subject)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid subject: must be non-empty after trim, ≤128 chars, no control characters")
			return
		}
		req.Subject = s
	}
	// An agent-class token is an AGENT, never a service. Its subject becomes the
	// audit identity, and some gates compare a fleet subject with well-known
	// service names ("bridge:acme-scif" carries SCIF approval authority), so an
	// agent-* registration whose subject could be read as a service identity
	// ('bridge:<name>', an email) is refused at the mint.
	if err := validateAgentFleetRegistration(name, req.Subject); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateLandFleetRegistration(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Only the token-mint capability may register a service identity or a
	// privileged token (gd-ua4DGNtZdG); an auth-disabled daemon gets ordinary
	// fleets only.
	if !s.isTrustedTokenMinter(r) {
		if err := s.validateUntrustedMint(name, req.Subject, req.Privileged); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}

	var expiresAt time.Time
	switch {
	case req.TTL != "":
		d, err := time.ParseDuration(req.TTL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ttl: "+err.Error())
			return
		}
		if d <= 0 {
			writeError(w, http.StatusBadRequest, "ttl must be positive")
			return
		}
		expiresAt = time.Now().Add(d)
	case req.ExpiresAt != "":
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid expires_at: use RFC3339 format")
			return
		}
		expiresAt = t
	}

	// Register in memory. `req.CrossFleetRead` is intentionally ignored —
	// all tokens read across all fleets (epic ka-0000000871 / ka-0000003285).
	s.fleetCfg.RegisterTokenFull(req.Token, req.Fleet, req.Project, req.AllowedTypes, req.AllowedOps, expiresAt, req.Subject, req.Privileged)

	// Persist to config store so it survives daemon restarts.
	persistedValue := map[string]any{"token": req.Token}
	if req.Project != "" {
		persistedValue["project"] = req.Project
	}
	if len(req.AllowedTypes) > 0 {
		persistedValue["allowed_types"] = req.AllowedTypes
	}
	if len(req.AllowedOps) > 0 {
		persistedValue["allowed_ops"] = req.AllowedOps
	}
	if !expiresAt.IsZero() {
		persistedValue["expires_at"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	if req.Subject != "" {
		persistedValue["subject"] = req.Subject
	}
	if req.Privileged {
		persistedValue["privileged"] = true
	}
	valueJSON, _ := json.Marshal(persistedValue)
	// New rows use the canonical bridge_token:<name> key (ka-0000000310).
	// Reads dual-read bridge_token:* then fall back to fleet_token:* (see
	// serve.go); there is no eager backfill — legacy fleet_token:* rows are
	// migrated one-shot by the cleanup bead (ka-0000004906).
	cfgKey := "bridge_token:" + name
	cfg := &model.Config{
		Key:   cfgKey,
		Value: valueJSON,
	}
	if err := s.store.SetConfig(r.Context(), cfg); err != nil {
		// Roll back in-memory registration on persist failure.
		s.fleetCfg.removeToken(req.Token)
		writeServerError(r.Context(), w, "handleRegisterBridgeToken", err, "failed to persist fleet token")
		return
	}

	// Emit the canonical `bridge` field plus the mirrored legacy `fleet`
	// field so old clients keep parsing during the compat window
	// (ka-0000000310). Both carry the identical identity value.
	resp := map[string]any{"bridge": name, "fleet": name}
	if req.Project != "" {
		resp["project"] = req.Project
	}
	if len(req.AllowedTypes) > 0 {
		resp["allowed_types"] = req.AllowedTypes
	}
	if len(req.AllowedOps) > 0 {
		resp["allowed_ops"] = req.AllowedOps
	}
	if !expiresAt.IsZero() {
		resp["expires_at"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	if req.Subject != "" {
		resp["subject"] = req.Subject
	}
	if req.Privileged {
		resp["privileged"] = true
	}

	// Optional companion refresh token. We mint it AFTER the access-token
	// row is persisted so a refresh failure doesn't leave a half-registered
	// fleet token; if Issue fails we drop the access token to keep state
	// consistent.
	if req.WithRefresh {
		if s.refreshTokens == nil {
			s.fleetCfg.removeToken(req.Token)
			_ = s.store.DeleteConfig(r.Context(), cfgKey)
			writeError(w, http.StatusServiceUnavailable, "refresh-token manager not configured")
			return
		}
		var refreshTTL time.Duration
		if req.RefreshTTL != "" {
			d, err := time.ParseDuration(req.RefreshTTL)
			if err != nil {
				s.fleetCfg.removeToken(req.Token)
				_ = s.store.DeleteConfig(r.Context(), cfgKey)
				writeError(w, http.StatusBadRequest, "invalid refresh_ttl: "+err.Error())
				return
			}
			if d <= 0 {
				s.fleetCfg.removeToken(req.Token)
				_ = s.store.DeleteConfig(r.Context(), cfgKey)
				writeError(w, http.StatusBadRequest, "refresh_ttl must be positive")
				return
			}
			refreshTTL = d
		}
		issue, err := s.refreshTokens.Issue(r.Context(), IssueRequest{
			Fleet:        req.Fleet,
			Project:      req.Project,
			Subject:      req.Subject,
			AllowedTypes: req.AllowedTypes,
			AllowedOps:   req.AllowedOps,
			TTL:          refreshTTL,
			Privileged:   req.Privileged,
		})
		if err != nil {
			s.fleetCfg.removeToken(req.Token)
			_ = s.store.DeleteConfig(r.Context(), cfgKey)
			writeServerError(r.Context(), w, "handleRegisterBridgeToken", err, "failed to issue refresh token")
			return
		}
		resp["refresh_token"] = issue.Plaintext
		resp["refresh_token_expires_at"] = issue.Row.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}

	writeJSON(w, http.StatusCreated, resp)
}

// handleListBridgeTokens handles GET /v1/fleet-tokens.
// Returns the list of fleet names with registered tokens (never exposes token
// values). When ?detailed=true is set, each entry carries expires_at, subject,
// and project so rotators can decide when to mint fresh credentials. Only the
// admin can call this endpoint. The legacy `cross_fleet_read` field is no
// longer surfaced (epic ka-0000000871 / ka-0000003285 — all tokens read across
// all fleets).
func (s *BeadsServer) handleListBridgeTokens(w http.ResponseWriter, r *http.Request) {
	if !s.isTokenMintRequest(r) {
		writeError(w, http.StatusForbidden, "fleet token management requires the token-mint capability")
		return
	}

	if r.URL.Query().Get("detailed") == "true" {
		summaries := s.fleetCfg.ListBridgeTokensDetailed()
		out := make([]map[string]any, 0, len(summaries))
		for _, sum := range summaries {
			entry := map[string]any{
				"name":    sum.Fleet,
				"project": sum.Project,
				"subject": sum.Subject,
			}
			if !sum.ExpiresAt.IsZero() {
				entry["expires_at"] = sum.ExpiresAt.UTC().Format(time.RFC3339Nano)
			}
			out = append(out, entry)
		}
		// Emit canonical `bridges` plus mirrored legacy `fleets`
		// (ka-0000000310 dual-accept).
		writeJSON(w, http.StatusOK, map[string]any{"bridges": out, "fleets": out})
		return
	}

	fleets := s.fleetCfg.ListBridgeTokens()
	if fleets == nil {
		fleets = []string{}
	}
	// Emit canonical `bridges` plus mirrored legacy `fleets` (ka-0000000310).
	writeJSON(w, http.StatusOK, map[string]any{"bridges": fleets, "fleets": fleets})
}

// handleRevokeBridgeToken handles DELETE /v1/bridge-tokens/{bridge} (canonical)
// and the dual-accept alias DELETE /v1/fleet-tokens/{fleet}.
// Removes every access-token mapping and refresh token of the bridge from memory,
// the refresh store and the config store. Only the admin
// can call this endpoint.
func (s *BeadsServer) handleRevokeBridgeToken(w http.ResponseWriter, r *http.Request) {
	if !s.isTokenMintRequest(r) {
		writeError(w, http.StatusForbidden, "fleet token management requires the token-mint capability")
		return
	}

	// The canonical route binds {bridge}; the legacy alias binds {fleet}.
	// Whichever pattern matched, the other PathValue is empty (ka-0000000310).
	name := r.PathValue("bridge")
	if name == "" {
		name = r.PathValue("fleet")
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "bridge name is required")
		return
	}

	removed, nRefresh, refreshErr := s.revokeBridgeName(r.Context(), name)
	if refreshErr != nil {
		writeServerError(r.Context(), w, "handleRevokeBridgeToken", refreshErr, "failed to revoke refresh tokens")
		return
	}
	if removed == 0 && nRefresh == 0 {
		writeError(w, http.StatusNotFound, "no token registered for bridge")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// revokeBridgeName removes every access token and refresh token of bridge
// name: from memory, the refresh store and the config store. It answers how
// many access mappings and refresh rows it removed.
func (s *BeadsServer) revokeBridgeName(ctx context.Context, name string) (removed, nRefresh int, err error) {
	// Revoke the fleet's refresh tokens (and their minted children) first so a
	// concurrent exchange cannot mint a fresh access token after the sweep
	// below. A failure here must not leave the access tokens live: finish the
	// sweep, then report the error so the caller retries.
	nRefresh, err = s.refreshTokens.RevokeFleet(ctx, name)

	// Remove every access-token mapping from memory.
	removed = len(s.fleetCfg.RevokeBridgeToken(name))

	// Remove from config store. Delete BOTH the canonical bridge_token:* key
	// and the legacy fleet_token:* key so a token persisted under either shape
	// is fully revoked during the compat window (ka-0000000310).
	_ = s.store.DeleteConfig(ctx, "bridge_token:"+name)
	_ = s.store.DeleteConfig(ctx, "fleet_token:"+name)
	return removed, nRefresh, err
}

// isTokenMintRequest reports whether the request may use the bridge/fleet-token
// routes: a machine principal holding the token-mint capability (design §7
// option 1, gd-gQ4iLIpWCm), or any caller of a daemon with auth disabled
// (dev/test). There is no shared admin token: the primary static token was
// retired (gd-ke6PC7bRto), so a fleet token or an OIDC admin never mints.
// Scoped to these three handlers; the embeddings routes use isAdminRequest.
func (s *BeadsServer) isTokenMintRequest(r *http.Request) bool {
	return authMethodFromContext(r.Context()) == authMethodNone || machineHasCapability(r.Context(), capTokenMint)
}

// isTrustedTokenMinter reports whether the caller may mint service identities
// and privileged tokens: a machine principal holding the token-mint capability
// (the controller). (An OIDC admin never reaches the mint handlers at all:
// isTokenMintRequest does not admit one.) Only an auth-disabled daemon admits
// a caller that is not a trusted minter, and it is held to the ordinary-fleet
// rules below.
func (s *BeadsServer) isTrustedTokenMinter(r *http.Request) bool {
	return machineHasCapability(r.Context(), capTokenMint)
}

// reservedServiceNames are the fleet and subject names that carry service
// authority somewhere in the daemon (the SCIF approval portal subject, the
// bridge relays, the controller). A caller that is not a trusted minter may
// not register a token under any of them, as fleet or as subject.
var reservedServiceNames = map[string]bool{
	"slack-bridge":  true,
	"jira-bridge":   true,
	"gitlab-bridge": true,
	"github-bridge": true,
	"slack":         true,
	"jira":          true,
	"gitlab":        true,
	"github":        true,
	"acme-scif":     true,
	"controller":    true,
	"gardenio":      true,
	"static":        true,
}

// validateUntrustedMint refuses a registration by a caller that is not a trusted
// minter (isTrustedTokenMinter) when it would produce a service identity: a
// privileged token, a reserved fleet or subject name, or any fleet or subject
// shaped like a namespaced principal ('bridge:x', 'system:x', an email). The
// subject stamped on a token is what callerIsScifApprovalPortal, the ownership
// gate and the bridge-relay checks read back, so it must never be client-chosen
// for a service name. The agent-* fleet namespace is reserved the same way, and
// so is the *-bridge suffix: isBridgeFleetIdentity admits any fleet ending in
// it as a decision resolver, so a minter that could name one could answer a
// captain's decision (gd-VuYwGGYLLT). Any name isBridgeSubject trusts to relay
// X-Operated-By is refused too, so a BEADS_BRIDGE_SUBJECTS entry can never
// drift out of the reserved set (gd-wStnnYEt3M).
func (s *BeadsServer) validateUntrustedMint(fleet, subject string, privileged bool) error {
	if privileged {
		return fmt.Errorf("privileged tokens require an OIDC admin or the token-mint capability")
	}
	// agent-* is the per-agent fleet namespace: the bead id the continuation gate,
	// taint lookups and the ownership bypass act on is the suffix of the fleet
	// name, so a minter that can pick the name can pick the identity. Only the
	// controller (token-mint capability) names agent fleets (gd-VuYwGGYLLT).
	if f := strings.ToLower(strings.TrimSpace(fleet)); strings.HasPrefix(f, agentFleetPrefix) {
		return fmt.Errorf("bridge %q is in the reserved %q namespace: per-agent tokens are minted by the controller, which requires the token-mint capability", fleet, agentFleetPrefix+"*")
	}
	if f := strings.ToLower(strings.TrimSpace(fleet)); strings.HasSuffix(f, bridgeFleetSuffix) {
		return fmt.Errorf("bridge %q is in the reserved %q namespace: bridge fleets are minted by the controller, which requires the token-mint capability", fleet, "*"+bridgeFleetSuffix)
	}
	for _, v := range []struct{ field, val string }{{"bridge", fleet}, {"subject", subject}} {
		if v.val == "" {
			continue
		}
		norm := strings.ToLower(strings.TrimSpace(v.val))
		if strings.ContainsAny(v.val, ":@") || reservedServiceNames[norm] ||
			s.isBridgeSubject(norm) || s.isBridgeSubject(strings.TrimSpace(v.val)) {
			return fmt.Errorf("%s %q is a reserved service identity: registering it requires the token-mint capability", v.field, v.val)
		}
	}
	return nil
}

// isAdminRequest reports whether the request may use the embeddings admin
// routes: an OIDC caller holding the admin role (`kd login` then `kd embed
// sync`), or any caller of a daemon with auth disabled. The primary static
// token that used to be the only way in is retired (gd-ke6PC7bRto).
func (s *BeadsServer) isAdminRequest(r *http.Request) bool {
	switch authMethodFromContext(r.Context()) {
	case authMethodNone:
		return true
	case authMethodOIDC:
		return hasMinimumRole(RolesFromContext(r.Context()), RoleAdmin)
	}
	return false
}
