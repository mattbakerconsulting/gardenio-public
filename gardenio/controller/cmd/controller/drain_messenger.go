package main

import (
	"context"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/mailproto"
)

// drainMessenger is how the reconciler's drain tracker reaches an agent whose
// pod is about to go (node reclaim, account rotation): the
// commit+push message goes out as interrupt mail from:controller (NudgeSender,
// set in main), and the agent bead's agent_state says when the turn is over.
// Nothing dials the pod.
type drainMessenger struct {
	*beadsapi.Client
	logger *slog.Logger
}

func (m drainMessenger) Nudge(ctx context.Context, agentName, message string) (string, error) {
	return bridge.NudgeAgentMail(ctx, m.Client, m.logger, agentName, message)
}

// CloseMail closes a drain mail once its drain is over, stamped like the mail
// subscriber's kill close (control_acted). Nobody runs `gb mail read` on it,
// and the next session under the same agent name would otherwise drain it and
// stop at once.
func (m drainMessenger) CloseMail(ctx context.Context, mailID string) error {
	acted := map[string]string{mailproto.FieldControlActed: time.Now().UTC().Format(time.RFC3339)}
	return m.CloseBead(ctx, mailID, acted)
}
