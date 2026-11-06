# Tracking Claude Code sessions and subagents in beads

**Status: PARKED.** Research complete, nothing implemented, no beads cut. Do not
start any phase without the operator's go-ahead. Written 2026-09-21 by the
laptop coordinator session against `main@df8e22c0`.

**Scope.** What gardenio can and cannot tell you, after the fact, about what a
Claude Code agent did: which tools it ran, which subagents it spawned, what it
said, and who reviewed its work. Covers `kbeads`, `gardenio/controller`
(`cmd/gb`, `cmd/beads-viewer`), `coop`, and the live `gardenio-k3s` deployment.

**Method note.** Every claim below was verified against the tree by an
adversarial pass (each "this is missing" claim was given to an agent told to
refute it). Three claims from the earlier draft did not survive and are
recorded in §6 so they are not re-proposed.

---

## 1. The finding that reframes everything

**Hook events are produced, durably stored, and read by nobody.**

The pipeline in `gardenio/docs/hook-event-schema.md` is fully built:

```
Claude Code hook
  -> gb hook relay                 (controller/cmd/gb/hook_relay.go:139-186)
  -> POST /v1/hooks/publish        (kbeads/internal/server/http_hooks.go:211-232)
  -> bus.PublishRaw                (JetStream only, no Postgres)
  -> HOOK_EVENTS  hooks.<agent>.<event>
```

All 25 event types are wired, `SubagentStart`/`SubagentStop` included
(`setup_config.go:473-478`, `hook_relay.go:225-231`). The stream is
`FileStorage` on a PVC, so it survives a restart
(`kbeads/internal/eventbus/streams.go:294`; `deploy-values/gardenio-k3s.yaml:476-490`).

Its only consumer is `bridge.HookStream`, a core-NATS subscriber that renders
Slack cards (`controller/internal/bridge/hookstream.go:229`). **That lives in
slack-bridge, and `deploy-values/gardenio-k3s.yaml:574` says "slackBridge stays
disabled".** The original consumer, beads3d "doots", was deleted in `f1fd0c5f`
(ka-0000006444).

So on the live cluster every agent is publishing a full tool-by-tool activity
stream into a 10,000-message ring buffer that nothing reads and nothing indexes.
This is not a missing pipeline. It is a pipeline with both ends disconnected.

Three consequences shape the whole plan:

- **The work is a sink and a UI, not a source.** Do not design new hook plumbing.
- **The ring is too small and mis-keyed to be the sink.** 10,000 messages / 100MB
  fleet-wide, `DiscardOld`, **no `MaxAge` and no per-subject floor**
  (`streams.go:183-189`), shared with 5-minute synthetic `PreToolUse` heartbeats
  (`agent_start.go:727-733`). A busy day evicts the morning. Subjects are keyed
  by pod agent *name*, which is reused across respawns.
- **Replay exists but is time-only.** `GET /v1/bus/events?stream=hooks&since=<ts>`
  (`http_bus_sse.go:66-97`), no CLI, no per-agent filter.

---

## 2. What already exists (do not rebuild)

Correcting several assumptions that a tracking plan would naturally make.

### The bead substrate is richer than it looks

`type:session` beads already exist and are populated: **742 on the live daemon,
83 open**, carrying `session_log` (jsonl path), `transcript_count`,
`recording_size`, `archive_path`/`archive_url`, `tokens`, `cost`,
`tool_call_count`, `compaction_count`, `resumed`, `exit_reason` — linked to the
agent bead by a `session_of` edge. `type:message` beads labelled `squawk` (942,
716 open) carry per-turn agent narration (`source_agent`, `turn_id`, `turn_role`,
`text`).

**A session/transcript store does not need to be invented. It needs to be
extended and surfaced.**

### The agent terminal viewer is shipped, not pending

