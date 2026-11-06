# Heraldry rubric (S1–S8, G2–G4)

> **Status:** **v3, 2026-10-26**, on the owner's verdict of 20:45Z (thread gd-lvbRHMaXID, "final
> answer"): arms are three to five emoji in a row, shown the same way everywhere. Spec
> [`2026-10-25-heraldry-design.md`](../superpowers/specs/2026-10-25-heraldry-design.md) §9 records
> it. Written by step 5.7 (gd-vQobmSXmne).
>
> History: v2 (accepted 2026-10-26 09:32Z, decision gd-bC2HJmTCl3) and draft v1 (step 0.4a,
> gd-CsUSkR9Nds) graded the first form, one to five emoji placed like the five on a die on a
> shield of one tincture. v3 strikes what only that form needed: G1 (the tincture suits the
> charges) and G5 (every charge reads at 40 px, which G3 now covers at 24), the place grammar in
> S1 and S3, and S6's tincture rule. It adds S8 (three to five charges). No check is renumbered.
> The worked examples come from round 1 ([`round-1.md`](round-1.md)) and round 2
> ([`round-2-sheet.md`](round-2-sheet.md)), which were drawn in the first form; they are quoted
> here as runs, in the order v3 asks for.

This document says **what good arms are**, as eleven numbered checks. An agent that chooses arms
applies them while it chooses (§4). A reviewer scores finished arms against them (§5). It
follows the idiom of the Commons rubric ([`commons-ux-rubric.md`](../commons-ux-rubric.md)):

- Each check is `PASS`, `FAIL` or `N/A`, and `N/A` needs a reason. A check that could not be run
  (no picture rendered, no register to compare against) is **ungraded**, not passed.
- **Graphic checks are judged from the rendered row**, opened and looked at. A measurement
  triages; the picture decides.
- Every `FAIL` names its evidence: the reason line for a semantic check, the picture or the
  measured number for a graphic one.

Scope: **v1 arms are for epics** (spec R6): plans, phases, other `type=epic` beads, and
projects. Round 1's tasks, decisions, escalations and agents appear below only where they
illustrate a check; they are not scored, because they do not bear arms in v1.

