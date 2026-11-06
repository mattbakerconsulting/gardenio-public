# Lands on a phone: support matrix

> **Status:** v1 (2026-10-28). Step M0.3 [[gd-ALXcTt29E5]] of the Lands mobile
> readiness pass [[gd-U4BtfWKjvc]] (narrative [[gd-jXGER3TVsN]]). Built on the
> M0.1 phone sweep [[gd-0B2drDTgFw]] (97 of 152 surfaces pass; every failure is
> in landscape or at 360 px). The real-phone walk M0.2 [[gd-qXom0SZr6z]] was
> deferred into the M4.3 owner check-in [[gd-YJPqjmLb47]], so nothing below has
> been checked on real WebKit yet. M4.2 [[gd-aY537mjJok]] added the device lab
> (§4): the emulation runs, and a real phone over the public edge.

This file says which phones and browsers Lands supports, what has to work on
them and what is allowed to be worse than on a desktop, and how each surface is
tested. Plan steps and bug triage use it: a failure on a **Supported** row in a
**Must work** cell is a bug in this plan; a gap in a **May degrade** cell is
not, unless it breaks something in the Must work column.

There is no mobile app. The phone browser is the client, and strangers arrive
on one on Open day (Lands phase 11, [[gd-xHvLXLLdXf]]).

---

## 1. Targets

| Tier | Platform | Browser | What the tier means |
|---|---|---|---|
| **Supported** | iOS and iPadOS 17 and later (17, 18, 26 and on: Apple jumped from 18 to 26 in 2025) | Safari, and every other iOS browser (Chrome, Firefox and Edge on iOS all run WebKit) | Tested by emulation in CI and the sweep, and on hardware at M4.3. A Must-work failure is a P1 or P2 bug under this plan. Includes the home-screen app (standalone). |
| **Supported** | Android 12 and later | Chrome, the current stable and the one before | As above. Chrome updates apart from Android, so the engine is what matters, not the OS. Includes the installed app. |
| **Best effort** | Android 12 and later | Samsung Internet, Edge (both Chromium), Firefox for Android | Not tested. A bug is fixed when the fix is cheap and costs a Supported browser nothing. |
| **Loads, unsupported** | iOS 16; Chrome 105 to 107 | | Above the engine floor (§2), so the page lays out, but nobody tests it. |
| **Below the floor** | iOS 15 and older; Chrome before 105; Firefox before 121 | | The World does not lay out (§2). Lands does not sniff browsers or show a warning. |

Why these lines:

- **iOS 17** covers every iPhone from 2018 on (XS, XR and later; iOS 18 runs on
  the same phones, iOS 26 needs an iPhone 11 or later), and Apple's security
  fixes go to recent majors. iOS 16 meets the engine floor but adds a release
  to test for very few phones.
- **Android 12**: phones from 2021 on, still on security updates. Below it the
  OS is not the problem, the test budget is: Chrome's engine is the same on any
  Android it installs on.
- **Passkeys** need iOS 16 or Android 9 with Google Play services, both below
  these lines. Keycloak's password sign-in stays for every other case.

### Screen sizes

