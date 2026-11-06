package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mockSlackEmailResolver is a test double for SlackEmailResolver.
type mockSlackEmailResolver struct {
	mu     sync.Mutex
	emails map[string]string // slackID → email
	calls  int
	err    error
}

func (m *mockSlackEmailResolver) GetUserEmail(_ context.Context, slackUserID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return m.emails[slackUserID], nil
}

func (m *mockSlackEmailResolver) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// seedUser adds a user bead to the mock daemon.
func (m *mockDaemon) seedUser(title string, fields map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "user-" + title
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: fields,
	}
}

func TestUserDirectory_LookupBySlackID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"slack_id":        "U-ALICE",
		"gitlab_username": "alice-gl",
		"github_username": "alice-gh",
		"email":           "alice@example.com",
		"display_name":    "Alice Smith",
		"team":            "platform",
	})
	daemon.seedUser("Bob", map[string]string{
		"slack_id":        "U-BOB",
		"gitlab_username": "bob-gl",
		"display_name":    "Bob Jones",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	// First lookup triggers auto-refresh.
	u := dir.LookupBySlackID(ctx, "U-ALICE")
	if u == nil {
		t.Fatal("expected to find Alice by Slack ID")
	}
	if u.DisplayName != "Alice Smith" {
		t.Errorf("expected display_name 'Alice Smith', got %q", u.DisplayName)
	}
	if u.GitLabUsername != "alice-gl" {
		t.Errorf("expected gitlab_username 'alice-gl', got %q", u.GitLabUsername)
	}
	if u.GitHubUsername != "alice-gh" {
		t.Errorf("expected github_username 'alice-gh', got %q", u.GitHubUsername)
	}
	if u.Email != "alice@example.com" {
		t.Errorf("expected email 'alice@example.com', got %q", u.Email)
	}
	if u.Team != "platform" {
		t.Errorf("expected team 'platform', got %q", u.Team)
	}
}

func TestUserDirectory_LookupByGitLabUsername(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Carol", map[string]string{
		"slack_id":        "U-CAROL",
		"gitlab_username": "carol-gl",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupByGitLabUsername(ctx, "carol-gl")
	if u == nil {
		t.Fatal("expected to find Carol by GitLab username")
	}
	if u.SlackID != "U-CAROL" {
		t.Errorf("expected slack_id 'U-CAROL', got %q", u.SlackID)
	}
}

// GitLab and GitHub logins are case-insensitive on the forge, so a webhook
// author in a different case than the bead still names the person
// (gd-yrnMQbmLgp).
func TestUserDirectory_ForgeUsernamesCaseInsensitive(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Carol", map[string]string{
		"gitlab_username": "Carol-GL",
		"github_username": "Carol-GH",
	})
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	for _, name := range []string{"Carol-GL", "carol-gl", "CAROL-GL", " carol-gl "} {
		if u := dir.LookupByGitLabUsername(ctx, name); u == nil || u.BeadID != "user-Carol" {
			t.Errorf("LookupByGitLabUsername(%q) = %+v, want Carol", name, u)
		}
	}
	for _, name := range []string{"Carol-GH", "carol-gh", "CAROL-GH"} {
		if u := dir.LookupByGitHubUsername(ctx, name); u == nil || u.BeadID != "user-Carol" {
			t.Errorf("LookupByGitHubUsername(%q) = %+v, want Carol", name, u)
		}
	}
}

// Two open user beads whose forge logins differ only by case (or match
// exactly) would name one login as two people; the login resolves to nobody
// until an admin closes the duplicate, as a shared email does.
func TestUserDirectory_DuplicateForgeKeysResolveToNobody(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Carol", map[string]string{
		"gitlab_username": "carol",
		"github_username": "carol",
		"gitlab_user_id":  "42",
	})
	daemon.seedUser("Mallory", map[string]string{
		"gitlab_username": "Carol",
		"github_username": "CAROL",
		"gitlab_user_id":  "42",
	})
	daemon.seedUser("Erin", map[string]string{
		"gitlab_username": "erin",
	})
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	if u := dir.LookupByGitLabUsername(ctx, "carol"); u != nil {
		t.Errorf("shared gitlab_username resolved to %s, want nobody", u.BeadID)
	}
	if u := dir.LookupByGitHubUsername(ctx, "carol"); u != nil {
		t.Errorf("shared github_username resolved to %s, want nobody", u.BeadID)
	}
	if u := dir.LookupByGitLabUserID(ctx, "42"); u != nil {
		t.Errorf("shared gitlab_user_id resolved to %s, want nobody", u.BeadID)
	}
	if u := dir.LookupByGitLabUsername(ctx, "erin"); u == nil {
		t.Error("an unshared login must still resolve")
	}
}

