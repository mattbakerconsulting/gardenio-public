# Plans UX Rubric (PACES)

> **Status:** v1 (2026-07-07). Authored under epic [[ka-0000001353]] in
> response to a Slack request from Matt Baker: *"get some e2e tests around
> plans UX, esp spawning and assistant and the UI being responsive. Use a
> typical UX enhancement gardener with a rubric and formula; 8 runs, 2 hours
> between runs."*

This document defines **how to score the Plans UX** — the `/plans` index and
the `/plans/{id}` cockpit — for a recurring UX-enhancement gardener. It is the
Plans-space sibling of the maps-beauty **CLUB** rubric
(`maps-beauty-audit-formula.json`), the info-density audit, and the Phase-3
`/threads` visual matrix ([[ka-0000007635]]), and it borrows their idiom:
objective metrics triage, **the screenshot is the source of truth**, each
dimension gets a 0–10 score + verdict, and low scores become bug beads.

The gardener that applies it is the plain `trigger:schedule` +
`action=apply_formula` shape (see `plans-ux-audit-formula.json`). It is NOT a
crew that edits the Plans code directly — it **observes, scores, and files**,
and any code fix flows through a normal MR by whoever picks up the filed bug.

---

## 1. What the Plans UX is

A **plan** is an ordinary `type=epic` carrying the `plan` label (`plans.go`,
`planLabel = "plan"`). The surface has two pages:

- **`/plans` index** (`handlePlans`) — one `.plan-card` per plan-labeled epic:
  title, status, creator, open-decision count, a step-progress rollup, the
  current-or-next step, the bound assistant's state chip, and (for operators) a
  one-line spawn/nudge form.
- **`/plans/{id}` cockpit** (`handlePlanDetail`) — a 2×2 named grid: the
  **narrative** (`#plan-narrative`, left), the **build-step timeline**
  (`#plan-steps`, right), and the **plan-agent card** (`#plan-agent`,
  full-width bottom — gardener levers + assistant conversation merged, step 6
  ka-0000000116). All three live-swap over SSE (`/plans/{id}/events`,
  `plan_events.js`).

The three behaviors Matt called out map directly onto dimensions below:
**spawning** and **assistant** → the **A** dimension; **responsive** → the
**S** dimension; and the cockpit content it all hangs on → **P** and **C**.

---

## 2. The five dimensions — PACES

Each dimension is scored **0–10** and combined from (a) objective signals the
`measure` step computes and (b) a visual review of the desktop **and** narrow
(375×667) screenshots the `capture` step takes. You MUST open the PNGs with the
Read tool — metrics only triage.

### P — Plan clarity (does the index tell the truth at a glance?)

The `/plans` index is the entry point. A newcomer must be able to scan it and
know, per plan: what it is, whose it is, how far along it is, what's next, and
whether an assistant is helping.

- **Objective:** every plan-labeled epic in scope renders exactly one card
  (`plan_card_count == kd_list_plan_epic_count`, delta 0). The progress rollup
  matches ground truth (`steps_closed`/`steps_total` from the child walk). The
  current-or-next step is the real in_progress step (or the first open step when
  none is active).
- **Visual:** card hierarchy reads title-first; status/creator/decision chips
  are legible and not a soup; the progress bar is visually proportional to the
  label; the "now/next" step is obvious.
- **Auto-cap:** any `card_vs_truth_delta != 0`, a wrong rollup, or a
  template-leak (`{{`, `<no value>`) ⇒ score ≤ 4.
- 10 = the index is a trustworthy at-a-glance board for every plan.

### A — Assistant (spawn + responsiveness) — **the headline dimension**

The spawn/plan-agent path is the feature Matt named twice. On both the index
card (`form.plan-card-spawn`) and the cockpit (`form.plan-assistant-form`), an
operator sends a message; the first message **spawns** a `plan-gardener`-role
agent bead (the unified role of the ka-0000000081 merge; the resolver keys on
`role:plan-gardener` only — the legacy `plan-assistant` alias was hard-removed,
decision ka-0000007411) bound by `fields.plan_id`; subsequent messages **wake**
a parked pod or **nudge** a live one.

- **Objective:** the spawn POST returns 200 (not 400/502); after it the card is
  bound (`data-state` ∈ {waking, live, parked}, never `none`, and
  `a.plan-agent-terminal` present). The unified chip state matches the agent
  bead's real liveness. Latency from submit → card-rerender is recorded.
