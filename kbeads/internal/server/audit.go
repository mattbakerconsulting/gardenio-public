package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// Resolver source labels used in structured logs (R9 §4.4 operated_by_source).
// Bounded set; do not introduce new values without updating R9.
const (
	operatedBySourceOIDC     = "oidc_subject"
	operatedBySourceHeader   = "static_header"
	operatedBySourceFallback = "agent_bead_fallback"
	operatedBySourceFleet    = "fleet_token"
	operatedBySourceMachine  = "machine_principal"
	// operatedBySourceTriggerLineage: a machine principal's create referenced a
	// trigger/schedule bead (fields.trigger_id) and the resolver lifted THAT
	// bead's human author server-side (decision gd-dI7PfsFlvr "lineage").
	operatedBySourceTriggerLineage = "trigger_lineage"
	operatedBySourceEmpty          = "empty"
)

// Refusal-source labels emitted by resolveAuditOperatorRequired when the
// resolver fails closed (no human operator attributable). Surfaced in the
// HTTP 400 error body as `resolver_source` so the kd CLI can exit with the
// EX_USAGE-family sentinel code 64 and the gardenio agent harness can react
// (squawk + file type=bug bead). Bounded set; do not extend without
// coordinating with the kd CLI / harness consumer (ka-0000006731).
const (
	// refusalSourceHeaderMissing: no X-Operated-By header was supplied AND
	// the agent-bead fallback step could not produce any candidate value
	// (bead missing, or no operated_by / slack_user_id fields). The caller
	// likely forgot to set BEADS_OPERATED_BY or is hitting an uninitialized
	// agent pod.
	refusalSourceHeaderMissing = "header_missing"
	// refusalSourceHeaderNonHuman: an X-Operated-By header was supplied but
	// rejected by sanitizeOperatedByHeader (control chars, length, or — most
	// commonly — non-human shape: agent name, sentinel, malformed prefix).
	// The header was discarded before reaching the resolver.
	refusalSourceHeaderNonHuman = "header_non_human"
	// refusalSourceFallbackAgentOnly: no header was supplied; the agent-bead
	// fallback ran and read a non-empty operated_by or slack_user_id but the
	// value was not a human-shape id (failed isHumanOperator). The spawner
	// pushed an agent-id-shaped value into the agent bead — gardener bug.
	refusalSourceFallbackAgentOnly = "fallback_agent_only"
	// chain_root_emailless is reserved for the X-Spawned-By chain walk
	// follow-up (ka-0000006147) and is not emitted by the resolver today;
	// the named constant will be reintroduced when the chain walker lands.
	// refusalSourceLegacySentinelCutover: the resolver settled on the
	// system:legacy-pre-attribution sentinel but the R6 cut-over is active
	// (ka-0000006798), so the write is refused rather than binding a new row
	// to the historical sink. Distinct from the header_* / fallback_* causes
	// because the operator IS resolvable — it is just the one operator no new
	// write may bind post-cut-over.
	refusalSourceLegacySentinelCutover = "legacy_sentinel_cutover"
	// refusalSourceUnknown: catch-all when classification could not pin a
	// specific cause. Should never be emitted in steady state; presence in
	// logs indicates classifyOperatorRefusal needs a new case.
	refusalSourceUnknown = "unknown"
)

// operatorRefusalKey holds tracking information populated during operator
// resolution so classifyOperatorRefusal can determine WHY the resolver
// fell through to empty. Set by the static-token interceptor (header
// presence/rejection) and by operatedByFromAgentBead (fallback attempt
// outcome).
const operatorRefusalKey contextKey = "operator_refusal_trace"

// operatorRefusalTrace records the breadcrumbs the resolver leaves so a
// failed resolution can be classified. All booleans default to false; a
// nil trace means "no information available" and yields refusalSourceUnknown.
type operatorRefusalTrace struct {
	// headerSent is true when the static-token interceptor saw a non-empty
	// X-Operated-By header on the request, before sanitization.
	headerSent bool
	// headerRejected is true when applyOperatedByHeader sanitized the
	// supplied header and rejected it (shape/length/control chars).
	headerRejected bool
	// fallbackAttempted is true when operatedByFromAgentBead ran (i.e. the
	// caller was a static-token identity with a Subject that could be looked
	// up in the bead store).
	fallbackAttempted bool
	// fallbackFoundNonHuman is true when operatedByFromAgentBead read a
	// non-empty value from the agent bead but rejected it as non-human shape.
	fallbackFoundNonHuman bool
	// legacySentinelCutover is true when the resolver DID attribute the write
	// to the system:legacy-pre-attribution sentinel but the R6 cut-over
	// (ka-0000006798) refused it. Distinct from the header_*/fallback_* causes
	// (the operator resolved fine — it is just the one operator no new write
	// may bind), so classifyOperatorRefusal reports it first.
	legacySentinelCutover bool
}

// withOperatorRefusalTrace attaches a new (zeroed) refusal trace to ctx
// so subsequent resolver steps can record their breadcrumbs. Idempotent —
// returns ctx unchanged if a trace already exists so the auth interceptor
// can install the trace before any header processing without clobbering
// downstream calls in tests that set up their own ctx.
func withOperatorRefusalTrace(ctx context.Context) context.Context {
	if _, ok := ctx.Value(operatorRefusalKey).(*operatorRefusalTrace); ok {
		return ctx
	}
	return context.WithValue(ctx, operatorRefusalKey, &operatorRefusalTrace{})
}

// operatorRefusalTraceFromContext returns the trace pointer, or nil when
// none was installed. The pointer is mutated in-place by resolver steps.
func operatorRefusalTraceFromContext(ctx context.Context) *operatorRefusalTrace {
	t, _ := ctx.Value(operatorRefusalKey).(*operatorRefusalTrace)
	return t
}

// classifyOperatorRefusal maps the trace breadcrumbs to a refusalSource*
// label. Called only when resolveAuditOperatorRequired is about to return
// ok=false. The precedence order:
//
//  1. R6 legacy-sentinel cut-over refusal → legacy_sentinel_cutover
//  2. header rejected by sanitizer  → header_non_human
//  3. fallback ran AND found non-human value → fallback_agent_only
//  4. no header sent at all (trace.headerSent == false) → header_missing
//  5. otherwise → unknown (defensive — should not occur post-rollout)
//
// chain_root_emailless is reserved for the X-Spawned-By chain walker
// (ka-0000006147); this function does not emit it today.
func classifyOperatorRefusal(ctx context.Context) string {
	t := operatorRefusalTraceFromContext(ctx)
	if t == nil {
		return refusalSourceUnknown
	}
	if t.legacySentinelCutover {
		return refusalSourceLegacySentinelCutover
	}
	if t.headerRejected {
		return refusalSourceHeaderNonHuman
	}
	if t.fallbackFoundNonHuman {
		return refusalSourceFallbackAgentOnly
	}
	if !t.headerSent {
		return refusalSourceHeaderMissing
	}
	return refusalSourceUnknown
}

// AuthIdentity describes the authenticated caller for audit logging.
type AuthIdentity struct {
	// Type is the authentication method: "fleet", "oidc", "machine"
	// (registered Keycloak service-account client, machine_identity.go), "edge"
	// (gardenio-edge-auth HMAC envelope), or "none".
	Type string
	// Subject is the user identifier — email for OIDC tokens, fleet name for
	// fleet tokens. (Historic rows carry "static" from the retired primary
	// token, gd-ke6PC7bRto.)
	Subject string
	// Privileged is set only for designated privileged service-fleet tokens
	// (Type=="fleet"). It relaxes the per-bead ownership gate so the identity
	// can perform system-managed graph writes (produces/triggered-by deps,
	// mr_url back-fill) without being the bead's creator/assignee/owner. It
	// does NOT relax type/project write-policy or token scopes. See
	// ka-0000001946.
	Privileged bool
	// ClientID is the OAuth client an OIDC token was issued to (its "azp"
	// claim); empty for non-OIDC identities and tokens without one. It
	// distinguishes a `kd login` device-grant token (DeviceGrantClientID)
	// from a browser SSO token. See ClientExtractor.
	ClientID string
	// GitHubID and GitHubLogin are the GitHub account an OIDC user signed in
	// through (the github_id and github_login claims, see GitHubExtractor);
	// zero values for every other identity. GitHubID is the stable key: a
	// login can be renamed on GitHub.
	GitHubID    int64
	GitHubLogin string
	// ImpersonatedBy is the admin behind an impersonated OIDC session: the
	// verified token's "impersonator" claim (ImpersonationExtractor), never a
	// header or a request field. Empty for every other session and identity.
	// Writes under it are stamped impersonated_by (impersonation.go).
	ImpersonatedBy string
	// SessionID is the verified token's Keycloak session ("sid"), read only
	// alongside ImpersonatedBy: it keys the one audit bead per impersonated
	// session.
	SessionID string
}

const authIdentityKey contextKey = "auth_identity"

// WithAuthIdentity returns a new context with the auth identity set.
func WithAuthIdentity(ctx context.Context, id AuthIdentity) context.Context {
	return context.WithValue(ctx, authIdentityKey, id)
}

// AuthIdentityFromContext returns the auth identity from the context.
// Returns a zero AuthIdentity with Type "anonymous" if not set.
func AuthIdentityFromContext(ctx context.Context) AuthIdentity {
	if id, ok := ctx.Value(authIdentityKey).(AuthIdentity); ok {
		return id
	}
	return AuthIdentity{Type: "anonymous"}
}

