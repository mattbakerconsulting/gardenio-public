package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// sentinelEmailDomain is the synthetic email domain used for operator rows
// that have no real human behind them (system:*, fleet:*) and for cold-path
// operator inserts where the resolver does not yet have a real email (slack
// before the resync job has run). RFC 6762 reserves `.local` for mDNS, so
// these addresses can never resolve as real mail — they are audit-trail
// markers, not deliverable addresses. Format:
// `<kind>-<subject>@noreply.kbeads.local`.
const sentinelEmailDomain = "noreply.kbeads.local"

// isSlackUserSubject reports whether subject is a well-formed Slack user id —
// a leading 'U' (workspace-user prefix) followed by one or more uppercase
// letters or digits, i.e. ^U[A-Z0-9]+$. This is the same shape the resolver's
// extractSlackUserID enforces on the audit.go side; pinning it at the store
// boundary too means a display name ("Matt Baker") or email ("matt.baker")
// can never be minted as a kind=slack operator subject (ka-0000001767 — prod
// junk rows op-264 'matt.baker', op-297 'Matt Baker'). deriveOperatorEmail
// already assumes U-ids for the slack sentinel; this makes that assumption a
// guard rather than a hope. The 64-char ceiling mirrors extractSlackUserID's
// defensive cap so an oversized value cannot smuggle a giant subject into the
// sentinel email.
func isSlackUserSubject(subject string) bool {
	if len(subject) < 2 || subject[0] != 'U' || len(subject) > 64 {
		return false
	}
	for _, r := range subject[1:] {
		switch {
		case r >= '0' && r <= '9',
			r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}

// deriveOperatorEmail returns a non-empty email for (kind, subject) and
// ok=true, or ("", false) when no email can be derived (the caller then
// returns store.UpsertOperatorEmailError so the cold-path INSERT does not
// run a guaranteed-to-fail CHECK violation against migration 027).
//
// Per-kind strategy:
//   - oidc:        subject is the email (must contain '@' away from the edges)
//   - slack:       subject MUST be a Slack user id (^U[A-Z0-9]+); a display
//     name or email is rejected with ok=false so it routes back through the
//     resolver (display/email lookup) instead of minting a junk slack
//     operator (ka-0000001767). The email is the sentinel
//     `slack-<U…>@noreply.kbeads.local`; the Slack ↔ OIDC resync job
//     (C3 / ka-0000000483) overwrites it with the real email within 6h via
//     UpdateOperatorIdentity.
//   - system, fleet, machine, gitlab, jira: synthetic per-kind sentinel. system:*,
//     machine:* (registered Keycloak service accounts, gd-gQ4iLIpWCm) and
//     fleet:* genuinely have no human; gitlab/jira upstream IDs
//     carry an email at the source but we do not have it at
//     cold-path insert time — the sentinel placeholder gives
//     the resync paths a row to refresh.
//
// Unknown kinds fall through to ok=false so a future integration that
// forgets to wire derivation hits a loud error in tests instead of
// silently writing junk.
func deriveOperatorEmail(kind, subject string) (string, bool) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", false
	}
	switch kind {
	case "oidc":
		// Subject must look like an email; otherwise treat as underivable
		// rather than writing a non-deliverable address that looks real.
		at := strings.IndexByte(subject, '@')
		if at <= 0 || at >= len(subject)-1 {
			return "", false
		}
		return subject, true
	case "slack":
		// Slack subject must be a real Slack user id. A non-U value (display
		// name, email, agent handle) that slipped this far is junk — fail
		// loud (ok=false) so UpsertOperator returns UpsertOperatorEmailError
		// and the caller falls back to the display/email resolver path
		// instead of creating a duplicate slack operator row.
		if !isSlackUserSubject(subject) {
			return "", false
		}
		return kind + "-" + subject + "@" + sentinelEmailDomain, true
	case "gitlab", "jira", "system", "fleet", "machine":
		return kind + "-" + subject + "@" + sentinelEmailDomain, true
	default:
		return "", false
	}
}

