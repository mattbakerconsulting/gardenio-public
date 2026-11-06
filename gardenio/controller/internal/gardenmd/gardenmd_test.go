package gardenmd

import (
	"html/template"
	"strings"
	"testing"
)

func TestRenderMarkdown(t *testing.T) {
	got := string(Render("# Why\n\nThe **plan** has:\n- one\n- `two`\nline one\nline two", Options{}))
	for _, want := range []string{"<h4>Why</h4>", "<strong>plan</strong>", "<li>one</li>", "<code>two</code>", "line one<br>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if Render("  \n ", Options{}) != "" {
		t.Error("blank text renders nothing")
	}
}

func TestRenderIsSafe(t *testing.T) {
	got := string(Render("<script>alert(1)</script>\n\nhi <img src=x onerror=alert(1)> [x](javascript:alert(1)) <b>b</b>", Options{}))
	for _, bad := range []string{"<script", "<img", "javascript:", "<b>"} {
		if strings.Contains(got, bad) {
			t.Errorf("unsafe %q survived: %s", bad, got)
		}
	}
	for _, want := range []string{"<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>", "&lt;b&gt;b&lt;/b&gt;", "&lt;img src=x onerror=alert(1)&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("raw HTML reads as its text: missing %q in %s", want, got)
		}
	}
	if got := string(Render("fill in <your-name> here", Options{})); got != "<p>fill in &lt;your-name&gt; here</p>\n" {
		t.Errorf("a placeholder is kept: %q", got)
	}
	// gd-oNoa0zf55W: the viewer's goldmark dropped this to "credits="
	if got := string(Render("set credits=<n> per run", Options{})); got != "<p>set credits=&lt;n&gt; per run</p>\n" {
		t.Errorf("an angle-bracket placeholder reads as its text, not dropped: %q", got)
	}
	if got := string(Render("<javascript:alert(1)> and [it](tel:+15550100)", Options{})); got != "<p>javascript:alert(1) and it</p>\n" {
		t.Errorf("a link the JS safeHref refuses reads as its words: %q", got)
	}
	// an autolink, a mixed-case, entity- or percent-encoded scheme and a picture's src never link or load a script or data URL
	for _, src := range []string{
		"<javascript:alert(1)>",
		"[x](JaVaScRiPt:alert(1))",
		"[x](&#106;avascript:alert(1))",
		"[x](java&#x73;cript:alert(1))",
		"[x](&#x6A;&#x61;&#x76;&#x61;&#x73;&#x63;&#x72;&#x69;&#x70;&#x74;&#x3A;alert(1))",
		"[x](%6Aavascript:alert(1))",
		"[x](vbscript:msgbox(1))",
		"[x](data:text/html;base64,PHNjcmlwdD4=)",
		"![x](javascript:alert(1))",
		"![x](data:image/svg+xml;base64,PHN2Zz4=)",
		"![x](https://evil.example/image/../x.png)",
		"[x](tel:+15550100)",
		"[x](vscode://file/etc/passwd)",
		"[x](//evil.example/x)",
	} {
		got := strings.ToLower(string(Render(src, Options{Base: "/b"})))
		for _, bad := range []string{`href="javascript`, `href="vbscript`, `href="data`, `href="tel`, `href="vscode`, `href="//`, `href=""`, "<img", "src="} {
			if strings.Contains(got, bad) {
				t.Errorf("%s: unsafe %q survived: %s", src, bad, got)
			}
		}
	}
}

func TestRenderLinks(t *testing.T) {
	got := string(Render("see [the PR](https://github.com/o/r/pull/1) and https://example.com/x and [here](/garden?plan=1)", Options{}))
	if !strings.Contains(got, `<a href="https://github.com/o/r/pull/1" target="_blank" rel="noopener noreferrer">the PR</a>`) {
		t.Errorf("a link out opens in a new tab: %s", got)
	}
	if !strings.Contains(got, `<a href="https://example.com/x" title="https://example.com/x" target="_blank" rel="noopener noreferrer">example.com/x</a>`) {
		t.Errorf("a bare URL is a link out: %s", got)
	}
	long := string(Render("CI: https://github.com/acme/app/actions/runs/123456789/job/987654321?pr=1", Options{}))
	if !strings.Contains(long, `>github.com/…/job/987654321</a>`) || !strings.Contains(long, `title="https://github.com/acme/app/actions/runs/123456789/job/987654321?pr=1"`) {
		t.Errorf("a long bare URL reads as its host and last segments, whole in its tooltip: %s", long)
	}
	if !strings.Contains(got, `<a href="/garden?plan=1">here</a>`) {
		t.Errorf("a link on the site stays in the tab: %s", got)
	}
}

