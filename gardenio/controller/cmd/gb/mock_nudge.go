package main

import (
	"context"
	"strings"

	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/harness"
	"gardenio/controller/internal/mailsub"
)

// claudelessASCII folds text to ASCII for a claudeless agent. claudeless
// 0.4.0's TUI aborts (SIGABRT) on any non-ASCII rune typed into its input, so
// a work prompt or mail nudge carrying an em dash kills the mock session
// (gd-K77LsUtqhK). Common punctuation maps to its ASCII look-alike; any other
// non-ASCII rune becomes '?'.
func claudelessASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r == '–' || r == '—' || r == '−':
			b.WriteByte('-')
		case r == '‘' || r == '’':
			b.WriteByte('\'')
		case r == '“' || r == '”':
			b.WriteByte('"')
		case r == '…':
			b.WriteString("...")
		case r == '→':
			b.WriteString("->")
		case r == ' ':
			b.WriteByte(' ')
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// mockSafeNudge returns msg folded to ASCII when command runs claudeless, and
// msg unchanged otherwise.
func mockSafeNudge(command, msg string) string {
	if harness.IsMockCommand(command) {
		return claudelessASCII(msg)
	}
	return msg
}

// mailCoop is the coop handle the mail subscriber delivers through. For a
// claudeless command it folds every mail nudge to ASCII.
func mailCoop(coop mailsub.Coop, command string) mailsub.Coop {
	if harness.IsMockCommand(command) {
		return asciiNudgeCoop{Coop: coop}
	}
	return coop
}

type asciiNudgeCoop struct {
	mailsub.Coop
}

func (c asciiNudgeCoop) Nudge(ctx context.Context, message string) (*coopapi.NudgeResponse, error) {
	return c.Coop.Nudge(ctx, claudelessASCII(message))
}
