# heraldry-proto

The Phase 0 renderer prototype for heraldry (plan gd-N6I3Oa7uDg, step 0.1
gd-cAQGv3TAan; spec `gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md`
§6, §11). **Throwaway, not product code**: it draws the sample sheets with the
drawing rule the product's `internal/heraldry` will use (step 2.1) and records
that Noto's SVGs do not render under oksvg. The findings and the geometry are
written up in [`gardenio/docs/heraldry/shield.md`](../../gardenio/docs/heraldry/shield.md).

It is its own Go module, kept out of `go.work` so oksvg never enters the
workspace build; `make test-heraldry-proto` runs its tests.

```sh
cd scripts/heraldry-proto
./fetch-noto.sh /tmp/noto                         # sparse checkout of the pinned Noto release, ~66 MB
GOWORK=off go run . svg                           # regenerate gardenio/docs/heraldry/shield{,-grid}.svg
GOWORK=off go run . render -noto /tmp/noto -out out   # sheet.png, tinctures.png, pictures/*.png
GOWORK=off go run . survey -noto /tmp/noto -out out   # oksvg-survey.md, oksvg-compare.png
GOWORK=off go run . round2 -noto /tmp/noto -out out     # step 0.4b's sheet: r2-*.png, matrix-*.png, scored.tsv, summary.md, contrast-matrix.tsv
GOWORK=off go test ./...
```

| File | What |
|---|---|
| `shield.go` | the shield geometry (the only source of it), the path tracer shared by raster and SVG, the two SVG generators |
| `render.go` | `Render`: outline and field from the path (`x/image/vector`), charges from the 72-px PNGs (`x/image/draw` Catmull-Rom), clipped to the field, at a CSS slot × device pixel ratio (1×–3×), with or without the Q14 keyline; R13's eight tinctures, day and night |
| `contrast.go` | WCAG luminance and contrast, per-charge median and decile statistics, the sinking measure (rubric G1), the contrast matrix (spec §6.6) and its per-tincture sheets (refusal beside keyline) |
| `noto.go`, `fetch-noto.sh` | the pinned release (`v2026-09-24-unicode18_0`) and its `2D/` asset layout |
| `oksvg.go` | the survey: every candidate single-codepoint SVG through oksvg vs the release PNG |
| `sheet.go`, `main.go` | the contact sheets, their labels, and the sample arms |
| `shapes.go` | Q11's other shield outlines; the heater stays in `shield.go` |
| `sets.go` | the charge names read from `library.md`, and a decode cache |
| `round2.go`, `round2_sheets.go` | step 0.4b: loads `samples/round-2/arms.tsv` and `grades.tsv`, validates as the daemon would (R2, R3, R5, R11), resolves `auto` and `plan` tinctures, grades the measured and mechanical rubric checks, lays the hand grades over them, writes `scored.tsv` and `summary.md`, draws the pages |

Round 1's quantised pictures, 8-px cell rules, uniqueness test, other artwork sets and browser-shots
script were removed with the quantised tier (spec §6.3); they are in git at fb2469a6.

The sample arms avoid every bridge theme glyph (R4) and use R13's eight tinctures, whose values
are round 2's proposal (`render.go`).

## Round 2

What the sheet shows and found is in
[`gardenio/docs/heraldry/round-2-sheet.md`](../../gardenio/docs/heraldry/round-2-sheet.md).
