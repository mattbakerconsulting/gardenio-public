# World render profile on the 10x farm (P2.1)

Step P2.1 (gd-PoCdbE1SJX) of the Lands UX performance pass (plan gd-MEGB3dEaHu): profile `/garden`'s World in Chromium
on the 10x fixture (`testdata/world_tenx.js`: 50 projects, 360 plans, about 12k task cells, 60 gardeners) while it is idle,
panning and zooming, and fix what the trace shows. The gate is no long task over 100 ms during a 10 s pan on the phone
profile.

**Result (P2.1): the fixes below land, and the gate is still missed on an agent pod: the longest pan task is 485–600 ms at 4x
(it was 572–603 ms).** That pod cannot measure the gate. A page that does nothing but repaint a full-screen canvas with 50
small fills a frame has long tasks of 178–469 ms there at the same 4x throttle. See [Reading the gate](#reading-the-gate).

## Rerun it

```bash
make world-pan-profile OUT=/tmp/world-pan        # ~2 min; fails when a long task in the pan runs over 100 ms
```

`TestWorldPanProfile` (`gardenio/controller/internal/gardenhub/world_pan_profile_test.go`) serves the goldens' fixture farm
and runs `internal/gardenhub/testdata/world_pan_profile.js` against it. The script answers the page's `/garden/farm` with
the 10x document and its stream with a heartbeat. It writes `OUT/report.json`. It first records a calibration (a bare
full-screen canvas at the same profile), then three phases:

- **idle:** 5 s of nothing.
- **pan:** 10 s of the cursor walking the world. A pad arrow (a CDP touch at the button's centre, not `page.tap()`, whose
  selector engine runs in the page's own main thread) or a swipe lands every 300 ms, so the camera is always easing.
- **zoom:** descends to a phase and back out by the pad, then a pinch and a spread.

The script's header lists the knobs for digging in: `WORLD_PAN_CPU=1` (no throttle), `WORLD_PAN_DPR`,
`WORLD_PAN_CPUPROFILE=1` (a `.cpuprofile` per phase) and `WORLD_PAN_TRACE=1` (a DevTools trace per phase).

## What the trace showed, and the fixes

| Finding | Fix |
|---|---|
| **Idle repainted ~15 times a second.** While a decision was open (`body.has-ripe`), the moon's glow animated `box-shadow`. That repaints on the main thread, so each frame was a paint plus a ~30 ms commit at 4x. Over 5 s of idle that made 32 long tasks (max 126 ms) with no `requestAnimationFrame` running. | The glow is now a `::after` halo whose `opacity` animates. The compositor runs that alone. Idle now has 0 long tasks and 0 frames. |
| **The canvas's backing store was the frame's biggest cost.** A 2D canvas without a GPU records its draw calls and rasterizes them at Commit, so Commit is the canvas's pixels. With `#cv` hidden, Commit over 12 moves fell from 8604 ms to 278 ms. Hiding the labels or the header changed nothing. At the Pixel 5's 2.75x the store is 1073x2321, and a frame repaints 1–4 screens of it. | The backing store is capped at 2x (`DPR_MAX`), and `#cv` scales up with `image-rendering: pixelated`. On a 2.75x phone that is 47% fewer pixels, and Commit over the same 12 moves fell from 8604 ms to 2916 ms. The art's pixels were already a non-integer count of device pixels at 2.75x. Goldens shoot at 1x and 2x, so they are unchanged. |
| **Layout thrash in the World-level header plates.** `boards()` appended each new plate and read its `offsetWidth` straight away: one forced layout per plate, up to 12 per cursor move. | All new plates are appended first, then measured together: one layout. |
| **Layout reads after label writes.** `boards()` and `syncBin()` re-read the HUD (`hudRects()`, `signVp()`) after `refreshSigns` had written the labels. | The HUD and the map's insets are read once, at the top of `refreshSigns`, before any write, and handed down. |
| **`SIGNFIT.ride` measured the camera buttons once per label** (a `querySelectorAll` plus `getBoundingClientRect` each), and built the keep-off list once per label per frame. | The buttons are measured once per pass (dropped after the task), and the keep-off list is built once per arrow list. |
| **`ART.palette()` was rebuilt on every call.** The map asks for one per biome per frame, and a night palette recolours ~60 hexes. | Palettes are memoized by id (the same id is already the same colours for the sprite cache). The cache is capped. |

Already fine, and left alone: rendering stops when nothing moves (`queueRender` on demand, 0 rAF callbacks while idle),
offscreen draws are culled (`WORLD.chunks` and `WORLD.visible` cut to the view), and native chunk bakes are budgeted per
frame.

## Before and after

The same agent pod and harness, `main` (c61fb7fa) against this branch, alternating. Long tasks are n / total / max (ms).

| Run | Calibration max | Idle | Pan | Pan frames | Zoom |
|---|--:|---|---|--:|---|
| 4x, main | 401 | 16 / 1662 / 239 | 47 / 9501 / 603 | 69 | 69 / 18625 / 1124 |
| 4x, branch | 469 | 0 / 0 / 0 | 56 / 8039 / 600 | 85 | 68 / 17031 / 1156 |
| 4x, branch (round 2) | 178 | 0 / 0 / 0 | 46 / 7800 / 485 | 79 | 64 / 15188 / 2223 |
| 4x, main (round 2) | 203 | 7 / 521 / 103 | 47 / 9265 / 572 | 61 | 66 / 18655 / 1493 |
| 1x, main | 71 | 4 / 266 / 83 | 52 / 6253 / 445 | 139 | 54 / 6476 / 490 |
| 1x, branch | 103 | 0 / 0 / 0 | 42 / 3813 / 352 | 236 | 24 / 2103 / 190 |

At 4x the pan spends 15–16% less time in long tasks and paints 23–30% more frames. At 1x, where the app's own cost is least buried under the throttle, the pan spends 39% less time in long tasks and
paints 70% more frames. The zoom spends 68% less, and its longest task is down from 490 ms to 190 ms. The 4x zoom's longest task is noise-bound: 1156 and 2223 ms on the branch against 1124 and 1493 ms on main, while its
long-task total fell 9% and 19%. At 4x the runs swing with the node's load (load average 10–13 on a 1.5-CPU quota), as the calibration column shows.

## Reading the gate

- **The pod is below the gate's floor.** The calibration page does nothing but fill 50 small rectangles a frame on a
  full-screen canvas at 2x. At 4x it reaches 17–27 frames a second, with long tasks of 178–469 ms. No full-screen canvas
  frame can be under 100 ms there. The 4x throttle assumes a fast host; a shared pod is several times slower than one.
- **A phone draws its canvas on the GPU.** Headless Chromium on the pod has none, so a 2D canvas is rasterized on the
  renderer's main thread at Commit. That is the dominant cost in the traces here (see the first two findings). On a phone it
  is GPU work, off the main thread.
- **What is left in the longest pan tasks is the cursor move itself.** The tap's `pointerdown` runs the whole move
  synchronously: `syncHots`, the first camera step, then `refreshSigns`, which rebuilds the labels and forces a layout to
  measure them. That is about 380 ms at 4x on the pod. A slow move can also let the pad's 300 ms auto-repeat fire a second
  move before `pointerup` is handled.

So the gate needs either a measurement that can show it, or more work that cuts the move:

1. Measure on a dedicated runner or a real mid-tier phone (P3.1's gate), with GPU raster. Gate on long-task totals
   relative to the calibration floor, not on a raw maximum from a shared pod. **Done in gd-K3KmHhfnhd:** the CI gate
   (`lands-perf.yml`) runs this script's calibration, pan and zoom three times per side through `perf_baseline.js`
   (`kind: pan` rows), and `perf_gate.js` grades B7 at 100 ms where the floor is within it, else on the pan's long-task
   load over the bare canvas's, against the base (rubric §7, B7). Its first run, on an agent pod at CPU bench 125–226 ms
   (`make lands-perf-baseline SURFACES=world PROFILES=phone IDLE=0 SETTLE=0`, about 60 s per pan run):

   | Run | Calibration long tasks (n / total / max) | Pan long tasks | Pan frames | Load |
   |--:|---|---|--:|--:|
   | 1 | 12 / 1940 / 375 | 42 / 8336 / 491 | 49 | 0.82x |
   | 2 | 15 / 2110 / 376 | 34 / 8958 / 855 | 49 | 0.82x |
   | 3 | 13 / 2185 / 496 | 38 / 7874 / 687 | 59 | 0.85x |

   The pan's longest task swung 491–855 ms while the load held at 0.82–0.85x. But the bare canvas spent about 97 % of
   its 2 s in long tasks, so on that node the load could not read much over 1.03x: a pod that busy shows a gross
   regression at best, and the gate marks the load ungraded where its ceiling is under the fail line.
2. ~~Split the cursor move across tasks.~~ Done in gd-fsuxCGGbZr; see [the follow-up](#follow-up-the-move-split-and-one-grass-fill).
3. ~~Cut canvas overdraw at the World and Project levels.~~ Done in gd-fsuxCGGbZr, the same follow-up.

## Follow-up: the move split and one grass fill

gd-fsuxCGGbZr did items 2 and 3 above.

- **A cursor move's signs run in a task of their own.** `cursorMoved` keeps the move, the hot links, the camera and the
  live region in the input task, and queues `refreshSigns` with a zero-delay `setTimeout` (`signsNext`). Held-arrow moves
  that land before that task runs share one refresh. The node mock (`testdata/world_dom_mock.js`) now queues zero-delay
  timers and runs them in `flush()` before the animation frames, so the label cost is still inside each frame's budget.
  `world_frame_test.js` reports the input task alone as `input_median_ms`, and fails a walk whose moves stop queuing that
  task. At the World depth the input task is 0.83 ms of a 3.3 ms move at the median.
- **World and Project draw one grass fill, not chunks.** The 1/4 and 1/16 mip chunks were one flat grass fill each
  (`ART.drawWild` without detail), blitted scaled and smoothed over the whole view under the blobs. `render()` now fills the
  canvas with that grass and draws the blobs over it. An unbaked native chunk's placeholder is the same flat fill.
  `WORLD.MIPS` and `WORLD.mipFor` are gone. The 78 map goldens (world, cursor, project, plan, birds, beacons and the rest)
  still match.

Same pod, the branch against `main` (9295def4), alternating. Pan long tasks are n / total / max (ms).

| Run | Calibration max | Pan | Pan frames | Zoom |
|---|--:|---|--:|---|
| 4x, branch | 441 | 64 / 8522 / 444 | 83 | 80 / 24756 / 1810 |
| 4x, main | 455 | 40 / 8653 / 744 | 47 | 64 / 31950 / 2746 |
| 4x, branch (round 2) | 231 | 75 / 7025 / 219 | 130 | 56 / 8341 / 643 |
| 4x, main (round 2) | 112 | 71 / 6673 / 231 | 158 | 72 / 8786 / 522 |
| 1x, branch | 0 | 1 / 54 / 54 | 507 | 4 / 308 / 120 |
| 1x, main | 0 | 19 / 1326 / 201 | 289 | 30 / 2271 / 151 |
| 1x, main (round 2) | 0 | 11 / 1033 / 355 | 306 | 30 / 2091 / 126 |
| 1x, branch (round 2) | 0 | 7 / 417 / 77 | 384 | 16 / 1162 / 153 |

At 1x, where the calibration page has no long tasks, the pan's long-task total falls 60–96% (1326 and 1033 ms to 54 and
417 ms). Its longest task falls from 201 and 355 ms to 54 and 77 ms, under the 100 ms gate. The pan also paints 25–75% more
frames, and the zoom spends 44–86% less time in long tasks. At 4x the calibration max moved from 455 to 112 ms between runs,
so those rows measure the node's load more than the page. The first pair is the only one under like load (441 and 455 ms):
there the pan's longest task fell from 744 to 444 ms and it painted 77% more frames. At 4x the
CI's B7 grade of item 1 reads it against the bare canvas.
