package gardenhub

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The page's landmarks and labels are a contract with screen readers: one <main>, a skip link to it, a
// live region that only changes on connection-state changes (not the ticking clock), and the cursor's grid, named.
func TestPageA11yStructure(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	for _, want := range []string{
		`<a class="skip" href="#main">`, `<main class="main" id="main" tabindex="-1">`, `</main>`,
		`id="livesr" role="status"`, `id="hots" role="grid" aria-label=`, `<html lang="en">`, `<h1>`,
		`aria-labelledby="h-decision"`, `aria-labelledby="h-work"`, `aria-labelledby="h-agent"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	if strings.Count(body, "<main") != 1 {
		t.Errorf("want exactly one <main>, got %d", strings.Count(body, "<main"))
	}
	// the ticking "synced Ns ago" text must not be a live region
	if m := regexp.MustCompile(`id="live"[^>]*role=`).FindString(body); m != "" {
		t.Errorf("the ticking live indicator is a live region: %q", m)
	}
}

func lum(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	c := [3]float64{}
	for i := range c {
		n, _ := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		v := float64(n) / 255
		if v <= 0.03928 {
			c[i] = v / 12.92
		} else {
			c[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func contrast(a, b string) float64 {
	x, y := lum(a), lum(b)
	if y > x {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
}

// palettes pulls the --token values of the light (first :root) and dark (prefers-color-scheme:dark :root) blocks.
func palettes(t *testing.T) (light, dark map[string]string) {
	t.Helper()
	tok := regexp.MustCompile(`--([a-z-]+):(#[0-9a-fA-F]{3,6})`)
	i := strings.Index(pageCSS, "@media (prefers-color-scheme:dark)")
	if i < 0 {
		t.Fatal("no dark palette")
	}
	collect := func(s string) map[string]string {
		m := map[string]string{}
		for _, x := range tok.FindAllStringSubmatch(s, -1) {
			m[x[1]] = x[2]
		}
		return m
	}
	light = collect(pageCSS[:i])
	dark = collect(pageCSS[i : strings.Index(pageCSS[i:], "}}")+i])
	for k, v := range light { // the dark block only overrides; inherit the rest
		if _, ok := dark[k]; !ok {
			dark[k] = v
		}
	}
	return light, dark
}

// Text colours must reach WCAG AA (4.5:1) on every surface they are drawn on, in both schemes. These pairs are
// the ones the stylesheet actually uses for text; a palette edit that breaks one fails here.
func TestPaletteTextContrastAA(t *testing.T) {
	light, dark := palettes(t)
	pairs := [][2]string{
		{"ink", "surface"}, {"ink", "bg"}, {"muted", "surface"}, {"muted", "bg"}, {"muted", "sky"}, {"warn", "surface"},
		{"warn", "bg"}, {"warn", "sky"}, {"ok", "surface"}, {"ok", "bg"}, {"accent-ink", "surface"}, {"accent-ink", "bg"},
		{"alert", "surface"}, {"box-ink", "box"}, {"box-dim", "box"}, {"box-gold", "box"}, // box-gold: a bead id and an Answer in the Commons composer
		{"on-alert", "alert"},
	}
	for name, p := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, pr := range pairs {
			fg, bg := p[pr[0]], p[pr[1]]
			if fg == "" || bg == "" {
				t.Errorf("%s: token --%s or --%s missing", name, pr[0], pr[1])
				continue
			}
			if r := contrast(fg, bg); r < 4.5 {
				t.Errorf("%s: --%s %s on --%s %s is %.2f:1, want >= 4.5", name, pr[0], fg, pr[1], bg, r)
			}
		}
		if r := contrast("#ffffff", p["tab-sel"]); r < 4.5 {
			t.Errorf("%s: white on --tab-sel is %.2f:1", name, r)
		}
	}
	// the splash's readout says the zoom (plan gd-I5QHVfGLbD, gd-Xs9LA8ONjn) in --hud-ink at full strength on its smoked chip,
	// whose colours do not follow the scheme: AA over white, the brightest thing behind it
	if regexp.MustCompile(`#splash-readout \.zoom \{[^}]*opacity`).MatchString(splashCSS) {
		t.Error("the splash's zoom line is dimmed: its text must stay --hud-ink at full strength")
	}
	if ink, chip := splashTok(t, "hud-ink"), splashChip(t); contrast(ink, chip) < 4.5 {
		t.Errorf("the splash's zoom line %s on its chip over white (%s) is %.2f:1, want 4.5", ink, chip, contrast(ink, chip))
	}
}

// The camera bar is In, Out, Go and the Menu (3b R8): the Menu is a plain disclosure, a button that says whether its list is
// open and a list, hidden until it opens, of Home, All, Plans, Outline, Tend, Costs (plan gd-gh5J0vVbJP step 2.3), Snap, Postcard (gd-WxRuyfdUtR), Sky, Settings
// (Lands 5.12) and Legend & keys, in that order. None
// of them is a bar button any more (Sky, 3b R10, was the header's), the script closes the list on Esc, and the help text names the Menu where it names the
// Outline. Plans and Legend & keys (3b R9) control their drawers, which are not on the map while closed and whose headers the
// signs no longer keep off.
func TestCameraMenu(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	cam := body[strings.Index(body, `<div class="cam"`):strings.Index(body, `<div class="jump" id="jump"`)]
	var bar []string
	for _, m := range regexp.MustCompile(`class="btn cb" id="([a-z-]+)"`).FindAllStringSubmatch(cam, -1) {
		bar = append(bar, m[1])
	}
	if got := strings.Join(bar, " "); got != "cam-in cam-out cam-go cam-menu" {
		t.Errorf("camera bar buttons = %s, want cam-in cam-out cam-go cam-menu", got)
	}
	if !strings.Contains(cam, `id="cam-menu" type="button" aria-expanded="false" aria-controls="menu-list">Menu</button>`) ||
		!strings.Contains(cam, `<ul class="menu-list" id="menu-list" aria-label="Map menu" hidden>`) {
		t.Error("the Menu is not a disclosure: a button with aria-expanded and aria-controls over a hidden list")
	}
	list := cam[strings.Index(cam, `id="menu-list"`):]
	var items []string
	for _, m := range regexp.MustCompile(`class="btn mi" id="([a-z-]+)"`).FindAllStringSubmatch(list, -1) {
		items = append(items, m[1])
	}
	if got := strings.Join(items, " "); got != "cam-home cam-fit cam-plans cam-outline cam-tend cam-costs cam-snap cam-snapsw cam-postcard cam-sky cam-settings cam-wall cam-help cam-old" {
		t.Errorf("Menu items = %s, want cam-home cam-fit cam-plans cam-outline cam-tend cam-costs cam-snap cam-snapsw cam-postcard cam-sky cam-settings cam-wall cam-help cam-old", got)
	}
	for _, want := range []string{
		`id="cam-plans" type="button" aria-expanded="false" aria-controls="dw-plans">Plans</button>`,
		`id="cam-help" type="button" aria-expanded="false" aria-controls="dw-help">Legend &amp; keys</button>`,
		`<a class="btn mi" id="cam-old" href="/v/old-viewer"`, // Lands 5.13: the viewer's LATER pages
	} {
		if !strings.Contains(list, want) {
			t.Errorf("Menu missing %s", want)
		}
	}
	if !strings.Contains(pageCSS, `details.dw:not([open]){display:none}`) {
		t.Error("a closed drawer must not be on the map (3b R9)")
	}
	if strings.Contains(pageAppJS, `.dw>summary`) {
		t.Error("hudRects still keeps the signs off the drawers' buttons, which are gone (3b R9)")
	}
	for _, want := range []string{"if(k==='Escape')menuShow(false,true);", "menuList.addEventListener('click',", "back:menuBtn,", "[['dw-plans','cam-plans'],['dw-help','cam-help']]"} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js missing %s", want)
		}
	}
	if !strings.Contains(body, "<b>Outline</b>, in the <b>Menu</b>") {
		t.Error("the Legend & keys prose must say the Outline is in the Menu")
	}
}

