// Package exclusions is the single source of truth for path globs that
// must be dropped from coverage computations.
//
// The Python normalizer at scripts/coverage/normalize-paths.py applies
// the same list at LCOV-merge time (W3, ka-0000007391). The patch-cov
// library (W2b-1) imports this package so the merge-time and gate-time
// decisions cannot drift.
//
// If you add or remove a glob here, mirror the change in
// scripts/coverage/normalize-paths.py DEFAULT_EXCLUDES.
package exclusions

import (
	"path"
	"strings"
)

// DefaultExcludes lists POSIX-style globs for paths to drop from coverage
// (numerator and denominator both shrink). Mirrors
// scripts/coverage/normalize-paths.py DEFAULT_EXCLUDES verbatim.
var DefaultExcludes = []string{
	"**/gen/**",
	"**/*.pb.go",
	"**/proto/**",
	"**/*_sqlc.go",
	"**/sqlc/**",
	"**/dist/**",
	"**/node_modules/**",
	"**/vendor/**",
	"**/target/**",
	"**/*_test.go",
	"**/tests/**",
	"**/fixtures/**",
	"**/testdata/**",
	"**/cmd/*-probe/**",
	"**/*.test.ts",
	"**/*.test.js",
	"**/*.spec.ts",
	"**/*.spec.js",
}

// IsExcluded reports whether p matches any glob in patterns. Matching
// mirrors the Python normalizer (scripts/coverage/normalize-paths.py):
// fnmatch is whole-string but we additionally try every tail-segment of
// the path so "**/tests/**" catches "coop/tests/foo".
func IsExcluded(p string, patterns []string) bool {
	p = strings.TrimPrefix(p, "/")
	parts := strings.Split(p, "/")
	tails := make([]string, 0, len(parts)+1)
	tails = append(tails, p)
	for i := 1; i < len(parts); i++ {
		tails = append(tails, strings.Join(parts[i:], "/"))
	}
	for _, pat := range patterns {
		for _, t := range tails {
			if matchGlob(pat, t) {
				return true
			}
		}
	}
	return false
}

// IsExcludedDefault is shorthand for IsExcluded(p, DefaultExcludes).
func IsExcludedDefault(p string) bool {
	return IsExcluded(p, DefaultExcludes)
}

// matchGlob implements a minimal doublestar matcher: "**" matches zero or
// more path segments, "*" matches anything within a single segment, "?"
// matches a single character. Sufficient for DefaultExcludes shapes
// ("**/dir/**", "**/*.ext", "*.ext", "exact/path").
func matchGlob(pattern, name string) bool {
	patSegs := splitSegs(pattern)
	nameSegs := splitSegs(name)
	return matchSegs(patSegs, nameSegs)
}

func splitSegs(s string) []string {
	s = strings.TrimPrefix(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 && len(name) > 0 {
		if pat[0] == "**" {
			break
		}
		if !segMatch(pat[0], name[0]) {
			return false
		}
		pat = pat[1:]
		name = name[1:]
	}
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] != "**" {
		return false
	}
	rest := pat[1:]
	// "**" at the end matches zero or more remaining segments.
	if len(rest) == 0 {
		return true
	}
	for i := 0; i <= len(name); i++ {
		if matchSegs(rest, name[i:]) {
			return true
		}
	}
	return false
}

func segMatch(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	ok, _ := path.Match(pattern, name)
	return ok
}
