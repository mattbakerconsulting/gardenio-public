package postgres

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/store"
)

// Matt Baker's two canonical operator identities. The C5 acceptance
// signal "owner-share-matt" (ka-0000004365) counts the fraction of
// recent beads still bound to Matt; pre-fix the resolver stamped Matt
// onto every unattributed write, so the share sat above 50%.
//
// Matt is stored in the wild under BOTH forms — ~250 beads as
// slack:U0OPERATOR1 and ~221 as oidc:matthew.baker@acme.example
// (ka-0000005200). Matching only the slack form undercounts his real
// share by nearly half, so queryOwnerShareMatt resolves through
// operator_aliases to count either form (and any other alias that
// resolves to the same operator).
const (
	MattSlackOperator = "slack:U0OPERATOR1"
	MattOidcOperator  = "oidc:matthew.baker@acme.example"

	// The bare subjects, used to look Matt's operator row(s) up in
	// operator_aliases by (kind, subject).
	mattSlackSubject = "U0OPERATOR1"
	mattOidcSubject  = "matthew.baker@acme.example"
)

// queryAcceptanceMetrics runs the C5 acceptance queries
// (ka-0000004365) in one round trip and returns the rolled-up
// metrics. Each sub-query is independent — failing one does not
// cascade — but they share a single context/cancel so a slow daemon
// fails fast.
func queryAcceptanceMetrics(ctx context.Context, db executor) (*store.AcceptanceMetrics, error) {
	out := &store.AcceptanceMetrics{}

	if err := queryOwnerShareMatt(ctx, db, &out.OwnerShareMatt); err != nil {
		return nil, fmt.Errorf("owner-share-matt: %w", err)
	}
	if err := queryAgentOperatorLeaks(ctx, db, &out.AgentOperatorLeaks); err != nil {
		return nil, fmt.Errorf("agent-operator-leaks: %w", err)
	}
	if err := queryFleetFallbackCount(ctx, db, &out.FleetFallbackCount); err != nil {
		return nil, fmt.Errorf("fleet-fallback-count: %w", err)
	}
	if err := queryOperatorAliasCoverage(ctx, db, &out.OperatorAliasCoverage); err != nil {
		return nil, fmt.Errorf("operator-alias-coverage: %w", err)
	}
	if err := queryOperatorCanonicalCoverage(ctx, db, &out.OperatorCanonicalCoverage); err != nil {
		return nil, fmt.Errorf("operator-canonical-coverage: %w", err)
	}
	return out, nil
}

// queryOwnerShareMatt computes the fraction of live beads created in
// the last 24h whose owner resolves to Matt. The window is inclusive
// on the start and exclusive on now() — the daemon's clock is the same
// one the resolver writes against, so there is no risk of looking back
// into a future no-op gap. Soft-deleted rows are excluded so the
// fraction tracks what the inbox UI actually shows.
//
// "Resolves to Matt" is identity-canonical (ka-0000005200): Matt is
// stored in the wild under BOTH slack:U0OPERATOR1 and
// oidc:matthew.baker@acme.example, so an exact match on the slack form
// alone undercounts his real share by nearly half. matt_ops finds the
// operator row(s) that own either canonical alias, matt_owner_strings
// reconstructs the "<kind>:<subject>" owner string for every alias
// those operators carry, and the FILTER counts owners in that set. The
// two canonical literals are UNIONed in unconditionally so the metric
// degrades to the old exact-match behaviour (never to zero) on a DB
// whose operator_aliases has not been backfilled.
func queryOwnerShareMatt(ctx context.Context, db executor, out *store.AcceptanceOwnerShare) error {
	row := db.QueryRowContext(ctx, `
		WITH matt_ops AS (
			SELECT DISTINCT operator_id
			FROM operator_aliases
			WHERE (kind = 'slack' AND subject = $1)
			   OR (kind = 'oidc'  AND subject = $2)
		),
		matt_owner_strings AS (
			SELECT oa.kind || ':' || oa.subject AS owner_str
			FROM operator_aliases oa
			JOIN matt_ops mo ON mo.operator_id = oa.operator_id
			UNION
			SELECT $3
			UNION
			SELECT $4
		)
		SELECT
			COUNT(*) FILTER (
				WHERE owner IN (SELECT owner_str FROM matt_owner_strings)
			)::int AS matt,
			COUNT(*)::int AS total
		FROM beads
		WHERE created_at > NOW() - INTERVAL '24 hours'
		  AND deleted_at IS NULL`,
		mattSlackSubject, mattOidcSubject, MattSlackOperator, MattOidcOperator)
	if err := row.Scan(&out.Matt, &out.Total); err != nil {
		return err
	}
	if out.Total > 0 {
		out.Fraction = float64(out.Matt) / float64(out.Total)
	}
	return nil
}

// queryAgentOperatorLeaks counts mutation_history rows from the last
// hour whose operator points at an agent-shape operators row.
// "Agent-shape" mirrors server.isAgentOperator: NOT a well-formed
// slack-user-id, NOT a well-formed oidc email, and NOT a sentinel
// (system:* / fleet:*). The patterns are kept loose on purpose — the
// goal is to catch resolver regressions, not to be a strict identity
// validator.
//
// Per ka-0000006809 Invariant §1, this MUST be zero in steady state.
// Migration 015 reserves operators.kind values
// {slack,oidc,gitlab,jira,system,fleet}; anything outside that set
// counts as a leak regardless of subject shape. Inside slack/oidc we
// also fail rows whose subject does not match the expected human
// shape.
func queryAgentOperatorLeaks(ctx context.Context, db executor, out *store.AcceptanceOperatorLeaks) error {
	return db.QueryRowContext(ctx, `
		SELECT COUNT(*)::int
		FROM mutation_history mh
		JOIN operators o ON o.id = mh.operated_by_id
		WHERE mh.created_at > NOW() - INTERVAL '1 hour'
		  AND mh.operated_by_id IS NOT NULL
		  AND NOT (
		    o.kind IN ('system','fleet')
		    OR (o.kind = 'slack' AND o.subject ~ '^U[A-Z0-9]{1,63}$')
		    OR (o.kind = 'oidc'  AND o.subject ~ '^[^@]+@[^@.]+\.[^@]+$')
		  )`).Scan(&out.Count)
}

