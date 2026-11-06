// Package decisionview is the one read model of a decision bead for display:
// what every surface that shows a decision, and above all every surface that
// lets a person ANSWER one, reads it through. beads-viewer (the bead page,
// /decisions, /deck, /up, a plan's clarifying questions), /garden (the
// gardenhub Mailbox and bead panel) and the Commons decision card all
// assemble their view from a Decision, so they agree on where the prompt, the
// context and the options come from, and so a fix to the reading lands
// everywhere at once (epic gd-hmwSI9lwKe, step 1: gd-hR3ImbMVhP).
//
// The rule the epic establishes: a decision's resolve control is never
// rendered without its context on the same surface. The context is what the
// agent wrote to make the decision decidable, and it lives in two places
// today: `gb decision create --context` writes Fields["context"] only, while
// older writers (the bridge's Grafana-conflict decisions, until this package
// landed) wrote Description only. Context reads the field first and falls
// back to the description, so both generations of decisions show it.
//
// A Decision is pure: it is built from the bead alone and never clipped,
// so a surface clips to its own budget. Comments are not on the bead detail;
// a caller that has fetched them sets Comments itself.
package decisionview

import (
	"encoding/json"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Decision is a decision bead as a surface shows it.
type Decision struct {
	ID string
	// Prompt is the question: Fields["prompt"], else Fields["question"] (the
	// older key), else the bead's title.
	Prompt string
	// Context is the background the agent wrote so the decision can be made:
	// Fields["context"], else the description. "" when there is none, which
	// a surface must say rather than render a resolve control over nothing.
	Context string
	// ContextSource names where Context came from: "context" (the field),
	// "description" (the fallback), or "" when there is none. For diagnostics
	// and for tests of the writers.
	ContextSource string
	// Options are the answers offered, in order, numbered from 1.
	Options []Option
	// Attachments are the files on the decision bead (Fields["files"], the
	// same JSON the Commons files store writes on a message bead). Step 4 of
	// the epic (gd-JjsT1AZiny) fills and renders them; the model carries them
	// from the start so no surface grows its own parser.
	Attachments []Attachment
	// Comments is how many comments the bead has, when the caller fetched
	// them (follow-ups land in comments because the context is immutable
	// after create). 0 when unknown.
	Comments int
	// RequestedBy is the agent that asked (Fields["requested_by"], else the
	// bead's creator).
	RequestedBy string
	// Open is true while the decision can still be answered.
	Open bool
}

// Option is one answer a decision offers.
type Option struct {
	N int // 1-based position, what the Commons and Slack cards call "Choose N"
	// ID is what a resolve posts; an option without one (a bare string in
	// the options array) can only be matched by its label.
	ID           string
	Short        string
	Label        string
	Description  string
	ArtifactType string
	// Recommended is the asker's pick ("recommended": true on the option), the
	// one a card may draw as its primary button.
	Recommended bool
}

// Title is the option as a button reads it: the short wording, else the
// label, else the id, else its number.
func (o Option) Title() string {
	for _, s := range []string{o.Short, o.Label, o.ID} {
		if s != "" {
			return s
		}
	}
	return strconv.Itoa(o.N)
}

// Attachment is one file on a decision.
type Attachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mimetype,omitempty"`
	Size     int    `json:"size,omitempty"`
	URL      string `json:"url"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// Image is true for a raster or vector image attachment.
func (a Attachment) Image() bool { return strings.HasPrefix(a.MimeType, "image/") }

// From reads a decision off its bead. It does not check b.Type: a surface
// that reaches here has already decided the bead is a decision.
func From(b *beadsapi.BeadDetail) Decision {
	if b == nil {
		return Decision{}
	}
	f := b.Fields
	d := Decision{
		ID:          b.ID,
		Prompt:      strings.TrimSpace(first(f["prompt"], f["question"], b.Title)),
		Options:     ParseOptions(f["options"]),
		Attachments: ParseAttachments(f["files"]),
		RequestedBy: strings.TrimSpace(first(f["requested_by"], b.CreatedBy)),
		Open:        b.Status != "closed" && b.Status != "resolved",
	}
	d.Context, d.ContextSource = Context(b)
	return d
}

// Context is the decision's context and where it came from: Fields["context"]
// first, else the bead's description. Whitespace-only is none.
func Context(b *beadsapi.BeadDetail) (text, source string) {
	if b == nil {
		return "", ""
	}
	if s := strings.TrimSpace(b.Fields["context"]); s != "" {
		return s, "context"
	}
	if s := strings.TrimSpace(b.Description); s != "" {
		return s, "description"
	}
	return "", ""
}

// ParseOptions reads a decision's options field: a JSON array whose elements
// are objects ({id, short, label, description, artifact_type, recommended})
// or bare strings. Each element is decoded on its own so one malformed element, or
// a string among objects (ka-0000006724), never drops the rest: a bare
// string becomes an option with that label and no id. Anything that is not
// a JSON array reads as no options.
func ParseOptions(raw string) []Option {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &elems); err != nil {
		return nil
	}
	out := make([]Option, 0, len(elems))
	for _, el := range elems {
		o := Option{N: len(out) + 1}
		var m map[string]any
		if err := json.Unmarshal(el, &m); err != nil {
			var s string
			if json.Unmarshal(el, &s) != nil || strings.TrimSpace(s) == "" {
				continue
			}
			o.Label = strings.TrimSpace(s)
			out = append(out, o)
			continue
		}
		o.ID = str(m, "id")
		o.Short = str(m, "short")
		o.Label = str(m, "label")
		o.Description = str(m, "description")
		o.ArtifactType = str(m, "artifact_type")
		o.Recommended, _ = m["recommended"].(bool)
		if o.ID == "" && o.Short == "" && o.Label == "" {
			continue // nothing to show and nothing to choose
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseAttachments reads a files field: a JSON array of {name, mimetype,
// size, url, width, height}. An entry without an absolute http(s) URL is
// dropped; a malformed field reads as none.
func ParseAttachments(raw string) []Attachment {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var refs []Attachment
	if json.Unmarshal([]byte(raw), &refs) != nil {
		return nil
	}
	out := refs[:0]
	for _, a := range refs {
		u := strings.ToLower(a.URL)
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			continue
		}
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" {
			a.Name = "file"
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func first(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
