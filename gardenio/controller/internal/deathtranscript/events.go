package deathtranscript

// Harness-neutral event streams (ADR 0004 §9, gd-MolQ67L3xX).
//
// A Claude agent leaves a Claude-shaped session log in S3
// (`transcripts/<N>.jsonl`). A Codex agent does not: its session log is a Codex
// rollout, and the ADR decision is that Go consumers must NOT grow a rollout
// parser. coop already uploads two harness-neutral logs beside the session log
// for every driver (coop s3.rs upload_event_logs):
//
//	hook_events.jsonl   {"hook_seq":N,"json":{"event":"post_tool_use","data":{…}},"timestamp_ms":N}
//	state_events.jsonl  {"prev":…,"next":…,"seq":N,"cause":…,"last_message":"…","timestamp_ms":N}
//
// Between them they carry what a death report needs — the prompts, every tool
// call (so the last command), and the last assistant message — in the same shape
// whichever CLI produced them. ParseEvents projects the pair into the same Turn
// type the Claude path produces; the Claude path itself is untouched.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/toollatency"
)

const (
	hookEventsObj  = "hook_events.jsonl"
	stateEventsObj = "state_events.jsonl"

	// HarnessClaude / HarnessCodex / HarnessGemini mirror beadsapi.Harness*;
	// meta.json carries the same strings (coop s3.rs SessionMeta.harness).
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
	HarnessGemini = "gemini"
)

// IsEventStreamHarness reports whether sessions from this harness are read from
// the neutral event streams instead of a Claude-shaped transcript. Empty and
// unknown values are treated as Claude so every pre-existing session keeps its
// current path.
func IsEventStreamHarness(harness string) bool {
	switch strings.TrimSpace(harness) {
	case HarnessCodex, HarnessGemini:
		return true
	}
	return false
}

// EventStreams is the raw pair of coop event logs for one spawn.
type EventStreams struct {
	// Spawn is the spawn-UUID segment ("" for the legacy flat layout).
	Spawn string
	// Hook / State are the raw JSONL bodies; either may be nil when coop never
	// uploaded that file.
	Hook  []byte
	State []byte
	// Key is the S3 key of the hook log when present, else of the state log. It
	// is what the report links as the recording.
	Key string
	// Harness is meta.json's harness for the spawn ("" when unreadable).
	Harness string
}

// SessionHarness returns the harness recorded in the meta.json of sessionKey's
// most recent spawn, or "" when it cannot be determined (no meta, old coop that
// predates the field, S3 error). Callers treat "" as Claude.
func (f *Fetcher) SessionHarness(ctx context.Context, sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	spawns, _ := f.eventObjects(ctx, sessionKey)
	if len(spawns) == 0 {
		// No event logs: fall back to the spawns that have transcripts.
		spawn, keys, err := f.ChunkKeys(ctx, sessionKey)
		if err != nil || len(keys) == 0 {
			return ""
		}
		return f.metaHarness(ctx, sessionKey, spawn)
	}
	return f.metaHarness(ctx, sessionKey, f.chooseSpawn(ctx, sessionKey, spawns))
}

func (f *Fetcher) metaHarness(ctx context.Context, sessionKey, spawn string) string {
	meta, err := f.spawnMeta(ctx, sessionKey, spawn)
	if err != nil || meta == nil {
		return ""
	}
	return strings.TrimSpace(meta.Harness)
}

// eventObjects buckets the event-log objects under sessionKey by spawn. The
// returned map reuses transcriptChunk so chooseSpawn's started_at preference
// applies unchanged; objects maps spawn → {file name → key}.
func (f *Fetcher) eventObjects(ctx context.Context, sessionKey string) (map[string][]transcriptChunk, map[string]map[string]string) {
	base := fmt.Sprintf("%s/%s/", f.prefix(), sessionKey)
	keys, err := f.Store.List(ctx, base)
	if err != nil {
		return nil, nil
	}
	spawns := map[string][]transcriptChunk{}
	objects := map[string]map[string]string{}
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, base)
		if !ok {
			continue
		}
		spawn, name := "", rest
		if i := strings.LastIndex(rest, "/"); i >= 0 {
			spawn, name = rest[:i], rest[i+1:]
		}
		if strings.Contains(spawn, "/") || (name != hookEventsObj && name != stateEventsObj) {
			continue
		}
		if objects[spawn] == nil {
			objects[spawn] = map[string]string{}
			spawns[spawn] = []transcriptChunk{{spawn: spawn, key: key}}
		}
		objects[spawn][name] = key
	}
	return spawns, objects
}

