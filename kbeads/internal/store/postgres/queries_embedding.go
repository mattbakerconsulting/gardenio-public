package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
	"github.com/lib/pq"
)

// vectorLiteral formats a []float32 as pgvector's text representation: "[1,2,3]".
func vectorLiteral(v []float32) string {
	var sb strings.Builder
	sb.Grow(2 + len(v)*8)
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

func queryUpsertEmbedding(ctx context.Context, db executor, beadID string, embedding []float32, modelName, textHash string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO bead_embeddings (bead_id, embedding, model, text_hash, fleet, created_at, updated_at)
		SELECT $1, $2::vector, $3, $4, b.fleet, NOW(), NOW()
		FROM beads b WHERE b.id = $1
		ON CONFLICT (bead_id) DO UPDATE SET
			embedding  = EXCLUDED.embedding,
			model      = EXCLUDED.model,
			text_hash  = EXCLUDED.text_hash,
			fleet      = EXCLUDED.fleet,
			updated_at = NOW()`,
		beadID, vectorLiteral(embedding), modelName, textHash)
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}
	return nil
}

// queryGetEmbedding returns the stored vector and model for a bead. It returns
// sql.ErrNoRows when the bead has no embedding row.
func queryGetEmbedding(ctx context.Context, db executor, beadID string) ([]float32, string, error) {
	var vecText, modelName string
	err := db.QueryRowContext(ctx,
		`SELECT embedding::text, model FROM bead_embeddings WHERE bead_id = $1`, beadID,
	).Scan(&vecText, &modelName)
	if err != nil {
		return nil, "", err
	}
	vec, err := parseVectorLiteral(vecText)
	if err != nil {
		return nil, "", fmt.Errorf("parse stored embedding: %w", err)
	}
	return vec, modelName, nil
}

// parseVectorLiteral parses pgvector's text representation "[1,2,3]" back to
// a []float32. Whitespace between brackets/commas is tolerated.
func parseVectorLiteral(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("invalid vector literal: %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []float32{}, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("invalid vector element %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// queryGetEmbeddingHashes returns a map of bead_id -> text_hash for the
// subset of beadIDs that currently have a row in bead_embeddings. Beads
// without an embedding row are simply omitted from the result. Used by
// the embed-sync backfill path to skip re-embedding beads whose text
// content has not changed.
func queryGetEmbeddingHashes(ctx context.Context, db executor, beadIDs []string) (map[string]string, error) {
	if len(beadIDs) == 0 {
		return map[string]string{}, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT bead_id, text_hash FROM bead_embeddings WHERE bead_id = ANY($1)`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("get embedding hashes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(beadIDs))
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, fmt.Errorf("scan embedding hash: %w", err)
		}
		out[id] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get embedding hashes: %w", err)
	}
	return out, nil
}

