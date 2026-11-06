# Commons UX Rubric (C1–C12)

> **Status:** v1 (2026-10-19). Task [[gd-dedrS0LKTA]] (step 0c), plan
> [[gd-vmw5s07A1F]] ("Gardenio: Lands"), narrative bundle [[gd-uj7BxrMQsH]].
> Adapted from the Slack rubric **U1–U12** (`slack-ux-rubric.md`), and from the
> parity inventories and product definition in that bundle ([[gd-jXfrmBe6Yy]],
> [[gd-RPic1giSw5]], [[gd-HU66xXpuS0]], [[gd-8rKFAEbjqT]]).
> **Amended 2026-11-05** ([[gd-r1IReMfQyW]], plan [[gd-gO1fTT7oGC]], one bead
> view): C11 gains the voice rules of that plan's step 8 ([[gd-d3irroS7Q1]]),
> and §1's Workbench row names the bead view's conversation and Work section.

This document defines **how to grade a Lands surface inside `/garden`** (a
Commons channel, thread or card, a Workbench page, a Glass session, or a World
drawer) after it has been rendered in real Chromium and screenshotted. It is
the in-game sibling of the Slack gates **U1–U12** (`slack-ux-rubric.md`), the
beads-viewer `/threads` gates **V1–V12**
(`gardenio/tests/beads-viewer-screenshots/THREADS-GRADE.md`), the Plans
**PACES** rubric (`plans-ux-rubric.md`) and the **CREWS** formula rubric
(`formula-ux-rubric.md`), and it keeps their idiom:

- **The screenshot is the source of truth.** DOM probes only triage; every
  verdict is confirmed by opening the PNG (Read tool).
- Each gate is `PASS`, `FAIL` or `N/A`, never fake-passed. `N/A` needs a reason
  (the thing the gate grades is not on the surface under test).
- **Every `FAIL` links its screenshot** (the V12 rule), and low scores become a
  bug bead.
- The grader **observes, grades and files**. A fix is a normal PR by whoever
  picks up the filed bug; the grader does not edit the surface it grades.

Section 7 adds **performance budgets B1–B17** (page weight, requests, LCP,
INP, long tasks, idle network and heap; and for the Commons, key to paint,
send to echo, node open and stream to paint), graded beside the twelve gates.

Consumers:

- the `commons-ux-acceptance` skill ([[gd-vzsNYKoGNX]], step 1b), which drives
  the harness to shoot a surface and grades it with this rubric;
- the golden-screenshot gate ([[gd-D953GccBTY]], step 1a): **a capture is
  promoted to a golden only after it grades `accept`** (§5), so the gate
  protects a graded look, not whatever happened to render first;
- every Lands phase check-in, which posts the grade beside its screenshots.

### What is different from Slack

In Slack we controlled only the `blocks` payload; fonts, wrapping, collapse and
the dark palette were Slack's. In `/garden` **we own every pixel**: the CSS,
the breakpoints, the theme tokens, the pixel-art sprites, the focus ring, the
live-update behaviour. So:

- Slack's "payload lint" (§2 there) becomes **DOM probes** here (§2): the page
  is ours, so its overflow, contrast and target sizes can be measured, not
  guessed.
- Things U-gates had to excuse as "Slack's choice" are now our defects:
  clipped text, a missing dark palette, a 28 px button on a phone, a list that
  reorders under the reader's thumb.
- A Slack card was static between `chat.update`s. A Lands surface is **live**
  (SSE), so updating in place without jumping, duplicating or lying gets a gate
  of its own (C10).

---

## 1. What is being graded

One **surface under test (SUT)**: a named view at a URL with a fixture state,
for example *"Commons thread, agent card in Working state, 14 replies"* or
*"Glass grid, 30 sessions, 2 at a prompt"*. Grade what a human sees, not what
the code intended: the templates, CSS and `web/*.js` are the *suspect*, the
rendered pixels are the *witness*.

Surfaces in scope (the four from the product definition, [[gd-jXfrmBe6Yy]]):

