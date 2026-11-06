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

// --- Test helpers ---

func newSlackReactionSource() *SlackReactionTriggerSource {
	return NewSlackReactionTriggerSource(slog.Default())
}

// slackReactionTriggerBead creates a trigger bead for slack-reaction tests.
//
// Defaults `last_checked` to 1 hour ago so the engine's runtime-bootstrap
// guard does not suppress first-evaluation firing in these tests.
func slackReactionTriggerBead(id, title, emoji, match, project, action, prompt string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"emoji":        emoji,
		"match":        match,
		"project":      project,
		"action":       action,
		"prompt":       prompt,
		"enabled":      "true",
		"last_checked": time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Title:  title,
		Status: "open",
		Fields: fields,
		Labels: []string{"trigger:slack-reaction"},
	}
}

// legacyReactionRuleBead creates a legacy reaction-rule bead.
func legacyReactionRuleBead(id, title, emoji, match, project, role, prompt string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"emoji":   emoji,
		"match":   match,
		"project": project,
		"role":    role,
		"prompt":  prompt,
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "reaction-rule",
		Title:  title,
		Status: "open",
		Fields: fields,
	}
}

// --- Name tests ---

func TestSlackReactionTriggerSource_Name(t *testing.T) {
	s := newSlackReactionSource()
	if got := s.Name(); got != "slack-reaction" {
		t.Errorf("Name() = %q, want %q", got, "slack-reaction")
	}
}

// --- LoadTriggers tests ---

func TestSlackReactionTriggerSource_LoadTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			slackReactionTriggerBead("t1", "Runner trigger", "runner", "e2e-test", "gardenio", "spawn_agent", "Run e2e tests"),
			slackReactionTriggerBead("t2", "No emoji", "", "", "gardenio", "spawn_agent", "Do something"),
			func() *beadsapi.BeadDetail {
				b := slackReactionTriggerBead("t3", "Disabled", "eyes", "", "gardenio", "spawn_agent", "Review")
				b.Fields["enabled"] = "false"
				return b
			}(),
		},
	}

	source := newSlackReactionSource()
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}

	// t1 should load, t2 skipped (no emoji), t3 skipped (disabled)
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	if triggers[0].ID != "t1" {
		t.Errorf("expected trigger t1, got %s", triggers[0].ID)
	}
	data, ok := triggers[0].SourceData.(slackReactionTriggerData)
	if !ok {
		t.Fatal("SourceData is not slackReactionTriggerData")
	}
	if data.Emoji != "runner" {
		t.Errorf("Emoji = %q, want %q", data.Emoji, "runner")
	}
	if data.Match != "e2e-test" {
		t.Errorf("Match = %q, want %q", data.Match, "e2e-test")
	}
}

func TestSlackReactionTriggerSource_LoadTriggers_WithChannelFilter(t *testing.T) {
	bead := slackReactionTriggerBead("t1", "Filtered", "rocket", "", "gardenio", "notify", "Launch")
	bead.Fields["channel_filter"] = "C12345"
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{bead}}

	source := newSlackReactionSource()
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	data := triggers[0].SourceData.(slackReactionTriggerData)
	if data.ChannelFilter != "C12345" {
		t.Errorf("ChannelFilter = %q, want %q", data.ChannelFilter, "C12345")
	}
}

func TestSlackReactionTriggerSource_LoadTriggers_LegacyReactionRules(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			legacyReactionRuleBead("r1", "E2E runner", "runner", "e2e-test", "monorepo", "crew", "Run e2e for MR {mr_iid}"),
			legacyReactionRuleBead("r2", "Review eyes", "eyes", "", "gardenio", "", "Review code"),
		},
	}

	source := newSlackReactionSource()
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}

	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers (legacy), got %d", len(triggers))
	}

	// Check first legacy trigger.
	if triggers[0].Source != "slack-reaction" {
		t.Errorf("Source = %q, want %q", triggers[0].Source, "slack-reaction")
	}
	if triggers[0].Action != TriggerActionSpawnAgent {
		t.Errorf("Action = %q, want %q", triggers[0].Action, TriggerActionSpawnAgent)
	}
	if triggers[0].Project != "monorepo" {
		t.Errorf("Project = %q, want %q", triggers[0].Project, "monorepo")
	}
	data0 := triggers[0].SourceData.(slackReactionTriggerData)
	if !data0.Legacy {
		t.Error("expected Legacy=true for converted reaction-rule")
	}
	if data0.Emoji != "runner" {
		t.Errorf("Emoji = %q, want %q", data0.Emoji, "runner")
	}

	// Second legacy trigger should default role to "crew".
	if triggers[1].Role != "crew" {
		t.Errorf("Role = %q, want %q", triggers[1].Role, "crew")
	}
}

