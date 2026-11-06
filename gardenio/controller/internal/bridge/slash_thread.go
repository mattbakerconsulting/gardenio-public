package bridge

import (
	"context"
	"encoding/json"
)

// slashThreadTSKey is the context key under which the originating Slack
// thread timestamp is stored for a slash command. The slack-go
// slack.SlashCommand struct does not surface `thread_ts`, so the socketmode
// dispatch re-parses the raw payload and attaches the value to the handler
// context via WithSlashThreadTS.
type slashThreadTSKey struct{}

// WithSlashThreadTS attaches the originating Slack thread timestamp to the
// context. Empty values are stored unchanged; callers should rely on
// SlashThreadTSFromContext returning "" when no thread context is present.
func WithSlashThreadTS(ctx context.Context, threadTS string) context.Context {
	return context.WithValue(ctx, slashThreadTSKey{}, threadTS)
}

// SlashThreadTSFromContext returns the originating Slack thread timestamp
// attached by WithSlashThreadTS, or "" when the slash command was not run
// from within a thread.
func SlashThreadTSFromContext(ctx context.Context) string {
	v, _ := ctx.Value(slashThreadTSKey{}).(string)
	return v
}

// parseSlashThreadTS extracts thread_ts from the raw socketmode slash-command
// payload. slack-go v0.18.0's slack.SlashCommand struct drops unknown fields,
// so the canonical thread timestamp must be recovered from the raw JSON.
// Returns "" when the payload is empty, unparseable, or carries no thread_ts.
func parseSlashThreadTS(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var probe struct {
		ThreadTS string `json:"thread_ts"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return ""
	}
	return probe.ThreadTS
}
