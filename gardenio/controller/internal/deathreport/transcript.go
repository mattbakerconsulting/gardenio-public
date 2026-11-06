package deathreport

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"gardenio/controller/internal/deathtranscript"
)

// StopGateMarkers are the distinctive substrings the stop-gate injects into a
// transcript when it BLOCKS a turn-end (cmd/gb/hook.go injectStopGateText +
// the "You have been blocked N time(s)" reinforcement, materialized from the
// config bead's stop_gate_blocked text in internal/bridge/init_prompts.go).
// Their appearance in a user/tool feedback entry is the transcript-visible
// fingerprint of ONE stop-gate block. StopHookBlocks counts them to reconstruct
// the Stop-hook cadence that the cooldown backoff acted on.
var StopGateMarkers = []string{
	"STOP BLOCKED — decision gate unsatisfied",
	"STOP BLOCKED - decision gate unsatisfied", // ASCII-hyphen variant
	"You have been blocked",
	"decision point offered before session end",
}

// Turn is one model turn distilled from the transcript for the report: the
// assistant's text, whether it ended cleanly, which tools it called, and the
// timestamp of the turn. A "turn" here is one assistant message (deduped by
// message.id — Claude Code fans a single message across one JSONL line per
// content block, all repeating the same id).
type Turn struct {
	// Index is the 0-based position of this turn in the extracted series (the
	// last N turns, oldest first).
	Index int `json:"index"`
	// Timestamp is the turn's wall-clock time (RFC3339). Zero when the transcript
	// carried no timestamp on the message.
	Timestamp time.Time `json:"timestamp,omitzero"`
	// Text is the concatenated assistant text content of the turn (thinking
	// blocks excluded — those are the model's private reasoning, not its words).
	Text string `json:"text,omitempty"`
	// ToolCalls names the tools the turn invoked, in order.
	ToolCalls []string `json:"tool_calls,omitempty"`
	// Commands are the shell commands behind ToolCalls, same order; only set on
	// the event-log path (TranscriptFromTurns).
	Commands []string `json:"commands,omitempty"`
	// StopReason is the API stop_reason for the turn (end_turn / tool_use /
	// max_tokens / …), when present.
	StopReason string `json:"stop_reason,omitempty"`
	// QuietTurnEnd is true when the turn ended with no tool calls — the "nothing
	// left to do but wait" shape that trips the stop-gate. A run of quiet
	// turn-ends is the signature of an agent parked on a decision.
	QuietTurnEnd bool `json:"quiet_turn_end"`
}

// Transcript is the distilled, report-relevant view of a session JSONL log:
// the last N turns, the agent's last words, the count of stop-gate blocks, and
// the timestamps of those blocks (for the cadence diff).
type Transcript struct {
	// Turns is the last N model turns, oldest first.
	Turns []Turn `json:"turns"`
	// TotalTurns is the total number of model turns in the whole transcript
	// (not just the retained last N), so the report can say "last 10 of 340".
	TotalTurns int `json:"total_turns"`
	// LastWords is the assistant text of the final model turn — the agent's
	// literal last words before it died. Empty when the last turn was pure tool
	// use with no text.
	LastWords string `json:"last_words,omitempty"`
	// LastCommand is the command of the final tool call, when the transcript came
	// from the harness-neutral event logs (TranscriptFromTurns, Codex agents).
	// Empty for a Claude JSONL, whose parse is unchanged.
	LastCommand string `json:"last_command,omitempty"`
	// StopGateBlocks is the number of stop-gate block injections observed in the
	// transcript (see StopGateMarkers).
	StopGateBlocks int `json:"stop_gate_blocks"`
	// BlockTimestamps are the wall-clock times of each observed stop-gate block,
	// oldest first — the raw material for the Stop-hook cadence diff.
	BlockTimestamps []time.Time `json:"block_timestamps,omitempty"`
	// FirstTimestamp / LastTimestamp bound the whole session (zero when the
	// transcript carried no timestamps).
	FirstTimestamp time.Time `json:"first_timestamp,omitzero"`
	LastTimestamp  time.Time `json:"last_timestamp,omitzero"`
}

