# badge Tier-0 golden fixtures / sample badges

Each `*.golden.svg` here is the **byte-exact** output of `badge.Tier0(id, title)`
for one representative plan. They serve two purposes at once:

1. **Reproducible-output guard** — `TestTier0_Golden` (in `../tier0_golden_test.go`)
   diffs live `Tier0` output against these files. Any change to the art spec
   (palette order, shapes, ring rule, monogram derivation) that would alter an
   already-generated plan's badge fails the test with a byte-level diff. This is
   what makes the deterministic tier safe to content-address and cache on
   `(id, title)` (ka-0000005930): the bytes are pinned, so a cache hit is a real
   hit, and a badge an operator has learned to recognize does not silently change.

2. **Sample badges** — they are real, standalone SVGs. Open any of them in a
   browser, or open `contact-sheet.html` to see all six rendered at 128 / 40 /
   24px (the three consumer sizes: detail header, card, cmdk row).

## The fixtures

| file | exercises |
|---|---|
| `flagship.golden.svg` | the badge feature's own plan (real id + title) |
| `stopword-lead.golden.svg` | leading stop-word dropped from the monogram |
| `e2e-fixture.golden.svg` | bracketed title (`[E2E fixture] -> EF` collision class) |
| `single-word.golden.svg` | one significant word -> first two letters |
| `special-chars.golden.svg` | `& < > " '` in the title escape cleanly |
| `empty-title.golden.svg` | no letters -> `?` monogram, never blank |

## Regenerating

Only after an **intentional** art-spec change. Regenerate, then review the diff:

```sh
cd gardenio/controller
GB_UPDATE_GOLDEN=1 go test ./internal/badge/ -run TestTier0_Golden
git diff internal/badge/testdata/   # eyeball every changed badge
```

`contact-sheet.html` is a static, hand-runnable convenience (regenerate it from
the `.golden.svg` files if you change the fixture set); it is not asserted by any
test.

## Raster goldens and the poster sample

`raster-<fixture>-<size>.golden.png` pin what `Rasterize` draws at each size,
compared pixel by pixel (`TestRasterize_Golden` in `../raster_golden_test.go`):
the Tier-0 flagship at 24 / 128 / 256 / 512px (the text overlay at size) and
`poster-sample.svg` at 128 / 512px (strokes and gradients at size).
`poster-sample.svg` is a `--style poster` badge for plan gd-pQZO9ie11D (arms
🏅🖌️🔎 on brown), drawn to the poster prompt. `prompt-poster-*.golden.txt`
pin the poster prompts. Regenerate after an intentional change, then look at
every changed PNG:

```sh
GB_UPDATE_GOLDEN=1 go test ./internal/badge/ -run 'TestRasterize_Golden|TestPrompt_Golden'
```
