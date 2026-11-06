package gardenhub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The caption on the activity wire (step 2 of plan gd-jMl4oF2cJO; spec 2026-10-28-bird-captions-design.md section 7):
// cap rides an event whose calls started, a fold carries its newest start's, gardeners[].doing carries the newest
// started call's, and a scoped viewer gets bead ids masked.

func preCap(agent, tool, id, caption string, at time.Time) HookActivity {
	e := pre(agent, tool, id, at)
	e.Cap = caption
	return e
}

func TestParseHookEventCaption(t *testing.T) {
	now := time.Unix(1792870000, 0)
	for _, c := range []struct{ name, raw, want string }{
		{"description in tool_input", `{"event":"PreToolUse","agent_bead_id":"gd-a","tool_name":"Bash","tool_input":{"command":"kd show x","description":"Show the plan"}}`, "Show the plan"},
		{"the relay's copy wins", `{"event":"PreToolUse","agent_bead_id":"gd-a","tool_name":"Bash","tool_input":{"command":"x","description":"old"},"tool_description":"Rebase on main"}`, "Rebase on main"},
		{"a cut tool_input with the relay's copy", `{"event":"PreToolUse","agent_bead_id":"gd-a","tool_name":"Write","tool_input":"{\"content\":\"xxxx...","tool_file_path":"/home/agent/bot/gardenio/work/docs/a.md"}`, "Write docs/a.md"},
		// An old relay cut an Agent's input to a string; its description sorts first, so it is still read.
		{"a cut Agent input from an old relay", `{"event":"PreToolUse","agent_bead_id":"gd-a","tool_name":"Agent","tool_input":"{\"description\":\"Explore \\\"gardenhub\\\" endpoints\",\"prompt\":\"long...` + `"}`, `Explore "gardenhub" endpoints`},
		{"a cut description is lost, not half read", `{"event":"PreToolUse","agent_bead_id":"gd-a","tool_name":"Bash","tool_input":"{\"command\":\"x\",\"description\":\"Half a sen...` + `"}`, ""},
		{"Gemini run_shell_command (fixture shape)", `{"event":"PreToolUse","harness":"gemini","agent_bead_id":"gd-a","tool_name":"run_shell_command","tool_input":{"command":"echo hi","description":"mock tool call"},"tool_description":"mock tool call"}`, "mock tool call"},
		{"Codex Bash (fixture shape): no description", `{"event":"PreToolUse","harness":"codex","agent_bead_id":"gd-a","tool_name":"Bash","tool_input":{"command":"echo hello-from-tool"},"tool_use_id":"call_398c5e51"}`, ""},
		{"a finish has none", `{"event":"PostToolUse","agent_bead_id":"gd-a","tool_name":"Bash","tool_input":{"command":"x","description":"Show the plan"},"tool_description":"Show the plan"}`, ""},
		{"a subagent start has none", `{"event":"SubagentStart","agent_bead_id":"gd-a","subagent_id":"sa1","tool_description":"x"}`, ""},
	} {
		ev, ok := ParseHookEvent([]byte(c.raw), now)
		if !ok || ev.Cap != c.want {
			t.Errorf("%s: cap %q (ok %v), want %q", c.name, ev.Cap, ok, c.want)
		}
	}
	// A truncated Bash input still classifies as before (other), and the class and tool word are unchanged by a caption.
	ev, _ := ParseHookEvent([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-a","tool_name":"Bash","tool_input":{"command":"go test ./x","description":"Run the tests"}}`), now)
	if ev.Doing != DoingTest || ev.Tool != "go_test" || ev.Src != CapSrcDescription {
		t.Errorf("Bash go test with a description: %+v", ev)
	}
}

func TestActivityCaptionFold(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	_, ch, cancel := a.subscribe(nil)
	defer cancel()

	a.observe(preCap("gd-a", "Bash", "b1", "Show the plan", c.t), c.t)
	got := drain(ch)
	if len(got) != 1 || got[0].Cap != "Show the plan" {
		t.Fatalf("a start goes at once with its caption: %+v", got)
	}

	// Inside the gap: two starts, then the first one's finish. The fold is the finish (the newest event) with the newest
	// start's caption, not the finish's (none) nor the oldest start's.
	a.observe(preCap("gd-a", "Bash", "b2", "Rebase on main", c.add(10*time.Millisecond)), c.t)
	a.observe(preCap("gd-a", "Read", "r1", "Read web/birds.js", c.add(10*time.Millisecond)), c.t)
	a.observe(post("gd-a", "Bash", "b1", c.add(10*time.Millisecond)), c.t)
	a.flush(c.add(activityMinGap))
	got = drain(ch)
	if len(got) != 1 || got[0].Dur == 0 || got[0].N != 2 || got[0].Cap != "Read web/birds.js" {
		t.Fatalf("the fold carries its newest start's caption: %+v", got)
	}

	// A fold of finishes only carries none, even though a start before it had one.
	a.observe(post("gd-a", "Bash", "b2", c.add(10*time.Millisecond)), c.t)
	a.observe(post("gd-a", "Read", "r1", c.add(10*time.Millisecond)), c.t)
	a.flush(c.add(activityMinGap))
	for _, e := range drain(ch) {
		if e.Cap != "" {
			t.Errorf("an event that started no call has a caption: %+v", e)
		}
	}

	// A thought carries none; doing keeps the newest started call's.
	c.add(activityMinGap)
	a.observe(HookActivity{Agent: "gd-a", Event: HookThinking, At: c.t}, c.t)
	if got := drain(ch); len(got) != 1 || got[0].Cap != "" || got[0].Doing != DoingThink {
		t.Errorf("a thought: %+v", got)
	}
	a.mu.Lock()
	d, _ := a.doingLocked("gd-a", c.t)
	a.mu.Unlock()
	if d.Cap != "Read web/birds.js" || d.Calls != 3 {
		t.Errorf("doing: %+v", d)
	}

	// A start with no caption clears the doing's: it is the newest started call's, not the newest caption heard.
	a.observe(preCap("gd-a", "Bash", "b3", "", c.add(activityMinGap)), c.t)
	if got := drain(ch); len(got) != 1 || got[0].Cap != "" {
		t.Errorf("a start with no caption: %+v", got)
	}
	a.mu.Lock()
	d, _ = a.doingLocked("gd-a", c.t)
	a.mu.Unlock()
	if d.Cap != "" {
		t.Errorf("doing kept an older call's caption: %+v", d)
	}

	// The wire cap.
	long := strings.Repeat("word ", 40)
	a.observe(preCap("gd-a", "Bash", "b4", long, c.add(activityMinGap)), c.t)
	if got := drain(ch); len(got) != 1 || len([]rune(got[0].Cap)) > wireCaptionMax {
		t.Errorf("a caption over the wire cap: %+v", got)
	}
}

func TestActivityCaptionMaskedForScopedViewer(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a", Project: "alpha"}})
	_, admin, c1 := a.subscribe(nil)
	defer c1()
	sid, scoped, c2 := a.subscribe(func(g FarmGardener) bool { return g.Project == "alpha" })
	defer c2()

	a.observe(preCap("gd-a", "Bash", "b1", "Claim gd-gCHKhIROvB and read ka-0000004089", c.t), c.t)
	if got := drain(admin); len(got) != 1 || got[0].Cap != "Claim gd-gCHKhIROvB and read ka-0000004089" {
		t.Errorf("an unrestricted viewer keeps bead ids: %+v", got)
	}
	if got := drain(scoped); len(got) != 1 || got[0].Cap != "Claim a bead and read a bead" {
		t.Errorf("a scoped viewer gets them masked: %+v", got)
	}

	// The doing event as the gardener comes into a scoped view.
	a.setView(sid, ActivityView{Level: 2, IDs: []string{"gd-x"}}, c.t)
	a.setView(sid, ActivityView{Level: 2, IDs: []string{"gd-a"}}, c.t)
	m := msgs(scoped)
	if len(m[DoingEvent]) != 1 {
		t.Fatalf("one doing event: %v", m)
	}
	var d FarmDoingOf
	if err := json.Unmarshal(m[DoingEvent][0].Data, &d); err != nil || d.Cap != "Claim a bead and read a bead" || d.Class != DoingShell {
		t.Errorf("scoped doing event: %s", m[DoingEvent][0].Data)
	}
}

func TestFarmCutDoingCaptionMasked(t *testing.T) {
	f := newCutFarm(t, multiLandWorld())
	now := f.now()
	f.act.observe(preCap("gd-gZeta", "Bash", "b1", "Close gd-gCHKhIROvB", now), now)

	doingOf := func(raw []byte) *FarmDoing {
		var doc struct {
			Gardeners []struct {
				ID    string     `json:"id"`
				Doing *FarmDoing `json:"doing"`
			} `json:"gardeners"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, g := range doc.Gardeners {
			if g.ID == "gd-gZeta" {
				return g.Doing
			}
		}
		return nil
	}
	_, _, raw, _ := cutDoc(t, f, "zeta")
	if d := doingOf(raw); d == nil || d.Cap != "Close a bead" {
		t.Errorf("scoped document: doing %+v", d)
	}
	_, admin, _, _ := f.CutSnapshot(FarmCut{})
	if d := doingOf(admin); d == nil || d.Cap != "Close gd-gCHKhIROvB" {
		t.Errorf("unrestricted document: doing %+v", d)
	}
}
