package badge

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// decode is a test helper that decodes a PNG and fails loudly.
func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding PNG: %v", err)
	}
	return img
}

func TestRasterize_Tier0_ProducesSizedPNG(t *testing.T) {
	b := Tier0("ka-0000007031", "Every gardenio plan gets its own icon badge")
	r, err := b.Rasterize(128)
	if err != nil {
		t.Fatalf("Rasterize: %v", err)
	}
	if r.Size != 128 {
		t.Errorf("Size = %d, want 128", r.Size)
	}
	if len(r.PNG) == 0 {
		t.Fatal("PNG is empty")
	}
	if len(r.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want 64 hex chars", r.SHA256)
	}
	img := decode(t, r.PNG)
	if got := img.Bounds().Dx(); got != 128 {
		t.Errorf("width = %d, want 128", got)
	}
	if got := img.Bounds().Dy(); got != 128 {
		t.Errorf("height = %d, want 128", got)
	}
}

func TestRasterize_DefaultSize(t *testing.T) {
	b := Tier0("gd-x", "Title")
	r, err := b.Rasterize(0)
	if err != nil {
		t.Fatalf("Rasterize: %v", err)
	}
	if r.Size != DefaultRasterSize {
		t.Errorf("Size = %d, want default %d", r.Size, DefaultRasterSize)
	}
}

// The content address must be a pure function of (SVG, size): the same badge
// rendered twice is byte-identical, which is what makes the S3 upload
// idempotent.
func TestRasterize_ContentAddressStable(t *testing.T) {
	b := Tier0("ka-0000007031", "Every gardenio plan gets its own icon badge")
	r1, err := b.Rasterize(128)
	if err != nil {
		t.Fatalf("Rasterize #1: %v", err)
	}
	r2, err := b.Rasterize(128)
	if err != nil {
		t.Fatalf("Rasterize #2: %v", err)
	}
	if r1.SHA256 != r2.SHA256 {
		t.Errorf("SHA256 not stable: %s != %s", r1.SHA256, r2.SHA256)
	}
	if !bytes.Equal(r1.PNG, r2.PNG) {
		t.Error("PNG bytes not byte-identical across renders")
	}
}

// A different plan id (=> different palette/monogram) must yield a different
// content address; otherwise the cache would collapse distinct badges.
func TestRasterize_DistinctPlansDiffer(t *testing.T) {
	a, err := Tier0("gd-aaaa", "Alpha Plan").Rasterize(128)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Tier0("gd-bbbb", "Beta Plan").Rasterize(128)
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 == b.SHA256 {
		t.Error("distinct plans produced the same content address")
	}
}

// Different sizes produce different bytes and different addresses.
func TestRasterize_SizeAffectsAddress(t *testing.T) {
	b := Tier0("ka-0000007031", "Title")
	small, err := b.Rasterize(24)
	if err != nil {
		t.Fatal(err)
	}
	large, err := b.Rasterize(256)
	if err != nil {
		t.Fatal(err)
	}
	if small.SHA256 == large.SHA256 {
		t.Error("different sizes produced the same content address")
	}
	if got := decode(t, small.PNG).Bounds().Dx(); got != 24 {
		t.Errorf("small width = %d, want 24", got)
	}
	if got := decode(t, large.PNG).Bounds().Dx(); got != 256 {
		t.Errorf("large width = %d, want 256", got)
	}
}

// The monogram overlay must actually put ink on the canvas: oksvg alone drops
// SVG <text>, so a Tier-0 badge rasterized without the overlay would be a bare
// disc. We assert the near-white monogram color appears somewhere in the
// central band where the text is drawn.
func TestRasterize_MonogramOverlaid(t *testing.T) {
	// Blue palette (#2563eb bg / #eff6ff fg) — id chosen so sum[0]%12 lands on
	// a light foreground that contrasts the background. Any id works; we just
	// need the fg pixels present.
	b := Tier0("ka-0000007031", "Every gardenio plan gets its own icon badge")
	r, err := b.Rasterize(128)
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, r.PNG)
	// Scan the central band (rows 40..88) for a near-white monogram pixel.
	found := false
	for y := 40; y < 88 && !found; y++ {
		for x := 20; x < 108; x++ {
			c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
			if c.R > 220 && c.G > 220 && c.B > 220 && c.A > 200 {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("no near-white monogram pixels found; overlay likely missing")
	}
}

// A Tier-2 style badge (line art built from <path>/<rect>, no <text>) must
// rasterize without an overlay and still be non-empty.
func TestRasterize_LineArtNoText(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">` +
		`<rect x="6" y="6" width="88" height="88" rx="20" fill="#2563eb"/>` +
		`<path d="M30 60 L50 30 L70 60 Z" fill="none" stroke="#eff6ff" stroke-width="6"/>` +
		`</svg>`
	b := Badge{SVG: svg, Tier: TierModel}
	r, err := b.Rasterize(128)
	if err != nil {
		t.Fatalf("Rasterize line art: %v", err)
	}
	if len(r.PNG) == 0 {
		t.Fatal("line-art PNG empty")
	}
	decode(t, r.PNG) // must be a valid PNG
}

func TestRasterize_InvalidSVG(t *testing.T) {
	b := Badge{SVG: "<svg><this is not valid", Tier: TierDeterministic}
	if _, err := b.Rasterize(128); err == nil {
		t.Error("expected an error for malformed SVG, got nil")
	}
}

func TestExtractText(t *testing.T) {
	b := Tier0("ka-0000007031", "Every gardenio plan")
	tx, ok := extractText(b.SVG)
	if !ok {
		t.Fatal("extractText returned ok=false for a Tier-0 badge")
	}
	if tx.content == "" {
		t.Error("extracted empty monogram")
	}
	if tx.fontSize <= 0 {
		t.Errorf("fontSize = %v, want > 0", tx.fontSize)
	}
	// Tier-0 centers the monogram.
	if tx.x != canvas/2.0 || tx.y != canvas/2.0 {
		t.Errorf("anchor = (%v,%v), want (%v,%v)", tx.x, tx.y, canvas/2.0, canvas/2.0)
	}
}

func TestExtractText_NoTextElement(t *testing.T) {
	if _, ok := extractText(`<svg><rect/></svg>`); ok {
		t.Error("extractText found text where there was none")
	}
}

func TestParseHexColor(t *testing.T) {
	cases := []struct {
		in      string
		ok      bool
		r, g, b uint8
	}{
		{"#2563eb", true, 0x25, 0x63, 0xeb},
		{"#fff", true, 0xff, 0xff, 0xff},
		{"#abc", true, 0xaa, 0xbb, 0xcc},
		{"white", false, 0, 0, 0},
		{"#12", false, 0, 0, 0},
		{"", false, 0, 0, 0},
	}
	for _, tc := range cases {
		c, ok := parseHexColor(tc.in)
		if ok != tc.ok {
			t.Errorf("parseHexColor(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		rgba := color.RGBAModel.Convert(c).(color.RGBA)
		if rgba.R != tc.r || rgba.G != tc.g || rgba.B != tc.b {
			t.Errorf("parseHexColor(%q) = %v, want (%d,%d,%d)", tc.in, rgba, tc.r, tc.g, tc.b)
		}
	}
}

func TestAtof(t *testing.T) {
	if got := atof("40", 1); got != 40 {
		t.Errorf("atof(40) = %v", got)
	}
	if got := atof("52px", 1); got != 52 {
		t.Errorf("atof(52px) = %v", got)
	}
	if got := atof("junk", 7); got != 7 {
		t.Errorf("atof(junk) = %v, want default 7", got)
	}
}
