package bridge

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gardenio/controller/internal/conversation"
)

// A Commons message posted into Slack shows as its author (gd-kktsmUog17):
// with the chat:write.customize scope the mirror posts under the author's
// display name and Slack avatar, and the text is the words alone. Without the
// scope it posts as the bot, attributed "*Name* (via Commons)" (viaCommons).
// The scope is read off Slack's own responses, so the mirror switches over by
// itself once the app is reinstalled with it, and back if it is removed.
//
// Either way the post is the bot's (it carries bot_id), so the loop guard in
// mirrorSlackMessage is unchanged, and the sweep's double-post guard
// (CommonsMirror.slackCopy) recognises both forms.

// scopeChatWriteCustomize lets a bot post under another username and avatar.
const scopeChatWriteCustomize = "chat:write.customize"

// slackHTTPClient is the client slack.OptionHTTPClient takes.
type slackHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// slackScopes is the bot token's OAuth scopes as Slack last reported them.
// Every Web API response carries an X-OAuth-Scopes header, so a scope a
// reinstall adds is seen on the next call, without a restart. Nil, or nothing
// heard yet, reads as no scopes.
type slackScopes struct {
	v atomic.Pointer[map[string]bool]
}

// Has reports whether the token was last reported to hold scope.
func (s *slackScopes) Has(scope string) bool {
	if s == nil {
		return false
	}
	m := s.v.Load()
	return m != nil && (*m)[scope]
}

// set records an X-OAuth-Scopes header value.
func (s *slackScopes) set(header string) {
	m := map[string]bool{}
	for _, sc := range strings.Split(header, ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			m[sc] = true
		}
	}
	s.v.Store(&m)
}

// recorder wraps next so every bot-token response updates s.
func (s *slackScopes) recorder(next slackHTTPClient) slackHTTPClient {
	return scopeRecorder{next: next, scopes: s}
}

type scopeRecorder struct {
	next   slackHTTPClient
	scopes *slackScopes
}

func (r scopeRecorder) Do(req *http.Request) (*http.Response, error) {
	resp, err := r.next.Do(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// The app-level token (Socket Mode's apps.connections.open) has scopes of
	// its own; a response with no header (a file upload URL) says nothing.
	if strings.HasPrefix(req.Header.Get("Authorization"), "Bearer xapp-") {
		return resp, err
	}
	if vs, ok := resp.Header[http.CanonicalHeaderKey("X-OAuth-Scopes")]; ok && len(vs) > 0 {
		r.scopes.set(vs[0])
	}
	return resp, err
}

// slackUsernameMax is the longest username Slack shows on a post.
const slackUsernameMax = 80

// commonsPersona is who a Commons message by author, shown as name, posts as
// in Slack: nil when the bot may not customize its posts, so the caller
// attributes the text instead.
func (b *Bot) commonsPersona(ctx context.Context, name, slackID string) *conversation.Persona {
	if b == nil || !b.slackScopes.Has(scopeChatWriteCustomize) {
		return nil
	}
	name = personaName(name)
	if name == "" {
		return nil
	}
	return &conversation.Persona{Name: name, IconURL: b.slackAvatar(ctx, slackID)}
}

// personaName is the username a post as name carries.
func personaName(name string) string {
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > slackUsernameMax {
		name = strings.TrimSpace(string(r[:slackUsernameMax]))
	}
	return name
}

// avatarTTL is how long a Slack profile image is reused, found or not.
const avatarTTL = time.Hour

// avatarCache maps a Slack user id to their profile image URL.
type avatarCache struct {
	mu sync.Mutex
	m  map[string]cachedAvatar
}

type cachedAvatar struct {
	url string
	at  time.Time
}

// slackAvatar returns the Slack profile image of slackID, or "" (Slack then
// shows the app's icon). Lookups, failed ones too, are cached for avatarTTL.
func (b *Bot) slackAvatar(ctx context.Context, slackID string) string {
	if slackID == "" || b.api == nil {
		return ""
	}
	c := &b.avatars
	c.mu.Lock()
	if e, ok := c.m[slackID]; ok && time.Since(e.at) < avatarTTL {
		c.mu.Unlock()
		return e.url
	}
	c.mu.Unlock()

	url := ""
	if u, err := b.api.GetUserInfoContext(ctx, slackID); err == nil && u != nil {
		url = firstNonEmpty(u.Profile.Image192, u.Profile.Image72)
		if !openableURL(url) {
			url = ""
		}
	} else if err != nil {
		b.logger.Debug("commons-mirror: avatar lookup failed", "slack_id", slackID, "error", err)
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]cachedAvatar{}
	}
	c.m[slackID] = cachedAvatar{url: url, at: time.Now()}
	c.mu.Unlock()
	return url
}
