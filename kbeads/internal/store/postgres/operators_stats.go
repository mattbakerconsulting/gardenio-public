package postgres

import (
	"context"
	"database/sql"

	"gardenio/kbeads/internal/store"
)

// defaultOperatorStatsRecentPerOperator is the per-operator cap on the
// "recently created" chip list when the caller passes <=0. Mirrors the
// beads-viewer's peopleRecentBeadCap — the directory is a check-in surface,
// not a feed.
const defaultOperatorStatsRecentPerOperator = 5

// defaultOperatorStatsRecentCeiling is the hard ceiling on the TOTAL number
// of recent-chip rows returned across all operators when the caller passes
// <=0. Bounds the single chip query so it can never fan out unbounded; it is
// NOT a cap on the open-count / last-active aggregate (which is a true
// full-table GROUP BY).
const defaultOperatorStatsRecentCeiling = 5000

// defaultOperatorStatsRecentWindowHours is the age bound on the "recently
// created" chips when the caller passes <=0 (ka-0000001991). Before this the
// chip query carried NO time window at all, so a bead raised in March sat on a
// card reading as current work and the directory could not answer "what has
// this person actually done lately" — the operator-reported "old issues and
// threads show up more prominently than they should, seemingly remaining
// current despite irrelevance".
//
// 7 days is the DEFAULT, not the only scope: the /people UI asks for 24h to
// answer "what did users do in the last day", and the wider default here keeps
// a direct API caller (and any future scope toggle) from silently getting the
// tightest window. Callers pass their own window explicitly.
const defaultOperatorStatsRecentWindowHours = 24 * 7

// OperatorDirectoryStats returns real per-operator aggregates for the
// /people directory (ka-0000001390). See the store.Store interface doc for
// the contract; this is the *PostgresStore receiver. Two queries run on the
// shared db handle:
//
//  1. A GROUP BY operated_by_id over the whole (non-deleted) beads table:
//     open/in_progress count, the in_progress slice, 7d created/closed
//     throughput, MR open/merged-7d, pending decisions, active
//     agents/gardeners, + max(updated_at) last-active. No sampling, no row
//     cap — these are the true totals the old 5000-bead sample only
//     approximated (design §4.1, ka-0000002003).
//  2. An age-bounded per-operator "recently created" chip query keyed on
//     operated_by_id, capped at recentPerOperator rows per operator and
//     recentCeiling rows total, restricted to the last recentWindowHours, and
//     excluding agent/session/trigger scaffolding.
//
// Alias-awareness: operated_by_id is the canonical operator id the write-path
// resolver bound through operator_aliases, so every linked identity folds into
// one row. Unlinked duplicate identities (a known data bug, cleaned up by a
// sibling task) surface as separate ids here and are folded for display by the
// beads-viewer's render-time group.
func (s *PostgresStore) OperatorDirectoryStats(ctx context.Context, recentPerOperator, recentCeiling, recentWindowHours int) (*store.OperatorDirectoryStats, error) {
	return operatorDirectoryStats(ctx, s.db, recentPerOperator, recentCeiling, recentWindowHours)
}

func (s *txStore) OperatorDirectoryStats(ctx context.Context, recentPerOperator, recentCeiling, recentWindowHours int) (*store.OperatorDirectoryStats, error) {
	return operatorDirectoryStats(ctx, s.tx, recentPerOperator, recentCeiling, recentWindowHours)
}

