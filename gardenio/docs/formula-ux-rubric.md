# Formula UX Rubric (CREWS)

> **Status:** v1 (2026-07-18). Authored under task [[ka-0000001490]] (plan
> [[ka-0000007376]], "Formula legibility & execution-state visualization in
> beads-viewer") in response to Matt's ask to *"recruit an existing formula for
> UI enhancement to grade our efforts"* — the surfaces exist; now a recurring
> gardener grades them.

This document defines **how to score the Formula UX** — the `/formulas`
catalog, the formula **structure DAG**, and the **molecule execution view** —
for a recurring UX-enhancement gardener. It is the formula-space sibling of the
Plans **PACES** rubric (`plans-ux-rubric.md`), the maps-beauty **CLUB** rubric,
and the info-density audit, and it borrows their idiom: objective metrics
triage, **the screenshot is the source of truth**, each dimension gets a 0–10
score + verdict, and low scores become bug beads.

The gardener that applies it is the plain `trigger:schedule` +
`action=apply_formula` shape (see `formula-ux-audit-formula.json`). It is NOT a
crew that edits the viewer code directly — it **observes, scores, and files**,
and any code fix flows through a normal MR by whoever picks up the filed bug.

The dimension names encode Matt's draft — Catalog clarity · Structure
legibility · Execution honesty · Ergonomics · Skin/responsive — as **CREWS**:
**C**atalog · **R**ecipe (structure) · **E**xecution · **W**orkbench
(ergonomics) · **S**kin.

---

## 1. What the Formula UX is

A **formula** is a `type=formula` bead whose `fields.steps`/`fields.vars`
define a reusable recipe; pouring/wisping one instantiates a `type=molecule`
bead with child step beads (each stamped `source_formula` +
`source_step_id`). The surface has three pages:

- **`/formulas` catalog** (`formulas.go`, `handleFormulas`; template
  `formulas.html`) — one `.formula-row` per recipe: title, `fbadge-*` type +
  phase badges, step/var counts, a **live-molecule rollup** (`.formula-live-count`,
  a `.fpip` status-pip strip capped at 12 with `.fpip-overflow`, and a
  `.formula-frontier` one-liner), plus project / formula_type / has-live
  filters (`.formula-catalog-filters`) and a `.stats-banner`.
- **structure DAG** (`formula_dag.go`; partials `type_formula.html` +
  `formula_dag_svg.html`, rendered on the formula's bead page `/{id}`) — the
  recipe drawn as a layered SVG graph: one `.fdag-node` per step carrying a
  DSL-shape glyph (⚙ task, ◆ gate, ⑃ branch, ↻ loop, ▤ children/expand), edges
  from `depends_on`, a legend, a vars side-rail (`.formula-side-rail`), and
  copy-pasteable Pour / Cook commands (`.copy-btn`). Falls back to the flat
  `.formula-step-list` when the graph is too large to draw legibly.
- **molecule execution view** (`molecule_view.go`; partial
  `type_molecule.html`, rendered on the molecule's bead page `/{id}`) — the
  SAME structure DAG overlaid with live per-step status, the active frontier
  called out as **"▶ You are here"** (`.molecule-frontier`), a top progress bar
  (`.molecule-progress`), status chips, a per-step execution table
  (`.molecule-steps-table`) with owner initials + per-step cost, applied vars,
  and SSE live-swap of `#molecule-exec` via `/molecule/{id}/events`
  (`.molecule-events-anchor`).

All three are reads behind ambient auth. The headline ask the plan answers —
*"view where in its execution a given molecule is"* — lives in the **E**
dimension below.

---

## 2. The five dimensions — CREWS

Each dimension is scored **0–10** and combined from (a) objective signals the
`measure` step computes and (b) a visual review of the desktop **and** narrow
(375×667) screenshots the `capture` step takes. You MUST open the PNGs with the
Read tool — metrics only triage.

### C — Catalog clarity (does /formulas answer "what recipes do we have"?)

A newcomer must be able to scan the catalog and know, per recipe: what it is,
its shape (workflow/expansion/aspect, vapor/liquid), how big it is, and whether
it's alive right now.

- **Objective:** every `type=formula` bead in scope renders exactly one row
  (`formula_row_count == kd_formula_count`, delta 0). Live-molecule counts
  match ground truth (`kd list --type=molecule --status=open` grouped by
  `fields.formula_id`). Step/var counts match the recipe JSON. The pip strip
  caps at 12 with an honest `+N` overflow. Filters round-trip (a filtered URL
  renders only matching rows).
- **Visual:** rows read title-first; badges are legible, not a soup; live
  recipes visually pop against dormant ones; the frontier one-liner is
  scannable; the stats banner is truthful.
- **Auto-cap:** a missing/phantom row, a live count that lies, or a
  template-leak (`{{`, `<no value>`) ⇒ score ≤ 4.
- 10 = the catalog is a trustworthy at-a-glance inventory of every recipe.

### R — Recipe legibility (is the structure DAG readable?)

Matt's "what the constituents are, in a pseudo-graphical way". The DAG must
faithfully draw the recipe and be *readable* — a person should trace the flow
without opening the raw JSON.

- **Objective:** one `.fdag-node` per step (`fdag_node_count == recipe step
  count`); one edge per `depends_on` pair; glyphs match step shapes (gate ⇒ ◆,
  loop ⇒ ↻, …); the legend renders; external/inherited refs render as phantom
  nodes, not crashes; the side-rail var count matches `vars`; Pour/Cook copy
  commands are syntactically valid `kd formula …` invocations; the flat-list
  fallback appears when (and only when) the DAG is nil.
- **Visual:** layers flow in one direction; edges don't spaghetti through
  nodes; node labels are readable at 100% zoom; glyphs are distinguishable; an
  unordered (edge-less) recipe is called out rather than reading as a mistake;
  a wide DAG scrolls inside `.formula-dag-scroll`, never the page.
- **Auto-cap:** a missing/phantom node, a wrong-direction or missing edge, or
  an unreadable (overlapping-text) graph ⇒ score ≤ 4.
- 10 = the recipe reads like a diagram someone would put in a design doc.

### E — Execution honesty (does "you are here" match ground truth?) — **the headline dimension**

The plan's headline ask. The molecule view must tell the truth about where a
live run is — the frontier, the per-step statuses, the progress bar, and the
costs must all match the bead graph, live.

- **Objective:** `Done/Total` in the progress label equals the ground-truth
  child-bead walk (children with `source_step_id`, closed vs all); each
  `.molecule-step-status` chip equals the underlying bead's status; the
  frontier rows (`.molecule-row-frontier` / "▶ You are here") are exactly the
  ready-or-in_progress steps; not-yet-poured steps render as
  `.molecule-not-poured`, not as lies; extras (children not in the recipe)
  appear in the table; the SSE anchor (`data-molecule-events`) is present and
  the events endpoint returns 200; owner initials match assignees; the cost
  link resolves to `/molecule/{id}/cost`.
- **Visual:** the "you are here" callout is the first thing your eye finds;
  done steps recede, the frontier draws attention; DAG status overlay and
  table agree with each other; the progress bar is proportional to its label.
- **Auto-cap:** a frontier that points at the wrong step, a status chip that
  contradicts the bead, or a progress bar that disagrees with its own label ⇒
  score ≤ 3. (An execution view that lies is worse than none.)
- 10 = you can steer a live run from this page alone and never be misled.

### W — Workbench ergonomics (interaction craft)

The operator affordances around the three pages: catalog filters and row
actions, the copy-pour/cook buttons, and every cross-link in the mesh
(catalog row → formula page → molecule pips → molecule page → step beads →
cost rollup).

- **Objective:** every rendered link resolves < 400 (no dead `View →`, pip,
  bead, or cost link); the copy buttons carry the right `data-value`; filter
  combinations produce consistent counts; the raw-JSON details expands with
  valid JSON.
- **Visual:** actions are where you'd reach for them; the drill-path
  catalog → recipe → live run is discoverable without docs; a drill-in has a
  way back; nothing looks clickable-but-dead.
- **Auto-cap:** a dead control or a copy command that would error when pasted
  ⇒ score ≤ 5.
- 10 = the three pages feel like one coherent workbench, not three templates.

### S — Skin (responsive + beauty)

Sweep desktop (≥1024) and narrow (375×667) on ALL THREE pages.

- **Objective (hard gates):** `documentElement.scrollWidth ≤ innerWidth + 1`
  at 375px on all three pages (wide DAG/table content must scroll inside its
  own container — `.formula-dag-scroll`, the steps table — never the page);
  the structure flex layout (`.formula-structure`) stacks at narrow; filter
  controls stay reachable; `near_white_ratio` sane on desktop.
- **Visual:** at 375px the reading stack is sane; no clipped badges, pips, or
  chips; no zero-width inputs; on desktop the DAG pane + side rail balance
  with purposeful whitespace; the surface looks *designed*.
- **Auto-cap:** any page-level horizontal overflow at 375px ⇒ score ≤ 4.
- 10 = flawless from phone to desktop; you'd screenshot it to show off.

---

## 3. Score bands + overall

Per-dimension bands (shared with PACES/CLUB):

| Score | Verdict     |
|-------|-------------|
| 0–3   | broken      |
| 4–5   | weak        |
| 6–7   | ok          |
| 8–9   | good        |
| 10    | excellent   |

`overall.score` = mean of the five. The filing bar is **≤ 6**: every dimension
scoring 6 or below gets a bug bead. (Plans files at ≤ 7 because it is the
primary work surface; the formula surfaces are newer — the bar tightens to ≤ 7
once they've held ≥ 7 across two consecutive iterations.)

The E (Execution honesty) dimension is the plan's headline ask — weight the
roll-up narrative toward it and never let an E regression hide behind a high
mean.

---

## 4. What the gardener does with a score

Same enforcement idiom as every other UX audit gardener — **autonomous to
observe/score/file; gated to change**:

1. **Score + file.** For each dimension ≤ 6, file (or update, if an open bead
   for that dimension already exists) ONE bug bead parented to the audit epic,
   with the verbatim observations, the concrete recommendations, and the
   screenshot evidence paths. Dedupe by `crews:<dimension>` label.
2. **Report.** Roll up a per-iteration `REPORT.md` with the 5-row CREWS
   scorecard, the headline, and a trend diff against the prior iteration.
3. **Never edit viewer code directly.** A code fix is a normal MR by whoever
   picks up the filed bug. A structural *proposal* is a `type=task`, not an MR.
4. **Escalate only real forks.** A P0 correctness bug (a frontier that lies, a
   catalog that drops recipes) is filed at P1 and squawked — no operator
   decision unless there's a genuine A-or-B choice.

---

## 5. Relationship to the deploy train

The molecule execution view merged 2026-07-18 (`105eb2ee8`) and rides the next
release tag. The boot step verifies all three surfaces are actually **deployed**
(`id="molecule-exec"` present on a molecule bead page) before grading; a
surface that is merged-but-not-yet-released is a **BLOCKED-pending-release
comment, not a bug** — grading an undeployed surface files noise.

See also:
- `formula-ux-audit-formula.json` — the boot→capture→measure→judge→file→report formula.
- `plans-ux-rubric.md` / `plans-ux-audit-formula.json` — the PACES sibling this idiom is cloned from.
- `formula-reference.md` (repo root `docs/`) — the formula DSL the DAG draws.
- [[ka-0000007376]] — the formula-legibility plan; [[ka-0000001490]] — this rubric's task.
