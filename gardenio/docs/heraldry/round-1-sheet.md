# Heraldry round 1: the sheet

> **History.** This records the first form of arms: one to five emoji placed like the five on a
> die on a shield of one tincture. The owner's verdict of 2026-10-26 20:45Z (thread gd-lvbRHMaXID)
> replaced it with three to five emoji in a row, shown the same way everywhere. Kept as history,
> not guidance: for the current form see [`README.md`](README.md) and [`rubric.md`](rubric.md).

Phase 0 step 0.3 (gd-NF8PUEH9K4) of the heraldry plan gd-N6I3Oa7uDg. Spec:
[`2026-10-25-heraldry-design.md`](../superpowers/specs/2026-10-25-heraldry-design.md) §10 step 2.
This sheet shows step 0.2's 47 proposals ([`round-1.md`](round-1.md)) in every form the spec asks for,
so the owner can say GO, iterate, or stop. Nothing here is product code. Every picture comes from the
throwaway prototype [`scripts/heraldry-proto`](../../../scripts/heraldry-proto/) (`go run . round1`), and
the browser shots come from its `shots/round1-shots.js`, run on the golden harness's fixture farm.

All the files are in [`samples/round-1/sheet/`](samples/round-1/sheet/).

## What is on the sheet

| Form (spec §10.2) | Files | How it was made |
|---|---|---|
| The picture at 64, 40, 24, 16 and 8 px, smooth and quantised, day and night, for all 47 beads | `round1-pictures-1-plans.png`, `-2-phases-tasks.png`, `-3-decisions-agents.png` | the prototype; each row also has the 16 px and three 8 px rules magnified 4× |
| The 8 px picture under two cell rules, beside the scaled drawing | the same rows: "8 scaled, 2×2 dominant, 2×2 sampled" | the prototype (`Render8`) |
| The same arms on four shield shapes (Q11) | `round1-shapes.png` | the prototype (`shapes.go`); three beads at 64 and 16 px, 16 px at 3×, 8 px quantised at 4× |
| The same arms in three artwork sets (Q10) | `round1-artwork-sets.png` | Noto `v2026-09-24-unicode18_0`, Twemoji 17.0.3, OpenMoji 15.1.0; three beads |
| `kd show`'s terminal block, `kd list`'s ARMS column, a `gb news` line | `terminal-light.png`, `terminal.txt` | a page in Noto Sans Mono with Noto Color Emoji, each emoji in two cells as a terminal draws it |
| A Slack agent card with the picture, and prose with the References footer | `slack-mock-*.png`, `slack-*.json` | a **mock** drawn from the Block Kit payloads; see "Slack" below |
| The picture at the plot gate (24), the phase stake (16) and the task cell (8), day and night, on the fixture farm | `farm-*.png` | real Chromium on `/garden`, golden fixture farm, pictures laid over the canvas at the world positions §6.3 names |
| The Workbench bead view header and the Choose arms picker | `workbench-*.png` | DOM put into the real bead view (`/garden?bead=gd-planA`), styled with its own tokens |
| The library: every charge at 16 px, and what the 8 px test drops | `round1-library-16.png`, `round1-library-dropped.png`, `round1-uniqueness.md` | the prototype; dropped charges are underlined red on the contact sheet |

The fixture farm's plans, phases and tasks are not the 47 real beads. The farm shots dress them in
round 1's arms in order: plans get plan arms, phase beds get the six Track A phases' arms, and cells get
the task and bug arms. The Workbench mock puts "Replace ziti with Keycloak"'s arms on the fixture plan
"Edge IdP rollout".

## Findings

### 1. The 8 px uniqueness test drops 44% of the library under the scaled drawing

Spec §6.6 says a charge is dropped from the library when its 8 px quantised tile cannot be told from
another charge's. The prototype ran that test: each charge alone in each of the five places, by day and
by night, on vert and on azure. A charge was dropped when its picture matched, pixel for pixel, an empty
place or a charge already kept.

