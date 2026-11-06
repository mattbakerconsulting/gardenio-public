package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/references"
	"gardenio/kbeads/internal/store"
)

// Source field constants for the bead_references.source_field column.
// See kbeads/docs/design-bead-references.md § 1.
const (
	refFieldTitle       = "title"
	refFieldDescription = "description"
	refFieldNotes       = "notes"
	// refFieldFields extracts bead-id references from the bead's fields JSON
	// blob (model.Bead.Fields). Several bead types keep their canonical,
	// operator-facing body in a fields key rather than description — most
	// notably `gb decision create`, which writes the decision body to
	// `fields.context` and leaves description EMPTY (ka-0000001507). Without
	// this bucket those ids produce no bead_references edge, so
	// `kd list --mentions <id>` — the §5.0 pre-mutation decision gate —
	// fails OPEN for the majority of open decisions. We extract only UNTYPED
	// (flat) references here: the gate queries the untyped family, and
	// promoting typed deps out of arbitrary JSON blob content (option
	// labels, prompts) would poison the deps graph. Extraction runs over the
	// DECODED string values (see extractFieldsRefs) so an id preceded by a
	// JSON escape sequence — most commonly `\n`, a newline in the operator's
	// prose — is not silently dropped by the word-boundary anchor.
	refFieldFields = "fields"
	// refFieldCommentPrefix is followed by the comment id (e.g. "comment:42")
	// so per-comment deletes don't over-delete on recompute.
	refFieldCommentPrefix = "comment:"
)

// extractFieldsRefs returns the untyped bead-id references found anywhere in
// the bead's fields JSON blob. Extraction runs over the DECODED string leaf
// values — not the raw JSON bytes — because a ka-id that sits immediately after
// a JSON escape sequence in the raw encoding (overwhelmingly `\n`, i.e. a
// newline directly before the id in the operator's prose) is preceded by the
// literal two bytes `\` `n`, and the word-boundary anchor `\b` does not match
// between `n` and `k`, so the id is silently dropped from the raw scan
// (ka-0000007416). Walking the unmarshalled value and running the extractor on
// each string leaf — recursing into nested objects and arrays so keys like
// options[].label and prompt are still covered — sees the real prose the
// operator typed, where the newline is a byte the regex ignores.
//
// Semantics preserved: only UNTYPED (flat) references are returned regardless
// of which key or nesting depth holds the id — no typed-dep promotion out of
// arbitrary JSON blob content. If the blob is not valid JSON, we fall back to
// the historical raw-bytes scan so a malformed blob degrades to the prior
// behaviour rather than losing every ref. Empty/nil fields yields no refs.
//
// Top-level keys in unreferencedFieldKeys are skipped: their values are bead
// ids that must not become a public mentioned-by backlink.
func extractFieldsRefs(fields []byte) []string {
	if len(fields) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(fields, &decoded); err != nil {
		// Malformed blob: degrade to the historical raw-bytes scan rather
		// than losing all refs.
		return references.Extract(string(fields))
	}
	seen := make(map[string]struct{})
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for _, ref := range references.Extract(t) {
				if _, ok := seen[ref]; ok {
					continue
				}
				seen[ref] = struct{}{}
				out = append(out, ref)
			}
		case map[string]any:
			// Walk map keys in sorted order so the emitted ref order is
			// DETERMINISTIC. Go randomizes map iteration, which otherwise made
			// the fields-bucket ref order depend on the run (ka-0000001227 deflake:
			// TestReplaceBeadReferences_ExtractsFieldsContext flaked ~50% because
			// it asserts a fixed id order). The set of refs is unchanged; only the
			// ordering is pinned.
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	if top, ok := decoded.(map[string]any); ok {
		for k := range unreferencedFieldKeys {
			delete(top, k)
		}
	}
	walk(decoded)
	return out
}

// unreferencedFieldKeys are top-level fields keys whose bead-id values are
// not references. read_cursors on a type:user bead maps each Commons channel
// to the last message the person read there (kbeads http_read_cursors.go); as
// references they would put the person on every such message's mentioned-by,
// so anyone who can read a message could see who has read up to it.
var unreferencedFieldKeys = map[string]bool{
	"read_cursors": true,
}

