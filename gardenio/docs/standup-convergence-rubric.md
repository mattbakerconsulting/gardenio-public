# Daily-Standup Convergence Rubric (STAND)

> **Status:** v1 (2026-08-03). Authored under task [[ka-0000001430]] (research
> epic [[ka-0000003646]]) in response to Matt's ask (thread [[ka-0000007399]],
> 2026-06-04): *"I want a new gardener that triggers a daily standup formula
> iteration loop **converging on a rubric**; create a research epic to review
> previous daily standup convos I've had and get an initial gardener designed and
> enabled."*

This document defines **how to score a gardenio daily-standup draft** so a
recurring gardener can iterate a draft to the accepted format *before* asking a
human to approve it. It is the standup-space sibling of the daily-bundle
**reporter-editor** loop (`daily-bundle-reporter-editor-formula.json`) and the
CREWS formula-UX audit (`formula-ux-rubric.md`), and it borrows their idiom: a
**writer** drafts, a fresh **critic** scores against this rubric, the writer
revises, and the loop **converges** on an accept before a single human decision
gate cross-posts it.

The dimension names encode the feedback Matt gave across ~10 hand-graded
iterations (below) as **STAND**: **S**cope · **T**ruth · **A**ccepted-format ·
**N**ews · **D**ensity.

The gardener that applies it is the plain `trigger:schedule` +
`action=apply_formula` shape (see `standup-convergence-formula.json`). It does
**not** post to the human channel autonomously — the loop converges to an
accepted draft, then a single `gb decision` gate lets a human approve the
cross-post to #daily-stand-up. **Observe → score → revise → converge → gate.**

---

## 1. What the standup is

A gardenio daily standup is **one participant's entry** in the human
#daily-stand-up channel (`C08QNP80M0T`) — gardenio posting as if it were a single
team member, reporting **only the gardenio fleet's own work** over the last
working day. The gold-standard shape, taken verbatim from live human entries in
that channel, is:

```
*gardenio* — {date}
:dart: *Goal:* {the epic/plan the fleet is driving — 1 line}
:white_check_mark: *Changed since yesterday:* {merged MRs, closed epics, releases — bullets, each an MR/bead link}
:technologist: *Priority today:* {the top few in-flight threads — bullets, each a link}
:warning: *Blocking / help:* {open decisions/escalations needing a human — or "None"}
```

Every MR, release tag, and ticket is a **clickable Slack link**
(`<https://gitlab.com/example-org/gardenio/-/merge_requests/NNN|!NNN>`,
`<https://.../browse/PE-XXXX|PE-XXXX>`). No bare IDs. The entry is short — a human
scans it in ~10 seconds alongside a dozen peers'.

---

## 2. The five dimensions — STAND

Each dimension is scored **0–10**. The score combines an **objective** signal the
critic can compute from the draft + a `kd`/`glab` ground-truth walk, and a
**judgement** of whether the draft reads like the accepted human entries. The
critic reads ONLY the draft and this rubric — never the writer's reasoning
(no grading-your-own-homework).

### S — Scope (only the gardenio fleet's own work) — **the headline dimension**

The founding defect. Matt, iteration ka-0000004908: *"how did you do those
calendar publish MRs? that wasn't you! ... **only gardenio's work should be in
here.**"* The standup must report the gardenio fleet's own output and nothing
else — no monorepo/site-deployment human work, no other project's beads,
nothing the fleet did not actually produce.

- **Objective:** every MR cited resolves to a `gardenio` repo
  (`<registry>/gardenio/*` / project
  `example-org/gardenio`), authored by a fleet agent; every bead cited
  carries `project:gardenio`; every release tag is a gardenio chart tag. Count
  **out-of-scope items** (a monorepo MR, a human-authored MR, another project's
  bead) — the target is **0**.
