package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// defaultAdvisoryText is the built-in advisory text template used when no
// authz-denied-text config bead has been set. Placeholders: {action}, {type},
// {reason}, {escalate_hint}.
const defaultAdvisoryText = `Write denied: {action} on type "{type}" was rejected. {reason}

{escalate_hint}`

// escalateHint is appended when the denial is escalatable.
const escalateHint = `To request elevated access, retry your command with the --escalate flag:
  kd {action} ... --escalate
This will create an escalation bead for operator approval.`

// AuthzDenial represents a write authorization failure with structured context
// for the caller. Includes advisory text guiding agents on how to proceed.
type AuthzDenial struct {
	Action        string `json:"denied_action"`
	BeadType      string `json:"bead_type"`
	RequiredRole  string `json:"required_role,omitempty"`
	RequiredScope string `json:"required_project,omitempty"`
	Escalatable   bool   `json:"escalatable"`
	Message       string `json:"error"`
	AdvisoryText  string `json:"advisory_text,omitempty"`

	// DeniedReason is the wire-visible counterpart of the internal-only
	// deriveAuthzDenialReason enum (authz_denials_metrics.go), populated only
	// for a taint write-gate denial today (ADR docs/adr/taints.md §9.4 pins
	// `denied_reason: taint` on the response body). Left empty on every other
	// denial kind — deriveAuthzDenialReason remains the source of truth for
	// those, computed at metrics-emit time rather than duplicated here.
	DeniedReason string `json:"denied_reason,omitempty"`

	// Ownership context, populated on an ownership denial (ka-0000005214).
	// These name WHO the resource belongs to and who the caller was resolved
	// to, so a client (or an agent reading the 403 body) can tell a genuine
	// permission problem from "you are testing against the wrong resource" —
	// the mistake that produced the retracted bug report ka-0000000798.
	//
	// CallerSubject is always the SERVER-DERIVED authenticated subject
	// (AuthIdentityFromContext(ctx).Subject). It is never taken from
	// caller-supplied input: X-Operated-By / BEADS_OPERATED_BY land in a
	// separate context slot and are not consulted for authorization.
	CallerSubject      string `json:"caller_subject,omitempty"`
	ResourceCreatedBy  string `json:"resource_created_by,omitempty"`
	ResourceAssignee   string `json:"resource_assignee,omitempty"`
	ResourceOwner      string `json:"resource_owner,omitempty"`
	ResourceOperatedBy string `json:"resource_operated_by,omitempty"`
	// AllowingGrants lists, on an ownership denial, up to five grant-table rows
	// that would admit this write for another caller: a machine role, a role
	// bead's row, or a row narrowed to a few types (ownershipAllowingGrants).
	AllowingGrants []AllowingGrant `json:"allowing_grants,omitempty"`

	// SpawnRole is set when this denial is the privileged-role spawn gate
	// (spawn_authz.go) rejecting a non-human create of type:agent.
	// handleEscalation copies it onto the resulting escalation bead as
	// `spawn_role` so the inbox can render it as a spawn request rather than
	// a generic write (design doc gd-sGW5Y1B8sg §3).
	SpawnRole string `json:"spawn_role,omitempty"`

	// TaintNames and TaintOwnerTeams are populated on a taint write-gate
	// denial (authorizeWriteTaintGate, ADR docs/adr/taints.md §9.4):
	// taintsOf(bead) ⊆ grantsOf(caller) failed for an existing bead. TaintNames
	// lists every taint the bead carries that the caller lacks a grant for;
	// TaintOwnerTeams is the deduplicated set of those taints' owner teams —
	// who to ask for a grant. Populated together; both empty on every other
	// denial kind (deriveAuthzDenialReason keys off TaintNames).
	TaintNames      []string `json:"taint_names,omitempty"`
	TaintOwnerTeams []string `json:"taint_owner_teams,omitempty"`

	// hidden marks a refusal of a write to a bead the caller cannot read
	// (beadHiddenFromCaller). updateBead sets it; the PATCH and UpdateBead
	// edges then answer not-found instead of this denial.
	hidden bool
}

func (d *AuthzDenial) Error() string { return d.Message }

// machineWriteContextKey marks ctx as a TRUE machine-origin write for
// authorizeWriteTitled's bypass below: the server's own code performing a write on
// no human's behalf, not a caller-supplied header.
//
// This is deliberately a SEPARATE marker from WithSystemOperator /
// WithMachineOperator (audit.go), which govern only audit attribution (the
// operated_by FK) and must never be reused here. WithSystemOperator(ctx,
// "machine") collides byte-for-byte with the operatorMachine sentinel
// WithMachineOperator writes to the same context key, and until the shared
// static token was retired (gd-ke6PC7bRto) any static-token holder (every
// agent pod) could set it through the X-System-Operator header. That header
// is now honoured for no one, but checking the attribution key here would
// reopen gd-ZjiSd9Kzio's bug the moment any header path set it again.
// withMachineWrite has no header path at all: only a code-reviewed,
// allowlisted call site can set it.
type machineWriteContextKey struct{}

// withMachineWrite marks ctx as a true machine-origin write. Call this ONLY
// at a call site enumerated in authorizeWriteTitled's doc comment (gd-ZjiSd9Kzio) —
// adding a new call site is a security-relevant change, not a convenience.
func withMachineWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, machineWriteContextKey{}, true)
}

// isMachineWrite reports whether ctx was marked by withMachineWrite.
func isMachineWrite(ctx context.Context) bool {
	v, _ := ctx.Value(machineWriteContextKey{}).(bool)
	return v
}

// machineWriteAllowKey identifies one (action, type) pair in the machine
// write allow-list below.
type machineWriteAllowKey struct {
	Action   string
	BeadType model.BeadType
}

