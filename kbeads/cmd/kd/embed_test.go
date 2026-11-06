package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/model"
)

// fakeEmbedder is a deterministic Embedder used in CLI tests.
type fakeEmbedder struct {
	model     string
	dims      int
	embedCall int
	batchCall int
	lastTexts []string
	err       error
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.embedCall++
	return make([]float32, f.dims), f.err
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	f.batchCall++
	f.lastTexts = append([]string(nil), texts...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		vec := make([]float32, f.dims)
		for j := range vec {
			vec[j] = float32(i) + float32(j)/10
		}
		out[i] = vec
	}
	return out, nil
}

func (f *fakeEmbedder) ModelName() string { return f.model }
func (f *fakeEmbedder) Dimensions() int   { return f.dims }

// withFakeEmbedder swaps embedderFactory for the duration of a test.
func withFakeEmbedder(t *testing.T, e embedding.Embedder) {
	t.Helper()
	orig := embedderFactory
	t.Cleanup(func() { embedderFactory = orig })
	embedderFactory = func() (embedding.Embedder, error) { return e, nil }
}

// resetEmbedFlags resets the package-level flag vars to defaults.
func resetEmbedFlags(t *testing.T) {
	t.Helper()
	origBatch := embedSyncBatchSize
	origForce := embedSyncForce
	origOnlyMissing := embedSyncOnlyMissing
	origTypes := embedSyncTypes
	origExclude := embedSyncExcludeTypes
	origLimit := embedSyncLimit
	t.Cleanup(func() {
		embedSyncBatchSize = origBatch
		embedSyncForce = origForce
		embedSyncOnlyMissing = origOnlyMissing
		embedSyncTypes = origTypes
		embedSyncExcludeTypes = origExclude
		embedSyncLimit = origLimit
	})
	embedSyncBatchSize = 50
	embedSyncForce = false
	embedSyncOnlyMissing = false
	embedSyncTypes = ""
	embedSyncExcludeTypes = ""
	embedSyncLimit = 0
	t.Setenv("BEADS_EMBED_TYPES", "")
	t.Setenv("BEADS_EMBED_EXCLUDE_TYPES", "")
}

func runEmbedSyncTest(t *testing.T) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	embedSyncCmd.SetOut(&buf)
	embedSyncCmd.SetErr(&buf)
	err := runEmbedSync(embedSyncCmd, nil)
	return buf.String(), err
}

func TestEmbedSync_BatchSizeValidation(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	withFakeEmbedder(t, &fakeEmbedder{model: "test-model", dims: 4})
	resetEmbedFlags(t)

	embedSyncBatchSize = 0
	if _, err := runEmbedSyncTest(t); err == nil || !strings.Contains(err.Error(), ">= 1") {
		t.Fatalf("expected batch-size >= 1 error, got %v", err)
	}

	embedSyncBatchSize = 501
	if _, err := runEmbedSyncTest(t); err == nil || !strings.Contains(err.Error(), "<= 500") {
		t.Fatalf("expected batch-size <= 500 error, got %v", err)
	}
}

// TestEmbedSync_SurfacesServerDisabled verifies the CLI propagates a
// server-side "embedding disabled" error (the server returns 503 when its
// configured embedder is nil or noop). Validation moved to the server when
// sync-batch became server-side.
func TestEmbedSync_SurfacesServerDisabled(t *testing.T) {
	mc := newMockClient()
	mc.SyncBatchErr = fmt.Errorf("embedding disabled (server BEADS_EMBED_BACKEND=noop)")
	withMockClient(t, mc)
	resetEmbedFlags(t)

	_, err := runEmbedSyncTest(t)
	if err == nil || !strings.Contains(err.Error(), "embedding disabled") {
		t.Fatalf("expected embedding-disabled error, got %v", err)
	}
	if len(mc.SyncBatchCalls) != 1 {
		t.Fatalf("expected 1 SyncBatch call, got %d", len(mc.SyncBatchCalls))
	}
}