func TestSlackReactionTriggerSource_LoadTriggers_Mixed(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			slackReactionTriggerBead("t1", "New trigger", "rocket", "", "gardenio", "notify", "Fire!"),
			legacyReactionRuleBead("r1", "Legacy runner", "runner", "", "monorepo", "crew", "Run tests"),
		},
	}

	source := newSlackReactionSource()
	triggers, err := source.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers error: %v", err)
	}
	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers (1 new + 1 legacy), got %d", len(triggers))
	}
}

// --- ConvertLegacyPromptTemplate tests ---

func TestConvertLegacyPromptTemplate(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			"Run tests for MR {mr_iid} on {source_branch}",
			"Run tests for MR {{mr_iid}} on {{source_branch}}",
		},
		{
			"User {user} reacted with {emoji}",
			"User {{user}} reacted with {{emoji}}",
		},
		{
			"No placeholders here",
			"No placeholders here",
		},
		{
			"",
			"",
		},
		{
			"Unknown {custom_var} stays unchanged",
			"Unknown {custom_var} stays unchanged",
		},
	}
	for _, tt := range tests {
		got := convertLegacyPromptTemplate(tt.input)
		if got != tt.want {
			t.Errorf("convertLegacyPromptTemplate(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- BufferEvent and PendingCount tests ---

func TestSlackReactionTriggerSource_BufferEvent(t *testing.T) {
	source := newSlackReactionSource()

	if source.PendingCount() != 0 {
		t.Fatalf("expected 0 pending events, got %d", source.PendingCount())
	}

	source.BufferEvent(slackReactionEvent{
		User:        "U123",
		Emoji:       "runner",
		Channel:     "C456",
		MessageTS:   "1234567890.123456",
		MessageText: "Some message",
	})

	if source.PendingCount() != 1 {
		t.Fatalf("expected 1 pending event, got %d", source.PendingCount())
	}

	source.DrainPending()
	if source.PendingCount() != 0 {
		t.Fatalf("expected 0 pending after drain, got %d", source.PendingCount())
	}
}

func TestSlackReactionTriggerSource_BufferEvent_Concurrent(t *testing.T) {
	source := newSlackReactionSource()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			source.BufferEvent(slackReactionEvent{
				User:      fmt.Sprintf("U%d", n),
				Emoji:     "runner",
				Channel:   "C1",
				MessageTS: fmt.Sprintf("ts%d", n),
			})
		}(i)
	}
	wg.Wait()

	if source.PendingCount() != 10 {
		t.Fatalf("expected 10 pending events, got %d", source.PendingCount())
	}
}

// TestSlackReactionTriggerSource_DrainFirst covers the bounded drain that the
// eval loop uses in place of DrainPending: it must remove exactly the first n
// (the evaluated prefix) and leave the tail intact for the next cycle.
func TestSlackReactionTriggerSource_DrainFirst(t *testing.T) {
	source := newSlackReactionSource()
	for i := 0; i < 5; i++ {
		source.BufferEvent(slackReactionEvent{Emoji: "runner", MessageTS: fmt.Sprintf("ts%d", i)})
	}

	// Drain the first 3 (as if a cycle had evaluated only those); 2 remain.
	source.DrainFirst(3)
	if got := source.PendingCount(); got != 2 {
		t.Fatalf("after DrainFirst(3) of 5, expected 2 pending, got %d", got)
	}
	// The survivors must be the untouched TAIL (ts3, ts4), in order.
	source.mu.Lock()
	surv := append([]slackReactionEvent(nil), source.pending...)
	source.mu.Unlock()
	if len(surv) != 2 || surv[0].MessageTS != "ts3" || surv[1].MessageTS != "ts4" {
		t.Fatalf("DrainFirst kept wrong tail: %+v", surv)
	}

	// n<=0 is a no-op; n>=len clears everything.
	source.DrainFirst(0)
	if got := source.PendingCount(); got != 2 {
		t.Fatalf("DrainFirst(0) must be a no-op, got %d pending", got)
	}
	source.DrainFirst(99)
	if got := source.PendingCount(); got != 0 {
		t.Fatalf("DrainFirst(>=len) must clear buffer, got %d pending", got)
	}
}

// TestSlackReactionTriggerSource_DrainFirst_PreservesMidCycleEvent is the
// regression for the drop bug (gd-Rxjj0JDHPW): a reaction that a concurrent
// BufferEvent appends AFTER a cycle snapshotted the buffer but BEFORE the drain
// must survive the drain and be evaluable next cycle. The old blind
// DrainPending() truncated the whole buffer and lost it.
//
// The eval loop's contract is: snapshot n = PendingCount(), Evaluate (which
// copies the first n under lock), then DrainFirst(n). We reproduce that
// ordering with an event injected between the snapshot and the drain.
func TestSlackReactionTriggerSource_DrainFirst_PreservesMidCycleEvent(t *testing.T) {
	source := newSlackReactionSource()

	// One event present when the cycle starts.
	source.BufferEvent(slackReactionEvent{Emoji: "runner", Channel: "C1", MessageTS: "cycle-start"})

	// Snapshot the count the way runReactionTriggerLoop does BEFORE evaluating.
	n := source.PendingCount()
	if n != 1 {
		t.Fatalf("setup: expected 1 pending, got %d", n)
	}

	// Evaluation copies the snapshot under lock (no drop yet).
	_, _ = source.Evaluate(context.Background(), TriggerDef{
		SourceData: slackReactionTriggerData{Emoji: "runner"},
	})

	// A reaction lands mid-cycle — appended after the snapshot, before drain.
	source.BufferEvent(slackReactionEvent{Emoji: "runner", Channel: "C1", MessageTS: "mid-cycle"})

	// Drain exactly what was evaluated. With blind DrainPending this dropped
	// BOTH events; DrainFirst(n) must keep the mid-cycle one.
	source.DrainFirst(n)

	if got := source.PendingCount(); got != 1 {
		t.Fatalf("mid-cycle event was dropped: expected 1 pending after DrainFirst(%d), got %d", n, got)
	}
	source.mu.Lock()
	surv := append([]slackReactionEvent(nil), source.pending...)
	source.mu.Unlock()
	if len(surv) != 1 || surv[0].MessageTS != "mid-cycle" {
		t.Fatalf("wrong survivor after DrainFirst: %+v (want the mid-cycle event)", surv)
	}

	// And it is evaluable next cycle (would spawn), proving it was not lost.
	matches, err := source.Evaluate(context.Background(), TriggerDef{
		SourceData: slackReactionTriggerData{Emoji: "runner"},
	})
	if err != nil {
		t.Fatalf("next-cycle Evaluate errored: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("mid-cycle event not matchable next cycle: got %d matches", len(matches))
	}
}

// --- Evaluate tests ---

func TestSlackReactionTriggerSource_Evaluate_EmojiMatch(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U123",
		Emoji:       "runner",
		Channel:     "C456",
		MessageTS:   "1234567890.123456",
		MessageText: "e2e test results",
	})

	trig := TriggerDef{
		ID:     "t1",
		Title:  "Runner trigger",
		Source: "slack-reaction",
		SourceData: slackReactionTriggerData{
			Emoji: "runner",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Key != "reaction:runner:C456:1234567890.123456" {
		t.Errorf("Key = %q, want %q", matches[0].Key, "reaction:runner:C456:1234567890.123456")
	}
}

func TestSlackReactionTriggerSource_Evaluate_EmojiNoMatch(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:  "U123",
		Emoji: "thumbsup",
	})

	trig := TriggerDef{
		ID:     "t1",
		Source: "slack-reaction",
		SourceData: slackReactionTriggerData{
			Emoji: "runner",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestSlackReactionTriggerSource_Evaluate_MessagePatternMatch(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U123",
		Emoji:       "runner",
		Channel:     "C456",
		MessageTS:   "ts1",
		MessageText: "E2E results: e2e-test-summary MR !789",
	})

	trig := TriggerDef{
		ID:     "t1",
		Source: "slack-reaction",
		SourceData: slackReactionTriggerData{
			Emoji: "runner",
			Match: "e2e-test-summary",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}

func TestSlackReactionTriggerSource_Evaluate_MessagePatternNoMatch(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U123",
		Emoji:       "runner",
		Channel:     "C456",
		MessageTS:   "ts1",
		MessageText: "random deployment message",
	})

	trig := TriggerDef{
		ID:     "t1",
		Source: "slack-reaction",
		SourceData: slackReactionTriggerData{
			Emoji: "runner",
			Match: "e2e-test-summary",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestSlackReactionTriggerSource_Evaluate_ChannelFilter(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:    "U123",
		Emoji:   "rocket",
		Channel: "C_WRONG",
	})
	source.BufferEvent(slackReactionEvent{
		User:    "U123",
		Emoji:   "rocket",
		Channel: "C_RIGHT",
	})

	trig := TriggerDef{
		ID:     "t1",
		Source: "slack-reaction",
		SourceData: slackReactionTriggerData{
			Emoji:         "rocket",
			ChannelFilter: "C_RIGHT",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (channel filtered), got %d", len(matches))
	}
}

func TestSlackReactionTriggerSource_Evaluate_BadSourceData(t *testing.T) {
	source := newSlackReactionSource()
	trig := TriggerDef{
		ID:         "t1",
		SourceData: "not the right type",
	}
	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches for bad source data, got %d", len(matches))
	}
}

func TestSlackReactionTriggerSource_Evaluate_MultipleEvents(t *testing.T) {
	source := newSlackReactionSource()
	for i := 0; i < 5; i++ {
		source.BufferEvent(slackReactionEvent{
			User:      "U123",
			Emoji:     "runner",
			Channel:   "C456",
			MessageTS: fmt.Sprintf("ts%d", i),
		})
	}
	source.BufferEvent(slackReactionEvent{
		User:      "U123",
		Emoji:     "eyes",
		Channel:   "C456",
		MessageTS: "ts-eyes",
	})

	trig := TriggerDef{
		ID:     "t1",
		Source: "slack-reaction",
		SourceData: slackReactionTriggerData{
			Emoji: "runner",
		},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 5 {
		t.Fatalf("expected 5 runner matches, got %d", len(matches))
	}
}

func TestSlackReactionTriggerSource_Evaluate_EventsNotCleared(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:      "U123",
		Emoji:     "runner",
		Channel:   "C456",
		MessageTS: "ts1",
	})

	trig := TriggerDef{
		ID:         "t1",
		Source:     "slack-reaction",
		SourceData: slackReactionTriggerData{Emoji: "runner"},
	}

	// First eval
	matches1, _ := source.Evaluate(context.Background(), trig)
	if len(matches1) != 1 {
		t.Fatalf("first eval: expected 1, got %d", len(matches1))
	}

	// Second eval should still see the event (not drained)
	matches2, _ := source.Evaluate(context.Background(), trig)
	if len(matches2) != 1 {
		t.Fatalf("second eval: expected 1, got %d", len(matches2))
	}

	// After drain, no events
	source.DrainPending()
	matches3, _ := source.Evaluate(context.Background(), trig)
	if len(matches3) != 0 {
		t.Fatalf("after drain: expected 0, got %d", len(matches3))
	}
}

// --- Template vars tests ---

func TestSlackReactionTriggerSource_TemplateVars(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U_USER",
		Emoji:       "runner",
		Channel:     "C_CHAN",
		MessageTS:   "ts123",
		MessageText: "E2E results MR !456 branch: agent/fix/thing",
	})

	trig := TriggerDef{
		ID:         "t1",
		Source:     "slack-reaction",
		SourceData: slackReactionTriggerData{Emoji: "runner"},
	}

	matches, err := source.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	vars := matches[0].TemplateVars
	checks := map[string]string{
		"{{emoji}}":         "runner",
		"{{channel}}":       "C_CHAN",
		"{{message_ts}}":    "ts123",
		"{{thread_ts}}":     "ts123",
		"{{user}}":          "U_USER",
		"{{mr_iid}}":        "456",
		"{{source_branch}}": "agent/fix/thing",
	}
	for k, want := range checks {
		if got := vars[k]; got != want {
			t.Errorf("var %s = %q, want %q", k, got, want)
		}
	}
}

