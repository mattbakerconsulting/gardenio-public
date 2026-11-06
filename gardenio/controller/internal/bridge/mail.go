// Package bridge provides the mail lifecycle watcher.
//
// Mail subscribes to kbeads SSE event stream for bead create events,
// filters for type=mail beads, and logs them. Delivery is the recipient's
// own mail subscriber (internal/mailsub).
package bridge

import (
	"context"
	"log/slog"
)

// MailConfig holds configuration for the Mail watcher.
type MailConfig struct {
	Daemon BeadClient
	Logger *slog.Logger
}

// Mail watches the kbeads SSE event stream for mail bead lifecycle events.
type Mail struct {
	daemon BeadClient
	logger *slog.Logger
}

// NewMail creates a new mail lifecycle watcher.
func NewMail(cfg MailConfig) *Mail {
	return &Mail{
		daemon: cfg.Daemon,
		logger: cfg.Logger,
	}
}

// RegisterHandlers registers SSE event handlers on the given stream for
// mail bead created events.
func (m *Mail) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", m.handleCreated)
	m.logger.Info("mail watcher registered SSE handlers",
		"topics", []string{"beads.bead.created"})
}

func (m *Mail) handleCreated(_ context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		m.logger.Debug("skipping malformed bead created event")
		return
	}
	if bead.Type != "mail" {
		return
	}

	m.logger.Info("mail bead created",
		"id", bead.ID,
		"title", bead.Title,
		"assignee", bead.Assignee,
		"priority", bead.Priority)

	// Delivery is the recipient's own subscriber (internal/mailsub); the
	// bridge only observes. Kept as a handler so the log line stays.
}
