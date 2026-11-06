package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// These cover the refresh-token and revoked-token stores (refresh_tokens.go,
// revoked_tokens.go) with sqlmock — both the *PostgresStore and txStore
// receivers plus the key error/edge contracts.

func TestInsertRefreshToken(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rt := &model.RefreshToken{
		TokenHash: "h1",
		Fleet:     "f1",
		Project:   "p1",
		Subject:   "s1",
		ExpiresAt: now.Add(24 * time.Hour),
	}

	// Empty ScopesJSON defaults to "{}"; empty ParentRefreshHash -> NULL.
	mock.ExpectExec(`INSERT INTO refresh_tokens`).
		WithArgs("h1", "f1", "p1", []byte("{}"), "s1", rt.ExpiresAt, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := st.InsertRefreshToken(context.Background(), rt); err != nil {
		t.Fatalf("InsertRefreshToken: %v", err)
	}
}

func TestInsertRefreshToken_WithParentAndScopes(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rt := &model.RefreshToken{
		TokenHash:         "h2",
		Fleet:             "f1",
		Project:           "p1",
		ScopesJSON:        []byte(`{"a":1}`),
		Subject:           "s1",
		ExpiresAt:         now,
		ParentRefreshHash: "h1",
	}
	mock.ExpectExec(`INSERT INTO refresh_tokens`).
		WithArgs("h2", "f1", "p1", []byte(`{"a":1}`), "s1", now, "h1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := st.InsertRefreshToken(context.Background(), rt); err != nil {
		t.Fatalf("InsertRefreshToken: %v", err)
	}
}

func TestGetRefreshToken_EmptyHashShortCircuits(t *testing.T) {
	db, _ := newMockDB(t)
	st := &PostgresStore{db: db}

	// No query should run for an empty hash.
	_, err := st.GetRefreshToken(context.Background(), "")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestGetRefreshToken_Found(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	revoked := now.Add(time.Hour)
	mock.ExpectQuery(`FROM refresh_tokens\s+WHERE token_hash = \$1`).
		WithArgs("h1").
		WillReturnRows(sqlmock.NewRows([]string{
			"token_hash", "fleet", "project", "scopes_json", "subject",
			"created_at", "expires_at", "revoked_at", "parent_refresh_hash",
		}).AddRow("h1", "f1", "p1", []byte(`{"a":1}`), "s1", now, now.Add(24*time.Hour), revoked, "h0"))

	got, err := st.GetRefreshToken(context.Background(), "h1")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if got.TokenHash != "h1" || got.ParentRefreshHash != "h0" {
		t.Errorf("got = %+v, want h1 with parent h0", got)
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(revoked) {
		t.Errorf("RevokedAt = %v, want %v", got.RevokedAt, revoked)
	}
}

func TestGetRefreshToken_NullableFields(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM refresh_tokens`).
		WithArgs("h1").
		WillReturnRows(sqlmock.NewRows([]string{
			"token_hash", "fleet", "project", "scopes_json", "subject",
			"created_at", "expires_at", "revoked_at", "parent_refresh_hash",
		}).AddRow("h1", "f1", "p1", []byte(`{}`), "s1", now, now, nil, nil))

	got, err := st.GetRefreshToken(context.Background(), "h1")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if got.RevokedAt != nil {
		t.Errorf("RevokedAt = %v, want nil", got.RevokedAt)
	}
	if got.ParentRefreshHash != "" {
		t.Errorf("ParentRefreshHash = %q, want empty", got.ParentRefreshHash)
	}
}

func TestGetRefreshToken_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM refresh_tokens`).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)

	if _, err := st.GetRefreshToken(context.Background(), "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestRevokeRefreshTokenChain(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`WITH RECURSIVE chain AS`).
		WithArgs("h1", at).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := st.RevokeRefreshTokenChain(context.Background(), "h1", at)
	if err != nil {
		t.Fatalf("RevokeRefreshTokenChain: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
}

func TestRevokeRefreshTokenChain_EmptyHashErrors(t *testing.T) {
	db, _ := newMockDB(t)
	st := &PostgresStore{db: db}

	// Empty hash is a programming error and must not touch the DB.
	if _, err := st.RevokeRefreshTokenChain(context.Background(), "", time.Now()); err == nil {
		t.Fatal("RevokeRefreshTokenChain(\"\"): err = nil, want non-nil")
	}
}

func TestRevokeRefreshTokenChain_ZeroTimeDefaultsToNow(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	// A zero revokedAt is replaced with time.Now().UTC() inside the helper —
	// match any time arg and assert the call still succeeds.
	mock.ExpectExec(`WITH RECURSIVE chain AS`).
		WithArgs("h1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := st.RevokeRefreshTokenChain(context.Background(), "h1", time.Time{})
	if err != nil {
		t.Fatalf("RevokeRefreshTokenChain: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
}

func TestListActiveRefreshTokenHashesByFleet(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT token_hash FROM refresh_tokens\s+WHERE fleet = \$1 AND revoked_at IS NULL`).
		WithArgs("agent-a").
		WillReturnRows(sqlmock.NewRows([]string{"token_hash"}).AddRow("h1").AddRow("h2"))

	got, err := st.ListActiveRefreshTokenHashesByFleet(context.Background(), "agent-a")
	if err != nil {
		t.Fatalf("ListActiveRefreshTokenHashesByFleet: %v", err)
	}
	if len(got) != 2 || got[0] != "h1" || got[1] != "h2" {
		t.Fatalf("got %v, want [h1 h2]", got)
	}
}

func TestInsertRevokedToken(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectExec(`INSERT INTO revoked_tokens`).
		WithArgs("h1", "matt", "compromised").
		WillReturnResult(sqlmock.NewResult(0, 1))

	rt := &model.RevokedToken{TokenHash: "h1", RevokedBy: "matt", Reason: "compromised"}
	if err := st.InsertRevokedToken(context.Background(), rt); err != nil {
		t.Fatalf("InsertRevokedToken: %v", err)
	}
}

func TestListRevokedTokenHashes(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT token_hash FROM revoked_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"token_hash"}).AddRow("h1").AddRow("h2"))

	hashes, err := st.ListRevokedTokenHashes(context.Background())
	if err != nil {
		t.Fatalf("ListRevokedTokenHashes: %v", err)
	}
	if len(hashes) != 2 || hashes[0] != "h1" {
		t.Errorf("hashes = %v, want [h1 h2]", hashes)
	}
}

func TestListRevokedTokenHashes_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT token_hash FROM revoked_tokens`).
		WillReturnError(errors.New("boom"))

	if _, err := st.ListRevokedTokenHashes(context.Background()); err == nil {
		t.Fatal("ListRevokedTokenHashes: err = nil, want non-nil")
	}
}

// txStore variants run the token stores through a real *sql.Tx.
func TestTxStore_Tokens(t *testing.T) {
	db, mock := newMockDB(t)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO refresh_tokens`).
		WithArgs("h1", "f1", "p1", []byte("{}"), "s1", now, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM refresh_tokens`).
		WithArgs("h1").
		WillReturnRows(sqlmock.NewRows([]string{
			"token_hash", "fleet", "project", "scopes_json", "subject",
			"created_at", "expires_at", "revoked_at", "parent_refresh_hash",
		}).AddRow("h1", "f1", "p1", []byte("{}"), "s1", now, now, nil, nil))
	mock.ExpectExec(`WITH RECURSIVE chain AS`).
		WithArgs("h1", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO revoked_tokens`).
		WithArgs("h1", "matt", "x").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT token_hash FROM revoked_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"token_hash"}).AddRow("h1"))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	if err := ts.InsertRefreshToken(context.Background(), &model.RefreshToken{
		TokenHash: "h1", Fleet: "f1", Project: "p1", Subject: "s1", ExpiresAt: now,
	}); err != nil {
		t.Fatalf("txStore.InsertRefreshToken: %v", err)
	}
	if _, err := ts.GetRefreshToken(context.Background(), "h1"); err != nil {
		t.Fatalf("txStore.GetRefreshToken: %v", err)
	}
	if _, err := ts.RevokeRefreshTokenChain(context.Background(), "h1", now); err != nil {
		t.Fatalf("txStore.RevokeRefreshTokenChain: %v", err)
	}
	if err := ts.InsertRevokedToken(context.Background(), &model.RevokedToken{TokenHash: "h1", RevokedBy: "matt", Reason: "x"}); err != nil {
		t.Fatalf("txStore.InsertRevokedToken: %v", err)
	}
	if _, err := ts.ListRevokedTokenHashes(context.Background()); err != nil {
		t.Fatalf("txStore.ListRevokedTokenHashes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
