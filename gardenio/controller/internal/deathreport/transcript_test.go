package deathreport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/deathtranscript"
)

// assistantLine builds one assistant JSONL entry with the given id, timestamp,
// text, and tool names.
func assistantLine(id, ts, text string, tools ...string) string {
	var blocks []string
	if text != "" {
		blocks = append(blocks, `{"type":"text","text":`+jsonStr(text)+`}`)
	}
	for _, name := range tools {
		blocks = append(blocks, `{"type":"tool_use","name":`+jsonStr(name)+`}`)
	}
	content := "[" + strings.Join(blocks, ",") + "]"
	return `{"type":"assistant","timestamp":` + jsonStr(ts) + `,"message":{"id":` + jsonStr(id) +
		`,"role":"assistant","stop_reason":"end_turn","content":` + content + `}}`
}

// userBlockLine builds a user entry carrying a stop-gate block injection.
func userBlockLine(ts string) string {
	return `{"type":"user","timestamp":` + jsonStr(ts) +
		`,"message":{"role":"user","content":"<system-reminder>\nSTOP BLOCKED — decision gate unsatisfied.\n</system-reminder>"}}`
}

func jsonStr(s string) string {
	// minimal JSON string encoder for test fixtures
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestParseTranscript_LastWordsAndTurns(t *testing.T) {
	lines := []string{
		assistantLine("m1", "2026-07-24T01:00:00Z", "Working on the task.", "Bash"),
		assistantLine("m2", "2026-07-24T01:00:10Z", "Still going.", "Edit", "Read"),
		assistantLine("m3", "2026-07-24T01:00:20Z", "Holding for decision gd-abc."), // quiet turn-end
	}
	tr, err := ParseTranscript(strings.NewReader(strings.Join(lines, "\n")), 10)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TotalTurns != 3 {
		t.Errorf("TotalTurns = %d, want 3", tr.TotalTurns)
	}
	if len(tr.Turns) != 3 {
		t.Fatalf("len(Turns) = %d, want 3", len(tr.Turns))
	}
	if tr.LastWords != "Holding for decision gd-abc." {
		t.Errorf("LastWords = %q", tr.LastWords)
	}
	// Last turn had no tools → quiet turn-end.
	if !tr.Turns[2].QuietTurnEnd {
		t.Error("last turn should be a quiet turn-end")
	}
	if tr.Turns[0].QuietTurnEnd {
		t.Error("first turn had a tool call, should not be quiet")
	}
	// Tool calls captured in order.
	if got := strings.Join(tr.Turns[1].ToolCalls, ","); got != "Edit,Read" {
		t.Errorf("turn 2 tools = %q, want Edit,Read", got)
	}
	// Indexes assigned oldest-first.
	for i, turn := range tr.Turns {
		if turn.Index != i {
			t.Errorf("turn[%d].Index = %d", i, turn.Index)
		}
	}
}

func TestParseTranscript_LastNWindow(t *testing.T) {
	var lines []string
	for i := 0; i < 25; i++ {
		lines = append(lines, assistantLine("m"+itoa(i), "2026-07-24T01:00:00Z", "turn "+itoa(i), "Bash"))
	}
	tr, err := ParseTranscript(strings.NewReader(strings.Join(lines, "\n")), 10)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TotalTurns != 25 {
		t.Errorf("TotalTurns = %d, want 25", tr.TotalTurns)
	}
	if len(tr.Turns) != 10 {
		t.Fatalf("len(Turns) = %d, want 10 (last-N window)", len(tr.Turns))
	}
	// Should be the LAST 10 (turns 15..24).
	if tr.Turns[0].Text != "turn 15" {
		t.Errorf("first retained turn = %q, want 'turn 15'", tr.Turns[0].Text)
	}
	if tr.LastWords != "turn 24" {
		t.Errorf("LastWords = %q, want 'turn 24'", tr.LastWords)
	}
}

func TestParseTranscript_MultiLineMessageDedup(t *testing.T) {
	// One assistant message split across three JSONL lines (Claude Code fans a
	// message across one line per content block, all sharing message.id). They
	// must collapse into ONE turn with merged text + tools.
	lines := []string{
		`{"type":"assistant","timestamp":"2026-07-24T01:00:00Z","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"part one"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-24T01:00:00Z","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-24T01:00:00Z","message":{"id":"m1","role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"part two"}]}}`,
	}
	tr, err := ParseTranscript(strings.NewReader(strings.Join(lines, "\n")), 10)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TotalTurns != 1 {
		t.Errorf("TotalTurns = %d, want 1 (deduped by message.id)", tr.TotalTurns)
	}
	if len(tr.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(tr.Turns))
	}
	if tr.Turns[0].Text != "part one\npart two" {
		t.Errorf("merged text = %q", tr.Turns[0].Text)
	}
	if got := strings.Join(tr.Turns[0].ToolCalls, ","); got != "Bash" {
		t.Errorf("merged tools = %q, want Bash", got)
	}
	if tr.Turns[0].StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use (upgraded from later line)", tr.Turns[0].StopReason)
	}
	// Had a tool call → not a quiet turn-end.
	if tr.Turns[0].QuietTurnEnd {
		t.Error("merged turn has a tool call, should not be quiet")
	}
}

