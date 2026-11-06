package commons

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/toollatency"
)

// The jobs record (Commons v2 step 3, gd-qlk31NBV6o): each agent's last tool
// calls as the Jobs rail shows them, a structured middle ground between the
// agent's transcript and the raw PTY in the Glass. beads-viewer feeds it the
// hook events `gb hook relay` publishes (cmd/beads-viewer/activity_feed.go, the
// feed the farm's activity already reads): a PreToolUse starts a job, its
// PostToolUse or PostToolUseFailure finishes it. Each job is the call's
// command (redacted), exit code, duration_ms, output bytes and the first lines
// of its output (redacted), keyed by the agent bead the event names.
//
// It is kept in memory beside the hub, as the farm's transcript ring is
// (gardenhub transcript.go): a restart empties it. It is not written to the
// agent bead: a bead write per tool call, across the fleet, would be a bead
// event per call on every subscriber's firehose. The agent's node fragment
// serves it (node.go), and each job started or finished goes out on the event
// stream as a "job" event to the streams that watch the agent (stream.go).
//
// The agent's land is read from its bead as the system and memoized, never
// taken from the hook payload; a payload that names no agent bead, or one that
// is not an agent, is dropped.

const (
	// jobsKept is the jobs one agent's ring keeps, newest last.
	jobsKept = 50
	// jobsAgentsMax bounds the agents remembered; past it the one heard from
	// least recently is forgotten.
	jobsAgentsMax = 2048
	// jobsForget is how long an agent not heard from is remembered.
	jobsForget = time.Hour
	// jobHeadLines and jobLineMax bound the output lines a job keeps.
	jobHeadLines = 4
	jobLineMax   = 200
	// jobCommandMax is the rune cap on a job's command.
	jobCommandMax = 300
	// jobsQueue is the depth between the hook feed and the worker that reads
	// each agent's land.
	jobsQueue = 1024
	// jobAgentTTL is how long an agent bead's land and type are memoized.
	jobAgentTTL = time.Minute
)

// kindJob is the event of a job an agent started or finished.
const kindJob = "job"

