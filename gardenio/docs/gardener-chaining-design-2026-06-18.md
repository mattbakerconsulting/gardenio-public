# Gardener Chaining — design research

**Tracking:** ka-0000004188 · **Operator:** Matt Baker · **Thread:** thread-1781729756-864039
**Builds on / does not re-derive:**
- `plans/trigger-schedule-unification.md` — Phase 3b: `type=trigger` absorbs `type=schedule`. A gardener = a trigger + loop fields.
- `docs/gardener-resume-observability-and-succession-plan.md` §2 — **gardener succession** is already plan-of-record (`successor` block, `succeeds:` provenance, engine-side vs agent-side execution).
- `docs/triggers.md` — `trigger:bead` (`bead_event`: created/updated/closed) and `decision_resolved` event sources.
- `docs/gardener-loop-runbook.md` — `triggers:` / `stop:` / `progress:` condition families; the closed-root short-circuit (ka-0000008051).

> **One-line thesis:** chaining is NOT a new primitive. It is the intersection of three things we already have or have planned — gardener **succession** (§2 of the resume plan), the **`bead_event`/`decision_resolved`** trigger sources (choreography substrate), and the **card-graph** (the surface that finally makes a chain *visible*). The design job is to pick the orchestration model, give it one authoring shape, and render the chain as edges.

---

## 1. The framing: orchestration vs. choreography

The distributed-systems literature splits multi-step workflows into two coordination models, and our two existing primitives map onto them exactly:

| Model | Coordination lives… | Our analogue | Strength | Weakness |
|---|---|---|---|---|
| **Choreography** | Decentralized. Each gardener reacts to *events* others emit; nobody is the conductor. | `trigger:bead` on `bead.closed` of the predecessor's epic; `decision_resolved`. | Loose coupling, resilience, gardeners stay autonomous, zero new engine authority. | **The chain is implicit and invisible** — you trace it by reading logs. Risk of accidental cycles / infinite loops. |
| **Orchestration** | Centralized. A controller drives the sequence and the handoff. | A `successor` block the engine executes in `gardenerTerminate`; or a top-level "workflow" bead that owns the DAG. | One place to read the whole flow, simple monitoring, clean error handling / compensation. | Central bottleneck; the engine gains "create a gardener" authority (bigger blast radius). |

