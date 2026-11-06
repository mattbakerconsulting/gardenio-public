package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// TestCovOpsBF covers zero-coverage pure/near-pure helpers in
// queries_operators_backfill.go and queries_operated_by_legacy_backfill.go
// using the existing sqlmock style. No real DB.

// formatRebindKey is a pure string formatter — table-test it.
func TestCovOpsBFFormatRebindKey(t *testing.T) {
	for _, tc := range []struct {
		category string
		opID     int64
		want     string
	}{
		{"A", 1, "A:1"},
		{"B", 42, "B:42"},
		{"C", 0, "C:0"},
		{"D", 9999, "D:9999"},
		{"", -1, ":-1"},
	} {
		if got := formatRebindKey(tc.category, tc.opID); got != tc.want {
			t.Errorf("formatRebindKey(%q,%d) = %q, want %q", tc.category, tc.opID, got, tc.want)
		}
	}
}

func TestCovOpsBFLookupSentinel_Found(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	id, err := lookupLegacyPreAttributionSentinel(ctx, conn)
	if err != nil {
		t.Fatalf("lookupLegacyPreAttributionSentinel: %v", err)
	}
	if id != 7 {
		t.Errorf("id = %d, want 7", id)
	}
}

func TestCovOpsBFLookupSentinel_Missing(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id FROM operators WHERE kind = 'system'`).
		WillReturnError(sql.ErrNoRows)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	if _, err := lookupLegacyPreAttributionSentinel(ctx, conn); err == nil {
		t.Fatal("want error when sentinel missing, got nil")
	}
}

func TestCovOpsBFCountCandidates_Found(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`FROM beads t\s+JOIN operator_aliases a`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(12)))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	n, err := countOperatorsBackfillCandidates(ctx, conn, "beads")
	if err != nil {
		t.Fatalf("countOperatorsBackfillCandidates: %v", err)
	}
	if n != 12 {
		t.Errorf("n = %d, want 12", n)
	}
}

func TestCovOpsBFCountCandidates_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`FROM comments t`).
		WillReturnError(errors.New("boom"))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	if _, err := countOperatorsBackfillCandidates(ctx, conn, "comments"); err == nil {
		t.Fatal("want error from countOperatorsBackfillCandidates, got nil")
	}
}

func TestCovOpsBFCountEmptySentinel_Found(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT count\(\*\) FROM beads\s+WHERE operated_by_id IS NULL AND operated_by = ''`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	n, err := countEmptySentinelCandidates(ctx, conn, "beads")
	if err != nil {
		t.Fatalf("countEmptySentinelCandidates: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
}

func TestCovOpsBFCountEmptySentinel_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT count\(\*\) FROM deps`).
		WillReturnError(errors.New("boom"))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	if _, err := countEmptySentinelCandidates(ctx, conn, "deps"); err == nil {
		t.Fatal("want error from countEmptySentinelCandidates, got nil")
	}
}

func TestCovOpsBFBindEmptyToSentinel_Updates(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE beads\s+SET operated_by_id = \$1\s+WHERE operated_by_id IS NULL AND operated_by = ''`).
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 5))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	n, err := bindEmptyToSentinel(ctx, conn, "beads", 7)
	if err != nil {
		t.Fatalf("bindEmptyToSentinel: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
}

func TestCovOpsBFBindEmptyToSentinel_Error(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE comments`).
		WithArgs(int64(7)).
		WillReturnError(errors.New("boom"))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	if _, err := bindEmptyToSentinel(ctx, conn, "comments", 7); err == nil {
		t.Fatal("want error from bindEmptyToSentinel, got nil")
	}
}

func TestCovOpsBFSingleShot_Updates(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE deps t\s+SET operated_by_id = a\.operator_id`).
		WillReturnResult(sqlmock.NewResult(0, 4))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	n, err := singleShotOperatorsBackfill(ctx, conn, "deps")
	if err != nil {
		t.Fatalf("singleShotOperatorsBackfill: %v", err)
	}
	if n != 4 {
		t.Errorf("n = %d, want 4", n)
	}
}

