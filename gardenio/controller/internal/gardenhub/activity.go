package gardenhub

// activity.go — what each gardener is doing right now (gd-H4jk9KgUAG, step 4a of plan gd-gAKb8Bf0Nm; spec
// gd-zE5JlcI54m section 4a). beads-viewer feeds it the hook events `gb hook relay` publishes on
// hooks.<agent>.<event> (core NATS, no replay: cmd/beads-viewer/activity_feed.go) and the thinking events of the
// telepathy hub. Each is reduced here to a bounded doing class, a tool word and, on a call's start, a scrubbed caption
// (caption.go, gd-gCHKhIROvB): tool_input is read only to classify a Bash command (toollatency.ClassifyCmd) and for
// the one field a caption is made from, and is never kept, so it cannot reach the wire. Out of it come the ephemeral
// SSE `activity` event on /garden/farm/events (outside the versioned patch chain, at most 4 a second per gardener,
// cut per viewer like gardeners[], then to the viewer's viewport: gd-fBGGNdgrLc, and folded longer for a stream with
// many busy gardeners in view, so no stream passes a budget: gd-iAq4va9XDt) and gardeners[].doing on the farm
// document (the in-memory last-seen, never a bead write). Each finished call is also a row of the gardener's transcript
// (transcript.go). Shapes: gardenio/docs/homestead-farm-read-model.md, "Activity" and "Transcript".

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/toollatency"
)

// The doing classes: a bounded domain, by tool and never by purpose.
const (
	DoingRead     = "read"
	DoingEdit     = "edit"
	DoingTest     = "test"
	DoingBuild    = "build"
	DoingGit      = "git"
	DoingBeads    = "beads"
	DoingDeploy   = "deploy"
	DoingWeb      = "web"
	DoingShell    = "shell"
	DoingDelegate = "delegate"
	DoingThink    = "think"
	DoingSay      = "say"     // what the gardener said: an assistant text block (ObserveSay); a row of its transcript at once
	DoingWaiting  = "waiting" // derived (a call started and not finished for activityWaiting); never in an activity event
)

// The hook events the feed reads, and the two it makes of what coop forwards: a telepathy thought, and what the
// gardener said (an assistant text block, coop.events.messages.*.assistant_text; gd-HHGjxt59Mr).
const (
	HookPreToolUse         = "PreToolUse"
	HookPostToolUse        = "PostToolUse"
	HookPostToolUseFailure = "PostToolUseFailure"
	HookSubagentStart      = "SubagentStart"
	HookSubagentStop       = "SubagentStop"
	HookThinking           = "Thinking"
	HookSay                = "Say"
)

const (
	activityMinGap       = 250 * time.Millisecond // at most 4 activity events a second per gardener
	activityWaiting      = 5 * time.Second        // a call started this long ago and not finished reads as waiting
	activityTick         = 100 * time.Millisecond // how often folded events are flushed
	activitySnapEvery    = 2 * time.Second        // the farm document re-bakes doing at most this often at one version
	activityClientBuffer = 64                     // a viewer's queue; past it the newest events are dropped
	activityForget       = time.Hour              // an agent not heard from (and not a gardener) is forgotten
	activityOpenForget   = 30 * time.Minute       // a call or subagent with no finish this long is let go
	maxActivityAgents    = 1000                   // agents tracked; past it a new non-gardener is ignored
	maxActivityInflight  = 16                     // open calls and subagents tracked per agent
	maxActivityTool      = 40                     // bytes of the tool word

	activityBusyEvery  = 5 * time.Second  // a viewer below the Plan level hears which projects are busy at most this often
	activityBusyWindow = 30 * time.Second // and, as its view drops there, which were in the last this long (BIRDS.BUSY_S)
	viewPlanLevel      = 2                // the Plan level: from here down a viewer is sent its view's calls
	maxViewIDs         = 256              // gardener ids a view names; the rest are ignored
	maxViewID          = 64               // bytes of one; a longer one is ignored

	// A stream's budget (gd-iAq4va9XDt). Each gardener's calls fold over activityMinGap << k for one of
	// activityFoldTiers k; a stream is put on the shortest fold at which its busy gardeners in view (heard in the last
	// activityFoldWindow) come to at most activityStreamRate events a second, chosen again every activityFoldEvery.
	// So 16 busy gardeners keep 250 ms, 60 (a whole farm) fold over 1 s, and 256 (a view's cap) over 4 s.
	activityStreamRate = 64
	activityFoldTiers  = 5 // 250 ms, 500 ms, 1 s, 2 s, 4 s
	activityFoldEvery  = time.Second
	activityFoldWindow = 10 * time.Second
)