// rawEntry is the subset of a Claude Code JSONL line the summarizer reads.
type rawEntry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// rawMessage is the subset of the nested "message" object we read.
type rawMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"` // string OR []block
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// ParseTranscript reads a Claude Code session JSONL stream and distills it into
// a Transcript retaining the last lastN model turns. It is a pure streaming
// parse: it never seeks, holds only lastN turns in memory, and tolerates
// malformed lines (skipped) so a truncated/salvaged recording still yields a
// report. A lastN <= 0 falls back to DefaultLastN.
//
// Turn identity: one model turn == one assistant message. Claude Code writes a
// single assistant message across MULTIPLE JSONL lines (one per content block),
// every line repeating the same message.id — so turns are deduped by id, the
// first line establishing the turn and later lines of the same id merging their
// text/tool blocks in. Result/user/system entries are scanned for stop-gate
// block markers and timestamps but are not themselves turns.
func ParseTranscript(r io.Reader, lastN int) (*Transcript, error) {
	if lastN <= 0 {
		lastN = DefaultLastN
	}

	t := &Transcript{}

	// Ring of the most recent lastN turns, plus an index from message.id to the
	// ring slot so repeated content-block lines merge into their turn.
	ring := newTurnRing(lastN)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024) // up to 4MB lines

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e rawEntry
		if json.Unmarshal(line, &e) != nil {
			continue // skip malformed
		}

		ts := parseTS(e.Timestamp)
		if !ts.IsZero() {
			if t.FirstTimestamp.IsZero() {
				t.FirstTimestamp = ts
			}
			t.LastTimestamp = ts
		}

		// Stop-gate block detection runs over the RAW line so it catches the
		// marker wherever Claude Code embedded it (tool_result content, a system
		// feedback entry, or a user turn echoing the injection).
		if isStopGateBlock(line) {
			t.StopGateBlocks++
			if !ts.IsZero() {
				t.BlockTimestamps = append(t.BlockTimestamps, ts)
			}
		}

		var msg rawMessage
		if len(e.Message) == 0 || json.Unmarshal(e.Message, &msg) != nil {
			continue
		}
		if msg.Role != "assistant" {
			continue
		}

		text, tools := extractContent(msg.Content)

		// New assistant message id => a new turn. A blank id (older formats)
		// always starts a new turn.
		if msg.ID == "" || !ring.has(msg.ID) {
			t.TotalTurns++
			turn := Turn{
				Timestamp:  ts,
				Text:       text,
				ToolCalls:  tools,
				StopReason: msg.StopReason,
			}
			ring.push(msg.ID, turn)
		} else {
			// Continuation line of an existing turn: merge extra text/tools and
			// upgrade the stop_reason / timestamp if this line carries them.
			ring.merge(msg.ID, text, tools, msg.StopReason, ts)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	t.Turns = ring.drain()
	for i := range t.Turns {
		t.Turns[i].Index = i
		t.Turns[i].QuietTurnEnd = len(t.Turns[i].ToolCalls) == 0
	}
	if n := len(t.Turns); n > 0 {
		t.LastWords = strings.TrimSpace(t.Turns[n-1].Text)
	}
	return t, nil
}

// TranscriptFromTurns distills the turns projected from a harness-neutral event
// log (deathtranscript.ParseEvents — Codex agents, ADR 0004 §9) into the same
// Transcript the Claude JSONL parse produces. A "turn" is one assistant message
// together with the tool calls that followed it up to the next assistant message
// or user prompt; tool calls before any assistant message form a turn of their
// own. user turns are not turns but bound the previous one. Stop-gate block
// detection needs the raw Claude entries, so StopGateBlocks stays zero here.
func TranscriptFromTurns(in []deathtranscript.Turn, lastN int) *Transcript {
	if lastN <= 0 {
		lastN = DefaultLastN
	}
	t := &Transcript{}
	var turns []Turn
	open := false // whether the last element of turns can still take tool calls
	for _, e := range in {
		if !e.Timestamp.IsZero() {
			if t.FirstTimestamp.IsZero() {
				t.FirstTimestamp = e.Timestamp
			}
			t.LastTimestamp = e.Timestamp
		}
		switch e.Type {
		case "user":
			open = false
		case "assistant":
			turns = append(turns, Turn{Timestamp: e.Timestamp, Text: strings.TrimSpace(e.Text)})
			open = true
		case "tool":
			if !open {
				turns = append(turns, Turn{Timestamp: e.Timestamp})
				open = true
			}
			cur := &turns[len(turns)-1]
			cur.ToolCalls = append(cur.ToolCalls, e.Tool)
			cur.Commands = append(cur.Commands, e.Command)
		}
	}
	t.TotalTurns = len(turns)
	if len(turns) > lastN {
		turns = turns[len(turns)-lastN:]
	}
	for i := range turns {
		turns[i].Index = i
		turns[i].QuietTurnEnd = len(turns[i].ToolCalls) == 0
	}
	t.Turns = turns
	if n := len(turns); n > 0 {
		t.LastWords = turns[n-1].Text
	}
	t.LastCommand = deathtranscript.LastCommand(in)
	return t
}