// The outline is the accessible model of the map (design section 8): a named <nav> with a role=tree, a toolbar button that
// opens it and says so, and a canvas that screen readers skip. The world labels are painted text, also hidden.
func TestPageOutlineStructure(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	for _, want := range []string{
		`<nav class="outline" id="outline" aria-labelledby="outline-h">`, `<b id="outline-h">`,
		`<ul role="tree" id="tree" aria-label=`, `id="cam-outline" type="button" aria-expanded="false" aria-controls="outline"`,
		`id="outline-x" type="button" tabindex="-1" aria-label=`, `<canvas id="cv" width="384" height="240" aria-hidden="true">`, `id="lbls" aria-hidden="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	// the outline is clipped, never display:none, so Tab and a screen reader always reach it
	if !strings.Contains(pageCSS, ".outline:not(.open):not(:focus-within){clip:rect(0 0 0 0);clip-path:inset(50%)") {
		t.Error("the closed outline must be clipped off-screen, not hidden")
	}
	if m := regexp.MustCompile(`\.outline[^{]*\{[^}]*display:none`).FindString(pageCSS); m != "" {
		t.Errorf("the outline is display:none somewhere: %q", m)
	}
}

// Text painted over the map (a plot label, a transition's tldr) sits on its own 1 px outline colour, not on a palette
// surface, so those are the pairs that must reach AA; the cursor's signs sit on the --plate token instead; the pressed-button
// ink is a literal too.
func TestPaintedTextContrastAA(t *testing.T) {
	light, dark := palettes(t)
	const outline = "#3a2410"
	for _, fg := range []string{"#fbf6e4", "#ffe680"} {
		if !strings.Contains(pageCSS, "color:"+fg) {
			t.Errorf("label colour %s is not in the stylesheet any more; update this test", fg)
		}
		if r := contrast(fg, outline); r < 4.5 {
			t.Errorf("label %s on its outline %s is %.2f:1, want >= 4.5", fg, outline, r)
		}
	}
	for name, p := range map[string]map[string]string{"light": light, "dark": dark} {
		// plan signs and phase rows sit on a plate (gd-kIUS2y1z7I): its ink, and the gold of a plot that needs you
		for _, fg := range []string{p["plate-ink"], "#ffe680"} {
			if r := contrast(fg, p["plate"]); fg == "" || p["plate"] == "" || r < 4.5 {
				t.Errorf("%s: label %q on --plate %q is %.2f:1, want >= 4.5", name, fg, p["plate"], r)
			}
		}
		if r := contrast("#2a1d0c", p["accent"]); r < 4.5 {
			t.Errorf("%s: pressed-button ink #2a1d0c on --accent %s is %.2f:1", name, p["accent"], r)
		}
	}
}

// The focus ring on a hot link is two-tone (gold outside a dark band) because it lands on any ground: wild grass in both schemes
// and the eight biome palettes. The two bands contrast with each other, and on every ground at least one band reaches the 3:1
// non-text minimum (WCAG 1.4.11). Skipped when node is not installed (the biome palettes live in art.js).
func TestFocusRingContrast(t *testing.T) {
	const gold, dark = "#ffe680", "#2a1a14"
	if !strings.Contains(pageCSS, ".hot:focus-visible,.hot.cur{outline:calc(2px / var(--z,1)) solid "+gold) || !strings.Contains(pageCSS, "box-shadow:0 0 0 calc(2px / var(--z,1)) "+dark) {
		t.Fatal("the hot focus ring is no longer the two-tone gold/dark ring; update this test with the new colours")
	}
	if r := contrast(gold, dark); r < 3 {
		t.Errorf("ring bands %s on %s are %.2f:1", gold, dark, r)
	}
	light, darkPal := palettes(t)
	grounds := map[string]string{"light --grass": light["grass"], "dark --grass": darkPal["grass"]}
	if node, err := exec.LookPath("node"); err == nil {
		art, _ := filepath.Abs(filepath.Join("web", "art.js"))
		out, err := exec.Command(node, "-e", `var A=require(process.argv[1]),o={};[false,true].forEach(function(n){for(var i=0;i<8;i++){var p=A.biomePalette(n,i);o[(n?"night":"day")+" biome "+i+" g"]=p.g;o[(n?"night":"day")+" biome "+i+" d"]=p.d;}});o["day wild"]=A.palette(false).g;o["night wild"]=A.palette(true).g;o["day lane"]=A.palette(false).P;console.log(JSON.stringify(o));`, art).CombinedOutput()
		if err != nil {
			t.Fatalf("palette dump failed: %v\n%s", err, out)
		}
		var m map[string]string
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("palette dump: %v\n%s", err, out)
		}
		for k, v := range m {
			grounds[k] = v
		}
		// by day the band outside the gold is dark too, as the gold alone is 1.3:1 on the lane round a home cell or a plot
		// (gd-y7dymfevBP): the ::before under the ring, which night drops (the gold reads on the night lane)
		if r := contrast(dark, m["day lane"]); r < 3 {
			t.Errorf("the ring's outer band %s on the day lane %s is %.2f:1, want >= 3", dark, m["day lane"], r)
		}
	}
	for _, rule := range []string{".hot:focus-visible::before,.hot.cur::before{content:\"\";position:absolute;inset:calc(-6px / var(--z,1));border:calc(2px / var(--z,1)) solid " + dark,
		":root[data-sky=night] .hot::before{content:none}", "@media (prefers-color-scheme:dark){:root:not([data-sky=day]) .hot::before{content:none}}"} {
		if !strings.Contains(pageCSS, rule) {
			t.Errorf("the ring's day-only outer band changed; update this test: no %q", rule)
		}
	}
	for name, g := range grounds {
		if g == "" {
			t.Errorf("no colour for %s", name)
			continue
		}
		if a, b := contrast(gold, g), contrast(dark, g); a < 3 && b < 3 {
			t.Errorf("on %s %s neither ring band reaches 3:1 (gold %.2f, dark %.2f)", name, g, a, b)
		}
	}
	// the splash's controls, the time's and the view's buttons (gd-Xs9LA8ONjn), wear a two-tone ring, white outside dark, seen
	// on the noon sky and the night alike
	ring := regexp.MustCompile(`#splash-time :focus-visible, #splash-view :focus-visible \{ outline: 2px solid var\(--hud-thumb\); outline-offset: 2px; box-shadow: 0 0 0 2px var\(--hud-ring\); \}`)
	if !ring.MatchString(splashCSS) {
		t.Fatal("the splash's controls lost their two-tone focus ring (white outline over a dark band); update this test with the new ring")
	}
	if r := contrast(splashTok(t, "hud-thumb"), splashTok(t, "hud-ring")); r < 3 {
		t.Errorf("the splash's focus ring bands %s and %s are %.2f:1", splashTok(t, "hud-thumb"), splashTok(t, "hud-ring"), r)
	}
}

// mediaBlock returns the body of the first `@media <q>{...}` block in css and css with that block removed.
func mediaBlock(css, q string) (inner, rest string) {
	start := strings.Index(css, "@media "+q+"{")
	if start < 0 {
		return "", css
	}
	i, depth := start+len("@media "+q+"{"), 1
	for ; i < len(css) && depth > 0; i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return css[start+len("@media "+q+"{") : i-1], css[:start] + css[i:]
}

// Every animation on the page is off under prefers-reduced-motion (design section 8): a CSS animation is either declared inside
// a no-preference block or switched off for the same selector in the reduce block, and the three script-driven motions (fly-to,
// the "needs you" ring pulse, the sprite loop) all gate on the `reduce` flag, which is read live.
func TestReducedMotionCoversEveryAnimation(t *testing.T) {
	_, css := mediaBlock(pageCSS, "(prefers-reduced-motion:no-preference)")
	reduce, css := mediaBlock(css, "(prefers-reduced-motion:reduce)")
	if reduce == "" {
		t.Fatal("no prefers-reduced-motion:reduce block")
	}
	rule := regexp.MustCompile(`([^{}@;]+)\{([^{}]*)\}`)
	for _, m := range rule.FindAllStringSubmatch(css, -1) {
		sel, body := strings.TrimSpace(m[1]), m[2]
		if !strings.Contains(body, "animation:") || strings.Contains(body, "animation:none") || strings.Contains(sel, "%") || sel == "from" || sel == "to" {
			continue
		}
		if !strings.Contains(reduce, sel+"{animation:none}") {
			t.Errorf("%s animates but the reduce block does not switch it off", sel)
		}
	}
	for _, want := range []string{
		"window.matchMedia('(prefers-reduced-motion: reduce)')", "onMq(reduceMq,",
		"if(reduce||!(ms>0)){setCam(to);return;}",             // fly-to is a jump cut
		"(pulse&&!reduce&&frame%2)",                           // the ring does not pulse (scene.js)
		"if(reduce||document.hidden||!world||level<3)return;", // no sprite loop
	} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js lost the reduced-motion gate %q", want)
		}
	}
}

