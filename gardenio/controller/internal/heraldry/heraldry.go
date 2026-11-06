// Package heraldry is the one place a bead's arms become pixels (heraldry
// spec gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §6.4).
//
// Arms are a run of three to five emoji in order (🔥🧭📦). Input is the
// stored form kbeads validates and the wire carries (BeadDetail.Heraldry):
// the run, or the older six-token form ("blue 🔥 . 🧭 . 📦") a bead stored
// before the run, which reads as its charges in place order with the colour
// dropped. Output is the run drawn as one row of charges from the pinned
// Noto Emoji artwork, each a square the height of the slot, an eighth of
// the height between them, on a transparent ground: PNG bytes at any device
// height or resolution-free SVG. There is no shield, field or colour. The
// theme is the surface the picture sits on, day or night, and only sets the
// 1-unit keyline (Q14 (a)) that keeps each charge's edge on that surface.
//
// The size key is the height. A surface asks for its slot (24, 40 or 64
// CSS px) at its density:
//
//	a, err := heraldry.Parse(bead.Heraldry)
//	png, err := a.PNG(heraldry.DeviceHeight(24, 2), heraldry.Day) // 48 px high, as wide as the run needs
//	svg := a.SVG(heraldry.Night, heraldry.SVGOptions{})           // inline, any size
//	alt := a.PlainName()                                          // aria-label / alt (§8)
//
// or goes through a Cache, which content-addresses each picture (Key) for
// URLs and ETags.
//
// The library (names and codepoints) mirrors kbeads/internal/heraldry,
// which the controller cannot import: genassets writes library_gen.go from
// kbeads' source, and TestKbeadsParity fails when the two drift. The word
// "charge" is for this code only; a person sees a picture and its
// plain-words name.
package heraldry

//go:generate go run ./genassets -noto $NOTO_EMOJI_DIR

import (
	"embed"
	"errors"
	"fmt"
	"strings"
)

// Charge is one emoji the library can put in a run.
type Charge struct {
	// Emoji is the charge in its fully-qualified form (with VS16 where the
	// library carries it), as the canonical form spells it.
	Emoji string
	// Name is the plain-words name, the charge's part of the accessible name.
	Name string
	// Codepoints is the emoji's codepoints in lower-case hex, VS16 left out:
	// the artwork is assets/emoji_u<Codepoints>.png.
	Codepoints string
}

// Theme is the surface a picture is drawn for: the light day surface or the
// dark night one. It changes only the keyline's ink; the charges are the
// same.
type Theme int

const (
	Day Theme = iota
	Night
)

func (t Theme) String() string {
	if t == Night {
		return "night"
	}
	return "day"
}

// MaxCharges is the most charges a run holds. The daemon refuses a new run
// of fewer than three; Parse still reads the one or two a bead stored
// before, so every stored value keeps drawing.
const MaxCharges = 5

// Arms is a parsed run: its charges, in order.
type Arms struct {
	Charges []Charge
}

//go:embed assets/*.png assets/72/*.png
var assets embed.FS

var byKey map[string]*Charge

func init() {
	byKey = make(map[string]*Charge, len(library))
	for i := range library {
		byKey[key(library[i].Emoji)] = &library[i]
	}
}

// key is an emoji's lookup form: presentation selectors dropped, so ⚙, ⚙︎
// and ⚙️ find the one library entry.
func key(s string) string {
	return strings.NewReplacer("\ufe0e", "", "\ufe0f", "").Replace(s)
}

// Library returns the charges, in Unicode emoji order. The slice is shared;
// callers must not modify it.
func Library() []Charge { return library }

// Lookup finds the library charge for an emoji, with or without its
// presentation selector.
func Lookup(emoji string) (Charge, bool) {
	c, ok := byKey[key(emoji)]
	if !ok {
		return Charge{}, false
	}
	return *c, true
}

// ChargePNG is a charge's artwork, the 128 px Noto PNG, for a surface that
// serves charges on their own (SVGOptions.ChargeHref).
func ChargePNG(c Charge) ([]byte, bool) {
	b, err := assets.ReadFile("assets/emoji_u" + c.Codepoints + ".png")
	return b, err == nil
}

// smallChargePNG is a charge's 72 px Noto PNG (SVGOptions.Small).
func smallChargePNG(c Charge) ([]byte, bool) {
	b, err := assets.ReadFile("assets/72/emoji_u" + c.Codepoints + ".png")
	return b, err == nil
}

// ErrNoArms is Parse's error for an empty value: the bead bears no arms.
var ErrNoArms = errors.New("heraldry: no arms")

// legacyColours are the colour words of the six-token form, which a bead
// stored before arms became a run. Parse reads the colour and drops it.
var legacyColours = map[string]bool{
	"blue": true, "red": true, "green": true, "gold": true,
	"purple": true, "brown": true, "slate": true, "sand": true,
}

// Parse reads arms in a stored form: the run ("🔥🧭📦", spaces between the
// emoji allowed), or the six-token form ("blue 🔥 . 🧭 . 📦": a colour,
// then five places, "." for an empty one), which reads as its charges in
// place order (top-left, top-right, centre, bottom-left, bottom-right) with
// the colour dropped, as kbeads reads it. Parse does not take the other
// forms a person types (kbeads parses those). "" gives ErrNoArms.
func Parse(stored string) (Arms, error) {
	var a Arms
	f := strings.Fields(stored)
	if len(f) == 0 {
		return a, ErrNoArms
	}
	if legacyColours[f[0]] {
		if len(f) != 6 {
			return a, fmt.Errorf("heraldry: %q is not the six-token form (a colour and five places)", stored)
		}
		for _, tok := range f[1:] {
			if tok == "." {
				continue
			}
			c, ok := Lookup(tok)
			if !ok {
				return a, fmt.Errorf("heraldry: %s is not one library emoji", tok)
			}
			a.Charges = append(a.Charges, c)
		}
	} else {
		for _, r := range strings.Join(f, "") {
			if r == '\ufe0e' || r == '\ufe0f' {
				continue
			}
			c, ok := Lookup(string(r))
			if !ok {
				return a, fmt.Errorf("heraldry: %c is not in the library", r)
			}
			a.Charges = append(a.Charges, c)
		}
	}
	switch n := len(a.Charges); {
	case n == 0:
		return a, fmt.Errorf("heraldry: %q holds no emoji", stored)
	case n > MaxCharges:
		return a, fmt.Errorf("heraldry: %q holds %d emoji, more than %d", stored, n, MaxCharges)
	}
	seen := map[string]bool{}
	for _, c := range a.Charges {
		if seen[c.Emoji] {
			return a, fmt.Errorf("heraldry: %s is used twice", c.Emoji)
		}
		seen[c.Emoji] = true
	}
	return a, nil
}

// Canonical is the arms in the stored form, the run, with each charge
// spelled as the library spells it: Parse(Canonical(a)) == a.
func (a Arms) Canonical() string { return a.Run() }

// Run is the emoji run, the charges in order with nothing between them
// (spec §4.2).
func (a Arms) Run() string {
	var sb strings.Builder
	for _, c := range a.Charges {
		sb.WriteString(c.Emoji)
	}
	return sb.String()
}

// PlainName is the accessible name of the picture (spec §8), the alt and
// aria-label of every rendering: "arms: fire, compass, package".
func (a Arms) PlainName() string {
	names := make([]string, len(a.Charges))
	for i, c := range a.Charges {
		names[i] = c.Name
	}
	return "arms: " + strings.Join(names, ", ")
}
