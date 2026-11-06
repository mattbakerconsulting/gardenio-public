package formula

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// mapGetter is a test Getter backed by an in-memory map of bead ID -> Bead.
// A missing ID returns an error, mimicking a store miss.
type mapGetter map[string]*Bead

func (m mapGetter) GetFormulaBead(_ context.Context, id string) (*Bead, error) {
	if b, ok := m[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bead %s not found", id)
}

// mustFields marshals a Fields into a formula Bead of the given ID.
func mustFields(t *testing.T, id string, f Fields) *Bead {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal fields for %s: %v", id, err)
	}
	return &Bead{ID: id, Type: "formula", Fields: raw}
}

func TestParseFields(t *testing.T) {
	t.Run("empty raw errors", func(t *testing.T) {
		if _, err := ParseFields(nil); err == nil {
			t.Fatal("expected error for empty fields")
		}
	})
	t.Run("malformed json errors", func(t *testing.T) {
		if _, err := ParseFields(json.RawMessage(`{bad`)); err == nil {
			t.Fatal("expected error for malformed json")
		}
	})
	t.Run("valid", func(t *testing.T) {
		f, err := ParseFields(json.RawMessage(`{"formula_type":"workflow","steps":[{"id":"s1"}]}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.FormulaType != "workflow" || len(f.Steps) != 1 {
			t.Errorf("parsed = %+v", f)
		}
	})
}

func TestRejectNonWorkflow(t *testing.T) {
	tests := []struct {
		name    string
		ftype   string
		wantErr bool
	}{
		{"workflow ok", "workflow", false},
		{"empty ok", "", false},
		{"expansion rejected", "expansion", true},
		{"aspect rejected", "aspect", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RejectNonWorkflow("gd-x", Fields{FormulaType: tt.ftype})
			if (err != nil) != tt.wantErr {
				t.Errorf("RejectNonWorkflow(%q) err = %v, wantErr %v", tt.ftype, err, tt.wantErr)
			}
		})
	}
}

func TestApplyVarDefaults(t *testing.T) {
	t.Run("fills default", func(t *testing.T) {
		vars := map[string]string{}
		fields := Fields{Vars: []VarDef{{Name: "env", Default: "dev"}}}
		if err := ApplyVarDefaults(fields, vars); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if vars["env"] != "dev" {
			t.Errorf("env = %q, want dev", vars["env"])
		}
	})

	t.Run("provided value not overwritten", func(t *testing.T) {
		vars := map[string]string{"env": "prod"}
		fields := Fields{Vars: []VarDef{{Name: "env", Default: "dev"}}}
		if err := ApplyVarDefaults(fields, vars); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if vars["env"] != "prod" {
			t.Errorf("env = %q, want prod", vars["env"])
		}
	})

	t.Run("required missing errors", func(t *testing.T) {
		fields := Fields{Vars: []VarDef{{Name: "env", Required: true}}}
		if err := ApplyVarDefaults(fields, map[string]string{}); err == nil {
			t.Fatal("expected required-missing error")
		}
	})

	t.Run("enum valid", func(t *testing.T) {
		fields := Fields{Vars: []VarDef{{Name: "env", Enum: []string{"dev", "prod"}}}}
		if err := ApplyVarDefaults(fields, map[string]string{"env": "prod"}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("enum invalid errors", func(t *testing.T) {
		fields := Fields{Vars: []VarDef{{Name: "env", Enum: []string{"dev", "prod"}}}}
		if err := ApplyVarDefaults(fields, map[string]string{"env": "staging"}); err == nil {
			t.Fatal("expected enum-violation error")
		}
	})

	t.Run("pattern valid", func(t *testing.T) {
		fields := Fields{Vars: []VarDef{{Name: "v", Pattern: `^\d+$`}}}
		if err := ApplyVarDefaults(fields, map[string]string{"v": "123"}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("pattern mismatch errors", func(t *testing.T) {
		fields := Fields{Vars: []VarDef{{Name: "v", Pattern: `^\d+$`}}}
		if err := ApplyVarDefaults(fields, map[string]string{"v": "abc"}); err == nil {
			t.Fatal("expected pattern-mismatch error")
		}
	})

	t.Run("invalid pattern regex errors", func(t *testing.T) {
		fields := Fields{Vars: []VarDef{{Name: "v", Pattern: `(`}}}
		if err := ApplyVarDefaults(fields, map[string]string{"v": "x"}); err == nil {
			t.Fatal("expected invalid-regex error")
		}
	})

	t.Run("default then enum-checked together", func(t *testing.T) {
		// Default fills in, and the filled value is enum-validated.
		fields := Fields{Vars: []VarDef{{Name: "env", Default: "dev", Enum: []string{"dev", "prod"}}}}
		vars := map[string]string{}
		if err := ApplyVarDefaults(fields, vars); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if vars["env"] != "dev" {
			t.Errorf("env = %q, want dev", vars["env"])
		}
	})
}

func TestResolveInheritanceNoExtends(t *testing.T) {
	child := Fields{Steps: []Step{{ID: "s1"}}}
	got, err := ResolveInheritance(context.Background(), mapGetter{}, "gd-child", child)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !reflect.DeepEqual(got, child) {
		t.Errorf("no-extends should return child unchanged, got %+v", got)
	}
}

func TestResolveInheritanceMergesParent(t *testing.T) {
	parent := mustFields(t, "gd-parent", Fields{
		Vars:         []VarDef{{Name: "a", Default: "1"}, {Name: "b", Default: "2"}},
		Steps:        []Step{{ID: "p1", Title: "parent step"}},
		DefaultRoles: []string{"crew"},
		Phase:        "vapor",
	})
	g := mapGetter{"gd-parent": parent}

	child := Fields{
		Extends: []string{"gd-parent"},
		Vars:    []VarDef{{Name: "b", Default: "override"}, {Name: "c", Default: "3"}},
		Steps:   []Step{{ID: "c1", Title: "child step"}},
	}
	got, err := ResolveInheritance(context.Background(), g, "gd-child", child)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Vars: parent a,b then child overrides b and adds c.
	wantVars := VarDefList{{Name: "a", Default: "1"}, {Name: "b", Default: "override"}, {Name: "c", Default: "3"}}
	if !reflect.DeepEqual(got.Vars, wantVars) {
		t.Errorf("Vars = %+v, want %+v", got.Vars, wantVars)
	}
	// Steps: parent p1 then child c1.
	if len(got.Steps) != 2 || got.Steps[0].ID != "p1" || got.Steps[1].ID != "c1" {
		t.Errorf("Steps = %+v, want [p1 c1]", got.Steps)
	}
	// DefaultRoles inherited (child had none).
	if !reflect.DeepEqual(got.DefaultRoles, []string{"crew"}) {
		t.Errorf("DefaultRoles = %v, want [crew]", got.DefaultRoles)
	}
	// Phase inherited.
	if got.Phase != "vapor" {
		t.Errorf("Phase = %q, want vapor", got.Phase)
	}
	// Extends cleared.
	if got.Extends != nil {
		t.Errorf("Extends = %v, want nil (resolved)", got.Extends)
	}
}

func TestResolveInheritanceCircular(t *testing.T) {
	// gd-a extends gd-b, gd-b extends gd-a.
	a := mustFields(t, "gd-a", Fields{Extends: []string{"gd-b"}})
	b := mustFields(t, "gd-b", Fields{Extends: []string{"gd-a"}})
	g := mapGetter{"gd-a": a, "gd-b": b}

	child := Fields{Extends: []string{"gd-a"}}
	_, err := ResolveInheritance(context.Background(), g, "gd-root", child)
	if err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("expected circular error, got %v", err)
	}
}

// TestResolveInheritanceDiamond guards against a false-positive cycle detection
// on a diamond-shaped (but acyclic) inheritance graph: child extends [A, B],
// and both A and B extend a shared ancestor Z. Before the fix, the `visited`
// set was never rewound on ascent, so resolving A marked Z visited and resolving
// B then saw Z as "already in chain" and aborted with a spurious circular error.
func TestResolveInheritanceDiamond(t *testing.T) {
	z := mustFields(t, "gd-z", Fields{
		Vars:  []VarDef{{Name: "z", Default: "z"}},
		Steps: []Step{{ID: "z1", Title: "shared ancestor step"}},
	})
	a := mustFields(t, "gd-a", Fields{
		Extends: []string{"gd-z"},
		Steps:   []Step{{ID: "a1", Title: "a step"}},
	})
	b := mustFields(t, "gd-b", Fields{
		Extends: []string{"gd-z"},
		Steps:   []Step{{ID: "b1", Title: "b step"}},
	})
	g := mapGetter{"gd-a": a, "gd-b": b, "gd-z": z}

	child := Fields{Extends: []string{"gd-a", "gd-b"}, Steps: []Step{{ID: "c1"}}}
	got, err := ResolveInheritance(context.Background(), g, "gd-root", child)
	if err != nil {
		t.Fatalf("diamond inheritance rejected: %v", err)
	}

	// The shared ancestor's step appears exactly once (deduped by ID), followed
	// by A's, B's, and the child's steps.
	var ids []string
	for _, s := range got.Steps {
		ids = append(ids, s.ID)
	}
	want := []string{"z1", "a1", "b1", "c1"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("Steps = %v, want %v (z1 deduped, not duplicated)", ids, want)
	}
	// The shared ancestor's var survives the diamond merge.
	if len(got.Vars) != 1 || got.Vars[0].Name != "z" {
		t.Errorf("Vars = %+v, want single [z]", got.Vars)
	}
}

func TestResolveInheritanceMissingParent(t *testing.T) {
	child := Fields{Extends: []string{"gd-missing"}}
	_, err := ResolveInheritance(context.Background(), mapGetter{}, "gd-root", child)
	if err == nil {
		t.Fatal("expected error fetching missing parent")
	}
}

func TestResolveInheritanceWrongType(t *testing.T) {
	notFormula := &Bead{ID: "gd-nf", Type: "task", Fields: json.RawMessage(`{}`)}
	g := mapGetter{"gd-nf": notFormula}
	child := Fields{Extends: []string{"gd-nf"}}
	_, err := ResolveInheritance(context.Background(), g, "gd-root", child)
	if err == nil || !strings.Contains(err.Error(), "not formula") {
		t.Fatalf("expected not-formula error, got %v", err)
	}
}

func TestResolveInheritanceParentNoFields(t *testing.T) {
	g := mapGetter{"gd-empty": {ID: "gd-empty", Type: "formula"}}
	child := Fields{Extends: []string{"gd-empty"}}
	_, err := ResolveInheritance(context.Background(), g, "gd-root", child)
	if err == nil || !strings.Contains(err.Error(), "no fields") {
		t.Fatalf("expected no-fields error, got %v", err)
	}
}

func TestResolveInheritanceMaxDepth(t *testing.T) {
	// Build a linear extends chain longer than maxDepth (10): gd-0 -> gd-1 -> ... -> gd-11.
	g := mapGetter{}
	for i := 0; i <= 11; i++ {
		f := Fields{}
		if i < 11 {
			f.Extends = []string{fmt.Sprintf("gd-%d", i+1)}
		}
		g[fmt.Sprintf("gd-%d", i)] = mustFields(t, fmt.Sprintf("gd-%d", i), f)
	}
	child := Fields{Extends: []string{"gd-0"}}
	_, err := ResolveInheritance(context.Background(), g, "gd-root", child)
	if err == nil || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("expected chain-too-deep error, got %v", err)
	}
}

func TestResolveInheritanceParentMalformedFields(t *testing.T) {
	bad := &Bead{ID: "gd-bad", Type: "formula", Fields: json.RawMessage(`{"steps": 123}`)}
	g := mapGetter{"gd-bad": bad}
	child := Fields{Extends: []string{"gd-bad"}}
	_, err := ResolveInheritance(context.Background(), g, "gd-root", child)
	if err == nil || !strings.Contains(err.Error(), "parsing parent formula") {
		t.Fatalf("expected parse-parent error, got %v", err)
	}
}

func TestResolveInheritanceGrandparentChain(t *testing.T) {
	grand := mustFields(t, "gd-grand", Fields{Steps: []Step{{ID: "g1"}}})
	parent := mustFields(t, "gd-parent", Fields{
		Extends: []string{"gd-grand"},
		Steps:   []Step{{ID: "p1"}},
	})
	g := mapGetter{"gd-grand": grand, "gd-parent": parent}

	child := Fields{Extends: []string{"gd-parent"}, Steps: []Step{{ID: "c1"}}}
	got, err := ResolveInheritance(context.Background(), g, "gd-child", child)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// grand g1, parent p1, child c1 in order.
	var ids []string
	for _, s := range got.Steps {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"g1", "p1", "c1"}) {
		t.Errorf("step ids = %v, want [g1 p1 c1]", ids)
	}
}

func TestInlineExpansion(t *testing.T) {
	expansion := mustFields(t, "gd-exp", Fields{
		FormulaType: "expansion",
		Vars:        []VarDef{{Name: "region", Default: "us"}},
		Steps: []Step{
			{ID: "root", Title: "scan {{region}}"},
			{ID: "child", Title: "process", DependsOn: []string{"root"}},
		},
	})
	g := mapGetter{"gd-exp": expansion}

	expandStep := Step{ID: "deploy", Expand: "gd-exp", DependsOn: []string{"upstream"}}
	got, err := InlineExpansion(context.Background(), g, expandStep, map[string]string{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(got))
	}
	// IDs prefixed by expand step ID.
	if got[0].ID != "deploy/root" || got[1].ID != "deploy/child" {
		t.Errorf("ids = %q, %q; want deploy/root, deploy/child", got[0].ID, got[1].ID)
	}
	// Var default applied + substituted into title.
	if got[0].Title != "scan us" {
		t.Errorf("root title = %q, want 'scan us'", got[0].Title)
	}
	// Root step inherits the expand step's external depends_on (prepended).
	if !reflect.DeepEqual(got[0].DependsOn, []string{"upstream"}) {
		t.Errorf("root DependsOn = %v, want [upstream]", got[0].DependsOn)
	}
	// Internal dep rewritten with prefix; non-root does NOT inherit upstream.
	if !reflect.DeepEqual(got[1].DependsOn, []string{"deploy/root"}) {
		t.Errorf("child DependsOn = %v, want [deploy/root]", got[1].DependsOn)
	}
	// Expand fields cleared on inlined steps.
	if got[0].Expand != "" || got[0].ExpandVars != nil {
		t.Errorf("inlined step should have cleared expand fields")
	}
}

func TestInlineExpansionExpandVarsOverride(t *testing.T) {
	expansion := mustFields(t, "gd-exp", Fields{
		FormulaType: "expansion",
		Steps:       []Step{{ID: "s", Title: "{{x}}"}},
	})
	g := mapGetter{"gd-exp": expansion}
	expandStep := Step{ID: "e", Expand: "gd-exp", ExpandVars: map[string]string{"x": "override"}}
	got, err := InlineExpansion(context.Background(), g, expandStep, map[string]string{"x": "parent"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got[0].Title != "override" {
		t.Errorf("title = %q, want override (expand_vars beats parent var)", got[0].Title)
	}
}

func TestInlineExpansionErrors(t *testing.T) {
	tests := []struct {
		name    string
		getter  mapGetter
		step    Step
		wantSub string
	}{
		{
			"missing bead",
			mapGetter{},
			Step{ID: "e", Expand: "gd-missing"},
			"fetching expansion",
		},
		{
			"wrong bead type",
			mapGetter{"gd-t": {ID: "gd-t", Type: "task", Fields: json.RawMessage(`{}`)}},
			Step{ID: "e", Expand: "gd-t"},
			"not formula",
		},
		{
			"no fields",
			mapGetter{"gd-nf": {ID: "gd-nf", Type: "formula"}},
			Step{ID: "e", Expand: "gd-nf"},
			"no fields",
		},
		{
			"not an expansion type",
			mapGetter{"gd-wf": mustFields(t, "gd-wf", Fields{FormulaType: "workflow", Steps: []Step{{ID: "x"}}})},
			Step{ID: "e", Expand: "gd-wf"},
			"not expansion",
		},
		{
			"expansion with no steps",
			mapGetter{"gd-empty": mustFields(t, "gd-empty", Fields{FormulaType: "expansion"})},
			Step{ID: "e", Expand: "gd-empty"},
			"no steps",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := InlineExpansion(context.Background(), tt.getter, tt.step, map[string]string{})
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

func TestExpandBranch(t *testing.T) {
	expProd := mustFields(t, "gd-prod", Fields{
		FormulaType: "expansion",
		Steps:       []Step{{ID: "deploy", Title: "prod deploy"}},
	})
	expDev := mustFields(t, "gd-dev", Fields{
		FormulaType: "expansion",
		Steps:       []Step{{ID: "deploy", Title: "dev deploy"}},
	})
	g := mapGetter{"gd-prod": expProd, "gd-dev": expDev}

	branchStep := Step{
		ID: "b",
		Branch: &Branch{
			On: "{{env}}",
			Cases: map[string]BranchCase{
				"prod": {Expand: "gd-prod"},
				"dev":  {Expand: "gd-dev"},
			},
		},
	}

	t.Run("matching case", func(t *testing.T) {
		got, err := ExpandBranch(context.Background(), g, branchStep, map[string]string{"env": "prod"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(got) != 1 || got[0].Title != "prod deploy" {
			t.Errorf("got %+v, want prod deploy", got)
		}
	})

	t.Run("default fallback", func(t *testing.T) {
		bs := branchStep
		bs.Branch = &Branch{
			On:      "{{env}}",
			Cases:   map[string]BranchCase{"prod": {Expand: "gd-prod"}},
			Default: &BranchCase{Expand: "gd-dev"},
		}
		got, err := ExpandBranch(context.Background(), g, bs, map[string]string{"env": "unknown"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(got) != 1 || got[0].Title != "dev deploy" {
			t.Errorf("got %+v, want dev deploy (default)", got)
		}
	})

	t.Run("no match no default errors", func(t *testing.T) {
		_, err := ExpandBranch(context.Background(), g, branchStep, map[string]string{"env": "staging"})
		if err == nil || !strings.Contains(err.Error(), "no case matches") {
			t.Fatalf("expected no-case error, got %v", err)
		}
	})
}

func TestApplyAspects(t *testing.T) {
	aspect := mustFields(t, "gd-asp", Fields{
		FormulaType: "aspect",
		Match:       "build*",
		Before:      StepList{{ID: "pre", Title: "setup"}},
		After:       StepList{{ID: "post", Title: "cleanup"}},
	})
	g := mapGetter{"gd-asp": aspect}

	steps := []Step{
		{ID: "build-x", DependsOn: []string{"upstream"}},
		{ID: "test-x"},
	}
	got, err := ApplyAspects(context.Background(), g, []string{"gd-asp"}, steps)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	// build-x matches: before/pre, build-x, after/post injected; test-x untouched.
	want := []string{"build-x/before/pre", "build-x", "build-x/after/post", "test-x"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}

	// The before step inherits the matched step's deps.
	if !reflect.DeepEqual(got[0].DependsOn, []string{"upstream"}) {
		t.Errorf("before DependsOn = %v, want [upstream]", got[0].DependsOn)
	}
	// The matched step now depends on the last before step (prepended).
	if got[1].DependsOn[0] != "build-x/before/pre" {
		t.Errorf("matched step should depend on before step, got %v", got[1].DependsOn)
	}
	// The after step depends on the original step.
	if !reflect.DeepEqual(got[2].DependsOn, []string{"build-x"}) {
		t.Errorf("after DependsOn = %v, want [build-x]", got[2].DependsOn)
	}
}

func TestApplyAspectsErrors(t *testing.T) {
	tests := []struct {
		name    string
		getter  mapGetter
		wantSub string
	}{
		{"missing bead", mapGetter{}, "fetching aspect"},
		{
			"wrong bead type",
			mapGetter{"gd-t": {ID: "gd-t", Type: "task", Fields: json.RawMessage(`{}`)}},
			"not formula",
		},
		{
			"no fields",
			mapGetter{"gd-nf": {ID: "gd-nf", Type: "formula"}},
			"no fields",
		},
		{
			"not an aspect type",
			mapGetter{"gd-wf": mustFields(t, "gd-wf", Fields{FormulaType: "workflow"})},
			"not aspect",
		},
		{
			"aspect with no match pattern",
			mapGetter{"gd-nomatch": mustFields(t, "gd-nomatch", Fields{FormulaType: "aspect"})},
			"no match pattern",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ApplyAspects(context.Background(), tt.getter, []string{firstKey(tt.getter, "gd-missing")}, []Step{{ID: "x"}})
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

// firstKey returns the single key of a one-entry getter, or fallback when empty.
func firstKey(g mapGetter, fallback string) string {
	for k := range g {
		return k
	}
	return fallback
}

func TestExpandStepsPlain(t *testing.T) {
	steps := []Step{
		{ID: "s1", Title: "Build {{env}}", Needs: []string{"s0"}},
		{ID: "s2", Title: "Test", DependsOn: []string{"s1"}},
	}
	got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{"env": "prod"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d steps, want 2", len(got))
	}
	if got[0].Title != "Build prod" {
		t.Errorf("title = %q, want 'Build prod'", got[0].Title)
	}
	// needs merged into depends_on.
	if !reflect.DeepEqual(got[0].DependsOn, []string{"s0"}) {
		t.Errorf("s1 DependsOn = %v, want [s0]", got[0].DependsOn)
	}
}

func TestExpandStepsLoop(t *testing.T) {
	steps := []Step{
		{
			ID:    "shard-{{i}}",
			Title: "shard {{i}}",
			Loop:  &Loop{Var: "i", Count: 3},
		},
	}
	got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d steps, want 3", len(got))
	}
	for i, es := range got {
		wantID := "shard-" + itoa(i)
		if es.ID != wantID {
			t.Errorf("step %d ID = %q, want %q", i, es.ID, wantID)
		}
		if es.Title != "shard "+itoa(i) {
			t.Errorf("step %d title = %q", i, es.Title)
		}
	}
}

func TestExpandStepsLoopDependsSubstituted(t *testing.T) {
	steps := []Step{
		{
			ID:        "job-{{i}}",
			Title:     "job {{i}}",
			DependsOn: []string{"setup-{{i}}"},
			Loop:      &Loop{Var: "i", Values: []string{"a", "b"}},
		},
	}
	got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got[0].DependsOn[0] != "setup-a" || got[1].DependsOn[0] != "setup-b" {
		t.Errorf("loop var not substituted into depends_on: %v / %v", got[0].DependsOn, got[1].DependsOn)
	}
}

func TestExpandStepsCondition(t *testing.T) {
	steps := []Step{
		{ID: "always", Title: "A"},
		{ID: "maybe", Title: "M", Condition: "{{run}}", DependsOn: []string{"always"}},
		{ID: "after", Title: "F", DependsOn: []string{"maybe"}},
	}

	t.Run("condition false skips and drops dangling dep", func(t *testing.T) {
		got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{"run": ""})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		if !reflect.DeepEqual(ids, []string{"always", "after"}) {
			t.Errorf("ids = %v, want [always after] (maybe skipped)", ids)
		}
		// "after" depended on the skipped "maybe" — that dangling dep is dropped.
		for _, s := range got {
			if s.ID == "after" && len(s.DependsOn) != 0 {
				t.Errorf("after DependsOn = %v, want [] (dangling dropped)", s.DependsOn)
			}
		}
	})

	t.Run("condition true keeps step", func(t *testing.T) {
		got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{"run": "yes"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("got %d steps, want 3", len(got))
		}
	})
}

func TestExpandStepsChildren(t *testing.T) {
	steps := []Step{
		{
			ID:    "epic",
			Title: "Epic {{env}}",
			Children: []Step{
				{ID: "child1", Title: "c1 {{env}}"},
				{ID: "child2", Title: "c2", Condition: "!{{env}}"},
			},
		},
	}
	got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{"env": "prod"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d top steps, want 1", len(got))
	}
	if got[0].Title != "Epic prod" {
		t.Errorf("epic title = %q", got[0].Title)
	}
	// child2's condition !{{env}} is false (env non-empty) => skipped.
	if len(got[0].Children) != 1 || got[0].Children[0].ID != "child1" {
		t.Errorf("children = %+v, want just child1", got[0].Children)
	}
	if got[0].Children[0].Title != "c1 prod" {
		t.Errorf("child title = %q", got[0].Children[0].Title)
	}
}

func TestExpandStepsLoopWithChildren(t *testing.T) {
	// A loop step that ALSO has children: each iteration's children must be
	// expanded with the loop variable bound, so their IDs/deps resolve per
	// iteration instead of keeping the literal {{i}} (ka-0000004104). Duplicate
	// child IDs would otherwise collide on stepIdempotencyKey and silently drop
	// every iteration but the first at pour time.
	steps := []Step{
		{
			ID:    "shard-{{i}}",
			Title: "shard {{i}}",
			Loop:  &Loop{Var: "i", Count: 2},
			Children: []Step{
				{ID: "build-{{i}}", Title: "build {{i}}"},
				{ID: "test-{{i}}", Title: "test {{i}}", DependsOn: []string{"build-{{i}}"}},
			},
		},
	}
	got, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d loop iterations, want 2", len(got))
	}

	// Collect every child ID to assert none collide across iterations.
	seen := map[string]bool{}
	for i, es := range got {
		wantParent := "shard-" + itoa(i)
		if es.ID != wantParent {
			t.Errorf("iteration %d parent ID = %q, want %q", i, es.ID, wantParent)
		}
		if len(es.Children) != 2 {
			t.Fatalf("iteration %d: got %d children, want 2", i, len(es.Children))
		}
		wantBuild := "build-" + itoa(i)
		wantTest := "test-" + itoa(i)
		if es.Children[0].ID != wantBuild {
			t.Errorf("iteration %d child[0] ID = %q, want %q (loop var not bound)", i, es.Children[0].ID, wantBuild)
		}
		if es.Children[0].Title != "build "+itoa(i) {
			t.Errorf("iteration %d child[0] title = %q", i, es.Children[0].Title)
		}
		if es.Children[1].ID != wantTest {
			t.Errorf("iteration %d child[1] ID = %q, want %q", i, es.Children[1].ID, wantTest)
		}
		// depends_on inside the child must substitute the loop var too, so the
		// blocks edge wires to THIS iteration's build step, not iteration 0's.
		if len(es.Children[1].DependsOn) != 1 || es.Children[1].DependsOn[0] != wantBuild {
			t.Errorf("iteration %d child[1] DependsOn = %v, want [%s]", i, es.Children[1].DependsOn, wantBuild)
		}
		for _, c := range es.Children {
			if seen[c.ID] {
				t.Errorf("duplicate child ID %q across iterations — would collide on stepIdempotencyKey", c.ID)
			}
			seen[c.ID] = true
		}
	}
}

func TestExpandStepsInlineExpansion(t *testing.T) {
	expansion := mustFields(t, "gd-exp", Fields{
		FormulaType: "expansion",
		Steps:       []Step{{ID: "inner", Title: "inner {{env}}"}},
	})
	g := mapGetter{"gd-exp": expansion}
	steps := []Step{{ID: "e", Expand: "gd-exp"}}
	got, err := ExpandSteps(context.Background(), g, steps, map[string]string{"env": "prod"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 || got[0].ID != "e/inner" || got[0].Title != "inner prod" {
		t.Errorf("got %+v, want e/inner 'inner prod'", got)
	}
}

func TestExpandStepsBranch(t *testing.T) {
	expDev := mustFields(t, "gd-dev", Fields{
		FormulaType: "expansion",
		Steps:       []Step{{ID: "d", Title: "dev path"}},
	})
	g := mapGetter{"gd-dev": expDev}
	steps := []Step{
		{
			ID: "b",
			Branch: &Branch{
				On:    "{{env}}",
				Cases: map[string]BranchCase{"dev": {Expand: "gd-dev"}},
			},
		},
	}
	got, err := ExpandSteps(context.Background(), g, steps, map[string]string{"env": "dev"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 || got[0].Title != "dev path" {
		t.Errorf("got %+v, want dev path", got)
	}
}

func TestExpandStepsBranchError(t *testing.T) {
	steps := []Step{
		{ID: "b", Branch: &Branch{On: "{{env}}", Cases: map[string]BranchCase{}}},
	}
	_, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{"env": "x"})
	if err == nil || !strings.Contains(err.Error(), "branching step") {
		t.Fatalf("expected branching-step error, got %v", err)
	}
}

func TestExpandStepsExpansionError(t *testing.T) {
	steps := []Step{{ID: "e", Expand: "gd-missing"}}
	_, err := ExpandSteps(context.Background(), mapGetter{}, steps, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "expanding step") {
		t.Fatalf("expected expanding-step error, got %v", err)
	}
}

func TestCountSteps(t *testing.T) {
	tree := []ExpandedStep{
		{Step: Step{ID: "a"}, Children: []ExpandedStep{
			{Step: Step{ID: "a1"}},
			{Step: Step{ID: "a2"}, Children: []ExpandedStep{
				{Step: Step{ID: "a2i"}},
			}},
		}},
		{Step: Step{ID: "b"}},
	}
	if got := CountSteps(tree); got != 5 {
		t.Errorf("CountSteps = %d, want 5", got)
	}
	if got := CountSteps(nil); got != 0 {
		t.Errorf("CountSteps(nil) = %d, want 0", got)
	}
}

// itoa is a tiny local int->string to avoid importing strconv in the test just
// for loop-index labels.
func itoa(i int) string {
	return string(rune('0' + i))
}

func TestValidateDependencies(t *testing.T) {
	t.Run("all resolvable passes", func(t *testing.T) {
		tree := []ExpandedStep{
			{Step: Step{ID: "a"}},
			{Step: Step{ID: "b", DependsOn: []string{"a"}}},
			{Step: Step{ID: "c", DependsOn: []string{"a", "b"}}},
		}
		if err := ValidateDependencies(tree); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("nested child dep resolves across levels", func(t *testing.T) {
		tree := []ExpandedStep{
			{Step: Step{ID: "setup"}},
			{Step: Step{ID: "parent"}, Children: []ExpandedStep{
				// child depends on a top-level sibling — must resolve.
				{Step: Step{ID: "impl", DependsOn: []string{"setup"}}},
			}},
		}
		if err := ValidateDependencies(tree); err != nil {
			t.Fatalf("expected nil for cross-level dep, got %v", err)
		}
	})

	t.Run("dangling top-level dep fails loudly", func(t *testing.T) {
		tree := []ExpandedStep{
			{Step: Step{ID: "a"}},
			// gate depends_on a step that does not exist — the ka-0000002771 bug.
			{Step: Step{ID: "gate", DependsOn: []string{"nonexistent-critic"}}},
		}
		err := ValidateDependencies(tree)
		if err == nil {
			t.Fatal("expected error for dangling depends_on, got nil")
		}
		if !strings.Contains(err.Error(), "gate") || !strings.Contains(err.Error(), "nonexistent-critic") {
			t.Errorf("error should name the offending step and ref, got: %v", err)
		}
		if !strings.Contains(err.Error(), "ungated molecule") {
			t.Errorf("error should explain the ungated-molecule impact, got: %v", err)
		}
	})

	t.Run("dangling dep inside a child fails loudly", func(t *testing.T) {
		tree := []ExpandedStep{
			{Step: Step{ID: "root"}, Children: []ExpandedStep{
				{Step: Step{ID: "child", DependsOn: []string{"ghost"}}},
			}},
		}
		err := ValidateDependencies(tree)
		if err == nil || !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("expected error naming ghost dep, got %v", err)
		}
	})

	t.Run("empty tree passes", func(t *testing.T) {
		if err := ValidateDependencies(nil); err != nil {
			t.Fatalf("expected nil for empty tree, got %v", err)
		}
	})

	t.Run("multiple dangling refs all reported", func(t *testing.T) {
		tree := []ExpandedStep{
			{Step: Step{ID: "a", DependsOn: []string{"x"}}},
			{Step: Step{ID: "b", DependsOn: []string{"y"}}},
		}
		err := ValidateDependencies(tree)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "x") || !strings.Contains(err.Error(), "y") {
			t.Errorf("both dangling refs should be reported, got: %v", err)
		}
	})
}
