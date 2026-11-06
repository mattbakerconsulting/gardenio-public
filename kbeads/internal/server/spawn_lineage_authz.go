package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Trigger-lineage spawn authorization (decision gd-wNOUd0MCJo "lineage",
// task gd-is1H5NmI05).
//
// Once the per-agent-token flip gives reviewer, gardener, plan-gardener,
// plan-captain, captain and release-captain an all-types ownership_grants row,
// every role a trigger spawns is privileged (ownershipGrantPrivileged) and
// authorizeNonHumanSpawn escalates every non-OIDC spawn, so every automated
// spawn would wait for the owner. The trigger-engine is the one machine
// principal whose spawns are the delegated act of a human: a trigger bead
// authored by a gardenio-admin is that admin's
// standing instruction to spawn the role. A spawn the engine makes because of
// such a trigger is authorized AS that admin, exactly as if they had spawned it
// themselves (an OIDC gardenio-admin passes authorizeAgentRole unconditionally).
//
// Nothing here trusts a client value. The engine's identity and capability come
// from the verified JWT; fields.trigger_id is only a reference the server
// dereferences; the author is the OIDC admin the server sealed the trigger to
// (spawn_lineage_seal.go), not its operated_by / created_by, which non-OIDC
// callers can influence; and the author's role is read from Keycloak, not from
// the request.
// Every other case falls through to the gate exactly as before.

// triggerEngineSubject is the only machine principal whose spawns may be
// authorized through trigger lineage.
const triggerEngineSubject = machineSubjectPrefix + "gardenio-trigger-engine"

type triggerLineageSpawnKey struct{}

// triggerLineageSpawn is the server-computed authorization for one spawn.
type triggerLineageSpawn struct {
	TriggerID string
	Author    string // the admin the spawn is authorized as (operated_by)
}

// triggerLineageSpawnFromContext reports whether ctx carries a lineage
// authorization. Only withTriggerLineageSpawnAuth sets it, and the key is
// unexported, so no caller-supplied value can forge one.
func triggerLineageSpawnFromContext(ctx context.Context) (triggerLineageSpawn, bool) {
	v, ok := ctx.Value(triggerLineageSpawnKey{}).(triggerLineageSpawn)
	return v, ok && v.TriggerID != "" && v.Author != ""
}

// withTriggerLineageSpawnAuth returns ctx stamped with the lineage
// authorization when this create qualifies, and ctx unchanged otherwise. It
// never fails a request: a spawn that does not qualify simply meets the
// privileged-role gate as it always did.
//
// The create qualifies only when ALL hold:
//   - the caller is machine:gardenio-trigger-engine holding capTriggerLineage;
//   - fields.trigger_id names an existing, open trigger bead (or an enabled
//     schedule) in the same project as the spawn, and the requested role is the
//     role that trigger is configured to spawn;
//   - the bead carries a valid spawn-lineage seal: an OIDC gardenio-admin
//     session wrote it and no spawn-relevant field has changed since
//     (spawn_lineage_seal.go);
//   - the seal is younger than the seal TTL, so a demoted admin's seals lapse.
//
// The home garden's steward firing one of its plans' gardeners (gb gardener
// fire-now) qualifies the same way, in place of the engine, when the trigger is
// a plan-gardener pinned to a plan by its gardener-epic:<plan> label and its
// seal's author is the home garden's owner (homeStewardFiresOwnGardener): the
// owner stands behind the trigger, and the steward fires it for them.
func (s *BeadsServer) withTriggerLineageSpawnAuth(ctx context.Context, fields json.RawMessage) context.Context {
	id := AuthIdentityFromContext(ctx)
	engine := authMethodFromContext(ctx) == authMethodMachine && id.Subject == triggerEngineSubject &&
		machineHasCapability(ctx, capTriggerLineage)
	var homeOwner string
	if !engine {
		if _, isAgent := agentBeadIDFromContext(ctx); !isAgent {
			return ctx
		}
		caller, err := landsTenantCallerFrom(ctx, s.store)
		if err != nil || caller == nil || !caller.Home || caller.HomeOwner == "" {
			return ctx
		}
		homeOwner = caller.HomeOwner
	}
	triggerID := triggerIDFromFields(fields)
	if triggerID == "" {
		return ctx
	}
	deny := func(reason string) context.Context {
		slog.Info("spawn authorization: trigger lineage not applied",
			"trigger", triggerID, "reason", reason, "subject", id.Subject)
		return ctx
	}

	trig, err := s.store.GetBead(ctx, triggerID)
	if err != nil || trig == nil {
		return deny("trigger_not_found")
	}
	if !isAutonomousBeadType(trig.Type) {
		return deny("not_a_trigger")
	}
	if trig.Status == model.StatusClosed || !effectiveGardenerEnabled(trig.Type, trig.Fields) {
		return deny("trigger_closed_or_disabled")
	}
	project := projectFromFields(fields)
	if project == "" || projectFromLabelsOrEmpty(trig.Labels) != project ||
		strings.TrimSpace(fieldsStringValue(trig.Fields, "project")) != project {
		return deny("project_mismatch")
	}

	// The role the engine asks for must be the role this trigger is configured
	// to spawn, so citing an admin trigger buys nothing for any other role.
	requestedRole, _, rErr := exactFieldString(fields, agentRoleField)
	if rErr != nil || canonicalRole(requestedRole) == "" || canonicalRole(requestedRole) != triggerSpawnRole(trig) {
		return deny("role_mismatch")
	}

	// Who stands behind the trigger comes from the server-stamped seal, never
	// from operated_by / created_by / X-Operated-By, which non-OIDC callers
	// influence. The seal exists only if an OIDC gardenio-admin session wrote
	// the bead, and it is void if any spawn-relevant field changed since.
	seal := validSpawnSeal(trig)
	if seal == nil {
		return deny("not_sealed_by_oidc_admin")
	}
	if !s.spawnSealFresh(seal) {
		return deny("seal_expired")
	}
	author := seal.Author
	if homeOwner != "" && (triggerSpawnRole(trig) != planGardenerSpawnRole ||
		gardenerEpicFromLabels(trig.Labels) == "" || author != homeOwner) {
		return deny("home_steward_not_its_owners_plan_gardener")
	}
	return context.WithValue(ctx, triggerLineageSpawnKey{}, triggerLineageSpawn{TriggerID: triggerID, Author: author})
}

// projectFromLabelsOrEmpty is the project: label's value, or "" when there is
// none (projectFromLabels reports "unknown", which must never match).
// fieldsStringValue is the string value of key in a fields blob, or "".
func fieldsStringValue(fields json.RawMessage, key string) string {
	v, _ := fieldsAsMap(fields)[key].(string)
	return v
}

func projectFromLabelsOrEmpty(labels []string) string {
	if !hasProjectLabel(labels) {
		return ""
	}
	return projectFromLabels(labels)
}
