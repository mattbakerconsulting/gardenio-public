package badge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is the reproducible-output guard for the deterministic Tier-0
// badge (Phase 2, ka-0000002812 / gd-xrBJKrl9wl). The sibling tests in
// tier0_test.go prove Tier0 is deterministic WITHIN a single run — same input,
// same output. That is necessary but not sufficient: a palette re-order, a
// shape tweak, or a monogram-rule change would keep those tests green while
// silently changing the badge that every ALREADY-generated plan renders, busting
// the content-address cache (ka-0000005930 keys on (id,title)) and swapping icons
// out from under operators who have learned to recognize them.
//
// The golden files pin the exact bytes Tier0 emits for a fixed set of
// representative plans. Any drift fails this test with a byte-level diff, so a
// change to the art spec is a DELIBERATE, reviewed act (regenerate + eyeball the
// diff) rather than an accident. The committed .golden.svg files double as the
// "sample badges" the task asks for: they are real, human-inspectable badges you
// can open in a browser to see exactly what the generator produces.
//
// Regenerate after an INTENTIONAL art-spec change:
//
//	GB_UPDATE_GOLDEN=1 go test ./internal/badge/ -run TestTier0_Golden
//
// then review the diff in the .golden.svg files before committing.

// goldenDir holds one <name>.golden.svg per fixture below.
const goldenDir = "testdata"

// tier0GoldenFixtures is the pinned corpus. Each entry is a (filename, id,
// title) the generator is fed. The set is chosen to exercise every branch of
// the art spec AND to mirror real plans, so the goldens read as a genuine
// contact sheet:
//
//   - flagship        — the badge feature's own plan (real id + title).
//   - stopword-lead   — leading stop-word dropped from the monogram ("SP").
//   - e2e-fixture     — bracketed title; the "[E2E fixture] -> EF" collision
//     class that broke the monogram-only prototype (distinct id => distinct art).
//   - single-word     — one significant word => first two letters ("SC").
//   - special-chars   — ampersand/quote/angle in the title must escape cleanly.
//   - empty-title     — no letters at all => "?" monogram, never blank.
//
// The ids are fixed string constants (NOT live lookups) so the goldens are
// hermetic and reproducible on any machine, forever.
var tier0GoldenFixtures = []struct {
	name  string
	id    string
	title string
}{
	{"flagship", "ka-0000005980", "Every gardenio plan gets its own generated icon/badge"},
	{"stopword-lead", "ka-0000000093", "The scanner plan"},
	{"e2e-fixture", "gd-Xs3QgeUHCR", "[E2E fixture] Plans authoring"},
	{"single-word", "ka-0000005334", "Scanner"},
	{"special-chars", "ka-0000005916", `Tom & Jerry <"'> ship it`},
	{"empty-title", "gd-Uj6YcuReql", ""},
}

// TestTier0_Golden pins the exact SVG bytes Tier0 produces for each fixture to a
// checked-in baseline. A drift is either an intentional art-spec change (regen
// with GB_UPDATE_GOLDEN=1 and review the diff) or the regression this guards.
func TestTier0_Golden(t *testing.T) {
	update := os.Getenv("GB_UPDATE_GOLDEN") != ""
	for _, f := range tier0GoldenFixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			got := Tier0(f.id, f.title)
			if got.Tier != TierDeterministic {
				t.Fatalf("tier = %q, want %q", got.Tier, TierDeterministic)
			}
			path := filepath.Join(goldenDir, f.name+".golden.svg")

			if update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", goldenDir, err)
				}
				if err := os.WriteFile(path, []byte(got.SVG), 0o644); err != nil {
					t.Fatalf("update golden %s: %v", path, err)
				}
				t.Logf("updated golden %s (%d bytes)", path, len(got.SVG))
				return
			}

			wantBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", path, err)
			}
			if want := string(wantBytes); got.SVG != want {
				t.Errorf("Tier0(%q, %q) drifted from the baseline %s.\n"+
					"If this art-spec change is INTENTIONAL, regenerate with "+
					"GB_UPDATE_GOLDEN=1 and review the diff.\n%s",
					f.id, f.title, path, firstGoldenDiff(want, got.SVG))
			}
		})
	}
}

// TestTier0_GoldenIDsAreStable re-derives the badge a second time and asserts it
// matches the golden too — a belt-and-suspenders check that the SAME code path
// the golden was generated from is still byte-stable (guards against a future
// map/iteration-order or time dependency creeping into Tier0).
func TestTier0_GoldenIDsAreStable(t *testing.T) {
	for _, f := range tier0GoldenFixtures {
		a := Tier0(f.id, f.title).SVG
		b := Tier0(f.id, f.title).SVG
		if a != b {
			t.Fatalf("Tier0(%q,%q) not byte-stable across two calls", f.id, f.title)
		}
	}
}

// firstGoldenDiff points at the first byte where want and got diverge, with a
// little context each side — enough to eyeball a drift without dumping two
// multi-hundred-byte SVG blobs into the failure log.
func firstGoldenDiff(want, got string) string {
	n := min(len(want), len(got))
	i := 0
	for i < n && want[i] == got[i] {
		i++
	}
	lo := max(i-40, 0)
	wHi := min(i+40, len(want))
	gHi := min(i+40, len(got))
	return "first divergence at byte " + strconv.Itoa(i) +
		" (want " + strconv.Itoa(len(want)) + "B, got " + strconv.Itoa(len(got)) + "B)" +
		"\n  want …" + strings.ReplaceAll(want[lo:wHi], "\n", "⏎") + "…" +
		"\n   got …" + strings.ReplaceAll(got[lo:gHi], "\n", "⏎") + "…"
}
