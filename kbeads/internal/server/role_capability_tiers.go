package server

import (
	"encoding/json"
	"fmt"

	"gardenio/kbeads/internal/model"
)

// roleTierFields are the two type:role fields kept in step by
// model.NormalizeRoleCapabilityTiers: capability_tiers and the privileged
// flag derived from it (gd-Bcqj2r4m7E).
var roleTierFields = map[string]bool{
	model.RoleCapabilityTiersField: true,
	model.RolePrivilegedField:      true,
}

// normalizeRoleTierFields applies model.NormalizeRoleCapabilityTiers to a
// type:role write. merged is the bead's fields after the write (the create
// body on create) and patch is what the caller sent (the same blob on create).
// It returns the fields to persist. A blob that is not a JSON object is
// returned unchanged so the schema check reports it.
func normalizeRoleTierFields(merged, patch json.RawMessage) (json.RawMessage, error) {
	m := map[string]any{}
	if len(merged) > 0 {
		if err := json.Unmarshal(merged, &m); err != nil || m == nil {
			return merged, nil
		}
	}
	p := map[string]any{}
	if len(patch) > 0 {
		if err := json.Unmarshal(patch, &p); err != nil || p == nil {
			return merged, nil
		}
	}
	if err := model.NormalizeRoleCapabilityTiers(m, p); err != nil {
		return nil, err
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to encode role fields: %w", err)
	}
	return out, nil
}

// roleTierFieldEndpointError refuses writing privileged or capability_tiers on
// a type:role bead through a single-field endpoint (PUT/POST fields, a close
// body, mergeBeadFields). Those routes write one key straight to the store,
// so they could set one of the pair without the other; a fields PATCH runs
// normalizeRoleTierFields and keeps them in step.
func roleTierFieldEndpointError(bead *model.Bead, keys ...string) error {
	if bead == nil || bead.Type != "role" {
		return nil
	}
	for _, k := range keys {
		if roleTierFields[k] {
			return fmt.Errorf("%s on a role bead is written with a fields update (kd update <id> -f %s=...), which keeps privileged and %s in step",
				k, model.RoleCapabilityTiersField, model.RoleCapabilityTiersField)
		}
	}
	return nil
}
