package main

// Background-work pass for the stop gate (gd-F91YfKwYXl).
//
// A Claude Code turn can end while work the CLI itself runs is still in flight:
// an async subagent (Agent tool), a Monitor, or a run_in_background Bash
// command. The CLI re-invokes the session with a <task-notification> when each
// one finishes, so ending the turn is the right move. The decision gate used
// to block that turn end ("decision point offered before session end"), and
// agents then held the turn with a foreground `gh run watch` / `tail -f` /
// until-loop instead (session-sweep classes stop-gate-wait and idle-wait;
// gd-MCt40UZjB4, gd-QIfW6AlvSZ, gd-qYvQZnZZyD).
//
// The evidence is the session transcript (the Stop hook input's
// transcript_path): every launch is a tool_result that names its task id, and
// every finish is a <task-notification> carrying <status>. A launch with no
// finish is still running. A notification the CLI has queued but not yet
// delivered (a queue-operation enqueue with no matching remove) also means the
// session is about to be re-invoked.
//
// The pass never weakens the gate for a genuinely idle turn end. It only sees
// tasks the transcript shows were launched by the CLI process now running, and
// no task counts once it is older than stopGateBackgroundMaxAge, so a lost
// notification or a restarted CLI cannot keep the gate open.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// stopGateBackgroundMaxAge is the oldest a launch may be and still count as
// running. It matches the stale reaper (STALE_AGENT_TIMEOUT, 2h): a task the
// CLI never reported on is not evidence for longer than the fleet keeps an
// idle agent anyway.
const stopGateBackgroundMaxAge = 2 * time.Hour

// stopGateQueuedMaxAge is the oldest a queued, undelivered notification may be
// and still count. The CLI delivers its queue as soon as a turn ends, so an
// entry older than this is one whose delivery the transcript did not record.
const stopGateQueuedMaxAge = 10 * time.Minute

// Background task kinds, as the stderr line and the tests name them.
const (
	stopGateBgSubagent = "subagent"
	stopGateBgMonitor  = "monitor"
	stopGateBgCommand  = "command"
)

// stopGateBackgroundTask is one task the CLI launched and has not reported
// finished.
type stopGateBackgroundTask struct {
	ID          string // agentId, Monitor taskId or Bash backgroundTaskId
	Kind        string
	Description string
	Started     time.Time
}

// stopGateBackgroundState is what the transcript shows is still in flight.
type stopGateBackgroundState struct {
	Running []stopGateBackgroundTask
	Queued  int // notifications queued for delivery but not yet delivered
}

// Pending reports whether the session will be re-invoked without the agent
// doing anything: a task is still running or a notification is queued.
func (s stopGateBackgroundState) Pending() bool { return len(s.Running) > 0 || s.Queued > 0 }

// String is the stderr summary, e.g. "2 subagent(s) (Review PR 1382, ...), 1 queued notification(s)".
func (s stopGateBackgroundState) String() string {
	var parts []string
	byKind := map[string][]string{}
	for _, t := range s.Running {
		byKind[t.Kind] = append(byKind[t.Kind], t.Description)
	}
	for _, k := range []string{stopGateBgSubagent, stopGateBgMonitor, stopGateBgCommand} {
		if d := byKind[k]; len(d) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s(s) (%s)", len(d), k, strings.Join(d, ", ")))
		}
	}
	if s.Queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued notification(s)", s.Queued))
	}
	return strings.Join(parts, ", ")
}

// stopGateTranscriptLine is the subset of a Claude Code transcript line the
// pass reads.
type stopGateTranscriptLine struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Operation     string          `json:"operation"`
	Content       string          `json:"content"`
	Attachment    struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
	} `json:"attachment"`
}

// stopGateContentItem is one item of a message's content array.
type stopGateContentItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// stopGateToolUse is a tool call the pass may need when its result arrives.
type stopGateToolUse struct {
	name        string
	description string
	to          string // SendMessage recipient
	stopID      string // TaskStop task_id, or KillShell/KillBash shell_id
}

var (
	stopGateNotifTaskRe   = regexp.MustCompile(`<task-id>([^<\s]+)</task-id>`)
	stopGateNotifStatusRe = regexp.MustCompile(`<status>[^<]*</status>`)
	stopGateAgentIDRe     = regexp.MustCompile(`agentId: ([A-Za-z0-9_-]+)`)
)

// stopGateBackgroundTools are the tools whose results can launch, resume or
// stop background work. Lines for any other tool are skipped without decoding.
var stopGateBackgroundTools = map[string]bool{
	"Agent": true, "Task": true, "Monitor": true, "Bash": true, "SendMessage": true,
	"TaskStop": true, "KillShell": true, "KillBash": true,
}

