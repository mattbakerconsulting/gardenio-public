# Heraldry charge artwork

The 128 px PNG of every library charge, `emoji_u<codepoints>.png` (VS16 left out of the
name), copied unchanged from [googlefonts/noto-emoji](https://github.com/googlefonts/noto-emoji)
at tag `v2026-09-24-unicode18_0` (commit e20cbc2), `2D/png/128/`, and its 72 px PNG in `72/`,
from `2D/png/72/` (`SVGOptions.Small`, for slots up to 24 px: gd-5ZVgasQaHh). The renderer
embeds them (`go:embed`); nothing else in gardenio draws charges.

Noto Emoji's README puts the image resources under the Apache License 2.0 and the fonts under
the SIL Open Font License 1.1. `LICENSE` here is the repository's root `LICENSE` file at that
tag, copied verbatim.

Do not edit or add files by hand. `go run ./genassets -noto DIR` (see `../genassets`) copies
exactly the library's charges from a checkout at the pinned tag and removes the rest;
`TestEveryChargeHasArtwork` and `TestKbeadsParity` fail when the assets, the library and kbeads
disagree.
