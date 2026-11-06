package gardenhub

import (
	"strings"
	"testing"
)

// The map is the page (gd-rdMNXor7Ip): the body is exactly the viewport and never scrolls, the board fills <main> with no
// aspect-ratio box, the header floats over the top of the map, and the Plans panel, the legend and keys, and the Porch/Mailbox
// dock are <details> drawers inside the map window (so they open with no script). Measured in Chrome at 1707x842, 1280x720
// and 390x844: document scrollHeight == innerHeight and the board is 100% of the viewport.
func TestPageFillsTheViewport(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	for _, want := range []string{
		`body{margin:0;height:100vh;height:100dvh;overflow:hidden;`,
		`.farm,.left,.scene-wrap{position:absolute;inset:0}`,
		`.board{position:absolute;inset:0;`,
		`body.view-farm .main{overflow:hidden}`,
		`body.view-farm .crumb{position:absolute;top:0;left:0;right:0;`,
		`.cam{position:absolute;z-index:6;top:calc(var(--bar) + 8px);`,
		`.outline{position:absolute;z-index:8;left:8px;top:calc(var(--bar) + 8px);`,
		`.jump{position:absolute;z-index:8;top:calc(var(--bar) + 8px);`,
		`.dw{position:absolute;z-index:7;`, `.dw[open]{display:flex;flex-direction:column;`,
		`.dbody{flex:1;min-height:0;overflow:auto;`,
	} {
		if !strings.Contains(pageCSS, want) {
			t.Errorf("garden.css missing %s", want)
		}
	}
	if strings.Contains(pageCSS, "aspect-ratio") {
		t.Error("the board must not be an aspect-ratio box any more")
	}
	for _, want := range []string{
		`<details class="dw dw-plans" id="dw-plans">`, `<details class="dw dw-help" id="dw-help">`, `<div class="dw dw-dock" id="dw-dock" hidden>`,
		`<summary class="dsum">Plans</summary>`, `id="dpip-porch"`, `id="dpip-mail"`,
		`<button class="btn dbtn" id="dbtn-porch" type="button" data-pane="porch" aria-expanded="false" aria-controls="dw-dock">`,
		`<button class="btn dbtn" id="dbtn-mail" type="button" data-pane="mail" aria-expanded="false" aria-controls="dw-dock">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	// each drawer wraps its panel: the plans slot, the legend + keys, and the dock
	for _, pair := range [][3]string{{`id="dw-plans"`, `id="plans-slot"`, "</details>"}, {`id="dw-help"`, `id="legend"`, "</details>"}, {`id="dw-dock"`, `id="dock"`, "</aside>"}} {
		i, j := strings.Index(body, pair[0]), strings.Index(body, pair[1])
		k := strings.Index(body[i:], pair[2]) + i
		if i < 0 || j < i || j > k {
			t.Errorf("%s is not inside the %s drawer", pair[1], pair[0])
		}
	}
	// the dock repeats nothing: its two buttons are the only Porch and Mailbox controls on a desktop (3b R5)
	for _, gone := range []string{`class="dock-tabs"`, `id="tab-porch"`, `id="tab-mail"`, `id="psub-porch"`} {
		if strings.Contains(body, gone) {
			t.Errorf("page still has %s", gone)
		}
	}
	// phone: the drawers are sheets between the header and the tab bar, and the camera strip leaves the top of the map clear
	phone, rest := "", pageCSS
	for {
		var b string
		b, rest = mediaBlock(rest, "(max-width:899px)")
		if b == "" {
			break
		}
		phone += b
	}
	for _, want := range []string{
		`.dw[open],.dw-dock{left:0;right:0;top:var(--bar);bottom:calc(69px + env(safe-area-inset-bottom,0px));`,
		`.cam{top:auto;left:auto;right:8px;bottom:var(--cam-b);--cam-b:calc(84px + env(safe-area-inset-bottom,0px));`, `body[data-screen=farm] .dw-dock{display:none}`, `.dbtns{display:none}`,
	} {
		if !strings.Contains(phone, want) {
			t.Errorf("phone block missing %s", want)
		}
	}
	// garden.js: the dock drawer opens for a phone screen or a #mail link, its pips are kept, and the header's height is --bar
	for _, want := range []string{"if(dd&&s!=='farm')dd.hidden=false;", "'dpip-porch'", "'dpip-mail'", "style.setProperty('--bar',hdr.offsetHeight+'px')", "style.setProperty('--camw',camRow.offsetWidth+'px')"} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js missing %s", want)
		}
	}
}

// Text lives inside the window (gd-YJI2vPZwp4): on the farm the header is a HUD inside the map, not a bar. The header box is
// transparent and lets drags through; the title plaque carries the title, the counts and the live state, and keeps the counts
// on a phone (they were hidden there). The phone tab bar floats over the map too, and outline rows never break mid-word.
func TestTextLivesInsideTheWindow(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	for _, want := range []string{
		`body.view-farm .crumb{position:absolute;top:0;left:0;right:0;z-index:7;align-items:flex-start;padding:8px 8px 0;background:transparent;border:0;pointer-events:none}`,
		`body.view-farm .crumb>:not(.sp){pointer-events:auto}`,
		`body.view-farm .plaque{background:var(--surface);border:2px solid var(--edge);`,
		`.ot{font:700 13px/1.3 var(--font-display);overflow-wrap:break-word;`,
		`.os{color:var(--muted);font-size:12px;overflow-wrap:break-word;`,
		`@media (max-width:899px){.outline{bottom:calc(84px + env(safe-area-inset-bottom,0px))}}`,
	} {
		if !strings.Contains(pageCSS, want) {
			t.Errorf("garden.css missing %s", want)
		}
	}
	if strings.Contains(pageCSS, "overflow-wrap:anywhere}") && strings.Contains(pageCSS, ".os{color:var(--muted);font-size:12px;overflow-wrap:anywhere") {
		t.Error("outline counts must not break mid-word ('0 ste ps')")
	}
	// the title, the counts and the live state are one plaque inside the header
	i, j := strings.Index(body, `<div class="plaque">`), strings.Index(body, `<div class="sp"></div>`)
	for _, id := range []string{`<h1>The Garden</h1>`, `id="sub"`, `id="live"`} {
		k := strings.Index(body, id)
		if i < 0 || k < i || k > j {
			t.Errorf("%s is not on the title plaque", id)
		}
	}
	phone, rest := "", pageCSS
	for {
		var b string
		b, rest = mediaBlock(rest, "(max-width:899px)")
		if b == "" {
			break
		}
		phone += b
	}
	if strings.Contains(phone, ".sub{display:none}") {
		t.Error("the phone hides the counts; they belong on the plaque")
	}
	for _, want := range []string{`.sub .lw{display:none}`, `body.view-farm .tabbar{background:transparent;border-top:0;pointer-events:none}`, `.tabbar button{pointer-events:auto;`} {
		if !strings.Contains(phone, want) {
			t.Errorf("phone block missing %s", want)
		}
	}
	if !strings.Contains(pageAppJS, `<span class="lw">ready </span>to harvest · `) {
		t.Error("garden.js: the counts line lost its phone-droppable words")
	}
}

// One view with script, the farm (3b R4, gd-ynRqThFqwE): the header has no Farm/Ledger toggle and garden.js no route to the
// Ledger. The Ledger is the page without script, so it must show its rows there: the body is not served as scope-mine (only
// garden.js tags an item .mine, so that class would hide every row), and the Mine / Everyone buttons, which need script, hide.
// So does everything else garden.js drives (gd-LJBuckU7SD): the plaque's live state, which would read "connecting" forever,
// the tab bar's buttons, and the section counts, which only counts() fills; an empty list shows its empty line by itself,
// since the .none class that shows it is counts()'s too; and a row's link gets the Porch rows' 44 px hit slop on a touch screen.
func TestLedgerIsTheNoScriptPage(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	for _, gone := range []string{`id="b-farm"`, `id="b-ledger"`, `aria-label="View"`, `class="scope-mine`, ` scope-mine `} {
		if strings.Contains(body, gone) {
			t.Errorf("page still has %s", gone)
		}
	}
	for _, gone := range []string{"setView", "garden.view", "view-ledger"} {
		if strings.Contains(pageAppJS, gone) {
			t.Errorf("garden.js still has %s", gone)
		}
	}
	if strings.Contains(pageCSS, "view-ledger") {
		t.Error("garden.css still styles body.view-ledger")
	}
	for _, want := range []string{`.crumb .seg{display:none !important}`, `.ledger{display:grid !important}`, `<div class="grid ledger" id="ledger">`,
		`.live{display:none !important}`, `.tabbar{display:none !important}`, `.ledger .n{display:none !important}`, `.ledger .list:empty+.empty{display:block !important}`,
		`.ledger .item>.t::after{content:"";position:absolute;left:0;right:0;top:-12px;bottom:-14px}`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	// :empty matches only a list with nothing in it at all, not even whitespace around the server-rendered rows
	for _, sec := range []string{"decision", "work", "agent"} {
		if want := `id="l-` + sec + `">{{.Lists.` + sec + `}}</ul>`; !strings.Contains(pageHTML, want) {
			t.Errorf("garden.html: the %s list is not tight around its rows (want %s)", sec, want)
		}
	}
}