func TestEmbedSync_HappyPath(t *testing.T) {
	mc := newMockClient()
	b1 := &model.Bead{ID: "gd-a", Title: "Task A", Description: "alpha"}
	b2 := &model.Bead{ID: "gd-b", Title: "Task B", Description: "beta"}
	mc.Beads = map[string]*model.Bead{b1.ID: b1, b2.ID: b2}
	mc.ListBeadsResult = []*model.Bead{b1, b2}
	// No existing hashes -> both beads should be embedded server-side.
	mc.EmbeddingHashes = map[string]string{}

	withMockClient(t, mc)
	resetEmbedFlags(t)

	out, err := runEmbedSyncTest(t)
	if err != nil {
		t.Fatalf("runEmbedSync: %v", err)
	}
	if len(mc.SyncBatchCalls) != 1 {
		t.Fatalf("expected 1 SyncBatch call, got %d", len(mc.SyncBatchCalls))
	}
	if mc.SyncBatchCalls[0].Offset != 0 || mc.SyncBatchCalls[0].Limit != 50 {
		t.Errorf("unexpected SyncBatch args: %+v", mc.SyncBatchCalls[0])
	}
	if len(mc.UpsertEmbeddingCalls) != 2 {
		t.Fatalf("expected 2 synthesized UpsertEmbedding rows, got %d", len(mc.UpsertEmbeddingCalls))
	}
	for _, c := range mc.UpsertEmbeddingCalls {
		if c.Model != "test-model" {
			t.Errorf("expected model=test-model, got %q", c.Model)
		}
		if c.TextHash == "" {
			t.Errorf("expected non-empty TextHash for %s", c.BeadID)
		}
	}
	if !strings.Contains(out, "Embedded 2") {
		t.Errorf("expected progress line mentioning embedded count, got:\n%s", out)
	}
}

func TestEmbedSync_SkipUnchanged(t *testing.T) {
	mc := newMockClient()
	b1 := &model.Bead{ID: "gd-a", Title: "Task A", Description: "alpha"}
	b2 := &model.Bead{ID: "gd-b", Title: "Task B", Description: "beta"}
	mc.ListBeadsResult = []*model.Bead{b1, b2}

	// Pre-populate matching hashes so the server would skip both.
	text1 := embedding.BeadText(b1)
	text2 := embedding.BeadText(b2)
	mc.EmbeddingHashes = map[string]string{
		b1.ID: embedding.TextHash(text1),
		b2.ID: embedding.TextHash(text2),
	}

	withMockClient(t, mc)
	resetEmbedFlags(t)

	out, err := runEmbedSyncTest(t)
	if err != nil {
		t.Fatalf("runEmbedSync: %v", err)
	}
	if len(mc.UpsertEmbeddingCalls) != 0 {
		t.Fatalf("expected 0 UpsertEmbedding rows, got %d", len(mc.UpsertEmbeddingCalls))
	}
	if !strings.Contains(out, "skipped 2") {
		t.Errorf("expected 'skipped 2' in output, got:\n%s", out)
	}
}

func TestEmbedSync_ForceReEmbedsAll(t *testing.T) {
	mc := newMockClient()
	b1 := &model.Bead{ID: "gd-a", Title: "Task A", Description: "alpha"}
	mc.ListBeadsResult = []*model.Bead{b1}
	// Matching hashes that would normally skip.
	text := embedding.BeadText(b1)
	mc.EmbeddingHashes = map[string]string{b1.ID: embedding.TextHash(text)}

	withMockClient(t, mc)
	resetEmbedFlags(t)
	embedSyncForce = true

	if _, err := runEmbedSyncTest(t); err != nil {
		t.Fatalf("runEmbedSync: %v", err)
	}
	if len(mc.SyncBatchCalls) != 1 || !mc.SyncBatchCalls[0].Force {
		t.Fatalf("expected SyncBatch call with Force=true, got %+v", mc.SyncBatchCalls)
	}
	if len(mc.UpsertEmbeddingCalls) != 1 {
		t.Fatalf("expected 1 UpsertEmbedding row with --force, got %d", len(mc.UpsertEmbeddingCalls))
	}
	// With --force, the server-side handler skips the hash check — the
	// mock mirrors that behavior and should not have recorded a
	// GetEmbeddingHashes call.
	if len(mc.GetEmbeddingHashesCalls) != 0 {
		t.Fatalf("expected no GetEmbeddingHashes calls with --force, got %d", len(mc.GetEmbeddingHashesCalls))
	}
}

