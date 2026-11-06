package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

func TestExtractImageTag(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"full reference", "ghcr.io/org/agent:v2026.58.3", "v2026.58.3"},
		{"latest tag", "ghcr.io/org/agent:latest", "latest"},
		{"bare tag", "v2026.58.3", "v2026.58.3"},
		{"empty", "", ""},
		{"no tag", "ghcr.io/org/agent", "ghcr.io/org/agent"},
		{"port in registry", "registry:5000/img:v1", "v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractImageTag(tc.input); got != tc.want {
				t.Errorf("extractImageTag(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestBuildAgentCardBlocks_ImageTagInContext(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"",                // subject
		0,                 // pending
		"working",         // state
		"",                // taskTitle
		time.Time{},       // seen (zero)
		"",                // agentHref
		"v2026.58.3",      // imageTag
		"",                // role
		"",                // costLine
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	// The context block (index 1) should contain the image tag.
	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}

	// Render the context block text to check for the image tag.
	// Context blocks contain TextBlockObject elements.
	contextBlock := blocks[1]
	rendered := contextBlock.BlockType()
	if rendered != "context" {
		t.Fatalf("expected context block at index 1, got %q", rendered)
	}

	// Verify the image tag appears in the agent card context.
	// We check all text elements in the context block.
	found := false
	for _, elem := range contextBlock.(*slack.ContextBlock).ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			if strings.Contains(textObj.Text, "v2026.58.3") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected image tag v2026.58.3 in context block text")
	}
}

func TestBuildAgentCardBlocks_BridgeVersionInContext(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", time.Now(), "", "", "", "",
		"",                // contextLine
		nil,               // toolStats
		"v2026.105.5",     // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}

	contextBlock := blocks[1].(*slack.ContextBlock)
	found := false
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			if strings.Contains(textObj.Text, "v2026.105.5") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected bridge version v2026.105.5 in context block text")
	}
}

func TestBuildAgentCardBlocks_NoBridgeVersion(t *testing.T) {
	seen := time.Now()
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", seen, "", "", "", "",
		"",                // contextLine
		nil,               // toolStats
		"",                // no bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}

	contextBlock := blocks[1].(*slack.ContextBlock)
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			// Time should be the last separator-delimited field when no version is present.
			if strings.HasSuffix(strings.TrimSpace(strings.Split(textObj.Text, "\n")[0]), "· ") {
				t.Error("trailing separator when bridgeVersion is empty")
			}
		}
	}
}

func TestBuildAgentCardBlocks_NoImageTag(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0,
		"working",
		"",
		time.Time{},
		"",
		"",                // no imageTag
		"",                // role
		"",                // costLine
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}

	contextBlock := blocks[1].(*slack.ContextBlock)
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			// The context should still have the bead ID and thread line, but no image tag separator after "Decisions thread below".
			if strings.Contains(textObj.Text, "Decisions thread below ·") && !strings.Contains(textObj.Text, "Decisions thread below · _") {
				// If there's a dot after "Decisions thread below" and it's not the seen timestamp,
				// then something is wrong — an empty tag might have been appended.
				t.Error("unexpected separator after 'Decisions thread below' when imageTag is empty")
			}
		}
	}
}

func TestBuildAgentCardBlocks_RoleInHeader(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"",                // subject
		0,                 // pending
		"working",         // state
		"",                // taskTitle
		time.Time{},       // seen (zero)
		"",                // agentHref
		"",                // imageTag
		"lead",            // role
		"",                // costLine
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	if len(blocks) < 1 {
		t.Fatalf("expected at least 1 block, got %d", len(blocks))
	}

	// The header section (index 0) should contain the role.
	headerBlock, ok := blocks[0].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("expected section block at index 0, got %T", blocks[0])
	}
	if !strings.Contains(headerBlock.Text.Text, "lead") {
		t.Errorf("expected role 'lead' in header text, got %q", headerBlock.Text.Text)
	}
}

func TestBuildAgentCardBlocks_MultiRole(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", time.Time{}, "", "",
		"crew,thread",     // multi-role
		"",                // costLine
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	headerBlock := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(headerBlock.Text.Text, "crew,thread") {
		t.Errorf("expected multi-role 'crew,thread' in header, got %q", headerBlock.Text.Text)
	}
}

func TestBuildAgentCardBlocks_NoRole(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", time.Time{}, "", "",
		"",                // no role
		"",                // costLine
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	headerBlock := blocks[0].(*slack.SectionBlock)
	// Should have project and agent-link separators but none extra for empty role.
	// Format: ":large_green_circle: working · _gardenio_ · *test-bot*"
	if strings.Count(headerBlock.Text.Text, "·") != 2 {
		t.Errorf("expected 2 separators (project + status) with no role, got %q", headerBlock.Text.Text)
	}
}

// TestBuildAgentCardBlocks_AgentLinkAtEnd guards the Slack-preview ordering
// (ka-0000002097): the full channel card must LEAD with status text and put the
// clickable agent link LAST. Slack derives channel/sidebar previews from the
// leading text of the first block, so a link-first header made every preview
// open with an ugly bare link. A prior fix (ka-0000003557) reordered only the
// compact thread card and left this one regressed; this test pins the ordering
// so it can't silently recur.
func TestBuildAgentCardBlocks_AgentLinkAtEnd(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject -> falls back to agent name "test-bot"
		0, "working", "", time.Time{}, "", "",
		"crew",            // role
		"",                // costLine
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	header := blocks[0].(*slack.SectionBlock).Text.Text
	statusIdx := strings.Index(header, "working")
	linkIdx := strings.Index(header, "test-bot")
	if statusIdx == -1 || linkIdx == -1 {
		t.Fatalf("expected both status and agent name in header, got %q", header)
	}
	if statusIdx > linkIdx {
		t.Errorf("expected status to lead and agent link to come LAST, got %q", header)
	}
	// The header must not OPEN with the agent link — that is what poisons the
	// Slack chat preview. Strip the leading indicator + space, then assert the
	// next token is the status, not the bold link.
	if strings.HasPrefix(header, ":large_green_circle: *") {
		t.Errorf("header must not lead with the bold agent link, got %q", header)
	}
}

func TestBuildAgentCardBlocks_SubjectAsHeaderTitle(t *testing.T) {
	// When a canonical subject is supplied (e.g. a ticket key), the header
	// title shows the subject instead of the verby agent slug. The slug still
	// appears verbatim in the context line as the bead identifier.
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/thread-1780064144-940679",
		"FSM-1234", // subject
		0, "working", "", time.Time{}, "", "", "", "",
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	headerBlock := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(headerBlock.Text.Text, "FSM-1234") {
		t.Errorf("expected subject 'FSM-1234' in header, got %q", headerBlock.Text.Text)
	}
	if strings.Contains(headerBlock.Text.Text, "thread-1780064144-940679") {
		t.Errorf("expected verby slug to be absent from header, got %q", headerBlock.Text.Text)
	}

	// The agent slug remains the bead identifier in the context line.
	contextBlock := blocks[1].(*slack.ContextBlock)
	foundSlug := false
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			if strings.Contains(textObj.Text, "thread-1780064144-940679") {
				foundSlug = true
				break
			}
		}
	}
	if !foundSlug {
		t.Error("expected agent slug in context line as bead identifier")
	}
}

func TestBuildAgentCardBlocks_EmptySubjectFallsBackToSlug(t *testing.T) {
	// With no subject, the header falls back to the agent short name.
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", time.Time{}, "", "", "", "",
		"", nil, "", false, triggerCardInfo{},
		nil, // local
	)
	headerBlock := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(headerBlock.Text.Text, "test-bot") {
		t.Errorf("expected agent name 'test-bot' in header fallback, got %q", headerBlock.Text.Text)
	}
}

