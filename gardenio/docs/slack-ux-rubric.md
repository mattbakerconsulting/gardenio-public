# Slack UX Rubric (U1–U12)

> **Status:** v1 (2026-10-14). Task [[gd-NvQnzUhNAy]], epic [[gd-D0ybl5EMM3]]
> ("Loop: UX-acceptance workflow, rubric and dogfood"), from the Slack thread
> [[gd-H9rqCO6FYU]]: *"gardenio agents view slack channels using chrome … so
> they can actually see what's going on and how their messages come across,
> especially for ux acceptance testing and revisions."*
>
> The in-game sibling for Lands surfaces in `/garden` (Commons, Workbench,
> Glass) is **C1–C12** in `commons-ux-rubric.md`.

This document defines **how to grade a Slack message the gardenio bridge posts**
(an agent card, decision card, escalation, thread card, alert, command reply)
after it has been rendered in a real Slack client and screenshotted. It is the
Slack-space sibling of the beads-viewer Phase-3 `/threads` gates **V1–V12**
(`gardenio/tests/beads-viewer-screenshots/THREADS-GRADE.md`), the Plans
**PACES** rubric (`plans-ux-rubric.md`) and the **CREWS** formula rubric
(`formula-ux-rubric.md`), and it keeps their idiom:

- **The screenshot is the source of truth.** Payload checks only triage; every
  verdict is confirmed by opening the PNG (Read tool).
