package formula

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// VarPattern matches {{variable}} placeholders.
var VarPattern = regexp.MustCompile(`\{\{(\w+)\}\}`)

// SubstituteVars replaces {{name}} placeholders with values from vars.
// Unmatched placeholders are left as-is.
func SubstituteVars(s string, vars map[string]string) string {
	return VarPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-2]
		if val, ok := vars[name]; ok {
			return val
		}
		return match
	})
}

// UnresolvedVarValues returns `name="value"` strings for vars whose VALUES
// still contain {{...}} placeholders — a template the caller never resolved.
// Substituting such a value bakes the literal placeholder into every step
// title (ka-0000006473). Sorted for stable error messages.
func UnresolvedVarValues(vars map[string]string) []string {
	var bad []string
	for name, val := range vars {
		if VarPattern.MatchString(val) {
			bad = append(bad, fmt.Sprintf("%s=%q", name, val))
		}
	}
	sort.Strings(bad)
	return bad
}

// EvaluateCondition evaluates a simple condition string against variables.
// Supported formats:
//   - "{{var}}"          — truthy (non-empty)
//   - "!{{var}}"         — falsy (empty)
//   - "{{var}} == value" — equality
//   - "{{var}} != value" — inequality
func EvaluateCondition(cond string, vars map[string]string) bool {
	cond = strings.TrimSpace(cond)

	// Negation: !{{var}}
	if strings.HasPrefix(cond, "!") {
		inner := strings.TrimPrefix(cond, "!")
		return !EvaluateCondition(inner, vars)
	}

	// Equality/inequality: {{var}} == value / {{var}} != value
	for _, op := range []string{"!=", "=="} {
		if parts := strings.SplitN(cond, op, 2); len(parts) == 2 {
			left := SubstituteVars(strings.TrimSpace(parts[0]), vars)
			right := strings.TrimSpace(parts[1])
			if op == "==" {
				return left == right
			}
			return left != right
		}
	}

	// Simple truthy: {{var}} — evaluate to non-empty after substitution.
	resolved := SubstituteVars(cond, vars)
	// If the variable was substituted, check truthiness.
	if resolved != cond {
		return resolved != "" && resolved != "false" && resolved != "0"
	}
	// Unresolved placeholder — treat as falsy.
	if VarPattern.MatchString(cond) {
		return false
	}
	// Literal non-empty string — truthy.
	return resolved != ""
}

// PriorityOrDefault returns the step priority if set, otherwise the default.
func PriorityOrDefault(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

// MergeLabels combines two label slices, deduplicating.
func MergeLabels(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	var result []string
	for _, l := range base {
		if !seen[l] {
			seen[l] = true
			result = append(result, l)
		}
	}
	for _, l := range extra {
		if !seen[l] {
			seen[l] = true
			result = append(result, l)
		}
	}
	return result
}

// ExpandLoopValues returns the iteration values for a step loop.
func ExpandLoopValues(loop *Loop) []string {
	if len(loop.Values) > 0 {
		return loop.Values
	}
	if loop.Count > 0 {
		vals := make([]string, loop.Count)
		for i := 0; i < loop.Count; i++ {
			vals[i] = strconv.Itoa(i)
		}
		return vals
	}
	if loop.Range != "" {
		start, end, err := ParseRange(loop.Range)
		if err != nil {
			return nil // validation should have caught this
		}
		vals := make([]string, end-start+1)
		for i := start; i <= end; i++ {
			vals[i-start] = strconv.Itoa(i)
		}
		return vals
	}
	return nil
}

// MergeNeedsRecursive merges `needs` into `depends_on` for a slice of steps, recursively.
func MergeNeedsRecursive(steps []Step) {
	for i := range steps {
		if len(steps[i].Needs) > 0 {
			seen := make(map[string]bool, len(steps[i].DependsOn))
			for _, d := range steps[i].DependsOn {
				seen[d] = true
			}
			for _, n := range steps[i].Needs {
				if !seen[n] {
					steps[i].DependsOn = append(steps[i].DependsOn, n)
					seen[n] = true
				}
			}
		}
		if len(steps[i].Children) > 0 {
			MergeNeedsRecursive(steps[i].Children)
		}
	}
}

// MatchGlob performs simple glob matching: * matches any sequence of chars.
func MatchGlob(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
		// *foo* — contains
		inner := pattern[1 : len(pattern)-1]
		return strings.Contains(s, inner)
	}
	if strings.HasSuffix(pattern, "*") {
		// prefix*
		return strings.HasPrefix(s, pattern[:len(pattern)-1])
	}
	if strings.HasPrefix(pattern, "*") {
		// *suffix
		return strings.HasSuffix(s, pattern[1:])
	}
	return s == pattern
}

// MapsEqual returns true if two string maps have the same keys and values.
func MapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// MergeVars merges two var definition slices. Later entries override earlier ones with the same name.
func MergeVars(base, overlay []VarDef) []VarDef {
	if len(overlay) == 0 {
		return base
	}
	if len(base) == 0 {
		return overlay
	}
	seen := make(map[string]int, len(base))
	result := make([]VarDef, len(base))
	copy(result, base)
	for i, v := range result {
		seen[v.Name] = i
	}
	for _, v := range overlay {
		if idx, ok := seen[v.Name]; ok {
			result[idx] = v // override
		} else {
			seen[v.Name] = len(result)
			result = append(result, v)
		}
	}
	return result
}

// MergeSteps merges two step slices. Later entries override earlier ones with the same ID.
func MergeSteps(base, overlay []Step) []Step {
	if len(overlay) == 0 {
		return base
	}
	if len(base) == 0 {
		return overlay
	}
	seen := make(map[string]int, len(base))
	result := make([]Step, len(base))
	copy(result, base)
	for i, s := range result {
		seen[s.ID] = i
	}
	for _, s := range overlay {
		if idx, ok := seen[s.ID]; ok {
			result[idx] = s // override
		} else {
			seen[s.ID] = len(result)
			result = append(result, s)
		}
	}
	return result
}
