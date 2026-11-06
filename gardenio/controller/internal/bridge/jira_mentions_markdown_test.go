package bridge

// Tests for the H3 inbound ADF->markdown path (epic ka-0000007401): a
// @gardenio mention comment reaches the spawned agent's instruction as markdown
// (fenced code, inline code, links, lists, headings, bold/italic) instead of
// the flattened plain text adfToPlainText produced. Detection still runs on
// plain text; only the *instruction body* handed to the agent gains formatting.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestJiraCommentBodyToMarkdown covers the body->markdown helper against
// representative ADF fixtures — each construct the flattening previously lost.
func TestJiraCommentBodyToMarkdown(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "empty",
			body: ``,
			want: "",
		},
		{
			name: "jira server plain string is returned verbatim",
			body: `"@gardenio just fix it"`,
			want: "@gardenio just fix it",
		},
		{
			name: "fenced code block keeps language and body",
			body: `{"version":1,"type":"doc","content":[
				{"type":"paragraph","content":[{"type":"text","text":"run this:"}]},
				{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"fmt.Println(\"hi\")"}]}
			]}`,
			want: "run this:\n\n```go\nfmt.Println(\"hi\")\n```",
		},
		{
			name: "inline code, bold, italic and link marks",
			body: `{"version":1,"type":"doc","content":[
				{"type":"paragraph","content":[
					{"type":"text","text":"call "},
					{"type":"text","text":"doThing()","marks":[{"type":"code"}]},
					{"type":"text","text":" then read the "},
					{"type":"text","text":"runbook","marks":[{"type":"link","attrs":{"href":"https://example.com/rb"}}]},
					{"type":"text","text":" — this is "},
					{"type":"text","text":"urgent","marks":[{"type":"strong"}]},
					{"type":"text","text":" and "},
					{"type":"text","text":"time-sensitive","marks":[{"type":"em"}]}
				]}
			]}`,
			want: "call `doThing()` then read the [runbook](https://example.com/rb) — this is **urgent** and _time-sensitive_",
		},
		{
			name: "bullet list",
			body: `{"version":1,"type":"doc","content":[
				{"type":"bulletList","content":[
					{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]}]},
					{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}
				]}
			]}`,
			want: "- first\n- second",
		},
		{
			name: "heading levels",
			body: `{"version":1,"type":"doc","content":[
				{"type":"heading","attrs":{"level":3},"content":[{"type":"text","text":"Context"}]},
				{"type":"paragraph","content":[{"type":"text","text":"body"}]}
			]}`,
			want: "### Context\n\nbody",
		},
		{
			name: "mention node survives into markdown",
			body: `{"version":1,"type":"doc","content":[
				{"type":"paragraph","content":[
					{"type":"mention","attrs":{"id":"acc-1","text":"@gardenio"}},
					{"type":"text","text":" ship it"}
				]}
			]}`,
			want: "@gardenio ship it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jiraCommentBodyToMarkdown(json.RawMessage(tt.body))
			if got != tt.want {
				t.Errorf("jiraCommentBodyToMarkdown():\n  got:  %q\n  want: %q", got, tt.want)
			}
		})
	}
}

