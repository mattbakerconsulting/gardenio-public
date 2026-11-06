package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gardenio/kbeads/internal/model"
)

// DefaultUndoRetentionDays is the maximum age of a mutation that may be undone.
// Mirrors the soft-delete retention window from step 8b — once a mutation is
// older than this it falls out of the supported reversal range.
const DefaultUndoRetentionDays = 30

// ErrUndoOutOfWindow is returned when the targeted mutation is older than the
// retention window. The CLI surfaces this as a refusal.
var ErrUndoOutOfWindow = errors.New("mutation is outside the undo retention window")

// ErrUndoConflict is returned when the bead's current value for the field has
// drifted from the mutation's recorded new_value (i.e. another change has
// landed since the mutation was recorded). We refuse rather than silently
// clobbering newer state.
var ErrUndoConflict = errors.New("field has changed since this mutation; refusing to undo")

// ErrUndoUnsupportedField is returned when the mutation targets a field that
// the undo path does not yet know how to reverse (e.g. labels, fields
// patches). Surfaced as a 400 to the caller.
var ErrUndoUnsupportedField = errors.New("undo for this field is not supported")

// ErrUndoUnsupportedMutation is returned for mutation types other than
// "update". Create/delete/restore reversals already have dedicated
// affordances (delete the bead, kd restore <id>) and trying to undo them
// through this path would conflict with those primitives.
var ErrUndoUnsupportedMutation = errors.New("only update mutations may be undone via this endpoint")

// undoMutation reverses one mutation_history row by setting the targeted
// bead field back to the mutation's recorded old_value. Returns the resulting
// bead and the just-recorded reverse-mutation row so the caller can surface
// both to the operator.
//
// Guarantees:
//   - Only update mutations are supported (ErrUndoUnsupportedMutation).
//   - Mutations older than retentionDays are refused (ErrUndoOutOfWindow).
//   - The bead's current value MUST equal the mutation's new_value
//     (ErrUndoConflict). This blocks lossy "undo over newer state" moves.
//   - The reversal is itself recorded as an update mutation, so the audit
//     trail shows both the original change and its undo.
func (s *BeadsServer) undoMutation(ctx context.Context, mutationID int64, retentionDays int) (*model.Bead, *model.MutationRow, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultUndoRetentionDays
	}

	mut, err := s.store.GetMutation(ctx, mutationID)
	if err != nil {
		return nil, nil, err
	}

	if mut.MutationType != model.MutationTypeUpdate {
		return nil, mut, fmt.Errorf("%w (type=%s)", ErrUndoUnsupportedMutation, mut.MutationType)
	}

	// A scrub (docs/adr/taints.md §6.5, gd-fVAy32nWDf) destroys content on
	// purpose — "undo" would mean restoring what the scrub was specifically
	// asked to destroy, so every mutation ScrubTaint writes is refused here
	// regardless of mutation type, identified by its sentinel actor.
	if mut.Actor == model.SystemActorTaintScrub {
		return nil, mut, fmt.Errorf("%w (actor=%s)", ErrUndoUnsupportedMutation, mut.Actor)
	}

	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	if !mut.CreatedAt.IsZero() && mut.CreatedAt.Before(cutoff) {
		return nil, mut, fmt.Errorf("%w (created_at=%s, cutoff=%s)", ErrUndoOutOfWindow,
			mut.CreatedAt.Format(time.RFC3339), cutoff.Format(time.RFC3339))
	}

	bead, err := s.store.GetBead(ctx, mut.BeadID)
	if err != nil {
		return nil, mut, err
	}
	if bead == nil {
		return nil, mut, sql.ErrNoRows
	}

	currentVal, ok := currentFieldValue(bead, mut.Field)
	if !ok {
		return nil, mut, fmt.Errorf("%w (field=%q)", ErrUndoUnsupportedField, mut.Field)
	}
	if currentVal != mut.NewValue {
		return nil, mut, fmt.Errorf("%w (field=%q current=%q expected=%q)",
			ErrUndoConflict, mut.Field, currentVal, mut.NewValue)
	}

	in, err := buildUndoUpdate(mut.Field, mut.OldValue)
	if err != nil {
		return nil, mut, err
	}

	updated, err := s.updateBead(ctx, mut.BeadID, in)
	if err != nil {
		return nil, mut, err
	}

	return updated, mut, nil
}

// currentFieldValue returns the bead's current value for a top-level mutation
// field, formatted via fmtValue so it can be compared against the
// mutation_history row's new_value (which was recorded the same way). The
// boolean return signals whether the field is one we know how to reverse;
// unsupported fields (labels, fields.<key>, etc.) report false so the caller
// can emit ErrUndoUnsupportedField.
func currentFieldValue(bead *model.Bead, field string) (string, bool) {
	switch field {
	case "title":
		return bead.Title, true
	case "short_title":
		return bead.ShortTitle, true
	case "heraldry":
		return bead.Heraldry, true
	case "description":
		return bead.Description, true
	case "notes":
		return bead.Notes, true
	case "status":
		return string(bead.Status), true
	case "priority":
		return fmtValue(bead.Priority), true
	case "assignee":
		return bead.Assignee, true
	case "owner":
		return bead.Owner, true
	}
	return "", false
}

// buildUndoUpdate translates a (field, old_value) pair into an
// updateBeadInput patch. Only the fields enumerated in currentFieldValue are
// reversible here; everything else returns ErrUndoUnsupportedField.
func buildUndoUpdate(field, oldVal string) (updateBeadInput, error) {
	in := updateBeadInput{}
	switch field {
	case "title":
		v := oldVal
		in.Title = &v
	case "short_title":
		v := oldVal
		in.ShortTitle = &v
	case "heraldry":
		// "" (the bead bore no arms) removes them; restored arms go back
		// through the register, so arms another bead took since are a 409.
		v := oldVal
		in.Heraldry = &v
	case "description":
		v := oldVal
		in.Description = &v
	case "notes":
		v := oldVal
		in.Notes = &v
	case "status":
		v := oldVal
		in.Status = &v
	case "priority":
		n, err := strconv.Atoi(oldVal)
		if err != nil {
			return in, fmt.Errorf("undo priority: parsing %q: %w", oldVal, err)
		}
		in.Priority = &n
	case "assignee":
		v := oldVal
		in.Assignee = &v
	case "owner":
		v := oldVal
		in.Owner = &v
	default:
		return in, fmt.Errorf("%w (field=%q)", ErrUndoUnsupportedField, field)
	}
	return in, nil
}
