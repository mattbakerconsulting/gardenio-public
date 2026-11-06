package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// projectLabelCandidatesSQL selects the live beads that break the project-label
// invariant (server/project_label_invariant.go, gd-YdsEpnN3Qo) as (id, type,
// label to add): a bead of one of the types in $1 with a non-blank
// fields.project and no project: label, then a message with no project: label
// whose thread head (fields.thread_id) carries exactly one known project label.
// The count (lint gauge) and the backfill share it so they cannot disagree.
const projectLabelCandidatesSQL = `
		SELECT b.id, b.type, 'project:' || btrim(b.fields->>'project') AS label
		FROM beads b
		WHERE b.deleted_at IS NULL
		  AND b.type = ANY($1::text[])
		  AND btrim(COALESCE(b.fields->>'project', '')) <> ''
		  AND NOT EXISTS (
			SELECT 1 FROM labels l WHERE l.bead_id = b.id AND l.label LIKE 'project:_%')
		UNION ALL
		SELECT m.id, m.type, h.label
		FROM beads m
		JOIN LATERAL (
			SELECT MIN(hl.label) AS label, COUNT(*) AS n
			FROM labels hl
			WHERE hl.bead_id = btrim(m.fields->>'thread_id')
			  AND hl.label LIKE 'project:_%'
			  AND hl.label <> 'project:unknown'
		) h ON h.n = 1
		WHERE m.deleted_at IS NULL
		  AND m.type = 'message'
		  AND COALESCE(m.fields->>'thread_id', '') <> ''
		  AND NOT EXISTS (
			SELECT 1 FROM labels l WHERE l.bead_id = m.id AND l.label LIKE 'project:_%')`

// queryProjectLabelMissingCounts counts the project-label invariant's
// candidates by type. Types with none are absent from the result.
func queryProjectLabelMissingCounts(ctx context.Context, db executor, projectTypes []string) (map[string]int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT type, COUNT(*) FROM (`+projectLabelCandidatesSQL+`) c GROUP BY type`,
		pq.Array(projectTypes))
	if err != nil {
		return nil, fmt.Errorf("project label lint: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var beadType string
		var count int
		if err := rows.Scan(&beadType, &count); err != nil {
			return nil, fmt.Errorf("scan project label lint row: %w", err)
		}
		result[beadType] = count
	}
	return result, rows.Err()
}

// queryBackfillProjectLabelsPage labels up to batchSize candidates through
// queryAddLabel (fleet column, beads.project sync) in one transaction. A
// labelled bead is no longer a candidate, so the next page needs no cursor.
func queryBackfillProjectLabelsPage(ctx context.Context, db *sql.DB, projectTypes []string, batchSize int) (*store.BackfillProjectLabelsStats, error) {
	if batchSize <= 0 {
		batchSize = 500
	}
	type candidate struct{ id, beadType, label string }
	rows, err := db.QueryContext(ctx, projectLabelCandidatesSQL+` LIMIT $2`, pq.Array(projectTypes), batchSize)
	if err != nil {
		return nil, fmt.Errorf("project label backfill candidates: %w", err)
	}
	var page []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.beadType, &c.label); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan project label backfill candidate: %w", err)
		}
		page = append(page, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("project label backfill candidates: %w", err)
	}

	stats := &store.BackfillProjectLabelsStats{Labelled: map[string]int{}, Done: len(page) < batchSize}
	if len(page) == 0 {
		return stats, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for _, c := range page {
		if err := queryAddLabel(ctx, tx, c.id, c.label); err != nil {
			return nil, fmt.Errorf("labelling %s %s: %w", c.id, c.label, err)
		}
		stats.Labelled[c.beadType]++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stats, nil
}
