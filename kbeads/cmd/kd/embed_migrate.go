package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var embedMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate bead_embeddings to a new embedding model / dimensionality",
	Long: `Destructive: drops the ANN index, truncates bead_embeddings, alters the
embedding column to vector(--dims), runs ` + "`embed sync --force`" + ` to
repopulate every bead under the new model, then re-creates the ANN index.

Existing embedding vectors are NOT preserved — switching models changes
the embedding space, so old vectors would be meaningless in the new space.

Requires:
  - BEADS_EMBED_BACKEND set to a non-noop backend on the SERVER, configured
    for the target model. The migrate endpoint rejects the request when the
    server embedder does not report the same ModelName()/Dimensions() as
    --to-model/--dims — so no destructive DDL runs unless the daemon is
    already wired up to produce the new vectors.
  - Admin token on the kbeads server.
  - The operator safety flag --yes-i-understand-embeddings-are-destroyed.

Flags:
  --to-model                                      target embedding model name
  --dims                                          target vector dimensionality
  --yes-i-understand-embeddings-are-destroyed     required before any DDL runs
  --batch-size                                    passed to the internal backfill
  --note                                          recorded in the audit row
  --fleet                                         passed to the backfill (scope)`,
	RunE: runEmbedMigrate,
}

var (
	embedMigrateToModel   string
	embedMigrateDims      int
	embedMigrateConfirm   bool
	embedMigrateBatchSize int
	embedMigrateNote      string
)

func runEmbedMigrate(cmd *cobra.Command, _ []string) error {
	if embedMigrateToModel == "" {
		return fmt.Errorf("--to-model is required")
	}
	if embedMigrateDims <= 0 {
		return fmt.Errorf("--dims is required and must be > 0")
	}
	if !embedMigrateConfirm {
		return fmt.Errorf("--yes-i-understand-embeddings-are-destroyed is required: " +
			"this command truncates bead_embeddings and drops the ANN index")
	}
	if embedMigrateBatchSize < 1 || embedMigrateBatchSize > 500 {
		return fmt.Errorf("--batch-size must be between 1 and 500")
	}

	// Model/dims validation now happens server-side inside
	// /v1/embeddings/migrate — the daemon is what actually produces the new
	// vectors, so it's the authoritative source of truth for what ModelName
	// and Dimensions the backend reports. The CLI no longer needs a local
	// embedder configured for migrate to run.

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Migrating bead_embeddings to model=%s dims=%d (actor=%s)\n",
		embedMigrateToModel, embedMigrateDims, actor)
	fmt.Fprintln(out, "Step 1/3: dropping ANN index, truncating table, altering column type...")

	if err := beadsClient.MigrateEmbeddings(ctx, &client.MigrateEmbeddingsRequest{
		Model: embedMigrateToModel,
		Dims:  embedMigrateDims,
		Actor: actor,
		Note:  embedMigrateNote,
	}); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}

	fmt.Fprintln(out, "Step 2/3: running embed sync --force against empty table...")
	// Invoke the existing sync command under --force. The caller's batch-size
	// is propagated; other sync flags keep their defaults.
	origBatch := embedSyncBatchSize
	origForce := embedSyncForce
	defer func() {
		embedSyncBatchSize = origBatch
		embedSyncForce = origForce
	}()
	embedSyncBatchSize = embedMigrateBatchSize
	embedSyncForce = true
	if err := runEmbedSync(cmd, nil); err != nil {
		return fmt.Errorf("backfill after migrate: %w", err)
	}

	fmt.Fprintln(out, "Step 3/3: re-creating ANN index on repopulated table...")
	if err := beadsClient.ReindexEmbeddings(ctx); err != nil {
		return fmt.Errorf("reindex: %w", err)
	}

	fmt.Fprintf(out, "Done. bead_embeddings now uses model=%s dims=%d.\n",
		embedMigrateToModel, embedMigrateDims)
	return nil
}
