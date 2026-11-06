# Death-transcript mapping: dead agent bead → coop session → S3 transcript

**Package:** `gardenio/controller/internal/deathtranscript`
**Epic:** ka-0000005447 (auto-forensics for involuntary agent deaths)
**Task:** ka-0000005669

When an agent dies with a `close_reason` carrying the `_involuntary` marker, the
auto-forensics pipeline needs to pull that agent's coop recording and reason
over what it was doing when it died. This package is the **mapping + fetch**
layer between a dead agent bead and its parsed `.jsonl` transcript. It hands the
structured turns to the death-cause summarizer (ka-0000001381) and the Slack
death report (ka-0000004746).

## The session key IS the agent bead ID

The controller launches coop with

```
COOP_S3_SESSION_KEY=<agent-bead-id>
```

(wired in `gb init_s3_restore.go` / `session.go` from `KD_AGENT_ID`). coop's
`resolve_session_key` (`coop/crates/cli/src/s3.rs`) uses that explicit key as
the S3 prefix component, so **a session belongs to an agent bead, not a pod** —
it survives restarts and respawns.

`deathtranscript.SessionKeyForBead(bead)` therefore returns the bead ID as the
session key. An explicit `coop_session_id` / `s3_session_key` field on the bead
overrides it (for the rare case where an operator repointed a session), falling
back to the bead ID.

## S3 storage location and layout

Recordings live in the bucket named by `COOP_S3_BUCKET` under the prefix
`COOP_S3_PREFIX` (default `coop/sessions`). The current, **bead-namespaced**
layout is:

```text
s3://<COOP_S3_BUCKET>/<COOP_S3_PREFIX>/<agent-bead-id>/<spawn-uuid>/…
                     └──── prefix ────┘└─ session key ┘└── spawn ──┘
    ├── meta.json                       # SessionMeta: started_at, exit_status, transcript_count, …
    ├── transcripts/
    │   ├── 1.jsonl                      # numbered snapshots of the Claude session log
    │   ├── 2.jsonl
    │   └── N.jsonl                      # highest N = most complete pre-death snapshot
    ├── recording/                       # coop screen-recording chunks (NOT the transcript)
    ├── workflow/                        # dynamic-workflow journals (agent-*.jsonl)
    └── salvage/                         # git bundles of uncommitted WIP (gb salvage)
```

