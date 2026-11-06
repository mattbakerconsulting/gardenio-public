# Persistent Agents in Gardenio: Unified Design Memo

> Research epic: ka-0000006566 — spawned from Slack thread (Matt Baker, 2026-06-02).
> Produced by a fan-out "uberthink" workflow: 6 design memos (successor-flag,
> persistence-model, naming, gardener-ownership, orphan-prevention, viewer-display)
> → adversarial critique each → synthesis. Findings below were verified against
> shipped code and corrected several initial assumptions.

## 1. Executive Summary

Matt's proposal — "an agent card is an agent bead," with a wrapup-time choice of
new-card-vs-same-card, stable names, no orphans, and gardener ownership — is mostly
about **promoting machinery that already exists into surfaces that don't use it
yet**, plus one piece of genuinely net-new infrastructure. The single most
important fact, verified across all critiques: **timer-wake respawn does not exist
in shipped code.** `next_respawn_at`, `respawn_count`/`max_respawns`, and
`goal_satisfied` are *written* by `gb stop` (wrapup.go) and *displayed* by the
Slack card (bot_agents_wrapup.go), but **no controller, reconciler, or scheduler
reads them to spawn a pod** — `terminal-goals.md` is a Draft. So "same card" is not
a rename of working behavior; it requires building a wake spawner.

We therefore make the **stable NAME the unit of persistence** (the working
substrate — `respawnThreadAgent` already mints a fresh bead with the same name and
`coop --resume` reconnects the conversation), keep succession as a **field
back-pointer (not a deps edge)**, and use the **already-existing `type:fleet` bead
as the guaranteed ownership root** rather than inventing a parallel "fleet gardener"
hierarchy. Phase 1 ships the smallest thing that delivers Matt's "new card on
wrapup" and easier display: a validated `successor` enum on WrapUp, a single
`prior_agent_id` back-pointer written on every recreate, a `gardener_id` stamped at
spawn (so nothing is ever unowned), and viewer lineage rendered as a linear strip
— **zero deps-table changes, zero new sweepers, zero new bead types.** Persistence
(open dormant beads, timer wake) is deferred to Phase 2/3 behind the wake spawner
it actually requires. The default everywhere is **non-persistent / terminal**: an
agent must affirmatively opt into surviving its own wrapup.

## 2. Unified Design (conflicts resolved)

### 2.1 Identity: the NAME persists, the bead is per-incarnation

The memos collided on "is a card a bead?" The answer the code already gives:
**identity is the stable agent NAME; a bead is one incarnation.**
`respawnThreadAgent` (bot_agent_kill.go) proves a name can span many beads with one
resumed conversation and one reused Slack card (`slack_card_ts`). We adopt this as
the canonical model and **reject the "open dormant bead" model** as the primary
mechanism — it fights the reconciler (which desires one pod per open bead), the
StaleReaper (which kills open-bead-no-pod within `r.timeout`), and the active-agent
cache. A parked agent is a **closed bead** carrying succession metadata; waking =
recreate a pod for the same name.

Consequently we **drop the same-bead/new-bead dichotomy.** "Same card" = recreate
this name, reuse `slack_card_ts`, `coop --resume` the conversation. "New card" =
mint a fresh name + fresh card + fresh conversation, pointer back to predecessor.
Both close the predecessor bead. There is exactly one open bead per live name at
any time.

### 2.2 Succession: one field back-pointer, NOT a deps edge

All memos wanted lineage; the viewer critique killed the deps-edge approach
decisively. `RelationshipType` lives on `bead_references` (typed mentions /
free-text), **not** the structured deps table — so "constellation renders it for
free" is false. A real deps row would make succession a **dependency** (walked by
blocked-by, ready-work, and the orphan reaper), so a live head would read as
blocked on its closed predecessor or get reaped.

**Decision:** succession is a single neutral field `prior_agent_id` (predecessor
bead ID), written on *every* recreate — thread respawn, gardener iteration, and
wrapup-successor alike. This unifies the three lineage paths into one chain
abstraction. The viewer gets ONE field-chain walk that synthesizes display-only
edges; the deps graph is untouched.