// actorFromContext returns the server-derived actor string for audit fields
// (CreatedBy, ClosedBy, Author). The server MUST NOT trust a client-supplied
// actor — every mutation path resolves the actor from the authenticated
// identity to prevent audit-log spoofing (AUDIT-2026-04-18 §C3).
//
// The returned value is the AuthIdentity Subject, which is:
//   - an email for OIDC tokens,
//   - the fleet name for fleet tokens,
//   - the machine client id for machine principals,
//   - "" when auth is disabled or the caller is anonymous.
func actorFromContext(ctx context.Context) string {
	return AuthIdentityFromContext(ctx).Subject
}

// sanitizeActorHeader validates an X-Actor header/metadata value supplied by
// a static-primary-token caller. Returns the cleaned value and true when
// acceptable; "" and false otherwise.
//
// Rules: non-empty after trimming, length ≤ 128, and no control characters
// (including CR/LF/TAB) — these would corrupt downstream log lines and open
// header-injection vectors.
func sanitizeActorHeader(raw string) (string, bool) {
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

// operatedByHeaderKey holds the sanitized X-Operated-By header value when
// the static-token interceptor accepted it. Access via WithOperatedByHeader /
// operatedByHeaderFromContext; the value is read by resolveOperatedBy.
const operatedByHeaderKey contextKey = "operated_by_header"

// WithOperatedByHeader returns a new context with the operator header value set.
// The value MUST already be sanitized — see sanitizeOperatedByHeader.
func WithOperatedByHeader(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, operatedByHeaderKey, v)
}

// systemOperatorOverrideKey holds a "system:<source>" fallback operator string
// that the resolver consults ONLY when no identity-based attribution is
// available. Use WithSystemOperator to set it.
const systemOperatorOverrideKey contextKey = "system_operator_override"

// WithSystemOperator attaches a last-resort system-operator string to ctx for
// server-internal recording paths (audit log, escalation replay, follower
// notifications). The resolver consults the override only when normal
// identity-based attribution returns empty — an OIDC ctx (alice@example.com)
// still resolves to oidc:alice and the system override is ignored. Use this
// at the entry of any code path that may perform a write on the server's own
// behalf from a context that lacks attributable identity (e.g. an audit bead
// recorded after an anonymous denial); without it, post-R2 (ka-0000005772)
// fail-closed semantics reject the write.
//
// The override only governs the operator FK / operated_by column. The bead's
// CreatedBy and audit Actor still derive from the original AuthIdentity, so
// the originating human (when present) is preserved.
func WithSystemOperator(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, systemOperatorOverrideKey, "system:"+source)
}

// WithMachineOperator attaches the single allowlisted machine-origin operator
// (operatorMachine == "system:machine") to ctx for a TRUE machine-origin write
// with no human behind it. It is the post-collapse, self-documenting form of
// the per-source WithSystemOperator overrides (system:audit-log,
// system:escalation-replay, system:audit-retention, …): when the authoring
// human gate is enabled the resolver COLLAPSES every system:<source> override
// — including the ones still set via WithSystemOperator at not-yet-migrated
// call sites — into this one system:machine sink, so every infra write funnels
// through a single auditable terminal. New infra call sites should prefer
// WithMachineOperator so the intent ("this is a machine write") is explicit at
// the call site rather than implied by the resolver collapse.
//
// Like WithSystemOperator the override governs only the operator FK /
// operated_by column; CreatedBy and the audit Actor still derive from the
// originating AuthIdentity. Use this ONLY at the code-reviewed infra call sites
// enumerated in the directive design (audit_log.go, http_escalations.go,
// audit_retention.go, and — docs/adr/taints.md §6.5's scrub execution step,
// gd-fVAy32nWDf — http_scrub_requests.go) — never as a resolver fallback for a
// write that should have carried a human.
func WithMachineOperator(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemOperatorOverrideKey, operatorMachine)
}

// systemOperatorFromContext returns the stored override or "" when unset.
func systemOperatorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(systemOperatorOverrideKey).(string)
	return v
}

// operatedByHeaderFromContext returns the sanitized X-Operated-By header value,
// or "" when not set or when the auth interceptor refused the override.
func operatedByHeaderFromContext(ctx context.Context) string {
	v, _ := ctx.Value(operatedByHeaderKey).(string)
	return v
}

// noAutoParentKey holds the bool flag set by the static-token interceptor when
// a caller passed X-No-Auto-Parent: 1 to opt out of the server-side
// auto-parent-child dep that createBead adds when an agent actor writes a new
// bead. Access via WithNoAutoParent / noAutoParentFromContext.
const noAutoParentKey contextKey = "no_auto_parent"

// WithNoAutoParent returns a new context that opts out of the server-side
// auto-parent-child dep injection. Only set by the static-token interceptor
// when the caller sent X-No-Auto-Parent: 1.
func WithNoAutoParent(ctx context.Context) context.Context {
	return context.WithValue(ctx, noAutoParentKey, true)
}

// noAutoParentFromContext reports whether the caller opted out of the
// server-side auto-parent-child dep injection.
func noAutoParentFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noAutoParentKey).(bool)
	return v
}

// parseNoAutoParentHeader reports whether a raw X-No-Auto-Parent header value
// should be interpreted as an opt-out. Accepts the same truthy set as the
// operated_by feature flag for consistency.
func parseNoAutoParentHeader(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// forceParentEdgeKey holds the bool flag set by the interceptor when a caller
// passed X-Force-Parent-Edge: 1 to override the closed-parent guard on an
// explicit dep-add (ka-0000005756). It is the deliberate escape hatch for
// legitimately re-parenting new work onto a recently-closed epic — the author
// named the parent knowing it is closed. Access via WithForceParentEdge /
// forceParentEdgeFromContext.
const forceParentEdgeKey contextKey = "force_parent_edge"

// WithForceParentEdge returns a new context that overrides the closed-parent
// guard for an explicit parent-child dep-add. Set by the interceptor when the
// caller sent X-Force-Parent-Edge: 1 (`kd dep add --force`).
func WithForceParentEdge(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceParentEdgeKey, true)
}

// forceParentEdgeFromContext reports whether the caller opted to override the
// closed-parent guard on an explicit parent-child dep-add.
func forceParentEdgeFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(forceParentEdgeKey).(bool)
	return v
}

// parseForceParentEdgeHeader reports whether a raw X-Force-Parent-Edge header
// value should be interpreted as an override. Accepts the same truthy set as
// the other opt-out headers for consistency.
func parseForceParentEdgeHeader(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// triggerBeadKey holds the sanitized X-Trigger-Bead header value set by the
// static-token interceptor. Read by the claim path in updateBead so it can
// wire a `triggered-by` lineage edge from the claimed task to the trigger
// bead — the M3 milestone of the auto-link epic (ka-0000000933, ka-0000003164).
// Catches the case M2 cannot reach: an agent that creates its OWN tracking
// task and claims it, where the per-spawn-path triggered-by writes never see
// the agent-authored bead.
const triggerBeadKey contextKey = "trigger_bead"

// WithTriggerBead returns a new context carrying the sanitized X-Trigger-Bead
// header value. The value is the ka-XXX id of the originating trigger bead
// (trigger definition, source bead, Slack thread bead, etc.) propagated from
// the agent pod's BOAT_TRIGGER_BEAD_ID env var.
func WithTriggerBead(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, triggerBeadKey, id)
}

// triggerBeadFromContext returns the sanitized X-Trigger-Bead header value
// set by the static-token interceptor, or "" when not set / rejected.
func triggerBeadFromContext(ctx context.Context) string {
	v, _ := ctx.Value(triggerBeadKey).(string)
	return v
}

// spawnChainSubjectKey holds the lineage-seed agent/bead id that the fleet
// resolver branch chain-walks when a header-less service write references a
// spawn source (ka-0000001404). Set by createBead from the write's own
// fields.spawned_by BEFORE the resolver runs, it is the daemon-side backstop
// to the client stamping (ka-0000007919): a bridge/service write that carries
// a spawned_by edge but no X-Operated-By header gets chain-walked back to the
// ultimate invoking human instead of silently binding the fleet sentinel.
const spawnChainSubjectKey contextKey = "spawn_chain_subject"

// WithSpawnChainSubject returns a new context carrying the lineage-seed id the
// fleet resolver branch should chain-walk (operatedByFromAgentBead) when no
// X-Operated-By header is present. The value is an agent NAME or a gd- bead id
// lifted from the write's own fields.spawned_by.
func WithSpawnChainSubject(ctx context.Context, id string) context.Context {
	if strings.TrimSpace(id) == "" {
		return ctx
	}
	return context.WithValue(ctx, spawnChainSubjectKey, id)
}

// spawnChainSubjectFromContext returns the lineage-seed id set by createBead,
// or "" when the write carried no fields.spawned_by.
func spawnChainSubjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(spawnChainSubjectKey).(string)
	return v
}

// spawnedByFromFields extracts the trimmed fields.spawned_by value from a
// createBead input's raw Fields JSON, or "" when the field is absent, empty,
// or the JSON is malformed. Used by createBead to seed the fleet-branch
// chain-walk backstop (ka-0000001404) — kept resilient (never errors) because
// a malformed Fields blob is already rejected downstream by field validation.
func spawnedByFromFields(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	v, _ := fields["spawned_by"].(string)
	return strings.TrimSpace(v)
}

// triggerLineageSubjectKey holds the trigger/schedule bead id a MACHINE
// principal's create references through its own fields.trigger_id (decision
// gd-dI7PfsFlvr "lineage"). Set by createBead BEFORE the resolver runs, read by
// resolveOperatedByPure's machine case, which lifts the referenced bead's human
// author as the write's operated_by. The id is a reference the server
// dereferences and verifies (type must be trigger/schedule, author must be
// human); nothing client-asserted is trusted, unlike the X-Operated-By relay
// the fleet/static branches honour. This is what keeps the trigger-engine's
// fires attributed to the human who authored the trigger once the engine no
// longer holds the static token: without it every agent it spawns would carry
// operated_by=machine:gardenio-trigger-engine, and the agent's own writes would
// then fail the resolver (no human in the agent-bead walk).
const triggerLineageSubjectKey contextKey = "trigger_lineage_subject"

