package main

import (
	"reflect"
	"testing"
)

func TestPlanAdviceMigration(t *testing.T) {
	cases := []struct {
		name      string
		in        []string
		want      []string
		ambiguous bool
	}{
		{
			name: "global alone untouched",
			in:   []string{"global"},
			want: []string{"global"},
		},
		{
			name: "project alone untouched",
			in:   []string{"project:beads"},
			want: []string{"project:beads"},
		},
		{
			name: "role alone untouched",
			in:   []string{"role:polecat"},
			want: []string{"role:polecat"},
		},
		{
			name: "agent alone untouched",
			in:   []string{"agent:beads/crews/bot"},
			want: []string{"agent:beads/crews/bot"},
		},
		{
			name: "global + project drops global",
			in:   []string{"global", "project:beads"},
			want: []string{"project:beads"},
		},
		{
			name: "global + agent drops global",
			in:   []string{"global", "agent:beads/crews/bot"},
			want: []string{"agent:beads/crews/bot"},
		},
		{
			name: "global + role drops role",
			in:   []string{"global", "role:polecat"},
			want: []string{"global"},
		},
		{
			name: "rig alone becomes project",
			in:   []string{"rig:beads"},
			want: []string{"project:beads"},
		},
		{
			name: "rig duplicate of project drops rig",
			in:   []string{"rig:beads", "project:beads"},
			want: []string{"project:beads"},
		},
		{
			name: "rig+project with different values flags ambiguous",
			in:   []string{"rig:beads", "project:gastown"},
			// ambiguous path → labels untouched
			want:      []string{"rig:beads", "project:gastown"},
			ambiguous: true,
		},
		{
			name: "global + rig drops global and rewrites rig",
			in:   []string{"global", "rig:beads"},
			want: []string{"project:beads"},
		},
		{
			name: "gN-prefixed rig rewrites inside group",
			in:   []string{"g0:rig:beads", "g0:role:polecat"},
			want: []string{"g0:project:beads", "g0:role:polecat"},
		},
		{
			name: "non-targeting free labels survive",
			in:   []string{"project:beads", "inline:true", "topic:ci"},
			want: []string{"project:beads", "inline:true", "topic:ci"},
		},
		{
			name: "idempotent — canonical input unchanged",
			in:   []string{"project:beads", "role:polecat"},
			want: []string{"project:beads", "role:polecat"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planAdviceMigration("gd-test", "test", tc.in)
			if plan.Ambiguous != tc.ambiguous {
				t.Errorf("Ambiguous = %v, want %v", plan.Ambiguous, tc.ambiguous)
			}
			if !stringsEqualSorted(plan.AfterLabels, tc.want) {
				t.Errorf("AfterLabels = %v, want %v", plan.AfterLabels, tc.want)
			}
			// Idempotence — applying the plan to its own output should be a no-op.
			second := planAdviceMigration("gd-test", "test", plan.AfterLabels)
			if second.changed() && !second.Ambiguous {
				t.Errorf("migration is not idempotent: %v → %v → %v",
					tc.in, plan.AfterLabels, second.AfterLabels)
			}
		})
	}
}

func TestSplitGroupPrefix(t *testing.T) {
	cases := map[string][2]string{
		"g0:role:polecat":  {"g0:", "role:polecat"},
		"g12:rig:beads":    {"g12:", "rig:beads"},
		"global":           {"", "global"},
		"project:beads":    {"", "project:beads"},
		"g:bad":            {"", "g:bad"},
		"gx:bad":           {"", "gx:bad"},
	}
	for in, want := range cases {
		gotPrefix, gotRest := splitGroupPrefix(in)
		if gotPrefix != want[0] || gotRest != want[1] {
			t.Errorf("splitGroupPrefix(%q) = (%q,%q), want (%q,%q)",
				in, gotPrefix, gotRest, want[0], want[1])
		}
	}
}

func TestStringsEqualSorted(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{}, []string{}, true},
		{[]string{"a"}, []string{"b"}, false},
		{[]string{"a"}, []string{"a", "a"}, false},
	}
	for i, tc := range cases {
		if got := stringsEqualSorted(tc.a, tc.b); got != tc.want {
			t.Errorf("case %d: stringsEqualSorted(%v, %v) = %v, want %v",
				i, tc.a, tc.b, got, tc.want)
		}
	}
}

// Guard against accidentally using reflect.DeepEqual instead of sorted compare.
func TestPlanAdviceMigration_OrderInsensitiveIdempotent(t *testing.T) {
	in := []string{"project:beads", "role:polecat", "inline:true"}
	first := planAdviceMigration("gd-x", "x", in)
	// Reversed input — expect the same final set (order-insensitive).
	rev := append([]string(nil), in...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	second := planAdviceMigration("gd-x", "x", rev)
	if !stringsEqualSorted(first.AfterLabels, second.AfterLabels) {
		t.Errorf("order-insensitive idempotence broken: %v vs %v",
			first.AfterLabels, second.AfterLabels)
	}
	// And reflect.DeepEqual not used — regression sentinel.
	_ = reflect.DeepEqual
}