`<spawn-uuid>` is coop's internal per-spawn session UUID. A bead that is
resumed/respawned accumulates **several** spawn sub-prefixes under the same
bead (ka-0000003664); `deathtranscript` picks the most recent spawn by
`meta.json.started_at` (coop's own `pick_latest_spawn` rule), falling back to
the highest snapshot number, then to the lexicographically-greatest spawn id.

### Legacy flat layout

Sessions started before ~2026-05-03 (or with no `COOP_S3_SESSION_KEY`) omit the
spawn segment — coop keyed by its own UUID:

```
s3://<bucket>/<prefix>/<session-key>/transcripts/<N>.jsonl
```

`deathtranscript` handles both layouts transparently: an object whose key has no
`<spawn>/transcripts/` segment is treated as a flat-layout snapshot
(`spawn == ""`).

## Transcript chunks are cumulative snapshots

Each `transcripts/<N>.jsonl` is a **full copy** of the Claude session log taken
just before a context compaction (coop `transcript.rs::save_snapshot`), not a
per-turn delta. So the highest-numbered chunk is the most complete record of the
conversation and is what the forensics pass fetches. Each line is one Claude
session-log record (`{"type":"assistant"|"user"|"system"|"result", "timestamp":…,
"message":{"role":…,"content":…}}`); `content` is either a string or an array of
typed blocks (`text` / `thinking` / `tool_use` / `tool_result`).

## Using the package

```go
import "gardenio/controller/internal/deathtranscript"

// One-call: dead bead → latest parsed transcript.
store := deathtranscript.NewS3Store(s3Client, deathtranscript.BucketFromEnv())
tr, err := deathtranscript.FetchForBead(ctx, store, deathtranscript.PrefixFromEnv(), deadBead)
switch {
case errors.Is(err, deathtranscript.ErrNoTranscript):
    // Session died before its first compaction snapshot, or S3 upload was off.
    // Degrade to a metadata-only death report.
case err != nil:
    // Real fetch/parse failure.
default:
    lastWords := tr.LastWords()      // the agent's final assistant text
    recent    := tr.LastN(20)        // last 20 turns for the "why did it die" summary
}
```

- `SessionKeyForBead(bead) string` — dead bead → S3 session key.
- `Fetcher.ChunkKeys(ctx, key) (spawn, keys, err)` — enumerate every snapshot key
  for the chosen spawn (ascending by number).
- `Fetcher.FetchLatest(ctx, key) (*Transcript, error)` — fetch + parse the newest
  snapshot; returns `ErrNoTranscript` when none is archived.
- `ParseTranscript(data) ([]Turn, error)` — parse raw `.jsonl` bytes into turns;
  blank/garbled lines (e.g. a truncated final line on a killed session) are
  skipped rather than failing the snapshot.
- `Transcript.LastWords()` / `Transcript.LastN(n)` — forensics helpers.

### Testability

All S3 access goes through the `ObjectStore` interface (`List` + `Get`), so unit
tests run against an in-memory backend with no AWS dependency (see
`deathtranscript_test.go`). `s3store.go` is the production `aws-sdk-go-v2`
adapter; it bounds each object read at 64 MiB so a corrupt object cannot exhaust
memory.

## Codex agents (ADR 0004 §9, gd-MolQ67L3xX)

A Codex agent leaves no Claude-shaped transcript: there are no `transcripts/<N>.jsonl`
snapshots, and its `session.jsonl` is a Codex rollout. Go does **not** parse the
rollout. coop uploads two harness-neutral logs next to it for every driver, and the
Codex path reads those:

| Object | Used for |
|---|---|
| `hook_events.jsonl` | user prompts (`user_prompt_submit`) and every tool call (`post_tool_use`: tool name + command, redacted with `toollatency.RedactText` because the file is raw) |
| `state_events.jsonl` | the assistant's final message of each turn (`last_message` on a transition) |
| `meta.json` `harness` | selects the path (`codex`); the dead bead's `harness` field wins when set |

- `Fetcher.SessionHarness` / `Fetcher.FetchLatestEvents` pick the newest spawn (same
  `started_at` rule) and return the raw logs; `ParseEvents(hook, state)` projects them
  into the same `Turn` shape, with `Tool`/`Command` set on tool turns.
- `deathreport.Options.Events` feeds `TranscriptFromTurns`, so `gb session death-report
  --bead <dead-agent>` renders last words **and** a `Last command` line (JSON:
  `transcript.last_command`) for a Codex agent. The Claude path (`Build` with a reader) is
  unchanged and its JSON gains no new keys.
- Not available on this path: stop-gate block counting / cadence diff (it greps Claude
  JSONL entries), so `stop_gate_blocks` is 0 for a Codex agent.

Related Go seams that follow the same rule: `gb`'s session usage reads coop
`GET /api/v1/session/usage` when `GARDENIO_HARNESS=codex` (`loadSessionUsage`), and
`internal/contextwindow.OccupancyPctForAgent` returns "no estimate" for a Codex agent
unless the `model_context_window` bead field is set.

## Related

- [`docs/runbooks/s3-resume.md`](runbooks/s3-resume.md) — the resume contract and
  the same `<bucket>/<prefix>/<bead>/<spawn>/` layout, from the resume angle.
- `gardenio/controller/cmd/gb/audit.go` — `gb audit` fetches the same transcript
  objects for fleet-health auditing; this package is the death-forensics-scoped,
  mockable equivalent.
