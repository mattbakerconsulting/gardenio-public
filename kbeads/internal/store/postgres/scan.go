package postgres

import (
	"database/sql"
	"encoding/json"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/lib/pq"
)

// scannable is the interface satisfied by both *sql.Row and *sql.Rows.
type scannable interface {
	Scan(dest ...any) error
}

// labelsScannable appends a trailing labels destination to every Scan, so
// the bead scanners stay unchanged while a beadLabelsSelectSuffix projection
// carries one extra column.
type labelsScannable struct {
	row    scannable
	labels *pq.StringArray
}

func (r labelsScannable) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.labels)...)
}

// scanWithLabels scans a row whose projection ends in beadLabelsSelectSuffix:
// scan reads the bead columns, and the trailing text[] becomes b.Labels (nil
// when the bead has no labels, matching queryGetLabels).
func scanWithLabels(row scannable, scan func(scannable) (*model.Bead, error)) (*model.Bead, error) {
	var labels pq.StringArray
	b, err := scan(labelsScannable{row: row, labels: &labels})
	if err != nil {
		return nil, err
	}
	b.Labels = []string(labels)
	return b, nil
}

// scanBead scans a single row into a model.Bead.
// The row must contain columns in the order defined by beadColumns.
func scanBead(row scannable) (*model.Bead, error) {
	var b model.Bead
	var (
		slug         sql.NullString
		shortTitle   sql.NullString
		heraldry     sql.NullString
		description  sql.NullString
		notes        sql.NullString
		assignee     sql.NullString
		owner        sql.NullString
		createdBy    sql.NullString
		closedAt     sql.NullTime
		closedBy     sql.NullString
		dueAt        sql.NullTime
		deferUntil   sql.NullTime
		deletedAt    sql.NullTime
		fields       []byte
		category     sql.NullString
		operatedByID sql.NullInt64
	)

	err := row.Scan(
		&b.ID,
		&slug,
		&shortTitle,
		&heraldry,
		&b.Kind,
		&b.Type,
		&b.Title,
		&description,
		&notes,
		&b.Status,
		&b.Priority,
		&assignee,
		&owner,
		&b.CreatedAt,
		&createdBy,
		&b.UpdatedAt,
		&closedAt,
		&closedBy,
		&dueAt,
		&deferUntil,
		&fields,
		&b.Fleet,
		&deletedAt,
		&category,
		&operatedByID,
	)
	if err != nil {
		return nil, err
	}

	b.Slug = slug.String
	b.ShortTitle = shortTitle.String
	b.Heraldry = heraldry.String
	b.Description = description.String
	b.Notes = notes.String
	b.Assignee = assignee.String
	b.Owner = owner.String
	b.CreatedBy = createdBy.String
	b.ClosedBy = closedBy.String

	if closedAt.Valid {
		t := closedAt.Time
		b.ClosedAt = &t
	}
	if dueAt.Valid {
		t := dueAt.Time
		b.DueAt = &t
	}
	if deferUntil.Valid {
		t := deferUntil.Time
		b.DeferUntil = &t
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		b.DeletedAt = &t
	}
	if len(fields) > 0 {
		b.Fields = json.RawMessage(fields)
	}
	b.Category = category.String
	if operatedByID.Valid {
		v := operatedByID.Int64
		b.OperatedByID = &v
	}

	return &b, nil
}

