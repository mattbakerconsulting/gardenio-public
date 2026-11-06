package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// scrubBeadRow builds a mock row (beadRowColumnsBase order) for the
// lockBeadForScrub FOR UPDATE read.
func scrubBeadRow(id, title, description, notes, fields, status string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(beadRowColumnsBase).AddRow(
		id, nil, nil, nil, "issue", "task", title, description, notes,
		status, 0, nil, nil, now, "someone", now, nil, nil, nil, nil,
		[]byte(fields), "default", nil, nil, nil,
	)
}

// scrubBeadRowDeleted mirrors scrubBeadRow but stamps a non-NULL deleted_at,
// for tests pinning gd-T5P13cSoBw: lockBeadForScrub locks and returns a
// soft-deleted bead exactly like a live one (no deleted_at filter in the
// query), so purge/strip must scrub it in place rather than skip it.
func scrubBeadRowDeleted(id, title, description, notes, fields, status string) *sqlmock.Rows {
	now := time.Now().UTC()
	deletedAt := now.Add(-time.Hour)
	return sqlmock.NewRows(beadRowColumnsBase).AddRow(
		id, nil, nil, nil, "issue", "task", title, description, notes,
		status, 0, nil, nil, now, "someone", now, nil, nil, nil, nil,
		[]byte(fields), "default", deletedAt, nil, nil,
	)
}

func expectTaintAuditInsert(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`INSERT INTO taint_audit`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(1, time.Now().UTC()))
}

// expectBeadTaintLock stubs the taintStillOnBead FOR UPDATE lock cascadeContent
// runs before touching anything: present=true returns a row (taint still
// applied), present=false returns sql.ErrNoRows (already scrubbed/untainted).
func expectBeadTaintLock(mock sqlmock.Sqlmock, beadID string, taintID int64, present bool) {
	q := mock.ExpectQuery(`SELECT 1 FROM bead_taints WHERE bead_id = \$1 AND taint_id = \$2 FOR UPDATE`).
		WithArgs(beadID, taintID)
	if present {
		q.WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	} else {
		q.WillReturnError(sql.ErrNoRows)
	}
}

