package server

import (
	"bytes"
	"encoding/json"
	"strings"
)

// parseRoleList parses an agent bead's `role` field into its elements: split on
// commas, trim whitespace, drop empty elements. It is deliberately the SAME
// parse the controller applies when it resolves the role beads that shape a pod
// (cmd/controller/podspec.go parseRoleList / applyRoleDefaults); the shared
// table testdata/role-parse-cases.json pins the two together (gd-Yhbc5wdjwj).
// Any authorization decision keyed on a role string MUST go through this, never
// compare the whole string: "crew,infra" and " infra" both resolve to the
// privileged infra role bead in the controller.
func parseRoleList(role string) []string {
	return splitComma(role)
}

// roleListsEqual reports whether a and b are the same ordered element list.
func roleListsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// canonicalRole is the stored form of an agent's role: the parsed elements
// joined with a bare comma. The controller parses role the same way, so the
// canonical form resolves to the same role beads, but every place that matches
// a stored role by exact string (spawn_policy cap counting, pool lookups) sees
// one spelling for one role set, not one per whitespace variant.
func canonicalRole(role string) string {
	return strings.Join(parseRoleList(role), ",")
}

// exactFieldString reads key from a fields blob by its EXACT name. Decoding into
// a struct would match keys case-insensitively, but the controller reads the
// field with an exact-key map lookup (beadsapi fields["role"]), so a key the
// controller never sees as `role` must not feed an authorization decision.
// present is false for an absent key or a JSON null; err is set when the value
// is present but not a string.
func exactFieldString(fields json.RawMessage, key string) (val string, present bool, err error) {
	if len(fields) == 0 {
		return "", false, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(fields, &m) != nil {
		return "", false, nil
	}
	raw, ok := m[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	if err := json.Unmarshal(raw, &val); err != nil {
		return "", true, err
	}
	return val, true, nil
}

// caseVariantKeys returns the keys of a fields blob that case-fold to one of
// names without being spelled exactly like it. encoding/json would fold such a
// key onto the field in a struct decode while the controller ignores it, so a
// type:agent write carrying one is refused outright.
func caseVariantKeys(fields json.RawMessage, names ...string) []string {
	var m map[string]json.RawMessage
	if len(fields) == 0 || json.Unmarshal(fields, &m) != nil {
		return nil
	}
	var out []string
	for k := range m {
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				out = append(out, k)
			}
		}
	}
	return out
}

// canonicalizeRoleField rewrites a fields blob's exact `role` key to its
// canonical form (canonicalRole). Blobs with no string role, or a role already
// canonical, are returned untouched.
func canonicalizeRoleField(fields json.RawMessage) json.RawMessage {
	role, present, err := exactFieldString(fields, agentRoleField)
	if !present || err != nil || canonicalRole(role) == role {
		return fields
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(fields, &m) != nil {
		return fields
	}
	m[agentRoleField], _ = json.Marshal(canonicalRole(role))
	out, err := json.Marshal(m)
	if err != nil {
		return fields
	}
	return out
}
