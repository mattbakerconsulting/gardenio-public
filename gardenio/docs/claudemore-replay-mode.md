# claudemore REPLAY mode

**Scope:** Phase 2 task 2.1 (ka-0000005558, epic ka-0000001159). REPLAY is the
Target-rung substrate: a deterministic, **$0** LLM path that serves recorded
cassettes so a CI e2e tier can run the **real `claude` binary** with no real
Anthropic/litellm cost and no network dependence on the model.

## TL;DR

> **Test lane only (2026-10-31, gd-Y4ZCGr2m2t, decision gd-rZJdGewUse).** The
> chart no longer ships claudemore and there is no claudemore image. The binary
> is built from source (`go build ./cmd/claudemore`, or `make -C gardenio
> build-claudemore`) by the hermetic e2e replay modules
> (`gardenio/tests/e2e/scripts/test-claudemore-*.sh`). The Helm values this doc
> used to name (`claudemore.mode`, `claudemore.recording.enabled`) are gone.

Run the binary with `CLAUDEMORE_MODE=replay` and a cassette store configured
(`CLAUDEMORE_CASSETTE_DIR`, or the `COOP_S3_*` env). claudemore then:

- Canonicalizes + hashes each inbound Messages-API request with the **same**
  logic the LIVE recorder used, and serves the matching cassette's response
  **byte-for-byte** from S3.
- **Never contacts litellm.** REPLAY is fully offline w.r.t. the model.
- On a cassette **miss**, fails loudly (`fail_on_miss`) with HTTP `424 Failed
  Dependency` — it does **not** fall back to LIVE. A silent LIVE fall-back would
  make the tier neither $0 nor deterministic, so a miss is a hard failure the
  e2e harness surfaces immediately.

## How a request is matched

REPLAY reuses `claudemore.CanonicalizeRequest` (the exact recorder path), so a
request recorded in LIVE mode resolves to its cassette regardless of run-to-run
noise:

- Object-key ordering is canonicalized (recursive key sort).
- Volatile fields (`metadata`, carrying the per-session `user_id`) are stripped
  before hashing, so two runs of the same logical request collide (A3).
- The lookup key is `(model, request_hash)`, matching the store layout
  `<prefix>/cassettes/<model>/<hash>.json`.

A cassette hit is served with its recorded status, allowlisted response headers
(`content-type`), and body. Streaming (SSE) cassettes are served with
`text/event-stream` + a flush, so the real `claude` binary's stream parser sees
an identical wire. Every REPLAY response carries `X-Claudemore-Replay: hit` (or
`miss`/`error`) so an e2e can assert the response came from a cassette, never
from litellm.

## fail_on_miss — the loud-failure contract

| Situation | HTTP | Header | Body |
|---|---|---|---|
| Cassette found | recorded status (usually 200) | `X-Claudemore-Replay: hit` | recorded response, byte-for-byte |
| No cassette for request | **424** | `X-Claudemore-Replay: miss` + `X-Claudemore-Replay-Miss: <model>/<hash>` | Anthropic-shaped error naming `fail_on_miss` + the model/hash to re-record |
| Store error (S3 down, corrupt) | 502 | `X-Claudemore-Replay: error` | Anthropic-shaped `api_error` |

The `X-Claudemore-Replay-Miss` header names the exact `(model, hash)` that had no
cassette, so a red CI run points straight at what to re-record.

## Running REPLAY

There is no Helm deployment any more (gd-Y4ZCGr2m2t). The e2e modules build the
binary from source and boot it locally, for example:

```bash
cd gardenio/controller && go build -o /tmp/claudemore ./cmd/claudemore
CLAUDEMORE_MODE=replay CLAUDEMORE_CASSETTE_DIR=/tmp/cassettes /tmp/claudemore
```

REPLAY **requires** a cassette store: the binary hard-errors at boot if neither
`CLAUDEMORE_CASSETTE_DIR` nor `COOP_S3_BUCKET` is set (there is nothing to
serve), rather than silently degrading to pass-through.

With `CLAUDEMORE_MODE` unset (or `live`), the binary is the Phase-1 MVP
pass-through — REPLAY is strictly additive.

## Recording the cassettes REPLAY serves

Cassettes are produced by the LIVE record path (task 1.2; the binary records
whenever a cassette store is configured in LIVE mode): run agents through claudemore in LIVE mode once, and each Messages
round-trip is captured to `<prefix>/cassettes/<model>/<hash>.json`. Those same
objects are what REPLAY reads back. Because both paths share
`CanonicalizeRequest` and the store key layout, a request recorded in LIVE is
guaranteed to be found in REPLAY.

## Filesystem cassette store (Gasbox/BYO + hermetic e2e)

REPLAY also accepts a **filesystem** cassette store via
`CLAUDEMORE_CASSETTE_DIR=<dir>` — an alternative to S3 that mirrors the S3 store's
key layout byte-for-byte (`<dir>/cassettes/<model>/<hash>.json`, same
`sanitizeModelSegment`, same `ErrCassetteNotFound` contract). When set it takes
precedence over `COOP_S3_BUCKET` for both record and replay and satisfies
REPLAY's store-required gate.