| Surface | SUTs to grade (minimum set per phase) |
|---|---|
| **World** | opening view, plot card / tip, Porch, Projects sheet, in-garden bead view |
| **Commons** | channel list, channel timeline, thread, compose box, each card face (agent, decision, escalation, tool-permission, wrap-up, death report), decision modal / "Other" free text, DM, mention picker, notification preview |
| **Workbench** | bead detail (each per-type renderer, the wish face included), inline edit, the conversation (a thread's messages and the bead's comments as one timeline; comments alone for a bead in no thread), the Work section (the Commons node's tree, embedded), search / Cmd-K, feed, inbox, decision and escalation queues, plans index, plan cockpit. `/garden?bead=<id>` and `/garden/c/n/<id>` are one view (Workbench spec WB-19): grade the bead's document once, and the node route for its tree and rails |
| **Glass** | session grid with thumbnails, attention-sorted list, one session expanded (xterm), respond-to-prompt panel, session identity panel, credential pool (admin) |

**The terminal is the witness, not the suspect.** Inside a Glass xterm, the
characters are the agent's real PTY: a long line wrapping at 80 columns is the
agent's output, not our defect. The Glass is graded on the chrome around the
terminal, the terminal palette in both themes, the thumbnails, and the controls.

### Capture matrix

Every SUT is captured at these cells; each gate says which cells it needs.
Viewports match the 1a golden harness and the existing `/garden` browser tests
(`layout_test.go` pins 390×844).

| Cell | Setting | Why |
|---|---|---|
| `phone-light` | 390×844, `isMobile`, `hasTouch` (so `pointer: coarse` matches), `colorScheme: light` | The phone is the first-class client: there is no mobile app (narrative, "Decisions taken"). `garden.css` switches to its phone layout at `max-width: 899px` and to 44 px targets at `pointer: coarse`. |
| `phone-dark` | 390×844, as above, `colorScheme: dark` | Phone + dark catches the worst contrast and wrap combinations together. |
| `desktop-light` | 1440×900, `colorScheme: light`, fine pointer | The default operator reading surface. |
| `desktop-dark` | 1440×900, `colorScheme: dark` | `/garden` resolves its sky (`web/sky.js`, `html[data-sky]`) from `prefers-color-scheme`, the clock (night 19:00–07:00 local) and the Menu's Sky item; the harnesses pin `garden.sky` to the emulated scheme, so dark is reached by emulation, never by a click, and a run at night still shoots `light` as day. The cell that is forgotten: test it every time. |
| `docked` | 1440×900, light, the Commons (or Workbench / Glass) pane open **beside** the World | Desktop is a docked pane, not a full page (narrative, "Decisions taken"): the pane is narrow (~ 360–520 px), the case most cards live in, like Slack's `thread-open`. |
| `mid` | 820×1180, light, `hasTouch` | Sits between the 640/900 breakpoints. A layout that only works at exactly 390 and 1440 fails here. |
| `theme` ×2 | The same SUT under the `garden` and `business` card themes (`card_theme` on the project / channel bead) | Only where the surface honours a card theme. See C6 for when this is `N/A`. |
| `live` | A sequence: capture, emit one SSE event from the fixture (state change, new reply, new session), capture again after it settles, then once more after a forced reconnect | Lands surfaces update in place; C10 grades the sequence, not one frame. |

Capture after the SUT has **settled**: no spinner, fonts loaded
(`document.fonts.ready`), SSE connected and the fixture's last event applied,
animations finished (or the run uses `reducedMotion: 'reduce'` for stills; the
`live` cell runs with motion on). The fixture runs on a **frozen clock** so
"3m ago" reads the same every run. A SUT taller than the viewport is captured
as a full-page image **plus** the first viewport, because the first viewport is
what the reader sees.

### Fixture set (grade under stress, not the happy path)

Slack content was whatever the fleet posted. Here we choose the fixture, so
every SUT is graded against at least **F1, F2, F3 and F6**, and against the
others where the surface can show them. A surface that only looks right with
short, ASCII, single-item data has not been graded.

| Id | Fixture | Catches |
|---|---|---|
| F1 | **Long human text**: a 200-char bead title, a 1,200-char message body, a 90-char agent name | C1 truncation, C11 density |
| F2 | **Unbroken tokens**: a 120-char URL, a 40-char SHA, `agent/some-very-long-branch-name/gd-AbCdEfGhIj`, a bare bead id, a 60-char file path | C2 wrapping, C3 horizontal scroll |
| F3 | **Counts at the edges**: 0 items (empty state), 1 item, many (≥ 30 sessions, ≥ 50 replies, ≥ 12 options) | C9 empty state, C11 lists, C4 grid |
| F4 | **Emoji and non-Latin**: ZWJ emoji (`🧑‍🌾`), a flag, VS16 (`⚠️`), CJK and Arabic names, a combining accent | C7, C1 rune-safe cuts |
| F5 | **Every lifecycle state** of the card or session (Starting, Working, Idle, Prompt, Error, Parked, Rate-limited, Exited / Done, Aborted, Reaped) | C9, C5 (one colour per state in both themes) |
| F6 | **Degraded**: daemon error (partial data), SSE disconnected, a referenced bead missing, an image that 404s | C9 honesty, C10 reconnect |
| F7 | **Needs-you**: a decision with 5 options + Other, an escalation, a tool-permission prompt, a mention of the viewer | C8, C9, C11 |
| F8 | **Two themes**: the same channel with `card_theme: garden` and `card_theme: business` | C6 |

---

## 2. DOM probes (the objective half)

Slack had a payload lint; we have the DOM. These probes run in the page after
the SUT settles (Playwright `page.evaluate`) and **triage** a gate: a probe hit
is a candidate FAIL that the screenshot confirms or clears. They are written as
predicates so the 1b skill can lift them; each names the gate it feeds.

| Probe | Predicate (FAIL candidate when true) | Gate |
|---|---|---|
| Page scrolls sideways | `document.documentElement.scrollWidth > document.documentElement.clientWidth` (the layout viewport: Chromium with `isMobile` widens `innerWidth` to the content, gd-t11FduPB9K); or, inside the probe root, a box with computed `overflow-x` `auto` / `scroll` and `scrollWidth > clientWidth + 1` (a pane scrolling sideways inside itself: gd-JPFtrLnNxK), unless it is built to scroll sideways: a form field, `pre` / `code` and anything inside a `pre`, a table's wrapper (`:has(> table)`), the Glass terminal (`.gl-term`, `.xterm`), a picture strip or chip row (`.pic-strip`, `.cm-quick`, `.bin-rows`), a pane's tab and scope rows (`.qv-tabs`, `.qv-scope`, one sideways row on a phone held sideways), a carousel with `scroll-snap-type` on x, a box marked `data-scroll-x`, or a box inside a visually hidden one (the closed outline) | C2, C3 |
| Silent clip | an element with text where `scrollWidth > clientWidth + 1` (or `scrollHeight > clientHeight + 1` for line-clamped boxes) and computed `text-overflow` is not `ellipsis` and no `-webkit-line-clamp` is set | C1 |
| Ellipsis with no way to the rest | an ellipsized element with no `title`, no `aria-label`/`aria-description` carrying the full text, and no ancestor that expands or links to the full thing | C1 |
| Small target | under `pointer: coarse`, a visible `button`, `a`, `[role=button]`, `summary` or `input` whose bounding box is under **44×44** CSS px (the `garden.css` coarse-pointer rule), counting its `::after` hit-slop where one is declared | C3, C8 |
| Zoom field | under `pointer: coarse`, a visible text `input`, `textarea` or `select` whose computed `font-size` is under **16 px**: iOS zooms the page into it on focus (`garden.css` lifts every field to 16 px under a finger; `docs/lands-mobile.md` §2, gd-xWWqgcmzXM) | C3 |
| Tiny text | visible text with computed `font-size` < **11 px**; body text (messages, card bodies, titles) < **13 px** | C3, C12 |
| Low contrast | text whose colour against its resolved background is < **4.5 : 1** (< 3 : 1 for text ≥ 18.66 px bold or ≥ 24 px, and for icons, borders and focus rings that carry meaning: WCAG 1.4.3 / 1.4.11) | C5 |
| Untokened colour | a hex / `rgb()` colour literal in the surface's CSS outside the `:root` token blocks (`garden.css` defines light tokens in `:root` and night ones twice, under `prefers-color-scheme: dark` and under `:root[data-sky=night]`) | C5 |
| Nameless control | a `button`, link or `[role=button]` with an empty accessible name (icon-only buttons need `aria-label`) | C8, C12 |
| Image without text | an `img` without `alt`, or a `canvas` / sprite that conveys state with no `aria-label` or adjacent text | C12 |
| Duplicate heading | more than one `h1` on the page, or a card with more than one heading element at its top level | C11, C12 |
| Bare id | text matching the bead-id pattern (`\b[a-z]{2}-` and a suffix of any length carrying a capital or a digit: `gd-AbCdEfGhIj`, `gd-A1y`) that is not inside a link or chip, outside code blocks | C12 |
| Hover-only | an affordance shown only under `:hover` with no `:focus-visible` / tap equivalent (check the stylesheet for the selector) | C3, C8 |
| Tip off the screen | a shown `[role=tooltip]` whose bounding box leaves the viewport; the page keeps it 16 px in (`web/signfit.js`, gd-O3nwaqbNUG), so any overrun is a candidate FAIL | C1, C3 |
| Reorder under the pointer | in the `live` sequence, the element under the last pointer position changes identity between captures | C10 |

A probe that **cannot run** (cross-origin frame, canvas-only content) is
recorded as "probe n/a" and the gate is graded from the screenshot alone; it
does not pass by default.

---

## 3. The twelve gates

Each gate has an **objective** check (a §2 probe or a scripted DOM read) and a
**vision test**: questions an agent answers from the PNG alone. Any "no" to a
vision question is a FAIL in that cell. A gate's verdict is the *worse* of the
two checks across its required cells (in parentheses).

| C | Name | From U | What changed |
|---|---|---|---|
| C1 | Truncation | U1 | DOM-measurable; no "Show more" from a host app |
| C2 | Wrapping and overflow | U2 | We own the wrap: long tokens must break |
| C3 | Phone (390 px) | U3 | No longer a proxy: 390 is the real client |
| C4 | Desktop and docked | new | The pane beside the World |
| C5 | Light and dark | U4 | Our own token palette, sprites included |
| C6 | Garden and Business | U5 | Only where a card theme applies |
| C7 | Emoji and glyphs | U6 | Font fallback and sprites, not shortcodes |
| C8 | Controls | U7 | Adds compose, keyboard, double-fire |
| C9 | State at a glance | U8 | Adds the surface's own empty/error/offline state |
| C10 | Live in place | new (U8 lying card + U10 thread discipline) | SSE: no jump, no dup, honest reconnect |
| C11 | Hierarchy and density | U9 + U10 | One gate: reading order and proportion |
| C12 | Links, ids and accessibility | U11 + U12 | Keyboard, screen reader, push preview |

### Truncation

#### C1 — Nothing the reader needs is cut off, and every cut is honest
*(phone-light, desktop-light, docked)* · fixtures F1, F4

- **Objective:** the *silent clip* and *ellipsis with no way to the rest*
  probes are clean. Any text the server shortens goes through a rune-safe cap
  (never a byte slice that splits a multi-byte rune, a ZWJ sequence or a link),
  and the cut is marked with `…`. A shortened title keeps the full text in
  `title` / an accessible description, or the element opens the full thing.
- **Vision test:**
  1. Can you tell, without opening anything, what this is, what state it is in
     and what (if anything) is being asked of the reader?
  2. Is every visible cut marked with `…` (no word, emoji or link sliced flat
     against a box edge)?
  3. Is every cut free of a broken entity or half token (`&am…`, `gd-AbC…` as
     the only identifier, half an emoji)?
  4. Where a body was folded, does the surface say so and offer the way in
     ("Show all 1,200 characters", "Open in Workbench")?
  5. At phone, are the headline **and** the primary action both inside the
     first viewport of the SUT?
- **Auto-FAIL:** text clipped by `overflow: hidden` with no ellipsis on a title,
  name or button label; a card that renders blank where the fixture has text.

### Wrapping

#### C2 — Text wraps cleanly; nothing pushes the page sideways
*(phone-light, mid, docked)* · fixtures F2, F4

- **Objective:** the *page scrolls sideways* probe is clean at every required
  width. Long tokens (URLs, SHAs, branch names, file paths, bead ids) are
  either links with a short label or break (`overflow-wrap: anywhere` /
  `break-word` on message and card bodies; `min-width: 0` on flex and grid
  children so they can shrink). Code blocks and tables scroll **inside their
  own box**, never the page. No layout depends on a fixed text width.
- **Vision test:**
  1. Is the page free of a horizontal scroll bar, and of content running past
     the right edge?
  2. Does each long token stay inside its bubble or card?
  3. Is every line free of an orphaned single glyph, bullet or punctuation mark
     left alone by a bad join?
  4. Do lists, indents and quotes keep their meaning after wrapping?
  5. Is the same SUT readable at `phone-light`, `mid` **and** `docked`? A surface
     readable at only one width is a FAIL.

### Mobile

#### C3 — Usable on a 390 px phone
*(phone-light, phone-dark)* · fixtures F1, F3, F7

The phone is a first-class client here, not a proxy: Lands has no native app,
so the mobile web **is** the mobile client. Only the OS layer (web push banner,
share sheet, on-screen keyboard) is out of reach of emulation and is recorded
as untested, not passed.

- **Objective:** *small target*, *zoom field*, *tiny text*, *page scrolls
  sideways* and *hover-only* probes are clean. The pane takes the full screen below the
  header (`--bar`) and above the tab bar, honouring
  `env(safe-area-inset-bottom)`. The compose box and any action row stay
  above the tab bar.
- **Vision test:**
  1. Is every button and link fully legible and clearly tappable, with visible
     space between neighbours (no two targets touching)?
  2. Is each action next to the thing it acts on (not stranded at the bottom of
     a long card)?
  3. Can the reader reach the headline, the state and the primary action
     without scrolling the SUT?
  4. Is nothing covered by the header, the tab bar, the pad, a floating
     button or a toast?
  5. Is all body text comfortably readable, with only deliberately demoted
     metadata smaller?
  6. Is there a visible way back (close, back, or tab) from the pane to the
     World?

### Desktop

#### C4 — The desktop uses its space; the docked pane holds up
*(desktop-light, docked)* · fixtures F1, F3

- **Objective:** at 1440 the Commons / Workbench / Glass pane docks beside the
  World instead of covering it (Glass may take the full screen when a session
  is expanded, with a visible way back). Running text has a reading measure of
  ≤ ~80 characters per line; nothing stretches edge to edge at 1440. The
  docked pane has a minimum width at which its cards still pass C1/C2.
- **Vision test:**
  1. With the pane docked, is the World still visible and recognisably the
     same place?
  2. Do message bodies and card text sit in a comfortable column (no 1,400 px
     lines)?
  3. Do grids (Glass thumbnails, queues) fill the width with readable cells and
     no large empty gutters or a lone stretched cell?
  4. Are header, tabs and pane chrome aligned with each other and with the
     World's chrome, with consistent spacing?
  5. Does the docked pane look designed for its width (not the phone layout
     stretched, and not the desktop layout squeezed)?

### Theme

#### C5 — Legible in light **and** dark
*(desktop-light, desktop-dark, phone-dark)* · fixtures F5, F6

- **Objective:** *low contrast* and *untokened colour* probes are clean. Every
  colour comes from the `garden.css` tokens (`--ink`, `--muted`, `--surface`,
  `--box*`, `--alert`, `--ok`, `--warn`, …), which have a night value under
  `prefers-color-scheme: dark` and under `html[data-sky=night]` (the clock or
  the Menu's Sky item, `web/sky.js`). Sprites and pixel art (`art.js`) either have a
  dark variant or sit on a token background that keeps them distinguishable.
  The Glass xterm theme follows the page theme, or is a deliberate dark
  terminal in both themes with its own contrast check. Colour is never the only
  signal for a state (cf. C9).
- **Vision test** (compare the light and dark captures side by side):
  1. Is every piece of text, every glyph, sprite, border and focus ring
     distinguishable from its background in **both** captures?
  2. Is there no light box stranded on the dark page (a panel that forgot its
     dark token), and no dark-on-dark text or icon?
  3. Does each state colour (Working, Prompt, Error, Done) stay distinct from
     the others in dark, not only in light?
  4. Do the two captures have the same structure, with only colours changing?
- Sample pixels for contrast; do not eyeball. This is the V4
  "`data-skin="dark"` trap" in a new place: a panel added without dark tokens
  silently renders the light palette on a dark page.

#### C6 — Garden and Business differ in vocabulary, not in quality
*(desktop-light, phone-light, ×2 card themes)* · fixture F8

`card_theme` is a project / channel field the bridge honours today
(`theme.go`); the narrative keeps it as a channel routing field. The product
definition lists the themes under DROP ([[gd-jXfrmBe6Yy]] §4). Until the owner
settles it (plan step 0d, [[gd-lyIZyN6wof]]), this gate is graded **only where
the SUT renders a card theme**, and is `N/A — no card theme on this surface`
everywhere else. It never fails a surface for having only one theme.

- **Objective:** every lifecycle glyph and state word is resolved through the
  theme (the same `Glyph` / `Semantic` mapping the bridge uses), never
  hard-coded in a template or `web/*.js`. A Business render contains no plant
  glyph and no Garden-only word (Seedling, Greenhouse, Harvest…).
- **Vision test:**
  1. Do the two renders have the same blocks in the same order, the same
     hierarchy and the same set of controls?
  2. Is the Business render free of garden vocabulary and plant glyphs?
  3. Do wider labels (bracket tags instead of a glyph) still fit their header,
     chip or button without clipping (C1) or wrapping badly (C2)?

The World itself is a farm in both themes; C6 grades card faces and labels,
not the scene.

### Emoji

#### C7 — Glyphs are glued to words, render everywhere, and are few
*(desktop-light, desktop-dark, phone-light)* · fixtures F4, F5

- **Objective:** every state glyph sits **immediately before its word in the
  same text run** (`🍃 Working`), and the word is in the accessible name (a
  bare glyph is a FAIL). Glyphs come from the theme mapping, not literals.
  Reactions / quick verbs are buttons with text labels (the product definition
  replaces Slack's ⏹️ reaction with a "Done" button). At most **6** distinct
  glyphs per card, and no glyph used for two different states.
- **Arms** (an epic's three to five emoji in a row, `gardenio/docs/heraldry/README.md`) count as
  **one glyph glued to the short title**: the run sits immediately before the title in the same
  run, and its plain-words name (`arms: fire, compass, package`) is in the accessible name. In
  `/garden` the run is drawn as the row picture, never as text emoji.
- **Vision test:**
  1. Does every glyph render as a picture (no tofu `▯`, no `:shortcode:` text,
     no ZWJ sequence split into its parts, no flat ⚠ where ⚠️ was meant)?
  2. Do the glyphs read as one family at the size shown, alongside the pixel
     art?
  3. Is every glyph still visible in dark (cf. C5)?
  4. Would the line make sense read aloud with the glyph replaced by its name
     ("seedling Working")?
  5. Is body prose free of decoration, with at most one glyph per line?

### Button affordance

#### C8 — Controls look pressable, say what they do, and are safe
*(desktop-light, desktop-dark, phone-light)* · fixture F7, plus one click per control in the `live` cell

Covers buttons, the compose box, the decision options and "Other", the
respond-to-prompt panel and the Glass key sender.

- **Objective:**
  - At most **one** primary-styled control per card or action row, and it is
    the recommended or safe default.
  - Destructive or irreversible actions (Kill, Deny, Reject, Close, Retire,
    sending Ctrl-C or Esc into a PTY) use the danger style **and** confirm,
    with the confirm naming the action and its target ("Kill agent
    gardenio-crew-4ae?").
  - Labels are verbs ("Approve", "Reopen", "Send"), not "OK" or "Option 2"; a
    decision option button shows the option's own words.
  - Every control has a handler (no dead button), an accessible name, a
    visible `:focus-visible` ring, and works with Enter / Space.
  - A disabled control looks disabled and says why (tooltip *and* inline text
    at phone, where there is no hover).
  - The compose box says where the message goes ("Message #crew", "Reply in
    thread", "Message steward"), and whether sending spawns or nudges an agent.
  - Respond-to-prompt buttons are driven by coop's prompt schema, so option *n*
    on screen is option *n* sent: compare the label to the fixture.
- **Vision test:**
  1. Does every button look like a button (bordered or filled, not plain text
     that reads as a link) in both themes?
  2. Can primary, danger and default be told apart **without colour** (label,
     weight, position, icon)?
  3. Is the destructive control separate from the safe one, and no larger or
     more prominent than it?
  4. Does every label say what will happen?
  5. Is it clear where typed text will go?
- **After a click** (the `live` cell; `N/A` for a static grade, with that
  reason): within ~1 s the control shows it is working (pending state), then the
  card shows the outcome in words ("Approved by you, 05:12") and the spent
  controls are removed or disabled. A second click while pending does **not**
  fire twice. An outcome that is only a toast is a FAIL: the card is where the
  reader looks later.

### State

#### C9 — State is unambiguous at a glance
*(desktop-light, desktop-dark, phone-light)* · fixtures F5, F6, F7

Two kinds of state: the **subject's** (this agent is Working; this decision is
open) and the **surface's own** (loading, empty, error, offline).

- **Objective:**
  - The subject's lifecycle state appears as a **word** (plus its theme glyph
    and colour) in the card's first line or header, derived from the bead's
    current state, never the state when the card was first drawn (the "lying
    card", cf. C10).
  - Terminal states (Done, Aborted, Reaped, Exited) show no live controls.
  - Needs-a-human states (decision open, Prompt, escalation pending, mention of
    the viewer) say so in words and carry the action row.
  - Empty, error and disconnected states render a sentence inside the full page
    chrome (V1: no blank or white panes); partial data on a daemon error shows
    what loaded plus a line saying what did not ("soft degradation", product
    definition §3).
- **Vision test:**
  1. Within a second, can you answer *what is this, what state is it in, does
     it need me?*
  2. Does a finished thing read as finished (muted, no live buttons)?
  3. Does a thing that needs the reader stand out from things that do not?
  4. Is the state readable with colour removed (imagine it in greyscale)?
  5. In the F6 captures, does the screen say what went wrong in plain words,
     instead of showing a blank, a spinner forever or a stack trace?
  6. Where something waits on a person, does it name who ("waiting on you",
     "waiting on the operator"), never just "waiting"?

### Live update

#### C10 — Live in place: updates land where they belong, without jumping
*(live, run at phone-light and desktop-light)* · fixtures F5, F6

New gate. Slack's `chat.update` gave us in-place edits for free; the Commons
builds them on the SSE hub (product definition: "live without reload: SSE with
`?since=` replay"; narrative D1: one live card per bead, re-rendered in place;
D8: id-only events, the client refetches the rendered card).

- **Objective (the `live` sequence):**
  - A state change re-renders the **existing** card; it does not append a
    second card for the same bead (count cards per bead id: exactly 1).
  - A new reply appears exactly once, including after an optimistic send is
    reconciled with the server copy (V10: zero dup).
  - If the reader has scrolled up, new messages do not move what they are
    reading; a "N new" marker appears instead. If they are at the bottom, the
    view follows.
  - The *reorder under the pointer* probe is clean: an attention-sorted list
    (Glass, inbox) does not move the row under the pointer or focus between
    two frames; it defers the reorder or marks it.
  - After a forced reconnect, the surface shows it was offline while it was,
    replays missed events (`?since=`), and ends in the same state as a fresh
    page load (compare against one).
  - Update animations respect `prefers-reduced-motion` (as `garden.css` does for
    `.fresh` and `.tip`), and the screen-reader live region announces arrivals
    politely, not every tick (`a11y_test.go`: live region changes on
    connection state, not the clock).
- **Vision test** (compare frames before, after and after-reconnect):
  1. Did the change land on the card it belongs to, with the rest of the
     screen where it was?
  2. Is the new state visible without the reader having to reload or hunt for
     it?
  3. Is there no duplicate card, bubble or row?
  4. During the disconnect, did the surface say it was offline or stale?
  5. Does the after-reconnect frame match a fresh load of the same fixture?
- **Thread discipline** (carried from U10): a status that changes is one card
  updated in place, not a stream of new messages. More than ~3 agent messages
  in a row with no human message between them in a thread is a FAIL (noise),
  unless it is the live-stream status surface by design.

### Card hierarchy

#### C11 — One reading order, proportionate length
*(desktop-light, phone-light, docked)* · fixtures F1, F3, F7

Merges U9 (hierarchy) and U10 (density).

- **Objective:**
  - Card order is **headline → state → detail → action**, with metadata
    (agent, cost, turns, ids, timestamps) demoted to a quieter line. One
    heading per card; one `h1` per page (`a11y_test.go`).
  - A card's body shows ≤ ~600 characters before folding to "show more" or a
    Workbench link; lists over 8 items are summarised with a count and a link.
  - A normal card fits within one phone viewport (844 px) and about half a
    desktop one.
  - Conversation leads and work is collapsed by default where both appear (V2,
    V3: narrative first, work tally as quiet meta).
- **Vision test:**
  1. Does the eye land first on the outcome or the ask, in plain words (the
     wrap-up `tldr`, the decision question)?
  2. Is metadata visibly quieter than content?
  3. Is the spacing between groups consistent, with dividers only between
     groups?
  4. Is there no wall of text, and no card that pushes everything else off the
     screen?
  5. Can a long thread or queue be scanned: each item distinguishable, its
     subject in the first line?
- **Voices** (amended 2026-11-05; plan [[gd-gO1fTT7oGC]] step 8,
  [[gd-d3irroS7Q1]]). Every line of a conversation (the bead view's
  Conversation, the Commons thread, the steward's transcript) names who speaks,
  in which voice, and how it came. One rule set decides it, `internal/kin`
  `Speaker`, so every surface agrees on who wrote what:
  - A **person's** line is under the person's name in the person voice: a Slack
    reply the bridge forwarded (label `slack-thread-reply`, no `author`) is
    named from its `created_by` when a person wrote it, else the `operated_by`
    the bridge stamped, else `slack:<user id>`; a Commons message by an `oidc:`
    principal is theirs.
  - A **squawk** (label `squawk`) is under the agent's name, from its
    `from:<agent>` label, else `source_agent`, and carries the squawk mark.
    Those fields are what the writer put there, so they name the agent only
    when the daemon's `created_by` is that agent or a relay; otherwise
    `created_by` names the line.
  - A **card** (`kind=card`, the agent card's mirror) is one state chip: the
    agent's name, linked to the agent's panel, and its state word. It is never
    a prose row ("Agent: <name>") and never counts as a reply.
  - A **steward run**'s line (`<land>-steward-<suffix>`) is in the steward
    voice.
  - A bead's own **kd comment** is in the comment voice, in time order among
    the messages.
  - The **via** word says how the line came: in Slack, squawk, in the Commons,
    said (`gb say`, the Glass), card, or mentions this (a message out of the
    thread that names the bead).
- **Auto-FAIL:** a card that is metadata only when the bead has a tldr, close
  reason or question to show; a heading that is just an id; a squawk or a Slack
  reply drawn under a relay's name (`slack-bridge`) instead of its speaker; a
  squawk without its mark; a card drawn as a text message.

### Links, ids and accessibility

#### C12 — Links resolve and read as labels; the surface works without sight or a mouse
*(desktop-light, phone-light; keyboard pass at desktop-light)* · fixtures F2, F6

Merges U11 (links and mentions) and U12 (fallback and accessibility).

- **Objective:**
  - The *bare id*, *nameless control*, *image without text*, *tiny text* and
    *duplicate heading* probes are clean.
  - Bead ids render as titled chips linking to one URL per bead (`/b/{id}`),
    which opens **inside** the game (the in-garden bead view / Workbench), not a
    jump to beads-viewer. Agent names link to their Glass session.
  - @mentions render as a person or agent chip; an unresolved mention says it
    is unresolved instead of showing a raw id or `oidc:` string.
  - URLs show a human label; a missing referenced bead (F6) renders as a
    labelled "not found" chip that still links, not as a broken link.
  - Keyboard: Tab reaches every control in reading order with a visible focus
    ring; Esc closes the pane, modal or sheet and returns focus to what opened
    it; the compose box is reachable in one or two Tab presses from the
    timeline.
  - Landmarks and names: one `<main>`, the skip link, labelled regions; images
    and state sprites have text alternatives; no information exists only in an
    image, a colour or a sprite.
  - Notifications (web push, unread badges, the tab title) carry a one-line
    summary that reads on its own: state, subject, and who.
- **Vision test:**
  1. Does every link look like a link (and only links look like links)?
  2. Is every id shown with its title, not bare, and never twice beside itself?
  3. Is every mention rendered as a name, not a raw id?
  4. In the keyboard pass screenshot, is the focus ring visible on the focused
     control in both themes?
  5. Read the notification preview / tab title capture out of context: does it
     say what happened and to what?
- **Stop, do not grade:** anything from **another land** visible on the SUT
  (a channel, name, card or bead outside the viewer's scope). That is a P0
  security defect, not a UX finding: stop, file it with `security` and
  `project:gardenio` labels, and link the screenshot.

---

## 4. Grading output

For each SUT the grader writes `COMMONS-UX-GRADE.md` next to the screenshots,
in the V-grade shape:

```markdown
# Commons UX grade — <surface / state> — <date>

SUT: <url + fixture id>   code: <template / web/*.js / handler>   card theme: <garden|business|none>
Captured cells: phone-light, phone-dark, desktop-light, desktop-dark, docked, mid, live
Fixtures: F1 F2 F3 F6 (+ F4 F5 F7 F8 where applicable)

| Gate | Verdict | Cell(s) | Evidence |
|---|---|---|---|
| C1 Truncation | PASS | phone-light | shots/…png |
| C5 Light and dark | FAIL | desktop-dark | shots/…png ← linked, per the V12 rule |
| …

Score = 10 / 11 = 91 %   Band: accept-with-nits
Perf: 7/10 budgets PASS · FAIL: B1 B4 B5 · ungraded: B7   (beside the score, not in it)

## Performance budgets (rubric §7)

| Budget | Limit | Load | phone-light | desktop-light |
|---|---|---|---|---|
| B1 cold weight | ≤ 300 KB | cold | 472 KB **FAIL** | — |
| B3 requests | ≤ 20 req | cold | 5 req PASS | — |
| B4 LCP | ≤ 3000 ms | cold | 3392 ms **FAIL** | — |
| B6 INP | ≤ 200 ms | cold | 144 ms PASS | — |
| B7 pan long task | ≤ 100 ms | pan | UNGRADED: not run: no pan pass … | — |
| B8 idle network | ≤ 5.0 KB/min | idle | 0.9 KB/min PASS | — |
| B10 desktop LCP | ≤ 1500 ms | cold | — | 772 ms PASS |
| …
```

Plus: the §2 probe results (each hit, with the selector and the measured
value), one paragraph per FAIL (mechanism → where in the code → the fix that
closes it), and the list of `N/A` gates **with the reason**.

### Perf rows

Every grade also carries one row per §7 budget check, read from the same
measurement the CI gate reads (`perf_baseline.js`, P0.1), so a PR that changes
a surface reports its perf numbers next to its C1–C12 gates:

- **Rows:** B1 compressed cold page weight, B2 warm weight, B3 request count,
  B4 phone LCP (and the World's Ready), B5 long tasks to TTI, B6 phone INP on
  the first interaction (cold and warm), B7 the longest task during a pan (the
  World only; *ungraded* in a grade, which runs no pan pass), B8 idle network per
  minute, B9 idle heap and its growth, B10 desktop LCP, B11 desktop INP. A row
  the surface does not have (B7 off the World) is left out.
- **Cells:** each row has a verdict in the `phone-light` and `desktop-light`
  cells, for the profile its budget names (§7: B1–B8 phone, B10–B11
  desktop, B9 both); `—` where the budget does not apply. The numbers are
  measured on §7's profiles (the phone's 4x CPU and 4G), not in the shot
  cells' own contexts, and filed under the cell of that profile.
- **Verdicts** are `PASS` (the median is within the budget), `FAIL` (over it)
  or `UNGRADED` (no value: the load or its first interaction failed, or the
  budget has no measurement yet), never `N/A`. A grade uses the **budget
  itself**: no ratchet and no 1.5x line, which are the CI gate's. A time
  `FAIL` (B4–B6, B10, B11) is marked *noisy* when the run's CPU bench is not
  within 20 % of the reference, or the fastest run is within the budget;
  re-measure on a quiet node before filing it.
- **Per budget:** FAIL when any of its checks fails, PASS when every one
  passed, else UNGRADED. The grade's perf line is `Perf: <PASS>/<PASS + FAIL>
  budgets PASS · FAIL: <ids> · ungraded: <ids>`.

**Perf verdicts sit beside the C1–C12 score and never change it.** The score,
`of` and percent (and the band they give) keep their meaning: gates passed,
gates graded, and their ratio, over C1–C12 only. A perf FAIL does not move a
grade from accept to revise; it is reported, and a FAIL that is not marked
noisy is filed as a P2 bug deduped by a `commons-ux:<budget>:<surface>` label
(the CI gate already holds a budget that was over on `main` to its ratchet,
so most of today's B1 FAILs are known). The evidence bead carries the perf result in
fields of its own next to `score`, `of` and `percent`: `perf_pass` and
`perf_of` (budgets passed and graded), `perf_fail` and `perf_ungraded`
(comma-joined ids).

### Verdicts and bands

- `PASS`: meets the objective check and every vision question in every
  required cell.
- `FAIL`: any required cell fails either check. An auto-FAIL condition fails
  the gate regardless of the rest.
- `N/A`: the thing the gate grades is absent from the SUT (no controls ⇒ C8
  `N/A`; no card theme ⇒ C6 `N/A`; a static grade ⇒ C10 and C8's after-click
  part `N/A`). Never use `N/A` to skip a cell that is merely inconvenient; a
  cell that could not be captured is **ungraded**, and the grade says so.

**Per-surface score** = `PASS` count ÷ (12 − `N/A` count), as a percentage,
with the gate letters of every FAIL. Bands (same as U1–U12): **100 %** accept ·
**≥ 83 %** accept-with-nits (file the FAILs, do not block) · **< 83 %** revise
(block the PR the surface ships in until the FAILs are fixed and re-shot).

**Severity of the filed bug**, independent of the score:

| Severity | Gates | Why |
|---|---|---|
| P0 | Another land's data visible (C12 stop rule) | Scope breach, not UX. |
| P1 | C1 auto-FAIL, C8 unsafe destructive control / no confirm / double-fire / wrong option sent, C9 lying state, C10 duplicate or lost update | Wrong or unsafe information reaches a human, or a click does the wrong thing. |
| P2 | C2, C3, C4, C5, C6, C11, C12 | A reader is slowed or misled but nothing is wrong. |
| P3 | C7, C10 reduced-motion / live-region noise | Polish and proportion. |

Dedupe by a `commons-ux:<gate>:<surface>` label so a re-run **updates** the
open bug instead of filing a duplicate. File under the Lands phase that owns
the surface (`parent:` that phase's epic).

---

## 5. How the loop uses it

1. **Author** changes a Lands surface (template, `web/*.js`, `garden.css`,
   card renderer, theme) in a PR.
2. The `commons-ux-acceptance` skill ([[gd-vzsNYKoGNX]]) serves the surface
   from fixtures through the 1a harness ([[gd-D953GccBTY]]) and captures the §1
   matrix.
   A shoot of named surfaces also measures each one against the §7 budgets
   and writes `<surface>-<cell>.perf.json` for its `phone-light` and
   `desktop-light` cells, next to the probes JSON.
3. The grader (an agent with vision) runs the §2 probes, reads each PNG,
   answers the §3 vision questions, and writes `COMMONS-UX-GRADE.md`, with
   the §4 perf rows copied from the draft beside the score.
4. **Accept** → the captures become (or update) the surface's goldens in the
   same PR, and the grade, perf rows included, is linked on the PR. The band
   comes from C1–C12 alone; a perf FAIL is reported and filed beside it. **Revise** → file the bugs, push
   a fix, re-shoot only the failing cells (the grade lists which), re-grade.
5. From then on the golden diff in CI guards the graded look; an intentional
   change re-runs this loop before its golden is updated.

A before/after pair for the same SUT is the preferred evidence on a PR that
*changes* an existing surface.

Failure modes the grader must report instead of guessing:

- **Not signed in / wrong principal** (the capture lands on the login page or
  a 403): stop and report; a login page is not a FAIL of the surface.
- **Surface not served** (404, the route is not built yet): report it as
  ungraded with the URL.
- **Fixture did not load** (the SUT shows real fleet data, or none): stop.
  Grades on live data cannot be compared across runs and may leak other lands
  into screenshots.
- **Dark cell unreachable** (emulation not applied: the dark capture is
  identical to the light one): C5 is *ungraded*, not passed.
- **Small text in the PNG**: vision is unreliable below ~12 px at DPR 1. Read
  the DOM text for what *should* be there, and judge from a crop at device
  scale whether it renders legibly; do not pass text you cannot read.

---

## 6. Known gaps (v1)

- Written before the Commons, Workbench and Glass exist: the gates are
  calibrated on the Slack cards, the `/threads` grade and today's `/garden`
  (World, Porch, Projects sheet, bead view). The first real grade
  ([[gd-vzsNYKoGNX]] proves the skill on a `/garden` surface) should report
  which questions were hard to answer, and v2 should tighten them.
- The OS layer is not reachable: web push banners, the on-screen keyboard
  covering the compose box, and the share sheet are untested, not passed.
- Contrast and target-size probes measure CSS boxes; pixel-art sprites drawn on
  a canvas need a manual sample until the harness emits per-element crops.
- C6 depends on whether card themes survive into Lands (open question for step
  0d, [[gd-lyIZyN6wof]]); if they are dropped, C6 becomes permanently `N/A`
  and the score denominator is 11.
- Presence and typing indicators (narrative Phase 4; DROP in the product
  definition) have no gate yet; if they ship, they belong in C10.
- **From the first grade** ([[gd-vzsNYKoGNX]], the in-garden bead view, 60 % →
  90 %), for v2 to settle:
  - C11's "≤ ~600 characters before folding" is written for cards. The bead
    view *is* the "open the full thing" target, so it was graded as a detail
    view (full text is its job). Say which surfaces are details.
  - `docked` has no meaning yet for panes that already sit beside the World
    at desktop width (bead view, Porch); the docked questions were answered
    from `desktop-*`.
  - "Full page plus first viewport" misses panes that scroll inside
    themselves; the skill steps the pane's own scroller instead (`-scrollN`).
  - The stylesheet probes are page-wide (`garden.css` has 37 rules with a
    colour literal outside `:root`); they need scoping to the SUT's rules
    before they can fail a gate on their own.
  - The *tiny text* "body text < 13 px" probe flags long metadata lines (a
    90-char agent name at 12 px); C3 Q5 clears them, but the probe should
    know metadata from body.
  - CJK cannot be graded in a pod without a CJK font (C7, F4).

---

## 7. Performance budgets (B1–B17)

> **Status:** v1 (2026-10-29). Step P0.2 ([[gd-TYrcWqxryk]]) of the Lands UX
> performance pass ([[gd-MEGB3dEaHu]]). The numbers start from the plan's
> proposal and are checked against the P0.1 baseline ([[gd-j8jXq1TC1m]],
> `gardenio/docs/lands-perf-baseline.md`). The owner confirmed them in
> decision [[gd-gmp7Tn29fy]], relaxing the two phone CPU budgets from the
> first draft: B4 from 2.5 s to 3.0 s and B5 from 1,000 ms to 1,500 ms.

C1–C12 grade how a surface looks. These budgets grade how fast it loads and
how much it costs to keep open. P3.1 ([[gd-7Ns0SDsF7R]]) fails CI on them,
P3.2 ([[gd-qeaDwFSDqq]]) puts a row for each one in the §4 grade (§4, *Perf
rows*: graded at the budget, beside the score), and P3.3
([[gd-mIUDS8zFaD]]) grades the before/after check-in against them. Each budget
has an id (B1–B17) so a grade row, a CI failure or a bug can name it.

### How they are measured

Every budget is read from one script:
`gardenio/controller/internal/gardenhub/testdata/perf_baseline.js`, run by
`make lands-perf-baseline OUT=<dir>` on the fixture farm, or by hand against a
deployed `/garden` with `LANDS_PERF_COOKIE`. The script header defines every
metric. The "Field" column below names the `results.json` field the budget
reads. A budget is met when the **median of 3 runs** is within it, for the load
kind and profile named, on **every surface it applies to**. B8 and B9 read the
idle pass, which runs once per profile.

- **Profiles** are the script's, not the §1 capture cells. `phone` is 390×844,
  touch, 4x CPU slowdown, 9 Mbps down, 1.5 Mbps up, 170 ms RTT. `desktop` is
  1280×800, unthrottled.
- **Loads:** `cold` is a fresh browser context and `warm` is the same context
  loading again.
- **Surfaces** are the script's rows: `world`, `beads`, `plans`, `inbox`,
  `decisions`, `glass`, `wall`, and the Commons, which is the workbench
  (`/garden/c/n/{id}`) and the steward (`/garden/c/s/{land}`) on their own
  page. The band's `channel`, `thread` and `compose` rows went with the band
  ([[gd-ZeiSZV7j2Y]], `gardenio/docs/adr-commons-band-replaced.md`). A new
  Lands surface gets a row in the script and is held to the same budgets.
- **Both targets:** the budgets apply to the fixture farm (what CI runs) and to
  production (P0.1b [[gd-dQHzmKGlBR]] and the P3.3 check-in). The fixture
  snapshot is 5 KB gzipped, so production, with its real farm and Commons
  history, is the harder test of B1 and B4.

### The budgets

| Id | Budget | Profile, load | Applies to | Field (`results.json`) | Class |
|---|---|---|---|---|---|
| B1 | Cold page weight **≤ 300 KB** on the wire (compressed, as served); the Glass watch, which also loads its terminal, **≤ 367 KB** | phone, cold | every surface; `glass` at 367 KB | `bytes.wire` | stable |
| B2 | Repeat-visit weight **≤ 50 KB** on the wire | phone, warm | every surface | `bytes.wire` | stable |
| B3 | **≤ 20 requests** to TTI, streams not counted | phone, cold | every surface | `bytes.requests` | stable |
| B4 | **LCP ≤ 3.0 s**. On the World, whose LCP is its `h1`, the map's **Ready ≤ 3.0 s** as well | phone, cold | every surface | `lcp`, and `ready` for `world` | CPU |
| B5 | Long tasks before TTI **≤ 1,500 ms in total** | phone, cold | every surface | `longTasks.total` | CPU |
| B6 | **INP ≤ 200 ms** on the first interaction (the script's `act`) | phone, cold and warm | every surface | `inp` | CPU |
| B7 | **No long task over 100 ms** during a 10 s pan and zoom | phone | `world` | `longTasks.max` of the `pan` rows, against their `calibration` | CPU |
| B8 | Idle network **≤ 5 KB per minute**, streams included | phone, idle pass | every surface | `idleBytes ÷ (idleMs ÷ 60,000)` | stable |
| B9 | **No heap growth** while idle: heap after GC ≤ heap loaded, and **≤ 10 MB** | phone and desktop, idle pass | every surface | `heapIdleGC`, `heapLoaded` | stable |
| B10 | **LCP ≤ 1.5 s** (on the World, Ready ≤ 1.5 s) | desktop, cold | every surface | `lcp`, `ready` | stable |
| B11 | **INP ≤ 100 ms** on the first interaction | desktop, cold and warm | every surface | `inp` | stable |
| B12 | Composer key to paint **≤ 16 ms** (the median key of the latency pass), and **no row repainted** by any key | phone, cold | the Commons | `latency.keyPaint.median`, `latency.rows` | CPU |
| B13 | Send to echo **≤ 50 ms**: the sent message on screen before the server answers (optimistic) | phone, cold | the Commons | `latency.echo` | CPU |
| B14 | Node open **≤ 100 ms warm** (from cache) and **≤ 300 ms cold**, for a thread, a plan and an epic root alike | phone, cold | the workbench | `latency.open.<root>.cold`, `.warm` | CPU |
| B15 | Stream event to paint **≤ 100 ms**: a message on the stream is on screen with no fetch of its own | phone, cold | the Commons | `latency.stream` | CPU |
| B16 | Commons cold weight **≤ 150 KB** on the wire | phone, cold | the Commons | `bytes.wire` | stable |
| B17 | Commons **LCP ≤ 1.5 s** | phone, cold | the Commons | `lcp` | CPU |

Notes on single budgets:

- **B1** counts JavaScript, CSS, fonts, sprites and data up to TTI, including
  the Glass's xterm bundle. 300 KB is the total. The Glass watch (`glass`,
  `?glass=<agent>`) is a pane deep link plus xterm.js, about 67 KB gzipped that
  no other surface loads, so it is held to 367 KB: 300 KB plus its terminal
  (owner decision [[gd-T8Lrqgp23V]]). The fixture serves `/garden`
  gzipped already, so on it `wire` is about `gz`. If production `wire` is well
  above `gz`, the edge is serving something uncompressed (P1.1).
- **B3** does not stop P1.2 ([[gd-jrIxewKgjF]]) from splitting the inline
  scripts out of `garden.html`. It stops each of the ~50 scripts becoming its
  own request on a 170 ms phone link: serve them as a few content-hashed
  bundles.
- **B4** is LCP because that is what a person waits for. The World's LCP lands
  at 0.4 s on its heading while the map is not usable until 2.6 s, so the World
  is held to Ready as well. The bar is 3.0 s, not the plan's proposed 2.5 s
  (the owner's call in [[gd-gmp7Tn29fy]]). Tightening it to 2.5 s once the P1
  and P2 levers land is a later change to this table.
- **B5** is the stable stand-in for B4 and B6. On a phone the cost is CPU, not
  network (P0.1 finding 2), so parsing and running less script before TTI is
  what moves LCP and INP.
- **B7** comes from the script's pan pass ([[gd-K3KmHhfnhd]]): on the phone,
  `LANDS_PERF_PAN` runs of P2.1's `world_pan_profile.js` ([[gd-PoCdbE1SJX]]),
  the World on the 10x farm with a 10 s pan and a zoom, graded on the pan.
  Each run first repaints a bare full-screen canvas at the same throttle for
  2 s (the `calibration`). Its longest task is the machine's **floor**: no
  full-screen canvas frame can be faster there. A shared pod has no GPU, so a
  2D canvas rasterizes on the main thread at Commit, and its floor is
  178–469 ms at 4x on an agent pod and 56–258 ms on the CI pool. Where the
  floor is within 50 ms, half the budget (a GPU runner, a real phone), B7 is
  graded on the pan's longest task. Where it is not, the raw maximum says
  nothing about the page, so B7 is *ungraded* and the CI gate grades its
  **load** instead: the pan's time per frame over the bare canvas's, both read
  in the same run. See the CI gate below.
- **B8** comes from the script's idle pass: 300 s with every surface open at
  once. The fixture farm sends no live events, so this is a floor. P0.4's
  inventory (`gardenio/docs/perf/netinv-2026-10-28.md`) measures active
  traffic, which has no budget yet. The idle window starts once the last
  surface is open and ends at each surface's read (`idleMs`). Before
  [[gd-CLQT35hQnk]] each page counted from its own open, against `idleS`. A CI
  pod takes 40–140 s to open the surfaces after it, so B8 read the pod's
  speed: the Commons' steady 8 requests a minute (322 B each, about 2.5 KB/min)
  read 4–8 KB/min. Streams count by the bytes they receive in the window:
  the tap starts before the page loads, and a stream the load opened adds
  its frames after the window starts, not its load-time bytes.

- **B12–B17** are the Commons v2 budgets (plan [[gd-lRaCxsiLvC]], step 1
  [[gd-J3LZYVcuBK]]), tighter than B1–B11 and set before the workbench was
  built, so the band was the "before". "The Commons" is every workbench
  surface: a node root (a thread, a plan, a phase, an epic), the steward, and
  a §4 grade's `commons*` surfaces, all on the workbench's page. B12–B15 read the script's **latency pass**
  (`LANDS_PERF_LATENCY=1`; a §4 grade always runs it), which drives each
  interaction for real on the fixture farm once a cold load's first
  interaction is read: seven keys typed into the composer, one every 150 ms;
  a message sent from it (Enter on a desktop, Send tapped on a phone); a
  thread, a plan and an epic opened from the workbench by id, then opened
  again from memory; and a message the farm says on the workbench's stream. Each is
  timed from the input's event (the server's push, same host, for the
  stream) to just after the frame that shows it. B12's rows are the
  timeline rows (`.cm-messages > li`) any key mutated. Each root's cold
  and warm open is a check of its own, so a plan that opens slowly is not
  hidden by a fast thread. A step that does
  not show within 10 s reads `null`: ungraded, never passed.
  The interactions are graded on the phone, where the round trips and the
  CPU show; the desktop's numbers are in `summary.md` for reading.
- **B16 and B17** tighten B1 and B4 for the Commons alone: the band was
  470 KB cold and 3.4 s to LCP on the phone. The rest of `/garden` is held to
  B1 and B4.
- **B12–B17 are in the CI gate, and hard** (step 9, [[gd-4s2Rw9hgBd]]):
  `lands-perf.yml` runs the latency pass on `perf_baseline.js`'s two
  workbench surfaces, `commonsthread` (a thread root) and `commonsplan` (a
  plan root), on a second fixture farm with a live Commons stream and the
  test routes the pass needs (`POST /test/perf/say`, `/test/perf/forget`).
  They have no ratchet: a number over its budget (a CPU one over its line)
  fails even where the base read over too, so the budgets the plan met hold
  after it. A PR that touches `internal/commons` or `web/commons2.js`
  proves them with one dispatch on its branch, narrowed to those surfaces
  (`gb ci dispatch lands-perf.yml --ref <branch> -f
  surfaces=commonsthread,commonsplan`); the workflow never runs on a pull
  request. A §4 grade reads them with `make commons-ux-shoot
  SURFACES=commonsthread,commonsplan`, on the same second farm, so the shots
  stay the goldens' world. A tap is timed from its release (`touchend`), not
  its press: the time a finger rests on the glass is not the page's, and
  Event Timing does not count it either. The step-1 baseline, the band with
  every Commons v2 budget but B12's rows red on the phone, is
  `gardenio/docs/perf/commons-v2-baseline-2026-10-31.md`.

### Classes: what CI fails on

P0.1 found that phone CPU numbers move up to **2x** with node load on a shared
agent pod, while bytes, request counts and desktop numbers stay put. So each
budget has a class:

- **stable** (B1, B2, B3, B8, B9, B10, B11): bytes, requests, heap and desktop
  times, which hold still from run to run. CI fails when the median is over
  the budget.
- **CPU** (B4, B5, B6, B7). Phone numbers under 4x throttling. They are graded
  at the budget only on a run whose CPU bench (`meta.bench` in
  `results.json`, also in the header of `summary.md`) is within 20 % of the reference bench P3.1 records, or on a
  dedicated runner. On a shared CI pod they fail only above **1.5x the
  budget**, and between 1x and 1.5x they are reported in the gate's summary
  as "over budget, noisy". P3.3's check-in and a rubric grade use the budget itself.

A budget that cannot be measured on a run (the script errored on that surface,
there was no pan pass, or the machine's floor is over B7) is *ungraded*, never
passed, as with a §1 cell that could not be captured.

### The CI gate (P3.1)

`.github/workflows/lands-perf.yml` never runs on a pull request (owner rule
2026-10-31, gd-yqSVQsjtyW). It runs nightly on main, grading main's tip
against main as it stood 25 h earlier, so a regression shows within a day,
and on dispatch (`gh workflow run lands-perf.yml --ref <branch>`), grading a
branch against its merge-base with main. It measures the two commits on the
fixture farm in parallel, with the same script, and `testdata/perf_gate.js`
grades them against this table. In the rules below, "the PR" is the head
commit and its changes. It writes the deltas to the run's summary and keeps a
Playwright trace per surface (phone, cold) in the run's `lands-perf-head`
artifact. The reference
bench is `REFERENCE_BENCH` in `perf_gate.js` (61 ms, P0.1's machine). Since
every surface is over B1 today, the gate works as a ratchet:

- **stable:** a number that goes over its budget fails. So does a number
  that was already over and gets worse than the base by more than the
  budget's tolerance in `perf_gate.js`, for example 2 % or 2 KB for B1, or
  any extra request for B3. A number that stays over without getting worse is
  reported, not failed. A surface the base does not have is held to the
  budget.
- **CPU:** a number fails only when it crosses the line above (the budget, or
  1.5x on a shared pod), the base was within that line, and the number is
  more than 20 % worse than the base once the two runs' benches are scaled.
  The bench is the one read next to each cold phone load (before and after
  it), not the run's opening one: a pod's speed moves 2–4x within a run. On
  PR #1449 (one CSS property) the head ran like the base for three surfaces,
  then 2.5–4.3x slower, at the same opening bench. A run with no such bench on
  either side cannot tell node from page, so it reports those as noisy. The
  base is scaled by the larger of the two ratios (the loads' benches, the
  openings'): either one can miss a slow node, and a page the PR slowed is
  slower than the base at both.
- **times** (B4–B6, B10, B11): a time fails only when the PR's fastest run
  is over as well. Node load only adds time, so a real regression shows in
  every run. The first run on identical code read the World's desktop INP at
  200, 48 and 232 ms, against 40, 48 and 40 on the base. A median over the
  line whose fastest run is within it is reported as noisy.
- **B7:** graded at 100 ms as a CPU time where the PR's calibration floor
  (the median of its runs' longest bare-canvas task) is within 50 ms
  (`FLOOR_SHARE`, half the budget). A floor just under 100 ms is not a machine
  that can show B7: PR #1466's splash-only diff read a floor of 98 ms and a
  pan of 281 ms against the base's 99 ([[gd-caHXOpfxNC]]), and the CI pool's
  floor is 94 ms in the median. Over the line, B7's *load* check is graded
  instead: the pan's time per frame over the bare canvas's. It fails only
  when the PR's fastest run is more than 50 % (`PAN_MARGIN`) over the base's
  median load. Node load slows the pan and the bare canvas together, so the
  ratio holds where the raw maximum swings (P2.1: 485–600 ms on the same
  code; the CI pool: 61–485 ms), and a frame time does not saturate on a busy
  node the way a share of time in long tasks did ([[gd-b9F02iUPb3]]). On 24
  CI results of the same code the load read 1.40–2.72x; every head-to-base
  pairing of them fails 2.5 % of the time at 50 %, and a pan made twice as
  slow a frame fails 76 %. The load catches work added to every frame of the
  pan, not a lone long task, which only a machine whose floor allows it can
  show. With no base the load is ungraded.
- **ungraded:** a number the PR's run could not measure fails when the base
  measured it, a broken pan pass included.

`perf_gate.test.js` (`make test-lands-perf-gate`) holds the gate's budgets to
this table's Budget and Class columns. A change to a budget here fails it
until `perf_gate.js` changes to match.

### Where each surface stands (P0.1, fixture farm, 2026-10-28)

Medians of 3 runs from `gardenio/docs/lands-perf-baseline.md`. **Bold** is over
budget. Times are in ms and sizes in KB. The band's three Commons rows are left
out: the band is gone ([[gd-ZeiSZV7j2Y]]), and that baseline keeps them. The
workbench's own numbers are in `gardenio/docs/perf/commons-v2-*.md`. Streams are from P0.4's inventory.
B7 was not measured then (P2.1's pan profile on an agent pod: the longest pan
task 485–600 ms over a 178–469 ms floor, `lands-perf-world-render.md`).

| Surface | B1 cold KB | B2 warm KB | B3 req | B4 LCP (Ready) | B5 long tasks | B6 INP cold / warm | B8 idle KB/min | B9 heap after GC MB | B10 desktop LCP | B11 desktop INP |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| World | **456** | 1.2 | 4 | 424 (2584) | **1568** | 184 / **296** | 1.4 | 3.5 | 164 (Ready 501) | 32 |
| Workbench bead view | **457** | 2.5 | 5 | **3616** | **2525** | 40 / 48 | 1.2 | 4.2 | 772 | 24 |
| Workbench plan cockpit | **459** | 4.6 | 5 | 2648 | 1468 | 104 / 112 | 1.2 | 4.2 | 700 | 40 |
| Workbench inbox | **457** | 1.7 | 5 | 2452 | **1706** | 176 / 120 | 1.2 | 4.1 | 752 | 24 |
| Workbench decisions | **457** | 2.2 | 5 | 2752 | **1723** | 112 / 96 | 1.0 | 4.1 | 912 | 32 |
| Glass (watch) | **742** | 2.8 | 8 | **3252** | **2045** | 40 / 56 | 0.9 | 5.0 | 876 | 32 |
| Glass (Wall) | **457** | 2.4 | 5 | 2816 | 1477 | 72 / 48 | 0.9 | 4.1 | 760 | 40 |

What it says:

- **B1 fails on every surface**, by 1.5x (2.5x on the Glass, which also loads
  285 KB of uncompressed xterm). That is the one 1.49 MB inline document. P1.2
  (split), P1.3 (lazy modules) and P1.5 (minify) are the levers.
- **B4 failed on five of ten surfaces** then: the band's three Commons views,
  the bead view and the Glass watch, at 3.25–3.62 s. The World passes on Ready
  (2.58 s). **B5 fails on eight**: loads run 1.47–2.53 s of long tasks before
  TTI, against a 1.5 s budget. The plan cockpit and the Wall pass B5 by
  under 35 ms, so on a noisy run they can flip. The same levers apply, plus
  P2.1 and P2.2 for the World.
- **B6 fails only on the World's warm first move** (296 ms). The cold move
  (184 ms) and the inbox tab switch (176 ms) are close. P2.1 owns the World.
- **Already within budget:** B2, B3, B8, B9 and all of desktop (B10, B11).
  For these the gate stops regressions. B3 is the one P1.2 could break.
