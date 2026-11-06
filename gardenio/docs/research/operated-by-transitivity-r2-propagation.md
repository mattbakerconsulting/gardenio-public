# R2 — Transitive-propagation model for `operated_by`

- **Bead:** ka-0000000973
- **Parent epic:** ka-0000003404 (operated_by transitivity, "dibs" rule, audit trail)
- **Sibling research:** R1 inventory (`operated-by-transitivity-r1-inventory.md`),
  R3 dibs rule (`operated-by-transitivity-r3-dibs.md`), R4 audit substrate
  (`operated-by-transitivity-r4-audit.md`).
- **Status:** Research only. No code changes, no schema migration, no tests.

## 1. Goal and scope

Decide the propagation model for `operated_by` along the spawn / mutation
chain — schedule → gardener → crew → child task → grand-child — and along
non-spawn fan-outs (formula expansion, trigger actions, reconciler-driven
recreation).

Two pre-existing mechanisms (R3 ADR §4.2, P3/P5 implementation, podmanager
env injection) need to be reconciled into one rule so that future work
(impl epic gated by the parent decision checkpoint) has a single answer to
"how does `operated_by` arrive at this row?".

Out of scope (owned elsewhere):

- The dibs rule itself (R3) — what the *server* does on each mutation.
- The audit substrate (R4) — where the history lives.
- Specific bridge gaps (R1) — the call-site census that this doc closes.
- Test design and rollout (R5 / impl epic).

The hard rule from Matt is reproduced verbatim from the parent epic:

> "we should make this transparent to the agent."

— meaning agents must NOT have to explicitly forward `operated_by` to
their child writes. The propagation model is what makes that true.

## 2. The two mechanisms in play

### 2.1 Push (inheritance at spawn time)

When a parent process creates a child bead (or spawns an agent that
will create child beads), the parent **writes `operated_by` into the new
row's payload** so that downstream consumers can read it without phoning
home:

- For an **agent bead**: the spawner sets `Fields["operated_by"]` (and,
  where available, `Fields["slack_user_id"]` / `jira_user_id` /
  `gitlab_user_id`) at create time. Podmanager (`manager.go:758`) lifts
  that value into the pod's `BEADS_OPERATED_BY` env var. Every `kd` /
  `gb` call from inside the pod stamps it as `X-Operated-By` on the
  client default (`kbeads/cmd/kd/main.go:20,47,111`,
  `gardenio/controller/cmd/gb/main.go:26,45,115`).
- For a **task bead** under a schedule: the spawner sets
  `Fields["operated_by"]` directly on the task at creation, so a later
  read by the reconciler / viewer / `kd list -f operated_by=me` sees it
  even before any mutation lands.

Push is what closes R1 gap rows #1 (`scheduler.spawnScheduledAgent`),
#3 (`/spawn` two-step), #8 (JIRA mention spawn), #10–11 (GitLab),
#12 (`gb spawn` CLI), #15–17 (trigger engine), and #22 (gardener
self-spawn). Without push, the reconciler-driven pod-env path
(`controller/cmd/controller/podspec.go:38`,
`operatedByFromMetadata`) is starved and the spawned agent boots with
`BEADS_OPERATED_BY=""`.

### 2.2 Pull (server-side fallback resolver)

The R3 ADR §4 resolver, implemented in
`kbeads/internal/server/audit.go::resolveOperatedBy`, handles every
write at the server. Its ladder:

1. OIDC subject from the auth interceptor.
2. Sanitized `X-Operated-By` header (static-token caller).
3. Static-token caller without header → look up the calling agent's
   bead by Subject and read `Fields["slack_user_id"]` (today; R1 §3
   recommends generalising to `<provider>_user_id`).
4. Empty.

Pull is the safety net. It catches:

- A static-token caller that didn't stamp the header (bridge gap, legacy
  client, partial rollout).
- A child bead created via a `kd` call where the per-call client default
  is right but a particular RPC path forgot to thread the context.
- Writes from inside an agent pod where the env was set correctly but the
  CLI wrapper invocation in question did not propagate it (defensive
  fallback).

Pull is what makes the existing R3 path work without every caller having
to be perfect.

### 2.3 What each mechanism alone does NOT cover