// machineAllowedWrites is the enumerated, reviewed allow-list of (action,
// type) pairs a non-OIDC caller (the shared primary static token, or a
// scoped bridge/agent fleet token) may perform against an admin-gated type
// (min_role: gardenio-admin) without holding gardenio-admin (gd-ZjiSd9Kzio
// review). This is narrower than isMachineWrite/withMachineWrite above:
// those bypass write policy entirely for the server's OWN code; this list
// still runs checkTokenScopes (a scoped fleet token restricted away from
// e.g. type:config is still denied) and only widens the min_role check for
// the caller-supplied credentials enumerated here.
//
// Each entry is a real, deployed, non-OIDC call site inventoried against
// this repo at review time — adding an entry is a security-relevant change
// and needs the same evidence: a concrete file:line call site, not "might
// need it later".
//
//   - report/create      — gb decision report (controller/cmd/gb/decision.go),
//     an agent pod submitting a decision artifact on the primary static token.
//   - advice/create      — gb advice add, kd advice add, gb config load
//     (restore), beads-viewer's create_advice.go — all agent/service
//     static or fleet tokens.
//   - advice/close       — gb advice remove, kd advice remove, and the
//     controller's own advice_sweeper reconciler (expiry/supersession
//     sweep), all non-OIDC.
//   - role/create        — gb role scaffold, gb config load (role section).
//   - config/create      — gb config load / gb config migrate, gb role
//     scaffold's instructions bead, and EnsureConfigs
//     (controller/internal/bridge/init_ensure.go) run by every bridge
//     (slack/gitlab/jira/grafana) at startup on its own
//     bridge fleet token.
//   - config/update      — the same EnsureConfigs call sites, for the
//     update-existing-config-bead path.
//   - account/create     — gb account create.
//   - project/update     — gb pool enable/disable (controller/cmd/gb/pool.go,
//     updatePoolConfig) and the slack-bridge's /schedule and /trigger
//     on|off commands (bot_schedule_cmd.go, bot_trigger_cmd.go), both on
//     non-OIDC fleet tokens toggling fields on the project bead.
//   - project/comment    — the same /schedule and /trigger on|off commands
//     above post an audit comment on the project bead right after the
//     field update.
//   - report/update      — gb decision report's AddDependency calls
//     (controller/cmd/gb/decision.go) wiring a freshly created report bead
//     under its decision/originating-task parent.
//   - report/close       — gb decision report closes the report bead it
//     just created (controller/cmd/gb/decision.go) once it is wired up.
//   - user/create        — gb identity add / gb user add / gb identity seed.
//     NOT for identity fields: authorizeUserIdentityFields (user_identity_authz.go)
//     refuses any static/fleet-token write that sets email, email_aliases,
//     slack_id, gitlab_username, ... or the access grants, so seeding those
//     needs an OIDC gardenio-admin or an approved escalation (gd-uAvleAZUvV).
//   - user/update        — beads-viewer's own self-service field writes on
//     the caller's user bead (handleUpdateUserStatusMessage in
//     cmd/beads-viewer/user_status_message.go, the homepage-section-
//     visibility write in cmd/beads-viewer/settings.go, and the inbox
//     last-visit stamp in cmd/beads-viewer/user_inbox.go). beads-viewer
//     itself authenticates every daemon call with one process-wide static
//     BEADS_DAEMON_TOKEN (cmd/beads-viewer/main.go) — there is no per-user
//     OIDC credential to forward — and does its own self/peer check at the
//     HTTP layer (isSelf) before issuing the write, same trust boundary as
//     project/update above. Missing this entry regressed the round-4
//     gd-ZjiSd9Kzio re-inventory: a user editing their OWN status message or
//     homepage prefs got 403 (caught by viewer-e2e once the round-4 seed fix
//     let that job's tests actually run to completion).
//   - jira-poller/create — jira-bridge's seedConfigBead
//     (controller/internal/bridge/jira_config.go), on the jira-bridge fleet
//     token.
//
// Machine principals do not consult this list. A registered machine principal
// (authMethodMachine, machine_identity.go) gets only the gardenio-machine-*
// grants in machine_grants.go, so adding a pair here never widens a machine
// client, and removing one never narrows it. The list is the static-token and
// fleet-token path only.
//
// Retirement status (gd-whES7ZdkAy, checked at main 899839b4): an entry may be
// deleted only when every remaining caller holds a machine grant for the pair.
// None does yet. The machine grants are typed to each component's own
// background writes (decision gd-ZsfxvXRiyv "tight"), so the three entries the
// inventory (docs/machine-principal-inventory.md §6.1) listed as candidates
// stay for these callers:
//   - update/config      — the controller's auth_config_legacy.go:227 (the
//     controller's machine principal seeds its own 47 titles through
//     gardenio-machine-controller-config-seed but is NOT granted `auth`, so on
//     that principal this write is refused; the static path keeps it), coopmux
//     failover.rs:710, and the gitlab/jira/wl bridges (no
//     machine client). The config-seed roles cover only
//     grafana-bridge's type:dashboard, slack-bridge's eight seed titles and the
//     controller's configs() titles.
//   - comment/project    — slack-bridge /schedule and /trigger and beads-viewer
//     project_config.go:320, both human-driven and not on a machine role.
//   - create/jira-poller — jira-bridge, which has no machine client.
//
// The other eleven entries stay for gb/kd agent-pod callers on the shared
// static token until P7 per-agent tokens. TestMachineAllowedWrites_ExactSet
// pins the set. Rationale: docs/machine-principal-design.md §12.
//
// project/create was removed (gd-WCM9rfOL8L), not retired: a project bead
// with no land_kind is an operator project the controller mints fleet-App
// GitHub tokens for, so the shared static token every agent pod holds must not
// open one. gb project create and gb config load's project section now need an
// OIDC gardenio-admin; the Lands provisioner keeps its machine grant, for
// tenant lands only (authorizeProjectCreate, land_switch_authz.go).
//
// Deliberately NOT allow-listed: rig, convoy, credential (no non-OIDC call
// site found anywhere in the repo — stay admin-only until one exists with
// evidence), team (this bead's own acceptance-criteria surface — must stay
// OIDC-admin-only), project/create (see above), fleet, audit
// (recordAuditEntry already uses withMachineWrite, a stronger and narrower
// bypass), and every
// (action,type) combination not listed above — including update/close/
// delete on the types above where only a narrower action is listed. Those
// still deny non-OIDC callers exactly as
// TestAuthorizeWrite_StaticToken_AdminGatedTypeDenied asserts.
var machineAllowedWrites = map[machineWriteAllowKey]bool{
	{Action: "create", BeadType: "report"}:      true,
	{Action: "update", BeadType: "report"}:      true,
	{Action: "close", BeadType: "report"}:       true,
	{Action: "create", BeadType: "advice"}:      true,
	{Action: "close", BeadType: "advice"}:       true,
	{Action: "create", BeadType: "role"}:        true,
	{Action: "create", BeadType: "config"}:      true,
	{Action: "update", BeadType: "config"}:      true,
	{Action: "create", BeadType: "account"}:     true,
	{Action: "update", BeadType: "project"}:     true,
	{Action: "comment", BeadType: "project"}:    true,
	{Action: "create", BeadType: "user"}:        true,
	{Action: "update", BeadType: "user"}:        true,
	{Action: "create", BeadType: "jira-poller"}: true,
}