// stopGateStopTools end a background task. A stopped task gets no
// <task-notification>, so its stop result is the only record that it finished.
var stopGateStopTools = map[string]bool{"TaskStop": true, "KillShell": true, "KillBash": true}

// stopGatePendingBackground reads a Claude Code transcript and returns what is
// still in flight at now. Launches before cliStart (the running CLI's start
// time; zero to skip the check) belong to a CLI process that has since exited,
// so they are not counted. Lines that are not JSON, sidechain lines and lines
// with no timestamp are skipped.
func stopGatePendingBackground(r io.Reader, now, cliStart time.Time) (stopGateBackgroundState, error) {
	uses := map[string]stopGateToolUse{}
	running := map[string]stopGateBackgroundTask{}
	known := map[string]stopGateBackgroundTask{} // every launch, for SendMessage resumes
	queued := map[string][]time.Time{}

	finish := func(text string) {
		if !stopGateNotifStatusRe.MatchString(text) {
			return // a Monitor event, not the end of the stream
		}
		if m := stopGateNotifTaskRe.FindStringSubmatch(text); m != nil {
			delete(running, m[1])
		}
	}

	br := bufio.NewReader(r)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			stopGateScanLine(raw, uses, running, known, queued, finish)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stopGateBackgroundState{}, err
		}
	}

	var st stopGateBackgroundState
	for _, t := range running {
		if t.Started.IsZero() || now.Sub(t.Started) > stopGateBackgroundMaxAge {
			continue
		}
		if !cliStart.IsZero() && t.Started.Before(cliStart) {
			continue
		}
		st.Running = append(st.Running, t)
	}
	sort.Slice(st.Running, func(i, j int) bool { return st.Running[i].Started.Before(st.Running[j].Started) })
	for _, ts := range queued {
		for _, at := range ts {
			if now.Sub(at) <= stopGateQueuedMaxAge && (cliStart.IsZero() || !at.Before(cliStart)) {
				st.Queued++
			}
		}
	}
	return st, nil
}

// stopGateScanLine folds one transcript line into the pass's state. A cheap
// substring test skips the large lines (prompt snapshots, file reads) that
// cannot matter before any JSON is decoded.
func stopGateScanLine(raw []byte, uses map[string]stopGateToolUse, running, known map[string]stopGateBackgroundTask,
	queued map[string][]time.Time, finish func(string)) {
	s := string(raw)
	if !strings.Contains(s, `"tool_use`) && !strings.Contains(s, "task-notification") && !strings.Contains(s, `"queue-operation"`) {
		return
	}
	var line stopGateTranscriptLine
	if json.Unmarshal(raw, &line) != nil || line.IsSidechain {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, line.Timestamp)
	if err != nil {
		return
	}

	switch line.Type {
	case "assistant":
		for _, c := range stopGateContentItems(line.Message.Content) {
			if c.Type != "tool_use" || !stopGateBackgroundTools[c.Name] {
				continue
			}
			var in struct {
				Description string `json:"description"`
				To          string `json:"to"`
				TaskID      string `json:"task_id"`
				ShellID     string `json:"shell_id"`
			}
			_ = json.Unmarshal(c.Input, &in)
			stopID := in.TaskID
			if stopID == "" {
				stopID = in.ShellID
			}
			uses[c.ID] = stopGateToolUse{name: c.Name, description: in.Description, to: in.To, stopID: stopID}
		}
	case "user":
		if text, ok := stopGateStringContent(line.Message.Content); ok {
			finish(text)
			return
		}
		for _, c := range stopGateContentItems(line.Message.Content) {
			if c.Type != "tool_result" || c.IsError {
				continue
			}
			use, ok := uses[c.ToolUseID]
			if !ok {
				continue
			}
			if stopGateStopTools[use.name] {
				delete(running, stopGateStoppedTaskID(use, line.ToolUseResult))
				continue
			}
			if t, ok := stopGateLaunchedTask(use, line.ToolUseResult, c, ts); ok {
				running[t.ID] = t
				known[t.ID] = t
				continue
			}
			// A message to a finished subagent resumes it, and it notifies
			// again when it stops.
			if use.name == "SendMessage" {
				if t, ok := known[use.to]; ok {
					t.Started = ts
					running[t.ID] = t
				}
			}
		}
	case "queue-operation":
		if line.Content == "" {
			return
		}
		if line.Operation == "enqueue" {
			queued[line.Content] = append(queued[line.Content], ts)
			finish(line.Content)
			return
		}
		if q := queued[line.Content]; len(q) > 0 {
			queued[line.Content] = q[1:]
		}
	case "attachment":
		if line.Attachment.Type == "queued_command" {
			finish(line.Attachment.Prompt)
		}
	}
}

