# Re-nudge mid-THAW coordination (dedup / buffer / interrupt)

Design child **ka-0000005125** · parent epic **ka-0000004667** (gVisor production
readiness — severe e2e + mid-cycle re-nudge hardening) · operator: Matt Baker.

This is the design-of-record for what the controller + bridge do when a user
message targeting an agent arrives while a **thaw of that agent is in flight**
(Pod B being created / restored / rehydrated, gated on the thawepoch acks). It
is the THAW-side companion to the landed FREEZE-side interrupt design
([`fast-interruptible-freeze.md`](fast-interruptible-freeze.md) §C2,
ka-0000005834). It blocks the impl + e2e children
(DEDUP e2e **ka-0000007641**, and the mid-thaw legs of the readiness gate).

## TL;DR — the decision

| Concern | Decision | Why |
|---|---|---|
| **DEDUP** (a 2nd spawn racing the 1st → two Pod Bs) | **Confirm + strengthen the existing guards. Add one belt-and-suspenders single-flight on `agentName` in the manager.** | The dominant dedup is already structural (single-threaded event loop + `spawnInFlight` + reconciler snapshot-exemption). The remaining gap is a *cross-path* race (event loop vs. reconciler vs. a second bridge spawn) that the existing guards mostly-but-not-provably close. A cheap per-agent in-manager single-flight makes "one thaw per agent at a time" an invariant, not an emergent property. |
| **BUFFER vs INTERRUPT+RESTART** for the in-flight-thaw nudge | **BUFFER (hold the nudge until the thaw releases), with a bounded deadline → fall through to the existing respawn/cold-start path on timeout.** | Thaw has no live fallback Pod A (the asymmetry the epic calls out), so "interrupt and serve live" is unavailable. The message must land on the *freshly-credentialed, rehydrated* Pod B, which is exactly what the thawepoch ack gate already produces. INTERRUPT+RESTART is rejected — it discards sub-second work to re-do it, and re-introduces the dedup race it would be trying to avoid. |
| **FAIL-CLOSED visibility** | **The pending nudge rides the existing partial-success → cold-start fallback, and the user sees one honest "starting up…/resuming…" status that resolves to a delivered answer or a single explicit failure — never a silent drop or a "picked up next cycle" lie.** | The fail-closed teardown path (`main.go` partial-success) already cold-starts on a rehydrate failure; the buffered nudge simply waits for whichever pod wins and is delivered there. |

The shortest statement of the design: **a mid-thaw nudge is a buffered nudge.**
We do not invent a new abort path; we make the message *wait for the thaw to
finish* (success → deliver to the rehydrated Pod B; fail-closed → deliver to the
cold-started pod), bounded by a deadline so a wedged thaw can never strand the
user.

---

## Background: the thaw lifecycle and where a re-nudge lands

A frozen agent is **suspended, not closed**: `gb stop` on a runsc agent leaves
the agent bead open at `agent_state=stopping`, the controller
`runsc checkpoint`s Pod A and tears it down, and a name-keyed snapshot sits on
the gVisor node (`HasSnapshot(agentName)`,
[`freezethaw_lifecycle.go`](../controller/cmd/controller/freezethaw_lifecycle.go)).
The wake is **spawn-triggered**: a new `AgentSpawn` event for a bead with the
same agent name makes `handleEvent` call `ft.shouldThaw` →
`ft.thaw` ([`main.go:1191`](../controller/cmd/controller/main.go)) instead of a
cold `CreateAgentPod`.

`FreezeThawManager.Thaw` ([`freezethaw.go:1524`](../controller/internal/podmanager/freezethaw.go))
runs, in order:

1. `registerThaw(agentName)` — refcounted in-flight marker (`IsThawing` true for
   the whole call).
2. Load snapshot; teardown-complete barrier (`waitPodGone` on Pod A).
3. Create (or warm-graft) **placeholder Pod B**, wait Ready.
4. N-container barrier restore (`runsc restore`) — the process tree comes back.
5. **Rehydration hand-off** — the `NodeRehydrationTrigger` writes the thaw-epoch
   marker; `Thaw` gates on the agent-side `gb`+`coop` acks under
   `ThawRehydrateDeadline` (fail-closed). This is where the restored agent
   **re-mints credentials and reconnects NATS** before it is considered live.
6. Reap the consumed checkpoint image.

