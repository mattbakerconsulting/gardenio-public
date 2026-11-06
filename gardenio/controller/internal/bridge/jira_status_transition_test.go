package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// statusEvent builds a jira:issue_updated event carrying a single status
// changelog item, editor identity, and the ticket key — the shape
// handleJiraStatusTransition reads.
func statusEvent(jiraKey, from, to, editorAccountID string) jiraWebhookEvent {
	return jiraWebhookEvent{
		WebhookEvent: "jira:issue_updated",
		Issue:        &jiraIssueRef{Key: jiraKey},
		Changelog: &jiraChangelog{Items: []jiraChangelogItem{
			{Field: "status", FieldID: "status", FromString: from, ToString: to},
		}},
		User: &jiraWebhookUser{AccountID: editorAccountID},
	}
}

// ── pure-helper unit tests ───────────────────────────────────────────────────

func TestJiraChangelog_StatusChange(t *testing.T) {
	t.Run("status item present", func(t *testing.T) {
		c := &jiraChangelog{Items: []jiraChangelogItem{
			{Field: "assignee", FieldID: "assignee", FromString: "a", ToString: "b"},
			{Field: "status", FieldID: "status", FromString: "In Progress", ToString: "Done"},
		}}
		from, to, ok := c.statusChange()
		if !ok || from != "In Progress" || to != "Done" {
			t.Fatalf("statusChange() = (%q,%q,%v), want (In Progress, Done, true)", from, to, ok)
		}
	})
	t.Run("server field-name only", func(t *testing.T) {
		// Jira Server carries the field NAME with no fieldId.
		c := &jiraChangelog{Items: []jiraChangelogItem{
			{Field: "status", FromString: "Open", ToString: "Cancelled"},
		}}
		from, to, ok := c.statusChange()
		if !ok || from != "Open" || to != "Cancelled" {
			t.Fatalf("statusChange() = (%q,%q,%v), want (Open, Cancelled, true)", from, to, ok)
		}
	})
	t.Run("no status item", func(t *testing.T) {
		c := &jiraChangelog{Items: []jiraChangelogItem{
			{Field: "description", FieldID: "description", FromString: "x", ToString: "y"},
		}}
		if _, _, ok := c.statusChange(); ok {
			t.Fatal("statusChange() ok = true for a description-only changelog, want false")
		}
	})
	t.Run("nil changelog", func(t *testing.T) {
		var c *jiraChangelog
		if _, _, ok := c.statusChange(); ok {
			t.Fatal("statusChange() ok = true for nil changelog, want false")
		}
	})
}

