package postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// beadRowWithFields builds a single mock bead row (base columns) carrying the
// given raw JSON fields blob — the RETURNING/read shape the sub-field primitives
// scan.
func beadRowWithFields(fields string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(beadRowColumnsBase).AddRow(
		"gd-proj", nil, nil, nil, "config", "project", "Project: gardenio", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
		[]byte(fields), "default", nil, nil, nil,
	)
}

// TestDecodeStringMapField covers the two real storage shapes (native jsonb
// object and stringified-JSON string, incl. a trailing newline) plus the empty
// and malformed cases.
func TestDecodeStringMapField(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{"native object", `{"C1":"ops","C2":"crew"}`, map[string]string{"C1": "ops", "C2": "crew"}, false},
		{"stringified json", `"{\"C1\":\"ops\"}"`, map[string]string{"C1": "ops"}, false},
		{"stringified json trailing newline", `"{\"C1\":\"ops\"}\n"`, map[string]string{"C1": "ops"}, false},
		{"empty raw", ``, map[string]string{}, false},
		{"empty string", `""`, map[string]string{}, false},
		{"malformed", `"not json"`, nil, true},
		{"number", `42`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeStringMapField([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got[%q]=%q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestDecodeCSVField covers the CSV-string shape, the defensive array shape,
// trimming/empty-dropping, and the empty/malformed cases.
func TestDecodeCSVField(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{"csv string", `"C1,C2,C3"`, []string{"C1", "C2", "C3"}, false},
		{"csv with spaces", `"C1, C2 ,C3"`, []string{"C1", "C2", "C3"}, false},
		{"csv trailing empties", `"C1,,C2,"`, []string{"C1", "C2"}, false},
		{"array shape", `["C1","C2"]`, []string{"C1", "C2"}, false},
		{"empty raw", ``, nil, false},
		{"empty string", `""`, nil, false},
		{"malformed", `42`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCSVField([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d]=%q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestQueryMergeBeadMapField_RejectsInvalidFieldKey guards the field-key
// validation before any query runs (no SQL expectations set).
func TestQueryMergeBeadMapField_RejectsInvalidFieldKey(t *testing.T) {
	db, _ := newMockDB(t)
	_, _, err := queryMergeBeadMapField(context.Background(), db, "gd-proj", "bad key", SubFieldOpSet, "C1", "ops")
	if !errors.Is(err, store.ErrInvalidFieldKey) {
		t.Fatalf("want ErrInvalidFieldKey, got %v", err)
	}
}

// TestQueryMergeBeadMapField_RejectsEmptyKey and unknown op are ErrInvalidSubFieldEdit.
func TestQueryMergeBeadMapField_RejectsBadInput(t *testing.T) {
	db, _ := newMockDB(t)
	if _, _, err := queryMergeBeadMapField(context.Background(), db, "gd-proj", "channel_roles", SubFieldOpSet, "", "ops"); !errors.Is(err, store.ErrInvalidSubFieldEdit) {
		t.Fatalf("empty key: want ErrInvalidSubFieldEdit, got %v", err)
	}
}

// TestQueryMergeBeadMapField_SetPreservesOtherKeys is the headline acceptance
// case: setting one key of a map with pre-existing keys leaves the others
// intact. The mock returns the locked row, then expects the UPDATE to carry a
// fields blob that still contains BOTH the old and the new key.
func TestQueryMergeBeadMapField_SetPreservesOtherKeys(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COALESCE\(fields, '\{\}'::jsonb\) FROM beads WHERE id = \$1 AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("gd-proj").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).AddRow([]byte(`{"channel_roles":{"C1":"ops"},"prefix":"gb"}`)))

	// The write must carry a merged map with BOTH keys; assert via an argument
	// matcher that checks the serialized blob.
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb, updated_at = NOW\(\)`).
		WithArgs("gd-proj", fieldsContain(t, `"C1":"ops"`, `"C2":"crew"`, `"prefix":"gb"`)).
		WillReturnRows(beadRowWithFields(`{"channel_roles":{"C1":"ops","C2":"crew"},"prefix":"gb"}`))

	bead, changed, err := queryMergeBeadMapField(context.Background(), db, "gd-proj", "channel_roles", SubFieldOpSet, "C2", "crew")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
	if bead == nil || bead.ID != "gd-proj" {
		t.Errorf("bead = %+v, want gd-proj", bead)
	}
}

// TestQueryMergeBeadMapField_SetSameValueIsNoOp verifies an idempotent no-op:
// setting a key to its current value does not issue an UPDATE, only a read-back.
func TestQueryMergeBeadMapField_SetSameValueIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-proj").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).AddRow([]byte(`{"channel_roles":{"C1":"ops"}}`)))
	// No UPDATE — just the base-column read-back for the no-op return.
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("gd-proj").
		WillReturnRows(beadRowWithFields(`{"channel_roles":{"C1":"ops"}}`))

	_, changed, err := queryMergeBeadMapField(context.Background(), db, "gd-proj", "channel_roles", SubFieldOpSet, "C1", "ops")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false (set to current value is a no-op)")
	}
}

// TestQueryMergeBeadMapField_UnsetKeyPreservesOthers verifies deleting one key
// leaves the others.
func TestQueryMergeBeadMapField_UnsetKeyPreservesOthers(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-proj").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).AddRow([]byte(`{"channel_roles":{"C1":"ops","C2":"crew"}}`)))
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb`).
		WithArgs("gd-proj", fieldsContainNot(t, `"C1"`)).
		WillReturnRows(beadRowWithFields(`{"channel_roles":{"C2":"crew"}}`))

	_, changed, err := queryMergeBeadMapField(context.Background(), db, "gd-proj", "channel_roles", SubFieldOpUnset, "C1", "")
	if err != nil {
		t.Fatalf("unset: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
}

// TestQueryMergeBeadMapField_UnsetAbsentKeyIsNoOp verifies unsetting a key that
// isn't there does not write.
func TestQueryMergeBeadMapField_UnsetAbsentKeyIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-proj").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).AddRow([]byte(`{"channel_roles":{"C1":"ops"}}`)))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("gd-proj").
		WillReturnRows(beadRowWithFields(`{"channel_roles":{"C1":"ops"}}`))

	_, changed, err := queryMergeBeadMapField(context.Background(), db, "gd-proj", "channel_roles", SubFieldOpUnset, "C9", "")
	if err != nil {
		t.Fatalf("unset: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false")
	}
}

// TestQueryEditBeadCSVField_AppendPreservesExisting is the CSV acceptance case:
// appending one element to a multi-entry CSV preserves the existing entries.
func TestQueryEditBeadCSVField_AppendPreservesExisting(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-proj").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).AddRow([]byte(`{"slack_channel":"C1,C2"}`)))
	// The written CSV must contain all three.
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb`).
		WithArgs("gd-proj", fieldsContain(t, `"slack_channel":"C1,C2,C3"`)).
		WillReturnRows(beadRowWithFields(`{"slack_channel":"C1,C2,C3"}`))

	_, changed, err := queryEditBeadCSVField(context.Background(), db, "gd-proj", "slack_channel", SubFieldOpAppend, "C3")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
}

// TestQueryEditBeadCSVField_AppendPresentIsNoOp verifies appending an element
// already in the CSV is idempotent.
func TestQueryEditBeadCSVField_AppendPresentIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-proj").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).AddRow([]byte(`{"slack_channel":"C1,C2"}`)))
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("gd-proj").
		WillReturnRows(beadRowWithFields(`{"slack_channel":"C1,C2"}`))

	_, changed, err := queryEditBeadCSVField(context.Background(), db, "gd-proj", "slack_channel", SubFieldOpAppend, "C2")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false (element already present)")
	}
}

// TestQueryEditBeadCSVField_RemoveDropsOnlyTarget verifies remove drops only the
// named element, preserving the rest.
func TestQueryEditBeadCSVField_RemoveDropsOnlyTarget(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-proj").
		WillReturnRows(sqlmock.NewRows([]string{"fields"}).AddRow([]byte(`{"slack_channel":"C1,C2,C3"}`)))
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb`).
		WithArgs("gd-proj", fieldsContain(t, `"slack_channel":"C1,C3"`)).
		WillReturnRows(beadRowWithFields(`{"slack_channel":"C1,C3"}`))

	_, changed, err := queryEditBeadCSVField(context.Background(), db, "gd-proj", "slack_channel", SubFieldOpRemove, "C2")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
}

// TestQueryEditBeadCSVField_RejectsCommaElement guards CSV integrity: an element
// containing a comma would corrupt the CSV, so it is rejected before any query.
func TestQueryEditBeadCSVField_RejectsCommaElement(t *testing.T) {
	db, _ := newMockDB(t)
	if _, _, err := queryEditBeadCSVField(context.Background(), db, "gd-proj", "slack_channel", SubFieldOpAppend, "C1,C2"); !errors.Is(err, store.ErrInvalidSubFieldEdit) {
		t.Fatalf("want ErrInvalidSubFieldEdit for comma element, got %v", err)
	}
}

// TestQueryEditBeadCSVField_RejectsEmptyElement guards against an empty element.
func TestQueryEditBeadCSVField_RejectsEmptyElement(t *testing.T) {
	db, _ := newMockDB(t)
	if _, _, err := queryEditBeadCSVField(context.Background(), db, "gd-proj", "slack_channel", SubFieldOpAppend, "   "); !errors.Is(err, store.ErrInvalidSubFieldEdit) {
		t.Fatalf("want ErrInvalidSubFieldEdit for empty element, got %v", err)
	}
}

// fieldsContain returns a sqlmock argument matcher that asserts the string arg
// contains every substring. Used to check the serialized fields blob written by
// the UPDATE preserves untouched keys.
func fieldsContain(t *testing.T, subs ...string) sqlmock.Argument {
	t.Helper()
	return argMatch(func(s string) bool {
		for _, sub := range subs {
			if !strings.Contains(s, sub) {
				t.Errorf("written fields %q missing %q", s, sub)
				return false
			}
		}
		return true
	})
}

// fieldsContainNot asserts the string arg does NOT contain any of the substrings.
func fieldsContainNot(t *testing.T, subs ...string) sqlmock.Argument {
	t.Helper()
	return argMatch(func(s string) bool {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				t.Errorf("written fields %q must not contain %q", s, sub)
				return false
			}
		}
		return true
	})
}

// argMatch adapts a string predicate to a sqlmock.Argument, decoding both the
// string and []byte driver representations of the bound jsonb parameter.
type argMatch func(string) bool

func (f argMatch) Match(v driver.Value) bool {
	switch s := v.(type) {
	case string:
		return f(s)
	case []byte:
		return f(string(s))
	default:
		return false
	}
}
