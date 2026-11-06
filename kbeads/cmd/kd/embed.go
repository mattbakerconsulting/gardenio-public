package main

import (
	"context"
	"fmt"
	"os"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/config"
	"gardenio/kbeads/internal/embedding"
	"github.com/spf13/cobra"
)

// embedderFactory is overridable in tests.
//
// Note: `kd embed sync` no longer calls the embedder directly — embedding is
// now performed server-side by the kbeads daemon (see
// POST /v1/embeddings/sync-batch). The factory is kept so tests that used to
// stub out a local embedder can continue to, and so advanced callers with an
// in-process daemon can still compose the two. The CLI talks to the daemon
// over HTTP and never opens a database connection itself, so this
// deliberately bypasses config.Load() (which requires BEADS_DATABASE_URL)
// and reads only the embedding-specific env vars.
var embedderFactory = func() (embedding.Embedder, error) {
	cfg := &config.Config{
		EmbedBackend: envOrDefault("BEADS_EMBED_BACKEND", "noop"),
		EmbedURL:     envOrDefault("BEADS_EMBED_URL", "http://localhost:11434"),
		EmbedAPIKey:  os.Getenv("BEADS_EMBED_API_KEY"),
		EmbedModel:   os.Getenv("BEADS_EMBED_MODEL"),
	}
	return embedding.NewEmbedder(cfg)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var embedCmd = &cobra.Command{
	Use:     "embed",
	Short:   "Manage bead embeddings",
	GroupID: "system",
}

var embedSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Backfill or refresh bead embeddings",
	Long: `Drive a resumable server-side backfill of bead embeddings. Each iteration
calls POST /v1/embeddings/sync-batch on the daemon: the server lists one
page of beads, skips any whose canonical text_hash is unchanged, embeds the
rest with its configured backend, and upserts the vectors. The CLI
advances ` + "`offset`" + ` by the returned page_count and stops when the page
is smaller than ` + "`--batch-size`" + `.

The embedding API call originates from the daemon, so the CLI no longer
needs BEADS_EMBED_* env on its host (important when the embedder is behind
an IP whitelist, e.g. LiteLLM reached from CI runners).

Because the text_hash skip makes every page idempotent, the loop is
naturally resumable — if interrupted, just re-run: unchanged beads are
skipped and work continues where it stopped.

Requires:
  - BEADS_EMBED_BACKEND set to a non-noop backend on the SERVER
  - Admin token on the kbeads server (fleet tokens are rejected)

Flags:
  --batch-size       Beads per server-side page (default 50)
  --force            Re-embed every bead, even when the text_hash matches
  --only-missing     SQL-filter the page to beads with no embedding or stale
                     embedding (skips walking every bead — preferred for large
                     pods; mutually exclusive with --force)
  --fleet            Fleet to scope the backfill to (overrides BEADS_FLEET)
  --types            Only embed beads whose type is in this comma-separated
                     list. Falls back to BEADS_EMBED_TYPES env var. Empty
                     means "all types".
  --exclude-types    Skip beads whose type is in this comma-separated list.
                     Falls back to BEADS_EMBED_EXCLUDE_TYPES env var. Applied
                     after --types so it can subtract from an allowlist.
  --limit            Cap total beads visited across all pages. Combined with
                     --only-missing this caps the work the CLI will drive in
                     one invocation — useful for time-budgeted callers (e2e
                     smoke tests) that need a freshly-created bead embedded
                     but cannot wait for a multi-thousand-bead backlog to
                     drain. 0 (default) means no limit.

Type filtering is operator-driven cost control: telemetry-shaped types
(event, session, agent stubs) embed to ~zero semantic value and dominate
request volume in fleets that produce them. A typical allowlist is
task,decision,advice,bug,epic,escalation,molecule,memory,feature,formula,
config,runbook,role,chore.`,
	RunE: runEmbedSync,
}

var (
	embedSyncBatchSize    int
	embedSyncForce        bool
	embedSyncOnlyMissing  bool
	embedSyncTypes        string
	embedSyncExcludeTypes string
	embedSyncLimit        int
)

func init() {
	embedSyncCmd.Flags().IntVar(&embedSyncBatchSize, "batch-size", 50, "beads per page and per embed-batch call (1-500)")
	embedSyncCmd.Flags().BoolVar(&embedSyncForce, "force", false, "re-embed every bead, even when the text_hash matches")
	embedSyncCmd.Flags().BoolVar(&embedSyncOnlyMissing, "only-missing", false, "SQL-filter the backfill to beads with no embedding or stale embedding (skips walking every bead)")
	embedSyncCmd.Flags().StringVar(&embedSyncTypes, "types", "", "only embed beads whose type is in this CSV (env: BEADS_EMBED_TYPES). Empty = all types.")
	embedSyncCmd.Flags().StringVar(&embedSyncExcludeTypes, "exclude-types", "", "skip beads whose type is in this CSV (env: BEADS_EMBED_EXCLUDE_TYPES). Applied after --types.")
	embedSyncCmd.Flags().IntVar(&embedSyncLimit, "limit", 0, "cap total beads visited across all pages (0 = no limit). Combined with --only-missing, bounds wall time for time-budgeted callers (e.g. e2e tests).")

	embedMigrateCmd.Flags().StringVar(&embedMigrateToModel, "to-model", "", "target embedding model name (must match configured backend)")
	embedMigrateCmd.Flags().IntVar(&embedMigrateDims, "dims", 0, "target embedding dimensionality (e.g. 1024, 1536)")
	embedMigrateCmd.Flags().BoolVar(&embedMigrateConfirm, "yes-i-understand-embeddings-are-destroyed", false, "required safety flag — truncates bead_embeddings")
	embedMigrateCmd.Flags().IntVar(&embedMigrateBatchSize, "batch-size", 50, "passed to the internal `embed sync --force` step (1-500)")
	embedMigrateCmd.Flags().StringVar(&embedMigrateNote, "note", "", "free-form note recorded in bead_embedding_migrations")

	embedCmd.AddCommand(embedSyncCmd)
	embedCmd.AddCommand(embedMigrateCmd)
}

