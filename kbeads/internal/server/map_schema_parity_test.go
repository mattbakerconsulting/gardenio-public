package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/model"
)

// TestMapSchemaParity is the Lands contract's parity test (contract design
// gd-EWSKPH7pa5 §3 and §8 step 5, D.7 gd-WV1InzSogg): every bead key the map
// (internal/landmap, the Lands API served next to the daemon) projects, read
// off its per-type field table (landmap.TypeRows, with landmap.FieldKinds for
// each fields.<key>), must still exist in the live bead schema with the type
// and enum values the map reads it as. A kbeads change that renames, retypes
// or removes one fails here, at review time, instead of turning into an empty
// or wrong tenant response with a 200.
//
// The design asked for the bridge_schema_parity_test.go pattern, parsing the
// map's source, because the map was to be a separate land-pod module. Under
// edge-route (gd-BTHHmA2rYc) it is a kbeads package, so the table is imported;
// the source parse is kept for keys the daemon stamps outside the schema
// (FieldKind.Stamped), which no schema declares.
//
// Parity proves existence, not meaning: a rename that keeps the old field
// passes. The scenario fixtures replayed in conformance catch that class.
func TestMapSchemaParity(t *testing.T) {
	kinds := map[string]landmap.FieldKind{}
	for _, k := range landmap.FieldKinds {
		id := string(k.Type) + " fields." + k.Key
		if _, dup := kinds[id]; dup {
			t.Errorf("landmap.FieldKinds has two rows for %s", id)
		}
		kinds[id] = k
	}

	beadKeys := jsonKeysOf(reflect.TypeOf(model.Bead{}))
	writeKeys := jsonKeysOf(reflect.TypeOf(createBeadInput{}), reflect.TypeOf(updateBeadInput{}))
	used := map[string]bool{}
	for _, row := range landmap.TypeRows {
		if row.Noun == "" {
			continue
		}
		tc := parseBuiltinSchema(t, string(row.Type))
		check := func(key string, top map[string]bool, how string) {
			fk, isField := strings.CutPrefix(key, "fields.")
			if !isField {
				if !top[key] {
					t.Errorf("landmap %s %s %q, which the daemon's bead no longer has (renamed or removed?)", how, row.Type, key)
				}
				return
			}
			id := string(row.Type) + " " + key
			k, ok := kinds[id]
			if !ok {
				t.Errorf("landmap %s %s, which has no landmap.FieldKinds row to say what type it is", how, id)
				return
			}
			used[id] = true
			fd := fieldByName(tc, fk)
			if fd == nil {
				if k.Stamped == "" {
					t.Errorf("landmap %s %s, which builtinConfigs[\"type:%s\"] (config.go) no longer declares (renamed or removed?)", how, id, row.Type)
					return
				}
				checkStamped(t, k)
				return
			}
			if fd.Type != k.Kind {
				t.Errorf("landmap reads %s as %s; builtinConfigs[\"type:%s\"] declares it %s", id, k.Kind, row.Type, fd.Type)
				return
			}
			if k.Kind == model.FieldTypeEnum || k.Kind == model.FieldTypeEnums {
				want, got := slices.Sorted(slices.Values(k.Values)), slices.Sorted(slices.Values(fd.Values))
				if !slices.Equal(want, got) {
					t.Errorf("landmap maps %s values %v; builtinConfigs[\"type:%s\"] allows %v", id, want, row.Type, got)
				}
			}
		}
		for _, key := range row.Fields {
			check(key, beadKeys, "reads")
		}
		for _, key := range row.Writes {
			check(key, writeKeys, "writes")
		}
	}
	for _, k := range landmap.FieldKinds {
		if id := string(k.Type) + " fields." + k.Key; !used[id] {
			t.Errorf("landmap.FieldKinds has %s, which no visible TypeRow reads or writes", id)
		}
	}

	// A field the schema requires is one every create the map makes must carry.
	for _, v := range landmap.Verbs {
		if v.Action != "create" || v.Deferred != "" {
			continue
		}
		for _, typ := range v.Types {
			row, _ := landmap.RowFor(typ)
			for _, fd := range parseBuiltinSchema(t, string(typ)).Fields {
				if fd.Required && !slices.Contains(row.Writes, "fields."+fd.Name) {
					t.Errorf("builtinConfigs[\"type:%s\"] requires %q, which landmap's %s does not write: every create 400s", typ, fd.Name, v.Op)
				}
			}
		}
	}
}

// checkStamped reads the daemon source a FieldKind names and finds the key
// written as a map literal entry, `"<key>": <value>`, with a value of the kind
// the map reads.
func checkStamped(t *testing.T, k landmap.FieldKind) {
	t.Helper()
	path := filepath.Join(findRepoRoot(t), "kbeads", k.Stamped)
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Errorf("landmap reads %s fields.%s as stamped by %s: %v", k.Type, k.Key, k.Stamped, err)
		return
	}
	var kinds []model.FieldType
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, _ := strconv.Unquote(lit.Value); s == k.Key {
				kinds = append(kinds, literalKind(kv.Value))
			}
		}
		return true
	})
	switch {
	case len(kinds) == 0:
		t.Errorf("landmap reads %s fields.%s as stamped by %s, which no longer writes %q (renamed or removed?)", k.Type, k.Key, k.Stamped, k.Key)
	case !slices.Contains(kinds, k.Kind) && !slices.Contains(kinds, ""):
		t.Errorf("landmap reads %s fields.%s as %s; %s stamps it as %v", k.Type, k.Key, k.Kind, k.Stamped, kinds)
	}
}

// literalKind is the schema type of a literal stamp value, or "" when the
// value is not a literal.
func literalKind(e ast.Expr) model.FieldType {
	switch v := e.(type) {
	case *ast.BasicLit:
		switch v.Kind {
		case token.STRING:
			return model.FieldTypeString
		case token.INT:
			return model.FieldTypeInteger
		case token.FLOAT:
			return model.FieldTypeFloat
		}
	case *ast.Ident:
		if v.Name == "true" || v.Name == "false" {
			return model.FieldTypeBoolean
		}
	}
	return ""
}

// jsonKeysOf is the top-level JSON keys of the given structs.
func jsonKeysOf(types ...reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			if k, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ","); k != "" && k != "-" {
				keys[k] = true
			}
		}
	}
	return keys
}
