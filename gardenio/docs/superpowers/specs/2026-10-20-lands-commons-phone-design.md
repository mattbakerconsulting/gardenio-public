# The Commons on a phone

> **Superseded** (2026-11-04): the band and its phone sheet that this note designed are gone; the Commons is read in
> the workbench, a push stack on a phone. See [`adr-commons-band-replaced.md`](../../adr-commons-band-replaced.md).
> Kept as a record.

**Status:** accepted 2026-10-20 (decision gd-5Si5j9pWHm: "cross reference against the other work related to
LSDJ and begin all you can in parallel, including that"). The cross-reference is §8; the steps are beads
gd-4hal7KPuxv (M1), gd-jPY7UN2twY (M2), gd-9ZaWoQ5VBT (M3), gd-rOxr4Fya0y (M4), gd-uGWKgBsebA (M5), gd-JSRReAJLRL (M6) under the Lands plan. Owner ask (Slack thread gd-69giNL8Jxa): "serious thought given to
commons in the mobile experience; screenshots, affordances to make navigation awesome, real estate".
Task gd-HlNLilZv1p. Amends the phone paragraph of §8 in
[`2026-10-19-lands-commons-design.md`](2026-10-19-lands-commons-design.md) (§4 below is that paragraph); the desktop
paragraph is being amended separately by gd-Cyv7OuD66X (chat band along the bottom).

The rubric says it plainly: the phone is the first-class client, there is no app
(`gardenio/docs/commons-ux-rubric.md`, C3). This note takes that seriously for the one surface a
person on a phone will spend most of their time in.

## 0. Summary

Today the Commons on a phone is a correct, complete chat sheet that is reached from the twelfth
control on the map, spends a fifth of the screen on two headers, gives the composer's text field
barely half the row, lists channels as six bare names, and dead-ends when a card leads to a bead.
Every one of those is a layout decision, not a model problem; the fragments, routes, stream, presence,
typing and attachments all carry over unchanged.

The proposal, in one breath: **the Commons becomes a tab on the phone's tab bar, every screen of its
stack has exactly one header, the composer is a field with icons, the channel list is an inbox, and
Back always returns to where you came from.** Six steps (§5), each a PR with phone goldens re-shot
under the Looking glass gate, desktop goldens byte-identical.

## 1. Today, at 390 × 844

The pictures are the goldens on `main`
(`gardenio/controller/internal/gardenhub/testdata/golden/*-phone-*.png`), so they are exactly what
ships. Measurements are read off the light captures.

### 1.1 The map (`opening-phone-light`)

- Thirteen tappable HUD controls on one 390 px screen: a two-row header (title, Mine / Everyone /
  Farm / Ledger, Plans, Legend), a six-button camera strip (+, −, Yard, All, Go, Outline), the
  minimap, Commons, Projects, and the three-tab bar (Farm, Porch, Mailbox).
- **The Commons is a small boxed button at bottom-left, the same weight as Projects.** It is the room
  where the fleet talks to the person, and it is the twelfth thing on the screen.
- The Porch tab's own copy says "Pick one to read its thread in the Commons": the Porch is already a
  doorway into the Commons, yet the Commons is not a tab.

### 1.2 The channel list (`commonslist-phone-dark`)

- Six channel names in two land groups, 44 px rows, nothing else: no unread count (#611 adds the
  badge), no last message, no time, no faces. **Eighty percent of the screen is empty.**
- The only control is × at the top-right, the corner furthest from the thumb.
- No DMs section (§2.4), no way to find a channel or a person.

### 1.3 The timeline (`commons-phone-light`)

- **Two stacked headers before the first message**: the sheet bar (← Channels · Commons · ×,
  57 px) and the channel head (#general, topic, presence row, 98 px). 155 px, 18 % of the screen,
  before any content. With an on-screen keyboard up (about 300 px) the messages get roughly 320 px.
- The bar's title says *Commons* while the head says *#general*: the same fact twice, the useful one
  second.
- **The composer gives the text field 214 of 390 px.** Attach and Send are text buttons (150 px with
  gaps). Snap (#667) adds a third; with a word on it the field would be about 130 px wide.
- *Reply* is a 13 px underlined link under every message. It costs a line per message and is under
  44 px tall on a coarse pointer (the rubric's small-target probe flags one on #crew today).
- Scrolled up, nothing tells you that new messages arrived below, and there is no "new since you
  last read" line (#611 brings the cursor, not the divider).
- Every message carries "Oct 14 20:13". On a phone the day belongs on a separator and the message
  on the time alone.
- Good, and kept: inline images at a sane height with a lightbox (D12), the typing line, the
  house-voice placeholder, 44 px Send, `enterkeyhint=send`.

### 1.4 The thread (`commonsthread-phone-light`)

- Good, and kept: the composer follows the visual viewport so it sits above the keyboard; Back
  names the channel; the root message has its own divider; swipe-back from the left edge.
- The bar says *Thread* and the head's `::after` says *Thread* again. The header never says **which**
  thread: you read the first message to find out.

### 1.5 #crew (`commonscrew-phone-light`)

- Agent cards stack well at 390. Kept.
- "Open gardenio-310-death-and-vqf" opens the bead view. On a phone the sheet steps aside for it
  (`commons.js`, the bead-link branch of the click handler) and the bead view's × returns to the
  **map**, not to the thread. Card → bead → back is a dead end; the person re-opens the Commons and
  re-finds the thread.

### 1.6 Mid (820 × 1180)

Under 900 px the sheet is the layout, so a tablet gets a full-screen chat with `max-width: 72ch`
text. Readable, not designed for; out of scope here except that nothing below makes it worse.

## 2. Principles

1. **On a phone the Commons is home.** The world is what you glance at; the Commons is what you
   read. It gets a tab, not a button.
2. **One header per screen.** Back on the left, what you are looking at in the middle, at most one
   action on the right. Nothing is said twice.
3. **Bottom first.** What the thumb uses most sits in the bottom 40 %: the tab bar, the composer,
   the new-messages pill, Back as a swipe. The top-right corner holds nothing you need often.
4. **Real estate goes to reading.** Messages get at least 60 % of the viewport with the keyboard
   down and 40 % with it up, on a 390 × 844 phone.
5. **No dead ends.** Back returns to where you came from at every depth: list → channel → thread →
   bead, and the phone's own back gesture works at each.
6. **One render path.** Every change here is CSS, the pane's JS and small additions to the fragments
   the server already renders (D8). No phone-only fragments.

## 3. The proposal

### 3.1 A Commons tab (M1)

The phone tab bar becomes **Farm · Commons · Porch · Mailbox**. Four tabs at 390 px are 86 px each,
over the 44 px minimum.

- The Commons tab carries the unread total (`[data-slot=unread-total]`, #611) as its pip, in the
  alert colour when a thread of mine has a reply, muted otherwise.
- The sheet sits **above the tab bar** (`bottom: 69px + safe-area`, like the Porch and Mailbox
  screens and the bead view), so the tab bar is never covered: Farm is one tap away from any depth,
  and the sheet's × goes. Tapping the Commons tab while it is open does nothing; Farm returns to the
  same camera (today's behaviour).
- With the keyboard up the sheet keeps following the visual viewport (today's `viewport()`); the tab
  bar is behind the keyboard, which is where it should be.
- The floating Commons button on the map goes. The map loses one of its thirteen controls; Projects
  stays beside the minimap.
- The Commons sheet remembers depth and channel across tab switches (today's `S`), so Farm → Commons
  lands back in the thread you were reading.

Later folds, not in this note: the Porch's agent rows already lead into Commons threads and #665
groups them by plot; the Mailbox's decisions and escalations arrive as Commons cards with #647. When
both have landed, three tabs (Farm · Commons · Mailbox, then two) is the natural end state. Four tabs
now avoids coupling this to #665 and #647.

### 3.2 One header per screen (M2)

| Screen | Left | Middle | Right |
|---|---|---|---|
| List | — | **Commons** | (later: new DM, find) |
| Channel | ‹ (to the list) | **#general**, topic under it in 12 px, two lines max | presence avatars; tap opens the who-list as a sheet |
| Thread | ‹ #general | **Thread** · *author: first line of the root*, one line, ellipsis | — |
| Bead / plan over the Commons | ‹ #crew (or the thread) | the bead's title | — |

On a phone the channel head (`.cm-channel-head`) **moves into the bar** instead of rendering under it:
the fragment is unchanged, the CSS places it. The bar's "Commons" title is dropped on the channel
and thread screens (the list keeps it). The *Thread* `::after` label stays, the duplicate title goes.

Gain: about 75 px per screen, which is one and a half messages with the keyboard down and a quarter
more reading room with it up.

### 3.3 The composer: a field and icons (M2)

- **+** (44 × 44, left): opens a two-item sheet, *Attach a file* and *Snap the farm* (#667), so the
  composer never grows a third text button.
- The field takes the rest, about 290 px against 214 today, grows to five rows, keeps the house
  voice placeholder and `enterkeyhint=send`.
- **Send** (44 × 44, right): the arrow glyph as an SVG mask like the back chevron (the pinned Noto
  subsets have no arrows, see `.cm-back::before`), `aria-label="Send"`. Always visible, disabled
  while the field and the file list are both empty, so a double tap cannot send twice (C8).
- A picked file shows as a chip above the field with its own remove, as today.
- The same markup serves the desktop; only the phone CSS hides the words. `aria-label`s carry them.

### 3.4 Messages on a phone (M5)

- **Day separators** ("Tuesday, Oct 14") and the meta line shows the time only.
- **A message with replies opens its thread on a tap** anywhere on it (the message, not its links),
  the way every phone messenger does; "2 replies · 20:05" stays as the visible affordance.
- A message without replies keeps **Reply**, but inline at the right of the meta line, 44 px hit
  box, so it costs no line of its own.
- **"↓ 3 new" pill** above the composer when the timeline is scrolled up and the stream brings
  messages (C10). Tap: scroll to the first new one.
- **An unread divider** ("new") at the read cursor when the channel opens with unread (after #611).
- Long press on a message: copy link, which is `/garden/c/{land}/{channel}/t/{thread}#m-{id}`
  (the D8 fragments already anchor messages). Nothing else on long press in v1.

### 3.5 The channel list as an inbox (M3)

Each row, 60 px:

```
#crew                                      20:11   ← bold name while unread, time of the last entry
gardenio-forensics: Death storm. 7 more …   [3]    ← last entry: author, first 60 chars; unread badge
```

- The channels fragment (`internal/commons`, `channels.golden.html`) gains per row: the last
  top-level entry's author, first line and time (`data-last-at`, a `.cm-ch-last` span). One extra
  read per channel on the list render, the same beads the unread count (#611) already reads; a
  `message` stream event for the channel refreshes its row, which the pane does today for the list.
- Sections stay as today (one per land, the land's name as the header), plus a **Direct** section
  when DMs land (§2.4). With more than one land the others fold to their header and a count.
- Order within a land stays fixed (general, crew, decisions, escalations, user channels); the badge
  and the bold name do the sorting work, so rows never move under the thumb (rubric C8).
- Desktop keeps its narrow column: the `.cm-ch-last` line is hidden there and in the folded rail.

### 3.6 Back always returns (M4)

- The depth stack (list, channel, thread) **pushes history** (`pushState`, like `bead.js`), so the
  phone's back gesture and the browser's Back walk the stack instead of leaving `/garden`. The URL
  already names each depth (`/garden/c/{land}/{channel}[/t/{thread}]`); the list is `/garden/c`.
- A bead, plan or project link from the Commons opens its panel **over** the sheet (`.bv` above
  `.commons` on a phone, `z-index` only) instead of closing the sheet. Its back bar reads
  ‹ #crew; closing it returns to the thread at the same scroll position. `bead.js` already remembers
  a screen to return to on a phone (its Mailbox/Porch rule); the Commons becomes a third such screen.
- The left-edge swipe (28 px, today) extends to the bead panel when it sits over the Commons.
- A push notification or a Slack link lands at the right depth with the stack beneath it, so Back
  from a deep-linked thread goes to its channel, then the list, then the Farm.

### 3.7 Reach and gestures

- Everything the thumb uses most: the tab bar (bottom), the composer and + / Send (bottom), the new
  pill (bottom), Back (left-edge swipe, or the hardware gesture). The bar's ‹ is the visible fallback.
- The who-list, the + menu and the lightbox are bottom sheets, dismissed by a tap outside, Escape,
  or a downward swipe.
- No pull-to-refresh: the stream keeps the timeline current and a reconnect says so (C10).

### 3.8 The map behind the Commons

- A plot's or gardener's **bead view** gets **Open in the Commons**, which switches to the tab at
  that agent's thread (the desktop's "clicking a plot filters the Commons", §8), and its "show on
  map" does the reverse. Both already exist as URLs. The bead view, not the card: the cursor world
  removes the hover card (its §6.5, step 2b.5) and moves the card's content into the bead view
  (2b.6), so a control placed on the card today would be deleted within weeks.
- Snap (#667) on a phone opens the Commons tab on the timeline with the file picked, as its PR says.

### 3.9 Home screen and push (M6)

- A web app manifest (`display: standalone`, the garden's icon, `start_url: /garden`) and the iOS
  meta tags so the Commons opens from the home screen without browser chrome. The safe-area insets
  are already handled throughout `garden.css`.
- A push (4.7b registers devices) deep-links to `/garden/c/...`, which lands at the right depth
  (3.6). The notification's title is the channel, its body the message's first line, which is what
  the Slack mirror already builds.

## 4. Proposed §8 phone paragraph

Replaced the "Phone (< 900 px)" bullet in the Commons design spec §8 (applied with this note):

> **Phone (< 900 px).** The Commons is a tab on the tab bar (Farm · Commons · Porch · Mailbox) with
> the unread total as its pip; the sheet fills the screen above the tab bar, so Farm is one tap away
> at any depth. Channel list → channel → thread is a push stack with one header per screen (back,
> the channel or the thread's subject, presence), swipe-back from the left edge and the phone's own
> back gesture (history is pushed per depth). The channel list is an inbox: unread badge, last entry
> and its time per row. The composer is a field with + (attach, snap) and Send; it follows the
> visual viewport so it sits above the keyboard. A bead, plan or project opened from the Commons
> sits over it and returns to it. Leaving the sheet returns to the same camera.

## 5. Steps

Each step is one PR on a feature branch, graded with `/commons-ux-acceptance` (C1–C12), with
`*-phone-*` goldens re-shot and `*-desktop-*` goldens byte-identical.

| Step | What | Touches | After |
|---|---|---|---|
| **M1** | Commons tab; sheet above the tab bar; floating button gone; remembers depth across tabs | `garden.html`, `garden.css`, `commons.js`, `garden.js` (tab switch) | #611 (merged); started 2026-10-20 (gd-4hal7KPuxv). 2b.7 waits for it |
| **M2** | One header per screen; the thread's subject in its bar; composer + / field / Send icons | `garden.css`, `commons.js`, thread fragment head (`internal/commons`) | M1 (gd-jPY7UN2twY) |
| **M4** | History per depth; bead / plan panels over the sheet and back to it; swipe-back on them | `commons.js`, `bead.js` | M1 (gd-rOxr4Fya0y) |
| **M3** | Channel list as an inbox: last entry and time per row, Direct section when DMs land | `internal/commons` channels fragment, `garden.css` | nothing; started 2026-10-20 (gd-9ZaWoQ5VBT) |
| **M5** | Day separators, tap-to-thread, Reply inline at 44 px, new-messages pill, unread divider | timeline fragment, `commons.js`, `garden.css` | nothing; started 2026-10-20 (gd-uGWKgBsebA); rebases over M1/M2 |
| **M6** | Manifest and iOS meta; push deep links at depth | `gardenhub` page, 4.7 | manifest half: nothing, started 2026-10-20 (gd-JSRReAJLRL); the deep link's Back is M4's |

Order as first proposed: M1 → M2 → M4 → M3 → M5 → M6, after the desktop band (gd-Cyv7OuD66X)
and the queued Commons PRs. The owner chose parallel instead (decision gd-5Si5j9pWHm): #611, #647
and #667 have merged; M1, M3, M5 and M6 started the same day as separate crews, M2 and M4 follow M1,
and nothing waits for the desktop band (PR #672): whichever of it and M1 merges second rebases, since
they touch the same three files but different rules (desktop docking vs. the phone sheet).

Phone goldens affected: every `*-phone-*` row (the tab bar is on all of them) in M1; `commons*-phone-*`
in M2–M5. Desktop: none.

## 6. What this does not change

The conversation model (§2), the surface interface (§3), the fragments' content and routes (D8),
the Slack mirror, presence and typing, attachments and the lightbox, the desktop layout (owned by
gd-Cyv7OuD66X), the Looking glass gate. Everything here is CSS, the pane's JS, and a few fields on
fragments the server already renders.

## 7. Prototype (M1 + M2, on this note's branch)

Branch `proto/commons-mobile-m1m2-gd-HlNLilZv1p` (commit 080ac683; this PR is the note alone) is a
working prototype of M1 and M2 plus the Reply placement from M5 and the z-order half of M4, in `garden.html`, `garden.css` and
`commons.js` only, shot with `make commons-ux-shoot` at `phone-light` on the same fixture land as
the goldens. The pictures are in the Slack thread (gd-69giNL8Jxa). Read off the shots:

| | Today (`main`) | Prototype |
|---|---|---|
| Pixels above the first message on #general | 155 | 58 |
| Composer text field width | 214 px | 270 px |
| Controls to reach Farm from a thread | × then the map | one tap (the tab) |
| Rubric small-target probe, #crew | 1 hit | 0 |
| The header names the thread | no | yes (author: first line) |

What the prototype got wrong, for the real M2: it hides the words *Channels*, *Attach* and *Send*
with `font-size: 0`, which the rubric's tiny-text probe flags; the real step keeps the words in
`.sr` spans and draws the glyphs on the buttons. The thread subject is built in the pane from the
root message; the real step puts it in the thread fragment's head so the server renders it once
(D8). The prototype does not re-shoot the phone goldens, so its CI is red by design; it is not for
merging as is.

## 8. Cross-reference with the cursor world (LSDJ)

The cursor world ([`2026-10-20-lands-cursor-grid-design.md`](2026-10-20-lands-cursor-grid-design.md),
epic gd-7bfFegkYjS, steps 2b.1–2b.9) rebuilds the Farm tab at the same time as this note rebuilds the
Commons tab. They meet at the tab bar, at Back, at gestures, at the card, and at the goldens. One rule
for each, so neither plan has to reopen the other:

| Where they meet | Cursor spec says | This note says | Rule |
|---|---|---|---|
| **Tab bar and pad** | The pad is one 60 px row between the map and the tab bar, Farm view only (§7, N-D15); the Commons button stays put "until M1 lands" and 2b.7 "places the Commons per #674's outcome". | The Commons is a tab; the sheet fills the screen above the tab bar (M1). | M1 lands first; 2b.7 is blocked by gd-4hal7KPuxv. The sheet covers the pad's row, so the pad never shows under the Commons. 2b.7 removes the Projects button the way M1 removes the Commons button; between them the phone HUD loses two of its floating controls. |
| **Back** | Open question 12.1: a descend does not push history (competes with the bead view's history and with Back meaning "leave the page"). | History is pushed per Commons depth; Back ascends one level; panels open over the sheet and return to it (M4). | 12.1 stays **no**. While the Commons tab is active, its stack is the only history owner; on the Farm tab, Back leaves the page or closes the bead view (`HW §19`). A tab switch pushes nothing. |
| **Gestures** | On the map a swipe is one cursor step (N-D13), a tap moves the cursor, a pinch is the depth gesture (N-D6). | Left-edge swipe is Back on the Commons panels (M4); a downward swipe dismisses a bottom sheet (3.7). | Each binds only on its own surface: the Commons never listens on the map, the cursor never listens inside the sheet. |
| **The card** | The hover card and `cardplace.js` are removed (6.5, 2b.5); the bead view carries the card's content (2b.6). | "Open in the Commons" on a plot card (3.8, first draft). | 3.8 now puts it in the bead view. "Show on the map" from a bead view opened in the Commons switches to the Farm tab, then flies today and is a cursor jump after 2b.3. |
| **Targets** | 32 px inside the grid (a mistap only moves the cursor); 44 px everywhere else (4.1). | 44 px for Reply, the tab bar, the composer (M2, M5). | The Commons keeps 44 px throughout. |
| **Focus** | A data-driven cursor move never pulls focus out of the Commons composer (3.6). | The composer follows the visual viewport above the keyboard (M2). | Stands as written in both. |
| **URL** | `at=` is the cursor's Thing; `z=` is ignored (3.5); the cursor is remembered in `garden.cursor.v1`. | `/garden/c/...` is the Commons route; the tab remembers its depth (M1); a push deep-links at depth (M6). | The active tab owns the URL; switching tabs changes the URL and loses neither the cursor nor the Commons depth. A deep link to `/garden/c/...` opens the Commons tab. |
| **The chore tour** | Stops become cursor jumps; the action sheet is a dialog as an inset (2b.8). | The Commons sheet sits above the tab bar. | The tour runs on the Farm tab; its sheet sits above the tab bar and the pad, never under the Commons. |
| **Goldens** | 2b.1 re-shoots the map goldens, 2b.5 again, 2b.7 the phone goldens. | M1 re-shoots every `*-phone-*` golden; M2–M5 the `commons*-phone-*` rows. | Each PR re-shoots only what it changes, on merged main, in its own PR. Binary conflicts resolve by re-shooting, never by picking a side. |

**What started in parallel on 2026-10-20** (decision gd-5Si5j9pWHm): M1 (gd-4hal7KPuxv), M3 (gd-9ZaWoQ5VBT), M5 (gd-uGWKgBsebA),
M6 (gd-JSRReAJLRL) from this note, and 2b.1 (gd-zjATSCW3s1) and 2b.2 (gd-WConFPNPie) from the cursor spec.
2b.1's "wait for #629 and #655" was a conflict guard, not a dependency: neither PR touches `world.js`
or `art.js`, only the golden PNGs that 2b.1 re-shoots anyway. 2b.2 is a new pure module written against
the spec's fixed geometry, with the `world.js` helpers taken from 2b.1's branch and merged after it.
Waiting: M2 and M4 on M1; 2b.3 on 2b.2; 2b.7 on 2b.4 and M1.