// activityGap is the fold of tier k.
func activityGap(k int) time.Duration { return activityMinGap << k }

// ActivityFolds names the activity folds a stream may be on, as the fold label of the metrics (bounded: five).
var ActivityFolds = func() (out [activityFoldTiers]string) {
	for k := range out {
		out[k] = activityGap(k).String()
	}
	return out
}()

// The SSE events the activity feed writes (ActivityMsg.Event).
const (
	ActivityEvent = "activity" // FarmActivity: one call (or a fold of them) of a gardener in the view
	BusyEvent     = "busy"     // FarmBusy: the projects busy, for a view below the Plan level
	DoingEvent    = "doing"    // FarmDoingOf: where a gardener that came into the view is at
)

// toolSubagent is the tool word of a SubagentStart / SubagentStop event: the hawk, not a call of the gardener's.
const toolSubagent = "subagent"

// HookActivity is one hook event reduced to what the feed may use. It has no field for tool_input: parseHookEvent
// reads the Bash command into Tool (a cmd_class) and the fields a caption is made from into Cap (scrubbed: caption.go),
// and drops the rest.
type HookActivity struct {
	Agent     string // agent_bead_id: the gardener's id
	Event     string // one of the Hook* names
	Tool      string // the tool word: cmd_class for Bash, else the tool name
	Doing     string // the class
	Cap       string // the caption of a call's start (captionOf), at most captionMax runes; empty on any other event
	Src       string // where Cap came from (CapSrc*), on a call's start
	CallID    string // tool_use_id or subagent id, to pair a finish with its start; never sent
	Session   string // never sent; a new one restarts the call count
	DurMS     int64  // duration_ms when the harness reported it
	At        time.Time
	Sidechain bool // a call from inside a subagent: the hawk stands for it, so it is not a call of the gardener's

	haiku   bool   // on a start: Haiku is writing its caption (Farm.ObserveHook), so its transcript row waits for it
	capTool string // on a start captionOf made no caption for: the tool, as captionToolLabel bounds it
}

// hookWire is the slice of cmd/gb/hook_relay.go's hookRelayEvent the feed decodes. tool_input stays raw and is read
// only for a Bash command and the fields of toolFields. tool_description and tool_file_path are the relay's copies of
// those two fields from before it cut tool_input to 1024 bytes (spec 6.2); a relay older than them sends neither.
type hookWire struct {
	Event           string          `json:"event"`
	TS              string          `json:"ts"`
	AgentBeadID     string          `json:"agent_bead_id"`
	SessionID       string          `json:"session_id"`
	IsSidechain     bool            `json:"is_sidechain"`
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	ToolDescription string          `json:"tool_description"`
	ToolFilePath    string          `json:"tool_file_path"`
	ToolUseID       string          `json:"tool_use_id"`
	DurationMs      int64           `json:"duration_ms"`
	SubagentID      string          `json:"subagent_id"`
}

// truncatedField reads one string field out of a tool_input the relay cut to a string (its first 1024 bytes of JSON
// and "..."): a field whose value ended inside the kept bytes, as an Agent's description does (it sorts first).
func truncatedField(prefix, key string) string {
	needle := `"` + key + `":"`
	i := strings.Index(prefix, needle)
	if i < 0 {
		return ""
	}
	rest := prefix[i+len(needle):]
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '\\':
			j++
		case '"':
			var s string
			if json.Unmarshal([]byte(`"`+rest[:j]+`"`), &s) == nil {
				return s
			}
			return ""
		}
	}
	return ""
}

// hookBash is a Bash PreToolUse's command, for the Haiku fallback alone (Farm.ObserveHook): it is handed to the
// captioner, which scrubs it before anything is sent, and is never kept. bash is false for any other event.
type hookBash struct {
	bash    bool
	command string
}

