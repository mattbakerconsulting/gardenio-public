package bridge

import (
	"log/slog"

	"github.com/slack-go/slack"
)

// slackUserResolver resolves Slack user IDs to display names with per-instance
// caching. Use one resolver per request to avoid stale names while still
// deduplicating GetUserInfo calls within the request.
type slackUserResolver struct {
	api    *slack.Client
	logger *slog.Logger
	cache  map[string]string
}

// newSlackUserResolver creates a resolver scoped to a single request.
func newSlackUserResolver(api *slack.Client, logger *slog.Logger) *slackUserResolver {
	return &slackUserResolver{
		api:    api,
		logger: logger,
		cache:  make(map[string]string),
	}
}

// resolve maps a Slack user ID to a human-readable display name. Empty input
// returns empty output. Lookup failures cache and return the raw ID so
// repeated calls do not retry.
func (r *slackUserResolver) resolve(userID string) string {
	if userID == "" {
		return ""
	}
	if name, ok := r.cache[userID]; ok {
		return name
	}
	if r.api == nil {
		r.cache[userID] = userID
		return userID
	}
	info, err := r.api.GetUserInfo(userID)
	if err != nil {
		if r.logger != nil {
			r.logger.Debug("failed to resolve user", "user_id", userID, "error", err)
		}
		r.cache[userID] = userID
		return userID
	}
	name := info.Profile.DisplayName
	if name == "" {
		name = info.RealName
	}
	if name == "" {
		name = info.Name
	}
	if name == "" {
		name = userID
	}
	r.cache[userID] = name
	return name
}
