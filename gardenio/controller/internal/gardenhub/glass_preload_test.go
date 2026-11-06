package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/oidcauth"
)

// The head preloads xterm on a Glass deep link (P1.5, gd-YU9gGOMvur): only on a page that offers the Glass, only for
// ?glass=<agent> by workbench.js panelOf's rule (the first panel parameter decides; ?glass= alone is the Wall), and from
// where glass.js loads it (its hashed asset, body data-xterm-*), so glass.js's own <link> and <script> take the preloaded responses.
func TestGlassDeepLinkPreloadsXterm(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	who := func(*http.Request) coopmuxapi.Identity {
		return coopmuxapi.Identity{Roles: []string{oidcauth.RoleOperator}}
	}
	page := func(h *Handler) string {
		mux := http.NewServeMux()
		h.Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		return rec.Body.String()
	}
	if body := page(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}); strings.Contains(body, `id="glass-preload"`) {
		t.Error("a page without the Glass preloads xterm")
	}
	body := page(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Glass: &Glass{Who: who}})
	if h, p := strings.Index(body, "</head>"), strings.Index(body, `id="glass-preload"`); p < 0 || p > h {
		t.Fatalf("the preload is not in the head: %d, head ends %d", p, h)
	}
	m := regexp.MustCompile(`(?s)<script id="glass-preload">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no glass-preload script")
	}

	// The script's panel kinds are workbench.js's KINDS, in its order.
	wb, err := os.ReadFile(filepath.Join("web", "workbench.js"))
	if err != nil {
		t.Fatal(err)
	}
	kinds := regexp.MustCompile(`var KINDS = (\[[^\]]*\]);`).FindStringSubmatch(string(wb))
	if kinds == nil || !strings.Contains(m[1], strings.ReplaceAll(kinds[1], " ", "")) {
		t.Errorf("the preload's panel kinds are not workbench.js's KINDS %v", kinds)
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "preload.js")
	if err := os.WriteFile(src, []byte(m[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
var fs = require("fs"), vm = require("vm"), assert = require("assert");
var src = fs.readFileSync(process.argv[1], "utf8");
function run(search) {
  var links = [];
  var doc = { createElement: function () { return {}; }, head: { appendChild: function (l) { links.push(l.rel + " " + l.as + " " + l.href); } } };
  vm.runInNewContext(src, { document: doc, location: { search: search } });
  return links;
}
var both = ["preload style /v/garden/assets/` + pageAssets.byName["xterm.css"].file + `", "preload script /v/garden/assets/` + pageAssets.byName["xterm.bundle.js"].file + `"];
assert.deepStrictEqual(run("?glass=gd-GlassLive01"), both);
assert.deepStrictEqual(run("?at=3,4&glass=gd-A1a"), both, "a non-panel parameter first");
assert.deepStrictEqual(run("?gl%61ss=gd-A1a"), both, "an encoded key");
assert.deepStrictEqual(run(""), []);
assert.deepStrictEqual(run("?glass="), [], "the Wall");
assert.deepStrictEqual(run("?glass"), [], "the Wall");
assert.deepStrictEqual(run("?glass=wall"), [], "the Wall");
assert.deepStrictEqual(run("?glass=+"), [], "a blank agent is the Wall");
assert.deepStrictEqual(run("?bead=gd-1&glass=gd-A1a"), [], "the first panel parameter decides");
assert.deepStrictEqual(run("?bead=&glass=gd-A1a"), [], "an empty first panel names none, as panelOf");
assert.deepStrictEqual(run("?glass=%E0%A4%A"), [], "a malformed value");
`
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
