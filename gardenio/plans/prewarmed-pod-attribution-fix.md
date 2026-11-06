# Prewarmed pod failures: permanent fix + thorough E2E

**Bead:** ka-0000002816 · **Reported by:** Matt Baker (Slack, 2026-06-08)
**Related:** bug ka-0000006843 (MR !3869), epic ka-0000006528 (cold-start), project pool ka-0000001738

## Symptom

Prewarmed (pool-assigned) thread agents "have trouble doing things." Observed in
the session log of `crew-gardenio-thread-prewarmed-1780959034-2`:

```
[gb agent start] warning: could not register session bead: ... HTTP 400: operator could not be resolved
```

and pool-assigned agents' `gb squawk` messages lose their Slack-thread parent/footer.

## Root cause (one class, two surfaces)

A prewarmed pod is **created before it knows which thread / operator it will serve.**
The controller bakes env vars at pod-create time, so on a warm pod these are **empty**:

- `SLACK_THREAD_CHANNEL` / `SLACK_THREAD_TS` = empty
- `BEADS_OPERATED_BY` = empty  ← verified live: `BEADS_OPERATED_BY=[]`

On `AssignPrewarmed`, the controller writes `slack_thread_channel`, `slack_thread_ts`,
and `operated_by` onto the agent **bead** (poolmanager + `bot_mentions.go` post-assign
`UpdateBeadFields`). But **it cannot mutate the env of a running pod.** The pod's env
stays empty for its whole life.

### Why env-hydration does NOT fix it

`init_agent.go:hydrateAssignmentEnv()` calls `os.Setenv(...)` after standby — but this is
ineffective for in-session work. In `runCoopSession()` the coop subprocess is launched
(`coopCmd.Start()`, env frozen via `buildCoopEnv`) **before** `runSessionLifecycle →
standbyWait → hydrateAssignmentEnv` runs. Claude (and every `kd`/`gb` it spawns) runs
under coop's PTY and inherits coop's **frozen, empty** env. `os.Setenv` only mutates
gb-init's own process, which Claude never sees. This is precisely why MR !3869 fixed the
thread-coords surface with a **bead-fallback in the reader**, not env hydration.

### Why the daemon can't self-recover the operator

Agent pods authenticate to the beads daemon with a **shared static `BEADS_DAEMON_TOKEN`**
(auth subject = `static`). The server's operator-resolution ladder
(`audit.go:resolveOperatedByPure`) for static tokens is:
1. `X-Operated-By` header → used verbatim. (This is fed by `BEADS_OPERATED_BY` → empty.)
2. agent-bead fallback keyed on **auth subject == bead ID** — but the subject is the
   literal string `static`, not the pool agent's bead ID, so `operatedByFromAgentBead`
   gets `subject="static"` and returns `""` immediately.

So pool agents depend **100%** on the `X-Operated-By` header derived from
`BEADS_OPERATED_BY`, which is baked empty → every attributed write 400s
`operator could not be resolved`.

## What MR !3869 already fixes (in flight, open)

- Shallow primary clone (cold-start latency, unrelated to attribution).
- `gb squawk` thread-coords: new `resolveThreadCoords()` (env → agent-bead fallback),
  wired into `lookupSlackThreadBead()` + squawk footer. **Good pattern to mirror.**

It does **NOT** fix: (a) `BEADS_OPERATED_BY` attribution 400s, (b) the remaining
env-only thread readers (`prime_shared.go`, `human_survey.go`), (c) any E2E coverage
that would have caught this.

## Permanent fix (mirror the proven reader-fallback pattern)

### Change 1 — operator resolution falls back to the agent bead (the 400 fix)

In **both** `gb` (`cmd/gb/main.go:defaultOperatedBy`) and `kd`
(`cmd/kd/main.go:defaultOperatedBy`), extend the precedence ladder:

