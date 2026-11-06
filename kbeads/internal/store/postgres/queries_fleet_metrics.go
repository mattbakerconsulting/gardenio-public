package postgres

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// queryFleetTypeStatusCounts returns COUNT(*) grouped by (type, status) for
// all live beads (deleted_at IS NULL). Result cardinality is bounded by the
// product of distinct types and statuses (a few dozen rows). This replaces
// the 50K-bead full-row scan that fleetCollector.fetchSnapshot used to do
// per Prometheus scrape (ka-0000006525).
func queryFleetTypeStatusCounts(ctx context.Context, db executor) (map[store.FleetTypeStatusKey]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT type, status, COUNT(*)
		FROM beads
		WHERE deleted_at IS NULL
		GROUP BY type, status`)
	if err != nil {
		return nil, fmt.Errorf("fleet type/status counts: %w", err)
	}
	defer rows.Close()

	result := make(map[store.FleetTypeStatusKey]int)
	for rows.Next() {
		var key store.FleetTypeStatusKey
		var count int
		if err := rows.Scan(&key.Type, &key.Status, &count); err != nil {
			return nil, fmt.Errorf("scan fleet type/status row: %w", err)
		}
		result[key] = count
	}
	return result, rows.Err()
}

// beadProjectSQL is the project of the beads row aliased b: the suffix of its
// lexicographically first non-empty `project:*` label, or 'unknown' when it has
// none (matching the projectFromLabels helper). labels has no insertion-order
// column, hence lexicographic order.
//
// It is substr(min(label), 9), not SUBSTRING(label FROM 'project:(.+)'): the
// regex form cost about 0.17 ms per bead against 0.03 ms on a PG16 copy of the
// fleet's shape, and it ran once per agent bead ever created (gd-ipZ10t3oTo).
//
// beads.project (migration 057) is not used: it is NULL for rows not written
// since 057 and for beads with two project: labels, so it would move those
// beads to 'unknown'. Because beads has a project column, a query that groups
// by this expression must GROUP BY its ordinal: GROUP BY project binds the
// column, not the output alias, and Postgres rejects the query.
const beadProjectSQL = `COALESCE(
				(
					SELECT substr(min(l.label), 9)
					FROM labels l
					WHERE l.bead_id = b.id
					  AND l.label LIKE 'project:_%'
				),
				'unknown'
			)`

// queryFleetQueueDepthByProject returns COUNT(*) of OPEN operator-facing queue
// beads grouped by (project, type), project as beadProjectSQL resolves it. The
// type set is bound in-query via = ANY($1::text[]) so it is passed as a single
// parameter regardless of size. Result cardinality is bounded by (#projects ×
// #queue types) — a few rows — so the whole thing is aggregated in the
// database, never as a full-row scan.
func queryFleetQueueDepthByProject(ctx context.Context, db executor, queueTypes []string) (map[store.FleetQueueDepthKey]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			`+beadProjectSQL+` AS project,
			b.type,
			COUNT(*)
		FROM beads b
		WHERE b.deleted_at IS NULL
		  AND b.status = 'open'
		  AND b.type = ANY($1::text[])
		GROUP BY 1, b.type`, pq.Array(queueTypes))
	if err != nil {
		return nil, fmt.Errorf("fleet queue depth: %w", err)
	}
	defer rows.Close()

	result := make(map[store.FleetQueueDepthKey]int)
	for rows.Next() {
		var key store.FleetQueueDepthKey
		var count int
		if err := rows.Scan(&key.Project, &key.Type, &count); err != nil {
			return nil, fmt.Errorf("scan fleet queue depth row: %w", err)
		}
		result[key] = count
	}
	return result, rows.Err()
}

