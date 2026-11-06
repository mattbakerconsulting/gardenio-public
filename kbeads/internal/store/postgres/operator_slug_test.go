package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// expectSlugClaim pins the conditional-UPDATE shape used by
// claimOperatorSlug to fill operators.slug only when it is currently NULL.
// Returned ExpectedQuery lets the caller chain WillReturnRows / WillReturnError.
func expectSlugClaim(mock sqlmock.Sqlmock, id int64, candidate string) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`UPDATE operators\s+SET slug = \$2\s+WHERE id = \$1\s+AND slug IS NULL\s+RETURNING slug`).
		WithArgs(id, candidate)
}

// expectSlugAliasInsert pins the canonical-alias seed that follows a
// successful slug claim — single ON CONFLICT DO NOTHING insert so the
// /u/{slug} resolver lands on a hot-path row.
func expectSlugAliasInsert(mock sqlmock.Sqlmock, slug string, id int64) *sqlmock.ExpectedExec {
	return mock.ExpectExec(`INSERT INTO operator_slug_aliases \(slug, operator_id\)\s+VALUES \(\$1, \$2\)\s+ON CONFLICT \(slug\) DO NOTHING`).
		WithArgs(slug, id)
}

// TestCandidateOperatorSlug exercises the per-kind / per-email derivation
// matrix so the Go helper stays in lockstep with migration 028's SQL CASE.
func TestCandidateOperatorSlug(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		email string
		id    int64
		want  string
	}{
		{"oidc-basic", "oidc", "alice@example.com", 1, "alice"},
		{"oidc-lowercased", "oidc", "ALICE@Example.COM", 1, "alice"},
		{"oidc-dot-underscore", "oidc", "first.last_99@x.io", 1, "first.last_99"},
		{"oidc-reserved-falls-back", "oidc", "admin@example.com", 5, "op-5"},
		{"oidc-reserved-op-prefix", "oidc", "op@example.com", 5, "op-5"},
		{"oidc-invalid-chars", "oidc", "alice+work@example.com", 9, "op-9"},
		{"oidc-no-at", "oidc", "alice", 3, "op-3"},
		{"oidc-leading-at", "oidc", "@example.com", 4, "op-4"},
		{"oidc-trailing-at", "oidc", "alice@", 4, "op-4"},
		{"slack-fallback", "slack", "slack-U123@noreply.kbeads.local", 7, "op-7"},
		{"system-fallback", "system", "system-sched@noreply.kbeads.local", 8, "op-8"},
		{"unknown-kind-fallback", "future-saml", "x@y", 11, "op-11"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateOperatorSlug(tc.kind, tc.email, tc.id)
			if got != tc.want {
				t.Errorf("candidateOperatorSlug(%q, %q, %d) = %q, want %q",
					tc.kind, tc.email, tc.id, got, tc.want)
			}
		})
	}
}

// TestIsValidSlugLocal pins the regex-equivalent local validator.
func TestIsValidSlugLocal(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"alice", true},
		{"first.last", true},
		{"a_b-c.d", true},
		{"abc123", true},
		{"", false},
		{"Alice", false},
		{"alice+work", false},
		{"alice@bob", false},
		{"alice/bob", false},
	}
	for _, tc := range cases {
		if got := isValidSlugLocal(tc.s); got != tc.want {
			t.Errorf("isValidSlugLocal(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// TestIsUniqueViolation_PqError matches a real pq error with SQLSTATE 23505.
func TestIsUniqueViolation_PqError(t *testing.T) {
	err := &pq.Error{Code: "23505", Message: "duplicate key value violates unique constraint"}
	if !isUniqueViolation(err) {
		t.Fatal("isUniqueViolation should match pq SQLSTATE 23505")
	}
}

// TestIsUniqueViolation_OtherSQLState is false on non-23505 codes — the
// fallback retry must not fire on FK / check / etc.
func TestIsUniqueViolation_OtherSQLState(t *testing.T) {
	err := &pq.Error{Code: "23503"}
	if isUniqueViolation(err) {
		t.Fatal("isUniqueViolation must not match foreign-key violation")
	}
}

// TestIsUniqueViolation_Nil short-circuits cleanly.
func TestIsUniqueViolation_Nil(t *testing.T) {
	if isUniqueViolation(nil) {
		t.Fatal("isUniqueViolation(nil) must be false")
	}
}

// TestEnsureOperatorSlug_OidcHappyPath: a new oidc operator gets the
// email local-part as slug and a canonical alias row.
func TestEnsureOperatorSlug_OidcHappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	expectSlugClaim(mock, int64(42), "alice").
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("alice"))
	expectSlugAliasInsert(mock, "alice", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ensureOperatorSlug(context.Background(), db, 42, "oidc", "alice@example.com"); err != nil {
		t.Fatalf("ensureOperatorSlug: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestEnsureOperatorSlug_AlreadyHasSlug: when the row already has a slug
// (existing operator, e.g. migration 028 backfill already ran), the
// UPDATE matches zero rows, the alias write is skipped, and we return
// cleanly. This is the idempotent re-entry case driven by the cold-path
// ON CONFLICT DO UPDATE on operators.
func TestEnsureOperatorSlug_AlreadyHasSlug(t *testing.T) {
	db, mock := newMockDB(t)
	expectSlugClaim(mock, int64(42), "alice").
		WillReturnError(sql.ErrNoRows)

	if err := ensureOperatorSlug(context.Background(), db, 42, "oidc", "alice@example.com"); err != nil {
		t.Fatalf("ensureOperatorSlug: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestEnsureOperatorSlug_CollisionRetriesWithOpID: when the candidate slug
// collides (partial UNIQUE INDEX operators_slug_uniq, SQLSTATE 23505),
// we retry with the opaque op-<id> form and seed its alias.
func TestEnsureOperatorSlug_CollisionRetriesWithOpID(t *testing.T) {
	db, mock := newMockDB(t)
	expectSlugClaim(mock, int64(42), "alice").
		WillReturnError(&pq.Error{Code: "23505", Message: "operators_slug_uniq"})
	expectSlugClaim(mock, int64(42), "op-42").
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("op-42"))
	expectSlugAliasInsert(mock, "op-42", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ensureOperatorSlug(context.Background(), db, 42, "oidc", "alice@example.com"); err != nil {
		t.Fatalf("ensureOperatorSlug: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestEnsureOperatorSlug_NonUniqueErrorBubbles: a non-unique DB error
// (e.g. connection failure) on the claim must bubble up, not silently
// fall through to op-<id> fallback. Otherwise transient DB issues mask
// the real failure mode.
func TestEnsureOperatorSlug_NonUniqueErrorBubbles(t *testing.T) {
	db, mock := newMockDB(t)
	expectSlugClaim(mock, int64(42), "alice").
		WillReturnError(sql.ErrConnDone)

	err := ensureOperatorSlug(context.Background(), db, 42, "oidc", "alice@example.com")
	if !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("err = %v, want sql.ErrConnDone", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestEnsureOperatorSlug_SlackUsesOpID: slack operators don't have an
// email local-part, so candidateOperatorSlug returns op-<id> directly —
// only one claim attempt is needed.
func TestEnsureOperatorSlug_SlackUsesOpID(t *testing.T) {
	db, mock := newMockDB(t)
	expectSlugClaim(mock, int64(7), "op-7").
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("op-7"))
	expectSlugAliasInsert(mock, "op-7", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ensureOperatorSlug(context.Background(), db, 7, "slack", "slack-U0OPERATOR1@noreply.kbeads.local"); err != nil {
		t.Fatalf("ensureOperatorSlug: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