// TestBuildAgentCardBlocks_DecisionPendingIndicator verifies that a pending
// decision surfaces ⏸️ "decision pending" as the canonical card's primary state
// indicator (ka-0000000977 acceptance #5), rather than the old blue-circle
// "N pending" status. The decision prompt + resolution UI live in the thread
// reply; the card shows only the paused state.
func TestBuildAgentCardBlocks_DecisionPendingIndicator(t *testing.T) {
	cases := []struct {
		name       string
		pending    int
		wantStatus string
	}{
		{"single decision", 1, "decision pending"},
		{"multiple decisions", 3, "3 decisions pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := buildAgentCardBlocks(Garden,
				"gardenio/crew/test-bot",
				"",         // subject
				tc.pending, // pending
				"working",  // state — pending wins over working
				"", time.Time{}, "", "", "", "",
				"", nil, "", false, triggerCardInfo{},
				nil, // local
			)
			headerBlock := blocks[0].(*slack.SectionBlock)
			text := headerBlock.Text.Text
			if !strings.Contains(text, Garden.Glyph(LifecycleNeedsDecision)) {
				t.Errorf("expected needs-decision indicator, got %q", text)
			}
			if !strings.Contains(text, tc.wantStatus) {
				t.Errorf("expected status %q in header, got %q", tc.wantStatus, text)
			}
			// The old working / blue-circle indicators must NOT win while a
			// decision is pending.
			if strings.Contains(text, ":large_green_circle:") || strings.Contains(text, ":large_blue_circle:") {
				t.Errorf("expected pending to override working indicator, got %q", text)
			}
		})
	}
}

func TestBuildWrapUpAgentCardBlocks_Done(t *testing.T) {
	// Blockers present → outcome is "blocked", not "done".
	wrapupJSON := `{"accomplishments":"Closed 3 bugs","blockers":"API key pending"}`
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "gardenio/crew/test-bot", "done", wrapupJSON, "", 0, nil)

	// header, accomplishments, blockers/handoff detail (context), action.
	if len(blocks) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(blocks))
	}

	// First block: header with agent name and outcome.
	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, "test-bot") {
		t.Errorf("expected agent name in header, got: %s", header.Text.Text)
	}
	// Blockers present → blocked glyph + label, not the done check.
	if !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleBlocked)) {
		t.Errorf("expected blocked indicator in header, got: %s", header.Text.Text)
	}
	if !strings.Contains(header.Text.Text, "blocked") {
		t.Errorf("expected 'blocked' outcome in header, got: %s", header.Text.Text)
	}

	// Second block: accomplishments headline.
	acc := blocks[1].(*slack.SectionBlock)
	if !strings.Contains(acc.Text.Text, "Accomplishments") || !strings.Contains(acc.Text.Text, "Closed 3 bugs") {
		t.Errorf("expected accomplishments in block, got: %s", acc.Text.Text)
	}

	// Blockers move into a de-emphasized context block, not the headline.
	detail := blocks[2].(*slack.ContextBlock)
	detailText := detail.ContextElements.Elements[0].(*slack.TextBlockObject).Text
	if !strings.Contains(detailText, "Blockers") || !strings.Contains(detailText, "API key pending") {
		t.Errorf("expected blockers in context detail block, got: %s", detailText)
	}

	// Last block: action with Clear button.
	if blocks[3].BlockType() != "actions" {
		t.Errorf("expected actions block at end, got %q", blocks[3].BlockType())
	}
}

func TestBuildWrapUpAgentCardBlocks_CleanDone(t *testing.T) {
	// No blockers, no handoff → clean done outcome.
	wrapupJSON := `{"accomplishments":"Shipped the feature"}`
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", wrapupJSON, "", 0, nil)

	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleDone)) {
		t.Errorf("expected done indicator in header, got: %s", header.Text.Text)
	}
	if !strings.Contains(header.Text.Text, "done") {
		t.Errorf("expected 'done' outcome in header, got: %s", header.Text.Text)
	}
}

func TestBuildWrapUpAgentCardBlocks_HandedOff(t *testing.T) {
	// Handoff notes but no blockers → handed-off outcome.
	wrapupJSON := `{"accomplishments":"Drafted the migration","handoff_notes":"Run the migration after review"}`
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", wrapupJSON, "", 0, nil)

	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleHandedOff)) {
		t.Errorf("expected handed-off indicator in header, got: %s", header.Text.Text)
	}
	if !strings.Contains(header.Text.Text, "handed-off") {
		t.Errorf("expected 'handed-off' outcome in header, got: %s", header.Text.Text)
	}
}

func TestBuildWrapUpAgentCardBlocks_Failed(t *testing.T) {
	wrapupJSON := `{"accomplishments":"Partial work done"}`
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "failed", wrapupJSON, "", 0, nil)

	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleAborted)) {
		t.Errorf("expected aborted indicator in header, got: %s", header.Text.Text)
	}
	if !strings.Contains(header.Text.Text, "aborted") {
		t.Errorf("expected 'aborted' outcome in header, got: %s", header.Text.Text)
	}
}

func TestBuildWrapUpAgentCardBlocks_Duration(t *testing.T) {
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", `{"accomplishments":"done"}`, "", 95*time.Second, nil)
	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, "ran 1m 35s") {
		t.Errorf("expected duration in header, got: %s", header.Text.Text)
	}
}

func TestBuildWrapUpAgentCardBlocks_Links(t *testing.T) {
	b := &Bot{}
	wrapupJSON := `{"accomplishments":"shipped","pull_requests":["https://gitlab.com/x/-/merge_requests/1"],"beads_closed":["gd-ABC"]}`
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", wrapupJSON, "", 0, nil)
	// header, accomplishments, links, action.
	var found bool
	for _, blk := range blocks {
		if sec, ok := blk.(*slack.SectionBlock); ok && sec.Text != nil {
			if strings.Contains(sec.Text.Text, "MRs/PRs") && strings.Contains(sec.Text.Text, "merge_requests/1") && strings.Contains(sec.Text.Text, "gd-ABC") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected inline MR + bead links block, blocks: %+v", blocks)
	}
}

func TestBuildWrapUpAgentCardBlocks_EmptyWrapUp(t *testing.T) {
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", `{}`, "", 0, nil)

	// Empty wrapup: header + action only (no content/detail blocks).
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks for empty wrapup, got %d", len(blocks))
	}
}

func TestFormatWrapUpSlack_FullWrapUp(t *testing.T) {
	wrapupJSON := `{"accomplishments":"Closed 3 bugs","blockers":"API key pending","handoff_notes":"Check PR #42","beads_closed":["gd-1","gd-2"],"pull_requests":["https://github.com/org/repo/pull/42"]}`
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, wrapupJSON)

	if !strings.Contains(result, "*Accomplishments:* Closed 3 bugs") {
		t.Errorf("expected accomplishments in output, got: %s", result)
	}
	if !strings.Contains(result, "*Blockers:* API key pending") {
		t.Errorf("expected blockers in output, got: %s", result)
	}
	if !strings.Contains(result, "*Handoff:* Check PR #42") {
		t.Errorf("expected handoff notes in output, got: %s", result)
	}
	if !strings.Contains(result, "*Beads closed:* gd-1, gd-2") {
		t.Errorf("expected beads closed in output, got: %s", result)
	}
	if !strings.Contains(result, "*PRs:* https://github.com/org/repo/pull/42") {
		t.Errorf("expected pull requests in output, got: %s", result)
	}
}

func TestFormatWrapUpSlack_AccomplishmentsOnly(t *testing.T) {
	wrapupJSON := `{"accomplishments":"Fixed login flow"}`
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, wrapupJSON)

	if !strings.Contains(result, "*Accomplishments:* Fixed login flow") {
		t.Errorf("expected accomplishments in output, got: %s", result)
	}
	if strings.Contains(result, "Blockers") {
		t.Errorf("should not contain blockers when empty, got: %s", result)
	}
}

func TestFormatWrapUpSlack_InvalidJSON(t *testing.T) {
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, "not valid json")
	if !strings.Contains(result, "not valid json") {
		t.Errorf("expected raw text fallback for invalid JSON, got: %s", result)
	}
}

// TestFormatWrapUpSlack_TLDRIsFirstLine verifies that a validated tldr field
// is surfaced as the FIRST body line of the wrap-up (bead ka-0000006621), so
// it sits at the top of the agent card above accomplishments/blockers.
func TestFormatWrapUpSlack_TLDRIsFirstLine(t *testing.T) {
	wrapupJSON := `{"tldr":"Shipped the card TLDR feature","accomplishments":"Closed 3 bugs","blockers":"API key pending"}`
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, wrapupJSON)

	if !strings.Contains(result, "Shipped the card TLDR feature") {
		t.Fatalf("expected tldr in output, got: %s", result)
	}
	// The tldr must precede accomplishments so it lands at the top of the card.
	tldrIdx := strings.Index(result, "Shipped the card TLDR feature")
	accIdx := strings.Index(result, "Accomplishments")
	if accIdx >= 0 && tldrIdx > accIdx {
		t.Errorf("expected tldr before accomplishments, got: %s", result)
	}
	// The featured headline carries the note glyph + bold.
	if !strings.Contains(result, Garden.Semantic(SemNote)+" *Shipped the card TLDR feature*") {
		t.Errorf("expected featured tldr headline, got: %s", result)
	}
}

