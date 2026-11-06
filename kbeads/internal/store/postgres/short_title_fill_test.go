package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"gardenio/kbeads/internal/model"
)

// queryFillShortTitle (gd-JOOXA7UqLh): the emptiness check is in the UPDATE's
// own WHERE, and only short_title and updated_at are written.
func TestQueryFillShortTitle(t *testing.T) {
	const stmt = `UPDATE beads SET short_title = $2, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL AND btrim(COALESCE(short_title, '')) = '' RETURNING updated_at`
	sqlRe := regexp.QuoteMeta(stmt)
	// Collapse the query's indentation so the expectation matches it.
	normalize := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		return sqlmock.QueryMatcherRegexp.Match(expected, strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(actual, " ")))
	})

	t.Run("empty: written", func(t *testing.T) {
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(normalize))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
		mock.ExpectQuery(sqlRe).WithArgs("gd-t1", "name the bead").
			WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(now))
		b := &model.Bead{ID: "gd-t1", ShortTitle: "name the bead"}
		ok, err := queryFillShortTitle(context.Background(), db, b)
		if err != nil || !ok {
			t.Fatalf("fill = %v, %v; want true, nil", ok, err)
		}
		if !b.UpdatedAt.Equal(now) {
			t.Errorf("UpdatedAt = %v, want %v", b.UpdatedAt, now)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("already set: no row, not an error", func(t *testing.T) {
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(normalize))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(sqlRe).WithArgs("gd-t1", "name the bead").WillReturnError(sql.ErrNoRows)
		ok, err := queryFillShortTitle(context.Background(), db, &model.Bead{ID: "gd-t1", ShortTitle: "name the bead"})
		if err != nil || ok {
			t.Fatalf("fill = %v, %v; want false, nil", ok, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}
