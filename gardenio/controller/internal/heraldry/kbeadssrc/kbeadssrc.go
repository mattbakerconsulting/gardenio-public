// Package kbeadssrc reads the heraldry library out of kbeads'
// Go source (kbeads/internal/heraldry), which the controller cannot import:
// Go's internal rule keeps that package to kbeads. The renderer's generator
// (../genassets) uses it to write library_gen.go, and the renderer's parity
// test uses it to fail when kbeads moves without a regenerate. It parses the
// source with go/parser; it never runs or links kbeads code.
package kbeadssrc

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
)

// Charge is one library entry, as kbeads' Charge carries it.
type Charge struct {
	Emoji, Name, Codepoints string
	VS16                    bool
}

// Source is what the renderer mirrors from kbeads.
type Source struct {
	NotoEmojiTag string
	Library      []Charge
}

// Read parses library.go in dir (kbeads/internal/heraldry).
func Read(dir string) (Source, error) {
	var src Source
	lib, err := parseFile(filepath.Join(dir, "library.go"))
	if err != nil {
		return src, err
	}
	if src.NotoEmojiTag, err = constString(lib, "NotoEmojiTag"); err != nil {
		return src, err
	}
	err = eachElement(lib, "library", func(f map[string]ast.Expr) error {
		c := Charge{}
		var err error
		if c.Emoji, err = str(f["Emoji"]); err != nil {
			return err
		}
		if c.Name, err = str(f["Name"]); err != nil {
			return err
		}
		if c.Codepoints, err = str(f["Codepoints"]); err != nil {
			return err
		}
		id, ok := f["VS16"].(*ast.Ident)
		if !ok || (id.Name != "true" && id.Name != "false") {
			return fmt.Errorf("charge %s: VS16 is not a bool literal", c.Emoji)
		}
		c.VS16 = id.Name == "true"
		src.Library = append(src.Library, c)
		return nil
	})
	if err != nil {
		return src, err
	}
	if len(src.Library) == 0 {
		return src, fmt.Errorf("kbeadssrc: %s holds no charges", dir)
	}
	return src, nil
}

func parseFile(path string) (*ast.File, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("kbeadssrc: %w", err)
	}
	return f, nil
}

// constString is the value of a top-level string constant.
func constString(f *ast.File, name string) (string, error) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name == name && i < len(vs.Values) {
					return str(vs.Values[i])
				}
			}
		}
	}
	return "", fmt.Errorf("kbeadssrc: no constant %s", name)
}

// eachElement calls fn with the keyed fields of every element of the
// top-level slice literal var name.
func eachElement(f *ast.File, name string, fn func(map[string]ast.Expr) error) error {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					return fmt.Errorf("kbeadssrc: %s is not a literal", name)
				}
				for _, el := range lit.Elts {
					cl, ok := el.(*ast.CompositeLit)
					if !ok {
						return fmt.Errorf("kbeadssrc: an element of %s is not a literal", name)
					}
					fields := map[string]ast.Expr{}
					for _, e := range cl.Elts {
						kv, ok := e.(*ast.KeyValueExpr)
						if !ok {
							return fmt.Errorf("kbeadssrc: an element of %s has unkeyed fields", name)
						}
						if k, ok := kv.Key.(*ast.Ident); ok {
							fields[k.Name] = kv.Value
						}
					}
					if err := fn(fields); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}
	return fmt.Errorf("kbeadssrc: no var %s", name)
}

func str(e ast.Expr) (string, error) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", fmt.Errorf("kbeadssrc: expected a string literal")
	}
	return strconv.Unquote(lit.Value)
}
