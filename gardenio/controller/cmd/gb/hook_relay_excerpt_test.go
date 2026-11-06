package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// tool_response_excerpt (gd-YaYOmNpbnW): a PostToolUse response cut at 1024 bytes keeps the lines past the cut that
// say why the call went wrong, and its last line, so gb session sweep still sees a refusal printed after the cut.

func postToolUse(tool string, resp any) *hookRelayEvent {
	evt, _, _ := buildRelayEvent(map[string]any{"hook_event_name": "PostToolUse", "session_id": "s", "tool_name": tool,
		"tool_input": map[string]any{"command": "x"}, "tool_response": resp}, "w", "gd-a")
	return evt
}

func bashOut(stdout string) map[string]any {
	return map[string]any{"stdout": stdout, "stderr": "", "interrupted": false, "isImage": false}
}

func TestToolResponseExcerpt_KeepsRefusalsPastTheCut(t *testing.T) {
	table := strings.Repeat("replicaset.apps/public-edge-55b867d5f5   0   0   0   7d21h   nginx\n", 30)
	forbidden := `Error from server (Forbidden): ingresses.networking.k8s.io is forbidden: User "system:serviceaccount:gardenio:gardenio-infra-reader" cannot list resource "ingresses" in API group "networking.k8s.io"`
	accessLog := strings.Repeat(`54.227.61.18 - - [29/Oct/2026:16:08:57 +0000] "GET /garden/c/unread HTTP/1.1" 401 172 "" "uptime-probe/1" "-"`+"\n", 20)
	last := "run 40817571440 cannot be rerun; Resource not accessible by personal access token"
	evt := postToolUse("Bash", bashOut(table+forbidden+"\n---\n"+accessLog+last+"\n"))

	if _, cut := evt.ToolResponse.(string); !cut {
		t.Fatalf("tool_response should be cut, is %T", evt.ToolResponse)
	}
	if want := forbidden + "\n" + last; evt.ToolResponseExcerpt != want {
		t.Errorf("tool_response_excerpt = %q, want %q", evt.ToolResponseExcerpt, want)
	}

	// The sweep reads it off the bus line.
	raw, _ := json.Marshal(evt)
	var ev sweepHookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if resp := sweepResponseText(ev); !strings.Contains(resp, sweepPATNotAccessible) || !sweepInfraDenied.MatchString(resp) {
		t.Errorf("sweep text lacks the refusals: %.300q", resp)
	}
}

func TestToolResponseExcerpt_OnlyWhenCut(t *testing.T) {
	for _, c := range []struct {
		name string
		tool string
		resp any
	}{
		{"short Bash response", "Bash", bashOut("Error: nope\nlast line\n")},
		{"Read of source text", "Read", strings.Repeat("\treturn fmt.Errorf(\"read failed: %w\", err)\n", 60)},
		{"Grep", "Grep", strings.Repeat("x.go:1: permission denied\n", 60)},
		{"no response", "Bash", nil},
	} {
		if got := postToolUse(c.tool, c.resp).ToolResponseExcerpt; got != "" {
			t.Errorf("%s: tool_response_excerpt = %.120q, want none", c.name, got)
		}
	}
}

func TestToolResponseExcerpt_SkipsTheHeadAndRepeats(t *testing.T) {
	// The first error is inside the head; the second repeats past the cut and is kept once.
	out := "fatal: early\n" + strings.Repeat("ok\n", 400) + "Error: late\n" + strings.Repeat("ok\n", 10) + "Error: late\ndone\n"
	if got, want := postToolUse("Bash", bashOut(out)).ToolResponseExcerpt, "Error: late\ndone"; got != want {
		t.Errorf("tool_response_excerpt = %q, want %q", got, want)
	}

	// A plain string response (an MCP tool) is read the same way.
	if got, want := postToolUse("mcp__x__y", strings.Repeat("row\n", 400)+"HTTP 403: forbidden\n").ToolResponseExcerpt, "HTTP 403: forbidden"; got != want {
		t.Errorf("string response: tool_response_excerpt = %q, want %q", got, want)
	}
}

func TestToolResponseExcerpt_CapAndRedact(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("ok\n", 400))
	for i := 0; i < 50; i++ {
		b.WriteString("error: step failed " + strings.Repeat("z", 200) + "\n")
	}
	b.WriteString("Error: " + strings.Repeat("y", 600) + "\n")
	b.WriteString("final: token=hunter2abcdef\n")
	got := postToolUse("Bash", bashOut(b.String())).ToolResponseExcerpt
	if len(got) > toolResponseExcerptMax {
		t.Errorf("tool_response_excerpt is %d bytes, want at most %d", len(got), toolResponseExcerptMax)
	}
	for _, l := range strings.Split(got, "\n") {
		if len(l) > toolResponseExcerptLineMax {
			t.Errorf("a line is %d bytes, want at most %d", len(l), toolResponseExcerptLineMax)
		}
	}
	// The last line is kept ahead of the error lines, and redacted.
	if !strings.HasSuffix(got, "\nfinal: token=[REDACTED]") || strings.Contains(got, "hunter2") {
		t.Errorf("tool_response_excerpt should end with the redacted last line: %.80q...%q", got, got[max(0, len(got)-60):])
	}
	if !strings.HasPrefix(got, "error: step failed") {
		t.Errorf("tool_response_excerpt should start with the first error past the cut: %.80q", got)
	}
}