func TestNormalizeJiraStatus(t *testing.T) {
	cases := map[string]string{
		"Done":       "done",
		"  Won't Do": "won't do",
		"CLOSED":     "closed",
		"":           "",
		"   ":        "",
	}
	for in, want := range cases {
		if got := normalizeJiraStatus(in); got != want {
			t.Errorf("normalizeJiraStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNonActionableStatusSet(t *testing.T) {
	t.Run("default set", func(t *testing.T) {
		set := nonActionableStatusSet(nil)
		for _, s := range []string{"deprioritized", "cancelled", "canceled", "won't do", "done", "closed", "resolved"} {
			if _, ok := set[s]; !ok {
				t.Errorf("default set missing %q", s)
			}
		}
		// Refine is deliberately actionable in gardenio's own pickup JQL.
		if _, ok := set["refine"]; ok {
			t.Error("default set should NOT contain refine (see file header)")
		}
	})
	t.Run("override replaces default and normalizes", func(t *testing.T) {
		set := nonActionableStatusSet([]string{"Refine", "  Parked  ", ""})
		if _, ok := set["refine"]; !ok {
			t.Error("override set should contain refine")
		}
		if _, ok := set["parked"]; !ok {
			t.Error("override set should contain trimmed 'parked'")
		}
		// Override REPLACES the default — a default member is no longer present.
		if _, ok := set["done"]; ok {
			t.Error("override set should not carry default 'done'")
		}
		// A blank entry never becomes a match.
		if _, ok := set[""]; ok {
			t.Error("override set should not contain the empty string")
		}
	})
}

func TestJiraTaskBeadInFlight(t *testing.T) {
	cases := map[string]bool{
		"open":        true,
		"in_progress": true,
		"IN_PROGRESS": true,
		" open ":      true,
		"blocked":     false,
		"closed":      false,
		"done":        false,
		"":            false,
	}
	for in, want := range cases {
		if got := jiraTaskBeadInFlight(in); got != want {
			t.Errorf("jiraTaskBeadInFlight(%q) = %v, want %v", in, got, want)
		}
	}
}

// ── handler behavior tests ───────────────────────────────────────────────────

// metricCount reads the current value of the reconsider counter for one outcome
// label so a test can assert a per-call DELTA (the counter is a package global
// that accumulates across the whole test binary).
func metricCount(outcome string) float64 {
	return testutil.ToFloat64(jiraStatusReconsiderTotal.WithLabelValues(outcome))
}

func TestHandleJiraStatusTransition_UnattendedPauses(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-1"},
	}
	before := metricCount("paused")

	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	handleJiraStatusTransition(context.Background(), statusEvent("PE-1", "To Do", "Cancelled", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	bead := daemon.getBead("b1")
	if bead.Status != "blocked" {
		t.Errorf("unattended bead status = %q, want blocked", bead.Status)
	}
	if bead.Fields["jira_nonactionable"] != "true" {
		t.Errorf("jira_nonactionable = %q, want true", bead.Fields["jira_nonactionable"])
	}
	if bead.Fields["jira_nonactionable_status"] != "Cancelled" || bead.Fields["jira_status"] != "Cancelled" {
		t.Errorf("status fields not stamped: %+v", bead.Fields)
	}
	if got := metricCount("paused") - before; got != 1 {
		t.Errorf("paused metric delta = %v, want 1", got)
	}
	comments := daemon.getComments()
	if len(comments) != 1 || comments[0].BeadID != "b1" {
		t.Fatalf("expected 1 note on b1, got %+v", comments)
	}
	if !strings.Contains(comments[0].Text, "paused") {
		t.Errorf("unattended note should mention paused: %q", comments[0].Text)
	}
}

func TestHandleJiraStatusTransition_AttendedNudges(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:       "b1",
		Status:   "in_progress",
		Assignee: "worker-agent",
		Fields:   map[string]string{"jira_key": "PE-2"},
	}
	before := metricCount("nudged")

	var gotAgent, gotMsg string
	cfg := JiraWebhookConfig{
		Daemon: daemon,
		Logger: slog.Default(),
		Nudge: func(_ context.Context, agent, msg string) error {
			gotAgent, gotMsg = agent, msg
			return nil
		},
	}
	handleJiraStatusTransition(context.Background(), statusEvent("PE-2", "In Progress", "Deprioritized", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	bead := daemon.getBead("b1")
	// An attended bead is flagged but NOT force-paused — the agent owns the call.
	if bead.Status != "in_progress" {
		t.Errorf("attended bead status = %q, want in_progress (unchanged)", bead.Status)
	}
	if bead.Fields["jira_nonactionable"] != "true" {
		t.Errorf("attended bead not flagged: %+v", bead.Fields)
	}
	if gotAgent != "worker-agent" {
		t.Errorf("nudged agent = %q, want worker-agent", gotAgent)
	}
	if !strings.Contains(gotMsg, "PE-2") || !strings.Contains(gotMsg, "b1") {
		t.Errorf("nudge message missing key/bead: %q", gotMsg)
	}
	if got := metricCount("nudged") - before; got != 1 {
		t.Errorf("nudged metric delta = %v, want 1", got)
	}
}

func TestHandleJiraStatusTransition_AttendedNoNudgeFuncFlags(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:       "b1",
		Status:   "in_progress",
		Assignee: "worker-agent",
		Fields:   map[string]string{"jira_key": "PE-3"},
	}
	before := metricCount("flagged")

	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()} // no Nudge
	handleJiraStatusTransition(context.Background(), statusEvent("PE-3", "In Progress", "Done", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	if daemon.getBead("b1").Fields["jira_nonactionable"] != "true" {
		t.Error("bead should still be flagged with no nudge func")
	}
	if got := metricCount("flagged") - before; got != 1 {
		t.Errorf("flagged metric delta = %v, want 1", got)
	}
}

func TestHandleJiraStatusTransition_ActionableTargetNoOp(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-4"},
	}
	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	handleJiraStatusTransition(context.Background(), statusEvent("PE-4", "To Do", "In Progress", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	if daemon.getBead("b1").Status != "open" {
		t.Error("bead should be untouched on an actionable transition")
	}
	if _, ok := daemon.getBead("b1").Fields["jira_nonactionable"]; ok {
		t.Error("bead should not be flagged on an actionable transition")
	}
	if len(daemon.getComments()) != 0 {
		t.Error("no note should be posted on an actionable transition")
	}
}

func TestHandleJiraStatusTransition_RefineActionableByDefault(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-5"},
	}
	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	handleJiraStatusTransition(context.Background(), statusEvent("PE-5", "To Do", "Refine", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	if daemon.getBead("b1").Status != "open" {
		t.Error("Refine is actionable by default — bead must not be paused")
	}
}

func TestHandleJiraStatusTransition_RefineNonActionableWhenConfigured(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-6"},
	}
	cfg := JiraWebhookConfig{
		Daemon:                daemon,
		Logger:                slog.Default(),
		NonActionableStatuses: []string{"Refine"},
	}
	handleJiraStatusTransition(context.Background(), statusEvent("PE-6", "To Do", "Refine", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	if daemon.getBead("b1").Status != "blocked" {
		t.Error("Refine configured non-actionable — unattended bead should be paused")
	}
}

func TestHandleJiraStatusTransition_NonActionableToNonActionableNoOp(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-7"},
	}
	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	// Done → Closed: both already non-actionable, only the transition EDGE fires.
	handleJiraStatusTransition(context.Background(), statusEvent("PE-7", "Done", "Closed", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	if daemon.getBead("b1").Status != "open" {
		t.Error("a non-actionable → non-actionable move must not re-fire")
	}
	if len(daemon.getComments()) != 0 {
		t.Error("no note on a non-actionable → non-actionable move")
	}
}

func TestHandleJiraStatusTransition_BotEditorIgnored(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-8"},
	}
	t.Run("BotAccountID singular", func(t *testing.T) {
		cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default(), BotAccountID: "bot-acct"}
		handleJiraStatusTransition(context.Background(), statusEvent("PE-8", "In Progress", "Done", "bot-acct"), cfg, newNudgeThrottle(0), slog.Default())
		if daemon.getBead("b1").Status != "open" {
			t.Error("a bot-authored transition must be ignored")
		}
	})
	t.Run("BotAccountIDs set", func(t *testing.T) {
		cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default(), BotAccountIDs: []string{"bot-a", "bot-b"}}
		handleJiraStatusTransition(context.Background(), statusEvent("PE-8", "In Progress", "Done", "bot-b"), cfg, newNudgeThrottle(0), slog.Default())
		if daemon.getBead("b1").Status != "open" {
			t.Error("a transition by a member of the bot account set must be ignored")
		}
	})
}

func TestHandleJiraStatusTransition_ClosedBeadSkipped(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "closed",
		Fields: map[string]string{"jira_key": "PE-9"},
	}
	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	handleJiraStatusTransition(context.Background(), statusEvent("PE-9", "In Progress", "Done", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	if _, ok := daemon.getBead("b1").Fields["jira_nonactionable"]; ok {
		t.Error("a closed (finished) bead must not be reconsidered")
	}
	if len(daemon.getComments()) != 0 {
		t.Error("no note on a closed bead")
	}
}

func TestHandleJiraStatusTransition_DarkBeadSkipped(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Labels: []string{jiraDarkLabel},
		Fields: map[string]string{"jira_key": "PE-10"},
	}
	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	handleJiraStatusTransition(context.Background(), statusEvent("PE-10", "In Progress", "Cancelled", "human-acct"), cfg, newNudgeThrottle(0), slog.Default())

	if daemon.getBead("b1").Status != "open" {
		t.Error("a jira-dark bead must be skipped")
	}
	if _, ok := daemon.getBead("b1").Fields["jira_nonactionable"]; ok {
		t.Error("a jira-dark bead must not be flagged")
	}
}

func TestHandleJiraStatusTransition_DedupThrottle(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-11"},
	}
	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	throttle := newNudgeThrottle(jiraStatusReconsiderDedupWindow)
	ev := statusEvent("PE-11", "To Do", "Cancelled", "human-acct")

	handleJiraStatusTransition(context.Background(), ev, cfg, throttle, slog.Default())
	handleJiraStatusTransition(context.Background(), ev, cfg, throttle, slog.Default()) // redelivery

	if n := len(daemon.getComments()); n != 1 {
		t.Errorf("expected exactly 1 note across a duplicate delivery, got %d", n)
	}
}

func TestHandleJiraStatusTransition_NoStatusChangeNoOp(t *testing.T) {
	daemon := newMockJiraWebhookDaemon()
	daemon.beads["b1"] = &beadsapi.BeadDetail{
		ID:     "b1",
		Status: "open",
		Fields: map[string]string{"jira_key": "PE-12"},
	}
	cfg := JiraWebhookConfig{Daemon: daemon, Logger: slog.Default()}
	// A description-only edit: no status item in the changelog.
	ev := jiraWebhookEvent{
		WebhookEvent: "jira:issue_updated",
		Issue:        &jiraIssueRef{Key: "PE-12"},
		Changelog: &jiraChangelog{Items: []jiraChangelogItem{
			{Field: "description", FieldID: "description", FromString: "a", ToString: "b"},
		}},
		User: &jiraWebhookUser{AccountID: "human-acct"},
	}
	handleJiraStatusTransition(context.Background(), ev, cfg, newNudgeThrottle(0), slog.Default())

	if daemon.getBead("b1").Status != "open" || len(daemon.getComments()) != 0 {
		t.Error("a non-status edit must be a no-op")
	}
}
