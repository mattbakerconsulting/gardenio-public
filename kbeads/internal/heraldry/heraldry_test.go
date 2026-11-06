package heraldry

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rivo/uniseg"

	"gardenio/kbeads/internal/heraldry/themeglyphs"
	"gardenio/kbeads/internal/model"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"three", "🔥🧭📦", "🔥🧭📦"},
		{"four", "🔥🧭📦🎯", "🔥🧭📦🎯"},
		{"five", "🔥🧭📦🎯🦊", "🔥🧭📦🎯🦊"},
		{"order is kept", "📦🧭🔥", "📦🧭🔥"},
		{"with spaces", " 🔥  🧭 📦 ", "🔥🧭📦"},
		{"VS16 added", "❤🔥📦", "❤️🔥📦"},
		{"VS16 kept", "❤️🔥📦", "❤️🔥📦"},
		{"VS15 swapped for VS16", "☂︎🔥📦", "☂️🔥📦"},
		{"VS16 dropped where the library has none", "🔥️🧭📦", "🔥🧭📦"},
		// The old shield forms still read, for stored values: the charges
		// in place order (top-left, top-right, centre, bottom-left,
		// bottom-right), the colour dropped.
		{"old stored form", "blue 🔥 . 🧭 . 📦", "🔥🧭📦"},
		{"old stored form, every place", "purple 🔥 🧭 📦 🎯 🦊", "🔥🧭📦🎯🦊"},
		{"old stored form, VS16 added", "green ☂ . ❤ . 🔥", "☂️❤️🔥"},
		{"old five places", "🔥 . 🧭 . 📦", "🔥🧭📦"},
		{"old placed form", "tl:🔥 c:🧭 br:📦", "🔥🧭📦"},
		{"old placed form, read in place order", "br:📦 tl:🔥 tr:🧭", "🔥🧭📦"},
		{"old placed form, upper-case place", "TL:🔥 C:🧭 BR:📦", "🔥🧭📦"},
		{"old colour after on", "🔥🧭📦 on blue", "🔥🧭📦"},
		{"old colour after on, any case", "🔥🧭📦 ON Sand", "🔥🧭📦"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.in, err)
			}
			if got := Canonical(a); got != tt.want {
				t.Fatalf("Canonical(Parse(%q)) = %q, want %q", tt.in, got, tt.want)
			}
			back, err := Parse(Canonical(a))
			if err != nil || !slices.Equal(back.Charges, a.Charges) {
				t.Fatalf("round trip of %q: got %q, %v", tt.want, Canonical(back), err)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"nothing", "", "arms are three to five emoji"},
		{"only spaces", "   ", "arms are three to five emoji"},
		{"one emoji", "🔥", "arms are three to five emoji"},
		{"two emoji", "🔥🧭", "arms are three to five emoji"},
		{"six emoji", "🔥🧭📦🎯🦊✨", "arms are three to five emoji"},
		{"old stored form with two", "blue 🔥 . . . 📦", "arms are three to five emoji"},
		{"all five places empty", ". . . . .", "arms are three to five emoji"},
		{"only a colour", "on blue", "arms are three to five emoji"},
		{"six placed", "tl:🔥 tr:🧭 c:📦 bl:🎯 br:🦊 c:✨", "arms are three to five emoji"},
		{"used twice", "🔥🔥🧭", "🔥 is used twice"},
		{"used twice, VS16 and not", "❤ ❤️ 🔥", "❤️ is used twice"},
		{"used twice, old form", "red 🔥 🔥 🧭 . .", "🔥 is used twice"},
		{"flag", "🏴", "🏴 is not in the library"},
		{"flag sequence", "🇬🇧", "🇬🇧 is not in the library"},
		{"skin tone", "👍🏽", "👍🏽 is not in the library"},
		{"skin tone modifier alone", "🏽", "🏽 is not in the library"},
		{"ZWJ sequence", "👨‍💻", "👨‍💻 is not in the library"},
		{"keycap", "1️⃣", "1️⃣ is not in the library"},
		{"letter", "a", "a is not in the library"},
		{"dot in a run", "🔥 . 📦", ". is not in the library"},
		{"unknown colour", "🔥🧭📦 on teal", "o is not in the library"},
		{"theme glyph", "🌱🔥📦", "🌱 is not in the library: it already marks something on gardenio's Slack cards"},
		{"theme glyph without its VS16", "⚙🔥📦", "⚙ is not in the library: it already marks something on gardenio's Slack cards"},
		{"some placed, some not", "tl:🔥 🧭 📦", "give the emoji in a row, 🔥🧭📦"},
		{"unknown place", "top:🔥 c:🧭 br:📦", "give the emoji in a row, 🔥🧭📦"},
		{"two emoji on one place", "tl:🔥🧭 c:📦 br:🎯", "give the emoji in a row, 🔥🧭📦"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.in)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want %q", tt.in, tt.want)
			}
			if err.Error() != tt.want {
				t.Fatalf("Parse(%q) error = %q, want %q", tt.in, err, tt.want)
			}
			for _, word := range []string{"blazon", "charge", "crest", "canonical", "tincture", "shield", "place"} {
				if strings.Contains(err.Error(), word) {
					t.Errorf("error %q uses the internal word %q", err, word)
				}
			}
		})
	}
}

