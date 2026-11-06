// Package respond is the garden's one way to answer an agent's prompt (Lands workbench-glass spec GL-8, step 6.5,
// gd-m1aiBXtDak): a Form of labelled answers and the "respond" template that renders it, shared by the Glass (the
// prompt under a watched terminal, internal/gardenhub glass_send.go) and the Commons tool-permission card
// (internal/commons render.go). Each answer is its own small form, so it works without script; the page's script
// (web/glass.js, web/commons.js) posts it and shows the outcome.
//
// FromPrompt builds the Form from coop's prompt schema (coopmuxapi.Prompt, coop's PromptContext) the way coopmux's own
// actions panel reads it (coop/crates/web/src/components/inspector/ActionsPanel.tsx): every option by its label, never
// a bare index. Check turns a submitted Answer back into coop's respond request, but only against the prompt the agent
// is at now: an answer chosen for an option the prompt no longer has at that place (the prompt moved on, or its options
// were re-read) is ErrChanged, never forwarded, so a stale button cannot approve the wrong thing (spec §8, Respond
// indexing).
package respond

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gardenio/controller/internal/coopmuxapi"
)

// Field is one hidden form field.
type Field struct{ Name, Value string }

// Choice is one answer: a button that submits Form.Name=Value.
type Choice struct {
	Label string
	Value string
	// Class is added to the button's classes (to the summary's when the choice is confirmed first).
	Class string
	// Hidden fields this answer also submits: the option's label, checked again when it lands.
	Hidden []Field
	// Confirm puts the answer behind a second step: Label (with an ellipsis) opens it, Confirm says what it does, and
	// ConfirmLabel submits it.
	Confirm      string
	ConfirmLabel string
}

// Text is an answer in the person's own words.
type Text struct {
	Name        string
	Label       string // what the field is for, said above it
	Placeholder string
	Submit      string // the button's words
	Required    bool
	Max         int // characters, 0 for no cap
	Hidden      []Field
}

// Form is a prompt's answers.
type Form struct {
	// Action is where every answer posts.
	Action string
	// Hook names the script that posts the forms: "glass" (web/glass.js) or anything else for the Commons
	// (web/commons.js, data-commons="act").
	Hook string
	// Class is the wrapper's class.
	Class string
	// Name is the field each Choice sets.
	Name string
	// Hidden fields every form submits.
	Hidden  []Field
	Choices []Choice
	Text    *Text
	// Link is a page the person opens to answer (a setup prompt's sign-in page); LinkLabel says what it is.
	Link      string
	LinkLabel string
}

// Template is the "respond" template. A package parses it into its own set: {{template "respond" .}} with a Form.
const Template = `{{define "respond"}}<div class="{{.Class}}">
{{- with .Link}}
<p class="rsp-link"><a class="btn small ghost" href="{{.}}" target="_blank" rel="noopener noreferrer">{{$.LinkLabel}}</a></p>
{{- end}}
{{- range .Choices}}
{{- if .Confirm}}
<details class="cm-switch cm-kill"><summary class="btn ghost{{with .Class}} {{.}}{{end}}">{{.Label}}…</summary>
<form class="cm-killconfirm" method="post" action="{{$.Action}}" {{if eq $.Hook "glass"}}data-glass="respond"{{else}}data-commons="act"{{end}}>{{range $.Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}{{range .Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}
<p class="cm-note">{{.Confirm}}</p>
<button class="btn small cm-killbtn" type="submit" name="{{$.Name}}" value="{{.Value}}">{{.ConfirmLabel}}</button>
</form>
</details>
{{- else}}
<form method="post" action="{{$.Action}}" {{if eq $.Hook "glass"}}data-glass="respond"{{else}}data-commons="act"{{end}}>{{range $.Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}{{range .Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}<button class="btn{{with .Class}} {{.}}{{end}}" type="submit" name="{{$.Name}}" value="{{.Value}}">{{.Label}}</button></form>
{{- end}}
{{- end}}
{{- with .Text}}
<form class="rsp-text" method="post" action="{{$.Action}}" {{if eq $.Hook "glass"}}data-glass="respond"{{else}}data-commons="act"{{end}}>{{range $.Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}{{range .Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">{{end}}
<label class="rsp-text-label">{{.Label}}
<textarea name="{{.Name}}" rows="2"{{if .Max}} maxlength="{{.Max}}"{{end}}{{if .Placeholder}} placeholder="{{.Placeholder}}"{{end}}{{if .Required}} required{{end}}></textarea></label>
<button class="btn small" type="submit">{{.Submit}}</button>
</form>
{{- end}}
</div>{{end}}`

// Prompt types, as coop names them.
const (
	TypePermission = "permission"
	TypePlan       = "plan"
	TypeQuestion   = "question"
	TypeSetup      = "setup"
)

// Answer field names, as the forms FromPrompt builds submit them.
const (
	FieldPrompt   = "prompt"
	FieldOption   = "option"
	FieldLabel    = "label"
	FieldText     = "text"
	FieldQuestion = "question"
	FieldConfirm  = "confirm"
)

