# Lands performance baseline (P0.1)

The baseline the Lands UX performance pass (plan gd-MEGB3dEaHu) measures every later change against: `/garden` and each
Commons, Workbench and Glass surface, loaded cold and warm on a throttled phone and on a desktop. Step P0.1
(gd-j8jXq1TC1m). P0.2 turns the proposed budgets into hard numbers from this table; P3.1's CI gate reruns the same
script.

The before/after against this baseline, with every P1–P3 step in, is [`perf/checkin-2026-11-02.md`](perf/checkin-2026-11-02.md)
(P3.3, gd-mIUDS8zFaD).

## Rerun it

```bash
make lands-perf-baseline OUT=/tmp/lands-perf                                  # ~45 min: 10 surfaces x 2 profiles x cold/warm x 3 runs + 300 s idle
make lands-perf-baseline OUT=/tmp/lands-perf SURFACES=world,channel PROFILES=phone RUNS=1 IDLE=0   # a quick look
```

`TestLandsPerfBaseline` (`gardenio/controller/internal/gardenhub/perf_baseline_test.go`) serves the goldens' fixture farm
(`cardWorld` at `goldenNow` with `goldenPorch`) and runs `internal/gardenhub/testdata/perf_baseline.js` against it. It
writes `results.json` (every run, with per-request detail and the LCP element) and `summary.md` (medians). The script's
header defines every metric. Against a deployed `/garden`, run the script by hand with a signed-in session:

```bash
make gardenhub-browser-deps
LANDS_PERF_COOKIE='<session cookie name>=<value>' LANDS_PERF_PATHS='{"beads":"/garden?bead=<id>","plans":"/garden?plan=<id>","glass":"/garden?glass=<agent bead>","thread":"/garden/c/<land>/general/t/<id>"}' \
  node gardenio/controller/internal/gardenhub/testdata/perf_baseline.js https://gardenio.clonk.tv gardenio/tests/gardenhub-browser/node_modules /tmp/lands-perf-prod
```

### P2.2: the long thread (gd-NRi4Fdu79J)

A thread of 1,000 replies (the server sends the newest 500). Its gate is INP ≤ 200 ms on the phone, checked on every PR
by `TestCommonsLongThreadUnderChromium` (`testdata/commons_long_browser.js`), not by a perf_baseline.js surface: the
P3.1 gate below holds a surface its PR adds to B1, which no surface meets yet. The walk times 43 typed
keys in the reply composer at 4x CPU, in one of up to three rounds of the same words. One round can fail on a shared
runner, since one stalled frame decides INP; a slow page fails every round. Measured on a shared agent pod, worst key
per run: before P2.2 (all 501 rows on the page) 88, 120 and 256 ms over three runs, a typical key 48–56 ms; with the
window (the root and the newest 60 replies, the rest behind an Earlier row) 96, 104, 104 and 168 ms over four, a typical
key 32–40 ms.