`gardenio/plans/beads-viewer-agent-terminal.md:5` still reads "Research-only
until a human green-lights phase 1". **That line is stale.** Phases 1 through 4
are in production code and deployed: routes `GET /agents/{id}/screen|events` and
`POST /agents/{id}/input|keys|nudge|respond|compact`
(`cmd/beads-viewer/server.go:1686-1784`), the xterm pane
(`templates/partials/type_agent.html:130-148`), the bundle-size guard
(`bundle_size_test.go:10-19`), and live values pointing at
`https://mux.clonk.tv/mux`. Fixing that status line is a one-line chore, listed
as P0 below.

### The S3 session archive is built and switched off

coop archives every spawn to
`s3://$COOP_S3_BUCKET/$COOP_S3_PREFIX/<agent-bead-id>/<spawn-uuid>/` —
`transcripts/N.jsonl`, `session.jsonl`, `recording/*.jsonl`, `state_events.jsonl`,
`hook_events.jsonl` (`coop/crates/cli/src/s3.rs:780-960`), built with
`--features s3` in every agent image (`images/agent/Dockerfile:109`). The read
side exists too: `gb audit list/fetch`, `gb session death-report --bead`,
`internal/deathtranscript`, `scripts/fetch-agent-session.sh`, documented in
`docs/session-s3-forensics.md`.

**On gardenio-k3s it is off:** `deploy-values/gardenio-k3s.yaml:728` sets
`s3: {bucket: ""}` and line 492 sets `objectStore: {enabled: false}`. The only
surviving copy of a session is the node-pinned local-path PVC, which dies with
the spot node.

This is the highest value-per-effort item in the plan: a config change and a
bucket, not a feature.

### Three hook pipelines, not one

1. **Relay** — `gb hook relay` → NATS (above). Carries `subagent_id`/`subagent_type`.
2. **Emit** — `gb bus emit`, `gb hook stop-gate`, the `gb agent start` heartbeat
   → `POST /v1/hooks/emit` → the **in-memory** presence tracker
   (`kbeads/internal/presence/tracker.go`, 15m reap) plus the Stop decision gate.
3. **coop** — its own FIFO hooks → `<session_dir>/hook_events.jsonl` with full
   untruncated payloads. **coop registers no Subagent hooks**
   (`coop/crates/cli/src/driver/claude/hooks.rs:74-122`), so subagent spawns are
   absent from that file.

Pipelines 1 and 2 publish **two different payload shapes onto the same subject
space**; `hookstream.go:287-296` parses only the relay shape and silently drops
the other. Any new consumer must handle both, or the shapes should be unified.

---

## 3. The actual gaps

Narrowed to what survived verification.

| # | Gap | Evidence |
|---|---|---|
| A | Hook events reach no durable, queryable, per-agent store. Nothing in `kbeads/internal/store` writes them; no migration 001-047 defines a hook table. | `http_hooks.go:211-232`, migrations |
| B | The relay payload has no agent bead id, though `BOAT_AGENT_BEAD_ID` is in every pod env. Consumers can only key on agent *name*, reused across respawns. | `hook_relay.go:190-285`, `podmanager/manager.go:1944` |
| C | No subagent is represented anywhere in beads. Parent→child is expressible today as `(agent, session_id, subagent_id)` but is never written down. | `hook_relay.go:225-231` |
| D | Session archive off on the live cluster, so post-mortem forensics has no artifacts. | `deploy-values/gardenio-k3s.yaml:492,728` |
| E | Escalation verdicts: `reject` and `result` trust body-supplied `rejected_by`/`executor`; no requester-vs-closer check. Any static-token holder can close its own escalation. | `http_escalations.go:476-492,553-596` |
| F | No `github_pr_merge` guard, executor, or consumer. The string does not appear in Go code at all; prompts still instruct agents to file `gitlab_mr_merge`. | `escalation_create.go:131`, `init_prompts.go:68-69` |
| G | Presence is in-memory only and fed by the emit path, so it is blind to the relay stream. | `presence/tracker.go` |

