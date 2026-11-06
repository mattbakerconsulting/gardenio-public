package server

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
)

type contextKey string

const fleetContextKey contextKey = "fleet"

// tokenProjectContextKey holds the project scope for project-bound fleet tokens.
const tokenProjectContextKey contextKey = "token_project"

// tokenTypeScopeContextKey holds the allowed bead types for a scoped static token.
const tokenTypeScopeContextKey contextKey = "token_type_scope"

// tokenOpScopeContextKey holds the allowed operations for a scoped static token.
const tokenOpScopeContextKey contextKey = "token_op_scope"

// bridgeTokenInfo holds the fleet (audit-attribution) name and optional
// project/type/operation scopes for a bridge token.
//
// `Fleet` is an audit-attribution field only — writes stamp this as `bead.fleet`
// so the audit trail can identify which bridge / service made each write. The
// old per-token read-isolation gate (`CrossFleetRead`) was a YAGNI multi-tenant
// escape hatch; removed under epic ka-0000000871 / ka-0000003285. All tokens
// now read across all fleets; project-scope (Project field) remains as the
// real per-token authz boundary if needed.
type bridgeTokenInfo struct {
	Fleet        string
	Project      string    // empty means no project scope
	AllowedTypes []string  // empty means unrestricted
	AllowedOps   []string  // empty means unrestricted
	ExpiresAt    time.Time // zero means never expires (back-compat for seed/admin tokens)
	// Subject scopes audit attribution for tokens minted under this fleet.
	// When set, AuthIdentity.Subject is populated from this value; otherwise
	// it falls back to Fleet (back-compat). Per ADR §C1.4.
	Subject string
	// Privileged marks a designated service-fleet identity (e.g. the
	// gitlab-bridge) that may perform system-managed graph writes —
	// produces/triggered-by deps and mr_url back-fill — without tripping
	// per-bead ownership. It relaxes ONLY the ownership gate
	// (authorizeOwnership); type/project write-policy and token scopes still
	// apply. Ordinary fleet tokens leave this false and stay ownership-gated.
	// See ka-0000001946.
	Privileged bool
}

// FleetConfig holds fleet resolution configuration. It is safe for concurrent
// use; the token-to-fleet map can be updated at runtime via RegisterToken and
// RevokeBridgeToken.
type FleetConfig struct {
	// DefaultFleet is used when no token-to-fleet mapping is found.
	DefaultFleet string

	// now is used for IsExpired; tests can override it. Defaults to time.Now.
	now func() time.Time

	mu          sync.RWMutex
	tokenFleets map[string]bridgeTokenInfo // token -> fleet + project
	// removed holds every token this daemon has dropped (RevokeBridgeToken,
	// removeToken) since it started. RegisterPersistedToken skips them, so the
	// persisted-token re-sync never brings back a token whose config row
	// outlived its revoke (the row delete is best-effort). It lives only as
	// long as the process and grows only with revokes.
	removed map[string]struct{}
	// privilegedFleets is the set of fleet names whose tokens are PRIVILEGED
	// service-fleet identities (ka-0000001946). Any token whose Fleet is in
	// this set is marked Privileged at registration time, regardless of which
	// load path registered it (BEADS_FLEET_MAP env, a bridge_token: config
	// bead, or the controller token-rotator). This is what lets the live
	// gitlab-bridge token — loaded from the bridge-tokens Secret via the
	// fleet-map, which has no per-token privileged field — actually receive
	// the bypass. Populated from BEADS_PRIVILEGED_FLEETS via SetPrivilegedFleets.
	privilegedFleets map[string]bool
	// machineClients is the registry of Keycloak client ids whose
	// service-account tokens are machine principals (authMethodMachine,
	// gd-gQ4iLIpWCm). It rides on FleetConfig so AuthMiddleware /
	// AuthInterceptor keep their signatures. Populated from
	// BEADS_MACHINE_CLIENTS via SetMachineClients; empty means the machine
	// door is closed.
	machineClients map[string]bool
}

