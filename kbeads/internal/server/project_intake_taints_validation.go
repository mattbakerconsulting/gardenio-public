package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/store"
)

// validateProjectIntakeTaints enforces decision gd-H9L64SF3Bk
// (docs/adr/taints.md §10.3): every name in a type:project bead's
// intake_taints field must reference a taint that already exists, checked at
// project write time — HARD rejected (never a silent forward reference), on
// both create and update. This mirrors validateWidgetInstanceFields's shape
// (store-backed, called from beads.go's case "project": alongside the pure
// validateProjectFields/validateProjectFieldsForUpdate checks, which cannot
// do this since they take no ctx/store).
//
// A missing/empty/malformed intake_taints field is not this function's
// concern — the generic FieldDef "json" check on config.go's type:project
// schema already enforces "valid JSON"; an absent field is the common case
// (most projects opt out of intake taints entirely) and is a no-op here.
func validateProjectIntakeTaints(ctx context.Context, st store.Store, fields json.RawMessage) error {
	names := parseIntakeTaints(fields)
	for _, name := range names {
		if _, err := st.GetTaint(ctx, name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("intake_taints references unknown taint %q", name)
			}
			return fmt.Errorf("looking up intake taint %q: %w", name, err)
		}
	}
	return nil
}

// validateProjectIntakeTaintsForUpdate is the UPDATE-path variant, mirroring
// validateProjectFieldsForUpdate's grandfather semantics (firstShapeError):
// re-validated only when the raw intake_taints value actually changed from
// what was already persisted, so a legacy project bead whose intake_taints
// already names a since-deleted taint is not wedged against unrelated-field
// PATCHes.
func validateProjectIntakeTaintsForUpdate(ctx context.Context, st store.Store, fields, prior json.RawMessage) error {
	f := parseFieldsToStringMap(fields)
	if f == nil {
		return nil
	}
	raw := f["intake_taints"]
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if priorMap := parseFieldsToStringMap(prior); priorMap != nil && raw == priorMap["intake_taints"] {
		return nil
	}
	return validateProjectIntakeTaints(ctx, st, fields)
}
