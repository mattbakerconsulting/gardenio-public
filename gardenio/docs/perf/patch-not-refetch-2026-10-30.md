# /garden: patch rather than refetch on live events, 2026-10-30

A follow-up to P2.4 (gd-HprlA40a7z, plan gd-MEGB3dEaHu). After P2.4 the page held one stream, but the active bytes were
still refetches: the bead view and cockpit fetched their whole document on every `moved`, and the project panel fetched its
whole project every 20 s on any farm patch. See [`one-eventsource-2026-10-29.md`](one-eventsource-2026-10-29.md).

## What changed

- **An unchanged document is a 304.** `GET /garden/bead/{id}`, `/garden/plan/{id}` and `/garden/project/{name}` now send
  an ETag that hashes the body (`writeTagged`, `internal/gardenhub/http.go`), with `Cache-Control: private, no-cache`.
  The Workbench fetches with `cache: 'no-cache'`, so the browser revalidates every refetch and a document that did not
  change comes back with no body. The body is cut to the viewer, so the ETag names that viewer's bytes. The other panel
  routes keep `no-store`.
- **A move that only added comments says so.** The Hub's fan-out (`moves`, `live.go`) marks a panel whole for any event
  but `beads.comment.added`. A burst made only of comments is sent as `moved {"id", "only": "comments"}`. Any other
  event in the same burst, and the reconnect's `touchAll`, keep the plain `{"id"}`.
- **The bead view fetches only the new comments.** On that move it reads `GET /garden/bead/{id}?after=N` (`bead.go`),
  where N is the `comments_total` it holds. The answer is the comments after N, the new total, and the titles of the
  ids those comments name. The bead is read as well, so a bead the viewer cannot see is still a 404.
  - The panel appends them, keeps the newest it shows, and repaints in place (`part`/`merge` in `web/workbench.js`, laid on with `LIVE.patch` in `web/live.js`).
  - It fetches the whole bead when the totals do not add up, for example after a comment was deleted or more than 40
    came at once. It also fetches the whole bead after a stream gap, or when its comments could not be read.
- **The project panel counts only its own rows.** On each farm document, `web/project.js` compares this project's
  rows with the last document's:
  - its plans, seeds and gardeners, by identity (plans.js keeps every row a patch did not name);
  - its meadow and harvested rows, as text.

  A patch that names none of them does nothing. One that names only gardeners the Agents tab lists patches their state
  and task in place. Anything else refetches, at most every `REFRESH_S`, never over a form, and a change held back by
  that waits for the next document.

## Measured

`make garden-netinv` with idle 60 s and active 120 s, on bead, cockpit and project, on this branch and on main
(9261f0da) back to back. Chromium 145, the same pod, 2026-10-30.

| Surface | Active KB/min, main → this | The panel's own refetches over 120 s, main → this |
|---|---|---|
| bead | 17.54 → **8.29** | `GET /garden/bead/{id}` 12 / 20.0 KB → 12 / **1.3 KB** (each is `?after=`) |
| cockpit | 26.93 → **7.51** | `GET /garden/plan/{id}` 12 / 38.9 KB → 12 / **0.0 KB** (every one a 304) |
| project | 17.27 → **7.20** | `GET /garden/project/gardenio` 6 / 20.1 KB → 6 / **0.0 KB** (every one a 304) |

- **Idle is unchanged:** 0.46, 0.46 and 0.38 KB/min.
- **Blip and outage are unchanged:** the same reconnects, with no reset and 0 duplicates. The resume refetches the
  whole document as before, which is now a 304 when nothing moved.
- **What remains of the active cost** is the shared stream itself, about 6.1 to 6.6 KB/min at the fixture's 12 farm
  patches a minute, and the Commons unread recount, 1.1 KB/min, which is P2.2 (gd-NRi4Fdu79J).
- **The project panel still asks every 20 s on the fixture.** Every gardener there is in gardenio, and each tick moves
  one of them. But the fixture's project reader is static: its Agents tab lists two other agents, so it cannot patch
  them in place. Each of those asks finds the project unchanged and comes back as a 304. On a live farm, patches for
  other projects no longer reach the panel at all. A patch that moves only the gardeners its Agents tab lists is applied
  in place, with no request.

The page's JS grows by about 2.4 KB raw and 0.9 KB gzipped. Main had about 0.45 KB raw headroom left when this was merged with it, so `TestEmbeddedJSBudget` goes from 1460 to 1462 KiB raw and from 507 to 508 KiB gzipped. Main then grew about 0.3 KB past that before this landed. Trimming 0.24 KB of this change's comments and code brought its growth to about 2.2 KB raw, and the raw cap goes to 1463 KiB, leaving about 0.9 KB. The `workbench.js` change stays under quench's 1500-line cap because the patch logic lives in `live.js`.