On a busier pod (1.5 CPUs, node load ~13) the worst key ran 128–440 ms on the same code, so a time-based worst-key
number there is mostly noise. Renderer main-thread CPU over the 42 keys does not swing like that (Chrome trace,
thread time): main's commons.js 3,100 ms, the window 2,030 ms. Two CSS rules took it to 1,400 ms. `.cm-messages
{contain:content}` stops a key repainting the rows (Paint 260 → 75 ms). The phone sheet hides the map it covers
(`body.cm-sheet .wrap{visibility:hidden}`, the Workbench panel over the sheet kept), so a frame no longer commits the
map's layers (Commit 550 → 80 ms). What is left is Chromium's own editing, selection sync, layout and commit; the
composer's handlers are ~4% of it.

## The CI gate (P3.1)

`.github/workflows/lands-perf.yml` reruns this script nightly on main (main's tip against main 25 h earlier) and on
dispatch (`gh workflow run lands-perf.yml --ref <branch>`: the branch against its merge-base with main), never on a pull
request (owner rule 2026-10-31, gd-yqSVQsjtyW), and grades both sides with `internal/gardenhub/testdata/perf_gate.js` against the budgets in
`gardenio/docs/commons-ux-rubric.md` §7. It uses 3 runs, a 60 s idle pass and no settled window (`LANDS_PERF_SETTLE=0`).
The rules are in the rubric under "The CI gate" and in `perf_gate.js`'s header. The same steps run locally:

```bash
make lands-perf-baseline OUT=/tmp/lp-head RUNS=3 IDLE=60 SETTLE=0 TRACE=/tmp/lp-head/traces   # on the branch
make lands-perf-baseline OUT=/tmp/lp-base RUNS=3 IDLE=60 SETTLE=0                             # on its base
make lands-perf-gate HEAD=/tmp/lp-head BASE=/tmp/lp-base OUT=/tmp/lp-gate                     # gate.md, exit 1 on a regression
```

`TRACE=<dir>` adds one cold load per surface on the phone under a Playwright trace, `<dir>/phone-<surface>.zip`. That load
is not measured. Open a trace at https://trace.playwright.dev.

The dispatch path was proved once on the PR that took both workflows off pull requests (#1758, at befc1fd2, 11-01):
[lands-perf 41273450651](https://github.com/mattbakerconsulting/gardenio/actions/runs/41273450651) and
[reachability 41273453009](https://github.com/mattbakerconsulting/gardenio/actions/runs/41273453009) both concluded
green. A dispatch cannot run the nightly's "main 25 h earlier" base step; the first scheduled run on main proves that.

## Method

- **Phone:** 390x844, touch, mobile UA, 4x CPU slowdown, 9 Mbps down, 1.5 Mbps up, 170 ms RTT (CDP emulation: latency
  per request, not a TCP model). **Desktop:** 1280x800, unthrottled.
- **Cold:** a fresh browser context. **Warm:** the same context loads the surface again.
- **Surfaces and first interactions:** World `/garden` (pad Right on a phone, ArrowRight on a desktop); Commons channel
  (opening a thread); Commons thread (a key in the reply box); Commons compose (`@` in #general's box, which opens the
  mention list); Workbench bead view (a tap in the body); plan cockpit (its first step); inbox and decisions (switching
  queue tab); Glass watch and the Wall (a tap on the pane).
- **TTI** is Lighthouse-style. From FCP, it finds the first 5 s window with no long task and at most two requests in
  flight, and reports the end of the last long task before that window. **INP** is the first interaction's longest Event
  Timing entry (Chromium reports none under 16 ms). **Settled** counts the long tasks in the 10 s after that interaction.
- **Machine:** an agent pod with 2 CPUs on a shared node, Chromium 145.0.7632.6.

## Baseline: fixture farm, 2026-10-28

Medians of 3 runs. Times are in ms from navigation start; sizes are in KB. Raw means decoded bodies; wire means as served
(the app gzips `/garden` and the farm snapshot itself); gz means every body gzipped.

### Phone (4x CPU, 4G)

| Surface | Load | Raw | Wire | gz | Req | TTFB | FCP | LCP | Ready | TTI | INP | Long tasks to TTI (n / total / max) |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| World | cold | 1496 | 456 | 453 | 4 | 178 | 424 | 424 | 2584 | 2891 | 184 | 10 / 1568 / 263 |
| World | warm | 1496 | 1.2 | 453 | 4 | 176 | 680 | 680 | 2269 | 2536 | 296 | 11 / 1455 / 273 |
| Commons channel | cold | 1510 | 472 | 457 | 12 | 176 | 452 | 3392 | 3621 | 3155 | 40 | 13 / 1988 / 320 |
| Commons channel | warm | 1509 | 15 | 456 | 12 | 176 | 524 | 3116 | 3489 | 3030 | 40 | 11 / 1853 / 498 |
| Commons thread | cold | 1539 | 500 | 477 | 12 | 177 | 432 | 3464 | 4119 | 3787 | 88 | 12 / 1865 / 393 |
| Commons thread | warm | 1539 | 46 | 477 | 13 | 176 | 548 | 3204 | 3743 | 3149 | 88 | 11 / 1599 / 255 |
| Commons compose | cold | 1510 | 472 | 457 | 11 | 177 | 428 | 3260 | 3376 | 2936 | 64 | 14 / 1720 / 265 |
| Commons compose | warm | 1509 | 15 | 456 | 12 | 176 | 524 | 3028 | 3598 | 2948 | 64 | 11 / 1705 / 342 |
| Workbench bead view | cold | 1497 | 457 | 453 | 5 | 178 | 540 | 3616 | 3765 | 3981 | 40 | 16 / 2525 / 366 |
| Workbench bead view | warm | 1497 | 2.5 | 453 | 5 | 176 | 540 | 3144 | 3290 | 3308 | 48 | 14 / 2120 / 419 |
| Workbench plan cockpit | cold | 1499 | 459 | 454 | 5 | 179 | 476 | 2648 | 2653 | 2471 | 104 | 12 / 1468 / 273 |
| Workbench plan cockpit | warm | 1499 | 4.6 | 454 | 5 | 178 | 536 | 2732 | 2740 | 2717 | 112 | 13 / 1570 / 336 |
| Workbench inbox | cold | 1496 | 457 | 453 | 5 | 179 | 520 | 2452 | 2794 | 2726 | 176 | 13 / 1706 / 266 |
| Workbench inbox | warm | 1496 | 1.7 | 453 | 5 | 175 | 548 | 2348 | 2583 | 4872 | 120 | 14 / 1683 / 250 |
| Workbench decisions | cold | 1497 | 457 | 453 | 5 | 178 | 560 | 2752 | 3199 | 3132 | 112 | 13 / 1723 / 356 |
| Workbench decisions | warm | 1497 | 2.2 | 453 | 5 | 178 | 572 | 2872 | 3111 | 3110 | 96 | 12 / 1737 / 273 |
| Glass (watch) | cold | 1781 | 742 | 520 | 8 | 179 | 492 | 3252 | 4008 | 3987 | 40 | 16 / 2045 / 323 |
| Glass (watch) | warm | 1497 | 2.8 | 453 | 8 | 181 | 552 | 2832 | 3222 | 3202 | 56 | 13 / 1703 / 263 |
| Glass (Wall) | cold | 1497 | 457 | 453 | 5 | 178 | 416 | 2816 | 2917 | 2904 | 72 | 12 / 1477 / 267 |
| Glass (Wall) | warm | 1497 | 2.4 | 453 | 5 | 178 | 544 | 2900 | 3035 | 2875 | 48 | 11 / 1478 / 275 |

### Desktop

| Surface | Load | Raw | Wire | Req | TTFB | FCP | LCP | Ready | TTI | INP | Long tasks (n / total / max) |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| World | cold | 1496 | 456 | 4 | 13 | 164 | 164 | 501 | 599 | 32 | 3 / 186 / 69 |
| World | warm | 1496 | 1.2 | 4 | 5 | 268 | 268 | 855 | 852 | 32 | 6 / 443 / 135 |
| Commons channel | cold | 1510 | 472 | 11 | 32 | 276 | 988 | 1042 | 801 | 48 | 3 / 222 / 95 |
| Commons channel | warm | 1509 | 15 | 11 | 4 | 164 | 676 | 733 | 544 | 56 | 3 / 179 / 74 |
| Commons thread | cold | 1541 | 502 | 13 | 15 | 184 | 732 | 865 | 570 | 48 | 2 / 142 / 74 |
| Commons compose | cold | 1510 | 472 | 11 | 13 | 160 | 704 | 726 | 541 | 40 | 2 / 135 / 83 |
| Workbench bead view | cold | 1497 | 457 | 5 | 21 | 184 | 772 | 862 | 676 | 24 | 3 / 226 / 72 |
| Workbench plan cockpit | cold | 1499 | 459 | 5 | 21 | 192 | 700 | 705 | 646 | 40 | 2 / 144 / 80 |
| Workbench inbox | cold | 1496 | 457 | 5 | 14 | 204 | 752 | 774 | 719 | 24 | 3 / 258 / 122 |
| Workbench decisions | cold | 1497 | 457 | 5 | 19 | 200 | 912 | 1023 | 988 | 32 | 4 / 359 / 98 |
| Glass (watch) | cold | 1781 | 742 | 8 | 13 | 196 | 876 | 867 | 785 | 32 | 3 / 237 / 83 |
| Glass (Wall) | cold | 1497 | 457 | 5 | 17 | 196 | 760 | 777 | 1020 | 40 | 3 / 216 / 93 |

Desktop warm loads are within about 150 ms of cold on every surface. All of them are in `results.json`.

### Idle, 300 s with every surface open at once

| Surface | Phone heap loaded / idle / after GC (MB) | Desktop heap loaded / idle / after GC (MB) | Idle requests (phone) | Idle KB (phone) |
|---|---|---|--:|--:|
| World | 4.3 / 3.8 / 3.5 | 4.2 / 3.4 / 3.1 | 8 | 6.9 |
| Commons channel | 5.2 / 4.9 / 4.3 | 5.9 / 3.9 / 3.9 | 65 | 16 |
| Commons thread | 5.4 / 4.9 / 4.3 | 6.0 / 4.1 / 3.9 | 63 | 16 |
| Commons compose | 5.3 / 4.7 / 4.3 | 5.9 / 4.0 / 3.9 | 61 | 15 |
| Workbench bead view | 5.1 / 5.1 / 4.2 | 6.1 / 4.0 / 3.8 | 7 | 6.0 |
| Workbench plan cockpit | 5.1 / 4.7 / 4.2 | 5.2 / 4.0 / 3.8 | 7 | 6.0 |
| Workbench inbox | 5.0 / 4.4 / 4.1 | 5.1 / 3.9 / 3.7 | 7 | 6.0 |
| Workbench decisions | 5.4 / 4.7 / 4.1 | 5.1 / 3.8 / 3.7 | 6 | 5.1 |
| Glass (watch) | 6.3 / 5.4 / 5.0 | 6.0 / 5.3 / 4.5 | 5 | 4.3 |
| Glass (Wall) | 5.5 / 4.5 / 4.1 | 5.2 / 3.8 / 3.7 | 5 | 4.3 |

## What it says

1. **Every surface ships the same 1.49 MB document.** All the JS and CSS is inlined: about 50 scripts plus garden.css,
   unminified and heavily commented. The biggest pieces are garden.css 143 KB, garden.js 127 KB, workbench.js 103 KB,
   art.js 97 KB, world.js 94 KB and commons.js 81 KB. It gzips to 456 KB (3.3x), so the phone cold-load budget of
   ≤ 300 KB compressed is missed by about 1.5x before any data loads. A surface adds only 3–45 KB of its own. The Glass
   adds xterm.bundle.js, which is 285 KB **served uncompressed** (66 KB gzipped). Splitting the scripts (P1.2), lazy
   modules (P1.3) and minification (P1.5) are the levers. Gzip is already on for the page (wire ≈ gz).
2. **On a phone the cost is CPU, not network.** A warm load fetches 1–46 KB yet is no faster than a cold one: LCP 2.3–3.2
   s warm against 2.5–3.6 s cold. Every load runs 10–16 long tasks totalling 1.5–2.5 s before TTI, the longest 250–500
   ms. That is parsing and running the whole inline bundle, then building the surface. Pane LCP (Workbench, Commons,
   Glass) is 2.5–3.6 s against a 2.5 s budget. On the World, FCP and LCP are its `h1` at 0.4 s, but the map is ready
   only at 2.6 s.
3. **INP:** the World's first cursor move costs 184–296 ms on the phone, at or over the 200 ms budget. The plan
   cockpit's first step (104–112 ms) and the queue tab switch (96–176 ms) come next. Commons typing and taps are 40–88
   ms. Every desktop interaction is 24–56 ms.
4. **Desktop is within the proposed budget:** LCP 0.2–1.0 s against ≤ 1.5 s, TTI ≤ 1.1 s.
5. **Idle:** heap stays at 3–5 MB with no growth over 5 minutes. Commons polls about 13 times a minute, about 3.2 KB a
   minute (typing, presence, unread). The other surfaces send about 1 KB a minute. Both are within the proposed 5 KB a
   minute, but the fixture farm sends no live events, so this is a floor.

## P1.2: scripts and stylesheets as hashed assets (gd-jrIxewKgjF)

`/garden` is now a small shell. It loads seven assets from `/garden/assets/<name>.<sha256:12>.<ext>` with
`Cache-Control: public, max-age=31536000, immutable` (`internal/gardenhub/assets.go`): page.css (garden.css and
tunes.css), sky.js in `<head>`, and five script bundles. The bundles are page-scene, page-farm, page-plans,
page-workbench and page-commons. Each `{{define}}` block in garden.html is one bundle: its files are joined in the
order the block lists them. The page keeps its per-snapshot ETag and `private, no-cache`.

| Fixture farm, gzipped | Before | After |
|---|--:|--:|
| Repeat visit after a data change (new snapshot version, so the page is a 200) | 1 request, 467,685 B | 1 request, 4,618 B |
| First visit | 1 request, 467,685 B | 8 requests, 478,383 B |
| Repeat visit, snapshot unchanged | a 304 | a 304 |

`TestPageShellIsSmall` logs the first two rows and fails if the shell grows past 32 KB gzipped.

**Why five bundles.** The lands-perf gate (P3.1) shaped this. The first cut served each of the 52 scripts at its own
URL, and the gate failed it on B3 and B1. A cold phone load took 57–65 requests, up from 4–12, against a budget
of 20. Each file was gzipped alone and lost the shared dictionary, so every surface got about 60 KB heavier. The
second cut joined all the scripts into one page.js. That passed B1 (+1.6 KB) and B3 (+4), but it failed B5. A
classic `<script>` runs as one task, so 1.35 MB of script became one long task: the longest cold task on the phone
went from 125–300 ms to 620–1,470 ms, on a node that was 2.2x slower. Five bundles, cut where the page's parts
are, keep each part its own task. They add 7 requests to a cold load, so the thread surface comes to 19 against
B3's 20.

One cost of bundling: a script that throws while loading stops the scripts after it in its bundle, which its own
`<script>` tag did not. P1.3 (lazy panels) moves a panel's bundle behind its first open. It cuts along these blocks
and does not go back to one URL per file.

- **Times:** the stylesheet is a request now, so a cold FCP waits one more round trip. An earlier A/B moved phone
  World FCP from 660 to 824 ms, about one 170 ms RTT. The phone CPU columns move up to 2x between runs of the same
  build (see Caveats). Compare runs by the longest task and by B5, at a similar CPU bench.
- **Minify (gd-s75uSI2rpW)** is done in `buildAsset`, so the hash covers the minified bytes. See the section on
  minification below.

## Caveats

- **Phone CPU numbers move up to 2x with node load.** Two more runs the same day gave pane LCP 2.7–6.8 s (a full run
  while the node was busy) and 3.0–6.1 s (a phone-only run, CPU bench 69 → 53 ms within the run). Bytes, request
  counts, TTFB and desktop numbers stay put. Compare runs only at a similar CPU bench, which `summary.md` records. A CI gate (P3.1) should gate on bytes, request counts and long-task totals with
  headroom, or run on a dedicated runner, not on raw phone LCP from a shared pod.
- **The fixture farm is small:** a 37 KB snapshot, 5 KB gzipped. Production's farm and Commons history are larger, so
  production adds data weight and render time (P1.4) that this table does not show.
- **Production was not measured; every number here is from the fixture farm.** The owner chose to skip the production
  column (P0.1b gd-dQHzmKGlBR, decision gd-LsRagw9GjL): agents have no read-only signed-in session. A pod can now reach
  the edge (Service `public-edge.public-edge.svc:443`), so a later run needs only a session cookie (above).

## P1.4 snapshot diet: the farm document and its stream (gd-DuHnvvBYMr)

Measured on the Go 10x farm (`bigWorld(360, …)` with harvests: the row cap keeps 83 plans, 2,075 listed steps and 480
closed steps; `TestFarmTenXPatchBytes` logs the byte figures) and the JS 10x fixture (`testdata/world_tenx.js`, 4.08 MB
raw / 570 KB gzipped). Parse times are the best of 15 runs of `JSON.parse` under node on an agent pod, unthrottled; a
phone at 4x CPU is about four times slower.

| What the page is sent | Before | After | Parse before / after |
|---|--:|--:|--:|
| Back on the stream after ten plans moved, past the 256-patch ring | reset: the whole document, 722 KB | one delta patch, 86 KB | 13.1 ms / 0.59 ms |
| One step moves (a live patch) | its plan whole, 8.5 KB | the plan with its other phases as stubs, 2.4 KB | 0.069 / 0.020 ms |
| First load (`GET /garden/farm`, gzipped) | 722 KB raw | unchanged | 13.1 ms |

SSE is never compressed (P1.1), so the stream's bytes are raw bytes on the wire. A full reset now goes only to a page
with no version, one below the change log's floor (about 4096 versions back), or one from before a restart.

**The trim, measured and not done.** Every field the document carries is read by some client file, except a few that
add almost nothing (`built_at`, `meta.gardeners_as_of`/`gardeners_truncated`, `wishes.open[].kin_closed`). The server
already lists closed things only up to what the World draws: a phase's closed steps up to the cells its bed has left,
24 closed plans, 24 closed wishes and 40 open wishes per project, 40 loose beads. What is left is eliding derivable
values and filling them in on the page: a step's project when it is its plan's, `type: "task"`, zero counts, a closed
step's `state`. On the Go 10x document that saves 16% raw (722 → 605 KB) and 1% gzipped (21.3 → 21.5 KB, no better),
and on the JS fixture 17% raw and 3% gzipped. Gzip already squeezes repeated keys, so the first load would barely
change, while it would change the wire contract and every Go test and golden that decodes the document. Plan bodies
are 98% of the document (task rows 60%, closed steps 20%), so a real cut in first-load bytes means sending less of each
plan up front, for example its phases on demand. That is a P1.3-style lazy load, not a trim.

## P1.5 static asset hygiene, 2026-10-29

Step P1.5 (gd-YU9gGOMvur) checked each item in its brief against the page as served:

- **Fonts:** none are downloaded. `--font-body` and `--font-display` are system stacks (garden.css `:root`), so nothing
  blocks first paint and there is nothing to preload. The Glass waits for its monospace face with `document.fonts.load`,
  which resolves at once for an installed font.
- **Sprites:** none are image files. art.js draws the sprites from its pixel data into canvases at art size
  (`ART.spriteCache`), and `/garden/arms.png` is already rendered at slot x devicePixelRatio device pixels (heraldry.js).
- **Render-blocking CSS:** none beyond garden.css. The only other style is the Tunes stylesheet, inlined beside it.
  The Glass's xterm.css is added when the Glass opens.
- **Preconnect:** there is nothing to preconnect to. Every request is same-origin (`TestPageHasNoExternalRequests`).

What changed:

| Change | Before | After |
|---|---|---|
| A Glass deep link (`?glass=<agent>`) preloads xterm.css and xterm.bundle.js from the head | xterm.bundle.js fetched after the panel JSON: it started at 11.3 s and finished 0.8–2.5 s (median 1.5 s) after the JSON | starts at 0.5 s and is finished long before the JSON arrives, so the fetch is off the critical path |
| beads-viewer `/assets/static/` sends a content-hash ETag and `Cache-Control: no-cache` | no validator at all (embed FS, no modtime), so every visit downloaded every static file in full again, including xterm.bundle.js (285 KB raw) | an unchanged file is a bodyless 304 |
| The garden's icons are paletted PNGs at best compression, with an ETag | icon-192 (the tab icon) 950 B, icon-512 2346 B, no validator | 396 B and 846 B; a revisit is a 304 |

The preload numbers come from an A/B probe: phone profile, cold contexts, 5 interleaved runs per build on the fixture
farm. The node was loaded during the probe, so phone CPU ran about 4x slower than the baseline above, and Glass ready
(xterm rows on screen) was 14.1 s before and 13.5 s after (medians), a difference inside the noise. On the baseline's
own profile, the fetch it takes off the critical path is one round trip plus xterm's transfer, about 0.2–0.5 s.
Minification is not part of this change. It belongs with P1.2's asset pipeline.

## Minify: the served bytes (gd-s75uSI2rpW)

`buildAsset` (`internal/gardenhub/assets.go`) minifies each asset with `github.com/tdewolff/minify/v2` (v2.24.19, the
newest release that builds on Go 1.25) before it hashes the asset. The minifier strips comments and whitespace and
shortens a function's local names. Top-level names (`var ART`, `var BEAD`) are global, so it keeps them. The files in
`web/` stay as written, because node tests run them and Go tests read them. `pageAsset.src` keeps the text before
minification, and the tests that look for a line of a script read that text. `TestPageAssetsAreMinified` checks
that every served asset is at least 10% smaller than its source. It also checks that each bundle still parses
under node and still declares its top-level names. A source the minifier cannot parse is served as written, and that
test fails. A change that only touches comments keeps the asset's URL.

| The seven assets, fixture farm | As written | Served |
|---|--:|--:|
| Raw | 1,554,977 B | 794,648 B |
| Gzipped | 485,168 B | 254,526 B |
| First visit, all 8 requests gzipped (`TestPageShellIsSmall`) | 478,383 B | 259,220 B |

The heaviest bundles, gzipped: page-workbench 126 → 70 KB, page-scene 135 → 65 KB, page-farm 109 → 54 KB,
page.css 40 → 23 KB.

`make lands-perf-baseline SURFACES=world,channel PROFILES=phone RUNS=1 IDLE=0 SETTLE=0` on one agent pod, with minify
off and then on. The "on" World row is the median of 3 runs at CPU bench 171–173 ms. The other three rows are single runs:

| Phone, cold | Wire KB off → on | Long tasks to TTI (n / total / max ms) off → on | Ready ms off → on |
|---|--:|---|--:|
| World | 488 → 263 | 12 / 6400 / 1487 → 13 / 5540 / 1463 | 7935 → 7053 |
| Commons channel | 504 → 279 | 15 / 8164 / 2460 → 18 / 7341 / 1468 | 11219 → 10338 |

Both surfaces now load under the 300 KB phone budget (B1). The time columns are inside the phone CPU noise
described under Caveats. One busy single run put World cold FCP at 3.2 s, and three runs at a normal bench gave
1.3 s. A browser also has less script to parse, but this table cannot show that cleanly. The lands-perf CI gate now
also runs when `assets.go` changes.

## P1.3 lazy surface modules: the World first (gd-p02ZelpMxk)

The page loads two bundles by tag now: page-scene.js (art.js, world.js and what is drawn on the map) and page-farm.js
(the cursor, the Porch, the Mailbox, garden.js, plans.js and tunes.js). The other two, page-workbench.js (the Porch
chat, the bead view's helpers, the Workbench and its panels, the Glass) and page-commons.js, are loaded by
`web/lazy.js`, in that order. They load a second after the World has painted from its first farm document, once the
browser is idle. They load at once for a deep link, a panel link, a bead or a gardener's thread opened, or the Commons
tab. For a deep link (`/garden?<panel>=`, `/garden/c/...`), an inline `<head>` script starts the download before the
page's own scripts run. Until the bundles run, `BEAD` and `CHAT` are stubs that load them and then do what was asked. A
click that needs them (a panel link, a Porch row, Settings, the Commons rail) is held and done once they have run, and
Cmd-K opens the '/' jump.

What stays on the first paint, and why:

- **plans.js** is the farm's feed, not only the plans drawer. It fetches `/garden/farm`, holds the stream and hands
  every `farm*` event to the map. It now opens page-farm.js, so the fetch starts as soon as that script runs, not after
  it. `PLANS.doc()` gives the current document to a file loaded after it was handed over.
- **weeds.js and beacons.js** paint the map's first frame (thistles and fog, the edge arrows), and `BEACONS.inset`
  frames the opening camera. Together they are about 9 KB gzipped.
- **The answer box** (`ANSWER.box`, moved from workbench.js) is the one a pot's card draws on the map (web/stip.js).
- **tunes.js** mounts the header's music button. Loaded late, it would shift the header row.

Fixture farm, phone, cold, medians of 3 runs, both builds measured back to back on one agent pod:

| World, phone, cold | Before (main) | After |
|---|--:|--:|
| Script before first paint, wire | 436 KB (sky.js + 5 bundles) | 260 KB (sky.js + 2 bundles) |
| Document, stylesheet and script before DOMContentLoaded, wire | 482 KB | 305 KB |
| Script parsed before first paint, raw | 1,364 KB | 768 KB |
| DOMContentLoaded | 6.1 s | 3.4 s |
| Desktop DOMContentLoaded | 1.34 s | 0.75 s |

Page weight to TTI (B1) and long tasks to TTI (B5) do not move much: the lazy bundles still load inside the TTI window,
after first paint instead of before it. The pod's CPU bench was 145–365 ms in these runs, against the 61 ms reference,
so phone LCP, Ready and long-task numbers moved ±30 % between runs of the same build. Compare those on the CI gate's
paired runs, not on this table.
