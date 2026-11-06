# Commons v2 step 2: one stream that carries content (2026-11-01)

Step 2 ([[gd-RvJ6t2Nagr]]) of plan [[gd-lRaCxsiLvC]]. The baseline is
`commons-v2-baseline-2026-10-31.md`.

## What changed

- `GET /garden/c/events` carries content in the channel its query names
  (`?land=&channel=`), rendered on the viewer's own bearer by the routes' own
  templates (`internal/commons/stream.go`; D8: the server renders, the client
  inserts):
  - `message`, `card`: `html` is the row; a reply also carries `head`, its
    thread head with the reply count; `v` versions the row on this stream, and
    a plain message that only gained paragraphs since version `on` carries them
    as `append`, which the pane appends to the row's text in place (an agent's
    turn as it is written). `stale` means the row could not be read just now,
    so the pane refetches the channel.
  - `typing`: `line`, who else is typing.
  - `presence`: `html`, the header's presence slot.
  - `node`: with `?root=<id>` (and `&node=<id>` per child the pane shows), the
    beads under that root and what hangs from them (a step, then its agent),
    as `{id, type, status, title, agent_state, mr_state, mr_pipeline}`, read on
    the viewer's bearer.
  - Events outside the channel stay ids only. `since=` / `Last-Event-ID`
    replay carries the same content.
- The pane (`web/commons.js`) puts what the event carries in place and has no
  caller of `GET /garden/c/m/{id}` any more. A presence or typing fetch that
  started before a streamed slot is dropped rather than painted over it.

## Measured

`make commons-ux-shoot SURFACES=commons,commonsthread CELLS=phone-light
PERF_RUNS=3`, phone profile (4x CPU, 170 ms RTT), medians of 3, CPU bench
57–77 ms against the 61 ms reference.

| Budget | Limit | `commons` | `commonsthread` | Baseline |
|---|---|--:|--:|--:|
| B15 stream event to paint | ≤ 100 ms | **27** PASS | **21** PASS | 254 / 326 |

The paint no longer waits on a round trip: the event is the row. The browser
walk (`TestCommonsPaneUnderChromium`) asserts that a streamed message causes
zero requests to `/garden/c/m/{id}`.

The other budgets are other steps' (B13 echo is step 6, B14 the node cache
of steps 3–4, B16/B17 the lazy split #1513 and step 8).

## A channel that could not be looked up

The pane paints the channel on screen only from content, so a stream that
carries ids only for that channel would drop every message there. When the
channel lookup at stream open fails for any reason other than not found (a
slow or down kbeads, past the 3 s lookup timeout), the stream now ends right
after its first ping. The client asks again from that ping's id (live.js
backs off), and the replay brings what was said in between, rows included
(`TestStreamEndsOnFailedChannelLookup`). The client JS is unchanged, so the
bundle budget is unchanged.