// scanBeadWithTotal scans a row that has a leading total_count column
// followed by the standard bead columns. Used by queryListBeads with
// COUNT(*) OVER().
func scanBeadWithTotal(row scannable) (*model.Bead, int, error) {
	var total int
	var b model.Bead
	var (
		slug         sql.NullString
		shortTitle   sql.NullString
		heraldry     sql.NullString
		description  sql.NullString
		notes        sql.NullString
		assignee     sql.NullString
		owner        sql.NullString
		createdBy    sql.NullString
		closedAt     sql.NullTime
		closedBy     sql.NullString
		dueAt        sql.NullTime
		deferUntil   sql.NullTime
		deletedAt    sql.NullTime
		fields       []byte
		category     sql.NullString
		operatedByID sql.NullInt64
	)

	err := row.Scan(
		&total,
		&b.ID,
		&slug,
		&shortTitle,
		&heraldry,
		&b.Kind,
		&b.Type,
		&b.Title,
		&description,
		&notes,
		&b.Status,
		&b.Priority,
		&assignee,
		&owner,
		&b.CreatedAt,
		&createdBy,
		&b.UpdatedAt,
		&closedAt,
		&closedBy,
		&dueAt,
		&deferUntil,
		&fields,
		&b.Fleet,
		&deletedAt,
		&category,
		&operatedByID,
	)
	if err != nil {
		return nil, 0, err
	}

	b.Slug = slug.String
	b.ShortTitle = shortTitle.String
	b.Heraldry = heraldry.String
	b.Description = description.String
	b.Notes = notes.String
	b.Assignee = assignee.String
	b.Owner = owner.String
	b.CreatedBy = createdBy.String
	b.ClosedBy = closedBy.String

	if closedAt.Valid {
		t := closedAt.Time
		b.ClosedAt = &t
	}
	if dueAt.Valid {
		t := dueAt.Time
		b.DueAt = &t
	}
	if deferUntil.Valid {
		t := deferUntil.Time
		b.DeferUntil = &t
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		b.DeletedAt = &t
	}
	if len(fields) > 0 {
		b.Fields = json.RawMessage(fields)
	}
	b.Category = category.String
	if operatedByID.Valid {
		v := operatedByID.Int64
		b.OperatedByID = &v
	}

	return &b, total, nil
}

// scanBeadFull scans a row produced by SELECT beadColumnsBeads +
// beadOperatorSelectSuffix (i.e. the standard bead columns — including the
// operated_by_id FK — followed by the LEFT-JOINed operator display + email
// columns from the operators table). Used by read paths that surface the
// resolved operator chip on Bead (queryGetBead, batched fetches, etc.). The
// base bead columns are scanned in the same order as scanBead; the trailing
// three columns are the COALESCE-pinned alias/display/email projections, so
// they are plain string destinations, not sql.NullString.
func scanBeadFull(row scannable) (*model.Bead, error) {
	var b model.Bead
	var (
		slug         sql.NullString
		shortTitle   sql.NullString
		heraldry     sql.NullString
		description  sql.NullString
		notes        sql.NullString
		assignee     sql.NullString
		owner        sql.NullString
		createdBy    sql.NullString
		closedAt     sql.NullTime
		closedBy     sql.NullString
		dueAt        sql.NullTime
		deferUntil   sql.NullTime
		deletedAt    sql.NullTime
		fields       []byte
		category     sql.NullString
		operatedByID sql.NullInt64
	)

	err := row.Scan(
		&b.ID,
		&slug,
		&shortTitle,
		&heraldry,
		&b.Kind,
		&b.Type,
		&b.Title,
		&description,
		&notes,
		&b.Status,
		&b.Priority,
		&assignee,
		&owner,
		&b.CreatedAt,
		&createdBy,
		&b.UpdatedAt,
		&closedAt,
		&closedBy,
		&dueAt,
		&deferUntil,
		&fields,
		&b.Fleet,
		&deletedAt,
		&category,
		&operatedByID,
		&b.OperatedBy,
		&b.OperatedByDisplay,
		&b.OperatedByEmail,
	)
	if err != nil {
		return nil, err
	}

	b.Slug = slug.String
	b.ShortTitle = shortTitle.String
	b.Heraldry = heraldry.String
	b.Description = description.String
	b.Notes = notes.String
	b.Assignee = assignee.String
	b.Owner = owner.String
	b.CreatedBy = createdBy.String
	b.ClosedBy = closedBy.String

	if closedAt.Valid {
		t := closedAt.Time
		b.ClosedAt = &t
	}
	if dueAt.Valid {
		t := dueAt.Time
		b.DueAt = &t
	}
	if deferUntil.Valid {
		t := deferUntil.Time
		b.DeferUntil = &t
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		b.DeletedAt = &t
	}
	if len(fields) > 0 {
		b.Fields = json.RawMessage(fields)
	}
	b.Category = category.String
	if operatedByID.Valid {
		v := operatedByID.Int64
		b.OperatedByID = &v
	}

	return &b, nil
}