Two reasons it exists:

- **Gasbox / BYO** (ka-0000005379): the in-box single-EC2 product has zero
  external cloud deps — no S3. A local dir lets claudemore REPLAY there with no
  MinIO/S3 wiring.
- **The hooks e2e** (see below): CI has no S3/MinIO, so the e2e records to a temp
  dir and replays from it in one run.

## Hooks e2e — driving the real `claude` binary against REPLAY (T5/T6)

`gardenio/tests/e2e/scripts/test-claudemore-hooks.sh` (bead ka-0000004452) is the
first tier that runs the **real `claude` binary** in the loop against
claudemore-REPLAY. It is hermetic (no cluster, no S3) and `$0`:

- Each scenario **records** the real `claude` session against a deterministic
  stub upstream (LIVE+record, filesystem cassette store), then **replays** it
  from the same dir with no upstream — same clock, so the date embedded in the
  opus system prompt hashes identically and the cassettes are never stale. They
  are ephemeral temp files, never committed.
- It asserts **T5** (a SessionStart hook injects a marker the model then sends
  back inside a request body — prime *consumption*, not just emission), **T6**
  (a stop-gate hook exits 2→0 and the model acts on the injected block reason
  before stopping — the full Stop loop), and **PreToolUse/PostToolUse → NATS**
  via the real `gb hook relay`.
- The Anthropic SDK's `HEAD /` connectivity preflight is answered with `200` +
  `X-Claudemore-Replay: preflight` (not a cassette lookup), so it never shows up
  as a spurious `fail_on_miss`.

## Agent-choice e2e — asserting the model's *choices* (T2/T3/T4)

`gardenio/tests/e2e/scripts/test-claudemore-agentchoice.sh` (bead ka-0000005944)
is the sibling of the hooks e2e. Where the hooks e2e asserts claude's real *hook
plumbing*, this module asserts the model's real *semantic choices* — the
agent-choice failure classes from the R1 report
(`docs/research/claudemore-r1-why-claudemore.md` §3):

- **T2 right-bead selection** (class 5): given a `gb ready` listing with decoys
  (wrong project, already-claimed), the real model reasons and runs a **real `kd
  claim`** on the correct bead — asserted by the bead-state PATCH the daemon
  received. claudeless uses `echo 'Simulated claim…'`; the parity doc §3.1 DROPs
  "picks the right bead".
- **T3 decision divergence** (class 1): the real model files a **real `gb
  decision`** only when options genuinely diverge (a fork → a 2-option decision
  the server guard accepts) and files **none** for a non-divergent status update
  (it squawks instead). The "options must diverge / no daily check-in" rule is
  prose to the model, enforced nowhere in code.
- **T4 wrap-up blockers judgment** (class 3): on a clean run the real model
  **omits** the `blockers` field, so the bridge's derived card outcome is `done`,
  not ⛔ `blocked` — it does not stamp `blockers:"none"`.

Two design choices distinguish it from the hooks e2e:

1. **Records against real litellm, not a stub.** A scripted upstream would make
   the choice assertions circular (they would only prove the stub emitted what we
   scripted). So it records **real reasoning** through claudemore→litellm, then
   replays the recorded cassettes in the same run to prove the $0/byte-identical
   property. Because a real record costs real tokens, the module is **opt-in**
   (`CLAUDEMORE_AGENTCHOICE_E2E=1`) and self-skips otherwise — the scheduled-tier,
   non-gating placement (§T7).
2. **A stub beads daemon** (`fixtures/claudemore-agentchoice/stub_beads_daemon.py`)
   stands in for the Postgres-backed daemon a hermetic shard cannot boot. It
   records every `kd`/`gb` write to a JSONL log (the assertion oracle) and
   emulates the server-side decision-divergence guard so T3's non-divergent leg
   fails through the real binary exactly as prod would.

Getting real reasoning to replay at $0 with **zero cassette misses** required
neutralizing every volatile token that reaches claudemore's request hash. The
harness runs the driven `claude` in a **clean cwd** (no repo `.claude/` hooks →
the real stop-gate never loops the model), under a **fresh isolated HOME with a
fixed `--session-id`** (so `gb stop`'s `syncAgentSessions` fsync output — which
embeds the transcript path — is byte-identical across the record and replay
phases), with the agent bead **seeded with a real `project:` label** (so `gb
stop`'s advice enrichment does not emit a *timestamped* warning), and with
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` (so the concurrent haiku
title/quota side-call cannot race the main request into a different trajectory).

## Why REPLAY (vs. claudeless)

claudeless fakes the **agent binary** (deterministic by construction, but it can
only assert the *plumbing contract*). claudemore-REPLAY fakes only the **LLM**,
keeping the agent **real** — so a REPLAY e2e can assert real-reasoning failure
classes (right-bead selection, decision divergence, wrap-up judgment) that a
faked binary structurally cannot. Assertions target **outcomes/side-effects**
(a bead state change, a file, a NATS event), never model text (R4-DET-1), so the
replayed run is stable.