// isMachineAllowedWrite reports whether (action, beadType) is in the
// enumerated machine allow-list above.
func isMachineAllowedWrite(action string, beadType model.BeadType) bool {
	return machineAllowedWrites[machineWriteAllowKey{Action: action, BeadType: beadType}]
}

// writeAuthzDenial writes a structured 403 response for a write policy denial.
func writeAuthzDenial(w http.ResponseWriter, d *AuthzDenial) {
	writeJSON(w, http.StatusForbidden, d)
}

// authorizeWriteTitled checks the WritePolicy on a TypeConfig before allowing a
// write operation. Static/fleet tokens with scopes are checked against
// type/op restrictions. Every non-OIDC caller — in practice the shared
// primary static token, unscoped fleet tokens, and a legacy v1 edge-auth
// caller (authMethodEdge, which bypassed write policy entirely until
// gd-i6GZX5jzHM and route roles until gd-AvSKOUyqRz) — is evaluated against min_role with no roles attached, so admin-gated types deny it
// instead of bypassing write policy outright (gd-ZjiSd9Kzio) — UNLESS the
// specific (action, type) pair is in the enumerated machineAllowedWrites
// allow-list below, covering the real non-OIDC call sites (gb/kd CLI,
// bridges, controller reconcilers) that legitimately need to keep writing
// those admin-gated types. A machine principal
// (authMethodMachine) is evaluated against its gardenio-machine-* grants
// instead (machineGrantsWrite, machineGrantsConfigWrite), never against that
// list; a machine client with no matching grant is denied like a roleless OIDC
// caller. project_scoped
// stays OIDC-only here: a non-OIDC identity has no resolvable operator for
// checkProjectScope (Subject is the literal token kind, e.g. "static", not
// an operator identity), and tightening that is a distinct, much larger
// change than the admin-everywhere bug this closes.
//
// A ctx marked withMachineWrite bypasses write policy entirely: this is the
// server's own code writing on no human's behalf, not a caller-supplied credential of any kind. The two known,
// allowlisted call sites are audit_log.go's auditRecorderContext, used by
// recordAuditEntry (type=audit is min_role admin; the server must always be
// able to record its own authz decisions), and http_escalations.go's
// replayEscalation (replaying a write an operator has already approved
// through the escalation flow — the approval IS the authorization, checked by
// handleApproveEscalation before the replay ever runs). Two owner-gated
// routes write admin-gated records on their caller's behalf after their own
// gate: land_steward_delegation.go (stewardDelegationDenial) and
// window_routes.go's handleCreateWindow (windowManageDenial; type=window).
// lands_agents.go writes a member's own-machine agent bead after its own
// gate (landAgentMayStart, landAgentMayRevoke; landAgentWriteContext).
// Do not add another call site without updating this comment.
// title is the bead's title: a machine principal's config writes are scoped
// by title (machineGrantsConfigWrite), so the create/update/close/dep paths
// pass it; a caller that omits it gets no config grant, i.e. fails closed.
// Returns nil if authorized, or an *AuthzDenial on failure.
func (s *BeadsServer) authorizeWriteTitled(ctx context.Context, action string, beadType model.BeadType, labels []string, title string) *AuthzDenial {
	return s.authorizeWriteFrozen(ctx, action, beadType, labels, title, false)
}

// confinedWriteDenial applies what holds before any write_policy (a type with
// none, like task, would pass): a scoped principal's token projects and types,
// a runner's project allowlist, and land_frozen (the Lands kill switch), which
// only a post in the frozen land's own #support (supportPost) gets past.
func (s *BeadsServer) confinedWriteDenial(ctx context.Context, action string, beadType model.BeadType, labels []string, supportPost bool) *AuthzDenial {
	if denial := scopedWriteDenial(ctx, action, beadType, labels); denial != nil {
		s.populateAdvisoryText(ctx, denial, denial.Message+".")
		return denial
	}
	if denial := runnerProjectDenial(ctx, action, labels, nil); denial != nil {
		return denial
	}
	if supportPost {
		return nil
	}
	return frozenLandDenial(ctx, s.store, action, beadType, labels)
}