// Coarse pointers get 44 CSS px targets (design section 8): the camera buttons and outline rows grow, every hot link gets a
// centred 44 px hit box sized in world px (so it stays 44 px on screen at any zoom), and one-tile sprites too small to tap leave
// the DOM (WORLD.wantHot, tested under node) so their plot stands in.
func TestCoarsePointerTargets(t *testing.T) {
	coarse := ""
	rest := pageCSS
	for {
		var b string
		b, rest = mediaBlock(rest, "(pointer:coarse)")
		if b == "" {
			break
		}
		coarse += b
	}
	for _, want := range []string{
		`.hot::after{content:"";position:absolute;left:50%;top:50%;width:max(100%,calc(44px / var(--z,1)));height:max(100%,calc(44px / var(--z,1)));transform:translate(-50%,-50%)}`,
		`.cam .cb,.menu-list .mi{min-width:44px;min-height:44px}`, `.outline .orow{min-height:44px}`, `.btn,.plans-slot .btn.small,`,
	} {
		if !strings.Contains(coarse, want) {
			t.Errorf("pointer:coarse block missing %s", want)
		}
	}
	if !strings.Contains(pageWorldJS, "var MIN_HOT = 44") || !strings.Contains(pageAppJS, "WORLD.wantHot(h,level,cam.z,coarse)") {
		t.Error("the 44 px hot policy is not wired: world.js MIN_HOT / garden.js hotWanted")
	}
	// the splash: the scrubber, the time's buttons and the view's (gd-Xs9LA8ONjn)
	splash := splashCSS[strings.Index(splashCSS, "@media (pointer: coarse)"):]
	for _, want := range []string{"#splash-scrub { height: 44px;", "#splash-time button, #splash-view button { min-width: 44px; min-height: 44px; }"} {
		if !strings.Contains(splash, want) {
			t.Errorf("the splash's pointer: coarse block is missing %s", want)
		}
	}
}

