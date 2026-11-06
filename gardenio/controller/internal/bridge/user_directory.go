package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// userDirectoryRefreshInterval controls how often the in-memory user directory
// is refreshed from the beads daemon. User beads change infrequently.
const userDirectoryRefreshInterval = 5 * time.Minute

// UserIdentity is a resolved cross-platform identity for a single person.
type UserIdentity struct {
	BeadID         string
	SlackID        string
	SlackChannelID string
	GitLabUsername string
	GitHubUsername string
	GitLabUserID   string // forge numeric id; once pinned it outranks the username (lookupGitLabPerson)
	GitHubUserID   string // forge numeric id (github_user_id)
	Email          string
	DisplayName    string
	Pronouns       string // e.g. "he/him", "she/her", "they/them"
	Team           string
	JiraUser       string
	Capabilities   []string // e.g. ["approve:escalation", "aws:admin"]
	AuthMethods    []string // e.g. ["slack_oauth", "github_oauth"]
	// VibeMode is the user's `preferences.vibe_mode` opt-in (gb-ux epic
	// ka-0000005059): true ⇒ full-verbosity Slack, absent/false ⇒ the simple
	// default. Parsed once here so the notification hot path reads it from the
	// cache instead of re-decoding the preferences blob per send.
	VibeMode bool
	// DMProject is the user's `preferences.dm_project`: the project their DMs
	// with the bot spawn into, outranking DEFAULT_DM_PROJECT / dm_default.
	// Empty when unset. See dm_project.go.
	DMProject string
}

// HasCapability reports whether the user has the given capability string.
// Supports wildcard prefixes: "approve:*" matches "approve:escalation".
func (u *UserIdentity) HasCapability(cap string) bool {
	for _, c := range u.Capabilities {
		if c == cap {
			return true
		}
		// Wildcard: "approve:*" matches any "approve:<x>".
		if strings.HasSuffix(c, ":*") {
			prefix := strings.TrimSuffix(c, "*")
			if strings.HasPrefix(cap, prefix) {
				return true
			}
		}
	}
	return false
}

// SlackEmailResolver resolves a Slack user ID to their profile email address.
// The profile email is only ever a hint (SlackEmailHint, the link prompt's
// soft warning): Slack admins, guests, Slack Connect and SSO attribute mapping
// can all set it, so it never grants capabilities or decides who acts.
type SlackEmailResolver interface {
	GetUserEmail(ctx context.Context, slackUserID string) (string, error)
}

// UserDirectory provides cached, cross-platform identity lookups backed by
// type=user beads in the daemon. Call Refresh to prime the cache; lookups
// auto-refresh when the cache is stale.
type UserDirectory struct {
	daemon        BeadClient
	slackResolver SlackEmailResolver // optional, enables SlackEmailHint
	logger        *slog.Logger

	mu             sync.RWMutex
	users          []UserIdentity
	byBeadID       map[string]*UserIdentity
	bySlackID      map[string]*UserIdentity
	bySlackChannel map[string]*UserIdentity
	byGitLab       map[string]*UserIdentity // lowercased gitlab_username
	byGitHub       map[string]*UserIdentity // lowercased github_username
	byGitLabID     map[string]*UserIdentity
	byGitHubID     map[string]*UserIdentity
	byEmail        map[string]*UserIdentity
	byJiraUser     map[string]*UserIdentity
	refreshedAt    time.Time
	slackNegCache  map[string]time.Time // Slack IDs whose profile email matched no user bead
}

// NewUserDirectory creates a UserDirectory backed by the given BeadClient.
func NewUserDirectory(daemon BeadClient, logger *slog.Logger) *UserDirectory {
	return &UserDirectory{
		daemon:         daemon,
		logger:         logger,
		byBeadID:       make(map[string]*UserIdentity),
		bySlackID:      make(map[string]*UserIdentity),
		bySlackChannel: make(map[string]*UserIdentity),
		byGitLab:       make(map[string]*UserIdentity),
		byGitHub:       make(map[string]*UserIdentity),
		byEmail:        make(map[string]*UserIdentity),
		byJiraUser:     make(map[string]*UserIdentity),
		slackNegCache:  make(map[string]time.Time),
	}
}

// SetSlackEmailResolver configures an optional Slack API resolver behind
// SlackEmailHint, which tells an unlinked Slack user that their profile email
// matches a user bead so the denial can point them at linking.
func (d *UserDirectory) SetSlackEmailResolver(r SlackEmailResolver) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.slackResolver = r
}