// triggerLineageField is the create-input field the trigger-engine stamps on
// every agent bead it spawns, every task bead it files and every molecule it
// pours (internal/bridge/triggers.go actionSpawnAgent / actionApplyFormula /
// createTaskBead: extraFields["trigger_id"] = trig.ID).
const triggerLineageField = "trigger_id"

// WithTriggerLineageSubject returns a new context carrying the trigger/schedule
// bead id the machine resolver branch should dereference for a human author.
func WithTriggerLineageSubject(ctx context.Context, id string) context.Context {
	if strings.TrimSpace(id) == "" {
		return ctx
	}
	return context.WithValue(ctx, triggerLineageSubjectKey, id)
}

// triggerLineageSubjectFromContext returns the id set by createBead, or ""
// when the write carried no fields.trigger_id.
func triggerLineageSubjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(triggerLineageSubjectKey).(string)
	return v
}

// triggerIDFromFields extracts the trimmed fields.trigger_id value from a
// createBead input's raw Fields JSON, or "" when absent, empty, non-string or
// malformed (a malformed blob is rejected downstream by field validation).
func triggerIDFromFields(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	v, _ := fields[triggerLineageField].(string)
	return strings.TrimSpace(v)
}

// operatedByFromTriggerBead dereferences a trigger lineage reference: it loads
// the bead, requires it to be an autonomous type (trigger/schedule — the only
// types whose author legitimately stands behind unattended fires), and returns
// that bead's human author, or "" when there is none.
//
// The author is read from the bead's server-resolved operated_by column first
// (the operator kbeads itself attributed when the trigger was created, through
// a human credential or a human-relaying bridge) and then from
// fields.operated_by (what a chain heir inherits from its predecessor when the
// heir was itself created by the engine). Both are accepted ONLY when
// isHumanOperator: a trigger authored by a sentinel, an agent or a machine
// yields "" and the caller falls back to the machine subject, so this can only
// ADD a human, never bind a non-human or downgrade the fail-closed boundary
// (the same invariant as the fleet-branch spawn-chain backstop).
//
// The fields.operated_by fallback is honoured only when the bead's CreatedBy is
// engine, the calling machine's own subject. fields is client-supplied JSON, so
// on a trigger anyone else created it proves nothing: a crew agent could stamp
// a victim's identity there and have the engine's fires attributed to them.
// Only the engine writes the chain-heir stamp, and its writes carry
// CreatedBy=machine:<clientId>.
func (s *BeadsServer) operatedByFromTriggerBead(ctx context.Context, id, engine string) string {
	if s == nil || s.store == nil || id == "" {
		return ""
	}
	bead, err := s.store.GetBead(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			recordResolverError("trigger_lookup_fail")
		}
		return ""
	}
	return triggerBeadHumanAuthor(bead, engine)
}

// triggerBeadHumanAuthor is the pure half of operatedByFromTriggerBead: the
// human author of an already-loaded trigger/schedule bead, or "" when it is
// not an autonomous type or has no human author. The spawn gate
// (authorizeTriggerLineageSpawn) shares it so the identity it authorizes is
// exactly the one the resolver stamps as operated_by.
func triggerBeadHumanAuthor(bead *model.Bead, engine string) string {
	if bead == nil || !isAutonomousBeadType(bead.Type) {
		return ""
	}
	if isHumanOperator(bead.OperatedBy) {
		return bead.OperatedBy
	}
	if engine == "" || bead.CreatedBy != engine || len(bead.Fields) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		return ""
	}
	if v, _ := fields["operated_by"].(string); isHumanOperator(strings.TrimSpace(v)) {
		return strings.TrimSpace(v)
	}
	return ""
}

// sanitizeTriggerBeadHeader validates an X-Trigger-Bead header value supplied
// by a static-primary-token caller. Returns the cleaned value and true when
// acceptable; "" and false otherwise.
//
// Rules:
//   - non-empty after trimming, length ≤ 64.
//   - must begin with "gd-" (newly minted) or "ka-" (archived ACME
//     reference) so a stray non-id value cannot smuggle a bogus lineage edge.
//   - body after the prefix must be alphanumeric or `-` (matches the bead-id
//     character set used elsewhere, e.g. sanitizeUserBeadIDHeader).
//     Control characters and other punctuation are rejected.
func sanitizeTriggerBeadHeader(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 64 {
		return "", false
	}
	body, ok := strings.CutPrefix(s, "gd-")
	if !ok {
		body, ok = strings.CutPrefix(s, "ka-")
	}
	if !ok {
		return "", false
	}
	if body == "" {
		return "", false
	}
	for _, r := range body {
		switch {
		case r >= '0' && r <= '9',
			r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r == '-':
		default:
			return "", false
		}
	}
	return s, true
}

// autoParentTarget carries the two pointers the server-side auto-parent step
// reads off an agent bead in ONE fetch (ka-0000007300):
//
//   - DepTarget — the parent-child dep target (the work the agent is doing).
//   - ThreadID  — the agent's carried wish pointer (fields.thread_id), copied
//     onto each new bead so thread membership propagates transitively without
//     a second store round-trip.
type autoParentTarget struct {
	DepTarget string
	ThreadID  string
}

// autoParentFromAgentBead reads the agent bead ONCE and derives both the
// auto-parent dep target and the agent's carried thread_id (wish pointer) for
// the server-side auto-parent step. Folding both reads into a single GetBead
// (the ka-0000007300 "no double fetch" requirement) keeps the create path's
// extra cost to one query regardless of how many pointers the step consults.
//
// DepTarget resolution (first non-empty wins):
//  1. task_id — set on agents spawned pre-assigned to a task bead (controller
//     `gb agent start --task`, gardener fan-out). The canonical "I am working
//     task X" pointer.
//  2. slack_thread_bead_id — set on thread-spawned agents, which carry NO
//     task_id because they are opened from a Slack thread, not pre-assigned a
//     task. The thread (wish) head is the per-thread epic that derivative work
//     hangs off of (ka-0000006984: thread-spawned `kd create` calls used to
//     orphan because only task_id was consulted; e.g. ka-0000000816).
//  3. thread_id — the origin-agnostic wish pointer (Threads initiative epic
//     ka-0000001399, task 1a-1). A gardener/manual/viewer-minted thread agent
//     may carry thread_id without the Slack-specific slack_thread_bead_id
//     alias, so it is the third ladder rung for the dep target too.
//
// ThreadID (the wish pointer to copy onto new beads) is the agent's
// fields.thread_id, falling back to its legacy alias slack_thread_bead_id
// (both hold the same wish-head ka-id per
// gardenio/controller/internal/bridge/thread_bead.go). A pre-assigned crew
// agent that carries only task_id (not in any wish) yields ThreadID == "" and
// no copy happens.
//
// Returns the zero value when the subject is missing/"static", the agent bead
// cannot be loaded, or it has no Fields. Returned values are trimmed; callers
// MUST still skip self-references (bead.ID == target) and treat missing-FK
// errors as non-fatal.

// resolveAgentBeadBySubject loads the acting agent's bead from the static-token
// subject (the X-Actor / KD_ACTOR value). The subject is the agent's NAME (the
// controller sets KD_ACTOR = spec.AgentName in the pod env, podmanager
// manager.go), NOT its bead id: agent bead ids are random nanoids minted by
// CreateBead (SpawnAgent passes only Title=agentName, no id), so name != id for
// every real spawn.
//
// The lookup therefore tries, in order:
//
//  1. GetBead(subject) — the by-id path. Handles the rare case where the
//     subject already IS a gd- bead id (e.g. a caller that stamped X-Actor with
//     its own KD_AGENT_ID), and the legacy fixtures/tests that seed an agent
//     bead whose id equals its name.
//  2. FindAgentByName(subject) — the by-name path (fields.agent == subject),
//     which is the ACTUAL shape of every controller-spawned agent. Without this
//     rung the server-side auto-parent step silently no-ops for every agent —
//     every `kd create` an agent runs orphans instead of parenting to its
//     task_id / thread head (bug ka-0000005744: the /plans clarifying-question
//     decisions never appeared because the decision bead never got the
//     parent-child edge the cockpit queries on).
//
// Best-effort: any error / miss on both rungs returns nil so the caller degrades
// to "no auto-parent" rather than failing the create.
func (s *BeadsServer) resolveAgentBeadBySubject(ctx context.Context, subject string) *model.Bead {
	if s == nil || s.store == nil || subject == "" || subject == "static" {
		return nil
	}
	if bead, err := s.store.GetBead(ctx, subject); err == nil && bead != nil {
		return bead
	}
	// By-name fallback: the subject is an agent name, not a bead id. Match on
	// fields.agent regardless of status (a just-spawned agent may still be
	// open/in_progress; a parked one closed) — the caller only reads fields.
	if bead, err := s.store.FindAgentByName(ctx, subject, nil); err == nil && bead != nil {
		return bead
	}
	return nil
}

func (s *BeadsServer) autoParentFromAgentBead(ctx context.Context, subject string) autoParentTarget {
	if s == nil || s.store == nil || subject == "" || subject == "static" {
		return autoParentTarget{}
	}
	bead := s.resolveAgentBeadBySubject(ctx, subject)
	if bead == nil || len(bead.Fields) == 0 {
		return autoParentTarget{}
	}
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		return autoParentTarget{}
	}
	field := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := fields[key].(string); ok {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed
				}
			}
		}
		return ""
	}
	return autoParentTarget{
		// Prefer an explicit claimed task; fall back to the wish head so
		// thread-spawned agents (which carry no task_id) still auto-parent
		// their derivative work to the per-thread epic instead of orphaning.
		DepTarget: field("task_id", "slack_thread_bead_id", "thread_id"),
		// thread_id is the canonical wish pointer; slack_thread_bead_id is its
		// legacy alias holding the same value.
		ThreadID: field("thread_id", "slack_thread_bead_id"),
	}
}

