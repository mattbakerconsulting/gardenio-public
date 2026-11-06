package postgres

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// InsertRevokedToken persists a revoked token hash. Inserts are idempotent —
// re-revoking an already-revoked hash is a no-op and returns no error.
func (s *PostgresStore) InsertRevokedToken(ctx context.Context, rt *model.RevokedToken) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO revoked_tokens (token_hash, revoked_by, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (token_hash) DO NOTHING`,
		rt.TokenHash, rt.RevokedBy, rt.Reason,
	)
	return err
}

// ListRevokedTokenHashes returns every persisted revoked-token hash. Called
// at daemon startup to warm the in-process revocation cache.
func (s *PostgresStore) ListRevokedTokenHashes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash FROM revoked_tokens`)
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

func (s *txStore) InsertRevokedToken(ctx context.Context, rt *model.RevokedToken) error {
	_, err := s.tx.ExecContext(ctx, `
		INSERT INTO revoked_tokens (token_hash, revoked_by, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (token_hash) DO NOTHING`,
		rt.TokenHash, rt.RevokedBy, rt.Reason,
	)
	return err
}

func (s *txStore) ListRevokedTokenHashes(ctx context.Context) ([]string, error) {
	rows, err := s.tx.QueryContext(ctx, `SELECT token_hash FROM revoked_tokens`)
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
