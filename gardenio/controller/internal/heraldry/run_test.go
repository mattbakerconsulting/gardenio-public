package heraldry

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestParseRun(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"🔥🧭📦", "🔥🧭📦"},
		{"🔥 🧭 📦", "🔥🧭📦"},
		{"🏅🖌️🔎", "🏅🖌️🔎"},
		{"🏅🖌🔎", "🏅🖌️🔎"}, // the presentation selector is the library's
		{"🔥🧭📦🔑🌊", "🔥🧭📦🔑🌊"},
	} {
		got, err := ParseRun(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseRun(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// A run kd would refuse is refused with kd's words.
func TestParseRun_Refuses(t *testing.T) {
	for _, tc := range []struct{ in, why string }{
		{"", "arms are three to five emoji"},
		{"🔥🧭", "arms are three to five emoji"},
		{"🔥🧭📦🔑🌊🏅", "arms are three to five emoji"},
		{"🔥🧭🔥", "🔥 is used twice"},
		{"🔥🧭x", "x is not in the library"},
		{"🔥🧭🌱", "🌱 is not in the library"}, // a theme emoji
	} {
		_, err := ParseRun(tc.in)
		if want := "invalid arms: " + tc.why + ". " + Help; err == nil || err.Error() != want {
			t.Errorf("ParseRun(%q) = %v, want %q", tc.in, err, want)
		}
	}
}

func TestRunSet(t *testing.T) {
	if RunSet("🔥🧭📦") != RunSet("📦 🔥🧭") || RunSet("🔥🧭📦") == RunSet("🔥🧭🔑") {
		t.Errorf("RunSet: same emoji in another order must match, other emoji must not")
	}
	if RunSet("🖌️🔥🧭") != RunSet("🖌🔥🧭") {
		t.Errorf("RunSet ignores the presentation selector")
	}
}

// TestHelpParity fails when kbeads' model.HeraldryHelp or HeraldryExample
// moves without Help and RunExample here (gd-esv40X6rlP).
func TestHelpParity(t *testing.T) {
	src, err := os.ReadFile("../../../../kbeads/internal/model/validate.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if want := "const HeraldryExample = " + strconv.Quote(RunExample); !strings.Contains(text, want) {
		t.Errorf("kbeads' HeraldryExample is not %s", RunExample)
	}
	quoted := strconv.Quote(Help)
	// strconv.Quote escapes nothing in Help, so the source spells it as
	// kbeads does, with the example spliced in.
	want := "const HeraldryHelp = " + strings.Replace(quoted, RunExample, `" + HeraldryExample + "`, 1)
	if !strings.Contains(text, want) {
		t.Errorf("kbeads' HeraldryHelp moved; copy it to Help:\nwant %s", want)
	}
}