// sanitizeOperatedByHeader validates an X-Operated-By header value supplied by
// a static-primary-token caller (per R3 trust model: the static token already
// has full write access, so allowing it to self-attribute the human-operator
// stays inside the §C3 trust boundary). Returns the cleaned value and true when
// acceptable; "" and false otherwise.
//
// Rules:
//   - non-empty after trimming, length ≤ 128, and no control characters
//     (CR/LF/TAB/0x7f) so a malicious header cannot smuggle log-line breaks
//     or terminal escapes into downstream logs.
//   - shape must be a human operator id: "slack:U<alnum>" or
//     "oidc:<local>@<domain>" (ka-0000002601 / ka-0000006809 Invariant §1).
//     Agent identities (bare agent names, "thread-…", "sched-…",
//     "gardenio-…") and sentinels ("system:…", "fleet:…") are rejected at
//     the header boundary so they cannot land as the recorded operator.
func sanitizeOperatedByHeader(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 128 {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	if !isHumanOperator(s) {
		return "", false
	}
	// Canonicalize before returning so a case-variant oidc email supplied via
	// X-Operated-By collapses onto the single operator row rather than
	// cold-path-forking a duplicate (ka-0000002687).
	return canonicalizeHumanOperator(s), true
}

// isHumanOperator reports whether v is a well-formed human-operator id —
// either "slack:U<alnum>" (Slack user id) or "oidc:<local>@<domain>" (an
// email subject). Sentinels (system:*, fleet:*), bare agent names, and
// anything else return false. Per the ka-0000006809 Invariant: "the
// operator is ALWAYS a human with an email address" — these two shapes
// are the only resolver outputs that can map to an emailed human (Slack
// profile email, OIDC subject).
func isHumanOperator(v string) bool {
	if v == "" {
		return false
	}
	if _, ok := extractSlackUserID(v); ok {
		return true
	}
	const oidcPrefix = "oidc:"
	if strings.HasPrefix(v, oidcPrefix) {
		rest := v[len(oidcPrefix):]
		// require an @ with a non-empty local + dotted domain
		at := strings.IndexByte(rest, '@')
		if at <= 0 || at == len(rest)-1 {
			return false
		}
		domain := rest[at+1:]
		if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
			return false
		}
		return true
	}
	return false
}

// canonicalizeHumanOperator collapses case-variant human-operator ids to a
// single stable form so one human resolves to one operator (ka-0000002687).
//
// OIDC subjects are email addresses, which are case-insensitive in the domain
// (RFC 5321 §2.4) and treated so in the local part by every identity provider
// we integrate. The write-path resolver (resolveOperator → LookupOperatorAlias)
// matches operator_aliases.subject EXACTLY, while the read-side owner filter
// already lower-cases both sides (queries.go operated_by_alias_in uses
// LOWER(kind||':'||subject)). That asymmetry is a live fragmentation vector: a
// write carrying "oidc:Matthew.Baker@acme.example" misses the lower-cased alias
// row, cold-path-inserts a DUPLICATE operator, and re-splits the very identity
// operator_aliases had unified — the slack:/oidc: fragmentation this task
// exists to end, reintroduced one casing at a time. Canonicalizing here (in the
// sanitize/resolve region the ROOT CAUSE names) makes the write path agree with
// the read filter so both forms land on one operator row.
//
// slack ids (already an uppercase ^U[A-Z0-9]+$ shape), sentinels, and every
// non-oidc value pass through unchanged. Idempotent: canonicalizing an
// already-canonical value is a no-op, so calling it at both the header boundary
// and the resolver wrapper is safe.
func canonicalizeHumanOperator(value string) string {
	const oidcPrefix = "oidc:"
	if !strings.HasPrefix(value, oidcPrefix) {
		return value
	}
	return oidcPrefix + strings.ToLower(value[len(oidcPrefix):])
}

// resolveOperatedBy returns the human-operator string for audit fields
// (operated_by column on beads, comments, deps, events, mutation_history).
// The value is server-resolved — the client cannot supply it directly except
// via the X-Operated-By header on a static-primary-token call (R3 §4).
//
// The resolver walks a three-step ladder:
//
//  1. OIDC-authenticated calls return "oidc:<email>" — the JWT subject is the
//     trusted operator identity.
//  2. Static-primary-token calls with a sanitized X-Operated-By header return
//     the header value verbatim. Agents send "slack:U…" or "oidc:…@…" so the
//     resolver does not re-prefix.
//  3. Static-primary-token calls without a header fall back to the calling
//     agent's bead — look up the bead by Subject (== agent name == bead ID),
//     extract the slack_user_id field, and return "slack:<id>". Lets long-lived
//     agent pods inherit the human who originally spawned them.
//
// Fleet, anonymous, and unrecognized identities return "". Live-state write
// paths (beads/comments/deps) reject in that case via resolveAuditOperatorRequired
// — see R2 server-enforcement task ka-0000005772.
//
// It also returns the source label R9 §4.4 enumerates ("oidc_subject",
// "static_header", "agent_bead_fallback", "empty"), and fires the
// rollout-observability metrics: M2 (write classification) on every call, M3
// (agent_lookup_fail) when the static-token fallback's GetBead errors,
// and M4 (fallback resolved/empty) when the fallback fires.
//
// Every audit/mutation write path goes through this resolver exactly once
// — the metrics correspond 1:1 to writes that touched operated_by. Pure
// readers of the operator string (no write happening) should not call
// this; today that does not occur because the only callers are write
// paths. If a read-only caller is added later, route it through a new
// helper that does NOT increment M2.
func (s *BeadsServer) resolveOperatedBy(ctx context.Context) (string, string) {
	value, source := s.resolveOperatedByPure(ctx)
	// Canonicalize every resolver exit — not just the header path — so the
	// OIDC-authenticated case (case "oidc" returns "oidc:"+id.Subject verbatim
	// from the JWT) and the edge/fleet header paths all agree on one form for
	// one human (ka-0000002687). Idempotent for already-canonical values.
	value = canonicalizeHumanOperator(value)
	recordOperatedByWrite(value)
	recordOperatedByWriteBySource(value, source)
	return value, source
}

// splitOperator takes a resolver-emitted "<kind>:<subject>" string and
// returns its parts. Empty input → empty parts (caller decides what to do).
// Inputs without a colon return ("", value) so the caller can detect them
// and skip the FK lookup; the resolver only ever emits prefixed values, so
// reaching this branch indicates a misconfigured fallback that should not
// silently land in operators(kind=”, subject=…).
func splitOperator(value string) (kind, subject string) {
	if value == "" {
		return "", ""
	}
	k, s, ok := strings.Cut(value, ":")
	if !ok {
		return "", value
	}
	return k, s
}

// operatorLegacyPreAttribution is the canonical alias for the
// system-owned sentinel that backfill (P3) bound to every empty-string
// operated_by row on live-state tables (beads, comments, deps). After
// the P4 cutover (migration 017) those columns are NOT NULL, so any
// new write that the resolver cannot attribute to a real operator must
// also fall back to this sentinel — see resolveAuditOperatorRequired.
const operatorLegacyPreAttribution = "system:legacy-pre-attribution"

// operatorMachine is the single, narrowly-scoped machine-origin sink for
// TRUE machine-origin writes that have no human behind them — audit-log /
// retention bookkeeping, escalation replay, reconciler housekeeping, cron
// sweeps, CI-pipeline-status with no event user. It is the ONE allowlisted
// system operator the "real operator for every bead" directive (epic
// ka-0000004835) retains after collapsing the old system:<source> /
// fleet:<name> sentinel family.
//
// Critically, it is NOT a resolver fallback: a missing human operator on a
// human-or-agent write is a loud refusal, never a silent bind to this sink.
// system:machine is reachable ONLY via an explicit, code-reviewed
// WithMachineOperator call at an allowlisted infra call site (audit_log.go,
// http_escalations.go, audit_retention.go) — attributing a machine write to
// a named human would manufacture false accountability and pollute operator
// metrics, so the design (bundle ka-0000006634 §3) rejects that.
const operatorMachine = "system:machine"

// legacySentinelCutoverDate is the declared R6 cut-over date (ka-0000006798,
// epic ka-0000006809 surface 6): the first UTC day on which no new write may
// bind operated_by = system:legacy-pre-attribution. It is a compile-time
// constant so the cut-over is auditable in the repo history rather than a
// mutable runtime config; the env override below only lets an operator force
// the gate ON EARLY (or hold it OFF for a rollback) without a redeploy.
//
// The date was chosen to follow the ka-0000001880 legacy-Matt cleanup track
// (closed) which migrated every historical sentinel-bound row, so on/after
// this date a resolver that still settles on the sentinel indicates an
// unmigrated write path — a bug to fix at the source, not a row to persist.
// Documented in the addendum bundle on the parent epic (ka-0000006809).
const legacySentinelCutoverDate = "2026-09-01"

