package deathtranscript

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mockStore is an in-memory ObjectStore for tests: a flat key→bytes map plus an
// optional per-key error injector. No AWS dependency.
type mockStore struct {
	objects map[string][]byte
	// listErr / getErr, when set, force the corresponding op to fail.
	listErr error
	getErr  map[string]error
}

func newMockStore() *mockStore {
	return &mockStore{objects: map[string][]byte{}, getErr: map[string]error{}}
}

func (m *mockStore) put(key, body string) { m.objects[key] = []byte(body) }

func (m *mockStore) List(_ context.Context, prefix string) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (m *mockStore) Get(_ context.Context, key string) ([]byte, error) {
	if err := m.getErr[key]; err != nil {
		return nil, err
	}
	body, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("mock: key not found: %s", key)
	}
	return body, nil
}

const (
	beadID = "gd-FUtbLnTvSr"
	spawnA = "11111111-1111-1111-1111-111111111111"
	spawnB = "22222222-2222-2222-2222-222222222222"
)

// ── SessionKeyForBead ─────────────────────────────────────────────────────

func TestSessionKeyForBead(t *testing.T) {
	tests := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{"nil bead", nil, ""},
		{"bead id is the key", &beadsapi.BeadDetail{ID: beadID}, beadID},
		{
			"coop_session_id field overrides",
			&beadsapi.BeadDetail{ID: beadID, Fields: map[string]string{"coop_session_id": "sess-xyz"}},
			"sess-xyz",
		},
		{
			"s3_session_key field overrides",
			&beadsapi.BeadDetail{ID: beadID, Fields: map[string]string{"s3_session_key": "override-key"}},
			"override-key",
		},
		{
			"coop_session_id preferred over s3_session_key",
			&beadsapi.BeadDetail{ID: beadID, Fields: map[string]string{
				"coop_session_id": "first", "s3_session_key": "second",
			}},
			"first",
		},
		{
			"blank field falls back to bead id",
			&beadsapi.BeadDetail{ID: beadID, Fields: map[string]string{"coop_session_id": "   "}},
			beadID,
		},
		{"id-less bead", &beadsapi.BeadDetail{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionKeyForBead(tc.bead); got != tc.want {
				t.Fatalf("SessionKeyForBead = %q, want %q", got, tc.want)
			}
		})
	}
}

// ── ChunkKeys ─────────────────────────────────────────────────────────────

