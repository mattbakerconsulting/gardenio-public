// Package mailer is the one transactional mailer for Lands (Track B decision
// B-D10, gd-ESQOD6JOcU). The Gate tells an applicant their application was
// received, approved, denied, questioned or withdrawn; the purse warns a land that it is
// low or empty; the operator's freeze and close reach the tenant; and Commons
// step 4.7d's email digest reuses the same sender.
//
// The package has three implementations of one interface:
//
//   - SES sends through Amazon SES v2 (ses.go). It is the only path that
//     reaches a real inbox.
//   - Recorder keeps every message in memory (recorder.go). Tests and the
//     Gate check-in's dry runs read what would have been sent.
//   - Disabled drops every message. It is what New returns when MAILER_ENABLED
//     is unset, so a binary that wires the mailer sends nothing until the
//     operator turns it on.
//
// The words live in templates.go: one template per Kind, rendered into a
// Message with a plain-text body and an HTML body built from the same text.
//
// Mail is off by default. Turning it on needs the operator's SES sender
// identity and an IRSA role limited to ses:SendEmail on that identity; the
// escalation filed from gd-ESQOD6JOcU carries the exact change.
package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// Mailer sends one transactional message.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Message is one email, ready to send. Templates.Message builds one from a
// Kind; callers rarely fill it by hand.
type Message struct {
	// Kind names the template the message came from. SES tags the send with
	// it so bounces and complaints can be traced to a template. Empty for a
	// hand-built message.
	Kind Kind
	// To holds the recipient addresses. A Lands mail goes to one person.
	To []string
	// Subject is one line; Validate refuses CR or LF in it.
	Subject string
	// Text is the plain-text body. It is required: every client can show it.
	Text string
	// HTML is the optional HTML body, built from Text by Templates.
	HTML string
}

// MaxRecipients is SES's limit on destinations for one SendEmail call.
const MaxRecipients = 50

// ErrInvalidMessage wraps every error Validate returns.
var ErrInvalidMessage = errors.New("mailer: invalid message")

// Validate reports whether m is sendable. Every implementation runs it, so
// the Recorder refuses exactly what SES would.
func Validate(m Message) error {
	if len(m.To) == 0 {
		return fmt.Errorf("%w: no recipient", ErrInvalidMessage)
	}
	if len(m.To) > MaxRecipients {
		return fmt.Errorf("%w: %d recipients, at most %d", ErrInvalidMessage, len(m.To), MaxRecipients)
	}
	for _, to := range m.To {
		if err := validateAddress(to); err != nil {
			return fmt.Errorf("%w: recipient %q: %v", ErrInvalidMessage, to, err)
		}
	}
	if strings.TrimSpace(m.Subject) == "" {
		return fmt.Errorf("%w: empty subject", ErrInvalidMessage)
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return fmt.Errorf("%w: subject contains a line break", ErrInvalidMessage)
	}
	if strings.TrimSpace(m.Text) == "" {
		return fmt.Errorf("%w: empty text body", ErrInvalidMessage)
	}
	return nil
}

// validateAddress accepts a bare address ("a@example.com") or a named one
// ("A <a@example.com>"), and refuses line breaks, which net/mail tolerates
// in some positions but which have no place in a header.
func validateAddress(addr string) error {
	if strings.ContainsAny(addr, "\r\n") {
		return errors.New("contains a line break")
	}
	if _, err := mail.ParseAddress(addr); err != nil {
		return err
	}
	return nil
}

// Disabled is the mailer for a binary whose mail is off. Send validates the
// message, so a caller's bug still surfaces in tests run without mail, and
// then drops it.
type Disabled struct{}

// Send validates m and drops it.
func (Disabled) Send(_ context.Context, m Message) error {
	return Validate(m)
}

// Enabled reports whether m delivers mail, that is, whether it is anything
// other than Disabled. A caller uses it to say "we emailed you" only when it
// did.
func Enabled(m Mailer) bool {
	switch m.(type) {
	case nil, Disabled, *Disabled:
		return false
	}
	return true
}