A re-nudge can arrive at **any** of these steps. The hazard the epic names is a
nudge landing during steps 3–5: the process tree may be back but the agent is
**not yet re-credentialed / NATS-reconnected** — a "half-restored" agent. A nudge
delivered then either fails (`coop_url` not yet set on the bead) or, worse, lands
on a Pod B that has not finished re-establishing its identity.

### What the bridge does today (the gap)

When a Slack mention resolves to an agent, `bot_mentions.go`:

1. calls `maybeInterruptFreeze(agent)` — `POST /api/v1/freeze/interrupt`. **Mid-thaw
   this 404s** (no *freeze* in flight) and is a no-op.
2. routes based on the bead's `agent_state`:
   - terminal (`done`/`failed`/`stopping`/`budget_exceeded`) → `respawnThreadAgent`
     (which detects `stopping` as `frozenAwaitingThaw` and mints a same-name
     resume bead → fires the thaw);
   - `spawning`/`assigning` → background nudge with `NudgeAgentWithBootRetry`
     (3 attempts, 2→4→8 s backoff, retrying only while the bead is in a startup
     state and has no `coop_url`);
   - otherwise → immediate live nudge.

There is **no `isThawing` consultation anywhere in the bridge** — the bridge can't
even ask "is this agent mid-thaw?" There is no thaw-status endpoint. So a mid-thaw
nudge today is uncoordinated: it either races a same-name respawn (handled by
`spawnInFlight`, see DEDUP below) or boot-retries blindly against a bead whose
`coop_url` is not yet set.

The one accidental piece of buffering that **already exists**: if the in-flight
thaw has progressed far enough that the bead reads `agent_state=spawning` and has
no `coop_url`, `NudgeAgentWithBootRetry` will retry for ~14 s (2+4+8). That is a
*de facto* partial buffer — but it is keyed on the wrong signal (`spawning` +
no-coop-url, not "thaw in flight"), it gives up after 3 attempts regardless of
where the thaw is, and it does not exist for the window before the bead flips to
`spawning`. The design below makes the buffer **explicit and keyed on
`IsThawing`**, subsuming this accident.

---

## DEDUP — one thaw per agent, provably

**Goal:** a second spawn event (or a reconciler create-missing pass, or a second
bridge respawn) during an in-flight thaw must **not** start a second thaw of the
same snapshot. Two concurrent thaws would create two Pod Bs contending for the
same node-local hostPath / sandbox CID family → the kubelet admit-fail signature
(`ka-0000005680`) at best, two live duplicates at worst (`ka-0000007549` class).

### What already guards this (confirmed against source)

1. **Single-threaded controller event loop.** `cmd/controller/main.go` `dispatch(event)`
   runs **synchronously** in the `for { select … }` loop (lines 824–892) — it is
   *not* `go dispatch(...)`. Two `AgentSpawn` events for the same agent are
   processed strictly one-after-another. The first runs `ft.thaw` to completion
   (the post-commit critical section is detached, but the *call* returns before
   the next event is pulled). So **two spawn events alone cannot interleave two
   thaws** within a single controller.
2. **Reconciler create-missing exemption.** `reconciler.go:640` skips
   create-missing for any agent with `agent_state ∈ {done,failed,rate_limited}`
   **and** a live snapshot (`r.hasSnapshot`). A frozen-awaiting-thaw agent is
   therefore never cold-started by the reconciler racing the wake path
   (ka-0000006427, decision ka-0000004662).
3. **Reconciler terminal-pod reaper exemption.** `IsThawing` makes the reaper
   stand off Pod B while it rehydrates (ka-0000002058) — so the reconciler never
   *deletes* the in-flight Pod B either.
4. **Bridge respawn single-flight.** `respawnThreadAgent` holds `spawnInFlight[channel:threadTS]`
   and double-checks `FindAgentBead` for a live bead, so concurrent thread replies
   for the same dead/suspended agent collapse to **one** same-name resume bead →
   **one** spawn event.
5. **Thaw teardown barrier.** `Thaw` step 0 (`waitPodGone` on Pod A) blocks a
   too-early Pod B creation against a not-yet-released hostPath (ka-0000005680).

### The residual gap → the one addition

The guards above close the *common* races but rely on a chain of preconditions
(single controller process, bead-state timing, the bridge guard firing). The
cases they do **not** provably close:

- A **leader handoff / controller restart** mid-thaw: the new leader's reconciler
  or a replayed spawn event could re-enter `Thaw` for an agent whose first thaw is
  still draining. (`registerThaw` is in-memory and does not survive the restart;
  the durable ConfigMap snapshot store means `shouldThaw` still returns true.)
