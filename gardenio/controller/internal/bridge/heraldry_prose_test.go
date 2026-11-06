package bridge

// heraldry_prose_test.go pins heraldry step 2.2 (spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md): where no
// picture can go (prose, the References footer, a link unfurl) a bead's emoji
// run stands where its type glyph would (R9, Q5, Q9), the hydrate cache keeps
// the arms, and no theme glyph is ever a library charge (R4).

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// Canonical arms for the fixtures, checked against the renderer's library in
// TestHeraldryFixturesAreArms so a library change cannot quietly void them.
const (
	planArms = "green 💵 . 👛 . 🧾"
	epicArms = "blue . 🧭 ⚓ . ."
)

func TestHeraldryFixturesAreArms(t *testing.T) {
	for _, c := range []string{planArms, epicArms} {
		if _, err := heraldry.Parse(c); err != nil {
			t.Errorf("fixture %q: %v", c, err)
		}
	}
}

// armedFixture is hydrateFixture with arms on its plan and one more armed
// epic.
func armedFixture() map[string]*beadsapi.BeadDetail {
	beads := map[string]*beadsapi.BeadDetail{}
	for id, b := range hydrateFixture {
		cp := *b
		beads[id] = &cp
	}
	beads["gd-Oc3GVm9gc6"].Heraldry = planArms
	beads["gd-EpicArms01"] = &beadsapi.BeadDetail{ID: "gd-EpicArms01", Type: "epic", Status: "open", Priority: 2,
		Heraldry: epicArms, ShortTitle: "Harbour the fleet"}
	return beads
}

func TestHydrateForm_ArmsReplaceTypeGlyphInline(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		t.Run(th.Name, func(t *testing.T) {
			h, _ := newTestHydrator(armedFixture(), th)
			vals := formVals("channel", "C1", "text", hydrateFixtureText)
			h.hydrateForm(context.Background(), vals)
			got := vals.Get("text")

			link := "<" + testViewer + "/gd-Oc3GVm9gc6|"
			if want := "Plan-gardener 💵👛🧾 " + link; !strings.Contains(got, want) {
				t.Errorf("inline text missing %q\n got: %s", want, got)
			}
			if typeGlyph := hydrateTypeGlyph(th, armedFixture()["gd-Oc3GVm9gc6"]); strings.Contains(got, typeGlyph+" "+link) {
				t.Errorf("plan with arms still shows its type glyph %q inline:\n%s", typeGlyph, got)
			}
		})
	}
}

// TestHydrateForm_ArmsFooter pins the footer line shape for a bead with arms:
// the run takes the type glyph's slot and the status glyph stays.
func TestHydrateForm_ArmsFooter(t *testing.T) {
	h, _ := newTestHydrator(armedFixture(), Garden)
	vals := formVals("channel", "C1", "text", hydrateFixtureText)
	h.hydrateForm(context.Background(), vals)
	got := vals.Get("text")

	want := "> 💵👛🧾 🍃 <" + testViewer + "/gd-Oc3GVm9gc6|Attribute agent slowness to specific…> · P1"
	if !strings.Contains(got, want+"\n") {
		t.Errorf("footer missing %q\n got:\n%s", want, got)
	}
	// Beads without arms keep their type glyph.
	if want := "> 🪧 🫘 <" + testViewer + "/gd-Lh590iWd8Y|"; !strings.Contains(got, want) {
		t.Errorf("unarmed decision lost its type glyph in the footer:\n%s", got)
	}
}

// TestHydrateForm_OneRunPerLine pins R10: a line naming several beads with
// arms shows the run only for the first; the next takes its type glyph. A new
// line, inside or outside a protected span, starts the count again.
func TestHydrateForm_OneRunPerLine(t *testing.T) {
	r := hydrateRenderer{viewerURL: testViewer, theme: Garden, beads: armedFixture()}
	plan := "<" + testViewer + "/gd-Oc3GVm9gc6|"
	epic := "<" + testViewer + "/gd-EpicArms01|Harbour the fleet>"

	cases := []struct {
		name, text string
		want       []string
	}{
		{"same line", "gd-Oc3GVm9gc6 lands before gd-EpicArms01.",
			[]string{"💵👛🧾 " + plan, "🌻 " + epic}},
		{"next line", "gd-Oc3GVm9gc6 lands.\nThen gd-EpicArms01.",
			[]string{"💵👛🧾 " + plan, "Then 🧭⚓ " + epic}},
		{"newline inside a code span", "gd-Oc3GVm9gc6 ```x\ny``` gd-EpicArms01",
			[]string{"💵👛🧾 " + plan, "🧭⚓ " + epic}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.inline(tc.text)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q\n got: %s", w, got)
				}
			}
		})
	}
}