// UpsertOperator inserts (kind, subject, email) into operators if absent and
// returns the id. Idempotent: ON CONFLICT (kind, subject) DO UPDATE flips
// updated_at and uses RETURNING id so the existing row's id comes back from
// the same statement (a plain DO NOTHING returns no row when the conflict
// fires, which would force a second SELECT round-trip on every cold-path
// miss). The conflict branch deliberately does NOT overwrite the existing
// email — a real email harvested by the resync jobs must not be clobbered
// back to the synthetic sentinel by a later cold-path miss.
//
// Email is derived from (kind, subject) via deriveOperatorEmail; if no
// email can be derived the call returns a *store.UpsertOperatorEmailError
// without touching the DB so the resolver gets a typed, loud failure
// instead of an opaque CHECK violation from migration 027.
//
// Caller is responsible for upserting the canonical alias row separately
// (UpsertOperatorAlias with source="resolver") so future lookups land in the
// hot path. Splitting the two writes keeps the cold-path tx small and lets
// alias-only repairs (resolver re-discovers an existing operator under a new
// alias) reuse UpsertOperatorAlias without touching the operators table.
//
// Migration 015 §3.1 / ADR docs/adr/human-attribution-identity-v2.md §5.1.
// Migration 027 (ka-0000008244) added the CHECK (email <> ”) invariant.
func (s *PostgresStore) UpsertOperator(ctx context.Context, kind, subject string) (int64, error) {
	return upsertOperator(ctx, s.db, kind, subject)
}

func (s *txStore) UpsertOperator(ctx context.Context, kind, subject string) (int64, error) {
	return upsertOperator(ctx, s.tx, kind, subject)
}

func upsertOperator(ctx context.Context, db execer, kind, subject string) (int64, error) {
	email, ok := deriveOperatorEmail(kind, subject)
	if !ok {
		return 0, &store.UpsertOperatorEmailError{Kind: kind, Subject: subject}
	}
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO operators (kind, subject, email)
		VALUES ($1, $2, $3)
		ON CONFLICT (kind, subject)
		DO UPDATE SET updated_at = NOW()
		RETURNING id`, kind, subject, email).Scan(&id)
	if err != nil {
		return 0, err
	}
	// Fill operators.slug + seed canonical operator_slug_aliases row
	// when this is a new operator (slug IS NULL). Migration 028
	// (ka-0000000022) handles the cold backfill for pre-migration rows;
	// this is the hot-path equivalent so post-migration upserts don't
	// land with slug=NULL and break /u/{slug} resolution until a
	// re-backfill runs.
	if err := ensureOperatorSlug(ctx, db, id, kind, email); err != nil {
		return id, err
	}
	return id, nil
}

// LookupOperatorAlias returns the operator_id for a known (kind, subject)
// alias, or sql.ErrNoRows when no alias exists. Single PK probe; the resolver
// hot path runs one of these per audit-bearing write.
func (s *PostgresStore) LookupOperatorAlias(ctx context.Context, kind, subject string) (int64, error) {
	return lookupOperatorAlias(ctx, s.db, kind, subject)
}

func (s *txStore) LookupOperatorAlias(ctx context.Context, kind, subject string) (int64, error) {
	return lookupOperatorAlias(ctx, s.tx, kind, subject)
}

func lookupOperatorAlias(ctx context.Context, db execer, kind, subject string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `
		SELECT operator_id FROM operator_aliases WHERE kind = $1 AND subject = $2`,
		kind, subject).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, sql.ErrNoRows
	}
	return id, err
}

// LookupOperatorByAlias returns the operator_id and canonical operator kind
// for a (kind, subject) alias, joined with operators. Single PK probe + a
// PK join — the viewer-resolver hot path runs one of these per cold-cache
// read.
func (s *PostgresStore) LookupOperatorByAlias(ctx context.Context, kind, subject string) (int64, string, error) {
	return lookupOperatorByAlias(ctx, s.db, kind, subject)
}

func (s *txStore) LookupOperatorByAlias(ctx context.Context, kind, subject string) (int64, string, error) {
	return lookupOperatorByAlias(ctx, s.tx, kind, subject)
}

func lookupOperatorByAlias(ctx context.Context, db execer, kind, subject string) (int64, string, error) {
	var (
		id     int64
		opKind string
	)
	err := db.QueryRowContext(ctx, `
		SELECT a.operator_id, o.kind
		  FROM operator_aliases a
		  JOIN operators o ON o.id = a.operator_id
		 WHERE a.kind = $1 AND a.subject = $2`,
		kind, subject).Scan(&id, &opKind)
	if err == sql.ErrNoRows {
		return 0, "", sql.ErrNoRows
	}
	return id, opKind, err
}

// UpsertOperatorAlias inserts an alias row pointing at operatorID, or — when
// (kind, subject) already exists — updates the row to point at operatorID.
// The update path is the consolidation-in-place affordance ADR §3.1 calls
// out: when two humans previously seen as separate operators turn out to be
// the same person, repointing the alias rebinds future lookups without
// duplicating rows.
func (s *PostgresStore) UpsertOperatorAlias(ctx context.Context, operatorID int64, kind, subject, source string) error {
	return upsertOperatorAlias(ctx, s.db, operatorID, kind, subject, source)
}

func (s *txStore) UpsertOperatorAlias(ctx context.Context, operatorID int64, kind, subject, source string) error {
	return upsertOperatorAlias(ctx, s.tx, operatorID, kind, subject, source)
}

func upsertOperatorAlias(ctx context.Context, db execer, operatorID int64, kind, subject, source string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO operator_aliases (operator_id, kind, subject, source)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, subject)
		DO UPDATE SET operator_id = EXCLUDED.operator_id`,
		operatorID, kind, subject, source)
	return err
}

