package bridge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestTaskBeadFieldBanList is a source-level lint that asserts no code path in
// gitlab_sync.go writes the banned MR-derived keys onto a task bead.
//
// The MR bead (type=merge-request) is the sole source of truth for
// mr_state, mr_merged, mr_outcome, gitlab_mr_iid, mr_pipeline_status,
// mr_pipeline_url, and gitlab_project_id. Earlier dual-writes were dropped
// under epic ka-0000001613 / task ka-0000008134 (first batch) and task
// ka-0000000586 (residual cleanup).
//
// The check works by parsing gitlab_sync.go and walking every call to
// daemon.UpdateBeadFields / s.daemon.UpdateBeadFields / p.daemon.UpdateBeadFields
// back to the map literal it was passed and asserting none of the banned
// keys appear. New writes via different shapes (e.g. helper functions) will
// not be caught here — but every site we have today flows through a local
// `fields := map[string]string{...}; daemon.UpdateBeadFields(ctx, bead.ID, fields)`
// idiom, which is what this lint covers.
func TestTaskBeadFieldBanList(t *testing.T) {
	const path = "gitlab_sync.go"
	banned := []string{
		"mr_state",
		"mr_merged",
		"mr_outcome",
		"gitlab_mr_iid",
		"mr_pipeline_status",
		"mr_pipeline_url",
		"gitlab_project_id",
	}

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Index every map[string]string composite literal by its identifier name
	// within each enclosing function. We then walk UpdateBeadFields call
	// sites and look up the most recent assignment to the argument name.
	type litRef struct {
		keys []string
		pos  token.Pos
	}

	check := func(keys []string, where token.Pos) {
		for _, k := range keys {
			for _, b := range banned {
				if k == b {
					t.Errorf("%s: task-bead UpdateBeadFields writes banned key %q (source of truth is the MR bead — see task ka-0000000586 / ka-0000008134)",
						fset.Position(where), k)
				}
			}
		}
	}

	literalKeys := func(cl *ast.CompositeLit) []string {
		var keys []string
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			bl, ok := kv.Key.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				continue
			}
			k, err := strconvUnquote(bl.Value)
			if err != nil {
				continue
			}
			keys = append(keys, k)
		}
		return keys
	}

	isUpdateBeadFieldsCall := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		return sel.Sel.Name == "UpdateBeadFields"
	}

	// Walk each function and track local `name := map[string]string{...}` and
	// `name["k"] = v` assignments preceding each UpdateBeadFields call.
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		// fieldLiterals: variable name -> declared keys
		fieldLiterals := map[string]*litRef{}

		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.AssignStmt:
				// Match `name := map[string]string{...}` and
				// `name = map[string]string{...}`.
				if len(v.Lhs) == 1 && len(v.Rhs) == 1 {
					if id, ok := v.Lhs[0].(*ast.Ident); ok {
						if cl, ok := v.Rhs[0].(*ast.CompositeLit); ok && isStringStringMap(cl.Type) {
							fieldLiterals[id.Name] = &litRef{keys: literalKeys(cl), pos: v.Pos()}
						}
					}
					// Match `name[\"k\"] = v` index assignments and append.
					if ix, ok := v.Lhs[0].(*ast.IndexExpr); ok {
						if id, ok := ix.X.(*ast.Ident); ok {
							if lr, ok := fieldLiterals[id.Name]; ok {
								if bl, ok := ix.Index.(*ast.BasicLit); ok && bl.Kind == token.STRING {
									if k, err := strconvUnquote(bl.Value); err == nil {
										lr.keys = append(lr.keys, k)
									}
								}
							}
						}
					}
				}
			case *ast.CallExpr:
				if !isUpdateBeadFieldsCall(v) {
					return true
				}
				if len(v.Args) < 3 {
					return true
				}
				// The third arg is the fields map. Two shapes:
				//   1) an identifier that names a tracked literal,
				//   2) an inline composite literal.
				switch a := v.Args[2].(type) {
				case *ast.Ident:
					if lr, ok := fieldLiterals[a.Name]; ok {
						check(lr.keys, v.Pos())
					}
				case *ast.CompositeLit:
					check(literalKeys(a), v.Pos())
				}
			}
			return true
		})
		return true
	})
}

// strconvUnquote is a tiny replacement for strconv.Unquote that avoids the
// import (the test file already pulls a number of stdlib packages).
func strconvUnquote(s string) (string, error) {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"') {
		return s[1 : len(s)-1], nil
	}
	return s, nil
}

func isStringStringMap(expr ast.Expr) bool {
	mt, ok := expr.(*ast.MapType)
	if !ok {
		return false
	}
	k, kok := mt.Key.(*ast.Ident)
	v, vok := mt.Value.(*ast.Ident)
	return kok && vok && k.Name == "string" && v.Name == "string"
}