// parseHookEvent decodes a hook relay payload, and the command of a Bash PreToolUse beside it (never in the
// HookActivity). false for a malformed one, one without an agent bead id, or an event the feed does not show.
func parseHookEvent(data []byte, now time.Time) (HookActivity, hookBash, bool) {
	var w hookWire
	if json.Unmarshal(data, &w) != nil || strings.TrimSpace(w.AgentBeadID) == "" {
		return HookActivity{}, hookBash{}, false
	}
	var hb hookBash
	a := HookActivity{
		Agent: strings.TrimSpace(w.AgentBeadID), Event: w.Event, Session: w.SessionID, DurMS: w.DurationMs,
		At: now, Sidechain: w.IsSidechain, CallID: w.ToolUseID,
	}
	if t, err := time.Parse(time.RFC3339Nano, w.TS); err == nil && !t.After(now) && now.Sub(t) < time.Minute {
		a.At = t // a producer clock ahead of ours, or a late event, is stamped on arrival instead
	}
	switch w.Event {
	case HookPreToolUse, HookPostToolUse, HookPostToolUseFailure:
		var in toolFields
		var prefix string
		if json.Unmarshal(w.ToolInput, &prefix) == nil {
			// The relay cut it to a string: a field that ended in the kept bytes is still read.
			in.Description, in.FilePath = truncatedField(prefix, "description"), truncatedField(prefix, "file_path")
		} else {
			_ = json.Unmarshal(w.ToolInput, &in)
		}
		cmd := ""
		if w.ToolName == "Bash" {
			cmd = toollatency.ClassifyCmd(in.Command)
		}
		a.Doing, a.Tool = DoingOf(w.ToolName, cmd), toolWord(w.ToolName, cmd)
		if w.Event == HookPreToolUse {
			if w.ToolDescription != "" {
				in.Description = w.ToolDescription
			}
			if w.ToolFilePath != "" {
				in.FilePath = w.ToolFilePath
			}
			a.Cap, a.Src = captionOf(w.ToolName, in)
			if a.Cap == "" {
				a.capTool = captionToolLabel(w.ToolName)
			}
			if w.ToolName == "Bash" {
				hb = hookBash{bash: true, command: in.Command}
			}
		}
	case HookSubagentStart, HookSubagentStop:
		a.Doing, a.Tool, a.CallID = DoingDelegate, toolSubagent, w.SubagentID
	default:
		return HookActivity{}, hookBash{}, false
	}
	return a, hb, true
}

// DoingOf is the class of a tool call; cmdClass is toollatency.ClassifyCmd of a Bash command ("" for other tools).
func DoingOf(tool, cmdClass string) string {
	switch tool {
	case "Read", "Grep", "Glob", "LSP", "NotebookRead", "ToolSearch":
		return DoingRead
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return DoingEdit
	case "WebFetch", "WebSearch":
		return DoingWeb
	case "Agent", "Task":
		return DoingDelegate
	case "Bash":
		switch cmdClass {
		case "go_test", "cargo_test", "npm_test", "make":
			return DoingTest
		case "go_build", "cargo_build", "docker_build":
			return DoingBuild
		}
		bin, _, _ := strings.Cut(cmdClass, "_")
		switch bin {
		case "git", "gh":
			return DoingGit
		case "kd", "gb":
			return DoingBeads
		case "helm", "kubectl", "aws":
			return DoingDeploy
		}
	}
	return DoingShell
}