// TestRead: a stored value from the shield, which allowed one or two emoji,
// still reads; Parse refuses it as input.
func TestRead(t *testing.T) {
	for in, want := range map[string]string{
		"purple . . 🔥 . .": "🔥",
		"blue 🔥 . . . 📦":   "🔥📦",
		"🔥🧭📦":              "🔥🧭📦",
	} {
		a, err := Read(in)
		if err != nil || Canonical(a) != want {
			t.Errorf("Read(%q) = %q, %v; want %q", in, Canonical(a), err, want)
		}
	}
	if _, err := Read(""); err != ErrCount {
		t.Errorf("Read(\"\") error = %v, want ErrCount", err)
	}
}

func TestFormatters(t *testing.T) {
	a, err := Parse("🔥☂📦")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Canonical(a), "🔥☂️📦"; got != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}
	if got, want := Run(a), "🔥☂️📦"; got != want {
		t.Errorf("Run = %q, want %q", got, want)
	}
	if got, want := PlainName(a), "arms: fire, umbrella, package"; got != want {
		t.Errorf("PlainName = %q, want %q", got, want)
	}
	if PlainName(Arms{}) != "" || Run(Arms{}) != "" || !(Arms{}).Empty() || a.Empty() {
		t.Error("empty arms should have no name, no run, and report Empty")
	}
}

func TestEmojiSet(t *testing.T) {
	a, _ := Parse("🔥🧭📦")
	b, _ := Parse("📦🔥🧭")
	c, _ := Parse("🔥🧭🎯")
	if Canonical(a) == Canonical(b) {
		t.Fatal("the same emoji in another order must be other arms")
	}
	if EmojiSet(a) != EmojiSet(b) {
		t.Errorf("EmojiSet(%q) = %q, EmojiSet(%q) = %q; want equal", Canonical(a), EmojiSet(a), Canonical(b), EmojiSet(b))
	}
	if EmojiSet(a) == EmojiSet(c) {
		t.Errorf("different emoji share EmojiSet %q", EmojiSet(a))
	}
	if Canonical(a) != "🔥🧭📦" {
		t.Error("EmojiSet must not reorder the arms it was given")
	}
}

func TestEveryChargeRoundTrips(t *testing.T) {
	for _, c := range Library() {
		for _, in := range []string{c.Emoji, key(c.Emoji), key(c.Emoji) + "︎"} {
			a, err := Read(in)
			if err != nil {
				t.Errorf("Read(%q) [%s]: %v", in, c.Name, err)
				continue
			}
			if len(a.Charges) != 1 || a.Charges[0] != c.Emoji {
				t.Errorf("Read(%q) = %q, want %q", in, a.Charges, c.Emoji)
			}
			if back, err := Read(Canonical(a)); err != nil || !slices.Equal(back.Charges, a.Charges) {
				t.Errorf("round trip of %q: %q, %v", Canonical(a), Canonical(back), err)
			}
		}
	}
}

