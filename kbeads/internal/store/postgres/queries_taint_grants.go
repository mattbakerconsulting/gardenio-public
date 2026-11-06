package postgres

import (
	"context"
	"fmt"

	"github.com/lib/pq"

	"gardenio/kbeads/internal/model"
)

// queryGrantTaint creates or refreshes a taint_grants row, per ADR
// docs/adr/taints.md §3/§5.1/§7/§10.4. This is the only write path into
// taint_grants, so validation lives here: principal_kind must be one of the
// four kinds model.IsValidPrincipalKind recognizes, and principal is
// required. Re-granting an existing (taint_id, principal_kind, principal)
// refreshes granted_by/granted_at/expires_at to the new call's values.
func queryGrantTaint(ctx context.Context, db executor, g *model.TaintGrant) error {
	var ve model.ValidationError

	if !model.IsValidPrincipalKind(g.PrincipalKind) {
		ve.Errors = append(ve.Errors, model.FieldError{
			Field:   "principal_kind",
			Message: fmt.Sprintf("unknown kind %q", g.PrincipalKind),
		})
	}
	if g.Principal == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "principal", Message: "is required"})
	}
	if g.GrantedBy == 0 {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "granted_by", Message: "is required"})
	}
	if ve.HasErrors() {
		return &ve
	}

	return db.QueryRowContext(ctx, `
		INSERT INTO taint_grants (taint_id, principal_kind, principal, granted_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (taint_id, principal_kind, principal) DO UPDATE
			SET granted_by = $4, granted_at = NOW(), expires_at = $5
		RETURNING granted_at`,
		g.TaintID, g.PrincipalKind, g.Principal, g.GrantedBy, g.ExpiresAt,
	).Scan(&g.GrantedAt)
}

// queryRevokeTaint deletes one taint_grants row. removed is false only when
// no row matched — a repeat revoke is not an error.
func queryRevokeTaint(ctx context.Context, db executor, taintID int64, principalKind, principal string) (bool, error) {
	res, err := db.ExecContext(ctx, `
		DELETE FROM taint_grants WHERE taint_id = $1 AND principal_kind = $2 AND principal = $3`,
		taintID, principalKind, principal,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// queryGetTaintGrant returns the CURRENTLY VALID grant for the given
// taint/principal, or sql.ErrNoRows if none exists or the only matching row
// has expired.
func queryGetTaintGrant(ctx context.Context, db executor, taintID int64, principalKind, principal string) (*model.TaintGrant, error) {
	row := db.QueryRowContext(ctx, `
		SELECT taint_id, principal_kind, principal, granted_by, granted_at, expires_at
		FROM taint_grants
		WHERE taint_id = $1 AND principal_kind = $2 AND principal = $3
			AND (expires_at IS NULL OR expires_at > NOW())`,
		taintID, principalKind, principal,
	)
	return scanTaintGrant(row)
}

// queryApplyTaint writes a bead_taints row, per ADR docs/adr/taints.md
// §3/§4.2/§7. Idempotent: applying a taint already on the bead is a no-op
// that preserves the original source/applied_by/applied_at. inserted is
// false when the taint was already applied.
func queryApplyTaint(ctx context.Context, db executor, bt *model.BeadTaint) (bool, error) {
	var ve model.ValidationError
	if bt.BeadID == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "bead_id", Message: "is required"})
	}
	if bt.Source == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "source", Message: "is required"})
	}
	if bt.AppliedBy == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "applied_by", Message: "is required"})
	}
	if ve.HasErrors() {
		return false, &ve
	}

	var sourceBeadID *string
	if bt.SourceBeadID != "" {
		sourceBeadID = &bt.SourceBeadID
	}

	res, err := db.ExecContext(ctx, `
		INSERT INTO bead_taints (bead_id, taint_id, source, source_bead_id, applied_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (bead_id, taint_id) DO NOTHING`,
		bt.BeadID, bt.TaintID, bt.Source, sourceBeadID, bt.AppliedBy,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// queryRecordTaintAudit writes one taint_audit row, per ADR
// docs/adr/taints.md §3: a hash-only record of a taint mutation. Never
// holds content. a.ID and a.CreatedAt are populated on success.
func queryRecordTaintAudit(ctx context.Context, db executor, a *model.TaintAudit) error {
	var beadID, requestID, approvalMode, contentSHA *string
	if a.BeadID != "" {
		beadID = &a.BeadID
	}
	if a.RequestID != "" {
		requestID = &a.RequestID
	}
	if a.ApprovalMode != "" {
		approvalMode = &a.ApprovalMode
	}
	if a.ContentSHA != "" {
		contentSHA = &a.ContentSHA
	}
	return db.QueryRowContext(ctx, `
		INSERT INTO taint_audit (taint_id, action, bead_id, request_id, actor, approval_mode, content_sha)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`,
		a.TaintID, a.Action, beadID, requestID, a.Actor, approvalMode, contentSHA,
	).Scan(&a.ID, &a.CreatedAt)
}

// queryListTaintGrants returns every taint_grants row for taintID, including
// expired ones, newest-granted first — the viewer's Grants tab
// (docs/adr/taints.md §9.1) renders expiry itself rather than hiding an
// expired row.
func queryListTaintGrants(ctx context.Context, db executor, taintID int64) ([]*model.TaintGrant, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT taint_id, principal_kind, principal, granted_by, granted_at, expires_at
		FROM taint_grants
		WHERE taint_id = $1
		ORDER BY granted_at DESC`,
		taintID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.TaintGrant
	for rows.Next() {
		g, err := scanTaintGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// queryGetBeadTaintsForTaint is the batch form of queryGetBeadTaints,
// filtered to one taint and restricted to beadIDs, keyed by bead id — the
// viewer's Beads tab (docs/adr/taints.md §9.1) uses this to annotate an
// already-read-gated page of beads with their propagation source for this
// taint without an N+1 query per row (kbeads CLAUDE.md's ANY($1::text[])
// batch-query pattern). A bead absent from the result map does not carry
// this taint (should not happen for a page already filtered by TaintID, but
// is not itself an error here). An empty beadIDs short-circuits to no rows.
func queryGetBeadTaintsForTaint(ctx context.Context, db executor, taintID int64, beadIDs []string) (map[string]*model.BeadTaint, error) {
	if len(beadIDs) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT bt.bead_id, bt.taint_id, t.name, bt.source, bt.source_bead_id, bt.applied_by, bt.applied_at
		FROM bead_taints bt
		JOIN taints t ON t.id = bt.taint_id
		WHERE bt.taint_id = $1 AND bt.bead_id = ANY($2::text[])`,
		taintID, pq.Array(beadIDs),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]*model.BeadTaint, len(beadIDs))
	for rows.Next() {
		bt, err := scanBeadTaint(rows)
		if err != nil {
			return nil, err
		}
		out[bt.BeadID] = bt
	}
	return out, rows.Err()
}

