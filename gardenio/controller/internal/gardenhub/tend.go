package gardenhub

import (
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Send a gardener (gd-I3Qx07esJj): the in-garden bead view offers "Send a gardener" on every bead (web/tend.js). One
// press says an @gardenio mention in the bead's land's general channel, the Commons compose path Adopt also takes
// (internal/commons mention.go), which starts a thread agent with the prompt as its brief: by default, to figure out
// what is up with the bead and make progress on it, or say what unblocks it. The mention opens with one fixed phrase,
// "@gardenio tend <bead>:", and the bridge keeps a thread's root text on the agent bead (slack_root_text, both for a
// Commons thread and a Slack one), so the farm can read which bead a gardener was sent to without a new field or
// route: the gardener then stands on that bead's plot (FarmGardener.Tends), the Porch names it, and the bead view lists
// the gardeners sent to it and what they said when they left (FarmExit.Tends). web/tend.js carries the same grammar;
// TestTendGrammarUnderNode pins the two together.

// tendRe is the opening phrase of a Send-a-gardener mention. The bead id is a two-letter prefix, a dash and the id
// body (gd-lAPxJpwcYd, ka-0000001089); a colon or the end of the text closes it, so a mention that merely talks about
// tending is not one.
var tendRe = regexp.MustCompile(`^\s*@gardenio\s+tend\s+([A-Za-z]{2}-[A-Za-z0-9]{2,20})\s*(?::|$)`)

// tendMax bounds the root text read: the phrase is at its head, and the bridge cuts the field at 4000 bytes anyway.
const tendMax = 400

// TendedBead returns the bead a thread agent was sent to by the garden's "Send a gardener", read from the mention
// that started it (the thread's root text on its agent bead), or "" for any other agent.
func TendedBead(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	return tendedIn(b.Fields["slack_root_text"])
}

// tendedIn parses the mention text itself.
func tendedIn(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > tendMax {
		text = text[:tendMax]
	}
	m := tendRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}