func TestSlackReactionTriggerSource_TemplateVars_NoMR(t *testing.T) {
	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U1",
		Emoji:       "runner",
		Channel:     "C1",
		MessageTS:   "ts1",
		MessageText: "no MR reference here",
	})

	trig := TriggerDef{
		ID:         "t1",
		SourceData: slackReactionTriggerData{Emoji: "runner"},
	}

	matches, _ := source.Evaluate(context.Background(), trig)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if _, ok := matches[0].TemplateVars["{{mr_iid}}"]; ok {
		t.Error("expected no {{mr_iid}} var when no MR in message")
	}
}

// --- CatchUpKeys tests ---

func TestSlackReactionTriggerSource_CatchUpKeys(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "b1",
				Type:   "task",
				Status: "open",
				Fields: map[string]string{"reaction_key": "reaction:runner:C1:ts1"},
				Labels: []string{"trigger:t1"},
			},
			{
				ID:     "b2",
				Type:   "task",
				Status: "closed",
				Fields: map[string]string{"reaction_key": "reaction:runner:C1:ts2"},
				Labels: []string{"trigger:t1"},
			},
			{
				ID:     "b3",
				Type:   "task",
				Status: "open",
				Fields: map[string]string{}, // no reaction_key
				Labels: []string{"trigger:t1"},
			},
		},
	}

	source := newSlackReactionSource()
	keys, err := source.CatchUpKeys(context.Background(), daemon, "t1")
	if err != nil {
		t.Fatalf("CatchUpKeys error: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

// --- matchReactionEvent tests ---

func TestMatchReactionEvent(t *testing.T) {
	tests := []struct {
		name string
		data slackReactionTriggerData
		evt  slackReactionEvent
		want bool
	}{
		{
			name: "exact emoji match",
			data: slackReactionTriggerData{Emoji: "runner"},
			evt:  slackReactionEvent{Emoji: "runner"},
			want: true,
		},
		{
			name: "emoji mismatch",
			data: slackReactionTriggerData{Emoji: "runner"},
			evt:  slackReactionEvent{Emoji: "eyes"},
			want: false,
		},
		{
			name: "channel filter match",
			data: slackReactionTriggerData{Emoji: "rocket", ChannelFilter: "C1"},
			evt:  slackReactionEvent{Emoji: "rocket", Channel: "C1"},
			want: true,
		},
		{
			name: "channel filter mismatch",
			data: slackReactionTriggerData{Emoji: "rocket", ChannelFilter: "C1"},
			evt:  slackReactionEvent{Emoji: "rocket", Channel: "C2"},
			want: false,
		},
		{
			name: "no channel filter",
			data: slackReactionTriggerData{Emoji: "rocket"},
			evt:  slackReactionEvent{Emoji: "rocket", Channel: "C_ANY"},
			want: true,
		},
		{
			name: "message pattern match",
			data: slackReactionTriggerData{Emoji: "runner", Match: "e2e-test"},
			evt:  slackReactionEvent{Emoji: "runner", MessageText: "results: e2e-test-summary"},
			want: true,
		},
		{
			name: "message pattern no match",
			data: slackReactionTriggerData{Emoji: "runner", Match: "e2e-test"},
			evt:  slackReactionEvent{Emoji: "runner", MessageText: "deployment complete"},
			want: false,
		},
		{
			name: "no pattern matches any text",
			data: slackReactionTriggerData{Emoji: "runner"},
			evt:  slackReactionEvent{Emoji: "runner", MessageText: "anything here"},
			want: true,
		},
		{
			name: "all filters combined",
			data: slackReactionTriggerData{Emoji: "runner", Match: "e2e", ChannelFilter: "C1"},
			evt:  slackReactionEvent{Emoji: "runner", Channel: "C1", MessageText: "e2e results"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchReactionEvent(tt.data, tt.evt)
			if got != tt.want {
				t.Errorf("matchReactionEvent() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Engine integration tests ---

func TestSlackReactionTriggerSource_EngineIntegration_Notify(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			func() *beadsapi.BeadDetail {
				b := slackReactionTriggerBead("t1", "React notify", "runner", "", "gardenio", "notify", "Reaction ={{emoji}}= in ={{channel}}=")
				return b
			}(),
		},
	}

	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:      "U1",
		Emoji:     "runner",
		Channel:   "C1",
		MessageTS: "ts1",
	})

	var notified bool
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) { notified = true }
	engine.RegisterSource(source)

	engine.Evaluate(context.Background())

	if !notified {
		t.Error("expected notification to fire")
	}
}

func TestSlackReactionTriggerSource_EngineIntegration_SpawnAgent(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			func() *beadsapi.BeadDetail {
				b := slackReactionTriggerBead("t1", "React spawn", "runner", "", "gardenio", "spawn_agent", "Work on {{mr_iid}}")
				b.Fields["role"] = "crew"
				return b
			}(),
		},
	}

	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U1",
		Emoji:       "runner",
		Channel:     "C1",
		MessageTS:   "ts1",
		MessageText: "MR !789 ready",
	})

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) {}
	engine.RegisterSource(source)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("expected 1 spawn, got %d", len(daemon.spawnedNames))
	}
	daemon.mu.Unlock()
}

