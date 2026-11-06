package commons

import (
	"html/template"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
)

// Decision and permission card faces (spec 2.5, Commons II step 3). A decision
// card shows the priority, the prompt and its options as buttons, with Other
// and Escalate opening a lightbox (D12) and Dismiss, while the decision is
// open; once it closes it shows what was chosen and by whom. A permission card
// shows the tool and what it would run, with Allow and Deny while the request
// waits, and the verdict after.
//
// The buttons post to the card action and lightbox submit routes
// (POST /garden/c/action/{card}, /garden/c/submit/{card}, spec 6.3), which
// Commons II step 5 (slice 4a) carries to the bridge's inbound endpoint.

// Bounds on what a card shows of its subject.
const (
	cardPromptMax  = 2000
	cardContextMax = 2900 // the Slack card's limit, so both say the same
	cardOptionMax  = 150  // an option's description, as the Slack card cuts it
	cardPreviewMax = 500  // a permission request's input, as the Slack card cuts it
)

// chosenReturned is the chosen value of a decision handed back to its agent
// for a structured escalation (the Slack card's Escalate button).
const chosenReturned = "returned_to_agent_for_escalation"

// DecisionCard is a decision card's face.
type DecisionCard struct {
	ID       string // the decision bead
	Land     string
	Priority int
	Prompt   string
	Context  string
	// Evidence is the strip of screenshots from the beads the decision is
	// about, under the context (evidence.go, gd-Pi7C83262N); "" when none.
	Evidence template.HTML
	Asker    string // the agent that asked
	// AskerBead is the asking agent's bead in the land (withCards), "" when it
	// is not known: the card's See the screen opens its Glass.
	AskerBead string
	// For is whom the decision asks, its operated_by (else its asking
	// agent's, withCards); Waiting is that in words for this viewer (waiting).
	For     string
	Waiting string
	Options []DecisionOption
	Open    bool
	// The closed face.
	Outcome   string // answered | dismissed | escalated
	Chosen    string // the chosen option's title, or the written answer
	Rationale string
	By        string // who answered
}

// DecisionOption is one answer a decision offers.
type DecisionOption struct {
	N           int // 1-based, the Slack card's "Choose N"
	ID          string
	Title       string
	Label       string // the longer wording, when it differs from Title
	Description string
	Artifact    string // the artifact type the answer requires
	// Recommended is the asker's pick, said under the button; Primary is the
	// one option drawn filled, the rest ghost (rubric C8: one primary per
	// card): the recommended option, else the first.
	Recommended bool
	Primary     bool
}

// Urgency is the priority as the card colours it: the Slack card's red, white
// and green circles.
func (c DecisionCard) Urgency() string {
	switch {
	case c.Priority <= 1:
		return "high"
	case c.Priority >= 3:
		return "low"
	}
	return "normal"
}

// decisionCardFrom reads a decision card's face off its subject bead. ok is
// false when b is not a decision of land.
func decisionCardFrom(b *beadsapi.BeadDetail, land string) (DecisionCard, bool) {
	if b == nil || b.Type != "decision" || landOf(b.Labels) != land {
		return DecisionCard{}, false
	}
	f := b.Fields
	d := decisionview.From(b)
	c := DecisionCard{
		ID: b.ID, Land: land, Priority: b.Priority,
		Prompt:  clipRunes(d.Prompt, cardPromptMax),
		Context: clipRunes(d.Context, cardContextMax),
		Asker:   strings.TrimSpace(firstNonEmpty(f["requested_by"], b.Assignee)),
		For:     firstNonEmpty(b.OperatedBy, f["operated_by"]),
		Options: decisionOptions(d.Options),
		Open:    b.Status != "closed",
	}
	if c.Asker != "" {
		c.Asker = authorName(c.Asker)
	}
	if c.Open {
		return c, true
	}
	chosen := strings.TrimSpace(f["chosen"])
	c.Rationale = clipRunes(strings.TrimSpace(firstNonEmpty(f["rationale"], f["cancel_reason"])), cardContextMax)
	if by := strings.TrimSpace(firstNonEmpty(f["responded_by"], f["canceled_by"])); by != "" {
		c.By = authorName(by)
	}
	switch {
	case chosen == "dismissed" || f["canceled"] == "true":
		c.Outcome = "dismissed"
	case chosen == chosenReturned:
		c.Outcome = "escalated"
	default:
		c.Outcome = "answered"
		c.Chosen = chosen
		for _, o := range c.Options {
			if o.ID == chosen || strconv.Itoa(o.N) == chosen {
				c.Chosen = o.Title
			}
		}
		// A written answer is the answer when no option was chosen, and the
		// reason when one was.
		if text := clipRunes(strings.TrimSpace(f["response_text"]), cardPromptMax); text != "" {
			if c.Chosen == "" {
				c.Chosen = text
			} else if c.Rationale == "" {
				c.Rationale = text
			}
		}
	}
	return c, true
}