// legacySentinelCutoverActive reports whether the R6 cut-over is in force —
// i.e. whether a resolver that settles on system:legacy-pre-attribution must
// refuse the write (true) rather than bind the sentinel (false).
//
// Resolution order:
//  1. BEADS_LEGACY_SENTINEL_CUTOVER, when set to a truthy/falsy value, wins
//     outright. This is the operator's manual override — force the gate ON
//     early (e.g. after confirming a clean fleet in staging) or hold it OFF
//     to roll back a bad flip without waiting on a redeploy.
//  2. Otherwise the gate activates on/after legacySentinelCutoverDate,
//     compared against the current UTC day. `now` is injected for tests; the
//     production call site passes time.Now().
func legacySentinelCutoverActive(now time.Time) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_LEGACY_SENTINEL_CUTOVER"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	cut, err := time.Parse("2006-01-02", legacySentinelCutoverDate)
	if err != nil {
		// Unparseable constant — should never happen (compile-time literal),
		// but fail OPEN (no cut-over) so a typo cannot brick every write path.
		return false
	}
	return !now.UTC().Before(cut)
}

// resolveAuditOperator returns the resolver triple every audit-bearing
// write path needs: the legacy TEXT value, the operators(id) FK pointer
// (nil when no operator can be attributed), and the R9 §4.4 source label.
//
// The TEXT column remains authoritative for reads during the dual-write
// window (ADR §4.4 / §6 P2). The FK is best-effort: a lookup or upsert
// failure is logged but does not fail the parent write — the row still
// commits with operated_by = "<kind>:<subject>" and operated_by_id = NULL.
//
// Sentinel operators (system:*, fleet:*) are pre-seeded in migration 015,
// so the hot-path lookup hits on them after first-cold-path. Empty resolver
// output (flag off, anonymous, fleet token without sentinel match) skips
// the FK work entirely.
//
// Use this resolver for history-only tables (mutation_history, events)
// where operated_by_id stays NULLABLE post-cutover (§4.5 P1). Live-state
// writes (beads, comments, deps) MUST go through
// resolveAuditOperatorRequired so the NOT NULL constraint added by
// migration 017 is always satisfied.
func (s *BeadsServer) resolveAuditOperator(ctx context.Context) (string, *int64, string) {
	value, source := s.resolveOperatedBy(ctx)
	if value == "" {
		return "", nil, source
	}
	id, ok := s.resolveOperator(ctx, value)
	if !ok {
		return value, nil, source
	}
	return value, &id, source
}

// resolveAuditOperatorRequired is the live-state-table variant of
// resolveAuditOperator: it returns a non-zero operators(id) FK so the
// NOT NULL constraint that migration 017 added on
// beads/comments/deps.operated_by_id is satisfied, or ok=false so the
// caller MUST reject the write.
//
// Fail-closed semantics (R2 server-enforcement, ka-0000005772): when
// the resolver returns an empty TEXT value (anonymous request, fleet
// token without sentinel match, OIDC subject missing), this returns
// ok=false. Callers map ok=false to InvalidArgument / HTTP 400.
//
// Sentinel fallback to system:legacy-pre-attribution is reserved for
// the case where the resolver produced a non-empty TEXT value but the
// FK lookup/upsert failed (DB hiccup, operator table out of sync). The
// TEXT column then disagrees with the FK only when the sentinel itself
// is unresolvable (DB outage, sentinel row missing on a freshly
// bootstrapped store); in that case we still return ok=false so the
// parent write is surfaced as an error rather than silently committing
// NULL into a NOT NULL column.
func (s *BeadsServer) resolveAuditOperatorRequired(ctx context.Context) (string, int64, string, bool) {
	// Install the refusal trace before any resolver step runs so
	// applyOperatedByHeader (already on ctx from the auth interceptor) and
	// operatedByFromAgentBead can record their breadcrumbs against a shared
	// pointer the failure path can read back. Idempotent — when the auth
	// interceptor already installed a trace via applyOperatedByHeader, the
	// existing trace is preserved with its headerSent breadcrumb intact.
	ctx = withOperatorRefusalTrace(ctx)
	value, idPtr, source := s.resolveAuditOperator(ctx)
	if idPtr != nil {
		// R6 cut-over (ka-0000006798): a resolver that settled on the
		// legacy sentinel — via an explicit X-Operated-By header or the
		// agent-bead sawLegacy rescue — must be refused post-cut-over so no
		// NEW live-state row binds the historical sink. Every pre-cut-over
		// row was migrated by the ka-0000001880 cleanup track; a caller still
		// routing here after the date is an unmigrated write path to fix at
		// the source (the LegacySentinelWriteAfterCutover alert pages on it).
		if refused, src := s.gateLegacySentinel(ctx, value); refused {
			return "", 0, src, false
		}
		return value, *idPtr, source, true
	}
	if value == "" {
		// Resolver could not attribute the request to any operator —
		// fail-closed instead of sentinel-binding to system:legacy-pre-attribution.
		// Replace the resolver source label (always "empty" here) with the
		// refusal classification (header_missing / header_non_human /
		// fallback_agent_only / chain_root_emailless) so transport layers
		// can surface it in the HTTP 400 body and the kd CLI can map it to
		// the EX_USAGE-family sentinel exit code 64 (ka-0000006731).
		return "", 0, classifyOperatorRefusal(ctx), false
	}
	// Last-resort DB path: value is a non-empty sentinel whose FK could not be
	// resolved on the first pass. Apply the same R6 cut-over gate before
	// binding it below.
	if refused, src := s.gateLegacySentinel(ctx, value); refused {
		return "", 0, src, false
	}
	id, ok := s.resolveOperator(ctx, operatorLegacyPreAttribution)
	if !ok {
		return value, 0, source, false
	}
	return operatorLegacyPreAttribution, id, source, true
}

// gateLegacySentinel applies the R6 cut-over gate (ka-0000006798) to a resolved
// operator value. It fires the gardenio_beads_operated_by_legacy_sentinel_total metric
// on EVERY sentinel-bound write so the alert can page, and reports refused=true
// (with the legacy_sentinel_cutover refusal source) when the cut-over is active
// so the caller fails the write closed with a 4xx. For any non-sentinel value it
// is a no-op. now is time.Now() — extracted so the gate is time-deterministic in
// tests via legacySentinelCutoverActive.
func (s *BeadsServer) gateLegacySentinel(ctx context.Context, value string) (refused bool, source string) {
	if value != operatorLegacyPreAttribution {
		return false, ""
	}
	active := legacySentinelCutoverActive(time.Now())
	recordLegacySentinelWrite(active)
	if active {
		// Record the cut-over breadcrumb so callers that reclassify the
		// refusal from ctx (CreateBead → newOperatorRefusal → classifyOperatorRefusal)
		// surface legacy_sentinel_cutover rather than a header_* cause.
		if t := operatorRefusalTraceFromContext(ctx); t != nil {
			t.legacySentinelCutover = true
		}
		return true, refusalSourceLegacySentinelCutover
	}
	return false, ""
}

// resolveOperator maps a resolver-emitted "<kind>:<subject>" string to the
// matching operators(id) FK. The hot path is a single
// SELECT operator_id FROM operator_aliases — typically a 1-row PK probe.
// On miss the cold path inserts a new operator + canonical alias and
// returns the new id. Returns ok=false on any error or on inputs the
// resolver should never emit (no colon, empty kind/subject) so the caller
// can leave operated_by_id = NULL and let migration 016 catch up later.
func (s *BeadsServer) resolveOperator(ctx context.Context, value string) (int64, bool) {
	if s == nil || s.store == nil {
		return 0, false
	}
	kind, subject := splitOperator(value)
	// Case-fold the subject like the alias write path does, so a
	// "gitlab:Alice" fallback from the bridge lands on the "alice" alias a
	// user bead linked instead of cold-path-forking a duplicate operator.
	subject = canonicalAliasSubject(kind, subject)
	if kind == "" || subject == "" {
		// Resolver emitted a value without "<kind>:<subject>" shape — should
		// not happen, but degrade gracefully so the parent write still lands.
		recordResolverError("unexpected")
		return 0, false
	}
	if id, err := s.store.LookupOperatorAlias(ctx, kind, subject); err == nil {
		return id, true
	}
	// Cold path: alias miss. Upsert the operator (returns existing id when
	// only the alias was missing) and stamp the canonical alias so the next
	// lookup hits the hot path.
	id, err := s.store.UpsertOperator(ctx, kind, subject)
	if err != nil {
		recordResolverError("unexpected")
		slog.Warn("operator upsert failed",
			"kind", kind,
			"subject_length", len(subject),
			"err", err.Error(),
		)
		return 0, false
	}
	if err := s.store.UpsertOperatorAlias(ctx, id, kind, subject, "resolver"); err != nil {
		recordResolverError("unexpected")
		slog.Warn("operator alias upsert failed",
			"kind", kind,
			"subject_length", len(subject),
			"err", err.Error(),
		)
		// Operator row landed; alias didn't. Caller still gets the id so this
		// write is attributed; the next write for the same operator will
		// retry the alias upsert via the cold path.
		return id, true
	}
	return id, true
}

