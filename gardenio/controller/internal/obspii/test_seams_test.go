package obspii

import "sort"

// IsAllowedLabel reports whether label is a member of the bounded allow-list.
func IsAllowedLabel(label string) bool {
	_, ok := AllowedOperatorLabels[label]
	return ok
}

// IsForbiddenLabel reports whether label is on the explicit PII deny-list.
func IsForbiddenLabel(label string) bool {
	_, ok := ForbiddenOperatorLabels[label]
	return ok
}

// ValidateMetricLabels checks that every label on an operator-keyed series is
// permitted: not on the deny-list, and present on the allow-list. It returns
// the sorted list of offending labels (empty when the series is clean) so the
// CI test can print exactly what to fix. Default-deny: an unknown label is an
// error, not a pass.
func ValidateMetricLabels(labels []string) (forbidden []string) {
	seen := map[string]struct{}{}
	for _, l := range labels {
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		if IsForbiddenLabel(l) || !IsAllowedLabel(l) {
			forbidden = append(forbidden, l)
		}
	}
	sort.Strings(forbidden)
	return forbidden
}