// The confirm step of a question dialog: submit the answers, or cancel the dialog.
const (
	ConfirmSubmit = "submit"
	ConfirmCancel = "cancel"
)

// TextMax caps an answer in the person's own words, as beads-viewer's /agents/{id}/respond does.
const TextMax = 8 * 1024

// Heading is what a prompt asks, in words: "Permission needed", "Plan to approve", ...
func Heading(p *coopmuxapi.Prompt) string {
	if p == nil {
		return "No prompt"
	}
	switch p.Type {
	case TypePermission:
		if p.Subtype == "trust" {
			return "Trust this workspace?"
		}
		return "Permission needed"
	case TypePlan:
		return "Plan to approve"
	case TypeQuestion:
		if q := current(p); q != nil {
			return "Question"
		}
		return "Answers ready to submit"
	case TypeSetup:
		switch p.Subtype {
		case "oauth_login":
			return "Sign-in needed"
		case "fable_consent":
			return "Consent needed"
		}
		if p.Subtype != "" {
			return "Setup: " + strings.ReplaceAll(p.Subtype, "_", " ")
		}
		return "Setup"
	}
	return "Prompt"
}

// Question is the question a question dialog is asking now, "" for none.
func Question(p *coopmuxapi.Prompt) string {
	if q := current(p); q != nil {
		return q.Question
	}
	return ""
}

func current(p *coopmuxapi.Prompt) *coopmuxapi.PromptQuestion {
	if p == nil || p.Type != TypeQuestion || p.QuestionCurrent < 0 || p.QuestionCurrent >= len(p.Questions) {
		return nil
	}
	return &p.Questions[p.QuestionCurrent]
}

// FromPrompt is the Form for p, posting to action with hook's script; base are hidden fields every answer carries
// (the route's own, such as type=respond). ok is false when there is nothing to answer yet: no prompt, a type coop does
// not name, or a permission or plan prompt whose options coop is still reading.
func FromPrompt(p *coopmuxapi.Prompt, action, hook string, base ...Field) (Form, bool) {
	f := Form{Action: action, Hook: hook, Class: "rsp", Name: FieldOption}
	if p == nil {
		return f, false
	}
	f.Hidden = append(append([]Field(nil), base...), Field{FieldPrompt, p.Type})
	option := func(i int, label string, extra ...Field) Choice {
		label = strings.TrimSpace(label)
		return Choice{Label: label, Value: strconv.Itoa(i + 1), Hidden: append([]Field{{FieldLabel, label}}, extra...)}
	}
	switch p.Type {
	case TypePermission:
		if len(p.Options) == 0 {
			return f, false
		}
		for i, o := range p.Options {
			f.Choices = append(f.Choices, option(i, o))
		}
	case TypePlan:
		if len(p.Options) == 0 {
			return f, false
		}
		last := len(p.Options) - 1
		if p.OptionsFallback || last == 0 {
			last = -1 // coop's stand-ins (Accept, Cancel) take no feedback
		}
		for i, o := range p.Options {
			if i != last {
				f.Choices = append(f.Choices, option(i, o))
			}
		}
		if last >= 0 {
			label := strings.TrimSpace(p.Options[last])
			f.Text = &Text{Name: FieldText, Label: "Or tell it what to change", Placeholder: label, Submit: label, Max: TextMax,
				Hidden: []Field{{FieldOption, strconv.Itoa(last + 1)}, {FieldLabel, label}}}
		}
	case TypeQuestion:
		q := current(p)
		if q == nil { // the confirm step: every question is answered
			f.Name = FieldConfirm
			f.Choices = []Choice{{Label: "Submit the answers", Value: ConfirmSubmit, Class: "cm-allow"}, {Label: "Cancel", Value: ConfirmCancel}}
			return f, true
		}
		at := Field{FieldQuestion, strconv.Itoa(p.QuestionCurrent)}
		for i, o := range q.Options {
			f.Choices = append(f.Choices, option(i, o, at))
		}
		f.Text = &Text{Name: FieldText, Label: "Or answer in your own words", Submit: "Send the answer", Required: true, Max: TextMax,
			Hidden: []Field{at}}
	case TypeSetup:
		opts := p.Options
		if len(opts) == 0 {
			opts = []string{"Continue"}
		}
		for i, o := range opts {
			f.Choices = append(f.Choices, option(i, o))
		}
		if p.Subtype == "oauth_login" && isWebURL(p.Input) {
			f.Link, f.LinkLabel = p.Input, "Open the sign-in page"
		}
	default:
		return f, false
	}
	return f, true
}

func isWebURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// Answer is one submitted answer, as the forms FromPrompt builds submit it.
type Answer struct {
	// Prompt is the prompt type the answer was chosen for.
	Prompt string
	// Option is the chosen option, 1-based; 0 for none.
	Option int
	// Label is the chosen option's label as the person saw it.
	Label string
	// Text is an answer in the person's own words.
	Text string
	// Question is the question dialog's question it answers, 0-based; -1 for none.
	Question int
	// Confirm is ConfirmSubmit or ConfirmCancel at a question dialog's confirm step.
	Confirm string
}

