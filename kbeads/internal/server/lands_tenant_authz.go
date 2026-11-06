package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Capability tiers on the spawn gate and escalation authz (P9 gd-aR6UzS0HgM,
// task gd-Pcyz0ftcTg). Two readers:
//
//   - roleHoldsTier answers "does this type:role bead hold tier X" for
//     roleIsPrivileged (the privileged tier) and for the checks below (the
//     lands-tenant tier).
//   - landsTenantCallerFrom resolves whether the CALLER is a lands-tenant agent,
//     so the spawn gate, the write gate and the escalation paths can apply the
//     Lands Track B spec §5.1 table: a tenant agent spawns only lands-gardener
//     in its own project and only as the steward, files only a merge
//     escalation about its own land, approves only that as the steward,
//     rejects nothing and writes no bead that configures the fleet.
//
// The rules themselves, and the tier's other §5.1 rows, are the shared
// restricted-ops check (restricted_ops.go, step 10.1).

const (
	// landsStewardRole and landsGardenerRole are the Lands roles that hold the
	// lands-tenant tier (spec §4.2, step 9.1). Only the steward spawns, and it
	// spawns only lands-gardener.
	landsStewardRole  = "steward"
	landsGardenerRole = "lands-gardener"

	// landsTenantDeniedReason is the denied_reason a tenant-tier refusal
	// carries, so an agent can tell it from a role or project denial and
	// explain it in the Commons.
	landsTenantDeniedReason = "lands_tenant"
	landsTenantSupportHint  = "ask the land's owner, or the operator in #support"
)

// roleHoldsTier reports whether a type:role bead's fields hold tier. The set is
// capability_tiers; the legacy privileged flag still counts for the privileged
// tier while it is a derived view (gd-Bcqj2r4m7E), so a bead whose two fields
// disagree reads as privileged rather than not. Both readings fail closed: a
// fields blob or capability_tiers value that does not parse holds every tier,
// which makes the role privileged (a spawn escalates) and, for a caller,
// restricted (the lands-tenant checks apply).
func roleHoldsTier(fields json.RawMessage, tier string) bool {
	var f map[string]any
	if len(fields) > 0 {
		if err := json.Unmarshal(fields, &f); err != nil {
			return true
		}
	}
	if f == nil { // no fields, or JSON null
		f = map[string]any{}
	}
	if v, has := f[model.RoleCapabilityTiersField]; has {
		tiers, ok := model.ParseCapabilityTiers(v)
		if !ok {
			return true
		}
		if model.HasCapabilityTier(tiers, tier) {
			return true
		}
	}
	return tier == model.CapabilityTierPrivileged && model.RolePrivilegedFlag(f)
}

// landsTenantCaller is a per-agent-token caller whose role holds the
// lands-tenant tier, or who is bound to a tenant land whatever its role.
type landsTenantCaller struct {
	AgentBeadID string
	Role        string
	Roles       []string
	Project     string
	// ByLand is set when no role element holds the tier and the caller is a
	// tenant because it is bound to a tenant land (tenantLandOfAgent).
	ByLand bool
	// Home is set for the home garden's steward (homeGardenOwner): it is
	// still the steward of its land (its notes), but the §5.1 restrictions
	// (restricted_ops.go) and their carve-outs do not apply to it. HomeOwner
	// is the home garden's operator ("oidc:<email>"), who stands behind the
	// gardeners it binds (spawn_lineage_home_steward.go).
	Home      bool
	HomeOwner string
}

// isSteward reports a steward whose role holds the tier. A ByLand caller is
// never one, whatever its role names, so it gets none of the steward's
// carve-outs (gd-PtbbUxKWds).
func (c *landsTenantCaller) isSteward() bool {
	return !c.ByLand && slices.Contains(c.Roles, landsStewardRole)
}

// holder names the caller in a refusal: "role X", plus its land when the land,
// not the role, makes it a tenant.
func (c *landsTenantCaller) holder() string {
	if c.ByLand {
		return "role " + c.Role + " in tenant land " + strconvQuote(c.Project)
	}
	return "role " + c.Role
}

// landsTenantCallerFrom returns the caller as a lands-tenant agent, or nil when
// it is not one. The agent is the one the per-agent token names
// (agentBeadIDFromContext), never a header; a tenant pod holds only its own
// per-agent token (spec §5.3). ANY element of the agent's role holding the
// tier makes it a tenant, closed role beads included: the restriction is the
// conservative reading. So does being bound to a tenant land, whatever the
// role: a Commons spawn in a land may run a role that holds no tier
// (gd-PtbbUxKWds). Every lookup failure is an error so each caller fails
// closed, a missing or deleted agent bead included: the controller mints a
// per-agent token only for an agent bead (a closed one still resolves), so a
// token whose bead is gone is a pod awaiting teardown, and reading it as "no
// tenant" would let a steward lift its own child's restriction by deleting
// the child's bead.
func landsTenantCallerFrom(ctx context.Context, s store.Store) (*landsTenantCaller, error) {
	beadID, ok := agentBeadIDFromContext(ctx)
	if !ok || s == nil {
		return nil, nil
	}
	agent, err := s.GetBead(ctx, beadID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && agent == nil) {
		return nil, fmt.Errorf("the caller's agent bead %s does not exist", beadID)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up the caller's agent bead %s: %w", beadID, err)
	}
	if agent.Type != model.BeadType("agent") {
		return nil, fmt.Errorf("the caller's bead %s is not an agent bead", beadID)
	}
	role, _, err := exactFieldString(agent.Fields, agentRoleField)
	if err != nil {
		return nil, fmt.Errorf("the caller's agent bead %s has a non-string role", beadID)
	}
	elems := parseRoleList(role)
	project := projectFromFields(agent.Fields)
	var roleBeads []*model.Bead
	if len(elems) > 0 {
		if roleBeads, err = roleBeadsByTitle(ctx, s, elems); err != nil {
			return nil, fmt.Errorf("looking up the caller's role beads: %w", err)
		}
	}
	for _, rb := range roleBeads {
		if !roleHoldsTier(rb.Fields, model.CapabilityTierLandsTenant) {
			continue
		}
		owner, err := homeGardenOwner(ctx, s, elems, roleBeads, project)
		if err != nil {
			return nil, err
		}
		return &landsTenantCaller{
			AgentBeadID: beadID,
			Role:        role,
			Roles:       elems,
			Project:     project,
			Home:        owner != "",
			HomeOwner:   owner,
		}, nil
	}
	land, err := tenantLandOfAgent(ctx, s, project)
	if err != nil || land == "" {
		return nil, err
	}
	return &landsTenantCaller{AgentBeadID: beadID, Role: role, Roles: elems, Project: land, ByLand: true}, nil
}

// tenantLandOfAgent returns the tenant land (land_kind=tenant) a per-agent
// caller is bound to: its token's project scope, which the controller sets at
// mint, else its agent bead's project. "" when neither is a tenant land; a
// lookup error fails closed.
func tenantLandOfAgent(ctx context.Context, s store.Store, project string) (string, error) {
	candidates := []string{TokenProjectFromContext(ctx)}
	if project != candidates[0] {
		candidates = append(candidates, project)
	}
	for _, name := range candidates {
		if name == "" {
			continue
		}
		p, err := s.FindProjectByName(ctx, name)
		switch {
		case errors.Is(err, sql.ErrNoRows) || (err == nil && p == nil):
			continue
		case err != nil:
			return "", fmt.Errorf("looking up the caller's project %s: %w", name, err)
		}
		if kind, _, _ := exactFieldString(p.Fields, "land_kind"); kind == landKindTenant {
			return name, nil
		}
	}
	return "", nil
}

// homeGardenOwner returns the home garden's operator ("oidc:<email>") when the
// caller is the home garden's steward, else "". The home garden's steward is
// role exactly steward, the only tier-holding role it runs as, in a project
// that is the home garden. The home garden is the operator's own land, not a
// tenant one, and its steward keeps its plans' gardeners running (fire-now,
// rebind as role plan-gardener, gd-bo2RUmtpHb), which the §5.1 tenant table
// forbids; it shares the steward role bead with tenant stewards, so the
// project, not the role, tells them apart (owner, 11-02). The home garden is
// read as HomeOwner reads it (controller steward.HomeOwner): a project bead
// whose land_kind is unset or operator, that has a default channel and is
// operated by a person. Anything else, a project bead that cannot be found
// included, stays a tenant; a lookup error fails closed.
func homeGardenOwner(ctx context.Context, s store.Store, elems []string, roleBeads []*model.Bead, project string) (string, error) {
	if len(elems) != 1 || elems[0] != landsStewardRole || project == "" {
		return "", nil
	}
	for _, rb := range roleBeads {
		if rb.Title != landsStewardRole {
			return "", nil
		}
	}
	return homeGardenOperator(ctx, s, project)
}

// homeGardenOperator returns the operator ("oidc:<email>") of project when it
// is the home garden, else "". A lookup error fails closed.
func homeGardenOperator(ctx context.Context, s store.Store, project string) (string, error) {
	if project == "" {
		return "", nil
	}
	p, err := s.FindProjectByName(ctx, project)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && p == nil):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("looking up the caller's project %s: %w", project, err)
	}
	f := parseFieldsToStringMap(p.Fields)
	if kind := strings.TrimSpace(f["land_kind"]); kind != "" && kind != "operator" {
		return "", nil
	}
	if strings.TrimSpace(f["land_default_channel"]) == "" {
		return "", nil
	}
	op := strings.TrimSpace(p.OperatedBy)
	if !strings.HasPrefix(op, "oidc:") || !strings.Contains(op, "@") {
		return "", nil
	}
	return canonicalizeHumanOperator(op), nil
}

