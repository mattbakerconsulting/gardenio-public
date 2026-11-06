package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// uiTelemetryHardLimit caps the rows ListUITelemetry will return regardless
// of caller-supplied Limit. v1 is small (one row per click on the
// incident-intel surfaces) so a generous cap still fits comfortably in a
// single page.
const uiTelemetryHardLimit = 1000

func uiTelemetryEffectiveLimit(req int) int {
	if req <= 0 || req > uiTelemetryHardLimit {
		return uiTelemetryHardLimit
	}
	return req
}

func (s *PostgresStore) RecordUITelemetry(ctx context.Context, row *model.UITelemetryRow) error {
	return queryRecordUITelemetry(ctx, s.db, row)
}

func (s *PostgresStore) ListUITelemetry(ctx context.Context, filter model.UITelemetryFilter) ([]*model.UITelemetryRow, error) {
	return queryListUITelemetry(ctx, s.db, filter)
}

func (s *txStore) RecordUITelemetry(ctx context.Context, row *model.UITelemetryRow) error {
	return queryRecordUITelemetry(ctx, s.tx, row)
}

func (s *txStore) ListUITelemetry(ctx context.Context, filter model.UITelemetryFilter) ([]*model.UITelemetryRow, error) {
	return queryListUITelemetry(ctx, s.tx, filter)
}

func queryRecordUITelemetry(ctx context.Context, db executor, row *model.UITelemetryRow) error {
	var operatorID sql.NullString
	if row.OperatorID != "" {
		operatorID = sql.NullString{String: row.OperatorID, Valid: true}
	}
	metadata := []byte(row.Metadata)
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	return db.QueryRowContext(ctx, `
		INSERT INTO ui_telemetry (event, target, from_path, path, operator_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		row.Event, row.Target, row.From, row.Path, operatorID, metadata,
	).Scan(&row.ID, &row.CreatedAt)
}

func queryListUITelemetry(ctx context.Context, db executor, filter model.UITelemetryFilter) ([]*model.UITelemetryRow, error) {
	limit := uiTelemetryEffectiveLimit(filter.Limit)
	args := []any{limit}
	q := `SELECT id, event, target, from_path, path, operator_id, metadata, created_at
	      FROM ui_telemetry`
	where := ""
	if filter.Event != "" {
		args = append(args, filter.Event)
		where = appendWhere(where, "event = $"+strconv.Itoa(len(args)))
	}
	if !filter.Since.IsZero() {
		args = append(args, filter.Since)
		where = appendWhere(where, "created_at >= $"+strconv.Itoa(len(args)))
	}
	q += where + " ORDER BY id DESC LIMIT $1"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*model.UITelemetryRow, 0, limit)
	for rows.Next() {
		r := &model.UITelemetryRow{}
		var operatorID sql.NullString
		var metadata []byte
		if err := rows.Scan(&r.ID, &r.Event, &r.Target, &r.From, &r.Path, &operatorID, &metadata, &r.CreatedAt); err != nil {
			return nil, err
		}
		if operatorID.Valid {
			r.OperatorID = operatorID.String
		}
		if len(metadata) > 0 {
			r.Metadata = json.RawMessage(metadata)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func appendWhere(prev, clause string) string {
	if prev == "" {
		return " WHERE " + clause
	}
	return prev + " AND " + clause
}

// statically assert *PostgresStore satisfies the relevant Store slice.
var _ store.Store = (*PostgresStore)(nil)
