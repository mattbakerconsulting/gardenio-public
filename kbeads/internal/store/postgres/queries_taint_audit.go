package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

// queryListTaintAudit returns taint_audit rows for taintID matching filter,
// newest first, per docs/adr/taints.md §9.1's Audit tab: "filterable by
// action and actor". total is the unfiltered-by-page count matching
// action/actor, for pagination.
func queryListTaintAudit(ctx context.Context, db executor, taintID int64, filter model.TaintAuditFilter) ([]*model.TaintAudit, int, error) {
	var conditions []string
	args := []any{taintID}
	conditions = append(conditions, "taint_id = $1")

	if filter.Action != "" {
		args = append(args, filter.Action)
		conditions = append(conditions, fmt.Sprintf("action = $%d", len(args)))
	}
	if filter.Actor != "" {
		args = append(args, filter.Actor)
		conditions = append(conditions, fmt.Sprintf("actor = $%d", len(args)))
	}
	if filter.CreatedAfter != nil {
		args = append(args, *filter.CreatedAfter)
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	where := strings.Join(conditions, " AND ")

	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM taint_audit WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT id, taint_id, action, bead_id, request_id, actor, approval_mode, content_sha, created_at " +
		"FROM taint_audit WHERE " + where + " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if filter.Offset > 0 {
		args = append(args, filter.Offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*model.TaintAudit
	for rows.Next() {
		a, err := scanTaintAudit(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// scanTaintAudit scans one row of taint_audit.
func scanTaintAudit(row scannable) (*model.TaintAudit, error) {
	var a model.TaintAudit
	var beadID, requestID, approvalMode, contentSHA sql.NullString
	err := row.Scan(&a.ID, &a.TaintID, &a.Action, &beadID, &requestID, &a.Actor, &approvalMode, &contentSHA, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	a.BeadID = beadID.String
	a.RequestID = requestID.String
	a.ApprovalMode = approvalMode.String
	a.ContentSHA = contentSHA.String
	return &a, nil
}