func runEmbedSync(cmd *cobra.Command, _ []string) error {
	if embedSyncBatchSize < 1 {
		return fmt.Errorf("--batch-size must be >= 1")
	}
	if embedSyncBatchSize > 500 {
		return fmt.Errorf("--batch-size must be <= 500")
	}
	if embedSyncOnlyMissing && embedSyncForce {
		return fmt.Errorf("--only-missing and --force are mutually exclusive")
	}

	allowedTypes := parseTypeFilter(embedSyncTypes, "BEADS_EMBED_TYPES")
	excludedTypes := parseTypeFilter(embedSyncExcludeTypes, "BEADS_EMBED_EXCLUDE_TYPES")

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	out := cmd.OutOrStdout()
	if len(allowedTypes) > 0 {
		fmt.Fprintf(out, "Type allowlist: %v\n", sortedKeys(allowedTypes))
	}
	if len(excludedTypes) > 0 {
		fmt.Fprintf(out, "Type excludelist: %v\n", sortedKeys(excludedTypes))
	}

	var (
		offset       int
		total        int
		embedded     int
		skipped      int
		filteredType int
		processed    int
		modelName    string
	)

	for {
		// Per-page cap. With --limit, shrink the request to the remaining
		// allowance so the server doesn't return more than we want — the
		// alternative ("walk a full page, then stop") would overshoot --limit
		// by up to batch-size and waste embedder calls in the e2e case.
		pageLimit := embedSyncBatchSize
		if embedSyncLimit > 0 {
			remaining := embedSyncLimit - processed
			if remaining <= 0 {
				break
			}
			if remaining < pageLimit {
				pageLimit = remaining
			}
		}

		resp, err := beadsClient.SyncBatchEmbeddings(ctx, &client.SyncBatchEmbeddingsRequest{
			Limit:        pageLimit,
			Offset:       offset,
			Fleet:        fleetFlag,
			Force:        embedSyncForce,
			OnlyMissing:  embedSyncOnlyMissing,
			Types:        sortedKeys(allowedTypes),
			ExcludeTypes: sortedKeys(excludedTypes),
		})
		if err != nil {
			return fmt.Errorf("sync-batch (offset=%d): %w", offset, err)
		}
		if resp == nil || resp.PageCount == 0 {
			break
		}
		if resp.Total > 0 {
			total = resp.Total
		}
		if modelName == "" && resp.Model != "" {
			modelName = resp.Model
			fmt.Fprintf(out, "Embedding backend: %s\n", modelName)
		}

		embedded += resp.Embedded
		skipped += resp.Skipped
		filteredType += resp.FilteredType
		processed += resp.PageCount

		if total > 0 {
			fmt.Fprintf(out, "Embedded %d/%d beads (skipped %d, filtered-type %d)...\n", embedded, total, skipped, filteredType)
		} else {
			fmt.Fprintf(out, "Embedded %d beads (skipped %d, filtered-type %d)...\n", embedded, skipped, filteredType)
		}

		// Server returned fewer rows than we asked for → exhausted.
		if resp.PageCount < pageLimit {
			break
		}
		// --limit budget exhausted.
		if embedSyncLimit > 0 && processed >= embedSyncLimit {
			break
		}
		// In --only-missing mode each successful page removes those beads
		// from the "needs embedding" set, so the next call with offset=0
		// returns the next slice of pending work. Advancing offset would
		// skip beads that still need embedding.
		if !embedSyncOnlyMissing {
			offset += resp.PageCount
		}
	}

	fmt.Fprintf(out, "Done. Embedded %d, skipped %d, filtered-type %d, processed %d.\n", embedded, skipped, filteredType, processed)
	return nil
}

// parseTypeFilter resolves a CSV from a flag, falling back to an env var.
// Returns nil when both are empty (= no filter).
func parseTypeFilter(flagVal, envKey string) map[string]struct{} {
	csv := flagVal
	if csv == "" {
		csv = os.Getenv(envKey)
	}
	if csv == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, t := range splitCSV(csv) {
		out[t] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	cur := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ',' || c == ' ' || c == '\t' {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			continue
		}
		cur = append(cur, c)
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// sortedKeys returns the map keys in ascending order — used for deterministic
// logging output and for sending the filter to the server as a stable list.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