Gap E deserves a note: **it is deliberate.** Epic gd-3UQKfj0nA8's design has a
crew close its own escalation after a reviewer verdict. The fix is therefore not
"require a human" — `/result` has legitimate machine callers today (the agent
escalation watcher, the escalation reaper, `gb escalation close`). The fix is
**closer bead ≠ requester bead**, which only becomes enforceable once PR #63
lands, since that PR server-stamps `requesting_agent` from the auth identity.
PR #63 is a dependency of this plan, not a duplicate of it.

---

## 4. Phases

Each phase ships alone, with its own bead and PR. Effort is one session unless
noted.

**Operator go given 2026-09-21.** P0 and P2 have landed; P1 and P5 remain
deliberately unstarted for the reasons stated in their sections. See §8 for the
epic and per-phase beads.

### P0 — Chores that need no decision (hours) — LANDED `046b7a37`

> Landed with one caveat. The five listed documents are corrected, but the
> acceptance criterion below is **not** met: roughly 200 `beads3d`/`doots`
> references remain outside archive directories, 162 of them in
> `gardenio/docs/research`. Most are historical prose. The ones that are not —
> live kbeads files citing a deleted `beads3d/docs/` plan, and a Connect shim
> registered for a deleted client — moved to bead `gd-wdFg7gbRhj`.

- Correct the stale "Research-only" status in `beads-viewer-agent-terminal.md:5`
  to record phases 1-4 as shipped.
- Correct the stale doots/beads3d references in `scripts/claude-hooks.json`,
  `docs/onboarding-mac-operator.md:128`, `nats/statefulset.yaml:55-80`,
  `hook-event-schema.md:5-7`, and the wrong claim in
  `docs/research/timeseries-beads-events-metrics.md:67-68` that the controller
  subscribes to `hooks.>`.
- **Acceptance:** `rg 'beads3d|doots'` returns only archive/historical hits.

### P1 — Turn the session archive back on (config + a bucket)

Provision the S3 bucket and IRSA role, set `agents.s3.bucket` and
`objectStore.enabled`. Nothing to build; verify with `gb audit list` and
`gb session death-report --bead <id>` against a real finished agent.

- **Acceptance:** a spot-killed agent's transcript is still readable after its
  node is gone.
- **Owner:** needs a terraform lane owner (no live owner as of 2026-09-21) and
  falls under the AWS cost ceiling, so cost it first. Storage is the whole risk:
  transcripts are large and unredacted.
- **Precondition:** §5 sensitivity gate.

### P2 — Stamp the bead id on the relay event (one file) — LANDED `bbbd88a2`

> Landed. The consumer struct `bridge.hookEvent` also gained the field, so the
> stamp is readable rather than discarded at unmarshal; no behaviour is wired
> to it yet. The e2e regression-net gap flagged in §9 is closed for this
> change: `TestSubagentStopForwardsFinalMessageE2E` was run under `-tags e2e`
> against the new payload and passes. Still unverified: a live
> `GET /v1/bus/events?stream=hooks` showing the field on real traffic, which
> needs the new `gb` binary rolled to agent pods.

Add `agent_bead_id` to `hookRelayEvent` and `buildRelayEvent`, read from
`BOAT_AGENT_BEAD_ID`/`KD_AGENT_ID` the way `cmd/gb/session_metrics_push.go:51-60`
already resolves it. Touches `hook_relay.go`, `hook_relay_test.go:620`, and
`hook-event-schema.md`.

This is the keystone: **every later phase depends on it**, and it is cheap.

- **Acceptance:** `TestBuildRelayEvent_SubagentStart` asserts the bead id;
  a live `GET /v1/bus/events?stream=hooks` shows it on real traffic.

### P3 — A durable sink (the one real design decision)

Three options were evaluated. **Recommended: a dedicated table**, modelled on
the `ui_telemetry` precedent (migration 021), which exists precisely because
high-volume append-only data should not pollute bead history.

