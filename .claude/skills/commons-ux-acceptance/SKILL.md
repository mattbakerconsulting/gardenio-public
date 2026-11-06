---
name: commons-ux-acceptance
description: >-
  Grade how a /garden (Lands) surface really renders before asking a human: shoot it in
  real Chromium on the golden harness's fixture farm across the C1-C12 capture matrix
  (phone, desktop, dark, mid, keyboard focus), run the rubric's DOM probes, read every PNG,
  grade against gardenio/docs/commons-ux-rubric.md, fix and re-shoot, promote an accepted
  surface to a golden, and attach before/after PNGs to the bead or PR. Use for
  "/commons-ux-acceptance", "UX-accept this /garden surface", "grade the bead view / Porch /
  Commons pane", and whenever a PR changes gardenhub's web/*.js, web/css/*.css, garden.html or
  a surface renderer.
allowed-tools: "Bash,Read,Write,Glob,Grep"
version: "1.0.0"
author: "gardenio"
---

# commons-ux-acceptance

The in-game sibling of `slack-ux-acceptance`. An agent that changes a `/garden` surface
**looks at it in a real browser**, grades it against the rubric, revises, and keeps the
screenshots as evidence, so a human is asked to look only at something that already passes.
In `/garden` we own every pixel, so there is no workspace to provision: the surface is served
from fixtures on an `httptest` server and shot in the pod.

| Piece | Where |
|---|---|
| Rubric C1-C12, capture matrix, fixtures F1-F8, probes, bands, severities | `gardenio/docs/commons-ux-rubric.md` (read it first) |
| Shoot: serve the fixture farm, capture every cell, run probes, draft the grade | `make commons-ux-shoot OUT=<dir> [SURFACES=a,b] [CELLS=a,b]` → `TestCommonsUXShoot` (`gardenhub/accept_browser_test.go`) → `scripts/shoot.js` |
| The §2 DOM probes | `scripts/probes.js` (self-test `scripts/test/probes.test.js`) |
| The §4 perf rows (§7 budgets B1-B11) | measured with P0.1's `gardenhub/testdata/perf_baseline.js` (`measure`, `idle`, `bench`), graded with P3.1's `perf_gate.js` (`verdicts`, self-test `make test-lands-perf-gate`) |
| Surfaces | the golden harness's `SHOTS` (`gardenhub/testdata/golden_browser.js`) plus `EXTRA` in `shoot.js` |
| Fixture world | `cardWorld` + `goldenPorch` (golden harness), `stressBead` (`card_browser_test.go`); the Workbench readers' stress, empty and down worlds behind the `X-UX-Fixture` header (`ux_fixture_test.go`) |
| Goldens, and how to add one | `gardenio/tests/gardenhub-browser/GOLDENS.md` |

The grader **observes, grades and files**. It edits the surface only when it is also the author
of the change under review (then the fix and the before/after belong in that PR); grading
someone else's PR, it files bugs (Step 6).

## Prerequisites

Linux, node, `playwright-core` and its Chromium. `make commons-ux-shoot` runs
`gardenhub-browser-deps` first (npm install + Chromium download); in an agent pod Chromium is
already under `/ms-playwright` and the target uses it. Builds one Go
package (`./internal/gardenhub/`), never the module: pods OOM on `go build ./...`.

No secrets, no cluster, no daemon, no Slack. Real fleet data never reaches a shot.

## Surfaces

