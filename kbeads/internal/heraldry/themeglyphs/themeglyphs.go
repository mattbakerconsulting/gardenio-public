// Package themeglyphs reads the glyphs the bridge's Garden and Business
// themes use straight out of the controller's theme.go. kbeads cannot import
// the controller, so the heraldry library generator and the heraldry drift
// test both parse the source file instead (spec 2026-10-25-heraldry-design R4).
package themeglyphs

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"

	"github.com/rivo/uniseg"
)

// ThemePath is theme.go relative to the repository root.
const ThemePath = "gardenio/controller/internal/bridge/theme.go"

// ThemeVars are the theme values whose glyphs the library must not hold.
var ThemeVars = []string{"Garden", "Business"}

// reactionEmoji maps the Slack reaction names the themes' ReactionNames
// use to the emoji Slack draws for them. A name missing here is an error, so
// a theme change that adds a reaction has to add it.
var reactionEmoji = map[string]string{
	"seedling":               "🌱",
	"leaves":                 "🍃",
	"deciduous_tree":         "🌳",
	"wilted_flower":          "🥀",
	"fallen_leaf":            "🍂",
	"white_circle":           "⚪",
	"hourglass_flowing_sand": "⏳",
	"white_check_mark":       "✅",
	"x":                      "❌",
	"no_entry_sign":          "🚫",
}

// Key is the comparison form of a glyph: presentation selectors (VS15,
// VS16) dropped, so ⚙ and ⚙️ are the same glyph.
func Key(s string) string {
	return strings.NewReplacer("︎", "", "️", "").Replace(s)
}

// Extract parses the theme.go source and returns every non-ASCII grapheme
// cluster in the string values of the Garden and Business theme literals
// (reaction names resolved to their emoji), deduplicated by Key and sorted.
// Callers decide which of them are emoji.
func Extract(src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "theme.go", src, 0)
	if err != nil {
		return nil, err
	}
	seen := map[string]string{}
	found := map[string]bool{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !isThemeVar(name.Name) || i >= len(vs.Values) {
					continue
				}
				found[name.Name] = true
				if err := collect(vs.Values[i], false, seen); err != nil {
					return nil, fmt.Errorf("%s: %w", name.Name, err)
				}
			}
		}
	}
	for _, v := range ThemeVars {
		if !found[v] {
			return nil, fmt.Errorf("theme var %s not found", v)
		}
	}
	out := make([]string, 0, len(seen))
	for _, g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
}

func isThemeVar(name string) bool {
	for _, v := range ThemeVars {
		if v == name {
			return true
		}
	}
	return false
}

// collect walks a composite literal, adding the glyphs of every string
// value. reactions is true inside the ReactionNames map.
func collect(e ast.Expr, reactions bool, seen map[string]string) error {
	switch e := e.(type) {
	case *ast.CompositeLit:
		for _, elt := range e.Elts {
			if err := collect(elt, reactions, seen); err != nil {
				return err
			}
		}
	case *ast.KeyValueExpr:
		if id, ok := e.Key.(*ast.Ident); ok && id.Name == "ReactionNames" {
			reactions = true
		}
		return collect(e.Value, reactions, seen)
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return err
		}
		if reactions {
			emoji, ok := reactionEmoji[s]
			if !ok {
				return fmt.Errorf("reaction name %q has no emoji in themeglyphs.reactionEmoji", s)
			}
			s = emoji
		}
		gr := uniseg.NewGraphemes(s)
		for gr.Next() {
			g := gr.Str()
			if isASCII(g) {
				continue
			}
			if _, ok := seen[Key(g)]; !ok {
				seen[Key(g)] = g
			}
		}
	}
	return nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
