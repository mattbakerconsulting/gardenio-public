package postgres

import (
	"context"
	"database/sql"
	"errors"

	"gardenio/kbeads/internal/model"
)

// UpsertGate ensures a gate row exists in pending state.
// Uses INSERT...ON CONFLICT DO NOTHING so existing state is never reset.
func (s *PostgresStore) UpsertGate(ctx context.Context, agentBeadID, gateID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_gates (agent_bead_id, gate_id)
		VALUES ($1, $2)
		ON CONFLICT (agent_bead_id, gate_id) DO NOTHING`,
		agentBeadID, gateID,
	)
	return err
}

// MarkGateSatisfied sets a gate's status to 'satisfied' and records the time.
func (s *PostgresStore) MarkGateSatisfied(ctx context.Context, agentBeadID, gateID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE session_gates
		SET status = 'satisfied', satisfied_at = NOW()
		WHERE agent_bead_id = $1 AND gate_id = $2`,
		agentBeadID, gateID,
	)
	return err
}

// ClearGate resets a gate back to 'pending', clearing the satisfied timestamp.
func (s *PostgresStore) ClearGate(ctx context.Context, agentBeadID, gateID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE session_gates
		SET status = 'pending', satisfied_at = NULL
		WHERE agent_bead_id = $1 AND gate_id = $2`,
		agentBeadID, gateID,
	)
	return err
}

// IsGateSatisfied returns true if the gate row exists and has status
// 'satisfied'. Returns false (not an error) when the row is not found.
func (s *PostgresStore) IsGateSatisfied(ctx context.Context, agentBeadID, gateID string) (bool, error) {
	var gateStatus string
	err := s.db.QueryRowContext(ctx, `
		SELECT status FROM session_gates
		WHERE agent_bead_id = $1 AND gate_id = $2`,
		agentBeadID, gateID,
	).Scan(&gateStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return gateStatus == "satisfied", nil
}

// ListGates returns all gate rows for an agent bead, ordered by gate_id.
func (s *PostgresStore) ListGates(ctx context.Context, agentBeadID string) ([]model.GateRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent_bead_id, gate_id, status, satisfied_at
		FROM session_gates
		WHERE agent_bead_id = $1
		ORDER BY gate_id`,
		agentBeadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var gates []model.GateRow
	for rows.Next() {
		var g model.GateRow
		var satisfiedAt sql.NullTime
		if err := rows.Scan(
			&g.AgentBeadID,
			&g.GateID,
			&g.Status,
			&satisfiedAt,
		); err != nil {
			return nil, err
		}
		if satisfiedAt.Valid {
			g.SatisfiedAt = &satisfiedAt.Time
		}
		gates = append(gates, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return gates, nil
}

// The txStore gate methods delegate to the same query functions via the transaction executor.

func (s *txStore) UpsertGate(ctx context.Context, agentBeadID, gateID string) error {
	_, err := s.tx.ExecContext(ctx, `
		INSERT INTO session_gates (agent_bead_id, gate_id)
		VALUES ($1, $2)
		ON CONFLICT (agent_bead_id, gate_id) DO NOTHING`,
		agentBeadID, gateID,
	)
	return err
}

func (s *txStore) MarkGateSatisfied(ctx context.Context, agentBeadID, gateID string) error {
	_, err := s.tx.ExecContext(ctx, `
		UPDATE session_gates
		SET status = 'satisfied', satisfied_at = NOW()
		WHERE agent_bead_id = $1 AND gate_id = $2`,
		agentBeadID, gateID,
	)
	return err
}

func (s *txStore) ClearGate(ctx context.Context, agentBeadID, gateID string) error {
	_, err := s.tx.ExecContext(ctx, `
		UPDATE session_gates
		SET status = 'pending', satisfied_at = NULL
		WHERE agent_bead_id = $1 AND gate_id = $2`,
		agentBeadID, gateID,
	)
	return err
}

func (s *txStore) IsGateSatisfied(ctx context.Context, agentBeadID, gateID string) (bool, error) {
	var gateStatus string
	err := s.tx.QueryRowContext(ctx, `
		SELECT status FROM session_gates
		WHERE agent_bead_id = $1 AND gate_id = $2`,
		agentBeadID, gateID,
	).Scan(&gateStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return gateStatus == "satisfied", nil
}

func (s *txStore) ListGates(ctx context.Context, agentBeadID string) ([]model.GateRow, error) {
	rows, err := s.tx.QueryContext(ctx, `
		SELECT agent_bead_id, gate_id, status, satisfied_at
		FROM session_gates
		WHERE agent_bead_id = $1
		ORDER BY gate_id`,
		agentBeadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var gates []model.GateRow
	for rows.Next() {
		var g model.GateRow
		var satisfiedAt sql.NullTime
		if err := rows.Scan(
			&g.AgentBeadID,
			&g.GateID,
			&g.Status,
			&satisfiedAt,
		); err != nil {
			return nil, err
		}
		if satisfiedAt.Valid {
			g.SatisfiedAt = &satisfiedAt.Time
		}
		gates = append(gates, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return gates, nil
}
