package steward

import (
	"encoding/json"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// The owner errands list (agent-access programme step P9.6,
// docs/superpowers/plans/2026-11-02-agent-access-programme.md): the digest's
// "What you must do" section. Every open owner-only item in the land, a
// decision waiting on a person, an owner-audience escalation (the floor
// included) and an owner-at-device step, is one errand, grouped by the place
// the owner has to be, with the clicks to make there and the evidence that
// closes it. The digest is where an owner-only item waits: it is listed once
// a day here, not filed or pinged again.

// Errand fields a filer may set on the bead (or, on an escalation, as keys of
// operation_spec) so the errand says where to go and what to do. Without
// them the place is read off the operation type or the title, and the
// evidence is the bead closing.
const (
	ErrandPlaceField    = "errand_place"
	ErrandClicksField   = "errand_clicks"
	ErrandEvidenceField = "errand_evidence"
)

// Errand kinds.
const (
	ErrandDecision   = "decision"
	ErrandEscalation = "escalation"
	ErrandStep       = "step"
)

// errandPlaces are the known places, in the order the digest lists them.
// An errand_place outside these is its own group, after them and before
// "other".
var errandPlaces = []struct{ key, name string }{
	{"github", "GitHub"},
	{"slack", "Slack admin"},
	{"keycloak", "Keycloak"},
	{"aws", "AWS console"},
	{"phone", "Phone"},
	{"device", "At a device"},
	{"decisions", "Decisions to answer"},
	{"other", "Elsewhere"},
}

// placeAliases fold the spellings a filer might write to a place key.
var placeAliases = map[string]string{
	"gh": "github", "github settings": "github", "gitlab": "github",
	"slack admin": "slack",
	"kc":          "keycloak",
	"aws console": "aws", "terraform": "aws", "iam": "aws",
	"mobile": "phone", "iphone": "phone", "android": "phone",
	"laptop": "device", "browser": "device", "mac": "device", "at a device": "device",
	"decision": "decisions", "slack decision": "decisions",
	"elsewhere": "other",
}

// Errand is one thing the owner (or the person a decision waits on) must do.
type Errand struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	// Place is the place key (errandPlaces), or the filer's own words when
	// they name no known place.
	Place string `json:"place"`
	// Clicks are the exact steps, as the filer wrote them; "" when none were.
	Clicks string `json:"clicks,omitempty"`
	// Evidence is what closes the errand.
	Evidence string `json:"evidence"`
	// Who is the person a decision waits on.
	Who string `json:"who,omitempty"`
	// New marks an errand filed since the last digest.
	New bool `json:"new,omitempty"`
	// Approved marks an escalation already approved and waiting to be
	// carried out.
	Approved bool `json:"approved,omitempty"`
	// Also are the ids folded into this errand: the same ask filed again, or
	// an escalation filed for this step.
	Also []string `json:"also,omitempty"`

	requestingTask string
}

// PlaceName is how the digest heads the errand's place.
func (e Errand) PlaceName() string { return placeName(e.Place) }

func placeName(key string) string {
	for _, p := range errandPlaces {
		if p.key == key {
			return p.name
		}
	}
	return key
}

func placeRank(key string) int {
	for i, p := range errandPlaces {
		if p.key == key {
			if key == "other" {
				return len(errandPlaces) // after the filers' own places
			}
			return i
		}
	}
	return len(errandPlaces) - 1
}

