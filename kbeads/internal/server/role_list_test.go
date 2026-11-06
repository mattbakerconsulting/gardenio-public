package server

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// roleParseCasesPath is the table shared with the controller
// (cmd/controller/rolelist_test.go): both sides must parse an agent's `role`
// string to the same element list, or a role kbeads does not see as privileged
// can still resolve to a privileged role bead in the pod spec (gd-Yhbc5wdjwj).
const roleParseCasesPath = "../../../testdata/role-parse-cases.json"

func TestParseRoleList_SharedTable(t *testing.T) {
	raw, err := os.ReadFile(roleParseCasesPath)
	if err != nil {
		t.Fatalf("reading shared role-parse table: %v", err)
	}
	var table struct {
		Cases []struct {
			Role string   `json:"role"`
			Want []string `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("decoding shared role-parse table: %v", err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("shared role-parse table is empty")
	}
	for _, tc := range table.Cases {
		got := parseRoleList(tc.Role)
		if len(got) == 0 && len(tc.Want) == 0 {
			continue
		}
		if !slices.Equal(got, tc.Want) {
			t.Errorf("parseRoleList(%q) = %q, want %q", tc.Role, got, tc.Want)
		}
	}
}

func TestRoleListsEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"crew", " crew ", true},
		{"crew,infra", "crew, infra", true},
		{"crew,infra", "infra,crew", false},
		{"crew", "crew,infra", false},
		{"", "  ", true},
		{"", "crew", false},
	}
	for _, tc := range cases {
		if got := roleListsEqual(parseRoleList(tc.a), parseRoleList(tc.b)); got != tc.want {
			t.Errorf("roleListsEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
