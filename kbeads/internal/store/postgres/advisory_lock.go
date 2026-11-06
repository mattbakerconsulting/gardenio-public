package postgres

import (
	"context"
	"fmt"
)

// TryAdvisoryLock implements store.Store's cross-replica sweep serialization
// primitive (Phase 2 2-replica spike, gd-XxrObXvc8r) using a Postgres
// transaction-scoped advisory lock (pg_try_advisory_xact_lock): the lock is
// held only for the lifetime of one transaction and releases automatically
// on commit or rollback, so a crash mid-fn can never leak it past this
// call — unlike a session-scoped advisory lock, which would stay held on
// whatever pooled connection served it until that connection is explicitly
// unlocked or closed.
func (s *PostgresStore) TryAdvisoryLock(ctx context.Context, key int64, fn func(ctx context.Context) error) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; releases the xact lock on any early return

	var acquired bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	if !acquired {
		return false, nil
	}

	if err := fn(ctx); err != nil {
		return true, err
	}
	if err := tx.Commit(); err != nil {
		return true, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// TryAdvisoryLock on a txStore acquires the lock within the ALREADY-OPEN
// transaction it wraps, rather than opening a nested one (Postgres has no
// nested transactions). The lock still releases automatically at that outer
// transaction's eventual commit/rollback — just later than the PostgresStore
// case above, whose transaction exists only for the lock's duration.
func (s *txStore) TryAdvisoryLock(ctx context.Context, key int64, fn func(ctx context.Context) error) (bool, error) {
	var acquired bool
	if err := s.tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	if !acquired {
		return false, nil
	}
	if err := fn(ctx); err != nil {
		return true, err
	}
	return true, nil
}
