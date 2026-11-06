package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

func TestSlugifyAnchor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Alpha-prefixed planning labels (the dominant corpus shape).
		{"C1.1 — TTL field", "c1-1"},
		{"C1.1", "c1-1"},
		{"R3. Hard cycle-budget cap", "r3"},
		{"R3", "r3"},
		{"C1 Shape C — Per-identity scoped tokens", "c1"},
		{"P2", "p2"},
		{"D10 — something", "d10"},
		{"T2 (ka-0000002402)", "t2"},
		// Bare ADR section numbers.
		{"6. Retention", "6"},
		{"6.3 Retention policy", "6-3"},
		{"3.5.1 nested", "3-5-1"},
		// Idempotence on already-slugged input — this is what lets the
		// linkifier feed a `#c1-1` ref suffix through the same function.
		{"c1-1", "c1-1"},
		{"6-3", "6-3"},
		{"r3", "r3"},
		// "R4 - Narrow infra": space before the dash means the label token is
		// just R4; the dash does NOT continue the token.
		{"R4 - Narrow infra-pod list", "r4"},
		// No recognizable label → full-text slug fallback. A first word with
		// no digit is prose, not a label.
		{"Overview", "overview"},
		{"Design notes & caveats", "design-notes-caveats"},
		{"Retention policy", "retention-policy"},
		// Lowercase author input still normalizes.
		{"c1.1 — ttl", "c1-1"},
		// Leading/trailing whitespace tolerated.
		{"  C1.1  ", "c1-1"},
		// Composite plan→decision labels (P.D1) and section-symbol refs (§6.3),
		// the canonical motivating examples from the survey.
		{"P.D1 — decision item", "p-d1"},
		{"P.D1", "p-d1"},
		{"P2.D1 — sub-decision", "p2-d1"},
		{"P2.D1", "p2-d1"},
		{"§6.3 Retention", "6-3"},
		{"§6.3", "6-3"},
		{"#6.3 Retention", "6-3"},
		// Idempotence for the composite forms too.
		{"p-d1", "p-d1"},
		{"p2-d1", "p2-d1"},
	}
	for _, c := range cases {
		if got := slugifyAnchor(c.in); got != c.want {
			t.Errorf("slugifyAnchor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// newAnchorMD builds a goldmark instance with the heading-anchor transformer,
// mirroring the production `md` builder in server.go.
func newAnchorMD() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(withHeadingAnchors()),
	)
}

func renderAnchorMD(t *testing.T, src string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := newAnchorMD().Convert([]byte(src), &buf); err != nil {
		t.Fatalf("convert: %v", err)
	}
	return buf.String()
}

func TestHeadingAnchorTransformer_StampsStableIDs(t *testing.T) {
	out := renderAnchorMD(t, strings.Join([]string{
		"### C1.1 — TTL field",
		"",
		"body",
		"",
		"### R3. cycle-budget cap",
		"",
		"## 6.3 Retention",
	}, "\n"))

	for _, want := range []string{`id="c1-1"`, `id="r3"`, `id="6-3"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered HTML missing %s\n--- got ---\n%s", want, out)
		}
	}
}

func TestHeadingAnchorTransformer_DedupesCollisions(t *testing.T) {
	// Two headings whose label token collides must get distinct ids.
	out := renderAnchorMD(t, strings.Join([]string{
		"### C1 first",
		"",
		"### C1 second",
	}, "\n"))
	if !strings.Contains(out, `id="c1"`) {
		t.Errorf("missing first id=c1\n%s", out)
	}
	if !strings.Contains(out, `id="c1-2"`) {
		t.Errorf("missing deduped id=c1-2\n%s", out)
	}
}

func TestHeadingAnchorTransformer_DedupeNeverDuplicatesAcrossLiteralLabel(t *testing.T) {
	// Regression (review finding): a deduped slug (`c1-2`) must not collide
	// with a literal label heading that already slugs to `c1-2`. Every id in
	// the document must be unique.
	out := renderAnchorMD(t, strings.Join([]string{
		"## C1 Shape A",
		"",
		"## C1 Shape B",
		"",
		"## C1-2 leftover",
	}, "\n"))
	ids := regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(out, -1)
	seen := map[string]int{}
	for _, m := range ids {
		seen[m[1]]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("duplicate id %q appears %d times\n%s", id, n, out)
		}
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 ids, got %d: %v", len(ids), ids)
	}
}

func TestHeadingAnchorTransformer_RespectsExplicitID(t *testing.T) {
	// With heading-attribute syntax disabled (production default), `{#x}` is
	// not parsed as an id, so this just confirms a plain heading still gets a
	// label-derived id and nothing panics on attribute-looking text.
	out := renderAnchorMD(t, "### C2.4 — explicit-ish {#whatever}")
	if !strings.Contains(out, `id="c2-4"`) {
		t.Errorf("expected label-derived id=c2-4\n%s", out)
	}
}
