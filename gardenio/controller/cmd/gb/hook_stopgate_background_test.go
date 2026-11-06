package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Coverage for the stop gate's background-work pass (gd-F91YfKwYXl). The
// fixtures in testdata/stop_gate_background are trimmed from the transcripts of
// the sessions the friction review cited (gb audit fetch): only the background
// launches, their results, the queue operations and the stop-gate blocks are
// kept, with subagent result bodies cut.
//
//	rz6p-reviewer-subagents.jsonl  gd-MCt40UZjB4: three async review subagents, then a
//	                               background `gh run list` loop; blocked at 01:29:08 and 01:29:58
//	9kv-monitor-queued.jsonl       gd-QIfW6AlvSZ: a Monitor on PR #1552 whose stream ended
//	                               0.5 s before the turn end; blocked at 09:19:40
//	ejm-monitor-ci.jsonl           gd-qYvQZnZZyD: a Monitor on PR #1549 CI, events while it
//	                               ran; blocked at 08:49:08

// transcriptUntil returns the fixture's lines whose timestamp is before at: the
// transcript as the Stop hook saw it at that moment.
func transcriptUntil(t *testing.T, name string, at time.Time) *bytes.Buffer {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "stop_gate_background", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var l struct {
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ts.Before(at) {
			out.Write(sc.Bytes())
			out.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return &out
}

func runningIDs(st stopGateBackgroundState) []string {
	ids := make([]string, 0, len(st.Running))
	for _, r := range st.Running {
		ids = append(ids, r.Kind+":"+r.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestStopGatePendingBackground_CitedSessions(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, at string
		wantRunning       []string
		wantQueued        int
	}{
		{
			name: "rz6p first block: three review subagents running", fixture: "rz6p-reviewer-subagents.jsonl",
			at:          "2026-10-29T01:29:08.708Z",
			wantRunning: []string{"subagent:aa9abed36aa1e7fd7", "subagent:aacaf590f87454e6e", "subagent:ae91ad4e133b708b8"},
		},
		{
			name: "rz6p second block: subagents plus a background CI loop", fixture: "rz6p-reviewer-subagents.jsonl",
			at:          "2026-10-29T01:29:58.837Z",
			wantRunning: []string{"command:bou0dkaxi", "subagent:aa9abed36aa1e7fd7", "subagent:aacaf590f87454e6e", "subagent:ae91ad4e133b708b8"},
		},
		{
			name: "rz6p CI loop finished, its notification still queued", fixture: "rz6p-reviewer-subagents.jsonl",
			at:          "2026-10-29T01:31:00Z",
			wantRunning: []string{"subagent:aa9abed36aa1e7fd7", "subagent:aacaf590f87454e6e", "subagent:ae91ad4e133b708b8"},
			wantQueued:  1,
		},
		{
			name: "rz6p every notification delivered: nothing pending", fixture: "rz6p-reviewer-subagents.jsonl",
			at: "2026-10-29T01:38:00Z",
		},
		{
			name: "9kv Monitor ended just before the turn end: notification queued", fixture: "9kv-monitor-queued.jsonl",
			at: "2026-10-30T09:19:40.854Z", wantQueued: 1,
		},
		{
			name: "9kv notification delivered: nothing pending", fixture: "9kv-monitor-queued.jsonl",
			at: "2026-10-30T09:21:00Z",
		},
		{
			name: "ejm block: Monitor on CI running", fixture: "ejm-monitor-ci.jsonl",
			at: "2026-10-30T08:49:08.672Z", wantRunning: []string{"monitor:bdoz78ppv"},
		},
		{
			name: "ejm Monitor events do not end the Monitor", fixture: "ejm-monitor-ci.jsonl",
			at: "2026-10-30T09:00:00Z", wantRunning: []string{"monitor:bdoz78ppv"}, wantQueued: 2,
		},
		{
			name: "ejm Monitor stream ended and delivered: nothing pending", fixture: "ejm-monitor-ci.jsonl",
			at: "2026-10-30T09:10:00Z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := mustTime(t, tc.at)
			st, err := stopGatePendingBackground(transcriptUntil(t, tc.fixture, at), at, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if got := runningIDs(st); strings.Join(got, ",") != strings.Join(tc.wantRunning, ",") {
				t.Errorf("running = %v, want %v", got, tc.wantRunning)
			}
			if st.Queued != tc.wantQueued {
				t.Errorf("queued = %d, want %d", st.Queued, tc.wantQueued)
			}
			if want := len(tc.wantRunning) > 0 || tc.wantQueued > 0; st.Pending() != want {
				t.Errorf("Pending() = %v, want %v", st.Pending(), want)
			}
		})
	}
}

// synthetic transcript lines, in the shapes the fixtures show.
func bgAssistantToolUse(ts, id, name, input string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}`
}

func bgToolResult(ts, id, text, toolUseResult string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"content":[{"tool_use_id":"` + id + `","type":"tool_result","content":"` + text + `"}]},"toolUseResult":` + toolUseResult + `}`
}

func bgNotification(ts, op, taskID string) string {
	content := `<task-notification>\n<task-id>` + taskID + `</task-id>\n<status>completed</status>\n</task-notification>`
	return `{"type":"queue-operation","operation":"` + op + `","timestamp":"` + ts + `","content":"` + content + `"}`
}

func TestStopGatePendingBackground_Synthetic(t *testing.T) {
	launch := bgAssistantToolUse("2026-10-30T10:00:00Z", "toolu_A", "Agent", `{"description":"review","prompt":"x"}`) + "\n" +
		bgToolResult("2026-10-30T10:00:01Z", "toolu_A", "Async agent launched successfully.", `{"isAsync":true,"status":"async_launched","agentId":"agA"}`)
	now := mustTime(t, "2026-10-30T10:05:00Z")

	for _, tc := range []struct {
		name       string
		transcript string
		now        time.Time
		cliStart   time.Time
		want       bool
	}{
		{name: "running subagent", transcript: launch, now: now, want: true},
		{name: "older than the max age", transcript: launch, now: now.Add(stopGateBackgroundMaxAge), want: false},
		{name: "launched by a CLI process that has since restarted", transcript: launch, now: now,
			cliStart: mustTime(t, "2026-10-30T10:02:00Z"), want: false},
		{name: "launched by the running CLI", transcript: launch, now: now,
			cliStart: mustTime(t, "2026-10-30T09:59:00Z"), want: true},
		{name: "finished and delivered", now: now, want: false, transcript: launch + "\n" +
			bgNotification("2026-10-30T10:03:00Z", "enqueue", "agA") + "\n" + bgNotification("2026-10-30T10:03:05Z", "remove", "agA")},
		{name: "finished, delivered as a plain user message", now: now, want: false, transcript: launch + "\n" +
			`{"type":"user","timestamp":"2026-10-30T10:03:00Z","message":{"content":"<task-notification>\n<task-id>agA</task-id>\n<status>completed</status>\n</task-notification>"}}`},
		{name: "a queued entry the transcript never shows delivered goes stale", want: false,
			now: mustTime(t, "2026-10-30T10:03:00Z").Add(stopGateQueuedMaxAge + time.Minute), transcript: launch + "\n" +
				bgNotification("2026-10-30T10:03:00Z", "enqueue", "agA")},
		{name: "resumed with SendMessage after finishing", now: now, want: true, transcript: launch + "\n" +
			bgNotification("2026-10-30T10:01:00Z", "enqueue", "agA") + "\n" + bgNotification("2026-10-30T10:01:05Z", "remove", "agA") + "\n" +
			bgAssistantToolUse("2026-10-30T10:02:00Z", "toolu_S", "SendMessage", `{"to":"agA","message":"one more"}`) + "\n" +
			bgToolResult("2026-10-30T10:02:01Z", "toolu_S", "sent", `{"success":true}`)},
		{name: "stopped with TaskStop", now: now, want: false, transcript: launch + "\n" +
			bgAssistantToolUse("2026-10-30T10:02:00Z", "toolu_K", "TaskStop", `{"task_id":"agA"}`) + "\n" +
			bgToolResult("2026-10-30T10:02:01Z", "toolu_K", "stopped", `{"message":"Successfully stopped task: agA","task_id":"agA","task_type":"local_agent"}`)},
		{name: "stopped with TaskStop, id only in the input", now: now, want: false, transcript: launch + "\n" +
			bgAssistantToolUse("2026-10-30T10:02:00Z", "toolu_K", "TaskStop", `{"task_id":"agA"}`) + "\n" +
			bgToolResult("2026-10-30T10:02:01Z", "toolu_K", "stopped", `{}`)},
		{name: "a background command stopped with legacy KillShell", now: now, want: false, transcript: bgAssistantToolUse("2026-10-30T10:00:00Z", "toolu_B", "Bash", `{"command":"gh run watch","run_in_background":true}`) + "\n" +
			bgToolResult("2026-10-30T10:00:01Z", "toolu_B", "started", `{"backgroundTaskId":"bsh1"}`) + "\n" +
			bgAssistantToolUse("2026-10-30T10:02:00Z", "toolu_K", "KillShell", `{"shell_id":"bsh1"}`) + "\n" +
			bgToolResult("2026-10-30T10:02:01Z", "toolu_K", "killed", `{"success":true}`)},
		{name: "a failed TaskStop leaves the task running", now: now, want: true, transcript: launch + "\n" +
			bgAssistantToolUse("2026-10-30T10:02:00Z", "toolu_K", "TaskStop", `{"task_id":"agA"}`) + "\n" +
			`{"type":"user","timestamp":"2026-10-30T10:02:01Z","message":{"content":[{"tool_use_id":"toolu_K","type":"tool_result","is_error":true,"content":"no such task"}]},"toolUseResult":"Error: no such task"}`},
		{name: "a foreground Agent call is not background work", now: now, want: false, transcript: bgAssistantToolUse("2026-10-30T10:00:00Z", "toolu_F", "Agent", `{"description":"sync"}`) + "\n" +
			bgToolResult("2026-10-30T10:00:30Z", "toolu_F", "done: findings", `{"status":"completed","agentId":"agF"}`)},
		{name: "a sidechain launch belongs to a subagent transcript", now: now, want: false,
			transcript: strings.ReplaceAll(launch, `"type":"user",`, `"type":"user","isSidechain":true,`)},
		{name: "notification text inside a file read does not finish anything", now: now, want: true, transcript: launch + "\n" +
			bgAssistantToolUse("2026-10-30T10:02:00Z", "toolu_R", "Read", `{"file_path":"x"}`) + "\n" +
			bgToolResult("2026-10-30T10:02:01Z", "toolu_R", "<task-notification><task-id>agA</task-id><status>completed</status></task-notification>", `{}`)},
		{name: "a malformed line is skipped", now: now, want: true, transcript: "{not json\n" + launch},
		{name: "empty transcript", now: now, want: false, transcript: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := stopGatePendingBackground(strings.NewReader(tc.transcript), tc.now, tc.cliStart)
			if err != nil {
				t.Fatal(err)
			}
			if st.Pending() != tc.want {
				t.Errorf("Pending() = %v (%s), want %v", st.Pending(), st, tc.want)
			}
		})
	}
}

func TestStopGateBackgroundWork_ReadsTranscriptPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	ts := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	transcript := bgAssistantToolUse(ts, "toolu_M", "Monitor", `{"description":"ci on PR 1"}`) + "\n" +
		bgToolResult(ts, "toolu_M", "Monitor started", `{"taskId":"bmon1","timeoutMs":1800000,"persistent":false}`) + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	// The test process is not under a Claude CLI, so stopGateCLIStartTime is zero.
	if got := stopGateBackgroundWork(map[string]any{"transcript_path": path}); !got.Pending() || got.String() != "1 monitor(s) (ci on PR 1)" {
		t.Errorf("stopGateBackgroundWork = %+v (%q), want one running monitor", got, got.String())
	}
	for name, in := range map[string]map[string]any{
		"no transcript_path": {},
		"missing file":       {"transcript_path": filepath.Join(dir, "gone.jsonl")},
	} {
		if got := stopGateBackgroundWork(in); got.Pending() {
			t.Errorf("%s: Pending() = true, want false (fail closed)", name)
		}
	}
}

func TestStopGateProcStartTime(t *testing.T) {
	root := useProcFixture(t)
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu  1 2 3\nbtime 1793300000\nprocesses 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// starttime (field 22) = 12345 ticks = 123.45 s after boot.
	fields := []string{"S", "1"}
	for i := 0; i < 17; i++ {
		fields = append(fields, "0")
	}
	fields = append(fields, "12345", "0")
	if err := os.MkdirAll(filepath.Join(root, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "42", "stat"), []byte("42 (claude (x)) "+strings.Join(fields, " ")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1793300000, 0).Add(123450 * time.Millisecond)
	if got := stopGateProcStartTime(42); !got.Equal(want) {
		t.Errorf("stopGateProcStartTime = %v, want %v", got, want)
	}
	if got := stopGateProcStartTime(43); !got.IsZero() {
		t.Errorf("missing pid: got %v, want zero", got)
	}
}

func TestStopGateWaitHint(t *testing.T) {
	agent := func(state string) map[string]map[string]any {
		return map[string]map[string]any{"gd-AGENT01": ciWaitAgentBead("gd-AGENT01", state, "gd-WORK01")}
	}

	t.Run("pending CI wait: says gb stop, on every block", func(t *testing.T) {
		parkFixture(t, nil, nil, agent(beadsapi.CIWaitStatePending))
		for _, n := range []int{1, 3} {
			if got := stopGateWaitHint(context.Background(), "gd-AGENT01", n); !strings.Contains(got, "is recorded and still pending") || !strings.Contains(got, "gb stop") {
				t.Errorf("block %d: hint = %q, want the pending-wait gb stop hint", n, got)
			}
		}
	})
	t.Run("no pending wait: the park recipe on the first block only", func(t *testing.T) {
		parkFixture(t, nil, nil, agent(beadsapi.CIWaitStateConsumed))
		if got := stopGateWaitHint(context.Background(), "gd-AGENT01", 1); !strings.Contains(got, "gb wait ci --repo") {
			t.Errorf("first block: hint = %q, want the gb wait ci recipe", got)
		}
		if got := stopGateWaitHint(context.Background(), "gd-AGENT01", 2); got != "" {
			t.Errorf("second block: hint = %q, want none", got)
		}
	})
	t.Run("agent bead unreadable: falls back to the recipe", func(t *testing.T) {
		parkFixture(t, nil, nil, nil)
		if got := stopGateWaitHint(context.Background(), "gd-AGENT01", 1); !strings.Contains(got, "gb wait ci --repo") {
			t.Errorf("hint = %q, want the gb wait ci recipe", got)
		}
	})
}
