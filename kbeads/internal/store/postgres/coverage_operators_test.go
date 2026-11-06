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

// TestCovOpsBeginTx opens a transaction on the mock db and returns a *txStore
// wrapping it. Callers use this to exercise the *txStore operator wrappers
// (which the *PostgresStore-focused operators_test.go does not touch). The
// caller is responsible for registering the matching mock.ExpectBegin() and a
// terminal mock.ExpectCommit()/ExpectRollback() expectation; this helper only
// issues the BeginTx so the *sql.Tx is real.
func testCovOpsBeginTx(t *testing.T, db *sql.DB) *txStore {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	return &txStore{tx: tx}
}

// TestCovOpsTxUpsertOperator covers the *txStore.UpsertOperator wrapper end to
// end: the INSERT…ON CONFLICT…RETURNING + the slug-seed follow-ups run against
// the *sql.Tx rather than the pool.
func TestCovOpsTxUpsertOperator(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectQuery(`INSERT INTO operators \(kind, subject, email\)\s+VALUES \(\$1, \$2, \$3\)\s+ON CONFLICT \(kind, subject\)\s+DO UPDATE SET updated_at = NOW\(\)\s+RETURNING id`).
		WithArgs("oidc", "tx-alice@example.com", "tx-alice@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(101)))
	expectSlugClaim(mock, int64(101), "tx-alice").
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("tx-alice"))
	expectSlugAliasInsert(mock, "tx-alice", int64(101)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	id, err := ts.UpsertOperator(context.Background(), "oidc", "tx-alice@example.com")
	if err != nil {
		t.Fatalf("txStore.UpsertOperator: %v", err)
	}
	if id != 101 {
		t.Fatalf("id = %d, want 101", id)
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsTxUpsertOperatorEmailError covers the no-DB-traffic error path on
// the *txStore receiver: an underivable (kind, subject) returns the typed
// store.UpsertOperatorEmailError without issuing any statement.
func TestCovOpsTxUpsertOperatorEmailError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	_, err := ts.UpsertOperator(context.Background(), "future-kind", "whoever")
	var ue *store.UpsertOperatorEmailError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *store.UpsertOperatorEmailError", err)
	}
	if ue.Kind != "future-kind" || ue.Subject != "whoever" {
		t.Errorf("err = %+v, want kind=future-kind subject=whoever", ue)
	}
	mock.ExpectRollback()
	if err := ts.tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}

// TestCovOpsUpsertOperatorReturnError covers the upsertOperator branch where
// the RETURNING scan itself errors — the function returns (0, err) without
// reaching the slug seed.
func TestCovOpsUpsertOperatorReturnError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`INSERT INTO operators \(kind, subject, email\)`).
		WithArgs("system", "scheduler", "system-scheduler@noreply.kbeads.local").
		WillReturnError(sql.ErrConnDone)

	id, err := st.UpsertOperator(context.Background(), "system", "scheduler")
	if !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("err = %v, want sql.ErrConnDone", err)
	}
	if id != 0 {
		t.Fatalf("id = %d, want 0 on error", id)
	}
}

// TestCovOpsLookupOperatorByAlias_Hit covers lookupOperatorByAlias via the
// *PostgresStore wrapper: the alias→operators PK join returns both the
// operator id and the canonical operator kind.
func TestCovOpsLookupOperatorByAlias_Hit(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT a\.operator_id, o\.kind\s+FROM operator_aliases a\s+JOIN operators o ON o\.id = a\.operator_id\s+WHERE a\.kind = \$1 AND a\.subject = \$2`).
		WithArgs("slack", "U0OPERATOR1").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "kind"}).AddRow(int64(7), "oidc"))

	id, kind, err := st.LookupOperatorByAlias(context.Background(), "slack", "U0OPERATOR1")
	if err != nil {
		t.Fatalf("LookupOperatorByAlias: %v", err)
	}
	if id != 7 || kind != "oidc" {
		t.Fatalf("got (id=%d, kind=%q), want (7, oidc)", id, kind)
	}
}

// TestCovOpsLookupOperatorByAlias_Miss covers the sql.ErrNoRows branch of
// lookupOperatorByAlias — the resolver treats this as a cold-cache miss.
func TestCovOpsLookupOperatorByAlias_Miss(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT a\.operator_id, o\.kind\s+FROM operator_aliases a`).
		WithArgs("oidc", "nobody@example.com").
		WillReturnError(sql.ErrNoRows)

	id, kind, err := st.LookupOperatorByAlias(context.Background(), "oidc", "nobody@example.com")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
	if id != 0 || kind != "" {
		t.Fatalf("got (id=%d, kind=%q), want (0, \"\")", id, kind)
	}
}

