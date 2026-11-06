# Reading & analyzing old agent sessions from S3 (+ a recent failure post-mortem)

**Bead:** ka-0000001724 · **Operator:** Matt Baker · **Date:** 2026-08-14

Every coop agent session is archived to S3. This doc is the standing runbook for
pulling an old session and reasoning over *why it died*, followed by a worked
post-mortem of a recent (2026-08-09) gardener-release-owner failure that shows the
method in action — and turns up a live gap the earlier fix did **not** close.

Helper: `scripts/fetch-agent-session.sh` (fetch + one-line forensic summary).
Related: `gardenio/docs/death-transcript-mapping.md` (the in-controller auto-forensics
path), `gardenio/docs/audit-session-spend.md` (why session cost reads `$0`).

---

## 1. Where sessions live, and the layout (verified 2026-08-14)

Bucket + prefix come from the pod env:

| env | prod value |
|---|---|
| `COOP_S3_BUCKET` | `gardenio-sessions-prod` |
| `COOP_S3_PREFIX` | `coop/sessions` |

> **Layout changed since the June 2026 proof-of-concept.** The original PoC (this
> bead's first comment) documented `gardenio-sessions` with a two-level
> `<agent-bead-id>/<uuid>/` key. Today the bucket is **`gardenio-sessions-prod`**
> and the canonical key is **bead-namespaced then spawn-scoped**:

```text
s3://gardenio-sessions-prod/coop/sessions/<agent-bead-id>/<spawn-uuid>/
    ├── meta.json            # SessionMeta: agent_name, role, project, bead_id,
    │                        #   started_at, ended_at, exit_status, exit_code, transcript_count
    ├── state_events.jsonl   # coop state machine: starting->idle->working->error->...
    │                        #   each transition carries {prev,next,seq,cause,last_message,timestamp_ms}
    ├── hook_events.jsonl    # every SessionStart / UserPromptSubmit / Pre+PostToolUse (full tool I/O)
    ├── transcripts/N.jsonl  # numbered snapshots of the Claude session log (highest N = latest/most complete)
    ├── recording/*.jsonl    # coop terminal screen recordings (final frame = death screen)
    └── stdout-tail.txt      # coop process stdout tail (SIGTERM / drain / S3 restore lines)
```

Key facts that matter for forensics:

- **The session key IS the agent bead ID** (`COOP_S3_SESSION_KEY=<agent-bead-id>`,
  wired from `KD_AGENT_ID`). A session belongs to an *agent bead*, not a pod, so
  it survives restarts/respawns. Each pod launch adds a new `<spawn-uuid>/` under
  the bead; the **newest UUID is the latest spawn**.
- The agent bead's `last_session_id` field is the **coop spawn-UUID**, NOT an S3
  key — you cannot `aws s3 cp` it directly. Fetch by bead ID (the helper resolves
  the newest spawn for you) or `aws s3 ls .../<bead-id>/` to list spawns.
- Access is plain **IRSA** (`arn:aws:sts::123456789012:...:gardenio-prod-agent`).
  `s3:ListBucket`/`GetObject` work under `coop/sessions/`. (Listing the *bucket
  root* by its short name `gardenio-sessions` `AccessDenied`s — that's the old
  name; always use `$COOP_S3_BUCKET`.)
- Scale: ~63,600 bead-keyed session prefixes present as of 2026-08-14.

## 2. How to pull and read one

```bash
# Newest spawn under an agent bead, into ./session-<bead>_<uuid>/
scripts/fetch-agent-session.sh ka-0000007425

# A specific spawn:
scripts/fetch-agent-session.sh ka-0000007425/815873b0-f0c6-497d-84b3-f272860b8187 ./sess

# List spawns under a bead:
scripts/fetch-agent-session.sh --list ka-0000007425

# One-line forensic verdict from a fetched dir:
scripts/fetch-agent-session.sh --summary ./sess
```

Manual reading, in the order that pays off fastest:

1. **`meta.json`** — `exit_status` + `exit_code` first. `exit_code 143` = SIGTERM
   (128+15) = the pod was reaped, not a clean `gb stop`. `exit_status:"error"`
   means coop's state machine ended in `error`; `"stopped"` means it was
   terminated while alive.
2. **`state_events.jsonl`** — the state machine is the spine of the autopsy.
   Count `error`-transitions: a long run of `working->error->working->error…` is a
   **retry hard-loop**, and each `error` row's `last_message` is the API/tool
   error verbatim.
3. **`hook_events.jsonl`** — what the agent actually *did*. `PostToolUse` events
   are completed tool calls (with I/O); if there are only one or two before a wall
   of `UserPromptSubmit` re-nudges, the agent never got traction.
4. **`transcripts/N.jsonl`** (highest N) — the Claude transcript: assistant text,
   tool_use blocks, and any final wrap-up. Cross-check against the hook log.
5. **`stdout-tail.txt`** / **`recording/`** — the death screen and coop's own
   shutdown/drain lines (`s3: drained in-memory state before exit`, mux
   deregister, SIGTERM).

## 3. Finding failed sessions to analyze

```bash
# Failed agent beads (fielded, server-side — a client-side jq scan false-empties):
kd count --all-types -f agent_state=failed
kd list  --all-types -f agent_state=failed --sort -updated_at --limit 60 --json

# Then, per bead, fetch the newest spawn and summarize:
scripts/fetch-agent-session.sh <bead-id> && \
  scripts/fetch-agent-session.sh --summary ./session-<bead>_*
```

Caveat: the bulk `updated_at` on a batch of failed beads often reflects a
**reaper sweep**, not the death time — trust `meta.json`'s `started_at`/`ended_at`
for the real window.

---

## 4. Worked post-mortem — gardener-release-owner, 2026-08-09

**Session:** `ka-0000007425/815873b0-f0c6-497d-84b3-f272860b8187`
**Agent:** `gardenio-gardenio-gardener-release-owner-v3-2i0r` (role `gardener-release-owner`)
**Task:** ka-0000005844 (P0 hourly golden-delivery cron, `13,43 * * * *`)
**Window:** 2026-08-09 15:45:42Z → 16:48:20Z (**62.6 min**) · `exit_status=stopped`, `exit_code=143` (SIGTERM)

### What happened

The cron gardener spawned, ran exactly **two** tool calls —
`kd show ka-0000005844` and `gb news` — and then **every subsequent model turn
429'd** on the account's monthly spend limit. The coop state machine logged
**18 `working→error→working` retry transitions across 62 minutes**, doing zero
further work, until the pod was SIGTERM'd (`exit 143`) and drained. The P0
release-owner task **ka-0000005844 is still `open`, unassigned** — the golden
hourly delivery for that slot silently didn't happen.

The verbatim error (from `state_events.jsonl`, seq 3):

```
API Error: Server is temporarily limiting requests (not your usage limit) ·
{"type":"error","error":{"type":"rate_limit_error",
 "message":"This request would exceed your account's monthly spend limit. Please try again later."}}
No fallback model group found for original model_group=anthropic/claude-haiku-4-5.
Fallbacks=[{'claude-fable-5-oauth': ['claude-opus-5-oauth']},
           {'claude-opus-4-8-oauth': ['claude-opus-5-oauth']}].
Received Model Group=anthropic/claude-haiku-4-5
Available Model Group Fallbacks=None
```

### Root cause — and why the earlier fix did NOT prevent this

This looks identical to bug **ka-0000000592** (2026-07-29: "haiku has no fallback,
gardener-delivery dark ~6h") and its follow-up **ka-0000001221** (2026-08-02:
"haiku-4-5 must NOT fall back to opus-5"). But read the `Fallbacks=[…]` line: it
**already excludes haiku** and lists only `fable-5→opus-5` and `opus-4-8→opus-5`.
That is the *post-fix* config — verified still live in
`gardenio-litellm-config` today. **ka-0000001221 was correctly deployed.** So the
2026-08-09 failure is **not** a regression of that bug.

The actual chain:

1. The gardener-delivery lane **pins the cheap `anthropic/claude-haiku-4-5`**
   model group (deliberate — cheap watchdog work).
2. ka-0000001221 **intentionally removed** the `haiku→opus-5` fallback arm,
   because falling from an *ungated* cheap model onto the *gated* expensive one
   inverts a fallback's purpose (drags haiku onto opus-5's spend cap). The
   documented, intended behavior for a pinned cheap model that exhausts is: **spill
   nowhere and block** — "correct, non-fatal … the operator/probe rotates the
   account" (values.yaml comment, ka-0000001221).