func TestSlimBead_KeepsHeraldry(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-x", Type: "epic", Title: "T", Heraldry: planArms}
	if got := slimBead(b).Heraldry; got != planArms {
		t.Errorf("slimBead dropped the arms: got %q, want %q", got, planArms)
	}
	h, _ := newTestHydrator(map[string]*beadsapi.BeadDetail{"gd-x": b}, Garden)
	if got := armsRun(h.resolve(context.Background(), []string{"gd-x"})["gd-x"].Heraldry); got != "💵👛🧾" {
		t.Errorf("cached bead run = %q, want 💵👛🧾", got)
	}
}

// TestBuildUnfurlAttachment_ArmsRun pins Q9: an unfurl cannot carry the
// picture, so the run sits with the title and the type badge keeps its slot
// in the metadata line (R9).
func TestBuildUnfurlAttachment_ArmsRun(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://prod.gardenio.acme.example/beads"}
	link := "https://prod.gardenio.acme.example/beads/gd-EpicArms01"
	bead := &beadsapi.BeadDetail{ID: "gd-EpicArms01", Title: "Harbour the fleet", Type: "epic", Status: "open", Heraldry: epicArms}
	text := renderBlocksText(b.buildUnfurlAttachment(Garden, bead, link))
	for _, want := range []string{"🧭⚓ <" + link + "|Harbour the fleet>", "🌻 `epic`"} {
		if !strings.Contains(text, want) {
			t.Errorf("unfurl missing %q\n---\n%s", want, text)
		}
	}

	bead.Heraldry = ""
	if text := renderBlocksText(b.buildUnfurlAttachment(Garden, bead, link)); strings.Contains(text, "🧭") {
		t.Errorf("unarmed unfurl shows a run:\n%s", text)
	}
}

// themeReactionEmoji is the emoji Slack draws for each reaction name the
// themes use. A name missing here fails TestThemeGlyphsAreNotCharges, so a
// theme that adds a reaction has to say what it draws.
var themeReactionEmoji = map[string]string{
	"seedling":               "🌱",
	"leaves":                 "🍃",
	"deciduous_tree":         "🌳",
	"wilted_flower":          "🥀",
	"fallen_leaf":            "🍂",
	"white_circle":           "⚪",
	"hourglass_flowing_sand": "⏳",
	"white_check_mark":       "✅",
	"x":                      "❌",
	"no_entry_sign":          "🚫",
}

// TestThemeGlyphsAreNotCharges is R4's bridge-side check: no glyph the Garden
// or Business theme draws is a charge in the renderer's library, so an emoji
// run beside a type or state glyph is never ambiguous and a theme change may
// not take an emoji a shield can bear. kbeads' TestThemeExclusionsMatchTheme
// checks the same from the library's side by parsing theme.go; this one reads
// the live values, fallbacks included.
func TestThemeGlyphsAreNotCharges(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		glyphs := map[string]string{}
		for k, g := range th.Lifecycle {
			glyphs["Lifecycle["+k+"]"] = g
		}
		for k, g := range th.BeadType {
			glyphs["BeadType["+k+"]"] = g
		}
		for k, g := range th.SemanticGlyphs {
			glyphs["SemanticGlyphs["+k+"]"] = g
		}
		for k, g := range th.ToolGlyphs {
			glyphs["ToolGlyphs["+k+"]"] = g
		}
		for k, n := range th.ReactionNames {
			e, ok := themeReactionEmoji[n]
			if !ok {
				t.Errorf("%s ReactionNames[%s] = %q: add the emoji Slack draws for it to themeReactionEmoji", th.Name, k, n)
				continue
			}
			glyphs["ReactionNames["+k+"]"] = e
		}
		for where, g := range glyphs {
			if c, ok := heraldry.Lookup(g); ok {
				t.Errorf("%s %s = %s is the library charge %q (spec R4: the library is not the theme)", th.Name, where, g, c.Name)
			}
		}
	}
}
