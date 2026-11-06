package bridge

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// twoRoleMentionTriggerBeads returns two enabled spawn_agent triggers for the
// project: the first (default) plus a selectable alternate, so a mention-time
// --role hint has something valid to re-select. Mirrors twoRoleLister in
// jira_spawn_role_test.go but shaped for the mention daemon fixture.
func twoRoleMentionTriggerBeads(project string) []*beadsapi.BeadDetail {
	return []*beadsapi.BeadDetail{
		{Fields: map[string]string{
			"action": "spawn_agent", "project": project,
			"role": "gardenio-crew", "enabled": "true", // default (first enabled)
		}},
		{Fields: map[string]string{
			"action": "spawn_agent", "project": project,
			"role": "gardenio-reviewer", "enabled": "true", // selectable alternate
		}},
	}
}

// driveMentionRoleHint POSTs a comment_created mention through the real webhook
// handler and returns the single spawnCall the mention path produced.
func driveMentionRoleHint(t *testing.T, commentBody string) spawnCall {
	t.Helper()
	const (
		jiraKey = "PE-8100"
		taskID  = "gd-role-hint-task"
		agentID = "gd-role-hint-agent"
	)

	whDaemon := newMockJiraWebhookDaemon()
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     taskID,
		spawnID:      agentID,
		triggerBeads: twoRoleMentionTriggerBeads("gardenio"),
	}
	mentionHandler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         whDaemon,
		DevMode:        true, // skip HMAC for this wire test
		MentionHandler: mentionHandler,
		Logger:         slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})

	body := makeJiraWebhookPayload("comment_created", jiraKey, commentBody, "Jane Doe")
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook POST returned %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected exactly 1 agent spawned, got %d", len(mentionDaemon.spawned))
	}
	if len(mentionDaemon.created) != 1 {
		t.Fatalf("expected exactly 1 task bead created, got %d", len(mentionDaemon.created))
	}
	return mentionDaemon.spawned[0]
}

// TestJiraMentionRoleHint_HonorsConfiguredRole: "@gardenio --role gardenio-reviewer …"
// spawns the reviewer role (the selectable alternate) rather than the default,
// and the flag is stripped from the forwarded prompt.
func TestJiraMentionRoleHint_HonorsConfiguredRole(t *testing.T) {
	sc := driveMentionRoleHint(t, "@gardenio --role gardenio-reviewer please review this change")
	if sc.Role != "gardenio-reviewer" {
		t.Fatalf("spawn role = %q, want gardenio-reviewer (honored --role hint)", sc.Role)
	}
	if strings.Contains(sc.Prompt, "--role") || strings.Contains(sc.Prompt, "gardenio-reviewer") {
		t.Fatalf("prompt leaked the --role flag: %q", sc.Prompt)
	}
	if !strings.Contains(sc.Prompt, "please review this change") {
		t.Fatalf("prompt lost the instruction body: %q", sc.Prompt)
	}
}

// TestJiraMentionRoleHint_ColonSyntax: the role:<name> syntax works too.
func TestJiraMentionRoleHint_ColonSyntax(t *testing.T) {
	sc := driveMentionRoleHint(t, "@gardenio role:gardenio-reviewer take a look")
	if sc.Role != "gardenio-reviewer" {
		t.Fatalf("spawn role = %q, want gardenio-reviewer (role: syntax)", sc.Role)
	}
	if strings.Contains(sc.Prompt, "role:gardenio-reviewer") {
		t.Fatalf("prompt leaked the role: flag: %q", sc.Prompt)
	}
}

// TestJiraMentionRoleHint_UnconfiguredRoleFallsBackToDefault: a role the
// project is not configured to spawn is ignored (deny-by-default), falling back
// to the default role — but the flag is STILL stripped from the prompt so the
// agent never sees "--role root" as instruction text.
func TestJiraMentionRoleHint_UnconfiguredRoleFallsBackToDefault(t *testing.T) {
	sc := driveMentionRoleHint(t, "@gardenio --role root do something sketchy")
	if sc.Role != "gardenio-crew" {
		t.Fatalf("spawn role = %q, want gardenio-crew (fallback for unconfigured hint)", sc.Role)
	}
	if strings.Contains(sc.Prompt, "--role") || strings.Contains(sc.Prompt, "root") {
		t.Fatalf("prompt leaked the ignored --role flag: %q", sc.Prompt)
	}
	if !strings.Contains(sc.Prompt, "do something sketchy") {
		t.Fatalf("prompt lost the instruction body: %q", sc.Prompt)
	}
}