// Job is one tool call of an agent.
type Job struct {
	ID         string    `json:"id"` // the call's tool_use_id
	Tool       string    `json:"tool"`
	Command    string    `json:"command,omitempty"`
	Start      time.Time `json:"start"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	// Exit is a shell call's exit code: 0 for a PostToolUse, the failure's
	// "Exit code N" for a PostToolUseFailure. Nil for any other tool, and while
	// the job runs.
	Exit *int `json:"exit,omitempty"`
	// OK is nil while the job runs.
	OK       *bool    `json:"ok,omitempty"`
	OutBytes int      `json:"out_bytes,omitempty"`
	Head     []string `json:"head,omitempty"`
}

// Running reports whether the job has not finished.
func (j Job) Running() bool { return j.OK == nil }

// Failed reports whether the job finished and failed.
func (j Job) Failed() bool { return j.OK != nil && !*j.OK }

// Jobs is the jobs record. A nil *Jobs is safe: it observes nothing and
// serves no jobs.
type Jobs struct {
	beads  BeadGetter
	hub    *Hub
	logger *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	agents map[string]*agentJobs
	memo   map[string]jobAgent

	queue  chan []byte
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type agentJobs struct {
	land  string
	ring  []Job
	count int // jobs started or finished since the record first heard of the agent
	heard time.Time
}

type jobAgent struct {
	land    string // "" when the bead is not an agent with one land
	expires time.Time
}

// NewJobs returns a jobs record that reads agent beads with beads and sends
// each job to hub's streams. It returns nil when beads is nil.
func NewJobs(beads BeadGetter, hub *Hub, logger *slog.Logger) *Jobs {
	if beads == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Jobs{beads: beads, hub: hub, logger: logger, now: time.Now,
		agents: map[string]*agentJobs{}, memo: map[string]jobAgent{}, queue: make(chan []byte, jobsQueue)}
}

// Start runs the worker. Safe on nil.
func (j *Jobs) Start() {
	if j == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-j.queue:
				j.observe(ctx, data)
			}
		}
	}()
}

// Close stops the worker. Safe on nil.
func (j *Jobs) Close() {
	if j == nil || j.cancel == nil {
		return
	}
	j.cancel()
	j.wg.Wait()
}

// Observe takes one hook event payload. It never blocks: a full queue drops
// the event. Safe on nil.
func (j *Jobs) Observe(data []byte) {
	if j == nil {
		return
	}
	select {
	case j.queue <- data:
	default:
		j.logger.Warn("commons jobs: queue full, dropping a hook event")
	}
}

// Of returns the agent's jobs, newest first, and how many it has had.
func (j *Jobs) Of(agent string) ([]Job, int) {
	if j == nil {
		return nil, 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.agents[agent]
	if a == nil {
		return nil, 0
	}
	out := make([]Job, len(a.ring))
	for i, jb := range a.ring {
		out[len(a.ring)-1-i] = jb
	}
	return out, a.count
}

// Count is how many jobs the agent has had.
func (j *Jobs) Count(agent string) int {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if a := j.agents[agent]; a != nil {
		return a.count
	}
	return 0
}

// hookJob is the slice of a gb hook relay event (cmd/gb/hook_relay.go
// hookRelayEvent) a job is made of.
type hookJob struct {
	Event        string `json:"event"`
	TS           string `json:"ts"`
	AgentBeadID  string `json:"agent_bead_id"`
	ToolName     string `json:"tool_name"`
	ToolInput    any    `json:"tool_input"`
	ToolResponse any    `json:"tool_response"`
	ToolUseID    string `json:"tool_use_id"`
	DurationMs   int64  `json:"duration_ms"`
	OutputBytes  int    `json:"output_bytes"`
	Description  string `json:"tool_description"`
	FilePath     string `json:"tool_file_path"`
	Error        string `json:"error"`
}

func (j *Jobs) observe(ctx context.Context, data []byte) {
	var e hookJob
	if json.Unmarshal(data, &e) != nil || e.AgentBeadID == "" || e.ToolUseID == "" {
		return
	}
	switch e.Event {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure":
	default:
		return
	}
	land := j.landOf(ctx, e.AgentBeadID)
	if land == "" {
		return
	}
	at := j.now()
	if t, err := time.Parse(time.RFC3339Nano, e.TS); err == nil {
		at = t
	}
	jb, ok := j.record(land, e, at)
	if !ok {
		return
	}
	j.hub.Publish(Invalidation{Kind: kindJob, ID: e.AgentBeadID, Land: land, At: j.now(), job: &jb})
}

// record writes e into its agent's ring and returns the job as it stands.
func (j *Jobs) record(land string, e hookJob, at time.Time) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.agents[e.AgentBeadID]
	if a == nil {
		j.forget(at)
		a = &agentJobs{}
		j.agents[e.AgentBeadID] = a
	}
	a.land, a.heard = land, at
	i := len(a.ring) - 1
	for ; i >= 0 && a.ring[i].ID != e.ToolUseID; i-- {
	}
	if e.Event == "PreToolUse" {
		if i >= 0 {
			return Job{}, false // a repeat
		}
		jb := Job{ID: e.ToolUseID, Tool: e.ToolName, Command: jobCommand(e), Start: at}
		a.push(jb)
		return jb, true
	}
	var jb Job
	if i >= 0 {
		jb = a.ring[i]
		if !jb.Running() {
			return Job{}, false
		}
	} else {
		jb = Job{ID: e.ToolUseID, Tool: e.ToolName, Command: jobCommand(e), Start: at.Add(-time.Duration(e.DurationMs) * time.Millisecond)}
	}
	finish(&jb, e, at)
	if i >= 0 {
		a.ring[i] = jb
	} else {
		a.push(jb)
	}
	return jb, true
}

func (a *agentJobs) push(jb Job) {
	if len(a.ring) >= jobsKept {
		a.ring = append(a.ring[:0], a.ring[1:]...)
	}
	a.ring = append(a.ring, jb)
	a.count++
}

// forget drops the agents not heard from in jobsForget and, when the record is
// full, the one heard from least recently. Under mu.
func (j *Jobs) forget(now time.Time) {
	var oldest string
	for id, a := range j.agents {
		if now.Sub(a.heard) > jobsForget {
			delete(j.agents, id)
			continue
		}
		if oldest == "" || a.heard.Before(j.agents[oldest].heard) {
			oldest = id
		}
	}
	if len(j.agents) >= jobsAgentsMax && oldest != "" {
		delete(j.agents, oldest)
	}
}

// landOf reads the agent bead's land as the system, memoized for jobAgentTTL;
// "" for a bead that is not an agent with one land.
func (j *Jobs) landOf(ctx context.Context, id string) string {
	now := j.now()
	j.mu.Lock()
	m, hit := j.memo[id]
	j.mu.Unlock()
	if hit && now.Before(m.expires) {
		return m.land
	}
	rctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(ctx), lookupTimeout)
	defer cancel()
	b, err := j.beads.GetBead(rctx, id)
	if err != nil {
		return "" // not memoized: the next event asks again
	}
	land := ""
	if b != nil && b.Type == "agent" {
		land = landOf(b.Labels)
	}
	j.mu.Lock()
	if len(j.memo) >= jobsAgentsMax {
		j.memo = map[string]jobAgent{}
	}
	j.memo[id] = jobAgent{land: land, expires: now.Add(jobAgentTTL)}
	j.mu.Unlock()
	return land
}

// exitCode is how Claude Code opens a failed shell call's error.
var exitCode = regexp.MustCompile(`^Exit code (\d+)`)

// finish fills a job's ending from its PostToolUse or PostToolUseFailure.
func finish(jb *Job, e hookJob, at time.Time) {
	ok := e.Event == "PostToolUse"
	jb.OK = &ok
	jb.DurationMs = e.DurationMs
	if jb.DurationMs <= 0 && !jb.Start.IsZero() && at.After(jb.Start) {
		jb.DurationMs = at.Sub(jb.Start).Milliseconds()
	}
	text := e.Error
	if ok {
		text = responseText(e.ToolResponse)
	}
	shell := e.ToolName == "Bash"
	if shell && ok {
		zero := 0
		jb.Exit = &zero
	}
	if shell && !ok {
		if m := exitCode.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				jb.Exit = &n
			}
			text = strings.TrimPrefix(text[len(m[0]):], "\n")
		}
	}
	jb.OutBytes = e.OutputBytes
	if jb.OutBytes <= 0 {
		jb.OutBytes = len(text)
	}
	jb.Head = headLines(text)
}

// cutField finds a string field's start in a JSON text the relay cut at 1024
// bytes ({"stderr":"…","stdout":"… or {"command":"…), keys in json.Marshal's
// sorted order: the value up to its end, or to the cut.
func cutField(text, key string) (string, bool) {
	k := `"` + key + `":"`
	i := strings.Index(text, k)
	if i < 0 {
		return "", false
	}
	raw := text[i+len(k):]
	var b strings.Builder
	for n := 0; n < len(raw); n++ {
		c := raw[n]
		if c == '"' {
			break
		}
		if c == '\\' && n+1 < len(raw) {
			n++
			switch raw[n] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if n+4 < len(raw) {
					if r, err := strconv.ParseUint(raw[n+1:n+5], 16, 32); err == nil {
						b.WriteRune(rune(r))
					}
					n += 4
				}
			default:
				b.WriteByte(raw[n])
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

// responseText is a tool response's output: a shell call's stdout, else its
// stderr, else the text it is.
func responseText(v any) string {
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(t, "{") { // the relay's cut of a larger response
			for _, k := range []string{"stdout", "stderr", "output", "content"} {
				if s, ok := cutField(t, k); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
		return t
	case map[string]any:
		for _, k := range []string{"stdout", "stderr", "output", "content"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// headLines is the first non-blank lines of text, redacted and clipped.
func headLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, clipRunes(toollatency.RedactText(l), jobLineMax))
		if len(out) == jobHeadLines {
			break
		}
	}
	return out
}

// jobCommand is what a job ran: a shell call's command, else the file, pattern
// or address it worked on, else its description; redacted and clipped.
func jobCommand(e hookJob) string {
	cmd := ""
	switch in := e.ToolInput.(type) {
	case map[string]any:
		for _, k := range []string{"command", "file_path", "pattern", "url", "query", "description"} {
			if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
				cmd = s
				break
			}
		}
	case string:
		cmd, _ = cutField(in, "command")
	}
	cmd = firstNonEmpty(cmd, e.FilePath, e.Description)
	return clipRunes(strings.TrimSpace(toollatency.RedactText(cmd)), jobCommandMax)
}