// stopGateLaunchedTask recognises a tool result that started background work:
// an async Agent launch, a Monitor, or a run_in_background Bash command.
func stopGateLaunchedTask(use stopGateToolUse, toolUseResult json.RawMessage, c stopGateContentItem, ts time.Time) (stopGateBackgroundTask, bool) {
	var res struct {
		Status           string `json:"status"`
		AgentID          string `json:"agentId"`
		TaskID           string `json:"taskId"`
		BackgroundTaskID string `json:"backgroundTaskId"`
	}
	_ = json.Unmarshal(toolUseResult, &res)
	t := stopGateBackgroundTask{Description: use.description, Started: ts}
	switch use.name {
	case "Agent", "Task":
		t.Kind, t.ID = stopGateBgSubagent, res.AgentID
		if res.Status != "async_launched" {
			// Older CLIs carry the launch only in the result text.
			text := stopGateResultText(c)
			m := stopGateAgentIDRe.FindStringSubmatch(text)
			if !strings.HasPrefix(text, "Async agent launched") || m == nil {
				return t, false
			}
			t.ID = m[1]
		}
	case "Monitor":
		t.Kind, t.ID = stopGateBgMonitor, res.TaskID
	case "Bash":
		t.Kind, t.ID = stopGateBgCommand, res.BackgroundTaskID
	}
	return t, t.ID != ""
}

// stopGateStoppedTaskID is the task a TaskStop/KillShell/KillBash result
// stopped: toolUseResult.task_id, else the id the call was given.
func stopGateStoppedTaskID(use stopGateToolUse, toolUseResult json.RawMessage) string {
	var res struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(toolUseResult, &res)
	if res.TaskID != "" {
		return res.TaskID
	}
	return use.stopID
}

func stopGateContentItems(raw json.RawMessage) []stopGateContentItem {
	var items []stopGateContentItem
	_ = json.Unmarshal(raw, &items)
	return items
}

func stopGateStringContent(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// stopGateResultText is a tool_result's text, whether its content is a string
// or an array of text items.
func stopGateResultText(c stopGateContentItem) string {
	if s, ok := stopGateStringContent(c.Content); ok {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(c.Content, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// stopGateBackgroundWork is the gate's entry point: what the transcript named
// in the Stop hook input shows is still in flight. Any failure (no path, an
// unreadable file) reports nothing pending, so the gate falls through to its
// normal checks.
func stopGateBackgroundWork(stdinEvent map[string]any) stopGateBackgroundState {
	path, _ := stdinEvent["transcript_path"].(string)
	if path == "" {
		return stopGateBackgroundState{}
	}
	f, err := os.Open(path)
	if err != nil {
		return stopGateBackgroundState{}
	}
	defer f.Close()
	st, err := stopGatePendingBackground(f, time.Now(), stopGateCLIStartTime())
	if err != nil {
		return stopGateBackgroundState{}
	}
	return st
}

// stopGateCLIStartTime is when the Claude Code CLI that spawned this hook
// started, found by walking up from the hook's parent. Zero when it cannot be
// established, which leaves the max-age bound as the only staleness check.
func stopGateCLIStartTime() time.Time {
	pid := os.Getppid()
	for hops := 0; hops < stopGateAncestryMaxHops && pid > 1; hops++ {
		comm, _, ppid, ok := stopGateReadProcStat(pid)
		if !ok {
			return time.Time{}
		}
		if stopGateIsClaudeCLIEntry(stopGateProcEntry{comm: comm, cmdline: stopGateReadProcCmdline(pid)}) {
			return stopGateProcStartTime(pid)
		}
		pid = ppid
	}
	return time.Time{}
}

// stopGateClockTicks is USER_HZ, the unit of /proc/<pid>/stat starttime. It is
// 100 on every Linux the agent image runs on.
const stopGateClockTicks = 100

// stopGateProcStartTime converts /proc/<pid>/stat field 22 (starttime, in
// clock ticks since boot) to wall time using btime from /proc/stat.
func stopGateProcStartTime(pid int) time.Time {
	data, err := os.ReadFile(stopGateProcRoot + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}
	}
	line := string(data)
	closeParen := strings.LastIndexByte(line, ')')
	if closeParen < 0 {
		return time.Time{}
	}
	// Fields after "(comm)" start at field 3 (state), so starttime is index 19.
	rest := strings.Fields(line[closeParen+1:])
	if len(rest) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	statData, err := os.ReadFile(stopGateProcRoot + "/stat")
	if err != nil {
		return time.Time{}
	}
	for _, l := range strings.Split(string(statData), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			btime, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}
			}
			return time.Unix(btime, 0).Add(time.Duration(ticks) * time.Second / stopGateClockTicks)
		}
	}
	return time.Time{}
}
