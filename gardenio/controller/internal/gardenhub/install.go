package gardenhub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"slices"
	"strconv"
	"sync"
)

// Home-screen install (Lands, Commons on a phone M6, gd-JSRReAJLRL; design note
// 2026-10-20-lands-commons-phone-design.md 3.9). The garden serves its own web
// app manifest and icons, so "Add to Home Screen" opens /garden standalone with
// no browser chrome, apart from the Deck the rest of the viewer installs as
// (cmd/beads-viewer/pwa.go). garden.html links both and carries the iOS meta
// tags; the safe-area insets are already in garden.css.

// The bar's colours (garden.css --sky), so the standalone status bar runs into
// the header by day and by night; the splash is the page's --bg.
const (
	themeDay   = "#bfe1ee"
	themeNight = "#16223b"
	splashDay  = "#e4eed0"
)

// iconSizes are the icon PNGs the garden serves: 180 is iOS's apple-touch-icon,
// 192 and 512 the manifest's.
var iconSizes = []int{180, 192, 512}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

type webManifest struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	ShortName       string         `json:"short_name"`
	StartURL        string         `json:"start_url"`
	Scope           string         `json:"scope"`
	Display         string         `json:"display"`
	ThemeColor      string         `json:"theme_color"`
	BackgroundColor string         `json:"background_color"`
	Icons           []manifestIcon `json:"icons"`
}

// manifest serves GET /garden/manifest.webmanifest. The scope is /garden, so the
// garden's own links (which stay inside it, links_test.go) open in the installed
// window and a push's /garden/c/... deep link lands in it.
func (h *Handler) manifest(w http.ResponseWriter, _ *http.Request) {
	g := h.BasePath + "/garden"
	m := webManifest{
		ID: g, Name: "The Garden", ShortName: "Garden", StartURL: g, Scope: g, Display: "standalone",
		ThemeColor: themeDay, BackgroundColor: splashDay,
	}
	for _, n := range iconSizes[1:] {
		px := strconv.Itoa(n)
		// The sprite sits inside the maskable safe zone, so one picture serves both purposes.
		m.Icons = append(m.Icons, manifestIcon{Src: g + "/icon-" + px + ".png", Sizes: px + "x" + px, Type: "image/png", Purpose: "any maskable"})
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(m)
}

// icon serves GET /garden/icon-{n}.png, one route per size in iconSizes. The 192 is the tab's icon, so every page
// load asks for it: a day's freshness, then a revalidation that its ETag (the PNG's hash) answers with a bodyless 304.
func icon(n int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ic := icons()[n]
		w.Header().Set("ETag", ic.etag)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if r.Header.Get("If-None-Match") == ic.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(ic.png)))
		_, _ = w.Write(ic.png)
	}
}

type encodedIcon struct {
	png  []byte
	etag string
}

// icons is the encoded PNG of each size, drawn once: paletted (the art has nine colours) and at best compression, about
// 40% of RGBA at the default level (the 192, 396 bytes where it was 950; P1.5, gd-YU9gGOMvur).
var icons = sync.OnceValue(func() map[int]encodedIcon {
	out := map[int]encodedIcon{}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	for _, n := range iconSizes {
		var b bytes.Buffer
		_ = enc.Encode(&b, drawIcon(n))
		sum := sha256.Sum256(b.Bytes())
		out[n] = encodedIcon{png: b.Bytes(), etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	}
	return out
})

// iconArt is the garden's icon: art.js's leafy plant on its soil mound, in the
// day palette (DAY), 16 x 16, "." clear.
var iconArt = [16]string{
	"................",
	"....kk....kk....",
	"...kmnk..knmk...",
	"..kmnmmkkmmmnk..",
	"..kkmmLmmLmmkk..",
	"...kLmmLLmmLk...",
	"....kkmLLmkk....",
	".....kkLLkk.....",
	"......kLLk......",
	"....kkkkkkkk....",
	"..kkuuuuuuuukk..",
	".kuuussssssstkk.",
	".kussssssssssttk",
	".ksssssssssssttk",
	"..kttttttttttk..",
	"...kkkkkkkkkk...",
}

// iconHorizon is the art row where the sky meets the grass behind the mound.
const iconHorizon = 12

var iconPal = map[byte]color.RGBA{
	'k': {0x2a, 0x1a, 0x14, 0xff}, 'L': {0x1f, 0x6e, 0x34, 0xff}, 'm': {0x5c, 0xcf, 0x4f, 0xff}, 'n': {0xa6, 0xee, 0x7a, 0xff},
	's': {0xa8, 0x70, 0x3c, 0xff}, 't': {0x7e, 0x4c, 0x26, 0xff}, 'u': {0xc8, 0x90, 0x5a, 0xff},
	'^': {0xbf, 0xe1, 0xee, 0xff}, // --sky
	'_': {0x5f, 0xae, 0x3e, 0xff}, // --grass
}

// iconPalette is iconPal's colours as a PNG palette, in key order so the encoded bytes (and their ETag) are stable.
func iconPalette() color.Palette {
	keys := make([]byte, 0, len(iconPal))
	for k := range iconPal {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	p := make(color.Palette, len(keys))
	for i, k := range keys {
		p[i] = iconPal[k]
	}
	return p
}

// drawIcon paints the icon at n x n: opaque (iOS fills transparency with black),
// the art scaled by whole pixels to about five eighths of the side and centred,
// which keeps it inside a maskable icon's safe circle.
func drawIcon(n int) *image.Paletted {
	s := n * 5 / 8 / 16
	off := (n - 16*s) / 2
	img := image.NewPaletted(image.Rect(0, 0, n, n), iconPalette())
	for y := 0; y < n; y++ {
		bg := iconPal['^']
		if y >= off+iconHorizon*s {
			bg = iconPal['_']
		}
		for x := 0; x < n; x++ {
			c := bg
			if i, j := (x-off)/s, (y-off)/s; x >= off && y >= off && i < 16 && j < 16 {
				if p, ok := iconPal[iconArt[j][i]]; ok {
					c = p
				}
			}
			img.Set(x, y, c)
		}
	}
	return img
}
