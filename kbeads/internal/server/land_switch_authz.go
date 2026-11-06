package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Lands kill-switch gate (Track B §5.5, step 10.5; gd-QVP0Q6tzYB). The two
// kill switches live in ordinary beads:
//
//   - a land's freeze is land_frozen on its project bead, and land_kind says
//     whether the land is a tenant's at all (the spawn wall, the purse and the
//     freeze only apply to tenant lands);
//   - the all-lands pause is an open type:gate bead titled lands-pause.
//
// The machine allow-list admits {update, project} for the shared static token
// and fleet tokens (write_authz.go), and the static token bypasses ownership,
// so without this gate any agent pod could thaw a frozen land, turn a tenant
// land into an operator one, rename or close its project bead out from under
// the wall, or close the lands-pause gate. Only an OIDC gardenio-admin (or an
// approved escalation replay) throws a kill switch either way.
//
// The Lands provisioner (gd-t92sawEzFJ) is the one exception, for the writes
// that open, mark and close a land and nothing else: land_kind=tenant on the
// project bead it creates (the only project bead it may create, and no one but
// an admin creates any other: authorizeProjectCreate), land_frozen=true on a tenant land it is closing,
// the close of that frozen tenant project bead (the wall refuses a known
// tenant land with no live project bead, so it stays shut), and the gatehouse
// marks on a tenant land (provisionerLandSwitchWrite). A thaw, any other
// land_kind write, a rename, a status change through update, a delete and the
// lands-pause gate stay admin-only for it too.

// landSwitchFields are the gated type:project fields. The gatehouse fields
// ride the same gate (gd-Ymdaxee0sX): land_gatehouse_tools turns on the
// gatehouse steward's apply and demo tools and the viewer's reactor, which the
// operator does once the owner signs off (gardenio/docs/lands/gatehouse.md);
// land_gatehouse and land_gatehouse_backfilled_at are the provisioner's marks
// (provisionerLandSwitchWrite).
var landSwitchFields = map[string]bool{
	"land_kind":                    true,
	"land_frozen":                  true,
	"land_frozen_by":               true,
	"land_frozen_at":               true,
	"land_frozen_reason":           true,
	"land_gatehouse":               true,
	"land_gatehouse_backfilled_at": true,
	"land_gatehouse_tools":         true,
}

// Lands mint pins (Track B §3.2, step 8.7; gd-Ld5r7Ok2rK review). The
// controller mints a project's GitHub tokens with the App github_app names,
// from the installation github_installation_id pins, on the repo git_url
// names, or repos' primary entry when repos is set (the controller prefers
// it, podspec.go applyCommonConfig). The Lands provisioner writes all four
// when it opens a land, from the setup it verified. A tenant who could rewrite them on its own project
// bead (land members and the land's agent pods can update it) would repoint
// its token at another installation or another repo, so:
//
//   - github_app and github_installation_id are written only by an admin or
//     the Lands provisioner, on every project bead;
//   - git_url and repos are too, on a project that is a tenant land or names
//     the Lands App. An operator project's repos stay as writable as before.
//
// The controller also checks at mint and refresh time that the installation
// is on the repo owner's account and can reach the repo
// (cmd/controller/github_app_credentials.go).

// landMintFields are the always-gated mint pins.
var landMintFields = map[string]bool{
	"github_app":             true,
	"github_installation_id": true,
}

// landRepoFields name the repo a token is minted for; they are gated on a
// land whose tokens are pinned (landRepoPinned).
var landRepoFields = map[string]bool{
	"git_url": true,
	"repos":   true,
}

// landsProvisionerRole is the Lands provisioner's machine realm role
// (machine_grants.go).
const landsProvisionerRole = "gardenio-machine-lands-provisioner"

// isLandMintKey reports whether k is gated on some project bead: a mint pin
// or a repo field. The extra-fields merge uses it to decide whether to load the
// bead at all.
func isLandMintKey(k string) bool { return landMintFields[k] || landRepoFields[k] }