// TestCovOpsTxLookupOperatorByAlias covers the *txStore.LookupOperatorByAlias
// wrapper against a real *sql.Tx.
func TestCovOpsTxLookupOperatorByAlias(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectQuery(`SELECT a\.operator_id, o\.kind\s+FROM operator_aliases a`).
		WithArgs("oidc", "carol@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "kind"}).AddRow(int64(55), "oidc"))
	mock.ExpectCommit()

	id, kind, err := ts.LookupOperatorByAlias(context.Background(), "oidc", "carol@example.com")
	if err != nil {
		t.Fatalf("txStore.LookupOperatorByAlias: %v", err)
	}
	if id != 55 || kind != "oidc" {
		t.Fatalf("got (id=%d, kind=%q), want (55, oidc)", id, kind)
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsTxLookupOperatorAlias covers the *txStore.LookupOperatorAlias
// single-row probe wrapper.
func TestCovOpsTxLookupOperatorAlias(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("slack", "U999").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(88)))
	mock.ExpectCommit()

	id, err := ts.LookupOperatorAlias(context.Background(), "slack", "U999")
	if err != nil {
		t.Fatalf("txStore.LookupOperatorAlias: %v", err)
	}
	if id != 88 {
		t.Fatalf("id = %d, want 88", id)
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsTxUpsertOperatorAlias covers the *txStore.UpsertOperatorAlias
// wrapper — alias insert/repoint against the tx.
func TestCovOpsTxUpsertOperatorAlias(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectExec(`INSERT INTO operator_aliases \(operator_id, kind, subject, source\)\s+VALUES \(\$1, \$2, \$3, \$4\)\s+ON CONFLICT \(kind, subject\)\s+DO UPDATE SET operator_id = EXCLUDED\.operator_id`).
		WithArgs(int64(7), "oidc", "dave@example.com", "resolver").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := ts.UpsertOperatorAlias(context.Background(), 7, "oidc", "dave@example.com", "resolver"); err != nil {
		t.Fatalf("txStore.UpsertOperatorAlias: %v", err)
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsTxListOperatorAliasesByKind covers the *txStore variant of the
// resync-job join (it has its own method body, distinct from the
// *PostgresStore one).
func TestCovOpsTxListOperatorAliasesByKind(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectQuery(`SELECT a\.operator_id, a\.subject, o\.display, o\.email\s+FROM operator_aliases a\s+JOIN operators o ON o\.id = a\.operator_id\s+WHERE a\.kind = \$1\s+ORDER BY a\.operator_id`).
		WithArgs("slack").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "subject", "display", "email"}).
			AddRow(int64(1), "U1", "Alice", "alice@example.com"))
	mock.ExpectCommit()

	rows, err := ts.ListOperatorAliasesByKind(context.Background(), "slack")
	if err != nil {
		t.Fatalf("txStore.ListOperatorAliasesByKind: %v", err)
	}
	if len(rows) != 1 || rows[0].OperatorID != 1 || rows[0].Subject != "U1" {
		t.Fatalf("rows = %+v, want one row for U1", rows)
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsListOperatorAliasesByKindQueryError covers the early error return
// of the *PostgresStore variant (Query fails before the row loop).
func TestCovOpsListOperatorAliasesByKindQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT a\.operator_id, a\.subject, o\.display, o\.email\s+FROM operator_aliases a`).
		WithArgs("oidc").
		WillReturnError(sql.ErrConnDone)

	_, err := st.ListOperatorAliasesByKind(context.Background(), "oidc")
	if !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("err = %v, want sql.ErrConnDone", err)
	}
}

// TestCovOpsTxListOperatorsWithSlugs covers the *txStore.ListOperatorsWithSlugs
// wrapper and confirms the default-limit fallback flows through the tx path.
func TestCovOpsTxListOperatorsWithSlugs(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectQuery(`SELECT id, kind, subject, COALESCE\(display, ''\), COALESCE\(email, ''\), slug\s+FROM operators\s+WHERE slug IS NOT NULL\s+ORDER BY slug ASC\s+LIMIT \$1`).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "subject", "display", "email", "slug"}).
			AddRow(int64(3), "oidc", "carol@example.com", "Carol", "carol@example.com", "carol"))
	mock.ExpectCommit()

	rows, err := ts.ListOperatorsWithSlugs(context.Background(), 0)
	if err != nil {
		t.Fatalf("txStore.ListOperatorsWithSlugs: %v", err)
	}
	if len(rows) != 1 || rows[0].Slug != "carol" {
		t.Fatalf("rows = %+v, want one row slug=carol", rows)
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsListOperatorsWithSlugsQueryError covers the Query-error early
// return on the package-level listOperatorsWithSlugs via the *PostgresStore
// wrapper.
func TestCovOpsListOperatorsWithSlugsQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM operators\s+WHERE slug IS NOT NULL`).
		WithArgs(10).
		WillReturnError(sql.ErrConnDone)

	_, err := st.ListOperatorsWithSlugs(context.Background(), 10)
	if !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("err = %v, want sql.ErrConnDone", err)
	}
}

// TestCovOpsTxUpdateOperatorIdentity covers the *txStore.UpdateOperatorIdentity
// wrapper, returning ok=true on a matched row.
func TestCovOpsTxUpdateOperatorIdentity(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectExec(`UPDATE operators\s+SET display = \$1,\s+email = \$2,\s+updated_at = CASE\s+WHEN display IS DISTINCT FROM \$1 OR email IS DISTINCT FROM \$2 THEN NOW\(\)\s+ELSE updated_at\s+END\s+WHERE id = \$3`).
		WithArgs("Carol", "carol@example.com", int64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ok, err := ts.UpdateOperatorIdentity(context.Background(), 3, "Carol", "carol@example.com")
	if err != nil {
		t.Fatalf("txStore.UpdateOperatorIdentity: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsUpdateOperatorIdentityExecError covers the ExecContext error path
// of updateOperatorIdentity — (false, err) before RowsAffected is consulted.
func TestCovOpsUpdateOperatorIdentityExecError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE operators\s+SET display = \$1`).
		WithArgs("Boom", "boom@x", int64(5)).
		WillReturnError(sql.ErrConnDone)

	ok, err := st.UpdateOperatorIdentity(context.Background(), 5, "Boom", "boom@x")
	if !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("err = %v, want sql.ErrConnDone", err)
	}
	if ok {
		t.Fatalf("ok = true, want false on error")
	}
}

// TestCovOpsTxMergeOperators covers the *txStore.MergeOperators five-table
// sweep against a real *sql.Tx.
func TestCovOpsTxMergeOperators(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	tables := []string{"beads", "comments", "deps", "events", "mutation_history"}
	for _, table := range tables {
		mock.ExpectExec(`UPDATE `+table+` SET operated_by_id = \$1 WHERE operated_by_id = \$2`).
			WithArgs(int64(99), int64(17)).
			WillReturnResult(sqlmock.NewResult(0, 2))
	}
	// Slug absorb (ka-0000004514): the loser's slug aliases repoint+retire onto
	// the survivor, then the loser's operators.slug is NULLed so it drops out of
	// the /people directory feed. Neither is counted in the repointed total.
	mock.ExpectExec(`UPDATE operator_slug_aliases\s+SET operator_id = \$1,\s+retired_at = COALESCE\(retired_at, NOW\(\)\)\s+WHERE operator_id = \$2`).
		WithArgs(int64(99), int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE operators SET slug = NULL WHERE id = \$1`).
		WithArgs(int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	total, err := ts.MergeOperators(context.Background(), 17, 99)
	if err != nil {
		t.Fatalf("txStore.MergeOperators: %v", err)
	}
	if total != int64(2*len(tables)) {
		t.Fatalf("total = %d, want %d", total, 2*len(tables))
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsTxResolveOperators covers the *txStore.ResolveOperators wrapper and
// the forgotten-redaction branch against a real *sql.Tx.
func TestCovOpsTxResolveOperators(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	mock.ExpectQuery(`FROM operators\s+WHERE id = ANY\(\$1::bigint\[\]\)`).
		WithArgs(pq.Array([]int64{4})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "display", "email", "slug", "forgotten"}).
			AddRow(int64(4), "Gone", "gone@x", "gone", true))
	mock.ExpectCommit()

	got, err := ts.ResolveOperators(context.Background(), []int64{4})
	if err != nil {
		t.Fatalf("txStore.ResolveOperators: %v", err)
	}
	r, ok := got[4]
	if !ok {
		t.Fatalf("operator 4 missing from result %+v", got)
	}
	if !r.Forgotten || r.Display != "" || r.Email != store.ForgottenOperatorEmail {
		t.Fatalf("operator 4 = %+v, want redacted forgotten identity", r)
	}
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsTxResolveOperatorsEmpty covers the empty-input short-circuit on the
// *txStore receiver — no query issued, non-nil empty map.
func TestCovOpsTxResolveOperatorsEmpty(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	ts := testCovOpsBeginTx(t, db)

	got, err := ts.ResolveOperators(context.Background(), nil)
	if err != nil {
		t.Fatalf("txStore.ResolveOperators(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v, want non-nil empty map", got)
	}
	mock.ExpectCommit()
	if err := ts.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestCovOpsResolveOperatorsQueryError covers the Query-error early return of
// resolveOperators via the *PostgresStore wrapper.
func TestCovOpsResolveOperatorsQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM operators\s+WHERE id = ANY\(\$1::bigint\[\]\)`).
		WithArgs(pq.Array([]int64{1, 2})).
		WillReturnError(sql.ErrConnDone)

	_, err := st.ResolveOperators(context.Background(), []int64{1, 2})
	if !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("err = %v, want sql.ErrConnDone", err)
	}
}
