# The gardenio splash: what was built

Plan gd-EJGA3zDGCk, asked in thread gd-Mdz1gsnubn. It shipped in release 2026.301.3, live 20:35Z on 2026-10-28.

The splash is a demo page at `/garden/splash` inside beads-viewer. It is a 3D scene in WebGL with a perspective camera. The
camera lies low on the near slope of a hill and looks over its crest. On the brow stands a weathered stone with the gardenio
mark cut into its face. Beyond it a farmed valley opens. The sun and the moon light it from the page's clock. This file pins
what each part is, so the owner's next round of notes can name what to change. The code is the source of truth: each module
opens with a comment that says the same things in more detail.

| Part | Where |
|---|---|
| Page, time controls, boot | `gardenio/controller/internal/gardenhub/web/splash.js`, `splash.html`, `splash.css` |
| Server | `gardenio/controller/internal/gardenhub/splash.go` |
| Almanac | `web/almanac.js` |
| Land, stone, flora, farm, light, tiers | `web/splash/{land,stone,flora,farm,light,quality}.js` |
| Zoom and look (pure maths) | `web/splash/view.js` |
| three.js | `web/vendor/three.module.min.js`, r186 pinned and hashed (`web/vendor/VENDOR.md`, `make vendor-three`) |
| Goldens | `testdata/golden/splash*.png`, shot by `testdata/golden_browser.js` |

## Rules the whole scene keeps

- **Nothing is fetched at run time.** The page is one response with the scripts and CSS inlined. It loads only two embedded
  files, each cached for a year under a `?v=` hash: three.js and the still. Every texture is drawn in code.
- **Deterministic layout.** The land, stone, flora and farm place everything using only `+ - * /`, `Math.floor`,
  `Math.sqrt` and `Math.imul`, so the scene is the same in every engine. Node tests pin each layout's hash.
- **Never a blank canvas.** Without WebGL, or when three.js fails to load, the page shows a still (`web/splash-fallback.png`,
  the noon golden) and a one-line note.
- **Public.** beads-viewer serves the page without a session (`routePublic`, gd-hzRHm1KB5r). The edge admits it with no
  session once the hand-applied manifests are in (gd-NCnYL9WIdj).

## Land (step 3)

- A heightfield of 257 × 257 over 400 × 400 m, plus a coarse ring out to 1 km for the far hills.
- The camera stands 1.4 m up on the south slope at z = 170, looking north. The crest is 20 m ahead and dips 0.8 m below the
  eye. A knoll stands right of centre. Below lies a valley with a river, and beyond it strip fields climb to two wooded ridges.
- The crest crosses the frame a third of the way up. On a portrait screen the field of view widens from 50° up to 80°, so the
  stone and the oak stay in frame.
- The field map and the splat (grass, earth, chalk, gravel) are painted on two 1024 px canvases. Height fog in the scene's fog
  colour sinks the far ridges into haze.
- The default place is latitude 45°. Longitude comes from the browser's timezone. `?lat=` and `?lon=` override both.

## Stone (step 4)

- A quarried block 1.1 × 1.6 × 0.9 m with worn, chipped arrises and noise-displaced faces. It stands on a dry-stone plinth
  on the brow, 8 m ahead and right of centre, turned so its face meets the camera at three-quarters.
- The mark is the sprout over the wordmark from `keycloak-theme/.../logo.svg`, 70% of the face's width, cut 2 cm deep.
  - The cut is a real displacement, so it throws shadow on its floor.
  - A normal map gives the chisel's bright rim.
  - An AO-roughness map leaves the cut floor duller than the polished face.
- On a 390 px phone the wordmark is about 80 px wide, so it can be read.
- Eight gems are set round the mark in four tinctures, each with its own index of refraction: garnet (two, top and
  bottom), emerald, topaz and sapphire (a pair each).
- The weathering is vertex colour: rain streaks, lichen on the faces turned from the sun, and moss at the foot.

## Flora (step 5)

- **Meadow:** instanced grass bent by a wind field in the vertex shader. Wildflowers grow among it as crossed quads:
  poppy, cornflower, daisy and buttercup. They are thickest along the path to the stone.
- **Hedges and trees:** a hedgerow with a great oak runs up the lane on the left, its boughs closing the top of the frame.
  More hedgerows line the valley's furlongs. Poplars stand along the river and apple trees in the orchard's rows.
- **Far woods and birds:** the far woods are billboard impostors in the haze, and a dozen rooks circle over them.
- Nothing is planted where the near hill hides it from the eye.

## Farm (step 6)

- **Buildings:** a stone farmhouse with a slate roof, dormers and two chimneys, and a timber barn on a plinth with a
  hayloft and a lamp.