func operatorDirectoryStats(ctx context.Context, db executor, recentPerOperator, recentCeiling, recentWindowHours int) (*store.OperatorDirectoryStats, error) {
	if recentPerOperator <= 0 {
		recentPerOperator = defaultOperatorStatsRecentPerOperator
	}
	if recentCeiling <= 0 {
		recentCeiling = defaultOperatorStatsRecentCeiling
	}
	if recentWindowHours <= 0 {
		recentWindowHours = defaultOperatorStatsRecentWindowHours
	}
	out := &store.OperatorDirectoryStats{}

	// (1) Real per-operator open-count + last-active. A full GROUP BY over
	// every non-deleted bead — the whole point of this task is that this is
	// NOT a sample. The partial index idx_beads_operated_by_id (migration
	// 015) covers the operated_by_id IS NOT NULL scan. A superset of COUNT(*)
	// FILTER columns keeps every per-row stat — open/in_progress count, the
	// in_progress slice, 7d created/closed throughput, MR open/merged,
	// pending decisions, and active agents/gardeners — plus the recency max in
	// one pass (design §4.1, ka-0000002003). The 7d windows use
	// now() - interval '7 days' server-side so the counts are clock-consistent.
	//
	// last_active is deliberately a FILTERED max over HUMAN-authored bead
	// types only (ka-0000001991). Without the filter it spanned every row
	// attributed to the operator, including machine scaffolding the human
	// never touched: type='agent' pods heartbeat live_turns/context_tokens
	// every turn, type='session' rows stamp themselves, and type='trigger'
	// gardener loops bump last_fired_at each cycle. That rendered "active 2m
	// ago" on /people for a person whose last real action was days earlier —
	// measured on prod, 7 of 15 active humans had a machine bead as their
	// recency driver, one of them 5 days stale. The chip query below already
	// excluded agent/session; this aggregate simply never got the same screen.
	// It is a FILTER, not a WHERE, because the agents_active / gardeners_active
	// counts above must keep seeing those very rows — the fleet-footprint
	// chips are about the machines, the recency is about the human.
	statRows, err := db.QueryContext(ctx, `
		SELECT operated_by_id,
		       COUNT(*) FILTER (WHERE status IN ('open','in_progress'))::int AS open_beads,
		       COUNT(*) FILTER (WHERE status = 'in_progress')::int AS in_progress,
		       COUNT(*) FILTER (WHERE created_at > now() - interval '7 days')::int AS created_7d,
		       COUNT(*) FILTER (WHERE status = 'closed' AND closed_at > now() - interval '7 days')::int AS closed_7d,
		       COUNT(*) FILTER (WHERE type = 'merge-request' AND status IN ('open','in_progress'))::int AS mrs_open,
		       COUNT(*) FILTER (WHERE type = 'merge-request' AND status = 'closed' AND closed_at > now() - interval '7 days')::int AS mrs_merged_7d,
		       COUNT(*) FILTER (WHERE type = 'decision' AND status = 'open')::int AS decisions_pending,
		       COUNT(*) FILTER (WHERE type = 'agent' AND status IN ('open','in_progress'))::int AS agents_active,
		       COUNT(*) FILTER (WHERE type = 'trigger' AND status IN ('open','in_progress'))::int AS gardeners_active,
		       MAX(updated_at) FILTER (WHERE type NOT IN ('agent','session','trigger')) AS last_active
		  FROM beads
		 WHERE operated_by_id IS NOT NULL
		   AND deleted_at IS NULL
		 GROUP BY operated_by_id`)
	if err != nil {
		return nil, err
	}
	defer statRows.Close()
	for statRows.Next() {
		var st store.OperatorStat
		// last_active is NULL for an operator whose every bead is machine
		// scaffolding (the FILTER above matched no row) — scan through a
		// NullTime so that operator reports the zero time ("no human
		// activity") instead of failing the whole query.
		var lastActive sql.NullTime
		if err := statRows.Scan(
			&st.OperatorID,
			&st.OpenBeads,
			&st.InProgress,
			&st.Created7d,
			&st.Closed7d,
			&st.MRsOpen,
			&st.MRsMerged7d,
			&st.DecisionsPending,
			&st.AgentsActive,
			&st.GardenersActive,
			&lastActive,
		); err != nil {
			return nil, err
		}
		if lastActive.Valid {
			st.LastActive = lastActive.Time
		}
		out.Stats = append(out.Stats, st)
	}
	if err := statRows.Err(); err != nil {
		return nil, err
	}

	// (2) Per-operator recently-created chips. ROW_NUMBER() partitioned by
	// the operator gives each one its top-N most-recently-created beads; the
	// outer LIMIT is the hard ceiling on total rows so the query can never fan
	// out unbounded. Machine scaffolding is excluded (same type screen as the
	// recency aggregate above) so the chips reflect real work. created_at
	// DESC, id DESC is a stable order so chips reflect genuine creation
	// recency rather than update churn.
	//
	// Keyed on operated_by_id, NOT created_by_operator_id (ka-0000001991).
	// created_by_operator_id is the semantically nicer column — the immortal
	// originator, which does not flip on a dibs handoff — but NOTHING on the
	// normal write path populates it: CreateBead sets only OperatedByID, and
	// the sole writer of CreatedByOperatorID is user provisioning. Migration
	// 018 backfilled what history it could from an alias hint and every write
	// since has landed NULL, so this query returned ONE row fleet-wide and the
	// chip strip under every other card rendered empty — the operator-visible
	// "recent interactions don't show up" bug. Measured on prod: 200/200 beads
	// created since 08-10 had created_by_operator_id NULL and operated_by_id
	// populated. Re-keying to the column the write path actually fills is what
	// makes the strip real; it also matches how the sibling recent-requests
	// scan (beads-viewer lookupRecentThreadMessages) already attributes. If
	// the originator column is ever populated on the create path, this can
	// move back — the ordering and caps are unaffected.
	recentRows, err := db.QueryContext(ctx, `
		SELECT operated_by_id, id, title, created_at
		  FROM (
		    SELECT operated_by_id, id, title, created_at,
		           ROW_NUMBER() OVER (
		             PARTITION BY operated_by_id
		             ORDER BY created_at DESC, id DESC
		           ) AS rn
		      FROM beads
		     WHERE operated_by_id IS NOT NULL
		       AND deleted_at IS NULL
		       AND type NOT IN ('agent','session','trigger')
		       AND created_at >= now() - ($3 || ' hours')::interval
		  ) ranked
		 WHERE rn <= $1
		 ORDER BY operated_by_id, created_at DESC, id DESC
		 LIMIT $2`, recentPerOperator, recentCeiling, recentWindowHours)
	if err != nil {
		return nil, err
	}
	defer recentRows.Close()
	for recentRows.Next() {
		var rb store.OperatorRecentBead
		if err := recentRows.Scan(&rb.OperatorID, &rb.BeadID, &rb.Title, &rb.CreatedAt); err != nil {
			return nil, err
		}
		out.Recent = append(out.Recent, rb)
	}
	if err := recentRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}
