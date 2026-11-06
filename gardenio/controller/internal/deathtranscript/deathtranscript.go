// Package deathtranscript is the mapping layer of the auto-forensics pipeline
// (epic ka-0000005447): given a dead agent bead, it resolves the coop session's
// S3 recording location and fetches the Claude session-log `.jsonl` transcript
// (or, for event-stream harnesses, the coop event logs) so a downstream
// death-cause classifier / summarizer
// (ka-0000001381) and the Slack death report (ka-0000004746) can reason over
// what the agent was doing when it died.
//
// # Session key → S3 layout
//
// coop is launched by the controller with `COOP_S3_SESSION_KEY=<agent-bead-id>`
// (see gb `init_s3_restore.go` / `session.go`), so the *session key is the
// agent bead ID*. Every session artifact lives under a bead-namespaced prefix:
//
//	s3://<COOP_S3_BUCKET>/<COOP_S3_PREFIX>/<bead>/<spawn-uuid>/transcripts/<N>.jsonl
//	                     └─ default prefix: coop/sessions ─┘
//
// The `<spawn-uuid>` segment is coop's internal per-spawn session UUID; a bead
// that is resumed / respawned accumulates several spawn sub-prefixes under the
// same bead (ka-0000003664). The legacy *flat* layout (no COOP_S3_SESSION_KEY,
// coop keyed by its own UUID) omits the spawn segment:
//
//	s3://<bucket>/<prefix>/<bead>/transcripts/<N>.jsonl
//
// Both layouts are handled transparently.
//
// # Transcript chunks are cumulative snapshots
//
// Each `transcripts/<N>.jsonl` is a full snapshot of the session log taken
// before a context compaction (coop `transcript.rs::save_snapshot`), so the
// highest-numbered chunk is the most complete record of the conversation. For
// forensics we fetch that latest snapshot; callers that want every snapshot can
// enumerate them with ChunkKeys.
//
// # Testability
//
// All S3 access goes through the ObjectStore interface so tests run against an
// in-memory backend with no AWS dependency; s3store.go provides the real
// aws-sdk-go-v2 adapter used in production.
package deathtranscript

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// DefaultPrefix is the S3 key prefix coop writes under when COOP_S3_PREFIX
	// is unset. Mirrors gb audit.go's s3Prefix() default.
	DefaultPrefix = "coop/sessions"

	// EnvBucket / EnvPrefix / EnvSessionKey are the environment variables coop
	// and gb read to locate the recording bucket, key prefix, and the explicit
	// per-agent session key respectively.
	EnvBucket     = "COOP_S3_BUCKET"
	EnvPrefix     = "COOP_S3_PREFIX"
	EnvSessionKey = "COOP_S3_SESSION_KEY"

	// transcriptsSeg is the fixed path segment separating a spawn prefix from
	// its numbered transcript snapshots.
	transcriptsSeg = "transcripts/"

	// maxTranscriptBytes caps how much of a single transcript chunk we will
	// read+parse, so a pathological snapshot cannot exhaust memory during a
	// forensics pass. Snapshots are typically well under this.
	maxTranscriptBytes = 64 << 20 // 64 MiB
)

// Session-bearing bead fields, in resolution-preference order. The canonical
// key is the agent bead ID itself (COOP_S3_SESSION_KEY=<agent-bead-id>), but a
// bead may also carry an explicit coop session id in a field; we accept those
// as overrides so an operator that repointed a session still resolves.
var sessionKeyFields = []string{"coop_session_id", "s3_session_key"}

// ObjectStore is the minimal S3 surface this package needs, abstracted so unit
// tests can supply an in-memory mock. List returns every object key under
// prefix; Get returns the full object bytes for an exact key.
type ObjectStore interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Get(ctx context.Context, key string) ([]byte, error)
}

