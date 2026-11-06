package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// scopeDecision is the read-side project scope of one request's caller.
//
// Unrestricted callers fall through to the pre-existing paths (fleet-token
// project binding, or oidcAllowedProjects' user-bead lookup for global
// operators). A scoped principal is never Unrestricted: its Projects come from
// the verified token on this request, and an empty Projects means deny all.
//
// Tiers is the caller's actionTable row in each of its Projects (a scoped
// principal only), resolved from the same token in the same call, so the read
// scope and the write table can never disagree about which lands are whose.
type scopeDecision struct {
	Unrestricted bool
	Projects     []string
	Tiers        map[string]tier
}

// allows reports whether the decision admits a bead of the given project.
func (d scopeDecision) allows(project string) bool {
	if d.Unrestricted {
		return true
	}
	return slices.Contains(d.Projects, project)
}

// isScopedPrincipal reports whether the caller is an OIDC human that is not a
// gardenio-admin and holds RoleProjectMember, an OIDC token minted for a land
// (a land- audience, land_audience.go), or a land principal (authMethodLand,
// land_principal.go), which is always scoped. Other callers (static, fleet,
// machine, edge) have their own scoping and are never scoped principals.
func isScopedPrincipal(ctx context.Context) bool {
	switch authMethodFromContext(ctx) {
	case authMethodLand:
		return true
	case authMethodOIDC:
	default:
		return false
	}
	if hasLandAudience(ctx) {
		return true
	}
	roles := RolesFromContext(ctx)
	if hasMinimumRole(roles, RoleAdmin) {
		return false
	}
	return slices.Contains(roles, RoleProjectMember)
}

// scopeFromContext resolves the caller's project scope from the verified token
// on this request. For a scoped principal the user bead's allowed_projects is
// deliberately NOT consulted, so a stale bead cannot widen access, and a
// missing, empty or unparseable groups claim yields an empty Projects, which
// denies instead of meaning "unrestricted".
//
// A window principal's scope is empty (window_principal.go). A land
// principal's scope is its one land at tierLand, from the credential alone; with no land on the context it is nothing. A token minted for a land
// is narrowed to that land (narrowToTokenLand): a user in lands A and B whose
// token is for A reaches A only, and a token for several lands reaches none.
func scopeFromContext(ctx context.Context) scopeDecision {
	// A window principal reads one plan through its own route and nothing
	// through this engine: its scope is empty, never Unrestricted.
	if authMethodFromContext(ctx) == authMethodWindow {
		return scopeDecision{Tiers: map[string]tier{}}
	}
	if !isScopedPrincipal(ctx) {
		return scopeDecision{Unrestricted: true}
	}
	if authMethodFromContext(ctx) == authMethodLand {
		land, ok := landPrincipalFromContext(ctx)
		if !ok {
			return scopeDecision{Tiers: map[string]tier{}}
		}
		return scopeDecision{Projects: []string{land}, Tiers: map[string]tier{land: tierLand}}
	}
	d := scopeDecision{Tiers: map[string]tier{}}
	for project, role := range groupsToLandRoles(GroupsFromContext(ctx)) {
		d.Projects = append(d.Projects, project)
		d.Tiers[project] = role.tier()
	}
	slices.Sort(d.Projects)
	return narrowToTokenLand(ctx, d)
}

// scopedUpdateAction returns the action updateBead checks a scoped principal's
// patch against: scopedActionReact for a message patch that changes only
// fields.reactions, scopedActionJoin for a channel patch that changes only
// fields.members, "update" for everything else and for every caller that is
// not a scoped principal.
func scopedUpdateAction(ctx context.Context, bead *model.Bead, in *updateBeadInput) string {
	switch {
	case !isScopedPrincipal(ctx):
		return "update"
	case messageReactionsOnly(bead, in):
		return scopedActionReact
	case channelMembershipOnly(bead, in):
		return scopedActionJoin
	case notesContentOnly(bead, in):
		return scopedActionEditHolds
	}
	return "update"
}

