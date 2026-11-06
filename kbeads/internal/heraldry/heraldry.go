// Package heraldry is a bead's arms: a run of three to five emoji
// ("charges") in order, 🔥🧭📦 (spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md; the owner's
// verdict of 2026-10-26 replaced the shield with the run).
//
// It holds the library of charges (a generated table, library.go), the
// parser for the input forms, and the canonical stored form, which is the run
// itself. It is pure: no store, no network. The words "blazon", "charge" and
// "crest" are for this code only; every message a person can see uses plain
// words (arms, emoji).
package heraldry

//go:generate go run ./genlibrary

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

// Charge is one emoji the library can put in arms.
type Charge struct {
	// Emoji is the charge in its fully-qualified form: with VS16 when VS16
	// is true, so ⚙️ never becomes ⚙ (R12).
	Emoji string
	// Name is the plain-words name (the Unicode CLDR short name, lower
	// case), used for the accessible name of rendered arms (§8).
	Name string
	// Codepoints is the emoji's codepoint in lower-case hex, VS16 left
	// out, as in the artwork's asset name emoji_u<Codepoints>.png.
	Codepoints string
	// VS16 reports whether the fully-qualified form carries U+FE0F.
	VS16 bool
}

// MinCharges and MaxCharges are how many emoji arms hold.
const (
	MinCharges = 3
	MaxCharges = 5
)

// Arms is a bead's arms: its charges in order, in their library form. A
// valid Arms (one returned by Parse) holds three to five library charges,
// none twice.
type Arms struct {
	Charges []string
}

// legacyPlaces is how many places the shield had. Stored values from before
// the run name them, "blue 🔥 . 🧭 . 📦", in the order top-left, top-right,
// centre, bottom-left, bottom-right; Read takes the charges in that order.
const legacyPlaces = 5

// legacyPlaceTokens are the shield's places in that order, as the old placed
// input form named them (tl:🔥). It is still read, for one release.
var legacyPlaceTokens = [legacyPlaces]string{"tl", "tr", "c", "bl", "br"}

// legacyEmpty is an empty place in the old stored form.
const legacyEmpty = "."

var (
	byKey      map[string]*Charge
	themeByKey map[string]string
)

func init() {
	byKey = make(map[string]*Charge, len(library))
	for i := range library {
		byKey[key(library[i].Emoji)] = &library[i]
	}
	themeByKey = make(map[string]string, len(themeExclusions))
	for _, g := range themeExclusions {
		themeByKey[key(g)] = g
	}
}

// key is the lookup form of an emoji: presentation selectors dropped, so
// ⚙, ⚙︎ and ⚙️ all find the one library entry.
func key(s string) string {
	return strings.NewReplacer("︎", "", "️", "").Replace(s)
}

// Library returns the charges, in Unicode emoji order. The slice is shared;
// callers must not modify it.
func Library() []Charge { return library }

// Lookup finds the library charge for an emoji, with or without its
// presentation selector.
func Lookup(emoji string) (Charge, bool) {
	c, ok := byKey[key(norm.NFC.String(emoji))]
	if !ok {
		return Charge{}, false
	}
	return *c, true
}

// ErrCount is returned for fewer than three emoji or more than five.
var ErrCount = errors.New("arms are three to five emoji")

// Parse reads arms a person gave: three to five emoji in order, with or
// without spaces between them ("🔥🧭📦"). For values stored before arms were
// a run it also reads the old shield forms, the colour word and five places
// ("blue 🔥 . 🧭 . 📦"), the five places alone, and the placed form ("tl:🔥
// c:🧭 br:📦"), taking the charges in place order and dropping the colour; a
// trailing "on <colour>" is dropped too. "none" is not arms; callers that
// accept it handle it before Parse.
func Parse(input string) (Arms, error) {
	a, err := Read(input)
	if err != nil {
		return Arms{}, err
	}
	if len(a.Charges) < MinCharges {
		return Arms{}, ErrCount
	}
	return a, nil
}

// Read is Parse for a value already in the store: the same forms, but arms of
// one or two emoji, which the shield allowed, still read. Input from a person
// goes through Parse.
func Read(stored string) (Arms, error) {
	fields := dropColour(strings.Fields(norm.NFC.String(stored)))
	var a Arms
	var err error
	placed := 0
	for _, f := range fields {
		if strings.Contains(f, ":") {
			placed++
		}
	}
	switch {
	case len(fields) == 0:
		return Arms{}, ErrCount
	case placed > 0:
		if placed != len(fields) {
			return Arms{}, errors.New("give the emoji in a row, " + exampleRun)
		}
		a.Charges, err = readPlaced(fields)
	case len(fields) == legacyPlaces && contains(fields, legacyEmpty):
		a.Charges, err = readPlaces(fields)
	default:
		a.Charges, err = readRun(fields)
	}
	if err != nil {
		return Arms{}, err
	}
	return a, check(a)
}