// landRepoPinned reports whether any of fields makes the project's repo a
// pinned one: land_kind set to anything but operator, or github_app lands.
// Callers pass the stored fields and the patch, so a write that makes a
// project a tenant land or moves it to the Lands App is gated too.
func landRepoPinned(fields ...map[string]any) bool {
	for _, f := range fields {
		if k := strings.TrimSpace(channelBindingValue(f["land_kind"])); k != "" && k != "operator" {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(channelBindingValue(f["github_app"])), "lands") {
			return true
		}
	}
	return false
}

// changedLandMintFields returns the sorted mint keys a create (existing ==
// nil) or update would set, change or clear: github_app and
// github_installation_id always, git_url and repos when landRepoPinned. An unchanged
// value echoed back does not count.
func changedLandMintFields(patch, existing json.RawMessage) []string {
	if len(patch) == 0 {
		return nil
	}
	var p, e map[string]any
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil
	}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &e)
	}
	pinned := landRepoPinned(e, p)
	var changed []string
	for k, pv := range p {
		if !landMintFields[k] && (!landRepoFields[k] || !pinned) {
			continue
		}
		if channelBindingValue(pv) == channelBindingValue(e[k]) {
			continue
		}
		changed = append(changed, k)
	}
	sort.Strings(changed)
	return changed
}

// landMintWriter reports whether the caller may write the mint pins: whoever
// may throw a kill switch (killSwitchWriter), or the Lands provisioner.
func landMintWriter(ctx context.Context) bool {
	return killSwitchWriter(ctx) || landsProvisioner(ctx)
}

// landsProvisioner reports whether the caller is the Lands provisioner's
// machine principal.
func landsProvisioner(ctx context.Context) bool {
	return authMethodFromContext(ctx) == authMethodMachine &&
		slices.Contains(machineRolesFromContext(ctx), landsProvisionerRole)
}

// denyLandMintWrite is the mint pins' decision point.
func (s *BeadsServer) denyLandMintWrite(ctx context.Context, action string, beadType model.BeadType, what string) *AuthzDenial {
	if landMintWriter(ctx) {
		return nil
	}
	d := &AuthzDenial{
		Action:       action,
		BeadType:     string(beadType),
		RequiredRole: RoleAdmin,
		Escalatable:  true,
		Message: "insufficient permissions: " + action + " on type " + string(beadType) + " to " + what +
			" requires " + RoleAdmin + "; which GitHub App and installation mint a project's tokens, and a tenant land's repo, are set only by the operator or the Lands provisioner",
	}
	s.populateAdvisoryText(ctx, d, "github_app, github_installation_id and a tenant land's git_url and repos decide which GitHub repo a land's agents get a token for; changing them needs "+RoleAdmin+".")
	return d
}

// landsPauseGateTitle is the title of the all-lands pause gate
// (controller internal/pausegate.LandsPauseTitle).
const landsPauseGateTitle = "lands-pause"

// isLandsPauseGate reports whether b is the lands-pause gate.
func isLandsPauseGate(b *model.Bead) bool {
	return b != nil && b.Type == "gate" && b.Title == landsPauseGateTitle
}

// changedLandSwitchFields returns the sorted gated keys a create (existing ==
// nil) or update (existing = the stored fields) would set, change or clear. An
// unchanged value echoed back by a read-modify-write client does not count.
func changedLandSwitchFields(patch, existing json.RawMessage) []string {
	if len(patch) == 0 {
		return nil
	}
	var p, e map[string]any
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil
	}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &e)
	}
	var changed []string
	for k, pv := range p {
		if !landSwitchFields[k] || channelBindingValue(pv) == channelBindingValue(e[k]) {
			continue
		}
		changed = append(changed, k)
	}
	sort.Strings(changed)
	return changed
}

// authorizeLandSwitchFields is the field-level gate on type:project creates
// and updates: the kill switches, then the mint pins, then (for every caller)
// no Lands pin on an operator org (land_operator_org.go). patch is the incoming fields; existing is the stored fields on
// update, nil on create.
func (s *BeadsServer) authorizeLandSwitchFields(ctx context.Context, action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if beadType != "project" {
		return nil
	}
	if changed := changedLandSwitchFields(patch, existing); len(changed) > 0 && !provisionerLandSwitchWrite(ctx, action, changed, patch, existing) {
		if d := s.denyKillSwitchWrite(ctx, action, beadType, "set "+strings.Join(changed, ", ")); d != nil {
			return d
		}
	}
	if changed := changedLandMintFields(patch, existing); len(changed) > 0 {
		if d := s.denyLandMintWrite(ctx, action, beadType, "set "+strings.Join(changed, ", ")); d != nil {
			return d
		}
	}
	return s.denyOperatorOrgPin(action, beadType, patch, existing)
}

