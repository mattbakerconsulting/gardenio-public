package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// slowToolCallsHardLimit caps ListSlowToolCalls regardless of caller-supplied
// Limit, mirroring the ui_telemetry hard-limit pattern.
const slowToolCallsHardLimit = 500

func slowToolCallsEffectiveLimit(req int) int {
	if req <= 0 || req > slowToolCallsHardLimit {
		return slowToolCallsHardLimit
	}
	return req
}

func (s *PostgresStore) InsertSlowToolCall(ctx context.Context, row *model.SlowToolCallRow) error {
	return queryInsertSlowToolCall(ctx, s.db, row)
}

// slowToolCallsDeleteDefaultBatch bounds one DeleteSlowToolCallsBefore call so
// a first sweep over a large backlog never holds a single long-running
// DELETE (rows carry full redacted argv, so they are not small).
const slowToolCallsDeleteDefaultBatch = 1000

func (s *PostgresStore) DeleteSlowToolCallsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return queryDeleteSlowToolCallsBefore(ctx, s.db, cutoff, limit)
}

func (s *txStore) DeleteSlowToolCallsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return queryDeleteSlowToolCallsBefore(ctx, s.tx, cutoff, limit)
}

func queryDeleteSlowToolCallsBefore(ctx context.Context, db executor, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = slowToolCallsDeleteDefaultBatch
	}
	res, err := db.ExecContext(ctx, `
		DELETE FROM slow_tool_calls
		WHERE id IN (
			SELECT id FROM slow_tool_calls
			WHERE ts < $1
			ORDER BY ts
			LIMIT $2
		)`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *PostgresStore) ListSlowToolCalls(ctx context.Context, filter model.SlowToolCallFilter) ([]*model.SlowToolCallRow, error) {
	return queryListSlowToolCalls(ctx, s.db, filter)
}

func (s *txStore) InsertSlowToolCall(ctx context.Context, row *model.SlowToolCallRow) error {
	return queryInsertSlowToolCall(ctx, s.tx, row)
}

func (s *txStore) ListSlowToolCalls(ctx context.Context, filter model.SlowToolCallFilter) ([]*model.SlowToolCallRow, error) {
	return queryListSlowToolCalls(ctx, s.tx, filter)
}

func queryInsertSlowToolCall(ctx context.Context, db executor, row *model.SlowToolCallRow) error {
	var exitStatus sql.NullInt64
	if row.ExitStatus != nil {
		exitStatus = sql.NullInt64{Int64: int64(*row.ExitStatus), Valid: true}
	}
	toolInput := []byte(row.ToolInputRedacted)
	var toolInputArg any
	if len(toolInput) > 0 {
		toolInputArg = toolInput
	}
	return db.QueryRowContext(ctx, `
		INSERT INTO slow_tool_calls (
			ts, agent_bead_id, session_id, project, role,
			tool_name, tool_use_id, duration_ms, source, cmd_class,
			command_redacted, tool_input_redacted, exit_status, output_bytes, is_sidechain
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
		)
		RETURNING id, created_at`,
		row.TS, row.AgentBeadID, row.SessionID, row.Project, row.Role,
		row.ToolName, row.ToolUseID, row.DurationMs, row.Source, row.CmdClass,
		row.CommandRedacted, toolInputArg, exitStatus, row.OutputBytes, row.IsSidechain,
	).Scan(&row.ID, &row.CreatedAt)
}

func queryListSlowToolCalls(ctx context.Context, db executor, filter model.SlowToolCallFilter) ([]*model.SlowToolCallRow, error) {
	limit := slowToolCallsEffectiveLimit(filter.Limit)
	args := []any{limit}
	q := `SELECT id, ts, agent_bead_id, session_id, project, role,
	             tool_name, tool_use_id, duration_ms, source, cmd_class,
	             command_redacted, tool_input_redacted, exit_status, output_bytes, is_sidechain, created_at
	      FROM slow_tool_calls`
	where := ""
	if !filter.Since.IsZero() {
		args = append(args, filter.Since)
		where = appendWhere(where, "ts >= $"+strconv.Itoa(len(args)))
	}
	if !filter.Until.IsZero() {
		args = append(args, filter.Until)
		where = appendWhere(where, "ts <= $"+strconv.Itoa(len(args)))
	}
	if filter.CmdClass != "" {
		args = append(args, filter.CmdClass)
		where = appendWhere(where, "cmd_class = $"+strconv.Itoa(len(args)))
	}
	if filter.AgentBeadID != "" {
		args = append(args, filter.AgentBeadID)
		where = appendWhere(where, "agent_bead_id = $"+strconv.Itoa(len(args)))
	}
	q += where + " ORDER BY duration_ms DESC LIMIT $1"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*model.SlowToolCallRow, 0, limit)
	for rows.Next() {
		r := &model.SlowToolCallRow{}
		var exitStatus sql.NullInt64
		var toolInput []byte
		if err := rows.Scan(&r.ID, &r.TS, &r.AgentBeadID, &r.SessionID, &r.Project, &r.Role,
			&r.ToolName, &r.ToolUseID, &r.DurationMs, &r.Source, &r.CmdClass,
			&r.CommandRedacted, &toolInput, &exitStatus, &r.OutputBytes, &r.IsSidechain, &r.CreatedAt); err != nil {
			return nil, err
		}
		if exitStatus.Valid {
			v := int(exitStatus.Int64)
			r.ExitStatus = &v
		}
		if len(toolInput) > 0 {
			r.ToolInputRedacted = json.RawMessage(toolInput)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// statically assert *PostgresStore satisfies the relevant Store slice.
var _ store.Store = (*PostgresStore)(nil)