func TestChunkKeys_BeadNamespaced_SortedAscending(t *testing.T) {
	m := newMockStore()
	// Out-of-order insertion; keys must come back ascending by snapshot number.
	m.put(TranscriptChunkKey("", beadID, spawnA, 10), `{"type":"user"}`)
	m.put(TranscriptChunkKey("", beadID, spawnA, 2), `{"type":"user"}`)
	m.put(TranscriptChunkKey("", beadID, spawnA, 1), `{"type":"user"}`)
	// Noise objects under the same bead prefix must be ignored.
	m.put(fmt.Sprintf("coop/sessions/%s/%s/meta.json", beadID, spawnA), `{"started_at":100}`)
	m.put(fmt.Sprintf("coop/sessions/%s/%s/recording/1-50.jsonl", beadID, spawnA), `{}`)
	m.put(fmt.Sprintf("coop/sessions/%s/%s/salvage/work.bundle", beadID, spawnA), `x`)

	f := NewFetcher(m, "")
	spawn, keys, err := f.ChunkKeys(context.Background(), beadID)
	if err != nil {
		t.Fatalf("ChunkKeys: %v", err)
	}
	if spawn != spawnA {
		t.Fatalf("spawn = %q, want %q", spawn, spawnA)
	}
	want := []string{
		TranscriptChunkKey("", beadID, spawnA, 1),
		TranscriptChunkKey("", beadID, spawnA, 2),
		TranscriptChunkKey("", beadID, spawnA, 10),
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

func TestChunkKeys_FlatLayout(t *testing.T) {
	m := newMockStore()
	m.put(TranscriptChunkKey("", beadID, "", 1), `{"type":"user"}`)
	m.put(TranscriptChunkKey("", beadID, "", 2), `{"type":"user"}`)

	f := NewFetcher(m, "")
	spawn, keys, err := f.ChunkKeys(context.Background(), beadID)
	if err != nil {
		t.Fatalf("ChunkKeys: %v", err)
	}
	if spawn != "" {
		t.Fatalf("flat layout spawn = %q, want empty", spawn)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want 2", keys)
	}
}

func TestChunkKeys_MultiSpawn_PicksLatestByStartedAt(t *testing.T) {
	m := newMockStore()
	// spawnA is older, spawnB is newer per meta.json started_at.
	m.put(fmt.Sprintf("coop/sessions/%s/%s/meta.json", beadID, spawnA), `{"started_at":100}`)
	m.put(fmt.Sprintf("coop/sessions/%s/%s/meta.json", beadID, spawnB), `{"started_at":200}`)
	m.put(TranscriptChunkKey("", beadID, spawnA, 1), `{"type":"user"}`)
	m.put(TranscriptChunkKey("", beadID, spawnA, 2), `{"type":"user"}`)
	m.put(TranscriptChunkKey("", beadID, spawnB, 1), `{"type":"user"}`)

	f := NewFetcher(m, "")
	spawn, keys, err := f.ChunkKeys(context.Background(), beadID)
	if err != nil {
		t.Fatalf("ChunkKeys: %v", err)
	}
	if spawn != spawnB {
		t.Fatalf("spawn = %q, want newest %q", spawn, spawnB)
	}
	if len(keys) != 1 || keys[0] != TranscriptChunkKey("", beadID, spawnB, 1) {
		t.Fatalf("keys = %v, want single spawnB chunk", keys)
	}
}

func TestChunkKeys_MultiSpawn_NoMeta_FallsBackToHighestSnapshot(t *testing.T) {
	m := newMockStore()
	// No meta.json anywhere; spawnA has the newest snapshot number (5).
	m.put(TranscriptChunkKey("", beadID, spawnA, 5), `{"type":"user"}`)
	m.put(TranscriptChunkKey("", beadID, spawnB, 1), `{"type":"user"}`)

	f := NewFetcher(m, "")
	spawn, _, err := f.ChunkKeys(context.Background(), beadID)
	if err != nil {
		t.Fatalf("ChunkKeys: %v", err)
	}
	if spawn != spawnA {
		t.Fatalf("spawn = %q, want %q (highest snapshot)", spawn, spawnA)
	}
}

func TestChunkKeys_NoTranscript(t *testing.T) {
	m := newMockStore()
	m.put(fmt.Sprintf("coop/sessions/%s/%s/meta.json", beadID, spawnA), `{"started_at":1}`)

	f := NewFetcher(m, "")
	spawn, keys, err := f.ChunkKeys(context.Background(), beadID)
	if err != nil {
		t.Fatalf("ChunkKeys: %v", err)
	}
	if spawn != "" || len(keys) != 0 {
		t.Fatalf("want empty result, got spawn=%q keys=%v", spawn, keys)
	}
}

func TestChunkKeys_EmptySessionKey(t *testing.T) {
	f := NewFetcher(newMockStore(), "")
	if _, _, err := f.ChunkKeys(context.Background(), "  "); err == nil {
		t.Fatal("expected error for empty session key")
	}
}

func TestChunkKeys_ListError(t *testing.T) {
	m := newMockStore()
	m.listErr = errors.New("s3 down")
	f := NewFetcher(m, "")
	if _, _, err := f.ChunkKeys(context.Background(), beadID); err == nil {
		t.Fatal("expected list error to propagate")
	}
}

// ── FetchLatestRaw ────────────────────────────────────────────────────────

const sampleTranscript = `{"type":"user","timestamp":"2026-07-24T10:00:00.000Z","message":{"role":"user","content":"do the thing"}}
{"type":"assistant","timestamp":"2026-07-24T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"on it"},{"type":"tool_use","name":"Bash"}]}}
{"type":"assistant","timestamp":"2026-07-24T10:00:09.000Z","message":{"role":"assistant","content":[{"type":"text","text":"almost done"}]}}`

func TestFetchLatestRaw_ReturnsLatestBytesAndKey(t *testing.T) {
	m := newMockStore()
	// Snapshot 1 is stale; snapshot 2 is the complete pre-death snapshot.
	m.put(TranscriptChunkKey("", beadID, spawnA, 1), `{"type":"user","message":{"role":"user","content":"old"}}`)
	m.put(TranscriptChunkKey("", beadID, spawnA, 2), sampleTranscript)
	m.put(fmt.Sprintf("coop/sessions/%s/%s/meta.json", beadID, spawnA), `{"started_at":1}`)

	f := NewFetcher(m, "")
	data, spawn, chunkKey, err := f.FetchLatestRaw(context.Background(), beadID)
	if err != nil {
		t.Fatalf("FetchLatestRaw: %v", err)
	}
	if chunkKey != TranscriptChunkKey("", beadID, spawnA, 2) {
		t.Fatalf("fetched %q, want snapshot 2", chunkKey)
	}
	if spawn != spawnA {
		t.Fatalf("spawn = %q, want %q", spawn, spawnA)
	}
	// The RAW bytes are the untouched snapshot — the summarizer's own parser
	// consumes them, so they must be byte-identical to what was stored (not this
	// package's parsed projection).
	if string(data) != sampleTranscript {
		t.Fatalf("raw bytes differ from stored snapshot:\n got %q\nwant %q", data, sampleTranscript)
	}
}

func TestFetchLatestRaw_NoTranscript(t *testing.T) {
	f := NewFetcher(newMockStore(), "")
	_, _, _, err := f.FetchLatestRaw(context.Background(), beadID)
	if !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("err = %v, want ErrNoTranscript", err)
	}
}

func TestFetchLatestRaw_GetError(t *testing.T) {
	m := newMockStore()
	key := TranscriptChunkKey("", beadID, spawnA, 1)
	m.put(key, sampleTranscript)
	m.getErr[key] = errors.New("throttled")

	f := NewFetcher(m, "")
	if _, _, _, err := f.FetchLatestRaw(context.Background(), beadID); err == nil {
		t.Fatal("expected fetch error to propagate")
	}
}