// Refresh loads (or reloads) user beads from the daemon into the in-memory
// indexes. Safe to call concurrently.
func (d *UserDirectory) Refresh(ctx context.Context) error {
	result, err := d.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		return err
	}

	users := make([]UserIdentity, 0, len(result.Beads))
	for _, b := range result.Beads {
		users = append(users, buildUserIdentity(b.ID, b.Title, b.Fields))
	}

	d.mu.Lock()
	d.users = users
	d.reindexLocked()
	d.slackNegCache = make(map[string]time.Time) // clear on refresh — new beads may exist
	d.refreshedAt = time.Now()
	d.mu.Unlock()

	d.logger.Info("user directory refreshed", "count", len(users))
	return nil
}

// buildUserIdentity constructs a UserIdentity from a bead's title and fields.
// Shared between Refresh (daemon list) and upsert (SSE event).
func buildUserIdentity(beadID, title string, fields map[string]string) UserIdentity {
	u := UserIdentity{
		BeadID:         beadID,
		SlackID:        fields["slack_id"],
		SlackChannelID: fields["slack_channel_id"],
		GitLabUsername: fields["gitlab_username"],
		GitHubUsername: fields["github_username"],
		GitLabUserID:   strings.TrimSpace(fields["gitlab_user_id"]),
		GitHubUserID:   strings.TrimSpace(fields["github_user_id"]),
		Email:          fields["email"],
		DisplayName:    fields["display_name"],
		Pronouns:       fields["pronouns"],
		Team:           fields["team"],
		JiraUser:       fields["jira_user"],
		Capabilities:   parseJSONStringSlice(fields["capabilities"]),
		AuthMethods:    parseJSONStringSlice(fields["auth_methods"]),
		VibeMode:       parseVibeModeFromPreferences(fields["preferences"]),
		DMProject:      parseDMProjectFromPreferences(fields["preferences"]),
	}
	if u.DisplayName == "" {
		u.DisplayName = title
	}
	return u
}

// reindexLocked rebuilds every lookup map from d.users. Must be called after
// any mutation to d.users since append/truncate can relocate the backing array
// and invalidate &d.users[i] pointers. Caller must hold d.mu.
func (d *UserDirectory) reindexLocked() {
	d.byBeadID = make(map[string]*UserIdentity, len(d.users))
	d.bySlackID = make(map[string]*UserIdentity, len(d.users))
	d.bySlackChannel = make(map[string]*UserIdentity, len(d.users))
	d.byGitLab = make(map[string]*UserIdentity, len(d.users))
	d.byGitHub = make(map[string]*UserIdentity, len(d.users))
	d.byGitLabID = make(map[string]*UserIdentity, len(d.users))
	d.byGitHubID = make(map[string]*UserIdentity, len(d.users))
	d.byEmail = make(map[string]*UserIdentity, len(d.users))
	d.byJiraUser = make(map[string]*UserIdentity, len(d.users))
	ambiguous := make(map[string]bool) // "<field>\x00<key>" seen on two beads
	for i := range d.users {
		ptr := &d.users[i]
		d.byBeadID[ptr.BeadID] = ptr
		if ptr.SlackID != "" {
			d.bySlackID[ptr.SlackID] = ptr
		}
		if ptr.SlackChannelID != "" {
			d.bySlackChannel[ptr.SlackChannelID] = ptr
		}
		// GitLab and GitHub logins are case-insensitive on the forge, and a
		// webhook may carry a different case than the bead (gd-yrnMQbmLgp), so
		// the keys are case-folded like email; the lookups fold to match.
		d.indexUniqueLocked(d.byGitLab, ambiguous, "gitlab_username", strings.ToLower(strings.TrimSpace(ptr.GitLabUsername)), ptr)
		d.indexUniqueLocked(d.byGitHub, ambiguous, "github_username", strings.ToLower(strings.TrimSpace(ptr.GitHubUsername)), ptr)
		d.indexUniqueLocked(d.byGitLabID, ambiguous, "gitlab_user_id", ptr.GitLabUserID, ptr)
		d.indexUniqueLocked(d.byGitHubID, ambiguous, "github_user_id", ptr.GitHubUserID, ptr)
		// Case-fold the key: an IdP may emit a mixed-case email while a
		// lookup key (OIDC subject, Slack profile) arrives lowercase, or
		// vice versa (ka-0000004473). LookupByEmail lowercases to match.
		d.indexUniqueLocked(d.byEmail, ambiguous, "email", strings.ToLower(ptr.Email), ptr)
		if ptr.JiraUser != "" {
			// Index jira_user case-insensitively: user beads store a
			// human-readable username (e.g. "Matthew.Baker") while the Jira
			// webhook author / a backfilled accountId may differ in case.
			// Lookup lowercases its key to match (ka-0000004473).
			d.byJiraUser[strings.ToLower(ptr.JiraUser)] = ptr
		}
	}
}