// queryFleetFallbackCount counts mutation_history rows from the last
// hour whose operator resolves to a fleet sentinel (o.kind='fleet').
//
// queryAgentOperatorLeaks excludes o.kind IN ('system','fleet') from
// its leak count, which is correct — a fleet sentinel is a well-formed
// operators row, not a malformed agent-shape one — but that exclusion
// left fleet fallbacks entirely invisible to monitoring (ka-0000005200):
// 17 live fleet-stamped agents read as ZERO leaks. A fleet fallback is
// its own failure mode: an autonomous or respawn path lost the human on
// the way to the daemon and the daemon bound the fleet:<source>
// sentinel instead. This signal surfaces those writes as a first-class
// acceptance number that MUST trend to zero as the spawn-propagation
// fixes (epic ka-0000000095) land — do NOT fold it back into the leak
// count and do NOT silently exclude fleet from health.
func queryFleetFallbackCount(ctx context.Context, db executor, out *store.AcceptanceFleetFallback) error {
	return db.QueryRowContext(ctx, `
		SELECT COUNT(*)::int
		FROM mutation_history mh
		JOIN operators o ON o.id = mh.operated_by_id
		WHERE mh.created_at > NOW() - INTERVAL '1 hour'
		  AND mh.operated_by_id IS NOT NULL
		  AND o.kind = 'fleet'`).Scan(&out.Count)
}

// queryOperatorAliasCoverage measures the share of live type=user
// beads with both a slack_id and an email field whose two identities
// are linked in operator_aliases under a single operator id. The
// matched filter requires that one operator row owns BOTH the
// (slack,U…) alias and the (oidc,email) alias — splitting them across
// two operator ids is exactly the failure mode this metric exists to
// catch.
func queryOperatorAliasCoverage(ctx context.Context, db executor, out *store.AcceptanceAliasCoverage) error {
	if err := db.QueryRowContext(ctx, `
		WITH users AS (
			SELECT
				b.id,
				b.fields->>'slack_id' AS slack_id,
				b.fields->>'email'    AS email
			FROM beads b
			WHERE b.type = 'user'
			  AND b.deleted_at IS NULL
			  AND COALESCE(b.fields->>'slack_id', '') <> ''
			  AND COALESCE(b.fields->>'email',    '') <> ''
		)
		SELECT
			COUNT(*)::int AS total,
			COUNT(*) FILTER (WHERE EXISTS (
				SELECT 1
				FROM operator_aliases sa
				JOIN operator_aliases ea
				  ON ea.operator_id = sa.operator_id
				WHERE sa.kind = 'slack' AND sa.subject = u.slack_id
				  AND ea.kind = 'oidc'  AND ea.subject = u.email
			))::int AS matched
		FROM users u`).Scan(&out.Total, &out.Matched); err != nil {
		return err
	}
	if out.Total > 0 {
		out.Fraction = float64(out.Matched) / float64(out.Total)
	}
	return nil
}

// queryOperatorCanonicalCoverage measures whether "one human is one
// operator" holds at the operators-ROW level (ka-0000002687). The write-path
// resolver folds slack:U… and oidc:<email> aliases onto a single operator_id
// via operator_aliases, but a duplicate operators ROW sharing the same human
// email still fragments that person's beads across two operated_by_id values.
// This is the daemon analogue of the C5 alias-coverage signal: it counts
// distinct real human emails against the operators rows carrying them, so
// HumanEmails == OperatorRows (Fraction 1.0) means no human is split and any
// surplus rows (Fragmented > 0) name consolidation work still outstanding.
//
// Only real humans count: kind must be slack/oidc, the email must be a real
// deliverable address (the synthetic @noreply.kbeads.local sentinel and empty
// emails are excluded), and forgotten operators drop out. Email is lower-cased
// so a case-variant address is not counted as a second human — matching the
// write-path canonicalization in server.canonicalizeHumanOperator.
func queryOperatorCanonicalCoverage(ctx context.Context, db executor, out *store.AcceptanceCanonicalCoverage) error {
	if err := db.QueryRowContext(ctx, `
		WITH human_ops AS (
			SELECT LOWER(email) AS email
			FROM operators
			WHERE kind IN ('slack', 'oidc')
			  AND COALESCE(email, '') <> ''
			  AND email NOT LIKE ('%@' || $1)
			  AND forgotten_at IS NULL
		)
		SELECT
			COUNT(DISTINCT email)::int AS humans,
			COUNT(*)::int              AS operator_rows
		FROM human_ops`, sentinelEmailDomain).Scan(&out.HumanEmails, &out.OperatorRows); err != nil {
		return err
	}
	out.Fragmented = out.OperatorRows - out.HumanEmails
	if out.Fragmented < 0 {
		out.Fragmented = 0
	}
	if out.OperatorRows > 0 {
		out.Fraction = float64(out.HumanEmails) / float64(out.OperatorRows)
	} else {
		out.Fraction = 1
	}
	return nil
}