// toolWord is what the wire says for the tool: the cmd_class for Bash (a bounded domain), else the tool name kept to
// [A-Za-z0-9_.-] and maxActivityTool bytes.
func toolWord(tool, cmdClass string) string {
	if tool == "Bash" {
		return cmdClass
	}
	var b strings.Builder
	for _, r := range tool {
		if b.Len() >= maxActivityTool {
			break
		}
		if r == '_' || r == '-' || r == '.' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// FarmActivity is the SSE `activity` event. dur is 0 when the call started, else how long it took in ms (at least 1).
// n is set only on an event that folds several: the calls started in the fold, when that is not what the event alone
// says (1 for a call's start; 0 for a finish, a thought, or a subagent's start or stop). cap is set only on an event
// whose calls started: the caption of the call's start, or of the newest start in the fold.
type FarmActivity struct {
	ID    string `json:"id"`
	Doing string `json:"doing"`
	Tool  string `json:"tool"`
	At    int64  `json:"at"` // unix s
	Dur   int64  `json:"dur"`
	N     int    `json:"n,omitempty"`
	Cap   string `json:"cap,omitempty"`
}

// started is how many calls the event alone says started: a bird each. What the gardener said (DoingSay, dur 0) is one too,
// the robin, though it is no call (the page's call count leaves it out; web/activity.js).
func (e FarmActivity) started() int {
	if e.Dur == 0 && e.Doing != DoingThink && e.Tool != toolSubagent {
		return 1
	}
	return 0
}

// FarmDoing is gardeners[].doing: the last thing heard, so a fresh page poses the gardener before the next call.
type FarmDoing struct {
	Class string `json:"class"`
	Tool  string `json:"tool"`
	At    int64  `json:"at"`            // unix s
	Calls int    `json:"calls"`         // tool calls started this session
	Cap   string `json:"cap,omitempty"` // the caption of the newest call started
}

// masked is d for a scoped viewer: its caption masked (maskScoped).
func (d FarmDoing) masked() FarmDoing {
	d.Cap = maskScoped(d.Cap)
	return d
}

type openCall struct {
	at          time.Time
	doing, tool string
	cap         string // its caption (captionMax runes): the start's, or a late one (lateCaption)
	start       uint64 // as activity.starts numbered it
	haiku       bool   // Haiku is writing its caption
}

// actState is one agent's last-seen.
type actState struct {
	session  string
	calls    int
	cap      string // the caption of the newest call started, as sent
	start    uint64 // the newest call started, as activity.starts numbered it (lateCaption matches on it)
	last     FarmActivity
	lastAt   time.Time
	seen     time.Time
	open     map[string]openCall  // calls started and not finished, by tool_use_id
	subs     map[string]time.Time // subagents running, by id
	waitSeen bool                 // the waiting edge of the last start was counted in the epoch
	ring     callRing             // its transcript (transcript.go): the gardener's calls that finished
	parked   []parkedRow          // finished calls waiting for their Haiku caption

	fold [activityFoldTiers]actFold // one per fold a stream may be on
}

// actFold is one gardener's events folding over one tier's gap.
type actFold struct {
	sentAt  time.Time
	pending *FarmActivity
	folded  int    // hook events in pending
	starts  int    // calls started in pending
	cap     string // the caption of the newest of them
	due     time.Time
}

// ActivityMsg is one SSE event of the activity feed: its event name and its data, one JSON line.
type ActivityMsg struct {
	Event string
	Data  []byte
}

// size is its bytes on the wire: "event: <e>\ndata: <d>\n\n".
func (m ActivityMsg) size() int { return len("event: \ndata: \n\n") + len(m.Event) + len(m.Data) }

// FarmBusy is the SSE `busy` event: each project with a gardener heard since the viewer's last one, and how many whole
// seconds ago that was. Below the Plan level a viewer is sent this instead of the calls (gd-fBGGNdgrLc).
type FarmBusy struct {
	Busy map[string]int64 `json:"busy"`
}

// FarmDoingOf is the SSE `doing` event: a gardener's gardeners[].doing as it comes into the view, so the page poses it
// before its next call.
type FarmDoingOf struct {
	ID string `json:"id"`
	FarmDoing
}

// ActivityView is what a page can see, as it says on POST /garden/farm/events/view. Below viewPlanLevel no calls are
// sent, only the busy projects; from there down, only the calls of the gardeners IDs names (those in view, and a
// margin round it). An id that is not a gardener is ignored.
type ActivityView struct {
	Level int      `json:"level"`
	IDs   []string `json:"ids"`
}

// actView is a viewer's declared view: busy below the Plan level, else the ids it names.
type actView struct {
	busy bool
	ids  map[string]bool
}

type actClient struct {
	ch     chan ActivityMsg
	keep   func(FarmGardener) bool
	view   *actView  // nil until the page says what it sees: every gardener its cut keeps
	busyAt time.Time // its last busy event (or the drop below the Plan level)
	tier   int       // the fold its calls are sent at; -1 below the Plan level (no calls)
	tierAt time.Time // when it was last chosen

	tr     *transcriptScope // the transcript its page holds open (transcript.go); nil: no call rows
	trRows []CallRow        // rows for its next call event, at most transcriptRate, the newest kept
	trSkip int              // rows written past those since its last call event
	trAt   time.Time        // its last call event
}

// activity holds every agent's last-seen and the activity subscribers. Its lock is its own: the hook feed never waits
// on a farm refresh. Lock order: Farm.mu, then activity.mu, never the reverse.
type activity struct {
	mu        sync.Mutex
	agents    map[string]*actState
	gardeners map[string]FarmGardener // the current set: an event is sent only for one of these
	clients   map[int]*actClient
	sids      map[string]int // a stream's id (handed to its page in the `hello` event) to its client
	nextID    int
	epoch     uint64                    // bumps whenever a gardener's doing may read differently
	starts    uint64                    // calls started, numbered across every agent (actState.start)
	sent      uint64                    // events written to subscriber queues
	bytes     uint64                    // their bytes on the wire
	dropped   uint64                    // events a full queue refused
	views     uint64                    // views the pages declared
	rate      int                       // activityStreamRate (a test sets it huge to measure the unbudgeted stream)
	wireCap   int                       // runes of cap on the wire (CaptionSettings.WireCap), at most wireCaptionMax
	ringRows  int                       // rows a gardener's transcript ring keeps (CaptionSettings.TranscriptRows)
	tiers     [activityFoldTiers]int    // streams on each fold: a fold no stream is on is not kept
	tierBytes [activityFoldTiers]uint64 // bytes of the calls sent at each fold
	rows      uint64                    // transcript rows written (transcript.go)
	trClients int                       // streams holding a transcript open
	trBytes   uint64                    // bytes of the call events sent to them
}

func newActivity() *activity {
	return &activity{agents: map[string]*actState{}, gardeners: map[string]FarmGardener{}, clients: map[int]*actClient{}, sids: map[string]int{}, rate: activityStreamRate,
		wireCap: wireCaptionMax, ringRows: transcriptRows}
}

// observe takes one reduced event at now. For a call's start it returns the number lateCaption names that call by;
// 0 for anything else.
func (a *activity) observe(ev HookActivity, now time.Time) (start uint64) {
	if ev.Agent == "" || ev.Sidechain {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.agents[ev.Agent]
	if st == nil {
		if _, ok := a.gardeners[ev.Agent]; !ok && len(a.agents) >= maxActivityAgents {
			return 0
		}
		st = &actState{open: map[string]openCall{}, subs: map[string]time.Time{}}
		a.agents[ev.Agent] = st
	}
	if ev.Session != "" && ev.Event != HookThinking && ev.Session != st.session {
		st.session, st.calls, st.cap, st.parked = ev.Session, 0, "", nil
		clear(st.open)
		clear(st.subs)
		st.ring.reset()
	}
	st.seen = now
	at := ev.At
	if at.IsZero() {
		at = now
	}
	e := FarmActivity{ID: ev.Agent, Doing: ev.Doing, Tool: ev.Tool, At: at.Unix()}
	switch ev.Event {
	case HookPreToolUse:
		st.calls++
		e.Cap = clipWords(ev.Cap, a.wireCap)
		st.cap = e.Cap
		a.starts++
		st.start, start = a.starts, a.starts
		if len(st.open) < maxActivityInflight {
			st.open[callKey(ev)] = openCall{at: at, doing: ev.Doing, tool: ev.Tool, cap: clipWords(ev.Cap, captionMax), start: a.starts, haiku: ev.haiku}
		}
	case HookPostToolUse, HookPostToolUseFailure:
		key := callKey(ev)
		c, ok := st.open[key]
		delete(st.open, key)
		e.Dur = ev.DurMS
		if e.Dur <= 0 && ok {
			e.Dur = at.Sub(c.at).Milliseconds()
		}
		e.Dur = max(e.Dur, 1)
		a.finishLocked(ev.Agent, st, key, c, ok, e, ev.Event == HookPostToolUse, at, now)
	case HookSubagentStart:
		if len(st.subs) < maxActivityInflight {
			st.subs[ev.CallID] = at
		}
	case HookSubagentStop:
		if t, ok := st.subs[ev.CallID]; ok {
			e.Dur = at.Sub(t).Milliseconds()
			delete(st.subs, ev.CallID)
		}
		e.Dur = max(e.Dur, 1)
	case HookThinking:
		if len(st.open) > 0 || len(st.subs) > 0 {
			return 0 // a call is out: the gardener is waiting on it, not thinking
		}
		e.Doing, e.Tool = DoingThink, ""
	case HookSay:
		// What the gardener said has no finish, so its row is written now (gd-HHGjxt59Mr): class say, no tool, the text as
		// its caption at sayMax runes (the event's at the wire cap), dur 0 and no ok, like a call let go. It is said whether
		// or not a call is out: a turn's text comes just before the calls it makes.
		e.Doing, e.Tool = DoingSay, ""
		e.Cap = clipWords(ev.Cap, a.wireCap)
		st.cap = e.Cap
		a.writeRowLocked(ev.Agent, st, CallRow{At: at.Unix(), Class: DoingSay, Cap: clipWords(ev.Cap, sayMax)})
	default:
		return 0
	}
	st.last, st.lastAt, st.waitSeen = e, at, false
	a.epoch++

	for k := range st.fold {
		f := &st.fold[k]
		if a.tiers[k] == 0 {
			f.pending = nil // no stream is on this fold: a stream that comes to it starts afresh
			continue
		}
		if f.pending == nil && now.Sub(f.sentAt) >= activityGap(k) {
			f.sentAt = now
			a.emitLocked(e, k)
			continue
		}
		if f.pending == nil {
			f.folded, f.starts, f.cap, f.due = 0, 0, "", f.sentAt.Add(activityGap(k))
		}
		f.folded++
		if e.started() > 0 {
			f.starts += e.started()
			f.cap = e.Cap
		}
		ev := e
		f.pending = &ev
	}
	return start
}

// isGardener says id is in the current set: only a gardener's Bash call goes to Haiku (spec 8).
func (a *activity) isGardener(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.gardeners[id]
	return ok
}

// lateCaption takes a caption Haiku wrote for the call of agent that observe numbered start, after its start went out
// without one (spec 7.2). If that call is still the agent's newest start, it becomes the call's caption, the doing's
// cap and the cap any fold still holding the start will carry, and the gardener's doing is sent as one `doing` event
// under the activity event's rules: the viewer's cut keeps the gardener, and its view is at the Plan level or below
// with the gardener in ids, or it has declared none. A newer start has a caption of its own, so a stale one is let go,
// except from the call's transcript row (transcript.go), which takes it whichever start is newest.
func (a *activity) lateCaption(agent string, start uint64, caption, key string, now time.Time) {
	if caption == "" || start == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.agents[agent]
	if st == nil {
		return
	}
	a.lateRowLocked(agent, st, start, key, clipWords(caption, captionMax)) // the transcript keeps it, newest or not
	caption = clipWords(caption, a.wireCap)
	if caption == "" || st.start != start {
		return
	}
	st.cap = caption
	if st.last.started() == 1 {
		st.last.Cap = caption // the newest event is that start
	}
	for k := range st.fold {
		if f := &st.fold[k]; f.pending != nil && f.starts > 0 {
			f.cap = caption // the fold's newest start is the agent's newest
		}
	}
	a.epoch++
	g, ok := a.gardeners[agent]
	if !ok {
		return
	}
	d, ok := a.doingLocked(agent, now)
	if !ok {
		return
	}
	var data, scoped []byte
	for _, c := range a.clients {
		if (c.keep != nil && !c.keep(g)) || (c.view != nil && (c.view.busy || !c.view.ids[agent])) {
			continue
		}
		if data == nil {
			data, _ = json.Marshal(FarmDoingOf{ID: agent, FarmDoing: d})
			scoped, _ = json.Marshal(FarmDoingOf{ID: agent, FarmDoing: d.masked()})
		}
		m := ActivityMsg{Event: DoingEvent, Data: data}
		if c.keep != nil {
			m.Data = scoped
		}
		a.sendLocked(c, m)
	}
}

// callKey pairs a finish with its start: the tool_use_id, else the tool word (a harness that sends no id).
func callKey(ev HookActivity) string {
	if ev.CallID != "" {
		return ev.CallID
	}
	return "tool:" + ev.Tool
}

// flush sends every folded event that is due, notes calls that turned into waiting, and forgets agents long gone.
func (a *activity) flush(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, st := range a.agents {
		for k := range st.fold {
			f := &st.fold[k]
			if f.pending != nil && !now.Before(f.due) {
				e := *f.pending
				if f.folded > 1 && f.starts != e.started() {
					e.N = f.starts
				}
				if f.starts > 0 {
					e.Cap = f.cap // a fold carries its newest start's caption, not its newest event's
				}
				f.pending, f.sentAt = nil, now
				a.emitLocked(e, k)
			}
		}
		if !st.waitSeen && st.last.started() == 1 && len(st.open) > 0 && now.Sub(st.lastAt) >= activityWaiting {
			st.waitSeen = true
			a.epoch++
		}
		for k, c := range st.open {
			if now.Sub(c.at) >= activityOpenForget {
				delete(st.open, k)
				a.writeRowLocked(id, st, CallRow{At: c.at.Unix(), Class: c.doing, Tool: c.tool, Cap: c.cap}) // never finished
			}
		}
		for k, t := range st.subs {
			if now.Sub(t) >= activityOpenForget {
				delete(st.subs, k)
			}
		}
		a.writeDueLocked(id, st, now)
		if _, live := a.gardeners[id]; !live && now.Sub(st.seen) >= activityForget {
			delete(a.agents, id)
		}
	}
	for _, c := range a.clients {
		if c.view != nil && c.view.busy && now.Sub(c.busyAt) >= activityBusyEvery {
			a.busyLocked(c, c.busyAt, now)
		}
		if now.Sub(c.tierAt) >= activityFoldEvery {
			a.retierLocked(c, now)
		}
		a.sendCallsLocked(c, now)
	}
}

// retierLocked puts c on the fold its view needs at now (gd-iAq4va9XDt): the shortest at which one event per gap for
// each busy gardener in it (one its cut keeps, heard in the last activityFoldWindow) is at most a.rate a second. A
// view below the Plan level is sent no calls, so it is on none.
func (a *activity) retierLocked(c *actClient, now time.Time) {
	c.tierAt = now
	k := -1
	if c.view == nil || !c.view.busy {
		n := 0
		busy := func(id string, g FarmGardener) {
			if st := a.agents[id]; st != nil && now.Sub(st.seen) < activityFoldWindow && (c.keep == nil || c.keep(g)) {
				n++
			}
		}
		if c.view == nil {
			for id, g := range a.gardeners {
				busy(id, g)
			}
		} else {
			for id := range c.view.ids {
				if g, ok := a.gardeners[id]; ok {
					busy(id, g)
				}
			}
		}
		k = 0
		for k < activityFoldTiers-1 && int64(n)*int64(time.Second) > int64(a.rate)*int64(activityGap(k)) {
			k++
		}
	}
	a.setTierLocked(c, k)
}

// setTierLocked moves c to fold k (-1: none).
func (a *activity) setTierLocked(c *actClient, k int) {
	if c.tier == k {
		return
	}
	if c.tier >= 0 {
		a.tiers[c.tier]--
	}
	if k >= 0 {
		a.tiers[k]++
	}
	c.tier = k
}

// busyLocked sends c the projects of the gardeners its cut keeps that were heard after since, if there are any.
func (a *activity) busyLocked(c *actClient, since, now time.Time) {
	c.busyAt = now
	var b FarmBusy
	for id, g := range a.gardeners {
		st := a.agents[id]
		if st == nil || g.Project == "" || !st.seen.After(since) || (c.keep != nil && !c.keep(g)) {
			continue
		}
		ago := int64(now.Sub(st.seen) / time.Second)
		if was, ok := b.Busy[g.Project]; !ok || ago < was {
			if b.Busy == nil {
				b.Busy = map[string]int64{}
			}
			b.Busy[g.Project] = ago
		}
	}
	if b.Busy != nil {
		data, _ := json.Marshal(b)
		a.sendLocked(c, ActivityMsg{Event: BusyEvent, Data: data})
	}
}

// emitLocked fans one event, folded at tier k, out to the viewers on that fold whose cut keeps its gardener and whose
// view has it. An agent that is not a gardener (a pool shell, one on its way out, one past the gardener cap) is sent to
// nobody; nor is any call to a view below the Plan level (it hears the busy projects instead).
func (a *activity) emitLocked(e FarmActivity, k int) {
	g, ok := a.gardeners[e.ID]
	if !ok || a.tiers[k] == 0 {
		return
	}
	var data, scoped []byte
	for _, c := range a.clients {
		if c.tier != k || (c.keep != nil && !c.keep(g)) || (c.view != nil && (c.view.busy || !c.view.ids[e.ID])) {
			continue
		}
		if data == nil {
			data, _ = json.Marshal(e)
			scoped = data
			if m := maskScoped(e.Cap); m != e.Cap {
				se := e
				se.Cap = m
				scoped, _ = json.Marshal(se)
			}
		}
		m := ActivityMsg{Event: ActivityEvent, Data: data}
		if c.keep != nil {
			m.Data = scoped // a scoped viewer: its caption masked (maskScoped)
		}
		if a.sendLocked(c, m) {
			a.tierBytes[k] += uint64(m.size())
		}
	}
}

// sendLocked queues m for c, or drops it when c's queue is full; false when it was dropped.
func (a *activity) sendLocked(c *actClient, m ActivityMsg) bool {
	select {
	case c.ch <- m:
		a.sent++
		a.bytes += uint64(m.size())
		return true
	default:
		a.dropped++ // ephemeral: a laggard misses a bird, never a fact
		return false
	}
}

// setGardeners replaces the set an event must belong to. gs is not modified afterwards (each refresh builds a new map).
func (a *activity) setGardeners(gs map[string]FarmGardener) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gs == nil {
		gs = map[string]FarmGardener{}
	}
	a.gardeners = gs
	a.epoch++
}

// doings is gardeners[].doing for the ids in want at now, and the epoch it reflects.
func (a *activity) doings(want map[string]entry, now time.Time) (map[string]FarmDoing, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]FarmDoing{}
	for id := range want {
		if d, ok := a.doingLocked(id, now); ok {
			out[id] = d
		}
	}
	return out, a.epoch
}

// doingLocked is gardeners[].doing for id at now; false when nothing was heard from it.
func (a *activity) doingLocked(id string, now time.Time) (FarmDoing, bool) {
	st := a.agents[id]
	if st == nil || st.lastAt.IsZero() {
		return FarmDoing{}, false
	}
	d := FarmDoing{Class: st.last.Doing, Tool: st.last.Tool, At: st.last.At, Calls: st.calls, Cap: st.cap}
	switch {
	case len(st.subs) > 0:
		first := now
		for _, t := range st.subs {
			if t.Before(first) {
				first = t
			}
		}
		d.Class, d.Tool, d.At = DoingDelegate, toolSubagent, first.Unix()
	case st.last.started() == 1 && len(st.open) > 0 && now.Sub(st.lastAt) >= activityWaiting:
		d.Class = DoingWaiting
	}
	return d, true
}

func (a *activity) currentEpoch() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.epoch
}