// authorizeLandSwitchKeyWrite gates the single-field mutation endpoints and
// the extra-fields merge on a project bead. Only the key is known, not the
// value, so touching a gated key at all is denied for a non-admin: a kill
// switch, a mint pin, or a repo field on a land whose stored fields pin its
// repo. While operator GitHub orgs are set, the keys that could bind a Lands
// pin to one are refused for everyone (denyOperatorOrgPinKey).
func (s *BeadsServer) authorizeLandSwitchKeyWrite(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || bead.Type != "project" {
		return nil
	}
	var changed []string
	for _, k := range keys {
		if landSwitchFields[k] {
			changed = append(changed, k)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		if d := s.denyKillSwitchWrite(ctx, action, bead.Type, "set "+strings.Join(changed, ", ")); d != nil {
			return d
		}
		changed = nil
	}
	var stored map[string]any
	_ = json.Unmarshal(bead.Fields, &stored)
	pinned := landRepoPinned(stored)
	for _, k := range keys {
		if landMintFields[k] || (landRepoFields[k] && pinned) {
			changed = append(changed, k)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		if d := s.denyLandMintWrite(ctx, action, bead.Type, "set "+strings.Join(changed, ", ")); d != nil {
			return d
		}
	}
	return s.denyOperatorOrgPinKey(action, bead, keys...)
}

// authorizeKillSwitchLifecycle gates a status change or rename of a project
// bead or the lands-pause gate through update, and any close or delete of the
// lands-pause gate. Each takes a kill switch away: a project bead the wall can
// no longer find by name, or a gate that is no longer open, is a land that is
// not frozen and lands that are not paused. Closing and deleting a project
// bead are already admin-only (not on the machine allow-list); this holds even
// if a stored type:project write_policy is weakened.
func (s *BeadsServer) authorizeKillSwitchLifecycle(ctx context.Context, action string, bead *model.Bead, newStatus, newTitle *string) *AuthzDenial {
	if bead == nil || (bead.Type != "project" && !isLandsPauseGate(bead)) {
		return nil
	}
	var what []string
	switch action {
	case "close", "delete":
		what = append(what, action+" it")
	default:
		if newStatus != nil && model.Status(*newStatus) != bead.Status {
			what = append(what, "change its status")
		}
		if newTitle != nil && *newTitle != bead.Title {
			what = append(what, "rename it")
		}
	}
	if len(what) == 0 {
		return nil
	}
	if action == "close" && bead.Type == "project" && landsProvisioner(ctx) && frozenTenantLand(bead.Fields) {
		return nil
	}
	return s.denyKillSwitchWrite(ctx, action, bead.Type, strings.Join(what, " and "))
}

// authorizeProjectCreate refuses a type:project create by anyone but an admin
// (killSwitchWriter) and the Lands provisioner opening a tenant land
// (gd-WCM9rfOL8L). A project bead with no land_kind is an operator project:
// the controller mints fleet-App GitHub tokens for it, and the spawn wall, the
// purse and the freeze do not apply to it. {create, project} used to be on the
// static/fleet allow-list, so any agent pod could open one, and the
// provisioner's grant admitted a create with no land_kind at all. This holds
// even if a stored type:project write_policy is weakened.
func (s *BeadsServer) authorizeProjectCreate(ctx context.Context, beadType model.BeadType, fields json.RawMessage) *AuthzDenial {
	if beadType != "project" || killSwitchWriter(ctx) {
		return nil
	}
	if landsProvisioner(ctx) && tenantLandFields(fields) {
		return nil
	}
	d := &AuthzDenial{
		Action:       "create",
		BeadType:     string(beadType),
		RequiredRole: RoleAdmin,
		Escalatable:  true,
		Message: "insufficient permissions: create on type project requires " + RoleAdmin +
			"; the Lands provisioner may create one only for a tenant land (land_kind=tenant)",
	}
	s.populateAdvisoryText(ctx, d, "Only an operator creates a project; the Lands provisioner opens tenant lands.")
	return d
}

// tenantLandFields reports whether project fields say land_kind=tenant.
func tenantLandFields(fields json.RawMessage) bool {
	var f map[string]any
	if json.Unmarshal(fields, &f) != nil {
		return false
	}
	return channelBindingValue(f["land_kind"]) == landKindTenant
}

// authorizeProjectNameUnique refuses a non-admin create of a type:project
// bead whose name an existing project bead (any status) already carries. Two
// project beads with one name resolve to one of them by rank (purse.readLands
// and FindProjectByName), so a second bead with no land_kind would otherwise
// be a way to make a frozen tenant land read as an unfrozen operator one. A
// failed lookup refuses too: the name might be taken.
func (s *BeadsServer) authorizeProjectNameUnique(ctx context.Context, beadType model.BeadType, title string) *AuthzDenial {
	if beadType != "project" || killSwitchWriter(ctx) {
		return nil
	}
	name := landNameFromTitle(title)
	if name == "" {
		return nil
	}
	p, err := s.store.FindProjectByName(ctx, name)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && p == nil:
		return nil
	case err != nil:
		return s.denyKillSwitchWrite(ctx, "create", beadType, "create project "+name+" while existing project names cannot be checked")
	}
	return s.denyKillSwitchWrite(ctx, "create", beadType, "create a second project named "+name+" (project bead "+p.ID+" has that name)")
}

// landNameFromTitle is a project bead's land name the way the controller
// reads it (purse.readLands): the legacy "Project: " prefix and surrounding
// space dropped.
func landNameFromTitle(title string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(title), "Project: "))
}

