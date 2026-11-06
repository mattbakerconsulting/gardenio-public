# Commons v2 baseline: today's band against B12–B17 (2026-10-31)

Step 1 ([[gd-J3LZYVcuBK]]) of plan [[gd-lRaCxsiLvC]]. The budgets are in
`gardenio/docs/commons-ux-rubric.md` §7. This is the "before": today's
Commons band, measured before any of the workbench is built. Every later step
is graded against these budgets.

**Run:** `make commons-ux-shoot SURFACES=commons,commonsthread` on an agent pod,
at `main` 32330b145 plus this step's harness. The fixture farm, Chromium from
`/ms-playwright`, medians of 3 cold loads per profile. The latency pass ran on
`perfWorld`, a farm with its Commons stream live. The pod was busy: the CPU
bench read 120–288 ms against the 61 ms reference, so the phone's times are
inflated, up to 2–4x on the CPU-bound rows. Bytes and the round-trip-bound
rows (echo, open, stream) are red at any bench. A single earlier run, at a
bench of 111–118 ms, read the phone at: key 41 ms, echo 358 ms, opens
366–578 ms, stream 296 ms.

## Phone (graded): every Commons v2 budget fails but B12's rows

| Budget | Limit | `commons` (#general) | `commonsthread` (gd-wishPorch) |
|---|---|--:|--:|
| B12 key to paint (median key / max) | ≤ 16 ms | **47** / 171 | **42** / 94 |
| B12 rows repainted by a key | 0 | 0 | 0 |
| B13 send to echo | ≤ 50 ms | **557** | **515** |
| B14 open thread, cold / warm | ≤ 300 / ≤ 100 ms | **438** / **611** | – |
| B14 open plan, cold / warm | ≤ 300 / ≤ 100 ms | **904** / **495** | – |
| B14 open epic, cold / warm | ≤ 300 / ≤ 100 ms | **451** / **343** | – |
| B15 stream event to paint | ≤ 100 ms | **254** | **326** |
| B16 Commons cold weight | ≤ 150 KB | **294 KB** | **323 KB** |
| B17 Commons LCP | ≤ 1.5 s | **6.7 s** | **8.8 s** |

Bold is over budget. B14 is measured where a node is opened from, the
channel. The thread surface already shows its thread open.

## Desktop (for reading, not graded)

| | `commons` | `commonsthread` |
|---|--:|--:|
| Key to paint (median / max) | 15 / 42 | 14 / 20 |
| Send to echo | 45 | 45 |
| Open thread, cold / warm | 79 / 60 | – |
| Open plan, cold / warm | 94 / 74 | – |
| Open epic, cold / warm | 66 / 55 | – |
| Stream to paint | 24 | 48 |

## What it says

- **Every interaction waits on the network.** On the desktop, with no
  latency, the band is close to its budgets. On the phone's 170 ms RTT, each
  send, open and streamed message pays at least one round trip. The send waits
  for the server's fragment before it shows (`commons.js` `send()`). Every open
  refetches with `cache: 'no-store'` (`workbench.js` `load()`), so a warm open
  costs as much as a cold one. The stream names a message and the pane fetches
  it (`GET /garden/c/m/{id}`). The budgets ask for an optimistic echo
  (step 6), a node cache (steps 3–4) and a stream that carries content
  (step 2).
- **Typing costs about 40 ms a key on the phone**, against 16 ms. It does not
  repaint the timeline (B12 rows is 0), so the cost is the composer's own
  work on each key. Part of the 40 ms is the 4x throttle on a busy node: the
  desktop reads 15 ms.
- **B16:** the Commons is 294–323 KB cold on the wire. The lazy split P1.3
  (#1513) is still open, so B16 also waits on it.
