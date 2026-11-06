package heraldry

import (
	"errors"
	"strings"
	"testing"
)

func TestParseCanonicalRoundTrip(t *testing.T) {
	for _, s := range []string{
		"🧭🔥📦",
		"🛡️",
		"🚪📜🏡🛂🎉",
	} {
		a, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got := a.Canonical(); got != s {
			t.Errorf("Canonical(Parse(%q)) = %q", s, got)
		}
	}
}

// The six-token form a bead stored before the run reads as its charges in
// place order (top-left, top-right, centre, bottom-left, bottom-right), the
// colour dropped, and its canonical form is the run.
func TestParseSixTokenForm(t *testing.T) {
	for in, want := range map[string]string{
		"blue 🧭 . 🔥 . 📦":    "🧭🔥📦",
		"green 🚪 📜 🏡 🛂 🎉":   "🚪📜🏡🛂🎉",
		"sand . . 🛡️ . .":   "🛡️",
		"slate 🎲 . 🛡️ . 🖼️": "🎲🛡️🖼️",
	} {
		a, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got := a.Canonical(); got != want {
			t.Errorf("Canonical(Parse(%q)) = %q, want %q", in, got, want)
		}
		b, _ := Parse(want)
		if Key(a, FormatPNG, 48, Day) != Key(b, FormatPNG, 48, Day) {
			t.Errorf("%q and its run %q draw under different keys", in, want)
		}
	}
}

// A charge without its VS16, or with VS15, is the same library charge, and
// Canonical spells it as the library does (spec R12). Spaces between the
// emoji of a run are allowed.
func TestParseSelectorInsensitive(t *testing.T) {
	for _, s := range []string{"🎲\U0001F6E1🖼️", "🎲\U0001F6E1︎🖼", " 🎲 🛡️  🖼️ ", "slate 🎲 . \U0001F6E1 . 🖼"} {
		a, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got := a.Canonical(); got != "🎲🛡️🖼️" {
			t.Errorf("Canonical(Parse(%q)) = %q", s, got)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	if _, err := Parse("  "); !errors.Is(err, ErrNoArms) {
		t.Errorf("Parse(blank) = %v, want ErrNoArms", err)
	}
	for _, s := range []string{
		"🧭🔥📦🚪📜🏡",            // six emoji
		"🧭🔥🧭",               // one emoji twice
		"🌱🔥📦",               // a theme glyph, never in the library (R4)
		"🧭 . 🔥 . 📦",         // places without a colour
		"teal 🧭 . 🔥 . 📦",    // not a colour the six-token form had
		"Blue 🧭 . 🔥 . 📦",    // the stored form is lower case
		"blue . . . . .",    // no emoji
		"blue 🧭 . 🔥 .",      // four places
		"blue 🧭🔥 . . . .",   // two emoji on one place
		"blue tl:🧭 . . . .", // an input form, not a stored one
		"🔥🧭📦 on blue",       // an input form, not a stored one
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", s)
		}
	}
}

func TestPlainNameAndRun(t *testing.T) {
	a, err := Parse("🔥🧭📦")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.PlainName(), "arms: fire, compass, package"; got != want {
		t.Errorf("PlainName = %q, want %q", got, want)
	}
	if got := a.Run(); got != "🔥🧭📦" {
		t.Errorf("Run = %q", got)
	}
	// The plain words never carry the emoji (spec §1, §8).
	if strings.ContainsAny(a.PlainName(), "🧭🔥📦") {
		t.Errorf("PlainName %q carries emoji", a.PlainName())
	}
}

// TestRowGeometry: the size key is the height; the width is n squares and
// an eighth of the height between them.
func TestRowGeometry(t *testing.T) {
	for _, c := range []struct {
		slot int
		dpr  float64
		n    int
		h, w int
	}{
		{24, 1, 3, 24, 78}, {40, 1, 3, 40, 130}, {64, 1, 3, 64, 208},
		{24, 1, 5, 24, 132}, {40, 1, 5, 40, 220}, {64, 1, 5, 64, 352},
		{24, 2, 3, 48, 156}, {24, 3, 5, 72, 396}, {40, 0, 4, 40, 175}, {24, 2.625, 3, 63, 205},
	} {
		h := DeviceHeight(c.slot, c.dpr)
		if w := Width(c.n, h); h != c.h || w != c.w {
			t.Errorf("%d charges at %d px × %g: %d×%d, want %d×%d", c.n, c.slot, c.dpr, w, h, c.w, c.h)
		}
	}
}
