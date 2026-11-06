package gardenhub

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The witness (web/witness.js, gd-TNC80dPgv3) is on the farm page, in its bundle before garden.js, and off until a
// browser is switched on with /garden?witness=1: a phone "refresh" nobody can see in emulation is named by the page itself.
func TestWitnessWired(t *testing.T) {
	if _, ok := pageSources["witness.js"]; !ok {
		t.Fatal("witness.js is not a page source")
	}
	farm, err := bundle(pageHTML, "page-farm.js", pageSources)
	if err != nil {
		t.Fatal(err)
	}
	w, g := strings.Index(farm, "var WITNESS = (function () {"), strings.Index(farm, "var DPR_MAX=")
	if w < 0 || g < 0 || w > g {
		t.Errorf("witness.js is not in page-farm.js before garden.js (witness at %d, garden at %d)", w, g)
	}
	src := stripJSComments(pageSources["witness.js"])
	for _, bad := range []string{"innerHTML", "fetch(", "XMLHttpRequest"} {
		if strings.Contains(src, bad) {
			t.Errorf("witness.js carries %q: it says what it saw on the page and sends nothing anywhere", bad)
		}
	}
}

// Its pure half under node: the switch reads the query first and the stored flag after; a boot says how the page came
// back, whether it was closed first, and what it was doing.
func TestWitnessLogicUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `var W=require(process.argv[1]),a=require('assert');
a.strictEqual(W.flag('?witness=1',''),'on');a.strictEqual(W.flag('?at=x&witness=on','off'),'on');a.strictEqual(W.flag('?witness=0','on'),'off');
a.strictEqual(W.flag('','on'),'on');a.strictEqual(W.flag('?at=x','off'),'');a.strictEqual(W.flag('',null),'');
var b=W.boot(null,'navigate',1000);a.strictEqual(b.boots,1);a.ok(/first load/.test(b.say),b.say);
b=W.boot({boots:1,hid:true,at:1000,cam:{level:0,z:0.04},log:['1 window 393x870 @L0 z0.04','2 hidden @L0 z0.04']},'reload',10000);
a.strictEqual(b.boots,2);a.ok(/load #2, reloaded, the page was closed first/.test(b.say),b.say);a.ok(/9s ago at level 0 z 0.04; last events: window 393x870 @L0 z0.04 \/ hidden @L0 z0.04$/.test(b.say),b.say);
b=W.boot({boots:2,hid:false},'back_forward',0);a.ok(/load #3, came back through history, the page was NOT closed first: the engine killed it/.test(b.say),b.say);
b=W.boot({boots:3,hid:false},'',0);a.ok(/loaded again \(navigate\)/.test(b.say),b.say);
console.log('ok');`
	src, _ := filepath.Abs(filepath.Join("web", "witness.js"))
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("witness.js under node: %v\n%s", err, out)
	}
}

// In Chromium on the phone profile: off by default, on with ?witness=1 (the first load says so, a resize is said with the
// camera, a reload says it is load #2, reloaded and closed first, with the last event), off again with ?witness=0. What is
// asserted lives in testdata/witness_browser.js.
func TestWitnessUnderChromium(t *testing.T) {
	underChromium(t, "witness_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	})
}
