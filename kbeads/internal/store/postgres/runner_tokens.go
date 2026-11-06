package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gardenio/kbeads/internal/model"
)

// InsertRunnerToken persists a freshly-minted runner token row (hash only).
func (s *PostgresStore) InsertRunnerToken(ctx context.Context, rt *model.RunnerToken) error {
	return insertRunnerToken(ctx, s.db, rt)
}

// GetRunnerTokenByHash returns the row for the given SHA-256 hash, or
// (nil, nil) when no such token was ever issued.
func (s *PostgresStore) GetRunnerTokenByHash(ctx context.Context, hash string) (*model.RunnerToken, error) {
	return getRunnerTokenByHash(ctx, s.db, hash)
}

// RevokeRunnerTokens stamps revoked_at on every unrevoked token issued to
// runnerID and returns how many rows it revoked.
func (s *PostgresStore) RevokeRunnerTokens(ctx context.Context, runnerID string, at time.Time) (int, error) {
	return revokeRunnerTokens(ctx, s.db, runnerID, at)
}

func (s *txStore) InsertRunnerToken(ctx context.Context, rt *model.RunnerToken) error {
	return insertRunnerToken(ctx, s.tx, rt)
}

func (s *txStore) GetRunnerTokenByHash(ctx context.Context, hash string) (*model.RunnerToken, error) {
	return getRunnerTokenByHash(ctx, s.tx, hash)
}

func (s *txStore) RevokeRunnerTokens(ctx context.Context, runnerID string, at time.Time) (int, error) {
	return revokeRunnerTokens(ctx, s.tx, runnerID, at)
}

func insertRunnerToken(ctx context.Context, ex refreshTokenExecutor, rt *model.RunnerToken) error {
	createdAt := rt.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO runner_tokens (token_hash, runner_id, created_at) VALUES ($1, $2, $3)`,
		rt.TokenHash, rt.RunnerID, createdAt)
	return err
}

func getRunnerTokenByHash(ctx context.Context, ex refreshTokenExecutor, hash string) (*model.RunnerToken, error) {
	var rt model.RunnerToken
	var revoked sql.NullTime
	err := ex.QueryRowContext(ctx,
		`SELECT token_hash, runner_id, created_at, revoked_at FROM runner_tokens WHERE token_hash = $1`, hash).
		Scan(&rt.TokenHash, &rt.RunnerID, &rt.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid {
		t := revoked.Time
		rt.RevokedAt = &t
	}
	return &rt, nil
}

func revokeRunnerTokens(ctx context.Context, ex refreshTokenExecutor, runnerID string, at time.Time) (int, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE runner_tokens SET revoked_at = $2 WHERE runner_id = $1 AND revoked_at IS NULL`, runnerID, at)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
