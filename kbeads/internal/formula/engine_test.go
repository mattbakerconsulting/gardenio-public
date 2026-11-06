package formula

import (
	"reflect"
	"testing"
)

func TestSubstituteVars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		vars map[string]string
		want string
	}{
		{"single", "hello {{name}}", map[string]string{"name": "world"}, "hello world"},
		{"multiple", "{{a}}-{{b}}", map[string]string{"a": "1", "b": "2"}, "1-2"},
		{"repeated", "{{x}}{{x}}", map[string]string{"x": "z"}, "zz"},
		{"unmatched left as-is", "{{missing}} here", map[string]string{}, "{{missing}} here"},
		{"partial", "{{a}} {{missing}}", map[string]string{"a": "A"}, "A {{missing}}"},
		{"no placeholders", "plain text", map[string]string{"a": "A"}, "plain text"},
		{"empty value substitutes to empty", "x{{e}}y", map[string]string{"e": ""}, "xy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubstituteVars(tt.in, tt.vars); got != tt.want {
				t.Errorf("SubstituteVars(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestUnresolvedVarValues(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want []string
	}{
		{"none", map[string]string{"a": "x", "b": "y"}, nil},
		{"one unresolved", map[string]string{"a": "{{other}}"}, []string{`a="{{other}}"`}},
		{
			"multiple sorted",
			map[string]string{"z": "{{p}}", "a": "{{q}}", "ok": "fine"},
			[]string{`a="{{q}}"`, `z="{{p}}"`},
		},
		{"empty map", map[string]string{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnresolvedVarValues(tt.vars); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UnresolvedVarValues() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluateCondition(t *testing.T) {
	tests := []struct {
		name string
		cond string
		vars map[string]string
		want bool
	}{
		{"truthy non-empty", "{{v}}", map[string]string{"v": "yes"}, true},
		{"truthy empty is false", "{{v}}", map[string]string{"v": ""}, false},
		{"truthy literal false", "{{v}}", map[string]string{"v": "false"}, false},
		{"truthy literal zero", "{{v}}", map[string]string{"v": "0"}, false},
		{"negation of empty is true", "!{{v}}", map[string]string{"v": ""}, true},
		{"negation of truthy is false", "!{{v}}", map[string]string{"v": "yes"}, false},
		{"equality match", "{{v}} == prod", map[string]string{"v": "prod"}, true},
		{"equality mismatch", "{{v}} == prod", map[string]string{"v": "dev"}, false},
		{"inequality match", "{{v}} != prod", map[string]string{"v": "dev"}, true},
		{"inequality mismatch", "{{v}} != prod", map[string]string{"v": "prod"}, false},
		{"unresolved placeholder is falsy", "{{missing}}", map[string]string{}, false},
		{"literal non-empty truthy", "somestring", map[string]string{}, true},
		{"literal empty falsy", "", map[string]string{}, false},
		{"whitespace trimmed", "  {{v}}  ", map[string]string{"v": "yes"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateCondition(tt.cond, tt.vars); got != tt.want {
				t.Errorf("EvaluateCondition(%q) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

func TestPriorityOrDefault(t *testing.T) {
	three := 3
	if got := PriorityOrDefault(&three, 1); got != 3 {
		t.Errorf("PriorityOrDefault(&3, 1) = %d, want 3", got)
	}
	if got := PriorityOrDefault(nil, 1); got != 1 {
		t.Errorf("PriorityOrDefault(nil, 1) = %d, want 1", got)
	}
}

func TestMergeLabels(t *testing.T) {
	tests := []struct {
		name  string
		base  []string
		extra []string
		want  []string
	}{
		{"empty extra returns base", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"disjoint", []string{"a"}, []string{"b"}, []string{"a", "b"}},
		{"dedup overlap", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{"dedup within base", []string{"a", "a"}, []string{"b"}, []string{"a", "b"}},
		{"empty base", nil, []string{"x", "x", "y"}, []string{"x", "y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MergeLabels(tt.base, tt.extra); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergeLabels(%v, %v) = %v, want %v", tt.base, tt.extra, got, tt.want)
			}
		})
	}
}

func TestExpandLoopValues(t *testing.T) {
	tests := []struct {
		name string
		loop *Loop
		want []string
	}{
		{"explicit values", &Loop{Values: []string{"a", "b"}}, []string{"a", "b"}},
		{"count", &Loop{Count: 3}, []string{"0", "1", "2"}},
		{"range", &Loop{Range: "2..4"}, []string{"2", "3", "4"}},
		{"range single", &Loop{Range: "5..5"}, []string{"5"}},
		{"invalid range returns nil", &Loop{Range: "not-a-range"}, nil},
		{"reversed range returns nil", &Loop{Range: "5..1"}, nil},
		{"empty loop returns nil", &Loop{}, nil},
		{"values take precedence over count", &Loop{Values: []string{"x"}, Count: 3}, []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandLoopValues(tt.loop); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExpandLoopValues(%+v) = %v, want %v", tt.loop, got, tt.want)
			}
		})
	}
}

func TestMergeNeedsRecursive(t *testing.T) {
	steps := []Step{
		{
			ID:        "a",
			DependsOn: []string{"x"},
			Needs:     []string{"x", "y"}, // x already present, y new
			Children: []Step{
				{ID: "a1", Needs: []string{"z"}},
			},
		},
		{ID: "b"}, // no needs
	}
	MergeNeedsRecursive(steps)

	if got := steps[0].DependsOn; !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("step a DependsOn = %v, want [x y]", got)
	}
	if got := steps[0].Children[0].DependsOn; !reflect.DeepEqual(got, []string{"z"}) {
		t.Errorf("child a1 DependsOn = %v, want [z]", got)
	}
	if steps[1].DependsOn != nil {
		t.Errorf("step b DependsOn = %v, want nil", steps[1].DependsOn)
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"*", "anything", true},
		{"*foo*", "abfooxy", true},
		{"*foo*", "abcxy", false},
		{"build*", "build-image", true},
		{"build*", "test-build", false},
		{"*suffix", "my-suffix", true},
		{"*suffix", "suffix-my", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"~"+tt.s, func(t *testing.T) {
			if got := MatchGlob(tt.pattern, tt.s); got != tt.want {
				t.Errorf("MatchGlob(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
			}
		})
	}
}

func TestMapsEqual(t *testing.T) {
	tests := []struct {
		name string
		a    map[string]string
		b    map[string]string
		want bool
	}{
		{"both empty", map[string]string{}, map[string]string{}, true},
		{"equal", map[string]string{"a": "1"}, map[string]string{"a": "1"}, true},
		{"different length", map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, false},
		{"different value", map[string]string{"a": "1"}, map[string]string{"a": "2"}, false},
		{"different key", map[string]string{"a": "1"}, map[string]string{"b": "1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MapsEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("MapsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestMergeVars(t *testing.T) {
	base := []VarDef{{Name: "a", Default: "1"}, {Name: "b", Default: "2"}}
	overlay := []VarDef{{Name: "b", Default: "override"}, {Name: "c", Default: "3"}}

	got := MergeVars(base, overlay)
	want := []VarDef{
		{Name: "a", Default: "1"},
		{Name: "b", Default: "override"},
		{Name: "c", Default: "3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeVars = %+v, want %+v", got, want)
	}

	if got := MergeVars(base, nil); !reflect.DeepEqual(got, base) {
		t.Errorf("MergeVars(base, nil) = %+v, want base", got)
	}
	if got := MergeVars(nil, overlay); !reflect.DeepEqual(got, overlay) {
		t.Errorf("MergeVars(nil, overlay) = %+v, want overlay", got)
	}
}

func TestMergeSteps(t *testing.T) {
	base := []Step{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}}
	overlay := []Step{{ID: "b", Title: "B2"}, {ID: "c", Title: "C"}}

	got := MergeSteps(base, overlay)
	want := []Step{
		{ID: "a", Title: "A"},
		{ID: "b", Title: "B2"},
		{ID: "c", Title: "C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeSteps = %+v, want %+v", got, want)
	}

	if got := MergeSteps(base, nil); !reflect.DeepEqual(got, base) {
		t.Errorf("MergeSteps(base, nil) = %+v, want base", got)
	}
	if got := MergeSteps(nil, overlay); !reflect.DeepEqual(got, overlay) {
		t.Errorf("MergeSteps(nil, overlay) = %+v, want overlay", got)
	}
}
