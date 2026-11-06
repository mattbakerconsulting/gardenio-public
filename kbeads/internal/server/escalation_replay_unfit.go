package server

import "fmt"

// checkReplayUnfitExecutor refuses a replay-unfit operation type that names
// an executor (decision gd-UQTnDZATTU, spec D11, P7 Task 14): a human runs it
// outside gardenio. Filing and an approve-time target override
// (checkEscalationTargets) refuse one; a PATCH cannot add one, because
// execution_target, target_runner and target_owner are D8 filing fields
// (checkEscalationFieldsImmutable); and escalationDispatchFor dispatches none
// for these types whatever the fields say.
func checkReplayUnfitExecutor(f map[string]string) error {
	op := f["operation_type"]
	if isReplayUnfitOperation(op) && (f["execution_target"] != "" || f["target_runner"] != "" || f["target_owner"] != "") {
		return inputError(fmt.Sprintf("operation_type %q may not name an executor: file it without execution_target, target_runner or target_owner and a human runs it outside gardenio (decision gd-UQTnDZATTU)", op))
	}
	return nil
}