// landsTenantDenial builds the structured, non-escalatable refusal every
// lands-tenant check returns.
func landsTenantDenial(action string, beadType model.BeadType, msg string) *AuthzDenial {
	return &AuthzDenial{
		Action:       action,
		BeadType:     string(beadType),
		Message:      msg + "; " + landsTenantSupportHint,
		DeniedReason: landsTenantDeniedReason,
	}
}

// landsTenantLookupDenial is the fail-closed refusal when the caller's tier
// cannot be resolved.
func landsTenantLookupDenial(action string, beadType model.BeadType, err error) *AuthzDenial {
	return &AuthzDenial{
		Action:   action,
		BeadType: string(beadType),
		Message:  "denied: could not verify the caller's capability tiers (" + err.Error() + "); retry",
	}
}

// landsTenantSpawnDenial is the spawn gate's lands-tenant check, for a create
// of an agent bead or an update that sets its role (authorizeAgentRole): the
// shared check's opSpawn (restricted_ops.go). Returns nil for every caller
// that is not a tenant.
func landsTenantSpawnDenial(ctx context.Context, s store.Store, action, role, project string) error {
	if d := landsTenantOpDenial(ctx, s, restrictedOpRequest{Op: opSpawn, Action: action, BeadType: typeAgent, Role: role, Project: project}); d != nil {
		return d
	}
	return nil
}