func TestLibrary(t *testing.T) {
	lib := Library()
	if len(lib) < 1000 {
		t.Fatalf("library has %d charges; expected the ~1,300 Noto Emoji base emoji", len(lib))
	}
	if NotoEmojiTag == "" || UnicodeEmojiVersion == "" {
		t.Fatal("library pins are missing")
	}
	seen := map[string]string{}
	for _, c := range lib {
		if c.Name == "" || c.Name != strings.ToLower(c.Name) {
			t.Errorf("%s: name %q must be non-empty lower case", c.Emoji, c.Name)
		}
		if uniseg.GraphemeClusterCount(c.Emoji) != 1 {
			t.Errorf("%s (%s) is not one grapheme", c.Emoji, c.Name)
		}
		if got := strings.HasSuffix(c.Emoji, "️"); got != c.VS16 {
			t.Errorf("%s (%s): VS16 = %t but the emoji says %t", c.Emoji, c.Name, c.VS16, got)
		}
		runes := []rune(key(c.Emoji))
		if len(runes) != 1 || strconv.FormatInt(int64(runes[0]), 16) != c.Codepoints {
			t.Errorf("%s (%s): Codepoints %q do not match", c.Emoji, c.Name, c.Codepoints)
		}
		if prev, ok := seen[key(c.Emoji)]; ok {
			t.Errorf("%s appears twice (%s, %s)", c.Emoji, prev, c.Name)
		}
		seen[key(c.Emoji)] = c.Name
		if _, ok := themeByKey[key(c.Emoji)]; ok {
			t.Errorf("%s (%s) is a theme glyph and must not be in the library", c.Emoji, c.Name)
		}
	}
}

// TestThemeExclusionsMatchTheme keeps theme_exclusions.go in step with the
// bridge theme (R4): no glyph theme.go uses is a charge, and every listed
// exclusion is still a theme glyph. On failure, regenerate the library.
func TestThemeExclusionsMatchTheme(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", themeglyphs.ThemePath))
	if err != nil {
		t.Fatalf("read the bridge theme: %v", err)
	}
	glyphs, err := themeglyphs.Extract(src)
	if err != nil {
		t.Fatal(err)
	}
	inTheme := map[string]bool{}
	for _, g := range glyphs {
		inTheme[key(g)] = true
		if c, ok := byKey[key(g)]; ok {
			t.Errorf("theme.go uses %s, which is the library charge %q; run go generate ./internal/heraldry/", g, c.Name)
		}
	}
	for _, g := range themeExclusions {
		if !inTheme[key(g)] {
			t.Errorf("theme_exclusions.go lists %s, which theme.go no longer uses; run go generate ./internal/heraldry/", g)
		}
	}
	if len(themeExclusions) < 50 {
		t.Errorf("only %d theme exclusions; the themes use ~55 emoji", len(themeExclusions))
	}
}

func TestHeraldryHelpExampleParses(t *testing.T) {
	a, err := Parse(model.HeraldryExample)
	if err != nil {
		t.Fatalf("HeraldryExample %q: %v", model.HeraldryExample, err)
	}
	if Canonical(a) != model.HeraldryExample {
		t.Errorf("HeraldryExample %q is not in its stored form (%q)", model.HeraldryExample, Canonical(a))
	}
	if !strings.Contains(model.HeraldryHelp, model.HeraldryExample) {
		t.Error("HeraldryHelp does not show HeraldryExample")
	}
	if !strings.Contains(model.HeraldryHelp, "three to five emoji") {
		t.Error("HeraldryHelp does not say arms are three to five emoji")
	}
	for _, word := range []string{"blazon", "charge", "crest", "canonical", "tincture", "shield", "tl:", "colour"} {
		if strings.Contains(model.HeraldryHelp, word) {
			t.Errorf("HeraldryHelp uses %q, which the run no longer has", word)
		}
	}
}