// indexUniqueLocked maps key to ptr in idx, unless another open user bead
// already carries the same key for this field. Then which person the key names
// would depend on daemon list order, and unioning their capabilities would let
// a second bead add grants to the first (gd-W4FkF80BlX), so the key resolves
// to nobody (fail closed, as CountBySlackID does for a shared slack_id) until
// an admin closes the duplicate. An empty key is not indexed. Caller must hold
// d.mu.
func (d *UserDirectory) indexUniqueLocked(idx map[string]*UserIdentity, ambiguous map[string]bool, field, key string, ptr *UserIdentity) {
	if key == "" {
		return
	}
	akey := field + "\x00" + key
	if prev, dup := idx[key]; dup || ambiguous[akey] {
		if !ambiguous[akey] {
			d.logger.Error("duplicate open user beads share an identity key; it resolves to nobody until one is closed",
				"field", field, "key", key, "bead", prev.BeadID, "other_bead", ptr.BeadID)
		}
		ambiguous[akey] = true
		delete(idx, key)
		return
	}
	idx[key] = ptr
}

// RegisterHandlers subscribes to bead lifecycle events so the directory stays
// in sync with newly-created, edited, and closed user beads without waiting
// for the next periodic Refresh. Without this, the approval flow for a
// just-created user bead fails because LookupByEmail can miss for up to
// userDirectoryRefreshInterval (5 minutes). See ka-0000001175.
func (d *UserDirectory) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", d.handleSSEUpsert)
	stream.On("beads.bead.updated", d.handleSSEUpsert)
	stream.On("beads.bead.closed", d.handleSSEClosed)
	d.logger.Info("user directory registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"})
}

func (d *UserDirectory) handleSSEUpsert(_ context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "user" {
		return
	}
	// Refresh filters to open/in_progress; apply the same policy here so a
	// user bead closed by an update event is dropped from the directory.
	if bead.Status != "" && bead.Status != "open" && bead.Status != "in_progress" {
		d.removeByBeadID(bead.ID)
		return
	}
	d.upsertFromEvent(bead)
}

func (d *UserDirectory) handleSSEClosed(_ context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "user" {
		return
	}
	d.removeByBeadID(bead.ID)
}

// upsertFromEvent adds or replaces a single user identity from an SSE event.
// Any existing entry for the same bead ID is removed first so a field change
// (e.g. email rename) does not leave stale map entries pointing at old values.
func (d *UserDirectory) upsertFromEvent(bead *BeadEvent) {
	u := buildUserIdentity(bead.ID, bead.Title, bead.Fields)

	d.mu.Lock()
	defer d.mu.Unlock()

	// Drop the old entry in-place, then append and reindex so pointers are valid.
	for i := range d.users {
		if d.users[i].BeadID == u.BeadID {
			d.users = append(d.users[:i], d.users[i+1:]...)
			break
		}
	}
	d.users = append(d.users, u)
	d.reindexLocked()

	// Clear the hint negative cache: the new or updated bead's email may now
	// match a Slack profile that missed before.
	d.slackNegCache = make(map[string]time.Time)

	d.logger.Debug("user directory upsert",
		"bead_id", u.BeadID, "email", u.Email, "slack_id", u.SlackID)
}

func (d *UserDirectory) removeByBeadID(beadID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	removed := false
	for i := range d.users {
		if d.users[i].BeadID == beadID {
			d.users = append(d.users[:i], d.users[i+1:]...)
			removed = true
			break
		}
	}
	if !removed {
		return
	}
	d.reindexLocked()
	d.logger.Debug("user directory removed", "bead_id", beadID)
}

// ensureFresh auto-refreshes the cache if it's older than the refresh interval.
func (d *UserDirectory) ensureFresh(ctx context.Context) {
	d.mu.RLock()
	stale := time.Since(d.refreshedAt) > userDirectoryRefreshInterval
	d.mu.RUnlock()

	if stale {
		if err := d.Refresh(ctx); err != nil {
			d.logger.Warn("user directory auto-refresh failed", "error", err)
		}
	}
}

// LookupBySlackID returns the identity for a Slack user ID, or nil if unknown.
func (d *UserDirectory) LookupBySlackID(ctx context.Context, slackID string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.bySlackID[slackID]
}

// CountBySlackID reports how many open user beads carry slackID. LookupBySlackID
// returns only the last of them in daemon list order, so a caller that acts as
// the person (gd-7CDM9gdog0) must treat a count above one as ambiguous.
func (d *UserDirectory) CountBySlackID(ctx context.Context, slackID string) int {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := 0
	for i := range d.users {
		if d.users[i].SlackID == slackID {
			n++
		}
	}
	return n
}