```
agent_hook_events(
  id BIGSERIAL, agent_bead_id, parent_agent_bead_id NULL,
  session_id, parent_session_id NULL, event, tool_name,
  subagent_id, subagent_type, payload JSONB, fleet,
  operated_by_id, created_at)
  index (agent_bead_id, created_at), (session_id, created_at)
```

Write point: beside `PublishRaw` in `handleHookPublish`, or a JetStream durable
consumer. Must be **best-effort log-and-continue** like `recordAndPublish`, so a
Postgres hiccup never blocks an agent's tool call.

- *Rejected — reuse the `events` table via `beads.hook.*` topics:* zero schema
  change, but per-bead event history already OOM-killed beads-viewer once
  (ka-0000002256), `recordAndPublish` silently drops rows when the operator is
  unresolvable (agent pods often are), and there is no pruning.
- *Rejected as the primary sink — harden the JetStream ring:* worth doing anyway
  (add `MaxAge` + `MaxMsgsPerSubject`, copying `MESSAGE_EVENTS` at
  `streams.go:222-226`), but it stays unqueryable and outside Postgres backups.

**Retention must be designed before the first row lands.** `PreToolUse` and
`PostToolUse` fire twice per tool call per agent. Follow the 021/047 idempotent
policy: no backfill, a sweep job rather than a `DELETE` in a migration.

- **Acceptance:** after an agent finishes and its pod is gone, one SQL query
  returns its tool sequence in order.

### P4 — Subagent lineage

With P2 and P3 in place, `(agent_bead_id, session_id, subagent_id)` is a real
parent→child key, and the subagent tree becomes a query rather than a schema.

**Explicit re-decision required:** prior research settled that durable
per-subagent *beads* are a STRETCH and were not worth it
(`docs/research/claudemore-r3-cc-feature-inventory.md:85-91`,
`docs/research/claudemore-r5-synthesis.md:167` Q5).
This plan does not overturn that. It proposes subagents as **rows, not beads** —
queryable lineage without 942-bead-style fleet noise. Revisit only if rows prove
insufficient.

- **Acceptance:** for a run that used the Task tool, the viewer shows each
  subagent, its type, and its duration.

### P5 — Surface it (attach, do not add a fourth feed)

The agent card already has the landing spot: `<section id="agent-decisions">` in
`type_agent.html` is an empty phase-2 placeholder. The per-agent events lane is
live-only with no backfill; first paint should replay from P3.

- **Must coordinate with the agent-chat epic gd-fRF1LCCERc**, which is
  peer-owned, P0, and already building the per-agent conversation surface, SSE
  hints (T7) and respawn-with-session-resume (T10). **Sequence after its Task 2
  route registration**, or the two efforts produce duplicate fleet surfaces.
- **Acceptance:** an operator can answer "what did this agent do, and what did
  its subagents do" from the bead page, for a finished agent.

### P6 — Review loop (adjacent, separately owned)

Not this plan's work, listed so the boundary is explicit. Epic gd-3UQKfj0nA8 is
**open with phases 1-4 entirely unstarted**; its gardener has been serialised
behind bug gd-91b7zMOR2z since 02:00Z.

- The reviewer role bead is **already task gd-zFgIuc8LBY** (P0, phase 1,
  unclaimed). Do not re-propose it. `gb role scaffold reviewer --model <tier>`
  is the existing one-call mechanism, and `reviewer` is already a builtin role
  name (`cmd/controller/main.go:3436`).
- Verdict authz (gap E) is **already task gd-S12o3mOtxg**. Its real content is
  "closer ≠ requester", and it should be sequenced after PR #63.
- Gap F (`github_pr_merge`) has no bead. It is the smallest genuinely unclaimed
  item here.

---

## 5. Sensitivity gate — blocks P1, P3 and P5

Hook payloads and transcripts contain unredacted system-reminder text, prompts
and full tool I/O. `docs/research/security-audit-observability.md:183` records
`hook_events.jsonl` as carrying prime/system-reminder text and tool args with no
redaction, and finding **B3 (High)** at lines 217-221 notes that operator-
confidential prime advice lands in the start-hook event verbatim.