| Surface | What is on screen | Probe root |
|---|---|---|
| `opening` | `/garden` at its opening zoom-to-fit | `body` |
| `cursorworld`, `cursorproject`, `cursorplan`, `cursorphase` | the cursor's signs at each depth: the full label, child labels, glyphs (cursor-grid spec 6) | `#lbls` |
| `porch` | the Porch dock on its agent list | `#dock` |
| `pad` | the phone's one-row pad under the map, Select latched (phone layouts only) | `#pad` |
| `bead` | the in-garden bead view of plan A (golden fixture: a decision waiting, 4 children, a comment) | `#bead` |
| `beadstress` | the bead view of `gd-StressF1F2`: F1 200-char title / 90-char agent / 1,400-char body, F2 URL, SHA, branch, path and bare id, F4 ZWJ, flag, VS16, CJK, Arabic, combining accent, F6 a child missing from the daemon | `#bead` |
| `mailbox` | the Mailbox tab with an open decision | `#dock` |
| `facework` … `facegeneric` (14 rows) | the typed faces of the bead view (WB-1): one face fixture each (`faceBeads`), a decision and an escalation open and answered, the generic fallback | `#bead` |
| `facedecisionstress`, `faceescalationstress`, `faceagentstress` | the busiest faces under F1, F2, F3 (14 options), F4 and F6 (an asker and task not in the daemon) (`faceStressBeads`, `EXTRA`) | `#bead` |
| `report` | the bead view of `gd-Report3p6`, a screenshot report as an agent files one (`reportBeads`): markdown headings, captioned laptop and phone screenshots through `/image/`, a nested list, a table, a quote, code, an outside picture (stays a link), F2 a long URL, F4 emoji and Arabic | `#bead` |
| `<kind>stress`, `<kind>empty`, `<kind>err` (queues, search, cockpit, bundle, project, gardener, settings), `beaderr`, `costserr` | each Workbench panel kind with `X-UX-Fixture: stress` (F1 200-char titles, F2 URL/SHA/branch/path, F3 25-72 rows, F4, F6 ids the daemon lacks and a picture that 404s), `empty` (nothing in it) or `down` (the daemon answers 502: the panel's failure line; for the bead view, its comments and links only, WB-15). shoot.js sets the header through the row's `ctx` (`STRESS`, `EMPTY`, `DOWN`, `world(v, writer)`); the goldens never send it. Plus the forms: `commentdraft`, `commentpreview`, `cockpitrename`, `cockpitpour`, `settingseditstress`, `plansforest` | `#bead` (`plansforest`: `#dw-plans`) |

The first five are golden rows (`SHOTS`), so what you grade is exactly what the golden gate
guards; the golden gate also runs the C3 probes (44 px targets, sideways boxes) on every phone shot, portrait and
landscape, so a promoted surface is held to C3 in CI. A new Lands surface (a workbench node, Workbench page, Glass grid): add its row to
`EXTRA` in `shoot.js` and its probe root to `ROOTS` in
`gardenio/controller/internal/gardenhub/testdata/golden_roots_browser.js` (or the row's own `root`), with fixture data in the world (a
`stressBead`-style helper on the bead reader keeps it off the farm and out of other goldens).
Give every new surface a stress variant: the rubric grades F1, F2, F3 and F6 at least, and a
surface graded only on short ASCII data has not been graded.

## The loop

Pick a work dir, e.g. `OUT=$TMPDIR/commons-ux/<bead-id>` (outside the repo).

### 1. Shoot

```bash
make commons-ux-shoot OUT=$OUT/before SURFACES=bead,beadstress
```

About 2 s per cell. With no `SURFACES` the whole matrix (~110 surfaces) takes about 20 minutes, inside
the target's `-timeout 60m`; a hand-run `go test -run TestCommonsUXShoot` needs that flag too, or go test's
default 10m panics half-way and `manifest.json` is never written. Per surface it writes:

| File | What |
|---|---|
| `<surface>-<cell>.png` | the first viewport, cells `phone-light`, `phone-dark` (390x844, touch), `desktop-light`, `desktop-dark` (1440x900), `mid` (820x1180, touch) |
| `<surface>-<cell>-scrollN.png` | each pane that scrolls inside itself, largest first (a workbench transcript, then its tree, then its rails), stepped from its top down a page at a time (up to 8 per pane, the last at its bottom): what the first viewport hides. The as-opened `<surface>-<cell>.png` is frame 0, so a timeline that opens at the bottom gets its top as `-scroll1`; a position that repeats frame 0 is skipped, and the draft notes a pane too tall for 8 |
| `<surface>-<cell>-clip.png` | the golden's frame of the cell, for a `SHOTS` row with a `clip(page)` (`bead`, `plan`, `planout`); not in the focus cells |
| `<surface>-<cell>-full.png` | a full-page shot, only when the document itself is taller than the viewport |
| `<surface>-focus-{light,dark}.png` | desktop after Tab-ing until focus lands inside the surface; the draft records which control and its outline |
| `<surface>-<cell>.probes.json` | the §2 probe hits: per probe, gates, count and up to 25 `{sel, text, value}` |
| `<surface>-{phone,desktop}-light.perf.json` | the §7 budgets for that cell's profile: `checks` (each with `head` median, `headBest` fastest run, `limit`, `verdict` PASS/FAIL/UNGRADED, `noisy`), `budgets` (verdict per id), `bench`, and the `rows` they were read from (perf_baseline.js's results.json rows). Only when perf is measured (below) |
| `COMMONS-UX-GRADE.<surface>.draft.md` | rubric §4 table with `?` verdicts, required cells and evidence per gate, probe candidates per gate, page errors, focus result; the perf line under the score and the perf rows table, verdicts filled in |
| `manifest.json` | every cell, its files, HTTP status, page errors |