Source framing: [Camunda — Orchestration vs Choreography](https://camunda.com/blog/2023/02/orchestration-vs-choreography/), [Serverless Land](https://serverlessland.com/event-driven-architecture/choreography-and-orchestration), [API7](https://api7.ai/blog/service-orchestration-vs-service-choreography).

**The 2025 consensus is hybrid:** lightweight orchestration at the edge (declare the intended chain) over robust event-driven choreography in the core (the actual handoff fires on real events). That maps directly onto our recommendation below.

---

## 2. Why this is mostly already built

Three observations collapse the scope:

1. **Succession already exists as a plan.** `gardener-resume-observability-and-succession-plan.md` §2 already specifies a `successor` block (`phase`, `terminal_goal`, `prompt_advice`, `epic`, `inherit`, `when: goal_satisfied`), `succeeds: <predecessor>` provenance, and the engine-vs-agent execution question. **Chaining is succession generalized from "1 heir" to "a named edge in a graph."**

2. **The choreography substrate already exists — and is live.** `trigger:bead` fires on `bead.closed`; `decision_resolved` fires on a resolved decision. "Gardener B starts when Gardener A finishes its goal" is literally *"Gardener B is a `trigger:bead` watching the epic Gardener A closes."* `BeadStateTriggerSource` (and `ScheduleTriggerSource`) are **registered & live in the prod trigger-engine** (commits `6c18f1344` / `969040082`, on `main` since 2026-05-10), so **choreographed chaining works today with zero new code.** Phase 1 has shipped; only `AccountTriggerSource` and `SlackReactionTriggerSource` remained unregistered, and registering `AccountTriggerSource` (C0) does not affect chaining — `bead_event` already fires.

3. **The card-graph is the missing organ — visibility.** Choreography's one fatal weakness is that the chain is invisible; you debug it by grepping logs across services. The card-graph *is* the fix: a `succeeds:`/`triggered-by` edge between two gardener cards turns an implicit event-chain into a drawn arrow. This is the single biggest reason to do chaining *on* the card-graph rather than as a side feature.

So: **don't build a workflow engine. Promote succession to a first-class, multi-link, visible relationship, and let the card-graph render it.**

---

## 3. Recommended model: declarative chain, event-driven fire (hybrid)

### 3.1 The relationship

One new typed relationship between gardener (trigger) beads: **`chains_to`** (forward) / **`succeeds`** (reverse, already in the succession plan). Each is a typed mention → a graph edge in beads-viewer for free (per global advice ka-0000007977).

A gardener's chain is declared on its bead (the orchestration / edge half):

```json
"chain": {
  "to": {
    "phase": "impl",
    "terminal_goal": "All children of {{epic}} merged & green.",
    "prompt_advice": "kd-…",          // successor's gardener prompt
    "epic": "{{same}}",                // default: inherit predecessor's epic
    "inherit": ["project","slack_channel","operated_by","slack_thread"],
    "role": "gardener"
  },
  "on": "goal_satisfied",              // WHEN: predecessor's terminal gate = done
  "mode": "spawn"                       // spawn heir now | arm a dormant successor
}
```

### 3.2 How it actually fires (the choreography half)

Two execution sites, exactly mirroring the succession plan's open question §2d:

- **v0 — agent-side (no new engine authority).** In the predecessor gardener's final run, the wrap-up nudge tells it: *"goal met and you declared a `chain.to` → create the successor gardener bead now, stamp `succeeds: <self>`, enable it, then `gb stop`."* The successor wakes on its own preflight (a `bead_event` watching the now-closed epic, or just enabled cron). Pure choreography; engine stays dumb. **This is the v0 we ship and dogfood from this thread.**
- **v1 — engine-side (convenience, once proven).** The engine, in `gardenerTerminate`, sees `chain.to` + terminal `goal_satisfied` and materializes the successor itself. Zero agent latency, but the engine gains create-gardener authority. Defer until v0 proves the shape.

Either way the **trigger** that wakes the heir is a real event (`bead.closed` / `decision_resolved`), so we get choreography's resilience; the **declaration** (`chain.to`) gives us orchestration's visibility. Hybrid.

### 3.3 Beyond a single heir — workflows

The user asked for "**chaining gardeners in workflows**," not just A→B. `chains_to` being a typed edge (not a single `successor` field) gets us there:

- **Linear pipeline:** research → impl → regression-check → docs. Each declares one `chain.to`.
- **Fan-out:** one research gardener's done-gate arms *N* impl gardeners (one per sub-epic). `chain.to` becomes an array, or N gardeners each declare `succeeds: research`.
- **Fan-in / join:** a "ship" gardener that waits for *all* of {impl-A, impl-B, impl-C} to close. Expressed as its **preflight** `bead_query`: `{op:"==", value:"0", parent:<umbrella-epic>}` — it simply won't fire until the subtree is empty. We already have the closed-root short-circuit for exactly this (ka-0000008051). **Joins need no new primitive — they're a preflight condition.**
- **Conditional branch:** research gardener closes with `decision_resolved` → branch A or B gardener wakes on the matching decision key. `decision_resolved` match already supports this.

### 3.4 The guard rails (choreography's known hazards)

The literature flags two failure modes; both need explicit guards:

1. **Cycles / infinite loops.** A→B→A is a live gardener factory. Guard: a chain-depth / cycle check at successor-creation time — walk the `succeeds:` chain; refuse to create an heir that already appears as an ancestor within the same workflow root. Cap total chain length (configurable, default e.g. 8). Surface a loud warning + a `gb decision` to the operator rather than silently spawning.
2. **Invisible drift.** Mitigated structurally by the card-graph: every link is an edge; a "workflow" filter on `/triggers` shows the whole chain rooted at the workflow bead. The summary bar gets a "dangling chain" health pill (a `chain.to` whose heir never materialized).

---

## 4. How it renders on the card-graph (ties back to the unify epic)

This is where chaining *pays for* the card-graph rather than bolting onto it:

- **Gardener cards** (lit-halo trigger cards) connected by **`chains_to` edges** — a directed arrow from predecessor → heir. The arrow is dim while the predecessor is mid-loop, **lights up at the moment of handoff** (predecessor `goal_satisfied`), and the heir's halo ignites.
- A **workflow** is just the connected component of `chains_to` edges — selectable, nameable, collapsible to a single super-node ("Trigger→Unify Workflow: 4 gardeners, phase 2/4").
- **Authoring** (the "biggest" idea from the unify riff): drag from one gardener card's *right edge* to empty space → "create successor gardener" with `succeeds:` pre-stamped. The chain is *drawn*, then serialized to `chain.to` on the bead. Same card to read, author, debug — now including the chain.

---

## 5. Phasing (slots under the unify epic)

| Phase | What | Depends on |
|---|---|---|
| **C0** | Finish unification **Phase 1**: `BeadStateTriggerSource` + `ScheduleTriggerSource` already registered & live (since 2026-05-10) — register the remaining `AccountTriggerSource` and add a smoke proving `bead_event` choreographs end-to-end. Choreographed chaining already fires. | Unify Phase 1 |
| **C1** | `chain.to` field schema + `chains_to`/`succeeds` typed mentions + agent-side v0 (wrap-up nudge creates heir) + cycle/depth guard. | C0 |
| **C2** | Card-graph renders `chains_to` edges + handoff animation + workflow connected-component selection. | C1 + card-graph epic |
| **C3** | Authoring: drag-to-create-successor; fan-out array; join via preflight `bead_query`. | C2 |
| **C4** | Engine-side v1 succession (convenience), once v0 is proven in prod. | C1 |

---

## 6. The live test (per operator)

> *"this should really be the first instance of chaining gardeners, we could do like 4 and monitor from this thread."*

The unify epic itself **is** the 4-gardener chain — we dogfood the feature by building the feature:

1. **research** gardener → produces the unify design (this doc + the epic). Terminal goal: design epic + children created & approved.
2. **impl** gardener → finishes Phase 1 (register the remaining `AccountTriggerSource` + choreography smoke; bead/schedule sources already live) + `chain.to` schema. `succeeds: research`. Terminal: Phase 1 + C1 merged & green.
3. **ux** gardener → builds the `/triggers` console + card-graph chain rendering. `succeeds: impl`. Terminal: `/triggers` shipped, chain edges render.
4. **verify** gardener → live regression check + UX test of the whole chain, reports back to this thread. `succeeds: ux`. Terminal: end-to-end chain demonstrably fires; report posted.

Each hands off on `goal_satisfied`; we watch the batons pass live in this Slack thread. The 4-gardener chain is both the deliverable and the proof.

---

## 7. Open questions for the design epic

1. **`chain.to` vs `successor`** — reconcile naming with the existing succession plan. Recommend `chain.to` supersedes/renames `successor` (chaining is the general case; single-heir succession is `chain.to` with one link). **Resolved at C1 build time — see §7.1.**
2. **Cycle policy** — hard-refuse vs operator-decision-on-cycle. Lean: refuse + decision. **Resolved at C1 build time — see §7.2.**
3. **Inheritance defaults** — which fields auto-inherit (epic, project, slack thread, operator) vs must be re-declared. **Resolved at C1 build time — see §7.3.**
4. **Workflow identity** — does a workflow get its own bead (orchestration anchor) or is it purely the emergent connected component (pure choreography)? Lean: emergent for v0, optional anchor bead for naming/collapse in v1.

### 7.1 `chain.to` vs `successor` — RESOLVED (C1)

`chain.to` and the succession plan's `successor` block (`gardener-resume-observability-and-succession-plan.md` §2a) are **the same schema, not two competing ones.** Field-by-field they are identical:

| `successor` (§2a) | `chain.to` (§3.1) | Note |
|---|---|---|
| `phase` | `to.phase` | identical |
| `terminal_goal` | `to.terminal_goal` | identical |
| `prompt_advice` | `to.prompt_advice` | identical |
| `epic` | `to.epic` | identical (`{{same}}` default) |
| `inherit` | `to.inherit` | identical |
| `when: "goal_satisfied"` | `chain.on: "goal_satisfied"` | renamed `when`→`on`, same value/semantics |
| *(implicit single heir)* | `chain.mode: "spawn"\|"arm"` | new; `spawn` == §2a behavior |
| *(none)* | `to.role` | new; defaults to `gardener` |

**Decision: `chain.to` is the canonical schema; `successor` is the single-heir special case.** A `successor` block is exactly a `chain` with one `to` and `on: goal_satisfied`. The C1 Go type (`ChainSpec`/`ChainTo` in `internal/bridge/triggers.go`) is the one parser; the field key on the bead is `chain`. Provenance is the **same** `succeeds: <predecessor>` typed mention both documents already specified — now a real graph edge via `model.DepChainsTo` (forward verb `chains_to`, inverse `succeeds`). No migration is needed because `successor` was never shipped (it was plan-only); C1 lands `chain` directly. The succession plan's §2 mechanism (engine-side `gardenerTerminate` heir creation) remains the **v1** execution site (design §3.2); C1 ships only the **v0 agent-side** path, so no engine authority is added.

### 7.2 Cycle / depth policy — RESOLVED (C1): refuse + decide

`ChainHeirGuard` (`internal/bridge/gardener_chain.go`) walks the `succeeds:` chain from the predecessor and **refuses** to instruct heir creation when (a) the heir's identity already appears as an ancestor (`A→B→A`), (b) adding the heir would exceed `maxChainDepth` (default **8**), or (c) the stored chain already loops. On refusal the v0 wrap-up nudge does **not** tell the agent to create the heir; it tells the agent to open a `gb decision` describing the heir + guard verdict and then `gb stop` — i.e. *refuse-and-decide*, never silently spawn (matches §3.4 guard rail #1). Identity is matched on `prompt_advice` first (the stable handle for "which gardener this is"), falling back to `phase|epic`.

### 7.3 Inheritance defaults — RESOLVED (C1)

When `to.inherit` is omitted, `ChainSpec.Normalize()` applies the default set **`["project", "slack_channel", "operated_by", "slack_thread"]`** — the bindings an heir almost always needs to keep the workflow coherent (kept in `defaultChainInherit`). `to.epic` defaults to the predecessor's epic when empty or `{{same}}`; `to.role` defaults to `gardener`. Authors override any of these by declaring them explicitly.