// replayKillSwitchDenial vets an escalation's replay as the approver before
// it is approved. The replay runs as a machine write, which skips every gate
// in this file, and the type:gate write policy has no min_role, so without
// this any approving operator could lift the lands-pause gate (or, if the
// type:project policy were weakened, thaw a land) by approving an escalation
// whose operation does it. Only an approver who could throw the switch
// directly may approve one. "" when the replay throws no kill switch.
func (s *BeadsServer) replayKillSwitchDenial(ctx context.Context, esc *model.Bead) (string, error) {
	if killSwitchWriter(ctx) {
		return "", nil
	}
	f := parseSpawnEscalationFields(esc)
	switch op := normalizeOperationType(f.OperationType); op {
	case "create":
		var spec createBeadInput
		if json.Unmarshal(f.OperationSpec, &spec) != nil {
			return "", nil
		}
		t := model.BeadType(spec.Type)
		if canonical, ok := model.TypeAliases[t]; ok {
			t = canonical
		}
		if d := s.authorizeProjectCreate(ctx, t, spec.Fields); d != nil {
			return d.Message, nil
		}
		if d := s.authorizeLandSwitchFields(ctx, op, t, spec.Fields, nil); d != nil {
			return d.Message, nil
		}
		if d := s.authorizeProjectNameUnique(ctx, t, spec.Title); d != nil {
			return d.Message, nil
		}
	case "update", "close":
		var raw struct {
			TargetBeadID string `json:"target_bead_id"`
		}
		_ = json.Unmarshal(esc.Fields, &raw)
		if raw.TargetBeadID == "" {
			return "", nil
		}
		tb, err := s.store.GetBead(ctx, raw.TargetBeadID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && tb == nil) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if op == "close" {
			if d := s.authorizeKillSwitchLifecycle(ctx, op, tb, nil, nil); d != nil {
				return d.Message, nil
			}
			return "", nil
		}
		var spec updateBeadInput
		if json.Unmarshal(f.OperationSpec, &spec) != nil {
			return "", nil
		}
		if d := s.authorizeLandSwitchFields(ctx, op, tb.Type, spec.Fields, tb.Fields); d != nil {
			return d.Message, nil
		}
		if d := s.authorizeKillSwitchLifecycle(ctx, op, tb, spec.Status, spec.Title); d != nil {
			return d.Message, nil
		}
	}
	return "", nil
}