// subscribe registers a viewer. keep is its cut (nil keeps every gardener). sid names the stream for setView: random,
// so only the page it was handed to can narrow it. cancel closes the channel.
func (a *activity) subscribe(keep func(FarmGardener) bool) (sid string, ch <-chan ActivityMsg, cancel func()) {
	sid = rand.Text()
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.nextID
	a.nextID++
	c := &actClient{ch: make(chan ActivityMsg, activityClientBuffer), keep: keep, tier: -1}
	a.clients[id] = c
	a.sids[sid] = id
	a.setTierLocked(c, 0) // until the next flush chooses its fold
	return sid, c.ch, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if c, ok := a.clients[id]; ok {
			a.setTierLocked(c, -1)
			if c.tr != nil {
				a.trClients--
			}
			delete(a.clients, id)
			delete(a.sids, sid)
			close(c.ch)
		}
	}
}

// setView narrows stream sid to what its page can see; false when there is no such stream. As the view drops below
// the Plan level the page is told at once which projects were busy in the last activityBusyWindow; each gardener that
// comes into a view at the Plan level or below (one its cut keeps, with something heard) is sent its doing.
func (a *activity) setView(sid string, v ActivityView, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cid, ok := a.sids[sid]
	c := a.clients[cid]
	if !ok || c == nil {
		return false
	}
	a.views++
	was := c.view
	if v.Level < viewPlanLevel {
		c.view = &actView{busy: true}
		a.retierLocked(c, now)
		if was == nil || !was.busy {
			a.busyLocked(c, now.Add(-activityBusyWindow), now)
		}
		return true
	}
	nv := &actView{ids: make(map[string]bool, min(len(v.IDs), maxViewIDs))}
	for _, id := range v.IDs {
		if len(nv.ids) >= maxViewIDs {
			break
		}
		if id != "" && len(id) <= maxViewID {
			nv.ids[id] = true
		}
	}
	c.view = nv
	a.retierLocked(c, now)
	if was == nil {
		return true // it was sent every call: what it holds is current
	}
	for id := range nv.ids {
		g, ok := a.gardeners[id]
		if !ok || was.ids[id] || (c.keep != nil && !c.keep(g)) {
			continue
		}
		if d, ok := a.doingLocked(id, now); ok {
			if c.keep != nil {
				d = d.masked()
			}
			data, _ := json.Marshal(FarmDoingOf{ID: id, FarmDoing: d})
			a.sendLocked(c, ActivityMsg{Event: DoingEvent, Data: data})
		}
	}
	return true
}

func (a *activity) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, c := range a.clients {
		a.setTierLocked(c, -1)
		close(c.ch)
		if c.tr != nil {
			a.trClients--
		}
		delete(a.clients, id)
	}
	clear(a.sids)
}

// withDoing returns the gardener entries with doing spliced into each one that has it. A patch never carries doing:
// a client keeps it beside the entry and moves it on with activity events.
func withDoing(gs map[string]entry, ds map[string]FarmDoing) map[string]entry {
	if len(ds) == 0 {
		return gs
	}
	out := make(map[string]entry, len(gs))
	for id, e := range gs {
		d, ok := ds[id]
		if !ok || len(e.raw) < 2 || e.raw[len(e.raw)-1] != '}' {
			out[id] = e
			continue
		}
		raw, _ := json.Marshal(d)
		b := make([]byte, 0, len(e.raw)+len(raw)+10)
		b = append(b, e.raw[:len(e.raw)-1]...)
		b = append(b, `,"doing":`...)
		b = append(b, raw...)
		b = append(b, '}')
		out[id] = entry{key: e.key, raw: b}
	}
	return out
}
