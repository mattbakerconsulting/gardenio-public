package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBashCmdClass(t *testing.T) {
	cases := map[string]string{
		"git status":                  "git",
		"/usr/bin/git log":            "git",
		"FOO=bar git commit -m x":     "git",
		"FOO=bar BAZ=qux /bin/ls -la": "ls",
		"":                            "bash",
		"   ":                         "bash",
		"VAR=1":                       "bash",
		"go test ./...":               "go",
	}
	for cmd, want := range cases {
		if got := bashCmdClass(cmd); got != want {
			t.Errorf("bashCmdClass(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestExtractCommandAndClass(t *testing.T) {
	bashInput, _ := json.Marshal(map[string]string{"command": "/usr/bin/git status"})
	if cmd, class := extractCommandAndClass("Bash", bashInput); cmd != "/usr/bin/git status" || class != "git" {
		t.Errorf("Bash: got cmd=%q class=%q", cmd, class)
	}

	readInput, _ := json.Marshal(map[string]string{"file_path": "/foo/bar.go"})
	if cmd, class := extractCommandAndClass("Read", readInput); cmd != "Read /foo/bar.go" || class != "Read" {
		t.Errorf("Read: got cmd=%q class=%q", cmd, class)
	}

	grepInput, _ := json.Marshal(map[string]string{"pattern": "TODO"})
	if cmd, class := extractCommandAndClass("Grep", grepInput); cmd != `Grep "TODO"` || class != "Grep" {
		t.Errorf("Grep: got cmd=%q class=%q", cmd, class)
	}

	if cmd, class := extractCommandAndClass("WebFetch", json.RawMessage(`{"url":"https://example.com"}`)); class != "WebFetch" || !strings.HasPrefix(cmd, "WebFetch ") {
		t.Errorf("WebFetch: got cmd=%q class=%q", cmd, class)
	}

	if cmd, class := extractCommandAndClass("SomeTool", nil); cmd != "SomeTool" || class != "SomeTool" {
		t.Errorf("empty input: got cmd=%q class=%q", cmd, class)
	}
}

func TestBuildToolCallRecordDuration(t *testing.T) {
	when := time.Now()

	withDur := 123.5
	rec := buildToolCallRecord(toolHookFields{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"echo hi"}`), DurationMs: &withDur}, when)
	if !rec.HasDuration || rec.DurationMs != withDur {
		t.Errorf("expected duration %v, got HasDuration=%v DurationMs=%v", withDur, rec.HasDuration, rec.DurationMs)
	}

	noDur := buildToolCallRecord(toolHookFields{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"echo hi"}`)}, when)
	if noDur.HasDuration {
		t.Errorf("expected no duration, got HasDuration=true DurationMs=%v", noDur.DurationMs)
	}
}

func TestParseBusReplayStream(t *testing.T) {
	dur := 42.0
	payload := toolHookFields{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`), DurationMs: &dur}
	payloadJSON, _ := json.Marshal(payload)
	evt := map[string]json.RawMessage{
		"type":    json.RawMessage(`"PostToolUse"`),
		"ts":      json.RawMessage(`"2026-10-10T20:00:00Z"`),
		"payload": payloadJSON,
	}
	evtJSON, _ := json.Marshal(evt)

	body := strings.NewReader(
		"id:1\nevent:hooks\ndata:" + string(evtJSON) + "\n\n" +
			":keepalive\n\n" +
			"id:2\nevent:hooks\ndata:" + `{"type":"PreToolUse","ts":"2026-10-10T20:00:01Z","payload":{}}` + "\n\n",
	)

	records, err := parseBusReplayStream(body)
	if err != nil {
		t.Fatalf("parseBusReplayStream: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record (PreToolUse filtered out), got %d", len(records))
	}
	if records[0].Tool != "Bash" || !records[0].HasDuration || records[0].DurationMs != 42.0 {
		t.Errorf("unexpected record: %+v", records[0])
	}
	if !records[0].When.Equal(time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)) {
		t.Errorf("unexpected timestamp: %v", records[0].When)
	}
}

func TestPrintToolCallsByCommandSortsSlowestFirst(t *testing.T) {
	d1, d2 := 10.0, 500.0
	records := []toolCallRecord{
		{Tool: "Bash", Command: "fast", DurationMs: d1, HasDuration: true, When: time.Now()},
		{Tool: "Bash", Command: "slow", DurationMs: d2, HasDuration: true, When: time.Now()},
		{Tool: "Bash", Command: "unknown-duration", HasDuration: false, When: time.Now()},
	}
	var buf bytes.Buffer
	printToolCalls(&buf, records, "command", 10)
	out := buf.String()

	slowIdx := strings.Index(out, "slow")
	fastIdx := strings.Index(out, "fast")
	if slowIdx == -1 || fastIdx == -1 || slowIdx > fastIdx {
		t.Errorf("expected 'slow' row before 'fast' row, got:\n%s", out)
	}
}

func TestPrintToolCallsByClassAggregates(t *testing.T) {
	records := []toolCallRecord{
		{Tool: "Bash", Class: "git", DurationMs: 100, HasDuration: true, When: time.Now()},
		{Tool: "Bash", Class: "git", DurationMs: 300, HasDuration: true, When: time.Now()},
		{Tool: "Bash", Class: "ls", DurationMs: 10, HasDuration: true, When: time.Now()},
	}
	var buf bytes.Buffer
	printToolCalls(&buf, records, "class", 10)
	out := buf.String()

	if !strings.Contains(out, "git") || !strings.Contains(out, "2") {
		t.Errorf("expected git class with count 2, got:\n%s", out)
	}
	gitIdx := strings.Index(out, "git")
	lsIdx := strings.Index(out, "ls")
	if gitIdx == -1 || lsIdx == -1 || gitIdx > lsIdx {
		t.Errorf("expected git (higher total) before ls, got:\n%s", out)
	}
}

func TestPrintToolCallsEmpty(t *testing.T) {
	var buf bytes.Buffer
	printToolCalls(&buf, nil, "command", 10)
	if !strings.Contains(buf.String(), "no tool calls") {
		t.Errorf("expected empty-window message, got: %q", buf.String())
	}
}

func TestCollectSessionToolCallsFiltersByCutoffAndEventName(t *testing.T) {
	dur := 55.0
	postPayload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]string{"command": "echo hi"},
		"duration_ms":     dur,
	})
	prePayload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
	})

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	cutoff := now.Add(-1 * time.Hour)

	lines := []string{
		mustHookLogEntry(t, old.UnixMilli(), postPayload), // too old, filtered out
		mustHookLogEntry(t, now.UnixMilli(), postPayload), // kept
		mustHookLogEntry(t, now.UnixMilli(), prePayload),  // wrong event name, filtered out
	}

	dir := t.TempDir()
	path := dir + "/hook_events.jsonl"
	if err := writeLines(path, lines); err != nil {
		t.Fatalf("writeLines: %v", err)
	}

	records, err := collectSessionToolCalls(path, cutoff)
	if err != nil {
		t.Fatalf("collectSessionToolCalls: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d: %+v", len(records), records)
	}
	if records[0].Tool != "Bash" || !records[0].HasDuration || records[0].DurationMs != dur {
		t.Errorf("unexpected record: %+v", records[0])
	}
}

func mustHookLogEntry(t *testing.T, timestampMs int64, payload json.RawMessage) string {
	t.Helper()
	entry := map[string]any{
		"timestamp_ms": timestampMs,
		"json":         payload,
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return string(b)
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