- **Yard and lane:** dry-stone walls, a gate, a well, a cart, and a footbridge by the ford.
- **Fields:** round bales in the cut hay strips, and cattle and sheep grazing in the fenced pasture.
- **Pond:** it reflects the scene. That is a planar mirror at the high tier, a sky-tinted colour at mid, and plain water at low.
- **The farm keeps time:**

  | What | When |
  |---|---|
  | Windows and barn lamp lit | civil dusk to civil dawn |
  | Chimneys smoke | from an hour before sunset to sunrise |
  | Swifts fly | by day |
  | Bats fly | from dusk |

## Almanac and lighting (steps 2 and 7)

- **Almanac:**
  - Sun: low-precision solar coordinates, good to about 0.01°.
  - Moon: its age comes from the synodic month, good to about half a day. It is placed opposite the sun by the phase
    angle, ignoring its 5° tilt.
  - The almanac also gives the sky palette: zenith, horizon, ground light and fog, keyed to the sun's elevation.
- **One light at a time.** The sun lights the scene by day and the moon by night. The colour turns from gold to blue-silver
  through civil twilight, with nothing popping at the hand-over. At night the light's strength is the moon's lit fraction,
  so a full moon throws clear shadows and a new moon leaves the stars, the windows and the gems.
- **Shadows** are soft PCF in two cascades:
  - The near cascade is tight round the stone, so the chisel cuts shadow.
  - The far cascade covers the hill, the oak and the valley down to the farm.
- **Sky:**
  - The dome shader draws a warm glow round a low sun, the sun's disc with a halo, and the moon in its true phase with
    its lit limb towards the sun.
  - Seeded stars turn with sidereal time and fade in through civil twilight.
  - Fireflies drift over the meadow from dusk until full night.
- **Exposure** follows the twilight, so the night is dark but readable.

## Time and controls (step 8)

- The scene follows the real clock. The light moves every second, and the shadow map is redrawn only when the light has
  moved 0.25°.
- **Scrubber:** a thin bar along the bottom spans the local day. Arrow keys move it 10 minutes, or an hour with Shift.
  - Play runs a day in a minute; Now returns to the clock.
  - The readout says the time, the twilight, the sun's height and the moon's phase.
- **Address:**
  - `?t=` pins the time (unix ms or RFC 3339); the scrubber writes it back, so a frame can be shared as a link.
  - `?play=1` plays the day.