// TestEmbedderFactory_NoDatabaseURLRequired guards against a regression where
// `kd embed sync` (and `embed migrate`) called config.Load() and failed with
// "BEADS_DATABASE_URL is required" in environments — like the e2e harness —
// where the CLI talks to the daemon over HTTP and never opens a DB itself.
func TestEmbedderFactory_NoDatabaseURLRequired(t *testing.T) {
	t.Setenv("BEADS_DATABASE_URL", "")
	t.Setenv("BEADS_EMBED_BACKEND", "openai")
	t.Setenv("BEADS_EMBED_URL", "http://localhost:4000")
	t.Setenv("BEADS_EMBED_MODEL", "nomic-embed-text")

	emb, err := embedderFactory()
	if err != nil {
		t.Fatalf("embedderFactory() err = %v, want nil (DB URL must not be required)", err)
	}
	if emb == nil {
		t.Fatalf("embedderFactory() returned nil embedder")
	}
	if emb.ModelName() != "nomic-embed-text" {
		t.Errorf("ModelName() = %q, want %q", emb.ModelName(), "nomic-embed-text")
	}
}

// TestEmbedSync_OnlyMissingFlagPropagates verifies the --only-missing flag
// flows through to the SyncBatchEmbeddings request. This is the "fast path"
// added for ka-0000000728: server SQL-pre-filters to beads needing embedding
// instead of walking every bead and skipping client-side.
func TestEmbedSync_OnlyMissingFlagPropagates(t *testing.T) {
	mc := newMockClient()
	b1 := &model.Bead{ID: "gd-a", Title: "Task A", Description: "alpha"}
	b2 := &model.Bead{ID: "gd-b", Title: "Task B", Description: "beta"}
	mc.ListBeadsResult = []*model.Bead{b1, b2}
	// b1 already embedded with matching hash; only b2 is "missing".
	mc.EmbeddingHashes = map[string]string{
		b1.ID: embedding.TextHash(embedding.BeadText(b1)),
	}

	withMockClient(t, mc)
	resetEmbedFlags(t)
	embedSyncOnlyMissing = true

	if _, err := runEmbedSyncTest(t); err != nil {
		t.Fatalf("runEmbedSync: %v", err)
	}
	if len(mc.SyncBatchCalls) != 1 {
		t.Fatalf("expected 1 SyncBatch call, got %d", len(mc.SyncBatchCalls))
	}
	if !mc.SyncBatchCalls[0].OnlyMissing {
		t.Errorf("expected OnlyMissing=true on request, got %+v", mc.SyncBatchCalls[0])
	}
	if len(mc.UpsertEmbeddingCalls) != 1 {
		t.Fatalf("expected 1 UpsertEmbedding (fresh bead only), got %d", len(mc.UpsertEmbeddingCalls))
	}
	if mc.UpsertEmbeddingCalls[0].BeadID != b2.ID {
		t.Errorf("expected upsert for %s, got %s", b2.ID, mc.UpsertEmbeddingCalls[0].BeadID)
	}
}

// TestEmbedSync_OnlyMissingDoesNotAdvanceOffset verifies the loop fix: when
// --only-missing is set, each successful page removes those beads from the
// pending set, so offset must stay at 0. Advancing offset would skip beads
// that still need embedding.
func TestEmbedSync_OnlyMissingDoesNotAdvanceOffset(t *testing.T) {
	mc := newMockClient()
	// Three beads, none embedded — with batch-size=2 the loop should make
	// two calls (page of 2, then page of 1) both at offset=0.
	b1 := &model.Bead{ID: "gd-a", Title: "A"}
	b2 := &model.Bead{ID: "gd-b", Title: "B"}
	b3 := &model.Bead{ID: "gd-c", Title: "C"}
	mc.ListBeadsResult = []*model.Bead{b1, b2, b3}
	mc.EmbeddingHashes = map[string]string{}

	withMockClient(t, mc)
	resetEmbedFlags(t)
	embedSyncOnlyMissing = true
	embedSyncBatchSize = 2

	if _, err := runEmbedSyncTest(t); err != nil {
		t.Fatalf("runEmbedSync: %v", err)
	}
	// Mock embeds and records hashes per call, so subsequent calls will see
	// fewer "missing" beads. Two iterations is the minimum: 2-of-3 then 1.
	if len(mc.SyncBatchCalls) < 2 {
		t.Fatalf("expected >= 2 SyncBatch calls, got %d", len(mc.SyncBatchCalls))
	}
	for i, call := range mc.SyncBatchCalls {
		if call.Offset != 0 {
			t.Errorf("call %d: offset=%d, want 0 (--only-missing must not advance offset)", i, call.Offset)
		}
		if !call.OnlyMissing {
			t.Errorf("call %d: OnlyMissing=false, want true", i)
		}
	}
}

