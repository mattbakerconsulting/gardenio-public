package mailproto

import "slices"

// IsInterrupt mirrors the bridge mail watcher's rule: the delivery:interrupt
// label, or priority 0/1.
func IsInterrupt(labels []string, priority int) bool {
	return slices.Contains(labels, LabelDeliveryInterrupt) || priority <= 1
}
