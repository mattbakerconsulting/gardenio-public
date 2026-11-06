# ADR — The Commons band: replaced, not repaired

> **Status: ACCEPTED** (2026-11-04). Plan [[gd-lRaCxsiLvC]] (Commons v2), step 8,
> task [[gd-ZeiSZV7j2Y]]. Asked in Slack thread gd-7qi5Il9Sug.

## Decision

The Commons band inside `/garden` (channel list, timeline and thread drawer
along the bottom of the World, a sheet on a phone; `web/commons.js`) is
deleted, not fixed. The workbench replaces it: any bead as the root of a tree,
the selected node's transcript as a document, and the Jobs, Plan and Open
rails, on a page of its own.

## Why

- **It could not hold the tree and the rails.** The band was a strip of a
  third of the viewport under a running World. A tree of plans, phases and
  steps beside a transcript and three rails does not fit in it, on a desktop
  or a phone.
- **Its transport cost two round trips per message.** Every send, open and
  streamed message waited on the server before it could paint. The step-1
  baseline (`gardenio/docs/perf/commons-v2-baseline-2026-10-31.md`, phone
  profile): send-to-echo about 0.5 s against a 50 ms budget, LCP 6.7-8.8 s
  against 1.5 s, and about 300 KB cold against 150 KB.

Repairing it meant rebuilding its fragments, stream and pane anyway, so the
plan rebuilt them as the workbench and deletes the old ones here.

## What replaced it

- The workbench at `/garden/c/n/{id}` (`web/commons2.js`, `web/node.html`).
- The land's steward at `/garden/c/s` and `/garden/c/s/{land}`.

## Consequences

- **Every `/garden/c` address still works.** A browser navigation (HTML,
  no `X-Commons` header) gets a 302:

  | Address | Goes to |
  |---|---|
  | `/garden/c` | `/garden/c/s` |
  | `/garden/c/{land}/{channel}` | `/garden/c/s/{land}` |
  | `/garden/c/{land}/{channel}/t/{thread}` | `/garden/c/n/{thread}` |
  | `/garden/c/m/{id}` | the node of the message's thread |

  `/garden/c/m/{id}` redirects on any request: its per-id fragment is gone,
  since the event stream carries a message. The channel list fragment is gone
  too (`/garden/c` always redirects), and so is the channel timeline
  (`/garden/c/{land}/{channel}` always redirects, [[gd-1keb7z0nBn]]): its
  `?before=` pages, the "In hand" fold, the day separators and the "New"
  divider went with it. Join, leave, freeze and thaw answer 204 No Content.
- **Fragments that stay.** The thread fragment stays: the workbench's
  transcript reads it.
- **The gatehouse.** A gatehouse guest's `/` goes to the gatehouse's steward,
  which greets in its `#general`.
- **Ways in.** The Commons tab (phone) and a Commons link in the HUD (desktop)
  go to the steward. Porch rows, gardener sprites, Adopt and Tend's "Follow it
  in the Commons", the welcome tour's last stop and the bead view's thread rows
  go to the node.
- **Snap and Postcard.** They post the PNG straight into the land's `#general`
  or the plan's thread, then open the node. The viewer adds words as a reply,
  where before they typed them in the band's composer before sending.
- **Gone with the band's composer:** the Porch chat's context chip and its
  quick replies.
- **Wake and Restart** had their only `/garden` buttons in the band's thread
  header. Follow-up [[gd-7DyHdnx8fC]] puts them in the workbench's agent head;
  until then `gb wake` does it.
- **Goldens.** The band's golden rows (`commonsbar`, `commonsbeadover` and the
  other band-only rows) are removed from the harness; their PNGs go in a
  bless PR, never in a feature PR (`scripts/check-no-pr-goldens.py`).
- **Docs.** Spec §8 (`superpowers/specs/2026-10-19-lands-commons-design.md`)
  and the rubric's §7 now name only the workbench surfaces. The phone design
  (`superpowers/specs/2026-10-20-lands-commons-phone-design.md`) is kept as a
  record and marked superseded.