3. On 2026-08-09 the **pinned account itself hit its monthly spend cap**
   (account-wide 429, not a model-group-missing error). With no fallback by
   design, haiku had nowhere to spill — exactly the intended "block" state.
4. **But the agent had no circuit-breaker for that block.** Instead of the
   account being rotated (the operator/probe half of the intended design) or the
   agent yielding, the session **hard-looped ~18 retries for an hour** and then
   died to the reaper — burning a live pod and skipping a P0 delivery slot.

So the "block, don't spill" fix closed the *cost-inversion* half of the problem
but left the **liveness** half open: **a pinned-cheap agent whose account is
capped retries indefinitely and dies idle instead of failing fast or triggering
account rotation.** The pre-emptive control that *would* have caught this —
**ka-0000005907, "spawn-gate agents on per-account budget headroom"** — is marked
closed, yet this gardener was spawned onto an already-capped account and made no
useful progress. That gate is either not covering the gardener-delivery cron path
or not firing on the monthly-cap signal.

### Blast radius — this was an incident, not a one-off

The same account spend-cap signature clusters tightly in the **15:45–16:48Z**
window on 2026-08-09 across multiple gardenio roles (all `exit 143`, all with the
`monthly spend limit` / `Fallbacks=None (haiku)` state events), e.g.:

| bead | role | window (UTC) | spend-limit state events |
|---|---|---|---|
| ka-0000007425 | gardener-release-owner | 15:45–16:48 | 4 |
| ka-0000003270 | crew | 15:45–16:33 | 32 |
| ka-0000003261 | crew (monorepo) | 15:50–16:48 | 32 |

(Other 2026-08-09 failures — e.g. ka-0000000180, ka-0000002633 in the ~02:00Z
band — are `exit 143` reaps *without* the spend signature, i.e. ordinary
end-of-turn reaps, and should not be conflated with this incident. The batch
`updated_at ≈ 19:43Z` on all of them is a later reaper sweep, not the death time —
always confirm the window from `meta.json`.)

### Contrast with the June PoC failure (same bead, different death)

The June PoC (this bead's first comment) analyzed a *different* gardener-release
session (ka-0000001372) whose **work succeeded** and which only recorded `error`
because it died **idle-blocked on a synchronous `TaskOutput`/`Monitor`** when the
pod was reaped. That was a *graceful-exit* anti-pattern. The 2026-08-09 death is
the opposite: the agent **never got any work done** because its first real model
turn was refused, and then it **retry-looped** rather than blocked-on-monitor.
Two different failure modes, same terminal symptom (`error`/`143`) — which is
exactly why you must read `state_events.jsonl`, not just `meta.json`.

## 5. Lessons / recommended follow-ups

1. **`exit 143` + `error` is not self-explaining.** SIGTERM is the *messenger*
   (the reaper), not the cause. Read `state_events.jsonl`: distinguish
   *idle-blocked* (June PoC), *retry-hard-loop* (this one), and *clean end-of-turn
   reap* (the ~02:00Z beads) — they need opposite fixes.
2. **A pinned-cheap agent on a capped account needs a fast-fail, not a retry
   loop.** "Block, don't spill onto a gated model" (ka-0000001221) is right on
   *cost*, but the agent should detect a persistent account-level
   `monthly spend limit` 429 and **`gb stop` / raise an escalation for account
   rotation** within a few turns, instead of burning ~60 min of pod time hitting
   the same wall 18 times.
3. **The budget-headroom spawn-gate (ka-0000005907) should cover the
   gardener-delivery cron path** and key off the *monthly-cap* signal, not just
   per-request rate limits — this gardener was spawned straight onto a capped
   account. Worth filing a follow-up to confirm gate coverage.
4. **Session forensics is cheap and worth doing routinely** — `fetch-agent-session.sh
   --summary` turns a raw S3 prefix into a verdict in one command; run it on the
   `agent_state=failed` frontier to spot incidents (clustered windows + shared
   error signatures) before they're rediscovered as fresh bugs.