// TestCascadeContent_Purge pins the exact per-table cascade docs/adr/
// taints.md §6.5 specifies for "purge": every table in cascadeContentTables
// is touched in order, plus the bead_taints marker delete, and the returned
// TablesTouched list is exactly cascadeContentTables (gd-fVAy32nWDf
// acceptance criterion 1).
func TestCascadeContent_Purge(t *testing.T) {
	db, mock := newMockDB(t)

	expectBeadTaintLock(mock, "gd-1", 42, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WithArgs("gd-1", "[purged]", "", "", []byte(`{}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments SET text = '\[purged\]' WHERE bead_id = \$1`).
		WithArgs("gd-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE events SET payload = '\{\}'::jsonb, operated_by_id = COALESCE\(operated_by_id, \$2\) WHERE bead_id = \$1`).
		WithArgs("gd-1", int64(99)).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`UPDATE mutation_history SET old_value = '', new_value = '', actor = \$2, operated_by_id = COALESCE\(operated_by_id, \$3\)`).
		WithArgs("gd-1", model.SystemActorTaintScrub, int64(99)).
		WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(`DELETE FROM bead_embeddings WHERE bead_id = \$1`).
		WithArgs("gd-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).
		WithArgs("gd-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates SET metadata = NULL`).
		WithArgs("gd-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints WHERE bead_id = \$1 AND taint_id = \$2`).
		WithArgs("gd-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	tables, ok, err := cascadeContent(context.Background(), db, "gd-1", 42, 99, scrubContentReplacement{
		Title: "[purged]", Description: "", Notes: "", Fields: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("cascadeContent: %v", err)
	}
	if !ok {
		t.Fatal("cascadeContent: ok = false, want true (taint still applied)")
	}
	if len(tables) != len(cascadeContentTables) {
		t.Fatalf("TablesTouched = %v, want %v", tables, cascadeContentTables)
	}
	for i, tbl := range cascadeContentTables {
		if tables[i] != tbl {
			t.Fatalf("TablesTouched[%d] = %q, want %q", i, tables[i], tbl)
		}
	}
}

// TestCascadeContent_BlanksFieldsSubkeyMutationHistory pins the merge-desk
// fix (round 3, PR #144): per-key subfield mutations recorded as
// field="fields.<key>" (http_beads.go's subfield-set/array-append/CAS/
// increment paths, auto_parent.go's fields.thread_id) carry the same
// tainted content as the whole-blob field="fields" row and must be blanked
// by the same cascade — the WHERE clause must match `field LIKE 'fields.%'`
// in addition to the fixed IN-list, or the pre-scrub value stays readable
// via mutation_history.new_value.
func TestCascadeContent_BlanksFieldsSubkeyMutationHistory(t *testing.T) {
	db, mock := newMockDB(t)

	expectBeadTaintLock(mock, "gd-1", 42, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments SET text = '\[purged\]' WHERE bead_id = \$1`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events SET payload = '\{\}'::jsonb, operated_by_id = COALESCE\(operated_by_id, \$2\) WHERE bead_id = \$1`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history SET old_value = '', new_value = '', actor = \$2, operated_by_id = COALESCE\(operated_by_id, \$3\)\s+WHERE bead_id = \$1 AND \(field IN \('title', 'description', 'notes', 'fields'\) OR field LIKE 'fields\.%'\)`).
		WithArgs("gd-1", model.SystemActorTaintScrub, int64(99)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM bead_embeddings WHERE bead_id = \$1`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates SET metadata = NULL`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints WHERE bead_id = \$1 AND taint_id = \$2`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, ok, err := cascadeContent(context.Background(), db, "gd-1", 42, 99, scrubContentReplacement{
		Title: "[purged]", Fields: []byte(`{}`),
	}); err != nil {
		t.Fatalf("cascadeContent: %v", err)
	} else if !ok {
		t.Fatal("cascadeContent: ok = false, want true (taint still applied)")
	}
}

// TestScrubPurgeBead covers the full purge branch: title lands as the
// literal "[purged]" placeholder (not blanked — docs/adr/taints.md §6.5
// literally specifies `title = '[purged]'`, matching comments' treatment),
// description/notes are blanked, fields keeps its keys with string values
// blanked (gd-a8c9mEme1G — replacing the whole blob with `{}` used to drop
// keys a custom type's TypeConfig declares `required`), and the pre-scrub
// content is what gets hashed into taint_audit.content_sha and the returned
// ContentSHA.
func TestScrubPurgeBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT ` + regexp.QuoteMeta(beadColumns) + ` FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-1").
		WillReturnRows(scrubBeadRow("gd-1", "Secret Title", "Secret Description", "Secret Notes", `{"k":"v"}`, "open"))

	expectBeadTaintLock(mock, "gd-1", 42, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WithArgs("gd-1", "[purged]", "", "", []byte(`{"k":""}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WithArgs("gd-1", int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WithArgs("gd-1", model.SystemActorTaintScrub, int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WithArgs("gd-1", int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{RequestID: "gd-req", TaintID: 42, Kind: "purge", Actor: "oidc:alice@example.com", ActorOperatorID: 7}
	br, err := scrubPurgeBead(context.Background(), db, req, "gd-1")
	if err != nil {
		t.Fatalf("scrubPurgeBead: %v", err)
	}

	wantSHA := hashBeadContent("Secret Title", "Secret Description", "Secret Notes", []byte(`{"k":"v"}`))
	if br.ContentSHA != wantSHA {
		t.Errorf("ContentSHA = %q, want %q (hash of the PRE-scrub content)", br.ContentSHA, wantSHA)
	}
	if len(br.TablesTouched) != len(cascadeContentTables) {
		t.Errorf("TablesTouched = %v, want full cascadeContentTables list", br.TablesTouched)
	}
	if br.Skipped {
		t.Error("Skipped = true, want false — the taint was still applied")
	}
}

// TestScrubPurgeBead_SkipsWhenTaintAlreadyGone pins the gd-0fQDgy18lo fix:
// scrubPurgeBead must lock and re-check bead_taints for (bead_id, taint_id)
// before cascading, and skip the destructive cascade entirely (no content
// rewrite, no taint_audit row) when the row is already gone — e.g. a prior
// scrub or an explicit untaint cleared it between request-creation and
// execution. Only the bead lock and the bead_taints check run; nothing else
// on the mock is expected, so an unexpected cascade call fails the test.
func TestScrubPurgeBead_SkipsWhenTaintAlreadyGone(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT ` + regexp.QuoteMeta(beadColumns) + ` FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-1").
		WillReturnRows(scrubBeadRow("gd-1", "Secret Title", "Secret Description", "Secret Notes", `{"k":"v"}`, "open"))
	expectBeadTaintLock(mock, "gd-1", 42, false)

	req := &model.ScrubTaintRequest{RequestID: "gd-req", TaintID: 42, Kind: "purge", Actor: "oidc:alice@example.com", ActorOperatorID: 7}
	br, err := scrubPurgeBead(context.Background(), db, req, "gd-1")
	if err != nil {
		t.Fatalf("scrubPurgeBead: %v", err)
	}
	if !br.Skipped || br.SkipReason != model.ScrubSkipTaintGone {
		t.Errorf("Skipped/SkipReason = %v/%q, want true/%q — bead_taints no longer carries this pair", br.Skipped, br.SkipReason, model.ScrubSkipTaintGone)
	}
	if br.ContentSHA != "" {
		t.Errorf("ContentSHA = %q, want empty on skip", br.ContentSHA)
	}
	if len(br.TablesTouched) != 0 {
		t.Errorf("TablesTouched = %v, want empty on skip", br.TablesTouched)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubPurgeBead_ScrubsSoftDeletedBead pins the merge-desk-corrected
// gd-T5P13cSoBw fix: a resolved_bead_ids entry that has since been
// soft-deleted must NOT be skipped by purge — soft delete keeps every copy
// of the content and is restorable (ADR taints.md §6.5, §12.E), so skipping
// would leave tainted title/description/notes/fields/mutation_history/
// comments in place for `kd restore` to revive. lockBeadForScrub locks and
// returns the soft-deleted row like any other, and the full cascade +
// taint_audit row run exactly as TestScrubPurgeBead's live-bead case.
func TestScrubPurgeBead_ScrubsSoftDeletedBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT ` + regexp.QuoteMeta(beadColumns) + ` FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-1").
		WillReturnRows(scrubBeadRowDeleted("gd-1", "Secret Title", "Secret Description", "Secret Notes", `{"k":"v"}`, "open"))

	expectBeadTaintLock(mock, "gd-1", 42, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WithArgs("gd-1", "[purged]", "", "", []byte(`{"k":""}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WithArgs("gd-1", int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WithArgs("gd-1", model.SystemActorTaintScrub, int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WithArgs("gd-1", int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{RequestID: "gd-req", TaintID: 42, Kind: "purge", Actor: "oidc:alice@example.com", ActorOperatorID: 7}
	br, err := scrubPurgeBead(context.Background(), db, req, "gd-1")
	if err != nil {
		t.Fatalf("scrubPurgeBead: %v", err)
	}
	wantSHA := hashBeadContent("Secret Title", "Secret Description", "Secret Notes", []byte(`{"k":"v"}`))
	if br.Skipped {
		t.Errorf("Skipped = true, want false — a soft-deleted bead must still be scrubbed, not skipped")
	}
	if br.ContentSHA != wantSHA {
		t.Errorf("ContentSHA = %q, want %q (hash of the PRE-scrub content)", br.ContentSHA, wantSHA)
	}
	if len(br.TablesTouched) != len(cascadeContentTables) {
		t.Errorf("TablesTouched = %v, want full cascadeContentTables list", br.TablesTouched)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubPurgeBead_PreservesRequiredFieldKeys pins gd-a8c9mEme1G: purge
// used to replace the whole fields blob with `{}`, so a bead of a custom
// type with a `required` FieldDef lost that key and a later PATCH 400'd on
// "is required". purge must keep every key present (so validateFieldsImpl's
// present-and-non-nil check still passes) while destroying string content —
// including inside nested objects/arrays — and leaving non-string values
// (the count here) completely untouched.
func TestScrubPurgeBead_PreservesRequiredFieldKeys(t *testing.T) {
	db, mock := newMockDB(t)

	origFields := `{"severity":"critical secret","count":3,"tags":["a","b"],"meta":{"note":"secret note"}}`
	wantFields := `{"count":3,"meta":{"note":""},"severity":"","tags":["",""]}`

	mock.ExpectQuery(`SELECT ` + regexp.QuoteMeta(beadColumns) + ` FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-1").
		WillReturnRows(scrubBeadRow("gd-1", "Secret Title", "Secret Description", "Secret Notes", origFields, "open"))

	expectBeadTaintLock(mock, "gd-1", 42, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WithArgs("gd-1", "[purged]", "", "", []byte(wantFields)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WithArgs("gd-1", int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WithArgs("gd-1", model.SystemActorTaintScrub, int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WithArgs("gd-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WithArgs("gd-1", int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{RequestID: "gd-req", TaintID: 42, Kind: "purge", Actor: "oidc:alice@example.com", ActorOperatorID: 7}
	br, err := scrubPurgeBead(context.Background(), db, req, "gd-1")
	if err != nil {
		t.Fatalf("scrubPurgeBead: %v", err)
	}
	if br.Skipped {
		t.Error("Skipped = true, want false — the taint was still applied")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestPurgeProseFromFields is a table-driven pin of the value-level blanking
// purgeProseFromFields performs: strings (top-level and nested) become "",
// every other JSON value type and every key survives untouched.
func TestPurgeProseFromFields(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", `{}`},
		{"empty object", "{}", `{}`},
		{"mixed types", `{"s":"secret","n":3,"b":true,"z":null}`, `{"b":true,"n":3,"s":"","z":null}`},
		{"nested object", `{"o":{"inner":"secret"}}`, `{"o":{"inner":""}}`},
		{"array of strings", `{"a":["x","y"]}`, `{"a":["",""]}`},
		{"array of objects", `{"a":[{"k":"v"}]}`, `{"a":[{"k":""}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in json.RawMessage
			if tc.in != "" {
				in = json.RawMessage(tc.in)
			}
			got, err := purgeProseFromFields(in)
			if err != nil {
				t.Fatalf("purgeProseFromFields: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("purgeProseFromFields(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestScrubDetaintStripBead covers the detaint/strip branch: same cascade as
// purge, but the bead's own prose lands as the attested clean_content
// instead of a placeholder, and no status change/successor is involved
// (gd-fVAy32nWDf acceptance criterion 3).
func TestScrubDetaintStripBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-2").
		WillReturnRows(scrubBeadRow("gd-2", "Tainted Title", "Tainted Description", "Tainted Notes", `{"secret":true}`, "open"))

	expectBeadTaintLock(mock, "gd-2", 7, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WithArgs("gd-2", "Clean Title", "Clean Description", "Clean Notes", []byte(`{"clean":true}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WithArgs("gd-2", int64(11)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WithArgs("gd-2", model.SystemActorTaintScrub, int64(11)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WithArgs("gd-2", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req2", TaintID: 7, Kind: "detaint", DetaintMode: model.DetaintModeStrip,
		Actor: "oidc:bob@example.com", ActorOperatorID: 11,
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-2": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	br, err := scrubDetaintStripBead(context.Background(), db, req, "gd-2")
	if err != nil {
		t.Fatalf("scrubDetaintStripBead: %v", err)
	}
	wantSHA := hashBeadContent("Tainted Title", "Tainted Description", "Tainted Notes", []byte(`{"secret":true}`))
	if br.ContentSHA != wantSHA {
		t.Errorf("ContentSHA = %q, want %q (hash of the PRE-scrub content)", br.ContentSHA, wantSHA)
	}
	if br.SuccessorBeadID != "" {
		t.Errorf("SuccessorBeadID = %q, want empty — strip never creates a successor", br.SuccessorBeadID)
	}
	if br.Skipped {
		t.Error("Skipped = true, want false — the taint was still applied")
	}
}

// TestScrubDetaintStripBead_SkipsWhenTaintAlreadyGone mirrors
// TestScrubPurgeBead_SkipsWhenTaintAlreadyGone for the strip branch: it
// shares cascadeContent with purge, so the same stale-taint race applies —
// content must not be rewritten on behalf of a taint bead_taints no longer
// carries.
func TestScrubDetaintStripBead_SkipsWhenTaintAlreadyGone(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-2").
		WillReturnRows(scrubBeadRow("gd-2", "Tainted Title", "Tainted Description", "Tainted Notes", `{"secret":true}`, "open"))
	expectBeadTaintLock(mock, "gd-2", 7, false)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req2", TaintID: 7, Kind: "detaint", DetaintMode: model.DetaintModeStrip,
		Actor: "oidc:bob@example.com", ActorOperatorID: 11,
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-2": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	br, err := scrubDetaintStripBead(context.Background(), db, req, "gd-2")
	if err != nil {
		t.Fatalf("scrubDetaintStripBead: %v", err)
	}
	if !br.Skipped || br.SkipReason != model.ScrubSkipTaintGone {
		t.Errorf("Skipped/SkipReason = %v/%q, want true/%q — bead_taints no longer carries this pair", br.Skipped, br.SkipReason, model.ScrubSkipTaintGone)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintStripBead_ScrubsSoftDeletedBead mirrors
// TestScrubPurgeBead_ScrubsSoftDeletedBead for the strip branch
// (gd-T5P13cSoBw): a soft-deleted bead is locked and stripped exactly like a
// live one — same cascade, same taint_audit row — never skipped.
func TestScrubDetaintStripBead_ScrubsSoftDeletedBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-2").
		WillReturnRows(scrubBeadRowDeleted("gd-2", "Tainted Title", "Tainted Description", "Tainted Notes", `{"secret":true}`, "open"))

	expectBeadTaintLock(mock, "gd-2", 7, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WithArgs("gd-2", "Clean Title", "Clean Description", "Clean Notes", []byte(`{"clean":true}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WithArgs("gd-2", int64(11)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WithArgs("gd-2", model.SystemActorTaintScrub, int64(11)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WithArgs("gd-2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WithArgs("gd-2", int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req2", TaintID: 7, Kind: "detaint", DetaintMode: model.DetaintModeStrip,
		Actor: "oidc:bob@example.com", ActorOperatorID: 11,
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-2": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	br, err := scrubDetaintStripBead(context.Background(), db, req, "gd-2")
	if err != nil {
		t.Fatalf("scrubDetaintStripBead: %v", err)
	}
	if br.Skipped {
		t.Errorf("Skipped = true, want false — a soft-deleted bead must still be stripped, not skipped")
	}
	wantSHA := hashBeadContent("Tainted Title", "Tainted Description", "Tainted Notes", []byte(`{"secret":true}`))
	if br.ContentSHA != wantSHA {
		t.Errorf("ContentSHA = %q, want %q (hash of the PRE-scrub content)", br.ContentSHA, wantSHA)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintStripBead_AbortsOnAttestationDriftForSoftDeletedBead pins
// the merge desk's requirement that #175's verifyAttestationNotDrifted
// (gd-JnACECPcvy) keeps running on the FOR UPDATE-locked row even when that
// row is soft-deleted: strip must not scrub a soft-deleted bead whose
// content drifted since attestation any more than it would for a live one.
// No cascade/audit calls are expected on the mock.
func TestScrubDetaintStripBead_AbortsOnAttestationDriftForSoftDeletedBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-2").
		WillReturnRows(scrubBeadRowDeleted("gd-2", "Tainted Title", "Tainted Description", "Tainted Notes", `{"secret":true}`, "open"))

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req2", TaintID: 7, Kind: "detaint", DetaintMode: model.DetaintModeStrip,
		Actor: "oidc:bob@example.com", ActorOperatorID: 11,
		ContentSHAs: map[string]string{"gd-2": "stale-sha-from-before-the-edit"},
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-2": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	if _, err := scrubDetaintStripBead(context.Background(), db, req, "gd-2"); err == nil {
		t.Fatal("want error for drifted content on a soft-deleted bead, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintStripBead_AbortsOnAttestationDrift pins the gd-JnACECPcvy
// fix: when req.ContentSHAs carries an entry for beadID that no longer
// matches the bead's CURRENT content (read under lockBeadForScrub's FOR
// UPDATE lock, in-transaction), scrubDetaintStripBead must abort before
// cascading — no cascadeContent/taint_audit calls are expected on the mock,
// so an unexpected call fails the test — closing the TOCTOU window between
// the approve-time checkAttestationDrift (an earlier, unlocked read) and
// this transaction.
func TestScrubDetaintStripBead_AbortsOnAttestationDrift(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-2").
		WillReturnRows(scrubBeadRow("gd-2", "Tainted Title", "Tainted Description", "Tainted Notes", `{"secret":true}`, "open"))

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req2", TaintID: 7, Kind: "detaint", DetaintMode: model.DetaintModeStrip,
		Actor: "oidc:bob@example.com", ActorOperatorID: 11,
		ContentSHAs: map[string]string{"gd-2": "stale-sha-from-before-the-edit"},
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-2": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	if _, err := scrubDetaintStripBead(context.Background(), db, req, "gd-2"); err == nil {
		t.Fatal("want error for drifted content, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintForkBead_AbortsOnAttestationDrift mirrors the strip guard
// for the fork branch: fork still bases the successor on attestation.
// clean_content, so a bead that drifted after attestation must abort the
// same way — no bead close, successor create, or dep insert is expected on
// the mock (gd-JnACECPcvy).
func TestScrubDetaintForkBead_AbortsOnAttestationDrift(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-orig").
		WillReturnRows(scrubBeadRow("gd-orig", "Tainted Title", "Tainted Description", "Tainted Notes", `{}`, "open"))

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req3", TaintID: 9, Kind: "detaint", DetaintMode: model.DetaintModeFork,
		Actor: "oidc:carol@example.com", ActorOperatorID: 42,
		ContentSHAs: map[string]string{"gd-orig": "stale-sha-from-before-the-edit"},
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-orig": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	if _, err := scrubDetaintForkBead(context.Background(), db, req, "gd-orig"); err == nil {
		t.Fatal("want error for drifted content, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestVerifyAttestationNotDrifted covers the drift-check helper directly:
// no attestation (nil/empty ContentSHAs) is a no-op (purge, and any detaint
// request with no attestation on record); a matching hash passes; a
// mismatched hash or a missing entry for beadID both fail (gd-JnACECPcvy).
func TestVerifyAttestationNotDrifted(t *testing.T) {
	bead := &model.Bead{ID: "gd-1", Title: "T", Description: "D", Notes: "N", Fields: []byte(`{}`)}
	sha := model.BeadContentSHA(bead)

	if err := verifyAttestationNotDrifted(bead, "gd-1", nil); err != nil {
		t.Errorf("nil ContentSHAs: want no-op, got %v", err)
	}
	if err := verifyAttestationNotDrifted(bead, "gd-1", map[string]string{"gd-1": sha}); err != nil {
		t.Errorf("matching sha: want nil, got %v", err)
	}
	if err := verifyAttestationNotDrifted(bead, "gd-1", map[string]string{"gd-1": "wrong-sha"}); err == nil {
		t.Error("mismatched sha: want error, got nil")
	}
	if err := verifyAttestationNotDrifted(bead, "gd-1", map[string]string{"gd-other": sha}); err == nil {
		t.Error("missing entry for beadID: want error, got nil")
	}
}

// TestScrubDetaintStripBead_MissingCleanContent covers the input-validation
// guard: strip requires an attested clean_content entry for the bead.
func TestScrubDetaintStripBead_MissingCleanContent(t *testing.T) {
	db, _ := newMockDB(t)
	req := &model.ScrubTaintRequest{RequestID: "gd-req", TaintID: 1, Kind: "detaint", DetaintMode: model.DetaintModeStrip}
	if _, err := scrubDetaintStripBead(context.Background(), db, req, "gd-missing"); err == nil {
		t.Fatal("want error for missing clean_content, got nil")
	}
}

// TestScrubDetaintLiftBead covers the detaint/lift branch (ADR §15.4,
// gd-XAXEBMrKOD acceptance criterion 1): the bead_taints marker is deleted
// in place with no content cascade — content/comments/history/embeddings
// are all left untouched (no cascade calls are expected on the mock — an
// unexpected call fails the test) — and the usual taint_audit row is
// written, hashing the bead's CURRENT (unchanged) content.
func TestScrubDetaintLiftBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT effects FROM taints WHERE id = \$1`).
		WithArgs(int64(13)).
		WillReturnRows(sqlmock.NewRows([]string{"effects"}).AddRow("{}"))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-3").
		WillReturnRows(scrubBeadRow("gd-3", "Untrusted Title", "Untrusted Description", "Untrusted Notes", `{"x":1}`, "open"))
	expectBeadTaintLock(mock, "gd-3", 13, true)
	mock.ExpectExec(`DELETE FROM bead_taints WHERE bead_id = \$1 AND taint_id = \$2`).
		WithArgs("gd-3", int64(13)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req-lift", TaintID: 13, Kind: "detaint", DetaintMode: model.DetaintModeLift,
		Actor: "oidc:dora@example.com", ActorOperatorID: 5,
	}
	br, err := scrubDetaintLiftBead(context.Background(), db, req, "gd-3")
	if err != nil {
		t.Fatalf("scrubDetaintLiftBead: %v", err)
	}
	wantSHA := hashBeadContent("Untrusted Title", "Untrusted Description", "Untrusted Notes", []byte(`{"x":1}`))
	if br.ContentSHA != wantSHA {
		t.Errorf("ContentSHA = %q, want %q", br.ContentSHA, wantSHA)
	}
	if br.SuccessorBeadID != "" {
		t.Errorf("SuccessorBeadID = %q, want empty — lift never creates a successor", br.SuccessorBeadID)
	}
	if len(br.TablesTouched) != 0 {
		t.Errorf("TablesTouched = %v, want empty — lift runs no content cascade", br.TablesTouched)
	}
	if br.Skipped {
		t.Error("Skipped = true, want false — the taint was still applied")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintLiftBead_RefusedForGateEffect is the negative test ADR
// §15.4 requires: lift fails closed at execute time for a taint whose
// effects include "gate", even though the policy validator
// (ValidateScrubPolicy) already refuses to save such a policy — this is the
// defence-in-depth re-check, for a taint whose effects changed (or a
// scrub-request that was built and approved under an older policy) after
// that validation ran. No bead lock, bead_taints delete, or audit write may
// happen — the mock has no expectations set for any of them, so an
// unexpected call fails the test.
func TestScrubDetaintLiftBead_RefusedForGateEffect(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT effects FROM taints WHERE id = \$1`).
		WithArgs(int64(14)).
		WillReturnRows(sqlmock.NewRows([]string{"effects"}).AddRow("{gate}"))

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req-lift2", TaintID: 14, Kind: "detaint", DetaintMode: model.DetaintModeLift,
		Actor: "oidc:dora@example.com",
	}
	if _, err := scrubDetaintLiftBead(context.Background(), db, req, "gd-4"); err == nil {
		t.Fatal("want error for lift against a gate-effect taint, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintLiftBead_SkipsWhenTaintAlreadyGone mirrors the purge/strip
// stale-taint guard (gd-0fQDgy18lo): lift must not delete bead_taints or
// write an audit row for a taint bead_taints no longer carries.
func TestScrubDetaintLiftBead_SkipsWhenTaintAlreadyGone(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT effects FROM taints WHERE id = \$1`).
		WithArgs(int64(13)).
		WillReturnRows(sqlmock.NewRows([]string{"effects"}).AddRow("{}"))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-3").
		WillReturnRows(scrubBeadRow("gd-3", "T", "D", "N", `{}`, "open"))
	expectBeadTaintLock(mock, "gd-3", 13, false)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req-lift3", TaintID: 13, Kind: "detaint", DetaintMode: model.DetaintModeLift,
		Actor: "oidc:dora@example.com",
	}
	br, err := scrubDetaintLiftBead(context.Background(), db, req, "gd-3")
	if err != nil {
		t.Fatalf("scrubDetaintLiftBead: %v", err)
	}
	if !br.Skipped {
		t.Error("Skipped = false, want true — bead_taints no longer carries this pair")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintForkBead covers the detaint/fork branch (gd-fVAy32nWDf
// acceptance criterion 2): the original is closed with
// fields.close_reason="scrubbed" and its content/bead_taints untouched (no
// cascade calls are expected on the mock — an unexpected call fails the
// test); a successor bead is created from clean_content and linked back via
// a "supersedes" dependency edge; content_sha hashes the ORIGINAL's current
// (unchanged) content. It also pins the merge-desk fix (round 3, PR #144):
// every mutation_history/bead/dep write this branch makes carries the
// resolved actor operator id, never NULL — migration 026's
// operated_by_id CHECK is enforced on every new mutation_history insert
// (NOT VALID only grandfathers pre-existing rows), and beads/deps.
// operated_by_id are hard NOT NULL FKs, so a NULL here rolls back the whole
// scrub on real Postgres even though go-sqlmock has no constraints to catch
// it.
func TestScrubDetaintForkBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-orig").
		WillReturnRows(scrubBeadRow("gd-orig", "Tainted Title", "Tainted Description", "Tainted Notes", `{}`, "open"))

	mock.ExpectExec(`UPDATE beads SET status = 'closed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-orig", model.SystemActorTaintScrub, "status", "open", "closed", model.MutationTypeUpdate, int64(42)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-orig", model.SystemActorTaintScrub, "fields", "{}", `{"close_reason":"scrubbed"}`, model.MutationTypeUpdate, int64(42)).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectExec(`INSERT INTO beads`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO deps`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req3", TaintID: 9, Kind: "detaint", DetaintMode: model.DetaintModeFork,
		Actor: "oidc:carol@example.com", ActorOperatorID: 42,
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-orig": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	br, err := scrubDetaintForkBead(context.Background(), db, req, "gd-orig")
	if err != nil {
		t.Fatalf("scrubDetaintForkBead: %v", err)
	}
	wantSHA := hashBeadContent("Tainted Title", "Tainted Description", "Tainted Notes", []byte(`{}`))
	if br.ContentSHA != wantSHA {
		t.Errorf("ContentSHA = %q, want %q (hash of the CURRENT, unchanged content)", br.ContentSHA, wantSHA)
	}
	if br.SuccessorBeadID == "" {
		t.Error("SuccessorBeadID is empty, want a generated successor id")
	}
	if len(br.TablesTouched) != 0 {
		t.Errorf("TablesTouched = %v, want empty — fork never runs cascadeContent", br.TablesTouched)
	}
}

// TestScrubDetaintForkBead_SkipsSoftDeletedBead pins gd-T5P13cSoBw for the
// fork branch: an original that has since been soft-deleted has nothing live
// left to close or fork from (unlike purge/strip, which scrub a soft-deleted
// bead in place), so it must be skipped rather than erroring out the whole
// request — no UPDATE/INSERT calls beyond the lock are expected on the mock —
// but the skip still gets a durable taint_audit row (Action=
// TaintAuditScrubSkip) so the request's audit trail records it.
func TestScrubDetaintForkBead_SkipsSoftDeletedBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-orig").
		WillReturnRows(scrubBeadRowDeleted("gd-orig", "Tainted Title", "Tainted Description", "Tainted Notes", `{}`, "open"))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req3", TaintID: 9, Kind: "detaint", DetaintMode: model.DetaintModeFork,
		Actor: "oidc:carol@example.com", ActorOperatorID: 42,
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-orig": {Title: "Clean Title", Description: "Clean Description", Notes: "Clean Notes", Fields: []byte(`{"clean":true}`)},
		},
	}
	br, err := scrubDetaintForkBead(context.Background(), db, req, "gd-orig")
	if err != nil {
		t.Fatalf("scrubDetaintForkBead: %v", err)
	}
	if !br.Skipped || br.SkipReason != model.ScrubSkipSoftDeleted {
		t.Errorf("Skipped/SkipReason = %v/%q, want true/%q — bead was soft-deleted", br.Skipped, br.SkipReason, model.ScrubSkipSoftDeleted)
	}
	if br.SuccessorBeadID != "" {
		t.Errorf("SuccessorBeadID = %q, want empty on skip", br.SuccessorBeadID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestScrubDetaintForkBead_FallsBackToMachineOperator covers the case the
// merge desk flagged: an approver identity that never resolved to an
// operator (ActorOperatorID == 0, e.g. no operator_aliases row yet) must not
// leave mutation_history/bead/dep rows with a NULL operator — ScrubTaint
// falls back to the seeded system:machine operator (migration 036) instead.
func TestScrubDetaintForkBead_FallsBackToMachineOperator(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("system", "machine").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(99)))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-orig2").
		WillReturnRows(scrubBeadRow("gd-orig2", "T", "D", "N", `{}`, "open"))

	mock.ExpectExec(`UPDATE beads SET status = 'closed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-orig2", model.SystemActorTaintScrub, "status", "open", "closed", model.MutationTypeUpdate, int64(99)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-orig2", model.SystemActorTaintScrub, "fields", "{}", `{"close_reason":"scrubbed"}`, model.MutationTypeUpdate, int64(99)).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectExec(`INSERT INTO beads`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO deps`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req-fallback", TaintID: 9, Kind: "detaint", DetaintMode: model.DetaintModeFork,
		Actor: "oidc:unresolved@example.com", // ActorOperatorID left at 0
		CleanContent: map[string]model.ScrubCleanContent{
			"gd-orig2": {Title: "Clean Title"},
		},
	}
	br, err := scrubDetaintForkBead(context.Background(), db, req, "gd-orig2")
	if err != nil {
		t.Fatalf("scrubDetaintForkBead: %v", err)
	}
	if br.SuccessorBeadID == "" {
		t.Error("SuccessorBeadID is empty, want a generated successor id")
	}
}

// TestScrubDetaintForkBead_MissingCleanContent mirrors the strip guard.
func TestScrubDetaintForkBead_MissingCleanContent(t *testing.T) {
	db, _ := newMockDB(t)
	req := &model.ScrubTaintRequest{RequestID: "gd-req", TaintID: 1, Kind: "detaint", DetaintMode: model.DetaintModeFork}
	if _, err := scrubDetaintForkBead(context.Background(), db, req, "gd-missing"); err == nil {
		t.Fatal("want error for missing clean_content, got nil")
	}
}

// TestQueryScrubTaint_Dispatch covers queryScrubTaint's kind/mode routing:
// an empty DetaintMode defaults to fork (the scrub_policy default, §6.1),
// and an unknown kind/mode is refused rather than silently no-op'd.
func TestQueryScrubTaint_Dispatch(t *testing.T) {
	t.Run("unknown kind", func(t *testing.T) {
		db, _ := newMockDB(t)
		req := &model.ScrubTaintRequest{Kind: "not-a-kind", BeadIDs: []string{"gd-1"}}
		if _, err := queryScrubTaint(context.Background(), db, req); err == nil {
			t.Fatal("want error for unknown kind, got nil")
		}
	})

	t.Run("unknown detaint mode", func(t *testing.T) {
		db, _ := newMockDB(t)
		req := &model.ScrubTaintRequest{Kind: "detaint", DetaintMode: "yolo", BeadIDs: []string{"gd-1"}}
		if _, err := queryScrubTaint(context.Background(), db, req); err == nil {
			t.Fatal("want error for an unknown detaint_mode, got nil")
		}
	})

	t.Run("detaint mode lift", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT effects FROM taints WHERE id = \$1`).
			WithArgs(int64(0)).
			WillReturnRows(sqlmock.NewRows([]string{"effects"}).AddRow("{}"))
		mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
			WithArgs("gd-1").
			WillReturnRows(scrubBeadRow("gd-1", "T", "D", "N", `{}`, "open"))
		expectBeadTaintLock(mock, "gd-1", 0, true)
		mock.ExpectExec(`DELETE FROM bead_taints WHERE bead_id = \$1 AND taint_id = \$2`).
			WithArgs("gd-1", int64(0)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectTaintAuditInsert(mock)

		req := &model.ScrubTaintRequest{Kind: "detaint", DetaintMode: model.DetaintModeLift, BeadIDs: []string{"gd-1"}}
		result, err := queryScrubTaint(context.Background(), db, req)
		if err != nil {
			t.Fatalf("queryScrubTaint: %v", err)
		}
		if len(result.Beads) != 1 || result.Beads[0].Skipped {
			t.Fatalf("result = %+v, want one non-skipped bead", result)
		}
	})

	t.Run("empty detaint mode defaults to fork", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
			WithArgs("system", "machine").
			WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(1)))
		mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
			WithArgs("gd-1").
			WillReturnRows(scrubBeadRow("gd-1", "T", "D", "N", `{}`, "open"))
		mock.ExpectExec(`UPDATE beads SET status = 'closed'`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`INSERT INTO mutation_history`).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`INSERT INTO mutation_history`).WillReturnResult(sqlmock.NewResult(2, 1))
		mock.ExpectExec(`INSERT INTO beads`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`INSERT INTO deps`).WillReturnResult(sqlmock.NewResult(0, 1))
		expectTaintAuditInsert(mock)

		req := &model.ScrubTaintRequest{
			Kind: "detaint", BeadIDs: []string{"gd-1"},
			CleanContent: map[string]model.ScrubCleanContent{"gd-1": {Title: "C"}},
		}
		result, err := queryScrubTaint(context.Background(), db, req)
		if err != nil {
			t.Fatalf("queryScrubTaint: %v", err)
		}
		if len(result.Beads) != 1 || result.Beads[0].SuccessorBeadID == "" {
			t.Fatalf("result = %+v, want one bead with a successor (fork default)", result)
		}
	})
}

// TestQueryScrubTaint_PropagatesPerBeadError confirms a single bead's error
// aborts the whole request (the caller's transaction then rolls back
// everything — gd-fVAy32nWDf acceptance criterion 4) rather than silently
// skipping the failing bead. gd-bad names no row in beads at all, live or
// soft-deleted — a genuine data-integrity problem, distinct from the
// soft-deleted-scrub case pinned below (gd-T5P13cSoBw).
func TestQueryScrubTaint_PropagatesPerBeadError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("system", "machine").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-bad").
		WillReturnError(sql.ErrNoRows)

	req := &model.ScrubTaintRequest{Kind: "purge", BeadIDs: []string{"gd-bad"}}
	if _, err := queryScrubTaint(context.Background(), db, req); err == nil {
		t.Fatal("want error propagated from the failing bead, got nil")
	}
}

// TestQueryScrubTaint_ScrubsSoftDeletedBead pins the gd-T5P13cSoBw fix at the
// queryScrubTaint dispatch level: a resolved_bead_ids entry that has since
// been soft-deleted must not permanently fail the whole multi-bead scrub
// request. Per the merge-desk correction, purge does NOT skip a soft-deleted
// bead — soft delete is restorable (ADR taints.md §6.5, §12.E), so a purge
// request scrubs it in place exactly like a live bead, via the same
// unconditional FOR UPDATE lock and full cascade.
func TestQueryScrubTaint_ScrubsSoftDeletedBead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT ` + regexp.QuoteMeta(beadColumns) + ` FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-gone").
		WillReturnRows(scrubBeadRowDeleted("gd-gone", "Secret Title", "Secret Description", "Secret Notes", `{"k":"v"}`, "open"))

	expectBeadTaintLock(mock, "gd-gone", 9, true)
	mock.ExpectExec(`UPDATE beads\s+SET title = \$2, description = \$3, notes = \$4, fields = \$5::jsonb`).
		WithArgs("gd-gone", "[purged]", "", "", []byte(`{"k":""}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WithArgs("gd-gone").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WithArgs("gd-gone", int64(42)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WithArgs("gd-gone", model.SystemActorTaintScrub, int64(42)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WithArgs("gd-gone").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WithArgs("gd-gone").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WithArgs("gd-gone").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WithArgs("gd-gone", int64(9)).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{
		RequestID: "gd-req-gone", TaintID: 9, Kind: "purge", BeadIDs: []string{"gd-gone"},
		Actor: "oidc:carol@example.com", ActorOperatorID: 42,
	}
	result, err := queryScrubTaint(context.Background(), db, req)
	if err != nil {
		t.Fatalf("queryScrubTaint: %v", err)
	}
	if len(result.Beads) != 1 {
		t.Fatalf("result.Beads = %v, want exactly one bead", result.Beads)
	}
	br := result.Beads[0]
	if br.Skipped {
		t.Errorf("bead result = %+v, want Skipped=false — soft-deleted beads are scrubbed, not skipped", br)
	}
	if br.ContentSHA == "" || len(br.TablesTouched) == 0 {
		t.Errorf("bead result = %+v, want non-empty ContentSHA/TablesTouched", br)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected mock expectations: %v", err)
	}
}

// TestPostgresStore_ScrubTaint_CommitsOneTransaction pins the
// PostgresStore.ScrubTaint entry point: it opens exactly one transaction
// spanning every bead and commits once at the end (gd-fVAy32nWDf acceptance
// criterion 4).
func TestPostgresStore_ScrubTaint_CommitsOneTransaction(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("system", "machine").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-1").
		WillReturnRows(scrubBeadRow("gd-1", "T", "D", "N", `{}`, "open"))
	expectBeadTaintLock(mock, "gd-1", 1, true)
	mock.ExpectExec(`UPDATE beads\s+SET title`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)
	mock.ExpectCommit()

	req := &model.ScrubTaintRequest{Kind: "purge", TaintID: 1, BeadIDs: []string{"gd-1"}}
	if _, err := store.ScrubTaint(context.Background(), req); err != nil {
		t.Fatalf("ScrubTaint: %v", err)
	}
}

// TestPostgresStore_ScrubTaint_RollsBackOnError covers the transactional
// guarantee from the other direction: a failure partway through the cascade
// rolls the whole transaction back rather than committing a partial cascade.
func TestPostgresStore_ScrubTaint_RollsBackOnError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("system", "machine").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-1").
		WillReturnRows(scrubBeadRow("gd-1", "T", "D", "N", `{}`, "open"))
	expectBeadTaintLock(mock, "gd-1", 1, true)
	mock.ExpectExec(`UPDATE beads\s+SET title`).
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	req := &model.ScrubTaintRequest{Kind: "purge", TaintID: 1, BeadIDs: []string{"gd-1"}}
	if _, err := store.ScrubTaint(context.Background(), req); err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestTxStore_ScrubTaint confirms the txStore delegate runs against the
// caller's own transaction rather than opening a new one — no
// ExpectBegin/ExpectCommit here, since the caller (e.g. a formula/molecule
// step) already owns that transaction's lifecycle.
func TestTxStore_ScrubTaint(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	sqlTx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	txs := &txStore{tx: sqlTx}

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("system", "machine").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 FOR UPDATE`).
		WithArgs("gd-1").
		WillReturnRows(scrubBeadRow("gd-1", "T", "D", "N", `{}`, "open"))
	expectBeadTaintLock(mock, "gd-1", 1, true)
	mock.ExpectExec(`UPDATE beads\s+SET title`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE comments`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mutation_history`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_embeddings`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE deps SET metadata = ''`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE session_gates`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bead_taints`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectTaintAuditInsert(mock)

	req := &model.ScrubTaintRequest{Kind: "purge", TaintID: 1, BeadIDs: []string{"gd-1"}}
	if _, err := txs.ScrubTaint(context.Background(), req); err != nil {
		t.Fatalf("txStore.ScrubTaint: %v", err)
	}

	mock.ExpectCommit()
	if err := sqlTx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestHashBeadContent covers the content_sha helper's determinism and
// separator-collision resistance (title="ab",description="c" must not equal
// title="a",description="bc").
func TestHashBeadContent(t *testing.T) {
	a := hashBeadContent("ab", "c", "", nil)
	b := hashBeadContent("a", "bc", "", nil)
	if a == b {
		t.Error("hashBeadContent collided across a field boundary — NUL separator missing?")
	}
	h1 := hashBeadContent("x", "y", "z", []byte(`{"a":1}`))
	h2 := hashBeadContent("x", "y", "z", []byte(`{"a":1}`))
	if h1 != h2 {
		t.Error("hashBeadContent is not deterministic for identical input")
	}
}

// TestMergeCloseReason covers the fields-merge helper the fork branch uses:
// close_reason lands without disturbing any pre-existing key, and a nil/
// empty input is tolerated.
func TestMergeCloseReason(t *testing.T) {
	out, err := mergeCloseReason([]byte(`{"other":"keep-me"}`))
	if err != nil {
		t.Fatalf("mergeCloseReason: %v", err)
	}
	if !regexp.MustCompile(`"other"\s*:\s*"keep-me"`).MatchString(string(out)) {
		t.Errorf("out = %s, want it to still carry the pre-existing key", out)
	}
	if !regexp.MustCompile(`"close_reason"\s*:\s*"scrubbed"`).MatchString(string(out)) {
		t.Errorf("out = %s, want close_reason=scrubbed", out)
	}

	empty, err := mergeCloseReason(nil)
	if err != nil {
		t.Fatalf("mergeCloseReason(nil): %v", err)
	}
	if !regexp.MustCompile(`"close_reason"\s*:\s*"scrubbed"`).MatchString(string(empty)) {
		t.Errorf("out = %s, want close_reason=scrubbed", empty)
	}
}