- A spawn event arriving on the `internalEvents` channel (stuck-agent heuristic)
  concurrently with a watcher spawn event — both feed the same loop, so they
  serialize, but the *second* one will still call `ft.thaw` again because the
  first thaw's snapshot is only `Delete`d at the **end** of a successful thaw
  (`freezethaw.go:1389` is the *abort* path; the success reap is the image, not
  the snapshot record until rehydration completes).

**Decision: add a per-agent single-flight in `FreezeThawManager.Thaw` itself,
keyed on `agentName`.** This is the one belt-and-suspenders change DEDUP needs:

- `Thaw` already calls `registerThaw(agentName)` at its top. Promote that from a
  *refcount-for-reaper-exemption* to a *mutual-exclusion gate*: if a thaw for
  `agentName` is already in flight, the second caller does **not** start a second
  restore. Instead it **joins** the in-flight thaw — it waits for the first to
  complete and returns the first thaw's outcome (success → the same Pod B;
  failure → the same error, and the caller cold-starts once).
- Concretely: keep a `map[agentName]*inflightThawState` where the state carries a
  `done chan struct{}` and the eventual `(*ThawResult, error)`. The first caller
  creates the entry and runs the restore; a second caller that finds an existing
  entry blocks on `<-done` and returns the shared result. This makes "exactly one
  `runsc restore` per agent per wake" an **invariant of the manager**, independent
  of how many spawn events or reconciler passes race upstream.
- This composes with — does not replace — the upstream guards: they reduce the
  *rate* of duplicate spawn events; the in-manager single-flight makes a duplicate
  that slips through *harmless*.