// LookupByGitLabUsername returns the identity for a GitLab username, matched
// case-insensitively, or nil. A username carried by more than one open user
// bead returns nil. The username is a soft key: to decide who acts, use
// lookupGitLabPerson, which honours a pinned gitlab_user_id.
func (d *UserDirectory) LookupByGitLabUsername(ctx context.Context, username string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.byGitLab[strings.ToLower(strings.TrimSpace(username))]
}

// LookupByGitLabUserID returns the identity whose gitlab_user_id is id, or nil.
func (d *UserDirectory) LookupByGitLabUserID(ctx context.Context, id string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.byGitLabID[strings.TrimSpace(id)]
}

// LookupByGitHubUsername returns the identity for a GitHub username, matched
// case-insensitively, or nil. A username carried by more than one open user
// bead returns nil.
func (d *UserDirectory) LookupByGitHubUsername(ctx context.Context, username string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.byGitHub[strings.ToLower(strings.TrimSpace(username))]
}

// LookupByGitHubUserID returns the identity whose github_user_id is id, or nil.
func (d *UserDirectory) LookupByGitHubUserID(ctx context.Context, id string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.byGitHubID[strings.TrimSpace(id)]
}

// LookupByEmail returns the identity for an email address, or nil. An email
// carried by more than one open user bead returns nil (see reindexLocked).
func (d *UserDirectory) LookupByEmail(ctx context.Context, email string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	// byEmail is keyed by lowercased email (see reindexLocked) so a mixed-case
	// IdP email and a lowercase subject resolve to the same bead (ka-0000004473).
	return d.byEmail[strings.ToLower(email)]
}

// LookupByJiraUser returns the identity for a JIRA account ID or username, or nil.
func (d *UserDirectory) LookupByJiraUser(ctx context.Context, jiraUser string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	// byJiraUser is keyed by lowercased jira_user (see reindexLocked) so a
	// "Matthew.Baker" bead resolves a "matthew.baker" webhook author and vice
	// versa (ka-0000004473).
	return d.byJiraUser[strings.ToLower(jiraUser)]
}

// LookupBySlackChannelID returns the identity for a Slack DM channel ID, or nil.
func (d *UserDirectory) LookupBySlackChannelID(ctx context.Context, channelID string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.bySlackChannel[channelID]
}

// All returns a snapshot of all loaded identities.
func (d *UserDirectory) All(ctx context.Context) []UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]UserIdentity, len(d.users))
	copy(out, d.users)
	return out
}

// Count returns the number of loaded identities.
func (d *UserDirectory) Count() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.users)
}

// LookupByBeadID returns the identity for a user bead ID, or nil if unknown.
func (d *UserDirectory) LookupByBeadID(ctx context.Context, beadID string) *UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.byBeadID[beadID]
}

// UsersWithCapability returns all users that have the given capability.
func (d *UserDirectory) UsersWithCapability(ctx context.Context, cap string) []*UserIdentity {
	d.ensureFresh(ctx)
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []*UserIdentity
	for i := range d.users {
		if d.users[i].HasCapability(cap) {
			out = append(out, &d.users[i])
		}
	}
	return out
}

// SlackEmailHint reports whether the Slack user's PROFILE email matches a user
// bead, for a Slack user whose id is on no user bead. It is a hint for the
// denial message only ("your email matches a user; link your account"): the
// profile email is whatever Slack admins, guest/Slack Connect setup or SSO
// attribute mapping made it, so it must never grant capabilities, pick the
// person a Slack user acts as, or attach a slack_id (gd-W4FkF80BlX). Only a
// pinned slack_id (admin-written, or the confirmed link, #402/#412) does that.
//
// Returns false when no resolver is configured, the Slack call fails, or no
// single user bead carries the email. It returns no name or bead: whoever set
// the profile email learns nothing about the person it matches. Misses are
// cached for the refresh interval to spare the Slack API.
func (d *UserDirectory) SlackEmailHint(ctx context.Context, slackID string) bool {
	d.ensureFresh(ctx)

	d.mu.RLock()
	resolver := d.slackResolver
	if t, ok := d.slackNegCache[slackID]; ok && time.Since(t) < userDirectoryRefreshInterval {
		d.mu.RUnlock()
		return false
	}
	d.mu.RUnlock()

	if resolver == nil {
		return false
	}

	email, err := resolver.GetUserEmail(ctx, slackID)
	if err != nil {
		d.logger.Warn("slack email lookup failed", "slack_id", slackID, "error", err)
	}
	matched := false
	if err == nil && email != "" {
		d.mu.RLock()
		matched = d.byEmail[strings.ToLower(email)] != nil
		d.mu.RUnlock()
	}
	if !matched {
		d.mu.Lock()
		d.slackNegCache[slackID] = time.Now()
		d.mu.Unlock()
	}
	return matched
}

// parseJSONStringSlice parses a JSON string array field value. Returns nil on
// empty input or parse failure.
func parseJSONStringSlice(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