// TestEmbedSync_OnlyMissingForceMutuallyExclusive verifies the CLI rejects
// the combination at validation time (before any server call).
func TestEmbedSync_OnlyMissingForceMutuallyExclusive(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetEmbedFlags(t)
	embedSyncOnlyMissing = true
	embedSyncForce = true

	_, err := runEmbedSyncTest(t)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
	if len(mc.SyncBatchCalls) != 0 {
		t.Errorf("expected no server calls on validation error, got %d", len(mc.SyncBatchCalls))
	}
}

// TestEmbedSync_LimitBoundsTotalProcessed verifies --limit caps the total
// beads visited across all pages. The motivating use case (ka-0000000728):
// the e2e semantic-search smoke seeds a handful of new beads then runs sync,
// but a multi-thousand-bead embedding backlog blows the 15-min e2e shard
// budget. --limit lets the test bound wall time deterministically.
func TestEmbedSync_LimitBoundsTotalProcessed(t *testing.T) {
	mc := newMockClient()
	// 7 unembedded beads.
	beads := make([]*model.Bead, 7)
	for i := range beads {
		beads[i] = &model.Bead{ID: fmt.Sprintf("gd-%d", i), Title: fmt.Sprintf("T%d", i)}
	}
	mc.ListBeadsResult = beads
	mc.EmbeddingHashes = map[string]string{}

	withMockClient(t, mc)
	resetEmbedFlags(t)
	embedSyncOnlyMissing = true
	embedSyncBatchSize = 5
	embedSyncLimit = 3

	if _, err := runEmbedSyncTest(t); err != nil {
		t.Fatalf("runEmbedSync: %v", err)
	}
	// Exactly 1 SyncBatch call with Limit=3 (the remaining-budget clamp).
	if len(mc.SyncBatchCalls) != 1 {
		t.Fatalf("expected 1 SyncBatch call, got %d", len(mc.SyncBatchCalls))
	}
	if got := mc.SyncBatchCalls[0].Limit; got != 3 {
		t.Errorf("expected per-page Limit=3 (clamped to remaining budget), got %d", got)
	}
	if len(mc.UpsertEmbeddingCalls) != 3 {
		t.Errorf("expected 3 embeddings (bounded by --limit=3), got %d", len(mc.UpsertEmbeddingCalls))
	}
}

// TestEmbedSync_LimitZeroMeansUnlimited verifies --limit=0 (the default) keeps
// the prior behavior — the loop drains the full workload.
func TestEmbedSync_LimitZeroMeansUnlimited(t *testing.T) {
	mc := newMockClient()
	beads := make([]*model.Bead, 4)
	for i := range beads {
		beads[i] = &model.Bead{ID: fmt.Sprintf("gd-%d", i), Title: fmt.Sprintf("T%d", i)}
	}
	mc.ListBeadsResult = beads
	mc.EmbeddingHashes = map[string]string{}

	withMockClient(t, mc)
	resetEmbedFlags(t)
	embedSyncBatchSize = 10
	embedSyncLimit = 0

	if _, err := runEmbedSyncTest(t); err != nil {
		t.Fatalf("runEmbedSync: %v", err)
	}
	if len(mc.UpsertEmbeddingCalls) != 4 {
		t.Errorf("expected all 4 beads embedded with --limit=0, got %d", len(mc.UpsertEmbeddingCalls))
	}
	// Per-page Limit should be the user's batch-size, untouched by --limit logic.
	if got := mc.SyncBatchCalls[0].Limit; got != 10 {
		t.Errorf("expected Limit=batch-size=10, got %d", got)
	}
}