// TestFormatWrapUpSlack_TLDRSingleEscaped verifies the thread-reply path escapes
// a tldr's angle brackets exactly once for mrkdwn — never double-escaped
// (&amp;lt;) — for both a fresh raw tldr and a legacy pre-escaped one
// (ka-0000005806).
func TestFormatWrapUpSlack_TLDRSingleEscaped(t *testing.T) {
	cases := []struct {
		name   string
		wrapup string
	}{
		{"fresh raw", `{"tldr":"parked with fresh <2h P0; esc P1->P0"}`},
		{"legacy pre-escaped", `{"tldr":"parked with fresh &lt;2h P0; esc P1-&gt;P0"}`},
	}
	b := &Bot{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := b.formatWrapUpSlack(Garden, tc.wrapup)
			// Single mrkdwn escape: '<' -> "&lt;", '>' -> "&gt;".
			if !strings.Contains(result, "&lt;2h") || !strings.Contains(result, "P1-&gt;P0") {
				t.Errorf("expected single-escaped angle brackets, got: %s", result)
			}
			if strings.Contains(result, "&amp;lt;") || strings.Contains(result, "&amp;gt;") {
				t.Errorf("tldr is double-escaped, got: %s", result)
			}
		})
	}
}

// TestFormatWrapUpSlack_NoTLDRUnchanged verifies that when tldr is absent the
// output degrades gracefully — no empty top line, behavior unchanged.
func TestFormatWrapUpSlack_NoTLDRUnchanged(t *testing.T) {
	wrapupJSON := `{"accomplishments":"Fixed login flow"}`
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, wrapupJSON)

	if strings.Contains(result, Garden.Semantic(SemNote)) {
		t.Errorf("expected no tldr headline when tldr absent, got: %s", result)
	}
	// First line should still be accomplishments.
	if !strings.HasPrefix(strings.TrimPrefix(result, "\n"), "*Accomplishments:*") {
		t.Errorf("expected accomplishments as first line, got: %s", result)
	}
}

// TestFormatWrapUpSlack_TLDROnly verifies a wrap-up carrying only a tldr (no
// other fields) still renders the featured headline.
func TestFormatWrapUpSlack_TLDROnly(t *testing.T) {
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, `{"tldr":"One-line summary"}`)
	if !strings.Contains(result, Garden.Semantic(SemNote)+" *One-line summary*") {
		t.Errorf("expected featured tldr-only headline, got: %s", result)
	}
}

// TestFormatWrapUpSlack_TLDREscapedAndCapped verifies the headline is mrkdwn-
// escaped and length-capped so a long or markup-bearing tldr can't distort the
// card.
func TestFormatWrapUpSlack_TLDREscapedAndCapped(t *testing.T) {
	long := strings.Repeat("x", 500)
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, `{"tldr":"a <b> & c `+long+`"}`)

	if strings.Contains(result, "<b>") {
		t.Errorf("expected angle brackets escaped, got: %s", result)
	}
	if !strings.Contains(result, "&lt;b&gt;") || !strings.Contains(result, "&amp;") {
		t.Errorf("expected escaped mrkdwn in headline, got: %s", result)
	}
	// The raw tldr is truncated to the display cap before escaping, so the
	// headline ends in an ellipsis and does not carry all 500 x's.
	if !strings.Contains(result, "...") {
		t.Errorf("expected truncated tldr (ellipsis), got: %s", result)
	}
	if strings.Contains(result, long) {
		t.Errorf("expected long tldr to be capped, but full text present: %s", result)
	}
}