// ParseAnswer reads an Answer from the submitted form values.
func ParseAnswer(get func(string) string) (Answer, error) {
	a := Answer{Prompt: strings.TrimSpace(get(FieldPrompt)), Label: strings.TrimSpace(get(FieldLabel)), Text: get(FieldText),
		Question: -1, Confirm: strings.TrimSpace(get(FieldConfirm))}
	if v := strings.TrimSpace(get(FieldOption)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 99 {
			return a, errors.New("option must be a number from 1")
		}
		a.Option = n
	}
	if v := strings.TrimSpace(get(FieldQuestion)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 99 {
			return a, errors.New("question must be a number from 0")
		}
		a.Question = n
	}
	if len(a.Text) > TextMax {
		return a, errors.New("the answer is too long")
	}
	if a.Prompt == "" {
		return a, errors.New("the answer names no prompt")
	}
	if a.Option == 0 && strings.TrimSpace(a.Text) == "" && a.Confirm == "" {
		return a, errors.New("the answer chooses nothing")
	}
	return a, nil
}

// ErrChanged is Check's answer for an answer chosen for a prompt the agent is no longer at, or for an option the
// prompt no longer has at that place.
var ErrChanged = errors.New("the prompt changed")

// ErrNoPrompt is Check's answer when the agent is at no prompt.
var ErrNoPrompt = errors.New("the agent is at no prompt")

// Reply is what Check forwards: a respond request, or (a question dialog's confirm step) named keys.
type Reply struct {
	Request *coopmuxapi.RespondRequest
	Keys    []string
	// Said is the answer in words, for the audit line and the person: the option's label, "own words", ...
	Said string
}

// Check turns a against p, the prompt the agent is at now, into what coop is sent. A chosen option must still be at
// its place with the label the person saw.
func Check(p *coopmuxapi.Prompt, a Answer) (Reply, error) {
	if p == nil {
		return Reply{}, ErrNoPrompt
	}
	if a.Prompt != p.Type {
		return Reply{}, fmt.Errorf("%w: it is a %s prompt now", ErrChanged, p.Type)
	}
	labelAt := func(opts []string) error {
		if a.Option < 1 || a.Option > len(opts) {
			return fmt.Errorf("%w: it has no option %d now", ErrChanged, a.Option)
		}
		if got := strings.TrimSpace(opts[a.Option-1]); got != a.Label {
			return fmt.Errorf("%w: option %d is %q now", ErrChanged, a.Option, got)
		}
		return nil
	}
	n := a.Option
	switch p.Type {
	case TypePermission, TypeSetup:
		opts := p.Options
		if p.Type == TypeSetup && len(opts) == 0 {
			opts = []string{"Continue"}
		}
		if err := labelAt(opts); err != nil {
			return Reply{}, err
		}
		return Reply{Request: &coopmuxapi.RespondRequest{Option: &n}, Said: a.Label}, nil
	case TypePlan:
		if err := labelAt(p.Options); err != nil {
			return Reply{}, err
		}
		req := &coopmuxapi.RespondRequest{Option: &n}
		said := a.Label
		if t := strings.TrimSpace(a.Text); t != "" {
			if a.Option != len(p.Options) || p.OptionsFallback {
				return Reply{}, fmt.Errorf("%w: only its last option takes words", ErrChanged)
			}
			req.Text = &t
			said += ", in own words"
		}
		return Reply{Request: req, Said: said}, nil
	case TypeQuestion:
		q := current(p)
		if q == nil {
			switch {
			case a.Confirm == ConfirmSubmit:
				return Reply{Keys: []string{"Enter"}, Said: "submit the answers"}, nil
			case a.Confirm == ConfirmCancel:
				return Reply{Keys: []string{"Escape"}, Said: "cancel"}, nil
			}
			return Reply{}, fmt.Errorf("%w: every question is answered", ErrChanged)
		}
		if a.Confirm != "" || a.Question != p.QuestionCurrent {
			return Reply{}, fmt.Errorf("%w: it is asking question %d now", ErrChanged, p.QuestionCurrent+1)
		}
		if t := strings.TrimSpace(a.Text); t != "" && a.Option == 0 {
			return Reply{Request: &coopmuxapi.RespondRequest{Answers: []coopmuxapi.QuestionAnswer{{Text: &t}}}, Said: "own words"}, nil
		}
		if err := labelAt(q.Options); err != nil {
			return Reply{}, err
		}
		return Reply{Request: &coopmuxapi.RespondRequest{Answers: []coopmuxapi.QuestionAnswer{{Option: &n}}}, Said: a.Label}, nil
	}
	return Reply{}, fmt.Errorf("%w: coop names no %q prompt", ErrChanged, p.Type)
}