// nonOIDCWriteAllowed: a machine principal writes an admin-gated type by its
// grants (gd-gQ4iLIpWCm), any other non-OIDC caller but a runner by
// machineAllowedWrites (see the call site).
func nonOIDCWriteAllowed(ctx context.Context, method authMethod, action string, beadType model.BeadType, title string) bool {
	if method == authMethodMachine {
		return machineGrantsWrite(ctx, action, beadType) ||
			beadType == "config" && machineGrantsConfigWrite(ctx, action, title)
	}
	return !roleBearing(method) && method != authMethodRunner && isMachineAllowedWrite(action, beadType)
}

// authorizeWriteFrozen is authorizeWriteTitled that skips the freeze when
// supportPost is set: createBead sets it for a post in a frozen land's own
// #support (frozenSupportPost), the one write a freeze lets through.
func (s *BeadsServer) authorizeWriteFrozen(ctx context.Context, action string, beadType model.BeadType, labels []string, title string, supportPost bool) *AuthzDenial {
	if isMachineWrite(ctx) {
		return nil
	}

	if denial := s.confinedWriteDenial(ctx, action, beadType, labels, supportPost); denial != nil {
		return denial
	}

	method := authMethodFromContext(ctx)

	// authMethodNone means the daemon has no auth configured at all (dev/test
	// mode) — every route was always open before gd-ZjiSd9Kzio and stays open
	// here. This is not a real caller-presented
	// credential to evaluate against min_role.
	if method == authMethodNone {
		return nil
	}

	// A verified v1 edge envelope (authMethodEdge) is evaluated like the
	// static token from here on (gd-i6GZX5jzHM): it carries no roles, so an
	// admin-gated type denies it unless the pair is in machineAllowedWrites.
	// Its signer holds the shared HMAC key, a credential no stronger than the
	// primary static token, so it no longer skips write_policy outright.
	if !roleBearing(method) {
		if denial := checkTokenScopes(ctx, action, beadType); denial != nil {
			return denial
		}
		// A lands-tenant agent writes only the types landsTenantWrites
		// lists, even in its own project (Lands Track B §5.1,
		// restricted_ops.go), and an agent carrying a restrict taint writes
		// no restrictTaintFleetTypes bead.
		if denial := s.typeWriteDenial(ctx, action, beadType); denial != nil {
			return denial
		}
	}

	// type=release is admin-gated, but the release captain (an agent on its
	// own token) and a machine principal granted the type write it too: the
	// release writer check (releases.go) is the one place that says who.
	if beadType == model.TypeRelease && s.releaseWriterDenial(ctx, action) == nil {
		return nil
	}

	// type=gatehouse_request is admin-gated, but the gatehouse's steward
	// files one in its own land while the gatehouse's tools are on
	// (gatehouse_request.go, restricted_ops.go opGatehouseRequest).
	if beadType == typeGatehouseRequest && action == "create" && !roleBearing(method) && gatehouseRequestCarveOut(ctx, s.store, labels) {
		return nil
	}

	tc, err := s.resolveTypeConfig(ctx, beadType)
	if err != nil || tc == nil || tc.WritePolicy == nil {
		return nil
	}

	wp := tc.WritePolicy
	roles := RolesFromContext(ctx)

	// Admin role bypasses all write policies.
	if hasMinimumRole(roles, RoleAdmin) {
		return nil
	}

	// machineAllowedWrites (gd-ZjiSd9Kzio review) is the explicit set of
	// (action, type) pairs deployed non-OIDC callers need now that admin-gated
	// types deny them by default; checkTokenScopes already ran, so a scoped
	// fleet token restricted away from this type stays denied. A registered
	// machine principal uses its capability grants instead (design §4.1). A
	// runner (runner_auth.go) gets neither: it never exceeds its owner's OIDC
	// identity, which these admin-type pairs deny.
	if nonOIDCWriteAllowed(ctx, method, action, beadType, title) {
		return nil
	}

	// The home steward updating or closing its project's advice under an
	// approved advice escalation it claimed (escalation_steward_advice.go).
	if s.stewardAdviceWriteHeld(ctx, action, beadType, labels) {
		return nil
	}

	// Check min_role. Non-OIDC callers carry no roles (RolesFromContext is
	// OIDC-only), so this denies admin-gated types for them exactly as it
	// would for an OIDC caller with no roles.
	if wp.MinRole != "" && !hasMinimumRole(roles, wp.MinRole) {
		d := &AuthzDenial{
			Action:       action,
			BeadType:     string(beadType),
			RequiredRole: wp.MinRole,
			Escalatable:  wp.Escalatable,
			Message:      "insufficient permissions: " + action + " on type " + string(beadType) + " requires " + wp.MinRole,
		}
		if method == authMethodMachine {
			// Name the denied machine ("machine:<clientId>") so a visible 403
			// identifies which component lacks the grant.
			d.CallerSubject = AuthIdentityFromContext(ctx).Subject
		}
		s.populateAdvisoryText(ctx, d, "Requires role "+wp.MinRole+".")
		return d
	}

	// Check project_scoped. OIDC-only (see doc comment above). A land
	// principal has no user bead to check: its one land was checked by
	// scopedWriteDenial above, where actionTable decides which project_scoped
	// types it writes (decision create alone, gd-mh9ANzCHIE).
	if wp.ProjectScoped && method == authMethodOIDC {
		project := projectFromLabels(labels)
		if project != "" && project != "unknown" {
			if denial := s.checkProjectScope(ctx, action, beadType, project, wp.Escalatable); denial != nil {
				s.populateAdvisoryText(ctx, denial, "Not authorized for project "+project+".")
				return denial
			}
		}
	}

	return nil
}