// resolveOperatedByPure does the resolver walk without firing the
// per-write metric (M2). It still fires M3/M4 because those are tied to
// the resolution attempt itself, not to whether a row was written.
func (s *BeadsServer) resolveOperatedByPure(ctx context.Context) (string, string) {
	id := AuthIdentityFromContext(ctx)
	switch id.Type {
	case "oidc":
		if id.Subject != "" {
			return "oidc:" + id.Subject, operatedBySourceOIDC
		}
	case "fleet":
		// Fleet tokens are service identities. Bridges (slack-bridge,
		// jira-bridge, gitlab-bridge) re-attribute external-platform users
		// to a kbeads operator via X-Operated-By; honor it.
		// sanitizeOperatedByHeader restricts the header to human shapes
		// (slack:U… / oidc:…@…), so a token with write access self-attributes
		// the human operator (ka-0000001777).
		if h := operatedByHeaderFromContext(ctx); h != "" {
			return h, operatedBySourceHeader
		}
		// Per-agent tokens (P7 Task 1): the token's subject is the agent's own
		// name, so an agent write with no X-Operated-By resolves its human
		// by walking the agent bead.
		// Without this a per-agent-token pod could not create beads once the
		// authoring human gate is on.
		// The walk starts at the agent BEAD id carried by the credential
		// (agent-<bead id>), not the name: names are not unique across projects.
		if beadID, ok := agentBeadIDFromContext(ctx); ok {
			v := s.operatedByFromAgentBead(ctx, beadID)
			recordFallback(v != "")
			if v != "" {
				return v, operatedBySourceFallback
			}
		}
		// Spawn-chain backstop (ka-0000001404): a bridge/service write that
		// omitted X-Operated-By but references a spawn source (fields.spawned_by,
		// stamped into ctx by createBead) is chain-walked back to the ultimate
		// invoking human before the fleet sentinel/refusal. This is the
		// daemon-side pair to the client stamping (ka-0000007919): decision
		// respawns now set spawned_by = the prior agent bead id, so the edge
		// the walker needs exists. operatedByFromAgentBead returns ONLY a
		// human-shaped operator here (its user/slack/operated_by extractors
		// reject sentinels and non-human shapes), so a chain that roots in a
		// machine identity yields "" and falls through to the sentinel/refusal
		// exactly as before — the backstop can only ADD a human, never bind a
		// machine or downgrade the fail-closed boundary (ka-0000005772). The
		// static-token case runs the identical walk off id.Subject; the fleet
		// token's own Subject is a service name, so we seed the walk from the
		// write's referenced spawn source instead.
		if seed := spawnChainSubjectFromContext(ctx); seed != "" {
			if v := s.operatedByFromAgentBead(ctx, seed); v != "" && isHumanOperator(v) {
				return v, operatedBySourceFallback
			}
		}
		// Fleet-sentinel TERMINAL branch (ka-0000006012): under the
		// "real operator for every bead" directive the resolver no longer
		// invents a "fleet:<subject>" attribution for a header-less service
		// write. A bridge/service that omits X-Operated-By must either route
		// a true machine-origin write through WithSystemOperator /
		// WithMachineOperator (collapsed to the system:machine sink by the
		// system-override tail below) or be refused so the missing human
		// surfaces loudly instead of silently binding the fleet sentinel.
		// Behind BEADS_AUTHORING_HUMAN_GATE so the tightening can stage
		// warn→enforce; with the gate OFF the legacy sentinel still applies.
		if !authoringHumanGateEnabled() && id.Subject != "" {
			// Emit the per-source fleet-fallback signal at the exact write-time
			// moment the resolver settles on the fleet source override, so an
			// operator sees "a write just fell back to fleet:<source>" live
			// instead of hand-querying beads (task ka-0000002480).
			recordFleetFallback(id.Subject)
			return "fleet:" + id.Subject, operatedBySourceFleet
		}
	case "edge":
		// gardenio-edge-auth (cli.gardenio PAT shim / AVA-JWT proxy) verified
		// the caller's GitLab identity and stamped "oidc:<email>" via
		// EdgeAuthMiddleware's applyOperatedByHeader. Honor it exactly like
		// the fleet/static-with-header cases: trust derives from the HMAC
		// envelope (edgeauth.Verify), not the raw client header. Without this
		// case the verified operator falls through to empty and every write
		// over the shim refuses with resolver_source=unknown (ka-0000004118).
		if h := operatedByHeaderFromContext(ctx); h != "" {
			return h, operatedBySourceHeader
		}
	case runnerIdentityType:
		// A write for a runner's owner (local-agents spec §4.2, D6). The
		// header here is stamped server-side from the runner bead's owner
		// (runnerOwnerWriteContext in runners.go); no client X-Operated-By is
		// ever applied to a runner identity.
		if h := operatedByHeaderFromContext(ctx); h != "" {
			return h, operatedBySourceHeader
		}
	case landAgentIdentityType:
		// A member's own-machine agent bead (lands_agents.go): the header is
		// stamped server-side from the member's own sign-in
		// (landAgentWriteContext); no client header reaches this identity.
		if h := operatedByHeaderFromContext(ctx); h != "" {
			return h, operatedBySourceHeader
		}
	case "machine":
		// A registered machine client's service account (gd-gQ4iLIpWCm). The
		// subject is server-derived from the verified JWT ("machine:<clientId>")
		// and NO header is consulted: X-Operated-By / X-System-Operator cannot
		// re-attribute a machine write. The subject is not a human-shaped
		// operator, so under BEADS_AUTHORING_HUMAN_GATE=enforce an autonomous
		// create is refused by createBead exactly like a header-less fleet create.
		if id.Subject != "" {
			// Trigger lineage (decision gd-dI7PfsFlvr "lineage"): a principal
			// holding the trigger_lineage capability (the trigger-engine) whose
			// CREATE references a trigger/schedule bead through its own
			// fields.trigger_id is attributed to THAT bead's human author,
			// dereferenced and verified server-side. This is how a fired
			// agent's bead (and the task/molecule filed for it) keeps the human
			// who authored the trigger, exactly as it did when the engine
			// relayed trig.OperatedBy through X-Operated-By on the static
			// token — minus the header trust. Updates and comments carry no
			// seed and stay attributed to the machine.
			if machineHasCapability(ctx, capTriggerLineage) {
				if seed := triggerLineageSubjectFromContext(ctx); seed != "" {
					if v := s.operatedByFromTriggerBead(ctx, seed, id.Subject); v != "" {
						return v, operatedBySourceTriggerLineage
					}
				}
			}
			return id.Subject, operatedBySourceMachine
		}
	case "land":
		// A land principal (land_principal.go). Like a machine, the subject
		// ("land:<land>") is server-derived from the credential and no header
		// can re-attribute its write; it is not a human, so the authoring
		// human gate refuses its autonomous create.
		if id.Subject != "" {
			return id.Subject, operatedBySourceMachine
		}
	case "none", "anonymous":
		// Auth-disabled mode (BEADS_AUTH_TOKEN unset and no OIDC verifier
		// wired — dev/e2e harnesses and pre-R2 tests) and the
		// no-identity-in-context default. Honor X-Operated-By when
		// provided so callers can stamp attribution explicitly. These
		// branches are unreachable in production where AuthMiddleware
		// always resolves a real identity.
		if h := operatedByHeaderFromContext(ctx); h != "" {
			return h, operatedBySourceHeader
		}
	}
	// Last-resort: a server-internal recording path may have stamped a
	// system-operator override via WithSystemOperator / WithMachineOperator.
	// This fires only when identity-based resolution returned empty —
	// OIDC/static-with-header callers always win over the override.
	//
	// With the authoring human gate enabled every system:<source> override
	// COLLAPSES into the single allowlisted machine sink (system:machine).
	// This is the daemon half of the "real operator for every bead" directive
	// (epic ka-0000004835, ka-0000006425): the old system:<source> /
	// fleet:<name> sentinel family is folded into ONE auditable machine
	// terminal, so a TRUE machine-origin write that carries an explicit,
	// code-reviewed WithSystemOperator (statusreporter pod-phase, reconciler
	// housekeeping, EnsureConfigs) resolves cleanly instead of hard-refusing.
	// Collapsing rather than refusing is what lets the sibling client change
	// (ka-0000006293, MR !4813) drop the client-wide SetSystemOperator defaults
	// without its genuine machine writes hard-400ing the moment the gate flips
	// (gasbox RC3 fail-closed amplification, ka-0000005858).
	//
	// Note this is NOT a resolver fallback for a write that should have carried
	// a human: the override only exists when an infra call site explicitly set
	// it, and OIDC / X-Operated-By / spawn-chain-human all win above. A
	// header-less, override-less write still falls through to empty and is
	// refused. With the gate OFF every system:<source> override is honored
	// verbatim as before, preserving the zero-behavior-change deploy.
	if sys := systemOperatorFromContext(ctx); sys != "" {
		if !authoringHumanGateEnabled() {
			return sys, operatedBySourceHeader
		}
		// Gate ON: collapse any system:<source> (and system:machine itself)
		// into the single machine sink.
		return operatorMachine, operatedBySourceHeader
	}
	return "", operatedBySourceEmpty
}

// maxSpawnChainHops bounds the X-Spawned-By chain walk performed by
// operatedByFromAgentBead. Each agent's Fields.spawned_by points at its
// parent agent's NAME (the controller stamps spawned_by = spec.AgentName,
// not a bead id — see gardenio rollupGardenerSpend "spawned_by → agent-name
// edge"), forming a chain back to the spawner that the resolver follows
// when the current agent has no usable attribution of its own. The chain
// is bounded at 5 hops so a malformed or cyclic spawn graph cannot turn
// one write into an unbounded lookup loop. ka-0000006147 spec.
const maxSpawnChainHops = 5

