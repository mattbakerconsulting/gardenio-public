package gardenhub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The escalation kind vocabulary (web/esckind.js, homestead-world-design.md 28.11; gd-ftQFRlBDZI) is pure, so its contract
// runs under node (testdata/esckind_test.js): every family's noun, an unknown family read as other, the state words, who an
// escalation waits on, the since span, the accessible name, and the Escalations cell's order.
//
// Its family table must be the Go one's: farm_escalations.go names a family the page then has no noun or sprite for, or
// esckind.js names one the farm never sends. A unit test on either side alone cannot see the other file, so this one reads
// both: the Esc* constants in farm_model.go (parsed, so a constant no table uses yet still counts) against ESCKIND.FAMILIES.
func TestEscKindUnderNode(t *testing.T) {
	r := nodeScript(t, "esckind_test.js")
	var js []string
	for _, f := range r["families"].([]any) {
		js = append(js, f.(string))
	}
	goFams := escFamilyConsts(t)
	slices.Sort(js)
	if !slices.Equal(js, goFams) {
		t.Errorf("web/esckind.js families %v, the Go family table's (farm_model.go Esc*) %v: add the family to both", js, goFams)
	}
}

// Every family the Go table maps an operation type to is one of the Esc* constants, so the constants are the whole set.
func TestEscalationFamiliesAreConsts(t *testing.T) {
	consts := escFamilyConsts(t)
	for op, fam := range escalationFamilies {
		if !slices.Contains(consts, fam) {
			t.Errorf("escalationFamilies[%q] = %q, not an Esc* constant", op, fam)
		}
	}
	for _, p := range escalationPrefixes {
		if !slices.Contains(consts, p.family) {
			t.Errorf("escalationPrefixes %q -> %q, not an Esc* constant", p.prefix, p.family)
		}
	}
}

// escFamilyConsts is the sorted values of farm_model.go's Esc* string constants.
func escFamilyConsts(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "farm_model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Esc") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					out = append(out, v)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("farm_model.go has no Esc* family constants")
	}
	slices.Sort(out)
	return out
}
