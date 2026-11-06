package heraldry

import (
	"bytes"
	"image/png"
	"os"
	"testing"

	"gardenio/controller/internal/heraldry/kbeadssrc"
)

// kbeadsDir is kbeads' heraldry package, the library's source of truth, from
// this package's directory.
const kbeadsDir = "../../../../kbeads/internal/heraldry"

// TestKbeadsParity fails when kbeads' library or Noto pin moves
// without a regenerate here (go run ./genassets -noto DIR; gd-BVlugqdgq3).
func TestKbeadsParity(t *testing.T) {
	src, err := kbeadssrc.Read(kbeadsDir)
	if err != nil {
		t.Fatal(err)
	}
	const fix = "; regenerate with `go run ./genassets -noto DIR`"
	if src.NotoEmojiTag != NotoEmojiTag {
		t.Errorf("kbeads pins Noto %s, the assets are %s%s", src.NotoEmojiTag, NotoEmojiTag, fix)
	}
	if len(src.Library) != len(library) {
		t.Fatalf("kbeads has %d charges, this package %d%s", len(src.Library), len(library), fix)
	}
	for i, k := range src.Library {
		if c := library[i]; k.Emoji != c.Emoji || k.Name != c.Name || k.Codepoints != c.Codepoints {
			t.Errorf("charge %d: kbeads %+v, this package %+v%s", i, k, c, fix)
		}
	}
}

// TestEveryChargeHasArtwork: every library charge has its 128 px and its
// 72 px PNG (spec §6.6), and the assets hold nothing the library does not use.
func TestEveryChargeHasArtwork(t *testing.T) {
	for _, sz := range []struct {
		px  int
		dir string
		art func(Charge) ([]byte, bool)
	}{{128, "assets", ChargePNG}, {72, "assets/72", smallChargePNG}} {
		want := map[string]bool{}
		for _, c := range library {
			name := "emoji_u" + c.Codepoints + ".png"
			want[name] = true
			b, ok := sz.art(c)
			if !ok {
				t.Errorf("%s (%s) has no %d px artwork", c.Emoji, c.Name, sz.px)
				continue
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(b))
			if err != nil || cfg.Width != sz.px || cfg.Height != sz.px {
				t.Errorf("%s/%s is not a %d px PNG: %dx%d %v", sz.dir, name, sz.px, cfg.Width, cfg.Height, err)
			}
		}
		ents, err := assets.ReadDir(sz.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if !want[e.Name()] && !(e.IsDir() && e.Name() == "72") {
				t.Errorf("%s/%s is not a library charge's artwork", sz.dir, e.Name())
			}
		}
	}
	if _, err := os.Stat("assets/LICENSE"); err != nil {
		t.Errorf("the upstream licence is not vendored beside the artwork: %v", err)
	}
}
