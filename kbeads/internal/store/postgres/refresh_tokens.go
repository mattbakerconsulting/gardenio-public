package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gardenio/kbeads/internal/model"
)

// InsertRefreshToken persists a freshly-issued refresh token row. Inserts
// are NOT idempotent on the hash — issuance always uses a fresh random
// secret, so a primary-key collision is a programming error.
func (s *PostgresStore) InsertRefreshToken(ctx context.Context, rt *model.RefreshToken) error {
	return insertRefreshToken(ctx, s.db, rt)
}

// GetRefreshToken returns the persisted row for the given SHA-256 hash, or
// sql.ErrNoRows when not found.
func (s *PostgresStore) GetRefreshToken(ctx context.Context, hash string) (*model.RefreshToken, error) {
	return getRefreshToken(ctx, s.db, hash)
}

// RevokeRefreshTokenChain marks the row identified by hash and all
// descendants (rows whose parent_refresh_hash chain transitively reaches
// hash) as revoked. Returns the number of newly-revoked rows. Already-
// revoked rows are left alone (revoked_at is preserved).
func (s *PostgresStore) RevokeRefreshTokenChain(ctx context.Context, hash string, revokedAt time.Time) (int, error) {
	return revokeRefreshTokenChain(ctx, s.db, hash, revokedAt)
}

// ListActiveRefreshTokenHashesByFleet returns the hashes of every unrevoked
// refresh token issued to the fleet.
func (s *PostgresStore) ListActiveRefreshTokenHashesByFleet(ctx context.Context, fleet string) ([]string, error) {
	return listActiveRefreshTokenHashesByFleet(ctx, s.db, fleet)
}

func (s *txStore) ListActiveRefreshTokenHashesByFleet(ctx context.Context, fleet string) ([]string, error) {
	return listActiveRefreshTokenHashesByFleet(ctx, s.tx, fleet)
}

func (s *txStore) InsertRefreshToken(ctx context.Context, rt *model.RefreshToken) error {
	return insertRefreshToken(ctx, s.tx, rt)
}

func (s *txStore) GetRefreshToken(ctx context.Context, hash string) (*model.RefreshToken, error) {
	return getRefreshToken(ctx, s.tx, hash)
}

func (s *txStore) RevokeRefreshTokenChain(ctx context.Context, hash string, revokedAt time.Time) (int, error) {
	return revokeRefreshTokenChain(ctx, s.tx, hash, revokedAt)
}

// dbExec is the minimal interface satisfied by both *sql.DB and *sql.Tx.
type refreshTokenExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertRefreshToken(ctx context.Context, ex refreshTokenExecutor, rt *model.RefreshToken) error {
	scopes := rt.ScopesJSON
	if len(scopes) == 0 {
		scopes = []byte("{}")
	}
	var parent any
	if rt.ParentRefreshHash != "" {
		parent = rt.ParentRefreshHash
	}
	_, err := ex.ExecContext(ctx, `
		INSERT INTO refresh_tokens (token_hash, fleet, project, scopes_json, subject, expires_at, parent_refresh_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rt.TokenHash, rt.Fleet, rt.Project, scopes, rt.Subject, rt.ExpiresAt, parent,
	)
	return err
}

func getRefreshToken(ctx context.Context, ex refreshTokenExecutor, hash string) (*model.RefreshToken, error) {
	if hash == "" {
		return nil, sql.ErrNoRows
	}
	row := ex.QueryRowContext(ctx, `
		SELECT token_hash, fleet, project, scopes_json, subject,
		       created_at, expires_at, revoked_at, parent_refresh_hash
		  FROM refresh_tokens
		 WHERE token_hash = $1`, hash)

	var (
		rt        model.RefreshToken
		scopes    []byte
		revokedAt sql.NullTime
		parent    sql.NullString
	)
	if err := row.Scan(
		&rt.TokenHash, &rt.Fleet, &rt.Project, &scopes, &rt.Subject,
		&rt.CreatedAt, &rt.ExpiresAt, &revokedAt, &parent,
	); err != nil {
		return nil, err
	}
	rt.ScopesJSON = scopes
	if revokedAt.Valid {
		t := revokedAt.Time
		rt.RevokedAt = &t
	}
	if parent.Valid {
		rt.ParentRefreshHash = parent.String
	}
	return &rt, nil
}

func revokeRefreshTokenChain(ctx context.Context, ex refreshTokenExecutor, hash string, revokedAt time.Time) (int, error) {
	if hash == "" {
		return 0, errors.New("refresh-token hash is required")
	}
	if revokedAt.IsZero() {
		revokedAt = time.Now().UTC()
	}
	res, err := ex.ExecContext(ctx, `
		WITH RECURSIVE chain AS (
		    SELECT token_hash FROM refresh_tokens WHERE token_hash = $1
		  UNION ALL
		    SELECT rt.token_hash
		      FROM refresh_tokens rt
		      JOIN chain c ON rt.parent_refresh_hash = c.token_hash
		)
		UPDATE refresh_tokens
		   SET revoked_at = $2
		 WHERE token_hash IN (SELECT token_hash FROM chain)
		   AND revoked_at IS NULL`,
		hash, revokedAt,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func listActiveRefreshTokenHashesByFleet(ctx context.Context, ex refreshTokenExecutor, fleet string) ([]string, error) {
	rows, err := ex.QueryContext(ctx, `
		SELECT token_hash FROM refresh_tokens
		 WHERE fleet = $1 AND revoked_at IS NULL`, fleet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}
