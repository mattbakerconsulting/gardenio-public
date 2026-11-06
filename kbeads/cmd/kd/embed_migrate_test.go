package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func resetEmbedMigrateFlags(t *testing.T) {
	t.Helper()
	origModel := embedMigrateToModel
	origDims := embedMigrateDims
	origConfirm := embedMigrateConfirm
	origBatch := embedMigrateBatchSize
	origNote := embedMigrateNote
	t.Cleanup(func() {
		embedMigrateToModel = origModel
		embedMigrateDims = origDims
		embedMigrateConfirm = origConfirm
		embedMigrateBatchSize = origBatch
		embedMigrateNote = origNote
	})
	embedMigrateToModel = ""
	embedMigrateDims = 0
	embedMigrateConfirm = false
	embedMigrateBatchSize = 50
	embedMigrateNote = ""
}

func runEmbedMigrateTest(t *testing.T) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	embedMigrateCmd.SetOut(&buf)
	embedMigrateCmd.SetErr(&buf)
	err := runEmbedMigrate(embedMigrateCmd, nil)
	return buf.String(), err
}

func TestEmbedMigrate_RequiresToModel(t *testing.T) {
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateDims = 1024
	embedMigrateConfirm = true

	_, err := runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "--to-model") {
		t.Fatalf("expected --to-model error, got %v", err)
	}
}

func TestEmbedMigrate_RequiresDims(t *testing.T) {
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateToModel = "new-model"
	embedMigrateConfirm = true

	_, err := runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "--dims") {
		t.Fatalf("expected --dims error, got %v", err)
	}
}

func TestEmbedMigrate_RequiresSafetyFlag(t *testing.T) {
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateToModel = "new-model"
	embedMigrateDims = 1024

	_, err := runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "yes-i-understand") {
		t.Fatalf("expected safety-flag error, got %v", err)
	}
}

func TestEmbedMigrate_BatchSizeValidation(t *testing.T) {
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateToModel = "new-model"
	embedMigrateDims = 1024
	embedMigrateConfirm = true
	embedMigrateBatchSize = 0

	_, err := runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "--batch-size") {
		t.Fatalf("expected batch-size error, got %v", err)
	}

	embedMigrateBatchSize = 501
	_, err = runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "--batch-size") {
		t.Fatalf("expected batch-size error, got %v", err)
	}
}

// TestEmbedMigrate_SurfacesServerValidationErrors verifies that server-side
// validation errors (noop backend, model mismatch, dims mismatch) propagate
// through the CLI. The validation itself moved to handleMigrateEmbeddings
// when embed sync became server-side — see server/http_embeddings_test.go
// for the authoritative coverage of each case.
func TestEmbedMigrate_SurfacesServerValidationErrors(t *testing.T) {
	cases := []struct {
		name, serverErr, wantSubstr string
	}{
		{"noop", "embedding disabled (server BEADS_EMBED_BACKEND=noop)", "embedding disabled"},
		{"model", "server embedder reports model \"actual\", migrate target is \"new-model\"", "reports model"},
		{"dims", "server embedder reports 768 dims, migrate target is 1024 dims", "dims"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			mc.MigrateEmbeddingsErr = fmt.Errorf("%s", tc.serverErr)
			withMockClient(t, mc)
			resetEmbedMigrateFlags(t)
			resetEmbedFlags(t)
			embedMigrateToModel = "new-model"
			embedMigrateDims = 1024
			embedMigrateConfirm = true

			_, err := runEmbedMigrateTest(t)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("expected %q in error, got %v", tc.wantSubstr, err)
			}
			// Migrate call is made once (the server is what rejects).
			if len(mc.MigrateEmbeddingsCalls) != 1 {
				t.Fatalf("expected 1 MigrateEmbeddings call, got %d", len(mc.MigrateEmbeddingsCalls))
			}
		})
	}
}

func TestEmbedMigrate_HappyPath(t *testing.T) {
	mc := newMockClient()
	b1 := &model.Bead{ID: "gd-a", Title: "Task A", Description: "alpha"}
	b2 := &model.Bead{ID: "gd-b", Title: "Task B", Description: "beta"}
	mc.Beads = map[string]*model.Bead{b1.ID: b1, b2.ID: b2}
	mc.ListBeadsResult = []*model.Bead{b1, b2}
	// Start with old hashes that would match old text — MigrateEmbeddings clears them.
	mc.EmbeddingHashes = map[string]string{b1.ID: "old1", b2.ID: "old2"}

	mc.SyncBatchModel = "new-model"
	withMockClient(t, mc)
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateToModel = "new-model"
	embedMigrateDims = 1024
	embedMigrateConfirm = true
	embedMigrateBatchSize = 50
	embedMigrateNote = "routine model bump"

	out, err := runEmbedMigrateTest(t)
	if err != nil {
		t.Fatalf("runEmbedMigrate: %v", err)
	}

	if len(mc.MigrateEmbeddingsCalls) != 1 {
		t.Fatalf("expected 1 MigrateEmbeddings call, got %d", len(mc.MigrateEmbeddingsCalls))
	}
	call := mc.MigrateEmbeddingsCalls[0]
	if call.Model != "new-model" || call.Dims != 1024 {
		t.Errorf("MigrateEmbeddings args wrong: %+v", call)
	}
	if call.Actor != "test-actor" {
		t.Errorf("expected actor=test-actor, got %q", call.Actor)
	}
	if call.Note != "routine model bump" {
		t.Errorf("expected note to propagate, got %q", call.Note)
	}

	// Backfill step — force=true means GetEmbeddingHashes must NOT be called.
	if len(mc.GetEmbeddingHashesCalls) != 0 {
		t.Errorf("expected no GetEmbeddingHashes calls (force), got %d", len(mc.GetEmbeddingHashesCalls))
	}
	if len(mc.SyncBatchCalls) != 1 {
		t.Errorf("expected 1 SyncBatch call, got %d", len(mc.SyncBatchCalls))
	} else if !mc.SyncBatchCalls[0].Force {
		t.Errorf("expected SyncBatch with Force=true, got %+v", mc.SyncBatchCalls[0])
	}
	if len(mc.UpsertEmbeddingCalls) != 2 {
		t.Errorf("expected 2 UpsertEmbedding calls, got %d", len(mc.UpsertEmbeddingCalls))
	}
	for _, c := range mc.UpsertEmbeddingCalls {
		if c.Model != "new-model" {
			t.Errorf("upsert model wrong: %q", c.Model)
		}
	}

	if mc.ReindexEmbeddingsCalls != 1 {
		t.Errorf("expected 1 ReindexEmbeddings call, got %d", mc.ReindexEmbeddingsCalls)
	}
	if !strings.Contains(out, "Done") {
		t.Errorf("expected completion line, got:\n%s", out)
	}
}