func TestParseTranscript_StopGateBlocks(t *testing.T) {
	lines := []string{
		assistantLine("m1", "2026-07-24T01:00:05Z", "Holding for decision."),
		userBlockLine("2026-07-24T01:00:45Z"), // block #1
		assistantLine("m2", "2026-07-24T01:00:46Z", "Holding for decision."),
		userBlockLine("2026-07-24T01:03:42Z"), // block #2 (+177s)
		assistantLine("m3", "2026-07-24T01:03:43Z", "Holding for decision."),
		userBlockLine("2026-07-24T01:04:49Z"), // block #3 (+67s)
	}
	tr, err := ParseTranscript(strings.NewReader(strings.Join(lines, "\n")), 10)
	if err != nil {
		t.Fatal(err)
	}
	if tr.StopGateBlocks != 3 {
		t.Errorf("StopGateBlocks = %d, want 3", tr.StopGateBlocks)
	}
	if len(tr.BlockTimestamps) != 3 {
		t.Fatalf("len(BlockTimestamps) = %d, want 3", len(tr.BlockTimestamps))
	}
	// Feed the block timestamps into the cadence diff; the 3rd block (+67s vs
	// window 60 for count=2) survives, but let's assert the pipeline works.
	d := StopHookCadenceDiff(tr.BlockTimestamps, 30)
	if len(d.Events) != 3 {
		t.Errorf("cadence events = %d, want 3", len(d.Events))
	}
}

func TestParseTranscript_ToleratesMalformed(t *testing.T) {
	lines := []string{
		`{ this is not json`,
		assistantLine("m1", "2026-07-24T01:00:00Z", "hi", "Bash"),
		``,
		`{"type":"assistant","message":{"role":"assistant","content":"bare string content"}}`, // no id, string content
	}
	tr, err := ParseTranscript(strings.NewReader(strings.Join(lines, "\n")), 10)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TotalTurns != 2 {
		t.Errorf("TotalTurns = %d, want 2 (malformed + empty skipped)", tr.TotalTurns)
	}
	if tr.LastWords != "bare string content" {
		t.Errorf("LastWords = %q", tr.LastWords)
	}
}

func codexEvents() []deathtranscript.Turn {
	ts := func(s int) time.Time { return time.Unix(1760500000+int64(s), 0).UTC() }
	return []deathtranscript.Turn{
		{Type: "user", Role: "user", Text: "run the tool", Timestamp: ts(1)},
		{Type: "tool", Tool: "Bash", Command: "echo hi", Text: "[tool: Bash] echo hi", Timestamp: ts(2)},
		{Type: "assistant", Role: "assistant", Text: "first done", Timestamp: ts(4)},
		{Type: "tool", Tool: "Bash", Command: "git push origin HEAD", Text: "[tool: Bash] git push origin HEAD", Timestamp: ts(5)},
		{Type: "tool", Tool: "apply_patch", Text: "[tool: apply_patch]", Timestamp: ts(6)},
		{Type: "assistant", Role: "assistant", Text: "  all finished  ", Timestamp: ts(7)},
	}
}

