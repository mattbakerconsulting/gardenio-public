package deathreport

// Classify is ClassifyWithFields with no bead fields: the verdict from the
// close_reason alone.
func Classify(closeReason string) Classification {
	return ClassifyWithFields(closeReason, nil)
}