// FetchLatestEvents returns the event logs of sessionKey's most recent spawn.
// ErrNoTranscript when neither log was archived.
func (f *Fetcher) FetchLatestEvents(ctx context.Context, sessionKey string) (*EventStreams, error) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return nil, fmt.Errorf("deathtranscript: empty session key")
	}
	spawns, objects := f.eventObjects(ctx, sessionKey)
	if len(spawns) == 0 {
		return nil, ErrNoTranscript
	}
	spawn := f.chooseSpawn(ctx, sessionKey, spawns)
	out := &EventStreams{Spawn: spawn, Harness: f.metaHarness(ctx, sessionKey, spawn)}
	for name, dst := range map[string]*[]byte{hookEventsObj: &out.Hook, stateEventsObj: &out.State} {
		key, ok := objects[spawn][name]
		if !ok {
			continue
		}
		data, err := f.Store.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("fetching %s: %w", key, err)
		}
		if len(data) > maxTranscriptBytes {
			return nil, fmt.Errorf("deathtranscript: %s is %d bytes, exceeds %d cap", key, len(data), maxTranscriptBytes)
		}
		*dst = data
	}
	out.Key = objects[spawn][hookEventsObj]
	if out.Key == "" {
		out.Key = objects[spawn][stateEventsObj]
	}
	return out, nil
}

// hookEventEntry / stateEventEntry decode the two coop log shapes. Unknown
// fields — notably the potentially huge tool_response — are skipped.
type hookEventEntry struct {
	JSON struct {
		Event string `json:"event"`
		Data  struct {
			Prompt               string          `json:"prompt"`
			ToolName             string          `json:"tool_name"`
			ToolInput            json.RawMessage `json:"tool_input"`
			LastAssistantMessage string          `json:"last_assistant_message"`
		} `json:"data"`
	} `json:"json"`
	TimestampMs int64 `json:"timestamp_ms"`
}

type stateEventEntry struct {
	LastMessage string `json:"last_message"`
	TimestampMs int64  `json:"timestamp_ms"`
}

// ParseEvents projects coop's hook and state logs into ordered Turns:
//
//   - user_prompt_submit          → Type "user", Text = the prompt
//   - post_tool_use               → Type "tool", Tool = tool name, Command = the
//     shell command for command tools (else a compact rendering of the input)
//   - stop (last_assistant_message) and state transitions carrying a
//     last_message                → Type "assistant", Text = that message
//
// Consecutive identical assistant texts collapse (the Stop hook and the Idle
// transition report the same final message). Blank and unparseable lines are
// skipped.
func ParseEvents(hook, state []byte) []Turn {
	var turns []Turn
	add := func(ms int64, t Turn) {
		if ms > 0 {
			t.Timestamp = time.UnixMilli(ms).UTC()
		}
		turns = append(turns, t)
	}

	for _, raw := range splitJSONLines(hook) {
		var e hookEventEntry
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		d := e.JSON.Data
		switch e.JSON.Event {
		case "user_prompt_submit", "before_agent":
			if strings.TrimSpace(d.Prompt) != "" {
				add(e.TimestampMs, Turn{Type: "user", Role: "user", Text: d.Prompt})
			}
		case "post_tool_use":
			if d.ToolName == "" {
				continue
			}
			// hook_events.jsonl is raw (the S3 audit marks it Confidential); the
			// report is posted, so scrub the command like session_slowest does.
			cmd := toollatency.RedactText(toolCommand(d.ToolInput))
			text := "[tool: " + d.ToolName + "]"
			if cmd != "" {
				text += " " + cmd
			}
			add(e.TimestampMs, Turn{Type: "tool", Tool: d.ToolName, Command: cmd, Text: text})
		case "stop":
			if strings.TrimSpace(d.LastAssistantMessage) != "" {
				add(e.TimestampMs, Turn{Type: "assistant", Role: "assistant", Text: d.LastAssistantMessage})
			}
		}
	}
	for _, raw := range splitJSONLines(state) {
		var e stateEventEntry
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		if strings.TrimSpace(e.LastMessage) != "" {
			add(e.TimestampMs, Turn{Type: "assistant", Role: "assistant", Text: e.LastMessage})
		}
	}

	// Two files interleave by wall clock; stable so equal stamps keep file order.
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].Timestamp.Before(turns[j].Timestamp) })

	out := turns[:0]
	for _, t := range turns {
		if n := len(out); n > 0 && t.Type == "assistant" && out[n-1].Type == "assistant" && out[n-1].Text == t.Text {
			continue
		}
		out = append(out, t)
	}
	return out
}

// toolCommand extracts the shell command from a tool_input object ({"command":
// "…"}), falling back to its compact JSON, and "" when there is no input.
func toolCommand(input json.RawMessage) string {
	input = bytes.TrimSpace(input)
	if len(input) == 0 || string(input) == "null" {
		return ""
	}
	var obj struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(input, &obj) == nil && len(obj.Command) > 0 {
		var s string
		if json.Unmarshal(obj.Command, &s) == nil {
			return s
		}
		var argv []string // Codex exec_command can report the argv form
		if json.Unmarshal(obj.Command, &argv) == nil {
			return strings.Join(argv, " ")
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, input) == nil {
		return compact.String()
	}
	return ""
}

// LastCommand returns the command of the final tool call in turns, "" when
// there is none.
func LastCommand(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Type == "tool" && turns[i].Command != "" {
			return turns[i].Command
		}
	}
	return ""
}
