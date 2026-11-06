# Loop Runbooks

Versioned operating contracts for gardenio's recurring gardener loops.

This directory holds two kinds of documents:

- **One-off ops runbooks** (the pre-existing files: `s3-resume.md`, …) —
  procedures a human or agent follows once when a specific situation arises.
- **Loop runbooks** (this README + `intake-groomer.md`,
  `tarboat-regression-gardener.md`, `ced-session-inspector.md`, …) — the standing
  operating contract for a scheduled gardener loop. The trigger bead's prompt is a
  short pointer here; the repo owns the loop's policy.

## Why loop contracts live here, not in trigger prompts

Reference: the continuous-maintenance thesis (lopopolo/harness-engineering) —
**the scheduler stays thin; the repo owns the loop contract.** Migration tracked
in task ka-0000007116 under epic ka-0000007152.

Keeping a multi-KB operating contract inline in a trigger bead's `prompt` field
has three failure modes this directory fixes:

1. **No diffable review.** Policy edits to a trigger prompt are a `kd update`
   nobody reviews. Here, a policy change is an MR with a diff, a reviewer, and
   git history.
2. **Trigger-bead churn loses the contract.** Triggers get disabled, re-created,
   and re-authored on run-budget exhaustion; each re-authoring risks dropping a
   hard-won guardrail (most guardrails in these contracts were paid for with a
   real incident). The runbook survives the churn.
3. **Respawned agents read stale copies.** An agent resumed mid-loop follows
   whatever prompt it was spawned with. A runbook read fresh from `origin/main`
   at each wake is always current.

## What stays in the trigger prompt

The trigger prompt shrinks to a pointer plus the minimum an agent needs when
the runbook is unreachable:

- identity (which loop it is, anchor epic, trigger bead id),
- the command to read the runbook from `origin/main`,
- the fail-safe (if the runbook is unreadable: squawk + `gb stop`, do **not**
  improvise the mission from memory),
- the never-disable-your-own-trigger rule.

Scheduling mechanics (cron, conditions, stop conditions, run budgets) also stay
on the trigger bead — they are the scheduler's, not the contract's. How to
*author* those fields (the `triggers:` / `stop:` / `progress:` condition
families) is documented in
[`../gardener-loop-runbook.md`](../gardener-loop-runbook.md); this directory is
about what the loop should *do* each wake, not how the scheduler wakes it.

### Pointer-prompt skeleton

```
You are the <loop name> (trigger <trigger-id>, anchor epic <epic-id>).

Your full operating contract is the versioned runbook
gardenio/docs/runbooks/<loop>.md in the gardenio monorepo.

FIRST ACTION every wake — read the CURRENT contract from origin/main
(your working copy may be stale):

    git fetch origin main --quiet && git show origin/main:gardenio/docs/runbooks/<loop>.md

Then execute it top to bottom.

If the runbook is unreadable this wake (fetch fails, file missing): do NOT
improvise the mission from memory. gb squawk the failure, then FIRST
kd close <your-claimed-cycle-task-id> (or kd unclaim <id> if handing off
incomplete) — the despawn gate REJECTS a gb stop while you still hold claimed
in-progress work — and THEN
gb stop --wrapup '{"tldr":"runbook unreadable — no-op cycle","accomplishments":"none","progress_score":0,"progress_summary":"could not read gardenio/docs/runbooks/<loop>.md from origin/main"}'.

NEVER disable your own trigger; only the operator does that.
```

## The runbook template — five questions

Every loop runbook answers the same five questions, in order, before its
operational playbook. If a question has no crisp answer, the loop is not ready
to run unattended.

1. **Condition to keep true** — the invariant the loop exists to maintain.
   Not "what the loop does" but the state of the world that holds while the
   loop is healthy.
2. **Drift signal** — how you detect the condition has stopped being true
   (or the loop itself is wedged). Concrete, checkable signals.
3. **Restoring evidence** — what proves a cycle actually restored the
   condition. Verified evidence, not self-report: exit codes, KPI deltas,
   closed beads, green runs.
4. **Autonomous-vs-approval boundary** — what the loop does on its own
   authority vs. what requires a `gb decision` / `gb escalation`. Includes the
   hard "never touch" list.
5. **Durable state** — where the loop's memory lives between runs (beads,
   labels, comments, fields), so a fresh agent can reconstruct context from
   the data plane alone.

After the five questions, the runbook carries the **cycle playbook** (the
step-by-step each wake), **wrap-up contract** (required `gb stop --wrapup`
fields), and a **provenance** footer (which trigger bead it was migrated from,
when, by which task).

The wrap-up contract must state **close-before-stop**: the agent closes its
claimed cycle bead (`kd close <cycle-task-id>`, or `kd unclaim` when handing off
incomplete) *before* calling `gb stop`. Scheduled agents are spawned holding a
claimed `task_id`, and the despawn gate rejects `gb stop` while that claim is
open — omitting this costs every cycle a wasted reject-close-retry round trip
(bug ka-0000007846). The only exception is `gb yield`, which resumes the same
cycle and must *not* close the bead.

## Selection: first three runbooks (by run frequency)

Measured from `runs_spawned` on enabled trigger beads, 2026-07-19. The two
highest raw counters — ka-0000002685 (81 runs) and ka-0000005655 (59 runs) —
are instances of the shared Go prompt templates
(`gardenio/controller/cmd/gb/gardener.go` `gardenerPlaybookPrompt`,
`gardenio/controller/internal/plangardener/plangardener.go` `PlaybookPrompt`),
which are already versioned in this repo; they are not migration targets.
The top three *bespoke* contracts (hand-authored prose living only in the
trigger bead) are:

| Runbook | Trigger | Cadence | Runs |
|---|---|---|---|
| [`tarboat-regression-gardener.md`](tarboat-regression-gardener.md) | ka-0000001347 | hourly (`19 * * * *`) | 63 |
| [`intake-groomer.md`](intake-groomer.md) | ka-0000006210 | every 30m (`3,33 * * * *`) | 48 |
| [`triage-intake-janitor.md`](triage-intake-janitor.md) | ka-0000003783 | every 30m (`17,47 * * * *`) | 60 |
| [`ced-session-inspector.md`](ced-session-inspector.md) | ka-0000004081 | every 6h (`13 */6 * * *`) | 21 |

## Editing a loop runbook

1. Feature branch + MR, like any repo change (`docs(gardenio/runbooks): …`).
2. Policy changes take effect on the loop's **next wake** after merge — the
   pointer prompt reads `origin/main`, so no trigger-bead edit is needed.
3. If you change the *scheduling* (cron, stop conditions, budgets), that is a
   trigger-bead edit (`kd update <trigger-id> --field …`), not a runbook edit.
4. Keep guardrails' incident citations (bead ids) — they are the "why" that
   stops a future editor from deleting a load-bearing rule.