// populateAdvisoryText resolves advisory text from the authz-denied-text config
// bead (if customized) or uses the built-in default, then templates it into the
// denial struct.
func (s *BeadsServer) populateAdvisoryText(ctx context.Context, d *AuthzDenial, reason string) {
	populateAdvisoryTextForStore(ctx, s.store, d, reason)
}

// populateAdvisoryTextForStore is the store-only variant of
// populateAdvisoryText, usable by callers that only hold a store.Store (e.g.
// spawn_authz.go's authorizeAgentSpawn, which runs ahead of any *BeadsServer
// method dispatch).
func populateAdvisoryTextForStore(ctx context.Context, st store.Store, d *AuthzDenial, reason string) {
	tmpl := resolveAdvisoryTemplateForStore(ctx, st)

	hint := ""
	if d.Escalatable {
		hint = strings.ReplaceAll(escalateHint, "{action}", d.Action)
	}

	text := tmpl
	text = strings.ReplaceAll(text, "{action}", d.Action)
	text = strings.ReplaceAll(text, "{type}", d.BeadType)
	text = strings.ReplaceAll(text, "{reason}", reason)
	text = strings.ReplaceAll(text, "{escalate_hint}", hint)
	d.AdvisoryText = strings.TrimSpace(text)
}

// resolveAdvisoryTemplateForStore returns the advisory text template from the
// authz-denied-text config bead, falling back to the built-in default.
func resolveAdvisoryTemplateForStore(ctx context.Context, st store.Store) string {
	cfg, err := st.GetConfig(ctx, "authz-denied-text")
	if err != nil || cfg == nil {
		return defaultAdvisoryText
	}

	var parsed struct {
		Template string `json:"template"`
	}
	if json.Unmarshal(cfg.Value, &parsed) != nil || parsed.Template == "" {
		return defaultAdvisoryText
	}
	return parsed.Template
}

// authorizeWriteForBead checks write policy for an existing bead by looking
// up its type and labels. Used by update and close operations. It also runs
// the ownership gate (authorizeOwnership) so non-config-kind beads cannot be
// mutated by anyone other than their creator/assignee/owner or an admin.
func (s *BeadsServer) authorizeWriteForBead(ctx context.Context, action string, bead *model.Bead) *AuthzDenial {
	if denial := s.authorizeWriteTitled(ctx, action, bead.Type, bead.Labels, bead.Title); denial != nil {
		return denial
	}
	// The labels were checked above; a runner's fields.project is checked on
	// its own, as on update (close, comment, hook-emit; runner_auth.go).
	if denial := runnerProjectDenial(ctx, action, nil, bead.Fields); denial != nil {
		return denial
	}
	if denial := s.authorizeOwnership(ctx, action, bead); denial != nil {
		return denial
	}
	// Closing or deleting the lands-pause gate lifts the all-lands pause:
	// admin only (land_switch_authz.go).
	if denial := s.authorizeKillSwitchLifecycle(ctx, action, bead, nil, nil); denial != nil {
		return denial
	}
	if denial := s.authorizeWriteTaintGate(ctx, action, bead); denial != nil {
		return denial
	}
	// A close by the executor of an approved bead-close escalation that names
	// this bead leaves a mutation_history row naming the escalation. Recorded
	// only once every gate has passed (escalation_close_grant.go).
	s.recordEscalationCloseGrant(ctx, action, bead)
	return nil
}

// authorizeOwnership is the ownership gate for an existing non-config bead: the
// write is admitted only when a row of the grant table admits it
// (ownership_grants.go, design kbeads/docs/ownership-grants.md). The base rule
// (created_by, assignee or owner is the caller) is the row "owner"; every other
// exception is a row too, and a role's reach is rows on its role bead.
// Config-kind beads are governed by their type's write policy (B6) and are not
// gated here.
//
// The admitting row is counted in gardenio_authz_ownership_admits_total. A
// denial names the bead's owners and the caller (ka-0000005214) and the rows
// that would have admitted the write for someone else.
func (s *BeadsServer) authorizeOwnership(ctx context.Context, action string, bead *model.Bead) *AuthzDenial {
	if bead == nil || bead.Kind == model.KindConfig {
		return nil
	}
	if adm, ok := s.evaluateOwnershipGrants(ctx, action, bead); ok {
		authzOwnershipAdmitsTotal.WithLabelValues(adm.grant, authzPrincipalKind(ctx), action, authzBeadTypeLabel(string(bead.Type))).Inc()
		if adm.onAdmit != nil {
			adm.onAdmit()
		}
		return nil
	}

	// Name WHO the bead belongs to, and who the caller is (ka-0000005214), so a
	// correct refusal of a write to someone else's bead does not read as a
	// broken guard (ka-0000000798). Disclosure is bounded: write policy and
	// token scopes have already passed for this bead's project, and these are
	// the identity strings any bead read returns.
	actor := AuthIdentityFromContext(ctx).Subject
	d := &AuthzDenial{
		Action:             action,
		BeadType:           string(bead.Type),
		Escalatable:        true,
		CallerSubject:      actor,
		ResourceCreatedBy:  bead.CreatedBy,
		ResourceAssignee:   bead.Assignee,
		ResourceOwner:      bead.Owner,
		ResourceOperatedBy: bead.OperatedBy,
		AllowingGrants:     s.ownershipAllowingGrants(ctx, action, bead),
		Message: "ownership denied: " + action + " on bead " + bead.ID +
			" requires created_by, assignee, owner, or admin role — you are " +
			quoteIdentity(actor) + "; this bead is " + describeBeadOwnership(bead),
	}
	if len(d.AllowingGrants) > 0 {
		d.Message += "; would be allowed for: " + describeAllowingGrants(d.AllowingGrants)
	}
	s.populateAdvisoryText(ctx, d,
		"You are "+quoteIdentity(actor)+" but this bead is "+describeBeadOwnership(bead)+".")
	return d
}

