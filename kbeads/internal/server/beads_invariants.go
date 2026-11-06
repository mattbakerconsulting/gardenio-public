package server

import (
	"strings"

	"gardenio/kbeads/internal/model"
)

// checkInProgressInvariant enforces that a kind=issue bead transitioning to
// in_progress must have a non-empty assignee. Transition rule, not a state
// invariant: unrelated edits on historically-broken rows still succeed.
// Returns inputError on violation. See ka-0000006063.
func checkInProgressInvariant(newStatus *string, bead *model.Bead) error {
	if newStatus == nil || model.Status(*newStatus) != model.StatusInProgress {
		return nil
	}
	if bead.Kind != model.KindIssue {
		return nil
	}
	if strings.TrimSpace(bead.Assignee) == "" {
		return inputError("cannot transition to in_progress without an assignee (kind=issue; see ka-0000006063)")
	}
	return nil
}