// replaceBeadReferences re-extracts ka-id references from the bead's text
// fields (title, description, notes) plus its fields JSON blob and replaces
// the matching rows in bead_references. Runs in the supplied executor (typically the same tx as
// the bead INSERT/UPDATE) so the references update is atomic with the
// primary write. Returns the per-metric-bucket counts for observability.
//
// As of M5 of the auto-link epic (ka-0000006487 / design ka-0000000827
// M5/A) typed mentions in the closed promotion verb set additionally
// emit a row in the deps table — see bulkInsertTypedDeps. The deps
// writes are append-only and idempotent (ON CONFLICT DO NOTHING) so
// re-runs and concurrent writers converge to the same post-state, and
// deps added manually outside the parser path are preserved.
func replaceBeadReferences(ctx context.Context, db executor, b *model.Bead) error {
	start := time.Now()
	titleRefs := references.Extract(b.Title)
	descRefs := references.Extract(b.Description)
	notesRefs := references.Extract(b.Notes)
	fieldsRefs := extractFieldsRefs(b.Fields)
	titleTyped := references.ExtractTyped(b.Title)
	descTyped := references.ExtractTyped(b.Description)
	notesTyped := references.ExtractTyped(b.Notes)

	// Always DELETE the four field buckets so a removed reference
	// disappears from the table on the next write. The fields bucket is
	// untyped-only (no typed-dep promotion out of arbitrary JSON blobs).
	if _, err := db.ExecContext(ctx,
		`DELETE FROM bead_references WHERE source_bead_id = $1 AND source_field = ANY($2::text[])`,
		b.ID, pq.Array([]string{refFieldTitle, refFieldDescription, refFieldNotes, refFieldFields}),
	); err != nil {
		return fmt.Errorf("delete bead references: %w", err)
	}

	if err := bulkInsertReferences(ctx, db, b.ID, refFieldTitle, titleRefs); err != nil {
		return err
	}
	if err := bulkInsertTypedRefs(ctx, db, b.ID, refFieldTitle, titleTyped); err != nil {
		return err
	}
	if err := bulkInsertTypedDeps(ctx, db, b.ID, b.OperatedByID, titleTyped); err != nil {
		return err
	}
	if err := bulkInsertReferences(ctx, db, b.ID, refFieldDescription, descRefs); err != nil {
		return err
	}
	if err := bulkInsertTypedRefs(ctx, db, b.ID, refFieldDescription, descTyped); err != nil {
		return err
	}
	if err := bulkInsertTypedDeps(ctx, db, b.ID, b.OperatedByID, descTyped); err != nil {
		return err
	}
	if err := bulkInsertReferences(ctx, db, b.ID, refFieldNotes, notesRefs); err != nil {
		return err
	}
	if err := bulkInsertTypedRefs(ctx, db, b.ID, refFieldNotes, notesTyped); err != nil {
		return err
	}
	if err := bulkInsertTypedDeps(ctx, db, b.ID, b.OperatedByID, notesTyped); err != nil {
		return err
	}
	// Fields bucket: flat (untyped) references only. No bulkInsertTypedRefs /
	// bulkInsertTypedDeps — a decision's option labels or prompt text must not
	// mint typed edges or deps in the graph.
	if err := bulkInsertReferences(ctx, db, b.ID, refFieldFields, fieldsRefs); err != nil {
		return err
	}

	metrics.ObserveReferencesExtract(start, map[string]int{
		metrics.RefSourceTitle:       len(titleRefs),
		metrics.RefSourceDescription: len(descRefs),
		metrics.RefSourceNotes:       len(notesRefs),
		metrics.RefSourceFields:      len(fieldsRefs),
	})
	return nil
}

// replaceCommentReferences re-extracts ka-id references from a single
// comment's text and replaces the matching rows in bead_references. Uses
// source_field = "comment:<id>" so per-comment writes don't disturb
// references emitted by other comments on the same bead.
func replaceCommentReferences(ctx context.Context, db executor, c *model.Comment) error {
	start := time.Now()
	refs := references.Extract(c.Text)
	typed := references.ExtractTyped(c.Text)
	field := fmt.Sprintf("%s%d", refFieldCommentPrefix, c.ID)

	if _, err := db.ExecContext(ctx,
		`DELETE FROM bead_references WHERE source_bead_id = $1 AND source_field = $2`,
		c.BeadID, field,
	); err != nil {
		return fmt.Errorf("delete comment references: %w", err)
	}

	if err := bulkInsertReferences(ctx, db, c.BeadID, field, refs); err != nil {
		return err
	}
	if err := bulkInsertTypedRefs(ctx, db, c.BeadID, field, typed); err != nil {
		return err
	}
	if err := bulkInsertTypedDeps(ctx, db, c.BeadID, c.OperatedByID, typed); err != nil {
		return err
	}

	metrics.ObserveReferencesExtract(start, map[string]int{
		metrics.RefSourceComment: len(refs),
	})
	return nil
}

