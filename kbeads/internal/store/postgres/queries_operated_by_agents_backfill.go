package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gardenio/kbeads/internal/store"
)

// queryBackfillOperatedByAgents implements `kd admin
// backfill-operated-by-agents` — sub-item #1 of ka-0000006817
// (per-user homepage M2). See store.BackfillOperatedByAgentsOptions.
//
// Scope: `type=agent` beads whose operated_by_id is unset — either
// literally NULL (legacy envs that never ran migration 017/022) or
// bound to the `system:legacy-pre-attribution` sentinel (envs that did,
// per migration 022 §Pass 3). For each candidate the resolver tries
// fields.slack_user_id → fields.gitlab_username → fields.jira_user_id →
// fields.operated_by (legacy "<kind>:<subject>" text) in that order,
// joining through operator_aliases.
//
// Per kbeads/CLAUDE.md §Migration policy — no eager full-table updates.
// The scan is keyset-paginated by id; each batch is its own
// transaction. Every UPDATE writes one mutation_history row with
// actor='kd admin backfill-operated-by-agents',
// field='operated_by_id', mutation_type='backfill-operated-by-agents'.
// Re-runs on already-stamped data are no-ops via the WHERE filter.
func queryBackfillOperatedByAgents(ctx context.Context, db *sql.DB, opts store.BackfillOperatedByAgentsOptions) (*store.BackfillOperatedByAgentsStats, error) {
	start := time.Now()
	stats := &store.BackfillOperatedByAgentsStats{
		DryRun:         opts.DryRun,
		RebindBySignal: map[string]int64{},
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	// Resolve the sentinel operator id once. Absent (older envs) → 0,
	// which the scan treats as "no sentinel filter".
	var sentinelID int64
	err := db.QueryRowContext(ctx, `
		SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution'`,
	).Scan(&sentinelID)
	if errors.Is(err, sql.ErrNoRows) {
		sentinelID = 0
	} else if err != nil {
		return nil, fmt.Errorf("resolve sentinel: %w", err)
	}

	lastID := ""
	for {
		batch, err := fetchAgentBackfillBatch(ctx, db, lastID, opts.BatchSize, sentinelID, opts.IncludeSentinel)
		if err != nil {
			return stats, fmt.Errorf("fetch batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		lastID = batch[len(batch)-1].id
		stats.LastID = lastID
		stats.Processed += int64(len(batch))

		updates := map[int64][]agentUpdate{}
		for _, b := range batch {
			opID, signal := resolveAgentOperator(ctx, db, b)
			if opID == 0 || signal == "" {
				stats.Skipped++
				continue
			}
			updates[opID] = append(updates[opID], agentUpdate{id: b.id, signal: signal})
		}

		if opts.DryRun {
			for opID, ups := range updates {
				for _, u := range ups {
					stats.RebindBySignal[u.signal]++
					stats.Updated++
					_ = opID
				}
			}
		} else if len(updates) > 0 {
			if err := applyAgentBackfillBatch(ctx, db, updates, sentinelID, opts.IncludeSentinel, stats); err != nil {
				return stats, err
			}
		}

		stats.Batches++
		if opts.MaxBatches > 0 && stats.Batches >= opts.MaxBatches {
			if len(batch) == opts.BatchSize {
				stats.BatchesCapped = true
			}
			break
		}
		if len(batch) < opts.BatchSize {
			break
		}
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// agentCandidateRow is one type=agent bead with NULL or sentinel-bound
// operated_by_id.
type agentCandidateRow struct {
	id     string
	fields json.RawMessage
}

// agentUpdate carries one resolved row's id + the signal field that
// produced the resolution (for the per-signal histogram).
type agentUpdate struct {
	id     string
	signal string
}

func fetchAgentBackfillBatch(
	ctx context.Context,
	db *sql.DB,
	afterID string,
	limit int,
	sentinelID int64,
	includeSentinel bool,
) ([]agentCandidateRow, error) {
	// Build the WHERE clause around operated_by_id. Two shapes:
	//   includeSentinel + sentinelID>0  →  IS NULL OR = $sentinel
	//   otherwise                       →  IS NULL
	var rows *sql.Rows
	var err error
	if includeSentinel && sentinelID > 0 {
		rows, err = db.QueryContext(ctx, `
			SELECT id, COALESCE(fields, '{}'::jsonb)
			  FROM beads
			 WHERE type = 'agent'
			   AND deleted_at IS NULL
			   AND (operated_by_id IS NULL OR operated_by_id = $1)
			   AND id > $2
			 ORDER BY id
			 LIMIT $3`,
			sentinelID, afterID, limit,
		)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT id, COALESCE(fields, '{}'::jsonb)
			  FROM beads
			 WHERE type = 'agent'
			   AND deleted_at IS NULL
			   AND operated_by_id IS NULL
			   AND id > $1
			 ORDER BY id
			 LIMIT $2`,
			afterID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agentCandidateRow
	for rows.Next() {
		var r agentCandidateRow
		if err := rows.Scan(&r.id, &r.fields); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resolveAgentOperator returns the operator id resolved from the agent
// bead's fields and the name of the field that produced the resolution.
// Returns (0, "") when no field carries a resolvable signal. Priority
// order: slack_user_id → gitlab_username → jira_user_id → operated_by
// (legacy "<kind>:<subject>" text).
func resolveAgentOperator(ctx context.Context, db *sql.DB, b agentCandidateRow) (int64, string) {
	m := decodeAgentFields(b.fields)

	tryAlias := func(kind, subject, signal string) (int64, string) {
		// Alias rows of these kinds are stored lowercased (server
		// canonicalAliasSubject, migration 054).
		switch kind {
		case "oidc", "gitlab", "github":
			subject = strings.ToLower(strings.TrimSpace(subject))
		}
		if subject == "" {
			return 0, ""
		}
		id, err := lookupOperatorAliasID(ctx, db, kind, subject)
		if err != nil || id == 0 {
			return 0, ""
		}
		return id, signal
	}

	if s, _ := m["slack_user_id"].(string); s != "" {
		if id, sig := tryAlias("slack", s, "slack_user_id"); id != 0 {
			return id, sig
		}
	}
	if s, _ := m["gitlab_username"].(string); s != "" {
		if id, sig := tryAlias("gitlab", s, "gitlab_username"); id != 0 {
			return id, sig
		}
	}
	if s, _ := m["jira_user_id"].(string); s != "" {
		if id, sig := tryAlias("jira", s, "jira_user_id"); id != 0 {
			return id, sig
		}
	}
	if s, _ := m["operated_by"].(string); s != "" {
		kind, subject, ok := strings.Cut(s, ":")
		if ok && kind != "" && subject != "" {
			if id, sig := tryAlias(kind, subject, "operated_by"); id != 0 {
				return id, sig
			}
		}
	}
	return 0, ""
}

// applyAgentBackfillBatch runs one transaction containing all resolved
// UPDATEs for the batch plus a mutation_history row per affected bead.
func applyAgentBackfillBatch(
	ctx context.Context,
	db *sql.DB,
	updates map[int64][]agentUpdate,
	sentinelID int64,
	includeSentinel bool,
	stats *store.BackfillOperatedByAgentsStats,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback; no-op after Commit.

	for opID, ups := range updates {
		for _, u := range ups {
			var n int64
			if includeSentinel && sentinelID > 0 {
				res, err := tx.ExecContext(ctx, `
					UPDATE beads
					   SET operated_by_id = $1
					 WHERE id = $2
					   AND type = 'agent'
					   AND (operated_by_id IS NULL OR operated_by_id = $3)`,
					opID, u.id, sentinelID,
				)
				if err != nil {
					return fmt.Errorf("update %s -> op %d: %w", u.id, opID, err)
				}
				n, _ = res.RowsAffected()
			} else {
				res, err := tx.ExecContext(ctx, `
					UPDATE beads
					   SET operated_by_id = $1
					 WHERE id = $2
					   AND type = 'agent'
					   AND operated_by_id IS NULL`,
					opID, u.id,
				)
				if err != nil {
					return fmt.Errorf("update %s -> op %d: %w", u.id, opID, err)
				}
				n, _ = res.RowsAffected()
			}
			if n == 0 {
				// Lost a race — another writer stamped between scan and
				// update. Count as skipped (not updated).
				stats.Skipped++
				continue
			}
			newVal := fmt.Sprintf("%d", opID)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				u.id,
				"kd admin backfill-operated-by-agents",
				"operated_by_id",
				"", newVal, "backfill-operated-by-agents",
			); err != nil {
				return fmt.Errorf("mutation_history %s: %w", u.id, err)
			}
			stats.Updated++
			stats.RebindBySignal[u.signal]++
		}
	}
	return tx.Commit()
}

func decodeAgentFields(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}
