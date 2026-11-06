package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"encoding/json"

	"gardenio/kbeads/internal/model"
)

// Field-write gates on trigger/schedule beads (gd-is1H5NmI05, operator BLOCK #3
// on PR #426).
//
// The spawn-lineage seal (spawn_lineage_seal.go) is voided by updateBead when a
// non-admin turns a trigger on or changes its spawn content. The single-field
// endpoints (POST /v1/beads/{id}/fields/{field}/edit, .../append, .../reorder and
// /v1/beads/{id}/increment) and the extra-fields merge on close mutate fields
// without passing through updateBead, so they skipped both that strip and the
// plan-gardener enable gate: `fields/enabled/edit {op:append, element:"true"}`
// switched a sealed schedule on and the seal survived.
//
// They are closed the strict way rather than by re-running the strip. Those
// endpoints know only the field KEY, not the resulting value, and no service
// writes the enable flag or a spawn-content field through them (the trigger
// engine uses PATCH), so for a trigger/schedule bead:
//
//   - `enabled`, every fingerprinted field (spawnLineageDigestKeys) and the
//     `gardener` blob are writable through the field endpoints only by an OIDC
//     gardenio-admin session (autonomousFieldWriteDenial);
//   - the run counters run_budget and runs_spawned are writable, on EVERY write
//     path (PATCH, close, the field endpoints), only by an OIDC admin or the
//     trigger-engine machine principal (autonomousFieldWriteDenial, autonomousCounterPatchDenial). The run cap
//     reads run_budget, which is outside the seal digest, so zeroing it removed
//     the total-run cap on a sealed privileged gardener. The daily counters
//     runs_day and runs_today are gated the same way: a standing gardener (a
//     land's steward) skips run_budget, so its daily cap (max_runs_per_day,
//     sealed) is its only run bound, and zeroing runs_today would lift it.
var autonomousCounterKeys = []string{"run_budget", "runs_spawned", "runs_day", "runs_today"}

// counterWriterAllowed reports whether ctx may write the run counters: an OIDC
// gardenio-admin session, or the trigger-engine machine principal.
func counterWriterAllowed(ctx context.Context) bool {
	if isOIDCAdminSession(ctx) {
		return true
	}
	return authMethodFromContext(ctx) == authMethodMachine &&
		AuthIdentityFromContext(ctx).Subject == triggerEngineSubject
}

func isAutonomousCounterKey(k string) bool {
	for _, c := range autonomousCounterKeys {
		if k == c {
			return true
		}
	}
	return false
}

// autonomousFieldWriteDenial gates the key-only write paths. action is "update"
// for the field endpoints, "close" for the extra-fields merge on close; only the
// counter and seal rules apply to close, which cannot enable a bead.
func autonomousFieldWriteDenial(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || !isAutonomousBeadType(bead.Type) {
		return nil
	}
	var counters, locked []string
	for _, k := range keys {
		switch {
		case isAutonomousCounterKey(k):
			counters = append(counters, k)
		case action == "update" && (k == "enabled" || k == "gardener" || spawnLineageDigestKey(k)):
			locked = append(locked, k)
		}
	}
	if len(counters) > 0 && !counterWriterAllowed(ctx) {
		return autonomousDenial(action, bead.Type, counters, "run counters are written only by the trigger engine or a "+RoleAdmin+" OIDC session")
	}
	if len(locked) > 0 && !isOIDCAdminSession(ctx) {
		return autonomousDenial(action, bead.Type, locked, "the enable flag and spawn-relevant fields are written only by a "+RoleAdmin+" OIDC session (use PATCH, which voids the spawn-lineage seal)")
	}
	return nil
}

func autonomousDenial(action string, t model.BeadType, keys []string, why string) *AuthzDenial {
	sort.Strings(keys)
	return &AuthzDenial{
		Action:       action,
		BeadType:     string(t),
		RequiredRole: RoleAdmin,
		Message:      "insufficient permissions: " + action + " on type " + string(t) + " field(s) " + strings.Join(keys, ", ") + ": " + why,
	}
}

func spawnLineageDigestKey(k string) bool {
	for _, d := range spawnLineageDigestKeys {
		if d == k {
			return true
		}
	}
	return false
}

// autonomousCounterPatchDenial is the PATCH-path counterpart of
// autonomousCounterDenial: it refuses a patch that changes a run counter
// (autonomousCounterKeys), top-level or inside the `gardener` blob (where the engine keeps
// its copy), unless the caller may write counters. An echo of an unchanged value
// is tolerated, a removal is a change.
func autonomousCounterPatchDenial(ctx context.Context, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if !isAutonomousBeadType(beadType) || len(patch) == 0 || counterWriterAllowed(ctx) {
		return nil
	}
	pm, em := fieldsAsMap(patch), fieldsAsMap(existing)
	var changed []string
	for _, k := range autonomousCounterKeys {
		if pv, ok := pm[k]; ok && !sameScalar(pv, em[k]) {
			changed = append(changed, k)
		}
	}
	if pg, ok := pm["gardener"]; ok {
		pb, eb := gardenerBlob(pg), gardenerBlob(em["gardener"])
		for _, k := range autonomousCounterKeys {
			if !sameScalar(pb[k], eb[k]) {
				changed = append(changed, "gardener."+k)
			}
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return autonomousDenial("update", beadType, changed, "run counters are written only by the trigger engine or a "+RoleAdmin+" OIDC session")
}

func gardenerBlob(v any) map[string]any {
	switch g := v.(type) {
	case map[string]any:
		return g
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(g), &m) == nil {
			return m
		}
	}
	return nil
}

// sameScalar compares two JSON scalars by their printed form, so 3, 3.0 and "3"
// match and nil (absent) matches only nil.
func sameScalar(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}
