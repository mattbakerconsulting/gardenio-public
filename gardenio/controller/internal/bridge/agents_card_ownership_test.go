package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// TestFormatOwnershipCardLine covers the ownership footer helper (ka-0000003131):
// the "Resolved by: <gardener> · Owner: <human>" line rendered on the live card.
func TestFormatOwnershipCardLine(t *testing.T) {
	tests := []struct {
		name string
		in   triggerCardInfo
		want string
	}{
		{
			name: "neither field renders nothing",
			in:   triggerCardInfo{},
			want: "",
		},
		{
			name: "resolved gardener only",
			in:   triggerCardInfo{ResolvedBy: "Delivery Drive"},
			want: "🐝 Resolved by: Delivery Drive",
		},
		{
			name: "slack owner renders as a mention",
			in:   triggerCardInfo{Owner: "slack:U0OPERATOR1"},
			want: "🐝 Owner: <@U0OPERATOR1>",
		},
		{
			name: "oidc owner strips the prefix",
			in:   triggerCardInfo{Owner: "oidc:matthew.baker@acme.example"},
			want: "🐝 Owner: matthew.baker@acme.example",
		},
		{
			name: "both halves join with a middot",
			in: triggerCardInfo{
				ResolvedBy: "Nightly Cleanup",
				Owner:      "slack:U0OPERATOR1",
			},
			want: "🐝 Resolved by: Nightly Cleanup · Owner: <@U0OPERATOR1>",
		},
		{
			name: "gardener name with mrkdwn meta chars is escaped",
			in:   triggerCardInfo{ResolvedBy: "A<b>&c"},
			want: "🐝 Resolved by: A&lt;b&gt;&amp;c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatOwnershipCardLine(Garden, tt.in); got != tt.want {
				t.Errorf("formatOwnershipCardLine(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestBuildAgentCardBlocks_OwnershipFooter asserts the resolved-gardener + owner
// footer is rendered into the context block of the full agent card.
func TestBuildAgentCardBlocks_OwnershipFooter(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"",          // subject
		0,           // pending
		"working",   // state
		"",          // taskTitle
		time.Time{}, // seen (zero)
		"",          // agentHref
		"",          // imageTag
		"",          // role
		"",          // costLine
		"",          // contextLine
		nil,         // toolStats
		"",          // bridgeVersion
		false,       // schedulingBlocked
		triggerCardInfo{
			ResolvedBy: "Delivery Drive",
			Owner:      "slack:U0OPERATOR1",
		},
		nil, // local
	)

	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}
	contextBlock, ok := blocks[1].(*slack.ContextBlock)
	if !ok {
		t.Fatalf("expected context block at index 1, got %T", blocks[1])
	}
	var rendered string
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			rendered += textObj.Text
		}
	}
	if !strings.Contains(rendered, "Resolved by: Delivery Drive") {
		t.Errorf("expected resolved-gardener in card footer, got %q", rendered)
	}
	if !strings.Contains(rendered, "Owner: <@U0OPERATOR1>") {
		t.Errorf("expected owner mention in card footer, got %q", rendered)
	}
}

// TestBuildAgentCardBlocks_NoOwnershipFooter asserts the footer is absent when
// neither field is set — an ad-hoc, ownerless spawn keeps the card clean.
func TestBuildAgentCardBlocks_NoOwnershipFooter(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", 0, "working", "", time.Time{}, "", "", "", "", "",
		nil, "", false, triggerCardInfo{},
		nil, // local
	)
	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}
	contextBlock := blocks[1].(*slack.ContextBlock)
	var rendered string
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			rendered += textObj.Text
		}
	}
	if strings.Contains(rendered, Garden.Semantic(SemAssignee)) {
		t.Errorf("expected no ownership footer, got %q", rendered)
	}
}

// TestFormatOwnershipCardLine_BusinessTheme: Business renders the footer behind
// the [ASSIGNEE] tag instead of Garden's glyph.
func TestFormatOwnershipCardLine_BusinessTheme(t *testing.T) {
	got := formatOwnershipCardLine(Business, triggerCardInfo{ResolvedBy: "Nightly Cleanup", Owner: "slack:U0OPERATOR1"})
	want := "[ASSIGNEE] Resolved by: Nightly Cleanup · Owner: <@U0OPERATOR1>"
	if got != want {
		t.Errorf("Business ownership line = %q, want %q", got, want)
	}
}