// scanBeadWithTotalFull is scanBeadFull plus a leading total_count column
// (from COUNT(*) OVER()) and trailing has_report, blocked_on_decision_bead_id,
// and parked_on_decision_bead_id columns (from beadHasReportSelect,
// beadBlockedOnDecisionSelectSuffix, and beadParkedOnDecisionSelectSuffix).
// Mirrors scanBeadWithTotal for the JOINed list-path variant. The has_report
// bool is COALESCE-pinned at the SQL layer so it reads into a plain bool
// destination. blocked_on_decision_bead_id is empty when no open decision
// flags this bead's subtree (ka-0000001065 / R2 in
// gardenio/docs/research/sequencer-report-decision-visibility.md);
// parked_on_decision_bead_id is empty when no open decision names this bead in
// its prose (ka-0000003797).
func scanBeadWithTotalFull(row scannable) (*model.Bead, int, error) {
	var total int
	var b model.Bead
	var (
		slug                    sql.NullString
		shortTitle              sql.NullString
		heraldry                sql.NullString
		description             sql.NullString
		notes                   sql.NullString
		assignee                sql.NullString
		owner                   sql.NullString
		createdBy               sql.NullString
		closedAt                sql.NullTime
		closedBy                sql.NullString
		dueAt                   sql.NullTime
		deferUntil              sql.NullTime
		deletedAt               sql.NullTime
		fields                  []byte
		category                sql.NullString
		operatedByID            sql.NullInt64
		blockedOnDecisionBeadID sql.NullString
		parkedOnDecisionBeadID  sql.NullString
	)

	err := row.Scan(
		&total,
		&b.ID,
		&slug,
		&shortTitle,
		&heraldry,
		&b.Kind,
		&b.Type,
		&b.Title,
		&description,
		&notes,
		&b.Status,
		&b.Priority,
		&assignee,
		&owner,
		&b.CreatedAt,
		&createdBy,
		&b.UpdatedAt,
		&closedAt,
		&closedBy,
		&dueAt,
		&deferUntil,
		&fields,
		&b.Fleet,
		&deletedAt,
		&category,
		&operatedByID,
		&b.OperatedBy,
		&b.OperatedByDisplay,
		&b.OperatedByEmail,
		&b.HasReport,
		&blockedOnDecisionBeadID,
		&parkedOnDecisionBeadID,
		// relevance_score: trailing float8 projection (ka-0000003832). Always
		// present in the list-path SELECT — the composite text-search score when
		// a search is active, else a constant 0.
		&b.RelevanceScore,
	)
	if err != nil {
		return nil, 0, err
	}

	b.Slug = slug.String
	b.ShortTitle = shortTitle.String
	b.Heraldry = heraldry.String
	b.Description = description.String
	b.Notes = notes.String
	b.Assignee = assignee.String
	b.Owner = owner.String
	b.CreatedBy = createdBy.String
	b.ClosedBy = closedBy.String

	if closedAt.Valid {
		t := closedAt.Time
		b.ClosedAt = &t
	}
	if dueAt.Valid {
		t := dueAt.Time
		b.DueAt = &t
	}
	if deferUntil.Valid {
		t := deferUntil.Time
		b.DeferUntil = &t
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		b.DeletedAt = &t
	}
	if len(fields) > 0 {
		b.Fields = json.RawMessage(fields)
	}
	b.Category = category.String
	if operatedByID.Valid {
		v := operatedByID.Int64
		b.OperatedByID = &v
	}
	b.BlockedOnDecisionBeadID = blockedOnDecisionBeadID.String
	b.BlockedOnDecision = b.BlockedOnDecisionBeadID != ""
	b.ParkedOnDecisionBeadID = parkedOnDecisionBeadID.String
	b.ParkedOnDecision = b.ParkedOnDecisionBeadID != ""

	return &b, total, nil
}

