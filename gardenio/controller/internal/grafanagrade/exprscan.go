package grafanagrade

import (
	"regexp"
	"strings"
)

// This file is a deliberately small PromQL *lexer*, not a parser. The detector
// only needs three structural facts about an expression — where its `or`
// coalescing sites are, which metric names each side references, and whether a
// branch filters on a comparison — and a full parser would drag a PromQL
// dependency into the controller module for no extra signal. Everything here is
// paren/quote-aware string scanning; anything it cannot classify it reports as
// "unknown" so the detector fails open (no finding) rather than guessing.

// orSite is one `<lhs> or <rhs>` coalescing site, at any nesting depth.
type orSite struct {
	LHS string
	RHS string
}

// promFuncs are identifiers that are never metric names. Used to keep function
// and aggregation keywords out of metricNames.
var promFuncs = map[string]bool{
	"sum": true, "min": true, "max": true, "avg": true, "count": true, "group": true,
	"stddev": true, "stdvar": true, "topk": true, "bottomk": true, "quantile": true,
	"count_values": true, "rate": true, "irate": true, "increase": true, "delta": true,
	"idelta": true, "deriv": true, "predict_linear": true, "changes": true, "resets": true,
	"abs": true, "absent": true, "absent_over_time": true, "ceil": true, "floor": true,
	"round": true, "clamp": true, "clamp_min": true, "clamp_max": true, "exp": true,
	"ln": true, "log2": true, "log10": true, "sqrt": true, "scalar": true, "vector": true,
	"time": true, "timestamp": true, "label_replace": true, "label_join": true,
	"histogram_quantile": true, "sort": true, "sort_desc": true, "sgn": true, "lag": true,
	"avg_over_time": true, "min_over_time": true, "max_over_time": true, "sum_over_time": true,
	"count_over_time": true, "quantile_over_time": true, "stddev_over_time": true,
	"stdvar_over_time": true, "last_over_time": true, "present_over_time": true,
	"rollup": true, "rollup_rate": true, "aggr_over_time": true, "union": true,
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true,
	"group_right": true, "and": true, "or": true, "unless": true, "offset": true,
	"bool": true, "inf": true, "nan": true,
}

