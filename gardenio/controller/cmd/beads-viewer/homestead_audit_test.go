package main

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Contrast and motion audit, step 2.4 (gd-ixjDdayBz9). The audit ran axe and a
// pixel-sampled contrast check in Chromium on the ten busiest pages in both
// Homestead skins; these hold what it fixed.

// Under prefers-reduced-motion nothing animates in the Homestead skins, the
// pages' own transitions included: one !important net in the reduced-motion
// block, gated on the skins like every other rule.
func TestHomesteadCSS_ReducedMotionStopsEverything(t *testing.T) {
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(readHomesteadCSS(t), "")
	i := strings.Index(css, "@media (prefers-reduced-motion: reduce)")
	if i < 0 {
		t.Fatal("homestead.css has no prefers-reduced-motion block")
	}
	block := css[i:]
	block = block[:strings.Index(block, "\n}\n")+3]
	const gate = `html:is([data-skin="homestead"], [data-skin="dark-homestead"])`
	for _, want := range []string{
		gate + " *,\n",
		gate + " *::before,\n",
		gate + " *::after { animation-name: none !important; transition-property: none !important; scroll-behavior: auto !important; }",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the reduced-motion block lacks %q", want)
		}
	}
}

// The plate's paper and its bands are anchored to body, so a paint mid-parse
// moved them (0.5-0.85 CLS). The layout holds first paint until the marker at
// the end of body is parsed.
func TestLayout_RenderWaitsForTheWholeBody(t *testing.T) {
	b, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	layout := string(b)
	head := layout[:strings.Index(layout, "</head>")]
	if !strings.Contains(head, `<link rel="expect" href="#page-parsed" blocking="render">`) {
		t.Error(`layout.html's head must carry <link rel="expect" href="#page-parsed" blocking="render">`)
	}
	tail := layout[strings.LastIndex(layout, `{{block "content" .}}`):]
	if !strings.Contains(tail, `<i id="page-parsed" hidden></i>`+"\n</body>") {
		t.Error(`the page-parsed marker must be the last thing in body, after the content block`)
	}
}

// The inks the audit measured under 4.5:1 take the skin's muted or flower ink
// at full opacity, with the doubled gate that beats the pages' <style> blocks.
func TestHomesteadCSS_AuditInks(t *testing.T) {
	css := readHomesteadCSS(t)
	i := strings.Index(css, "/* ---- Contrast audit (2.4)")
	if i < 0 {
		t.Fatal("homestead.css has no Contrast audit (2.4) section")
	}
	sec := css[i:]
	if j := strings.Index(sec[1:], "\n/* ---- "); j >= 0 {
		sec = sec[:j+1]
	}
	const gate = `html:is([data-skin="homestead"], [data-skin="dark-homestead"]):is([data-skin="homestead"], [data-skin="dark-homestead"])`
	for _, want := range []string{
		gate + ` .chip.chip-type { opacity: 1; }`,
		gate + ` .project-row-actions .bead-action-close { color: var(--hs-ink-berry); }`,
		gate + ` .project-row-actions .bead-action-spawn { color: var(--hs-ink-sky); }`,
		gate + ` .cap-attention-badge { color: var(--hs-ink-sun); }`,
		`.cap-dash, .cap-status-disabled,`,
		`.maps-runs-empty, .maps-facet-label, .owner-initials-empty`,
		`.plans-lane-empty, .constellation-caption`,
		`.plan-decision-meta .meta, .plan-assistant-hint`,
		gate + ` :is(:is(p, .meta) a:not([class]), .operator-header-chip-link) { text-decoration: underline; }`,
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("the 2.4 section lacks %q", want)
		}
	}
	for _, line := range strings.Split(sec, "\n") {
		if strings.HasPrefix(line, "html") && !strings.HasPrefix(line, gate) {
			t.Errorf("2.4 rule %q must double the gate", line)
		}
	}
}