// decisionOptions is the shared read model's options as the card's buttons:
// a bare string option (no id) is chosen by its text, as the Slack card does.
// The first recommended option is the primary; with none recommended the
// first option is, since an asker lists its own pick first.
func decisionOptions(opts []decisionview.Option) []DecisionOption {
	var out []DecisionOption
	for _, o := range opts {
		opt := DecisionOption{N: o.N, ID: o.ID, Title: o.Title()}
		if opt.ID == "" && o.Short == "" {
			opt.ID = o.Label
		}
		if o.Label != opt.Title {
			opt.Label = o.Label
		}
		opt.Description = clipRunes(o.Description, cardOptionMax)
		opt.Artifact = o.ArtifactType
		opt.Recommended = o.Recommended
		out = append(out, opt)
	}
	primary := 0
	for i, o := range out {
		if o.Recommended {
			primary = i
			break
		}
	}
	if len(out) > 0 {
		out[primary].Primary = true
	}
	return out
}

// PermissionCard is a tool-permission request's face.
type PermissionCard struct {
	ID    string // the permission bead
	Land  string
	Tool  string
	Agent string
	// AgentBead is the asking agent's bead in the land (withCards), "" when it
	// is not known: the card's See the screen opens its Glass.
	AgentBead string
	// For is whom the request asks, its agent's operated_by (withCards);
	// Waiting is that in words for this viewer (waiting).
	For         string
	Waiting     string
	Description string
	Preview     string // the tool input, cut
	Open        bool
	// The closed face.
	Behavior string // allow | deny
	By       string
	Failed   bool // the verdict did not reach the agent
}

// waiting is who an open ask for principal waits on, in words, as viewer
// reads it (rubric C9: never just "waiting"): "you", the person's name, or
// the owner when the ask names nobody.
func waiting(principal, viewer string) string {
	switch {
	case principal == "":
		return "the owner"
	case viewer != "" && strings.EqualFold(principal, viewer):
		return "you"
	}
	return authorName(principal)
}

// permissionCardFrom reads a permission card's face off its subject bead. ok
// is false when b is not a permission request of land.
func permissionCardFrom(b *beadsapi.BeadDetail, land string) (PermissionCard, bool) {
	if b == nil || b.Type != "permission" || landOf(b.Labels) != land {
		return PermissionCard{}, false
	}
	f := b.Fields
	c := PermissionCard{
		ID: b.ID, Land: land, Tool: strings.TrimSpace(f["tool_name"]), Agent: strings.TrimSpace(f["agent"]),
		Description: clipRunes(strings.TrimSpace(f["description"]), cardContextMax),
		Preview:     clipRunes(f["input_preview"], cardPreviewMax),
		Open:        b.Status != "closed",
	}
	if !c.Open {
		c.Behavior = strings.TrimSpace(f["behavior"])
		// answered_by is the person (oidc:<email>); a verdict recorded before
		// gd-eemeNhUt5I carries "@<slack username>".
		c.By = authorName(strings.TrimPrefix(strings.TrimSpace(f["answered_by"]), "@"))
		c.Failed = strings.TrimSpace(f["verdict_error"]) != ""
	}
	return c, true
}
