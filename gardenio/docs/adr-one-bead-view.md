# ADR — One bead view

> **Status: ACCEPTED** (2026-11-05). Plan [[gd-gO1fTT7oGC]] (one bead view),
> step 12, task [[gd-r1IReMfQyW]]. Asked in Slack thread gd-Rrf36anbQj.

## Decision

`/garden?bead=<id>` is the one view of a bead in Lands. It draws, once each,
the bead's head and face, its whole conversation, and the work and asks it
gave rise to. The Commons workbench node draws the same document with the same
renderer, and the steward's node points at it for every wish of the land.

## Why

The owner, in the thread: "the workbench and bead viewer in gardenio lands
doesn't bring in related messages and wishes and squawks; this would be most
helpful for wish beads (steward workbench view almost completely useless
actually). Workbench chats should have the full chat and the best ux
affordances of beads viewer and gardenio lands: dense but also high level,
minimizing redundancy."

Before the plan, a wish had three views in Lands. Each held a different half,
and the halves overlapped:

| View | Address | What it had | What it lacked |
|---|---|---|---|
| The bead panel | `/garden?bead=<id>` (`gardenhub/bead.go`, `web/workbench.js`) | Face, fields, edges, kd comments, edits and lifecycle verbs | The thread: it never read the beads bound to it by `thread_id`, so a wish showed its first message and none of the replies, squawks, cards or agents. Its Children, Related, Waits on and Holds up lists named the node's tree again |
| The Commons workbench node | `/garden/c/n/<id>` (`commons/node.go`, `web/commons2.js`) | The thread's transcript and the tree of its work, with the Jobs, Plan and Open rails | Comments, fields and the face. It was reached only by the panel's `Open in workbench` button |
| The steward's node | `/garden/c/s/<land>` (`commons/stewardnode.go`) | The plans the steward tends, and the #general threads that said @steward | The land's wishes: none of them were on it |

So the same fact (a child bead, a reply) was drawn in two places or not at
all, and a reader had to know which view held which half.

## What the plan folded

- **The panel and the node are one view** (Workbench spec WB-19). The node
  draws the selected bead with the panel's renderer (`web/workbench.js`
  `BEAD.into`, from the same `GET /garden/bead/<id>`) and keeps only its tree
  and rails as its own part; `node.go` dropped its own face
  ([[gd-0GnBi7SRfK]]).
- **The node's tree is the panel's Work section.** The panel embeds the node
  fragment's tree and Open rail (`web/beadwork.js`) and hides the rows of its
  own relation lists that the tree draws, so each bead is listed once. The
  `Open in workbench` button went: the panel is the workbench
  ([[gd-efoAiLlzRC]]).
- **One conversation.** The bead document reads its thread's kin once by
  `thread_id` (`internal/kin`, [[gd-58mkDiWkNq]]). The panel draws the
  thread's messages and the bead's kd comments as one timeline, oldest first
  (WB-3, [[gd-tx2wMH48MV]]), each line in its speaker's voice by one rule set
  that the Commons thread uses too (`kin.Speaker`, [[gd-d3irroS7Q1]]), and live
  ([[gd-MpqfrPhLZP]]). A wish leads with the ask and one counts line (WB-1, the
  wish face, [[gd-zn6iUnPd9Q]]).
- **The steward's node holds the land's wishes**, as a Wishes tree whose rows
  open `?bead=<head>`, with unanswered wishes among its asks
  ([[gd-ak4P2qXMss]]).

## Consequences

- **beads-viewer stays whole** (W-D7). Its bead page, the bead chat, the
  thread chip and `/threads` are a fourth rendering, kept as the
  administrative interface and reached from the Menu's Old viewer. The panel
  took the two things it lacked from them, the reverse-mention messages and
  the `Part of wish` chip; what it left is listed on [[gd-UXDp9GUeMB]], and the
  rest was filed as [[gd-w1qUHcXKNe]] (the activity feed) and
  [[gd-GTqYxAem2S]] (an agent's own messages, membership by parent-child).
- **Every `/garden/c/n/<id>` address still works**: it is the same view with
  the tree rail beside it.
- **The bead document costs up to three more daemon calls** on a click (the
  kin list, the head, the reverse mentions), all in parallel with the comments
  and edges; nothing is read per row. The read is in
  `homestead-farm-read-model.md`, The bead view's kin.
- **Docs.** The Workbench spec (`superpowers/specs/2026-10-19-lands-workbench-glass-design.md`)
  was amended: WB-1 gains the wish face, WB-3 is the conversation, WB-19 says
  the two views are one. The Commons rubric's C11 carries the voice rules.
- **Grading.** The rubric grades the bead's document once; the node route is
  graded for its tree and rails. Step 11 ([[gd-1jjjx7CMU3]]) graded the panel,
  the node and the steward's node with no red, and promoted the golden rows
  `beadwish` (the panel as it opens) and `commonswishdoc` (the node's document
  on a phone) beside `commonswish` and `commonssteward`.
