package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// defaultUserCacheTTL is how long a users.list result is served from cache.
// users.list is a Slack Tier-2 method, so alias lookups must not page the whole
// workspace per call (same reasoning as defaultChannelCacheTTL).
const defaultUserCacheTTL = 10 * time.Minute

var (
	slackUserIDRe      = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)
	slackUserMentionRe = regexp.MustCompile(`^<@([UW][A-Z0-9]+)(?:\|[^>]*)?>$`)
)

// slackUsersClient is the subset of *slack.Client the user lookup needs.
type slackUsersClient interface {
	GetUsers(options ...slack.GetUsersOption) ([]slack.User, error)
	GetUserInfo(user string) (*slack.User, error)
	GetUserByEmail(email string) (*slack.User, error)
}

// SlackUserInfo is a workspace member as returned by /api/slack/users/lookup.
// Email needs the users:read.email scope and is empty when Slack withholds it.
type SlackUserInfo struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"` // the @alias (Slack's legacy "name")
	DisplayName string `json:"display_name,omitempty"`
	RealName    string `json:"real_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Title       string `json:"title,omitempty"`
	IsBot       bool   `json:"is_bot"`
	Deleted     bool   `json:"deleted,omitempty"`
}

// SlackUserAPI serves GET /api/slack/users/lookup so agents can turn a Slack
// alias, user ID, mention or email into a person's name and email without
// holding a Slack token or needing a seeded user bead.
type SlackUserAPI struct {
	api    slackUsersClient
	logger *slog.Logger

	cacheMu      sync.Mutex
	cachedUsers  []slack.User
	cacheExpiry  time.Time
	cacheStamped bool
	cacheTTL     time.Duration // configurable for tests
	now          func() time.Time
}

// NewSlackUserAPI creates a new Slack user lookup handler.
func NewSlackUserAPI(api slackUsersClient, logger *slog.Logger) *SlackUserAPI {
	return &SlackUserAPI{
		api:      api,
		logger:   logger,
		cacheTTL: defaultUserCacheTTL,
		now:      time.Now,
	}
}

// RegisterRoutes registers the user lookup route on the given mux.
func (a *SlackUserAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/users/lookup", a.handleLookup)
}

// handleLookup handles GET /api/slack/users/lookup?q=<alias|id|mention|email>.
// Returns a JSON array of matches (empty when none); an ambiguous alias yields
// several entries so the caller can disambiguate rather than guess.
func (a *SlackUserAPI) handleLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, `{"error":"q is required"}`, http.StatusBadRequest)
		return
	}

	matches, err := a.lookup(q)
	if err != nil {
		var rl *slack.RateLimitedError
		if errors.As(err, &rl) {
			secs := max(int(rl.RetryAfter.Round(time.Second)/time.Second), 1)
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			http.Error(w,
				fmt.Sprintf(`{"error":"slack rate limited, retry in %ds","retry_after_s":%d}`, secs, secs),
				http.StatusTooManyRequests)
			return
		}
		a.logger.Error("slack user lookup failed", "error", err)
		http.Error(w, `{"error":"user lookup failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(matches)
}

// lookup resolves q by shape: a user ID or <@U123|x> mention goes to users.info,
// an email to users.lookupByEmail, anything else (an @alias or a name) is matched
// against the cached users.list.
func (a *SlackUserAPI) lookup(q string) ([]SlackUserInfo, error) {
	if m := slackUserMentionRe.FindStringSubmatch(q); m != nil {
		q = m[1]
	}
	q = strings.TrimPrefix(q, "@")

	switch {
	case slackUserIDRe.MatchString(q):
		u, err := a.api.GetUserInfo(q)
		if err != nil {
			if isSlackNotFound(err) {
				return []SlackUserInfo{}, nil
			}
			return nil, err
		}
		return []SlackUserInfo{toSlackUserInfo(*u)}, nil
	case strings.Contains(q, "@"):
		u, err := a.api.GetUserByEmail(q)
		if err != nil {
			if isSlackNotFound(err) {
				return []SlackUserInfo{}, nil
			}
			return nil, err
		}
		return []SlackUserInfo{toSlackUserInfo(*u)}, nil
	}

	users, err := a.listUsersCached()
	if err != nil {
		return nil, err
	}
	return matchSlackUsers(users, q), nil
}

// isSlackNotFound reports a miss: users.info answers "user_not_found",
// users.lookupByEmail answers "users_not_found".
func isSlackNotFound(err error) bool {
	s := err.Error()
	return strings.Contains(s, "user_not_found") || strings.Contains(s, "users_not_found")
}

// listUsersCached serves users.list from cache while fresh, and falls back to a
// stale copy when a refresh fails (a transient 429 should not blank lookups).
func (a *SlackUserAPI) listUsersCached() ([]slack.User, error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	if a.cacheStamped && a.now().Before(a.cacheExpiry) {
		return a.cachedUsers, nil
	}
	users, err := a.api.GetUsers()
	if err != nil {
		if a.cacheStamped {
			a.logger.Warn("serving stale user cache after refresh error", "error", err)
			return a.cachedUsers, nil
		}
		return nil, err
	}
	a.cachedUsers = users
	a.cacheExpiry = a.now().Add(a.cacheTTL)
	a.cacheStamped = true
	return users, nil
}

// matchSlackUsers finds members for a name-ish query. Tiers, first non-empty
// wins: exact @handle, exact display/real name, then unique-prefix of either.
// Deactivated accounts are skipped so a stale alias never beats a live one.
func matchSlackUsers(users []slack.User, q string) []SlackUserInfo {
	needle := strings.ToLower(strings.TrimSpace(q))
	tiers := []func(u slack.User) bool{
		func(u slack.User) bool { return strings.EqualFold(u.Name, needle) },
		func(u slack.User) bool {
			return strings.EqualFold(u.Profile.DisplayName, needle) ||
				strings.EqualFold(u.RealName, needle) ||
				strings.EqualFold(u.Profile.RealName, needle)
		},
		func(u slack.User) bool {
			return strings.HasPrefix(strings.ToLower(u.Name), needle) ||
				strings.HasPrefix(strings.ToLower(u.Profile.DisplayName), needle) ||
				strings.HasPrefix(strings.ToLower(u.RealName), needle)
		},
	}
	for _, match := range tiers {
		var out []SlackUserInfo
		for _, u := range users {
			if u.Deleted || !match(u) {
				continue
			}
			out = append(out, toSlackUserInfo(u))
		}
		if len(out) > 0 {
			sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
			return out
		}
	}
	return []SlackUserInfo{}
}

func toSlackUserInfo(u slack.User) SlackUserInfo {
	real := u.RealName
	if real == "" {
		real = u.Profile.RealName
	}
	return SlackUserInfo{
		ID:          u.ID,
		Handle:      u.Name,
		DisplayName: u.Profile.DisplayName,
		RealName:    real,
		Email:       u.Profile.Email,
		Title:       u.Profile.Title,
		IsBot:       u.IsBot,
		Deleted:     u.Deleted,
	}
}