// Turn is one projected record of a session's event logs (ParseEvents).
type Turn struct {
	// Type is the record type: "user", "assistant", "system", "result", …
	Type string `json:"type"`
	// Role is message.role when present (usually mirrors Type for user/assistant).
	Role string `json:"role,omitempty"`
	// Timestamp is the parsed RFC3339 "timestamp" field; zero when absent.
	Timestamp time.Time `json:"timestamp,omitzero"`
	// Text is the best-effort human-readable text of the turn: string content
	// verbatim, or the concatenation of text/thinking blocks. Tool-use blocks
	// contribute a "[tool: <name>]" marker so the flow is still legible.
	Text string `json:"text,omitempty"`
	// Tool / Command are set only on Type "tool" turns projected from the
	// harness-neutral event logs (ParseEvents): the tool name and its shell
	// command. The Claude path leaves them empty.
	Tool    string `json:"tool,omitempty"`
	Command string `json:"command,omitempty"`
}

// sessionMeta is the subset of coop's meta.json needed to pick the most recent
// spawn when a bead has several. Matches the JSON written by coop's S3
// subscriber (coop/crates/cli/src/s3.rs SessionMeta).
type sessionMeta struct {
	StartedAt uint64 `json:"started_at"`
	// Harness is the agent CLI that wrote the session ("claude" / "codex");
	// absent in metas written before coop recorded it.
	Harness string `json:"harness"`
}

// Fetcher resolves and fetches transcripts from an ObjectStore.
type Fetcher struct {
	Store ObjectStore
	// Prefix is the S3 key prefix; DefaultPrefix is used when empty.
	Prefix string
}

// NewFetcher builds a Fetcher, defaulting the prefix when empty.
func NewFetcher(store ObjectStore, prefix string) *Fetcher {
	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultPrefix
	}
	return &Fetcher{Store: store, Prefix: strings.TrimRight(prefix, "/")}
}

// SessionKeyForBead maps a dead agent bead to its S3 session key.
//
// The controller launches coop with COOP_S3_SESSION_KEY=<agent-bead-id>, so the
// bead ID is the session key. An explicit coop_session_id / s3_session_key field
// on the bead overrides it (an operator that repointed the session), falling
// back to the bead ID. Returns "" only for a nil / id-less bead.
func SessionKeyForBead(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	for _, f := range sessionKeyFields {
		if v := strings.TrimSpace(bead.Fields[f]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(bead.ID)
}

// transcriptChunk is one discovered numbered snapshot object.
type transcriptChunk struct {
	spawn string
	n     int
	key   string
}

// ChunkKeys lists the transcript snapshot keys for the chosen (most recent)
// spawn of sessionKey, ordered by snapshot number ascending. It returns the
// selected spawn segment ("" for a flat layout) alongside the keys. When the
// session has no transcript objects it returns an empty slice and no error, so
// a caller can distinguish "no transcript archived" from a fetch failure.
func (f *Fetcher) ChunkKeys(ctx context.Context, sessionKey string) (string, []string, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return "", nil, fmt.Errorf("deathtranscript: empty session key")
	}

	base := fmt.Sprintf("%s/%s/", f.prefix(), sessionKey)
	keys, err := f.Store.List(ctx, base)
	if err != nil {
		return "", nil, fmt.Errorf("listing %s: %w", base, err)
	}

	// Bucket every transcript object by spawn segment.
	bySpawn := map[string][]transcriptChunk{}
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, base)
		if !ok {
			continue
		}
		before, after, ok := strings.Cut(rest, transcriptsSeg)
		if !ok {
			continue // meta.json, recording/, salvage/, workflow/, …
		}
		spawn := strings.TrimSuffix(before, "/") // "" for the flat layout
		numStr, ok := strings.CutSuffix(after, ".jsonl")
		if !ok || numStr == "" {
			continue
		}
		n, convErr := strconv.Atoi(numStr)
		if convErr != nil {
			continue
		}
		bySpawn[spawn] = append(bySpawn[spawn], transcriptChunk{spawn: spawn, n: n, key: key})
	}

	if len(bySpawn) == 0 {
		return "", nil, nil
	}

	spawn := f.chooseSpawn(ctx, sessionKey, bySpawn)
	chunks := bySpawn[spawn]
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].n < chunks[j].n })

	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.key
	}
	return spawn, out, nil
}

