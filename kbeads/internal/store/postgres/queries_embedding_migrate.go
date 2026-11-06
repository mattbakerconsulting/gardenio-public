package postgres

import (
	"context"
	"fmt"
)

// annIndexName is the name of the IVFFlat index created in migration 005 and
// re-created after a model migration. Kept as a constant so the drop/create
// stay in sync.
const annIndexName = "idx_bead_embeddings_ivfflat"

// queryMigrateEmbeddingsSchema performs the destructive embedding-space
// change for `kd embed migrate`:
//  1. drops the ANN index (ALTER on a column with an index is rejected)
//  2. truncates bead_embeddings (vectors in a different space are useless)
//  3. alters the embedding column to vector(dims)
//  4. records an audit row in bead_embedding_migrations
//
// The caller is expected to follow up with a `kd embed sync --force` and
// queryRecreateANNIndex once the table is repopulated.
func queryMigrateEmbeddingsSchema(ctx context.Context, db executor, modelName string, dims int, actor, note string) error {
	if modelName == "" {
		return fmt.Errorf("migrate embeddings: model is required")
	}
	if dims <= 0 {
		return fmt.Errorf("migrate embeddings: dims must be > 0, got %d", dims)
	}

	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+annIndexName); err != nil {
		return fmt.Errorf("drop ann index: %w", err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE TABLE bead_embeddings`); err != nil {
		return fmt.Errorf("truncate bead_embeddings: %w", err)
	}
	// ALTER TABLE interpolates the dimension directly — pgvector does not
	// accept a parameter marker in the type specifier, and `dims` is
	// validated to be a positive integer above.
	alter := fmt.Sprintf(`ALTER TABLE bead_embeddings ALTER COLUMN embedding TYPE vector(%d)`, dims)
	if _, err := db.ExecContext(ctx, alter); err != nil {
		return fmt.Errorf("alter embedding column: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO bead_embedding_migrations (model, dims, actor, note) VALUES ($1, $2, $3, $4)`,
		modelName, dims, actor, note,
	); err != nil {
		return fmt.Errorf("record embedding migration: %w", err)
	}
	return nil
}

// queryRecreateANNIndex re-creates the IVFFlat index on bead_embeddings.
// Called after the post-migration backfill has populated the table — an
// index on an empty table is useless (IVFFlat centroids are meaningless).
func queryRecreateANNIndex(ctx context.Context, db executor) error {
	sql := `CREATE INDEX IF NOT EXISTS ` + annIndexName + ` ON bead_embeddings USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100)`
	if _, err := db.ExecContext(ctx, sql); err != nil {
		return fmt.Errorf("create ann index: %w", err)
	}
	return nil
}