// TestSlackReactionTriggerSource_SpawnedTask_DependsOnTriggerBead drives a
// reaction-event → engine → actionSpawnAgent path and asserts the spawned
// tracking task carries a `triggered-by` dep to the trigger definition bead.
// Auto-link M2a (ka-0000007272, epic ka-0000000933).
func TestSlackReactionTriggerSource_SpawnedTask_DependsOnTriggerBead(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			func() *beadsapi.BeadDetail {
				b := slackReactionTriggerBead("react-m2a", "React spawn auto-link", "runner", "", "gardenio", "spawn_agent", "Work on it")
				b.Fields["role"] = "crew"
				return b
			}(),
		},
	}

	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U1",
		Emoji:       "runner",
		Channel:     "C1",
		MessageTS:   "ts-m2a",
		MessageText: "Please run it",
	})

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) {}
	engine.RegisterSource(source)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(daemon.spawnedNames))
	}

	var sawTriggerDep bool
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			t.Errorf("unexpected dep type %q (dep=%+v)", d.DepType, d)
			continue
		}
		if d.DependsOnID == "react-m2a" {
			sawTriggerDep = true
		}
	}
	if !sawTriggerDep {
		t.Errorf("missing triggered-by dep to trigger bead react-m2a (deps=%+v)", daemon.addedDeps)
	}
}