This is deliberately the **minimum** dedup change. We are not adding distributed
locking across controllers (a leader election already guarantees one active
controller; the single-flight covers the in-process residue and the brief
post-handoff overlap is bounded by the thaw deadline + Pod-A teardown barrier,
which already serializes a second Pod B behind the first's teardown).

---

## BUFFER — the chosen handling for the in-flight-thaw nudge

**Decision: hold (buffer) the nudge until the in-flight thaw releases Pod B
(rehydration ack complete), then deliver to that freshly-credentialed agent;
bound the wait by a deadline and fall through to the existing respawn/cold-start
fallback on timeout.**

### Why BUFFER and not INTERRUPT+RESTART

The freeze side could *interrupt* because `--leave-running` keeps Pod A alive —
there is always a live agent to serve the message (the `fast-interruptible-freeze.md`
asymmetry). **Thaw has no such fallback**: until the restore + rehydration
completes, there is no live, credentialed agent anywhere. So the only sound
options are (a) make the message **wait** for the agent the thaw is about to
produce, or (b) **abort** the thaw and restart it carrying the message.

INTERRUPT+RESTART is rejected:

- Thaw is **sub-second on the warm path** (Phase 5: R4 p50 ≈ 321–337 ms). Aborting
  a restore that is ~300 ms from completing, to restart it, *adds* latency and
  throws away work — the opposite of the < 20 s SLO intent.
- Aborting mid-restore is **dangerous**: the restore graft, Pod B promotion, and
  rehydration are a committed critical section with detached teardown
  (ka-0000001355). Tearing it down mid-flight to "restart with the message" is
  exactly the orphaned-container / re-drive-race surface the epic is hardening
  *against*. We would be adding a second way to produce the failure we are trying
  to eliminate.
- The message does not need to *change* the thaw. The thaw restores the agent's
  process tree and conversation; the new message is then delivered as a nudge —
  the same nudge that would have been delivered to a live agent. There is nothing
  to "carry into" the restore. BUFFER-then-deliver is strictly simpler and
  strictly safer.

### How BUFFER works (where it lives)

The buffer lives in the **bridge** (the message-ingress seam), not the controller,
because the bridge is what owns the nudge and the user-facing status. It needs one
new piece of information from the controller: **is this agent mid-thaw, and when
does the in-flight thaw resolve?**

**1. New controller endpoint: `GET /api/v1/thaw/status?agent=<name>`** (mirrors the
existing `POST /api/v1/freeze/interrupt` plumbing). Returns:

```json
{ "agent": "<name>", "thawing": true|false }
```

backed by `ft.isThawing(agent)` (already exists,
[`freezethaw_lifecycle.go:237`](../controller/cmd/controller/freezethaw_lifecycle.go)).
Inert (returns `thawing:false`) when the lifecycle is nil (flag off) — zero blast
radius, identical to how `freeze/interrupt` 404s when off.

**2. Bridge: consult thaw-status before routing the nudge.** In `bot_mentions.go`,
right where `maybeInterruptFreeze` is called (line 285), add a symmetric
`maybeBufferForThaw(agent)` step:

- If the controller reports `thawing:true`, the agent is mid-thaw. **Do not nudge
  yet, and do not respawn** (a respawn would race the in-flight thaw — the DEDUP
  hazard). Instead, set the user-facing thread status to a truthful
  "Resuming… (tracking: …)" and enter a **bounded poll-and-deliver loop** in the
  background goroutine the spawning-path already uses:
  - poll `GET /api/v1/thaw/status` (and/or `FindAgentBead` for `coop_url`) on a
    short interval (≈ 1 s, matching the thawepoch ack cadence);
  - when `thawing:false` **and** the bead has a `coop_url` → deliver the nudge via
    the normal `NudgeAgentWithBootRetry`. The agent is now the rehydrated Pod B —
    re-credentialed, NATS-connected — so the message lands on a fully-live agent.
  - bound the loop by a **buffer deadline** = `ThawRehydrateDeadline` +
    `ThawHardDeadline` + a small margin (i.e. "however long a healthy thaw can
    legitimately take, plus slack"). The controller already caps the thaw at
    `ThawHardDeadline`, so the buffer deadline is derived from the same budget —
    they cannot disagree about how long is "too long."

**3. On buffer-deadline timeout → fall through to the existing fallback.** If the
deadline expires with the agent still `thawing` (a wedged thaw) or `thawing:false`
but no `coop_url` (a fail-closed thaw that cold-started — see below), the bridge
takes its **existing** terminal-state path: `respawnThreadAgent` if in a thread
(session-resume cold start), or the "nudge failed, will pick up next cycle" chat
message otherwise. This is the same fallback that exists today; the buffer simply
gives the healthy thaw a chance to win first.

### Why this is safe and minimal

- It is **purely additive and default-off-compatible**: the thaw-status endpoint
  reports `thawing:false` whenever freeze/thaw is disabled, so the bridge's
  `maybeBufferForThaw` is a no-op and routing is byte-identical to today. Like
  `FreezeInterruptEnabled`, the buffer is only ever *active* on the runsc
  freeze/thaw path.
- It **reuses** the background-goroutine + thread-status machinery the
  spawning-path already has (`agentSpawning` branch, `SetThreadStatus`), so it is
  not a new concurrency surface.
- It **subsumes** the accidental boot-retry buffer: instead of "retry 3× while
  `spawning`+no-coop-url," it is "wait until `IsThawing` clears, bounded by the
  thaw budget" — keyed on the correct signal, covering the whole thaw window.
- It composes with DEDUP: because the bridge **does not respawn** while
  `thawing:true`, it cannot inject a duplicate same-name spawn event into the
  in-flight thaw. BUFFER and DEDUP reinforce each other.

---

## FAIL-CLOSED visibility — what the user sees if rehydrate fails-closed

The rehydration gate is **fail-closed by design** (C5): if a required `gb`/`coop`
ack does not arrive within `ThawRehydrateDeadline`, `Thaw` returns a partial
success (`*ThawResult` + error). `main.go`'s partial-success branch
([`main.go:1202`](../controller/cmd/controller/main.go)) then **tears down Pod B
and cold-starts** — the snapshot survives, so a clean cold start is always
reachable.

With BUFFER, the pending nudge experiences this transparently:

1. The bridge is in its poll-and-deliver loop, showing "Resuming…".
2. The thaw fail-closes: `Thaw` returns error → `IsThawing` clears (the
   `registerThaw` release fires on return) → the controller tears down Pod B and
   `CreateAgentPod`s a cold pod under the canonical name.
3. The bridge's next poll sees `thawing:false`. It checks for `coop_url`:
   - If the cold-started pod has come up and set `coop_url` → deliver the nudge to
     it. The user gets their answer from a cold-started (but session-resumed, since
     the bead carries `BOAT_SESSION_RESUME`) agent. **One status transition:
     "Resuming…" → "Forwarded".**
   - If `coop_url` is not yet set (cold start still booting) → the buffer loop
     keeps polling under the same deadline, then falls through to
     `NudgeAgentWithBootRetry` / the respawn fallback. The cold start's own boot is
     covered by boot-retry, exactly as a normal cold spawn is.
4. **The user never sees** a half-restored agent, a silent drop, or a misleading
   "picked up next cycle" while the agent is actually mid-restore. They see one
   honest "Resuming…" that resolves to either a delivered answer or — only if the
   *whole* budget (thaw + cold-start boot-retry) is exhausted — a single explicit
   failure message.

**Edge: controller restart mid-buffer.** If the controller restarts while the
bridge is buffering, the thaw-status endpoint on the new leader reports
`thawing:false` (in-memory registry reset). The bridge then checks `coop_url`; the
new leader's reconciler will re-drive the wake (snapshot still durable in the
ConfigMap store) under the DEDUP single-flight. The buffer deadline still bounds
the wait, so a restart cannot strand the user — worst case it falls through to the
respawn fallback, which is what happens today.

---

## Summary of changes the impl child must land

Scoped tightly so the impl + e2e children (ka-0000007641 and the epic's mid-thaw
e2e legs) have an unambiguous target:

| # | Change | Location | Gated by |
|---|---|---|---|
| 1 | `GET /api/v1/thaw/status?agent=` → `{agent,thawing}` backed by `ft.isThawing` | `cmd/controller/main.go` (new handler) + health mux registration | nil-lifecycle ⇒ `thawing:false` (flag-off no-op) |
| 2 | Per-agent single-flight in `Thaw`: a 2nd concurrent thaw for the same agent **joins** the first and returns its result; no 2nd `runsc restore` | `internal/podmanager/freezethaw.go` (`registerThaw` → join-or-run) | `FreezeThawEnabled` (manager only exists when on) |
| 3 | `maybeBufferForThaw(agent)` in the mention path: poll thaw-status, buffer the nudge until `thawing:false`+`coop_url`, bounded by the thaw budget, then fall through to the existing respawn/cold-start fallback | `internal/bridge/bot_mentions.go` (next to `maybeInterruptFreeze`, line 285) + a `controllerURL`-keyed client like `maybeInterruptFreeze` | `controllerURL` set + endpoint reports `thawing:true` (else no-op) |
| 4 | Honest single thread-status thread-through ("Resuming…" → delivered/failed) | `bot_mentions.go` status writes | reuses existing `SetThreadStatus` |

**No new config flag is strictly required**: BUFFER and the single-flight ride the
existing `FreezeThawEnabled` path and are no-ops when it is off (the endpoint
reports `thawing:false`). If the rollout wants an independent kill-switch, add an
optional `THAW_RENUDGE_BUFFER_ENABLED` (default **on** when `FreezeThawEnabled`,
since the behavior is strictly safer than today's uncoordinated nudge) — but the
default-off-compatibility comes for free from the `isThawing` gate, so a dedicated
flag is a nice-to-have, not a correctness requirement.

## Non-goals (carried from the epic)

- INTERRUPT+RESTART of an in-flight thaw (rejected above).
- Fleet-wide default-on of freeze/thaw (still a separate go/no-go).
- Multi-node freeze migration; resuming Claude session RAM on thaw
  (`session_resume=false` stays).
- Distributed cross-controller thaw locking (leader election + the in-process
  single-flight + the Pod-A teardown barrier already bound the overlap).

## E2E acceptance (hands to the e2e children)

On the live gvisor-poc substrate (the epic's fail-loud readiness gate):

1. **Re-nudge mid-thaw, healthy:** start a thaw; inject a Slack mention while
   `IsThawing` is true; assert (a) **no** second Pod B is created (DEDUP), (b) the
   message is **buffered** (thread status shows "Resuming…", no nudge delivered
   yet), (c) on thaw completion the nudge is delivered to the rehydrated Pod B and
   answered, (d) exactly one status transition to "Forwarded".
2. **Duplicate / concurrent spawn during in-flight thaw (ka-0000007641):** fire two
   same-name spawn events (or a reconciler pass) during a thaw; assert exactly one
   `runsc restore` and one live Pod B (single-flight join).
3. **Re-nudge mid-thaw, fail-closed:** force the rehydration to time out
   (`ThawRehydrateDeadline`); assert Pod B is torn down, a cold pod starts, the
   buffered nudge is delivered to the cold-started (session-resumed) agent, and the
   user sees one honest status — no half-restored delivery, no silent drop.
4. **Buffer-deadline timeout:** wedge the thaw past the buffer deadline; assert the
   bridge falls through to the respawn/cold-start fallback and the user gets a
   single explicit outcome (not an infinite "Resuming…").
