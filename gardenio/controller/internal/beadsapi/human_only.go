package beadsapi

import "slices"

// HumanOnlyLabels mark a step that waits on a person, not a crew: no agent
// may take it, so it is never ready work (gd-HM98b6dbAz).
var HumanOnlyLabels = []string{"owner-at-device"}

// HumanOnly reports whether b carries one of HumanOnlyLabels.
func (b *BeadDetail) HumanOnly() bool {
	return b != nil && slices.ContainsFunc(b.Labels, func(l string) bool { return slices.Contains(HumanOnlyLabels, l) })
}
