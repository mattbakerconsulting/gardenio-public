package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Capability tiers on type:role beads (P9 gd-aR6UzS0HgM, task gd-Bcqj2r4m7E).
// Decision gd-yldoBclgjx chose an unordered set of named tiers over a scalar
// trust rank: a role declares zero or more tiers in capability_tiers, and a
// check asks "does the role hold tier X", never "is it above level N".
//
// The legacy privileged boolean is a derived view of the set while callers
// still read it: privileged is true exactly when the set holds
// CapabilityTierPrivileged. The daemon keeps the two in step on every
// type:role write (NormalizeRoleCapabilityTiers), and `kd admin backfill
// capability-tiers` stamps the set onto role beads written before the field
// existed.
const (
	// RoleCapabilityTiersField is the type:role field holding the tier set.
	RoleCapabilityTiersField = "capability_tiers"
	// RolePrivilegedField is the legacy boolean derived from the set.
	RolePrivilegedField = "privileged"

	// CapabilityTierPrivileged is the tier today's privileged flag stands
	// for: a non-human spawn of the role goes through the escalation path
	// (spawn_authz.go roleIsPrivileged), and the role may set the elevated
	// runtime fields (needs_k8s_api, service_account, ziti_bind, docker, a
	// non-default execution_target).
	CapabilityTierPrivileged = "privileged"
	// CapabilityTierLandsTenant is the restricted tier the Lands steward and
	// lands-gardener roles hold (Lands Track B spec §5.1). The name is
	// registered here so those role beads can be written (step 9.1); what it
	// permits is step 10.1's.
	CapabilityTierLandsTenant = "lands-tenant"
)

// CapabilityTiers is the registered tier set. The type:role schema's
// capability_tiers enum[] values (server/config.go) must list exactly these;
// TestRoleSchemaCapabilityTiersMatchRegistry holds them together.
var CapabilityTiers = []string{CapabilityTierPrivileged, CapabilityTierLandsTenant}

// ParseCapabilityTiers decodes a capability_tiers value as stored or patched:
// a JSON array of strings ([]any after json.Unmarshal into map[string]any),
// a []string, the array's JSON text, or nil (no tiers). It returns the set
// sorted and de-duplicated, and false when the value is not an array of
// strings. Tier names are not checked here; the type:role schema's enum[]
// does that.
func ParseCapabilityTiers(v any) ([]string, bool) {
	var raw []string
	switch t := v.(type) {
	case nil:
		return []string{}, true
	case []string:
		raw = t
	case []any:
		for _, elem := range t {
			s, ok := elem.(string)
			if !ok {
				return nil, false
			}
			raw = append(raw, s)
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" || s == "null" {
			return []string{}, true
		}
		if err := json.Unmarshal([]byte(s), &raw); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, s := range raw {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, true
}

// HasCapabilityTier reports whether tiers holds tier.
func HasCapabilityTier(tiers []string, tier string) bool {
	for _, t := range tiers {
		if t == tier {
			return true
		}
	}
	return false
}

// RolePrivilegedFlag reads the legacy privileged flag as stored (JSON true or
// "true"), without looking at capability_tiers.
func RolePrivilegedFlag(fields map[string]any) bool {
	return fieldTruthy(fields[RolePrivilegedField])
}

// NormalizeRoleCapabilityTiers keeps privileged and capability_tiers in step
// on a type:role write. merged is the bead's fields after the write (on
// create, the create body) and is updated in place; patch is what the caller
// sent (on create, the same map). After it returns nil, merged carries both
// fields and privileged == HasCapabilityTier(tiers, "privileged").
//
//   - A patch that sets capability_tiers decides: privileged follows it. If
//     the same patch also sets privileged to the other value, the write is
//     refused rather than silently picking one.
//   - A patch that sets only privileged (a caller that has not moved to
//     tiers) adds or removes the privileged tier and leaves the rest.
//   - A patch that touches neither re-derives privileged from the stored set,
//     or, on a bead with no set yet, stamps the set its privileged flag
//     implies. Any role write therefore backfills that role.
//
// A JSON null (merge-patch delete) counts as setting the field empty.
func NormalizeRoleCapabilityTiers(merged, patch map[string]any) error {
	patchTiers, patchSetsTiers := patch[RoleCapabilityTiersField]
	patchPriv, patchSetsPriv := patch[RolePrivilegedField]

	var tiers []string
	switch {
	case patchSetsTiers:
		t, ok := ParseCapabilityTiers(patchTiers)
		if !ok {
			return fmt.Errorf("%s must be an array of tier names (one of %v)", RoleCapabilityTiersField, CapabilityTiers)
		}
		tiers = t
		if patchSetsPriv && fieldTruthy(patchPriv) != HasCapabilityTier(tiers, CapabilityTierPrivileged) {
			return fmt.Errorf("privileged=%t contradicts %s=%v in the same write; privileged is derived from %s, so set %s alone",
				fieldTruthy(patchPriv), RoleCapabilityTiersField, tiers, RoleCapabilityTiersField, RoleCapabilityTiersField)
		}
	case patchSetsPriv:
		stored, err := storedTiers(merged)
		if err != nil {
			return err
		}
		tiers = tiersFromPrivileged(stored, fieldTruthy(patchPriv))
	default:
		if _, has := merged[RoleCapabilityTiersField]; has {
			stored, err := storedTiers(merged)
			if err != nil {
				return err
			}
			tiers = stored
		} else {
			tiers = tiersFromPrivileged(nil, RolePrivilegedFlag(merged))
		}
	}

	merged[RoleCapabilityTiersField] = tiers
	merged[RolePrivilegedField] = HasCapabilityTier(tiers, CapabilityTierPrivileged)
	return nil
}

// storedTiers parses the capability_tiers already in merged. A bead with no
// set yet has none; a malformed stored set is an error, because syncing the
// privileged flag against it would guess.
func storedTiers(merged map[string]any) ([]string, error) {
	v, ok := merged[RoleCapabilityTiersField]
	if !ok {
		return []string{}, nil
	}
	tiers, ok := ParseCapabilityTiers(v)
	if !ok {
		return nil, fmt.Errorf("stored %s is not an array of tier names; rewrite it (one of %v)", RoleCapabilityTiersField, CapabilityTiers)
	}
	return tiers, nil
}

// tiersFromPrivileged returns base with the privileged tier added (on) or
// removed (off), sorted.
func tiersFromPrivileged(base []string, on bool) []string {
	out := make([]string, 0, len(base)+1)
	for _, t := range base {
		if t != CapabilityTierPrivileged {
			out = append(out, t)
		}
	}
	if on {
		out = append(out, CapabilityTierPrivileged)
	}
	sort.Strings(out)
	return out
}

// fieldTruthy reads a boolean field as stored (JSON true) or as text
// ("true"); anything else, including absent, is false.
func fieldTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil && b
	}
	return false
}
