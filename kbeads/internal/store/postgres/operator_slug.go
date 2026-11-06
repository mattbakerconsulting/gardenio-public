package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// reservedOperatorSlugs mirrors the SQL backfill exclusion list in
// migration 028_operator_slugs.up.sql. These are the existing top-level
// routes registered in beads-viewer that a slug would collide with.
// Keep both lists in sync — the SQL is the cold-path migration and this
// Go list is the hot-path UpsertOperator equivalent.
var reservedOperatorSlugs = map[string]struct{}{
	"me": {}, "inbox": {}, "people": {}, "agents": {}, "fleet": {},
	"new": {}, "search": {}, "admin": {}, "login": {}, "logout": {},
	"auth": {}, "health": {}, "healthz": {}, "readyz": {}, "api": {},
	"v1": {}, "image": {}, "htmx": {}, "dev": {}, "widgets": {},
	"active": {}, "up": {}, "decisions": {}, "escalations": {},
	"ratings": {}, "triggers": {}, "schedules": {}, "costs": {},
	"coverage": {}, "views": {}, "settings": {}, "banner": {},
	"cmdk": {}, "p": {}, "u": {}, "op": {},
}

// candidateOperatorSlug returns the slug candidate for (kind, email, id)
// matching the SQL backfill rules in migration 028:
//
//   - oidc with email local-part matching `^[a-z0-9._-]+$` and not reserved
//     → lowercased local-part
//   - everything else → `op-<id>`
//
// Collision resolution (`<slug>-<id>` suffix on the loser) is handled by the
// caller — the daemon's hot path passes the candidate to an INSERT ... ON
// CONFLICT, and on conflict re-tries with the suffixed form.
func candidateOperatorSlug(kind, email string, id int64) string {
	if kind == "oidc" {
		at := strings.IndexByte(email, '@')
		if at > 0 && at < len(email)-1 {
			local := strings.ToLower(email[:at])
			if isValidSlugLocal(local) {
				if _, reserved := reservedOperatorSlugs[local]; !reserved {
					return local
				}
			}
		}
	}
	return opFallbackSlug(id)
}

// readableSlugFromEmail derives a human-readable slug from an operator's
// email local-part, or ("", false) when no readable slug can be derived.
// Unlike candidateOperatorSlug it is kind-agnostic — it keys purely off the
// email — because the op-<id> slug backfill (ka-0000001149) slugs any human
// survivor that carries a real email, including slack/gitlab identities whose
// email was harvested by the resync jobs after a merge.
//
// A slug is derived only when ALL hold:
//   - the email has a local-part and a domain (a '@' away from both edges);
//   - the domain is NOT the synthetic sentinelEmailDomain — sentinel-email
//     rows (system:*, fleet:*, un-resynced slack) keep op-<id> per the task;
//   - the lowercased local-part matches `^[a-z0-9._-]+$` (isValidSlugLocal);
//   - the local-part is not a reserved top-level route.
//
// This mirrors migration 028's CASE derivation but adds the sentinel-domain
// exclusion, since a sentinel local-part like `slack-u123` would otherwise
// pass isValidSlugLocal and mint a junk readable slug.
func readableSlugFromEmail(email string) (string, bool) {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at >= len(email)-1 {
		return "", false
	}
	if email[at+1:] == sentinelEmailDomain {
		return "", false
	}
	local := strings.ToLower(email[:at])
	if !isValidSlugLocal(local) {
		return "", false
	}
	if _, reserved := reservedOperatorSlugs[local]; reserved {
		return "", false
	}
	return local, true
}

// opFallbackSlug returns the opaque `op-<id>` form used for sentinels,
// slack-only operators, and collision losers. id is unique per row so this
// form is guaranteed unique.
func opFallbackSlug(id int64) string {
	return "op-" + strconv.FormatInt(id, 10)
}

