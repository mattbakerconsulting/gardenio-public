# Commons v2 step 6: composer and switch latency (2026-11-03)

Step 6 ([[gd-Txos7COlj0]]) of plan [[gd-lRaCxsiLvC]], graded against step 1's
budgets (`gardenio/docs/commons-ux-rubric.md` §7; the "before" is
[commons-v2-baseline-2026-10-31.md](commons-v2-baseline-2026-10-31.md)).

**Run:** `make commons-ux-shoot SURFACES=commonsthread,commonsplan CELLS=desktop-light
PERF_RUNS=3 PERF_IDLE=5` on an agent pod, at `main` 9058da4ac ("before") and at
this step's branch ("after"). The fixture farm, medians of 3 cold loads. The
surfaces are the workbench (`web/commons2.js`) on a thread root (gd-wishPorch)
and a plan root (gd-wbPlan); each opens a thread, a plan and the stress epic
(gd-wbStress, 40 tasks with F1–F6 titles) by Ctrl-K, cold and then warm.

The pod was busy throughout (load average 11–15 on 2 CPUs). The phone's CPU
bench read 97–226 ms against the 61 ms reference before and 176–385 ms after,
so every phone time below is **noisy** by the rubric's rule (bench not within
20 %) and reads high. Bytes and request counts did not move (B1, B2, B16 pass).

## Medians, before and after

| Surface, profile | B12 key (≤ 16) | B13 echo (≤ 50) | B14 thread cold/warm | B14 plan cold/warm | B14 epic cold/warm | bench |
|---|--:|--:|--:|--:|--:|--:|
| thread root, phone, before | 33 | 322 | 118 / 175 | 366 / 174 | 565 / 275 | 97–226 |
| thread root, phone, after | 36 | 241 | 273 / 177 | 306 / 326 | 1002 / 228 | 176–385 |
| plan root, phone, before | 23 | 318 | 397 / 176 | 196 / 189 | 552 / 313 | 111–123 |
| plan root, phone, after | 37 | 207 | 456 / 177 | 169 / 155 | 760 / 298 | 207–274 |
| thread root, desktop, before | 5 | 37 | 42 / 38 | 74 / 33 | 77 / 83 | – |
| thread root, desktop, after | 10 | 30 | 45 / 44 | 73 / 27 | 208 / 37 | – |
| plan root, desktop, before | 6 | 40 | 61 / 32 | 34 / 39 | 101 / 58 | – |
| plan root, desktop, after | 9 | 21 | 54 / 39 | 58 / 39 | 156 / 67 | – |

B12's rows repainted by a key stay 0 everywhere. On the desktop every B12–B14
row is inside its budget, before and after; an earlier run of this branch,
before the kept trees, read the stress epic warm at 109 and 156 ms. The phone
rows do not pass on this pod. `perf_gate.js` grades B14 only on the `channel`
and `commons` surfaces (`OPEN_FROM`), so the B14 rows here are read off the
latency pass by hand.

## What changed, and where the phone's time goes

- **Send is optimistic.** The message is in the transcript, pending, in the
  frame after the click or Enter, and the POST starts after that frame. The
  stream's copy or the answer settles it into the server's row. With neither
  in 3 s, or a refusal, it says so and offers Retry. Traced on the phone
  profile, the workbench's own share of the echo is one frame (about 20 ms
  from `click` to paint). Most of the rest happens before the workbench sees
  the tap: Playwright's tap sends touchStart and touchEnd as two CDP calls, and
  on this pod `click` came 35–80 ms after `touchstart`, which is when the
  harness starts timing. The frame itself costs another 40–80 ms at 4x on a
  busy node.
- **A key repaints no row.** The transcript list is `contain: content` and the
  typing signal (now sent from the workbench too, at most every 3 s) goes out
  after the frame the key drew. A traced key is about 16 ms of the browser's
  own work at 4x (key events, text insertion, layout, paint), with none of the
  workbench's code in it. B12 on the phone is that, plus the wait for the
  next frame.
- **Warm switches come from memory.** Fragments (48), threads (8) and trees
  (8) are kept in LRUs. After a node event the fragments are served stale and
  revalidated. A tree left for another root stays in the column with
  `content-visibility: hidden`, so opening that root again shows it with its
  layout kept. Before that change, the stress epic's warm open on the desktop
  was 41 ms of layout and 29 ms of paint, at 1x, for the tree it had just
  rebuilt.
- **Prefetch.** A row's node is fetched on hover or focus, and so is the next
  row in the direction j/k is moving. A Ctrl-K jump, which is what the latency
  pass times, is not prefetched, so the cold B14 rows still pay one 170 ms
  round trip plus the render.
- Drafts are kept per node, unread marks come from the stream (tree rows now
  carry `data-thread`), and Open here re-roots without a page load, keeping
  the document when it already shows the node.

## Still open

- The phone rows need a quiet node to grade: step 9 ([[gd-4s2Rw9hgBd]]) puts
  the latency pass into `lands-perf.yml`.
- Cold opens by Ctrl-K pay the round trip. Prefetching from the jump field, or
  sending the children's fragments with the root, would take that off. Both
  are left for later.
