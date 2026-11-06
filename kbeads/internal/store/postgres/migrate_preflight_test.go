package postgres

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestFindCreateExtensions(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "if-not-exists-lowercase",
			sql:  "CREATE EXTENSION IF NOT EXISTS vector;",
			want: []string{"vector"},
		},
		{
			name: "case-insensitive-keyword",
			sql:  "create extension pgcrypto;",
			want: []string{"pgcrypto"},
		},
		{
			name: "quoted-name-with-hyphen",
			sql:  `CREATE EXTENSION "uuid-ossp";`,
			want: []string{"uuid-ossp"},
		},
		{
			name: "quoted-plain-name",
			sql:  `CREATE EXTENSION "vector";`,
			want: []string{"vector"},
		},
		{
			name: "multiple-in-one-file",
			sql: `CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION pgcrypto;`,
			want: []string{"vector", "pgcrypto"},
		},
		{
			name: "dedupe-within-file",
			sql: `CREATE EXTENSION vector;
CREATE EXTENSION IF NOT EXISTS vector;`,
			want: []string{"vector"},
		},
		{
			name: "inline-comment-stripped",
			sql:  `CREATE TABLE x (...); -- CREATE EXTENSION ignored`,
			want: nil,
		},
		{
			name: "comment-line-ignored",
			sql:  `-- CREATE EXTENSION vector;`,
			want: nil,
		},
		{
			name: "no-directive",
			sql: `CREATE TABLE beads (id text primary key);
ALTER TABLE beads ADD COLUMN notes text;`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findCreateExtensions([]byte(tc.sql))
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestScanRequiredExtensions_SkipsAppliedVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/001_initial.up.sql":     &fstest.MapFile{Data: []byte("CREATE EXTENSION pgcrypto;")},
		"migrations/005_embeddings.up.sql":  &fstest.MapFile{Data: []byte("CREATE EXTENSION IF NOT EXISTS vector;")},
		"migrations/006_extra.up.sql":       &fstest.MapFile{Data: []byte("-- no extensions here")},
		"migrations/005_embeddings.down.sql": &fstest.MapFile{Data: []byte("-- ignored")},
	}

	// DB is at version 4 — migration 005's extension must be flagged, 001 must not.
	got, err := scanRequiredExtensions(fsys, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got["pgcrypto"]; ok {
		t.Error("pgcrypto (migration 001) should be skipped when currentVersion=4")
	}
	if files, ok := got["vector"]; !ok || len(files) != 1 || files[0] != "005_embeddings.up.sql" {
		t.Errorf("expected vector required by 005_embeddings.up.sql; got %v", got)
	}
}

func TestPreflightExtensions_AllAvailable(t *testing.T) {
	db, mock := newMockDB(t)
	fsys := fstest.MapFS{
		"migrations/005_embeddings.up.sql": &fstest.MapFile{Data: []byte("CREATE EXTENSION IF NOT EXISTS vector;")},
	}
	mock.ExpectQuery("SELECT default_version FROM pg_available_extensions WHERE name = \\$1").
		WithArgs("vector").
		WillReturnRows(sqlmock.NewRows([]string{"default_version"}).AddRow("0.8.2"))

	if err := preflightExtensions(db, fsys, 4); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestPreflightExtensions_MissingExtension(t *testing.T) {
	db, mock := newMockDB(t)
	fsys := fstest.MapFS{
		"migrations/005_embeddings.up.sql": &fstest.MapFile{Data: []byte("CREATE EXTENSION IF NOT EXISTS vector;")},
	}
	mock.ExpectQuery("SELECT default_version FROM pg_available_extensions WHERE name = \\$1").
		WithArgs("vector").
		WillReturnRows(sqlmock.NewRows([]string{"default_version"})) // empty — extension absent

	err := preflightExtensions(db, fsys, 4)
	if err == nil {
		t.Fatal("expected error when extension is absent")
	}
	msg := err.Error()
	for _, want := range []string{"vector", "005_embeddings.up.sql", "pgvector/pgvector:pg17"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to contain %q; got %q", want, msg)
		}
	}
}

func TestPreflightExtensions_NoPendingMigrations(t *testing.T) {
	db, _ := newMockDB(t)
	fsys := fstest.MapFS{
		"migrations/005_embeddings.up.sql": &fstest.MapFile{Data: []byte("CREATE EXTENSION IF NOT EXISTS vector;")},
	}
	// DB already past version 5 — no query should fire (mock will ExpectationsWereMet check zero queries).
	if err := preflightExtensions(db, fsys, 10); err != nil {
		t.Fatalf("expected no-op success, got %v", err)
	}
}

// TestPreflightExtensions_AgainstRealMigrations exercises the production
// migrationsFS (embed) against a mocked DB that claims pgvector is present.
// This guards against the preflight silently failing due to a filesystem
// reading bug — if someone moves the migrations dir, this test catches it.
func TestPreflightExtensions_AgainstRealMigrations(t *testing.T) {
	db, mock := newMockDB(t)
	// Current version 0 — all migrations count as pending. The real migrations
	// declare two extensions: pg_trgm (migration 044, trgm indexes) and vector
	// (migration 005, pgvector). preflightExtensions sorts the names, so it
	// queries pg_trgm before vector.
	mock.ExpectQuery("SELECT default_version FROM pg_available_extensions WHERE name = \\$1").
		WithArgs("pg_trgm").
		WillReturnRows(sqlmock.NewRows([]string{"default_version"}).AddRow("1.6"))
	mock.ExpectQuery("SELECT default_version FROM pg_available_extensions WHERE name = \\$1").
		WithArgs("vector").
		WillReturnRows(sqlmock.NewRows([]string{"default_version"}).AddRow("0.8.2"))

	if err := preflightExtensions(db, migrationsFS, 0); err != nil {
		t.Fatalf("expected success against real migrations with pg_trgm + pgvector available; got %v", err)
	}
}