// lookupGitLabPerson lets a pinned gitlab_user_id outrank the login, so a
// re-registered login cannot inherit the person's bead (gd-yrnMQbmLgp).
func TestLookupGitLabPerson_PinnedUserID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Carol", map[string]string{
		"gitlab_username": "carol",
		"gitlab_user_id":  "42",
	})
	daemon.seedUser("Legacy", map[string]string{
		"gitlab_username": "legacy",
	})
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	cases := []struct {
		name     string
		username string
		userID   int
		want     string
	}{
		{"pinned id and login match", "carol", 42, "user-Carol"},
		{"login case differs", "CAROL", 42, "user-Carol"},
		{"person renamed: id wins over a stale login", "carol-renamed", 42, "user-Carol"},
		{"login re-registered by someone else", "carol", 99, ""},
		{"pinned bead, event carries no id", "carol", 0, ""},
		{"unpinned bead matches by login", "legacy", 7, "user-Legacy"},
		{"unpinned bead, event carries no id", "Legacy", 0, "user-Legacy"},
		{"unknown login and id", "nobody", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if u := lookupGitLabPerson(ctx, dir, tc.username, tc.userID); u != nil {
				got = u.BeadID
			}
			if got != tc.want {
				t.Errorf("lookupGitLabPerson(%q, %d) = %q, want %q", tc.username, tc.userID, got, tc.want)
			}
		})
	}
	if lookupGitLabPerson(ctx, nil, "carol", 42) != nil {
		t.Error("nil directory must resolve to nobody")
	}
}

func TestUserDirectory_LookupByGitHubUsername(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Dave", map[string]string{
		"slack_id":        "U-DAVE",
		"github_username": "dave-gh",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupByGitHubUsername(ctx, "dave-gh")
	if u == nil {
		t.Fatal("expected to find Dave by GitHub username")
	}
	if u.SlackID != "U-DAVE" {
		t.Errorf("expected slack_id 'U-DAVE', got %q", u.SlackID)
	}
}

func TestUserDirectory_LookupByEmail(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Eve", map[string]string{
		"slack_id": "U-EVE",
		"email":    "eve@example.com",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupByEmail(ctx, "eve@example.com")
	if u == nil {
		t.Fatal("expected to find Eve by email")
	}
	if u.SlackID != "U-EVE" {
		t.Errorf("expected slack_id 'U-EVE', got %q", u.SlackID)
	}
}