// landsTenantSpawnTargetDenial is the opSpawn rule for a tenant caller: it may
// only, as the steward, put an agent into lands-gardener (exactly that role,
// not a comma-joined list) in the steward's own project, and the
// lands-gardener role bead must itself hold lands-tenant and not privileged,
// so a mis-written role bead cannot hand a tenant an unrestricted child. A
// missing lands-gardener role bead refuses.
func landsTenantSpawnTargetDenial(ctx context.Context, s store.Store, caller *landsTenantCaller, action, role, project string) *AuthzDenial {
	if !caller.isSteward() {
		return landsTenantDenial(action, "agent", "spawn denied: "+caller.holder()+" holds the lands-tenant tier and only the "+landsStewardRole+" spawns")
	}
	if elems := parseRoleList(role); len(elems) != 1 || elems[0] != landsGardenerRole {
		return landsTenantDenial(action, "agent", "spawn denied: a lands-tenant "+landsStewardRole+" spawns only role "+landsGardenerRole+", not "+strconvQuote(role))
	}
	if caller.Project == "" || project != caller.Project {
		return landsTenantDenial(action, "agent", "spawn denied: a lands-tenant "+landsStewardRole+" spawns only in its own project "+strconvQuote(caller.Project)+", not "+strconvQuote(project))
	}
	return landsGardenerRoleDenial(ctx, s, action)
}

// landsGardenerRoleDenial refuses unless the lands-gardener role bead holds
// exactly the lands-tenant tier and no grant-table row, narrow or privileged,
// nor a grant value that does not parse: what a tenant
// steward's child and a member's own-machine agent (lands_agents.go) run as.
// A missing role bead refuses.
func landsGardenerRoleDenial(ctx context.Context, s store.Store, action string) *AuthzDenial {
	targets, err := roleBeadsByTitle(ctx, s, []string{landsGardenerRole})
	if err != nil {
		return landsTenantLookupDenial(action, "agent", fmt.Errorf("looking up role %s: %w", landsGardenerRole, err))
	}
	if len(targets) == 0 {
		return landsTenantDenial(action, "agent", "spawn denied: role "+landsGardenerRole+" has no role bead")
	}
	for _, rb := range targets {
		if !roleHoldsTier(rb.Fields, model.CapabilityTierLandsTenant) || roleHoldsTier(rb.Fields, model.CapabilityTierPrivileged) {
			return landsTenantDenial(action, "agent", "spawn denied: role bead "+rb.ID+" for "+landsGardenerRole+" does not hold exactly the lands-tenant tier")
		}
		if rows, err := roleOwnershipGrants(rb); err != nil || len(rows) > 0 {
			return landsTenantDenial(action, "agent", "spawn denied: role bead "+rb.ID+" for "+landsGardenerRole+" holds ownership grants")
		}
	}
	return nil
}
