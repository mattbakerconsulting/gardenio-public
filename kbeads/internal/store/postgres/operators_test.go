package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// TestUpsertOperator pins the cold-path INSERT…ON CONFLICT…RETURNING shape so
// the resolver gets the operator id back without a second round-trip.
// Migration 027 (ka-0000008244) added the email column to the insert with
// derivation via deriveOperatorEmail — the oidc case uses subject as email.
func TestUpsertOperator(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`INSERT INTO operators \(kind, subject, email\)\s+VALUES \(\$1, \$2, \$3\)\s+ON CONFLICT \(kind, subject\)\s+DO UPDATE SET updated_at = NOW\(\)\s+RETURNING id`).
		WithArgs("oidc", "alice@example.com", "alice@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
	expectSlugClaim(mock, int64(42), "alice").
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("alice"))
	expectSlugAliasInsert(mock, "alice", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	id, err := store.UpsertOperator(context.Background(), "oidc", "alice@example.com")
	if err != nil {
		t.Fatalf("UpsertOperator: %v", err)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUpsertOperator_SlackUsesSentinelEmail confirms the cold-path slack
// upsert writes the synthetic `.local` sentinel — the C3 resync job
// (ka-0000000483) overwrites this with the real email within 6h.
func TestUpsertOperator_SlackUsesSentinelEmail(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`INSERT INTO operators \(kind, subject, email\)\s+VALUES \(\$1, \$2, \$3\)\s+ON CONFLICT \(kind, subject\)\s+DO UPDATE SET updated_at = NOW\(\)\s+RETURNING id`).
		WithArgs("slack", "U0OPERATOR1", "slack-U0OPERATOR1@noreply.kbeads.local").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	expectSlugClaim(mock, int64(7), "op-7").
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("op-7"))
	expectSlugAliasInsert(mock, "op-7", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	id, err := store.UpsertOperator(context.Background(), "slack", "U0OPERATOR1")
	if err != nil {
		t.Fatalf("UpsertOperator: %v", err)
	}
	if id != 7 {
		t.Fatalf("id = %d, want 7", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUpsertOperator_UnknownKindFailsLoud is R2's structured-error
// guarantee: a caller passing a kind we have not wired email derivation
// for gets store.UpsertOperatorEmailError without any DB traffic, instead
// of an opaque CHECK violation from migration 027.
func TestUpsertOperator_UnknownKindFailsLoud(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	_, err := st.UpsertOperator(context.Background(), "future-saml", "alice@x")
	if err == nil {
		t.Fatal("expected error")
	}
	var ue *store.UpsertOperatorEmailError
	if !errors.As(err, &ue) {
		t.Fatalf("err type = %T, want *store.UpsertOperatorEmailError", err)
	}
	if ue.Kind != "future-saml" || ue.Subject != "alice@x" {
		t.Errorf("err = %+v, want kind=future-saml subject=alice@x", ue)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUpsertOperator_OidcMalformedSubjectFailsLoud — an oidc row whose
// subject isn't an email is just as bad as an unknown kind; we surface
// the same structured error rather than silently writing a synthetic
// `.local` sentinel under the oidc kind.
func TestUpsertOperator_OidcMalformedSubjectFailsLoud(t *testing.T) {
	db, _ := newMockDB(t)
	st := &PostgresStore{db: db}

	_, err := st.UpsertOperator(context.Background(), "oidc", "no-at-sign")
	var ue *store.UpsertOperatorEmailError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *store.UpsertOperatorEmailError", err)
	}
}

// TestUpsertOperator_SlackNonUSubjectFailsLoud is the ka-0000001767 guard:
// a kind=slack subject that is a display name or email — not a Slack U-id —
// must fail loud with store.UpsertOperatorEmailError and run NO DB traffic,
// so a junk slack operator row (op-264 'matt.baker', op-297 'Matt Baker')
// can never be minted. The non-U value routes back through the resolver
// (display/email lookup) instead. newMockDB's ExpectationsWereMet asserts
// the absence of any INSERT — no expectation is registered, so any query
// would fail the mock.
func TestUpsertOperator_SlackNonUSubjectFailsLoud(t *testing.T) {
	for _, subject := range []string{"Matt Baker", "matt.baker"} {
		t.Run(subject, func(t *testing.T) {
			db, mock := newMockDB(t)
			st := &PostgresStore{db: db}

			_, err := st.UpsertOperator(context.Background(), "slack", subject)
			var ue *store.UpsertOperatorEmailError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v, want *store.UpsertOperatorEmailError", err)
			}
			if ue.Kind != "slack" || ue.Subject != subject {
				t.Errorf("err = %+v, want kind=slack subject=%q", ue, subject)
			}
			// No INSERT/UPDATE was expected on the mock — a write here would
			// be an unmatched expectation. This is the "does NOT create a new
			// slack operator row" assertion.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}

// TestDeriveOperatorEmail tabulates the per-kind derivation matrix so it
// stays load-bearing as a spec, not just an implementation detail.
func TestDeriveOperatorEmail(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		subject   string
		wantEmail string
		wantOK    bool
	}{
		{"oidc-pass-through", "oidc", "alice@example.com", "alice@example.com", true},
		{"oidc-no-at", "oidc", "alice", "", false},
		{"oidc-only-at", "oidc", "@", "", false},
		{"oidc-trailing-at", "oidc", "alice@", "", false},
		{"slack-sentinel", "slack", "U0OPERATOR1", "slack-U0OPERATOR1@noreply.kbeads.local", true},
		// ka-0000001767: a slack subject that is not a Slack U-id (display
		// name, email, lowercase handle) must be rejected so it never mints a
		// junk slack operator row — the prod junk rows op-264 'matt.baker',
		// op-297 'Matt Baker' are exactly what this guard prevents.
		{"slack-display-name", "slack", "Matt Baker", "", false},
		{"slack-email-handle", "slack", "matt.baker", "", false},
		{"slack-lowercase-u", "slack", "u0operator1", "", false},
		{"slack-bare-u", "slack", "U", "", false},
		{"slack-missing-u-prefix", "slack", "07K1RLQAKF", "", false},
		{"system-sentinel", "system", "scheduler", "system-scheduler@noreply.kbeads.local", true},
		{"fleet-sentinel", "fleet", "gardenio", "fleet-gardenio@noreply.kbeads.local", true},
		{"machine-sentinel", "machine", "gardenio-controller", "machine-gardenio-controller@noreply.kbeads.local", true},
		{"gitlab-sentinel", "gitlab", "42", "gitlab-42@noreply.kbeads.local", true},
		{"jira-sentinel", "jira", "PROD-1", "jira-PROD-1@noreply.kbeads.local", true},
		{"unknown-kind", "future-saml", "anyone", "", false},
		{"empty-subject", "oidc", "", "", false},
		{"whitespace-subject", "slack", "   ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := deriveOperatorEmail(tc.kind, tc.subject)
			if ok != tc.wantOK || got != tc.wantEmail {
				t.Errorf("deriveOperatorEmail(%q, %q) = (%q, %v), want (%q, %v)",
					tc.kind, tc.subject, got, ok, tc.wantEmail, tc.wantOK)
			}
		})
	}
}

// TestLookupOperatorAlias_Hit pins the hot-path single-row probe shape that the
// resolver runs once per audit-bearing write.
func TestLookupOperatorAlias_Hit(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("slack", "U0OPERATOR1").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(7)))

	id, err := store.LookupOperatorAlias(context.Background(), "slack", "U0OPERATOR1")
	if err != nil {
		t.Fatalf("LookupOperatorAlias: %v", err)
	}
	if id != 7 {
		t.Fatalf("id = %d, want 7", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestLookupOperatorAlias_Miss exercises the cold-path entry condition: a miss
// returns sql.ErrNoRows so the resolver knows to upsert.
func TestLookupOperatorAlias_Miss(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("oidc", "newcomer@example.com").
		WillReturnError(sql.ErrNoRows)

	_, err := store.LookupOperatorAlias(context.Background(), "oidc", "newcomer@example.com")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// TestUpsertOperatorAlias pins the alias write that completes the cold-path:
// either inserts a fresh alias or repoints an existing one (ADR §3.1
// consolidation-in-place).
func TestUpsertOperatorAlias(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`INSERT INTO operator_aliases \(operator_id, kind, subject, source\)\s+VALUES \(\$1, \$2, \$3, \$4\)\s+ON CONFLICT \(kind, subject\)\s+DO UPDATE SET operator_id = EXCLUDED\.operator_id`).
		WithArgs(int64(42), "oidc", "alice@example.com", "resolver").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.UpsertOperatorAlias(context.Background(), 42, "oidc", "alice@example.com", "resolver"); err != nil {
		t.Fatalf("UpsertOperatorAlias: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestListOperatorAliasesByKind pins the join shape used by the Slack
// users.info resync job (ADR §7 follow-up #1) — alias rows joined with the
// parent operator's display/email so the caller can skip no-op refreshes.
func TestListOperatorAliasesByKind(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT a\.operator_id, a\.subject, o\.display, o\.email\s+FROM operator_aliases a\s+JOIN operators o ON o\.id = a\.operator_id\s+WHERE a\.kind = \$1\s+ORDER BY a\.operator_id`).
		WithArgs("slack").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "subject", "display", "email"}).
			AddRow(int64(1), "U1", "Alice", "alice@example.com").
			AddRow(int64(2), "U2", "", ""))

	rows, err := store.ListOperatorAliasesByKind(context.Background(), "slack")
	if err != nil {
		t.Fatalf("ListOperatorAliasesByKind: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].OperatorID != 1 || rows[0].Subject != "U1" || rows[0].Display != "Alice" || rows[0].Email != "alice@example.com" {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1].OperatorID != 2 || rows[1].Subject != "U2" || rows[1].Display != "" || rows[1].Email != "" {
		t.Errorf("rows[1] = %+v", rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestListOperatorsWithSlugs pins the SELECT shape used by the public /people
// directory feed (M2 §4, ka-0000000340). Only operators with a non-null slug
// are returned; result is ordered by slug ascending and bounded by limit.
func TestListOperatorsWithSlugs(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT id, kind, subject, COALESCE\(display, ''\), COALESCE\(email, ''\), slug\s+FROM operators\s+WHERE slug IS NOT NULL\s+ORDER BY slug ASC\s+LIMIT \$1`).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "subject", "display", "email", "slug"}).
			AddRow(int64(1), "oidc", "alice@example.com", "Alice", "alice@example.com", "alice").
			AddRow(int64(2), "oidc", "bob@example.com", "", "bob@example.com", "bob"))

	rows, err := store.ListOperatorsWithSlugs(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListOperatorsWithSlugs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Slug != "alice" || rows[0].Display != "Alice" {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1].Slug != "bob" || rows[1].Email != "bob@example.com" {
		t.Errorf("rows[1] = %+v", rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestListOperatorsWithSlugs_DefaultLimit confirms a zero / negative limit
// arg falls back to the 500-row default so unbounded scans never reach the
// daemon.
func TestListOperatorsWithSlugs_DefaultLimit(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT id, kind, subject, COALESCE\(display, ''\), COALESCE\(email, ''\), slug\s+FROM operators\s+WHERE slug IS NOT NULL\s+ORDER BY slug ASC\s+LIMIT \$1`).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "subject", "display", "email", "slug"}))

	rows, err := store.ListOperatorsWithSlugs(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListOperatorsWithSlugs: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("len(rows) = %d, want 0", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUpdateOperatorIdentity_RowFound pins the UPDATE shape (updated_at moves
// only when display or email changes) and confirms
// (true, nil) on a successful row update.
func TestUpdateOperatorIdentity_RowFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE operators\s+SET display = \$1,\s+email = \$2,\s+updated_at = CASE\s+WHEN display IS DISTINCT FROM \$1 OR email IS DISTINCT FROM \$2 THEN NOW\(\)\s+ELSE updated_at\s+END\s+WHERE id = \$3`).
		WithArgs("Alice", "alice@example.com", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := store.UpdateOperatorIdentity(context.Background(), 42, "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("UpdateOperatorIdentity: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUpdateOperatorIdentity_NoRow returns (false, nil) when the operator id
// no longer exists — caller drops the alias from its retry queue.
func TestUpdateOperatorIdentity_NoRow(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE operators\s+SET display = \$1,\s+email = \$2,\s+updated_at = CASE\s+WHEN display IS DISTINCT FROM \$1 OR email IS DISTINCT FROM \$2 THEN NOW\(\)\s+ELSE updated_at\s+END\s+WHERE id = \$3`).
		WithArgs("Ghost", "g@x", int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := store.UpdateOperatorIdentity(context.Background(), 999, "Ghost", "g@x")
	if err != nil {
		t.Fatalf("UpdateOperatorIdentity: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestMergeOperators pins the five-table UPDATE sweep that gb identity
// link-operator runs to repoint operated_by_id from a legacy slack operator
// to the caller's OIDC operator (ADR §3.1 consolidation-in-place).
func TestMergeOperators(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	tables := []string{"beads", "comments", "deps", "events", "mutation_history"}
	rowsPerTable := []int64{3, 5, 2, 7, 1}
	for i, table := range tables {
		mock.ExpectExec(`UPDATE `+table+` SET operated_by_id = \$1 WHERE operated_by_id = \$2`).
			WithArgs(int64(99), int64(17)).
			WillReturnResult(sqlmock.NewResult(0, rowsPerTable[i]))
	}
	// Slug absorb (ka-0000004514) runs after the five audit-FK sweeps: the
	// loser's slug aliases repoint+retire onto the survivor, then the loser's
	// operators.slug is NULLed. Neither is counted in the repointed total.
	mock.ExpectExec(`UPDATE operator_slug_aliases\s+SET operator_id = \$1,\s+retired_at = COALESCE\(retired_at, NOW\(\)\)\s+WHERE operator_id = \$2`).
		WithArgs(int64(99), int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE operators SET slug = NULL WHERE id = \$1`).
		WithArgs(int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	total, err := store.MergeOperators(context.Background(), 17, 99)
	if err != nil {
		t.Fatalf("MergeOperators: %v", err)
	}
	want := int64(3 + 5 + 2 + 7 + 1)
	if total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestMergeOperators_NoOp short-circuits without touching the DB when oldID
// and newID are equal or non-positive.
func TestMergeOperators_NoOp(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	cases := []struct {
		name         string
		oldID, newID int64
	}{
		{"same ids", 42, 42},
		{"zero old", 0, 1},
		{"zero new", 1, 0},
		{"negative", -1, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := store.MergeOperators(context.Background(), tc.oldID, tc.newID)
			if err != nil {
				t.Fatalf("MergeOperators: %v", err)
			}
			if n != 0 {
				t.Fatalf("n = %d, want 0 (short-circuit)", n)
			}
		})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestMergeOperators_PropagatesError surfaces the DB error and the partial
// row count so the caller can log how far the sweep got before failing.
func TestMergeOperators_PropagatesError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads SET operated_by_id = \$1 WHERE operated_by_id = \$2`).
		WithArgs(int64(99), int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(`UPDATE comments SET operated_by_id = \$1 WHERE operated_by_id = \$2`).
		WithArgs(int64(99), int64(17)).
		WillReturnError(sql.ErrConnDone)

	n, err := store.MergeOperators(context.Background(), 17, 99)
	if err == nil {
		t.Fatal("expected error")
	}
	if n != 4 {
		t.Fatalf("partial total = %d, want 4 (beads updated before comments failed)", n)
	}
}

// TestResolveOperators_JoinsAndRedactsForgotten pins the server-side
// operator_id -> identity join behind the engagement / security dashboards
// (ka-0000004824). It asserts the single-parameter ANY($1::bigint[]) shape
// (so the lib/pq 65535 cap can never bite) AND the right-to-be-forgotten
// redaction: forgotten=true => Display "" / Email "deleted operator", opaque
// id preserved.
func TestResolveOperators_JoinsAndRedactsForgotten(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT id,\s+COALESCE\(display, ''\),\s+COALESCE\(email, ''\),\s+COALESCE\(slug, ''\),\s+forgotten_at IS NOT NULL AS forgotten\s+FROM operators\s+WHERE id = ANY\(\$1::bigint\[\]\)`).
		WithArgs(pq.Array([]int64{7, 9})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "display", "email", "slug", "forgotten"}).
			AddRow(int64(7), "Matthew Baker", "matthew.baker@acme.example", "matthew.baker", false).
			AddRow(int64(9), "Should Not Appear", "gone@acme.example", "gone", true))

	got, err := st.ResolveOperators(context.Background(), []int64{7, 9})
	if err != nil {
		t.Fatalf("ResolveOperators: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d resolved, want 2", len(got))
	}
	if r := got[7]; r.Display != "Matthew Baker" || r.Email != "matthew.baker@acme.example" || r.Slug != "matthew.baker" || r.Forgotten {
		t.Errorf("operator 7 = %+v, want full identity, not forgotten", r)
	}
	r := got[9]
	if !r.Forgotten {
		t.Error("operator 9 must be Forgotten=true")
	}
	if r.Display != "" {
		t.Errorf("forgotten display = %q, want empty", r.Display)
	}
	if r.Email != store.ForgottenOperatorEmail {
		t.Errorf("forgotten email = %q, want %q", r.Email, store.ForgottenOperatorEmail)
	}
	if r.ID != 9 {
		t.Errorf("forgotten opaque id = %d, want 9 (must persist)", r.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestResolveOperators_EmptyInputNoQuery confirms an empty id slice short-
// circuits to an empty, non-nil map without touching the DB.
func TestResolveOperators_EmptyInputNoQuery(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	got, err := st.ResolveOperators(context.Background(), nil)
	if err != nil {
		t.Fatalf("ResolveOperators(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v, want empty non-nil map", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