func TestUserDirectory_LookupByJiraUser_CaseInsensitive(t *testing.T) {
	daemon := newMockDaemon()
	// User bead stores a mixed-case jira_user; the Jira webhook author (or a
	// backfilled accountId) may arrive in a different case (ka-0000004473).
	daemon.seedUser("Matt", map[string]string{
		"slack_id":  "U-MATT",
		"jira_user": "Matthew.Baker",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	for _, key := range []string{"Matthew.Baker", "matthew.baker", "MATTHEW.BAKER"} {
		u := dir.LookupByJiraUser(ctx, key)
		if u == nil {
			t.Fatalf("expected to find Matt by jira_user %q", key)
		}
		if u.SlackID != "U-MATT" {
			t.Errorf("jira_user %q: expected slack_id 'U-MATT', got %q", key, u.SlackID)
		}
	}

	if u := dir.LookupByJiraUser(ctx, "someone.else"); u != nil {
		t.Errorf("expected nil for unknown jira_user, got %+v", u)
	}
}

func TestUserDirectory_LookupNotFound(t *testing.T) {
	daemon := newMockDaemon()
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	if u := dir.LookupBySlackID(ctx, "U-NOBODY"); u != nil {
		t.Errorf("expected nil for unknown Slack ID, got %+v", u)
	}
	if u := dir.LookupByGitLabUsername(ctx, "nobody"); u != nil {
		t.Errorf("expected nil for unknown GitLab username, got %+v", u)
	}
	if u := dir.LookupByGitHubUsername(ctx, "nobody"); u != nil {
		t.Errorf("expected nil for unknown GitHub username, got %+v", u)
	}
	if u := dir.LookupByEmail(ctx, "nobody@example.com"); u != nil {
		t.Errorf("expected nil for unknown email, got %+v", u)
	}
}

func TestUserDirectory_DisplayNameFallsBackToTitle(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Frank Baker", map[string]string{
		"slack_id": "U-FRANK",
		// display_name intentionally omitted
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupBySlackID(ctx, "U-FRANK")
	if u == nil {
		t.Fatal("expected to find Frank by Slack ID")
	}
	if u.DisplayName != "Frank Baker" {
		t.Errorf("expected display_name to fall back to title 'Frank Baker', got %q", u.DisplayName)
	}
}

func TestUserDirectory_All(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{"slack_id": "U-ALICE"})
	daemon.seedUser("Bob", map[string]string{"slack_id": "U-BOB"})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	all := dir.All(ctx)
	if len(all) != 2 {
		t.Fatalf("expected 2 users, got %d", len(all))
	}
}

func TestUserDirectory_Count(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{"slack_id": "U-ALICE"})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	// Before refresh, count is 0.
	if c := dir.Count(); c != 0 {
		t.Errorf("expected count 0 before refresh, got %d", c)
	}

	// After refresh, count is 1.
	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if c := dir.Count(); c != 1 {
		t.Errorf("expected count 1 after refresh, got %d", c)
	}
}

func TestUserDirectory_LookupByJiraUser(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Grace", map[string]string{
		"slack_id":  "U-GRACE",
		"jira_user": "grace.hopper",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupByJiraUser(ctx, "grace.hopper")
	if u == nil {
		t.Fatal("expected to find Grace by JIRA user")
	}
	if u.SlackID != "U-GRACE" {
		t.Errorf("expected slack_id 'U-GRACE', got %q", u.SlackID)
	}
}

func TestUserDirectory_LookupBySlackChannelID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Hank", map[string]string{
		"slack_id":         "U-HANK",
		"slack_channel_id": "D-HANK-DM",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupBySlackChannelID(ctx, "D-HANK-DM")
	if u == nil {
		t.Fatal("expected to find Hank by Slack channel ID")
	}
	if u.SlackID != "U-HANK" {
		t.Errorf("expected slack_id 'U-HANK', got %q", u.SlackID)
	}
	if u.SlackChannelID != "D-HANK-DM" {
		t.Errorf("expected slack_channel_id 'D-HANK-DM', got %q", u.SlackChannelID)
	}
}

func TestUserDirectory_NewFieldsPopulated(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Ivy", map[string]string{
		"slack_id":         "U-IVY",
		"slack_channel_id": "D-IVY-DM",
		"jira_user":        "ivy.jira",
		"gitlab_username":  "ivy-gl",
		"github_username":  "ivy-gh",
		"email":            "ivy@example.com",
		"display_name":     "Ivy User",
		"team":             "infra",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupBySlackID(ctx, "U-IVY")
	if u == nil {
		t.Fatal("expected to find Ivy")
	}
	if u.SlackChannelID != "D-IVY-DM" {
		t.Errorf("expected slack_channel_id 'D-IVY-DM', got %q", u.SlackChannelID)
	}
	if u.JiraUser != "ivy.jira" {
		t.Errorf("expected jira_user 'ivy.jira', got %q", u.JiraUser)
	}
}

func TestUserIdentity_HasCapability(t *testing.T) {
	u := UserIdentity{
		Capabilities: []string{"approve:escalation", "aws:admin", "k8s:*"},
	}

	tests := []struct {
		cap  string
		want bool
	}{
		{"approve:escalation", true},
		{"aws:admin", true},
		{"k8s:rbac", true},   // wildcard match
		{"k8s:deploy", true}, // wildcard match
		{"approve:other", false},
		{"aws:readonly", false},
		{"unknown", false},
	}

	for _, tt := range tests {
		if got := u.HasCapability(tt.cap); got != tt.want {
			t.Errorf("HasCapability(%q) = %v, want %v", tt.cap, got, tt.want)
		}
	}
}