- The camera breathes a few centimetres, except under reduced motion and under `?t=`.
- Under reduced motion nothing moves, and the scene is redrawn once a minute.
- Drawing pauses while the page is hidden or the canvas is off screen. On `pagehide` the page frees all of its GPU memory.
- **Start menu** (gd-Fw6fffLZ3p): plain HTML over the scene, so it shows with or without WebGL. It holds the title and two
  links: **Request an invite** (`/lands/`) and **Sign in** (the edge's `/oauth2/start`, back to `/garden`).

## Look and zoom (plan gd-I5QHVfGLbD)

A visitor can zoom into the scene and turn to look round it. The view is three numbers, `{zoom, yaw, pitch}`, and the
default `{1, 0, 0}` is the designed frame. The maths is `web/splash/view.js` (pure, tested under node); the gestures,
buttons and keys are in `splash.js`.

- **Why zoom is a field-of-view change, not a walk forward.** Zooming narrows the camera's vertical field of view: the
  tangent of its half-angle is divided by the zoom, so the middle of the frame looks that many times bigger. The eye never
  moves. So the land, the stone, the shadow cascades, the fog and the pond's mirror stay exactly as built, and nothing new
  has to be modelled for a closer eye. A walk forward would need the scene to hold up close, and the camera would pass
  through the grass. The camera's breath sways the aim a zoom-th as far, so a zoomed frame does not swim.
- **Gestures** (Pointer Events; the canvas is `touch-action: none`):
  - **Wheel** zooms toward the point under the pointer and keeps that point under it: a 100 px notch is about 1.22×. A ctrl
    wheel (a trackpad pinch) zooms 5 times as hard.
  - **Drag** with the mouse or one finger turns the view so the ground follows the finger: a drag the frame's height turns
    it by about the field of view on screen.
  - **Two fingers** zoom by their change of distance toward their midpoint, and drag by its travel.
  - **Double click or double tap** (two taps within 300 ms and 24 px) zooms 2× toward the point. At the most zoom it goes
    back to the default.
  - Each change eases over 150 ms; under reduced motion it lands at once.
- **Buttons** (`#splash-view`, a column in the bottom-right corner above Play and Now, 44 px under a coarse pointer):
  - **Zoom in** and **Zoom out** step the zoom 1.5× toward the frame's middle.
  - **Reset view** shows only off the default view and goes back to it. Its mark is an SVG mask, not ↺, which no pinned
    golden font has.
  - The readout gains a line, `zoom 4×`, while zoomed.
- **Keys:** the canvas takes no focus, so the keyboard reaches the view through its buttons. On any of them, the arrows turn
  the view as a drag of a tenth of the frame would (a third with Shift), ArrowLeft looking left; Escape resets.
- **Bounds:** zoom 1 to 8; yaw 60° either way; pitch 25° down to 40° up. They keep the view off the unbuilt slope behind
  the camera and out from under the hill. A zoom toward a point past a bound holds the bound and lets the point slide.
- **Address:** `?z=<zoom>&look=<yaw>,<pitch>`, each to two decimals, held to the bounds; the default writes neither.
  - Yaw is degrees right and pitch degrees up of the designed aim.
  - The designed aim depends on the screen (a phone upright has a wider field of view and is pitched about 7° higher), so
    one address frames a phone and a desktop a little differently.
  - Gestures write the address when the pointer lifts and 200 ms after a wheel burst; the buttons and keys write it at once.
  - With `?t=`, a shared link pins both the hour and the frame.

## Quality tiers (step 9)

| Tier | Grass blades | Shadows | Pond | Max device px per CSS px | Also |
|---|---|---|---|---|---|
| high | 60,000 | two cascades, 2048 + 1024 px | planar mirror | 2 | |
| mid | 25,000 | one 2048 px cascade | sky-tinted colour | 1.5 | |
| low | 8,000 | one 1024 px cascade | own water | 1 | poplars and orchard as impostors |

- **Picking a tier:**
  - `?q=low|mid|high` picks the tier and holds it.
  - Otherwise the page guesses: a phone gets low, and a laptop is judged by its cores and pixel ratio.
  - The first frames' median can step the guess down.
- **Stepping down:** past that, a watch steps down one tier after 3 s over budget. It never steps up.
- **Budgets:** 33 ms a frame on a phone and 16 ms on a laptop.
- **Logging:**
  - The frame time and the tier are logged to the console once a minute.
  - `?perf=1` shows the same line in the top corner, over the last 10 s (gd-8nCJKznQzj).
- CI and agent pods draw in software (SwiftShader), so the budget is asserted only on hardware GL. The owner measures it by
  hand (decision gd-TxW74UIimS; follow-up gd-k3JDv6A10L).

## Accessibility and goldens (step 10)

- **Semantics:** one `<main>` under a visually hidden `<h1>`. The canvas is `role="img"`, and its name is the almanac's
  words for the hour, kept current as the light moves, for example "Dusk over the farm: the sun low in the west; …".
- **Focus and targets:** controls have a two-tone focus ring, and each is a 44 px target under a coarse pointer.
- **Goldens:**
  - Four scenes at `?q=mid`, light theme, on phone and desktop: noon, dusk, the full-moon midnight and dawn, all on
    26 Oct 2026.
  - One more pair for the no-WebGL still (`splashflat`), in both themes.
  - `splashzoom`: noon at `?z=4&look=5,-8.2`, phone and desktop. It centres the stone's mark on a desktop, under the menu,
    and puts it in the lower middle of a phone.
  - They run in `make test-garden-goldens`.

## Follow-ups

- **The owner's notes from the step 11 check-in.** Each note becomes its own bead under plan gd-EJGA3zDGCk; see that plan's
  children.
- **Frame budget on real hardware** (gd-k3JDv6A10L). The owner reads `?q=low&perf=1` on a phone and `?q=high&perf=1` on a
  laptop. If either is over budget, tune `TIERS` in `quality.js`.
- **Public edge** (gd-NCnYL9WIdj). Until the viewer-authproxy and public-edge manifests are applied, a signed-out visitor
  is sent to sign in.
- **The lift to the public landing page.** `landing.*` serves one nginx HTML from a ConfigMap
  (`helm/gardenio/templates/landing/`), and a ConfigMap is capped at 1 MiB. Two ways to lift it:
  - **Inline it into one file under the cap.** It does not fit as it stands:

    | Piece | Size |
    |---|---|
    | three.js bundle | 742 KB |
    | Scene, CSS and HTML, unminified | 303 KB |
    | Total | 1.045 MB of the 1.049 MB cap |

    That total is before the nginx config and the 553 KB fallback still, which would be about 737 KB as base64. This route
    needs a minified scene, a tree-shaken three.js and a smaller still (or none). Since gd-BqEPTwuyj6 the page inlines the
    scene and CSS minified: 147 KB, 58 KB gzipped.
  - **Point the landing at gardenhub's page.** Redirect or proxy `landing.*` to `/garden/splash`, which already serves it
    publicly with year-long caching. That costs nothing in bytes, but it ties the public front door to beads-viewer being up.

  The choice is its own plan, to be made after the owner agrees the look.
