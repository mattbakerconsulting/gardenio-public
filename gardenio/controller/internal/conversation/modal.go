package conversation

import (
	"errors"
	"fmt"
)

// Modal is a small surface-neutral form: enough for the modals that exist
// today (decision Other, decision resolve/Promote, manual survey, feedback in
// bot_decisions_modal.go and bot_human_survey_manual.go). The Slack surface
// builds a slack.ModalViewRequest from it; the Commons returns it as a lightbox
// fragment (D12).
type Modal struct {
	CallbackID string // submit id: today's Slack callback_id
	Title      string
	Submit     string // submit button label ("" = no submit button)
	Close      string // close button label ("" = surface default)
	Fields     []Field
	// Private is opaque state handed back on submit (Slack private_metadata).
	// Kept a string, not a map, so today's "a|b|c" encodings move unchanged.
	Private string
}

// FieldKind is the kind of one modal row.
type FieldKind string

const (
	FieldNote    FieldKind = "note"    // markdown text, no input
	FieldDivider FieldKind = "divider" // a rule
	FieldText    FieldKind = "text"    // free-text input
	FieldSelect  FieldKind = "select"  // one choice from Options
)

// Field is one row of a Modal.
type Field struct {
	Kind        FieldKind
	Text        string // FieldNote: markdown
	ID          string // input block id; Inbound.Values is keyed on it
	InputID     string // input element id (Slack action_id)
	Label       string
	Hint        string
	Placeholder string
	Multiline   bool
	Optional    bool
	Options     []Option // FieldSelect
	Initial     string   // FieldSelect: initially selected Option.Value
}

// Option is one choice of a FieldSelect.
type Option struct {
	Value string
	Label string
}

// ErrBadModal is returned for a Modal a surface cannot render.
var ErrBadModal = errors.New("conversation: invalid modal")

// Validate checks the shape every surface relies on: a callback id and title,
// an id and label on every input, options on every select, and an Initial that
// names one of them.
func (v Modal) Validate() error {
	if v.CallbackID == "" || v.Title == "" {
		return fmt.Errorf("%w: callback id and title are required", ErrBadModal)
	}
	for i, f := range v.Fields {
		switch f.Kind {
		case FieldNote, FieldDivider:
		case FieldText, FieldSelect:
			if f.ID == "" || f.InputID == "" || f.Label == "" {
				return fmt.Errorf("%w: field %d: input needs id, input id and label", ErrBadModal, i)
			}
			if f.Kind == FieldSelect && len(f.Options) == 0 {
				return fmt.Errorf("%w: field %d: select needs options", ErrBadModal, i)
			}
			if f.Kind == FieldSelect && f.Initial != "" && !hasOption(f.Options, f.Initial) {
				return fmt.Errorf("%w: field %d: initial %q is not an option", ErrBadModal, i, f.Initial)
			}
		default:
			return fmt.Errorf("%w: field %d: unknown kind %q", ErrBadModal, i, f.Kind)
		}
	}
	return nil
}

func hasOption(opts []Option, value string) bool {
	for _, o := range opts {
		if o.Value == value {
			return true
		}
	}
	return false
}