// exampleRun is the run the messages show.
const exampleRun = "🔥🧭📦"

// dropColour takes off the colour the old forms carried: a leading colour
// word in the six-token stored form, or a trailing "on <colour>". A word that
// is not a colour stays, and is refused as not in the library.
func dropColour(fields []string) []string {
	n := len(fields)
	if n == legacyPlaces+1 {
		if _, ok := LookupTincture(fields[0]); ok {
			return fields[1:]
		}
	}
	if n >= 2 && strings.EqualFold(fields[n-2], "on") {
		if _, ok := LookupTincture(fields[n-1]); ok {
			return fields[:n-2]
		}
	}
	return fields
}

// readRun reads the run: every grapheme of every field is one emoji.
func readRun(fields []string) ([]string, error) {
	var charges []string
	for _, f := range fields {
		g := uniseg.NewGraphemes(f)
		for g.Next() {
			c, err := charge(g.Str())
			if err != nil {
				return nil, err
			}
			charges = append(charges, c)
			if len(charges) > MaxCharges {
				return nil, ErrCount
			}
		}
	}
	return charges, nil
}

// readPlaces reads the old five-place form, "." for an empty place, taking
// the charges in place order.
func readPlaces(fields []string) ([]string, error) {
	var charges []string
	for _, f := range fields {
		if f == legacyEmpty {
			continue
		}
		c, err := one(f)
		if err != nil {
			return nil, err
		}
		charges = append(charges, c)
	}
	return charges, nil
}

// readPlaced reads the old placed form, "tl:🔥 c:🧭 br:📦", taking the
// charges in place order.
func readPlaced(fields []string) ([]string, error) {
	if len(fields) > MaxCharges {
		return nil, ErrCount
	}
	var places [legacyPlaces]string
	for _, f := range fields {
		tok, emoji, _ := strings.Cut(f, ":")
		p := -1
		for i, t := range legacyPlaceTokens {
			if strings.EqualFold(t, tok) {
				p = i
			}
		}
		if p < 0 || places[p] != "" {
			return nil, errors.New("give the emoji in a row, " + exampleRun)
		}
		c, err := one(emoji)
		if err != nil {
			return nil, err
		}
		places[p] = c
	}
	var charges []string
	for _, c := range places {
		if c != "" {
			charges = append(charges, c)
		}
	}
	return charges, nil
}

// one reads a field that holds a single emoji.
func one(s string) (string, error) {
	if n := uniseg.GraphemeClusterCount(s); n != 1 {
		return "", errors.New("give the emoji in a row, " + exampleRun)
	}
	return charge(s)
}

// charge resolves one grapheme to its library form.
func charge(g string) (string, error) {
	if c, ok := byKey[key(g)]; ok {
		return c.Emoji, nil
	}
	if _, ok := themeByKey[key(g)]; ok {
		return "", fmt.Errorf("%s is not in the library: it already marks something on gardenio's Slack cards", g)
	}
	return "", fmt.Errorf("%s is not in the library", g)
}

// check enforces at least one charge, at most five, none twice. Parse adds
// the floor of three.
func check(a Arms) error {
	if len(a.Charges) == 0 || len(a.Charges) > MaxCharges {
		return ErrCount
	}
	seen := map[string]bool{}
	for _, c := range a.Charges {
		if seen[c] {
			return fmt.Errorf("%s is used twice", c)
		}
		seen[c] = true
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Empty reports whether the arms hold no charge (no arms).
func (a Arms) Empty() bool { return len(a.Charges) == 0 }

// Canonical is the stored and wired form: the run, the charges in order with
// nothing between them, "🔥🧭📦". It is also what a person sees (Run). Arms
// from Parse round-trip: Parse(Canonical(a)) equals a.
func Canonical(a Arms) string { return strings.Join(a.Charges, "") }

// Run is the arms as a person sees them, the emoji in a row: the same string
// as Canonical.
func Run(a Arms) string { return Canonical(a) }

// EmojiSet is the near-duplicate key: the charges with their order
// forgotten, sorted and space-separated. Two arms with the same EmojiSet but
// different runs are the same emoji in another order (R5's warning).
func EmojiSet(a Arms) string {
	cs := append([]string(nil), a.Charges...)
	sort.Strings(cs)
	return strings.Join(cs, " ")
}

// PlainName is the accessible name of the arms (§8): "arms: fire, compass,
// package". Empty arms give "".
func PlainName(a Arms) string {
	if a.Empty() {
		return ""
	}
	names := make([]string, len(a.Charges))
	for i, c := range a.Charges {
		names[i] = c
		if ch, ok := Lookup(c); ok {
			names[i] = ch.Name
		}
	}
	return "arms: " + strings.Join(names, ", ")
}