**Gardener timeline compatibility (the golden-test trap):** `gardener_timeline.go`
requires `len(walk)==size` or it silently falls back to `created_at` ordering, and
`schedule_prior_agent_id` has **four** readers (gardener_timeline.go, gardener.go,
agents_events.go, prime_schedule.go — the last feeds handoff into the next agent's
prompt). We do **not** migrate that field in Phase 1. Instead the viewer reads
`prior_agent_id OR schedule_prior_agent_id`, and the gardener path keeps writing
`schedule_prior_agent_id` as today. Avoids the risky migration and the golden-test
re-bless.

### 2.3 Ownership: the fleet IS the root, gardeners hang under it

Dimension D invented a `fleet:<slug>` gardener forest; its critique showed
`type:fleet` **already exists** (model/bead.go `TypeFleet`, with a `parent_fleet`
field), every agent **already carries a required `fleet`** (the pod token is a
fleet token), and `project` is also required. So "every agent is eventually owned"
is **already structurally true.** We do not add a `type=gardener` bead, a
fleet-gardener, or a `fleet:unowned` sink.

**Decision:**
- The forest is **fleet → gardener → crew/thread**, with fleet-direct agents as a
  sibling lane. Fleet and project are existing required fields; no new root.
- Gardeners stay `type=trigger` + `role:gardener` (as gardener.go documents). We do
  **not** fork gardener identity into a new bead type.
- "Owned by a gardener" = stamp a single canonical `gardener_id` on the agent **at
  spawn time** (where `stampGardenerLoopContext` already writes
  `trigger_id`/`schedule_id`). For ad-hoc spawns (slack-thread, gitlab-mr-review,
  jira-mention), stamp a **per-spawn_source synthetic gardener id** so the
  "Unowned" bucket is **empty by construction** — no GC, no backfill-driven
  reparenting race.
