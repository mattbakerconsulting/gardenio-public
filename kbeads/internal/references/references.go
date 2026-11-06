// Package references extracts bead-id references (ka-0000001713) from text.
//
// A bead reference is the literal prefix "gd-" followed by exactly 10
// characters drawn from the alphabet [A-Za-z0-9], bounded by word
// boundaries on both sides. The pattern matches the IDs minted by
// internal/idgen.
package references

import "regexp"

// Pattern matches a single bead-id reference in text.
var Pattern = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]{10}\b`)

// Extract returns all unique bead-id references found in s, in
// first-seen order. Case is preserved. Returns nil when s contains no
// matches.
func Extract(s string) []string {
	if s == "" {
		return nil
	}
	matches := Pattern.FindAllString(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}