// isValidSlugLocal reports whether s is non-empty and matches the SQL regex
// `^[a-z0-9._-]+$` (case-sensitive — caller lowercases first).
func isValidSlugLocal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// ensureOperatorSlug fills operators.slug for the row id with the
// `candidateOperatorSlug(kind, email, id)` value when slug IS NULL, and
// seeds the canonical operator_slug_aliases row so /u/{slug} resolves
// without waiting for a re-backfill. Idempotent: when the row already has
// a slug (existing operator from migration 028, or a prior call), this
// short-circuits without touching either table.
//
// Collision handling: if the candidate races another operator's slug claim
// (partial UNIQUE INDEX operators_slug_uniq fires 23505), retry with the
// opaque op-<id> form, which is unique by id.
func ensureOperatorSlug(ctx context.Context, db execer, id int64, kind, email string) error {
	candidate := candidateOperatorSlug(kind, email, id)
	slug, err := claimOperatorSlug(ctx, db, id, candidate)
	if err != nil {
		if !isUniqueViolation(err) {
			return err
		}
		fallback := opFallbackSlug(id)
		if fallback == candidate {
			// Pathological — `op-<id>` itself collided. id is unique
			// so this should not be reachable; bubble the error up
			// rather than silently swallowing the invariant break.
			return err
		}
		slug, err = claimOperatorSlug(ctx, db, id, fallback)
		if err != nil {
			return err
		}
	}
	if slug == "" {
		// Row already had a slug — alias was seeded by migration 028
		// or an earlier upsert. Nothing more to do.
		return nil
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO operator_slug_aliases (slug, operator_id)
		VALUES ($1, $2)
		ON CONFLICT (slug) DO NOTHING`, slug, id)
	return err
}

// claimOperatorSlug sets operators.slug = candidate for the row id when
// the current slug IS NULL. Returns the written slug on a successful
// claim, "" when the row already had a slug (no-op), or the underlying
// error — including a unique-violation that the caller retries via the
// op-<id> fallback.
func claimOperatorSlug(ctx context.Context, db execer, id int64, candidate string) (string, error) {
	var slug string
	err := db.QueryRowContext(ctx, `
		UPDATE operators
		   SET slug = $2
		 WHERE id = $1
		   AND slug IS NULL
		RETURNING slug`, id, candidate).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return slug, nil
}

// isUniqueViolation reports whether err is a pq unique-violation (SQLSTATE
// 23505). Used to drive the candidate → op-<id> retry in ensureOperatorSlug.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pqe interface{ SQLState() string }
	if errors.As(err, &pqe) {
		return pqe.SQLState() == "23505"
	}
	return strings.Contains(err.Error(), "unique constraint")
}

// GetOperatorBySlug returns the operator id for the given slug, joining
// through operator_slug_aliases so retired (renamed) slugs still resolve.
// Returns sql.ErrNoRows when no alias exists for the slug.
//
// Hot path for the /u/{slug} viewer route — single PK probe + FK join.
func (s *PostgresStore) GetOperatorBySlug(ctx context.Context, slug string) (int64, error) {
	return getOperatorBySlug(ctx, s.db, slug)
}

func (s *txStore) GetOperatorBySlug(ctx context.Context, slug string) (int64, error) {
	return getOperatorBySlug(ctx, s.tx, slug)
}

func getOperatorBySlug(ctx context.Context, db execer, slug string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `
		SELECT operator_id
		  FROM operator_slug_aliases
		 WHERE slug = $1`, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, sql.ErrNoRows
	}
	return id, err
}

// GetOperatorSlug returns the current canonical slug for the given operator
// id, or sql.ErrNoRows when the operator has no slug (should not happen
// post-migration; safety net for races between an INSERT and the slug
// backfill trigger).
//
// Used by /me to look up the viewer's slug for the 308 redirect target.
func (s *PostgresStore) GetOperatorSlug(ctx context.Context, operatorID int64) (string, error) {
	return getOperatorSlug(ctx, s.db, operatorID)
}

func (s *txStore) GetOperatorSlug(ctx context.Context, operatorID int64) (string, error) {
	return getOperatorSlug(ctx, s.tx, operatorID)
}

func getOperatorSlug(ctx context.Context, db execer, operatorID int64) (string, error) {
	var slug sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT slug FROM operators WHERE id = $1`, operatorID).Scan(&slug)
	if err == sql.ErrNoRows {
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", err
	}
	if !slug.Valid || slug.String == "" {
		return "", sql.ErrNoRows
	}
	return slug.String, nil
}