// TestJiraMentionRoleHint_NoHintUnchanged: an un-hinted mention spawns the
// default role, unchanged from prior behavior.
func TestJiraMentionRoleHint_NoHintUnchanged(t *testing.T) {
	sc := driveMentionRoleHint(t, "@gardenio please investigate this")
	if sc.Role != "gardenio-crew" {
		t.Fatalf("spawn role = %q, want gardenio-crew (default, no hint)", sc.Role)
	}
	if !strings.Contains(sc.Prompt, "please investigate this") {
		t.Fatalf("prompt lost the instruction body: %q", sc.Prompt)
	}
}

// TestJiraMentionRoleHint_ComposesWithFresh: "--fresh --role" front-loaded run
// is honored together — the --fresh strip leaves --role leading so parseRoleOverride
// still consumes it.
func TestJiraMentionRoleHint_ComposesWithFresh(t *testing.T) {
	sc := driveMentionRoleHint(t, "@gardenio --fresh --role gardenio-reviewer look again")
	if sc.Role != "gardenio-reviewer" {
		t.Fatalf("spawn role = %q, want gardenio-reviewer (--fresh --role composed)", sc.Role)
	}
	if strings.Contains(sc.Prompt, "--fresh") || strings.Contains(sc.Prompt, "--role") {
		t.Fatalf("prompt leaked a flag: %q", sc.Prompt)
	}
	if !strings.Contains(sc.Prompt, "look again") {
		t.Fatalf("prompt lost the instruction body: %q", sc.Prompt)
	}
}

// sanity: the parse helper is package-shared with Slack; guard the leading-token
// contract the spawn path relies on (a mid-prose --role is NOT consumed).
func TestJiraMentionRoleHint_MidProseFlagNotConsumed(t *testing.T) {
	role, remaining := parseRoleOverride("please use --role reviewer here")
	if role != "" {
		t.Fatalf("mid-prose --role should not be parsed as a hint, got role=%q", role)
	}
	if remaining != "please use --role reviewer here" {
		t.Fatalf("mid-prose text should be untouched, got %q", remaining)
	}
}

// TestJiraMention_QuietHintStampsFieldAndStrips drives a "@gardenio --quiet …"
// mention through the real webhook and asserts the mention-time --quiet intent
// hint (design-doc F-E, epic ka-0000007401) is: (1) stamped as jira_quiet=true on
// the spawned agent bead's fields so the SSE forwarders can read it, and (2)
// stripped from the forwarded prompt so it never leaks as instruction text.
func TestJiraMention_QuietHintStampsFieldAndStrips(t *testing.T) {
	sc := driveMentionRoleHint(t, "@gardenio --quiet just tell me the root cause")
	if sc.Extra["jira_quiet"] != "true" {
		t.Fatalf("agent bead jira_quiet = %q, want \"true\"", sc.Extra["jira_quiet"])
	}
	if strings.Contains(sc.Prompt, "--quiet") {
		t.Fatalf("prompt leaked the --quiet flag: %q", sc.Prompt)
	}
	if !strings.Contains(sc.Prompt, "just tell me the root cause") {
		t.Fatalf("prompt lost the instruction body: %q", sc.Prompt)
	}
}

// TestJiraMention_NoQuietHintLeavesFieldUnset: an un-hinted mention leaves
// jira_quiet unset, so the automatic progress notes post as before (the default).
func TestJiraMention_NoQuietHintLeavesFieldUnset(t *testing.T) {
	sc := driveMentionRoleHint(t, "@gardenio please investigate this")
	if _, ok := sc.Extra["jira_quiet"]; ok {
		t.Fatalf("jira_quiet should be unset on an un-hinted mention, got %q", sc.Extra["jira_quiet"])
	}
}