// queryFleetQueueOldestAgeByProject returns, per (project, type), the age in
// seconds of the OLDEST open operator-facing queue bead — the head-of-line
// staleness of the undrained backlog. Aggregation (MAX of now() - created_at,
// which is the age of the MIN created_at) is pushed into the database, so the
// result cardinality is bounded by (#projects × #queue types) exactly like
// queryFleetQueueDepthByProject and never scans full rows. The project is
// resolved with the same beadProjectSQL so a bead lands in the same project
// bucket in both gauges. Buckets with no open queue bead produce no row, so an
// empty queue is absent rather than a misleading age of 0.
func queryFleetQueueOldestAgeByProject(ctx context.Context, db executor, queueTypes []string) (map[store.FleetQueueDepthKey]float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			`+beadProjectSQL+` AS project,
			b.type,
			EXTRACT(EPOCH FROM (now() - MIN(b.created_at))) AS oldest_age_seconds
		FROM beads b
		WHERE b.deleted_at IS NULL
		  AND b.status = 'open'
		  AND b.type = ANY($1::text[])
		GROUP BY 1, b.type`, pq.Array(queueTypes))
	if err != nil {
		return nil, fmt.Errorf("fleet queue oldest age: %w", err)
	}
	defer rows.Close()

	result := make(map[store.FleetQueueDepthKey]float64)
	for rows.Next() {
		var key store.FleetQueueDepthKey
		var ageSeconds float64
		if err := rows.Scan(&key.Project, &key.Type, &ageSeconds); err != nil {
			return nil, fmt.Errorf("scan fleet queue oldest age row: %w", err)
		}
		result[key] = ageSeconds
	}
	return result, rows.Err()
}

// queryFleetActiveAgentsByProject counts the active (open or in_progress)
// agent beads per project. It reads only the active agents — a few dozen rows
// — and never their fields, so it is cheap enough for the 30 s fleet refresh
// and the health snapshot (gd-ipZ10t3oTo).
func queryFleetActiveAgentsByProject(ctx context.Context, db executor) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			`+beadProjectSQL+` AS project,
			COUNT(*)
		FROM beads b
		WHERE b.type = 'agent'
		  AND b.status IN ('open', 'in_progress')
		  AND b.deleted_at IS NULL
		GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("fleet active agents: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var project string
		var count int
		if err := rows.Scan(&project, &count); err != nil {
			return nil, fmt.Errorf("scan fleet active agents row: %w", err)
		}
		result[project] = count
	}
	return result, rows.Err()
}

// queryFleetAgentLifetimeRollup returns per-project aggregates over every
// live agent bead, closed ones included. The numeric SUMs match parseFloat in
// http_metrics.go: a JSON number contributes its value, a JSON string
// contributes its parsed numeric value when it matches a numeric pattern, and
// anything else contributes zero.
//
// It reads every agent bead ever created, so the fleet collector runs it on
// its slow lifetime cadence, not every refresh. The MATERIALIZED CTE pulls the
// two values out of fields once per row: inlined, each of the up to three
// references per value detoasts fields again (gd-ipZ10t3oTo).
func queryFleetAgentLifetimeRollup(ctx context.Context, db executor) ([]store.FleetAgentProjectRollup, error) {
	const numericPattern = `^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`

	query := `
		WITH agent_values AS MATERIALIZED (
			SELECT
				` + beadProjectSQL + ` AS project,
				b.fields->'total_cost_usd' AS cost,
				b.fields->'session_count'  AS sessions
			FROM beads b
			WHERE b.type = 'agent' AND b.deleted_at IS NULL
		)
		SELECT
			project,
			COUNT(*) AS total_agents,
			COALESCE(SUM(
				CASE jsonb_typeof(cost)
					WHEN 'number' THEN (cost #>> '{}')::numeric
					WHEN 'string' THEN
						CASE WHEN (cost #>> '{}') ~ $1
							THEN (cost #>> '{}')::numeric
							ELSE 0
						END
					ELSE 0
				END
			), 0) AS total_cost_usd,
			COALESCE(SUM(
				CASE jsonb_typeof(sessions)
					WHEN 'number' THEN (sessions #>> '{}')::numeric
					WHEN 'string' THEN
						CASE WHEN (sessions #>> '{}') ~ $1
							THEN (sessions #>> '{}')::numeric
							ELSE 0
						END
					ELSE 0
				END
			), 0) AS session_count
		FROM agent_values
		GROUP BY project`

	rows, err := db.QueryContext(ctx, query, numericPattern)
	if err != nil {
		return nil, fmt.Errorf("fleet agent lifetime rollup: %w", err)
	}
	defer rows.Close()

	var result []store.FleetAgentProjectRollup
	for rows.Next() {
		var r store.FleetAgentProjectRollup
		if err := rows.Scan(&r.Project, &r.TotalAgents, &r.TotalCostUSD, &r.SessionCount); err != nil {
			return nil, fmt.Errorf("scan fleet agent lifetime rollup row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