- Each gate is `PASS`, `FAIL` or `N/A` — never fake-passed. `N/A` needs a reason
  (the surface isn't in the message under test).
- **Every `FAIL` links its screenshot** (the V12 rule), and low scores become a
  bug bead.
- The grader **observes, grades and files**. A fix is a normal MR by whoever
  picks up the filed bug; the grader does not edit bridge code.

The consumers are the skill `slack-ux-acceptance` ([[gd-iJj09luvBV]]), which
posts a Block Kit payload to a test channel and grades it with this rubric, and
the dogfood run on a real thread card ([[gd-An3AH4Yqr7]]). The capture path is
`tools/slack-view` ([[gd-G81tGJIRnC]]) / the Playwright MCP
([[gd-pW0jMruPRb]]), which load the test user's `storageState`
(`slack-e2e-provisioning.md`).

---

## 1. What is being graded

One **message under test (MUT)**: the root post plus its thread replies and any
`chat.update` edits that have landed, as rendered by `app.slack.com`. Grade what
a human sees, not what the code intended — the bridge's Block Kit builders are
the *suspect*, the rendered pixels are the *witness*.

Slack is not a web page we control. We control: the `blocks` array, the
`attachments` color bar, the notification `text` fallback, button
`action_id`/`style`/`confirm`, and the words and glyphs we choose. We do **not**
control fonts, Slack's own "Show more" collapse, or the user's theme. The rubric
grades our choices **under** those constraints.

### Capture matrix

Every MUT is captured at these cells; each gate says which cells it needs.

| Cell | Setting | Why |
|---|---|---|
| `desktop-light` | 1440×900, Slack theme *Light* | The default reading surface. |
| `desktop-dark` | 1440×900, Slack theme *Dark* (set via the Preferences → Themes toggle in the test user's session, or `prefers-color-scheme: dark` emulation) | Slack re-colors its own chrome; our attachment bars, images and glyphs are *not* re-colored for us. |
| `narrow-light` | 430×932 (mobile-web proxy), Light | The nearest thing to the phone client we can drive. Real iOS/Android clients are **not** reachable; where a gate depends on them it is marked *proxy* and the verdict says so. |
| `narrow-dark` | 430×932, Dark | Mobile + dark together catches the worst contrast and wrap combinations. |
| `thread-open` | Desktop, the thread pane open on the MUT | Cards posted in-thread render in a narrower pane (~ 350 px of text), the case most agent cards live in. |
| `card-theme` ×2 | The same MUT built under both `garden` and `business` card themes (`theme.go`) | The theme must change vocabulary and glyphs, never layout or readability. |

Capture after the message has **settled** (no spinner, link unfurls resolved,
`chat.update` edits applied) and with the MUT scrolled fully into view. A
message taller than the viewport is captured as scroll-stitched `fullPage`
plus the first viewport.

---

## 2. Slack hard limits (the payload lint)

These are cheap to check before any browser is involved and fail **silently
worse** than they look: a violation makes `chat.postMessage`/`chat.update` return
`invalid_blocks`, and a failed `chat.update` does **not** degrade to a plainer
card — the card simply never updates (this is the ka-0000006181 incident:
a composed header over 150 chars lost the banner on every gardener card). The
`slackmock` server (`gardenio/tests/slackmock`) does **not** enforce these, so a
green mock proves nothing about them; the lint below is the guard until it does.

| Element | Limit | Failure |
|---|---|---|
| `header` block text | **150** chars, `plain_text` only | `invalid_blocks`; mrkdwn/`&lt;` shows literally |
| `section` text | **3000** chars | `invalid_blocks` |
| `section` fields | ≤ **10** fields, **2000** chars each | `invalid_blocks` |
| `context` elements | ≤ **10** | `invalid_blocks` |
| `actions` elements | ≤ **25** (web renders ~5 per row, then wraps) | `invalid_blocks` |
| blocks per message | **50** (modal/home tab: 100) | `invalid_blocks` |
| button `text` | **75** chars | truncated with `…` by the client |
| button `value` | **2000** chars | `invalid_blocks` |
| `action_id` / `block_id` | **255** chars; `action_id` unique per message | duplicate ids → clicks route to the wrong handler |
| `confirm` title / text / button | 100 / 300 / 30 chars | `invalid_blocks` |
| modal `title` / `submit` / `close` | **24** chars each | `invalid_blocks` |
| select/option `text` | **75** chars | truncated |
| message `text` (notification fallback) | keep ≤ ~**300**; hard cap 40 000 | shown in push/desktop notification, search and screen readers |
| `image` `alt_text` | required, ≤ 2000 | `invalid_blocks` |

---

## 3. The twelve gates

Each gate has an **objective** check (payload or DOM, scripted) and a **visual**
check (the screenshot). A gate's verdict is the *worse* of the two. Cells in
parentheses are the capture cells that gate needs.

### Truncation

#### U1 — Nothing the reader needs is cut off, and every cut is honest
*(desktop-light, narrow-light, thread-open)*

- **Objective:** no field exceeds the §2 limit; any text the code shortens goes
  through a rune-safe cap (`truncateRunes`, never a byte slice that splits a
  multi-byte rune or a `<url|label>` / `<@U…>` token), and the cut is marked
  with `…`. A header is capped on the **composed** string (glyph + label +
  text), not on the text alone (`slackHeaderText`).
- **Visual:** the card's *decision-relevant* content — what happened, what is
  being asked, what to click — is visible without opening anything. No
  mid-word / mid-link / mid-entity cut (`&am…`, half a `<https://…`). If
  content was dropped, the card says so and says where the whole thing is
  ("full report: canvas / `kd show gd-…`") — an unmarked clip is a FAIL.
- **Slack's own collapse:** long messages get Slack's "Show more". The first
  ~5 lines / ~700 chars are all most readers see, so the headline and the call
  to action **must be inside that fold**; detail may sit below it.
- **Auto-FAIL:** a posted message whose `chat.postMessage`/`chat.update`
  returned `invalid_blocks`; a `&lt;`/`&amp;` literal visible in a header.

### Wrapping

#### U2 — Text wraps cleanly; no wrap-dependent layout
*(desktop-light, narrow-light, thread-open)*

- **Objective:** no hand-aligned columns in prose; any monospace table lives in
  a code fence and fits **≤ 40 columns** (the width of a phone/thread-pane code
  block before it scrolls sideways). Long unbroken tokens (URLs, bead ids,
  branch names, SHAs) are links with a short label, not raw strings.
- **Visual:** at `narrow-light` and `thread-open` no line is clipped or forces a
  horizontal scroll bar inside a code block *for content the reader needs*; no
  orphaned single glyph or single word on its own line from a bad join; list
  bullets and indented lines survive the wrap with their meaning intact; a
  `section` with `fields` reads in a sane order when the two columns collapse
  to one.
- **Reminder:** the same text wraps at different points in every cell — a
  message that is only readable at one width is a FAIL.

### Mobile

#### U3 — Usable on a phone-width client *(proxy)*
*(narrow-light, narrow-dark)*

- **Objective:** ≤ 5 buttons in one `actions` block (more wrap into a ragged
  second row on narrow); button labels ≤ ~20 chars so none truncates; no block
  relies on hover, tooltips or a wide table.
- **Visual:** at 430 px every button is fully legible, tappable (not crowded —
  the visible gap between buttons ≥ one button-label of height) and still next
  to the thing it acts on; the headline and the primary action are both
  reachable within one screen of the message top; no text is smaller than the
  context-block size except deliberately demoted metadata.
- Marked *proxy*: this is mobile-web emulation. A fully clean proxy run is
  "PASS (proxy)"; anything that would only show on the native client (push
  notification wording, Dynamic Type, share sheet) is recorded as untested, not
  passed.

### Theme

#### U4 — Legible in Light **and** Dark Slack
*(desktop-light, desktop-dark, narrow-dark)*

- **Objective:** no hard-coded foreground/background in any image we upload;
  uploaded images (charts, status art) have a transparent or neutral-safe
  background and carry `alt_text`. Attachment `color` bars are chosen from the
  theme palette (`Theme.Color`) only; the bar is a *supplement* to a verbal
  state, never the only signal.
- **Visual:** compare `desktop-light` to `desktop-dark`. Every glyph, bar and
  image is distinguishable from its background in **both**; a dark-on-dark or
  light-on-light image, a bar that vanishes into the dark sidebar, or an emoji
  that is a black blob in Dark is a FAIL. WCAG-style: text/glyph contrast
  against its real background ≥ 4.5 : 1 (sample the pixels; do not eyeball).
- This is the Slack analogue of V4's "readable contrast in BOTH themes — the
  `data-skin="dark"` trap". Test Dark every time; it is the cell that is
  forgotten.

#### U5 — Garden and Business themes differ in vocabulary, not in quality
*(desktop-light, ×2 card themes)*

- **Objective:** every lifecycle glyph is resolved through the theme
  (`th.Glyph`, `th.Semantic`, `th.Tool`, `th.Reaction`) and not hard-coded; no
  `:shortcode:` literal and no plant glyph appears in a Business render; a
  Business render contains no Garden-only word (Seedling, Greenhouse…).
- **Visual:** the two renders have the **same structure** — same blocks in the
  same order, same hierarchy, same button set — only glyph/tag and wording
  differ. Neither is cluttered or clipped relative to the other (bracket tags
  are wider than a single glyph: check the header and chips still fit).

### Emoji

#### U6 — Emoji are glued to words, resolvable, and few
*(desktop-light, desktop-dark, narrow-light)*

- **Objective:** (the `plans/slack-greenhouse-restyle.md` accessibility rule)
  every inline glyph appears **immediately before its verbal label in the same
  text run** (`🍃 Working`) — a bare glyph is a FAIL; glyphs come from the
  theme, so no raw `:shortcode:` is left unrendered; reactions use **stock**
  emoji names only (custom emoji are absent from some workspaces and render as
  literal `:name:`). Count of distinct glyphs in one card ≤ 6; no glyph reused
  for two different states (a reused glyph is a duplicate *label*).
- **Visual:** every glyph actually renders (no `:shortcode:` text, no tofu `▯`,
  no skin-tone / VS16 mismatch that turns ⚠️ into a flat ⚠); the glyph set reads
  as one family at the size shown; in Dark none disappears (cf. U4); a screen
  reader line ("seedling Working") would still make sense — meaning is carried
  by the word, the glyph only reinforces.
- **Density rule:** emoji decorate state and type; they do not decorate prose.
  More than one glyph per line of body text is a FAIL.

### Button affordance

#### U7 — Buttons look pressable, say what they do, and are safe
*(desktop-light, desktop-dark, narrow-light)*

- **Objective:** each `actions` block has **exactly one** `primary`-styled
  button at most, and it is the recommended / safe-default action; `danger` is
  used only for destructive or irreversible actions (kill, deny, close) **and**
  those carry a `confirm` dialog (title ≤ 100, text says what will happen and to
  what). Labels are verbs ("Approve", "Reopen") not nouns ("OK", "Option 2");
  `action_id`s are unique within the message; the `value` is parseable by the
  handler. A button that does nothing must not exist (no dead `action_id` —
  check there is a registered handler).
- **Visual:** buttons are visibly buttons (outlined/filled, not text that reads
  as a link) in both themes; primary vs danger vs default are distinguishable
  *without color* (label + position, not green-vs-red alone); the destructive
  button is not adjacent to — and not larger/more prominent than — the safe one.
- **Post-click honesty (needs a click; `N/A` for a static grade):** after a
  press the card changes within ~1 s to show the outcome and the buttons are
  removed or disabled — a card that still shows live buttons after the action
  was taken invites a double-fire. A button whose result is only an
  ephemeral toast leaves the thread unable to show *what was decided*.

### State

#### U8 — State is unambiguous at a glance (no color-only, no stale chrome)
*(desktop-light, desktop-dark, narrow-light)*

- **Objective:** the current lifecycle state appears as a word (and its theme
  glyph) in the header/first line, derived from the bead's real state — a card
  edited by `chat.update` carries the **latest** state, not the state at post
  time (the "lying card" class of bug).
- **Visual:** within a second of looking, the reader can answer *what is this,
  what state is it in, does it need me?* A card in a terminal state (done /
  aborted / reaped) shows no live affordances and reads as resolved; a card
  that needs a human reads as needing one (action row present, state word says
  so).

### Card hierarchy

#### U9 — One clear reading order: headline → state → detail → action
*(desktop-light, narrow-light, thread-open)*

- **Objective:** block order is `header` (or a single bold lead line) → `context`
  metadata (agent, cost, turns, ids — *demoted*) → body `section`s → `actions`.
  Exactly **one** `header` block per card. `divider`s separate groups, not every
  line. Metadata that is noise for the common reader (cost, context-turns,
  ids) sits in `context` blocks, not in the headline.
- **Visual:** the eye lands on the headline first (the outcome / ask in plain
  words — e.g. the wrap-up `tldr`), then the state, with metadata visibly
  quieter than content. The mirror of V3 ("narrative leads; work tally is
  quiet meta") and V2 ("conversation first, work collapsed"): the thing a human
  must act on is never buried under the thing an agent merely did.
- **Auto-FAIL:** a card that is metadata only (no outcome sentence) when the
  bead had a tldr / close_reason to show; a header that is just an id.

#### U10 — Density and length are proportionate
*(desktop-light, narrow-light, thread-open)*

- **Objective:** blocks per card ≤ ~12 for a normal card (hard cap 50);
  `section` bodies ≤ ~600 chars before they are folded to a link / canvas /
  thread reply; ≤ 3 `context` lines; lists > 8 items are summarized with a
  count and a link, not dumped.
- **Visual:** the card fits in about one screen (≈ 700 px tall at desktop) and
  does not push the thread it lives in off the screen; whitespace between
  groups is consistent; no wall of mrkdwn. A long report belongs in a canvas /
  linked page, with the card carrying the verdict and the link.
- **Thread discipline:** a status that updates in place is `chat.update` on one
  message, not a stream of new ones. More than ~3 bot messages in a row with no
  human message between them in a thread is a FAIL (noise), unless they are the
  streaming-card surface by design.

### Links and mentions (cross-cutting)

#### U11 — Links, mentions and ids resolve and read as labels
*(desktop-light, narrow-light)*

- **Objective:** bead ids are hydrated to a titled link (`slack_link_hydrate.go`,
  `slack_linkify.go`) rather than left as a bare `gd-…`; URLs are `<url|label>`
  with a human label; `&`, `<`, `>` are escaped in user/agent text (an
  unescaped `<` swallows the rest of the line as a broken link); user mentions
  are `<@U…>`, never `@name` text (which pings no one and reads as plain text);
  no `@channel` / `@here` from an automated post unless the message is an
  explicit page.
- **Visual:** every link renders as a link in the right color, no bare id
  next to its own title (the same thing twice), no unfurl preview that doubles
  the card's height (suppress unfurls on bot cards: `unfurl_links:false`),
  no `<@U…>` shown as a raw id (an unresolved mention is a FAIL — the
  `user_directory` lookup failed).

#### U12 — Fallback text and accessibility
*(any cell; plus a read of the posted JSON)*

- **Objective:** every message has a non-empty `text` fallback that works as a
  one-line summary on its own (it is the push-notification, desktop-banner,
  search-result and screen-reader text — a card posted with blocks only shows
  as *"This content can't be displayed"* in those places); every `image` and
  `image_url` accessory has `alt_text`; no information exists only in an image
  or only in an attachment color bar.
- **Visual:** the notification / "Activity" preview line for the MUT (capture
  the Activity or threads pane if reachable) reads sensibly out of context —
  state, subject, and who/what, not `[no text]` or a raw JSON-ish blob.

---

## 4. Grading output

For each MUT the grader writes `SLACK-UX-GRADE.md` next to the screenshots,
in the V-grade shape:

```markdown
# Slack UX grade — <message / surface> — <date>

MUT: <channel permalink>   builder: <file:func>   theme: <garden|business>
Captured cells: desktop-light, desktop-dark, narrow-light, narrow-dark, thread-open

| Gate | Verdict | Cell(s) | Evidence |
|---|---|---|---|
| U1 Truncation | PASS | desktop-light | shots/…png |
| U4 Theme | FAIL | desktop-dark | shots/…png ← linked, per the V12 rule |
| …
```

Plus: the objective payload dump (blocks JSON, the lint results of §2), one
paragraph per FAIL (mechanism → where in code → the fix that closes it), and
the list of `N/A` gates **with the reason**.

### Verdicts and bands

- `PASS` — meets objective and visual checks in every required cell.
- `FAIL` — any required cell fails either check. Auto-FAIL conditions above
  fail the gate regardless of the rest of the card.
- `N/A` — the surface is absent from the MUT (no buttons ⇒ U7 `N/A`; no image ⇒
  half of U4/U12 `N/A`). Never use `N/A` to skip a cell that is merely
  inconvenient.
- `PASS (proxy)` — U3 only, per the note above.

**Per-message score** = `PASS` count ÷ (12 − `N/A` count), shown as a
percentage, with the gate letters of every FAIL. Bands: **100 %** accept ·
**≥ 83 %** accept-with-nits (file the FAILs, do not block) · **< 83 %** revise
(block the MR the message ships in, until the FAILs are fixed and re-shot).

**Severity of the filed bug**, independent of the score:

| Severity | Gates | Why |
|---|---|---|
| P1 | U1 (auto-FAIL), U7 unsafe `danger`/missing confirm, U8 lying card, U12 empty fallback | Wrong or unsafe information reaches a human, or the post fails to land. |
| P2 | U2, U3, U4, U5, U9, U11 | A reader is slowed or misled but nothing is wrong. |
| P3 | U6, U10 | Polish and proportion. |

Dedupe by `slack-ux:<gate>:<surface>` label so a re-run **updates** the open
bug instead of filing a duplicate.

---

## 5. How the loop uses it

1. **Author** changes a Block Kit builder (or a theme) in an MR.
2. The `slack-ux-acceptance` skill (`.claude/skills/slack-ux-acceptance/`, [[gd-iJj09luvBV]]) posts the changed card —
   from a fixture or the builder under test — to the **test workspace**
   channel, never to a production channel (`slack-e2e-provisioning.md`).
3. `tools/slack-view` captures the §1 matrix as the logged-in test user.
4. The grader (an agent with vision) reads each PNG, runs the §2 lint on the
   posted payload, and writes `SLACK-UX-GRADE.md`.
5. **Accept** → link the grade on the MR. **Revise** → file the bugs, push a fix,
   re-shoot only the failing cells (the grade lists which), re-grade.

A before/after pair for the same MUT — the same shape as the beads-viewer
`capture-navset` before/after trees — is the preferred evidence on an MR that
*changes* an existing card.

Prerequisite and failure modes the grader must report instead of guessing:

- **Session expired / not logged in** (the capture lands on the Slack sign-in
  page, or the MUT is not in the DOM): stop, report *"session needs refresh per
  Part B"* ([[gd-6Jq5oR9fpd]]), grade nothing. A blank screenshot is not a
  `FAIL` of the message.
- **MUT not found in the channel**: the post failed — fetch the API response and
  report the `invalid_blocks` / error text; that is U1/U12 evidence.
- **Dark cell unreachable** (theme toggle not applied): `U4` is *ungraded*, not
  passed.

---

## 6. Known gaps (v1)

- No native iOS / Android capture: U3 and parts of U12 are proxy-graded.
- `slackmock` does not enforce §2; the lint is a script run by the grader, and
  the right long-term fix is to teach `slackmock` to return `invalid_blocks`.
- The contrast sample in U4 is manual-pixel until the capture tool emits
  per-glyph crops.
- Rubric is calibrated on agent / decision / escalation cards; alert and
  command-reply surfaces should be added to the dogfood set after
  [[gd-An3AH4Yqr7]] reports what the first real run finds hard to grade.