func TestCovOpsBFSingleShot_Error(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE deps t`).
		WillReturnError(errors.New("boom"))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	if _, err := singleShotOperatorsBackfill(ctx, conn, "deps"); err == nil {
		t.Fatal("want error from singleShotOperatorsBackfill, got nil")
	}
}

func TestCovOpsBFRunVerifier_AllZero(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	for _, tbl := range []string{"beads", "comments", "deps", "events", "mutation_history"} {
		mock.ExpectQuery(`SELECT count\(\*\) FROM ` + tbl + `\s+WHERE operated_by <> '' AND operated_by_id IS NULL`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	stats := &store.BackfillOperatorsStats{}
	if err := runVerifier(ctx, conn, stats); err != nil {
		t.Fatalf("runVerifier: %v", err)
	}
	if stats.VerifierBeads != 0 || stats.VerifierMutations != 0 {
		t.Errorf("expected all verifier counts zero, got %+v", stats)
	}
}

func TestCovOpsBFRunVerifier_NonZeroBeads(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT count\(\*\) FROM beads\s+WHERE operated_by <> ''`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	mock.ExpectQuery(`SELECT count\(\*\) FROM comments\s+WHERE operated_by <> ''`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(`SELECT count\(\*\) FROM deps\s+WHERE operated_by <> ''`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(`SELECT count\(\*\) FROM events\s+WHERE operated_by <> ''`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(`SELECT count\(\*\) FROM mutation_history\s+WHERE operated_by <> ''`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	stats := &store.BackfillOperatorsStats{}
	if err := runVerifier(ctx, conn, stats); err != nil {
		t.Fatalf("runVerifier: %v", err)
	}
	if stats.VerifierBeads != 2 {
		t.Errorf("VerifierBeads = %d, want 2", stats.VerifierBeads)
	}
}

func TestCovOpsBFRunVerifier_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT count\(\*\) FROM beads`).
		WillReturnError(errors.New("boom"))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	stats := &store.BackfillOperatorsStats{}
	if err := runVerifier(ctx, conn, stats); err == nil {
		t.Fatal("want error from runVerifier, got nil")
	}
}

func TestCovOpsBFCollectAndUpsert_DryRunQuarantine(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	// Distinct values: one valid (slack:U1), one quarantined (no colon),
	// one leading-colon (quarantined), one trailing-colon (quarantined).
	mock.ExpectQuery(`SELECT DISTINCT operated_by FROM`).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by"}).
			AddRow("slack:U1").
			AddRow("nocolon").
			AddRow(":leading").
			AddRow("trailing:"))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	stats := &store.BackfillOperatorsStats{DryRun: true}
	if err := collectAndUpsertOperators(ctx, conn, true, stats); err != nil {
		t.Fatalf("collectAndUpsertOperators: %v", err)
	}
	if stats.DistinctOperatedBy != 1 {
		t.Errorf("DistinctOperatedBy = %d, want 1", stats.DistinctOperatedBy)
	}
	if len(stats.QuarantinedOperatedBy) != 3 {
		t.Errorf("QuarantinedOperatedBy = %v, want 3 entries", stats.QuarantinedOperatedBy)
	}
}

func TestCovOpsBFCollectAndUpsert_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT DISTINCT operated_by FROM`).
		WillReturnError(errors.New("boom"))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	stats := &store.BackfillOperatorsStats{}
	if err := collectAndUpsertOperators(ctx, conn, true, stats); err == nil {
		t.Fatal("want error from collectAndUpsertOperators, got nil")
	}
}

func TestCovOpsBFCollectAndUpsert_LiveUpserts(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT DISTINCT operated_by FROM`).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by"}).AddRow("slack:U1"))

	// Per-operator upsert RETURNING true.
	mock.ExpectQuery(`INSERT INTO operators \(kind, subject\)`).
		WithArgs("slack", "U1").
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(true))

	// Bulk alias upsert.
	mock.ExpectExec(`INSERT INTO operator_aliases \(operator_id, kind, subject, source\)`).
		WithArgs("slack", "U1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	stats := &store.BackfillOperatorsStats{}
	if err := collectAndUpsertOperators(ctx, conn, false, stats); err != nil {
		t.Fatalf("collectAndUpsertOperators: %v", err)
	}
	if stats.OperatorsUpserted != 1 {
		t.Errorf("OperatorsUpserted = %d, want 1", stats.OperatorsUpserted)
	}
	if stats.AliasesUpserted != 1 {
		t.Errorf("AliasesUpserted = %d, want 1", stats.AliasesUpserted)
	}
}

// --- queries_operated_by_legacy_backfill.go helpers ---

func TestCovOpsBFCountOperatedByTarget_Found(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	ctx := context.Background()

	mock.ExpectQuery(`SELECT count\(\*\) FROM beads\s+WHERE operated_by_id = \$1`).
		WithArgs(int64(42), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(9)))

	n, err := countOperatedByTarget(ctx, store.db, 42, []string{"open"})
	if err != nil {
		t.Fatalf("countOperatedByTarget: %v", err)
	}
	if n != 9 {
		t.Errorf("n = %d, want 9", n)
	}
}

func TestCovOpsBFLookupOperatorAliasID_Found(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("slack", "U1").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(5)))

	id, err := lookupOperatorAliasID(ctx, db, "slack", "U1")
	if err != nil {
		t.Fatalf("lookupOperatorAliasID: %v", err)
	}
	if id != 5 {
		t.Errorf("id = %d, want 5", id)
	}
}

func TestCovOpsBFLookupOperatorAliasID_NoRows(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases`).
		WithArgs("slack", "ghost").
		WillReturnError(sql.ErrNoRows)

	id, err := lookupOperatorAliasID(ctx, db, "slack", "ghost")
	if err != nil {
		t.Fatalf("lookupOperatorAliasID (no rows): err = %v, want nil", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0 for no alias", id)
	}
}

func TestCovOpsBFLookupBeadOperatorID_Valid(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id"}).AddRow(int64(11)))

	id, err := lookupBeadOperatorID(ctx, db, "gd-1")
	if err != nil {
		t.Fatalf("lookupBeadOperatorID: %v", err)
	}
	if id != 11 {
		t.Errorf("id = %d, want 11", id)
	}
}

func TestCovOpsBFLookupBeadOperatorID_Null(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-2").
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id"}).AddRow(nil))

	id, err := lookupBeadOperatorID(ctx, db, "gd-2")
	if err != nil {
		t.Fatalf("lookupBeadOperatorID: %v", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0 for NULL operated_by_id", id)
	}
}

func TestCovOpsBFLookupBeadOperatorID_NoRows(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-missing").
		WillReturnError(sql.ErrNoRows)

	id, err := lookupBeadOperatorID(ctx, db, "gd-missing")
	if err != nil {
		t.Fatalf("lookupBeadOperatorID (no rows): err = %v, want nil", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0 for missing bead", id)
	}
}

func TestCovOpsBFDecodeFieldsLegacy(t *testing.T) {
	// Empty input → empty map.
	if m := decodeFieldsLegacy(nil); len(m) != 0 {
		t.Errorf("decodeFieldsLegacy(nil) = %v, want empty", m)
	}
	// Valid JSON → decoded.
	m := decodeFieldsLegacy([]byte(`{"slack_user_id":"U9"}`))
	if m["slack_user_id"] != "U9" {
		t.Errorf("decodeFieldsLegacy slack_user_id = %v, want U9", m["slack_user_id"])
	}
	// Invalid JSON → empty map, no panic.
	if m := decodeFieldsLegacy([]byte(`not-json`)); len(m) != 0 {
		t.Errorf("decodeFieldsLegacy(invalid) = %v, want empty", m)
	}
}

func TestCovOpsBFClassifyBead_ThreadResolved(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("slack", "U_OTHER").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(8)))

	b := candidateRow{
		id:        "gd-a",
		createdBy: "thread-123",
		fields:    []byte(`{"slack_user_id":"U_OTHER"}`),
	}
	cat, newOpID, err := classifyBead(ctx, db, b, "matt@x.ai", 42)
	if err != nil {
		t.Fatalf("classifyBead: %v", err)
	}
	if cat != "A" || newOpID != 8 {
		t.Errorf("cat=%q newOpID=%d, want A/8", cat, newOpID)
	}
}

func TestCovOpsBFClassifyBead_StaticSchedule(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("sched-1").
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id"}).AddRow(int64(13)))

	b := candidateRow{
		id:        "gd-b",
		createdBy: "static",
		fields:    []byte(`{"schedule_id":"sched-1"}`),
	}
	cat, newOpID, err := classifyBead(ctx, db, b, "matt@x.ai", 42)
	if err != nil {
		t.Fatalf("classifyBead: %v", err)
	}
	if cat != "B" || newOpID != 13 {
		t.Errorf("cat=%q newOpID=%d, want B/13", cat, newOpID)
	}
}

func TestCovOpsBFClassifyBead_EmptyCreatedByIsD(t *testing.T) {
	db, _ := newMockDB(t)
	ctx := context.Background()

	b := candidateRow{id: "gd-c", createdBy: "", fields: nil}
	cat, newOpID, err := classifyBead(ctx, db, b, "matt@x.ai", 42)
	if err != nil {
		t.Fatalf("classifyBead: %v", err)
	}
	if cat != "D" || newOpID != 0 {
		t.Errorf("cat=%q newOpID=%d, want D/0", cat, newOpID)
	}
}

func TestCovOpsBFClassifyBead_AgentSlugIsC(t *testing.T) {
	db, _ := newMockDB(t)
	ctx := context.Background()

	b := candidateRow{id: "gd-d", createdBy: "gardenio-agent-7", fields: nil}
	cat, newOpID, err := classifyBead(ctx, db, b, "matt@x.ai", 42)
	if err != nil {
		t.Fatalf("classifyBead: %v", err)
	}
	if cat != "C" || newOpID != 0 {
		t.Errorf("cat=%q newOpID=%d, want C/0", cat, newOpID)
	}
}

func TestCovOpsBFRebindGroup_UpdatesAndHistory(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	mock.ExpectExec(`UPDATE beads\s+SET operated_by_id = \$1\s+WHERE id = ANY`).
		WithArgs(int64(8), sqlmock.AnyArg(), int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-x", "kd admin backfill-operated-by-legacy", "operated_by_id", "42", "8", "backfill-legacy-A").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	groups := map[int64][]string{8: {"gd-x"}}
	if err := rebindGroup(ctx, tx, "A", 42, groups, stats); err != nil {
		t.Fatalf("rebindGroup: %v", err)
	}
	if stats.CategoryA != 1 {
		t.Errorf("CategoryA = %d, want 1", stats.CategoryA)
	}
	if stats.RebindCount["A:8"] != 1 {
		t.Errorf("RebindCount[A:8] = %d, want 1", stats.RebindCount["A:8"])
	}

	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestCovOpsBFRebindGroup_NoRowsAffectedSkipsHistory(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	// UPDATE affects zero rows -> no mutation_history insert, stats unchanged.
	mock.ExpectExec(`UPDATE beads\s+SET operated_by_id = \$1`).
		WithArgs(int64(8), sqlmock.AnyArg(), int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	groups := map[int64][]string{8: {"gd-x"}}
	if err := rebindGroup(ctx, tx, "B", 42, groups, stats); err != nil {
		t.Fatalf("rebindGroup: %v", err)
	}
	if stats.CategoryB != 0 {
		t.Errorf("CategoryB = %d, want 0 when no rows affected", stats.CategoryB)
	}

	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestCovOpsBFRebindGroup_UpdateError(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs(int64(8), sqlmock.AnyArg(), int64(42)).
		WillReturnError(errors.New("boom"))

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	groups := map[int64][]string{8: {"gd-x"}}
	if err := rebindGroup(ctx, tx, "A", 42, groups, stats); err == nil {
		t.Fatal("want error from rebindGroup, got nil")
	}

	mock.ExpectRollback()
	_ = tx.Rollback()
}

func TestCovOpsBFApplyBatchRebinds_Empty(t *testing.T) {
	db, _ := newMockDB(t)
	ctx := context.Background()

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	// Empty groups -> no tx begun, returns nil.
	if err := applyBatchRebinds(ctx, db, 42, map[int64][]string{}, map[int64][]string{}, stats); err != nil {
		t.Fatalf("applyBatchRebinds (empty): %v", err)
	}
}

func TestCovOpsBFApplyBatchRebinds_CommitsGroupA(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE beads\s+SET operated_by_id = \$1`).
		WithArgs(int64(8), sqlmock.AnyArg(), int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-x", "kd admin backfill-operated-by-legacy", "operated_by_id", "42", "8", "backfill-legacy-A").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	groupsA := map[int64][]string{8: {"gd-x"}}
	if err := applyBatchRebinds(ctx, db, 42, groupsA, map[int64][]string{}, stats); err != nil {
		t.Fatalf("applyBatchRebinds: %v", err)
	}
	if stats.CategoryA != 1 {
		t.Errorf("CategoryA = %d, want 1", stats.CategoryA)
	}
}

func TestCovOpsBFPass2OneIteration_NoUpdates(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectQuery(`WITH walked AS`).
		WithArgs(int64(42), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "new_op"}))
	mock.ExpectCommit()

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	n, err := pass2OneIteration(ctx, db, 42, pq.Array([]string{"open"}), stats)
	if err != nil {
		t.Fatalf("pass2OneIteration: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestCovOpsBFPass2OneIteration_OneUpdate(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectQuery(`WITH walked AS`).
		WithArgs(int64(42), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "new_op"}).AddRow("gd-c1", int64(99)))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-c1", "kd admin backfill-operated-by-legacy", "operated_by_id", "42", "99", "backfill-legacy-C").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	n, err := pass2OneIteration(ctx, db, 42, pq.Array([]string{"open"}), stats)
	if err != nil {
		t.Fatalf("pass2OneIteration: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	if stats.CategoryC != 1 {
		t.Errorf("CategoryC = %d, want 1", stats.CategoryC)
	}
	if stats.RebindCount["C:99"] != 1 {
		t.Errorf("RebindCount[C:99] = %d, want 1", stats.RebindCount["C:99"])
	}
}

func TestCovOpsBFPass2OneIteration_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectQuery(`WITH walked AS`).
		WithArgs(int64(42), sqlmock.AnyArg()).
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	stats := &store.BackfillOperatedByLegacyStats{RebindCount: map[string]int64{}}
	if _, err := pass2OneIteration(ctx, db, 42, pq.Array([]string{"open"}), stats); err == nil {
		t.Fatal("want error from pass2OneIteration, got nil")
	}
}