// scanBeadWithScoreFull is scanBeadFull plus a leading float64 similarity
// score column (from semantic search). Mirrors scanBeadWithScore for the
// JOINed variant.
func scanBeadWithScoreFull(row scannable) (*model.Bead, float64, error) {
	var score float64
	var b model.Bead
	var (
		slug         sql.NullString
		shortTitle   sql.NullString
		heraldry     sql.NullString
		description  sql.NullString
		notes        sql.NullString
		assignee     sql.NullString
		owner        sql.NullString
		createdBy    sql.NullString
		closedAt     sql.NullTime
		closedBy     sql.NullString
		dueAt        sql.NullTime
		deferUntil   sql.NullTime
		deletedAt    sql.NullTime
		fields       []byte
		category     sql.NullString
		operatedByID sql.NullInt64
	)

	err := row.Scan(
		&score,
		&b.ID,
		&slug,
		&shortTitle,
		&heraldry,
		&b.Kind,
		&b.Type,
		&b.Title,
		&description,
		&notes,
		&b.Status,
		&b.Priority,
		&assignee,
		&owner,
		&b.CreatedAt,
		&createdBy,
		&b.UpdatedAt,
		&closedAt,
		&closedBy,
		&dueAt,
		&deferUntil,
		&fields,
		&b.Fleet,
		&deletedAt,
		&category,
		&operatedByID,
		&b.OperatedBy,
		&b.OperatedByDisplay,
		&b.OperatedByEmail,
	)
	if err != nil {
		return nil, 0, err
	}

	b.Slug = slug.String
	b.ShortTitle = shortTitle.String
	b.Heraldry = heraldry.String
	b.Description = description.String
	b.Notes = notes.String
	b.Assignee = assignee.String
	b.Owner = owner.String
	b.CreatedBy = createdBy.String
	b.ClosedBy = closedBy.String

	if closedAt.Valid {
		t := closedAt.Time
		b.ClosedAt = &t
	}
	if dueAt.Valid {
		t := dueAt.Time
		b.DueAt = &t
	}
	if deferUntil.Valid {
		t := deferUntil.Time
		b.DeferUntil = &t
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		b.DeletedAt = &t
	}
	if len(fields) > 0 {
		b.Fields = json.RawMessage(fields)
	}
	b.Category = category.String
	if operatedByID.Valid {
		v := operatedByID.Int64
		b.OperatedByID = &v
	}

	return &b, score, nil
}

// scanDependency scans a single row into a model.Dependency.
func scanDependency(row scannable) (*model.Dependency, error) {
	var d model.Dependency
	var (
		createdBy sql.NullString
		metadata  sql.NullString
	)
	err := row.Scan(
		&d.BeadID,
		&d.DependsOnID,
		&d.Type,
		&d.CreatedAt,
		&createdBy,
		&metadata,
	)
	if err != nil {
		return nil, err
	}
	d.CreatedBy = createdBy.String
	d.Metadata = metadata.String
	return &d, nil
}

