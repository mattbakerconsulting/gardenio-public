# /threads visual grade — Phase 3 rubric ka-0000007635 (V1–V12)

Task 3-3 (ka-0000004063), epic ka-0000002477 / ka-0000000679. Operator: Matt Baker.

> **V5 + V10 update (task 3-4, ka-0000003162):** the composer + optimistic-send
> cells the table below marks `N/A-until-2-5` are now GRADED in
> [`THREADS-3-4-GRADE.md`](./THREADS-3-4-GRADE.md). Both V5 (composer
> reachable: textarea, focus ring, keyboard, no 480px h-scroll) and V10
> (optimistic bubble + ?since= reconcile, zero duplicate) are **PASS**.

Graded from the hermetic capture matrix (`tools/capture-threads.mjs`): 126
screenshot pairs (first-viewport + fullPage) across **SKINS** {baseline,
dark-terminal, dark} × **WIDTHS** {480, 768, 1280} × **cells** (index
{empty, populated, error}, detail {empty, populated, fulfilled, needstriage,
longthread, error}, bead-show chip {resolved, fallback, absent}) plus the live
`.thread2-work > summary` work-expand interaction.

Fixtures mirror `threads_test.go` (`addHead` / `addMemberByField` / `addChild`
/ `GetComments`): one open multi-message wish, one closed/fulfilled wish, one
needs-triage wish, a long-thread wish (16 replies + 9 work beads), an empty
wish, a member bead with `fields.thread_id` (chip resolves), an orphan member
whose head is missing (chip falls back), and an empty project scope.

## Verdict summary

| Gate | What it checks | Verdict | Notes |
|---|---|---|---|
| **V1** | No blank/white panes in any cell; error keeps full page chrome | **PASS** | Index empty → "No wishes yet" panel; detail empty → "opened this wish" + "No agent work yet"; index error → "Failed to load threads." banner + full chrome; detail error → "Failed to load thread timeline." banner with the wish head still rendered. |
| **V2** | Conversation-first / work-collapsed default on detail load (`<details>` CLOSED) | **PASS** | `<details class="thread2-work">` renders with NO `open` attribute on every detail cell; "Agent activity ▸ N beads" is collapsed; conversation lane leads. Verified in DOM (`grep` on rendered HTML) and visually. |
| **V3** | Wish narrative leads on the index; work tally demoted to quiet meta | **PASS** | Index cards lead with the bold wish text (`slack_root_text`); the work count ("N beads of work" / "no work yet") is a muted `.threads-work` meta cue below the chips. |
| **V4** | Readable contrast in BOTH themes (data-skin=dark trap is a tracked defect) | **PASS (baseline, dark-terminal)** · **FAIL (dark)** → **defect filed** | baseline + dark-terminal are fully legible across every cell. `data-skin="dark"` is the contrast-trap probe: the thread templates author only `dark-terminal` overrides and layout.html ships no `--bg` block for `dark`, so the thread surface silently falls back to the light baseline. Filed as a defect bead (linked in the grade). `dark` is not in the skin picker, so this is latent, not user-reachable today — but the 42+ `[data-skin="dark"]` color rules in layout.html make it a real trap to fix. |
| **V5** | Composer reachable: labelled textarea, focus ring, keyboard-operable | **N/A-until-2-5** | The composer fragment lands in task 2-5 (ka-0000006034) / 3-1. NOT captured, NOT graded here. Marked N/A — not fake-passed. |
| **V6** | No horizontal scroll / clipped cards at 480px; mobile single-column | **PASS** | At 480 the two-pane grid collapses to a single column (rail stacks above conversation); cards wrap, no horizontal overflow, work stays collapsed. Verified on index + every detail state in all three themes. |
| **V7** | Desktop two-column rail sticky; layout holds on long-thread scroll | **PASS** | At ≥961px the wish rail is `position: sticky`; the long-thread cell (16 replies + 9 work beads) holds layout through scroll, conversation column flows independently. |
| **V8** | part-of-wish chip present on member bead-show, links to /threads/{id}, unresolved fallback works, absent on heads | **PASS** | Resolved: "🧵 PART OF WISH: Our prod deploy has been red…" links to `/threads/gd-0netj9sn9P`. Fallback (head missing): "🧵 PART OF WISH: gd-wishGONE999" (bare id, still links). Absent: 0 chip anchors rendered on the thread head. Legible in baseline + dark-terminal (chip has a dedicated dark-terminal override). |
| **V9** | needs-triage banner + fulfilled close-marker render | **PASS** | needs-triage: amber "Needs triage. An agent stopped before this wish was fulfilled…" banner + NEEDS_TRIAGE pill. fulfilled: CLOSED + green "fulfilled" pill + "matthew.baker closed this wish" close marker in the conversation lane. |
| **V10** | Optimistic send: bubble appears immediately; ?since= poll reconciles, zero dup | **N/A-until-2-5** | ?since= polling + optimistic send land in task 2-5 / 3-1. NOT captured, NOT graded. Marked N/A — not fake-passed. |
| **V11** | Alignment/spacing sane; no overflow/overlap | **PASS** | Cards, chips, lane heads, work events, and the rail facts list are consistently aligned across all breakpoints; no overlap or clipped text observed in any captured cell. |
| **V12** | Every FAIL cell links its uploaded screenshot URL | **PASS** | The single FAIL family (V4 on `dark`) links its screenshots in the contact sheet + the defect bead. |

## The one FAIL: V4 on `data-skin="dark"` (defect)

**Mechanism.** layout.html defines `--bg`/`--text` skin-variable blocks for
`baseline`, `minimal-refined`, `dense-admin`, `docs-notion`, `dark-terminal`,
and `tufte` — but NOT for `dark`. Yet it carries 42+ `[data-skin="dark"]`
bright-on-dark color rules (chips, gardener verdicts, agent-state colors…) that
assume a dark background that the `dark` skin never actually sets. The
`/threads` templates compound this: they author `html[data-skin="dark-terminal"]`
text-color overrides but no `data-skin="dark"` equivalents. Net effect: selecting
`dark` renders the thread surface in the light baseline palette while a scattering
of layout-level rules paint bright-phosphor text — a latent contrast trap.

**Reachability.** `dark` is NOT one of the six options in the skin-switcher
`<select>` (layout.html:2937), so a user cannot select it through the UI today.
It is reachable only by setting `localStorage['beads-viewer:skin']='dark'`
directly. So the severity is **latent**, not a live user-facing break — but the
trap is real and worth closing (either register `dark` as a full skin with a
`--bg` block, or drop the orphan `[data-skin="dark"]` rules).

**Evidence.** Compare `threads-detail-populated-dark-1280.png` (light fallback)
against `threads-detail-populated-dark-terminal-1280.png` (correct dark) — same
page, same data, the `dark` one is the baseline light theme.

## Cells NOT graded (correctly N/A)

- **V5 composer**, **V10 optimistic send + ?since= reconcile** — these surfaces
  do not exist until task 2-5 (ka-0000006034) / 3-1 merge. They are marked
  `N/A-until-2-5` and were not captured. Re-run this matrix after 2-5 lands to
  grade them; the harness already has the URL slots (add composer cells to
  `urlPlan()`).
