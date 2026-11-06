# /garden: one EventSource per page, 2026-10-29

P2.4 of the Lands UX performance pass (gd-ymB1Pgg0fQ, plan gd-MEGB3dEaHu). The inputs are P0.4's inventory,
[`netinv-2026-10-28.md`](netinv-2026-10-28.md). This also fixes gd-bo9Yp4o82X: the Commons pane went dark for good
after a refused reconnect.

## What changed

- **One connection per page.** `GET /garden/live/events` carries every stream the page wants, each as a channel on it
  (`internal/gardenhub/livemux.go`, `web/live.js`). The streams are the lists, the farm, the Commons pane, the bead view,
  the cockpit, the Glass, the Wall and a gardener's hover card. The connection opens with the streams wanted at that
  moment. A stream opened or closed later is a `POST /garden/live/sub` on the connection's sid, so opening a panel or
  hovering a gardener never reconnects the others.
- **Each stream is still its own route.** beads-viewer serves each channel through scope and machine again, on the
  connection's own request (`Handler.Live`, `cmd/beads-viewer/main.go`). A stream is read, cut and refused exactly as it
  would be alone, and only the seven event-stream routes may be named.
- **Wire format.** A message's first data line is `<key> <event>[ <id>]`; its other lines are the event's own data, so
  nothing is re-encoded. A stream's lifecycle uses reserved names on the same channel:
  - `:open` once its route answered 200;
  - `:err <status>`, with the route's words, when the route refused;
  - `:end` when the stream closed.

  The connection's hello is key `*`.
- **Reconnecting is the page's, never the browser's.** live.js waits a full-jitter backoff before each retry: a random
  time below 1 s, doubling up to 30 s, reset once the connection or stream has stayed up 10 s. It retries:
  - a dropped, refused or silent (45 s) connection;
  - a stream that ended;
  - a stream refused with a 5xx, 408 or 429.

  A stream refused with any other 4xx closes for good, as a native one would, and its surface falls back as before.
  Every stream is asked for again with the last id it saw, which every stream route takes as its Last-Event-ID.
- **Polls retired:**
  - `GET /garden/c/unread` every 60 s is gone. The Commons stream is open from first paint on every visible page, with
    the pane open or folded, and it names no channel, so it puts the viewer nowhere in Presence. A message, card or
    channel event recounts the badges: after 3 s with the pane open, and at most every 20 s with it folded. Only a
    stream refused for good falls back to the minute's poll.
  - The typing line's 7 s re-fetch is gone. A typist that runs out on the server is already an event (`expire` in
    `internal/commons/typing.go`), and a replay refetches the lines.
  - The bead view's and cockpit's 60 s floor poll now runs only for a stream refused for good. A refused reconnect is
    retried, so it no longer drops a stream to the poll.
- **Off switch.** A server without `Handler.Live` renders the page without `data-live`, and every stream is its own
  native EventSource, as before.

## Measured

`make garden-netinv` was run on the same fixture farm (cardWorld, which now wires `Live`), with shortened phases: idle
120 s, active 60 s, then the blip and the 30 s outage, on 4 surfaces. Chromium 145, 2026-10-29.

| Surface | Streams open | Idle requests / KB/min (before → after) | Outage: streams that came back |
|---|---|---|---|
| world | 2 → 1 | 5 / 0.98 → **0 / 0.59** | 2 of 2 (after up to 17 s) → the connection after 13.4 s |
| commons | 3 → 1 | 43 / 1.42 → **0 / 0.59** | 2 of 3, Commons **never** → the connection after 4.2 s |
| bead | 3 → 1 | 5 / 0.96 → **0 / 0.67** | 2 of 3, bead view **never** → the connection after 10.9 s |
| glass | 3 → 1 | 5 / 0.96 → **0 / 0.67** | 2 of 3, Glass **never** → the connection after 15.1 s |

- **Gate (idle ≤ 5 KB/min per surface): met**, and lower than before.
- **Outage.** The connection made 6 or 7 attempts during the 30 s of 502s, spread by the jitter, and came back with no
  reset and 0 duplicates. Before, the Commons, bead, cockpit, Glass and Wall streams never came back.
- **Blip.** The connection reconnected in 0.2 to 0.8 s. Before, every viewer reconnected at exactly 2.0 s.
- **Active cost is mostly unchanged.** It is still refetch-on-event: the bead document on `moved`, and the channel list
  on `message`. That work is P2.2 for the Commons and gd-HprlA40a7z for the bead view and project panel
  ([`patch-not-refetch-2026-10-30.md`](patch-not-refetch-2026-10-30.md)).

The network inventory now counts every stream's bytes under `/garden/live/events`, which is what the wire carries. The
per-route breakdown of stream bytes is gone from its tables.
