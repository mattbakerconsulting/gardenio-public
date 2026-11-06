package gardenhub

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The activity feed (gd-H4jk9KgUAG, step 4a of plan gd-gAKb8Bf0Nm): a hook event becomes a bounded doing class, an
// ephemeral SSE event at most 4 a second per gardener, cut per viewer, and gardeners[].doing on the document. Nothing of
// tool_input ever reaches the wire.

func TestDoingOfClassTable(t *testing.T) {
	for _, c := range []struct{ tool, cmd, want string }{
		{"Read", "", DoingRead}, {"Grep", "", DoingRead}, {"Glob", "", DoingRead}, {"LSP", "", DoingRead},
		{"Edit", "", DoingEdit}, {"Write", "", DoingEdit}, {"NotebookEdit", "", DoingEdit},
		{"WebFetch", "", DoingWeb}, {"WebSearch", "", DoingWeb},
		{"Agent", "", DoingDelegate}, {"Task", "", DoingDelegate},
		{"Bash", "go_test", DoingTest}, {"Bash", "cargo_test", DoingTest}, {"Bash", "npm_test", DoingTest}, {"Bash", "make", DoingTest},
		{"Bash", "go_build", DoingBuild}, {"Bash", "cargo_build", DoingBuild}, {"Bash", "docker_build", DoingBuild},
		{"Bash", "git_push", DoingGit}, {"Bash", "git_other", DoingGit}, {"Bash", "gh_pr", DoingGit},
		{"Bash", "kd_create", DoingBeads}, {"Bash", "gb_squawk", DoingBeads}, {"Bash", "gb_other", DoingBeads},
		{"Bash", "helm_upgrade", DoingDeploy}, {"Bash", "kubectl_get", DoingDeploy}, {"Bash", "aws_sts", DoingDeploy},
		{"Bash", "other", DoingShell}, {"Bash", "go_run", DoingShell}, {"Bash", "docker_run", DoingShell}, {"Bash", "", DoingShell},
		{"mcp__slack__post", "", DoingShell}, {"", "", DoingShell},
	} {
		if got := DoingOf(c.tool, c.cmd); got != c.want {
			t.Errorf("DoingOf(%q, %q) = %q, want %q", c.tool, c.cmd, got, c.want)
		}
	}
}