| Scenario | Push alone | Pull alone |
|---|---|---|
| Agent boots with no `operated_by` field on its own bead | ✗ pod env empty → all child writes fall to pull | ✓ pull reads the agent bead's `slack_user_id` field if present |
| Spawner forgets to stamp `operated_by` on the spawned bead | ✗ chain breaks at the spawn boundary | ✓ pull reads the spawner-bead lineage IF the resolver climbs (it does NOT today) |
| Formula expansion fired by a human with no agent context | ✗ no parent to push from | ✓ pull picks up the caller's `X-Operated-By` |
| Sentinel-rooted spawn (system:scheduler) | ✓ writes `system:scheduler` into child Fields | ✓ pull resolves caller as `system:scheduler` |
| Cross-process race: bead created on one connection, child write on another before push lands | ✓ if push wins | ✗ if push hasn't committed, pull falls back to empty |
| Pod pre-flight `gb prime` reads its own bead | ✓ field is there | △ pull has nothing to fall back to until the bead exists |

The gaps in the "push alone" column come from the field-not-set boundary.
The gaps in the "pull alone" column come from race + empty-fallback. Each
mechanism patches the other's failure mode.

## 3. Decision: hybrid (push at spawn + pull as fallback)

**Recommended model: hybrid.** Push is the primary mechanism; pull is the
safety net. Concretely:

1. **Every spawn / create call site** (R1 inventory rows #1–3, #8, #10–12,
   #15–17, #22) is fixed to **push** `operated_by` into the new bead's
   `Fields` and (for agent beads) into pod env via `spec.OperatedBy`.
2. **The server resolver continues to run pull** for every write, so
   writes that bypass push (bridge sync, webhook event without an
   identifiable actor, reconciler-driven re-creation of a stale spawn)
   land with the right operator anyway.
3. **The resolver's agent-bead fallback** (R1 §3, step 3) is generalised
   from `slack_user_id` to "first non-empty `<provider>_user_id` in
   `Fields`, plus an explicit `operated_by` field if present"; the impl
   detail belongs to the impl epic but the decision is recorded here.

### 3.1 Why not push-only

- A push miss is silent. If `scheduler.spawnScheduledAgent` is patched
  but a future spawn site forgets, the resulting bead carries `""` and
  every downstream mutation does too. Pull catches that miss at write
  time and surfaces the operator in `mutation_history` (and, via the
  R3 dibs rule, on the bead itself).
- Bridge ingress paths (JIRA webhook, GitLab event, Slack reaction) are
  not "spawns" in the agent-pod sense. There is no parent process to
  read its own bead and write a child bead's Fields atomically — there
  is just an HTTP request handler. Forcing those paths through push
  would mean every bridge handler must do an extra read before it can
  write, *and* must remember to copy the operator into Fields. Pull is
  cheaper and simpler for those.
- Push-only forecloses the resolver climb (caller-bead lookup) which is
  the only thing that makes the existing R3 deployment work for
  static-token callers that don't stamp.

### 3.2 Why not pull-only

- Pod env (`BEADS_OPERATED_BY`) is the channel by which the client default
  gets set. If the agent bead lacks `operated_by` in Fields, the pod
  boots without an env value and every `kd` / `gb` call relies on the
  resolver's caller-bead fallback. That fallback works *only* if some
  human-identity field (`slack_user_id` / `jira_user_id` /
  `gitlab_user_id`) is present on the agent bead — which is a different
  push obligation under a different name. We cannot avoid the push of
  *some* identity field at spawn time; pushing `operated_by` is just the
  most general shape.
- Pull-only adds a server round-trip per write to a path that already
  has the answer in the caller's env. The resolver climb is a single
  indexed lookup but it is not free, and it runs inside the auth /
  resolver interceptor on every RPC.
- Pull-only does not solve the viewer-render path. The viewer reads
  `beads.operated_by` directly. If push doesn't populate the row, a
  freshly-spawned agent or task bead shows as anonymous in the chip
  until its first mutation triggers the dibs rule.

### 3.3 Why not "push everywhere, drop pull"

Pull is doing work that push can't do — bridge sync paths, formula
expansions invoked by human callers, raw `kd` writes from outside the
agent-pod world. Removing pull would re-introduce the very gaps R3
already closed.

### 3.4 The hybrid contract (one paragraph)

**Push writes the operator into every bead's `Fields` at creation time
so the pod env, the viewer chip, and downstream client defaults all see
it without a server round-trip. Pull resolves the operator on every
write at the server so any caller path that bypassed push (bridges,
ingress events, partial deploys) still attributes the row to the right
human or sentinel. The two mechanisms are redundant by design — neither
is allowed to be the only line of defence.**

## 4. Propagation diagram

ASCII diagram of the canonical chains. Each `→` is a propagation edge;
labels in `[brackets]` name the mechanism and the file/function that
owns it.

```
                            ┌───────────────────────────────────────┐
                            │ Human creates schedule bead (Slack    │
                            │ /spawn, viewer, or kd create)         │
                            │ operated_by = slack:U_alice           │
                            └───────────────────┬───────────────────┘
                                                │ [push at insert: P3 server resolver
                                                │  reads X-Operated-By from caller]
                                                ▼
                            ┌───────────────────────────────────────┐
                            │ schedule bead row                     │
                            │ beads.operated_by   = slack:U_alice   │
                            │ Fields.operated_by  = slack:U_alice   │  ← push (spawner writes Field)
                            │ Fields.slack_user_id = U_alice        │  ← push (legacy fallback support)
                            └───────────────────┬───────────────────┘
                                                │ [scheduler tick fires — caller is
                                                │  controller, NOT a human; pull
                                                │  resolver alone would see
                                                │  X-Operated-By="" and fall back
                                                │  to system:scheduler]
                                                │
                                                │ [push: spawnScheduledAgent reads
                                                │  schedule.Fields.operated_by →
                                                │  schedFields["operated_by"]   ─┐
                                                │  + schedFields["slack_user_id"] │  R1 gap #1, #2, #22
                                                │  + ctx = WithOperator(schedule.OperatedBy) ─┘  ← X-Operated-By stamped
                                                ▼
                            ┌───────────────────────────────────────┐
                            │ gardener agent bead                    │
                            │ beads.operated_by   = slack:U_alice   │  ← pull (resolver lifts header)
                            │ Fields.operated_by  = slack:U_alice   │  ← push (carried in schedFields)
                            │ Fields.slack_user_id = U_alice        │
                            │ pod env BEADS_OPERATED_BY=slack:U_alice │  ← push (podmanager:758 reads spec.OperatedBy)
                            └───────────────────┬───────────────────┘
                                                │ [gardener's kd/gb calls: client default
                                                │  reads BEADS_OPERATED_BY env →
                                                │  per-call WithOperator → header
                                                │  X-Operated-By: slack:U_alice]
                                                │
                                                │ [agent does NOT explicitly forward
                                                │  operated_by anywhere — transparent]
                                                ▼
                            ┌───────────────────────────────────────┐
                            │ child task bead (created by gardener)  │
                            │ beads.operated_by   = slack:U_alice   │  ← pull resolves header
                            │ Fields.operated_by  = slack:U_alice   │  ← push (gardener stamps Field on create)
                            └───────────────────┬───────────────────┘
                                                │ [gardener spawns crew via gb spawn]
                                                ▼
                            ┌───────────────────────────────────────┐
                            │ crew agent bead                       │
                            │ beads.operated_by   = slack:U_alice   │
                            │ Fields.operated_by  = slack:U_alice   │  ← push (gb spawn extraFields)  R1 gap #12
                            │ Fields.slack_user_id = U_alice        │
                            │ pod env BEADS_OPERATED_BY=slack:U_alice │
                            └───────────────────┬───────────────────┘
                                                │ [same as gardener — every kd/gb call
                                                │  inherits via env+header]
                                                ▼
                            ┌───────────────────────────────────────┐
                            │ grandchild task / comment / dep / …    │
                            │ beads.operated_by   = slack:U_alice   │
                            └───────────────────────────────────────┘
```

### 4.1 Sentinel-rooted chain

Identical shape, with `slack:U_alice` replaced by `system:scheduler` (or
another sentinel) at every level. The R3 dibs rule (§4 branch 2) makes
every sentinel mutation **transparent** — it appends to `mutation_history`
but does not flip any bead's `operated_by`. The chain does not "stop";
it stays sentinel until something in §5 transfers it to a human.

### 4.2 Formula-expansion fan-out (no agent context)

```
   ┌──────────────────────┐                ┌──────────────────────┐
   │ caller: kd / gb /    │ X-Operated-By  │ molecule / formula   │
   │ Slack interactive /  │ ─────────────► │ apply RPC            │
   │ trigger handler      │                │ (mol_pour / formula_ │
   └──────────────────────┘                │  apply)              │
              │                            └──────────┬───────────┘
              │                                       │ [pull: every per-pour
              │                                       │  CreateBead RPC carries
              │                                       │  the same X-Operated-By
              │                                       │  via the same RPC ctx]
              │                                       ▼
              │                            ┌──────────────────────┐
              └──── (no parent bead) ────► │ N child beads        │
                                           │ each operated_by =   │
                                           │ caller's operator    │
                                           └──────────────────────┘
```

For formulas there is no spawning agent to push from — the expansion is
a fan-out from the caller. **Pull alone owns this case.** Every per-pour
`CreateBead` RPC is on the same RPC context as the caller, so the same
`X-Operated-By` flows through the resolver and lands on every child.
This matches R1 row #18–19 (already ✓) and R3 §6.4 (formula default).

What changes vs. today: trigger-driven formula apply (R1 row #16,
`actionCreateBeadWithFormula`) is currently ✗ because the trigger
engine forgets to thread the source event's actor. R2 inherits the
same fix that R1 prescribed for trigger actions in general — the
trigger action handler stamps the source event's actor (or a sentinel
when no human is identifiable) on the RPC ctx, and the formula RPC
inherits it transparently.

