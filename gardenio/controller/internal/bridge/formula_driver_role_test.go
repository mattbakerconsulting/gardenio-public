package bridge

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestFormulaDriverRole covers the pour-time driver-role resolver: the formula's
// declared default_roles wins, then step-1's role, then the "crew" default.
// This is the pour-time half of the §7→§8 role handoff (ka-0000005928): before
// it, both pour paths hardcoded "crew" and dropped the formula's declared role.
func TestFormulaDriverRole(t *testing.T) {
	tests := []struct {
		name    string
		formula *beadsapi.BeadDetail
		steps   []formulaStep
		want    string
	}{
		{
			name:    "nil formula, no steps -> crew default",
			formula: nil,
			steps:   nil,
			want:    "crew",
		},
		{
			name:    "no fields, no steps -> crew default",
			formula: &beadsapi.BeadDetail{ID: "gd-f"},
			steps:   nil,
			want:    "crew",
		},
		{
			name: "default_roles JSON array -> first entry",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-fixproof",
				Fields: map[string]string{"default_roles": `["monorepo-fix"]`},
			},
			steps: []formulaStep{{ID: "s1", Roles: []string{"monorepo-fix"}}},
			want:  "monorepo-fix",
		},
		{
			name: "default_roles multi-entry -> first non-empty",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-multi",
				Fields: map[string]string{"default_roles": `["", "monorepo-verify", "monorepo-fix"]`},
			},
			want: "monorepo-verify",
		},
		{
			name: "default_roles as bare string (tolerance) -> that role",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-bare",
				Fields: map[string]string{"default_roles": "monorepo-publish"},
			},
			want: "monorepo-publish",
		},
		{
			name: "no default_roles, step 1 declares role -> step-1 role",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-repro",
				Fields: map[string]string{},
			},
			steps: []formulaStep{
				{ID: "triage", Roles: []string{"fsm-triage"}},
				{ID: "repro", Roles: []string{"monorepo-repro"}},
			},
			want: "fsm-triage",
		},
		{
			name: "no default_roles, step 1 uses legacy singular role -> that role",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-legacy",
				Fields: map[string]string{},
			},
			steps: []formulaStep{{ID: "s1", Role: "monorepo-fix"}},
			want:  "monorepo-fix",
		},
		{
			name: "default_roles beats step-1 role",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-both",
				Fields: map[string]string{"default_roles": `["monorepo-publish"]`},
			},
			steps: []formulaStep{{ID: "s1", Roles: []string{"monorepo-repro"}}},
			want:  "monorepo-publish",
		},
		{
			name: "empty default_roles array, no step roles -> crew",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-empty",
				Fields: map[string]string{"default_roles": `[]`},
			},
			steps: []formulaStep{{ID: "s1"}},
			want:  "crew",
		},
		{
			name: "step 1 with no role, step 2 with role -> crew (only step 1 considered)",
			formula: &beadsapi.BeadDetail{
				ID:     "gd-step2only",
				Fields: map[string]string{},
			},
			steps: []formulaStep{
				{ID: "s1"},
				{ID: "s2", Roles: []string{"monorepo-fix"}},
			},
			want: "crew",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formulaDriverRole(tt.formula, tt.steps)
			if got != tt.want {
				t.Errorf("formulaDriverRole() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseRolesField(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"json array", `["a","b"]`, []string{"a", "b"}},
		{"json single", `["only"]`, []string{"only"}},
		{"bare string fallback", "monorepo-fix", []string{"monorepo-fix"}},
		{"empty json array", `[]`, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRolesField(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("parseRolesField(%q) = %v (len %d), want %v (len %d)",
					tt.raw, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseRolesField(%q)[%d] = %q, want %q", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFirstNonEmptyRole(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  string
	}{
		{"nil", nil, ""},
		{"all empty", []string{"", "  ", ""}, ""},
		{"first wins", []string{"a", "b"}, "a"},
		{"skips empties", []string{"", " ", "c"}, "c"},
		{"trims", []string{"  trimmed  "}, "trimmed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstNonEmptyRole(tt.roles); got != tt.want {
				t.Errorf("firstNonEmptyRole(%v) = %q, want %q", tt.roles, got, tt.want)
			}
		})
	}
}
