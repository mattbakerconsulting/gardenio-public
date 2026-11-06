# How to work a PE ticket to completion — from Slack

You have a JIRA **PE-XXXX** ticket in the Monorepo project and you want it driven
to a **merge-ready, reviewer-tagged MR** without babysitting it. There is one
slash command for that:

```
/formula pour ka-0000007107 --var TICKET=PE-XXXX
```

> The formula is registered live as **`ka-0000007107`** ("PE Ticket to Completion";
> definition checked in at [`gardenio/docs/pe-ticket-to-completion-formula.json`](pe-ticket-to-completion-formula.json)).
> `/formula list` shows it too, but you can paste the id above directly.

That single command stands up an autonomous, self-checking workflow: a crew agent
starts the fix, three adversarial **critic gates** refuse to let weak work through,
and a **per-ticket gardener** drives the loop until everything is green and signs
off before a human reviewer is ever pulled in.

This doc covers: pouring the formula from Slack, what the 3 critics check, what the
gardener does, and the end-to-end flow from PE ticket → completed MR.

- [TL;DR — the one command](#tldr--the-one-command)
- [Pour vs. wisp, and the optional vars](#pour-vs-wisp-and-the-optional-vars)
- [What the pour creates](#what-the-pour-creates)
- [The 3 critic gates](#the-3-critic-gates)
- [The per-ticket gardener](#the-per-ticket-gardener)
- [End-to-end flow](#end-to-end-flow)
- [How to monitor it](#how-to-monitor-it)
- [When the gardener signs off (and when YOU sign off)](#when-the-gardener-signs-off-and-when-you-sign-off)
- [Cheat sheet](#cheat-sheet)

---

## TL;DR — the one command

In any Slack channel where the gardenio bot is present:

```
/formula list
```

Find **"PE Ticket to Completion"** in the list and copy its `kd-…` id — it is
**`ka-0000007107`**. Then:

```
/formula pour ka-0000007107 --var TICKET=PE-7289
```

That's it. The pour:

1. instantiates a **persistent molecule** (a live bead subtree, one bead per step),
2. records a *triggered-by* edge from the molecule back to your Slack thread, and
3. **auto-spawns a crew agent** (`role=crew`) that begins working the ticket —
   starting with research/triage, exactly as Matt asked.

You do not need to spawn anyone. Pouring the formula *is* the kick-off.

> **First time using it?** Run `kd formula show <id>` (CLI) or `/formula show <id>`
> (Slack) first — the formula's `description` field renders a full blurb explaining
> what gets created and how invocation works, so you can confirm you have the right
> formula before pouring.

---

## Pour vs. wisp, and the optional vars

The PE-Ticket-to-Completion formula is **`phase: liquid`**, which means you
**pour** it (persistent molecule that lives in beads and is browsable in
beads-viewer). The other phase, `vapor`, is what `/formula wisp` produces — an
ephemeral run that leaves no durable subtree. For working a real ticket to a real
MR you always want **pour**, never wisp.

`--var TICKET` is **required**. Two more vars are **optional** and only matter on a
re-pour or if you already started by hand:

| Var | Required? | Use it when… |
|---|---|---|
| `TICKET` | **yes** | always — e.g. `--var TICKET=PE-7289` |
| `BRANCH` | no | you already have a branch, e.g. `--var BRANCH=agent/me/fix-pe-7289` |
| `MR_IID` | no | an MR already exists, e.g. `--var MR_IID=385`; use `TBD` if not yet |

On a clean first run, just pass `TICKET` — the crew agent picks the branch and
opens the MR itself.

> **CLI equivalent.** Everything Slack does, `kd` does too:
> `kd formula show <id>`, `kd formula cook <id> --var TICKET=PE-7289` (dry-run
> preview — expands the steps and prints them without creating any beads), and
> `kd formula pour <id> --var TICKET=PE-7289`. `cook` is the safe way to preview
> exactly which step beads a pour will create.

---

## What the pour creates

The PE-Ticket-to-Completion formula **extends the golden Bug-Fix-MR formula**
(`ka-0000001189`) and adds the two things that make it trustworthy unattended:
**three critic gates** and a **per-ticket gardener**. The poured molecule is a
chain of gated step beads:

```
check-existing-work
  → investigate-fix
      → fix-critic                ◀ CRITIC 1 (fix correctness + narrow scope)
          → e2e-test
              → test-evidence-critic   ◀ CRITIC 2 (RED-before / GREEN-after proof)
                  → draft-mr
                      → before/after screenshots → visual-diff-review
                          → mark-mr-ready
                              → mr-description-critic   ◀ CRITIC 3 (MR body rubric)
                                  → gardener-signoff      ◀ GARDENER GATE
                                      → tag-reviewer
```

Plus, created **early and in parallel** (right after `check-existing-work`):

- **`gardener-setup`** — stands up the per-ticket gardener as a `schedule` bead,
  created **disabled** (`enabled=false`). An operator or the gardener itself flips
  it on.

Each step is a real bead with dependencies, so the chain is enforced: a downstream
step cannot start until its upstream gate is satisfied.

---

## The 3 critic gates

A **critic** is an *adversarial reviewer* step, not the author. Its default posture
is skepticism — it assumes the work is **not** good enough until evidence forces a
score ≥ 8 out of 10. Every critic follows the same **measure → judge → bounce**
loop:

1. **Measure** — read the actual artifact (the diff, the test output, the MR body).
   Never score from memory.
2. **Judge** — assign a 0–10 score with a written verdict citing specific evidence
   (`file:line`, a quoted assertion, a screenshot URL).
3. **Bounce or pass** — record the verdict as a comment on the critic step bead:
   - **score ≥ 8** → `CRITIC PASS n/10`, close the step, chain proceeds.
   - **score < 8** → `CRITIC BOUNCE n/10`, naming exactly what must change, and
     **return to the named bounce-target step**. Everything downstream stays
     blocked until the author redoes the target and the critic **re-scores ≥ 8**.

This is how iteration/"bounce" works: a low score doesn't fail the ticket, it
**reopens the upstream step** and loops until the work clears the bar.

### Critic 1 — `fix-critic`: does the diff actually fix it, and is the scope narrow?

Gates after `investigate-fix`. Two independent questions, **both** must hold:

- **A. Does it actually fix the ticket?** Trace the diff against the ticket's
  reported behaviour. Is the changed code on the exact path that produces the bug?
  Is the **root cause** addressed, or just a symptom papered over?
- **B. Is the scope narrow?** `git diff --stat origin/main...HEAD` — every changed
  file must be justified by the ticket. Drive-by refactors, reformatting churn,
  unrelated dependency bumps, and "while I was in here" edits all drag the score
  down. A correct fix buried in unrelated changes still scores low.

**Bounces to `investigate-fix`** if score < 8. Nothing (not even writing the test)
proceeds until the fix is confirmed correct *and* surgical.

### Critic 2 — `test-evidence-critic`: RED-before / GREEN-after proof

Gates after `e2e-test`. Verifies the new test is a **real regression guard**, not a
test that would pass with or without the fix. It demands a **bisection proof**:

- **RED before** — captured output showing the *same* test **failing** on pre-fix
  code, with an assertion error naming the buggy value/state.
- **GREEN after** — captured output showing the *same* test (same file + same test
  name) **passing** on post-fix code.
- **Assertion specificity** — at least one assertion targets the exact behaviour
  the ticket reported, not a generic `assert no error` / `assert 200`.

If failure and success come from *different* tests, or the assertion is loose
enough to pass on unfixed code, it scores ≤ 4. **Bounces to `e2e-test`** if < 8.

### Critic 3 — `mr-description-critic`: the MR body rubric

Gates after `mark-mr-ready`. A reviewer who never saw the ticket must understand
the bug, the fix, and the proof from the **MR description alone**. Six required
elements, each scored 0/1/2 (max 12, normalised to /10):

1. **Root cause** — names the actual cause with a code reference, not just the symptom.
2. **Fix summary** — 2+ sentences on what changed and why it resolves the root cause.
3. **Test evidence** — the RED-before / GREEN-after outputs are present and render.
4. **Before/after screenshots** — both render inline and are apples-to-apples.
5. **Narrow-scope statement** — an explicit sentence + the changed-file list.
6. **Linked ticket** — the JIRA `PE-XXXX` link is present and the ticket was
   commented with the MR link.

Any element scoring **0 caps the overall at ≤ 6** — a missing required element is a
fail, not an average. **Bounces to `mark-mr-ready`** if < 8.

---

## The per-ticket gardener

`gardener-setup` creates a **per-ticket gardener** — a `type=schedule` bead scoped to
*this* molecule's subtree — that drives the loop to completion so you don't have to.
It satisfies the every-gardener-self-blocks rule (advice `ka-0000000974`).

**Each wake-up (cron cadence) the gardener:**

1. Loads context (`gb prime`) and reads its Loop-Progress block.
2. Lists the molecule's open / in-progress / blocked children, identifies the
   critical-path step.
3. **Dispatches or nudges** work: if a critic bounced, makes sure the bounce-target
   step is being redone; if a step is unowned, spawns a crew agent on it; if
   blocked, resolves or escalates.
4. **Guards scope** — rejects drive-by changes that would fail `fix-critic`.
5. Runs the **end-state check** (see below).
6. Squawks a one-line status and stops.

**It self-blocks three ways** so it can never loop forever:

- a **run budget** (`max_runs=24`),
- a **progress-aware wedge guard** (`no_progress_for: {runs: 3}`), and
- a **bead-query loop-end** that fires when the molecule subtree has **zero** open /
  in-progress / blocked children, plus a 168h wall-clock backstop.

The gardener is created **disabled** (`enabled=false`). Enable it when you want the
autonomous drive to begin (an operator flips it on, or the gardener enables on first
manual fire). If you'd rather drive the molecule yourself, you can leave the gardener
off and just work the step beads in order — the critic gates still enforce quality.

---

## End-to-end flow

```text
Slack:  /formula pour <id> --var TICKET=PE-7289
   │
   ├─▶ molecule poured (persistent) + crew agent auto-spawned
   │     · triggered-by edge back to your Slack thread
   │
   ├─▶ gardener-setup creates the per-ticket gardener (disabled) ── enable to autopilot
   │
   ▼
check-existing-work → investigate-fix
   │
   ▼  CRITIC 1: fix-critic   ── bounce<8 ▶ back to investigate-fix
   │  (root cause? narrow scope?)
   ▼
e2e-test
   │
   ▼  CRITIC 2: test-evidence-critic  ── bounce<8 ▶ back to e2e-test
   │  (RED before / GREEN after, same test)
   ▼
draft-mr → before/after screenshots → visual-diff-review → mark-mr-ready
   │
   ▼  CRITIC 3: mr-description-critic ── bounce<8 ▶ back to mark-mr-ready
   │  (6-element MR-body rubric)
   ▼
GARDENER SIGN-OFF gate  ── blocks tag-reviewer until:
   · fix-critic ≥ 8   · test-evidence-critic ≥ 8   · mr-description-critic ≥ 8
   · CI green on the MR   · MR marked ready (not draft)
   │
   ▼
tag-reviewer ── a human reviewer is finally pulled in, on a verified-green MR
```

The key property: **no human reviewer is tagged until the gardener has signed off**
that all three critics scored ≥ 8 *and* CI is green *and* the MR is out of draft.
The critics + gardener absorb the iteration so the human sees the MR once.

---

## How to monitor it

- **beads-viewer** — open the molecule subtree to watch steps flip
  open → in-progress → closed, see critic verdict comments, and follow the
  dependency graph live:
  `https://beads-viewer.e2e.example.com/bead/<molecule-root-id>`
  (the pour confirmation in Slack links the molecule root).
- **Slack squawks** — the crew agent and the gardener squawk one-line status updates
  to the thread as they work; bead lifecycle events and comments are auto-posted by
  the bridge, so you'll see PASS/BOUNCE verdicts appear without asking.
- **CLI** — `kd show <molecule-root> ` to see children and status,
  `kd list --status=in_progress`, and `kd show <critic-step>  --comments` to read a
  critic's written verdict.

---

## When the gardener signs off (and when YOU sign off)

The `gardener-signoff` step is a **manual gate** (72h timeout). It stays blocked
until the gardener works its checklist and clears it:

- [ ] `fix-critic` closed with a recorded `CRITIC PASS` ≥ 8
- [ ] `test-evidence-critic` closed with a recorded `CRITIC PASS` ≥ 8
- [ ] `mr-description-critic` closed with a recorded `CRITIC PASS` ≥ 8
- [ ] **CI green** on the MR — verified via `glab mr view <iid> --json head_pipeline`,
      not assumed
- [ ] MR marked **ready** (not draft)

When all boxes are checked the gardener records a `GARDENER SIGN-OFF` comment citing
the three scores + the green pipeline id, closes the step, and `tag-reviewer` fires.
A critic that merely *ran* is not a pass — only a recorded score ≥ 8 counts.

**Your sign-off** as the operator happens at the very end: review and merge the MR
on GitLab as usual. The formula's job is to hand you an MR that is already
critic-green and CI-green, with a description good enough to review from cold.

> If you're driving by hand without the gardener enabled, *you* are the one who
> verifies the `gardener-signoff` checklist before tagging a reviewer. The critic
> step beads carry the verdicts you need.

---

## Cheat sheet

| I want to… | Command |
|---|---|
| Find the formula id | `/formula list` → "PE Ticket to Completion" (`ka-0000007107`) |
| Read what it does | `/formula show ka-0000007107` or `kd formula show ka-0000007107` |
| Preview the steps (no beads) | `kd formula cook ka-0000007107 --var TICKET=PE-7289` |
| **Work a ticket to completion** | `/formula pour ka-0000007107 --var TICKET=PE-7289` |
| Re-pour with an existing branch/MR | `… --var TICKET=PE-7289 --var BRANCH=… --var MR_IID=385` |
| Watch it live | beads-viewer link from the pour confirmation |
| Read a critic's verdict | `kd show <critic-step-id> --comments` |
| Enable the autonomous gardener | flip the `gardener-pe-PE-XXXX` schedule bead `enabled=true` |

**Source of record:** the formula definition lives at
[`gardenio/docs/pe-ticket-to-completion-formula.json`](pe-ticket-to-completion-formula.json)
and is applied with `kd formula apply`. It extends the golden Bug-Fix-MR formula
(`ka-0000001189`). Epic: `ka-0000004458`.