// TestBuildWrapUpAgentCardBlocks_TLDRAtTop verifies that the edit-in-place
// block-layout agent card promotes the validated tldr to the leading Slack
// *header* banner (bead ka-0000002919), with the agent/run metadata demoted to
// a context line beneath it and accomplishments below that.
func TestBuildWrapUpAgentCardBlocks_TLDRAtTop(t *testing.T) {
	wrapupJSON := `{"tldr":"Top-line headline","accomplishments":"Did the work"}`
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", wrapupJSON, "", 0, nil)

	// header banner + meta context + accomplishments section + action.
	if len(blocks) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(blocks))
	}
	// blocks[0] is the tldr promoted to a header banner (done glyph + text).
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected blocks[0] to be a HeaderBlock, got %T", blocks[0])
	}
	if header.Text.Type != "plain_text" {
		t.Errorf("header must be plain_text, got %q", header.Text.Type)
	}
	if !strings.Contains(header.Text.Text, "Top-line headline") || !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleDone)+" done") {
		t.Errorf("expected glyph glued to its label + tldr in header banner, got: %s", header.Text.Text)
	}
	// blocks[1] is the de-emphasized metadata context line (name + outcome).
	meta := blocks[1].(*slack.ContextBlock)
	metaText := meta.ContextElements.Elements[0].(*slack.TextBlockObject).Text
	if !strings.Contains(metaText, "test-bot") || !strings.Contains(metaText, "done") {
		t.Errorf("expected agent name + outcome in meta context, got: %s", metaText)
	}
	// blocks[2] is the accomplishments headline, below the banner.
	acc := blocks[2].(*slack.SectionBlock)
	if !strings.Contains(acc.Text.Text, "Did the work") {
		t.Errorf("expected accomplishments in block 2, got: %s", acc.Text.Text)
	}
	// The header banner must not carry the accomplishments prose.
	if strings.Contains(header.Text.Text, "Did the work") {
		t.Errorf("header banner should not contain accomplishments text, got: %s", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_TLDRHeaderUnescaped verifies that a tldr
// carrying mrkdwn-escaped angle brackets (as normalizeAndValidateTLDR stores
// them) renders as literal characters in the plain_text header banner — not as
// "&lt;"/"&gt;" entities.
func TestBuildWrapUpAgentCardBlocks_TLDRHeaderUnescaped(t *testing.T) {
	wrapupJSON := `{"tldr":"Fixed &lt;Foo&gt; parser","accomplishments":"x"}`
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", wrapupJSON, "", 0, nil)
	header := blocks[0].(*slack.HeaderBlock)
	if !strings.Contains(header.Text.Text, "<Foo>") {
		t.Errorf("expected un-escaped angle brackets in header, got: %s", header.Text.Text)
	}
	if strings.Contains(header.Text.Text, "&lt;") || strings.Contains(header.Text.Text, "&gt;") {
		t.Errorf("header should not contain mrkdwn entities, got: %s", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_NoTLDR verifies that when tldr is absent the
// card has no featured headline and accomplishments is the first body block.
func TestBuildWrapUpAgentCardBlocks_NoTLDR(t *testing.T) {
	wrapupJSON := `{"accomplishments":"Did the work"}`
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", wrapupJSON, "", 0, nil)

	// header + accomplishments section + action (no tldr block).
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks when tldr absent, got %d", len(blocks))
	}
	body := blocks[1].(*slack.SectionBlock)
	if strings.Contains(body.Text.Text, Garden.Semantic(SemNote)) {
		t.Errorf("expected no tldr headline when tldr absent, got: %s", body.Text.Text)
	}
	if !strings.Contains(body.Text.Text, "Accomplishments") {
		t.Errorf("expected accomplishments as first body block, got: %s", body.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_CloseReasonFallback verifies the render-side
// fallback (ka-0000000101): a terminal bead with NO wrap-up tldr but a
// close_reason in its fields still gets a *header* block derived from the
// close reason, so the card is never metadata-only.
func TestBuildWrapUpAgentCardBlocks_CloseReasonFallback(t *testing.T) {
	b := &Bot{}
	// No wrapup at all (empty JSON) + a stop-gate cooldown close_reason.
	fields := map[string]string{
		"close_reason": "stop_gate_cooldown: claude exited without gb done",
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", "", "", 0, fields)
	if len(blocks) == 0 {
		t.Fatal("expected at least one block")
	}
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block from the close_reason fallback, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "idled out") {
		t.Errorf("expected idle-out headline from close_reason, got: %q", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_ContextCancelledFallback verifies the
// context_cancelled crash path also yields a headline (acceptance criterion).
func TestBuildWrapUpAgentCardBlocks_ContextCancelledFallback(t *testing.T) {
	b := &Bot{}
	fields := map[string]string{"close_reason": "context_cancelled"}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", "", "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if strings.TrimSpace(header.Text.Text) == "" {
		t.Error("expected non-empty tldr header for context_cancelled close")
	}
}

// TestBuildWrapUpAgentCardBlocks_RealTLDRWins verifies a genuine wrap-up tldr
// takes precedence over the close_reason fallback (do not clobber real data).
func TestBuildWrapUpAgentCardBlocks_RealTLDRWins(t *testing.T) {
	b := &Bot{}
	wrapupJSON := `{"tldr":"Merged MR 1234; closed 3 beads","accomplishments":"work"}`
	fields := map[string]string{"close_reason": "stop_gate_cooldown: claude exited without gb done"}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", wrapupJSON, "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "Merged MR 1234") {
		t.Errorf("real tldr should win over close_reason fallback, got: %q", header.Text.Text)
	}
	if strings.Contains(header.Text.Text, "idled out") {
		t.Errorf("close_reason fallback should not appear when a real tldr exists, got: %q", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_ZeroInferencePlatformFailure verifies the fix
// for ka-0000000315: an agent that completed zero inference turns (a request
// was counted but no context reached the model and no tool ran — the exact
// state of the 2026-07-25 opus-5 outage) is re-badged as a platform failure
// instead of rendering the misleading synthesized "Pod terminated…" tldr, which
// reads as agent laziness.
func TestBuildWrapUpAgentCardBlocks_ZeroInferencePlatformFailure(t *testing.T) {
	b := &Bot{}
	// The synthesized fallback the close path writes for a never-started agent:
	// a machine tldr, marked synthesized, that would otherwise be the banner.
	wrapupJSON := `{"tldr":"Pod terminated before an explicit wrap-up was submitted","accomplishments":"Pod terminated before an explicit wrap-up was submitted","custom":{"synthesized":"true"}}`
	fields := map[string]string{
		"close_reason":        "context_cancelled",
		"total_request_count": "1",
		"total_input_tokens":  "0",
		"total_tool_calls":    "0",
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", wrapupJSON, "", 0, fields)

	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block for the platform-failure card, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "Platform failure") {
		t.Errorf("expected a platform-failure headline, got: %q", header.Text.Text)
	}
	if !strings.Contains(header.Text.Text, "never got an inference turn") {
		t.Errorf("expected the never-started phrasing, got: %q", header.Text.Text)
	}
	// The misleading synthesized headline must NOT survive.
	if strings.Contains(header.Text.Text, "Pod terminated") {
		t.Errorf("synthesized 'Pod terminated' tldr must be replaced for a zero-inference agent, got: %q", header.Text.Text)
	}
	// The platform-failure glyph rides the banner.
	if !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleAborted)+" platform failure") {
		t.Errorf("expected the aborted glyph glued to the platform-failure label in the banner, got: %q", header.Text.Text)
	}
	// The metadata context line carries the "platform failure" label.
	ctxBlock, ok := blocks[1].(*slack.ContextBlock)
	if !ok {
		t.Fatalf("expected a context block for the metadata line, got %T", blocks[1])
	}
	metaText := ctxBlock.ContextElements.Elements[0].(*slack.TextBlockObject).Text
	if !strings.Contains(metaText, "platform failure") {
		t.Errorf("expected 'platform failure' label in the metadata line, got: %q", metaText)
	}
}

// TestBuildWrapUpAgentCardBlocks_RealWrapUpBeatsPlatformFailure verifies the
// platform-failure override never clobbers a genuine agent-authored wrap-up:
// even if the usage fields spuriously read zero-inference, a real (non-
// synthesized) tldr wins (ka-0000000315).
func TestBuildWrapUpAgentCardBlocks_RealWrapUpBeatsPlatformFailure(t *testing.T) {
	b := &Bot{}
	wrapupJSON := `{"tldr":"Merged MR 4321; closed 2 beads","accomplishments":"real work"}`
	fields := map[string]string{
		"total_request_count": "1",
		"total_input_tokens":  "0",
		"total_tool_calls":    "0",
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", wrapupJSON, "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "Merged MR 4321") {
		t.Errorf("a genuine wrap-up tldr must win over the platform-failure override, got: %q", header.Text.Text)
	}
	if strings.Contains(header.Text.Text, "Platform failure") {
		t.Errorf("platform-failure override must not fire when a real wrap-up exists, got: %q", header.Text.Text)
	}
}

// priorWrapupJSON builds a prior_wrapup payload (the full JSON wrap-up the
// resume path archives) carrying the given tldr.
func priorWrapupJSON(tldr string) string {
	return fmt.Sprintf(`{"tldr":%q,"accomplishments":"prior work"}`, tldr)
}

// TestBuildWrapUpAgentCardBlocks_PriorWrapupFallback verifies the prior-turn
// fallback (ka-0000003692): a resumed agent whose live `wrapup` was nil-deleted
// still gets a headline — recovered from the archived prior_wrapup and MARKED
// "Previous turn:" — instead of a blank card.
func TestBuildWrapUpAgentCardBlocks_PriorWrapupFallback(t *testing.T) {
	b := &Bot{}
	fields := map[string]string{
		"prior_wrapup": priorWrapupJSON("Merged MR 1165; closed 2 beads"),
	}
	// No live wrapup (the nil-delete happened), no close_reason.
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", "", "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block from the prior_wrapup fallback, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "Merged MR 1165") {
		t.Errorf("expected the prior turn's tldr in the headline, got: %q", header.Text.Text)
	}
	// It MUST be marked as the previous turn so it can never masquerade as the
	// current outcome — the anti-stale invariant.
	if !strings.Contains(header.Text.Text, "Previous turn") {
		t.Errorf("prior_wrapup headline must be marked 'Previous turn', got: %q", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_RealTLDRBeatsPriorWrapup verifies a current
// turn's wrap-up always wins over the archived prior_wrapup.
func TestBuildWrapUpAgentCardBlocks_RealTLDRBeatsPriorWrapup(t *testing.T) {
	b := &Bot{}
	wrapupJSON := `{"tldr":"This turn: shipped the fix","accomplishments":"work"}`
	fields := map[string]string{
		"prior_wrapup": priorWrapupJSON("Previous turn summary that must not show"),
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", wrapupJSON, "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "This turn") {
		t.Errorf("the current turn's tldr must win, got: %q", header.Text.Text)
	}
	if strings.Contains(header.Text.Text, "Previous turn") {
		t.Errorf("prior_wrapup fallback must not fire when a real tldr exists, got: %q", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_PlatformFailureBeatsPriorWrapup is the
// important ladder-order assertion: a genuine platform failure on THIS turn
// must never be masked by a rosy previous-turn summary.
func TestBuildWrapUpAgentCardBlocks_PlatformFailureBeatsPriorWrapup(t *testing.T) {
	b := &Bot{}
	fields := map[string]string{
		"prior_wrapup":        priorWrapupJSON("All green last turn — merged everything"),
		"total_request_count": "1",
		"total_input_tokens":  "0",
		"total_tool_calls":    "0",
	}
	// No live wrapup — the resumed turn never got an inference turn.
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", "", "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "Platform failure") {
		t.Errorf("a THIS-turn platform failure must win over prior_wrapup, got: %q", header.Text.Text)
	}
	if strings.Contains(header.Text.Text, "Previous turn") {
		t.Errorf("prior_wrapup must not mask a genuine platform failure, got: %q", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_PriorWrapupBeatsCloseReason verifies a real
// human-authored prior-turn summary outranks the machine-derived close_reason
// headline (the ladder position ABOVE close_reason).
func TestBuildWrapUpAgentCardBlocks_PriorWrapupBeatsCloseReason(t *testing.T) {
	b := &Bot{}
	fields := map[string]string{
		"prior_wrapup": priorWrapupJSON("Real summary from the prior turn"),
		"close_reason": "stop_gate_cooldown: claude exited without gb done",
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", "", "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "Real summary from the prior turn") {
		t.Errorf("prior_wrapup should beat the close_reason derivation, got: %q", header.Text.Text)
	}
	if strings.Contains(header.Text.Text, "idled out") {
		t.Errorf("close_reason headline should not appear when prior_wrapup exists, got: %q", header.Text.Text)
	}
}

// TestPriorWrapupTLDR exercises the extractor across its boundary cases.
func TestPriorWrapupTLDR(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"nil fields", nil, ""},
		{"no prior_wrapup", map[string]string{}, ""},
		{"empty prior_wrapup", map[string]string{"prior_wrapup": "  "}, ""},
		{"malformed prior_wrapup", map[string]string{"prior_wrapup": "not json"}, ""},
		{"prior_wrapup with no tldr", map[string]string{"prior_wrapup": `{"accomplishments":"x"}`}, ""},
		{"prior_wrapup with tldr", map[string]string{"prior_wrapup": priorWrapupJSON("hello")}, "hello"},
	}
	for _, tc := range cases {
		if got := priorWrapupTLDR(tc.fields); got != tc.want {
			t.Errorf("%s: priorWrapupTLDR = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestZeroInferenceTurn exercises the render-side classifier across the
// boundary cases: the reported outage shape, a normal run, and legacy beads
// with absent usage fields (which must NOT be misclassified).
func TestZeroInferenceTurn(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"nil fields", nil, false},
		{"empty fields (legacy bead)", map[string]string{}, false},
		{"reported outage shape", map[string]string{"total_request_count": "1", "total_input_tokens": "0", "total_tool_calls": "0"}, true},
		{"request but no tokens, some tool calls", map[string]string{"total_request_count": "1", "total_input_tokens": "0", "total_tool_calls": "3"}, false},
		{"request with tokens", map[string]string{"total_request_count": "1", "total_input_tokens": "512", "total_tool_calls": "0"}, false},
		{"no request counted yet", map[string]string{"total_input_tokens": "0", "total_tool_calls": "0"}, false},
		{"float-formatted counters", map[string]string{"total_request_count": "2", "total_input_tokens": "0", "total_tool_calls": "0"}, true},
	}
	for _, tc := range cases {
		if got := zeroInferenceTurn(tc.fields); got != tc.want {
			t.Errorf("%s: zeroInferenceTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCloseReasonTLDR verifies the close_reason → tldr mapping covers the
// machine-written reasons and surfaces unknown reasons verbatim.
func TestCloseReasonTLDR(t *testing.T) {
	cases := []struct {
		reason       string
		wantSub      string
		wantNonEmpty bool
	}{
		{"", "", false},
		{"stop_gate_cooldown: claude exited without gb done", "idled out", true},
		{"stop_gate_escape_hatch: claude exited without gb done", "repeated", true},
		{"stop_gate_rate_limited: claude exited without gb done", "rate-limited", true},
		// Involuntary mid-work deaths (account exhaustion/rotation) must NOT read
		// as an idle-out even though the reason still starts with the cooldown /
		// escape-hatch / rate-limited prefix (ka-0000003464 item 3a). The
		// _involuntary case is matched before the bare-prefix cases.
		{"stop_gate_cooldown_involuntary: claude exited mid-work without gb done; reopened gd-TASK1", "involuntarily", true},
		{"stop_gate_escape_hatch_involuntary: claude exited mid-work without gb done", "involuntarily", true},
		{"stop_gate_rate_limited_involuntary: claude exited mid-work without gb done", "involuntarily", true},
		{"context_cancelled", "terminated", true},
		{"podless_leak: agent_state=rate_limited but no pod found", "pod was gone", true},
		{"stale: no activity", "stale", true},
		{"orphan: source task closed", "orphaned", true},
		{"weird_custom_reason", "weird_custom_reason", true},
	}
	for _, tc := range cases {
		got := closeReasonTLDR(tc.reason)
		if tc.wantNonEmpty && strings.TrimSpace(got) == "" {
			t.Errorf("closeReasonTLDR(%q) returned empty", tc.reason)
		}
		if !tc.wantNonEmpty && got != "" {
			t.Errorf("closeReasonTLDR(%q) = %q, want empty", tc.reason, got)
		}
		if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
			t.Errorf("closeReasonTLDR(%q) = %q, want substring %q", tc.reason, got, tc.wantSub)
		}
		// Involuntary mid-work closes must never render the misleading idle-out
		// phrasing, regardless of which stop_gate_* prefix they carry.
		if strings.Contains(tc.reason, "_involuntary") && strings.Contains(got, "idled out") {
			t.Errorf("closeReasonTLDR(%q) = %q must not read 'idled out' for an involuntary close", tc.reason, got)
		}
	}
}

// TestBuildWrapUpAgentCardBlocks_FailureReasonFallback verifies Fix B
// (ka-0000005547): a bead with an empty close_reason but
// failure_reason=killed-by-operator + agent_state=killed (the gvisor-poc HOLD
// canary shape) still gets a "Killed by operator" headline instead of
// rendering metadata-only.
func TestBuildWrapUpAgentCardBlocks_FailureReasonFallback(t *testing.T) {
	b := &Bot{}
	fields := map[string]string{
		"failure_reason": "killed-by-operator",
		"agent_state":    "killed",
		// close_reason intentionally empty — the legacy canary shape.
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "gvisor-poc-canary", "killed", "", "", 0, fields)
	if len(blocks) == 0 {
		t.Fatal("expected at least one block")
	}
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block from the failure_reason fallback, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "Killed by operator") {
		t.Errorf("expected 'Killed by operator' headline from failure_reason, got: %q", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_AgentStateKilledFallback verifies the
// agent_state=killed path yields a headline even when failure_reason is absent.
func TestBuildWrapUpAgentCardBlocks_AgentStateKilledFallback(t *testing.T) {
	b := &Bot{}
	fields := map[string]string{"agent_state": "killed"}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "killed", "", "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "killed") {
		t.Errorf("expected a killed headline from agent_state, got: %q", header.Text.Text)
	}
}

// TestBuildWrapUpAgentCardBlocks_CloseReasonWinsOverFailureReason verifies the
// close_reason fallback takes precedence over failure_reason when both are
// present (the more specific close_reason path fires first).
func TestBuildWrapUpAgentCardBlocks_CloseReasonWinsOverFailureReason(t *testing.T) {
	b := &Bot{}
	fields := map[string]string{
		"close_reason":   "stop_gate_cooldown: claude exited without gb done",
		"failure_reason": "killed-by-operator",
		"agent_state":    "killed",
	}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "thread-bot", "done", "", "", 0, fields)
	header, ok := blocks[0].(*slack.HeaderBlock)
	if !ok {
		t.Fatalf("expected a header block, got %T", blocks[0])
	}
	if !strings.Contains(header.Text.Text, "idled out") {
		t.Errorf("close_reason should win over failure_reason, got: %q", header.Text.Text)
	}
}

// TestFailureReasonTLDR verifies the failure_reason / agent_state → tldr
// mapping (Fix B). killed-by-operator and killed map to distinct phrases;
// empty/unknown inputs return "" so callers fall through to metadata-only.
func TestFailureReasonTLDR(t *testing.T) {
	cases := []struct {
		failureReason string
		agentState    string
		wantSub       string // "" means expect empty result
	}{
		{"", "", ""},
		{"killed-by-operator", "killed", "Killed by operator"},
		{"killed-by-operator", "", "Killed by operator"},
		{"killed_by_operator", "", "Killed by operator"},
		{"killed", "", "Pod killed"},
		{"", "killed", "Pod killed"},
		{"", "failed", "failed"},
		{"oom", "", "Failed: oom"},
		{"", "working", ""},
	}
	for _, tc := range cases {
		got := failureReasonTLDR(tc.failureReason, tc.agentState)
		if tc.wantSub == "" {
			if got != "" {
				t.Errorf("failureReasonTLDR(%q,%q) = %q, want empty", tc.failureReason, tc.agentState, got)
			}
			continue
		}
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("failureReasonTLDR(%q,%q) = %q, want substring %q", tc.failureReason, tc.agentState, got, tc.wantSub)
		}
	}
}

func TestFormatCostSummary(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			"full cost data",
			map[string]string{
				"total_cost_usd":      "1.23",
				"total_input_tokens":  "50000",
				"total_output_tokens": "12000",
				"session_count":       "3",
			},
			"$1.23 · 50.0k in · 12.0k out · 3 sessions",
		},
		{
			"cost only",
			map[string]string{"total_cost_usd": "5.00"},
			"$5.00",
		},
		{
			"empty fields",
			map[string]string{},
			"",
		},
		{
			"nil fields",
			nil,
			"",
		},
		{
			"zero cost ignored",
			map[string]string{"total_cost_usd": "0"},
			"",
		},
		{
			"million tokens",
			map[string]string{"total_input_tokens": "1500000"},
			"1.5M in",
		},
		{
			"small tokens",
			map[string]string{"total_input_tokens": "500"},
			"500 in",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCostSummary(tc.fields)
			if got != tc.want {
				t.Errorf("formatCostSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{500, "500"},
		{1000, "1.0k"},
		{1234, "1.2k"},
		{50000, "50.0k"},
		{1000000, "1.0M"},
		{1500000, "1.5M"},
	}
	for _, tc := range tests {
		got := formatTokenCount(tc.n)
		if got != tc.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestBuildAgentCardBlocks_CostLineInContext(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "done", "", time.Time{}, "", "", "",
		"$1.23 · 50.0k in · 12.0k out",
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)

	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}

	contextBlock := blocks[1].(*slack.ContextBlock)
	found := false
	for _, elem := range contextBlock.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			if strings.Contains(textObj.Text, "$1.23") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected cost line in context block")
	}
}

func TestBuildCompactAgentCardBlocks_CostLine(t *testing.T) {
	blocks := buildCompactAgentCardBlocks(Garden, "test-bot", "done", "$2.50 · 3 sessions", nil)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	section := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(section.Text.Text, "$2.50") {
		t.Errorf("expected cost in compact card, got: %s", section.Text.Text)
	}
}

func TestBuildWrapUpAgentCardBlocks_CostLine(t *testing.T) {
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-bot", "done", `{}`, "$3.00", 0, nil)
	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, "$3.00") {
		t.Errorf("expected cost in wrapup header, got: %s", header.Text.Text)
	}
}

func TestBuildCardStatusFromToolStats(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		stats        *AgentToolStats
		wantContains []string
	}{
		{
			"full stats with label",
			&AgentToolStats{TotalTools: 44, LastTool: "Bash", LastToolLabel: "git status", Activity: "running commands", StartedAt: now.Add(-10 * time.Minute)},
			[]string{"running commands...", "(44 tools)", "last: Bash(git status)"},
		},
		{
			"no label",
			&AgentToolStats{TotalTools: 12, LastTool: "Read", StartedAt: now.Add(-2 * time.Minute)},
			[]string{"working", "(12 tools)", "last: Read"},
		},
		{
			"with label no activity",
			&AgentToolStats{TotalTools: 5, LastTool: "Grep", LastToolLabel: "HandleToolEvent", StartedAt: now.Add(-1 * time.Minute)},
			[]string{"working", "(5 tools)", "last: Grep(HandleToolEvent)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCardStatusFromToolStats(tc.stats)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildCardStatusFromToolStats() = %q, want to contain %q", got, want)
				}
			}
		})
	}
}

func TestBuildAgentCardBlocks_WithToolStats(t *testing.T) {
	stats := &AgentToolStats{
		TotalTools:    22,
		LastTool:      "Edit",
		LastToolLabel: "main.go",
		Activity:      "editing code",
		StartedAt:     time.Now().Add(-5 * time.Minute),
	}
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", time.Now(), "", "", "", "",
		"", // contextLine
		stats,
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)
	if len(blocks) < 1 {
		t.Fatalf("expected at least 1 block, got %d", len(blocks))
	}
	header := blocks[0].(*slack.SectionBlock)
	// Status should include tool stats when working.
	if !strings.Contains(header.Text.Text, "(22 tools)") {
		t.Errorf("expected tool count in header, got: %s", header.Text.Text)
	}
	if !strings.Contains(header.Text.Text, "last: Edit(main.go)") {
		t.Errorf("expected last tool with label in header, got: %s", header.Text.Text)
	}
}

func TestBuildAgentCardBlocks_SchedulingBlockedShowsClusterCapacity(t *testing.T) {
	// ka-0000005976: when the controller surfaces pod_scheduling_blocked=true on
	// a Pending+Unschedulable pod, the agent card should swap the normal
	// "starting" hourglass for a warning + "stuck: cluster capacity" status.
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "spawning", "", time.Now(), "", "", "crew", "",
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		true,              // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)
	if len(blocks) < 1 {
		t.Fatalf("expected at least 1 block, got %d", len(blocks))
	}
	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, Garden.Glyph(LifecycleBlocked)) {
		t.Errorf("expected blocked indicator when schedulingBlocked, got: %s", header.Text.Text)
	}
	if !strings.Contains(header.Text.Text, "stuck: cluster capacity") {
		t.Errorf("expected 'stuck: cluster capacity' status, got: %s", header.Text.Text)
	}
}

func TestBuildAgentCardBlocks_SchedulingBlockedIgnoredOutsideSpawning(t *testing.T) {
	// schedulingBlocked is only meaningful while spawning — for working/done/etc.
	// the normal status takes over so a stale flag doesn't override real state.
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"", // subject
		0, "working", "", time.Now(), "", "", "crew", "",
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		true,              // schedulingBlocked (ignored — state is working)
		triggerCardInfo{}, // trigger
		nil,               // local
	)
	header := blocks[0].(*slack.SectionBlock)
	if strings.Contains(header.Text.Text, "stuck: cluster capacity") {
		t.Errorf("did not expect cluster-capacity status for working state, got: %s", header.Text.Text)
	}
}

func TestFormatWrapUpSlack_EmptyWrapUp(t *testing.T) {
	b := &Bot{}
	result := b.formatWrapUpSlack(Garden, `{}`)
	if result != "" {
		t.Errorf("expected empty string for empty wrapup, got: %q", result)
	}
}

func TestDefaultNotificationConfig_CostEnabled(t *testing.T) {
	cfg := DefaultNotificationConfig()
	if cfg.SuppressCostReporting {
		t.Error("expected SuppressCostReporting=false by default (LiteLLM provides accurate cost data)")
	}
}

// TestBuildAgentCardBlocks_ToolActivityPromotesSpawning verifies the
// self-healing card render (ka-0000005799): when the bead's agent_state is
// still "spawning" (the spawning→working SSE transition was missed/delayed)
// but live tool stats show activity, the card renders as "working" rather than
// the stale ⏳ "starting". Receiving tool events is proof the agent is running.
func TestBuildAgentCardBlocks_ToolActivityPromotesSpawning(t *testing.T) {
	stats := &AgentToolStats{TotalTools: 7, LastTool: "Bash", StartedAt: time.Now()}
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"",         // subject
		0,          // pending
		"spawning", // stale state
		"", time.Time{}, "", "", "", "",
		"", stats, "", false, triggerCardInfo{},
		nil, // local
	)
	header := blocks[0].(*slack.SectionBlock)
	text := header.Text.Text
	if strings.Contains(text, "starting") {
		t.Errorf("expected stale 'starting' to be promoted to working, got %q", text)
	}
	if !strings.Contains(text, Garden.Glyph(LifecycleWorking)+" working") {
		t.Errorf("expected working indicator glued to its label, got %q", text)
	}
}

// TestBuildAgentCardBlocks_NoToolsStaysStarting verifies the promotion is gated
// on actual tool activity: a spawning agent with no tool events yet correctly
// shows ⏳ "starting".
func TestBuildAgentCardBlocks_NoToolsStaysStarting(t *testing.T) {
	for _, stats := range []*AgentToolStats{nil, {TotalTools: 0}} {
		blocks := buildAgentCardBlocks(Garden,
			"gardenio/crew/test-bot",
			"", 0, "spawning",
			"", time.Time{}, "", "", "", "",
			"", stats, "", false, triggerCardInfo{},
			nil, // local
		)
		header := blocks[0].(*slack.SectionBlock)
		text := header.Text.Text
		if !strings.Contains(text, "starting") {
			t.Errorf("expected 'starting' for spawning with no tools, got %q", text)
		}
	}
}

// TestBuildCompactWorkingCardBlocks_ToolActivityPromotesSpawning verifies the
// same self-healing behavior on the compact thread-card render path.
func TestBuildCompactWorkingCardBlocks_ToolActivityPromotesSpawning(t *testing.T) {
	stats := &AgentToolStats{TotalTools: 3, LastTool: "Read", StartedAt: time.Now()}
	blocks := buildCompactWorkingCardBlocks(Garden,
		"gardenio/crew/test-bot", "", "spawning", "", "crew", "", stats,
	)
	header := blocks[0].(*slack.SectionBlock)
	text := header.Text.Text
	if strings.Contains(text, "starting") {
		t.Errorf("expected stale 'starting' to be promoted to working, got %q", text)
	}
	if !strings.Contains(text, Garden.Glyph(LifecycleWorking)+" working") {
		t.Errorf("expected working indicator glued to its label, got %q", text)
	}
}

// TestEffectiveAgentState_OnlyPromotesSpawning verifies the helper never
// overrides terminal or paused states even when tool stats are present.
func TestEffectiveAgentState_OnlyPromotesSpawning(t *testing.T) {
	stats := &AgentToolStats{TotalTools: 5}
	cases := []struct{ in, want string }{
		{"spawning", "working"},
		{"working", "working"},
		{"done", "done"},
		{"failed", "failed"},
		{"rate_limited", "rate_limited"},
		{"idle", "idle"},
	}
	for _, tc := range cases {
		if got := effectiveAgentState(tc.in, stats); got != tc.want {
			t.Errorf("effectiveAgentState(%q, active) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// No stats → no promotion.
	if got := effectiveAgentState("spawning", nil); got != "spawning" {
		t.Errorf("effectiveAgentState(spawning, nil) = %q, want spawning", got)
	}
}

// TestEffectiveAgentState_PromotesOnLiveBeforeTools verifies the pre-first-tool
// self-heal (ka-0000003479): a session that is Live (SessionStart hook fired)
// but has invoked zero tools still promotes a stale startup state to working,
// because the agent process is demonstrably alive even before its first tool.
func TestEffectiveAgentState_PromotesOnLiveBeforeTools(t *testing.T) {
	live := &AgentToolStats{TotalTools: 0, Live: true}

	// Live with no tools promotes both startup states.
	for _, in := range []string{"spawning", "assigning"} {
		if got := effectiveAgentState(in, live); got != "working" {
			t.Errorf("effectiveAgentState(%q, live) = %q, want working", in, got)
		}
	}

	// Live must NOT override terminal/paused/idle states — same guard as tools.
	for _, in := range []string{"done", "failed", "rate_limited", "idle", "paused"} {
		if got := effectiveAgentState(in, live); got != in {
			t.Errorf("effectiveAgentState(%q, live) = %q, want %q (no override)", in, got, in)
		}
	}

	// Not yet Live and no tools → stays spawning (the bug window before the fix:
	// pod Running but neither SessionStart nor a tool observed yet).
	notLive := &AgentToolStats{TotalTools: 0, Live: false}
	if got := effectiveAgentState("spawning", notLive); got != "spawning" {
		t.Errorf("effectiveAgentState(spawning, notLive/0 tools) = %q, want spawning", got)
	}
}

// TestHandleSessionStart_MarksLiveAndRefreshes verifies HandleSessionStart sets
// the session live flag (so GetSessionStats reports Live=true) and triggers a
// card refresh — the event-driven promotion path. ka-0000003479.
func TestHandleSessionStart_MarksLiveAndRefreshes(t *testing.T) {
	m := NewAgentStreamManager(nil, slog.Default())
	const agent = "thread-abc-123"

	// A card-only session exists (no Slack streaming API calls), mirroring a
	// thread-bound agent whose card was posted at spawn.
	if !m.StartCardSession(agent, "C123", "1700000000.1") {
		t.Fatal("StartCardSession returned false; expected a new session")
	}

	// Before SessionStart: not live, zero tools → card would render "starting".
	if st := m.GetSessionStats(agent); st == nil || st.Live {
		t.Fatalf("pre-SessionStart: got %+v, want non-nil stats with Live=false", st)
	}

	refresher := &countingCardRefresher{}
	m.SetAgentCardRefresher(refresher)

	m.HandleSessionStart(context.Background(), agent, "startup")

	st := m.GetSessionStats(agent)
	if st == nil || !st.Live {
		t.Fatalf("post-SessionStart: got %+v, want Live=true", st)
	}
	if refresher.count == 0 {
		t.Error("HandleSessionStart did not refresh the card")
	}
	// effectiveAgentState now promotes the still-spawning bead to working.
	if got := effectiveAgentState("spawning", st); got != "working" {
		t.Errorf("effectiveAgentState(spawning, post-SessionStart) = %q, want working", got)
	}
}

// countingCardRefresher records how many times RefreshAgentCard was called.
type countingCardRefresher struct{ count int }

func (c *countingCardRefresher) RefreshAgentCard(context.Context, string) { c.count++ }

// TestTerminalCardState verifies the prune-path fallback maps a closed agent
// bead's persisted agent_state / close_reason to an honest card state, instead
// of the old `state==""→"done"` default that painted reaped / rate_limited /
// podless_leak / orphan / context_cancelled cycles a false ✅ (ka-0000005513).
func TestTerminalCardState(t *testing.T) {
	cases := []struct {
		name        string
		agentState  string
		closeReason string
		want        string
	}{
		// Genuine completions stay ✅ "done".
		{"clean done", "done", "stop_gate_max_turns: claude exited without gb done", "done"},
		{"benign orphan (source task finished)", "", "orphan: source task already closed", "done"},
		{"benign orphan with done state", "done", "orphan: terminal state done", "done"},
		{"no signal at all", "", "", "done"},

		// Reaped / abnormal closes must NOT render ✅.
		{"podless_leak rate_limited", "rate_limited", "podless_leak: agent_state=rate_limited but no pod found", "rate_limited"},
		{"podless_leak lost state", "", "podless_leak: agent_state=rate_limited but no pod found", "reaped"},
		{"stale reap", "", "stale: stuck spawning for 30m", "reaped"},
		{"context_cancelled lost state", "", "context_cancelled", "reaped"},
		{"idle_ttl_expired", "", "idle_ttl_expired", "reaped"},
		{"orphan non-source-task", "", "orphan: decision parent closed", "reaped"},
		{"explicit failed", "failed", "stale: liveness timeout", "failed"},
		{"rate_limited preserved", "rate_limited", "", "rate_limited"},
	}
	for _, tc := range cases {
		if got := terminalCardState(tc.agentState, tc.closeReason); got != tc.want {
			t.Errorf("terminalCardState(%q, %q) = %q, want %q",
				tc.agentState, tc.closeReason, got, tc.want)
		}
	}
}

// TestBuildCompactAgentCardBlocks_ReapedNotGreen verifies a reaped /
// rate_limited compact card renders a non-✅ glyph + accurate label, so a
// killed cycle no longer lies about its outcome (ka-0000005513).
func TestBuildCompactAgentCardBlocks_ReapedNotGreen(t *testing.T) {
	cases := []struct {
		state          string
		wantGlyph      string
		wantLabel      string
		forbiddenGlyph string
	}{
		{"reaped", Garden.Glyph(LifecycleReaped), "reaped", Garden.Glyph(LifecycleDone)},
		{"rate_limited", Garden.Glyph(LifecycleBlocked), "rate limited", Garden.Glyph(LifecycleDone)},
		{"done", Garden.Glyph(LifecycleDone), "agent finished", Garden.Glyph(LifecycleReaped)},
	}
	for _, tc := range cases {
		blocks := buildCompactAgentCardBlocks(Garden, "gardenio/gardener/test-bot", tc.state, "", nil)
		section := blocks[0].(*slack.SectionBlock)
		text := section.Text.Text
		if !strings.Contains(text, tc.wantGlyph) {
			t.Errorf("state %q: expected glyph %q in card, got: %s", tc.state, tc.wantGlyph, text)
		}
		if !strings.Contains(text, tc.wantLabel) {
			t.Errorf("state %q: expected label %q in card, got: %s", tc.state, tc.wantLabel, text)
		}
		if strings.Contains(text, tc.forbiddenGlyph) {
			t.Errorf("state %q: card must NOT contain %q, got: %s", tc.state, tc.forbiddenGlyph, text)
		}
	}
}

// TestBuildCompactAgentCardBlocks_HoldCanary verifies that a HOLD canary — an
// agent designed never to wrap up — renders a neutral "no wrap-up expected"
// glyph instead of the alarming :headstone:/:x: stigma when reaped/failed, so
// real died-without-wrap-up failures stand out on the dashboard (ka-0000007028).
func TestBuildCompactAgentCardBlocks_HoldCanary(t *testing.T) {
	neutralGlyph := Garden.Semantic(SemIdle)
	const neutralLabel = "no wrap-up expected"

	// Each opt-out marker independently qualifies a bead as a hold canary.
	markers := []struct {
		name   string
		fields map[string]string
	}{
		{"wrapup_optout", map[string]string{"wrapup_optout": "true"}},
		{"role gvisor-poc", map[string]string{"role": "gvisor-poc"}},
		{"prompt marker", map[string]string{"prompt": "Spin up a HOLD canary. Do NOT call gb stop. Just hold."}},
	}
	// The stigma states that get suppressed for an opt-out canary.
	suppressedStates := []string{"reaped", "failed"}

	for _, m := range markers {
		for _, state := range suppressedStates {
			blocks := buildCompactAgentCardBlocks(Garden, "gardenio/gvisor-poc/canary", state, "", m.fields)
			text := blocks[0].(*slack.SectionBlock).Text.Text
			if !strings.Contains(text, neutralGlyph) {
				t.Errorf("%s/%s: expected neutral glyph %q, got: %s", m.name, state, neutralGlyph, text)
			}
			if !strings.Contains(text, neutralLabel) {
				t.Errorf("%s/%s: expected label %q, got: %s", m.name, state, neutralLabel, text)
			}
			for _, stigma := range []string{Garden.Glyph(LifecycleReaped), Garden.Glyph(LifecycleAborted)} {
				if strings.Contains(text, stigma) {
					t.Errorf("%s/%s: hold-canary card must NOT contain stigma glyph %q, got: %s", m.name, state, stigma, text)
				}
			}
		}
	}

	// A non-canary agent keeps the honest stigma glyphs.
	reaped := buildCompactAgentCardBlocks(Garden, "gardenio/crew/worker", "reaped", "", map[string]string{"role": "crew"})
	if text := reaped[0].(*slack.SectionBlock).Text.Text; !strings.Contains(text, Garden.Glyph(LifecycleReaped)) {
		t.Errorf("non-canary reaped agent must keep the reaped glyph, got: %s", text)
	}

	// A hold canary that genuinely finished clean keeps its honest ✅ — the
	// suppression only covers no-wrap-up stigma states, not real completions.
	done := buildCompactAgentCardBlocks(Garden, "gardenio/gvisor-poc/canary", "done", "", map[string]string{"role": "gvisor-poc"})
	if text := done[0].(*slack.SectionBlock).Text.Text; !strings.Contains(text, Garden.Glyph(LifecycleDone)) {
		t.Errorf("hold-canary done card must keep the done glyph, got: %s", text)
	}
}

// TestWrapUpHeaderNeverExceedsSlackCap is the regression guard for ka-0000006181.
// Slack rejects a header block whose text exceeds 150 chars, and the failure mode
// is brutal: the ENTIRE chat.update fails with `invalid_blocks`, so the card keeps
// its stale working-state form and the later prune pass compacts it to a bare
// "agent finished" line. Every gardener-style agent hit this, because declaring
// restart_when yields the "progressing" outcome whose :hourglass_flowing_sand:
// glyph was unmapped by outcomeHeaderGlyph and passed through as a 24-char
// shortcode literal — 24 + 1 + 140 = 165. Assert the composed header fits for
// EVERY outcome the classifier can produce, at maximum tldr length.
func TestWrapUpHeaderNeverExceedsSlackCap(t *testing.T) {
	longTLDR := strings.Repeat("x", 500)
	// One wrap-up per branch of wrapUpOutcome, plus the platform-failure override.
	cases := []struct {
		name   string
		state  string
		wrapup string
		fields map[string]string
	}{
		{"done", "done", `{"tldr":"` + longTLDR + `"}`, nil},
		{"blocked", "done", `{"tldr":"` + longTLDR + `","blockers":"stuck on X"}`, nil},
		{"progressing/restart_when", "done", `{"tldr":"` + longTLDR + `","restart_when":"when gd-X merges"}`, nil},
		{"progressing/goal_satisfied", "done", `{"tldr":"` + longTLDR + `","goal_satisfied":"no"}`, nil},
		{"progressing/respawn_after", "done", `{"tldr":"` + longTLDR + `","respawn_after":"10m"}`, nil},
		{"continued", "done", `{"tldr":"` + longTLDR + `","successor":"new"}`, nil},
		{"handed-off", "done", `{"tldr":"` + longTLDR + `","handoff_notes":"see bead"}`, nil},
		{"aborted", "failed", `{"tldr":"` + longTLDR + `"}`, nil},
		{"platform failure", "done", `{}`, map[string]string{
			"total_request_count": "1", "total_input_tokens": "0", "total_tool_calls": "0",
		}},
		{"close_reason fallback", "done", `{}`, map[string]string{"close_reason": "stop_gate_missing_wrapup"}},
	}

	b := &Bot{}
	for _, tc := range cases {
		blocks := b.buildWrapUpAgentCardBlocks(Garden, "gardenio-some-quite-long-agent-name-abc", tc.state, tc.wrapup, "$1.23", 0, tc.fields)
		hdr, ok := blocks[0].(*slack.HeaderBlock)
		if !ok {
			t.Fatalf("%s: blocks[0] = %T, want *slack.HeaderBlock", tc.name, blocks[0])
		}
		text := hdr.Text.Text
		if n := len([]rune(text)); n > slackHeaderTextCap {
			t.Errorf("%s: header is %d runes (> %d cap) — Slack would reject the whole update with invalid_blocks: %q",
				tc.name, n, slackHeaderTextCap, text)
		}
		if !utf8.ValidString(text) {
			t.Errorf("%s: header is not valid UTF-8 (byte-sliced mid-rune): %q", tc.name, text)
		}
		// The glyph must be substituted, never left as a raw `:shortcode:` — an
		// unmapped shortcode is both ugly and what overflowed the cap.
		if strings.HasPrefix(text, ":") {
			t.Errorf("%s: header leads with an unmapped emoji shortcode: %q", tc.name, text)
		}
	}
}

// TestTruncateRunes_NeverSplitsARune guards the second invalid_blocks source:
// truncateText slices BYTES, so clipping a tldr containing multi-byte runes
// (em-dashes and emoji are common in agent wrap-ups) could emit invalid UTF-8,
// which Slack also rejects.
func TestTruncateRunes_NeverSplitsARune(t *testing.T) {
	s := strings.Repeat("—", 50) // 3 bytes per rune
	for n := 1; n <= 60; n++ {
		got := truncateRunes(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateRunes(%d) produced invalid UTF-8: %q", n, got)
		}
		if r := len([]rune(got)); r > n {
			t.Fatalf("truncateRunes(%d) returned %d runes", n, r)
		}
	}
	if got := truncateRunes("short", 50); got != "short" {
		t.Errorf("under-cap input must pass through unchanged, got %q", got)
	}
}

// TestFormatWrapUpSlack_TLDRHeadlineBusinessTheme: Business renders the featured
// tldr headline behind the [NOTE] tag instead of a bare :speech_balloon:.
func TestFormatWrapUpSlack_TLDRHeadlineBusinessTheme(t *testing.T) {
	b := &Bot{}
	result := b.formatWrapUpSlack(Business, `{"tldr":"One-line summary"}`)
	if !strings.Contains(result, "[NOTE] *One-line summary*") {
		t.Errorf("expected Business tldr headline, got: %s", result)
	}
	if strings.Contains(result, ":speech_balloon:") {
		t.Errorf("Business headline leaked a legacy shortcode: %s", result)
	}
}

// TestBuildCompactAgentCardBlocks_HoldCanaryBusinessTheme: the idle indicator on
// a hold-canary card resolves through the theme, and keeps its verbal label.
func TestBuildCompactAgentCardBlocks_HoldCanaryBusinessTheme(t *testing.T) {
	blocks := buildCompactAgentCardBlocks(Business, "gardenio/gvisor-poc/canary", "reaped", "", map[string]string{"role": "gvisor-poc"})
	text := blocks[0].(*slack.SectionBlock).Text.Text
	if !strings.HasPrefix(text, "[IDLE] hold canary · no wrap-up expected") {
		t.Errorf("expected Business idle tag glued to the canary label, got: %s", text)
	}
}

// TestOutcomeHeaderGlyph_ThemesLegacyShortcodes: a retired outcome shortcode
// resolves to the active theme's lifecycle glyph, never passing through, in
// both themes; an already-themed glyph passes through unchanged.
func TestOutcomeHeaderGlyph_ThemesLegacyShortcodes(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		for shortcode, state := range legacyOutcomeShortcodes {
			if got, want := outcomeHeaderGlyph(th, shortcode), th.Glyph(state); got != want || got == "" {
				t.Errorf("%s: outcomeHeaderGlyph(%q) = %q, want %q", th.Name, shortcode, got, want)
			}
		}
		g := th.Glyph(LifecycleDone)
		if got := outcomeHeaderGlyph(th, g); got != g {
			t.Errorf("%s: themed glyph %q must pass through, got %q", th.Name, g, got)
		}
	}
}