// scanDependencies scans multiple rows into a slice of model.Dependency pointers.
func scanDependencies(rows *sql.Rows) ([]*model.Dependency, error) {
	var deps []*model.Dependency
	for rows.Next() {
		d, err := scanDependency(rows)
		if err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deps, nil
}

// scanCommentFull scans a comment row that includes the JOINed operator
// display + email columns trailing the standard 6 comment columns, then
// impersonated_by (migration 061). Used by queryGetComments so per-comment
// chips resolve server-side. The operator columns are COALESCE-pinned and
// impersonated_by is NOT NULL, so they read into plain string destinations.
func scanCommentFull(row scannable) (*model.Comment, error) {
	var c model.Comment
	var (
		author     sql.NullString
		operatedBy sql.NullString
	)
	err := row.Scan(
		&c.ID,
		&c.BeadID,
		&author,
		&c.Text,
		&c.CreatedAt,
		&operatedBy,
		&c.OperatedByDisplay,
		&c.OperatedByEmail,
		&c.ImpersonatedBy,
	)
	if err != nil {
		return nil, err
	}
	c.Author = author.String
	c.OperatedBy = operatedBy.String
	return &c, nil
}

// scanCommentsFull scans multiple JOINed rows into a slice of model.Comment.
func scanCommentsFull(rows *sql.Rows) ([]*model.Comment, error) {
	var comments []*model.Comment
	for rows.Next() {
		c, err := scanCommentFull(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return comments, nil
}

// scanEvent scans a single row into a model.Event.
func scanEvent(row scannable) (*model.Event, error) {
	var e model.Event
	var (
		actor      sql.NullString
		payload    []byte
		operatedBy sql.NullString
	)
	err := row.Scan(&e.ID, &e.Topic, &e.BeadID, &actor, &payload, &e.CreatedAt, &operatedBy)
	if err != nil {
		return nil, err
	}
	e.Actor = actor.String
	if len(payload) > 0 {
		e.Payload = json.RawMessage(payload)
	}
	e.OperatedBy = operatedBy.String
	return &e, nil
}

// scanEvents scans multiple rows into a slice of model.Event pointers.
func scanEvents(rows *sql.Rows) ([]*model.Event, error) {
	var events []*model.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// scanConfig scans a single row into a model.Config.
func scanConfig(row scannable) (*model.Config, error) {
	var c model.Config
	var value []byte
	err := row.Scan(&c.Key, &value, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.Value = json.RawMessage(value)
	return &c, nil
}

// scanConfigs scans multiple rows into a slice of model.Config pointers.
func scanConfigs(rows *sql.Rows) ([]*model.Config, error) {
	var configs []*model.Config
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return configs, nil
}

// scanTaint scans one row of the taints registry (docs/adr/taints.md
// §3/§15.2).
func scanTaint(row scannable) (*model.Taint, error) {
	var t model.Taint
	var policy []byte
	var retiredAt sql.NullTime
	err := row.Scan(&t.ID, &t.Name, &t.Description, &t.OwnerTeam, &policy, pq.Array(&t.Effects), &t.CreatedAt, &t.UpdatedAt, &retiredAt)
	if err != nil {
		return nil, err
	}
	t.ScrubPolicy = json.RawMessage(policy)
	if retiredAt.Valid {
		t.RetiredAt = &retiredAt.Time
	}
	return &t, nil
}

// scanTaints scans multiple taints rows into a slice of model.Taint pointers.
func scanTaints(rows *sql.Rows) ([]*model.Taint, error) {
	var taints []*model.Taint
	for rows.Next() {
		t, err := scanTaint(rows)
		if err != nil {
			return nil, err
		}
		taints = append(taints, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return taints, nil
}

// scanTaintGrant scans one row of taint_grants (docs/adr/taints.md §3).
func scanTaintGrant(row scannable) (*model.TaintGrant, error) {
	var g model.TaintGrant
	var expiresAt sql.NullTime
	err := row.Scan(&g.TaintID, &g.PrincipalKind, &g.Principal, &g.GrantedBy, &g.GrantedAt, &expiresAt)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		g.ExpiresAt = &expiresAt.Time
	}
	return &g, nil
}

// scanBeadTaint scans one row of bead_taints joined with taints.name.
func scanBeadTaint(row scannable) (*model.BeadTaint, error) {
	var bt model.BeadTaint
	var sourceBeadID sql.NullString
	err := row.Scan(&bt.BeadID, &bt.TaintID, &bt.TaintName, &bt.Source, &sourceBeadID, &bt.AppliedBy, &bt.AppliedAt)
	if err != nil {
		return nil, err
	}
	if sourceBeadID.Valid {
		bt.SourceBeadID = sourceBeadID.String
	}
	return &bt, nil
}

// scanBeadTaints scans multiple bead_taints rows into a slice of
// model.BeadTaint pointers.
func scanBeadTaints(rows *sql.Rows) ([]*model.BeadTaint, error) {
	var beadTaints []*model.BeadTaint
	for rows.Next() {
		bt, err := scanBeadTaint(rows)
		if err != nil {
			return nil, err
		}
		beadTaints = append(beadTaints, bt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return beadTaints, nil
}

// nullTimePtr converts a *time.Time to a sql.NullTime.
func nullTimePtr(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// nullInt64Ptr converts a *int64 to sql.NullInt64 for write-time bindings on
// nullable BIGINT columns. nil pointer → SQL NULL.
func nullInt64Ptr(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// nullString converts a string to sql.NullString; empty string is null.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// jsonbBytes converts json.RawMessage to a []byte suitable for JSONB columns.
func jsonbBytes(m json.RawMessage) []byte {
	if len(m) == 0 {
		return nil
	}
	return []byte(m)
}