// queryReextractReferencesPage walks the next page of beads (id > afterID,
// ORDER BY id ASC, LIMIT batchSize) and re-runs reference extraction for
// each bead's text fields and every one of its comments. Each bead +
// comments pair is wrapped in a single transaction so the full re-extraction
// for that bead is atomic. Per-bead failures abort the page so the caller
// can decide whether to retry; partial pages remain consistent because each
// bead's transaction either commits or rolls back as a unit.
//
// When beadType is non-empty the page walk is narrowed to beads of that type
// (index-backed by idx_beads_type, migration 001). This exists so a targeted
// backfill can reindex one bead type FIRST — most importantly `type=decision`,
// the only type the parked_on_decision badge queries (ka-0000003797). A full
// ~677k-bead backfill orders decision-shaped edges near the END of the keyset
// (decision ids sort late), so the badge renders silent false negatives for
// hours while the unfiltered run climbs toward them (ka-0000005898). A
// `--type decision` pass is seconds, not hours, and makes the badge sound for
// the one type it depends on. Done / TotalReferences still reflect only the
// filtered slice; TotalReferences on the final page reports the whole-table
// cardinality (an intentionally cheap COUNT(*), unaffected by the filter).
//
// As of T4 (typed-mention syntax epic ka-0000003840) the backfill also
// populates bead_references.relationship_type by running the typed parser
// (references.ExtractTyped) over the same text and writing one row per
// recognized canonical verb on top of the untyped (NULL relationship_type)
// rows produced by the original Extract. The two rowsets coexist on the
// extended unique index (source, target, field, COALESCE(rel_type,”)).
// Re-runs are idempotent: the per-bead tx DELETEs all rows for the four
// text/fields source_fields before re-inserting, so the post-state is fully
// determined by the current text. See kbeads/docs/operator-reextract-references.md.
//
// Pagination uses keyset (id ASC) rather than OFFSET so concurrent inserts
// during the backfill can't shift rows past the cursor. Soft-deleted beads
// are included: their references rows are owned by the row and would
// otherwise be left stale until the row is hard-deleted.
//
// On the final page (PageCount < batchSize) the function also reports the
// table's total cardinality so the caller can print the post-backfill
// figure without an extra round-trip.
func queryReextractReferencesPage(ctx context.Context, db *sql.DB, afterID string, batchSize int, extractCommentRefs, reconcileDeps bool, beadType string) (*store.ReextractReferencesStats, error) {
	if batchSize <= 0 {
		batchSize = 500
	}
	// The type filter is optional: when empty the walk covers every bead.
	// Placeholders are laid out so the LIMIT is always the last argument
	// regardless of whether the type predicate is present.
	query := `
		SELECT id, title, description, notes, fields
		FROM beads
		WHERE id > $1`
	args := []any{afterID}
	if beadType != "" {
		args = append(args, beadType)
		query += fmt.Sprintf(" AND type = $%d", len(args))
	}
	args = append(args, batchSize)
	query += fmt.Sprintf(`
		ORDER BY id ASC
		LIMIT $%d`, len(args))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list beads for reextract: %w", err)
	}
	type beadRow struct {
		id, title, description, notes string
		fields                        []byte
	}
	var page []beadRow
	for rows.Next() {
		var b beadRow
		if err := rows.Scan(&b.id, &b.title, &b.description, &b.notes, &b.fields); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan bead for reextract: %w", err)
		}
		page = append(page, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate beads for reextract: %w", err)
	}
	rows.Close()

	stats := &store.ReextractReferencesStats{}
	for _, b := range page {
		bead := &model.Bead{
			ID:          b.id,
			Title:       b.title,
			Description: b.description,
			Notes:       b.notes,
			Fields:      b.fields,
		}
		written, err := reextractOneBead(ctx, db, bead, extractCommentRefs, reconcileDeps)
		if err != nil {
			return stats, fmt.Errorf("reextract %s: %w", b.id, err)
		}
		stats.BeadsScanned++
		stats.CommentsScanned += written.commentsScanned
		stats.ReferencesWritten += written.referencesWritten
		stats.LastID = b.id
	}

	stats.Done = len(page) < batchSize
	if stats.Done {
		var total int64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM bead_references`).Scan(&total); err != nil {
			return stats, fmt.Errorf("count bead_references: %w", err)
		}
		stats.TotalReferences = total
	}
	return stats, nil
}

// perBeadResult is the per-bead work done inside one reextract transaction.
type perBeadResult struct {
	commentsScanned   int
	referencesWritten int
}

// reextractOneBead runs replaceBeadReferences for the bead and (when
// extractCommentRefs is true) replaceCommentReferences for each of its
// comments inside one transaction. The bead row itself is read inside the
// tx so concurrent updates that beat us to it don't get clobbered by stale
// text from the page list. When extractCommentRefs is false, the comment
// iteration is skipped — comment-text inclusion is gated on decision
// ka-0000000868 and remains opt-in via BEADS_REFERENCES_EXTRACT_COMMENTS.
func reextractOneBead(ctx context.Context, db *sql.DB, bead *model.Bead, extractCommentRefs, reconcileDeps bool) (perBeadResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return perBeadResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Re-read the bead text inside the tx to avoid clobbering a concurrent
	// update that landed between the page list and our processing turn.
	// operated_by_id (the BIGINT operator FK) flows through so
	// bulkInsertTypedDeps can stamp the M5/A dep promotion with the bead's
	// existing operator. The legacy operated_by TEXT column was permanently
	// dropped by migration 024 (POINT OF NO RETURN), so selecting it here
	// failed with `column "operated_by" does not exist` on every post-024
	// database — the very first bead of the backfill 500'd. Only the _id FK
	// is needed (bead.OperatedBy TEXT is never read by the insert path).
	var (
		title, description, notes string
		fields                    []byte
		operatedByID              sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = $1`,
		bead.ID,
	).Scan(&title, &description, &notes, &fields, &operatedByID)
	if err == sql.ErrNoRows {
		// Bead was deleted between the listing and processing — skip.
		return perBeadResult{}, nil
	}
	if err != nil {
		return perBeadResult{}, fmt.Errorf("re-read bead text: %w", err)
	}
	bead.Title, bead.Description, bead.Notes, bead.Fields = title, description, notes, fields
	if operatedByID.Valid {
		id := operatedByID.Int64
		bead.OperatedByID = &id
	}

	if err := replaceBeadReferences(ctx, tx, bead); err != nil {
		return perBeadResult{}, err
	}
	// Proposal B (ka-0000006581): auto-delete parser-created parent-child dep
	// rows whose typed mention no longer exists in any live source field.
	// Runs in the same per-bead tx so the edge delete is atomic with the
	// re-extraction. Opt-in via BEADS_REFERENCES_RECONCILE_DEPS.
	if reconcileDeps {
		if err := reconcileTypedMentionDeps(ctx, tx, bead, extractCommentRefs); err != nil {
			return perBeadResult{}, err
		}
	}
	beadRefs := len(references.Extract(title)) +
		len(references.Extract(description)) +
		len(references.Extract(notes)) +
		len(extractFieldsRefs(fields))

	// Typed mentions are written by replaceBeadReferences itself (T3's
	// bulkInsertTypedRefs path). Account for them here so the per-bead
	// references_written count reflects the typed rows alongside the flat
	// rows. See T4 of epic ka-0000003840.
	beadRefs += countCanonicalTyped(references.ExtractTyped(title)) +
		countCanonicalTyped(references.ExtractTyped(description)) +
		countCanonicalTyped(references.ExtractTyped(notes))

	var commentsScanned, commentRefs int
	if extractCommentRefs {
		// operated_by TEXT was dropped by migration 024 (see the bead re-read
		// above); select only the live operated_by_id FK.
		commentRows, err := tx.QueryContext(ctx,
			`SELECT id, text, operated_by_id FROM comments WHERE bead_id = $1 ORDER BY id ASC`,
			bead.ID,
		)
		if err != nil {
			return perBeadResult{}, fmt.Errorf("list comments: %w", err)
		}
		type commentRow struct {
			id           int64
			text         string
			operatedByID sql.NullInt64
		}
		var comments []commentRow
		for commentRows.Next() {
			var c commentRow
			if err := commentRows.Scan(&c.id, &c.text, &c.operatedByID); err != nil {
				commentRows.Close()
				return perBeadResult{}, fmt.Errorf("scan comment: %w", err)
			}
			comments = append(comments, c)
		}
		if err := commentRows.Err(); err != nil {
			commentRows.Close()
			return perBeadResult{}, fmt.Errorf("iterate comments: %w", err)
		}
		commentRows.Close()

		for _, c := range comments {
			comment := &model.Comment{ID: c.id, BeadID: bead.ID, Text: c.text}
			if c.operatedByID.Valid {
				id := c.operatedByID.Int64
				comment.OperatedByID = &id
			}
			if err := replaceCommentReferences(ctx, tx, comment); err != nil {
				return perBeadResult{}, err
			}
			commentRefs += len(references.Extract(c.text))
			commentRefs += countCanonicalTyped(references.ExtractTyped(c.text))
		}
		commentsScanned = len(comments)
	}

	if err := tx.Commit(); err != nil {
		return perBeadResult{}, fmt.Errorf("commit tx: %w", err)
	}
	return perBeadResult{
		commentsScanned:   commentsScanned,
		referencesWritten: beadRefs + commentRefs,
	}, nil
}