```
1. --operated-by flag        (unchanged)
2. $BEADS_OPERATED_BY        (unchanged)
3. credstore oidc:<email>    (unchanged; gb + kd)
4. NEW: agent-bead fallback  — read BOAT_AGENT_BEAD_ID / KD_AGENT_ID, GetBead,
        return fields.operated_by when it is a well-formed human shape
        (slack:U… / oidc:…@…). Reject agent-shaped / sentinel values, mirroring
        the server's isHumanOperator guard so we never round-trip a bad value.
```

Shared helper `operatedByFromAgentBead()` (one impl, used by both binaries or
duplicated per-module like the existing `defaultOperatedBy`). Cache the lookup per
process so we don't GetBead on every CLI invocation inside a session — resolve once,
memoize. This makes a pool-assigned pod stamp the correct human the moment its bead
carries `operated_by` (which the bridge writes at assign time).

### Change 2 — finish the env-only thread readers

Wire the remaining env-only readers to the same bead fallback MR !3869 introduced:
- `prime_shared.go:84` (prime Slack context)
- `human_survey.go:258`

Use `resolveThreadCoords()` (or lift it to a shared helper). Low-risk, mechanical.

### Change 3 (defense in depth) — controller stamps a non-empty operator earlier

When the bridge can't yet resolve a human (rare), the assign path should still write a
deterministic non-empty `operated_by` so writes never 400. Acceptable values, in order:
the resolved human, else the thread root author, else leave to Change 1's fallback.
This is belt-and-suspenders; Changes 1–2 are the load-bearing fixes.

## Thorough E2E (the explicit ask — "thorough e2e testing")

Current `test-pool-manager.sh` only asserts **bead-field state transitions**
(`prewarmed → assigning`, `spawn_source`, mode). It never drives a real prewarmed pod
through **attributed work after assignment**, which is exactly why this shipped.

### New E2E module: `test-pool-assign-attribution.sh` (claudeless harness)

Drive the real warm-pool path end to end with a `claudeless` agent (deterministic,
no model spend), asserting the pod can actually *do things* after assignment:

1. Enable a prewarmed pool for an ephemeral e2e project; wait for a warm pod `Ready` in
   `agent_state=prewarmed` with **empty** `BEADS_OPERATED_BY`/`SLACK_THREAD_*` in pod env
   (assert the precondition that previously broke things).
2. `POST /api/v1/pool/assign` with a channel/thread/operator (slackmock) + description.
3. Assert the agent bead now carries `operated_by` + `slack_thread_*`.
4. **Attribution assertion:** the assigned pod runs `kd create` (claudeless nudge script)
   and the created bead's `operated_by` == the assigning human (NOT empty, NO 400).
5. **Squawk assertion:** the assigned pod runs `gb squawk`; assert via slackmock the
   message threads under the right channel/ts and carries the footer.
6. Negative guard: a prewarmed pod that has **not** been assigned and has no bead
   `operated_by` either parks cleanly or refuses attributed writes loudly (no silent
   half-state).

### Unit tests (fast, in-tree)

- `TestDefaultOperatedBy_BeadFallback` (gb + kd): env unset + credstore empty + a bead
  carrying `operated_by=slack:U123` → returns `slack:U123`; agent-shaped/sentinel bead
  value → returns `""` (rejected).
- `TestResolveThreadCoords_*` already exist; add `prime_shared` + `human_survey`
  call-site coverage.
- Pool-manager: assert `AssignPrewarmed` writes a non-empty `operated_by` when the
  request carries one (Change 3).

## Sequencing

1. Land Change 1 (operator bead-fallback) — highest impact, unblocks the 400s. MR.
2. Land Change 2 (finish env-only readers) — can ride the same MR or a sibling.
3. Add unit tests with each change (gate in CI).
4. Add `test-pool-assign-attribution.sh` E2E module + register in the shard runner.
5. Change 3 (controller defense) as a follow-up MR.
6. Verify on gardenio-rwx via a real warm-pool assign before prod deploy.
```