// messageReactionsOnly reports whether a patch on a message bead changes
// fields.reactions and nothing else. bead must be the pre-patch state.
func messageReactionsOnly(bead *model.Bead, in *updateBeadInput) bool {
	return bead.Type == typeMessage && patchOnlyChangesFields([]string{"reactions"}, in, bead.Fields)
}

// scopedReactionDenial checks a scoped principal's reactions-only patch: it may
// add or remove only its own principal (oidc:<email>), so it cannot forge or
// erase anyone else's reaction. fields.reactions is a map of reaction to the
// list of principals who reacted (Lands spec §2.3). The patch replaces the
// whole map, so a patch built from a stale read also lands here, as a deny that
// says to re-read.
func scopedReactionDenial(ctx context.Context, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	deny := func(msg string) *AuthzDenial {
		return &AuthzDenial{Action: scopedActionReact, BeadType: string(bead.Type), Message: msg}
	}
	id := AuthIdentityFromContext(ctx)
	if id.Subject == "" {
		return deny("scoped principal has no identity to react as")
	}
	self := "oidc:" + id.Subject
	var p struct {
		Reactions map[string][]string `json:"reactions"`
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return deny("fields.reactions must be a map of reaction to a list of principals")
	}
	var e struct {
		Reactions map[string][]string `json:"reactions"`
	}
	if len(bead.Fields) > 0 {
		// An unparseable stored value is treated as empty: every other
		// principal in it is then "added" by the patch, which denies below.
		_ = json.Unmarshal(bead.Fields, &e)
	}
	others := func(m map[string][]string, reaction string) []string {
		var out []string
		for _, who := range m[reaction] {
			if who != self {
				out = append(out, who)
			}
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	for _, m := range []map[string][]string{p.Reactions, e.Reactions} {
		for reaction := range m {
			if !slices.Equal(others(p.Reactions, reaction), others(e.Reactions, reaction)) {
				return deny("scoped principal may change only its own reactions (" + self + "); reaction " + reaction + " changed for others, re-read the message and retry")
			}
		}
	}
	return nil
}

// scopedWriteDenial is the write-side twin of scopeFromContext. It returns nil
// for any caller that is not a scoped principal. For a scoped principal it
// denies unless some tier may take this action on the bead type, every
// project: label on the bead is in the principal's token projects, AND the
// principal's tier in each of those lands permits it (actionTable). A bead
// with no project label is denied: nothing says whose it is.
func scopedWriteDenial(ctx context.Context, action string, beadType model.BeadType, labels []string) *AuthzDenial {
	if denial := scopedTypeDenial(ctx, action, beadType); denial != nil {
		return denial
	}
	if denial := scopedProjectDenial(ctx, action, beadType, labels); denial != nil {
		return denial
	}
	return scopedTierDenial(ctx, action, beadType, labels)
}

// scopedTypeDenial is the type half of scopedWriteDenial. createBead runs it
// ahead of type resolution and field validation so a scoped principal gets the
// same 403 for every type no tier may write, including ones it could not have
// created anyway (unknown type, missing required fields). A write marked
// withMachineWrite skips it, as it skips authorizeWriteTitled: an allowlisted server
// write on the caller's behalf after its own gate (a window mint).
func scopedTypeDenial(ctx context.Context, action string, beadType model.BeadType) *AuthzDenial {
	if !isScopedPrincipal(ctx) || isMachineWrite(ctx) {
		return nil
	}
	if anyTierAllows(beadType, action) {
		return nil
	}
	return &AuthzDenial{
		Action:   action,
		BeadType: string(beadType),
		Message:  "scoped principal may not " + action + " beads of type " + string(beadType),
	}
}

// scopedProjectDenial is the project half of scopedWriteDenial, also used for
// targets that only need to be in scope (a dependency's other end).
func scopedProjectDenial(ctx context.Context, action string, beadType model.BeadType, labels []string) *AuthzDenial {
	if !isScopedPrincipal(ctx) {
		return nil
	}
	scope := scopeFromContext(ctx)
	var projects []string
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok && p != "" {
			projects = append(projects, p)
		}
	}
	if len(projects) == 0 {
		return &AuthzDenial{
			Action:   action,
			BeadType: string(beadType),
			Message:  "scoped principal may not " + action + " a " + string(beadType) + " bead with no project label",
		}
	}
	for _, p := range projects {
		if !scope.allows(p) {
			return &AuthzDenial{
				Action:        action,
				BeadType:      string(beadType),
				RequiredScope: p,
				Message:       "scoped principal not authorized for project " + p,
			}
		}
	}
	return nil
}

// scopedFieldsProjectDenial rejects a scoped principal's write whose fields
// name a project outside its token projects. The controller reads fields.project
// for some bead types, so a label in scope must not launder a field out of it.
func scopedFieldsProjectDenial(ctx context.Context, action string, beadType model.BeadType, fields json.RawMessage) *AuthzDenial {
	if !isScopedPrincipal(ctx) || len(fields) == 0 {
		return nil
	}
	p := projectFromFields(fields)
	if p == "" || scopeFromContext(ctx).allows(p) {
		return nil
	}
	return &AuthzDenial{
		Action:        action,
		BeadType:      string(beadType),
		RequiredScope: p,
		Message:       "scoped principal not authorized for project " + p,
	}
}

// scopedLabelChangeDenial gates a single label add/remove by a scoped principal.
// Any project: label may be neither added outside its projects nor removed (a
// bead stripped of its project label drops out of every scoped principal's
// reach, including its own), nor added for a frozen land. A runner is held
// the same way to its allowlist (runnerLabelChangeDenial).
func scopedLabelChangeDenial(ctx context.Context, st store.Store, beadType model.BeadType, label string, add bool) *AuthzDenial {
	if !isScopedPrincipal(ctx) {
		return runnerLabelChangeDenial(ctx, label, add)
	}
	p, ok := strings.CutPrefix(label, "project:")
	if !ok {
		return nil
	}
	if !add {
		return &AuthzDenial{
			Action:   "update",
			BeadType: string(beadType),
			Message:  "scoped principal may not remove a project label",
		}
	}
	if !scopeFromContext(ctx).allows(p) {
		return &AuthzDenial{
			Action:        "update",
			BeadType:      string(beadType),
			RequiredScope: p,
			Message:       "scoped principal not authorized for project " + p,
		}
	}
	// Labelling a bead into a frozen land writes in it (land_frozen.go).
	return frozenLandDenial(ctx, st, "update", beadType, []string{label})
}

// tenantAgentLabelDenial keeps a tenant land's agent inside its land: every
// project: label it creates a bead with, sets or adds must name its token's
// project, so it can neither move a bead into another land (gd-Mpy0lOLACZ)
// nor file one that also carries the fleet's or another land's project
// (gd-kwJ0jPMdNY). A scoped principal is gated by scopedWriteDenial and
// scopedLabelChangeDenial instead; any other caller passes.
func (s *BeadsServer) tenantAgentLabelDenial(ctx context.Context, action string, beadType model.BeadType, labels []string) *AuthzDenial {
	if isScopedPrincipal(ctx) || !s.callerIsTenant(ctx) {
		return nil
	}
	proj := TokenProjectFromContext(ctx)
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok && p != proj {
			return &AuthzDenial{
				Action:        action,
				BeadType:      string(beadType),
				RequiredScope: p,
				Message:       "token not authorized for project " + p,
			}
		}
	}
	return nil
}

