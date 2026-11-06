package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func TestClassifySpawnVector(t *testing.T) {
	cases := []struct {
		name      string
		fields    map[string]string
		wantIcon  string
		wantLabel string // substring match
	}{
		{
			// A webhook source ("fsm") is unregistered, so its match key is
			// stamped under the generic source_key field — never an invalid
			// "fsm_key" that the type:agent schema 400s on (ka-0000001885).
			name:      "webhook trigger with match key",
			fields:    map[string]string{"trigger_id": "gd-T1", "source": "fsm", "source_key": "FSM-1234"},
			wantIcon:  spawnIconWebhook,
			wantLabel: "FSM-1234 fsm webhook",
		},
		{
			name:      "webhook trigger title fallback",
			fields:    map[string]string{"trigger_id": "gd-T1", "trigger_title": "Nightly importer"},
			wantIcon:  spawnIconWebhook,
			wantLabel: "Nightly importer webhook",
		},
		{
			name:      "slack thread reply from user",
			fields:    map[string]string{"spawn_source": "slack-thread", "slack_user_id": "U07ABC"},
			wantIcon:  spawnIconSlack,
			wantLabel: "Slack reply from <@U07ABC>",
		},
		{
			name:      "slack thread no user",
			fields:    map[string]string{"spawn_source": "slack-thread-pool"},
			wantIcon:  spawnIconSlack,
			wantLabel: "Slack thread",
		},
		{
			name:      "slack thread resume",
			fields:    map[string]string{"spawn_source": "slack-thread-resume", "slack_user_id": "U07ABC"},
			wantIcon:  spawnIconSlack,
			wantLabel: "resume",
		},
		{
			name:      "concierge is slack",
			fields:    map[string]string{"spawn_source": "concierge"},
			wantIcon:  spawnIconSlack,
			wantLabel: "Slack thread",
		},
		{
			name:      "cron scheduled task with cron expr",
			fields:    map[string]string{"spawn_source": "schedule", "schedule_title": "nightly triage", "schedule_cron": "0 9 * * *"},
			wantIcon:  spawnIconCron,
			wantLabel: "nightly triage (cron `0 9 * * *`)",
		},
		{
			name:      "cron scheduled task no cron expr",
			fields:    map[string]string{"spawn_source": "schedule", "schedule_title": "hourly sweep"},
			wantIcon:  spawnIconCron,
			wantLabel: "hourly sweep (cron)",
		},
		{
			name:      "gardener loop run via schedule + terminal goal",
			fields:    map[string]string{"spawn_source": "schedule", "schedule_title": "delivery gardener", "schedule_terminal_goal": "drain ready queue"},
			wantIcon:  spawnIconGardener,
			wantLabel: "gardener loop: delivery gardener",
		},
		{
			name:      "gardener loop run via schedule + max runs",
			fields:    map[string]string{"spawn_source": "schedule", "schedule_max_runs": "5", "schedule_id": "gd-CAP"},
			wantIcon:  spawnIconGardener,
			wantLabel: "gardener loop gd-CAP", // falls to schedule_id when no title
		},
		{
			name:      "crew spawned by gardener agent",
			fields:    map[string]string{"spawned_by": "gardenio-gardenio-gardener-delivery-drive-r-skhk"},
			wantIcon:  spawnIconGardener,
			wantLabel: "spawned by gardener gardenio-gardener-delivery-drive-r-skhk",
		},
		{
			name:      "jira mention with key",
			fields:    map[string]string{"spawn_source": "jira-mention", "jira_key": "PE-7083"},
			wantIcon:  spawnIconJira,
			wantLabel: "jira-mention from PE-7083",
		},
		{
			name:      "gitlab mr review with iid",
			fields:    map[string]string{"spawn_source": "gitlab-mr-review", "gitlab_mr_iid": "42"},
			wantIcon:  spawnIconGitlab,
			wantLabel: "gitlab MR review on !42",
		},
		{
			name:      "gitlab mention with author",
			fields:    map[string]string{"spawn_source": "gitlab-mention", "mention_author": "tim"},
			wantIcon:  spawnIconGitlab,
			wantLabel: "gitlab-mention from tim",
		},
		{
			name:      "manual gb spawn",
			fields:    map[string]string{"spawn_source": "manual"},
			wantIcon:  spawnIconManual,
			wantLabel: "manual gb spawn",
		},
		{
			name:      "pool assign cli is manual",
			fields:    map[string]string{"spawn_source": "pool-assign-cli"},
			wantIcon:  spawnIconManual,
			wantLabel: "manual gb spawn",
		},
		{
			name:      "unrecognized source falls back to neutral icon + raw value",
			fields:    map[string]string{"spawn_source": "some-future-source"},
			wantIcon:  spawnIconFallback,
			wantLabel: "some-future-source",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			icon, label := classifySpawnVector(tc.fields)
			if icon != tc.wantIcon {
				t.Errorf("icon = %q, want %q", icon, tc.wantIcon)
			}
			if !strings.Contains(label, tc.wantLabel) {
				t.Errorf("label = %q, want it to contain %q", label, tc.wantLabel)
			}
		})
	}
}

func TestClassifySpawnVector_NoContext(t *testing.T) {
	for _, fields := range []map[string]string{
		nil,
		{},
		{"role": "crew", "project": "gardenio"}, // no spawn context at all
	} {
		icon, label := classifySpawnVector(fields)
		if icon != "" || label != "" {
			t.Errorf("classifySpawnVector(%v) = (%q,%q), want empty", fields, icon, label)
		}
	}
}

func TestFormatSpawnVectorLine(t *testing.T) {
	// A classifiable spawn renders "icon label".
	got := formatSpawnVectorLine(map[string]string{"spawn_source": "slack-thread", "slack_user_id": "U07X"})
	if !strings.HasPrefix(got, spawnIconSlack+" ") {
		t.Errorf("line %q should start with the slack icon", got)
	}
	if !strings.Contains(got, "<@U07X>") {
		t.Errorf("line %q should mention the user", got)
	}

	// No context → empty line (so the card omits it entirely).
	if line := formatSpawnVectorLine(map[string]string{}); line != "" {
		t.Errorf("expected empty line for no spawn context, got %q", line)
	}
}

func TestBuildAgentCardBlocks_SpawnVectorLineInContext(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", time.Time{}, "", "", "crew", "",
		"",    // contextLine
		nil,   // toolStats
		"",    // bridgeVersion
		false, // schedulingBlocked
		triggerCardInfo{SpawnVectorLine: spawnIconCron + " nightly triage (cron)"},
		nil, // local
	)
	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}
	contextBlock := blocks[1].(*slack.ContextBlock)
	found := false
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			if strings.Contains(textObj.Text, Garden.Semantic(SemSchedule)+" nightly triage (cron)") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected spawn-vector line in context block")
	}
}

func TestIsGardenerSpawner(t *testing.T) {
	cases := map[string]bool{
		"":               false,
		"trigger:gd-ABC": false, // trigger spawns are classified as webhook, not gardener
		"gardenio-gardenio-gardener-delivery-r-skhk": true,
		"gardenio-some-crew-agent-xyz":               false,
		"escalation-bridge":                          false,
	}
	for in, want := range cases {
		if got := isGardenerSpawner(in); got != want {
			t.Errorf("isGardenerSpawner(%q) = %v, want %v", in, got, want)
		}
	}
}