// resolveAgentBeadForChainWalk loads an agent bead for the operator-resolver
// chain walk (operatedByFromAgentBead), keyed by either the static-token
// subject (the FIRST hop) or a fields.spawned_by pointer (EVERY subsequent
// hop). Both are agent NAMES, not the random nanoid bead ids CreateBead mints:
// the controller sets KD_ACTOR = spec.AgentName in the pod env (podmanager)
// and stamps spawned_by = the spawner's name, while SpawnAgent passes only
// Title=agentName to CreateBead, so name != id for every real spawn.
//
// The lookup therefore mirrors resolveAgentBeadBySubject (the auto-parent
// path fixed for the sibling gap ka-0000005744): try GetBead (by-id) first —
// for the rare caller that stamped a gd- bead id and the legacy fixtures whose
// id equals the name — then FindAgentByName (fields.agent == key), the ACTUAL
// shape of every controller-spawned agent. Without the by-name rung GetBead
// 404s for every real agent, so the whole agent_bead_fallback operator source
// silently no-ops (bug ka-0000003065) — masked in prod only because the
// X-Operated-By header short-circuits the resolver before this step ever runs.
//
// Returns:
//   - (bead, false) when either rung resolves a bead;
//   - (nil, false) on a clean miss (both rungs not-found) — the caller stops
//     the walk WITHOUT bumping a metric (not-found is not a lookup error);
//   - (nil, true) when a rung returns a genuine store error (anything other
//     than sql.ErrNoRows) — the caller records agent_lookup_fail and stops.
func (s *BeadsServer) resolveAgentBeadForChainWalk(ctx context.Context, key string) (*model.Bead, bool) {
	bead, err := s.store.GetBead(ctx, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, true
	}
	if bead != nil {
		return bead, false
	}
	// By-name fallback: key is an agent name, not a bead id.
	bead, err = s.store.FindAgentByName(ctx, key, nil)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, true
	}
	if bead != nil {
		return bead, false
	}
	return nil, false
}

// operatedByFromAgentBead implements the agent-bead fallback step of the
// resolver: look up the calling agent's bead and resolve a human operator
// from it. The Subject for static-token calls is the agent's NAME (KD_ACTOR =
// spec.AgentName), NOT its bead id — agent beads key by a random nanoid with
// the name in fields.agent, so both the first hop and every spawned_by hop are
// resolved id-or-name via resolveAgentBeadForChainWalk (bug ka-0000003065).
// Returns "" when the bead is missing, the fields are absent, all
// attribution fields are empty/malformed, AND walking the X-Spawned-By
// chain (≤ maxSpawnChainHops) does not yield a human operator either.
//
// At each agent bead in the chain the resolver tries, in order:
//
//  1. fields.operated_by — already-prefixed opaque id stamped by the
//     spawner (gb spawn, scheduler, bridge handlers). Accepted ONLY when
//     it is a well-formed human shape (slack:U… / oidc:…@…) per the
//     ka-0000006809 Invariant §1; agent-shaped values are rejected so
//     they cannot round-trip as the recorded operator (ka-0000002601).
//  2. fields.user_bead_id — pointer to a type=user bead. The user bead's
//     slack_id (preferred) or email is lifted into a "slack:U…" / "oidc:…"
//     candidate and accepted iff it passes isHumanOperator.
//  3. fields.slack_user_id — legacy push-at-spawn shortcut. Prefixed with
//     "slack:" since the field stores the raw Slack user id.
//
// If none of (1)–(3) resolve on the current agent, the walk follows
// fields.spawned_by to the parent agent and retries. The walk is
// cycle-safe (visited-set) and bounded; on overflow it emits the
// resolver-error metric label spawn_chain_overflow and returns "".
// Per-hop GetBead errors emit agent_lookup_fail and stop the walk.
func (s *BeadsServer) operatedByFromAgentBead(ctx context.Context, subject string) string {
	if s == nil || s.store == nil || subject == "" || subject == "static" {
		return ""
	}
	// Mark fallback as attempted on the refusal trace so classifyOperatorRefusal
	// can distinguish "no header AND no agent bead" (header_missing) from
	// "no header but agent bead had a non-human value" (fallback_agent_only).
	if t := operatorRefusalTraceFromContext(ctx); t != nil {
		t.fallbackAttempted = true
	}
	visited := make(map[string]struct{}, maxSpawnChainHops)
	cur := subject
	// sawLegacy tracks whether any walked agent bead carried the
	// system:legacy-pre-attribution sentinel in fields.operated_by. When the
	// chain walk completes without finding a human, we return the sentinel
	// as a last-resort so resolveAuditOperatorRequired FK-binds the seeded
	// operators row (ka-0000005841) instead of failing closed.
	sawLegacy := false
	overflowed := true
	// subjectFields captures the originating agent's own fields (the first hop)
	// so the parent-task fallback can be consulted ONCE after the walk, against
	// the agent being resolved, rather than per-hop on the write hot-path.
	var subjectFields map[string]any
	for range maxSpawnChainHops {
		if _, seen := visited[cur]; seen {
			// Cycle in the spawn graph — stop without overflowing the
			// chain-overflow bucket (that's reserved for "chain too long
			// to resolve", not "chain malformed").
			overflowed = false
			break
		}
		visited[cur] = struct{}{}

		// Resolve by id-or-name: cur is an agent NAME (X-Actor on the first hop,
		// fields.spawned_by on every later hop), and real agent beads key by
		// nanoid with the name in fields.agent (bug ka-0000003065). GetBead alone
		// 404s for every controller-spawned agent, so the by-name fallback is
		// what makes the agent_bead_fallback source fire at all off the header.
		bead, lookupErr := s.resolveAgentBeadForChainWalk(ctx, cur)
		if lookupErr {
			recordResolverError("agent_lookup_fail")
			overflowed = false
			break
		}
		if bead == nil || len(bead.Fields) == 0 {
			overflowed = false
			break
		}
		var fields map[string]any
		if err := json.Unmarshal(bead.Fields, &fields); err != nil {
			overflowed = false
			break
		}

		if ob, ok := fields["operated_by"].(string); ok && strings.TrimSpace(ob) == operatorLegacyPreAttribution {
			sawLegacy = true
		}

		if v := operatorFromAgentFields(ctx, fields); v != "" {
			return v
		}
		if v := s.operatorFromUserBead(ctx, fields); v != "" {
			return v
		}
		if v := operatorFromSlackUserID(ctx, fields); v != "" {
			return v
		}
		// Capture the SUBJECT agent's own fields (first hop) for the
		// parent-task fallback, consulted once after the walk (see below).
		if subjectFields == nil {
			subjectFields = fields
		}

		next, _ := fields["spawned_by"].(string)
		next = strings.TrimSpace(next)
		if next == "" || next == cur {
			overflowed = false
			break
		}
		cur = next
	}

	// Parent task/trigger fallback (ka-0000002541 T5): a system-spawned crew may
	// carry NO human attribution of its own and root in a machine identity, so
	// the spawn-chain walk above yields nothing — yet it was pre-assigned a
	// parent TASK (task_id) or seeded by a TRIGGER/SCHEDULE (schedule_id) bead,
	// and those carry a real human author (the authoring-human gate,
	// ka-0000006012, guarantees a human author for trigger/schedule beads). Lift
	// a HUMAN-SHAPED operator from that parent bead. This ONLY adds a human
	// source — it never binds a sentinel/machine identity (fail-closed boundary
	// ka-0000005772): the parent's operated_by / user_bead_id are run through
	// the same human-shape checks as an agent bead, and a non-human value is
	// silently skipped, leaving the chain to refuse loudly. Gated by the staged
	// authoring-human gate so the new source ships dark first.
	//
	// Consulted ONCE here against the originating (subject) agent's parent
	// linkage — not per-hop — so a deep, unattributed chain does not multiply
	// store reads on the write hot-path. The parent linkage is a property of
	// the agent being resolved, not of every ancestor in its spawn chain.
	if v := s.operatorFromParentTaskBead(ctx, subjectFields); v != "" {
		return v
	}
	if overflowed {
		// Walked maxSpawnChainHops without resolving — the spawn chain root
		// is either unreachable or doesn't carry attribution.
		recordResolverError("spawn_chain_overflow")
	}
	if sawLegacy && !authoringHumanGateEnabled() {
		// Last-resort rescue (ka-0000005841): bind to the seeded sentinel
		// operator so the write commits attributed to the legacy actor.
		//
		// Suppressed under the authoring human gate (ka-0000006012): the
		// "real operator for every bead" directive collapses the resolver
		// ladder to OIDC → X-Operated-By → spawn-chain HUMAN → REFUSE, with
		// no sentinel terminal branch. A spawn chain whose only attribution
		// is the legacy sentinel now refuses so the spawn gap (the agent was
		// spawned with no walkable human) surfaces loudly and gets fixed at
		// the spawner, rather than silently re-binding the historical sink.
		return operatorLegacyPreAttribution
	}
	return ""
}

