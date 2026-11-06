# Badge art spec: badges from arms

> **Status:** pinned. This is the contract for the badge of any bead, as plan gd-pQZO9ie11D
> (badges from arms, decision gd-eThga7vg3I) shipped it. It records the shipped system, and
> every rule here is enforced by code and tests under `gardenio/controller/internal/badge/`,
> `gardenio/controller/cmd/gb/plan_badge*.go` and
> `gardenio/controller/cmd/beads-viewer/badge_image*.go`. Change a rule here only together with
> the code and test that enforce it. It replaces the plan-only badge contract of plan
> [ka-0000005980] (narrative [ka-0000005497]) and the separate raster note; the raster rules are
> now [Raster](#raster-png-output) below.

## Arms are the mark, the badge is the picture

A bead's **arms** are its mark: three to five emoji in a row (🏅🖌️🔎), chosen by the agent that
files or holds an epic, and shown as a text run before the short title wherever a bead is named
([`heraldry/README.md`](heraldry/README.md)). Arms are authored; nothing generates them.

A **badge** is a picture drawn from those arms: a self-contained SVG, and a PNG rasterized from
it, for the places a text run cannot go or is too small to remember. It is derived, never
authored, and it never stands in for the arms:

- **The run fills every small slot.** Lists, cards, cmd-k rows, Slack prose and card headers show
  the arms run, not a badge (heraldry spec R9). No badge is drawn at 24 px in the viewer.
- **The badge shows only where there is room:** the bead page and plan detail headers, the cover
  of a deck card, `og:image`, a Slack upload, an app icon.
- **A badge is always drawn from the arms when the bead's line bears any.** A new run draws a
  new picture; the old one is never served for the new arms (see [Caching](#caching)).
- **A bead with no badge shows nothing extra.** A badge is never required for a bead to render.

## The input: the arms and the bead

`gb badge <bead-id>` takes any bead, of any type. It reads the bead (id, title, description)
straight from the daemon, then resolves the arms the picture is drawn from:

1. The bead's own arms, if it bears any.
2. Else the nearest arms in its line: every parent-child edge is walked upward, breadth-first
   (task to phase to plan to project), so a nearer ancestor beats a farther one. Among armed
   ancestors at the same distance an epic wins, so a task filed under both a thread bead and a
   phase takes the phase's arms (gd-NgFx1XzPAL). The walk stops after 32 reads.
3. Else no arms. The picture is drawn from the title and description alone, and `gb badge` says
   so on stderr.

`gb badge` names on stderr the bead the arms came from when they are inherited, and `--json`
reports them as `arms` (the run) and `arms_from` (the bead id). A failed read part way up the line
is reported and treated as no arms, so a badge is still drawn.

**How the arms reach the model.** With arms, the prompt carries the run and each charge's plain
name (`fire, compass, package`), and the charges are the subject of the picture: the model
composes them into one picture, with no emoji glyphs and no letters. The title and description
are only context. With no arms, the model draws the bead's subject from its title and
description.

## Two tiers, one entry point

Generation always succeeds, so a caller never branches on model availability and no surface has
an empty state:

| Tier | Source | Cost | Guarantee |
|---|---|---|---|
| **Tier-0** (`Tier0`) | a pure function of `(bead id, title)` | zero, no network | never fails; byte-identical forever for the same input |
| **Tier-2** (`Generate`) | a model draws the picture from the arms (or the subject) | one model call | the memorable picture; on **any** failure falls back to Tier-0 |

`Generator.Generate` returns the Tier-0 badge with an advisory error for a `nil` generator, an
empty title, an API error, or an SVG that fails sanitization. The `Badge` is always valid.
`gb badge --tier0` forces Tier-0 (zero cost, byte-stable).

The model tier reads its endpoint and key from `BADGE_API_KEY` (or `ANTHROPIC_API_KEY`) and
`--base-url` (default: the pod's `ANTHROPIC_BASE_URL`, the in-cluster LiteLLM), and its model from
`--model` (default: `BADGE_MODEL`, else `badge.DefaultModel`, `claude-opus-5-5`).

## The three styles

`--style` is the art direction the model is asked for. Tier-0 has one look whatever the style.

- **`poster`** *(the `gb badge` default)*: the large, memorable picture (gd-RcmmFKWytW). A rich,
  layered composition of the charges with depth (background, middle ground, foreground; light and
  shadow from overlapping shapes, gradients and opacity), held together by one strong silhouette.
  It is judged at 128 px and above.
- **`line-art-bold`**: one saturated background, a light foreground glyph, thick clean strokes. It
  reads at 24 px, as a distinct sticker.
- **`line-art-subtle`**: a single muted foreground on a very light or transparent background, thin
  strokes. It reads at 24 px and recedes, as quiet wayfinding.

The defaults (`poster`, `claude-opus-5-5`) are the owner's pick from the sample sheet
[`heraldry/badges-round-1.md`](heraldry/badges-round-1.md) (decision gd-zqkw2TUnp7, 2026-10-27).
The badge package's own `Generator` still defaults to `line-art-bold` when no style is given, so
a library caller that never says otherwise gets a picture that reads at 24 px.

### The size contract per style

| Style | Smallest size | Raster `--size` | SVG cap | Reply cap, timeout | Where it shows |
|---|---|---|---|---|---|
| `poster` | 128 px (`PosterMinSize`) | 128 to 512; `gb badge` refuses a smaller `--size` | 24 KB | 32k tokens, 300 s | the bead page and plan detail headers, the deck card cover, `og:image`, Slack uploads, app icons |
| `line-art-bold` | 24 px | 1 to 512 | 8 KB | 16k tokens, 90 s | a slot under 128 px that wants a picture rather than the run |
| `line-art-subtle` | 24 px | 1 to 512 | 8 KB | 16k tokens, 90 s | as `line-art-bold`, where the picture should recede |
| Tier-0 (any style) | 24 px | 1 to 512 | none (generated, ~1 KB) | no model call | wherever a model picture was asked for and none came back |

The viewer draws the uploaded badge at these sizes (`beads-viewer/badge_image.go`, step 9,
gd-5qEMPflr4z):

| Surface | Size | Form |
|---|---|---|
| Bead page header, plan detail header | 64 CSS px (`.badge-64`), left of the arms run | the SVG, else the PNG |
| Deck card cover (`/deck`, `/plans/{id}/card`) | 112 CSS px | the SVG, else the PNG |
| `og:image` on those two pages | the uploaded PNG's size; upload at 512 px | the PNG only, as an absolute URL through the viewer's `/image/` proxy |

A 64 CSS px header is 128 device pixels on a 2× screen. On a 1× screen it is below the size a
poster is judged at; the header still takes the poster, because the arms run beside it carries
the identity.

## Palette

Colours are mid-saturation, deliberately: a board of ~20 beads must read as one system, not a
wall of loud stickers ([ka-0000004332]).

Tier-0 picks a `(background, foreground)` pair by `hash(id)` from this fixed 12-entry palette. The
monogram and ring stroke are drawn in the foreground.

| # | Name | Background | Foreground |
|---|---|---|---|
| 0 | blue | `#2563eb` | `#eff6ff` |
| 1 | emerald | `#059669` | `#ecfdf5` |
| 2 | amber | `#d97706` | `#fffbeb` |
| 3 | red | `#dc2626` | `#fef2f2` |
| 4 | violet | `#7c3aed` | `#f5f3ff` |
| 5 | cyan | `#0891b2` | `#ecfeff` |
| 6 | pink | `#db2777` | `#fdf2f8` |
| 7 | slate | `#4b5563` | `#f9fafb` |
| 8 | lime | `#65a30d` | `#f7fee7` |
| 9 | orange | `#ea580c` | `#fff7ed` |
| 10 | teal | `#0d9488` | `#f0fdfa` |
| 11 | purple | `#9333ea` | `#faf5ff` |

**Contract:**

- One saturated background fill and one light foreground, with high contrast so the glyph
  survives at 24 px.
- Entries are stable and index-addressed by `hash(id)`. Do not reorder them: that would silently
  recolour every Tier-0 badge.
- A Tier-2 badge follows the same loudness discipline through its style; it is not limited to
  these 12 hues.

## Stroke and shape

**Canvas.** Every badge is drawn into a **100×100 `viewBox`** with **no `width`/`height`
attributes**; the consumer scales it with CSS. This shared coordinate system makes the tiers
drop-in swappable, and it is enforced: a Tier-2 SVG without `viewBox="0 0 100 100"` is rejected
and falls back to Tier-0.

**Tier-0 geometry** is geometric so it stays legible:

- **Background silhouette**: one of four shapes picked by `hash(id)`, inset so nothing clips:
  `circle`, `rounded-square`, `hexagon`, `shield`. Shape variety keeps two beads that share a
  monogram apart.
- **Ring**: an optional concentric ring, inset, `stroke-opacity 0.35`, `stroke-width 3`.
- **Monogram**: 1–2 uppercase letters, `font-weight 700`, `font-size 52` (one letter) or `40`
  (two) on the 100-unit canvas, centred. Taken from the initials of the first two significant
  words (stop-words dropped: "Every gardenio plan gets its own icon" gives **GP**); a single word
  gives its first two letters; a title with no letters gives **?**, so a badge is never blank.

**Line-art strokes (`line-art-bold`, `line-art-subtle`):**

- Thick, clean strokes only: no thin lines, no small gaps.
- No text: a model-drawn badge draws the charges (or the subject) as shape and line, never
  letters, words or monograms. That is Tier-0's job.
- At most ~3 distinct elements; more detail turns to mush at 24 px.

## The 24 px legibility rule

This rule binds the two line-art styles and Tier-0. A `poster` is exempt because it is never put
in a slot under 128 px.

A line-art badge must be a single recognizable subject that reads at **24×24 px**. If it turns
into an undifferentiated blob at 24 px, it is not a valid badge, however good it looks at 128 px.
That is how the "metric triad" glyph of the first bake-off failed.

1. **Bold subject, not fine detail.** The line-art prompts require "BOLD and LEGIBLE at 24px — a
   single recognizable subject, not fine detail or text" and forbid thin lines, small gaps and
   more than ~3 elements.
2. **Distinct beads, distinct badges.** Tier-0 keeps two beads apart with independent
   hash-driven dimensions (palette × shape × ring); Tier-2 draws distinct charges. The sibling
   legibility test (ka-0000001220) asserts it.
3. **Byte budget.** A line-art badge is ~1 KB, and the sanitizer rejects one over 8 KB.

## Poster: the large size

The 24 px rule forces a single flat subject, which is right for a list row and wrong for a page
header, an `og:image`, a Slack upload or an app icon, where the owner wants a picture someone
remembers. `--style poster` is that picture.

- **Where it may appear:** 128 px and up. `gb badge --style poster` with `--png`, `--out` or
  `--upload` refuses a `--size` below `PosterMinSize` (128). A smaller slot takes `line-art-bold`
  or `line-art-subtle`.
- **Its own prompts.** The poster prompts (`posterPromptTemplate`, `posterArmsPromptTemplate`,
  pinned by `testdata/prompt-poster-*.golden.txt`) drop the 24 px sentence and the ~3-element cap.
  They ask for a scene with depth and one strong silhouette that still reads at 128 px, and keep
  the picture inside what the pure-Go rasterizer draws: shapes, groups, linear and radial
  gradients and opacity; no `<filter>`, `<mask>`, `<clipPath>`, `<pattern>`, `<text>`, `<use>` or
  CSS. The charges are still the subject; no letters or emoji glyphs.
- **Budget.** The sanitizer cap is 24 KB, the reply cap 32k tokens and the model call 300 s. A
  poster is never inlined into a list or card row.
- **Cached apart.** The style is part of the cache key, so a poster is never served for a
  line-art slot, or the other way round.
- **Sample.** `testdata/poster-sample.svg` is a poster for plan gd-pQZO9ie11D (arms 🏅🖌️🔎);
  `TestRasterize_Golden` rasterizes it at 128 and 512 px.

```sh
gb badge <bead-id> --style poster --png --size 512 --out x.png
```

## Sanitization (the Tier-2 gate)

A model returning something is not a badge we trust. `sanitizeSVG` is the gate, and every failed
check drops the caller back to Tier-0:

- Take the **first** `<svg>…</svg>` element (models sometimes wrap it in prose or ` ```svg `
  fences).
- **Reject** `<script>`, `<foreignObject>`, `<image>`, `on…=` event handlers, and any `http(s)`
  `href`, `xlink:href` or `url()` reference. A badge never touches the network when drawn.
- **Require** `viewBox="0 0 100 100"` (case-insensitive).
- **Reject** anything over **8 KB** (**24 KB** for a `poster`).

## Raster (PNG) output

SVG is the source of truth: text, diffable, reviewable in a PR, hand-editable, and scalable. The
PNG is a derived, content-addressed copy for the consumers that cannot draw SVG:

| Consumer | SVG? | Takes |
|---|---|---|
| beads-viewer headers and deck covers | yes | the SVG (the PNG if no SVG is stored) |
| `og:image` link previews | no (PNG/JPG) | the PNG at 512 px |
| Slack upload | no | the PNG at 512 px (`gb slack upload`) |
| GitLab MR / Jira `<img>` | no | the PNG (`gb mr embed`, `gb attachments upload`) |
| favicon, app and manifest icons | practically no | the PNG at the icon's size |
| email clients | mostly no | the PNG |

**512 px is the poster size.** `MaxRasterSize` is 512, and a `--size` above it is an error rather
than a huge canvas. `--size` defaults to 128 (`DefaultRasterSize`). Upload a poster at 512 so
`og:image` and Slack get the full picture; the viewer's 64 and 112 px slots use the SVG.

### Toolchain: pure-Go `oksvg` + `rasterx`

`internal/badge/raster.go` rasterizes with `github.com/srwiley/oksvg` and
`github.com/srwiley/rasterx`: no CGo, no external binary, so it runs anywhere the controller does,
including the in-box (Gasbox) image, which takes no external system dependencies
(ka-0000005379). `rsvg-convert`, ImageMagick/Inkscape, `resvg` and a headless-browser screenshot
were rejected because each adds a binary or a system package to every image.

Three corrections make the pure-Go path right:

- **`<text>`.** `oksvg` silently drops `<text>`, which would erase the Tier-0 monogram.
  `Rasterize` draws the geometry with `oksvg`, then parses the `<text>` element and overlays it
  with a bundled Go font (`golang.org/x/image/font`, `gofont/gobold`), scaled out of the 100×100
  viewBox and centred to match `text-anchor="middle"` / `dominant-baseline="central"`. Model
  badges carry no `<text>`, so for them the overlay does nothing.
- **Stroke width.** `oksvg` maps path geometry to pixels but leaves `stroke-width` and dashes in
  viewBox units, so at 512 px a 6-unit stroke drew 6 px wide instead of 31. `Rasterize` multiplies
  every stroke width and dash pattern by the scale.
- **Gradients.** A `gradientUnits="userSpaceOnUse"` gradient stayed in viewBox units and sat in
  the top-left corner. `Rasterize` puts the viewBox-to-pixel matrix at the head of its
  `gradientTransform` (`objectBoundingBox` gradients were already right).

`TestRasterize_Golden` pins the picture at 24/128/256/512 px for a Tier-0 badge and at 128/512 px
for a poster (`testdata/raster-*.golden.png`).

### Cost: raster vs SVG

Measured on the Tier-0 badge for `ka-0000005980` (shield, ring, two-letter monogram):

| | SVG | PNG |
|---|---|---|
| Bytes | 561 | 24 px: 1266 · 40 px: 2017 · 64 px: 3192 · 128 px: 5975 · 256 px: 11960 |
| Sizes | one asset, any size | one asset per size |
| Renders in Slack, GitLab, Jira, `og:image`, favicons | no | yes |
| Generation | a string build, microseconds | ~2.8 ms at 128 px, in-process |
| Diffable, hand-editable | yes | no |

Prefer SVG wherever the consumer draws it; use PNG only at the boundary to a raster-only consumer.
Rasterizing is cheap enough to do on demand, and the content-addressed upload means a given
picture and size is stored at most once.

### Upload: the picture written back onto the bead

`gb badge <bead-id> --upload` (any bead type; implies `--png`) puts the SVG and the PNG in the
private bucket and prefix that `gb image upload` uses (`COOP_S3_BUCKET` / `COOP_S3_PREFIX`), under
the `images/` tree the viewer's image proxy serves:

```text
<prefix>/images/badges/<bead-id>/<sha8>-<tier>.svg
<prefix>/images/badges/<bead-id>/<sha8>-<tier>-<size>.png
```

`sha8` is the start of the file's SHA-256, so identical bytes are one object (a re-upload is a
no-op) and a new picture is a new object rather than an overwrite. It then writes both
viewer-proxied URLs onto the bead as `fields.icon_svg_url` and `fields.icon_png_url` (the daemon
declares both on every type), and prints the PNG's viewer URL on stdout, with the SVG URL and the
raw S3 URL on stderr. `--json` adds `svg_key`, `svg_url` and `fields_written`. If the bead write
fails, the files are already up and the command exits non-zero naming the fields.

The viewer reads both fields off the bead it already fetched, so an upload shows on the next page
load with no restart and no extra daemon call. It uses a stored URL only if its path goes through
`/image/`, and re-roots it on the serving viewer.

**Who can see it.** The bucket is private, and the viewer's `/image/` route is admin-only. An
`og:image` URL therefore loads only for a signed-in reader; Slack's unfurler and other anonymous
fetchers cannot load it. A public image host for Slack unfurls is **not** in scope (heraldry spec
§13). To show a badge in Slack, GitLab or Jira, upload the local PNG with the destination's own
command (`gb slack upload`, `gb mr embed`, `gb attachments upload`; advice ka-0000002145).

## Caching

Only model (Tier-2) pictures are cached; Tier-0 is byte-identical for the same input and never
cached. The cache is keyed on **what the picture is drawn from** (gd-NyYxIxxbuw): `badge.CacheKey`
hashes the bead id, its title, the canonical run of the resolved arms, the style and
`badge.PromptVersion`. The object lives at
`<COOP_S3_PREFIX>/images/badges/<id>/<cache_key>.svg` (a full 64-hex key, so it never collides
with an upload's `sha8` name). A retitle, new arms, another style or reworded prompts each land on
a new key and redraw; a bead's badge is stable until one of them changes. `--refresh` redraws
without a change and replaces the pinned picture; `--no-cache` never reads or writes the cache;
`--tier0` bypasses it.

Bump `PromptVersion` (`internal/badge/model.go`) with any prompt change that would draw a
different picture; `TestPromptVersionPinsPrompts` fails until you do. Objects cached before the
key took in the arms sit under `images/plan-badges/`: still served by the viewer proxy, never read
again.

## Commands

```sh
# The poster, drawn from the bead's arms, as SVG on stdout
gb badge gd-XXXX

# The poster at 512 px to a file
gb badge gd-XXXX --png --size 512 --out badge.png

# Upload the SVG + 512 px PNG and write icon_svg_url / icon_png_url onto the bead
gb badge gd-XXXX --upload --size 512

# Redraw (skip and replace the cached picture), then upload
gb badge gd-XXXX --refresh --upload --size 512

# A picture for a small slot
gb badge gd-XXXX --style line-art-bold --png --size 40 --out small.png

# Deterministic Tier-0, no model
gb badge gd-XXXX --tier0 --png --out badge.png

# Structured output: tier, cache key, arms, arms_from (and URLs with --upload)
gb badge gd-XXXX --json
```

`--out` and `--upload` imply `--png`. `gb plan badge` is a deprecated alias of `gb badge`.

## Change protocol

Other surfaces (the viewer headers and deck cover, `og:image`, uploads) render against this
contract. Before changing a rule here:

1. Change the enforcing code **and** its test in the same PR: the palette table, the shape list,
   the sanitizer checks, the size floors and caps, and the prompts each have a test that reds on
   drift (`tier0_golden_test.go`, `model_test.go`, `prompt_test.go`, `raster_golden_test.go`,
   `badge_image_test.go`).
2. Palette and shape order is identity: reordering re-skins every Tier-0 badge. Append, don't
   reorder.
3. Show the owner a sample sheet against real beads (as
   [`heraldry/badges-round-1.md`](heraldry/badges-round-1.md) did) before changing a default
   style or model. Taste is the owner's call; plumbing is not.

## Source of truth

| Concern | File |
|---|---|
| Arms resolution up the line | `gardenio/controller/internal/badge/arms.go` |
| Tier-0 palette, shapes, monogram | `gardenio/controller/internal/badge/tier0.go` |
| Tier-2 prompts, styles, size floors and caps, sanitizer, `PromptVersion` | `gardenio/controller/internal/badge/model.go` |
| Cache key | `gardenio/controller/internal/badge/cache.go` |
| Rasterizer, `MaxRasterSize` | `gardenio/controller/internal/badge/raster.go` |
| `gb badge` CLI, upload | `gardenio/controller/cmd/gb/plan_badge.go`, `plan_badge_raster.go`, `plan_badge_cache.go` |
| Viewer: header, deck cover, `og:image` | `gardenio/controller/cmd/beads-viewer/badge_image.go`, goldens `testdata/badge_*.golden.html` |
| Plan, step 10 | gd-pQZO9ie11D, gd-XWocW0x1fq |

<!-- bead refs -->
[ka-0000005980]: https://beads/ka-0000005980
[ka-0000005497]: https://beads/ka-0000005497
[ka-0000004332]: https://beads/ka-0000004332