var identRe = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// stripLiterals blanks out label-matcher bodies, string literals, duration
// brackets and Grafana `$var` interpolations, so identifier scanning only sees
// metric and function names.
func stripLiterals(expr string) string {
	var b strings.Builder
	depth := 0 // inside {...}
	brack := 0 // inside [...]
	var quote rune
	for _, r := range expr {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			b.WriteRune(' ')
		case r == '"' || r == '\'' || r == '`':
			quote = r
			b.WriteRune(' ')
		case r == '{':
			depth++
			b.WriteRune(' ')
		case r == '}':
			if depth > 0 {
				depth--
			}
			b.WriteRune(' ')
		case r == '[':
			brack++
			b.WriteRune(' ')
		case r == ']':
			if brack > 0 {
				brack--
			}
			b.WriteRune(' ')
		case depth > 0 || brack > 0:
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// metricNames returns the metric identifiers an expression references. A name
// immediately followed by `(` is a function call, not a metric; Grafana
// template variables (`$project`) are already blanked by stripLiterals when
// they sit inside a matcher, and are skipped here otherwise.
// groupingClauseRe matches the label list of a grouping or matching modifier.
// Those names are label keys, not metrics, and letting them through would let a
// grouping label masquerade as an independent liveness control.
var groupingClauseRe = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\(([^()]*)\)`)

// blankGroupingLabels blanks the contents of every grouping clause, preserving
// length so byte offsets into the expression stay valid.
func blankGroupingLabels(clean string) string {
	b := []byte(clean)
	for _, m := range groupingClauseRe.FindAllStringSubmatchIndex(clean, -1) {
		for i := m[2]; i < m[3]; i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

func metricNames(expr string) map[string]bool {
	out := map[string]bool{}
	clean := blankGroupingLabels(stripLiterals(expr))
	for _, loc := range identRe.FindAllStringIndex(clean, -1) {
		name := clean[loc[0]:loc[1]]
		if promFuncs[name] {
			continue
		}
		if loc[0] > 0 && clean[loc[0]-1] == '$' {
			continue
		}
		// Skip function calls: identifier directly followed by `(`.
		rest := strings.TrimLeft(clean[loc[1]:], " ")
		if strings.HasPrefix(rest, "(") {
			continue
		}
		out[name] = true
	}
	return out
}

// splitTopLevel splits expr on a binary keyword operator (`or`, `and`,
// `unless`) that appears at paren depth zero and outside any literal.
func splitTopLevel(expr, op string) []string {
	clean := stripLiterals(expr)
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(clean); i++ {
		switch clean[i] {
		case '(':
			depth++
			continue
		case ')':
			depth--
			continue
		}
		if depth != 0 || clean[i] != op[0] {
			continue
		}
		if !strings.HasPrefix(clean[i:], op) {
			continue
		}
		// Require word boundaries so `origin_total` is not read as `or`.
		if i > 0 && isIdentByte(clean[i-1]) {
			continue
		}
		end := i + len(op)
		if end < len(clean) && isIdentByte(clean[end]) {
			continue
		}
		parts = append(parts, expr[start:i])
		start = end
		i = end - 1
	}
	if parts == nil {
		return []string{expr}
	}
	return append(parts, expr[start:])
}

func isIdentByte(b byte) bool {
	return b == '_' || b == ':' || (b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// parenGroups returns the contents of every depth-1 parenthesised group.
func parenGroups(expr string) []string {
	clean := stripLiterals(expr)
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(clean); i++ {
		switch clean[i] {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				out = append(out, expr[start:i])
			}
		}
	}
	return out
}

// orSites returns every coalescing site in expr, at any nesting depth. The LHS
// of a site is everything to the left of that `or` within its own group.
func orSites(expr string) []orSite {
	var out []orSite
	var walk func(string)
	walk = func(s string) {
		if parts := splitTopLevel(s, "or"); len(parts) > 1 {
			for i := 1; i < len(parts); i++ {
				out = append(out, orSite{
					LHS: strings.Join(parts[:i], " or "),
					RHS: parts[i],
				})
			}
		}
		for _, g := range parenGroups(s) {
			walk(g)
		}
	}
	walk(expr)
	return out
}

// gateOperands returns the branches that could carry an independent liveness
// term: the right-hand side of every `or`, plus every operand of a top-level
// `and`/`unless` (the two other ways a Grafana panel gates a reading).
func gateOperands(expr string) []string {
	var out []string
	for _, s := range orSites(expr) {
		out = append(out, s.RHS)
	}
	for _, op := range []string{"and", "unless"} {
		if parts := splitTopLevel(expr, op); len(parts) > 1 {
			out = append(out, parts[1:]...)
		}
	}
	return out
}

var comparisonRe = regexp.MustCompile(`(>=|<=|==|!=|>|<)`)

// balanced reports whether every parenthesis in s closes within s.
func balanced(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// unwrapParens strips redundant outer parentheses.
func unwrapParens(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && balanced(s[1:len(s)-1]) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

var constVectorRe = regexp.MustCompile(`^(on\s*\([^)]*\)\s*)?vector\(\s*-?[0-9.]+\s*\)$`)

// isConstantBranch reports whether a branch is a bare constant — `vector(0)`,
// `on() vector(1)`, or a scalar literal. This is the shape that coalesces an
// empty result into a confident number with nothing gating it.
func isConstantBranch(branch string) bool {
	s := unwrapParens(branch)
	if constVectorRe.MatchString(s) {
		return true
	}
	return regexp.MustCompile(`^-?[0-9.]+$`).MatchString(s)
}