**Phone sweep** (Lands mobile M0.1, gd-0B2drDTgFw): `CELLS=sweep` shoots eight more cells instead of the matrix,
`ios390-{light,dark}` / `ios390land-*` (Playwright's iPhone 12: Safari's user agent, 390x664 and 750x340, the viewport left
under Safari's bars) and `and360-*` / `and360land-*` (Galaxy S8: Android Chrome's, 360x740 and 740x360). They are never in
the default run. A full sweep of every surface is about 45 minutes. A cell that fails to shoot leaves `<file>-error.png`, a
picture of where it gave up: on a phone that is often a control the layout put off screen. The /lands/ pages sweep with
`LANDS_SWEEP=1 LANDS_GOLDEN_FULL=<dir>` on `TestLandsGoldens`, and CI's keycloak-theme job shoots the login screens in
`ios390`, `ios390land` and `and360land` too.

**Perf** (rubric §4 *Perf rows*, gd-qeaDwFSDqq): a shoot that names `SURFACES` also measures each
surface against the §7 budgets, after its cells: `RUNS` (default 3) cold and warm loads on §7's phone
profile (4x CPU, 9 Mbps / 170 ms RTT) and on the desktop, then an idle pass of `IDLE` seconds
(default 60) per profile, with the very `measure()`/`idle()`/`bench()` the CI gate's numbers come from.
It is filed under `phone-light` and `desktop-light`, so a `CELLS=` list without them measures nothing.
It adds about 4 minutes per surface on an agent pod. `PERF=0` skips it and `PERF=1` forces it on a
full-matrix run, where it is off by default (110 surfaces would take hours); `PERF_RUNS=1 PERF_IDLE=0`
is a quick look while iterating (no B8/B9 then). The perf loads run without the shots' pinned clock and
fonts. Their first interaction (INP) is P0.1's act for the same path, else the act of its kind (a bead
pane's click, the cockpit's step, the queues' tab, the Glass, the World's first move, a `j` in a
workbench's tree); an act that does not fit leaves B6/B11 UNGRADED with `actError` in the perf file.
A World surface (no pane open) is graded as the World: Ready under B4/B10, and B7, which stays
UNGRADED here (a grade runs no pan pass; the CI gate's pan pass grades it, rubric §7).

```bash
make commons-ux-shoot OUT=$OUT/after SURFACES=bead CELLS=phone-light,desktop-light   # one surface, perf included
```

Shots use the golden harness's pinned Noto fonts, frozen clock (`goldenNow`), reduced motion
and settle step. When the change under review is yours, take **before** with the surface
files stashed and **after** with them back:

```bash
git stash push -- gardenio/controller/internal/gardenhub/web     # only the surface, keep the harness
make commons-ux-shoot OUT=$OUT/before SURFACES=bead
git stash pop
make commons-ux-shoot OUT=$OUT/after SURFACES=bead
```

### 2. Grade

**Open every PNG with the Read tool**, including the `-scrollN` frames. The probes and the
draft only triage: a probe hit is a *candidate* FAIL that the screenshot confirms or clears,
and a clean probe row passes nothing by itself. Per gate C1-C12 answer the rubric §3 vision
questions in its required cells and decide `PASS` / `FAIL` / `N/A`:

- Every FAIL names its PNG (the V12 rule) and the probe value when there is one (`1.82:1`,
  `36x44`).
- Clearing a probe hit needs a reason in the grade ("10 px uppercase kind tag, deliberately
  demoted, legible at DPR 1 in both themes").
- `N/A` needs the rubric's reason: no card theme on the surface → C6; a static grade → C10
  and C8's after-click part. Never `N/A` a cell that was inconvenient; a cell that could not
  be captured is **ungraded**, and the grade says so.
- `docked`: no current surface has a separate docked state. For a pane that already sits
  beside the World at desktop width (bead view, Porch), grade the docked questions from
  `desktop-*` and say so.
- Contrast the probe could not resolve (text over the canvas or an image: `notRun.lowContrast`)
  needs a pixel sample from the PNG (glyph and background colour; a few lines of Go
  `image/png`, the pod has no Pillow); record the ratio.
- Stylesheet probes (`untokenedColour`, `hoverOnly`) scan all of the page's stylesheet (`web/css/*.css`) and are listed
  once per surface. Count only the rules that style the surface under test.

Write `$OUT/after/COMMONS-UX-GRADE.md` from the draft (keep the draft). **Score** = PASS ÷
(12 − N/A). 100 % accept · ≥ 83 % accept-with-nits (file the FAILs, do not block) · < 83 %
revise.

Keep the draft's perf line and perf rows as they are: their verdicts come from the numbers, at the
budget itself (rubric §4 *Perf rows*). They sit **beside** the score and never change it, its `of` or
its band. A FAIL marked *noisy* (the CPU bench in the table's header is not within 20 % of the
reference, or the fastest run was within the budget) is re-measured on a quieter node before it is
filed; say so in the grade if you could not. A budget that has no row or reads UNGRADED is not passed.
When the change under review is yours, compare the before and after perf files, not only the verdicts:
a number that got worse but stayed on the same side of its budget is still worth a line.

### 3. Revise (author only)

Fix, re-shoot (`CELLS=` narrows to the failing cells while iterating, e.g.
`CELLS=phone-light,desktop-light`), re-grade. Keep a full matrix for the final grade. Stop
at accept or after 3 rounds; on round 3 with FAILs left, file them (Step 6) and report.
Then run the walks and goldens the surface touches:

```bash
cd gardenio/controller && GARDENHUB_GOLDENS=1 go test ./internal/gardenhub/ -run 'UnderChromium$|^TestGardenGoldens$' -count=1
```

A golden that moved because of your fix: say which and why in the PR, and commit no PNG. Never
commit `testdata/golden` in a feature PR (`scripts/check-no-pr-goldens.py` fails it); the window
bless PR re-shoots it on main (`gardenio/tests/gardenhub-browser/GOLDENS.md`, "Updating goldens").

### 4. Promote to a golden (accept only)

Rubric §5: a surface that grades accept becomes (or updates) its golden, so the gate guards a
graded look. The PR adds the shot, not the PNG. For an `EXTRA` surface, move its row into `SHOTS` in
`golden_browser.js` (use `clip: paneClip('<selector>')` for a pane, so the golden is the pane and not
~500 KB of farm behind it; `clip` is always a `clip(page)` that resolves to a viewport rect), add its `goldenSurfaces` tolerance row, `make
update-garden-goldens` to render-check it, look at every new PNG, then commit the code without them:
the next window's bless PR adds the golden. Stress fixtures stay out of the
goldens (they are for grading, not for guarding).

### 5. Attach the evidence

Copy pairs as `before-<file>.png` / `after-<file>.png` so they read as a pair, then:

| Destination | How |
|---|---|
| Bead | Build `urls.txt` with the command below (one `<file>: <viewer URL>` line per PNG), then `kd comment add <bead> --body "$(cat COMMONS-UX-GRADE.md; echo; cat urls.txt)"`. The URL opens for a human with a viewer session. |
| GitHub PR | Put the grade (score, band, FAIL list with mechanism, `N/A` reasons) in the PR body or a comment as text, plus the `gb image upload` links. The PNGs do not render in the PR, so the text must stand on its own. A promoted golden *does* render in the PR's file diff. |
| Slack thread | `gb slack upload after-<surface>-phone-light.png --comment "after: C1-C12 90 %"` |

```bash
for f in before-*.png after-*.png; do
  echo "$f: $(gb image upload "$f" --json --jq .url)"
done > urls.txt
```

`gb image upload` prints TWO lines without `--json`: the viewer URL (`https://<viewer>/image/<key>`,
stdout) and a "storage location" (`https://<bucket>.s3.amazonaws.com/<key>`, stderr). Only the
viewer URL belongs in `urls.txt` and the evidence bead: the bucket is private, so the storage link
404s for a human, and the garden shows a grade's pictures only through its `/image/` proxy. Ten
grades filed with `2>&1` captures showed no pictures until gd-tNvYsFiDb8; `--jq .url` cannot pick
the wrong line. Check before filing: `grep -c 's3.amazonaws.com' urls.txt` must print 0.

Never commit the work dir's PNGs (only goldens under `testdata/golden/`).

Then file the grade as data on the **step** the surface belongs to: the task you hold
(`$BOAT_TASK_ID`), not the PR. The comment stays, because people read it; the evidence bead is
what the Lands harvest cell and the bead view read (harvest spec
`gardenio/docs/superpowers/specs/2026-10-22-lands-harvest-icons-design.md` §2.1).
`score` is the gates passed, `of` the gates graded (12 − N/A), `percent` the Score, all over C1-C12
only. The perf result has fields of its own beside them (rubric §4): `perf_pass` and `perf_of`, the
budgets passed and graded (PASS + FAIL), and `perf_fail` and `perf_ungraded`, comma-joined ids. Drop the
four on a run that measured no perf:

```bash
STEP=${BOAT_TASK_ID:?the step this surface belongs to}
EV=$(kd create "UX grade 5/5: cursorphase" --type evidence --parent "$STEP" \
  -f kind=ux -f step="$STEP" -f subject=cursorphase \
  -f score=5 -f of=5 -f percent=100 -f urls=@urls.txt \
  -f perf_pass=7 -f perf_of=10 -f perf_fail=B1,B4,B5 -f perf_ungraded=B7 \
  -f pr=https://github.com/<owner>/<repo>/pull/<n> --jq .id)   # drop -f pr= with no PR
kd close "$EV"
```

The title is `UX grade <score>/<of>: <surface>`. A re-grade is a **new** evidence bead, never
an edit of the old one: the newest `kind=ux` bead on a step is the one the farm shows.

### 6. File what you found

Per perf FAIL that is not *noisy* and not already filed, the same with the budget for the gate
(`-l commons-ux:B4:<surface>`, P2), so a perf bug dedupes like a gate's. Per FAIL left open, a bug: `kd create "..." --type=bug -l project:gardenio -l commons-ux:<gate>:<surface>`,
priority from the rubric's severity table (P1: C1 auto-fail, unsafe C8, C9 lying state, C10
dup/lost; P2: C2-C6, C11, C12; P3: C7, C10 motion/noise; P0 and `security`: another land's
data on screen). Dedupe first (`kd list -l commons-ux:<gate>:<surface> --status=open`, comment
on the open one instead), and `kd dep add` it to the task or the Lands phase that owns the
surface.

## Output contract

Report in the bead comment and the PR:

```
commons-ux-acceptance <surface> — before <n>% (<band>) → after <n>% (<band>) — FAIL: C12
perf: before <p>/<of> → after <p>/<of> budgets PASS — FAIL: B1 B4 (noisy: B4) — ungraded: B7   bench <ms> ms (ref 61)
grade: <bead comment or path>   evidence: <evidence-id> <before/after PNG links>   golden: <promoted | updated | none>
bugs filed: <ids>   ungraded: <gate/cell + reason>   N/A: <gates + reason>
```

The perf line is beside the score, never in it: `<n>%` and the band are C1-C12 only. A run that
measured no perf says `perf: not measured`.

## Failure modes (report, never guess)

| Symptom | Meaning | Do |
|---|---|---|
| `FAILED <surface>-<cell>.png: … Timeout` in the output | `show()` never saw the surface (selector moved, fixture missing) | read the `error` in `manifest.json`; fix the row or the fixture; that cell is ungraded until then |
| `page errors:` in the draft | the page threw while being shot | evidence for the grade (C9 honesty); fix before grading anything else |
| a shot shows the login page, a 404 or real fleet data | not the fixture farm | stop; rubric §5 failure modes. `manifest.json` has the HTTP status |
| dark PNG byte-identical to light (the draft says so) | emulation did not apply | C5 ungraded |
| blank gaps where CJK text should be | the pod has no CJK font and the pinned Noto subsets have no CJK | the glyphs are ungraded under C7 (not FAIL, not PASS); see Known limits |
| `TestWorldFrameBudgetUnderNode` red in a full package run | frame-time budget on a busy pod, red on `main` too | not caused by a CSS change; CI's runner is the judge |
| `node --test …/probes.test.js` red | a probe regressed | fix `probes.js` before trusting any grade |
| perf rows marked *noisy*, the draft says the bench is not within 20 % | the pod's node is busy: times read 3-4x P0.1's (a 242 ms bench read the bead view's phone LCP at 9.6 s, desktop at 2.4 s) | bytes, requests and heap still stand; re-measure the times on a quieter node, or report them as noisy |
| `tti -` on every phone load in the output | no 5 s quiet window within 30 s, so B5 counts every long task in 30 s | the node is busy (see the bench); B5 is noisy |
| B6/B11 UNGRADED, `actError` in the perf file | the first interaction picked for the surface does not fit it (its control is not on screen) | give the row an `act` of its own in `perfRow` (`shoot.js`) |

## Self-test

```bash
node --test .claude/skills/commons-ux-acceptance/scripts/test/probes.test.js
make test-lands-perf-gate   # perf_gate.js, verdicts() included, and its budgets against rubric §7
```

Every probe fires on a page built to trip it, none fires on a page built to pass, and small
targets count only under a coarse pointer. CI runs it in `ci.yml`'s `gardenhub-browser` job, and
the perf gate's self-test in `lands-perf.yml`.

## Real device

Emulation cannot show WebKit, Safari's bars, the real keyboard, a passkey prompt, Add to Home Screen or sleep and
wake. A real phone can, and only a human holds one. The how-to is the device lab in `gardenio/docs/lands-mobile.md`
§4 (M4.2): the URL on the public edge, signing in through Keycloak, the walk per surface and what to look for. Do not
copy it here. What the grader does with it:

- **Ask.** When the change is about one of those things, say in the PR that emulation cannot show it, and name the
  surfaces and the walk's rows a human should do (in a check-in decision, or the M4.3 walk gd-YJPqjmLb47).
- **Name.** Real shots are `real-<surface>-<cell>-<theme>.png`, the cell the sweep's nearest one (`ios390`,
  `ios390land`, `and360`, `and360land`), so each sits beside its emulation shot.
- **Fetch.** Shots posted from the phone in Commons: `gb commons file <message-id>`, then Read every one, as in
  Step 2. A real shot is graded with the rubric like any other, but it shows real fleet data: it goes to the bead or
  the Commons thread (Step 5), never to a public link, and is never committed.
- **File.** One bug per problem (Step 6), with the phone model, OS and browser.

## Sideways scroll inside a pane

The `sideways` probe reads two things: the document wider than the viewport, and any box in the
probe root whose `overflow-x` is `auto` or `scroll` with `scrollWidth > clientWidth + 1`. The second
is a Workbench pane body pushed sideways inside itself by a long URL while the page stays put
(gd-JPFtrLnNxK); its hit names the scroller (`div#bv-body`, `636 > 390 (overflow-x: auto)`), not
the thing that pushes it, so find the wide child in the DOM: the scroll frames step down, never
across, and can stop above it.

Boxes built to scroll sideways are exempt (rubric C2: code blocks and tables scroll in their own
box): form fields, `pre` / `code` and everything inside a `pre`, a table's wrapper (`:has(> table)`),
the Glass terminal (`.gl-term`, `.xterm`), the picture strip and chip rows (`.pic-strip`,
`.cm-quick`, the docked bin's `.bin-rows`, a pane's tab and scope rows `.qv-tabs` / `.qv-scope`), a carousel with `scroll-snap-type` on x, and a box inside
a visually hidden one (the closed outline is clipped to 1x1 like `.sr`). A new strip or carousel that
is meant to scroll sideways opts out with a `data-scroll-x` attribute on the scroller itself (not an
ancestor); add a class to `SCROLL_X_OK` in `probes.js` only for a pattern used in several places.

## Known limits

- **Chromium only.** The sweep's iOS cells emulate Safari's user agent, size and touch; WebKit's own rendering (and
  Safari's toolbar resizing, the on-screen keyboard) is not shot. A human checks those on a real phone (Real device,
  above); the owner's walk is M4.3 gd-YJPqjmLb47.
- **`live` is not shot.** No SSE driver yet, so C10 and C8's after-click part are `N/A —
  static grade` for every surface. Driving one means a fixture endpoint that mutates a bead
  and fires the Hub event, then capture → event → capture → forced reconnect → capture.
- **No `docked` state** exists to shoot yet (Commons, Workbench and Glass are not built);
  `docked` runs only for a surface with a `dock(page)` step.
- **No card themes** in `/garden`: C6 is `N/A` everywhere for now.
- **CJK** renders blank in the pod (no CJK system font, none in the pinned subset). Arabic,
  Latin with combining marks and colour emoji render. Adding a CJK subset to
  `testdata/golden/fonts/` would close it at the cost of golden size.
- Desktop cells are 1440x900 (the rubric) while the goldens are 1280x800: an accepted capture
  is promoted by the window bless PR re-shooting it, never by copying the PNG.
- Contrast is computed from CSS, opacity included, not from pixels; text on the canvas, on
  images or gradients is `notRun` and needs a pixel sample.
- First live run: gd-vzsNYKoGNX graded the bead view 60 % → 90 % and promoted it to a golden.
  What was hard to answer there is in that bead's comment; fold more back into this file and
  the rubric as more surfaces go through.