- **Judgement:** the entry reads as gardenio's own report; no borrowed credit.
- **Auto-cap:** ≥1 out-of-scope / falsely-attributed item ⇒ score **≤ 3**.
  (A standup that claims work the fleet didn't do is worse than none — it was
  the #1 thing Matt rejected.)
- 10 = every line is unambiguously the gardenio fleet's own work, correctly
  attributed.

### T — Truth (every citation is real and says what the line claims)

Journalism's inviolable rule, inherited from the reporter-editor accuracy floor.
A standup that links a merged MR that isn't merged, or a closed epic that's still
open, is a lie an operator will act on.

- **Objective:** spot-check EVERY MR/bead/tag citation against its source
  (`glab ... api`, `kd show`): the MR is really merged (for a "Changed since
  yesterday" line) or really open (for a "Priority today"/"Blocking" line); the
  bead status matches the tense; counts in any "— N items" tallies reconcile
  with the bullets. Count **false citations** — target **0**.
- **Judgement:** past-tense lines describe things that actually happened in the
  window; present/future lines describe things actually in flight.
- **Auto-cap:** any false citation ⇒ score **≤ 3**. This is a hard floor: the
  loop must **hold** (never cross-post) while a citation is unverifiable, the
  same posture the reporter-editor loop takes on its accuracy floor.
- 10 = every citation checked and true; tallies reconcile exactly.

### A — Accepted format (matches the real #daily-stand-up entry shape)

Matt, iteration ka-0000007050: *"doesn't match the accepted format"*;
ka-0000007519 reworked it to *"the real #daily-stand-up human format —
goal / changed-since-yesterday / priority-today / blocking / help."* The draft
must mirror the section shape human peers use in that channel.

- **Objective:** the four canonical sections are present and in order
  (Goal → Changed since yesterday → Priority today → Blocking/help); each uses
  the accepted emoji + bold label; bullets are plain (`•`/`-`), not nested prose;
  the header names gardenio + the date.
- **Judgement:** side-by-side with a same-week human entry, gardenio's reads as a
  peer, not an alien template. The "Blocking/help" line is present even when it's
  "None".
- **Auto-cap:** a missing or reordered canonical section ⇒ score **≤ 5**.
- 10 = indistinguishable in shape from the best human entries in the channel.

### N — News (the right items, proportioned to impact)

The standup must surface what actually mattered in the window and leave out
noise. Cron/session churn, trivial chores, and bot-lifecycle beads are not news;
merged epics, releases, P0/P1 incidents, and notable MRs are.

- **Objective:** the "Changed since yesterday" items are drawn from the real
  window (merged gardenio MRs + closed epics/releases since the prior standup);
  the highest-impact item (a release, a P0 fix, a shipped epic) leads; obvious
  noise (cron mirrors, `Scheduled:`/`ScheduleMirror` beads, session beads,
  self-referential standup beads) is filtered out.
- **Judgement:** a reader learns the day's real gardenio story; nothing important
  is missing and nothing trivial is padding the list.
- **Auto-cap:** the lead item is not the window's highest-impact thread, OR
  visible noise (a cron/session bead) appears as a line ⇒ score **≤ 5**.
- 10 = exactly the items a fleet lead would call out, in impact order.

### D — Density (slim, link-first, scannable)

Matt, iteration ka-0000007050: *"too verbose"*; ka-0000002984: *"slim it down,
focus on MR links ... summarize gardenio change in one line."* The entry must be
compact and link-dense — a human scans it in seconds.

- **Objective:** total length **≤ ~12 lines** (header + four sections with a few
  bullets each); every MR/tag/ticket rendered as a **clickable Slack link**, zero
  bare IDs (`bare_id_count == 0`); no line is a paragraph; sub-lists collapse to
  comma-joined items where they'd otherwise sprawl.
- **Judgement:** high signal-per-line; no throat-clearing, no restated context,
  no duplicate items across sections.
- **Auto-cap:** > ~14 lines, OR any bare (non-linked) MR/ticket id ⇒ score
  **≤ 5**.
- 10 = maximally scannable — every line earns its place, every ref is one click.

---

## 3. Score bands, decision rule + convergence

Per-dimension bands (shared with PACES/CREWS):

| Score | Verdict     |
|-------|-------------|
| 0–3   | broken      |
| 4–5   | weak        |
| 6–7   | ok          |
| 8–9   | good        |
| 10    | excellent   |

`overall` = mean of the five, weighted toward **S** (Scope) and **T** (Truth) —
the two dimensions Matt actually rejected drafts over. Never let an **S** or **T**
regression hide behind a high mean.

### The critic's verdict (per round)

Let `PASS = 8` (default; operator-tunable) and treat **S** and **T** as hard
floors at **4** (their auto-cap band — a broken Scope or Truth score can never be
"good enough to ship").

- `overall ≥ PASS` **AND** `S ≥ 4` **AND** `T ≥ 4` **AND** no dimension < 4
  ⇒ `verdict: accept`
- `S < 4` **OR** `T < 4` (a scope violation or a false citation) ⇒ `verdict: hold`
  — never cross-post; the writer must fix by *removing/correcting*, never by
  fabricating.
- otherwise ⇒ `verdict: revise` (with a prioritized, specific edit list).

### Convergence (why this is a *convergence* gardener, not a one-shot)

The loop runs a bounded number of writer↔critic rounds (default **3**). Each
critic verdict carries the round's `overall` and a `converging` flag:

- If a round's `overall` **does not improve** over the prior round and the
  verdict is still `revise`, set `converging: false` — the loop stops revising
  and takes the **best-scoring draft so far** to the gate. (Monotonic-improvement
  check, inherited from the reporter-editor loop — don't churn a draft that isn't
  getting better.)
- If a round returns `accept`, the loop terminates early (carry-forward).
- If the rounds exhaust at `revise` above the S/T floors but below `PASS`, the
  gate step posts the best draft **for human decision** (never silently) — the
  human approves, edits, or cancels.

The convergence trajectory (e.g. `r1 6.4 → r2 7.6 → r3 8.2 accept`) is the
artifact that proves the loop is doing its job, and is surfaced in the gate
decision and the wrap-up.

---

## 4. What the gardener does with a converged draft — **gated to publish**

Same enforcement idiom as every other gardenio loop — **autonomous to
draft/score/converge; gated to post to humans:**

1. **Converge, don't post.** The writer↔critic rounds run headlessly and produce
   a best accepted (or best-so-far) draft plus its STAND scorecard + trajectory.
2. **Gate the cross-post.** The final step files ONE `gb decision`
   (approve / revise / cancel) with the draft, the STAND scorecard, and the
   trajectory attached, and `gb yield`s. Only on **approve** does it cross-post to
   #daily-stand-up (`C08QNP80M0T`) via
   `SLACK_THREAD_CHANNEL=C08QNP80M0T SLACK_THREAD_TS=0 gb slack reply "$DRAFT"`
   (advice: cross-post trick), then verify it landed.
3. **Never invent to pass.** A `hold` (S or T below floor) is filed as a decision
   describing exactly what couldn't be verified / was out of scope — the loop
   never ships unverified or borrowed-credit prose to satisfy a score.
4. **hold on empty window.** If the window has genuinely no gardenio news (a quiet
   weekend), the writer says so in one honest line rather than padding — a
   truthful "quiet day" entry beats a manufactured one.

---

## 5. Relationship to prior standup work

- **ka-0000007076** — the existing DISABLED monorepo standup trigger (hand-written
  10-line prompt, human-graded every draft). This rubric + convergence formula
  **replace** its "one draft, hope a human likes it" model with a self-scoring
  loop that arrives at the accepted format on its own.
- **ka-0000004908 / ka-0000007050 / ka-0000002984 / ka-0000007519** — the four
  hand-graded iterations whose feedback this rubric encodes (see §2 dimension
  provenance and epic ka-0000003646).

See also:
- `standup-convergence-formula.json` — the setup→(writer↔critic)×3→gate loop.
- `daily-bundle-reporter-editor-formula.json` — the reporter-editor sibling this
  idiom is cloned from.
- `formula-ux-rubric.md` / `plans-ux-rubric.md` — the STAND/CREWS/PACES rubric
  family.
- `formula-reference.md` (repo root `docs/`) — the formula DSL.