// queryGetTextReferencesForBeads returns rows from bead_references where
// BOTH source_bead_id and target_bead_id are in beadIDs. Dangling targets
// (typos, cross-fleet IDs, soft-deleted beads) are intentionally filtered
// out at the query level so the /v1/graph response never surfaces edges
// pointing at nodes that don't exist in the response. See
// kbeads/docs/design-bead-references.md § 5b. Single SQL JOIN-equivalent
// using ANY($1::text[]) per project advice (ka-0000000229) — no N+1.
func queryGetTextReferencesForBeads(ctx context.Context, db executor, beadIDs []string) ([]*model.BeadReference, error) {
	if len(beadIDs) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT source_bead_id, target_bead_id, source_field, relationship_type
		FROM bead_references
		WHERE source_bead_id = ANY($1::text[])
		  AND target_bead_id = ANY($1::text[])`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("batch text references: %w", err)
	}
	defer rows.Close()

	var result []*model.BeadReference
	for rows.Next() {
		ref := &model.BeadReference{}
		var relType sql.NullString
		if err := rows.Scan(&ref.SourceBeadID, &ref.TargetBeadID, &ref.SourceField, &relType); err != nil {
			return nil, fmt.Errorf("scan text reference: %w", err)
		}
		if relType.Valid {
			s := relType.String
			ref.RelationshipType = &s
		}
		result = append(result, ref)
	}
	return result, rows.Err()
}

// relationshipTypeClause builds the WHERE-fragment that filters
// bead_references rows by relationship_type. Returns "" when filter is
// nil (no filter); otherwise appends to the running argument slice and
// returns the SQL fragment using `$N` placeholders starting at next.
//
// Filter semantics:
//   - nil              → no filter (all rows).
//   - non-nil ""       → only untyped mentions (relationship_type IS NULL).
//   - non-nil "<verb>" → exact match (relationship_type = $N).
func relationshipTypeClause(filter *string, args *[]any, next int) string {
	if filter == nil {
		return ""
	}
	if *filter == "" {
		return " AND br.relationship_type IS NULL"
	}
	*args = append(*args, *filter)
	return fmt.Sprintf(" AND br.relationship_type = $%d", next)
}

// queryListBeadsMentioning returns one row per (source bead, source field)
// that references targetID. Joined to beads(id) so the caller gets a minimal
// summary per row; soft-deleted source beads are excluded. Pagination via
// limit/offset. Also returns the unbounded total so callers can paginate.
// relationshipType applies the filter described in relationshipTypeClause.
// See kbeads/docs/design-bead-references.md § 5a and
// kbeads/docs/research-typed-mention-syntax.md § 10 (T6).
func queryListBeadsMentioning(ctx context.Context, db executor, targetID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	countArgs := []any{targetID}
	countFilter := relationshipTypeClause(relationshipType, &countArgs, 2)

	var total int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM bead_references br
		 JOIN beads b ON b.id = br.source_bead_id
		 WHERE br.target_bead_id = $1 AND b.deleted_at IS NULL`+countFilter,
		countArgs...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count mentioned-by: %w", err)
	}
	if total == 0 {
		return []*model.BeadReferenceSummary{}, 0, nil
	}

	pageArgs := []any{targetID}
	pageFilter := relationshipTypeClause(relationshipType, &pageArgs, 2)
	limitPlaceholder := len(pageArgs) + 1
	offsetPlaceholder := len(pageArgs) + 2
	pageArgs = append(pageArgs, limit, offset)

	rows, err := db.QueryContext(ctx,
		`SELECT b.id, b.title, b.type, b.status, br.source_field, br.relationship_type
		 FROM bead_references br
		 JOIN beads b ON b.id = br.source_bead_id
		 WHERE br.target_bead_id = $1 AND b.deleted_at IS NULL`+pageFilter+
			fmt.Sprintf(`
		 ORDER BY br.extracted_at DESC, b.id, br.source_field
		 LIMIT $%d OFFSET $%d`, limitPlaceholder, offsetPlaceholder),
		pageArgs...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("query mentioned-by: %w", err)
	}
	defer rows.Close()

	out := make([]*model.BeadReferenceSummary, 0)
	for rows.Next() {
		var r model.BeadReferenceSummary
		var relType sql.NullString
		if err := rows.Scan(&r.ID, &r.Title, &r.Type, &r.Status, &r.SourceField, &relType); err != nil {
			return nil, 0, fmt.Errorf("scan mentioned-by: %w", err)
		}
		if relType.Valid {
			v := relType.String
			r.RelationshipType = &v
		}
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate mentioned-by: %w", err)
	}
	return out, total, nil
}

