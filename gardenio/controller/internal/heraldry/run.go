package heraldry

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rivo/uniseg"
)

// The run a person types (spec §4.2, the owner's verdict of 2026-10-26):
// three to five emoji in order, "🔥🧭📦". kbeads parses and stores it
// (kbeads/internal/heraldry.Parse), which the controller cannot import;
// ParseRun is its run form, for a writer that checks arms before it sends
// them (gb lands plan write's Arms: line), and Help is the text every
// refusal ends with. TestHelpParity fails when kbeads' text moves.

// MinRun and MaxRun are how many emoji arms hold.
const (
	MinRun = 3
	MaxRun = 5
)

// RunExample is the run the messages show.
const RunExample = "🔥🧭📦"

// Help is kbeads' model.HeraldryHelp: the kd flag help, the daemon's error
// and the advice line all quote it.
const Help = "heraldry is a bead's arms: an optional mark of three to five emoji in a row, unique in the project, for epics (plans and phases) and projects. Type them in order (" + RunExample + "). Emoji that Slack cards already use for bead types and states are not in the library. 'none' removes the arms."

// errRunCount is kbeads' ErrCount.
var errRunCount = errors.New("arms are three to five emoji")

// ParseRun reads arms typed as a run, with or without spaces between the
// emoji, and returns the run in the library's spelling: every emoji in the
// library, three to five of them, none twice. Its error is kbeads' words,
// "invalid arms: <why>. <Help>", the text kd heraldry check and the daemon
// refuse with. An emoji the bridge's themes use is not in the library, and
// is refused as not in it.
func ParseRun(input string) (string, error) {
	// In kbeads' order: each emoji looked up and counted as it is read, then
	// none twice, then the floor of three.
	var run []string
	for _, f := range strings.Fields(input) {
		g := uniseg.NewGraphemes(f)
		for g.Next() {
			c, ok := Lookup(g.Str())
			if !ok {
				return "", invalidRun(fmt.Errorf("%s is not in the library", g.Str()))
			}
			if run = append(run, c.Emoji); len(run) > MaxRun {
				return "", invalidRun(errRunCount)
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range run {
		if seen[c] {
			return "", invalidRun(fmt.Errorf("%s is used twice", c))
		}
		seen[c] = true
	}
	if len(run) < MinRun {
		return "", invalidRun(errRunCount)
	}
	return strings.Join(run, ""), nil
}

// RunSet is a run's emoji with their order forgotten, as kbeads' EmojiSet
// keys them: two runs with the same RunSet are the same emoji, perhaps in
// another order (the register's look-alike warning).
func RunSet(run string) string {
	var cs []string
	g := uniseg.NewGraphemes(strings.Join(strings.Fields(run), ""))
	for g.Next() {
		cs = append(cs, key(g.Str()))
	}
	sort.Strings(cs)
	return strings.Join(cs, " ")
}

func invalidRun(err error) error {
	return errors.New("invalid arms: " + err.Error() + ". " + Help)
}