func TestUserIdentity_HasCapability_Empty(t *testing.T) {
	u := UserIdentity{}
	if u.HasCapability("anything") {
		t.Error("expected no capabilities on empty user")
	}
}

func TestUserDirectory_LookupByBeadID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"slack_id":     "U-ALICE",
		"display_name": "Alice Smith",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	u := dir.LookupByBeadID(ctx, "user-Alice")
	if u == nil {
		t.Fatal("expected to find Alice by bead ID")
	}
	if u.DisplayName != "Alice Smith" {
		t.Errorf("expected display_name 'Alice Smith', got %q", u.DisplayName)
	}

	if got := dir.LookupByBeadID(ctx, "user-NOBODY"); got != nil {
		t.Errorf("expected nil for unknown bead ID, got %+v", got)
	}
}

func TestUserDirectory_CapabilitiesParsed(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Admin", map[string]string{
		"slack_id":     "U-ADMIN",
		"capabilities": `["approve:escalation","aws:admin"]`,
		"auth_methods": `["slack_oauth","github_oauth"]`,
	})
	daemon.seedUser("Viewer", map[string]string{
		"slack_id": "U-VIEWER",
		// No capabilities or auth_methods
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	admin := dir.LookupBySlackID(ctx, "U-ADMIN")
	if admin == nil {
		t.Fatal("expected to find Admin")
	}
	if len(admin.Capabilities) != 2 {
		t.Fatalf("expected 2 capabilities, got %d: %v", len(admin.Capabilities), admin.Capabilities)
	}
	if !admin.HasCapability("approve:escalation") {
		t.Error("expected admin to have approve:escalation")
	}
	if !admin.HasCapability("aws:admin") {
		t.Error("expected admin to have aws:admin")
	}
	if len(admin.AuthMethods) != 2 {
		t.Fatalf("expected 2 auth methods, got %d: %v", len(admin.AuthMethods), admin.AuthMethods)
	}

	viewer := dir.LookupBySlackID(ctx, "U-VIEWER")
	if viewer == nil {
		t.Fatal("expected to find Viewer")
	}
	if len(viewer.Capabilities) != 0 {
		t.Errorf("expected 0 capabilities for viewer, got %d", len(viewer.Capabilities))
	}
}

func TestUserDirectory_UsersWithCapability(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Admin1", map[string]string{
		"slack_id":     "U-ADMIN1",
		"capabilities": `["approve:escalation","aws:admin"]`,
	})
	daemon.seedUser("Admin2", map[string]string{
		"slack_id":     "U-ADMIN2",
		"capabilities": `["approve:escalation"]`,
	})
	daemon.seedUser("Viewer", map[string]string{
		"slack_id":     "U-VIEWER",
		"capabilities": `["read:logs"]`,
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	approvers := dir.UsersWithCapability(ctx, "approve:escalation")
	if len(approvers) != 2 {
		t.Fatalf("expected 2 approvers, got %d", len(approvers))
	}

	awsAdmins := dir.UsersWithCapability(ctx, "aws:admin")
	if len(awsAdmins) != 1 {
		t.Fatalf("expected 1 aws:admin, got %d", len(awsAdmins))
	}

	none := dir.UsersWithCapability(ctx, "nonexistent")
	if len(none) != 0 {
		t.Fatalf("expected 0 for nonexistent cap, got %d", len(none))
	}
}

// TestUserDirectory_DuplicateEmailFailsClosed: two open user beads sharing one
// email must resolve to nobody (gd-W4FkF80BlX). Unioning their capabilities
// would let a second bead add grants to the first; picking one would depend
// on daemon list order. A third bead does not revive the email either.
func TestUserDirectory_DuplicateEmailFailsClosed(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Matt Baker", map[string]string{
		"email":        "matt@example.com",
		"slack_id":     "U-MATT",
		"capabilities": `["approve:escalation"]`,
	})
	daemon.seedUser("Matt", map[string]string{
		"email":        "Matt@Example.com",
		"capabilities": `["interact_decision"]`,
	})
	daemon.seedUser("Matt Again", map[string]string{
		"email": "matt@example.com",
	})
	daemon.seedUser("Other", map[string]string{
		"email": "other@example.com",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	if u := dir.LookupByEmail(ctx, "matt@example.com"); u != nil {
		t.Fatalf("shared email resolved to %s, want nobody", u.BeadID)
	}
	// No grant leaks across beads by the pinned slack_id either.
	u := dir.LookupBySlackID(ctx, "U-MATT")
	if u == nil {
		t.Fatal("expected the slack_id-pinned bead to still resolve by slack_id")
	}
	if u.HasCapability("interact_decision") {
		t.Error("capability from the duplicate-email bead was unioned into the pinned bead")
	}
	if !u.HasCapability("approve:escalation") {
		t.Error("the pinned bead lost its own capability")
	}
	if dir.LookupByEmail(ctx, "other@example.com") == nil {
		t.Error("an unshared email stopped resolving")
	}
}

// TestUserDirectory_LookupByEmailCaseInsensitive covers the latent case-folding
// bug: an IdP that emits a mixed-case email must resolve the same bead as a
// lowercase lookup key (ka-0000007675 / ka-0000004473).
func TestUserDirectory_LookupByEmailCaseInsensitive(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Mixed", map[string]string{
		"slack_id": "U-MIXED",
		"email":    "Mixed.Case@Example.com",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	if u := dir.LookupByEmail(ctx, "mixed.case@example.com"); u == nil {
		t.Error("expected lowercase lookup to resolve a mixed-case bead email")
	}
	if u := dir.LookupByEmail(ctx, "MIXED.CASE@EXAMPLE.COM"); u == nil {
		t.Error("expected uppercase lookup to resolve a mixed-case bead email")
	}
}

func TestParseJSONStringSlice(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{`["a","b","c"]`, 3},
		{`[]`, 0},
		{``, 0},
		{`not json`, 0},
		{`{"key":"value"}`, 0}, // object, not array of strings
	}

	for _, tt := range tests {
		got := parseJSONStringSlice(tt.input)
		if len(got) != tt.want {
			t.Errorf("parseJSONStringSlice(%q) len = %d, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestUserDirectory_RefreshUpdatesData(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"slack_id":     "U-ALICE",
		"display_name": "Alice V1",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	u := dir.LookupBySlackID(ctx, "U-ALICE")
	if u == nil || u.DisplayName != "Alice V1" {
		t.Fatalf("expected Alice V1, got %v", u)
	}

	// Update the bead in the mock daemon.
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == "user" && b.Fields["slack_id"] == "U-ALICE" {
			b.Fields["display_name"] = "Alice V2"
		}
	}
	daemon.mu.Unlock()

	// Force refresh.
	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	u = dir.LookupBySlackID(ctx, "U-ALICE")
	if u == nil || u.DisplayName != "Alice V2" {
		t.Fatalf("expected Alice V2 after refresh, got %v", u)
	}
}

// --- SlackEmailHint tests ---

// TestSlackEmailHint_NeverAttachesSlackID is the regression for gd-W4FkF80BlX:
// a Slack user whose PROFILE email equals a user bead's email must not be
// mapped to that bead. The hint reports the match, but the Slack id stays
// unknown to LookupBySlackID, the bead's SlackID stays empty, and no slack_id
// write is attempted.
func TestSlackEmailHint_NeverAttachesSlackID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"email":        "alice@example.com",
		"display_name": "Alice Smith",
		"capabilities": `["mention"]`,
	})

	resolver := &mockSlackEmailResolver{
		emails: map[string]string{"U-ATTACKER": "Alice@Example.com"},
	}

	dir := NewUserDirectory(daemon, slog.Default())
	dir.SetSlackEmailResolver(resolver)
	ctx := context.Background()

	if !dir.SlackEmailHint(ctx, "U-ATTACKER") {
		t.Fatal("expected the hint to report the profile-email match")
	}
	if u := dir.LookupBySlackID(ctx, "U-ATTACKER"); u != nil {
		t.Fatalf("profile email mapped the Slack id to %s", u.BeadID)
	}
	if n := dir.CountBySlackID(ctx, "U-ATTACKER"); n != 0 {
		t.Errorf("CountBySlackID = %d, want 0", n)
	}
	if u := dir.LookupByEmail(ctx, "alice@example.com"); u == nil || u.SlackID != "" {
		t.Errorf("user bead SlackID changed in memory: %+v", u)
	}

	time.Sleep(50 * time.Millisecond) // the old code backfilled in a goroutine
	daemon.mu.Lock()
	got := daemon.beads["user-Alice"].Fields["slack_id"]
	daemon.mu.Unlock()
	if got != "" {
		t.Errorf("slack_id written to the user bead: %q", got)
	}
}