// queryListBeadReferencesFrom returns one row per reference emitted by
// sourceID. LEFT-joined to beads(id) so dangling target IDs (typos,
// cross-fleet IDs, not-yet-created beads) still appear with empty
// Title/Type/Status — matching the table semantics that target_bead_id
// is intentionally NOT a foreign key. Pagination via limit/offset.
// relationshipType applies the filter described in relationshipTypeClause.
// See kbeads/docs/design-bead-references.md § 5a and
// kbeads/docs/research-typed-mention-syntax.md § 10 (T6).
func queryListBeadReferencesFrom(ctx context.Context, db executor, sourceID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	countArgs := []any{sourceID}
	countFilter := relationshipTypeClause(relationshipType, &countArgs, 2)

	var total int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bead_references br WHERE source_bead_id = $1`+countFilter,
		countArgs...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count mentions: %w", err)
	}
	if total == 0 {
		return []*model.BeadReferenceSummary{}, 0, nil
	}

	pageArgs := []any{sourceID}
	pageFilter := relationshipTypeClause(relationshipType, &pageArgs, 2)
	limitPlaceholder := len(pageArgs) + 1
	offsetPlaceholder := len(pageArgs) + 2
	pageArgs = append(pageArgs, limit, offset)

	rows, err := db.QueryContext(ctx,
		`SELECT br.target_bead_id,
		        COALESCE(b.title, ''),
		        COALESCE(b.type, ''),
		        COALESCE(b.status, ''),
		        br.source_field,
		        br.relationship_type
		 FROM bead_references br
		 LEFT JOIN beads b ON b.id = br.target_bead_id AND b.deleted_at IS NULL
		 WHERE br.source_bead_id = $1`+pageFilter+
			fmt.Sprintf(`
		 ORDER BY br.source_field, br.target_bead_id
		 LIMIT $%d OFFSET $%d`, limitPlaceholder, offsetPlaceholder),
		pageArgs...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("query mentions: %w", err)
	}
	defer rows.Close()

	out := make([]*model.BeadReferenceSummary, 0)
	for rows.Next() {
		var r model.BeadReferenceSummary
		var relType sql.NullString
		if err := rows.Scan(&r.ID, &r.Title, &r.Type, &r.Status, &r.SourceField, &relType); err != nil {
			return nil, 0, fmt.Errorf("scan mentions: %w", err)
		}
		if relType.Valid {
			v := relType.String
			r.RelationshipType = &v
		}
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate mentions: %w", err)
	}
	return out, total, nil
}

// countCanonicalTyped returns the number of typed references that carry a
// recognized canonical verb (i.e. would be written by
// bulkInsertTypedRefs). Used by the backfill to roll up the
// per-bead "references_written" count for telemetry.
func countCanonicalTyped(refs []references.TypedReference) int {
	type key struct {
		target string
		verb   string
	}
	seen := make(map[key]struct{}, len(refs))
	for _, r := range refs {
		if r.Verb == "" {
			continue
		}
		k := key{target: r.TargetID, verb: r.Verb}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
	}
	return len(seen)
}

// bulkInsertReferences inserts (sourceBeadID, target, sourceField) rows for
// every target in refs in a single multi-row INSERT. No-op when refs is
// empty. ON CONFLICT DO NOTHING tolerates duplicate keys defensively (the
// caller deletes first, but a concurrent re-run shouldn't error out).
func bulkInsertReferences(ctx context.Context, db executor, sourceBeadID, sourceField string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	values := make([]string, 0, len(refs))
	args := make([]any, 0, 2+len(refs))
	args = append(args, sourceBeadID, sourceField)
	for i, target := range refs {
		// $1 = source_bead_id, $2 = source_field, $3.. = targets
		values = append(values, fmt.Sprintf("($1, $%d, $2)", i+3))
		args = append(args, target)
	}
	q := `INSERT INTO bead_references (source_bead_id, target_bead_id, source_field) VALUES ` +
		strings.Join(values, ", ") +
		` ON CONFLICT DO NOTHING`
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("insert bead references: %w", err)
	}
	return nil
}

// bulkInsertTypedRefs inserts typed bead-id references — rows whose
// relationship_type column is set to the surface verb (e.g. "parent",
// "child-of", "blocks", "blocked-by") for the given source bead and
// source field. Typed mentions are emitted in addition to the flat-mention
// rows from bulkInsertReferences: a `parent(ka-X)` literal in description
// produces both a NULL-relationship row (from the bare-id Extract pass)
// and a 'parent' row here.
//
// Surface-verb storage (M0 of auto-link epic ka-0000000933): inverse
// verbs (blocked-by, child-of, duplicated-by, superseded-by, triggers)
// MUST produce distinct rows from their forward counterparts so the
// edge-direction reversal recorded by the parser survives into storage.
// Prior behavior canonicalized to model.DependencyType before insert,
// which collapsed `blocks(ka-X)` and `blocked-by(ka-X)` onto a single
// row and lost the direction. Storing the surface verb keeps both rows
// — the unique index on (source, target, field, COALESCE(rel_type, ”))
// in migration 013 already accommodates this.
//
// Inputs are grouped by surface verb so each unique verb is written in a
// single multi-row INSERT (one DB round-trip per verb per field). Unknown
// verbs (TypedReference.Verb == "") are skipped; their flat-mention row
// is already present from bulkInsertReferences. Targets are deduplicated
// within a (sourceField, verb) bucket so two `parent(ka-X)` occurrences
// in the same field collapse to one row.
//
// Iteration order over verb groups is deterministic (lexicographic on
// the surface verb) so callers — including tests — can rely on a stable
// INSERT sequence.
func bulkInsertTypedRefs(ctx context.Context, db executor, sourceBeadID, sourceField string, typed []references.TypedReference) error {
	if len(typed) == 0 {
		return nil
	}
	type bucket struct {
		// targets in first-seen order so the multi-row INSERT lays out
		// rows the way the author wrote them.
		targets []string
		seen    map[string]struct{}
	}
	groups := make(map[string]*bucket)
	for _, t := range typed {
		if t.Verb == "" {
			continue
		}
		if _, ok := references.CanonicalVerb(t.Verb); !ok {
			continue
		}
		b, exists := groups[t.Verb]
		if !exists {
			b = &bucket{seen: make(map[string]struct{})}
			groups[t.Verb] = b
		}
		if _, dup := b.seen[t.TargetID]; dup {
			continue
		}
		b.seen[t.TargetID] = struct{}{}
		b.targets = append(b.targets, t.TargetID)
	}
	if len(groups) == 0 {
		return nil
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, verb := range keys {
		targets := groups[verb].targets
		values := make([]string, 0, len(targets))
		args := make([]any, 0, 3+len(targets))
		args = append(args, sourceBeadID, sourceField, verb)
		for i, target := range targets {
			// $1 = source_bead_id, $2 = source_field, $3 = relationship_type, $4.. = targets
			values = append(values, fmt.Sprintf("($1, $%d, $2, $3)", i+4))
			args = append(args, target)
		}
		q := `INSERT INTO bead_references (source_bead_id, target_bead_id, source_field, relationship_type) VALUES ` +
			strings.Join(values, ", ") +
			` ON CONFLICT DO NOTHING`
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("insert typed bead references: %w", err)
		}
	}
	return nil
}

// closedM5DepsVerbs is the closed promotion verb set for M5/A: typed
// mentions whose surface verb appears here are auto-promoted to a row in
// the deps table in addition to their bead_references row. The set is
// intentionally narrow — only the verbs whose graph semantics are
// well-defined as direct dependencies. Other canonical verbs (related,
// duplicates, supersedes, triggered-by, …) remain bead_references-only
// in this milestone. See design doc ka-0000000827 § M5.
var closedM5DepsVerbs = map[string]struct{}{
	"parent":     {},
	"child-of":   {},
	"blocks":     {},
	"blocked-by": {},
}

// typedMentionDepCreator is the created_by stamp used on dep rows
// emitted by the typed-mention auto-link path so they can be told apart
// from operator-authored deps in the audit trail.
const typedMentionDepCreator = "kbeads/references/typed-mention"

// depRowForTypedVerb returns the (bead_id, depends_on_id, type) tuple
// for a typed mention's dep row. The convention for the deps table is
// that bead_id is the dependent (child / blocked) and depends_on_id is
// the dependency (parent / blocker). Forward verbs (parent, blocks)
// flip differently: parent(X) keeps the source as child of X, while
// blocks(X) flips so X becomes the dependent of source. Inverse verbs
// (child-of, blocked-by) write the row in the natural source-as-bead_id
// orientation and so do not flip. Returns ok=false for verbs outside
// the closed set so the caller can skip cleanly.
func depRowForTypedVerb(verb, sourceBeadID, targetBeadID string) (beadID, dependsOnID string, t model.DependencyType, ok bool) {
	switch verb {
	case "parent", "child-of":
		return sourceBeadID, targetBeadID, model.DepParentChild, true
	case "blocks":
		return targetBeadID, sourceBeadID, model.DepBlocks, true
	case "blocked-by":
		return sourceBeadID, targetBeadID, model.DepBlocks, true
	}
	return "", "", "", false
}

// bulkInsertTypedDeps emits one dep row per typed mention whose surface
// verb is in the closed M5 promotion set (closedM5DepsVerbs). Direction
// follows depRowForTypedVerb so a forward `blocks(X)` and an inverse
// `blocked-by(X)` from the same source produce the same dep row — this
// is the M0 surface/edge-direction fix carried into deps. Self-references
// (target == source) and duplicate (bead, depends_on, type) tuples are
// dropped before insert.
//
// Idempotency relies on the deps PK (bead_id, depends_on_id, type) plus
// `ON CONFLICT DO NOTHING`; a second call with the same input is a no-op
// at the DB level. FK violations (the target bead doesn't exist —
// dangling ka-id, typo, or cross-fleet reference) are non-fatal: the
// dep is skipped, the bead_references row remains. Other DB errors
// abort the call.
//
// Each dep INSERT runs inside a per-statement SAVEPOINT so an FK
// violation only rolls back that one failed INSERT, not the shared
// bead-write transaction this typically runs on. Without it a dangling
// target would abort the whole tx (Postgres marks a tx failed after any
// error — SQLSTATE 25P02), silently rolling back the bead INSERT/UPDATE
// and its bead_references despite the FK-`continue` here (ka-0000000195).
//
// The fleet column is inherited from the source bead via the same
// COALESCE subquery used by queryAddDependency, so cross-fleet writes
// follow the existing rules.
//
// Empty input is a no-op.
//
// Operator inheritance: deps.operated_by_id is NOT NULL post-cutover
// (migration 022). The dep is the consequence of an author writing a
// typed mention in the source bead, so operated_by{_id} are inherited
// from the source bead/comment passed in by the caller. If the source
// has no operator FK (operatedByID is nil), we use the
// system:legacy-pre-attribution sentinel as the fallback, allowing typed
// mentions to be mirrored into deps even when operatedByID is not set
// (ka-0000002664). This ensures bead_query conditions in gardener loop_end
// correctly count children even if typed mentions haven't been backfilled.
func bulkInsertTypedDeps(ctx context.Context, db executor, sourceBeadID string, operatedByID *int64, typed []references.TypedReference) error {
	if len(typed) == 0 {
		return nil
	}
	type key struct {
		beadID      string
		dependsOnID string
		t           model.DependencyType
	}
	seen := make(map[key]struct{}, len(typed))
	now := time.Now().UTC()
	for _, ref := range typed {
		if ref.Verb == "" {
			continue
		}
		if _, ok := closedM5DepsVerbs[ref.Verb]; !ok {
			continue
		}
		beadID, dependsOnID, depType, ok := depRowForTypedVerb(ref.Verb, sourceBeadID, ref.TargetID)
		if !ok {
			continue
		}
		if beadID == dependsOnID {
			// Self-reference — skip rather than write a self-loop dep.
			continue
		}
		k := key{beadID: beadID, dependsOnID: dependsOnID, t: depType}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}

		// Use the provided operatedByID if available; otherwise use a subquery
		// to fetch the system:legacy-pre-attribution sentinel (fallback for
		// beads with no operator FK). This ensures typed mentions are mirrored
		// into deps even when operatedByID is nil.
		//
		// The operator-FK column and the metadata column must use placeholder
		// numbers that line up with the args actually appended below. When
		// operatedByID is non-nil it is bound as $6 and metadata is $7. When
		// operatedByID is nil the column is filled by the inline subquery (no
		// placeholder consumed), so metadata shifts back to $6 — otherwise the
		// query references $7 while only six args are supplied and lib/pq
		// rejects the bind with a non-FK error that 500s the outer write.
		var operatedByIDValue, metadataPlaceholder string
		if operatedByID != nil {
			operatedByIDValue = "$6"
			metadataPlaceholder = "$7"
		} else {
			operatedByIDValue = "(SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution')"
			metadataPlaceholder = "$6"
		}

		query := fmt.Sprintf(`
			INSERT INTO deps (bead_id, depends_on_id, type, created_at, created_by, operated_by_id, metadata, fleet)
			VALUES (
			    $1, $2, $3, $4, $5, %s, %s,
			    COALESCE((SELECT fleet FROM beads WHERE id = $1), 'default')
			)
			ON CONFLICT (bead_id, depends_on_id, type) DO NOTHING`,
			operatedByIDValue,
			metadataPlaceholder,
		)

		var args []any
		args = append(args,
			beadID,
			dependsOnID,
			string(depType),
			now,
			typedMentionDepCreator,
		)
		if operatedByID != nil {
			args = append(args, *operatedByID)
		}
		args = append(args, []byte("{}"))

		// Wrap the dep INSERT in a SAVEPOINT. deps.depends_on_id /
		// deps.bead_id are `REFERENCES beads(id)` and NOT DEFERRABLE
		// (migration 001), so a dangling target raises a 23503
		// foreign_key_violation that, in Postgres, ABORTS the entire
		// enclosing transaction (SQLSTATE 25P02 on every subsequent
		// statement until ROLLBACK). Because this runs on the SHARED
		// bead-write tx (CreateBead / UpdateBead / reextract), a bare
		// `continue` on FK error does NOT skip the dangling dep — it
		// leaves the tx poisoned so the bead INSERT/UPDATE and its
		// bead_references all roll back at commit (ka-0000000195).
		//
		// A per-INSERT savepoint makes the documented "skip the dangling
		// dep, keep the bead + its other deps" behavior real: on FK
		// violation we ROLLBACK TO the savepoint (which recovers the tx
		// from the aborted state) and continue; on success we RELEASE it.
		// The savepoint name is reused each iteration — after a RELEASE or
		// ROLLBACK TO, re-establishing the same name is well-defined.
		if _, err := db.ExecContext(ctx, "SAVEPOINT typed_dep_sp"); err != nil {
			return fmt.Errorf("savepoint before typed-mention dep (%s -> %s, %s): %w", beadID, dependsOnID, depType, err)
		}
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			if isFKViolation(err) {
				// Dangling target — bead_references row is fine to keep,
				// but the deps FK won't allow the row. Roll back just this
				// failed INSERT so the shared tx stays alive, then skip.
				if _, rbErr := db.ExecContext(ctx, "ROLLBACK TO SAVEPOINT typed_dep_sp"); rbErr != nil {
					return fmt.Errorf("rollback to savepoint after dangling typed-mention dep (%s -> %s, %s): %w", beadID, dependsOnID, depType, rbErr)
				}
				continue
			}
			// Non-FK error: the tx is aborted. Return so the caller rolls
			// back the whole transaction; no RELEASE is needed.
			return fmt.Errorf("insert typed-mention dep (%s -> %s, %s): %w", beadID, dependsOnID, depType, err)
		}
		if _, err := db.ExecContext(ctx, "RELEASE SAVEPOINT typed_dep_sp"); err != nil {
			return fmt.Errorf("release savepoint after typed-mention dep (%s -> %s, %s): %w", beadID, dependsOnID, depType, err)
		}
	}
	return nil
}

// reconcileTypedMentionDeps DELETEs parser-created parent-child dep rows for the
// source bead whose `parent:`/`child-of:` typed mention no longer exists in any
// live source field. This is Proposal B (ka-0000006581): it makes edge removal
// automatic when the mention text is deleted, so re-parenting no longer needs
// the `kd reparent` command to orchestrate the dep delete. Full design +
// hazard/guardrail matrix: kbeads/docs/design-reconcile-typed-mention-deps.md.
//
// SCOPE — parent-child edges ONLY. A parent-child dep row (bead_id=source,
// depends_on_id=parent) is the only dep-row shape whose sole possible parser
// source is the source bead's own text: `parent`/`child-of` are the only verbs
// that emit type=parent-child, and depRowForTypedVerb sets bead_id=source for
// both, so NO other bead's text can produce this row. That makes single-bead
// reconcile unambiguous. blocks/blocked-by rows are multi-source — either
// endpoint's text can mint the same row (`blocks(X)` in B and `blocked-by(B)`
// in X produce the identical (X,B,blocks) row) — so a single write cannot
// decide staleness. They keep today's additive-only behavior.
//
// GUARDRAILS:
//   - created_by scope: only rows stamped typedMentionDepCreator are eligible;
//     manual `kd dep add` rows (created_by=<actor>) are never deleted. The
//     dual-assertion hazard (a manual add that no-op'd on an existing parser
//     row and left the marker) is handled at the add path, which now CLAIMS
//     the row for the manual actor (queryAddDependency's ON CONFLICT DO UPDATE).
//   - comment immutability: when extractComments is on, comment text also mints
//     parent-child dep rows (replaceCommentReferences). Comments cannot be
//     edited, so the desired-target set UNIONS the parent-edge targets named by
//     every live comment, preserving comment-sourced edges.
//
// Runs in the caller's transaction (the bead-write / per-bead reextract tx) so
// the edge delete is atomic with the text change that made it stale.
func reconcileTypedMentionDeps(ctx context.Context, db executor, b *model.Bead, extractComments bool) error {
	liveParents := make(map[string]struct{})
	for _, field := range []string{b.Title, b.Description, b.Notes} {
		for _, target := range references.ParentEdgeTargetsExcept(field, "") {
			liveParents[target] = struct{}{}
		}
	}
	if extractComments {
		rows, err := db.QueryContext(ctx,
			`SELECT text FROM comments WHERE bead_id = $1`, b.ID)
		if err != nil {
			return fmt.Errorf("reconcile: list comments for %s: %w", b.ID, err)
		}
		var commentTexts []string
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				rows.Close()
				return fmt.Errorf("reconcile: scan comment for %s: %w", b.ID, err)
			}
			commentTexts = append(commentTexts, text)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("reconcile: iterate comments for %s: %w", b.ID, err)
		}
		rows.Close()
		for _, text := range commentTexts {
			for _, target := range references.ParentEdgeTargetsExcept(text, "") {
				liveParents[target] = struct{}{}
			}
		}
	}

	keep := make([]string, 0, len(liveParents))
	for target := range liveParents {
		keep = append(keep, target)
	}
	sort.Strings(keep) // deterministic bind order (sqlmock/test stability)

	// DELETE parser-created parent-child rows for this source bead whose parent
	// (depends_on_id) is no longer named by any live field. `<> ALL($4::text[])`
	// is vacuously TRUE for an empty array, so an empty keep set removes every
	// parser-created parent-child row for the bead — the correct behavior when
	// the last parent mention was deleted. Rows with a live mention are in keep
	// and survive; manual (non-marker) rows are excluded by the created_by
	// predicate.
	if _, err := db.ExecContext(ctx, `
		DELETE FROM deps
		WHERE bead_id = $1
		  AND type = $2
		  AND created_by = $3
		  AND depends_on_id <> ALL($4::text[])`,
		b.ID, string(model.DepParentChild), typedMentionDepCreator, pq.Array(keep),
	); err != nil {
		return fmt.Errorf("reconcile typed-mention parent-child deps for %s: %w", b.ID, err)
	}
	return nil
}