// quoteIdentity renders an identity string for a human-facing message,
// collapsing the empty case to an explicit word rather than an empty pair of
// quotes.
func quoteIdentity(s string) string {
	if s == "" {
		return "(no identity)"
	}
	return `"` + s + `"`
}

// describeBeadOwnership renders the ownership metadata actually present on a
// bead, e.g.:
//
//	created_by="bridge:jira", operated_by="slack:U123" (assignee, owner unset)
//
// Only populated fields are shown with values; the rest are named as unset, so
// the reader can tell "this bead has no owner" from "you are not its owner" —
// a distinction the previous message erased. See ka-0000000798.
func describeBeadOwnership(bead *model.Bead) string {
	pairs := []struct {
		name  string
		value string
	}{
		{"created_by", bead.CreatedBy},
		{"assignee", bead.Assignee},
		{"owner", bead.Owner},
		{"operated_by", bead.OperatedBy},
	}

	var set, unset []string
	for _, p := range pairs {
		if p.value != "" {
			set = append(set, p.name+`="`+p.value+`"`)
		} else {
			unset = append(unset, p.name)
		}
	}

	switch {
	case len(set) == 0:
		return "owned by nobody (created_by, assignee, owner, operated_by all unset)"
	case len(unset) == 0:
		return strings.Join(set, ", ")
	default:
		return strings.Join(set, ", ") + " (" + strings.Join(unset, ", ") + " unset)"
	}
}

// bridgeSubjectMatchesLegacyCreatedBy reports whether a service bridge's auth
// subject and a bead's recorded created_by name the SAME bridge across the
// fleet-name→token-subject rename (ka-0000007614).
//
// A bridge authenticates with the per-identity token subject "bridge:<name>"
// (bridgetokenrotator.deriveSubject: jira-bridge → bridge:jira), while every
// bead it created before per-identity subjects existed carries the bare fleet
// name "<name>-bridge" as created_by. This maps the auth-subject form to the
// legacy created_by form and compares:
//
//	subject "bridge:jira"  → legacy "jira-bridge"  == created_by  ✓
//
// It returns false for any subject that is not a "bridge:<name>" form, an empty
// name, or a created_by that does not match the reconstructed "<name>-bridge"
// exactly — so it only ever admits a bridge to a bead it demonstrably created,
// never a bead created by a different bridge, an agent, or an operator.
func bridgeSubjectMatchesLegacyCreatedBy(subject, createdBy string) bool {
	name := strings.TrimPrefix(subject, "bridge:")
	if name == subject || name == "" {
		return false // not a "bridge:<name>" subject
	}
	return createdBy == name+"-bridge"
}

// callerIsBeadOperator reports whether the authenticated caller is the human
// operator on whose behalf the bead was created (bead.OperatedBy). It is the
// generalized form of the decision-only precedent (ka-0000007637): the
// principal human may mutate beads their agents filed for them.
//
// Matching is fail-closed and proceeds in two steps:
//  1. Byte-equal fast path — the caller's identity string equals
//     bead.OperatedBy verbatim (e.g. edge-auth Subject "slack:U…" equal to the
//     stored operator), needing no database round-trip.
//  2. Canonical path — resolve both the caller and the bead's recorded
//     operator to an operators(id) via the alias table and compare. This is
//     what lets a verified OIDC caller (Subject = bare email) match a
//     slack:U… operated_by once the two have been linked to one operator row.
//
// Any unresolved or mismatched side returns false, so an unlinked identity or
// a different operator is denied exactly as before.
func (s *BeadsServer) callerIsBeadOperator(ctx context.Context, id AuthIdentity, bead *model.Bead) bool {
	// Step 1: byte-equal fast path.
	if id.Subject != "" && id.Subject == bead.OperatedBy {
		return true
	}

	// Step 2: canonical operator_id comparison.
	callerKind, callerSubject := operatorAliasForIdentity(id)
	if callerKind == "" {
		return false
	}
	callerOp, err := s.store.LookupOperatorAlias(ctx, callerKind, callerSubject)
	if err != nil || callerOp == 0 {
		return false
	}

	beadOp := s.beadOperatorID(ctx, bead)
	return beadOp != 0 && callerOp == beadOp
}

// operatorAliasForIdentity maps an authenticated caller to the (kind, subject)
// pair used in the operators alias table. Only human-operator identities (OIDC
// tokens and edge-auth) resolve; static, fleet, anonymous, and empty-subject
// identities return ("", "") so the caller skips the lookup.
//
// Subject shapes vary by auth path:
//   - OIDC token        → bare email                  (Type "oidc")
//   - edge / GitLab PAT → bare email                  (Type "edge"; edge_auth
//     stamps Subject = res.Operator, and only the *attribution* copy gets the
//     "oidc:" prefix — the AuthIdentity Subject stays bare)
//   - edge / Slack      → "slack:U…" or "oidc:<email>" (Type "edge")
//
// So a scheme-prefixed subject is parsed as-is; a colon-less subject is an
// OIDC email — mirroring edge_auth's own bare→oidc default (edge_auth.go).
func operatorAliasForIdentity(id AuthIdentity) (kind, subject string) {
	// id.Type is the AuthIdentity string tag (see WithAuthIdentity call sites),
	// not the typed authMethod context value — compare against the literals.
	switch id.Type {
	case "oidc", "edge":
		if id.Subject == "" {
			return "", ""
		}
		if k, sub := splitOperator(id.Subject); k != "" {
			return k, sub // already scheme-prefixed: "slack:U…" / "oidc:<email>"
		}
		return "oidc", id.Subject // bare email (OIDC token or GitLab-PAT edge)
	default:
		return "", ""
	}
}

