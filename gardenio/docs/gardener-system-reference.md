# Gardener System Reference (source of truth)

> **This is the single front-door for understanding the gardenio gardener
> system.** It maps the whole thing — the lifecycle, the wake conditions, the
> stop/resume gates, and the meta layer (scorer + watchdog + first mate) — and
> points at the deeper docs for each part. If you are trying to understand,
> debug, author, or supervise a gardener, start here.
>
> **Tracking:** [[ka-0000007485]] (WS-D of epic [[ka-0000002254]] — event-driven
> gardeners + first-mate fleet review). Consolidates ~14 overlapping gardener-\*
> docs into one map and marks the historical snapshots.
>
> **Reflects the post-WS-A/B/C state (2026-08-12):** event-wake + cron-floor is
> the default (WS-A), the resume observability layer landed (WS-B), and the
> first-mate role is designed-but-unbuilt (WS-C).

---

## Contents

- [0. What a gardener is (the primitive)](#0-what-a-gardener-is-the-primitive)
- [1. Lifecycle map: create → run → stop/resume/succeed → retire](#1-lifecycle-map-create--run--stopresumesucceed--retire)
- [2. Wake conditions (what fires a run)](#2-wake-conditions-what-fires-a-run)
- [3. Stop / resume gates (when a run and a loop end)](#3-stop--resume-gates-when-a-run-and-a-loop-end)
- [4. The meta layer (scorer + watchdog + first mate)](#4-the-meta-layer-scorer--watchdog--first-mate)
- [5. Document map (which doc for what — with status)](#5-document-map-which-doc-for-what--with-status)
- [6. Source-code map (where the engine lives)](#6-source-code-map-where-the-engine-lives)

---

## 0. What a gardener is (the primitive)

A **gardener** is a `type=trigger` bead carrying gardener-loop fields, pinned to a
parent ("epic") bead it is driving toward a goal. It periodically spawns an
agent (`action=spawn_agent`) whose job is to advance that epic. Its loop is
fully described by three declarative condition families plus a self-declared
goal:

| Slot | Question it answers | Field on the bead |
|---|---|---|
| **Trigger** | What fires a run? | `triggers:` (JSON array) |
| **Stop** | When does this run end? When does the loop end? | `stop:` (JSON object) |
| **Progress** | Did this run move us forward? Is the loop wedged? | `progress:` (JSON object) + `progress_state` (scheduler-managed) |
| **Goal** | Is the whole thing actually done? | `terminal_goal` (self-declared, the **master gate** — see §3) |

The Go scheduler (`gardenio/controller/internal/scheduler/`) evaluates these; you
author them as bead fields. Since Phase 3b (2026-05-27) `type=trigger` has
absorbed `type=schedule` — a gardener **is** a trigger with loop fields; there is
no separate schedule primitive.

> **Authoring one?** Do not start from this reference — start from the
> **[Gardener Authoring Guide](./gardener-authoring-guide.md)**, the enforced
> front-door (`gb gardener start` refuses to run without
> `--i-read-the-gardener-instructions` and prints its companion advice bead
> inline). This reference explains *how the system works*; the authoring guide
> is the *checklist for creating one*.

---

## 1. Lifecycle map: create → run → stop/resume/succeed → retire

```
                        ┌─────────────────────────────────────────────┐
                        │                                             │
   CREATE ──▶ RUN ──▶ STOP(run) ──▶ [progress?] ──▶ RESUME ───────────┘
   (author)   (spawn   (this pod         │
    a trigger  agent)   ends)            ▼
    + epic)                          [terminal_goal
                                      satisfied?] ──yes──▶ SUCCEED ──▶ RETIRE
                                            │                (or hand
                                            no                off to a
                                            │                 successor)
                                            ▼
                                    RESUME (master gate:
                                    a premature stop is
                                    turned back into a run
                                    or an escalation)
```

| Phase | What happens | Governed by | Doc |
|---|---|---|---|
| **Create** | An operator/agent authors a `type=trigger` bead with gardener fields, pinned to an epic. `gb gardener start` gates on the authoring guide. | authoring guide + `ValidateGardenerBlob` bounds | [authoring-guide](./gardener-authoring-guide.md), [loop-runbook §1](./gardener-loop-runbook.md) |
| **Run** | A trigger fires → one agent pod spawns → does one turn of epic work → wraps up. | `triggers:` (§2) | [loop-runbook §1](./gardener-loop-runbook.md#1-trigger-conditions) |
| **Stop (run)** | The pod ends (`gb stop`, budget, yield-to-decision, terminal goal, …). | `stop:` (§3) | [loop-runbook §2](./gardener-loop-runbook.md#2-stop-conditions) |
| **Resume** | A stopped-but-unfinished gardener is brought back — by the **engine master gate** (alive-and-looping), by the **resumer watchdog** (dormant/wedged), or by the next trigger fire. | `terminal_goal` master gate + watchdog (§3, §4) | [resume-design](./gardener-resume-design-2026-06-09.md) |
| **Succeed** | The gardener declares `terminal_goal` satisfied — the only legitimate terminal stop. May first spawn a **successor** (research → impl → improvement → regression). | terminal_goal + succession (§3, §4) | [resume-obs+succession](./gardener-resume-observability-and-succession-plan.md) §2, [chaining](./gardener-chaining-design-2026-06-18.md) |
| **Retire** | A finite gardener whose frontier is empty goes dormant/closed; the meta-scorer flags STALE-STOP candidates; the first mate (proposed) manages orderly retirement/creation. | scorer + first mate (§4) | [effectiveness-rubric](./gardener-effectiveness-rubric.md), [first-mate design](./first-mate-fleet-role-design-2026-06-11.md) |

The three condition families answer the *mechanical* lifecycle (fire / end /
progress); the **master gate** (`terminal_goal`) plus the **meta layer** answer
the *teleological* one (is it done, is it stuck, should it live).

---

## 2. Wake conditions (what fires a run)

The `triggers:` field is a JSON array; each entry is one trigger source with its
own match/preflight/throttle. Any one entry firing produces one run (fan-in).

| `kind` | Fires when… |
|---|---|
| `cron` | The cron expression matches **and** `preflight` is true at fire time. |
| `bead_event` | A bead transitions (`closed` / `updated` / `created`, incl. edge events: comment / dependency / label add) **and** the payload satisfies `match`. |
| `decision_resolved` | A decision bead resolves **and** its payload satisfies `match`. |
| `manual` | An operator fires it (`gb schedule fire <id>` / `gb gardener` CLI). |
| `mail_received` / `webhook` / `slack_reaction` | Reserved; deferred. |

**The default shape post-WS-A is hybrid: event-wake + a slow cron floor.** The
gardener wakes *responsively* on the event that actually matters (an MR goes
green, an escalation resolves, a child of its epic closes) and keeps a slow cron
as a backup catch-up floor — not as its primary heartbeat. This closed the
"idle gardener" discoverability gap: 19/20 open gardeners were cron-only even
though the engine already supported event wake.

**Recipes (copy-paste JSON with exact `match` shapes):**
- Canonical cookbook: **[loop-runbook §8](./gardener-loop-runbook.md#8-wake-condition-recipe-cookbook)** — MR-green, escalation-resolved, decision-answered, child-close, hybrid event+cron-floor, graph-edge change, and "what does NOT work yet."
- Advice bead: **[[ka-0000001467]]** ("Gardener wake-condition recipes").
- **Payload-shape rule (read first):** [loop-runbook §8.1](./gardener-loop-runbook.md#81-the-payload-shape-rule-read-this-first) — a `match` against the wrong payload shape is a silent no-op.

Deeper: [loop-runbook §1](./gardener-loop-runbook.md#1-trigger-conditions),
[triggers.md](./triggers.md).

Historical context for how the fleet got here (point-in-time snapshots, now
absorbed into the runbook): the wake-conditions assessment, the two 2026-06-04
conversion reports and the wake survey were deleted from `main` on 2026-10-31;
they remain at git tag `archive/docs-prune-pre-delete-2026-10-31`.

---

## 3. Stop / resume gates (when a run and a loop end)

A gardener run can **stop** for six reasons; only one means "done."

| Stop reason | Terminal? |
|---|---|
| `terminal_goal` satisfied (self-declared done) | **Yes — the only legitimate terminal stop.** |
| `gb stop` with more work pending | No — premature |
| `run_budget` / `max_runs` exhausted | No — premature |
| Yield-to-decision / escalation (parked) | No — resumes on resolution |
| Legacy `loop_end` condition | No — no longer hard-closes |
| Crash / involuntary death | No |

**The master gate (`terminal_goal`).** A gardener may only truly terminate when
it has declared its goal satisfied. A *premature* stop (any of the other five)
is turned back into **resume-or-escalate**: the engine self-heals gardeners that
are *alive and looping*; the **resumer watchdog** (§4) covers what the engine
can't see from inside its own loop — gardeners that are `enabled=false`, were
`closed` by legacy budget exhaustion, or are otherwise dormant-but-unfinished.
Design: [gardener-resume-design-2026-06-09.md](./gardener-resume-design-2026-06-09.md).

**Observability (WS-B, landed).** Resume decisions are now visible:
- `gardener_resume_total` CounterVec + `gardener_resumes_without_progress` gauge
  (`gardenio/controller/internal/bridge/gardener_resume_metrics.go`).
- `/gardeners` surfaces a Resume pill (`resumes_without_progress/resume_budget`),
  a stuck badge (`gardener-stuck-escalate` label), and last-run `goal_satisfied`.
- Plan-of-record: [gardener-resume-observability-and-succession-plan.md](./gardener-resume-observability-and-succession-plan.md) §1.

**Stop-condition schema.** The `stop:` object and the two axes (progress-aware
bound vs. engine backstop) are documented in
[loop-runbook §2](./gardener-loop-runbook.md#2-stop-conditions). The design
rationale — *why* the two gardener systems needed reconciling — is the historical
[stop-conditions review](./gardener-stop-conditions-review-2026-06-07.md).

**Succession.** Before a gardener succeeds, it may spawn a **successor** so a
long arc (research → impl → improvement → regression-checking) continues without
an operator re-launching each phase. The declarative `successor` block +
`succeeds:` provenance is plan-of-record in
[resume-obs+succession §2](./gardener-resume-observability-and-succession-plan.md);
the general case (multi-link chains) is
[gardener-chaining-design-2026-06-18.md](./gardener-chaining-design-2026-06-18.md).

---

## 4. The meta layer (scorer + watchdog + first mate)

Three supervisors sit *above* the gardeners. Two are **reflexes** (built, live);
one is **deliberation** (designed, gated on operator sign-off). They share epic
[[ka-0000007687]] and are explicitly **non-overlapping**.

| Supervisor | Reflex or deliberation | What it does | Status | Prompt / design |
|---|---|---|---|---|
| **Meta-scorer** (gardener-of-gardeners) | Reflex | Twice a day, *grades* every live gardener against the effectiveness rubric; makes + enforces tuning suggestions on schedules/triggers/prompts; flags STALE-STOP retire candidates. | Live | [meta-scoring-prompt](./gardener-meta-scoring-prompt.md) + [effectiveness-rubric](./gardener-effectiveness-rubric.md) |
| **Resumer watchdog** | Reflex | The opposite reflex: finds gardeners that are **stuck-but-unfinished** (goal unmet AND not live) and *revives* them, escalating only when genuinely wedged. | Live | [resumer-watchdog-prompt](./gardener-resumer-watchdog-prompt.md) |
| **First mate** | Deliberation | An *active supervisor* that judges a gardener's **plan trajectory** (progressing toward `terminal_goal` vs. thrashing / off-track), smooths **succession**, and manages orderly **retirement / creation**. | **Designed, not built** (proposal; go-live gated on operator sign-off) | [first-mate design](./first-mate-fleet-role-design-2026-06-11.md) + [first-mate prompt v0](./gardener-first-mate-prompt.md) |

**The division of labor:** the scorer and watchdog are *reflexes* — they react
to a gardener's current state (weak → grade/retire; stuck → revive). The first
mate is the fleet's first *deliberative* supervisor — it judges **trajectory**,
not instantaneous activity, and composes with the reflexes without duplicating
them (it decides *whether the plan is on track*; the reflexes decide *whether
the loop is alive/effective*). See the first-mate design's non-overlap contract
for the exact boundary.

---

## 5. Document map (which doc for what — with status)

**Status legend:** **Canonical** = living source of truth, keep current ·
**Proposal** = designed, awaiting build/operator sign-off · **Historical
snapshot** = point-in-time survey/report, absorbed into a canonical doc; read
for provenance only.

### Understand / operate the loop
| Doc | Status | Covers |
|---|---|---|
| **gardener-system-reference.md** (this doc) | Canonical | The whole-system map + this index. |
| [gardener-loop-runbook.md](./gardener-loop-runbook.md) | Canonical | The DSL bible: `triggers:` / `stop:` / `progress:`, shared condition DSL, worked examples, debugging, scheduler internals, wake-recipe cookbook (§8). |
| [gardener-authoring-guide.md](./gardener-authoring-guide.md) | Canonical | Enforced front-door for *creating* a gardener (self-block, cadence, recipes, repo-less rule, try-before-escalate). |

### Wake conditions (§2)
The wake-conditions snapshots (assessment, survey, classification and the two
2026-06-04 conversion reports) are absorbed into
[gardener-loop-runbook.md §8](./gardener-loop-runbook.md) and live at git tag
`archive/docs-prune-pre-delete-2026-10-31`.

### Stop / resume / succession (§3)
| Doc | Status | Covers |
|---|---|---|
| [gardener-resume-design-2026-06-09.md](./gardener-resume-design-2026-06-09.md) | Canonical | `terminal_goal` master gate + resumer watchdog design. |
| [gardener-resume-observability-and-succession-plan.md](./gardener-resume-observability-and-succession-plan.md) | Canonical | WS-B obs layer (§1, landed) + succession `successor`/`succeeds:` (§2). |
| [gardener-chaining-design-2026-06-18.md](./gardener-chaining-design-2026-06-18.md) | Canonical | Chaining as the general case of succession (`chain.to`). |
| [gardener-stop-conditions-review-2026-06-07.md](./gardener-stop-conditions-review-2026-06-07.md) | Historical snapshot | The "two gardener systems in one codebase" review that motivated the resume epic. |

### Meta layer (§4)
| Doc | Status | Covers |
|---|---|---|
| [gardener-effectiveness-rubric.md](./gardener-effectiveness-rubric.md) | Canonical | How to score a gardener (D1–D6); STALE-STOP vs DORMANT-OK. |
| [gardener-meta-scoring-prompt.md](./gardener-meta-scoring-prompt.md) | Canonical | The exact prompt for the meta-scorer gardener. |
| [gardener-resumer-watchdog-prompt.md](./gardener-resumer-watchdog-prompt.md) | Canonical | The exact prompt for the resumer-watchdog gardener. |
| [first-mate-fleet-role-design-2026-06-11.md](./first-mate-fleet-role-design-2026-06-11.md) | Proposal | First-mate role design (trajectory verdict, placement, non-overlap, succession, retire/create). |
| [gardener-first-mate-prompt.md](./gardener-first-mate-prompt.md) | Proposal (v0) | First-mate prompt playbook; go-live gated. |

### Config surface / viewer / audits (adjacent)
| Doc | Status | Covers |
|---|---|---|
| [gardener-config-builder-schema-spec.md](./gardener-config-builder-schema-spec.md) | Canonical | Authoritative reverse-engineered schema a builder UI must cover + guided-builder design. |
| [gardener-config-cartographer-prompt.md](./gardener-config-cartographer-prompt.md) | Canonical | Config-cartographer gardener prompt playbook. |
| [gardener-page-v2-design.md](./gardener-page-v2-design.md) | Canonical | `/gardeners` + `/gardener/{id}` v2 design (R1–R6). |
| [gardener-page-test-plan.md](./gardener-page-test-plan.md) | Canonical | Test plan-of-record for the gardener viewer surfaces. |
| [gardener-spec-less-manifest-2026-06-15.md](./gardener-spec-less-manifest-2026-06-15.md) | Historical snapshot | Derived run-counts for legacy spec-less gardeners (backfill input). |

> Historical-snapshot docs carry a banner at their top pointing back here. The
> numbers/dates in them are true as of their snapshot date, not necessarily
> today. Snapshots no live doc or code cites were deleted on 2026-10-31 and
> remain at git tag `archive/docs-prune-pre-delete-2026-10-31`.

---

## 6. Source-code map (where the engine lives)

| Concern | Path |
|---|---|
| `gb gardener start` CLI + gate | `gardenio/controller/cmd/gb/gardener.go` |
| Gardener blob parse + `GardenerSpec` | `gardenio/controller/internal/bridge/triggers.go` |
| Bounds guard | `bridge.ValidateGardenerBlob` |
| Fire-time evaluation | `gardenio/controller/internal/bridge/gardener_evaluator.go` |
| Stop evaluator | `gardenio/controller/internal/scheduler/gardener_stop.go` |
| Wake-condition DSL | `gardenio/controller/internal/scheduler/conditions/{expression,leaf_daemon}.go` |
| Trigger sources | `gardenio/controller/internal/bridge/trigger_source_{schedule,multi,schedule_mirror}.go`, `cmd/trigger-engine/main.go` |
| Resume metrics | `gardenio/controller/internal/bridge/gardener_resume_metrics.go` |
| Viewer surfaces | `gardenio/controller/cmd/beads-viewer/` (`/gardeners`, `/gardener/{id}`) |

---

*Maintained as the gardener-system source of truth. When a gardener-\* doc changes
status (built → shipped, proposal → canonical, canonical → historical), update
its row in §5 and its banner.*