// The night palette is declared twice (gd-SwRqFcjWc6): once under prefers-color-scheme:dark for a dark OS (and a page with
// no script), once under html[data-sky=night] for the clock and the Menu's Sky item. A token edited in one block and not the
// other would give the two nights different colours; the pairs above are only checked on the first. Both selectors are
// pinned too: the media block must yield to a pinned day, and the pinned night must not need the media query.
func TestNightTokensAgree(t *testing.T) {
	tok := regexp.MustCompile(`--([a-z-]+):([^;}]+)`)
	block := func(start string) map[string]string {
		i := strings.Index(pageCSS, start)
		if i < 0 {
			t.Fatalf("garden.css has no %q block", start)
		}
		s := pageCSS[i+len(start):]
		s = s[:strings.Index(s, "}")]
		m := map[string]string{}
		for _, x := range tok.FindAllStringSubmatch(s, -1) {
			m[x[1]] = x[2]
		}
		if !strings.Contains(s, "color-scheme:dark") {
			t.Errorf("%s block does not set color-scheme:dark (form controls and scrollbars would stay light)", start)
		}
		return m
	}
	os := block("@media (prefers-color-scheme:dark){:root:not([data-sky=day]){")
	pinned := block(":root[data-sky=night]{")
	if len(os) == 0 || len(pinned) == 0 {
		t.Fatalf("empty night palette: os %d pinned %d tokens", len(os), len(pinned))
	}
	for k, v := range os {
		if pinned[k] != v {
			t.Errorf("--%s is %q under prefers-color-scheme:dark but %q under [data-sky=night]", k, v, pinned[k])
		}
	}
	for k := range pinned {
		if _, ok := os[k]; !ok {
			t.Errorf("--%s is set under [data-sky=night] only", k)
		}
	}
	// the light :root block must come first and the pinned night block after the media block, or palettes() (the AA
	// checks above) would read night values as light ones
	if strings.Index(pageCSS, ":root[data-sky=night]{") < strings.Index(pageCSS, "@media (prefers-color-scheme:dark)") {
		t.Error("the [data-sky=night] block must follow the prefers-color-scheme block")
	}
}