func TestSlackEmailHint_SharedEmailIsNoMatch(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("A", map[string]string{"email": "dup@example.com"})
	daemon.seedUser("B", map[string]string{"email": "dup@example.com"})

	dir := NewUserDirectory(daemon, slog.Default())
	dir.SetSlackEmailResolver(&mockSlackEmailResolver{
		emails: map[string]string{"U-X": "dup@example.com"},
	})
	if dir.SlackEmailHint(context.Background(), "U-X") {
		t.Error("expected no hint for an email carried by two user beads")
	}
}

func TestSlackEmailHint_NegativeCache(t *testing.T) {
	daemon := newMockDaemon()
	// No user beads at all.

	resolver := &mockSlackEmailResolver{
		emails: map[string]string{"U-UNKNOWN": "unknown@example.com"},
	}

	dir := NewUserDirectory(daemon, slog.Default())
	dir.SetSlackEmailResolver(resolver)
	ctx := context.Background()

	if dir.SlackEmailHint(ctx, "U-UNKNOWN") {
		t.Fatal("expected no hint for unknown email")
	}
	if resolver.callCount() != 1 {
		t.Fatalf("expected 1 Slack API call, got %d", resolver.callCount())
	}

	// Second call — should hit negative cache, no additional API call.
	if dir.SlackEmailHint(ctx, "U-UNKNOWN") {
		t.Fatal("expected no hint on second call")
	}
	if resolver.callCount() != 1 {
		t.Errorf("expected no additional Slack API call (negative cache), got %d total", resolver.callCount())
	}
}