// operatorFromAgentFields returns a human-shaped operator id from an
// agent bead's fields.operated_by, or "" if absent / non-human-shaped.
// When operated_by is non-empty but rejected as non-human, the refusal
// trace's fallbackFoundNonHuman breadcrumb is set so classifyOperatorRefusal
// can label the resulting refusal as fallback_agent_only.
//
// Sentinel-shaped values (system:*, fleet:*) are treated as silently
// absent (ka-0000005841): they are an expected marker on spawn-path beads
// (legacy-pre-attribution stamp, scheduler fallbacks, bridge defaults) and
// should not pollute the refusal trace — the chain walk must continue to
// look for a real human ancestor without being short-circuited into a
// fallback_agent_only classification. The legacy-pre-attribution sentinel
// is then re-introduced by operatedByFromAgentBead as a last-resort
// attribution so the write still commits.
func operatorFromAgentFields(ctx context.Context, fields map[string]any) string {
	v, ok := fields["operated_by"].(string)
	if !ok {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if isSentinelOperator(v) {
		return ""
	}
	if !isHumanOperator(v) {
		if v != operatorLegacyPreAttribution {
			if t := operatorRefusalTraceFromContext(ctx); t != nil {
				t.fallbackFoundNonHuman = true
			}
		}
		return ""
	}
	return v
}

// operatorFromSlackUserID lifts an agent bead's fields.slack_user_id
// (raw Slack U… id) into a "slack:U…" operator candidate. Returns ""
// when absent, empty, or the resulting shape isn't a human operator.
// A non-empty but non-human candidate sets the refusal trace's
// fallbackFoundNonHuman breadcrumb (ka-0000006731).
func operatorFromSlackUserID(ctx context.Context, fields map[string]any) string {
	v, ok := fields["slack_user_id"].(string)
	if !ok {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	candidate := "slack:" + v
	if !isHumanOperator(candidate) {
		if t := operatorRefusalTraceFromContext(ctx); t != nil {
			t.fallbackFoundNonHuman = true
		}
		return ""
	}
	return candidate
}

// operatorFromUserBead follows fields.user_bead_id to a type=user bead
// (kbeads config.go type:user schema) and lifts its slack_id (preferred)
// or email into a human-shaped operator id. Returns "" when the pointer
// is missing, the target bead is missing / wrong type, or neither field
// produces a candidate that passes isHumanOperator.
//
// slack_id is preferred because the resolver downstream maps to
// operators(kind='slack', subject=U…) which is the dominant alias kind
// in the operators table; the email fallback only fires when no Slack
// id is recorded on the user bead.
func (s *BeadsServer) operatorFromUserBead(ctx context.Context, fields map[string]any) string {
	id, _ := fields["user_bead_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	bead, err := s.store.GetBead(ctx, id)
	if err != nil || bead == nil {
		return ""
	}
	if bead.Type != "user" || len(bead.Fields) == 0 {
		return ""
	}
	var uf map[string]any
	if err := json.Unmarshal(bead.Fields, &uf); err != nil {
		return ""
	}
	if sv, ok := uf["slack_id"].(string); ok {
		sv = strings.TrimSpace(sv)
		if sv != "" {
			candidate := "slack:" + sv
			if isHumanOperator(candidate) {
				return candidate
			}
		}
	}
	if ev, ok := uf["email"].(string); ok {
		ev = strings.TrimSpace(ev)
		if ev != "" {
			candidate := "oidc:" + ev
			if isHumanOperator(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// parentTaskFieldKeys are the agent-bead fields that point at the bead this
// agent was spawned to serve: a pre-assigned task (task_id), or a defining
// trigger/schedule bead (schedule_id). These parent beads are authored by a
// real human (the authoring-human gate guarantees it for trigger/schedule
// beads, and a task_id task is created by whoever filed the work), so they are
// a valid LAST human-attribution source for a system-spawned agent whose own
// spawn chain roots in a machine identity. Order matters: a pre-assigned task
// is the more specific signal than the schedule that fanned out the agent.
var parentTaskFieldKeys = []string{"task_id", "schedule_id"}

// operatorFromParentTaskBead lifts a HUMAN-SHAPED operator id from the agent
// bead's parent task / trigger / schedule bead (ka-0000002541 T5). It is the
// final per-hop attribution source consulted by operatedByFromAgentBead, after
// the agent's own operated_by / user_bead_id / slack_user_id fields and before
// the walk follows spawned_by to the parent agent.
//
// Boundary (fail-closed, ka-0000005772): this ONLY adds a human-shaped source.
// The parent bead's operated_by, user_bead_id, and slack_user_id are run
// through the exact same human-shape checks used for an agent bead
// (operatorFromAgentFields / operatorFromUserBead / operatorFromSlackUserID),
// each of which skips sentinels (system:* / fleet:*) and rejects non-human
// shapes. A parent whose only attribution is a sentinel or a machine identity
// therefore yields "" here, leaving the chain to refuse loudly rather than
// silently binding a machine identity as the operator.
//
// Gated by the staged authoring-human gate so the new source ships dark first
// (warn→enforce). With the gate OFF this is a no-op, preserving exactly the
// pre-change resolver behavior.
func (s *BeadsServer) operatorFromParentTaskBead(ctx context.Context, fields map[string]any) string {
	if s == nil || s.store == nil {
		return ""
	}
	if !authoringHumanGateEnabled() {
		// Ship dark: the parent-task source is only consulted under the gate so
		// it can be staged warn→enforce alongside the createBead tightening.
		return ""
	}
	for _, key := range parentTaskFieldKeys {
		id, _ := fields[key].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		bead, err := s.store.GetBead(ctx, id)
		if err != nil || bead == nil || len(bead.Fields) == 0 {
			continue
		}
		var pf map[string]any
		if err := json.Unmarshal(bead.Fields, &pf); err != nil {
			continue
		}
		// Reuse the agent-field human-shape extractors verbatim — they enforce
		// the human-only / sentinel-skip boundary identically for a parent bead.
		if v := operatorFromAgentFields(ctx, pf); v != "" {
			return v
		}
		if v := s.operatorFromUserBead(ctx, pf); v != "" {
			return v
		}
		if v := operatorFromSlackUserID(ctx, pf); v != "" {
			return v
		}
	}
	return ""
}

// isSentinelOperator reports whether s is a sentinel-class operator id
// (system:* or fleet:*). Sentinels are recorded for audit but never take
// dibs and never propagate down spawn chains (R2 chain-stop, R3 §3).
func isSentinelOperator(s string) bool {
	return strings.HasPrefix(s, "system:") || strings.HasPrefix(s, "fleet:")
}

// isAutonomousBeadType reports whether a bead type defines future,
// unattended work that the scheduler / trigger engine will later spawn
// agents from. These types (and the gardener loop-specs carried on a
// type=trigger / type=schedule bead) seed downstream spawns whose
// operated_by inherits the defining bead's author, so the authoring-time
// human-operator gate (createBead, ka-0000006012) requires a real human
// author for them. "gardener" is not a distinct bead type — gardeners are a
// type=trigger / type=schedule bead carrying a `gardener` field — so it is
// covered transitively by the two type checks here.
func isAutonomousBeadType(t model.BeadType) bool {
	switch t {
	case "trigger", "schedule":
		return true
	default:
		return false
	}
}

// applyOperatedByHeader sanitizes the raw X-Operated-By value supplied by
// a static-token caller and stamps it into the context when acceptable.
// On rejection it logs operated_by_rejected=true (R9 §4.4) with the
// rejected length only and increments the M3 sanitize_reject counter.
//
// Legacy-pre-attribution carve-out (ka-0000005841): the sentinel still
// flows from spawners (scheduler, jira-bridge) into agent pods via
// BEADS_OPERATED_BY. It is not a human shape so the sanitizer would
// reject it, blocking every write the agent attempts (FSM-353 wrapup
// ka-0000007255). Route it through the system-operator pipeline instead
// so resolveAuditOperatorRequired binds the seeded sentinel operators
// row from migration 016.
func applyOperatedByHeader(ctx context.Context, raw, authType, callsite string) context.Context {
	if raw == "" {
		return ctx
	}
	ctx = withOperatorRefusalTrace(ctx)
	if t := operatorRefusalTraceFromContext(ctx); t != nil {
		t.headerSent = true
	}
	if raw == operatorLegacyPreAttribution {
		return WithSystemOperator(ctx, "legacy-pre-attribution")
	}
	op, ok := sanitizeOperatedByHeader(raw)
	if !ok {
		recordResolverError("sanitize_reject")
		if t := operatorRefusalTraceFromContext(ctx); t != nil {
			t.headerRejected = true
		}
		slog.Warn("operated_by header rejected by sanitizer",
			"operated_by_rejected", true,
			"rejected_length", len(raw),
			"auth_type", authType,
			"callsite", callsite,
		)
		return ctx
	}
	return WithOperatedByHeader(ctx, op)
}

// headerPresent reports whether any of the provided header values is a
// non-empty string. The auth interceptors pass the raw values from the
// inbound request so M1 records header_present=true even when sanitization
// later rejects the value (the operator still wants to see that the CLI
// is sending something — the kind of "something" is M3's job).
func headerPresent(values []string) bool {
	for _, v := range values {
		if v != "" {
			return true
		}
	}
	return false
}

// verifiedClientID returns the token's authorized-party (azp) claim when the
// verifier can extract it, else "". Called only after the token verified, so an
// extraction error is a claims-parse failure; it yields "" and the token keeps
// its normal authority (this is not an auth step). The "" is NOT trusted for
// escalation approval: escalationApprover refuses an oidc identity with no azp
// (fail closed), so a failed extraction can never approve.
func verifiedClientID(ctx context.Context, v TokenVerifier, rawToken string) string {
	if cv, ok := v.(ClientExtractor); ok {
		if id, err := cv.ExtractClientID(ctx, rawToken); err == nil {
			return id
		}
	}
	return ""
}

// oidcAuthIdentity builds the AuthIdentity for a verified OIDC human token:
// its email subject, the client it was issued to and, when the user signed in
// through GitHub, the GitHub account.
func oidcAuthIdentity(ctx context.Context, v TokenVerifier, rawToken, email string) AuthIdentity {
	id := AuthIdentity{Type: "oidc", Subject: email, ClientID: verifiedClientID(ctx, v, rawToken)}
	if gv, ok := v.(GitHubExtractor); ok {
		if ghID, login, err := gv.ExtractGitHub(ctx, rawToken); err == nil {
			id.GitHubID, id.GitHubLogin = ghID, login
		}
	}
	if iv, ok := v.(ImpersonationExtractor); ok {
		if imp, sid, err := iv.ExtractImpersonation(ctx, rawToken); err == nil && imp != "" {
			id.ImpersonatedBy, id.SessionID = imp, sid
		}
	}
	return id
}

// verifyAndIdentify attempts to verify the token and extract the user's email.
// If the verifier implements IdentityVerifier, it uses VerifyAndIdentify to get
// the email. Returns the email (possibly empty) on success, or empty string on
// verification failure.
func verifyAndIdentify(ctx context.Context, v TokenVerifier, rawToken string) string {
	if iv, ok := v.(IdentityVerifier); ok {
		email, err := iv.VerifyAndIdentify(ctx, rawToken)
		if err != nil {
			return ""
		}
		return email
	}
	return ""
}