### 4.3 Reconciler-driven recreation

The reconciler does not invent operators. When `BuildSpecFromBeadInfo`
re-creates a pod for an existing agent bead, it reads
`meta["operated_by"]` (or `"slack:" + meta["slack_user_id"]`) from the
already-persisted bead Fields. The push path put it there; the
reconciler is a pure derivation. **No new edge needed**, this works
once §3 is in place.

## 5. Chain-stop semantics

"Chain-stop" = under what conditions does the propagation chain
**break** (operator does not flow to descendants) or **transfer**
(operator changes mid-chain).

### 5.1 Formula expansion (no agent context) — chain-stop rule

| Caller | What flows into the formula's child beads | Chain-stop? |
|---|---|---|
| Human `kd formula apply` (OIDC or env-set) | the human's `X-Operated-By` | No — flows transparently |
| Crew agent invoking `kd formula apply` | the agent's inherited operator (human or sentinel) | No — flows transparently |
| Trigger handler firing `actionCreateBeadWithFormula` from a Slack reaction | `slack:<reactor>` (R1 fix #15–17) | No — flows |
| Trigger handler firing from a JIRA webhook with identifiable author | `jira:<accountId>` | No — flows |
| Trigger handler firing from a system poll / cron (no human author) | `system:<source-name>` (e.g. `system:jira-sync`) | **Yes — chain becomes sentinel.** Per R3 dibs §4 branch 2, sentinel-rooted children are transparent and do not take dibs on any bead they touch. |
| Schedule bead with no human operator (sentinel-only) firing a formula | `system:scheduler` | **Yes — sentinel chain.** |

The rule is simple: **the formula expansion never stops or rewrites the
operator — it inherits the caller's operator unchanged.** Whether that
operator is "human-class and takes dibs" or "sentinel-class and is
transparent" is decided by the R3 dibs rule on each per-row write, not
by the formula machinery.

This avoids two anti-patterns:

1. **Magic re-attribution.** A formula does not "look up" who originally
   defined the formula and use them as the operator — that would make
   formulas a hidden dibs-source. The caller is the operator.
2. **Empty-by-default.** A formula expansion fired from a system poll
   does not silently land with `operated_by = ""`. It lands with the
   sentinel, so the audit trail records "this came from
   `system:jira-sync`" rather than "this came from nowhere." Pull is
   responsible for stamping the sentinel; the trigger handler must
   not pass an empty header — it must stamp the sentinel explicitly.

### 5.2 Sentinel agent (`system:scheduler`) — chain-stop rule

A sentinel-rooted chain (e.g. an internal housekeeping schedule with
no human owner) propagates `system:scheduler` through every level. The
chain does not stop on its own: gardener inherits sentinel, crew
inherits sentinel, child task inherits sentinel.

The R3 dibs rule (§4 branch 2) ensures every mutation by these agents
is transparent — `mutation_history` records them but `beads.operated_by`
does not flip. The chip therefore stays empty / shows the bead's
original creator, not the sentinel.

**The chain is broken (transferred to a human) only by an explicit
rebind event.** Today there is exactly one such event: decision-answer
(R3 §5.6.2). The sequence is:

1. Sentinel-rooted crew agent `A` (inherited operator =
   `system:scheduler`) creates decision `D`. `D.operated_by =
   system:scheduler` per the dibs rule branch 2 — sentinel insert is
   recorded, no dibs flip.
2. A human `slack:U_bob` answers `D`. The dibs rule fires on `D` and
   `D.operated_by → slack:U_bob` (branch 6: sentinel current displaced
   by human incoming).
3. **In the same transaction**, the answering-human's identity is
   written back to `A.operated_by = slack:U_bob` and
   `A.Fields["operated_by"] = slack:U_bob` (push side-effect).
4. `A`'s pod refreshes its `BEADS_OPERATED_BY` env on resume — by
   re-reading its own bead's Fields. The simplest viable mechanism is
   a `gb prime` / `gb yield`-return-path refresh; the impl epic owns
   the mechanism, R2 just specifies that it must happen.
5. Every subsequent `kd` / `gb` call from `A` stamps
   `X-Operated-By: slack:U_bob`. From this point the chain rooted at
   `A` propagates Bob as a normal human.

Pre-rebind beads created by `A` are not retroactively re-attributed
(R3 §5.6.4). They keep their `system:scheduler` Fields and the
audit trail (R4) shows the lineage cleanly.

### 5.3 The non-rule: human commenters do NOT rebind

Per R3 §5.6.3, a human commenting on an agent bead is engagement, not
authorisation. The comment **does** flip dibs on the agent bead per
the normal R3 rule, but it does **NOT** rebind the agent's inherited
operator. The agent keeps acting on behalf of whoever spawned it /
last answered its decision.

This is a deliberate asymmetry: only privileged "do as I say"
events rebind, and decision-answer is the only such event today. R2
will not introduce additional rebind triggers; that is an explicit
expansion that future research can re-open.

### 5.4 The non-rule: re-parenting does NOT propagate

If `kd dep add` re-parents a bead under a different epic with a
different operator, the moved bead's `operated_by` does **not**
change. Push is a *create-time* mechanism; it does not run on dep
mutations. The dibs rule may flip `operated_by` if the human doing
the re-parenting is human-class (R3 branch 6/7b/13), but the change
is "this human took dibs", not "the new parent's operator was
inherited."

This avoids the cross-bead cascade R3 §8 question 5 explicitly
recommends against. Dibs is per-bead. Cascade is a spawn-time
concept only.

## 6. Transparency to the agent (the load-bearing rule)

Restated, with concrete invariants the impl epic must preserve:

1. **Agents do not call any "set operator" API.** No new `gb` flag,
   no new `kd` subcommand, no environment manipulation in the agent's
   prompt or tools. The agent's outgoing operator is determined by
   the pod env at boot plus the rebind-on-resume mechanism (§5.2
   step 4).
2. **The agent never reads `BEADS_OPERATED_BY`.** Reading the env
   would imply the agent could choose to override it. The CLI library
   reads it once on init (`defaultOperatedBy()` — R1 §3); the agent
   never sees the value.
3. **`X-Operated-By` is never an agent-visible request header.** It
   is set automatically by the client library's `WithOperator(ctx, …)`
   default. Agent code that builds a `kd` / `gb` invocation does not
   pass any operator-related argument.
4. **`gb spawn` from inside an agent does not require an operator
   argument.** The CLI inherits the env and stamps it for the
   spawned bead (R1 row #12 fix). The agent prompt for spawning child
   crews is the same regardless of who the operator is.
5. **Decision-answer rebind is invisible to the agent.** The pod
   refresh on `gb yield` return / `gb prime` reads its bead's current
   `operated_by` and updates the env. The agent's prompt and tool
   surface do not change.

If a future feature genuinely requires the agent to know its operator
(e.g. "address the human by name in a Slack reply"), the agent should
read it from a derived source (e.g. a bead Field exposed on `kd show`
of its own bead) — not by inspecting transport / env state. Operator
identity is a property of the chain, not a runtime knob.

## 7. Worked examples

### 7.1 Slack `/spawn @gardenio …` by Alice

1. Slack interaction handler stamps `WithOperator(ctx,
   "slack:U_alice")` (R1 row #3 — already ✓ for the synchronous
   write).
2. Handler calls `daemon.SpawnAgent(ctx, …)` with `extraFields`
   that — post-fix — include `operated_by: "slack:U_alice"` and
   `slack_user_id: "U_alice"` at create time. (Today these are added
   in a follow-up `UpdateBeadFields`; the fix folds them into the
   create.) **This is push.**
3. Server resolver lifts the header → `beads.operated_by =
   slack:U_alice`. **This is pull.**
4. Reconciler builds the pod spec from the agent bead Fields →
   `BEADS_OPERATED_BY=slack:U_alice`.
5. The agent boots, runs `kd create`, `kd add-comment`, `gb spawn`. All
   stamp `slack:U_alice` automatically. Children inherit transparently.

### 7.2 Cron-fired gardener with no human owner

1. The schedule bead was created by a system bootstrap script;
   `Fields.operated_by = "system:scheduler"`,
   `beads.operated_by = "system:scheduler"`.
2. Tick: `scheduler.spawnScheduledAgent` reads
   `schedule.Fields["operated_by"]` → stamps
   `WithOperator(ctx, "system:scheduler")`, copies into the spawned
   gardener's `schedFields`. **Push.**
3. Gardener bead inserts with `operated_by = system:scheduler`. Dibs
   rule branch 2: sentinel insert, no flip on any other bead.
4. Gardener pod env `BEADS_OPERATED_BY=system:scheduler`.
5. Gardener creates child task. Pull resolves the header →
   `beads.operated_by = system:scheduler` on the child. Dibs rule
   never flips chips.
6. Child task viewer chip is empty (no human dibs holder); activity
   feed shows the sentinel writes for audit.

### 7.3 Mid-chain rebind via decision answer

1. Continuing 7.2: gardener creates decision `D`. `D.operated_by =
   system:scheduler`.
2. Bob clicks "Approve" in Slack on `D`. R3 dibs flips
   `D.operated_by → slack:U_bob`. **Side-effect** (R2 spec): the same
   transaction updates the gardener's `beads.operated_by` and
   `Fields.operated_by` to `slack:U_bob`.
3. Gardener's `gb yield` returns; the pod's `gb prime` refresh re-reads
   its bead, refreshes the env to `slack:U_bob`.
4. Gardener's next `kd create` for a follow-up task stamps Bob. New
   beads operated_by Bob. The chain is now Bob's.
5. Pre-rebind beads (created during step 1) keep
   `operated_by = system:scheduler`. The audit trail (R4) records the
   transition with timestamps so the viewer can render
   "originally system:scheduler / now Bob is driving."

### 7.4 GitLab webhook fires a formula

1. GitLab MR comment arrives at the bridge's webhook handler. R1 row
   #11 fix: handler derives `gitlab:<userId>` from the event author
   and stamps `WithOperator(ctx, "gitlab:U_carol")`.
2. Handler invokes `actionCreateBeadWithFormula` which delegates to
   `formula apply`. The RPC ctx carries the header.
3. Pull resolves the header on every per-pour `CreateBead` →
   children land with `operated_by = gitlab:U_carol`.
4. No agent context, no parent bead — pull alone covers this case
   (§5.1).

### 7.5 JIRA poll-driven sync (no human author)

1. The JIRA sync poller fires every N seconds. R1 row #9 fix: the
   poll loop stamps `WithOperator(ctx, "system:jira-sync")` rather
   than leaving it empty.
2. New comments / status changes from the poll arrive at the server
   with that header. Pull resolves them → `system:jira-sync`.
3. Dibs rule branch 2: transparent. The chip on each touched bead
   remains the human dibs holder; the audit trail records the system
   activity.

## 8. Scope of change implied by this decision (informational)

This doc only **decides**. The implementation epic gated by the
parent's decision checkpoint will:

- Patch the push side at every R1 ✗ row: scheduler spawns, `gb spawn`
  CLI, JIRA mention spawn, GitLab handlers, trigger actions.
- Generalise the resolver fallback from `slack_user_id` to
  `<provider>_user_id` (R1 cross-cutting #3).
- Reconcile `spawned_by` vs `operated_by` Fields (R1 cross-cutting
  #1, #6) so the reconciler's pod-env derivation has a single source.
- Implement the decision-answer rebind side-effect (R3 §5.6
  invariant) with a corresponding `gb prime` / `gb yield`-return
  refresh path on the agent pod.
- Stamp explicit sentinel headers in poll-driven and webhook-driven
  bridge paths so empty-header writes become rare and visible (R1
  cross-cutting #4).

No new schema columns are required by R2 in isolation. The
`operated_by_set_at` column called for by R3 §6 / R4 is the only
schema delta gated by the dibs / audit decisions, and is owned by R4.

## 9. Open questions for the decision checkpoint

These should be folded into the parent epic's decision checkpoint
rather than answered unilaterally here:

1. **`gb prime` refresh cadence.** The agent must re-read its own
   bead's `operated_by` after a yield/resume so post-rebind work is
   correctly attributed. Should the refresh happen
   (a) only on `gb yield` return, (b) at every `gb prime` invocation,
   (c) on every `kd` / `gb` invocation? Recommend (a)+(b): yield is
   the privileged rebind moment and prime is the post-compaction
   re-orient, both already exist.
2. **`spawned_by` field deprecation.** R1 cross-cutting #1 flagged
   the parallel field. Should the impl epic delete it once writers
   migrate to `operated_by`, or alias it for one release cycle?
   Recommend: alias for one release, then delete; pure `operated_by`
   in viewer / resolver from day one.
3. **Sentinel namespace governance.** Should `system:` and `fleet:`
   be the only sentinel prefixes, or do bridges (`bridge:slack-sync`,
   `bridge:gitlab-sync`) merit their own prefix? Recommend: keep two
   prefixes, use `system:<source-name>` for poller-style writes
   (`system:jira-sync`, `system:gitlab-sync`); avoid prefix sprawl.
4. **Trigger-action header default.** When a trigger action cannot
   identify any human author (e.g. a webhook event with author
   stripped), should the handler stamp `system:<trigger-id>` or
   leave the header empty and let the resolver fall through to
   empty? Recommend: stamp the sentinel explicitly. Empty is
   indistinguishable from a bug; the sentinel is auditable.
5. **Reconciler's `BuildSpecFromBeadInfo` source-of-truth.** Today
   it reads either `meta["operated_by"]` or `"slack:" +
   meta["slack_user_id"]`. After R2 push fixes, both will usually be
   present. Should the reconciler require `operated_by` and treat
   the slack-id fallback as legacy, or keep both forever? Recommend:
   prefer `operated_by`, keep the slack-id fallback for one release
   to absorb backfill, then deprecate.

## 10. Summary

- **Model: hybrid.** Push at every spawn / create call site so the bead
  Fields, pod env, and viewer chip are populated before any mutation.
  Pull at the server resolver so any caller path that bypassed push
  still attributes correctly.
- **Formula expansion:** owned by pull. Caller's `X-Operated-By` flows
  through every per-pour `CreateBead`. Trigger-driven expansions stamp
  the source event's actor (or a sentinel when none).
- **Sentinel-rooted chains:** propagate the sentinel through every
  level. The R3 dibs rule keeps them transparent. The chain transfers
  to a human only via decision-answer rebind.
- **Transparent to the agent:** the agent has no operator-related API,
  argument, or env-read. Push at spawn + pull at the server is the
  whole mechanism.
- **Pre-existing R3 plumbing is preserved.** R2 closes the gaps R1
  identified; it does not introduce a third resolver layer.

## 11. Links

- Parent epic: ka-0000003404
- This task: ka-0000000973
- R1 inventory: ka-0000005258 — `operated-by-transitivity-r1-inventory.md`
- R3 dibs rule: ka-0000006715 — `operated-by-transitivity-r3-dibs.md`
  (especially §3.1 "no agent class", §5.6 spawn-inheritance + decision-answer rebind)
- R3 dibs follow-up: ka-0000003437 — `operated-by-transitivity-r3-dibs.md`
  §5.6 was added in MR !2008
- R4 audit substrate: ka-0000001481 —
  `operated-by-transitivity-r4-audit.md`
- ADR: `gardenio/docs/adr/human-attribution-propagation.md`
- Schema: migration `012_human_attribution.up.sql`
- Resolver: `kbeads/internal/server/audit.go::resolveOperatedBy`
- Pod env injection: `gardenio/controller/internal/podmanager/manager.go:758`
- Pod-spec derivation: `gardenio/controller/cmd/controller/podspec.go:38`
- Client default plumbing: `kbeads/cmd/kd/main.go`,
  `gardenio/controller/cmd/gb/main.go`,
  `gardenio/controller/internal/beadsapi/client.go:184`