Therefore: any UI rendering hook payloads sits behind the same
`gardenio-operator` gate as the terminal viewer, with the same access log;
`tool_input`/`tool_response` are **redacted by default**; and P1's bucket needs a
retention and access policy before it is switched on, not after. The relay
already truncates to 1024 bytes, which limits but does not remove exposure.

Default event filter, from the existing feed-quality scoring
(`docs/feed-quality/*/REPORT.md`) and the schema's own note that
`UserPromptSubmit` is low-signal: **lifecycle, subagent, task and idle events on;
tool-level events off by default.**

---

## 6. Claims that did not survive verification

Recorded so they are not re-proposed.

- **"Nothing durable stores hook events."** False. `HOOK_EVENTS` is `FileStorage`
  on a PVC and replayable by time; coop writes a never-truncated
  `hook_events.jsonl` per session. The true gap is *queryable, per-agent,
  bead-linked* storage, and a ring buffer far too small to span a day.
- **"There is no session transcript store."** False. It is fully built
  (coop → S3, `deathtranscript`, `gb audit`) and merely switched off in the k3s
  values.
- **"The SubagentStop final message is dropped."** Stale. It was true when
  `docs/research/claudemore-r5-synthesis.md:167` was written; it is forwarded today
  (`hookstream.go:329`, `agentstream.go:1063-1072`).

---

## 7. Preconditions the operator owns

1. Go-ahead to start any phase (this plan is parked).
2. P1 needs a terraform lane owner — nobody holds that lane as of 2026-09-21 —
   plus a cost decision against the AWS ceiling.
3. P3 is a kbeads schema migration and a cluster rollout: implementation-lane work.
4. P5 must be sequenced with the peer-owned agent-chat epic gd-fRF1LCCERc.
5. PR #63 should land before gap E is touched.

## 8. Bead anchoring

**Un-parked 2026-09-21. Epic: `gd-gF7ftRkSAb`.** Phase beads, in order:

| Phase | Bead | Status |
|-------|------|--------|
| P0 stale docs | `gd-YqLy4oyxBC` | closed — commit `046b7a37` |
| P1 S3 archive on | `gd-iHOPz73bPK` | open, blocked: no terraform owner + cost |
| P2 stamp bead id | `gd-tLQb92W8L6` | closed — commit `bbbd88a2` |
| P3 durable sink | `gd-J7Wqmqfnpq` | open — migration **049**, not 048 |
| P4 subagent lineage | `gd-uVJngJq2nL` | open, depends on P3 |
| P5 surface it | `gd-xUY7YO0h47` | open, blocked behind `gd-fRF1LCCERc` |
| Connect-shim sweep | `gd-wdFg7gbRhj` | open — found during P0 |

A new `gd-` epic was minted rather than reusing anything older, because every
prior design doc in this area cites dead `ka-*` ids on the retired acme.example
host, which is why their status cannot be read from bead state.

Live anchors: gd-3UQKfj0nA8 (review loop epic), gd-zFgIuc8LBY (reviewer role),
gd-S12o3mOtxg (verdict authz), gd-t0gm8q62Ac (resume/liveness reconciler),
gd-fRF1LCCERc (agent chat, peer-owned), gd-oI2AIaSzjF (working rules).

A lexical search of the live daemon found **no** bead covering Claude Code hook
events, subagent tracking, or presence, so this plan duplicates nothing. Caveat:
semantic search is disabled on the daemon (`BEADS_EMBED_BACKEND=noop`), so that
is a lexical negative only.

## 9. Testing posture

`tests/e2e/scripts/test-hooks-system.sh`, `subagentstop_e2e_test.go` and
`hookstream_e2e_test.go` all exercise these exact hops, and **none has run in CI
since the 2026-09-16 RWX/GitLab decommission**. A plan that changes the hook
payload cannot lean on them as a regression net. Either revive the
relay→publish→hookstream e2e as part of P2, or accept the gap in writing.