// chooseSpawn picks which spawn's transcripts to read when a bead has several.
// It prefers the spawn with the greatest meta.json started_at (the most recent
// run — coop's own pick_latest_spawn rule). When no meta is readable it falls
// back to the spawn with the highest snapshot number, then to the
// lexicographically-greatest spawn id, for determinism.
func (f *Fetcher) chooseSpawn(ctx context.Context, sessionKey string, bySpawn map[string][]transcriptChunk) string {
	spawns := make([]string, 0, len(bySpawn))
	for s := range bySpawn {
		spawns = append(spawns, s)
	}
	if len(spawns) == 1 {
		return spawns[0]
	}
	// Deterministic descending order is our tie-break at every stage.
	sort.Sort(sort.Reverse(sort.StringSlice(spawns)))

	bestByStart, bestStart := "", uint64(0)
	sawStart := false
	for _, s := range spawns {
		meta, err := f.spawnMeta(ctx, sessionKey, s)
		if err != nil || meta == nil {
			continue
		}
		if !sawStart || meta.StartedAt > bestStart {
			sawStart, bestStart, bestByStart = true, meta.StartedAt, s
		}
	}
	if sawStart {
		return bestByStart
	}

	// No readable meta anywhere: prefer the spawn with the newest snapshot.
	bestBySnap, bestSnap := spawns[0], -1
	for _, s := range spawns {
		for _, c := range bySpawn[s] {
			if c.n > bestSnap {
				bestSnap, bestBySnap = c.n, s
			}
		}
	}
	return bestBySnap
}

// spawnMeta fetches and parses meta.json for a spawn. spawn may be "".
func (f *Fetcher) spawnMeta(ctx context.Context, sessionKey, spawn string) (*sessionMeta, error) {
	key := fmt.Sprintf("%s/%s/meta.json", f.prefix(), sessionKey)
	if spawn != "" {
		key = fmt.Sprintf("%s/%s/%s/meta.json", f.prefix(), sessionKey, spawn)
	}
	data, err := f.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var m sessionMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", key, err)
	}
	return &m, nil
}

// FetchLatestRaw resolves the most recent spawn for sessionKey and returns the
// RAW bytes of its highest-numbered transcript snapshot alongside that
// snapshot's S3 key (spawn "" for the legacy flat layout).
//
// It exists for callers that need the untouched `.jsonl` — e.g. the death-report
// summarizer (internal/deathreport), whose own richer parser dedups turns by
// message.id and detects stop-gate blocks, so it must see the raw stream rather
// than a coarser Turn projection.
//
// Returns ErrNoTranscript when the session has no archived transcript so callers
// can degrade to a metadata-only report rather than treating an empty recording
// as a hard error.
func (f *Fetcher) FetchLatestRaw(ctx context.Context, sessionKey string) (data []byte, spawn, chunkKey string, err error) {
	spawn, keys, err := f.ChunkKeys(ctx, sessionKey)
	if err != nil {
		return nil, "", "", err
	}
	if len(keys) == 0 {
		return nil, "", "", ErrNoTranscript
	}
	latest := keys[len(keys)-1] // ChunkKeys returns ascending by snapshot number
	data, err = f.Store.Get(ctx, latest)
	if err != nil {
		return nil, "", "", fmt.Errorf("fetching %s: %w", latest, err)
	}
	return data, spawn, latest, nil
}

// ErrNoTranscript signals that a session key resolved but has no transcript
// archived in S3 (a session that died before its first compaction snapshot, or
// with S3 upload disabled).
var ErrNoTranscript = fmt.Errorf("deathtranscript: no transcript archived for session")

func (f *Fetcher) prefix() string {
	if strings.TrimSpace(f.Prefix) == "" {
		return DefaultPrefix
	}
	return strings.TrimRight(f.Prefix, "/")
}

// splitJSONLines splits on newlines and drops blank lines, without allocating a
// full string per line up front.
func splitJSONLines(data []byte) [][]byte {
	var out [][]byte
	for ln := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, []byte(ln))
	}
	return out
}