// tenantAgentProjectLabelDenial holds a tenant land's agent to the
// scoped-principal ban on removing a project: label (scopedLabelChangeDenial):
// a write whose label set after drops a project: label of before is refused,
// so its bead neither leaves its land's view nor reads as the fleet's
// (gd-U9AV7jG8hF). A scoped principal is gated by scopedLabelChangeDenial
// instead; any other caller passes.
func (s *BeadsServer) tenantAgentProjectLabelDenial(ctx context.Context, beadType model.BeadType, before, after []string) *AuthzDenial {
	i := slices.IndexFunc(before, func(l string) bool {
		return strings.HasPrefix(l, "project:") && !slices.Contains(after, l)
	})
	if i < 0 || isScopedPrincipal(ctx) || !s.callerIsTenant(ctx) {
		return nil
	}
	return &AuthzDenial{
		Action:   "update",
		BeadType: string(beadType),
		Message:  "a tenant agent may not remove the label " + before[i],
	}
}

// edgeTargetHidden reports whether a tenant caller (a scoped principal or a
// tenant land's agent) may not point an edge at target: target is missing or
// outside the caller's read scope. Every route that writes an edge from the
// caller's bead (a link, parent_bead_id on create, X-Trigger-Bead on a claim)
// answers such a target as a missing one, so a tenant can neither attach its
// bead to another land's or the fleet's nor learn that the bead exists
// (gd-fgMx8iN5Uh). Any other caller sees only a missing target as hidden, as
// does a machine write: its edge is the server's own (a land window's parent
// is the label-less land bead), authorized at its call site.
func (s *BeadsServer) edgeTargetHidden(ctx context.Context, target *model.Bead) bool {
	if target == nil {
		return true
	}
	return !isMachineWrite(ctx) && s.callerIsTenant(ctx) && s.beadHiddenFromCaller(ctx, target)
}