func TestEmbedMigrate_MigrateEmbeddingsErrorShortCircuits(t *testing.T) {
	mc := newMockClient()
	mc.MigrateEmbeddingsErr = fmt.Errorf("ddl failed")
	b1 := &model.Bead{ID: "gd-a", Title: "Task A"}
	mc.ListBeadsResult = []*model.Bead{b1}

	withMockClient(t, mc)
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateToModel = "new-model"
	embedMigrateDims = 1024
	embedMigrateConfirm = true

	_, err := runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "migrate schema") {
		t.Fatalf("expected migrate schema error, got %v", err)
	}
	if len(mc.SyncBatchCalls) != 0 {
		t.Errorf("sync must not run when migrate fails; got %d SyncBatch calls", len(mc.SyncBatchCalls))
	}
	if mc.ReindexEmbeddingsCalls != 0 {
		t.Errorf("reindex must not run when migrate fails; got %d calls", mc.ReindexEmbeddingsCalls)
	}
}

func TestEmbedMigrate_SyncErrorShortCircuitsReindex(t *testing.T) {
	mc := newMockClient()
	mc.SyncBatchErr = fmt.Errorf("embedder call failed: ollama down")
	b1 := &model.Bead{ID: "gd-a", Title: "Task A"}
	mc.ListBeadsResult = []*model.Bead{b1}

	withMockClient(t, mc)
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateToModel = "new-model"
	embedMigrateDims = 1024
	embedMigrateConfirm = true

	_, err := runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "backfill after migrate") {
		t.Fatalf("expected backfill error, got %v", err)
	}
	if len(mc.MigrateEmbeddingsCalls) != 1 {
		t.Errorf("expected 1 MigrateEmbeddings call, got %d", len(mc.MigrateEmbeddingsCalls))
	}
	if mc.ReindexEmbeddingsCalls != 0 {
		t.Errorf("reindex must not run when sync fails; got %d", mc.ReindexEmbeddingsCalls)
	}
}

func TestEmbedMigrate_ReindexErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.ReindexEmbeddingsErr = fmt.Errorf("index failed")
	b1 := &model.Bead{ID: "gd-a", Title: "Task A"}
	mc.ListBeadsResult = []*model.Bead{b1}

	withMockClient(t, mc)
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedMigrateToModel = "new-model"
	embedMigrateDims = 1024
	embedMigrateConfirm = true

	_, err := runEmbedMigrateTest(t)
	if err == nil || !strings.Contains(err.Error(), "reindex") {
		t.Fatalf("expected reindex error, got %v", err)
	}
	if len(mc.MigrateEmbeddingsCalls) != 1 {
		t.Errorf("expected MigrateEmbeddings to run, got %d calls", len(mc.MigrateEmbeddingsCalls))
	}
	if len(mc.SyncBatchCalls) != 1 {
		t.Errorf("expected sync to run, got %d SyncBatch calls", len(mc.SyncBatchCalls))
	}
	if mc.ReindexEmbeddingsCalls != 1 {
		t.Errorf("expected 1 ReindexEmbeddings call, got %d", mc.ReindexEmbeddingsCalls)
	}
}

func TestEmbedMigrate_RestoresSyncFlagsAfter(t *testing.T) {
	mc := newMockClient()
	b1 := &model.Bead{ID: "gd-a", Title: "Task A"}
	mc.ListBeadsResult = []*model.Bead{b1}

	withMockClient(t, mc)
	resetEmbedMigrateFlags(t)
	resetEmbedFlags(t)
	embedSyncBatchSize = 111
	embedSyncForce = false
	embedMigrateToModel = "new-model"
	embedMigrateDims = 1024
	embedMigrateConfirm = true
	embedMigrateBatchSize = 25

	if _, err := runEmbedMigrateTest(t); err != nil {
		t.Fatalf("runEmbedMigrate: %v", err)
	}
	// Sync flags must be restored to their pre-call values.
	if embedSyncBatchSize != 111 {
		t.Errorf("expected embedSyncBatchSize restored to 111, got %d", embedSyncBatchSize)
	}
	if embedSyncForce != false {
		t.Errorf("expected embedSyncForce restored to false, got %v", embedSyncForce)
	}
}