func TestTranscriptFromTurns(t *testing.T) {
	tr := TranscriptFromTurns(codexEvents(), 10)

	// Tool calls before any assistant message get their own turn; later calls
	// attach to the preceding assistant message.
	if tr.TotalTurns != 3 || len(tr.Turns) != 3 {
		t.Fatalf("turns = %d/%d, want 3/3: %+v", len(tr.Turns), tr.TotalTurns, tr.Turns)
	}
	if got := tr.Turns[0]; len(got.ToolCalls) != 1 || got.Commands[0] != "echo hi" {
		t.Errorf("turn 0 = %+v", got)
	}
	if got := tr.Turns[1]; got.Text != "first done" || len(got.ToolCalls) != 2 || got.Commands[0] != "git push origin HEAD" || got.QuietTurnEnd {
		t.Errorf("turn 1 = %+v", got)
	}
	if !tr.Turns[2].QuietTurnEnd || tr.Turns[2].Index != 2 {
		t.Errorf("turn 2 = %+v", tr.Turns[2])
	}
	if tr.LastWords != "all finished" {
		t.Errorf("LastWords = %q", tr.LastWords)
	}
	if tr.LastCommand != "git push origin HEAD" {
		t.Errorf("LastCommand = %q", tr.LastCommand)
	}
	if tr.FirstTimestamp.IsZero() || !tr.LastTimestamp.After(tr.FirstTimestamp) {
		t.Errorf("bounds = %v .. %v", tr.FirstTimestamp, tr.LastTimestamp)
	}
}

func TestTranscriptFromTurns_LastN(t *testing.T) {
	tr := TranscriptFromTurns(codexEvents(), 2)
	if tr.TotalTurns != 3 || len(tr.Turns) != 2 || tr.Turns[0].Index != 0 || tr.Turns[0].Text != "first done" {
		t.Errorf("got %+v", tr)
	}
	if empty := TranscriptFromTurns(nil, 0); empty.TotalTurns != 0 || empty.LastWords != "" || empty.LastCommand != "" {
		t.Errorf("empty = %+v", empty)
	}
}

// The Slack/CLI death report for a Codex agent shows the last assistant message
// and the last command, and serializes them in the JSON form.
func TestBuild_CodexEventsRenderLastMessageAndCommand(t *testing.T) {
	rep, err := Build(nil, Options{
		AgentBeadID: "gd-codex1", AgentName: "codex-agent",
		CloseReason: "context_cancelled",
		Events:      codexEvents(),
		LastN:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Transcript == nil || rep.Cadence == nil {
		t.Fatalf("transcript/cadence missing: %+v", rep)
	}
	var out bytes.Buffer
	rep.RenderText(&out)
	text := out.String()
	for _, want := range []string{"Last words", "all finished", "Last command", "$ git push origin HEAD", "$ echo hi"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered report missing %q:\n%s", want, text)
		}
	}
	js, _ := json.Marshal(rep)
	if !strings.Contains(string(js), `"last_command":"git push origin HEAD"`) {
		t.Errorf("json lacks last_command: %s", js)
	}
}

// Events == nil keeps the Claude path: no codex-only keys leak into its JSON.
func TestBuild_ClaudeJSONUnchangedByEventFields(t *testing.T) {
	rep, err := Build(strings.NewReader(assistantLine("m1", "2026-07-24T01:00:05Z", "hello")), Options{CloseReason: "context_cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(rep)
	for _, banned := range []string{"last_command", `"commands"`} {
		if strings.Contains(string(js), banned) {
			t.Errorf("claude report JSON contains %s: %s", banned, js)
		}
	}
}