// queryGetGrantedTaintIDs returns the distinct taint ids for which a
// currently-valid taint_grants row exists with the given principal_kind and
// principal IN principals, per ADR docs/adr/taints.md §5.1. An empty
// principals slice short-circuits to no rows without a query.
func queryGetGrantedTaintIDs(ctx context.Context, db executor, principalKind string, principals []string) ([]int64, error) {
	if len(principals) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT taint_id FROM taint_grants
		WHERE principal_kind = $1 AND principal = ANY($2::text[])
			AND (expires_at IS NULL OR expires_at > NOW())`,
		principalKind, pq.Array(principals),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaintIDs(rows)
}

// queryGetTaintIDsOwnedByTeams returns the ids of every taint whose
// owner_team is in teams, per ADR docs/adr/taints.md §5.1/§10.1's implicit
// owner-team grant. An empty teams slice short-circuits to no rows without a
// query.
func queryGetTaintIDsOwnedByTeams(ctx context.Context, db executor, teams []string) ([]int64, error) {
	if len(teams) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM taints WHERE owner_team = ANY($1::text[])`,
		pq.Array(teams),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaintIDs(rows)
}

// queryHasAnyTaints reports whether the taints registry holds at least one
// row, per ADR docs/adr/taints.md §10.1's zero-configuration short-circuit.
func queryHasAnyTaints(ctx context.Context, db executor) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM taints)`).Scan(&exists)
	return exists, err
}

// scanTaintIDs drains rows of a single bigint column into a slice.
func scanTaintIDs(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]int64, error) {
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// queryGetBeadTaints returns every taint applied to beadID, joined with each
// taint's name, ordered by taint name. Unfiltered by effects.
func queryGetBeadTaints(ctx context.Context, db executor, beadID string) ([]*model.BeadTaint, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT bt.bead_id, bt.taint_id, t.name, bt.source, bt.source_bead_id, bt.applied_by, bt.applied_at
		FROM bead_taints bt
		JOIN taints t ON t.id = bt.taint_id
		WHERE bt.bead_id = $1
		ORDER BY t.name`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBeadTaints(rows)
}

// queryGetGateBeadTaints is queryGetBeadTaints filtered to taints whose
// effects include "gate" (docs/adr/taints.md §15.2) — the write gate
// consults this instead, so a taint whose effects exclude "gate" never
// blocks a write.
func queryGetGateBeadTaints(ctx context.Context, db executor, beadID string) ([]*model.BeadTaint, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT bt.bead_id, bt.taint_id, t.name, bt.source, bt.source_bead_id, bt.applied_by, bt.applied_at
		FROM bead_taints bt
		JOIN taints t ON t.id = bt.taint_id
		WHERE bt.bead_id = $1 AND 'gate' = ANY(t.effects)
		ORDER BY t.name`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBeadTaints(rows)
}
