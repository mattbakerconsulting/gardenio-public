package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"gardenio/kbeads/internal/model"
)

// Field writes that would leave a bead with keys colliding under case folding
// are refused (model/field_key_case.go, gd-tDMK1PuKzT). Create and the merged
// PATCH are checked by ValidateTypeFields[ForUpdate]; updateBead also checks
// the patch before its field guards run, and tokenAuthorizedForBead checks the
// per-field endpoints (increment, append, reorder, edit).

// fieldKeyWrite stands in for the value a field endpoint is about to write. It
// never equals a decoded JSON value, so model.FieldKeyCaseErrors treats every
// written key as changed and grandfathers none of them.
type fieldKeyWrite struct{}

// fieldKeyWrites marks each key as written by a field endpoint.
func fieldKeyWrites(keys []string) map[string]any {
	w := make(map[string]any, len(keys))
	for _, k := range keys {
		w[k] = fieldKeyWrite{}
	}
	return w
}

// fieldKeyCaseError applies write to existing's stored fields with merge-patch
// semantics (a nil value deletes the key) and refuses the result if a written
// key collides under case folding with another key, or case-folds to a
// declared field of the bead's type without matching it. Keys already stored
// and left unchanged are grandfathered.
func (s *BeadsServer) fieldKeyCaseError(ctx context.Context, existing *model.Bead, write map[string]any) error {
	if len(write) == 0 {
		return nil
	}
	var prior map[string]any
	if len(existing.Fields) > 0 {
		_ = json.Unmarshal(existing.Fields, &prior)
	}
	merged := maps.Clone(prior)
	if merged == nil {
		merged = make(map[string]any, len(write))
	}
	for k, v := range write {
		if v == nil {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	var defs []model.FieldDef
	tc, err := s.resolveTypeConfig(ctx, existing.Type)
	if err != nil {
		return fmt.Errorf("resolve type config: %w", err)
	}
	if tc != nil {
		defs = tc.Fields
	}
	if errs := model.FieldKeyCaseErrors(merged, defs, prior); len(errs) > 0 {
		return &model.ValidationError{Errors: errs}
	}
	return nil
}