// TestJiraMentionHandler_Check_InstructionMarkdown asserts that detection still
// runs on plain text but the result carries a markdown instruction preserving
// the comment's formatting.
func TestJiraMentionHandler_Check_InstructionMarkdown(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	body := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{"type":"paragraph","content":[
				{"type":"text","text":"@gardenio reproduce with "},
				{"type":"text","text":"go test ./...","marks":[{"type":"code"}]},
				{"type":"text","text":" then patch"}
			]},
			{"type":"codeBlock","attrs":{"language":"sh"},"content":[{"type":"text","text":"git bisect start"}]}
		]
	}`)

	result := h.Check(body)
	if !result.Detected {
		t.Fatal("expected mention to be detected")
	}
	// Plain-text instruction stays flat (drives the title / dedup).
	if strings.Contains(result.Instruction, "`") || strings.Contains(result.Instruction, "```") {
		t.Errorf("plain instruction should carry no markdown, got %q", result.Instruction)
	}
	// Markdown instruction preserves inline code and the fenced block.
	if !strings.Contains(result.InstructionMarkdown, "`go test ./...`") {
		t.Errorf("markdown instruction missing inline code, got %q", result.InstructionMarkdown)
	}
	if !strings.Contains(result.InstructionMarkdown, "```sh\ngit bisect start\n```") {
		t.Errorf("markdown instruction missing fenced code block, got %q", result.InstructionMarkdown)
	}
	// The trigger token itself is stripped from both instruction forms.
	if strings.Contains(result.InstructionMarkdown, "@gardenio") {
		t.Errorf("trigger should be stripped from markdown instruction, got %q", result.InstructionMarkdown)
	}
}

// TestJiraMentionHandler_Check_MentionNodeTriggerMarkdown covers the @gardenio
// being an ADF mention node (Jira Cloud's usual shape) rather than literal
// text: the node must survive into markdown so the instruction after it can be
// extracted with formatting intact.
func TestJiraMentionHandler_Check_MentionNodeTriggerMarkdown(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	body := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{"type":"paragraph","content":[
				{"type":"mention","attrs":{"id":"acc-9","text":"@gardenio"}},
				{"type":"text","text":" see the "},
				{"type":"text","text":"dashboard","marks":[{"type":"link","attrs":{"href":"https://grafana.example/d/1"}}]}
			]}
		]
	}`)

	result := h.Check(body)
	if !result.Detected {
		t.Fatal("expected mention to be detected")
	}
	want := "see the [dashboard](https://grafana.example/d/1)"
	if result.InstructionMarkdown != want {
		t.Errorf("markdown instruction = %q, want %q", result.InstructionMarkdown, want)
	}
}

// TestJiraMentionHandler_Check_PlainTextNoMarkdown confirms a Jira Server
// plain-text body yields no separate markdown instruction (nothing to
// preserve) — the caller falls back to Instruction.
func TestJiraMentionHandler_Check_PlainTextNoMarkdown(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	result := h.Check(json.RawMessage(`"@gardenio bump the timeout"`))
	if !result.Detected {
		t.Fatal("expected mention to be detected")
	}
	if result.Instruction != "bump the timeout" {
		t.Errorf("instruction = %q", result.Instruction)
	}
	// Plain body: markdown render equals the plain text, so the extracted
	// markdown instruction matches Instruction (no formatting was lost).
	if result.InstructionMarkdown != "bump the timeout" {
		t.Errorf("markdown instruction = %q, want %q", result.InstructionMarkdown, "bump the timeout")
	}
}

// TestSpawnJiraMentionAgent_MarkdownInstructionInBeadAndPrompt is an
// end-to-end-ish check that the markdown instruction reaches both the task
// bead description and the spawned agent's prompt, with a plain-text fallback
// when no markdown was rendered.
func TestSpawnJiraMentionAgent_MarkdownInstructionInBeadAndPrompt(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action":  "spawn_agent",
				"project": "gardenio",
				"role":    "gardenio-crew",
				"enabled": "true",
			}},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := jiraWebhookEvent{
		Issue: &jiraIssueRef{
			Key:    "PE-4242",
			Fields: jiraWebhookIssueFields{Summary: "flaky deploy"},
		},
		Comment: &jiraComment{
			ID: "c-1",
			Body: json.RawMessage(`{
				"version":1,"type":"doc","content":[
					{"type":"paragraph","content":[
						{"type":"mention","attrs":{"id":"a","text":"@gardenio"}},
						{"type":"text","text":" run "},
						{"type":"text","text":"make test","marks":[{"type":"code"}]}
					]},
					{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"panic(\"boom\")"}]}
				]
			}`),
		},
	}
	event.Comment.Author.DisplayName = "Alice"
	event.Comment.Author.AccountID = "acct-alice"

	handled := h.HandleMention(context.Background(), event)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(daemon.created))
	}
	desc := daemon.created[0].Description
	if !strings.Contains(desc, "`make test`") {
		t.Errorf("task description missing inline code, got:\n%s", desc)
	}
	if !strings.Contains(desc, "```go\npanic(\"boom\")\n```") {
		t.Errorf("task description missing fenced code block, got:\n%s", desc)
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	prompt := daemon.spawned[0].Prompt
	if !strings.Contains(prompt, "`make test`") {
		t.Errorf("agent prompt missing inline code, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "```go\npanic(\"boom\")\n```") {
		t.Errorf("agent prompt missing fenced code block, got:\n%s", prompt)
	}
}
