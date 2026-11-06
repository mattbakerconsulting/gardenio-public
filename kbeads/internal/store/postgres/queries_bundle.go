package postgres

import (
	"context"
	"fmt"

	"github.com/lib/pq"

	"gardenio/kbeads/internal/model"
)

// queryHasBundleChild reports whether parentID has at least one child of
// type=bundle linked via a parent-child reference. Backed by
// idx_bead_references_target_type (target_bead_id, relationship_type) — see
// migration 013_bead_references_relationship_type.
//
// Accepts the surface verbs "parent" (forward) and "child-of" (inverse),
// both of which encode "source is a child of target". The legacy
// canonical DependencyType "parent-child" is also matched so rows written
// before the M0 auto-link fix (ka-0000006126) continue to read correctly
// until they are re-extracted.
func queryHasBundleChild(ctx context.Context, db executor, parentID string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1
			FROM bead_references br
			JOIN beads b ON b.id = br.source_bead_id
			WHERE br.target_bead_id = $1
			  AND br.relationship_type = ANY($2::text[])
			  AND b.type = $3
			  AND b.deleted_at IS NULL
		)`,
		parentID,
		pq.Array([]string{"parent", "child-of", string(model.DepParentChild)}),
		"bundle",
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check bundle child: %w", err)
	}
	return exists, nil
}
