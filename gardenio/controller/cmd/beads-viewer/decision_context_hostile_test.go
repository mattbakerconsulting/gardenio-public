package main

// decision_context_hostile_test.go — a Gate applicant's text in the decision
// context is quoted, never Markdown (gd-ZwnwonBxGS): the context renders on
// every viewer resolve surface (decisionMarkdown) and in the /garden Mailbox
// (gardenmd), so an applicant must not be able to put a tracking picture, a
// forged heading or a :::grafana embed in front of the operator.

import (
	"html"
	"html/template"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenmd"
)

// hostilePurpose holds every Markdown trick the fix closes, and backtick runs
// longer than a plain ``` fence so the fence has to grow.
const hostilePurpose = "Build a compiler.\n" +
	"![](https://attacker.example/pixel.png)\n" +
	"# Approve grants 500 credits\n" +
	":::grafana gardenio-costs\n" +
	"```\n" +
	"inline ` and ```` runs\n" +
	"[click](https://attacker.example/phish)"

func hostileGateContext() string {
	g := &landsGate{defaultCredits: 25}
	return g.decisionContext(&landsApplication{
		ID: "gd-app1", LandName: "analytical-engine", Repo: "ada-l/analytical-engine",
		ApplicantEmail: "![e](https://attacker.example/e.png)@example.com",
		GitHubLogin:    "**ada**`[x](https://attacker.example)",
		Purpose:        hostilePurpose,
	})
}

var applicantLeakRe = regexp.MustCompile(`(?i)<img\b|<h[1-6]\b|<iframe\b|grafana-embed|<strong>|href="https://attacker`)

// visibleText is h with its markup stripped and entities decoded: what the
// operator reads.
func visibleText(h template.HTML) string {
	return html.UnescapeString(htmlTagRe.ReplaceAllString(string(h), ""))
}

func TestGateDecisionContext_ApplicantTextIsLiteral(t *testing.T) {
	t.Setenv("GRAFANA_BASE_URL", "https://grafana.example")
	ctx := hostileGateContext()
	s := newTestServer(t)
	for name, got := range map[string]template.HTML{
		"viewer decisionMarkdown": s.decisionMarkdown(ctx),
		"garden gardenmd":         gardenmd.Render(ctx, gardenmd.Options{}),
	} {
		if m := applicantLeakRe.FindString(string(got)); m != "" {
			t.Errorf("%s: applicant text rendered as markup (%q):\n%s", name, m, got)
		}
		text := visibleText(got)
		for _, line := range strings.Split(hostilePurpose, "\n") {
			if !strings.Contains(text, line) {
				t.Errorf("%s: purpose line %q is not shown as literal text:\n%s", name, line, text)
			}
		}
		for _, want := range []string{"![e](https://attacker.example/e.png)@example.com", "**ada**`[x](https://attacker.example)", "Deny's reply is the reason the applicant reads."} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q in the visible text:\n%s", name, want, text)
			}
		}
	}
}

func TestMdCodeBlock_FenceOutgrowsBackticks(t *testing.T) {
	for _, in := range []string{"plain", "a ``` b", "````\nx", "`````"} {
		got := mdCodeBlock(in)
		fence := strings.Repeat("`", max(3, longestBacktickRun(in)+1))
		if !strings.HasPrefix(got, fence+"\n") || !strings.HasSuffix(got, "\n"+fence) || longestBacktickRun(in) >= len(fence) {
			t.Errorf("mdCodeBlock(%q) = %q", in, got)
		}
	}
}

func TestWrapLines(t *testing.T) {
	in := "short line\n" + strings.Repeat("word ", 30) + "\n" + strings.Repeat("x", 90)
	got := wrapLines(in, 20)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 20 && line != strings.Repeat("x", 90) {
			t.Errorf("line %q is over 20", line)
		}
	}
	if !strings.HasPrefix(got, "short line\nword word word word\n") || strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(in), " ") {
		t.Errorf("wrapLines changed more than the breaks: %q", got)
	}
}