// NewFleetConfig creates a FleetConfig with the given defaults and initial
// token mappings. If tokenFleets is nil an empty map is allocated.
func NewFleetConfig(defaultFleet string, tokenFleets map[string]string) *FleetConfig {
	m := make(map[string]bridgeTokenInfo)
	for tok, fleet := range tokenFleets {
		m[tok] = bridgeTokenInfo{Fleet: fleet}
	}
	return &FleetConfig{
		DefaultFleet: defaultFleet,
		tokenFleets:  m,
		now:          time.Now,
	}
}

// SetPrivilegedFleets designates a set of fleet names as PRIVILEGED
// service-fleet identities (ka-0000001946) and re-marks every already-registered
// token whose Fleet is in the set. Tokens registered afterwards are marked at
// registration time via put(). Call once at daemon startup after NewFleetConfig
// and before/after the config-bead merge — it is idempotent and order-independent.
func (fc *FleetConfig) SetPrivilegedFleets(fleets []string) {
	if fc == nil {
		return
	}
	set := make(map[string]bool, len(fleets))
	for _, f := range fleets {
		if f = strings.TrimSpace(f); f != "" {
			set[f] = true
		}
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.privilegedFleets = set
	// Re-mark existing tokens so fleet-map entries loaded before this call
	// (NewFleetConfig) pick up the privileged flag.
	for tok, info := range fc.tokenFleets {
		if set[info.Fleet] {
			info.Privileged = true
			fc.tokenFleets[tok] = info
		}
	}
}

// SetMachineClients registers the Keycloak client ids whose service-account
// tokens are machine principals, replacing any prior set. Blank entries are
// dropped. Call once at daemon startup.
func (fc *FleetConfig) SetMachineClients(clients []string) {
	if fc == nil {
		return
	}
	set := make(map[string]bool, len(clients))
	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" {
			set[c] = true
		}
	}
	fc.mu.Lock()
	fc.machineClients = set
	fc.mu.Unlock()
}

// IsMachineClient reports whether clientID is a registered machine client.
func (fc *FleetConfig) IsMachineClient(clientID string) bool {
	if fc == nil || clientID == "" {
		return false
	}
	fc.mu.RLock()
	ok := fc.machineClients[clientID]
	fc.mu.RUnlock()
	return ok
}

// HasMachineClients reports whether any machine client is registered.
func (fc *FleetConfig) HasMachineClients() bool {
	if fc == nil {
		return false
	}
	fc.mu.RLock()
	n := len(fc.machineClients)
	fc.mu.RUnlock()
	return n > 0
}

// put stores a token's info, forcing Privileged=true when the token's fleet is
// in the privilegedFleets set. Callers must hold fc.mu.
func (fc *FleetConfig) put(token string, info bridgeTokenInfo) {
	if fc.privilegedFleets[info.Fleet] {
		info.Privileged = true
	}
	fc.tokenFleets[token] = info
}

// SetNowFunc overrides the clock used by IsExpired. Test-only.
func (fc *FleetConfig) SetNowFunc(f func() time.Time) {
	if fc == nil || f == nil {
		return
	}
	fc.mu.Lock()
	fc.now = f
	fc.mu.Unlock()
}