func queryDeleteEmbedding(ctx context.Context, db executor, beadID string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM bead_embeddings WHERE bead_id = $1`, beadID)
	if err != nil {
		return fmt.Errorf("delete embedding: %w", err)
	}
	return nil
}

// ivfflatProbes is the per-query probe count for the bead_embeddings IVFFlat
// index (migration 005, lists=100). pgvector defaults to probes=1, which only
// scans 1% of the index partitions on every query — for small/sparse corpora
// this routinely misses true nearest-neighbors and breaks `kd similar` and
// `/v1/search/semantic` recall (release-blocking e2e regression ka-0000005881).
// 10 ≈ sqrt(lists) is the value pgvector recommends for balanced recall/speed.
// The caller MUST issue this on the same backend session as the SELECT — see
// PostgresStore.SemanticSearch where a *sql.Conn is acquired to guarantee that.
const ivfflatProbes = 10

// querySemanticSearch runs a vector nearest-neighbor query against bead_embeddings,
// applying BeadFilter structured filters against the joined beads table.
//
// Bumps ivfflat.probes for the duration of the executor's session before the
// SELECT — see ivfflatProbes for rationale. The bump is `SET` (not `SET LOCAL`)
// so it persists for the connection's lifetime; that is intentional and safe
// because probes=10 is a sensible default for every query that hits this index.
func querySemanticSearch(ctx context.Context, db executor, queryVec []float32, filter model.BeadFilter, limit int) ([]*model.SemanticResult, error) {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("SET ivfflat.probes = %d", ivfflatProbes)); err != nil {
		return nil, fmt.Errorf("set ivfflat.probes: %w", err)
	}
	if limit <= 0 {
		limit = 20
	}

	var (
		whereClauses []string
		args         []any
		argIdx       int
	)
	nextArg := func() string {
		argIdx++
		return fmt.Sprintf("$%d", argIdx)
	}

	// $1 is the query vector; $2 is the same (used in ORDER BY).
	args = append(args, vectorLiteral(queryVec))
	argIdx++

	if len(filter.Status) > 0 {
		placeholders := make([]string, len(filter.Status))
		for i, s := range filter.Status {
			placeholders[i] = nextArg()
			args = append(args, string(s))
		}
		whereClauses = append(whereClauses, "b.status IN ("+strings.Join(placeholders, ", ")+")")
	}

	if len(filter.Type) > 0 {
		placeholders := make([]string, len(filter.Type))
		for i, t := range filter.Type {
			placeholders[i] = nextArg()
			args = append(args, string(t))
		}
		whereClauses = append(whereClauses, "b.type IN ("+strings.Join(placeholders, ", ")+")")
	}

	if len(filter.ExcludeTypes) > 0 {
		placeholders := make([]string, len(filter.ExcludeTypes))
		for i, t := range filter.ExcludeTypes {
			placeholders[i] = nextArg()
			args = append(args, string(t))
		}
		whereClauses = append(whereClauses, "b.type NOT IN ("+strings.Join(placeholders, ", ")+")")
	}

	if len(filter.Kind) > 0 {
		placeholders := make([]string, len(filter.Kind))
		for i, k := range filter.Kind {
			placeholders[i] = nextArg()
			args = append(args, string(k))
		}
		whereClauses = append(whereClauses, "b.kind IN ("+strings.Join(placeholders, ", ")+")")
	}

	if filter.Priority != nil {
		whereClauses = append(whereClauses, "b.priority = "+nextArg())
		args = append(args, *filter.Priority)
	}
	if filter.PriorityMin != nil {
		whereClauses = append(whereClauses, "b.priority >= "+nextArg())
		args = append(args, *filter.PriorityMin)
	}
	if filter.PriorityMax != nil {
		whereClauses = append(whereClauses, "b.priority <= "+nextArg())
		args = append(args, *filter.PriorityMax)
	}

	if filter.Assignee != "" {
		whereClauses = append(whereClauses, "b.assignee = "+nextArg())
		args = append(args, filter.Assignee)
	}

	if filter.CreatedBy != "" {
		whereClauses = append(whereClauses, "b.created_by = "+nextArg())
		args = append(args, filter.CreatedBy)
	}

	if len(filter.Labels) > 0 {
		for _, label := range filter.Labels {
			p := nextArg()
			whereClauses = append(whereClauses,
				fmt.Sprintf("EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = b.id AND labels.label = %s)", p))
			args = append(args, label)
		}
	}

	if len(filter.LabelsAny) > 0 {
		p := nextArg()
		whereClauses = append(whereClauses,
			fmt.Sprintf("EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = b.id AND labels.label = ANY(%s::text[]))", p))
		args = append(args, pq.Array(filter.LabelsAny))
	}

	if len(filter.Titles) > 0 {
		p := nextArg()
		whereClauses = append(whereClauses, "b.title = ANY("+p+"::text[])")
		args = append(args, pq.Array(filter.Titles))
	}

	// LabelsOrNoProject: an in-scope label, or no project label at all (see
	// queryListBeads). Must be applied here too: the server swaps it in for the
	// strict Labels predicate, so a store path that ignored it would widen.
	if len(filter.LabelsOrNoProject) > 0 {
		p := nextArg()
		whereClauses = append(whereClauses,
			fmt.Sprintf("(EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = b.id AND labels.label = ANY(%s::text[])) OR NOT EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = b.id AND labels.label LIKE 'project:_%%'))", p))
		args = append(args, pq.Array(filter.LabelsOrNoProject))
	}

	// LabelsAnyUser: own EXISTS clause so the user --label-any ANDs with the
	// OIDC-scoping LabelsAny rather than widening it (ka-0000003307).
	if len(filter.LabelsAnyUser) > 0 {
		p := nextArg()
		whereClauses = append(whereClauses,
			fmt.Sprintf("EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = b.id AND labels.label = ANY(%s::text[]))", p))
		args = append(args, pq.Array(filter.LabelsAnyUser))
	}

	if len(filter.IDs) > 0 {
		p := nextArg()
		whereClauses = append(whereClauses, "b.id = ANY("+p+"::text[])")
		args = append(args, pq.Array(filter.IDs))
	}

	// TaintGrantIDs (ADR docs/adr/taints.md §4.3/§5.1/§15.2): the vector
	// index itself cannot express the bead_taints join, so the same NOT
	// EXISTS predicate queryListBeads applies is mirrored here post-index,
	// before results ever leave the server, filtered to gate-effect taints
	// only — a bead carrying a gate-effect bead_taints row outside the
	// caller's grant set matches no row of the inner SELECT. `<> ALL($N)`
	// (not `NOT IN`) so an empty grant set correctly excludes every
	// gate-tainted bead rather than vacuously matching everything; see
	// queryListBeads for the full rationale. Nil means the gate does not
	// apply (exempt caller, or an empty taints registry).
	if filter.TaintGrantIDs != nil {
		p := nextArg()
		whereClauses = append(whereClauses,
			fmt.Sprintf("NOT EXISTS (SELECT 1 FROM bead_taints bt JOIN taints t ON t.id = bt.taint_id WHERE bt.bead_id = b.id AND 'gate' = ANY(t.effects) AND bt.taint_id <> ALL(%s::bigint[]))", p))
		args = append(args, pq.Array(*filter.TaintGrantIDs))
	}

	// Mirror queries.go: skip fleet scoping when listing only cross-fleet
	// types (project, account). See model.AllTypesCrossFleet.
	if filter.Fleet != "" && filter.Fleet != model.FleetAll && !model.AllTypesCrossFleet(filter.Type) {
		whereClauses = append(whereClauses, "b.fleet = "+nextArg())
		args = append(args, filter.Fleet)
	}

	// Soft-delete filter (ka-0000004651 step 8b): semantic search must not
	// surface tombstoned beads. The embedding row may still exist (delete is
	// a soft-delete on `beads`, not on `bead_embeddings`) so we filter at the
	// join.
	whereClauses = append(whereClauses, "b.deleted_at IS NULL")

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = " WHERE " + strings.Join(whereClauses, " AND ")
	}

	limitArg := nextArg()
	args = append(args, limit)

	// Use $1 (the query vector) both for the score expression and the ORDER BY.
	// Column list MUST mirror scanBeadWithScoreFull's destinations (the standard
	// 22 b.* columns plus the trailing three operator columns: canonical alias,
	// display, email — see beadOperatorSelectSuffix in queries.go) — a
	// divergence between the SELECT projection and the scan target produces a
	// runtime "sql: expected N destination arguments in Scan" error which
	// surfaces as HTTP 500 "semantic search failed" (ka-0000007367).
	// The displayed `score` stays PURE cosine similarity (1 - distance) so the
	// reported number is not distorted by ranking policy. Down-ranking of
	// ephemeral/closed beads happens ONLY in the ORDER BY via an additive
	// distance penalty (ka-0000000838): pure cosine distance saturates the
	// top-N of any gardener/plan/escalation-flavored query with closed
	// type=message / type=agent gardener-lifecycle beads that sit semantically
	// near-but-not-relevant, burying the open issue-like beads a dedup search is
	// actually hunting. `<=>` is cosine distance (0 = identical, larger = less
	// similar), so ADDING a positive constant to the ORDER key sorts a penalized
	// bead AFTER an equally-similar issue-like bead. Constants: +0.15 for
	// ephemeral message/agent beads (observed live pollution clusters at
	// distance ~0.55-0.65 / score 0.35-0.46, so 0.15 reliably sinks it below a
	// genuinely-similar issue bead yet is small enough not to hide a
	// dramatically-closer hit), +0.05 for merely-closed beads (a gentler nudge
	// so a genuinely top-hit closed bug is not buried). The penalty is additive,
	// so recall is unchanged (the bead still returns, just lower) and `-t
	// message` opt-in still surfaces them exactly. The CASE is mutually
	// exclusive (first match wins): a closed message bead takes the 0.15 branch,
	// which already sinks it, so ephemeral and closed do not stack.
	sql := `SELECT 1 - (e.embedding <=> $1::vector) AS score, ` +
		"b.id, b.slug, b.short_title, b.heraldry, b.kind, b.type, b.title, b.description, b.notes, " +
		"b.status, b.priority, b.assignee, b.owner, b.created_at, b.created_by, b.updated_at, " +
		"b.closed_at, b.closed_by, b.due_at, b.defer_until, b.fields, b.fleet, b.deleted_at, b.category, b.operated_by_id, " +
		"COALESCE(op.kind || ':' || op.subject, '') AS operated_by, " +
		"COALESCE(op.display, '') AS operated_by_display, COALESCE(op.email, '') AS operated_by_email " +
		`FROM bead_embeddings e JOIN beads b ON b.id = e.bead_id ` +
		`LEFT JOIN operators op ON op.id = b.operated_by_id` + whereSQL +
		` ORDER BY (e.embedding <=> $1::vector)` +
		` + (CASE WHEN b.type IN ('message','agent') THEN 0.15` +
		` WHEN b.status = 'closed' THEN 0.05` +
		` ELSE 0 END) LIMIT ` + limitArg

	rows, err := db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	defer rows.Close()

	var results []*model.SemanticResult
	for rows.Next() {
		b, score, err := scanBeadWithScoreFull(rows)
		if err != nil {
			return nil, fmt.Errorf("scan semantic result: %w", err)
		}
		results = append(results, &model.SemanticResult{
			Bead:     b,
			Score:    score,
			Distance: 1 - score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	return results, nil
}

// queryListBeadsNeedingEmbedding returns the page of beads that have no
// embedding row at all OR whose embedding is stale relative to the bead
// (b.updated_at > e.updated_at). This is the SQL pre-filter that lets
// `kd embed sync --only-missing` skip walking the full beads table when
// most beads are already embedded — the prior behavior client-side walked
// every bead and computed text_hash before deciding to skip, which on a
// 50k-bead pod blew through e2e shard timeouts (ka-0000000728).
//
// Total is the total number of beads needing embedding (not the total
// beads count) so the CLI's progress display reflects work-to-do.
//
// Note: stale-by-updated_at is a strict superset of "text_hash differs" —
// a bead can have its updated_at bump from a metadata-only change that
// doesn't affect canonical text. The handler still re-checks text_hash
// per bead before invoking the embedder, so the pre-filter only
// expands the candidate set, never narrows it incorrectly.
func queryListBeadsNeedingEmbedding(ctx context.Context, db executor, fleet string, limit, offset int) ([]*model.Bead, int, error) {
	if limit <= 0 {
		limit = 50
	}

	var (
		whereClauses []string
		args         []any
		argIdx       int
	)
	nextArg := func() string {
		argIdx++
		return fmt.Sprintf("$%d", argIdx)
	}

	whereClauses = append(whereClauses,
		"(e.bead_id IS NULL OR b.updated_at > e.updated_at)")

	// Soft-delete filter (ka-0000004651 step 8b): never embed beads that have
	// been soft-deleted. Without this clause the embedder wastes work on
	// tombstoned rows and semantic search would surface them via the embedding
	// table even though they're gone from the canonical listings.
	whereClauses = append(whereClauses, "b.deleted_at IS NULL")

	if fleet != "" && fleet != model.FleetAll {
		whereClauses = append(whereClauses, "b.fleet = "+nextArg())
		args = append(args, fleet)
	}

	whereSQL := " WHERE " + strings.Join(whereClauses, " AND ")

	limitArg := nextArg()
	args = append(args, limit)
	offsetArg := nextArg()
	args = append(args, offset)

	// Explicit b.-prefixed column list — beadColumns is unprefixed and used
	// for unjoined SELECTs. Must mirror scanBeadWithTotal's destination list
	// (including b.deleted_at) — a divergence between the SELECT projection
	// and the scan target produces "sql: expected N destination arguments
	// in Scan, not M" at runtime, which surfaces as HTTP 500 "failed to list
	// beads" through the embed-sync handler (ka-0000004444).
	const cols = "b.id, b.slug, b.short_title, b.heraldry, b.kind, b.type, b.title, b.description, b.notes, " +
		"b.status, b.priority, b.assignee, b.owner, b.created_at, b.created_by, b.updated_at, " +
		"b.closed_at, b.closed_by, b.due_at, b.defer_until, b.fields, b.fleet, b.deleted_at, b.category, b.operated_by_id"

	// Newest beads first: when there's a backlog, freshly-created beads are
	// the most likely to be queried by users (and by the e2e semantic-search
	// smoke, which seeds new beads then immediately runs sync). Tie-break on
	// b.id for stable pagination across pages with equal created_at.
	query := `SELECT COUNT(*) OVER() AS total_count, ` + cols +
		` FROM beads b LEFT JOIN bead_embeddings e ON e.bead_id = b.id` +
		whereSQL +
		` ORDER BY b.created_at DESC, b.id LIMIT ` + limitArg + ` OFFSET ` + offsetArg

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list beads needing embedding: %w", err)
	}
	defer rows.Close()

	var beads []*model.Bead
	var total int
	for rows.Next() {
		b, t, err := scanBeadWithTotal(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan beads needing embedding: %w", err)
		}
		total = t
		beads = append(beads, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list beads needing embedding: %w", err)
	}
	return beads, total, nil
}

// queryRefreshEmbeddingTimestamps bumps bead_embeddings.updated_at = NOW()
// for the given bead IDs. Used when the embed-sync handler decides a bead's
// stored embedding is fresh (text_hash matches the canonical text) but the
// SQL pre-filter (b.updated_at > e.updated_at) had flagged it as stale.
// Without this bump the same beads would be re-listed forever by
// `kd embed sync --only-missing` (ka-0000000728).
func queryRefreshEmbeddingTimestamps(ctx context.Context, db executor, beadIDs []string) (int64, error) {
	if len(beadIDs) == 0 {
		return 0, nil
	}
	res, err := db.ExecContext(ctx,
		`UPDATE bead_embeddings SET updated_at = NOW() WHERE bead_id = ANY($1)`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return 0, fmt.Errorf("refresh embedding timestamps: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("refresh embedding timestamps rows affected: %w", err)
	}
	return n, nil
}

func queryGetEmbeddingStats(ctx context.Context, db executor) (*model.EmbeddingStats, error) {
	var stats model.EmbeddingStats
	// A single query lets Postgres aggregate both counts in one pass.
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM beads)            AS total_beads,
			(SELECT COUNT(*) FROM bead_embeddings)  AS embedded_beads,
			COALESCE((SELECT model FROM bead_embeddings ORDER BY updated_at DESC LIMIT 1), '') AS model,
			(SELECT COUNT(*) FROM bead_embeddings e
				JOIN beads b ON b.id = e.bead_id
				WHERE b.updated_at > e.updated_at)  AS stale_count
	`).Scan(&stats.TotalBeads, &stats.EmbeddedBeads, &stats.Model, &stats.StaleCount)
	if err != nil {
		return nil, fmt.Errorf("get embedding stats: %w", err)
	}
	return &stats, nil
}