- **Visual:** the state chip (live / waking / parked / not started) is
  unambiguous; the hint text under the box matches the state ("First message
  spawns…" vs "Instant — the agent is live."); the send affordance is
  obvious; the recent-activity feed reads as a conversation, not a log dump.
- **Auto-cap:** a spawn that renders the error lane, a chip that lies about
  liveness, or a lane stuck at `none` after a 200 POST ⇒ score ≤ 3. (This is
  the exact "spawn button is broken" class of bug from `quick_spawn` history —
  hold the bar high.)
- 10 = spawning feels instant and honest; the assistant lane is a live
  collaborator, not a form.

### C — Cockpit fidelity (steps + narrative correctness)

The cockpit is where the work lives. The step timeline and narrative must be a
faithful, complete picture of the plan.

- **Objective:** every direct issue child renders as one timeline row
  (`timeline_step_count == direct_issue_child_count`); status modifiers are
  right (`is-done` for closed, `is-active` for in_progress); the progress bar
  matches the rollup; the narrative bundle renders (no empty `#plan-narrative`
  when a `type=bundle` child exists); blocker/MR/section chips resolve (no
  dangling `⛔`/`§` with no target).
- **Visual:** the timeline reads top-to-bottom with a clear rail; done steps
  recede, the active step draws the eye; the narrative prose is readable and
  not cramped against the steps column.
- **Auto-cap:** a missing/phantom step, a wrong status modifier, an empty
  narrative when a bundle exists, or an SSE anchor that never reaches `live` ⇒
  score ≤ 4.
- 10 = the cockpit is a complete, correct, live picture of the plan.

### E — Ergonomics (interaction craft)

The operator write surface: add-step, close/reopen, phase reorder, mark/unmark,
and the SSE live-swap. Controls must be discoverable, wired, and honest about
their result.

- **Objective:** each operator control POSTs and returns 200; `#plan-steps`
  swaps in place (htmx `outerHTML`, no full reload where a swap is intended);
  disabled controls (phase-reorder at a boundary) carry the `disabled` attr;
  the SSE stream actually swaps on a child change.
- **Visual:** controls are where you'd reach for them; the post-action state is
  obvious (the flash animation, the "sent ✓" ack); no dead buttons; a drill-in
  has a way back (breadcrumb).
- **Auto-cap:** a dead control, a swap that silently no-ops, or a mutation with
  no visible feedback ⇒ score ≤ 5.
- 10 = every control is discoverable, wired, and gives honest feedback.

### S — Skin (responsive + beauty)

The "UI being responsive" ask, plus aesthetic craft. Sweep desktop (≥1024) and
narrow (375×667).

- **Objective (hard gates):** `documentElement.scrollWidth ≤ innerWidth + 1` on
  BOTH pages at 375px (no horizontal overflow — the catch-all regression
  detector); the cockpit `.plan-grid` collapses to a single track below 60rem
  (`grid_tracks_narrow == 1`); the spawn textarea and send button stay within
  the viewport; nav links stay reachable (wrap, not clip).
- **Visual:** at 375px the reading stack is sane (steps → plan → assistant); no
  clipped chips, no overlapping text, no zero-width inputs; on desktop the 2×2
  grid is balanced with purposeful whitespace; the surface looks *designed*.
- **Auto-cap:** any horizontal overflow at 375px, a grid that fails to collapse,
  or an input that overflows the viewport ⇒ score ≤ 4.
- 10 = flawless from phone to desktop; you'd screenshot it to show off.

---

## 3. Score bands + overall

Per-dimension bands (shared with maps-beauty CLUB):

| Score | Verdict     |
|-------|-------------|
| 0–3   | broken      |
| 4–5   | weak        |
| 6–7   | ok          |
| 8–9   | good        |
| 10    | excellent   |

`overall.score` = mean of the five. Because Plans is a **primary work
surface**, the filing bar is deliberately high: every dimension scoring **≤ 7**
(anything short of "good") gets a bug bead. "ok" still leaves a fix on the
table.

The A (Assistant) and S (Skin) dimensions are the operator's explicit asks —
weight the roll-up narrative toward them and never let an A or S regression hide
behind a high mean.

---

## 4. What the gardener does with a score

Same enforcement idiom as every other UX audit gardener — **autonomous to
observe/score/file; gated to change**:

1. **Score + file.** For each dimension ≤ 7, file (or update, if an open bead
   for that dimension already exists) ONE bug bead parented to the audit epic,
   with the verbatim observations, the concrete recommendations, and the
   screenshot evidence paths. Dedupe by `pace:<dimension>` label.
2. **Report.** Roll up a per-iteration `REPORT.md` with the 5-row PACES
   scorecard, the headline, and a trend diff against the prior iteration.
3. **Never edit Plans code directly.** A code fix is a normal MR by whoever
   picks up the filed bug. If the gardener wants to *propose* a structural change
   (e.g. "add a spawn confirmation toast"), it files a `type=task`, it does not
   open an MR itself.
4. **Escalate only real forks.** If the audit finds a P0 correctness bug (a
   spawn that 500s, a card that lies about progress), file the bug at P1 and
   `gb squawk` it — do not open an operator decision unless there's a genuine
   A-or-B choice.

---

## 5. The e2e tests are the floor; the audit is the ceiling

The behavioral `plans.spec.js` suite (hermetic Playwright, epic
[[ka-0000001353]]) is the **regression floor** — it fails CI the moment spawn
breaks, the cockpit stops rendering a lane, or the layout overflows at 375px.
This rubric is the **quality ceiling** — a recurring, judgement-driven audit
that pushes the surface from "correct" toward "flagship." They complement:
the suite guards what must never break; the audit finds what could be better.

See also:
- `plans-ux-audit-formula.json` — the capture→measure→judge→file→report formula.
- `maps-beauty-audit-formula.json` — the CLUB sibling this idiom is cloned from.
- `gardenio/tests/beads-viewer-e2e/tests/plans.spec.js` — the behavioral floor.
- [[ka-0000007635]] — the Phase-3 `/threads` visual matrix (prior art).
