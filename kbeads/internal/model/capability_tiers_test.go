package model

import (
	"reflect"
	"testing"
)

func TestParseCapabilityTiers(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want []string
		ok   bool
	}{
		{"nil is empty", nil, []string{}, true},
		{"decoded array", []any{"privileged", "lands-tenant"}, []string{"lands-tenant", "privileged"}, true},
		{"string slice", []string{"privileged", "privileged"}, []string{"privileged"}, true},
		{"json text", `["lands-tenant"]`, []string{"lands-tenant"}, true},
		{"empty text", "", []string{}, true},
		{"bare string", "privileged", nil, false},
		{"mixed array", []any{"privileged", true}, nil, false},
		{"object", map[string]any{"privileged": true}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseCapabilityTiers(tc.in)
			if ok != tc.ok || (tc.ok && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("got %v %t, want %v %t", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNormalizeRoleCapabilityTiers_MalformedStoredSet(t *testing.T) {
	merged := map[string]any{"capability_tiers": "privileged", "privileged": false}
	// Syncing the flag against a set it cannot read would guess.
	if err := NormalizeRoleCapabilityTiers(merged, map[string]any{"privileged": true}); err == nil {
		t.Fatal("privileged toggle against a malformed stored set accepted")
	}
	if err := NormalizeRoleCapabilityTiers(merged, map[string]any{"model": "opus"}); err == nil {
		t.Fatal("unrelated write over a malformed stored set accepted")
	}
	// Control: rewriting the set itself repairs it.
	if err := NormalizeRoleCapabilityTiers(merged, map[string]any{"capability_tiers": []any{"privileged"}}); err != nil {
		t.Fatalf("rewrite refused: %v", err)
	}
	if merged["privileged"] != true {
		t.Fatalf("privileged = %v after rewrite", merged["privileged"])
	}
}
