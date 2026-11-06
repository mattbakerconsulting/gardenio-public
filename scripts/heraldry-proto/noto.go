package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

// NotoTag is the pinned Noto Emoji release, the one 1.1a (gd-INpa3m02bc)
// generates the library from. fetch-noto.sh checks out exactly this tag.
const NotoTag = "v2026-09-24-unicode18_0"

// Noto reads artwork from a checkout of googlefonts/noto-emoji at NotoTag.
// Since the Unicode 18 release the per-codepoint assets live under 2D/:
// 2D/png/72/emoji_u<cp>[_<cp>…].png and 2D/svg/emoji_u<cp>….svg, named
// without VS16 (U+FE0F).
type Noto struct{ Dir string }

func (n Noto) base(emoji string) string {
	var cps []string
	for _, r := range emoji {
		if r == 0xFE0F {
			continue
		}
		cps = append(cps, fmt.Sprintf("%04x", r))
	}
	return "emoji_u" + strings.Join(cps, "_")
}

func (n Noto) PNGPath(emoji string) string {
	return filepath.Join(n.Dir, "2D", "png", "72", n.base(emoji)+".png")
}

func (n Noto) SVGPath(emoji string) string {
	return filepath.Join(n.Dir, "2D", "svg", n.base(emoji)+".svg")
}

func (n Noto) PNG72(emoji string) (image.Image, error) {
	f, err := os.Open(n.PNGPath(emoji))
	if err != nil {
		return nil, fmt.Errorf("%s has no Noto artwork: %w", emoji, err)
	}
	defer f.Close()
	return png.Decode(f)
}