- **Scaled drawing:** 751 kept and **582 dropped**. Of the dropped, 70 look like an empty place and 512
  look like another charge. **40 of the 111 charges round 1 uses would be dropped**, among them 🛡️ (the
  Heraldry plan's own centre), 🐐, 🎲, 🏛️, 🔨 and 🧾. At 8 px a corner charge is two or three pixels,
  because the outline bites into the corner places (`round1-library-dropped.png`).
- **Five fixed 2×2 cells** (0.1's suggestion) do not make single charges unique either. With one colour
  per cell, the 1,333 charges make 72 distinct cells. With the charge resampled to 2×2, they make 737.
- **Whole arms stay distinct.** All 47 round-1 arms have different 8 px pictures, by day and by night,
  under all three rules.

So "no two library charges share an 8 px tile" cannot hold without cutting the library almost in half.
"No two arms in a register share an 8 px picture" holds today, and a register check could keep it true.
**Question for the owner (new, Q12):** (a) cut the library as §6.6 says (to ~750 charges, losing 🛡️ and
🐐); (b) drop §6.6's cut, so 8 px is a likeness and the cell's hot target carries the name; or (c) (b)
plus a register warning when new arms' 8 px picture matches existing arms. The recommendation is **(c)**,
with the **2×2 dominant-colour rule** at 8 px, because its five colour patches keep the die layout
visible where the scaled drawing shows two or three specks.

### 2. At 16 px and up the picture reads; 24 px is the smallest size that shows charges

These match spec §6.3. At 64 and 40 px every charge is legible. At 24 px they are shapes (the goat, the
key, the window). At 16 px they are coloured marks on a shield. The quantised 24 and 16 px pictures keep
the shapes, with banding on gradients. By night the vert field becomes art.js's dark teal. The charges
keep their day colours, so they stand out more by night than by day.

### 3. Shield shape (Q11): the heater still wins

- **Heater:** all five places fit, and the point gives the shield a top and a bottom at 16 px.
- **Round:** the places have to shrink to 22 units and move in, so five charges crowd, and the rim clips
  the corners.
- **Square with a point:** roomier at the bottom, but at 16 px it reads as a badge, not a shield.
- **Banner:** the three tails cut into the bottom places. At 8 px the tails are a notched line.

### 4. Artwork set (Q10): Noto is still the recommendation, and Twemoji is a real alternative

Twemoji's flat, saturated shapes hold up best at 16 and 8 px: its goat stays a white goat on green.
OpenMoji's thin black outlines go muddy below 24 px. Noto sits between the two, matches the Noto fonts
the goldens pin, and its images are Apache 2.0. Twemoji (CC BY 4.0) would need an attribution line in
the UI.

### 5. Slack: a mock, because the test workspace does not exist yet

The `slack-ux-acceptance` loop (post to the test workspace, shoot it with `gb slack view`) cannot run:
the test workspace, app and user session are not provisioned (escalation gd-fWeCNSRdCd, in progress),
and this pod has none of `SLACK_UX_BOT_TOKEN`, `SLACK_UX_CHANNEL`, `SLACK_TEAM_ID` or
`SLACK_VIEW_STORAGE_STATE`. So the four payloads were written to match the bridge's builders
(`buildAgentCardBlocks`, the hydrator's `footerLine`) and linted with the skill's
`lint-blocks.mjs` (all four pass with no errors or warnings). They were then drawn on a mock page that
says MOCK across the top. **U1-U12 are ungraded.** Slack draws its own emoji, not Noto's.

- **A. Picture as the header section's accessory.** Slack shows an accessory at about 88 px, so a 40 px
  upload is scaled up and soft. Upload 128 px instead of 40 px (spec §6.4 names 40).
- **B. Picture as a context image beside the short title.** Slack shows context images at about 20 px,
  close to the 16 px row size. It is small, but it sits next to the name the way a favicon does.
- **C/D. Prose and the References footer, today and with arms (Q5).** The emoji run in place of the type
  glyph works in the sentence. In the footer, the status glyph comes straight after the run, so it reads
  as a fourth charge (🐘📍🕓 🫘). A separator or dropping the status glyph when arms show would fix it.

### 6. Terminal

The three-line block holds the die shape. Two charges on opposite corners leave an empty middle line,
which is honest but looks like a gap. Charges with VS16 (🗝️, 🛡️) are two cells here, but some terminals
give them one, which breaks the alignment. Spec §3.2's grapheme-width padding has to treat VS16 as two
cells.

### 7. The farm

At the Plan level (camera zoom 1.5 on a 1280×800 desktop) the 16 px stake picture is a clear little
shield beside the phase number, and the 8 px cell picture is a visible shield in the cell's corner
(`farm-*-crop@3x.png`). Done cells take bottom-left (Q6); five fixture cells are done. At the Project
level (zoom 0.31) the 24 px gate picture is about 7 screen pixels, a coloured fleck at the plot's
bottom-left. On a phone (zoom 0.52) the cell pictures are 4 px. The overlay is a mock and sits above the
canvas, so it covers gardeners standing on cells; art.js would draw the cell picture under them.

### 8. The Workbench

The header carries the picture at 32 px beside the kind and title. The Heraldry section shows it at
64 px with the plain-words name (spec §8). The picker draws the five places as slots over a 128 px
picture. A search box filters the library by name, and charges already on arms in gardenio are marked.
Collisions are stated in words, because `/garden` shows no text emoji. It fits at 390 px.

## What changed in the prototype

`render.go` takes a `Shape` (the heater is unchanged and is still the only geometry in `shield.go`);
`shapes.go` adds Q11's three other outlines; `sets.go` fetches Twemoji and OpenMoji PNGs (pinned tags,
nothing vendored) and reads `library.md`; `round1.go` draws the sheets, the two 8 px cell rules and
the uniqueness test; `shots/round1-shots.js` makes the browser shots. See the prototype's README.