func TestSlackEmailHint_NegativeCacheClearedOnRefresh(t *testing.T) {
	daemon := newMockDaemon()

	resolver := &mockSlackEmailResolver{
		emails: map[string]string{"U-NEW": "new@example.com"},
	}

	dir := NewUserDirectory(daemon, slog.Default())
	dir.SetSlackEmailResolver(resolver)
	ctx := context.Background()

	if dir.SlackEmailHint(ctx, "U-NEW") {
		t.Fatal("expected no hint before bead exists")
	}

	daemon.seedUser("NewUser", map[string]string{
		"email":        "new@example.com",
		"display_name": "New User",
	})
	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if !dir.SlackEmailHint(ctx, "U-NEW") {
		t.Fatal("expected a hint after refresh cleared the negative cache")
	}
}

func TestSlackEmailHint_NoResolver(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"email": "alice@example.com",
	})

	dir := NewUserDirectory(daemon, slog.Default())
	if dir.SlackEmailHint(context.Background(), "U-ALICE") {
		t.Error("expected no hint when no SlackEmailResolver configured")
	}
}

func TestSlackEmailHint_SlackAPIError(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"email": "alice@example.com",
	})

	resolver := &mockSlackEmailResolver{
		err: fmt.Errorf("slack API unavailable"),
	}

	dir := NewUserDirectory(daemon, slog.Default())
	dir.SetSlackEmailResolver(resolver)
	ctx := context.Background()

	if dir.SlackEmailHint(ctx, "U-ALICE") {
		t.Error("expected no hint when Slack API errors")
	}
	if dir.SlackEmailHint(ctx, "U-ALICE") {
		t.Error("expected no hint on second call (negative cache)")
	}
	if resolver.callCount() != 1 {
		t.Errorf("expected 1 API call (cached after error), got %d", resolver.callCount())
	}
}

func TestSlackEmailHint_EmptyEmail(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"email": "alice@example.com",
	})

	// Slack returns empty email (users:read.email scope missing).
	dir := NewUserDirectory(daemon, slog.Default())
	dir.SetSlackEmailResolver(&mockSlackEmailResolver{
		emails: map[string]string{"U-ALICE": ""},
	})
	if dir.SlackEmailHint(context.Background(), "U-ALICE") {
		t.Error("expected no hint when Slack returns empty email")
	}
}
