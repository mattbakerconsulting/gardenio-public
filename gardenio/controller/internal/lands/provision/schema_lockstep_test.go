package provision

import (
	"testing"

	"gardenio/controller/internal/kbeadsschematest"
)

// TestStepsAreKbeadsEnumValues: the provisioner records each step it finishes
// in provision_step and close_step, so every step must be a value kbeads'
// type:plot_application allows. gd-AQ9iCiKxML stalled after channels because
// "namespace" was missing: kbeads refused the record and the step re-ran every
// pass.
func TestStepsAreKbeadsEnumValues(t *testing.T) {
	kbeadsschematest.RequireEnumValues(t, "plot_application", "provision_step", Steps...)
	kbeadsschematest.RequireEnumValues(t, "plot_application", "close_step", CloseSteps...)
}