| | Width × height (CSS px) | Notes |
|---|---|---|
| Reference phone | 390 × 844 (Safari leaves 390 × 664) | The CI goldens' phone; iPhone 12 to 16 class. |
| Narrowest | 360 × 740 | Galaxy S8 class. Phones of 375 px (iPhone SE 2nd and 3rd gen, mini) sit between 360 and 390 and are covered by both. |
| Widest phone | 430 × 932 | Pro Max and Plus class; no cell, the 390 layout widens. |
| Landscape | 740 × 360 down to 750 × 340 | Under 900 px wide, so still the phone layout, with very little height. Under 481 px tall in landscape the tab bar is a rail down the left edge, so the map, its title bar and every pane run to the bottom of the screen and the bead view covers the header ([[gd-p0DsdxVxJU]]). |
| Tablet | 820 × 1180 (the rubric's `mid` cell) | Between the 640 and 900 breakpoints; an iPad in portrait gets the phone layout. |

`garden.css` switches to the phone layout under 900 px wide and to 44 px
targets under `pointer: coarse`, so a tablet or a touch laptop gets touch
targets whatever its width.

---

## 2. The engine floor

What the pages actually use, the first version that has it, and what happens
without it. The floor is the highest row whose loss breaks a Must-work cell.

| Feature | Where | Safari | Chrome | Without it |
|---|---|---|---|---|
| Container query units (`cqw`, `container-type`) | `garden.css` `.board` sizes the World in `100cqw / 384`; the plot cards | 16 | 105 | **The World map does not size. This is the floor.** |
| `:has()` | Commons presence row, bold unread channels, the Reply link's place | 15.4 | 105 | Cosmetic: unread is not bold, Reply may overlap a long name. |
| `100dvh` (with a `100vh` fallback) | `body`, the Commons lightbox | 15.4 | 108 | The address bar can cover the bottom of the page and the composer. |
| `<dialog>.showModal()` | Commons picture lightbox | 15.4 | 37 | Below the floor anyway. |
| `overscroll-behavior` | Panes, `body` | 16 | 63 | Scroll chains to the page and pulls to refresh. |
| `env(safe-area-inset-*)`, `viewport-fit=cover` | Tab bar, composer, docked panes | 11 | 69 | Below the floor anyway. |
| `visualViewport` (guarded) | `commons.js` keeps the composer, the tab bar, an open lightbox and the bead view above the keyboard | 13 | 61 | The keyboard can cover the composer, a lightbox's field and the bead view's fields. |
| `Intl.Segmenter` (guarded) | `chat.js` cuts text at whole graphemes | 14.1 | 87 | A cut can split an emoji. |
| ES modules, `import()`, WebGL | The splash's three.js scene (bundled for ES2020) | 14 | 80 | The splash falls back to its flat picture (`splash-fallback.png`). |
| `theme-color` with `media` | `garden.html`, day and night bar colours | 15 | 93 | One bar colour for both themes. |
| Web push, service worker | Not used | | | No notifications from the browser; see Notifications in §3. |

Keep this table true: a change that uses a newer feature without a fallback
raises the floor, and has to say so here and in §1.

### Phone quirks we design for

- **iOS Safari's bars** take 180 px of an 844 px screen and come and go as the
  page scrolls. Use `dvh` and the safe-area insets, never a bare `100vh`
  (M1.1 [[gd-H8gSsz2h85]]).
- **The on-screen keyboard** resizes only the visual viewport, on iOS and on
  Chrome 108 and later. The Commons sheet and its lightboxes follow
  `visualViewport` (M1.3 [[gd-xWWqgcmzXM]]; what the keyboard does is in §3,
  The keyboard on a phone).
- **iOS zooms into any input whose text is under 16 px** on focus. Under a
  touch pointer `garden.css` makes every text field on `/garden` 16 px, and
  the goldens' `zoomField` probe fails a phone shot with a smaller one (M1.3;
  the Gate's forms are M2.4 [[gd-KiRStJd56I]]).
- **The home-screen app has no browser Back button** on iOS. Every pane needs
  its own way back and a working history (M1.4 [[gd-4Y2qOgfmJE]]). The tab bar
  reaches every surface (Farm, Commons, Porch, Mail, Bench for the Workbench,
  and Glass for a viewer who may watch); a tab switch pushes no history, the
  Workbench and Glass tabs reopen the panel they last showed, and a deep link to
  a bead, plan or Glass lands with the Farm beneath it, so Back or the close
  goes to the map, as a thread link's Back walks the Commons stack.
- **iOS installs only from the Share sheet** ("Add to Home Screen"); there is no
  install prompt. It reads `apple-touch-icon` and ignores most of the manifest
  (`install.go`, M3.1 [[gd-7vQb34FtMc]]).
- **A backgrounded tab is frozen** and its EventSource dropped. It must
  reconnect and catch up on wake (M3.2 [[gd-Xo3Stftjv6]]).
- **There is no hover.** Anything shown on hover needs a tap or a long press
  (M1.2 [[gd-cihiPae1J9]]).

---

## 3. What must work, what may degrade

The bar for every Must-work cell, on every Supported row: done with touch
only, at 360 to 430 px wide, **portrait and landscape**, light and dark, with
nothing needed off screen, no sideways page scroll, 44 px targets and text the
rubric accepts (`commons-ux-rubric.md`, C1 to C3, C5, C8). Portrait is the main
use. Landscape is graded by the same gates but may show less at once, as long as
the content area never drops to nothing (the umbrella bug [[gd-p0DsdxVxJU]]).

| Surface | Must work on a phone | May degrade |
|---|---|---|
| **The Gate** (`/lands/` front of house, terms, accept, GitHub setup, apply, applications) | Read the front page, sign up with GitHub, fill in and send the plot application, see its status. Form fields do not zoom on focus; errors show above the keyboard. | The music button and decoration may be hidden or moved. The card header may shrink in landscape. |
| **Sign-in** (Keycloak: password, passkey, TOTP, verify email) | Sign in with a passkey (Face ID, Touch ID, fingerprint) or a password, enrol a passkey, do TOTP. A failed passkey offers the password. | The Keycloak account console's layout (it is Keycloak's own UI), as long as you can reach and manage your passkeys. |
| **World** (`/garden`: map, cursor, plot cards, tips, Porch, bin, Menu, Jump, legend) | Open the garden, pan, pinch-zoom the map without zooming the page, tap a plot or sprite to open its card, read every tip and card in full, use the Menu and Jump, reach the Porch and bin. | **Pinch-zoom precision**: picking one small sprite in a crowd may take a zoom in first; hit-slop and the cursor are the guarantee, not pixel-exact taps. The sibling jump (`Shift` and an arrow; on a phone a tap on a Thing outside the grid jumps to it, see No pad below). Keyboard shortcuts. The splash's 3D scene (falls back to flat) and its motion. |
| **Commons** (channel list, thread, compose, cards, decision modal, presence, DMs, pictures) | Read the channel list and a thread full screen and go back; write and send with the keyboard open and the thread keeping its place; read every card face in full; answer a decision, including Other; open a picture; attach a file from the phone's picker; see who is present. | The docked pane beside the World (desktop only; a phone gets a full-screen sheet). Hover previews (a tap opens instead). In landscape: fewer messages on screen at once. |
| **Workbench** (bead view, queues, inbox, decisions and escalations, plans cockpit, project, costs, pictures) | Open a bead from a list and read it in full; comment on it; answer a decision or an escalation; read a plan's timeline and phase summaries and tap into them; read a project's spend. | Dense charts (costs, the plan timeline) may show fewer labels; a value is there on tap. Cmd-K is a desktop key; a phone uses the search field. Inline edits of long fields may be easier on a desktop. |
| **Glass** (watch, the Wall, a session's identity, credential pool) | See which agents need you; watch a live session: its state chip and the terminal at the PTY's own size, scaled to fit or at Actual size with sideways pan, never rewrapped (GL-2, design `2026-10-19-lands-workbench-glass-design.md`); pinch the terminal to size its glyphs; answer a waiting prompt or nudge from the session's card. For an operator with attach: tap the terminal to type into it, with Esc, Tab, Ctrl, the arrows and Paste on the key row, and Let go (the Glass on a phone, below). | Reading a full 80- or 120-column screen needs a pinch or a sideways pan. Typing a long command is slower than on a desktop. Wall thumbnails are for a glance; open one to read it. The credential pool is admin only and may be desktop-first. |
| **Install** | Add to Home Screen on iOS and Android opens `/garden` standalone, with the right icon and bar colours, and every pane has a way back. | No install prompt on iOS (the Share sheet only). The splash colour on iOS (it ignores the manifest's). |
| **Sleep and wake** | After the phone sleeps or the tab is backgrounded, the page reconnects by itself, shows an honest reconnecting or offline state until it has, loses and duplicates no message, and gets unread right. | Offline use: there is no service worker, so a page opened with no network does not load. |
| **Notifications** | Nothing yet. | Everything: no web push. Native push is Lands 4.7c [[gd-XT4PVDe94D]], deferred; M3.3 [[gd-7quH53qSkN]] points at it. |
| **Data** | Nothing beyond the bar above. | Cold-load, repeat-visit and idle-network budgets belong to the UX performance pass [[gd-MEGB3dEaHu]]; M3.4 [[gd-cRvM4ip7kB]] re-measures on 4G. |

### Touch on the World: the free camera

Since the free camera (cursor-grid spec revision 3, [[gd-w37mF7SvFU]]) the map
is moved by hand, as any map is:

| Gesture | What it does |
|---|---|
| One finger | Pans the map. Past 5 px it is no tap. |
| Two fingers | Zoom about their midpoint, the point under it kept, and pan with it. A zoom past a level takes the cursor in (into the cell under the midpoint) or out with it. |
| Tap | Moves the cursor to the cell (or jumps to a Thing outside its grid); a second tap on the cursor's cell goes in, and on a step opens it. The camera stays. |
| Double tap | Elsewhere than the cursor's cell, a zoom step in about the point. |
| In and Out | The camera column's top two buttons, 44 px targets clear of the tab bar and the title bar: in or out one depth, zooming a step. The title bar on the tab bar names the cursor when it is off screen, and a tap on its last part frames it. |
| Long press on a cell | Nothing: a phone has no sibling jump. A tap on a Thing outside the cursor's grid is a jump to it, and Go jumps anywhere. |

The board is `touch-action: none`, so none of this scrolls or zooms the page.
`TestPinchZoomUnderChromium` (`touch_pinch_browser.js`) and
`TestCursorPointerUnderChromium` (`pointer_browser.js`) drive them.

### No pad

A phone has no on-screen pad ([[gd-wAN7P4Armw]], the owner in thread gd-NUuhwOU1l3).
Its one row of arrows, Jump, Out and In (60 px under the map, [[gd-UhoWZuCg7n]]) moved
a cursor that the camera has not followed since the free camera
([[gd-w37mF7SvFU]]). The map now takes that row and runs down to the tab bar. The
gestures above and the camera column's In and Out cover everything the pad did.

### The keyboard on a phone

M1.3 [[gd-xWWqgcmzXM]], checked by emulation (`TestKeyboardUnderChromium`,
`testdata/keyboard_browser.js`: a 390 × 844 touch phone whose visual viewport
is shrunk and panned as a keyboard does). Real iOS is the M4.3 walk's
[[gd-YJPqjmLb47]].

- **The composer stays above the keyboard.** The Commons sheet is sized to
  the visual viewport, so the field and Send sit on top of the keyboard, also
  when iOS pans the page to the field. With the keyboard down the sheet stops
  at the tab bar again.
- **The thread keeps its place.** A reader at the newest message stays at the
  newest as the keyboard opens and closes; a reader scrolled up keeps the same
  message at the same height.
- **Enter is a new line on a phone; Send sends.** The return key says so
  (`enterkeyhint="enter"` under a touch pointer). With a mouse and keyboard,
  Enter sends and Shift+Enter is a new line, and the hint says send. The
  Commons v2 workbench does the same.
- **No zoom on focus.** Every field is 16 px under a touch pointer (§2).
- **The stream never takes the field.** A reply that arrives, or a refetch of
  the thread, leaves the focus, the words and the caret where they were.
- **A lightbox stays above it too.** Other, Escalate and People are centred in
  the visual viewport; when the keyboard comes up, the field in focus and the
  buttons under it scroll into the lightbox's view. Other opens on its field,
  not on the evidence pictures above it.

- **The bead view stays above it too** ([[gd-KomHHeYfpT]]). With the
  keyboard up the panel ends at the keyboard, and the field in focus
  scrolls into the panel's view with its form's buttons when the form fits:
  a decision's Other answer and Send answer, the comment box and Comment,
  and the Glass's nudge box and Send the nudge. The edit form is taller than
  the panel over a keyboard, so the field in focus shows and Save is a scroll
  away. With the keyboard down the panel runs to the tab bar again. The
  same `visualViewport` listener moves it (`commons.js` `viewport()`); real
  iOS, where the panel is fixed and the keyboard real, is the M4.3 walk's
  [[gd-YJPqjmLb47]].

Not covered here: the Glass's terminal input (xterm's own field, which the
key row and the terminal's own scrolling serve) and the Gate's forms (M2.4)
keep the 16 px rule but are not moved above the keyboard by script; iOS
scrolls a focused field into view by itself, which the M4.3 walk checks.

**The Gate's forms** (M2.4 [[gd-KiRStJd56I]]) need no script: the apply
form sends a refusal about one field back with the message at that field,
between its hint and the field, and marks the field invalid, so it is in view
just above the field when iOS scrolls the field above the keyboard. The alert
at the top of the page says the same and links down to it. A refusal about
no field (an open application, the daily limit) stays at the top, where the
page opens. `TestLandsGoldens` shoots the form so refused (`landsapplyerror`,
the field in focus) and fails a Gate field under 16 px (probes.js
`zoomField`).

### The Glass on a phone: a terminal, not a watch mode

M2.3 [[gd-2yJFWJnO1I]] decided this on emulation evidence (M0.2's real phone is deferred to M4.3): **on a phone the
Glass is a terminal you can type into**, not a read-only watch mode. Watching is still where it starts: the Glass is
look only until the viewer taps the terminal, and nudge and the prompt's answers stay on the session's card for
anyone who should not take the keyboard. Why a terminal: attach already worked on a phone (a tap claims the keyboard
and opens the phone's own, `glasskey.js`), the key row is pinned under the terminal in portrait and landscape
(gd-dDsLGuIAGI), and the terminal starts readable (gd-bR8ZcOmzM5). What a phone keyboard lacked was the gap, and
that is what this step added:

| Need | On a phone |
|---|---|
| Esc, Tab, the arrows | Keys on the row under the terminal, shown on a touch screen only (`pointer: coarse`). A tap sends the key and keeps the focus in the terminal, so the phone's keyboard stays up. Esc asks first, as it does from a desktop keyboard. |
| Ctrl chords | Ctrl on the row, held for the next letter typed: Ctrl then c sends Ctrl-C (which asks first). |
| Paste | Paste on the row reads the clipboard (the browser may ask) and types it, a line break as Enter. Without clipboard access it says so. |
| Font size | A pinch on the terminal sizes its glyphs, 6 to 32 px, at Actual size, and does not zoom the page; the size toggle puts them back to 13 px, fitted or actual. The PTY's geometry never changes, so nothing rewraps for other watchers; Fit width is the one key that resizes the PTY. |
| Landscape | The terminal and the key row (or a key's question) share the screen; the row scrolls sideways in portrait. |
| Letting go | Let go on the row (Esc Esc needs a hardware keyboard), a minute away, or closing the Glass. |

Checked by `TestGlassPhoneUnderChromium` (`testdata/glass_phone_browser.js`): an iPhone 12 taps in, sends the keys,
Ctrl-C and a paste, pinches, and a desktop shows none of the touch keys. M4.1 grades it with the rest; the phone's
real keyboard, Safari's clipboard prompt and a real pinch are for the M4.3 walk.

Not covered by this matrix yet: page zoom and large system text, a screen
reader on the phone (VoiceOver, TalkBack), and right-to-left layout. A bug
in one of them is still a bug, filed under Lands, not under this plan.

---

## 4. How each surface is tested

All emulation is **Chromium**, wearing an iPhone's or a Galaxy's size, user
agent and touch. It catches layout, overflow, target size and contrast. It
does not catch WebKit's own rendering, Safari's bars moving, the real keyboard,
Add to Home Screen or Face ID, which only a real phone shows (the device lab,
below; the owner's walk is M4.3).

| Surface | CI today (on a PR that touches it) | Sweep (by hand, `commons-ux-acceptance`) | Real phone |
|---|---|---|---|
| World | `/garden` goldens at 390 × 844 portrait, light and dark, and 750 × 340 landscape, light (`golden_browser_test.go`, ci.yml `gardenhub-browser`), each phone shot held to the C3 probes; inside a plot the sideways map is framed no further out than the Plan depth ([[gd-e83LE907oK]]). The touch scroll and pinch harnesses (`testdata/touch_scroll_browser.js`, `touch_pinch_browser.js`) | `make commons-ux-shoot CELLS=sweep`: `ios390`, `ios390land`, `and360`, `and360land`, light and dark | M4.3 |
| Commons | `/garden` goldens at 390 portrait and 750 landscape (commons, commonsthread and the card faces), C3 probes; `commons_browser.js` | Sweep cells, as World | M4.3 |
| Workbench | `/garden` goldens at 390 portrait and 750 landscape (bead, queues, plan, project, costs), C3 probes | Sweep cells, as World | M4.3 |
| Glass | `/garden` goldens at 390 portrait and 750 landscape (glass, glassattach, wall), C3 probes; `glass_phone_browser.js` (touch keys, Ctrl, Paste, pinch) | Sweep cells, as World | M4.3 |
| The Gate | `TestLandsGoldens` at 390 × 844 portrait and 750 × 340 landscape, light and dark, C3 probes on both (`lands_golden_browser.js`) | `LANDS_SWEEP=1 LANDS_GOLDEN_FULL=<dir>` on `TestLandsGoldens` | M4.3 |
| Sign-in | `keycloak-theme.yml` on theme changes: the `mobile` project (360 × 740 portrait) and the sweep projects `ios390`, `ios390land`, `and360land` (passkey, TOTP and verify-email screens at 360 portrait only) | The same job, run by hand | M4.3 |
| Install | Manifest, icons and the iOS meta by unit test (`page_test.go`, `install.go`) | None | M4.3 (both platforms) |
| Sleep and wake | None (no SSE driver for the rubric's `live` cell yet) | None | M4.3; a harness test in M3.2 |

### The phone goldens and C3 in CI (M4.1)

M4.1 [[gd-T3JPWCD9xM]] added the phone turned sideways to the golden gates and
made rubric C3 a check on every PR that runs them:

- **Cells.** Portrait stays 390 × 844 in light and dark (the cell every phone
  golden was already blessed in; Safari's 390 × 664 is the sweep's `ios390`,
  and the narrower height moves nothing C3 checks). Landscape is **750 × 340,
  light only** on `/garden` (`phoneland`): one theme per added orientation is
  what keeps the sweep inside its 30 minute `-timeout`, and the probes judge
  layout, which a theme does not change. The Gate, ten pages, is shot sideways
  in both themes.
- **C3.** Every phone and landscape shot runs `probes.js` under touch and fails
  on a control under 44 × 44 or a box scrolling sideways that is not built to
  (`GOLDENS.md`, "C3 on every phone shot"). A filed bug can hold a
  `goldenC3Known` row until it is fixed; none is open.
- **Required.** The goldens run in ci.yml's `gardenhub-browser` job, which the
  `ci` check needs, on every ready PR that touches `internal/gardenhub/**`
  (`web/` included), the golden harness or the Gate's pages. No new job.
- **Upright rows.** A row that cannot be shot sideways until a bug is fixed
  is marked `upright: '<bead>'` in `golden_browser.js`. The World rows that
  need the Plan or Bead depth are shot sideways since [[gd-e83LE907oK]]: inside
  a plot the ~140 px landscape map frames a window of the plan's phases at the
  Plan depth, and the header growing (the purse loading) no longer refits a
  camera `GARDEN.look` set. A gardener's card keeps inside that map, under
  the header ([[gd-LWjSUInmGI]]), so `wallcardtap` is shot sideways too.

**Where this goes next:** M4.3 [[gd-YJPqjmLb47]] is the owner using Lands
from an iPhone for a day, which also carries the deferred M0.2 walk. Until then
M1.1, M1.3, M2.3 and M3.1 rest on emulation evidence only.

### Device lab (M4.2)

Two ways to look at a change on a phone. Emulation runs in the harness, on a
pod, at any time. A real phone runs over the public edge, and only a human can
do it: no agent has a phone.

#### Emulation in the harness

| Surface | Command | Cells |
|---|---|---|
| World, Commons, Workbench, Glass | `make commons-ux-shoot OUT=<dir> SURFACES=<a,b> CELLS=sweep` | `ios390`, `ios390land`, `and360`, `and360land`, light and dark |
| The Gate | `cd gardenio/controller && GARDENHUB_GOLDENS=1 LANDS_SWEEP=1 LANDS_GOLDEN_FULL=<dir> go test ./cmd/beads-viewer/ -run '^TestLandsGoldens$' -count=1` | The same four, light and dark, in `<dir>` (first viewport and full page), with `sweep.json` (sideways scrolls and small targets) |
| Sign-in | `keycloak-theme.yml` on a PR that changes the theme. By hand: `./run.sh -- tests/screens.spec.js --project=ios390-light` in `gardenio/tests/keycloak-theme` (needs Docker, so not on a pod) | `ios390`, `ios390land`, `and360land`, light and dark |

`ios390` is Playwright's iPhone 12 (Safari's user agent, 390 × 664, the
viewport left under Safari's bars) and `and360` its Galaxy S8 (Android
Chrome's, 360 × 740); `land` is the same phone sideways. To check a change:

1. Shoot the surfaces you touched in the sweep cells. Name them: every surface
   takes about 45 minutes. A `<file>-error.png` means the shot gave up, often
   because a control is off screen.
2. Read every PNG and grade it with `commons-ux-rubric.md`, landscape included.
3. Attach before and after PNGs to the bead or PR (`commons-ux-acceptance`,
   step 5).
4. If the change is about WebKit, the keyboard, install, sleep or a passkey,
   say in the PR that emulation cannot show it, and walk it on a real phone
   (below) or add it to the M4.3 walk.

#### A real phone over the public edge

Use the phone's own connection (mobile data or wifi). Note the phone model,
the OS version and the browser before you start.

- **Where.** `https://gardenio.clonk.tv/garden` is the World, and the way into
  Commons, Workbench and Glass. The Gate is `https://gardenio.clonk.tv/lands/`
  and needs no sign-in.
- **Signing in.** `/garden` sends you to Keycloak at `auth.clonk.tv`. Pick the
  phone's passkey and approve it with Face ID, Touch ID or a fingerprint. With
  no passkey on this phone yet, sign in with email and password (an admin is
  then asked for a passkey too), then enrol one at
  `https://auth.clonk.tv/auth/realms/gardenio/account/` under **Signing in**
  ([`keycloak-passkeys-runbook.md`](keycloak-passkeys-runbook.md) §1). There
  is no authenticator code: TOTP was retired on 2026-10-31 ([[gd-GiZfavru5G]]).

The walk, about 15 minutes. Take a screenshot of anything that looks wrong,
and one of each surface either way.

| Surface | Do | Look for |
|---|---|---|
| Sign-in | The sign-in above | The pages fit. The passkey prompt comes up by itself. A failed passkey offers the password. |
| World | Open `/garden`. Pan, pinch, tap a plot, open the Menu | Time to a usable map. The tab bar clear of Safari's toolbar. Nothing jumps when the address bar shrinks or grows. A pinch zooms the map, not the page. |
| Commons | Open a thread with a card, tap the card, tap the composer and type a line, open a decision | The keyboard covers neither the box nor Send. The thread keeps its place. Every option of the decision can be read. |
| Workbench | Open a bead from a card or search, scroll it, close it | You land back where you were. |
| Glass | **Watch live** on a working agent, pinch the terminal, Close | Readable at phone width, updates live, Close goes back. Try the key row and Paste only on a session you may type into, and never send Ctrl-C to a working agent. |
| The Gate | Open `/lands/`, tap into a form field | No zoom on focus. An error shows above the keyboard. |
| Landscape | Turn the phone sideways once on the World and in Commons | Only what is worse than cramped: content you cannot reach at all ([[gd-p0DsdxVxJU]]). |
| Install | iOS: Share, **Add to Home Screen**. Android: Chrome's menu, **Install app**. Open it from the icon | Full screen, the right icon and bar colours, still signed in, a way back from every pane. |
| Sleep and wake | Leave Commons open, lock the phone for a minute, unlock | An honest reconnecting state, then caught up. No message lost or doubled, unread right. |

**Naming.** Call each screenshot after the sweep cell it matches, so it sits
beside the emulation shot of the same surface: `real-<surface>-<cell>-<theme>.png`,
for example `real-commons-ios390-dark.png` or `real-world-and360land-light.png`.
The cell is the platform and the orientation, not the exact width: any iPhone is
`ios390` or `ios390land`, any Android phone `and360` or `and360land`.

**Where to attach.** A real phone's shots show real fleet data, so they stay in
signed-in places: never a public link, and crop out a token or a password.

- From the phone: post them in the Commons thread of the bead or the check-in,
  with a line per shot (OK, or what is wrong). An agent fetches them with
  `gb commons file <message-id>` and reads each one.
- From a laptop or a pod: `gb image upload <png> --json --jq .url` per file,
  then `kd comment add <bead>` with the viewer URLs (`commons-ux-acceptance`,
  step 5).
- On a PR: the findings as text, with the viewer URLs. The PNGs do not render
  there.
- Each problem is its own bug (`kd create --type=bug -l project:gardenio`),
  with its shot, the phone model, OS and browser, linked to the step or to the
  plan [[gd-U4BtfWKjvc]].