func (s *PostgresStore) PruneOperatorAliases(ctx context.Context, source string, keep []store.OperatorAliasKey) ([]store.OperatorAliasKey, error) {
	return pruneOperatorAliases(ctx, s.db, source, keep)
}

func (s *txStore) PruneOperatorAliases(ctx context.Context, source string, keep []store.OperatorAliasKey) ([]store.OperatorAliasKey, error) {
	return pruneOperatorAliases(ctx, s.tx, source, keep)
}

// pruneOperatorAliases deletes the alias rows written under source that are
// not in keep, via a single DELETE ... RETURNING so the set returned is exactly
// the set removed. An empty source is refused: it would match every row that
// never recorded a source.
func pruneOperatorAliases(ctx context.Context, db execer, source string, keep []store.OperatorAliasKey) ([]store.OperatorAliasKey, error) {
	if source == "" {
		return nil, errors.New("prune operator aliases: source is required")
	}
	kinds := make([]string, len(keep))
	subjects := make([]string, len(keep))
	for i, k := range keep {
		kinds[i], subjects[i] = k.Kind, k.Subject
	}
	rows, err := db.QueryContext(ctx, `
		DELETE FROM operator_aliases a
		 WHERE a.source = $1
		   AND NOT EXISTS (
		       SELECT 1 FROM unnest($2::text[], $3::text[]) AS k(kind, subject)
		        WHERE k.kind = a.kind AND k.subject = a.subject)
		RETURNING a.kind, a.subject`, source, pq.Array(kinds), pq.Array(subjects))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.OperatorAliasKey
	for rows.Next() {
		var k store.OperatorAliasKey
		if err := rows.Scan(&k.Kind, &k.Subject); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListOperatorAliasesByKind returns alias rows for a given kind joined with
// the parent operators row's current display/email. Driven by the Slack
// users.info resync job — callers compare the fresh Slack-fetched (display,
// email) against the existing pair to skip no-op writes.
func (s *PostgresStore) ListOperatorAliasesByKind(ctx context.Context, kind string) ([]store.OperatorAliasIdentity, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.operator_id, a.subject, o.display, o.email
		  FROM operator_aliases a
		  JOIN operators o ON o.id = a.operator_id
		 WHERE a.kind = $1
		 ORDER BY a.operator_id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.OperatorAliasIdentity
	for rows.Next() {
		var r store.OperatorAliasIdentity
		if err := rows.Scan(&r.OperatorID, &r.Subject, &r.Display, &r.Email); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *txStore) ListOperatorAliasesByKind(ctx context.Context, kind string) ([]store.OperatorAliasIdentity, error) {
	rows, err := s.tx.QueryContext(ctx, `
		SELECT a.operator_id, a.subject, o.display, o.email
		  FROM operator_aliases a
		  JOIN operators o ON o.id = a.operator_id
		 WHERE a.kind = $1
		 ORDER BY a.operator_id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.OperatorAliasIdentity
	for rows.Next() {
		var r store.OperatorAliasIdentity
		if err := rows.Scan(&r.OperatorID, &r.Subject, &r.Display, &r.Email); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListOperatorsWithSlugs returns operators with a canonical slug, ordered by
// slug ascending. Limit bounds the result; 0 falls back to a default of 500
// so unbounded scans cannot fall out of an empty query string. Powers the
// public /people directory (M2 §4 ka-0000000340).
func (s *PostgresStore) ListOperatorsWithSlugs(ctx context.Context, limit int) ([]store.OperatorDirectoryEntry, error) {
	return listOperatorsWithSlugs(ctx, s.db, limit)
}

func (s *txStore) ListOperatorsWithSlugs(ctx context.Context, limit int) ([]store.OperatorDirectoryEntry, error) {
	return listOperatorsWithSlugs(ctx, s.tx, limit)
}

func listOperatorsWithSlugs(ctx context.Context, db execer, limit int) ([]store.OperatorDirectoryEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, kind, subject, COALESCE(display, ''), COALESCE(email, ''), slug
		  FROM operators
		 WHERE slug IS NOT NULL
		 ORDER BY slug ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.OperatorDirectoryEntry, 0)
	for rows.Next() {
		var r store.OperatorDirectoryEntry
		if err := rows.Scan(&r.ID, &r.Kind, &r.Subject, &r.Display, &r.Email, &r.Slug); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateOperatorIdentity overwrites operators.display and operators.email and
// bumps updated_at when either value changes. Idempotent: writing the same
// (display, email) twice leaves updated_at alone, so the bridge's periodic
// from_user_bead resync does not churn every row. Returns ok=false when no
// row matched (operator id was deleted, e.g. by a merge).
func (s *PostgresStore) UpdateOperatorIdentity(ctx context.Context, operatorID int64, display, email string) (bool, error) {
	return updateOperatorIdentity(ctx, s.db, operatorID, display, email)
}

func (s *txStore) UpdateOperatorIdentity(ctx context.Context, operatorID int64, display, email string) (bool, error) {
	return updateOperatorIdentity(ctx, s.tx, operatorID, display, email)
}

func updateOperatorIdentity(ctx context.Context, db execer, operatorID int64, display, email string) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE operators
		   SET display = $1,
		       email = $2,
		       updated_at = CASE
		           WHEN display IS DISTINCT FROM $1 OR email IS DISTINCT FROM $2 THEN NOW()
		           ELSE updated_at
		       END
		 WHERE id = $3`, display, email, operatorID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MergeOperators repoints every operated_by_id reference from oldID to newID
// across the five tables that carry the audit FK (beads, comments, deps,
// events, mutation_history), then absorbs oldID's slug into newID so the
// merged-away operator stops rendering as its own /people directory row.
// Used by the gb identity link-operator flow (ADR
// docs/adr/human-attribution-identity-v2.md §3.1: "consolidation in place")
// to merge a legacy slack:U… (or a second cross-org oidc:<email>) operator
// into the surviving operator. Returns the total number of rows repointed
// across the five audit tables so callers can surface a "moved N rows"
// confirmation (the slug absorb is not counted — it is directory hygiene, not
// audit history).
//
// Slug absorb (ka-0000004514): the /people directory feed
// (ListOperatorsWithSlugs) emits EVERY operators row whose slug IS NOT NULL,
// and the viewer folds rows by email/display key. Two identities of one human
// with different emails (e.g. sam.rivers@acme.example and
// srivers@contractor-a.example) therefore never fold and render as two rows.
// Repointing the audit FKs alone leaves the loser slugged, so it lingers as a
// now-empty ghost row. So the merge also (a) repoints and retires every
// operator_slug_aliases row of oldID onto newID — preserving /u/{loser-slug}
// resolution as a retired alias so external links never 404 — and (b) NULLs
// operators.slug on oldID so the directory feed drops it. The loser operators
// row itself is left in place (its beads.created_by_operator_id FK and any
// historical references still hold); only its directory-visible slug moves.
//
// Caller is still responsible for repointing operator_aliases rows that
// pointed at oldID (UpsertOperatorAlias) so the write-path resolver routes
// future writes to newID — that is what makes the merge durable across a later
// login as the merged-away identity.
func (s *PostgresStore) MergeOperators(ctx context.Context, oldID, newID int64) (int64, error) {
	return mergeOperators(ctx, s.db, oldID, newID)
}

func (s *txStore) MergeOperators(ctx context.Context, oldID, newID int64) (int64, error) {
	return mergeOperators(ctx, s.tx, oldID, newID)
}

func mergeOperators(ctx context.Context, db execer, oldID, newID int64) (int64, error) {
	if oldID == newID || oldID <= 0 || newID <= 0 {
		return 0, nil
	}
	var total int64
	for _, table := range []string{"beads", "comments", "deps", "events", "mutation_history"} {
		res, err := db.ExecContext(ctx,
			"UPDATE "+table+" SET operated_by_id = $1 WHERE operated_by_id = $2",
			newID, oldID)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	if err := absorbOperatorSlug(ctx, db, oldID, newID); err != nil {
		return total, err
	}
	return total, nil
}

// absorbOperatorSlug moves the merged-away operator's slug onto the survivor:
// every operator_slug_aliases row of oldID is repointed to newID and marked
// retired (so /u/{loser-slug} still resolves to the survivor as a retired
// alias, keeping external links alive), and operators.slug on oldID is set
// NULL so the loser drops out of the /people directory feed
// (ListOperatorsWithSlugs filters on slug IS NOT NULL). Idempotent — a second
// call after the slug is already NULL repoints nothing and clears nothing.
//
// No slug collision is possible: operator_slug_aliases.slug is the primary key
// (globally unique) and operators_slug_uniq keeps live slugs distinct, so the
// loser and survivor can never share a slug value.
func absorbOperatorSlug(ctx context.Context, db execer, oldID, newID int64) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE operator_slug_aliases
		   SET operator_id = $1,
		       retired_at = COALESCE(retired_at, NOW())
		 WHERE operator_id = $2`, newID, oldID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		UPDATE operators SET slug = NULL WHERE id = $1`, oldID)
	return err
}

// ResolveOperators implements store.Store — the server-side operator_id ->
// identity join behind the chart backend's engagement / security dashboards
// (ka-0000004824, customer-1 telemetry plan §4). See the interface doc for the
// privacy contract; the redaction-on-forgotten and absent-on-unknown semantics
// live in resolveOperators so the *PostgresStore and *txStore receivers share
// one implementation.
func (s *PostgresStore) ResolveOperators(ctx context.Context, ids []int64) (map[int64]store.ResolvedOperator, error) {
	return resolveOperators(ctx, s.db, ids)
}

func (s *txStore) ResolveOperators(ctx context.Context, ids []int64) (map[int64]store.ResolvedOperator, error) {
	return resolveOperators(ctx, s.tx, ids)
}

func resolveOperators(ctx context.Context, db execer, ids []int64) (map[int64]store.ResolvedOperator, error) {
	out := make(map[int64]store.ResolvedOperator, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// ANY($1::bigint[]) (not IN (...)) so the parameter count is fixed at one
	// regardless of the number of distinct operator_ids a dashboard surfaces —
	// the lib/pq 65535-param ceiling can never bite (advice ka-0000000229).
	rows, err := db.QueryContext(ctx, `
		SELECT id,
		       COALESCE(display, ''),
		       COALESCE(email, ''),
		       COALESCE(slug, ''),
		       forgotten_at IS NOT NULL AS forgotten
		  FROM operators
		 WHERE id = ANY($1::bigint[])`,
		pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r         store.ResolvedOperator
			display   string
			email     string
			slug      string
			forgotten bool
		)
		if err := rows.Scan(&r.ID, &display, &email, &slug, &forgotten); err != nil {
			return nil, err
		}
		r.Slug = slug
		r.Forgotten = forgotten
		if forgotten {
			// Right-to-be-forgotten redaction (plan §4): drop the human fields
			// but keep the opaque id so historical series stay attributable.
			r.Display = ""
			r.Email = store.ForgottenOperatorEmail
		} else {
			r.Display = display
			r.Email = email
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}