// ParseFleetMap parses a BEADS_FLEET_MAP string into a token-to-fleet map.
// Format: "token1:fleet1,token2:fleet2"
func ParseFleetMap(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	m := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if k, v, ok := strings.Cut(entry, ":"); ok {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k != "" && v != "" {
				m[k] = v
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// ResolveFleet returns the fleet name for the given token.
// It checks the token-to-fleet map first, then falls back to the default.
func (fc *FleetConfig) ResolveFleet(token string) string {
	if fc == nil {
		return "default"
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	if ok {
		return info.Fleet
	}
	if fc.DefaultFleet != "" {
		return fc.DefaultFleet
	}
	return "default"
}

// ResolveProject returns the project scope for the given token, or empty if
// the token has no project restriction.
func (fc *FleetConfig) ResolveProject(token string) string {
	if fc == nil {
		return ""
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	if ok {
		return info.Project
	}
	return ""
}

// HasToken reports whether the given token exists in the fleet map.
func (fc *FleetConfig) HasToken(token string) bool {
	if fc == nil {
		return false
	}
	fc.mu.RLock()
	_, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	return ok
}

// HasTokens reports whether any token-to-fleet mappings exist.
func (fc *FleetConfig) HasTokens() bool {
	if fc == nil {
		return false
	}
	fc.mu.RLock()
	n := len(fc.tokenFleets)
	fc.mu.RUnlock()
	return n > 0
}

// RegisterToken adds a token-to-fleet mapping with no project scope.
func (fc *FleetConfig) RegisterToken(token, fleet string) {
	fc.mu.Lock()
	fc.put(token, bridgeTokenInfo{Fleet: fleet})
	fc.mu.Unlock()
}

// RegisterTokenWithProject adds a token-to-fleet mapping with an optional
// project scope. Tokens with a non-empty project can only access beads
// matching that project label.
func (fc *FleetConfig) RegisterTokenWithProject(token, fleet, project string) {
	fc.mu.Lock()
	fc.put(token, bridgeTokenInfo{Fleet: fleet, Project: project})
	fc.mu.Unlock()
}

// RegisterTokenWithScopes adds a token-to-fleet mapping with full scope
// control. Empty slices for allowedTypes/allowedOps mean unrestricted.
func (fc *FleetConfig) RegisterTokenWithScopes(token, fleet, project string, allowedTypes, allowedOps []string) {
	fc.mu.Lock()
	fc.put(token, bridgeTokenInfo{
		Fleet:        fleet,
		Project:      project,
		AllowedTypes: allowedTypes,
		AllowedOps:   allowedOps,
	})
	fc.mu.Unlock()
}

// RegisterTokenWithExpiry adds a token-to-fleet mapping with full scope control
// and an expiration time. A zero expiresAt means the token never expires
// (back-compat for seed/admin tokens).
func (fc *FleetConfig) RegisterTokenWithExpiry(token, fleet, project string, allowedTypes, allowedOps []string, expiresAt time.Time) {
	fc.mu.Lock()
	fc.put(token, bridgeTokenInfo{
		Fleet:        fleet,
		Project:      project,
		AllowedTypes: allowedTypes,
		AllowedOps:   allowedOps,
		ExpiresAt:    expiresAt,
	})
	fc.mu.Unlock()
}

// RegisterTokenWithSubject adds a token-to-fleet mapping with full scope
// control, an expiration time, and an audit subject. An empty subject falls
// back to the fleet name for audit attribution (back-compat).
func (fc *FleetConfig) RegisterTokenWithSubject(token, fleet, project string, allowedTypes, allowedOps []string, expiresAt time.Time, subject string) {
	fc.RegisterTokenFull(token, fleet, project, allowedTypes, allowedOps, expiresAt, subject, false)
}

// RegisterTokenFull is the full-fidelity registration path. `privileged`
// marks a designated service-fleet identity that may bypass per-bead
// ownership for system-managed graph writes (ka-0000001946). All other
// callers should use RegisterTokenWithSubject (privileged=false).
func (fc *FleetConfig) RegisterTokenFull(token, fleet, project string, allowedTypes, allowedOps []string, expiresAt time.Time, subject string, privileged bool) {
	fc.mu.Lock()
	fc.put(token, bridgeTokenInfo{
		Fleet:        fleet,
		Project:      project,
		AllowedTypes: allowedTypes,
		AllowedOps:   allowedOps,
		ExpiresAt:    expiresAt,
		Subject:      subject,
		Privileged:   privileged,
	})
	delete(fc.removed, token)
	fc.mu.Unlock()
}

// RegisterPersistedToken registers a token read back from the config store,
// unless this daemon already holds it or has removed it. It reports whether
// the token was added. The persisted-token re-sync uses it: a token another
// daemon process registered (one still serving during a rolling update, after
// this one loaded the store at startup) is picked up, while a token revoked
// here stays revoked even if its config row survived.
func (fc *FleetConfig) RegisterPersistedToken(token, fleet, project string, allowedTypes, allowedOps []string, expiresAt time.Time, subject string, privileged bool) bool {
	if fc == nil || token == "" {
		return false
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if _, ok := fc.tokenFleets[token]; ok {
		return false
	}
	if _, ok := fc.removed[token]; ok {
		return false
	}
	fc.put(token, bridgeTokenInfo{
		Fleet:        fleet,
		Project:      project,
		AllowedTypes: allowedTypes,
		AllowedOps:   allowedOps,
		ExpiresAt:    expiresAt,
		Subject:      subject,
		Privileged:   privileged,
	})
	return true
}

// markRemoved records token as removed. Callers must hold fc.mu.
func (fc *FleetConfig) markRemoved(token string) {
	if fc.removed == nil {
		fc.removed = make(map[string]struct{})
	}
	fc.removed[token] = struct{}{}
}

// RegisterBridgeToken adds a token-to-fleet mapping for a bridge service.
// `Fleet` is an audit-attribution tag; bridges write under their fleet name
// so the audit trail can identify which service made each write. All tokens
// (bridge or otherwise) read across every fleet — the old per-token
// read-isolation gate was removed under epic ka-0000000871 / ka-0000003285.
func (fc *FleetConfig) RegisterBridgeToken(token, fleet, subject string) {
	fc.mu.Lock()
	fc.put(token, bridgeTokenInfo{
		Fleet:   fleet,
		Subject: subject,
	})
	fc.mu.Unlock()
}

// ResolveSubject returns the audit subject for the given token, or empty if
// the token has no subject set (caller falls back to Fleet for attribution).
func (fc *FleetConfig) ResolveSubject(token string) string {
	if fc == nil {
		return ""
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	if !ok {
		return ""
	}
	return info.Subject
}

// ResolvePrivileged reports whether the given token is a privileged
// service-fleet identity that may bypass per-bead ownership for system-managed
// graph writes (ka-0000001946). Unknown tokens return false.
func (fc *FleetConfig) ResolvePrivileged(token string) bool {
	if fc == nil {
		return false
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	return ok && info.Privileged
}

// ResolveExpiresAt returns the expiration time for the given token, or the
// zero time if the token has no expiration (or is unknown).
func (fc *FleetConfig) ResolveExpiresAt(token string) time.Time {
	if fc == nil {
		return time.Time{}
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	if !ok {
		return time.Time{}
	}
	return info.ExpiresAt
}

// IsExpired reports whether the given token has a non-zero ExpiresAt that is
// in the past. Unknown tokens and tokens with zero ExpiresAt return false.
func (fc *FleetConfig) IsExpired(token string) bool {
	if fc == nil {
		return false
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	now := fc.now
	fc.mu.RUnlock()
	if !ok {
		return false
	}
	if info.ExpiresAt.IsZero() {
		return false
	}
	if now == nil {
		now = time.Now
	}
	return now().After(info.ExpiresAt)
}

// ResolveTypeScope returns the allowed bead types for the given token.
// Returns nil if the token has no type restrictions (unrestricted).
func (fc *FleetConfig) ResolveTypeScope(token string) []string {
	if fc == nil {
		return nil
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	if ok {
		return info.AllowedTypes
	}
	return nil
}

// ResolveOpScope returns the allowed operations for the given token.
// Returns nil if the token has no operation restrictions (unrestricted).
func (fc *FleetConfig) ResolveOpScope(token string) []string {
	if fc == nil {
		return nil
	}
	fc.mu.RLock()
	info, ok := fc.tokenFleets[token]
	fc.mu.RUnlock()
	if ok {
		return info.AllowedOps
	}
	return nil
}

// removeToken deletes a single token plaintext from the in-memory map.
// Used by the refresh-token cascade (RefreshTokenManager.Revoke) to purge
// access tokens that were minted by exchanging a now-revoked refresh.
func (fc *FleetConfig) removeToken(token string) {
	if fc == nil || token == "" {
		return
	}
	fc.mu.Lock()
	delete(fc.tokenFleets, token)
	fc.markRemoved(token)
	fc.mu.Unlock()
}

// RevokeBridgeToken removes every token mapping for the given fleet name and
// returns the removed tokens (nil when the fleet had none). A fleet holds
// several live tokens whenever it is re-minted, so stopping at the first match
// would leave the rest usable.
func (fc *FleetConfig) RevokeBridgeToken(fleet string) []string {
	if fc == nil {
		return nil
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var removed []string
	for t, info := range fc.tokenFleets {
		if info.Fleet == fleet {
			delete(fc.tokenFleets, t)
			fc.markRemoved(t)
			removed = append(removed, t)
		}
	}
	return removed
}

// ListBridgeTokens returns a sorted list of unique fleet names that have token
// mappings. Token values are never exposed.
func (fc *FleetConfig) ListBridgeTokens() []string {
	if fc == nil {
		return nil
	}
	fc.mu.RLock()
	seen := make(map[string]struct{}, len(fc.tokenFleets))
	for _, info := range fc.tokenFleets {
		seen[info.Fleet] = struct{}{}
	}
	fc.mu.RUnlock()
	fleets := make([]string, 0, len(seen))
	for f := range seen {
		fleets = append(fleets, f)
	}
	sort.Strings(fleets)
	return fleets
}

// BridgeTokenSummary is the per-fleet view emitted by ListBridgeTokensDetailed.
// Token plaintext is never included. ExpiresAt is the zero time when the
// fleet's token has no expiry (seed/admin path).
type BridgeTokenSummary struct {
	Fleet     string
	Project   string
	Subject   string
	ExpiresAt time.Time
}

// ListBridgeTokensDetailed returns one summary per fleet (sorted by fleet name).
// When a fleet has multiple registered tokens (e.g. mid-rotation overlap),
// the entry with the latest non-zero ExpiresAt wins so rotators see the
// active credential's expiry. Token values are never exposed.
func (fc *FleetConfig) ListBridgeTokensDetailed() []BridgeTokenSummary {
	if fc == nil {
		return nil
	}
	fc.mu.RLock()
	byFleet := make(map[string]BridgeTokenSummary, len(fc.tokenFleets))
	for _, info := range fc.tokenFleets {
		cur, ok := byFleet[info.Fleet]
		cand := BridgeTokenSummary{
			Fleet:     info.Fleet,
			Project:   info.Project,
			Subject:   info.Subject,
			ExpiresAt: info.ExpiresAt,
		}
		if !ok {
			byFleet[info.Fleet] = cand
			continue
		}
		// Prefer the entry with the latest non-zero ExpiresAt. A zero
		// ExpiresAt (never-expires) loses to any concrete expiry so the
		// rotator can detect rotation candidates.
		switch {
		case cur.ExpiresAt.IsZero() && !cand.ExpiresAt.IsZero():
			byFleet[info.Fleet] = cand
		case !cur.ExpiresAt.IsZero() && !cand.ExpiresAt.IsZero() && cand.ExpiresAt.After(cur.ExpiresAt):
			byFleet[info.Fleet] = cand
		}
	}
	fc.mu.RUnlock()
	out := make([]BridgeTokenSummary, 0, len(byFleet))
	for _, s := range byFleet {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fleet < out[j].Fleet })
	return out
}

// WithFleet returns a new context with the fleet value set.
func WithFleet(ctx context.Context, fleet string) context.Context {
	return context.WithValue(ctx, fleetContextKey, fleet)
}

// FleetFromContext returns the fleet from the context, or "default" if not set.
func FleetFromContext(ctx context.Context) string {
	if fleet, ok := ctx.Value(fleetContextKey).(string); ok && fleet != "" {
		return fleet
	}
	return "default"
}

// WithTokenProject returns a new context with the token's project scope set.
func WithTokenProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, tokenProjectContextKey, project)
}

// TokenProjectFromContext returns the project scope from the context set by a
// project-bound fleet token. Returns empty string if no project scope is set.
func TokenProjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tokenProjectContextKey).(string)
	return v
}

// WithTokenTypeScope returns a new context with the token's allowed bead types set.
func WithTokenTypeScope(ctx context.Context, types []string) context.Context {
	return context.WithValue(ctx, tokenTypeScopeContextKey, types)
}

// TokenTypeScopeFromContext returns the allowed bead types from the context set
// by a scoped fleet token. Returns nil if no type scope is set (unrestricted).
func TokenTypeScopeFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(tokenTypeScopeContextKey).([]string)
	return v
}

// WithTokenOpScope returns a new context with the token's allowed operations set.
func WithTokenOpScope(ctx context.Context, ops []string) context.Context {
	return context.WithValue(ctx, tokenOpScopeContextKey, ops)
}

// TokenOpScopeFromContext returns the allowed operations from the context set
// by a scoped fleet token. Returns nil if no op scope is set (unrestricted).
func TokenOpScopeFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(tokenOpScopeContextKey).([]string)
	return v
}

// tokenScopeAllows checks whether a value is permitted by the given scope list.
// An empty or nil scope list means unrestricted (all values allowed).
func tokenScopeAllows(scope []string, value string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, s := range scope {
		if s == "*" || s == value {
			return true
		}
	}
	return false
}

// beadMatchesTokenProject checks whether a bead's labels include the required
// project scope. Returns true if no project scope is set (unrestricted token)
// or if the bead has a matching "project:<name>" label.
func beadMatchesTokenProject(ctx context.Context, labels []string) bool {
	proj := TokenProjectFromContext(ctx)
	if proj == "" {
		return true // no project scope — unrestricted
	}
	target := "project:" + proj
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// labelLessReadableType reports whether a bead type is global by design when it
// carries no project label: advice (Global working rules, role-only advice) and
// config (claude-instructions, prerun, context, claude-settings), which
// `gb prime` / `gb setup` read with the agent's own token.
func labelLessReadableType(t model.BeadType) bool {
	return t == model.BeadType("advice") || t == model.BeadType("config")
}

// tokenReadsLabelLessBead reports whether a project-bound per-agent token may
// read this bead even though it lacks the token's project label: advice and
// config with NO project label at all, the label-less audit beads recording
// the agent's own actions (auditBeadOwnedByCaller), plus the token's OWN
// project's type:project bead (matched by title). A bead labelled for ANY
// project, this one or another, is untouched, so project isolation is
// unchanged. READ paths
// only: writes keep the strict beadMatchesTokenProject, so an agent still
// cannot edit global advice or config.
func tokenReadsLabelLessBead(ctx context.Context, b *model.Bead) bool {
	if hasProjectLabel(b.Labels) {
		return false
	}
	if _, isAgent := agentBeadIDFromContext(ctx); !isAgent {
		return false
	}
	if labelLessReadableType(b.Type) {
		return true
	}
	if auditBeadOwnedByCaller(ctx, b) {
		return true
	}
	if b.Type == model.BeadType("project") {
		proj := TokenProjectFromContext(ctx)
		return proj != "" && (b.Title == proj || b.Title == "Project: "+proj)
	}
	return false
}

// beadReadableByTokenProject is beadMatchesTokenProject for READ gates: the
// token's project label, or a label-less bead a per-agent token may read.
func beadReadableByTokenProject(ctx context.Context, b *model.Bead) bool {
	return beadMatchesTokenProject(ctx, b.Labels) || tokenReadsLabelLessBead(ctx, b)
}
