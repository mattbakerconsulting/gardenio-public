# Heraldry round 2: the large sample

> **History.** This records the first form of arms: one to five emoji placed like the five on a
> die on a shield of one tincture. The owner's verdict of 2026-10-26 20:45Z (thread gd-lvbRHMaXID)
> replaced it with three to five emoji in a row, shown the same way everywhere. Kept as history,
> not guidance: for the current form see [`README.md`](README.md) and [`rubric.md`](rubric.md).

Phase 0 step 0.4b (gd-jwBIGusQUd) of the heraldry plan gd-N6I3Oa7uDg. The brief is the owner's eight
round-1 notes, as spec [`2026-10-25-heraldry-design.md`](../superpowers/specs/2026-10-25-heraldry-design.md)
§10 records them. This is arms for **every open epic on the farm**, each with a one-line reason and a
score against the rubric ([`rubric.md`](rubric.md), step 0.4a). The arms are drawn with the round-1
changes: device resolution, nothing quantised, R13's eight tinctures day and night, and the contrast
matrix both ways (Q14). The 16 px stake is drawn at 2× and 3× so Q16 can be answered. Posted as
report gd-ax0mMd1BAz and decision gd-bC2HJmTCl3, which supersedes the round-1 decision gd-HKbQ2d25hI.

Nothing here is product code. Every picture comes from the throwaway prototype
[`scripts/heraldry-proto`](../../../scripts/heraldry-proto/) (`go run . round2`).

## The data

The sheet is drawn from three files in [`samples/round-2/`](samples/round-2/). Step 1.6 (agents choose
arms) can reuse them as worked examples.

