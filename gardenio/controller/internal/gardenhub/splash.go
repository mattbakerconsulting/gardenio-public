package gardenhub

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The splash (plan gd-EJGA3zDGCk) is a demo at /garden/splash: one full-viewport canvas that web/splash.js draws in WebGL
// with three.js, lit by web/almanac.js on the ground web/splash/land.js shapes, web/splash/stone.js's stone on its brow and
// web/splash/flora.js's grass, flowers, hedges and trees round it, web/splash/farm.js's farmstead and pond down in the
// valley, and web/splash/light.js's sun, moon, sky and stars, at the tier web/splash/quality.js picks, through the zoom and
// the look web/splash/view.js holds (gd-I5QHVfGLbD). The document is one response (splash.html the template, splash.css and
// the nine scripts inlined and minified, almanac, land, stone, flora, farm, light, quality and view first, as splashJS orders them) that reads no beads and no viewer, so every request gets the same bytes. Beside it the
// handler serves the two files the page fetches, each embedded and cached for a year under a ?v= of its own hash: the
// vendored three.js (web/vendor/VENDOR.md, re-vendored by `make vendor-three`) and the still the page shows where WebGL is
// unavailable: web/splash-fallback.png, the noon frame (?t=2026-10-26T12:00:00Z&q=mid, the `splash` golden's) shot in
// Chromium at 800 x 500 with the time controls hidden (step 10, gd-PBK9CTh5Xg). The page is public: beads-viewer serves it
// without a session and the edge admits it with none (gd-hzRHm1KB5r).
// Over the scene the page offers a start menu as a game's title screen does (gd-Fw6fffLZ3p): plain HTML, so it is there with or
// without WebGL, with two links: Request an invite, to the Lands front of house (/lands/ under the base path, where a visitor
// signs in with GitHub and applies for a plot), and Sign in, through the edge's /oauth2/start back to /garden
// (gd-QDdwxnGdTY). The links are navigations the visitor chooses, not requests the page makes: the page fetches nothing from
// another host. ExportSplash renders the same template as a static site for the landing image on another host
// (cmd/splash-export, gd-Z3eh4eEJ87): its assets beside it by relative URL, its two links absolute on the app's origin.
var (
	//go:embed web/splash.html
	splashHTML string
	//go:embed web/splash.css
	splashCSS string
	//go:embed web/almanac.js
	splashAlmanacJS string
	//go:embed web/splash/land.js
	splashLandJS string
	//go:embed web/splash/stone.js
	splashStoneJS string
	//go:embed web/splash/flora.js
	splashFloraJS string
	//go:embed web/splash/farm.js
	splashFarmJS string
	//go:embed web/splash/light.js
	splashLightJS string
	//go:embed web/splash/quality.js
	splashQualityJS string
	//go:embed web/splash/view.js
	splashViewJS string
	//go:embed web/splash.js
	splashSceneJS string
	//go:embed web/vendor/three.module.min.js
	splashThree []byte
	//go:embed web/splash-fallback.png
	splashStill []byte
)

const (
	// splashDir is where the viewer serves the page's two files, three.js and the still; ExportSplash writes them by the
	// same names beside index.html.
	splashDir       = "/garden/splash/"
	splashThreeFile = "three.module.min.js"
	splashStillFile = "still.png"
	splashThreePath = splashDir + splashThreeFile
	splashStillPath = splashDir + splashStillFile
	// splashInvitePath is where Request an invite goes: the Lands front of house (cmd/beads-viewer lands.go), public like
	// the splash, under the same base path.
	splashInvitePath = "/lands/"
	// splashSignInPath is where Sign in goes: the edge's sign-in, which lands the visitor on /garden. rd is a path on the
	// app's host, so the base path is put before both.
	splashSignInPath = "/oauth2/start?rd="
	splashSignInRd   = "/garden"
	// yearCache is for a file whose URL carries its own hash: a new file is a new URL.
	yearCache = "public, max-age=31536000, immutable"
)

var splashTmpl = template.Must(template.New("splash").Parse(splashHTML))