- Enforcement is **soft and at-spawn**, not a write-time gate and not a
  graph-mutating sweeper. If a backstop is ever wanted, it writes a queryable
  `needs-gardener` **label** only (mirroring OrphanSweeper's `needs-intake`), never
  a dep edge.
- Ownership is an **attribution/visibility** invariant, explicitly orthogonal to
  liveness: a gardener-owned agent whose source task closes is still reaped.
  Documented, not prevented.

### 2.4 Persistence lifecycle: closed-bead chain, wake spawner is the real work

We keep agents ephemeral-by-default. "Persistent" = a closed bead carrying
`successor=same` + a wake condition, which a **new controller wake-scanner** (the
genuinely net-new piece, specified but unbuilt in terminal-goals.md) turns back
into a pod reusing the name. This is gated to Phase 2 *because the scanner must
ship together with the thing that clears the wake field and the budget that bounds
it* — none works alone. Phase 1 does not keep any bead open past wrapup.

**Idle-TTL collision (resolved):** `monitorIdleTTL` (init_agent_idle_ttl.go) is
default-on for thread/DM agents and *closes* them — exactly the agents we want
resumable. In Phase 2, `monitorIdleTTL` resolves a `lifecycle` config bead via the
same `ResolveConfigBeads` scope-merge it already uses for idle-ttl; persistent
agents write `successor=same` + wake metadata instead of a plain close. Until
Phase 2 it keeps closing — and `successor=same` is a documented no-op (it strands
nothing because the bead still closes; it just records intent + lineage).

### 2.5 The successor enum, reconciled with everything above

`WrapUp.successor` ∈ `{none, same, new}`, validated at the `gb stop` CLI boundary
(reject unknown):
- `none` (**default**): terminal. Card flips to terminal state. Most crew/thread
  agents.
- `same`: recreate this name after wake; reuse card + `coop --resume`. **Phase 1:
  records intent + `prior_agent_id` + closes bead (no-op until the wake scanner
  exists).** Phase 2: armed.
- `new`: mint fresh name + fresh card + fresh conversation; stamp `prior_agent_id`
  = old bead; inherit role/mode/project/fleet/gardener_id/terminal_goal; seed prompt
  from predecessor `handoff_notes`; close predecessor `close_reason="superseded:
  continued as <id>"`.

**Contradiction handling:** `goal_satisfied=="no"` + `successor=="none"` is
giving-up-while-claiming-progress — warn and treat as `none`, and **do not render
the ⏳ progressing glyph** (which today lies "respawn coming" when none is). One
continuation budget per **lineage root** (not per bead — only per-chain bounds an
infinite self-spawn loop). `successor=new` is **disallowed for agents with no
gardener owner** until ownership lands (automatically satisfied in Phase 1 since
`gardener_id` is always stamped).

## 3. Concrete Changes

### 3.1 Bead schema (fields map — no type migration, same pattern as `terminal_goal`)
- `prior_agent_id` — predecessor bead ID. Written on **every** recreate (thread
  respawn, gardener iteration, successor). The one lineage pointer.
- `lineage_root_id` — first bead in the chain, stamped at mint. Lets the viewer
  group a life-thread by indexed lookup and bound the per-chain continuation
  budget.
- `gardener_id` — canonical owner, stamped **at spawn** for all agents (real gardener
  id, or per-spawn_source synthetic id). Read-only derived projection on
  `BeadDetail` resolving `gardener_id` ∨ `trigger_id`/`schedule_id` (dual-key
  fallback for legacy beads).
- WrapUp JSON gains `successor` (`none|same|new`, empty==none), validated at `gb
  stop`.
- **Phase 2:** `wake_reason` (`timer|mention|assignment|decision|schedule|manual`),
  generalize `next_respawn_at` semantics to "timer wake," extend
  `respawn_count`/`max_respawns` to count all wakes (verify the budget enforcer is
  wired — it currently is not).
- **Explicitly NOT added:** `successor_agent_id` forward pointer (derive by indexed
  query on `prior_agent_id`), a deps-table `succeeds` edge, `owner_gardener` scalar,
  `type=gardener` bead, `fleet:<slug>` gardeners, `successor_count`/`max_successors`
  (one budget, per lineage root).

### 3.2 CLI
- `gb stop --successor=none|same|new` convenience flag → `WrapUp.Successor`;
  `--wrapup` JSON authoritative; reject unknown values at parse time.
- `gb stop` wrapup-expectations output: a Successor section; document handoff_notes
  flow into the successor prompt under same/new.
- `gb agents --no-gardener` — query for `gardener_id` empty AND no
  `trigger_id`/`schedule_id` (should be empty by construction; a leak detector).
- `gb agent adopt <id> --gardener <id>` — manual override; must rewrite `gardener_id`
  **and** reconcile the live card/lineage atomically.
- **Phase 2:** `gb wake <agent>`, optional `gb park --wake-after <dur>`.
- **Deferred/maybe-never:** `kd admin backfill-*` — historical agent beads are far
  more numerous than work beads; only run if a surface actually needs historical
  lineage, with batching/checkpointing.

### 3.3 Bridge
- `bot_agents.go`: branch on `WrapUp.successor` — `same` keeps/edits the live card
  (reuse `slack_card_channel`/`slack_card_ts`; **re-mint if the message was
  deleted/archived**), suppress terminal ✅ until `none`; `new` runs
  `replaceAgentCardWithWrapUp` on the old card, appends `→ continued as <name>`,
  `persistCardRef` on the new bead.
- `stampGardenerLoopContext` (triggers.go): additionally write `prior_agent_id` +
  `gardener_id` at spawn so new gardeners never depend on a backstop.
- Add the resolved gardener (and human owner) to the live agent card footer.
- **Phase 2:** the wake-event path. The **bridge has no K8s deps** — it sets
  `wake_reason`/lineage on the bead; the **controller reconciler** observes and
  spawns. The bridge does not create pods.

### 3.4 Viewer (beads-viewer)
- `agents.go`: `?group=gardener` buckets keyed by canonical `gardener_id` with
  `gardenerRowView` headers; collapse each gardener's chain to the live head with an
  expandable successor history; fleet-direct/project lane as siblings.
- Lineage rendered as a **linear, paginated strip** anchored on the live head (head
  opaque, last K shown, `+N earlier`) — **not** a radial orbit. Constellation is a
  fixed 1000×500 viewBox with `constellationMaxReverse=6`; a long chain would clip
  silently.
- Constellation: succession is a **display-only synthesized edge** from the
  `prior_agent_id` field walk (distinct glyph/legend), kept out of the dep graph.
  Walk reads `prior_agent_id` ∨ `schedule_prior_agent_id`.
- Preserve `EngineCount`/`EnginePruned` framing on the new surfaces so pruned-bead
  gaps in the chain don't read as data loss.
- `user_home.go`: owned-gardeners section to complete `/u/{slug}` → `/gardener/{id}`
  navigation.
- **Phase 2:** `dormant` bucket. Make it **engine-set, not derived**, to avoid
  row-flapping during the respawn window.

## 4. Phased Rollout

### Phase 1 — Minimal, shippable: "new card on wrapup" + easier display
Delivers Matt's headline ask with zero new infrastructure and zero risk to
reconciler/reaper/cache. No bead stays open past wrapup.
- `successor` enum (validated) with `new` and `none` live; `same` records intent
  only (documented no-op until Phase 2).
- `prior_agent_id` + `lineage_root_id` written on every recreate, including `new`.
- `gardener_id` stamped at spawn for all agents (synthetic per-spawn_source for
  ad-hoc) → "Unowned" empty by construction.
- Bridge: successor=new card chaining; gardener footer on card.
- Viewer: `?group=gardener`, linear lineage strip, display-only succession edge,
  owned-gardeners nav.

### Phase 2 — Persistence (the wake spawner)
The genuinely net-new infrastructure, shipped as one atomic unit (scanner +
field-clear + budget + reaper-exclusions).
- Controller **wake-scanner**: scan closed agent beads with `next_respawn_at <=
  now`, recreate pod reusing name, **clear the wake field idempotently** (currently
  nothing clears it — invented invariant, must be built).
- Per-lineage-root continuation budget enforcer (verify/wire `respawn_count`
  enforcement, which is currently unwired).
- `successor=same` armed; idle-TTL `monitorIdleTTL` resolves `lifecycle` config and
  parks-instead-of-closes for persistent agents.
- Context-blowup policy: controller-driven force-`--clean` past a turn/token/wake
  threshold (a long `coop --resume` JSONL eventually fails at boot — silent death
  otherwise).
- `gb wake`; viewer engine-set `dormant` bucket.

### Phase 3 — Full lineage, event wake, hardening
- Event wake (mention/assignment/decision recreate a dormant identity) — bridge
  writes `wake_reason`, controller spawns; events reset TTL, **not** budget (budget
  is the hard thrash backstop).
- Stable human-meaningful `agent_handle` (gardener-scoped, monotonic counter that
  never resets after pruning) to replace `thread-1780422372-432609`.
- Crash-loop guard: a distinct "goal unachievable, stop" signal so
  `successor=new`/`same` doesn't burn pods on a deterministically failing task.
- Optional `needs-gardener` label backstop sweeper (label-only) + `gb agent adopt`.
- Optional historical lineage backfill (batched, only if a surface needs it).

## 5. Top Open Questions for the Operator

1. **Confirm the identity model:** identity is the stable NAME, a bead is one
   incarnation, and a parked agent is a CLOSED bead (not an open dormant one). This
   is the load-bearing decision; everything else follows. (Recommended: yes.)
2. **Is the Phase 2 wake-scanner approved as net-new work?** `terminal-goals.md` is
   still Draft and nothing reads `next_respawn_at` today. `successor=same` is inert
   until this ships. Green-light?
3. **Synthetic per-spawn_source gardeners vs. leaving ad-hoc agents fleet-direct:**
   synthetic gardeners make "Unowned" always empty but add high-cardinality
   pseudo-gardeners (esp. for thread/DM agents). Acceptable, or render thread agents
   under the fleet/project lane instead?
4. **`successor=same` default conversation behavior:** true `coop --resume`
   (continue context) vs. force `--clean` (fresh)? Recommended: resume, with a
   controller-driven clean threshold to avoid context-limit boot failures.
5. **Does `successor=new` get to revise the terminal_goal,** or inherit it
   verbatim? (Gardeners snapshot `schedule_terminal_goal` per run; the successor may
   legitimately want a refined goal.)
6. **Crash-loop policy:** when a task is deterministically unachievable, should
   automatic `new`/`same` continuation be capped purely by the per-lineage budget,
   or require a human ack before re-spawning? (Recommended: budget caps it; surface
   in viewer; human ack only past budget.)
