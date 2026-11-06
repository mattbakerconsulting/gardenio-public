# Commons v2 — plan narrative

**Status (2026-11-01 11:10Z): building, steps 1 and 2 are done and step 3 has started.** Step 1, the budgets and the harness, landed at 05:37Z (#1795). Step 2, one stream that carries content, landed at 11:04Z (#1792, 3e9cbe90b): stream-to-paint on a phone is 27 / 21 ms against a 100 ms budget. A crew is now building step 3, the node model that serves any bead and its children in one response. Steps 4-9 follow in order.

## What and why

The Commons band (channels, timeline, thread drawer along the bottom of the World) gives way to a workbench that opens on any bead with children and shows it as the root of a tree: a plan and its steps, a phase and its steps, an epic and its tasks, or a thread and its spawned agents. Beside the tree: the selected node's transcript as a document, and three rails (Jobs, Plan, Open). Mockups A–D (plan, phase, epic, phone) are in Slack thread gd-7qi5Il9Sug. Speed comes first. Step 1's baseline on the phone profile: today's band weighs about 300 KB cold against a 150 KB budget, LCP is 6.7–8.8 s against 1.5 s, and send-to-echo is about 0.5 s against 50 ms, because every send, open and streamed message waits on a server round trip.

## Where it stands

| # | Step | State |
|---|---|---|
| 1 | Latency budgets and the harness (gd-J3LZYVcuBK) | **Done** (#1795, 05:37Z) |
| 2 | One stream that carries content (gd-RvJ6t2Nagr) | **Done** (#1792, 11:04Z) |
| 3 | The node model: any bead, its children (gd-qlk31NBV6o) | **In progress** (crew started 11:10Z) |
| 4 | The workbench on the desktop (gd-sXB0CyaqTA) | waiting on 3 |
| 5 | The transcript as documents (gd-Z05Hye7yP4) | waiting on 4 |
| 6 | Composer and switch latency (gd-Txos7COlj0) | waiting on 5 |
| 7 | The phone: a push stack (gd-DhGFLKK0tj) | waiting on 6 |
| 8 | Retire the band (gd-ZeiSZV7j2Y) | waiting on 7 |
| 9 | The perf gate in CI and the check-in (gd-4s2Rw9hgBd) | waiting on 8 |

## Next

Step 3 builds `GET /garden/c/n/{id}`: one fragment for any plan, phase, epic or thread, with its children to two levels, and a per-agent jobs record fed by the hook relay's tool_use events and sent live on step 2's stream. It is done when each fixture root comes back in one response under 20 KB and a job finishing or a step closing reaches the stream within 100 ms. Step 4, the desktop workbench, starts when step 3 merges.

---

## Design and ordering (engineering)

- The steps are one linear chain of blocks edges, 1 → 9. Every step after 1 must pass the step-1 budgets before it is accepted.
- Budgets (rubric §7, perf_gate.js / perf_baseline.js; graded in the UX grade, `ci:false` until step 9 puts them in lands-perf.yml). Phone baseline from step 1, commons / commonsthread:

| Budget | Target | Band today |
|---|---|---|
| B12 composer key-to-paint | ≤ 16 ms, no row repaint | 47 / 42 ms, 0 rows |
| B13 send-to-echo | ≤ 50 ms | 557 / 515 ms |
| B14 node open, cold | ≤ 300 ms | thread 438, plan 904, epic 451 ms |
| B14 node open, warm | ≤ 100 ms | thread 611, plan 495, epic 343 ms |
| B15 stream event-to-paint | ≤ 100 ms | 254 / 326 ms |
| B16 cold weight | ≤ 150 KB | 294 / 323 KB |
| B17 phone LCP | ≤ 1.5 s | 6.7 / 8.8 s |

  Measured on a busy pod, so CPU rows run high; the bytes and round-trip rows are red at any load. Write-up: gardenio/docs/perf/commons-v2-baseline-2026-10-31.md. Rerun with `make commons-ux-shoot SURFACES=commons,commonsthread`.
- Nothing of the band's interaction model is kept; its fragments, stream and pane are rebuilt and the old ones deleted (step 8).

## Risks and open threads

- **Main drift.** Step 1 collided with a main change once and was reverted, then re-filed. Re-files and rebases should run against current main just before ready.
- **The JS budget is nearly full.** The desk measured main plus #1792 at about 1,542,592 of 1,543,168 raw bytes, so every later step has to pay for any bytes they add.
- **Sits on the Lands perf pass gd-MEGB3dEaHu.** P1.3 lazy loading (#1513) and patch-not-refetch on live panel events (#1603) are still open (checked 11:10Z). #1603 overlaps the transport that step 2 has now landed, so #1603 is the one that rebases.
- Phone CPU numbers move up to 2x with node load (perf pass P0.1); budgets that gate CI should lean on bytes, requests and long-task totals.

## History

Replaces plan gd-YE3LaIRzvd after the owner asked for plans, phases and epics to be first-class roots. Approved on gd-krHmGagU3E (2026-10-31 22:48Z). Step 1 merged 2026-11-01 03:44Z (b05c39059) and was reverted at 04:47Z (#1788, a build break from main drift). It was re-filed as #1795 and merged at 05:37Z. Step 2's #1792 was sent back by the desk once (10:27Z; the details are on gd-RvJ6t2Nagr), fixed and merged at 11:04Z.