// TestEmbedSync_SurfacesServerError verifies the CLI wraps server errors
// (e.g. the embedder backend returns 503) with a "sync-batch" prefix so
// pipeline operators can see which step of the loop failed.
func TestEmbedSync_SurfacesServerError(t *testing.T) {
	mc := newMockClient()
	mc.SyncBatchErr = fmt.Errorf("embedder call failed: ollama down")
	withMockClient(t, mc)
	resetEmbedFlags(t)

	_, err := runEmbedSyncTest(t)
	if err == nil || !strings.Contains(err.Error(), "sync-batch") {
		t.Fatalf("expected sync-batch error, got %v", err)
	}
}

func TestParseTypeFilter(t *testing.T) {
	t.Run("empty flag and env returns nil", func(t *testing.T) {
		t.Setenv("X_TYPES", "")
		if got := parseTypeFilter("", "X_TYPES"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("X_TYPES", "ignored")
		got := parseTypeFilter("task,decision", "X_TYPES")
		if _, ok := got["task"]; !ok {
			t.Errorf("missing 'task' in %v", got)
		}
		if _, ok := got["decision"]; !ok {
			t.Errorf("missing 'decision' in %v", got)
		}
		if _, ok := got["ignored"]; ok {
			t.Errorf("env value leaked into %v", got)
		}
	})
	t.Run("env used when flag empty", func(t *testing.T) {
		t.Setenv("X_TYPES", "task,event")
		got := parseTypeFilter("", "X_TYPES")
		if len(got) != 2 {
			t.Errorf("len = %d, want 2: %v", len(got), got)
		}
	})
	t.Run("whitespace tolerance", func(t *testing.T) {
		got := parseTypeFilter("task, decision , advice\tbug", "X")
		if len(got) != 4 {
			t.Errorf("len = %d, want 4: %v", len(got), got)
		}
	})
}

func TestEmbedSync_TypeFilterAllowlist(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-a", Type: "task", Title: "A", Description: "alpha"},
		{ID: "gd-b", Type: "event", Title: "B", Description: "beta"},
		{ID: "gd-c", Type: "decision", Title: "C", Description: "gamma"},
	}
	fe := &fakeEmbedder{model: "test", dims: 3}
	withMockClient(t, mc)
	withFakeEmbedder(t, fe)
	resetEmbedFlags(t)
	embedSyncTypes = "task,decision"

	out, err := runEmbedSyncTest(t)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	// Expect two embeddings (task + decision), event filtered.
	if len(mc.UpsertEmbeddingCalls) != 2 {
		t.Errorf("UpsertEmbedding calls = %d, want 2: %v", len(mc.UpsertEmbeddingCalls), mc.UpsertEmbeddingCalls)
	}
	for _, c := range mc.UpsertEmbeddingCalls {
		if c.BeadID == "gd-b" {
			t.Errorf("gd-b (event) should have been filtered: %v", c)
		}
	}
}

func TestEmbedSync_TypeFilterExcludelist(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-a", Type: "task", Title: "A", Description: "alpha"},
		{ID: "gd-b", Type: "event", Title: "B", Description: "beta"},
		{ID: "gd-c", Type: "session", Title: "C", Description: "gamma"},
	}
	fe := &fakeEmbedder{model: "test", dims: 3}
	withMockClient(t, mc)
	withFakeEmbedder(t, fe)
	resetEmbedFlags(t)
	embedSyncExcludeTypes = "event,session"

	out, err := runEmbedSyncTest(t)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if len(mc.UpsertEmbeddingCalls) != 1 {
		t.Errorf("UpsertEmbedding calls = %d, want 1 (only gd-a/task)", len(mc.UpsertEmbeddingCalls))
	}
}

func TestEmbedSync_TypeFilterFromEnv(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-a", Type: "task", Title: "A", Description: "alpha"},
		{ID: "gd-b", Type: "event", Title: "B", Description: "beta"},
	}
	fe := &fakeEmbedder{model: "test", dims: 3}
	withMockClient(t, mc)
	withFakeEmbedder(t, fe)
	resetEmbedFlags(t)
	t.Setenv("BEADS_EMBED_TYPES", "task")

	if _, err := runEmbedSyncTest(t); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(mc.UpsertEmbeddingCalls) != 1 {
		t.Errorf("env-driven allowlist failed: got %d upserts, want 1", len(mc.UpsertEmbeddingCalls))
	}
}
