package main

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// roleParseCasesPath is the table shared with kbeads
// (kbeads/internal/server/role_list_test.go): both sides must parse an agent's
// `role` string to the same element list (gd-Yhbc5wdjwj).
const roleParseCasesPath = "../../../../testdata/role-parse-cases.json"

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