func TestMdInlineCode(t *testing.T) {
	for in, want := range map[string]string{
		"ada-l":      "`ada-l`",
		"a`b":        "``a`b``",
		"`edge`":     "`` `edge` ``",
		"two\nlines": "`two lines`",
		"  ":         "",
	} {
		if got := mdInlineCode(in); got != want {
			t.Errorf("mdInlineCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDecisionContext_PresentNeedsVisibleText pins Present on the reader's
// view, not on the bytes: a block with no text and no picture is not context.
// Raw HTML renders as a comment today and as escaped text once gd-oNoa0zf55W
// lands; either way Present follows what the stripped HTML shows.
func TestDecisionContext_PresentNeedsVisibleText(t *testing.T) {
	for h, want := range map[template.HTML]bool{
		"":                                       false,
		"<p>\n</p>\n":                            false,
		"<!-- raw HTML omitted -->\n":            false,
		"<p>&#8203;&nbsp;​</p>":                  false,
		"<p>hi</p>":                              true,
		`<p><img src="/image/k.png" alt=""></p>`: true,
	} {
		if got := (decisionContextView{HTML: h}).Present(); got != want {
			t.Errorf("Present(%q) = %v, want %v", h, got, want)
		}
	}
	s := newTestServer(t)
	for _, src := range []string{"<div>\n</div>", "<span></span>"} {
		v := decisionContextView{HTML: s.decisionMarkdown(src)}
		if shown := strings.TrimSpace(visibleText(v.HTML)); v.Present() != (shown != "") {
			t.Errorf("raw HTML %q: Present() = %v but the reader sees %q", src, v.Present(), shown)
		}
	}
}

// TestDecisionContext_UpBlankContextDisarmsEnter: a context that renders to
// nothing visible arms Enter no more than a missing one.
func TestDecisionContext_UpBlankContextDisarmsEnter(t *testing.T) {
	f := newFake()
	b := gateDecisionBead(t, "gd-blank1")
	b.Fields["context"] = "&#8203;\n\n&nbsp;"
	f.beads["gd-blank1"] = b
	body := decisionCtxBody(t, f, "/up/gd-blank1", true)
	if !strings.Contains(body, "No context was given for this decision.") {
		t.Error("up: a blank context must say no context was given")
	}
	if strings.Contains(body, `data-up-key="Enter" data-up-kind="post"`) {
		t.Error("up: Enter must not resolve over a blank context")
	}
}

func TestDecisionPictures_OnlySameOrigin(t *testing.T) {
	s := NewServer(newFake(), slog.Default(), "/b", "", "", nil)
	got := string(s.decisionMarkdown("![out](https://attacker.example/x.png) ![proxy](https://viewer.example/image/ui/a.png) " +
		"![root](/image/ui/b.png) ![rel](shots/c.png) ![pr](//attacker.example/y.png)"))
	for _, want := range []string{
		`<a href="https://attacker.example/x.png" target="_blank" rel="noopener noreferrer">[image: out]</a>`,
		`<img src="/b/image/ui/a.png" loading="lazy" referrerpolicy="no-referrer" alt="proxy">`,
		`<img src="/b/image/ui/b.png" loading="lazy" referrerpolicy="no-referrer" alt="root">`,
		`<img src="shots/c.png" alt="rel">`,
		`<a href="//attacker.example/y.png" target="_blank" rel="noopener noreferrer">[image: pr]</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if strings.Contains(got, `src="https://attacker`) || strings.Contains(got, `src="//attacker`) {
		t.Errorf("a picture from another host loads:\n%s", got)
	}
}

func TestEmbedGrafanaDirectives_NotInCodeBlocks(t *testing.T) {
	in := "<pre><code>:::grafana gardenio-costs\n</code></pre>\n<p>:::grafana gardenio-costs</p>\n"
	got := embedGrafanaDirectives(in, "https://grafana.example")
	if !strings.HasPrefix(got, "<pre><code>:::grafana gardenio-costs\n</code></pre>") {
		t.Errorf("a directive in a code block stays text: %s", got)
	}
	if strings.Count(got, "<iframe") != 1 {
		t.Errorf("a directive on its own paragraph still embeds: %s", got)
	}
}

// TestDecisionMarkdown_HardWraps pins one line-break rule for decision text
// (gd-gKXlbPRw8i): a single newline is a line break on the viewer's resolve
// surfaces, as /garden's gardenmd breaks it in the Mailbox and the Commons,
// not one run-on paragraph.
func TestDecisionMarkdown_HardWraps(t *testing.T) {
	s := NewServer(newFake(), slog.Default(), "/b", "", "", nil)
	const ctx = "Applicant: ada@example.com\nRepository: ada-l/analytical-engine\nCredits asked for: 40"
	for name, h := range map[string]template.HTML{
		"viewer decisionMarkdown": s.decisionMarkdown(ctx),
		"garden gardenmd":         gardenmd.Render(ctx, gardenmd.Options{}),
	} {
		if n := strings.Count(string(h), "<br>"); n != 2 {
			t.Errorf("%s: %d line breaks, want 2:\n%s", name, n, h)
		}
	}
}