// killSwitchWriter reports whether the caller may throw a kill switch: an
// OIDC gardenio-admin (a verified v2 edge envelope counts as OIDC), an
// auth-disabled daemon, or an approved escalation replay (whose approver
// replayKillSwitchDenial has already vetted). Everyone else may not: a land
// member and a land principal, a non-admin operator, a v1 edge envelope (no
// role, gd-AvSKOUyqRz), and every service token (static, fleet, machine),
// which an agent pod in any land may hold.
func killSwitchWriter(ctx context.Context) bool {
	if isMachineWrite(ctx) {
		return true
	}
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return true
	case authMethodOIDC:
		return hasMinimumRole(RolesFromContext(ctx), RoleAdmin)
	}
	return false
}

// provisionerLandSwitchWrite reports whether the caller is the Lands
// provisioner and every key in changed (from changedLandSwitchFields) is a
// kill-switch write it makes: land_kind=tenant on create (opening a land),
// land_frozen=true on a stored tenant land (closing one), and the gatehouse
// marks (provision/gatehouse.go): land_gatehouse=true on a tenant land it
// creates or holds, and land_gatehouse_backfilled_at stamped once on a tenant
// land. Nothing else, so not a thaw, not a cleared or rewound mark, and never
// land_gatehouse_tools.
func provisionerLandSwitchWrite(ctx context.Context, action string, changed []string, patch, existing json.RawMessage) bool {
	if !landsProvisioner(ctx) || len(changed) == 0 {
		return false
	}
	var p, e map[string]any
	if json.Unmarshal(patch, &p) != nil {
		return false
	}
	_ = json.Unmarshal(existing, &e)
	var tenant bool
	switch action {
	case "create":
		// A create opens a tenant land, so it carries land_kind.
		tenant = channelBindingValue(p["land_kind"]) == "tenant"
		if !slices.Contains(changed, "land_kind") {
			return false
		}
	case "update":
		tenant = channelBindingValue(e["land_kind"]) == "tenant"
	default:
		return false
	}
	if !tenant {
		return false
	}
	for _, k := range changed {
		v := channelBindingValue(p[k])
		var ok bool
		switch k {
		case "land_kind":
			ok = action == "create"
		case "land_frozen":
			ok = action == "update" && v == "true"
		case "land_gatehouse":
			ok = v == "true"
		case "land_gatehouse_backfilled_at":
			ok = action == "update" && v != "" && channelBindingValue(e[k]) == ""
		}
		if !ok {
			return false
		}
	}
	return true
}

// frozenTenantLand reports whether stored project fields are a tenant land
// with land_frozen set: the only project bead the Lands provisioner closes.
func frozenTenantLand(fields json.RawMessage) bool {
	var f map[string]any
	if json.Unmarshal(fields, &f) != nil {
		return false
	}
	return channelBindingValue(f["land_kind"]) == "tenant" && channelBindingValue(f["land_frozen"]) == "true"
}

// denyKillSwitchWrite is the single decision point: a denial unless
// killSwitchWriter admits the caller.
func (s *BeadsServer) denyKillSwitchWrite(ctx context.Context, action string, beadType model.BeadType, what string) *AuthzDenial {
	if killSwitchWriter(ctx) {
		return nil
	}
	d := &AuthzDenial{
		Action:       action,
		BeadType:     string(beadType),
		RequiredRole: RoleAdmin,
		Escalatable:  true,
		Message: "insufficient permissions: " + action + " on type " + string(beadType) + " to " + what +
			" requires " + RoleAdmin + "; the Lands kill switches (land_frozen, land_kind, the gatehouse's land_gatehouse_tools, the " +
			landsPauseGateTitle + " gate) are thrown only by the operator",
	}
	s.populateAdvisoryText(ctx, d, "A land's freeze, its kind and the "+landsPauseGateTitle+" gate are the Lands kill switches; changing them needs "+RoleAdmin+".")
	return d
}