// beadOperatorID resolves the bead's recorded operator to an operators(id),
// preferring the already-populated OperatedByID FK and falling back to a
// lookup of the parsed "<kind>:<subject>" OperatedBy string. Returns 0 when
// the operator cannot be resolved.
func (s *BeadsServer) beadOperatorID(ctx context.Context, bead *model.Bead) int64 {
	if bead.OperatedByID != nil && *bead.OperatedByID != 0 {
		return *bead.OperatedByID
	}
	kind, subject := splitOperator(bead.OperatedBy)
	if kind == "" {
		return 0
	}
	op, err := s.store.LookupOperatorAlias(ctx, kind, subject)
	if err != nil {
		return 0
	}
	return op
}

// checkProjectScope verifies the OIDC user's authorized_projects include the
// target project. Returns nil if authorized, or an *AuthzDenial on failure.
func (s *BeadsServer) checkProjectScope(ctx context.Context, action string, beadType model.BeadType, project string, escalatable bool) *AuthzDenial {
	id := AuthIdentityFromContext(ctx)
	email := id.Subject
	if email == "" {
		return &AuthzDenial{
			Action:        action,
			BeadType:      string(beadType),
			RequiredScope: project,
			Escalatable:   escalatable,
			Message:       "project-scoped write denied: no user identity",
		}
	}

	perms, err := lookupSpawnPermissions(ctx, s.store, email)
	if err != nil {
		slog.Warn("write authorization: failed to look up user permissions",
			"email", email, "error", err)
		return &AuthzDenial{
			Action:        action,
			BeadType:      string(beadType),
			RequiredScope: project,
			Escalatable:   escalatable,
			Message:       "project-scoped write denied: failed to verify user permissions",
		}
	}
	if perms == nil {
		return &AuthzDenial{
			Action:        action,
			BeadType:      string(beadType),
			RequiredScope: project,
			Escalatable:   escalatable,
			Message:       "project-scoped write denied: not authorized",
		}
	}

	if len(perms.AllowedProjects) > 0 && !containsOrWildcard(perms.AllowedProjects, project) {
		return &AuthzDenial{
			Action:        action,
			BeadType:      string(beadType),
			RequiredScope: project,
			Escalatable:   escalatable,
			Message:       "project-scoped write denied: not authorized for project " + project,
		}
	}

	return nil
}

// projectFromFields extracts the project name from a bead's JSON fields by its
// exact key, the way the controller reads it (see exactFieldString).
func projectFromFields(fields json.RawMessage) string {
	project, _, _ := exactFieldString(fields, "project")
	return project
}

// EscalationResponse is returned as 202 when an escalatable denial is retried
// with X-Escalate: true. The caller can track the escalation bead to get notified
// when it is approved or rejected.
type EscalationResponse struct {
	EscalationID string `json:"escalation_id"`
	Message      string `json:"message"`
	DeniedAction string `json:"denied_action"`
	BeadType     string `json:"bead_type"`
}

// handleEscalation checks if the request has X-Escalate: true and the denial
// is escalatable. If so, creates an escalation bead and writes 202 Accepted.
// targetBeadID is the bead being mutated (empty for create operations).
// Returns true if the escalation was handled (caller should return), false if not.
func (s *BeadsServer) handleEscalation(w http.ResponseWriter, r *http.Request, d *AuthzDenial, requestBody json.RawMessage, targetBeadID string) bool {
	if r.Header.Get("X-Escalate") != "true" {
		return false
	}
	if !d.Escalatable {
		return false
	}
	// A lands-tenant agent may not escalate (Lands Track B §5.1): answer the
	// retry with the tenant refusal, which says where to ask instead, rather
	// than filing an escalation or a grant request.
	if td := s.restrictedOpDenial(r.Context(), restrictedOpRequest{Op: opEscalationRetry, Action: "create", BeadType: model.TypeEscalation}); td != nil {
		s.auditDenial(r, td, targetBeadID)
		writeAuthzDenial(w, td)
		return true
	}

	// A taint write-gate denial escalates to a `grant`-type escalation
	// addressed to the taint's owner team, not a replay of the originally
	// denied write (ADR docs/adr/taints.md §9.4: "This reuses the escalation
	// replay path rather than adding a separate grant-request type" — but the
	// operation_type it replays is "grant", handled by replayEscalation's
	// grant case, never the generic create/update/close cases below).
	if len(d.TaintNames) > 0 {
		return s.handleTaintGrantEscalation(w, r, d, targetBeadID)
	}

	identity := AuthIdentityFromContext(r.Context())

	fields := map[string]any{
		"operation_type":   d.Action,
		"requesting_agent": identity.Subject,
		"context":          d.Message,
		"operation_spec":   json.RawMessage(requestBody),
	}
	if targetBeadID != "" {
		fields["target_bead_id"] = targetBeadID
	}
	if d.SpawnRole != "" {
		fields["spawn_role"] = d.SpawnRole
		// requires_four_eyes is opt-in per spawn_policy rule (four_eyes on a
		// human-mode rule, validated satisfiable at write time). It is NOT
		// stamped by default: a single-human org could never satisfy it and
		// it would 403 the owner's own approval (gd-TXbrvA9E6F). Never-auto is
		// held independently by validateSpawnPolicyFields and
		// evaluateSpawnEscalation's role check; approval still needs an OIDC
		// human through an approval-capable client (escalationApprover).
		//
		// The matching human rule's approvers are copied onto the escalation
		// so the inbox shows who may approve; handleApproveEscalation
		// enforces them (and re-resolves them live, spawnApprovalDenial).
		if rule := s.matchingHumanSpawnRule(r.Context(), d.SpawnRole, requestBody, identity.Subject); rule != nil {
			if rule.FourEyes {
				fields["requires_four_eyes"] = true
			}
			if len(rule.Approvers) > 0 {
				fields["approvers"] = rule.Approvers
			}
		}
	}

	// Build the escalation bead.
	bead, err := s.createBead(withServerStampedEscalation(r.Context()), createBeadInput{
		Title:      "Escalation: " + d.Action + " on type " + d.BeadType + " denied",
		ShortTitle: escalationShortTitle(d),
		Type:       "escalation",
		Labels: []string{
			"auto:authz-escalation",
		},
		CreatedBy: identity.Subject,
		Fields:    mustMarshal(fields),
	})
	if writeOpenEscalation(w, d, err) {
		return true
	}
	if err != nil {
		slog.Warn("failed to create escalation bead", "error", err)
		writeAuthzDenial(w, d)
		return true
	}

	s.auditEscalationCreated(r, d, bead.ID, targetBeadID)

	// Give the spawn-policy evaluator (spawn_policy.go, gd-vmLhgqydEa) a look
	// at every privileged-spawn escalation the moment it lands, so a
	// declared auto rule can resolve it within this same request instead of
	// waiting for the next sweep tick. No-op (including the flag check)
	// unless d.SpawnRole is set — this bead type carries no other
	// escalatable action the evaluator understands.
	if d.SpawnRole != "" {
		s.evaluateSpawnEscalationSynchronous(r.Context(), bead)
	}

	writeJSON(w, http.StatusAccepted, EscalationResponse{
		EscalationID: bead.ID,
		Message:      "escalation created: pending operator approval",
		DeniedAction: d.Action,
		BeadType:     d.BeadType,
	})
	return true
}

