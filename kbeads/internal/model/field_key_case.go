package model

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Field keys must not collide under case folding (gd-tDMK1PuKzT).
//
// The store keeps `fields` as a JSONB object, so "author" and "Author" are two
// different keys. Go's encoding/json matches struct tags case-insensitively
// (and the last matching key wins), so a guard that reads the exact map key and
// a reader that decodes into a struct can see different values for the same
// bead: a guard checks fields["land_kind"] while the reader picks up a forged
// "Land_kind". Refusing such keys at the write boundary means guards and
// readers can never disagree. Two rules:
//
//   - no two top-level keys may be equal under strings.EqualFold;
//   - a key that case-folds to a schema-declared field must match it exactly.
//
// Only top-level keys are checked. On the update path a key whose value is
// unchanged from what was already stored is grandfathered, so a bead that
// already holds a variant (written before this check) is not wedged against
// PATCHes of unrelated fields. A write that adds or changes either key of a
// colliding pair is refused.

// FieldKeyCaseErrors returns one FieldError per key in m that breaks a rule
// above, sorted by key. defs are the type's declared fields (nil when the type
// has no schema). prior is the bead's field map before this write, or nil on
// create.
func FieldKeyCaseErrors(m map[string]any, defs []FieldDef, prior map[string]any) []FieldError {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	byFold := make(map[string][]string, len(keys))
	for _, k := range keys {
		f := foldFieldKey(k)
		byFold[f] = append(byFold[f], k)
	}
	declared := make(map[string]string, len(defs))
	for _, d := range defs {
		declared[foldFieldKey(d.Name)] = d.Name
	}

	untouched := func(k string) bool {
		if prior == nil {
			return false
		}
		pv, ok := prior[k]
		return ok && fieldValueUnchanged(pv, m[k])
	}

	var errs []FieldError
	for _, k := range keys {
		f := foldFieldKey(k)
		if name, ok := declared[f]; ok && name != k {
			if !untouched(k) {
				errs = append(errs, FieldError{
					Field:   k,
					Message: fmt.Sprintf("case-folds to the declared field %q; use the exact name", name),
				})
			}
			continue
		}
		group := byFold[f]
		if len(group) < 2 {
			continue
		}
		// A pair that was entirely stored before this write, and is unchanged
		// by it, is grandfathered; anything new or changed in the group is not.
		allUntouched := true
		for _, g := range group {
			if !untouched(g) {
				allUntouched = false
				break
			}
		}
		if allUntouched {
			continue
		}
		others := make([]string, 0, len(group)-1)
		for _, g := range group {
			if g != k {
				others = append(others, fmt.Sprintf("%q", g))
			}
		}
		errs = append(errs, FieldError{
			Field:   k,
			Message: "differs only in letter case from " + strings.Join(others, ", ") + "; field keys must not collide under case folding",
		})
	}
	return errs
}

// foldFieldKey maps s to a canonical form such that foldFieldKey(a) ==
// foldFieldKey(b) exactly when strings.EqualFold(a, b): each rune becomes the
// smallest rune in its unicode.SimpleFold orbit. That covers the non-ASCII
// folds encoding/json also applies (the Kelvin sign K folds to k, the long s ſ
// to s).
func foldFieldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		low := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < low {
				low = f
			}
		}
		b.WriteRune(low)
	}
	return b.String()
}
