package bridge

import "strings"

// PronounSet holds the three grammatical forms of a pronoun set.
type PronounSet struct {
	Subject    string // he, she, they
	Object     string // him, her, them
	Possessive string // his, her, their
}

// knownPronouns maps subject pronouns to their full sets.
var knownPronouns = map[string]PronounSet{
	"he":  {Subject: "he", Object: "him", Possessive: "his"},
	"she": {Subject: "she", Object: "her", Possessive: "her"},
	"they": {Subject: "they", Object: "them", Possessive: "their"},
	"ze":  {Subject: "ze", Object: "zir", Possessive: "zir"},
	"xe":  {Subject: "xe", Object: "xem", Possessive: "xyr"},
}

// defaultPronouns is used when no pronouns are set — gender-neutral "they/them".
var defaultPronouns = PronounSet{Subject: "they", Object: "them", Possessive: "their"}

// ParsePronouns parses a pronoun string like "he/him", "she/her", "they/them"
// into a PronounSet. For compound pronouns like "he/they", uses the first.
// Returns the default "they/them" if the input is empty or unrecognized.
func ParsePronouns(raw string) PronounSet {
	if raw == "" {
		return defaultPronouns
	}

	// Normalize: lowercase, trim whitespace.
	raw = strings.ToLower(strings.TrimSpace(raw))

	// Split on "/" — formats like "he/him", "she/her/hers", "he/they".
	parts := strings.Split(raw, "/")
	if len(parts) == 0 {
		return defaultPronouns
	}

	subject := strings.TrimSpace(parts[0])
	if ps, ok := knownPronouns[subject]; ok {
		return ps
	}

	return defaultPronouns
}

// SubjectPronoun returns the subject pronoun ("he", "she", "they") for the user.
func (u *UserIdentity) SubjectPronoun() string {
	return ParsePronouns(u.Pronouns).Subject
}

// ObjectPronoun returns the object pronoun ("him", "her", "them") for the user.
func (u *UserIdentity) ObjectPronoun() string {
	return ParsePronouns(u.Pronouns).Object
}

// PossessivePronoun returns the possessive pronoun ("his", "her", "their") for the user.
func (u *UserIdentity) PossessivePronoun() string {
	return ParsePronouns(u.Pronouns).Possessive
}

// appendPronouns returns "<name> (<pronouns>)" when the resolved user has
// pronouns set on their user bead, else returns name unchanged. Used to
// enrich Slack-attribution lines so downstream agents know how to refer
// to the speaker. Pronouns absent from the bead means we don't append —
// we don't want to invent or default them in user-visible attributions.
func appendPronouns(name string, u *UserIdentity) string {
	if u == nil || strings.TrimSpace(u.Pronouns) == "" {
		return name
	}
	return name + " (" + strings.TrimSpace(u.Pronouns) + ")"
}