func TestSlackReactionTriggerSource_EngineIntegration_Dedup(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			slackReactionTriggerBead("t1", "React dedup", "runner", "", "gardenio", "notify", "Fired"),
		},
	}

	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:      "U1",
		Emoji:     "runner",
		Channel:   "C1",
		MessageTS: "ts1",
	})

	notifyCount := 0
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) { notifyCount++ }
	engine.RegisterSource(source)

	// First eval fires.
	engine.Evaluate(context.Background())
	if notifyCount != 1 {
		t.Fatalf("first eval: expected 1 notify, got %d", notifyCount)
	}

	// Second eval with same events — deduped by engine.
	engine.Evaluate(context.Background())
	if notifyCount != 1 {
		t.Fatalf("second eval: expected still 1 notify (deduped), got %d", notifyCount)
	}

	// Verify the seen key.
	seen := engine.SeenKeys("t1")
	expectedKey := "reaction:runner:C1:ts1"
	if !seen[expectedKey] {
		t.Errorf("expected key %q in seen set", expectedKey)
	}
}

func TestSlackReactionTriggerSource_EngineIntegration_CatchUp(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			slackReactionTriggerBead("t1", "React catchup", "runner", "", "gardenio", "notify", "Fire"),
			// Existing bead created by this trigger.
			{
				ID:     "existing1",
				Type:   "task",
				Status: "open",
				Fields: map[string]string{"reaction_key": "reaction:runner:C1:ts_old"},
				Labels: []string{"trigger:t1"},
			},
		},
	}

	source := newSlackReactionSource()
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.RegisterSource(source)

	engine.CatchUp(context.Background())

	seen := engine.SeenKeys("t1")
	if !seen["reaction:runner:C1:ts_old"] {
		t.Error("expected catch-up key in seen set")
	}
}

func TestSlackReactionTriggerSource_EngineIntegration_LegacyMigration(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			legacyReactionRuleBead("r1", "Legacy runner", "runner", "", "monorepo", "crew", "Run tests for {mr_iid}"),
		},
	}

	source := newSlackReactionSource()
	source.BufferEvent(slackReactionEvent{
		User:        "U1",
		Emoji:       "runner",
		Channel:     "C1",
		MessageTS:   "ts1",
		MessageText: "MR !123 branch: fix/stuff",
	})

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	engine.notifier = func(_ context.Context, _, _ string, _ string) {}
	engine.RegisterSource(source)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawned := len(daemon.spawnedNames)
	daemon.mu.Unlock()

	if spawned != 1 {
		t.Errorf("expected 1 agent spawned from legacy rule, got %d", spawned)
	}
}