// errandField reads an errand field off the bead, then off an escalation's
// operation_spec.
func errandField(b *beadsapi.BeadDetail, key string) string {
	if v := strings.TrimSpace(b.Fields[key]); v != "" {
		return v
	}
	var spec map[string]any
	if json.Unmarshal([]byte(b.Fields["operation_spec"]), &spec) == nil {
		if v, ok := spec[key].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// normalizePlace maps a filer's errand_place to a place key, or keeps their
// words when it names no known place.
func normalizePlace(v string) string {
	k := strings.ToLower(strings.TrimSpace(v))
	for _, p := range errandPlaces {
		if k == p.key || k == strings.ToLower(p.name) {
			return p.key
		}
	}
	if a, ok := placeAliases[k]; ok {
		return a
	}
	return strings.TrimSpace(v)
}

// placeFromOperation is the place an escalation's operation type implies, or
// "".
func placeFromOperation(op string) string {
	op = strings.ToLower(strings.TrimSpace(op))
	switch {
	case op == "":
		return ""
	case strings.HasPrefix(op, "github_"), strings.HasPrefix(op, "gitlab_"):
		return "github"
	case strings.HasPrefix(op, "slack_"):
		return "slack"
	case strings.HasPrefix(op, "keycloak_"), op == "gardenio_user_capability":
		return "keycloak"
	case strings.HasPrefix(op, "aws_"), strings.HasPrefix(op, "terraform"), strings.HasPrefix(op, "iam_"):
		return "aws"
	}
	return ""
}

// titleWords name a place when they appear as a word of the title.
var titleWords = []struct{ word, place string }{
	{"github", "github"},
	{"keycloak", "keycloak"},
	{"aws", "aws"},
	{"terraform", "aws"},
	{"iam", "aws"},
	{"slack", "slack"},
	{"phone", "phone"},
	{"iphone", "phone"},
	{"android", "phone"},
}

func placeFromTitle(title string) string {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, tw := range titleWords {
		if slices.Contains(words, tw.word) {
			return tw.place
		}
	}
	return ""
}

// errandFor is b as an errand of kind.
func errandFor(b *beadsapi.BeadDetail, kind string) Errand {
	e := Errand{ID: b.ID, Title: b.Title, Kind: kind, Clicks: errandField(b, ErrandClicksField), Evidence: errandField(b, ErrandEvidenceField)}
	e.Place = normalizePlace(errandField(b, ErrandPlaceField))
	if e.Place == "" && kind == ErrandDecision {
		e.Place = "decisions"
	}
	if e.Place == "" {
		e.Place = placeFromOperation(b.Fields["operation_type"])
	}
	if e.Place == "" {
		e.Place = placeFromTitle(b.Title)
	}
	if e.Place == "" {
		e.Place = map[string]string{ErrandStep: "device", ErrandEscalation: "other"}[kind]
	}
	if e.Evidence == "" {
		e.Evidence = map[string]string{
			ErrandDecision:   "an option is chosen on the card",
			ErrandEscalation: "the escalation is closed with what was done",
			ErrandStep:       "the step is closed with what was seen",
		}[kind]
	}
	switch kind {
	case ErrandDecision:
		e.Who = strings.TrimSpace(b.Fields["operated_by"])
	case ErrandEscalation:
		e.Approved = strings.TrimSpace(b.Fields["approved_by"]) != ""
		e.requestingTask = strings.TrimSpace(b.Fields["requesting_task"])
	}
	return e
}

// foldErrands lists each ask once: an escalation filed for a step already on
// the list joins that step, and an errand with the same place and title as
// an earlier one joins it. The rest keep their order, grouped by place.
func foldErrands(in []Errand) []Errand {
	var out []Errand
	at := map[string]int{} // errand id -> index in out
	byTitle := map[string]int{}
	for _, e := range in {
		key := e.Place + "\x00" + strings.ToLower(strings.Join(strings.Fields(e.Title), " "))
		if i, ok := at[e.requestingTask]; ok && e.requestingTask != "" && out[i].Kind == ErrandStep {
			out[i].Also = append(out[i].Also, e.ID)
			out[i].New = out[i].New || e.New
			continue
		}
		if i, ok := byTitle[key]; ok {
			out[i].Also = append(out[i].Also, e.ID)
			out[i].New = out[i].New || e.New
			continue
		}
		at[e.ID], byTitle[key] = len(out), len(out)
		out = append(out, e)
	}
	slices.SortStableFunc(out, func(a, b Errand) int {
		if ra, rb := placeRank(a.Place), placeRank(b.Place); ra != rb {
			return ra - rb
		}
		return strings.Compare(placeName(a.Place), placeName(b.Place))
	})
	return out
}