func TestParseHookEvent(t *testing.T) {
	now := time.Unix(1792870000, 0)
	ev, ok := ParseHookEvent([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-a","session_id":"s1","tool_name":"Bash","tool_input":{"command":"cd /w && FOO=1 go test ./x"},"tool_use_id":"tu1","ts":"`+now.Add(-time.Second).Format(time.RFC3339Nano)+`"}`), now)
	if !ok || ev.Doing != DoingTest || ev.Tool != "go_test" || ev.CallID != "tu1" || ev.Session != "s1" || !ev.At.Equal(now.Add(-time.Second)) {
		t.Fatalf("Bash go test: %+v %v", ev, ok)
	}
	ev, ok = ParseHookEvent([]byte(`{"event":"PostToolUse","agent_bead_id":"gd-a","tool_name":"mcp__a b/c<d>__x","ts":"2099-01-01T00:00:00Z"}`), now)
	if !ok || ev.Tool != "mcp__abcd__x" || !ev.At.Equal(now) {
		t.Errorf("a tool name is kept to [A-Za-z0-9_.-]; a ts from the future is stamped on arrival: %+v", ev)
	}
	ev, ok = ParseHookEvent([]byte(`{"event":"SubagentStart","agent_bead_id":"gd-a","subagent_id":"sa1","subagent_type":"Explore"}`), now)
	if !ok || ev.Doing != DoingDelegate || ev.Tool != toolSubagent || ev.CallID != "sa1" {
		t.Errorf("SubagentStart: %+v", ev)
	}
	for _, raw := range []string{
		`{"event":"PreToolUse","tool_name":"Read"}`,                        // no agent bead
		`{"event":"Stop","agent_bead_id":"gd-a"}`,                          // not read
		`{"event":"UserPromptSubmit","agent_bead_id":"gd-a","prompt":"x"}`, // not read
		`not json`,
	} {
		if _, ok := ParseHookEvent([]byte(raw), now); ok {
			t.Errorf("parsed %s", raw)
		}
	}
}

// clock is a settable test time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time                { return c.t }
func (c *clock) add(d time.Duration) time.Time { c.t = c.t.Add(d); return c.t }

// drain is the activity events queued on ch (any other event is dropped).
func drain(ch <-chan ActivityMsg) []FarmActivity {
	var out []FarmActivity
	for {
		select {
		case m := <-ch:
			if m.Event != ActivityEvent {
				continue
			}
			var e FarmActivity
			_ = json.Unmarshal(m.Data, &e)
			out = append(out, e)
		default:
			return out
		}
	}
}

func pre(agent, tool, id string, at time.Time) HookActivity {
	return HookActivity{Agent: agent, Event: HookPreToolUse, Tool: tool, Doing: DoingOf(tool, ""), CallID: id, Session: "s", At: at}
}

func post(agent, tool, id string, at time.Time) HookActivity {
	e := pre(agent, tool, id, at)
	e.Event = HookPostToolUse
	return e
}

func TestActivityCoalescesToFourASecond(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	_, ch, cancel := a.subscribe(nil)
	defer cancel()

	// A run of 10 reads in 90 ms: the first goes at once, the other 9 fold into one event with their count.
	for i := 0; i < 10; i++ {
		a.observe(pre("gd-a", "Read", "r"+string(rune('0'+i)), c.t), c.t)
		c.add(10 * time.Millisecond)
	}
	got := drain(ch)
	if len(got) != 1 || got[0].N != 0 || got[0].Dur != 0 || got[0].Doing != DoingRead {
		t.Fatalf("first read goes at once, alone: %+v", got)
	}
	a.flush(c.t) // 100 ms after the first: not due
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("folded event sent before its 250 ms: %+v", got)
	}
	a.flush(c.add(150 * time.Millisecond))
	got = drain(ch)
	if len(got) != 1 || got[0].N != 9 || got[0].Doing != DoingRead || got[0].ID != "gd-a" {
		t.Fatalf("the fold is one event with n=9: %+v", got)
	}

	// A start and its finish folded: the finish, carrying the one start it folds.
	a.observe(pre("gd-a", "Edit", "e1", c.t), c.t)
	a.observe(post("gd-a", "Edit", "e1", c.add(40*time.Millisecond)), c.t)
	a.flush(c.add(250 * time.Millisecond))
	got = drain(ch)
	if len(got) != 1 || got[0].Doing != DoingEdit || got[0].Dur != 40 || got[0].N != 1 {
		t.Fatalf("start+finish fold: %+v", got)
	}

	// Sustained: 50 events a second for 2 s, flushed every 100 ms like the loop: at most 4 a second go out.
	start, sent := c.t, 0
	for i := 0; i < 100; i++ {
		a.observe(pre("gd-a", "Grep", "", c.t), c.t)
		if i%5 == 4 {
			a.flush(c.t)
		}
		sent += len(drain(ch))
		c.add(20 * time.Millisecond)
	}
	if secs := c.t.Sub(start).Seconds(); float64(sent) > 4*secs+1 {
		t.Errorf("%d events in %.1f s: more than 4 a second", sent, secs)
	}
	if sent < 6 {
		t.Errorf("only %d events in 2 s of steady work", sent)
	}
}

func TestActivityCutPerViewer(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a", Project: "alpha"}, "gd-b": {ID: "gd-b", Project: "beta"}})
	_, admin, c1 := a.subscribe(nil)
	defer c1()
	_, alpha, c2 := a.subscribe(func(g FarmGardener) bool { return g.Project == "alpha" })
	defer c2()

	for _, id := range []string{"gd-a", "gd-b", "gd-pool"} {
		a.observe(pre(id, "Read", "x", c.t), c.t)
	}
	ids := func(es []FarmActivity) (out []string) {
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}
	if got := ids(drain(admin)); strings.Join(got, ",") != "gd-a,gd-b" {
		t.Errorf("admin sees every gardener and no non-gardener: %v", got)
	}
	if got := ids(drain(alpha)); strings.Join(got, ",") != "gd-a" {
		t.Errorf("an alpha viewer sees only alpha's gardener: %v", got)
	}

	// A gardener that leaves the set is sent to nobody from then on.
	a.setGardeners(map[string]FarmGardener{"gd-b": {ID: "gd-b", Project: "beta"}})
	a.observe(post("gd-a", "Read", "x", c.add(time.Second)), c.t)
	if got := append(drain(admin), drain(alpha)...); len(got) != 0 {
		t.Errorf("event for a gardener no longer in the set: %+v", got)
	}
}

func TestFarmSnapshotDoing(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	gs := map[string]FarmGardener{"gd-a": {ID: "gd-a", Agent: "a"}, "gd-b": {ID: "gd-b", Agent: "b"}}
	f.mu.Lock()
	f.gardeners = gardenerEntries(gs)
	f.mu.Unlock()
	f.act.setGardeners(gs)

	doing := func() map[string]*FarmDoing {
		_, raw, _ := f.Snapshot()
		var doc struct {
			Gardeners []struct {
				ID    string     `json:"id"`
				Doing *FarmDoing `json:"doing"`
			} `json:"gardeners"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		out := map[string]*FarmDoing{}
		for _, g := range doc.Gardeners {
			out[g.ID] = g.Doing
		}
		return out
	}
	if d := doing(); d["gd-a"] != nil || d["gd-b"] != nil {
		t.Fatalf("no activity, no doing key: %+v", d)
	}
	tag0, _, _ := f.snapshotTag()

	f.act.observe(pre("gd-a", "Bash", "t1", c.t), c.t)
	f.act.observe(post("gd-a", "Bash", "t1", c.t), c.t)
	f.act.observe(pre("gd-a", "Read", "t2", c.t), c.t)
	c.add(activitySnapEvery) // the document re-bakes doing at most this often at one version
	if d := doing()["gd-a"]; d == nil || d.Class != DoingRead || d.Tool != "Read" || d.Calls != 2 || d.At != c.t.Add(-activitySnapEvery).Unix() {
		t.Fatalf("doing after two calls: %+v", d)
	}
	if tag, _, _ := f.snapshotTag(); tag == tag0 {
		t.Error("the ETag did not move with gardeners[].doing")
	}

	// The read hangs: waiting once it is 5 s old.
	c.add(activityWaiting)
	f.act.flush(c.t)
	c.add(activitySnapEvery)
	if d := doing()["gd-a"]; d == nil || d.Class != DoingWaiting || d.Tool != "Read" {
		t.Errorf("a call out for 5 s reads as waiting: %+v", d)
	}

	// A subagent: delegate until it stops.
	f.act.observe(HookActivity{Agent: "gd-b", Event: HookSubagentStart, Doing: DoingDelegate, Tool: toolSubagent, CallID: "sa", Session: "s", At: c.t}, c.t)
	f.act.observe(HookActivity{Agent: "gd-b", Event: HookPreToolUse, Doing: DoingRead, Tool: "Read", Session: "s", At: c.t, Sidechain: true}, c.t)
	c.add(activitySnapEvery)
	if d := doing()["gd-b"]; d == nil || d.Class != DoingDelegate || d.Calls != 0 {
		t.Errorf("delegating, and a sidechain call is not the gardener's: %+v", d)
	}
	f.act.observe(HookActivity{Agent: "gd-b", Event: HookSubagentStop, Doing: DoingDelegate, Tool: toolSubagent, CallID: "sa", Session: "s", At: c.t}, c.t)
	f.ObserveThinking("gd-b")
	c.add(activitySnapEvery)
	if d := doing()["gd-b"]; d == nil || d.Class != DoingThink || d.Tool != "" {
		t.Errorf("a thought with nothing out: think: %+v", d)
	}

	// A call never finished is let go after activityOpenForget: no longer waiting.
	c.add(activityOpenForget)
	f.act.flush(c.t)
	f.act.mu.Lock()
	open := len(f.act.agents["gd-a"].open)
	f.act.mu.Unlock()
	if open != 0 {
		t.Errorf("%d open calls kept past activityOpenForget", open)
	}

	// A new session restarts the count.
	e := pre("gd-a", "Edit", "n1", c.t)
	e.Session = "s2"
	f.act.observe(e, c.t)
	c.add(activitySnapEvery)
	if d := doing()["gd-a"]; d == nil || d.Calls != 1 || d.Class != DoingEdit {
		t.Errorf("new session: %+v", d)
	}
}

// secrets are planted in every part of a hook payload that is not the tool name, tool_input's description and
// file_path and their relay copies included; none may appear on the wire. A caption made of them is scrubbed first.
var activitySecrets = []string{"hunter2-SECRET", "/home/agent/private-path", "private-path", "sk-ant-PLANTED", "PLANTEDTOKEN", "s3cr3t-session", "tu_PLANTED", "promptPLANTED", "alice@example.com", "/pull/9", "notes-body"}

func TestActivityNoToolInputOnTheWire(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	gs := map[string]FarmGardener{"gd-a": {ID: "gd-a", Agent: "a"}}
	f.mu.Lock()
	f.gardeners = gardenerEntries(gs)
	f.mu.Unlock()
	f.act.setGardeners(gs)
	srv := httptest.NewServer(farmMux(&Handler{Farm: f}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/garden/farm/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitFor := func(prefix string) string {
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended waiting for %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				t.Fatalf("no %q line", prefix)
			}
		}
	}
	waitFor("event: reset")

	const head = `"agent_bead_id":"gd-a","session_id":"s3cr3t-session","prompt_id":"promptPLANTED","cwd":"/home/agent/private-path","tool_use_id":"tu_PLANTED"`
	payloads := []struct{ raw, want string }{
		// A relay older than tool_description: the description is read from tool_input, and scrubbed.
		{`{"event":"PreToolUse",` + head + `,"tool_name":"Bash","tool_input":{"command":"curl -H 'Authorization: Bearer PLANTEDTOKEN' https://x/?k=hunter2-SECRET","description":"Fetch /home/agent/private-path/page.html for alice@example.com using sk-ant-PLANTED12345","timeout":600000}}`, // gitleaks:allow: planted fake token for the redaction test
			`"doing":"shell","tool":"other","at":1792870000,"dur":0,"cap":"Fetch page.html for using [REDACTED]"}`},
		// The relay cut tool_input to a string; its copy of the description is read instead.
		{`{"event":"PreToolUse",` + head + `,"tool_name":"Bash","tool_input":"{\"command\":\"curl -H 'Authorization: Bearer PLANTEDTOKEN' https://x/?k=hunter2-SECRET ...","tool_description":"Trial-merge main into #1282 at https://github.com/x/y/pull/9?k=hunter2-SECRET"}`, // gitleaks:allow: planted fake token for the redaction test
			`"cap":"Trial-merge main into #1282 at github.com"}`},
		// An MCP call: its tool name in words; its input, description and all, is never read.
		{`{"event":"PreToolUse",` + head + `,"tool_name":"mcp__slack__post_message","tool_input":{"text":"hunter2-SECRET","description":"sk-ant-PLANTED12345 alice@example.com"}}`,
			`"cap":"slack: post message"}`},
		// A Write: its file by basename (outside the checkout); its content never.
		{`{"event":"PreToolUse",` + head + `,"tool_name":"Write","tool_input":"{\"content\":\"notes-body hunter2-SECRET sk-ant-PLANTED12345 ...","tool_file_path":"/home/agent/private-path/notes.txt"}`,
			`"doing":"edit","tool":"Write","at":1792870001,"dur":0,"cap":"Write notes.txt"}`},
		// A finish carries no caption.
		{`{"event":"PostToolUse",` + head + `,"tool_name":"Write","tool_input":{"file_path":"/home/agent/private-path/notes.txt","content":"notes-body hunter2-SECRET sk-ant-PLANTED"},"tool_description":"alice@example.com","tool_file_path":"/home/agent/private-path/x","tool_response":{"stdout":"Bearer PLANTEDTOKEN"},"duration_ms":1200}`,
			`"doing":"edit","tool":"Write","at":1792870001,"dur":1200}`},
	}
	var wire strings.Builder
	for _, p := range payloads {
		c.add(activityMinGap) // past the gap, so each goes at once
		f.ObserveHook([]byte(p.raw))
		ev := waitFor("event: activity")
		data := waitFor("data: ")
		wire.WriteString(ev + "\n" + data + "\n")
		if !strings.HasSuffix(data, p.want) {
			t.Errorf("activity event %s, want it to end %s", data, p.want)
		}
	}
	if strings.Contains(wire.String(), "id: ") {
		t.Error("an activity event carries an id: line, which would move Last-Event-ID off the patch chain")
	}

	c.add(activitySnapEvery) // the reset baked the document; doing re-bakes after this
	snap, err := http.Get(srv.URL + "/garden/farm")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(snap.Body)
	snap.Body.Close()
	if !strings.Contains(string(body), `"doing":{"class":"edit","tool":"Write","at":1792870001,"calls":4,"cap":"Write notes.txt"}`) {
		t.Fatalf("snapshot has no doing with the newest start's caption: %s", body)
	}
	all := wire.String() + string(body)
	for _, s := range activitySecrets {
		if strings.Contains(all, s) {
			t.Errorf("%q from the hook payload reached the wire", s)
		}
	}
	for _, e := range strings.Split(strings.TrimSpace(wire.String()), "\n") {
		d, ok := strings.CutPrefix(e, "data: ")
		if !ok {
			continue
		}
		var a FarmActivity
		_ = json.Unmarshal([]byte(d), &a)
		if n := len(d) - len(a.Cap) - len(a.Tool); n > 85 {
			t.Errorf("activity event is %d bytes besides its tool word and caption, want about 60: %s", n, d)
		}
	}
}

// farmMux registers the garden routes on a plain mux.
func farmMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}