// scopedDependencyTargetDenial gates the far end of a link a tenant caller
// (a scoped principal or a tenant land's agent) adds or removes; the source
// end is gated by the normal write path. hidden reports a target that is
// missing, unreadable or unfetchable: the route answers all three as a
// missing target, so a link cannot tell A that B's bead exists
// (gd-Mpy0lOLACZ). A readable target must be in a scoped principal's
// projects, whatever its type, and not in a frozen land. A runner's target
// must be in its project allowlist (runnerDependencyTargetDenial). Any other
// caller passes, and its missing target is left for the store to report.
func (s *BeadsServer) scopedDependencyTargetDenial(ctx context.Context, dependsOnID string) (hidden bool, denial *AuthzDenial) {
	if authMethodFromContext(ctx) == authMethodRunner {
		return s.runnerDependencyTargetDenial(ctx, dependsOnID)
	}
	if !s.callerIsTenant(ctx) {
		return false, nil
	}
	target, err := s.store.GetBead(ctx, dependsOnID)
	if err != nil || s.edgeTargetHidden(ctx, target) {
		return true, nil
	}
	if denial := scopedProjectDenial(ctx, "update", target.Type, target.Labels); denial != nil {
		return false, denial
	}
	// A link into a frozen land is a write in it (land_frozen.go).
	return false, frozenLandDenial(ctx, s.store, "update", target.Type, target.Labels)
}

// loadBeadForWriteGRPC is the gRPC twin of loadBeadForWriteCore for the
// relation RPCs (labels, dependencies, comments): it fetches the bead and runs
// the token-project scope and write_policy gates its HTTP twin runs, so a
// static or fleet token cannot relabel, link or comment on a bead whose
// min_role it lacks (gd-FiWt1xkXzb). With enforceOwnership it runs
// authorizeWriteForBead (write_policy, ownership and taint gates); without it
// only the type-level write_policy, as loadBeadForComment does for
// comment-create (ka-0000005884). The scoped-principal gate is part of
// write_policy (authorizeWriteTitled).
func (s *BeadsServer) loadBeadForWriteGRPC(ctx context.Context, beadID, action string, enforceOwnership bool) (*model.Bead, error) {
	bead, err := s.store.GetBead(ctx, beadID)
	if err != nil {
		return nil, storeError(err, "bead")
	}
	if bead == nil {
		return nil, status.Error(codes.NotFound, "bead not found")
	}
	// Each refusal below answers a bead the caller cannot read as missing.
	if !beadMatchesTokenProject(ctx, bead.Labels) {
		return nil, s.refusedGRPC(ctx, bead, status.Error(codes.PermissionDenied, "token not authorized for this bead's project"))
	}
	var denial *AuthzDenial
	if enforceOwnership {
		denial = s.authorizeWriteForBead(ctx, action, bead)
	} else {
		denial = s.authorizeWriteTitled(ctx, action, bead.Type, bead.Labels, bead.Title)
	}
	if denial != nil {
		return nil, s.refusedGRPC(ctx, bead, status.Error(codes.PermissionDenied, denial.Error()))
	}
	return bead, nil
}
