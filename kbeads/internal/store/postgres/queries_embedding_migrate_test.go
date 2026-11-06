package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestQueryMigrateEmbeddingsSchema_HappyPath(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectExec("DROP INDEX IF EXISTS " + annIndexName).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("TRUNCATE TABLE bead_embeddings").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ALTER TABLE bead_embeddings ALTER COLUMN embedding TYPE vector\(1024\)`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO bead_embedding_migrations").
		WithArgs("mxbai-embed-large", 1024, "alice", "model swap").
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := queryMigrateEmbeddingsSchema(context.Background(), db,
		"mxbai-embed-large", 1024, "alice", "model swap",
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestQueryMigrateEmbeddingsSchema_RejectsEmptyModel(t *testing.T) {
	db, _ := newMockDB(t)
	err := queryMigrateEmbeddingsSchema(context.Background(), db, "", 1024, "alice", "")
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("expected model-required error, got %v", err)
	}
}

func TestQueryMigrateEmbeddingsSchema_RejectsBadDims(t *testing.T) {
	db, _ := newMockDB(t)
	err := queryMigrateEmbeddingsSchema(context.Background(), db, "m", 0, "a", "")
	if err == nil || !strings.Contains(err.Error(), "dims must be > 0") {
		t.Fatalf("expected dims error, got %v", err)
	}
	err = queryMigrateEmbeddingsSchema(context.Background(), db, "m", -5, "a", "")
	if err == nil || !strings.Contains(err.Error(), "dims must be > 0") {
		t.Fatalf("expected dims error, got %v", err)
	}
}

func TestQueryMigrateEmbeddingsSchema_DropIndexError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DROP INDEX IF EXISTS " + annIndexName).
		WillReturnError(errors.New("pq: permission denied"))

	err := queryMigrateEmbeddingsSchema(context.Background(), db, "m", 1024, "a", "")
	if err == nil || !strings.Contains(err.Error(), "drop ann index") {
		t.Fatalf("expected drop ann index error, got %v", err)
	}
}

func TestQueryMigrateEmbeddingsSchema_TruncateError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DROP INDEX IF EXISTS " + annIndexName).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("TRUNCATE TABLE bead_embeddings").
		WillReturnError(errors.New("pq: locked"))

	err := queryMigrateEmbeddingsSchema(context.Background(), db, "m", 1024, "a", "")
	if err == nil || !strings.Contains(err.Error(), "truncate bead_embeddings") {
		t.Fatalf("expected truncate error, got %v", err)
	}
}

func TestQueryMigrateEmbeddingsSchema_AlterError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DROP INDEX IF EXISTS " + annIndexName).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("TRUNCATE TABLE bead_embeddings").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ALTER TABLE bead_embeddings ALTER COLUMN embedding TYPE vector\(1024\)`).
		WillReturnError(errors.New("pq: type mismatch"))

	err := queryMigrateEmbeddingsSchema(context.Background(), db, "m", 1024, "a", "")
	if err == nil || !strings.Contains(err.Error(), "alter embedding column") {
		t.Fatalf("expected alter error, got %v", err)
	}
}

func TestQueryMigrateEmbeddingsSchema_AuditInsertError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DROP INDEX IF EXISTS " + annIndexName).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("TRUNCATE TABLE bead_embeddings").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ALTER TABLE bead_embeddings ALTER COLUMN embedding TYPE vector\(1024\)`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO bead_embedding_migrations").
		WillReturnError(errors.New("pq: table missing"))

	err := queryMigrateEmbeddingsSchema(context.Background(), db, "m", 1024, "a", "")
	if err == nil || !strings.Contains(err.Error(), "record embedding migration") {
		t.Fatalf("expected record-migration error, got %v", err)
	}
}

func TestQueryRecreateANNIndex(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("CREATE INDEX IF NOT EXISTS " + annIndexName +
		" ON bead_embeddings USING ivfflat").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := queryRecreateANNIndex(context.Background(), db); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestQueryRecreateANNIndex_Error(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("CREATE INDEX IF NOT EXISTS " + annIndexName).
		WillReturnError(errors.New("pq: disk full"))

	err := queryRecreateANNIndex(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "create ann index") {
		t.Fatalf("expected create ann index error, got %v", err)
	}
}