// DefaultLastN is the default number of trailing turns summarized (acceptance
// criterion: N=10).
const DefaultLastN = 10

// extractContent pulls assistant text and tool-call names from a message's
// content, which is either a bare string (older format) or an array of blocks.
// Thinking blocks are deliberately excluded from Text — they are the model's
// private reasoning, not its words; the "last words" quote must be what the
// agent actually said.
func extractContent(content json.RawMessage) (string, []string) {
	if len(content) == 0 {
		return "", nil
	}
	// Bare string content.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return strings.TrimSpace(s), nil
	}
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return "", nil
	}
	var texts []string
	var tools []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if txt := strings.TrimSpace(b.Text); txt != "" {
				texts = append(texts, txt)
			}
		case "tool_use":
			if b.Name != "" {
				tools = append(tools, b.Name)
			}
		}
	}
	return strings.Join(texts, "\n"), tools
}

// isStopGateBlock reports whether a raw JSONL line carries a stop-gate block
// marker. Matched against the raw bytes so the injection is caught regardless of
// which entry type Claude Code embedded it in.
func isStopGateBlock(line []byte) bool {
	s := string(line)
	for _, m := range StopGateMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// turnRing is a fixed-capacity ring buffer of the most recent turns, keyed by
// message.id so multi-line assistant messages merge into one turn. It keeps only
// the last N turns while remembering ids that have scrolled out (so a late
// continuation line of an evicted turn does not resurrect it as a new turn).
type turnRing struct {
	cap     int
	order   []string        // message.ids in insertion order, current window
	byID    map[string]Turn // id -> turn, current window only
	evicted map[string]bool // ids that scrolled out of the window
}

func newTurnRing(capacity int) *turnRing {
	if capacity < 1 {
		capacity = 1
	}
	return &turnRing{
		cap:     capacity,
		byID:    make(map[string]Turn, capacity+1),
		evicted: make(map[string]bool),
	}
}

// has reports whether id is a turn currently retained in the window. An evicted
// id returns false here but is remembered by evicted[] so push() knows not to
// count it as brand new.
func (r *turnRing) has(id string) bool {
	if id == "" {
		return false
	}
	_, ok := r.byID[id]
	return ok
}

func (r *turnRing) push(id string, turn Turn) {
	// A continuation line for an already-evicted turn: merge is impossible (the
	// turn is gone), so drop it rather than create a duplicate partial turn.
	if id != "" && r.evicted[id] {
		return
	}
	key := id
	if key == "" {
		key = uniqueBlankKey(len(r.order))
	}
	r.order = append(r.order, key)
	r.byID[key] = turn
	for len(r.order) > r.cap {
		old := r.order[0]
		r.order = r.order[1:]
		delete(r.byID, old)
		r.evicted[old] = true
	}
}

func (r *turnRing) merge(id, text string, tools []string, stopReason string, ts time.Time) {
	turn, ok := r.byID[id]
	if !ok {
		return
	}
	if text != "" {
		if turn.Text != "" {
			turn.Text += "\n" + text
		} else {
			turn.Text = text
		}
	}
	turn.ToolCalls = append(turn.ToolCalls, tools...)
	if stopReason != "" {
		turn.StopReason = stopReason
	}
	if turn.Timestamp.IsZero() && !ts.IsZero() {
		turn.Timestamp = ts
	}
	r.byID[id] = turn
}

func (r *turnRing) drain() []Turn {
	out := make([]Turn, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// uniqueBlankKey mints a synthetic key for a turn whose message carried no id,
// so blank-id turns never collide with each other in the ring map.
func uniqueBlankKey(seq int) string {
	return "\x00blank\x00" + itoa(seq)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