| File | What |
|---|---|
| `arms.tsv` | the input: 137 epics, each with kind, plan, short title, placed arms, tincture (a name, `auto` for R13's default, or `plan`), the tincture's reason, one gloss per charge, the reason line, and its round-1 status |
| `grades.tsv` | the hand grades: S1–S5 from two independent reviewer agents (a third re-graded the nine arms changed after they started), S6 (d) and S7 misfits they found, and G3, G4 and G5 judged by eye from the pictures |
| `scored.tsv` | the output, one row per arms: every input column, all twelve checks with their evidence, G2 raw, and the score with the keyline and raw |
| `summary.md` | the counts: bands, fails per check, tinctures in use, the matrix's fail counts, the G4 distance distribution and the 30 closest pairs |
| `contrast-matrix.tsv` | the whole matrix: 1,333 charges × 8 tinctures × day and night, median and far-decile ratios and the verdict |

## What is on the sheet

The pages are ordered by room (owner, round 1). The 64 and 40 px pages come first, then 24, then the
16 px stake. Sheets marked @2x are drawn at device pixel ratio 2, so view them at 50 % zoom, or on a
2× screen, for true size.

| Page | What | Where |
|---|---|---|
| 1. 64 and 40 px, every arms (4 pages) | one row per epic: the reason, the glosses and the score with every FAIL; 64 and 40 px, day and night, raw and with the keyline | [p1](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/7559d34b-r2-1-64-40px-1-2x.png), [p2](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/63156380-r2-1-64-40px-2-2x.png), [p3](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/e3e64f51-r2-1-64-40px-3-2x.png), [p4](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/817cc6a6-r2-1-64-40px-4-2x.png) (4 to 5 MB each, so not committed) |
| 2. 40 px contact sheet | all 137 at 40 px, day and night (G5) | `sheet/r2-2-40px@2x.png` |
| 3. 24 px contact sheet | all 137 at 24 px: 2× day and night, and 1× doubled (G3) | `sheet/r2-3-24px@2x.png` |
| 4. The 16 px stake (Q16) | every phase on the farm's grass at 1×, 2× and 3×, day and night; the sheet is @3x | `sheet/r2-4-stake-16px@3x.png` |
| 5. Eight tinctures | eight arms on all eight tinctures, day and night, 64 and 24 px; once raw, with the pairs Q14 (b) would refuse marked, and once with the keyline | `sheet/r2-5-tinctures-raw@2x.png`, `sheet/r2-5-tinctures-keyline@2x.png` |
| 6. G4 closest pairs | the register's 30 closest pairs at 24 px | [link](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/f5de4996-r2-6-g4-closest-pairs.png) |
| 7. The contrast matrix | per tincture, the whole library: refusal (faded, with a red bar) beside the keyline, day and night | [blue](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/5ad98a09-matrix-blue.png), [red](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/bfc004e1-matrix-red.png), [green](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/7484584d-matrix-green.png), [gold](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/b63f4232-matrix-gold.png), [purple](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/c95aaca5-matrix-purple.png), [brown](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/7a9a5134-matrix-brown.png), [slate](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/1f65423f-matrix-slate.png), [sand](https://gardenio.clonk.tv/image/coop/sessions/images/2026-10-26/9043d3dc-matrix-sand.png) |

The links open in a browser that is signed in to gardenio. The same PNGs are in the Slack thread and on
the report bead.

## The sample

- **137 open epics**, all in one register (gardenio): 33 plans, 92 phases and 12 other epics. Two of
  the phases are plans themselves: the Window gd-pz87vf3xsB under Track A, and Phase B gd-aGiTndCXA4
  of the restart-churn plan. They are graded as phases, because S6 is about their plan. Two other
  epics are phases of a closed plan (the OpenZiti move), so they are graded as plain epics.
- **Round 1's epics carry over** and are re-scored: 15 plans and 6 Track A phases. Fourteen are kept.
  Seven are revised where round 1's rubric examples failed them:
  - Track A takes 🏛️ for the Commons, so its phases pass S6. It drops 🌍 for 🧩, so it has a charge
    for what it is for (S2).
  - The birds swap the pose into the centre (S1).
  - Ziti/Keycloak loses 🍝 and 🧥 (S4).
  - Short titles loses 💇 (S4).
  - The Grafana plan loses 🌀 (S4) and gold (G1).
  - The Workbench phase moves 🛠️ to a corner (S6 c).
  
  Round 1's tasks, decisions, escalations and agents are dropped (R6). One round-1 phase,
  gd-E1TBc0lNdL, has closed.
- **One meaning per charge.** Every charge has one gloss across the register (S7 by construction).
  The reviewers still flagged nine places where the shared gloss does not really fit its use.
- **Phases take their plan's tincture** (rubric §7 point 3). Where a phase's charge fails G1 or G2 on
  that tincture, the phase takes the best passing tincture instead. That happened to 50 of 92 phases,
  so the tincture is a weaker family mark than the shared charge.

### An authoring pass, as rubric §4 asks

The first render was compared against the register before scoring, as rubric §4 steps 5 and 6 say.
Fourteen arms changed:

- Twelve cleared a collision judged by eye at 24 px (G4), most often a phase beside its plan or a
  sibling with the same tincture, the same corner charge and a look-alike centre: for example
  👛🐷🧑 and 👛🔓🧑, or 🔀🗃️ and 🔀🎮. The quickstart phase took three tries.
- Two cleared the reviewers' hard S5 fails: 🔬 for a spike and 👌 for ergonomics.

Each change is in `arms.tsv`. Two epics name a tincture only to stay apart from a neighbour (G4):
gd-3UQKfj0nA8 and gd-BHLDnjX1Hw. The nine arms that changed after the reviewers started were
re-graded by a third reviewer.

## Scores

| Band (rubric §5) | G2 with the keyline (Q14 a) | G2 raw, refusing (Q14 b) |
|---|---:|---:|
| accept (100 %) | 43 | 27 |
| accept with notes (≥ 83 %) | 75 | 42 |
| revise (< 83 %) | 19 | 10 |
| revise, a hard check failed | 0 | 58 |

By kind, with the keyline: plans 15 / 11 / 7 (accept / with notes / revise), phases 24 / 58 / 10,
other epics 4 / 6 / 2. Of round 1's 20 carried-over arms, 7 accept, 9 accept with notes and 4 revise
(Track A at 67 %, the birds at 75 %, plans-first-class at 75 %, short titles at 82 %).

| Check | FAIL | Who trips on it |
|---|---:|---|
| S1 the centre is the thing | 21 | phases 17: the centre holds a place or one part, such as 🎮 for "P5 Lands" or 🌍 for "the banner in the World" |
| **S2 a charge says what it is for** | **62** | **phases 49 of 92**: the plan's charge (a "where") plus two parts leave no room for a purpose |
| S3 scan order | 12 | mostly phases whose subject sits in a corner |
| S4 private puns | 6 | 🧩 (one piece for "one place"), 🧬 for a model, ⚓ for a captain, 💡 for a prompt, ⚗️ for a formula, 🔀 (the shuffle button) for "predictable" routing |
| S5 type or state glyph | 0 | (two found and fixed in the authoring pass) |
| S6 family | 2 | the birds' phase 1 is its plan's three charges reordered (S6 d), so the plan fails too |
| S7 one meaning | 9 | gloss misfits: 🧪 for deleting tests, 🧹 for a groomer that adds titles |
| G1 the tincture suits | 2 | 🧩 sinks on Track A's green; 🕸️ sinks on blue by night |
| G2 contrast, with the keyline | 0 | the keyline clears 3:1 on every tincture by construction |
| G2 raw | 58 | 🕸️, 🌐, 🧮, 🎮, 🐐 and 🎟️ most often |
| G3 the centre at 24 px | 31 | a thin centre (pen, ruler, brush, paperclip) beside a loud corner (lifebuoy, number pad, bell) |
| G4 one silhouette | 0 | after the authoring pass, the 30 closest pairs were judged distinct by eye |
| G5 every charge at 40 px | 8 | 🕸️ (four arms), 🎮 and 🧩 on green, 🛂, 🎼 |

## Findings

### 1. Phases rarely say what they are for (S2)

49 of 92 phases fail S2. The family mark (S6) spends a corner on the plan's charge. A phase's own
subject takes the centre, and the last corner usually goes to a second part, not a purpose. Three
charges are too few to do all of S1, S2, S3 and S6 at once. **Proposal for rubric v2:** a phase passes
S2 when its plan's arms carry the purpose, or a phase may have four charges, with the shared one at TL.

### 2. The keyline makes Q14 easy to answer; refusal costs a third of the library

WCAG 3:1 against a mid-tone field is a high bar. A yellow face on green is about 2.5:1, and the matrix
pages show the face plainly anyway.

| Tincture | Day fails | Night fails |
|---|---:|---:|
| blue | 368 | 150 |
| red | 370 | 152 |
| green | 676 | 226 |
| gold | 892 | 847 |
| purple | 341 | 149 |
| brown | 337 | 140 |
| slate | 411 | 163 |
| sand | 723 | 1,082 |

- 1,242 of 1,333 charges fail on at least one of the sixteen (tincture, palette) cells. 40 fail on
  all sixteen. In all, 7,027 of 21,328 cells fail.
- Under Q14 (b), 58 of the 137 arms would be refused and have to change.
- Under (a), the 1-unit keyline in the opposite luminance carries every charge's edge on every
  tincture, and the pictures stay clean.

**Recommendation: (a), as the spec proposes.** The keyline looks right at 64 and 40 px and is barely
visible at 24 px, where it still helps. Gold and sand are the costly tinctures: their light fields
refuse two thirds of the library.

### 3. G3's "at least as large as any corner" fails most arms; the eye passes most of them

The objective half of G3 (the centre passes G2 raw, and its opaque area is at least any corner's)
fails 96 arms. Noto's square-ish artwork, such as 💬, 📈 and 🏢, fills more of its square than round
or thin artwork does. Looking at the 24 px pictures, 67 of those 96 still read with the centre leading.
29 do not, and nor do 2 arms that pass the measure: 31 G3 FAILs in all. In each, the centre is a thin
line or a dark mark next to a bright corner. **Proposal for rubric
v2:** keep the eye as the judge, and replace "at least as large" with "not a thin or low-contrast
shape next to a larger, brighter corner".

### 4. R13's default tincture spreads, but only if the tie-break is local

Ordered by contrast alone, the default puts most arms on the few dark tinctures (purple, brown, blue,
red). A tie-break by "least used so far" spreads them better, but it makes every arms' tincture depend
on every other: changing one arms moved twenty others. The prototype now scores each tincture by
(charges that clear 3:1) + (charges that do not sink), and breaks ties by a hash of the charges. The
choice is then local and deterministic, and it still spreads: blue 30, red 22, green 6, gold 3,
purple 42, brown 21, slate 11, sand 2. Purple's count is inflated by plans whose phases follow them.
Gold and sand are almost never the default, because too many charges fail on them.

### 5. Family marks and G4 pull against each other

Phases of one plan share a tincture, a corner charge and the TL C BR layout, so at 24 px only their
centres differ. Nine of the twelve G4 fixes were a phase colliding with its plan or a sibling. The
rubric's claim that a shared tincture "costs nothing in uniqueness" (§7 point 3) does not hold: it cost
about one fix in ten phases. The charges are small inside the shield at 24 px (about 18 device px at 2×),
so a sibling needs a centre of a clearly different colour, not just a different object.

The G4 pre-filter (same tincture, same places, centres of one colour family) flags **231 pairs** in a
register of 137. That is too many to judge by eye, so rubric v2 needs the measured threshold the
rubric deferred to this round. The 24 px distances, as mean redmean at 2× with day and night
averaged:

| Pairs | Min | 1 % | 5 % | 25 % | Median | Max |
|---:|---:|---:|---:|---:|---:|---:|
| 9,316 | about 20 | 47 | 63 | 117 | 155 | 259 |

Every pair the eye failed was at 37.4 or closer. **Proposal:** flag under 40, and judge those by
eye.

### 6. Q16, the 16 px stake: a likeness at 2× and 3×, not the arms

On the farm's grass, a 16 px stake at 2× and 3× is clearly a little shield: the tincture, the layout,
and one or two bright marks. A reader who knows the plan's arms would recognise the family. No charge
can be named, and at 1× it is a coloured blot. **Recommendation:** keep the 24 px plot gate, and drop
the stake for v1 (the spec's default when the stake does not clearly read). Phase 4 can revisit it at
3× once the gate is live.

### 7. The tinctures (Q13)

The eight proposed values are below. Day and night are the same hue, with night a shade deeper.

| Tincture | Day | Night |
|---|---|---|
| blue | `#3d6cc0` | `#2b4e8f` |
| red | `#c0443a` | `#8e3029` |
| green | `#3a8f47` | `#2a6834` |
| gold | `#d9a933` | `#a67e22` |
| purple | `#8556b8` | `#613f88` |
| brown | `#8f5f3a` | `#69452a` |
| slate | `#627284` | `#475363` |
| sand | `#d6c29a` | `#a69572` |

All eight tell apart at 24 px on page 5. Red is the one to watch: it is the colour of the failing
states elsewhere in the UI. G1 forbids choosing it to mean a state, but a reader may still read red
arms as alarming.

### 8. Charges to retire from agents' habits

- 🕸️ (grey, nearly black) fails G2 raw on eight arms, fails G5 on four and sinks on blue by night. An
  agent should not reach for it.
- 🎮 (dark grey) fails on green, brown and red.
- 🛂 cannot be named at 40 px.

These belong in step 1.6's advice line as "charges that read badly on every field".

## Questions for the owner

1. **GO, iterate or stop** on round 2 (the decision).
2. **Q14:** the keyline (a) or refusal (b)? This sheet recommends (a). See finding 2.
3. **Q16:** keep the stake, or the 24 px gate only? This sheet recommends the gate only. See finding 6.
4. **Q13:** are the eight tincture values right? See finding 7.
5. **Rubric v2:** accept the four proposals? They are S2 for phases (finding 1), G3's size rule
   (finding 3), G4's threshold of 40 (finding 5), and phases taking their plan's tincture, which costs
   some G4 fixes (finding 5).

## What changed in the prototype

- `render.go`:
  - R13's eight tinctures with day and night values.
  - Rendering at a CSS slot times a device pixel ratio (1×, 2× and 3×).
  - The Q14 keyline.
  - **The quantised path removed** (spec §6.3, owner round 1), together with round 1's 8 px rules, the
    uniqueness test, the other artwork sets and its browser-shots script. All of these remain in git
    at fb2469a6.
- `contrast.go`: WCAG luminance and contrast, per-charge median and decile statistics, the sinking
  measure, the matrix and its sheets.
- `round2.go` and `round2_sheets.go`:
  - Loads `arms.tsv` and `grades.tsv`.
  - Validates them as the daemon would (R2, R3, R5 and R11).
  - Resolves `auto` and `plan` tinctures.
  - Grades the measured and mechanical checks (G1, G2, G3's objective half, G4's distances, S6 and
    S7).
  - Lays the hand grades over them, and draws the pages.
