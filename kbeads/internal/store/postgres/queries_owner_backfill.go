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
	"github.com/lib/pq"
)

// queryBackfillOwner implements `kd admin backfill-owner
// --rebind-matt-default` (ka-0000006961 / C4 of epic ka-0000004247).
//
// Reverses the ka-0000000269 damage: the old "verbatim X-Operated-By →
// Owner" default stamped owner='slack:U0OPERATOR1' onto every spawn-path
// bead regardless of whether Matt actually originated the work. This
// sweep walks the spawn chain for each candidate and either rebinds the
// row to the real human or to the system:unowned sentinel.
//
// Per kbeads/CLAUDE.md §Migration policy — chunked, idempotent, every
// UPDATE filters `WHERE owner = target` so re-runs on already-cleaned
// data are no-ops. Each batch is its own transaction.
func queryBackfillOwner(ctx context.Context, db *sql.DB, opts store.BackfillOwnerOptions) (*store.BackfillOwnerStats, error) {
	start := time.Now()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	stats := &store.BackfillOwnerStats{
		DryRun:      opts.DryRun,
		Target:      opts.Target,
		Since:       opts.Since,
		RebindCount: map[string]int64{},
	}

	before, err := countOwnerTarget(ctx, db, opts.Target, opts.Since)
	if err != nil {
		return nil, fmt.Errorf("count before: %w", err)
	}
	stats.BeforeCandidates = before

	lastID := ""
	for {
		if opts.MaxBatches > 0 && stats.Batches >= opts.MaxBatches {
			stats.BatchesCapped = true
			break
		}
		batch, err := fetchOwnerCandidates(ctx, db, opts.Target, opts.Since, lastID, opts.BatchSize)
		if err != nil {
			return stats, fmt.Errorf("fetch batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		lastID = batch[len(batch)-1].id
		stats.LastID = lastID
		stats.CandidateBeads += int64(len(batch))
		stats.Batches++

		decisions := make([]ownerRebindDecision, 0, len(batch))
		for _, b := range batch {
			newOwner, kind := resolveOwnerForBead(ctx, db, b, opts.Target, opts.UnownedSentinel)
			decisions = append(decisions, ownerRebindDecision{
				id:       b.id,
				newOwner: newOwner,
				kind:     kind,
			})
		}

		if opts.DryRun {
			for _, d := range decisions {
				switch d.kind {
				case ownerDecisionKept:
					stats.KeptMattCount++
				case ownerDecisionUnresolved:
					stats.UnresolvedCount++
					stats.RebindCount[d.newOwner]++
				case ownerDecisionRebound:
					stats.RebindCount[d.newOwner]++
				}
			}
		} else {
			if err := applyOwnerRebinds(ctx, db, opts.Target, decisions, stats); err != nil {
				return stats, fmt.Errorf("apply batch: %w", err)
			}
		}

		if len(batch) < opts.BatchSize {
			break
		}
	}

	after, err := countOwnerTarget(ctx, db, opts.Target, opts.Since)
	if err != nil {
		return stats, fmt.Errorf("count after: %w", err)
	}
	stats.AfterCandidates = after
	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// ownerDecisionKind tags how a candidate row was classified.
type ownerDecisionKind int

const (
	// ownerDecisionKept — spawn-chain root genuinely resolves to Target;
	// leave the row alone.
	ownerDecisionKept ownerDecisionKind = iota
	// ownerDecisionRebound — spawn-chain walk resolved to a different
	// human; rebind to that human.
	ownerDecisionRebound
	// ownerDecisionUnresolved — no human reachable; rebind to the
	// UnownedSentinel.
	ownerDecisionUnresolved
)

type ownerRebindDecision struct {
	id       string
	newOwner string
	kind     ownerDecisionKind
}

// ownerCandidateRow is one row scanned in the owner-rebind sweep.
type ownerCandidateRow struct {
	id        string
	createdBy string
	fields    json.RawMessage
}

// countOwnerTarget returns the number of non-deleted beads currently bound
// to owner=target with created_at >= since.
func countOwnerTarget(ctx context.Context, db *sql.DB, target string, since time.Time) (int64, error) {
	if since.IsZero() {
		var n int64
		err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM beads
			 WHERE owner = $1
			   AND deleted_at IS NULL`,
			target,
		).Scan(&n)
		return n, err
	}
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM beads
		 WHERE owner = $1
		   AND created_at >= $2
		   AND deleted_at IS NULL`,
		target, since,
	).Scan(&n)
	return n, err
}

func fetchOwnerCandidates(
	ctx context.Context,
	db *sql.DB,
	target string,
	since time.Time,
	afterID string,
	limit int,
) ([]ownerCandidateRow, error) {
	if since.IsZero() {
		return fetchOwnerCandidatesNoSince(ctx, db, target, afterID, limit)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(created_by, ''), COALESCE(fields, '{}'::jsonb)
		  FROM beads
		 WHERE owner = $1
		   AND created_at >= $2
		   AND deleted_at IS NULL
		   AND id > $3
		 ORDER BY id
		 LIMIT $4`,
		target, since, afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ownerCandidateRow
	for rows.Next() {
		var r ownerCandidateRow
		if err := rows.Scan(&r.id, &r.createdBy, &r.fields); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func fetchOwnerCandidatesNoSince(
	ctx context.Context,
	db *sql.DB,
	target string,
	afterID string,
	limit int,
) ([]ownerCandidateRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(created_by, ''), COALESCE(fields, '{}'::jsonb)
		  FROM beads
		 WHERE owner = $1
		   AND deleted_at IS NULL
		   AND id > $2
		 ORDER BY id
		 LIMIT $3`,
		target, afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ownerCandidateRow
	for rows.Next() {
		var r ownerCandidateRow
		if err := rows.Scan(&r.id, &r.createdBy, &r.fields); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resolveOwnerForBead walks the spawn chain for one bead and returns the
// new owner value plus a decision kind. Algorithm:
//
//  1. If the bead's created_by is a "thread-…" id, read this bead's own
//     fields.slack_user_id (the slack-spawn handler writes the originating
//     user there). When non-empty AND != target subject, return
//     "slack:<uid>".
//  2. Otherwise treat created_by as an agent slug and walk the spawn
//     chain: find agent bead by slug (id, title, or fields.agent), read
//     fields.spawned_by, repeat to root (≤ 16 hops, cycle-safe).
//  3. From the root agent extract the human via fields.user_bead_id →
//     user bead (oidc:<email> or slack:<id>), fields.slack_user_id, or
//     fields.operated_by (rejecting sentinels).
//  4. If resolution lands on target, return ownerDecisionKept.
//     If resolution yields any other human, return ownerDecisionRebound.
//     Otherwise return ownerDecisionUnresolved + UnownedSentinel.
func resolveOwnerForBead(
	ctx context.Context,
	db *sql.DB,
	b ownerCandidateRow,
	target string,
	unownedSentinel string,
) (string, ownerDecisionKind) {
	resolved := walkSpawnChainForOwner(ctx, db, b)
	if resolved == "" {
		return unownedSentinel, ownerDecisionUnresolved
	}
	if resolved == target {
		return target, ownerDecisionKept
	}
	return resolved, ownerDecisionRebound
}

// ownerSpawnChainCap bounds the agent-slug walk so a malformed graph can
// not spin a goroutine.
const ownerSpawnChainCap = 16

// walkSpawnChainForOwner returns the canonical human identity for the
// candidate bead's spawn chain root, or "" when nothing resolves.
func walkSpawnChainForOwner(ctx context.Context, db *sql.DB, b ownerCandidateRow) string {
	fields := decodeFieldsOwner(b.fields)

	// Pass A: Slack-thread agent → read the originating user off this
	// bead's own fields. The slack-spawn handler populates slack_user_id
	// on every thread-prefixed agent bead and on every bead it creates
	// from a thread.
	if strings.HasPrefix(b.createdBy, "thread-") {
		if v := slackUserIDFromFields(fields); v != "" {
			return "slack:" + v
		}
		// Some thread agents stash the trigger subject instead.
		if v, _ := fields["trigger_created_by_subject"].(string); strings.TrimSpace(v) != "" {
			return "slack:" + strings.TrimSpace(v)
		}
		return ""
	}

	// Pass B: scheduler / trigger / static / bridge creators carry no
	// agent chain we can walk; fall through to the bead's own fields.
	if b.createdBy == "" || b.createdBy == "static" || isOwnerSentinelSpawner(b.createdBy) {
		return humanFromFieldsForOwner(ctx, db, fields)
	}

	// Pass C: created_by is an agent slug. Walk fields.spawned_by to the
	// chain root, then resolve a human off the root agent bead.
	visited := map[string]bool{}
	cur := b.createdBy
	var rootFields map[string]any
	for i := 0; i < ownerSpawnChainCap; i++ {
		if visited[cur] {
			break
		}
		visited[cur] = true
		agentBead, err := lookupAgentBySlugOrID(ctx, db, cur)
		if err != nil || agentBead == nil {
			break
		}
		rootFields = decodeFieldsOwner(agentBead.fields)
		spawner, _ := rootFields["spawned_by"].(string)
		spawner = strings.TrimSpace(spawner)
		if spawner == "" || isOwnerSentinelSpawner(spawner) {
			break
		}
		cur = spawner
	}
	if rootFields == nil {
		// Couldn't find any agent bead — try this bead's own fields as a
		// last resort (the create-time inheritance may have copied them).
		return humanFromFieldsForOwner(ctx, db, fields)
	}
	if v := humanFromFieldsForOwner(ctx, db, rootFields); v != "" {
		return v
	}
	// Root agent had no resolvable human — fall through to the candidate
	// bead's own fields. Some old spawn paths copied slack_user_id onto
	// child beads but not onto the agent's own fields.
	return humanFromFieldsForOwner(ctx, db, fields)
}

// ownerAgentBead is the minimal agent-bead projection used by the walker.
type ownerAgentBead struct {
	id     string
	fields json.RawMessage
}

// lookupAgentBySlugOrID resolves an agent bead by its slug. Tries
// fields.agent first (canonical), then title, then id. Returns (nil, nil)
// when no row matches. Soft-deleted rows are excluded.
func lookupAgentBySlugOrID(ctx context.Context, db *sql.DB, slug string) (*ownerAgentBead, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, nil
	}
	// Prefer the fields.agent expression index path (idx_beads_fields_agent
	// migration 025). Falls back to title / id for older agent beads that
	// used the slug as the bead id directly.
	row := db.QueryRowContext(ctx, `
		SELECT id, COALESCE(fields, '{}'::jsonb)
		  FROM beads
		 WHERE type = 'agent'
		   AND deleted_at IS NULL
		   AND (fields->>'agent' = $1 OR title = $1 OR id = $1)
		 ORDER BY updated_at DESC
		 LIMIT 1`,
		slug,
	)
	var b ownerAgentBead
	if err := row.Scan(&b.id, &b.fields); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// humanFromFieldsForOwner returns the canonical human identity reachable
// from an agent (or bead) fields map, or "" when no field resolves.
func humanFromFieldsForOwner(ctx context.Context, db *sql.DB, fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	if ubID, _ := fields["user_bead_id"].(string); strings.TrimSpace(ubID) != "" {
		if v := humanFromUserBead(ctx, db, strings.TrimSpace(ubID)); v != "" {
			return v
		}
	}
	if v := slackUserIDFromFields(fields); v != "" {
		return "slack:" + v
	}
	if v, _ := fields["operated_by"].(string); strings.TrimSpace(v) != "" {
		v = strings.TrimSpace(v)
		if isHumanShapedOperator(v) {
			return v
		}
	}
	return ""
}

// humanFromUserBead dereferences a user_bead_id pointer and returns the
// canonical human identity from its fields. Prefers oidc:<email>, falls
// back to slack:<slack_user_id>.
func humanFromUserBead(ctx context.Context, db *sql.DB, userBeadID string) string {
	row := db.QueryRowContext(ctx, `
		SELECT COALESCE(fields, '{}'::jsonb)
		  FROM beads
		 WHERE id = $1
		   AND deleted_at IS NULL
		 LIMIT 1`,
		userBeadID,
	)
	var raw json.RawMessage
	if err := row.Scan(&raw); err != nil {
		return ""
	}
	fields := decodeFieldsOwner(raw)
	if email, _ := fields["email"].(string); strings.TrimSpace(email) != "" {
		return "oidc:" + strings.TrimSpace(email)
	}
	if sl := slackUserIDFromFields(fields); sl != "" {
		return "slack:" + sl
	}
	return ""
}

// slackUserIDFromFields extracts the slack_user_id field from a fields map.
// Accepts slack_user_id (canonical) or slack_id (legacy on user beads).
func slackUserIDFromFields(fields map[string]any) string {
	if v, _ := fields["slack_user_id"].(string); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, _ := fields["slack_id"].(string); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// isOwnerSentinelSpawner reports whether a spawned_by / created_by value
// is a non-agent sentinel (scheduler/trigger/bridge/system/fleet). These
// end the spawn-chain walk.
func isOwnerSentinelSpawner(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "scheduler:") {
		return true
	}
	if strings.HasPrefix(s, "trigger:") {
		return true
	}
	if strings.HasPrefix(s, "escalation-bridge") {
		return true
	}
	if strings.HasPrefix(s, "system:") {
		return true
	}
	if strings.HasPrefix(s, "fleet:") {
		return true
	}
	return false
}

// isHumanShapedOperator reports whether v looks like a human operator id
// (slack:U…, oidc:…@…) rather than a sentinel (system:…, fleet:…) or
// agent-shaped identifier. The check is intentionally lenient — we
// rebind to a human identity even when the prefix is non-standard, as
// long as it is clearly not a sentinel.
func isHumanShapedOperator(v string) bool {
	if v == "" {
		return false
	}
	if isOwnerSentinelSpawner(v) {
		return false
	}
	// "slack:U..." or "oidc:foo@bar" — accept anything with a colon that
	// is not a known sentinel.
	if strings.Contains(v, ":") {
		return true
	}
	// Bare values (no prefix) are too ambiguous; treat as non-human.
	return false
}

func decodeFieldsOwner(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

// applyOwnerRebinds executes one batch's rebinds inside a single
// transaction. Decisions of kind ownerDecisionKept are skipped (no
// write). For each kept/rebound/unresolved row we write a
// mutation_history audit entry so the rebind is traceable.
func applyOwnerRebinds(
	ctx context.Context,
	db *sql.DB,
	target string,
	decisions []ownerRebindDecision,
	stats *store.BackfillOwnerStats,
) error {
	// Group rebind ids by destination owner so each UPDATE rebinds a set
	// of rows in one round trip.
	groups := map[string][]string{}
	keptIDs := []string{}
	for _, d := range decisions {
		switch d.kind {
		case ownerDecisionKept:
			keptIDs = append(keptIDs, d.id)
			stats.KeptMattCount++
		case ownerDecisionRebound, ownerDecisionUnresolved:
			groups[d.newOwner] = append(groups[d.newOwner], d.id)
			if d.kind == ownerDecisionUnresolved {
				stats.UnresolvedCount++
			}
		}
	}
	_ = keptIDs // reserved for future audit; we already account for it in stats.

	if len(groups) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback; no-op after Commit().

	for newOwner, ids := range groups {
		res, err := tx.ExecContext(ctx, `
			UPDATE beads
			   SET owner = $1
			 WHERE id = ANY($2::text[])
			   AND owner = $3`,
			newOwner, pq.Array(ids), target,
		)
		if err != nil {
			return fmt.Errorf("update owner -> %q: %w", newOwner, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				id,
				"kd admin backfill-owner",
				"owner",
				target, newOwner, "backfill-owner-matt-default",
			); err != nil {
				return fmt.Errorf("mutation_history %s: %w", id, err)
			}
		}
		stats.RebindCount[newOwner] += n
	}
	return tx.Commit()
}