// The Glass, the Wall and the credential pool draw their state colours from the --box-* tokens, never a literal of their own
// (gd-ckee0fXmqy, rubric C5 "untokened colour"): a palette edit then reaches every chip, meter and frame. The chips sit on the
// always-dark box in both themes, so each state colour must hold 3:1 there as a dot or a border, and --box-ask 4.5:1 as the
// look-only tag's text.
func TestGlassWallColoursAreTokens(t *testing.T) {
	rule := regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	lit := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(`)
	glass := regexp.MustCompile(`\.(gl|gc|wl)-`)
	seen := 0
	for _, m := range rule.FindAllStringSubmatch(pageCSS, -1) {
		sel := m[1]
		if strings.Contains(sel, ":root") || !glass.MatchString(sel) {
			continue
		}
		seen++
		if x := lit.FindString(m[2]); x != "" {
			t.Errorf("%s uses the literal colour %s; use a --box-* or --wl-* token", strings.TrimSpace(sel), x)
		}
	}
	if seen < 20 {
		t.Fatalf("found only %d Glass/Wall rules in garden.css; has the stylesheet been restructured?", seen)
	}
	light, dark := palettes(t)
	for name, p := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, k := range []string{"box-ok", "box-warn", "box-bad", "box-ask"} {
			if p[k] == "" {
				t.Errorf("%s: token --%s missing", name, k)
				continue
			}
			for _, bg := range []string{"box", "box-edge"} {
				if r := contrast(p[k], p[bg]); r < 3 {
					t.Errorf("%s: --%s %s on --%s %s is %.2f:1, want >= 3", name, k, p[k], bg, p[bg], r)
				}
			}
		}
		if p["box-ask"] == "" {
			continue
		}
		if r := contrast(p["box-ask"], p["box"]); r < 4.5 {
			t.Errorf("%s: --box-ask text on --box is %.2f:1, want >= 4.5", name, r)
		}
	}
}

// The splash (plan gd-EJGA3zDGCk step 10) to a screen reader and a keyboard: one <main> under a visually hidden <h1>, the canvas an image with a name (splash.js
// writes the picture's own words over it once the scene draws, testdata/splash_test.js), the still's alt, the time controls a
// named group whose button and scrubber each have a name, the view's buttons (gd-Xs9LA8ONjn) a named group between the menu and
// the time with a name each, through which the keyboard reaches the view as the canvas takes no focus, and the readout's and
// the buttons' text at AA on their smoked chips over the brightest thing the scene puts behind them, a white daisy. Their 44 px
// targets are TestCoarsePointerTargets's and their focus ring TestFocusRingContrast's.
func TestSplashA11y(t *testing.T) {
	mux := http.NewServeMux()
	(&Handler{}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden/splash", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`<html lang="en">`, `<title>Gardenio</title>`, "<main>\n<h1 class=\"sr\">Gardenio</h1>", `</main>`, `<canvas id="splash" role="img" aria-label="The gardenio stone on its hill`,
		`<div id="splash-time" role="group" aria-label="`, `<input id="splash-scrub" type="range"`, `aria-label="Time of day"`,
		`<div id="splash-view" role="group" aria-label="The view" hidden>`, `id="splash-in" type="button" aria-label="Zoom in"`,
		`id="splash-out" type="button" aria-label="Zoom out"`, `id="splash-reset" type="button" aria-label="Reset view"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("splash missing %s", want)
		}
	}
	if m := regexp.MustCompile(`<img [^>]*alt="([^"]+)"`).FindStringSubmatch(body); m == nil || len(m[1]) < 20 {
		t.Errorf("the still has no alt that says the picture: %v", m)
	}
	if m := regexp.MustCompile(`<canvas[^>]*>`).FindString(body); strings.Contains(m, "tabindex") {
		t.Errorf("the canvas takes focus: %s", m)
	}
	if v, ti, mn := strings.Index(body, `id="splash-view"`), strings.Index(body, `id="splash-time"`), strings.Index(body, `id="splash-menu"`); !(mn < v && v < ti) {
		t.Error("the view's buttons are not between the start menu and the time controls in the source, so Tab misses that order")
	}
	buttons := regexp.MustCompile(`<button[^>]*>([^<]*)</button>`).FindAllStringSubmatch(body, -1)
	if len(buttons) != 5 {
		t.Errorf("want Reset view, Zoom in, Zoom out, Play and Now, got %d buttons", len(buttons))
	}
	for _, b := range buttons {
		if strings.TrimSpace(b[1]) == "" && !strings.Contains(b[0], `aria-label="`) { // Reset view's mark is drawn by CSS
			t.Errorf("a button with no name: %s", b[0])
		}
	}

	// held sideways, a notch or the home bar must not cover the readout, the buttons or the scrubber's ends (gd-gE31BjFbnt)
	for _, sel := range []string{"#splash-time", "#splash-view"} {
		for _, inset := range []string{"safe-area-inset-left", "safe-area-inset-right", "safe-area-inset-bottom"} {
			if sel == "#splash-view" && inset == "safe-area-inset-left" {
				continue // the view's column stands at the right
			}
			if !regexp.MustCompile(sel + ` \{[^}]*` + inset).MatchString(splashCSS) {
				t.Errorf("the splash's %s ignores env(%s)", sel, inset)
			}
		}
	}
	if chip := splashChip(t); contrast(splashTok(t, "hud-ink"), chip) < 4.5 {
		t.Errorf("the controls' text %s on their chip over white (%s) is %.2f:1, want 4.5", splashTok(t, "hud-ink"), chip, contrast(splashTok(t, "hud-ink"), chip))
	}
}

// splashTok is the value of splash.css's --name.
func splashTok(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`--` + name + `: ([^;]+);`).FindStringSubmatch(splashCSS)
	if m == nil {
		t.Fatalf("no --%s in splash.css", name)
	}
	return m[1]
}

// splashChip is the splash's smoked chip (--hud, translucent) laid on white, the worst case for its light text, as #rrggbb.
func splashChip(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`rgba\((\d+), (\d+), (\d+), ([0-9.]+)\)`).FindStringSubmatch(splashTok(t, "hud"))
	if m == nil {
		t.Fatalf("--hud is not an rgba(): %s", splashTok(t, "hud"))
	}
	a, _ := strconv.ParseFloat(m[4], 64)
	chip := "#"
	for _, c := range m[1:4] {
		v, _ := strconv.ParseFloat(c, 64)
		chip += strconv.FormatInt(int64(math.Round(v*a+255*(1-a)))|0x100, 16)[1:]
	}
	return chip
}