// splashJS is the page's script: the almanac, the land, the stone, the flora, the farm, the light, the quality tiers and the
// view, which splash.js reads as globals, then the scene.
var splashJS = splashAlmanacJS + "\n" + splashLandJS + "\n" + splashStoneJS + "\n" + splashFloraJS + "\n" + splashFarmJS + "\n" + splashLightJS + "\n" + splashQualityJS + "\n" + splashViewJS + "\n" + splashSceneJS

// splashServedJS and splashServedCSS are the script and the stylesheet as the page inlines them: minified once at start, as
// the /garden bundles are, which halves the page gzipped (comments are about 40 percent of it, gd-BqEPTwuyj6). A source the
// minifier cannot parse or would miscompile is inlined as written (minified); TestSplashRoute fails on it.
var (
	splashServedJS  = minified("text/javascript; charset=utf-8", splashJS)
	splashServedCSS = minified("text/css", splashCSS)
)

func shortHash(b ...[]byte) string {
	h := sha256.New()
	for _, s := range b {
		h.Write(s)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

var (
	splashThreeTag = shortHash(splashThree)
	splashStillTag = shortHash(splashStill)
	// splashTag is the document's ETag: it depends on its sources, the files it names by hash and the base path only.
	splashTag = shortHash([]byte(splashHTML), []byte(splashCSS), []byte(splashJS), []byte(splashThreeTag), []byte(splashStillTag))
)

type splashData struct {
	Base   string
	Three  string // three.js's URL, for import()
	Still  string // the still's URL
	Invite string // Request an invite: the Lands front of house
	SignIn string // Sign in: the edge's sign-in, back to /garden
	CSS    template.CSS
	JS     template.JS
}

// splashLinks fills the page's URLs: three.js and the still at assets (with ?v= of their hash), the two links on app, each
// with the base path before it.
func splashLinks(base, assets, app string) splashData {
	return splashData{
		Base:   base,
		Three:  assets + splashThreeFile + "?v=" + splashThreeTag,
		Still:  assets + splashStillFile + "?v=" + splashStillTag,
		Invite: app + base + splashInvitePath,
		SignIn: app + base + splashSignInPath + base + splashSignInRd,
		CSS:    template.CSS(splashServedCSS), //nolint:gosec // embedded at build time, not user input
		JS:     template.JS(splashServedJS),   //nolint:gosec // embedded at build time, not user input
	}
}

// ExportSplash writes the splash to dir as a static site for a host other than the app's (gd-Z3eh4eEJ87): index.html, the
// same template the viewer serves, with three.module.min.js and still.png beside it by relative URL and its two links
// absolute on appOrigin (https://gardenio.clonk.tv gives https://gardenio.clonk.tv/lands/ and
// https://gardenio.clonk.tv/oauth2/start?rd=/garden). The page makes no request back to the viewer. The assets are named
// ./three.module.min.js and ./still.png, not bare: splash.js import()s data-three, and a module specifier without ./, ../
// or / is a bare one that only an import map resolves, so a bare data-three never loads and the page always shows the
// still (gd-OeWiwpmr5Y).
func ExportSplash(dir, appOrigin string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf strings.Builder
	if err := splashTmpl.Execute(&buf, splashLinks("", "./", strings.TrimRight(appOrigin, "/"))); err != nil {
		return fmt.Errorf("render splash: %w", err)
	}
	for name, body := range map[string][]byte{
		"index.html":    []byte(buf.String()),
		splashThreeFile: splashThree,
		splashStillFile: splashStill,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil { //nolint:gosec // a public static site
			return err
		}
	}
	return nil
}

func (h *Handler) splash(w http.ResponseWriter, r *http.Request) {
	etag := `W/"s-` + splashTag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, no-cache") // the same bytes for everyone, revalidated by ETag
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var buf strings.Builder
	if err := splashTmpl.Execute(&buf, splashLinks(h.BasePath, h.BasePath+splashDir, "")); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buf.String()))
}

// splashFile serves one embedded file of the splash, cached for a year: the page asks for it under ?v= of its hash.
func splashFile(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", yearCache)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(body)
	}
}