func TestRenderBeadIDs(t *testing.T) {
	src := "blocked on gd-AbCdEfGhIj, see ka-0000001234 but not re-introduced, `gd-CodeSpan01`, agent/br/gd-PathSeg001 or [gd-InLink0001](https://x.y/z)"
	got := string(Render(src, Options{Base: "/b"}))
	for _, want := range []string{
		`<a class="bl" href="/b/garden?bead=gd-AbCdEfGhIj" data-bead="gd-AbCdEfGhIj" title="gd-AbCdEfGhIj">gd-AbCdEfGhIj</a>, see `,
		`data-bead="ka-0000001234"`, "re-introduced", "<code>gd-CodeSpan01</code>", "agent/br/gd-PathSeg001", ">gd-InLink0001</a>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, bad := range []string{`data-bead="gd-CodeSpan01"`, `data-bead="gd-PathSeg001"`, `data-bead="gd-InLink0001"`} {
		if strings.Contains(got, bad) {
			t.Errorf("%s must not be linked: %s", bad, got)
		}
	}
	custom := string(Render("a gd-AbCdEfGhIj\nb", Options{BeadLink: func(id string) template.HTML { return template.HTML("<i>" + id + "</i>") }}))
	if custom != "<p>a <i>gd-AbCdEfGhIj</i><br>\nb</p>\n" {
		t.Errorf("BeadLink renders the id, and the line break stays after it: %q", custom)
	}
}

func TestRenderPictures(t *testing.T) {
	got := string(Render("![chart](https://viewer.example/image/ui/a.png) ![x](https://evil.example/t.png) ![raw](https://b.s3.us-east-1.amazonaws.com/p/images/ui/c.png)", Options{Base: "/b"}))
	if !strings.Contains(got, `<img src="/b/image/ui/a.png" alt="chart" loading="lazy" decoding="async" referrerpolicy="no-referrer">`) {
		t.Errorf("a proxied picture loads through the proxy: %s", got)
	}
	if strings.Contains(got, `src="https://evil.example`) || !strings.Contains(got, `<a href="https://evil.example/t.png" target="_blank" rel="noopener noreferrer">picture: x</a>`) {
		t.Errorf("any other picture is a link: %s", got)
	}
	if !strings.Contains(got, `src="/b/image/p/images/ui/c.png"`) {
		t.Errorf("a raw S3 picture goes through the proxy: %s", got)
	}
	for _, u := range []string{"https://viewer.example/image/../garden/x", "/image/../garden?bead=gd-AbCdEfGhIj", "/image/%2e%2e/garden/x", "/image/a/../../x"} {
		if s := PicSrc("/b", u); s != "" {
			t.Errorf("PicSrc(%q) = %q, a same-origin src outside /image/", u, s)
		}
	}
	if s := PicSrc("/b", "https://viewer.example/image/ui/./a/../b.png"); s != "/b/image/ui/b.png" {
		t.Errorf("a path that stays under /image/ is cleaned and kept: %q", s)
	}
}

func TestBlockFolds(t *testing.T) {
	short := string(Block("ctx", "short", Options{}))
	if short != `<div class="ctx md"><p>short</p>`+"\n</div>" || strings.Contains(short, "md-more") {
		t.Errorf("short text does not fold: %q", short)
	}
	long := string(Block("ctx", strings.Repeat("word ", FoldAt/5+1), Options{}))
	if !strings.Contains(long, `class="ctx md md-fold"`) || !strings.Contains(long, `<details class="md-more"><summary>`) || !strings.Contains(long, "Show more") {
		t.Errorf("long text folds behind Show more: %q", long)
	}
	if Block("ctx", "", Options{}) != "" {
		t.Error("no text, no block")
	}
}