// writeOpenEscalation answers a denial whose escalation create was refused as
// a repeat (create_dedupe.go) with the open escalation's id, and reports
// whether it did.
func writeOpenEscalation(w http.ResponseWriter, d *AuthzDenial, err error) bool {
	var dup *duplicateOpenError
	if !errors.As(err, &dup) {
		return false
	}
	writeJSON(w, http.StatusAccepted, EscalationResponse{
		EscalationID: dup.ExistingID,
		Message:      "escalation already open: pending operator approval",
		DeniedAction: d.Action,
		BeadType:     d.BeadType,
	})
	return true
}

// escalationShortTitle is the short_title handleEscalation files under: the
// daemon authors these escalations itself, so it must supply one or
// BEADS_SHORT_TITLE_GATE=enforce refuses the create for the agent caller and
// every `gb spawn --escalate` falls back to a bare 403 (gd-lYZUr0CqY0).
func escalationShortTitle(d *AuthzDenial) string {
	if d.SpawnRole != "" {
		return model.ClipShortTitle("spawn " + d.SpawnRole + " agent")
	}
	return model.ClipShortTitle(d.Action + " " + d.BeadType + " denied")
}

// handleTaintGrantEscalation files a `grant`-type escalation (ADR
// docs/adr/taints.md §9.4) for a taint write-gate denial retried with
// X-Escalate: true. It shares createGrantEscalation (taint_grant_request.go)
// with the direct `kd taint grant-request` endpoint (handleGrantTaintRequest)
// so both surfaces produce the same escalation shape. The requester's
// principal is resolved HERE, at request time — the identity context is not
// available later when the escalation is approved and replayed.
func (s *BeadsServer) handleTaintGrantEscalation(w http.ResponseWriter, r *http.Request, d *AuthzDenial, targetBeadID string) bool {
	ctx := r.Context()
	identity := AuthIdentityFromContext(ctx)

	kind, principal, ok := s.resolveRequesterPrincipal(ctx)
	if !ok {
		writeError(w, http.StatusForbidden, "caller identity cannot be resolved to a grantable principal; cannot file a grant request")
		return true
	}

	bead, err := s.createGrantEscalation(ctx, identity, d.TaintNames, d.TaintOwnerTeams, kind, principal, "", d.Message, targetBeadID)
	if err != nil {
		slog.Warn("failed to create taint grant escalation bead", "error", err)
		writeAuthzDenial(w, d)
		return true
	}

	s.auditEscalationCreated(r, d, bead.ID, targetBeadID)

	writeJSON(w, http.StatusAccepted, EscalationResponse{
		EscalationID: bead.ID,
		Message:      "grant request created: pending owner-team approval",
		DeniedAction: d.Action,
		BeadType:     d.BeadType,
	})
	return true
}

// mustMarshal marshals v to JSON or returns nil on error.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// checkTokenScopes verifies that a static token's type and operation scopes
// allow the requested action on the given bead type. Returns nil if the token
// has no scope restrictions or if the action is within scope.
func checkTokenScopes(ctx context.Context, action string, beadType model.BeadType) *AuthzDenial {
	typeScope := TokenTypeScopeFromContext(ctx)
	opScope := TokenOpScopeFromContext(ctx)

	// No scopes set — unrestricted static token, full bypass.
	if len(typeScope) == 0 && len(opScope) == 0 {
		return nil
	}

	if !tokenScopeAllows(typeScope, string(beadType)) {
		return &AuthzDenial{
			Action:   action,
			BeadType: string(beadType),
			Message:  "token scope denied: not authorized for bead type " + string(beadType),
		}
	}

	if !tokenScopeAllows(opScope, action) {
		return &AuthzDenial{
			Action:   action,
			BeadType: string(beadType),
			Message:  "token scope denied: operation " + action + " not permitted",
		}
	}

	return nil
}

// labelsForAuthz builds a label list for authorization from both explicit labels
// and field-embedded project. Used by createBead where the bead's project may be
// in the fields (e.g., agent beads) rather than labels.
func labelsForAuthz(labels []string, fields json.RawMessage) []string {
	if p := projectFromLabels(labels); p != "" && p != "unknown" {
		return labels
	}
	if p := projectFromFields(fields); p != "" {
		return append(labels, "project:"+p)
	}
	return labels
}
