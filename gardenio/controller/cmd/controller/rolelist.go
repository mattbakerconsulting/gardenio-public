package main

import "strings"

// parseRoleList parses an agent's `role` string into its elements: split on
// commas, trim whitespace, drop empty elements. Every place the controller
// resolves role beads from spec.Role goes through this, and kbeads applies the
// identical parse before any authorization decision keyed on a role
// (kbeads/internal/server/role_list.go parseRoleList), so a role string kbeads
// treats as privileged is exactly the set of role beads the pod spec merges.
// testdata/role-parse-cases.json pins the two together (gd-Yhbc5wdjwj).
func parseRoleList(role string) []string {
	var roles []string
	for _, r := range strings.Split(role, ",") {
		if r = strings.TrimSpace(r); r != "" {
			roles = append(roles, r)
		}
	}
	return roles
}
