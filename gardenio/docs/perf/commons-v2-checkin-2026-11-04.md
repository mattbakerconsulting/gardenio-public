# Commons v2 check-in: step 1's budgets on the finished workbench (2026-11-04)

Step 9 ([[gd-4s2Rw9hgBd]]) of plan [[gd-lRaCxsiLvC]]. "Before" is step 1's
band ([commons-v2-baseline-2026-10-31.md](commons-v2-baseline-2026-10-31.md)).
"After" is the workbench at `main` 698102cb8 plus PR #2223, measured by the CI gate
itself.

**Run:** `lands-perf.yml` dispatched on `agent/perf-gate/gd-4s2Rw9hgBd` (a0168709)
with `surfaces=commonsthread,commonsplan`, run
[41789484306](https://github.com/mattbakerconsulting/gardenio/actions/runs/41789484306).
Medians of 3 cold loads per profile on the lands-perf runner. The latency pass is on,
and taps are timed from their release. The phone loads' CPU bench read 72–86 ms
against the 61 ms reference, so by the rubric's rule the phone times are graded at
1.5x the budget. The gate passed (no budget regressed against the merge-base).

## Phone (graded)

| Budget | Limit | Before: band `commons` / `commonsthread` | After: `commonsthread` / `commonsplan` |
|---|---|--:|--:|
| B12 key to paint (median) | ≤ 16 ms | 47 / 42 | 15 / 14 |
| B12 rows repainted | 0 | 0 / 0 | 0 / 0 |
| B13 send to echo | ≤ 50 ms | 557 / 515 | 59 (fastest 49) / 44 |
| B14 open thread, cold / warm | ≤ 300 / ≤ 100 | 438 / 611 | – / 249 / 56 |
| B14 open plan, cold / warm | ≤ 300 / ≤ 100 | 904 / 495 | 59 / 45 / – |
| B14 open epic, cold / warm | ≤ 300 / ≤ 100 | 451 / 343 | **316** / 69 / **334** / 85 |
| B15 stream event to paint | ≤ 100 ms | 254 / 326 | 31 / 25 |
| B16 cold weight | ≤ 150 KB | 294 / 323 KB | 62 / 63 KB |
| B17 LCP | ≤ 1.5 s | 6.7 / 8.8 s | 0.82 / 0.84 s |
| B6 INP, cold / warm | ≤ 200 ms | 16 / 24 (channel / thread, CI nightly 41746004562) | 48 / 40, 32 / 40 |

Before was measured on a busy agent pod (bench 120–288 ms), and after on the CI
runner (72–86 ms). Bytes and round-trip-bound rows compare directly. The band's LCP on
a faster CI node (bench 26) was 1.4–1.5 s, so its 6.7–8.8 s is mostly the pod's. INP
reads each surface's own first interaction (the band opened a thread from the channel;
the workbench takes a j in its tree), so it shows only that both are inside B6. A
root the surface is already on is not opened, so commonsthread has no thread open and
commonsplan has no plan open.

**One row is not green: the cold epic open.** It reads 292–358 ms (commonsthread) and
314–344 ms (commonsplan) against 300 ms, over in 5 of 6 runs, and the merge-base read
the same (306–376). That is the 170 ms round trip plus the 40-row stress epic's render;
the fragment is 1.3 KB gzipped. The gate reports it as over budget, noisy, because the
bench is more than 20 % off the reference (the line there is 450 ms). It goes forward as
[[gd-50NuE93HTf]]. B13 on commonsthread is noisy too: 59 ms in the median, but its
fastest run (49 ms) is within the budget, and its slow run read a bench of 127 ms.

## Desktop (for reading)

| | `commonsthread` | `commonsplan` |
|---|--:|--:|
| Key to paint | 5 | 4 |
| Send to echo | 13 | 10 |
| Open thread, cold / warm | – | 43 / 17 |
| Open plan, cold / warm | 22 / 18 | – |
| Open epic, cold / warm | 55 / 23 | 42 / 19 |
| Stream to paint | 18 | 12 |
| LCP | 232 | 148 |

## What the gate holds from here

`perf_gate.js` grades B12–B17 on `commonsthread` and `commonsplan` in every
`lands-perf.yml` run, and they are hard: no ratchet, so an over-budget number fails
even where the base read over too. The nightly grades main. A PR that touches
`internal/commons` or `web/commons2.js` proves the budgets with one dispatch on its
branch (`gb ci dispatch lands-perf.yml --ref <branch> -f
surfaces=commonsthread,commonsplan`, about 8 minutes). The workflow never runs on a
pull request (owner rule 2026-10-31).