Words used here: **arms** (the whole mark), **run** (the emoji in order, left to right),
**charge** (one emoji in the run), **register** (the arms in use in one project). The **reason
line** is the one line per bead that says why each charge is there, in run order ("🐐🌠🪦: a
goat, from a wish; untended, it ends in the graveyard").

---

## 1. The checks at a glance

| # | Check | Kind | Scope | Hard? |
|---|---|---|---|---|
| S1 | The first charge names what the epic is | semantic | one arms | |
| S2 | A charge names what the epic is for | semantic | one arms | |
| S3 | The run reads left to right: what it is first, what it is for next | semantic | one arms | |
| S4 | No private puns | semantic | one arms | |
| S5 | No type glyphs, no state glyphs | semantic | one arms | hard |
| S6 | A plan and its phases read as a family | semantic | plan + phases | |
| S7 | A charge means one thing in a project | semantic | register | |
| S8 | Three to five charges, each once | form | one arms | hard |
| ~~G1~~ | *struck in v3: the row has no field colour* | | | |
| G2 | Every charge keeps its edge, day and night | graphic | one arms | hard |
| G3 | Every charge reads at 24 px | graphic | one arms | |
| G4 | No two arms in a project share a run or differ only in order | graphic | register | hard |
| ~~G5~~ | *struck in v3: G3 covers it* | | | |

---

## 2. Semantic checks

The semantic checks are graded from the bead (its title, short title, description, and for a
phase its plan) and the reason line. The picture is not needed.

### S1. The first charge names what the epic is

The first charge is **the thing itself**: the subject the epic builds or changes, as a reader
who knows the project would name it in one word. Not a tool used on the way, not a step, not a
place it lives.

- **Test.** Cover every charge but the first. From the first charge's library name alone, can the
  reviewer say which of four short titles from the same register (this epic's and three others)
  it belongs to? `PASS` if yes.
- **Pass:** gd-EGpbDpqsec "wishes as goats in the garden", 🐐🌠🪦. The goat is the subject.
  gd-obL67RJ1td "Show costs on garden", 👛💵🧾: the fleet's purse.
- **Pass:** gd-N6I3Oa7uDg "Heraldry: arms for a bead", 🛡️🎲🖼️. The arms are the subject, and 🛡️
  is the emoji a reader already knows for them.
- **Fail (round 1's proposal):** gd-gAKb8Bf0Nm "gardeners animate by activity", 🐦🔨🤸. The subject
  is the gardener's animation; the bird is one of its effects. The pose (🤸) is the thing, so it
  goes first: 🤸🔨🐦.

### S2. A charge names what the epic is for

At least one charge says **what the epic is for**: the outcome when it is done, the problem it
removes, or who it serves. Arms that only list parts (what the epic touches) fail.

- **Test.** In the reason line, mark each charge *is* (the thing), *where* (what it lives in or
  starts from) or *for* (what it achieves). `PASS` if at least one charge is *for*.
- **Phases.** A phase passes S2 when its plan's arms carry the purpose, or it may bear four
  charges: its own subject first, the purpose, and the charge it shares with its plan (S6). Under
  v1, 49 of round 2's 92 phases failed S2 with three charges (`round-2-sheet.md` finding 1).
- **Pass:** gd-aD7jOGmAWW "AWS cost cuts", ☁️📉🐳🌐: 📉 is the outcome, the bill going down.
- **Pass:** gd-y28dPqgbgg "Agents see Slack", 👀🧪📝: 📝, marking what is seen against a rubric,
  is the purpose of looking.
- **Fail:** gd-vmw5s07A1F "Lands: game inside garden", 🎮🌍💬🛠️🪟. All five charges are parts (the
  game, the World, the Commons, the Workbench and the Glass). Nothing says what the game in the
  garden is for. A part could give up its place to a *for* charge, or the parts could move to the
  phases (S6) and the plan keep the game and its purpose.

### S3. The run reads left to right: what it is first, what it is for next

A reader reads the run the way they read the short title beside it, left to right. The charges in
that order read as a short sentence about the epic: **first** the thing (S1), **next** what it
is for (S2), and then, when there is room, where it lives, what sets it off or how it ends.

- **Test.** Read the reason line in run order. `PASS` if it is a sentence with the epic as its
  subject and no charge is out of place: the outcome first, or a part ahead of the thing, is a
  `FAIL` even when every charge is apt.
- **Pass:** gd-EGpbDpqsec 🐐🌠🪦: a goat (the thing), from a wish, and, untended, it ends in the
  graveyard.
- **Pass:** gd-X07JyIHEzl "Lands: public plots (Track B)", 🏡🎉🚪📜🛂: a public plot, for the Open
  day, behind its Door, Deed and the Gate's registration.
- **Fail:** arms stored before v3 keep their old place order (spec §4.1), which put *where* ahead
  of the thing: gd-EGpbDpqsec was stored as 🌠🐐🪦, a wish first. Such arms are graded as stored;
  the agent that holds the epic reorders them when it next touches them.

### S4. No private puns

Every charge is read from **what it draws** or from a **widely known public mark** (🐘 for
Postgres, ☸️ for Kubernetes). A charge that works only through a sound-alike, a spelling, or
fleet jargon the author happens to know is a private pun, and it fails, however clever.

- **Test.** Write the charge's reason without the bead's or a product's name. If what is left
  still connects the picture to the epic for a reader who knows the project but not the author,
  `PASS`. If the reason needs "because X is called Y" or "Y sounds like", `FAIL`.
- **Fail:** gd-P78ygCUgA9 "Replace ziti with Keycloak", 🗝️🍝🧥. 🍝 stands for ziti because ziti
  is a pasta, and 🧥 for Keycloak because of "cloak". Both need the product's name to work. 🗝️
  passes: the plan is about who holds the key to the edge.
- **Fail:** gd-YZATAIOhfW "Short titles implementation", 📏✏️💇. 💇 (a haircut) stands for the
  Haiku agent that *grooms* short titles. "Groomer" is fleet jargon; the picture shows hair.
- **Borderline (reviewer's call, say which):** gd-LpISt8IyHx, the claimlayer project, 🥞🧾. The
  pancake stack is a picture of layers, which is a visual metaphor anyone sees, but it needs the
  word "layer" from the project's name to land. gd-gh5J0vVbJP's 🌀 for Grafana leans on Grafana's
  swirl logo, which is not widely known and is orange, not Noto's blue cyclone.
- **A trade to watch.** Round 1 replaced 🕸️ (lost by night) with 🍝, which fixed the contrast and
  introduced the pun. A swap for contrast must pass S4 again.

### S5. No type glyphs, no state glyphs (hard)

No charge says **the bead's kind** (plan, phase, epic, project), because the type glyph already
says it beside the name, and no charge says **a state** (in progress, blocked, done, urgent),
because arms carry identity and stay the same for the bead's life (spec R8).

R4 already keeps the theme's own glyphs out of the library (🌻 epic, 🗺️ plan, 🌱 task, ✅, 🚧 and
the rest, listed in `library.md`), so the validator refuses those. S5 covers the lookalikes the
library still holds: 📋 or 📌 for "a plan", 🎯 or 🏁 for "a goal", ☑️, ✔️, ⌛, 🔜 or 🔴/🟢 for a state.

- **Test.** For each charge: would it be equally true of **every** epic of this kind, or true of
  this epic only for now? If yes to either, `FAIL`. One `FAIL` fails the arms (hard check, §5).
- **Exception:** when the epic's subject *is* the kind or the state, the charge names the
  subject and passes, and the reason line says so.
- **Pass by the exception:** gd-QjJScyWs36 "plans first-class in garden", 📋🪜🚦. 📋 would be a
  type charge on any other plan; here plans are the subject.
- **Illustration (a task, not scored):** gd-RL1uPwUqAF's ☑️ is "the enable tick" the bug is
  about, so it names a subject, not the bead's state. On an epic, ☑️ meaning "done" would fail.

### S6. A plan and its phases read as a family

A plan's phases look related to the plan, and to each other, **without differencing** (spec Q4:
no phase is the plan's arms with one charge changed). The family mark is authored: each phase
**shares at least one charge with its plan**, and the shared charge means the same thing in both.

- **Test.** For each phase of a plan: (a) does it share a charge with the plan? (b) does the
  shared charge mean the same thing in both runs (compare the reason lines)? (c) is the shared
  charge **not first**, so the first charge stays the phase's own (S1, G4)? (d) is the phase not
  the plan's arms with one change? Grade the plan and each phase: the plan passes if every phase
  passes; a phase passes if (a)–(d) hold.
- **Worked:** Track A, plan gd-vmw5s07A1F 🎮🌍💬🛠️🪟, and six of its phases.

  | Phase | Arms | Shares | Verdict |
  |---|---|---|---|
  | gd-26faHO39t7 Workbench: beads and cockpit | 🛠️🎛️📥 | 🛠️ (the Workbench, on both) | **PASS**, but 🛠️ is first; (c) prefers the phase's own subject there |
  | gd-o0I19hS1k1 Glass: live sessions browser | 🖥️👁️🪟 | 🪟 (the Glass, on both) | **PASS**: shared, not first |
  | gd-KoOR5SPlNE Commons I: room model | 🧵📨🏛️ | nothing with the plan | **FAIL** (a): the plan draws the Commons as 💬, its phases as 🏛️ |
  | gd-xh6Ldnpjow Commons II: crew in room | 🃏🗳️🏛️ | nothing with the plan | **FAIL** (a), as above |
  | gd-lDmTW3GkPq Commons III: many people | 👪🔔🏛️ | nothing with the plan | **FAIL** (a), as above |
  | gd-E1TBc0lNdL golden-screenshot visual regression | 📸🪞🥇 | nothing | **FAIL** (a): a cross-cutting phase still needs one charge from its plan |

  The three Commons phases do read as a family with each other (🏛️ on all three, never first),
  which is the right shape. The fault is that the plan and the phases picked two different
  charges for the Commons. Either the plan takes 🏛️ or the phases take 💬; then all three pass.
- **Counter-example:** `round-1.md`'s differencing sample (Commons I as the plan's five with one
  changed) fails (d), and, as the sample notes, it costs the phase its own face.

### S7. A charge means one thing in a project

Within one register, a charge used in more than one run **means the same thing in each**. A
reader who learns that 🪟 is the Glass can rely on it everywhere in the project. This is what
makes S6's family marks readable.

- **Test.** List every charge that appears in more than one run in the register, with the reason
  lines. `FAIL` the newer arms if a charge means something else there. (A register-level check:
  it is run when the arms join the register, against what is already in it.)
- **Pass:** 🪟 appears four times in gardenio (Track A, the Glass phase, a task and a decision),
  always as the Glass. 🐘 is always Postgres, ☸️ always the cluster, 🥇 always the golden
  screenshots.
- **One to watch:** 🛡️ is on gd-N6I3Oa7uDg, gd-mF1e8qFR3c and gd-hzlWkr6DiY. The plan and the crew
  agent use it for *arms*; the escalation uses it for *the Heraldry plan*. The two meanings
  coincide only because this plan is about arms. A security plan that reached for 🛡️
  (gd-5f5bJPKLhC, "P7 pre-publish security close-out", could have) would fail S7 against them.

### S8. Three to five charges, each once (hard)

Arms are **three to five** library emoji, and no emoji appears twice. Two emoji cannot say what
the epic is and what it is for and still be told apart in a register of a hundred; six do not
read at a glance.

- **Test.** Count the charges. `FAIL` on fewer than three, more than five, or a repeat. The
  validator refuses all three (spec R2, R11), so a `FAIL` here means the arms were never stored or
  were stored before v3.
- **Fail:** the twelve two-emoji epics in the register at 20:55Z on 2026-10-26, for example
  gd-rZZhfrpMDv "Passkeys/TOTP", 🔑🔢. Step 5.6 (gd-LGIxGuLZ9y) gives each a third charge.

---

## 3. Graphic checks

The graphic checks are graded from the rendered row (spec §6): the run drawn from the pinned
Noto artwork in one row, on a transparent ground, at device resolution, **day and night**. A
check is graded on both surface backgrounds; a `FAIL` on either is a `FAIL`. Where the surface
shows the run as text emoji (Slack, the beads-viewer), the reviewer grades the text there too.

### ~~G1~~

Struck in v3 (history: it judged the field colour of the first form). The row has no field.

### G2. Every charge keeps its edge, day and night (hard)

Every charge in the row clears **3:1** against the surface background it sits on (WCAG 2, the
1.4.11 non-text threshold), **day and night**, measured as spec §6.6 defines: the charge's opaque
pixels at their median colour and at their darkest and lightest deciles, against the background.

- **Pass rule:** a charge passes when its median clears 3:1, **or** when the decile on the far
  side of the background's luminance clears 3:1 (the charge has an edge that reads even if its
  body is close to the background).
- **The keyline (spec Q14).** The renderer draws a 1-unit keyline in the opposite luminance to the
  background around every charge, so every pair passes in the picture. G2 is graded with the
  keyline in the row picture and raw for the text run, which has none.
- **Test.** The matrix value for each (charge, background); then the 64 and 24 px rows, both
  backgrounds, looked at. One `FAIL` fails the arms (hard check, §5).
- **Fixed in authoring:** 🕸️ (grey, lost by night) became 🍝, and 🎚️ (dark, lost by night) became
  ☑️ (`round-1.md`). Both were night failures the day picture hid; G2 is graded on both
  backgrounds for this reason. (See S4 for what the 🕸️ swap cost.)

### G3. Every charge reads at 24 px

At 24 px (the Commons card header, the plan's plot gate) each charge is a 24 px square in the row.
"Reads" means: **every** charge is still the recognisable shape of its object, not only the
first. 24 px is the smallest slot arms are drawn in, so a charge that reads there reads at 40 and
64.

- **Objective part.** Each charge passes G2, and none is a thin line or a speck inside its square
  (Noto's goat is small inside its square; its square-ish artwork, 💬, 📈, 🏢, fills more).
- **Vision part.** Shown the 24 px row at device pixel ratio 1 and 2, beside four library names
  per charge (the true one and three of the same dominant colour), the reviewer picks the right
  one for each charge, both backgrounds. `FAIL` if any charge cannot be named.
- **Pass:** gd-obL67RJ1td 👛💵🧾. At 24 px the purse, the note and the receipt are three clear
  shapes.
- **Watch:** gd-EGpbDpqsec 🐐🌠🪦. At 24 px Noto's goat is a brown speck. In a row it no longer
  competes with a larger corner, but it must still be named; a bigger goat-like charge would be
  safer.

### G4. No two arms in a project share a run or differ only in order (hard)

R5 makes the run unique as data. G4 adds what the eye needs: **two arms in one project may not
bear the same emoji in another order** either, because a reader who knows 🔥🧭📦 will not tell it
from 📦🧭🔥 at a glance. The daemon only warns on that (`X-Beads-Warning`); the rubric fails it.

- **Test.** For the new arms, look up every arms in the register with the same set of charges
  (`kd heraldry check` lists them). `FAIL` if any exists, whatever the order.
- **Then by eye.** Render the new row beside the register's closest rows at 24 px, both
  backgrounds: the rows that share two charges, or whose first charges are of the same dominant
  colour. Are they two different arms at a glance? `FAIL` if not; the newer arms change.
- **A family is not a lookalike.** S6 wants phases to share a charge; G4 forbids them to look the
  same. The way through is S6's (c): **share a later charge, never the first.** The three Commons
  phases (🧵, 🃏 and 👪 first, 🏛️ last on each) are the right shape for this.

### ~~G5. Every charge reads at 40 px~~

Struck in v3: G3 asks every charge to read at 24 px, which covers 40.

---

## 4. Choosing arms: how an agent applies the rubric

The agent that files or holds the epic chooses its arms (spec R7, §7). In order:

1. **Read the epic**, and for a phase, its plan and its plan's arms. Write one sentence: what it
   is, and what it is for.
2. **Pick the first charge** for what it is (S1), then one for what it is for (S2), then, if it
   adds something, where it lives, what sets it off or how it ends. Put them in S3's order.
   **Three to five** (S8): three is the usual count; four or five only for an epic with that many
   parts, or four for a phase whose plan's arms do not carry the purpose (S2).
3. **Screen each charge:** not a pun (S4), not a type or a state (S5), and if the charge is
   already in the register, the same meaning (S7). For a phase, reuse one of the plan's charges,
   not first (S6).
4. **Check them**: `kd heraldry check "<run>" --project <project>`. It refuses a run that breaks
   S8 or is taken, and lists the arms with the same emoji in another order (G4). Change the run
   until that list is empty.
5. **Look at the row** where you can (G2 and G3): the charges at 24 px, day and night. Swap a
   charge that does not read, then re-run S4 on the replacement.
6. **Write the reason line**, one clause per charge, in run order. The reviewer grades from it.
7. **Give them**: `kd update <id> --heraldry "<run>"`.

## 5. Scoring

For each arms the reviewer writes one row:

```markdown
| Bead | Short title | Arms | Reason | S1 | S2 | S3 | S4 | S5 | S6 | S7 | S8 | G2 | G3 | G4 | Score |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| gd-… | … | 🐐🌠🪦 | a goat, from a wish; untended, it ends in the graveyard | P | P | P | P | P | N/A: not a phase | P | P | P | F | P | 9/10 |
```

with one line under the table per `FAIL` (the check, the evidence, the fix), and every `N/A`
with its reason. S6 is `N/A` for an epic that is neither a plan with phases nor a phase; S7 and
G4 are `N/A` for the first arms in an empty register.

- **Score** = `PASS` ÷ (11 − `N/A`), as a fraction and a percentage.
- **Bands** (the Commons rubric's): **100 %** accept · **≥ 83 %** accept with notes (the `FAIL`s
  are recorded and fixed when convenient) · **< 83 %** revise.
- **Hard checks.** A `FAIL` on **S5, S8, G2 or G4** means revise, whatever the score: a type or
  state charge misleads for the bead's life, a run of the wrong length is not arms, a charge that
  cannot be seen is not there, and two arms with the same emoji are not unique.
- **Ungraded** checks (nothing rendered yet, no register) are listed as ungraded and leave the
  denominator; the row says how many were ungraded, so a score is never inflated by them.

A sheet's summary gives, per register: the count of arms in each band, the count of `FAIL`s per
check (which shows which checks agents trip on), and the closest G4 pairs with their pictures.

## 6. What the reviewer must not do

- **Grade the semantics from the picture alone.** A reviewer who cannot name a charge from its
  picture records that under G3, not as an S-check failure.
- **Pass a graphic check from the numbers alone.** The measures triage; the picture decides.
- **Use `N/A` for a check that is merely hard to run.** Ungraded is the honest word.
- **Re-choose the arms.** The reviewer scores and says what fails and why; the agent that holds
  the epic changes its arms.

## 7. Settled points

v1's open points, as v3 leaves them:

1. **S7** (one meaning per charge in a project) is kept. **G5** is struck; G3 now asks every
   charge to read at 24 px.
2. **S3's reading order** is the owner's linear form: the run reads left to right, what it is
   first, what it is for next.
3. **Phases taking the plan's field colour** is moot: the row has no field.
4. **G2's pass rule** stands as written, against the surface backgrounds instead of a field.
5. **G4** no longer measures a picture distance: the row's identity is its emoji, so the check is
   the run and its reorderings, then the eye.
6. **The bands and the hard checks** keep the Commons rubric's 100 / 83 % bands; S5, S8, G2 and
   G4 are hard.
7. **The spec's running example** is `🔥🧭📦`, which the library holds.
