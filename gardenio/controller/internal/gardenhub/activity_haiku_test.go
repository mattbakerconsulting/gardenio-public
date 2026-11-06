package gardenhub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/gardenhub/captioner"
)

// The Haiku fallback on the activity wire (gd-tGAvOOGeoM, plan gd-jMl4oF2cJO; spec
// 2026-10-28-bird-captions-design.md sections 7.2 and 8): a gardener's Bash call with no description goes out without
// a caption, and the caption Haiku writes lands as one `doing` event; a repeat is answered from the cache at once.

// fakeLiteLLM answers /v1/messages with reply, and counts what it was asked.
type fakeLiteLLM struct {
	mu    sync.Mutex
	reply string
	asked []string // the user content of each request
}

func (f *fakeLiteLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	if len(req.Messages) > 0 {
		f.asked = append(f.asked, req.Messages[0].Content)
	}
	reply := f.reply
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "message", "role": "assistant", "content": []map[string]string{{"type": "text", "text": reply}},
	})
}

func (f *fakeLiteLLM) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// captionCounts is gardenio_farm_caption_total by src.
func captionCounts(t *testing.T, k *captioner.Captioner) map[string]int {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(k.Metrics())
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_farm_caption_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "src" {
					out[l.GetValue()] = int(m.GetCounter().GetValue())
				}
			}
		}
	}
	return out
}

// nextMsg is the next message on ch, or fails after a while.
func nextMsg(t *testing.T, ch <-chan ActivityMsg) ActivityMsg {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5 s")
		return ActivityMsg{}
	}
}

func hookPre(agent, id, input string) []byte {
	return []byte(`{"event":"PreToolUse","agent_bead_id":"` + agent + `","session_id":"s","tool_name":"Bash","tool_use_id":"` +
		id + `","tool_input":` + input + `}`)
}

func hookPost(agent, id string) []byte {
	return []byte(`{"event":"PostToolUse","agent_bead_id":"` + agent + `","session_id":"s","tool_name":"Bash","tool_use_id":"` +
		id + `","tool_input":{"command":"x"},"duration_ms":40}`)
}

func TestActivityHaikuLateCaption(t *testing.T) {
	llm := &fakeLiteLLM{reply: "Build the viewer image"}
	srv := httptest.NewServer(llm)
	defer srv.Close()
	cfg := captioner.DefaultConfig()
	cfg.Enabled, cfg.BaseURL, cfg.APIKey = true, srv.URL, "sk-test-virtual-key"
	k := captioner.New(cfg, captioner.WithScrub(ScrubCaption))

	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	f.SetCaptioner(k)
	f.act.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	_, ch, cancel := f.SubscribeActivity(nil)
	defer cancel()

	// A description-less Bash start goes out at once without a caption; Haiku's lands as one doing event.
	cmd := `{"command":"cd /home/agent/bot/gardenio/work && docker build -t viewer ."}`
	f.ObserveHook(hookPre("gd-a", "t1", cmd))
	m := nextMsg(t, ch)
	var e FarmActivity
	if err := json.Unmarshal(m.Data, &e); err != nil || m.Event != ActivityEvent || e.Dur != 0 || e.Cap != "" {
		t.Fatalf("the start goes out at once, without a caption: %s %s", m.Event, m.Data)
	}
	m = nextMsg(t, ch)
	var d FarmDoingOf
	if err := json.Unmarshal(m.Data, &d); err != nil || m.Event != DoingEvent || d.ID != "gd-a" || d.Cap != "Build the viewer image" || d.Calls != 1 || d.At != e.At {
		t.Fatalf("Haiku's caption lands as a doing event: %s %s", m.Event, m.Data)
	}
	if got := llm.requests(); len(got) != 1 || got[0] != "docker build -t viewer ." {
		t.Fatalf("Haiku is sent only the scrubbed command, once: %q", got)
	}
	f.act.mu.Lock()
	doing, _ := f.act.doingLocked("gd-a", c.t)
	open := f.act.agents["gd-a"].open["t1"]
	f.act.mu.Unlock()
	if doing.Cap != "Build the viewer image" || open.cap != "Build the viewer image" {
		t.Errorf("the open call and gardeners[].doing carry the late caption: %+v %+v", doing, open)
	}

	// The same command again: the cache answers, so the start carries the caption at once and Haiku is not asked.
	// Each event is a gap past the last, so none folds.
	c.add(time.Second)
	f.ObserveHook(hookPost("gd-a", "t1"))
	c.add(time.Second)
	f.ObserveHook(hookPre("gd-a", "t2", cmd))
	var caps []string
	for range 2 {
		m := nextMsg(t, ch)
		_ = json.Unmarshal(m.Data, &e)
		if m.Event != ActivityEvent {
			t.Fatalf("only activity events after a cache hit: %s %s", m.Event, m.Data)
		}
		caps = append(caps, e.Cap)
	}
	if caps[0] != "" || caps[1] != "Build the viewer image" || len(llm.requests()) != 1 {
		t.Fatalf("finish then a cached start: caps %q, %d requests", caps, len(llm.requests()))
	}

	// A described Bash call, a tool nothing captions, a pool shell's call and a sidechain call: none goes to Haiku.
	c.add(time.Second)
	f.ObserveHook(hookPre("gd-a", "t3", `{"command":"go test ./x","description":"Run the hub tests"}`))
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-a","session_id":"s","tool_name":"Frobnicate","tool_use_id":"t4","tool_input":{}}`))
	f.ObserveHook(hookPre("gd-pool", "p1", `{"command":"make lint"}`))
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-a","session_id":"s","is_sidechain":true,"tool_name":"Bash","tool_use_id":"t5","tool_input":{"command":"make lint"}}`))
	if got := llm.requests(); len(got) != 1 {
		t.Fatalf("only a gardener's description-less Bash call goes to Haiku: %q", got)
	}

	got := captionCounts(t, k)
	for src, want := range map[string]int{captioner.SrcHaiku: 1, captioner.SrcCache: 1, captioner.SrcDescription: 1, captioner.SrcNone: 1} {
		if got[src] != want {
			t.Errorf("gardenio_farm_caption_total{src=%q} = %d, want %d (all: %v)", src, got[src], want, got)
		}
	}
}

func TestActivityHaikuOffCountsEverySource(t *testing.T) {
	k := captioner.New(captioner.DefaultConfig()) // Haiku off: the default
	f := NewFarm(nil, nil)
	f.SetCaptioner(k)
	f.act.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	f.ObserveHook(hookPre("gd-a", "t1", `{"command":"docker build ."}`))
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-a","session_id":"s","tool_name":"Read","tool_use_id":"t2","tool_input":{"file_path":"/home/agent/bot/gardenio/work/a.go"}}`))
	f.ObserveHook(hookPost("gd-a", "t1")) // a finish is no caption
	if got := captionCounts(t, k); got[captioner.SrcNone] != 1 || got[captioner.SrcPath] != 1 || got[captioner.SrcHaiku] != 0 {
		t.Errorf("with Haiku off every start is still counted by source: %v", got)
	}
}

// TestActivityUncaptionedByTool: a gardener's start with no caption of its own is counted by tool (gd-Ty2eNoZipX), a
// captioned one and a finish are not.
func TestActivityUncaptionedByTool(t *testing.T) {
	k := captioner.New(captioner.DefaultConfig())
	f := NewFarm(nil, nil)
	f.SetCaptioner(k)
	f.act.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	f.ObserveHook(hookPre("gd-a", "t1", `{"command":"ls"}`))
	f.ObserveHook(hookPre("gd-a", "t2", `{"command":"ls","description":"List files"}`))
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-a","session_id":"s","tool_name":"SomethingNew","tool_use_id":"t3","tool_input":{}}`))
	f.ObserveHook(hookPost("gd-a", "t1"))
	reg := prometheus.NewRegistry()
	reg.MustRegister(k.Metrics())
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_farm_uncaptioned_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			got[m.GetLabel()[0].GetValue()] = int(m.GetCounter().GetValue())
		}
	}
	if len(got) != 2 || got["Bash"] != 1 || got["other"] != 1 {
		t.Errorf("gardenio_farm_uncaptioned_total by tool = %v, want Bash 1 and other 1", got)
	}
}

func TestActivityLateCaptionRules(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a", Project: "p"}, "gd-b": {ID: "gd-b", Project: "p"}})
	_, all, cancelAll := a.subscribe(nil)
	defer cancelAll()
	_, scoped, cancelScoped := a.subscribe(func(g FarmGardener) bool { return g.Project == "p" })
	defer cancelScoped()
	_, other, cancelOther := a.subscribe(func(g FarmGardener) bool { return g.Project == "q" })
	defer cancelOther()
	sidBusy, busy, cancelBusy := a.subscribe(nil)
	defer cancelBusy()
	a.setView(sidBusy, ActivityView{Level: 1}, c.t)
	sidB, viewB, cancelB := a.subscribe(nil)
	defer cancelB()
	a.setView(sidB, ActivityView{Level: viewPlanLevel, IDs: []string{"gd-b"}}, c.t)
	flushAll := func() {
		for _, ch := range []<-chan ActivityMsg{all, scoped, other, busy, viewB} {
			for len(ch) > 0 {
				<-ch
			}
		}
	}
	doingsOn := func(ch <-chan ActivityMsg) (out []FarmDoingOf) {
		for len(ch) > 0 {
			if m := <-ch; m.Event == DoingEvent {
				var d FarmDoingOf
				_ = json.Unmarshal(m.Data, &d)
				out = append(out, d)
			}
		}
		return out
	}

	n1 := a.observe(pre("gd-a", "other", "t1", c.t), c.t)
	flushAll()
	a.lateCaption("gd-a", n1, "Rebase onto gd-gCHKhIROvB", "t1", c.t)
	if d := doingsOn(all); len(d) != 1 || d[0].Cap != "Rebase onto gd-gCHKhIROvB" {
		t.Errorf("an unscoped viewer with no view hears the late caption: %+v", d)
	}
	if d := doingsOn(scoped); len(d) != 1 || d[0].Cap != "Rebase onto a bead" {
		t.Errorf("a scoped viewer hears it with bead ids masked: %+v", d)
	}
	for name, ch := range map[string]<-chan ActivityMsg{"cut": other, "below the Plan level": busy, "out of view": viewB} {
		if d := doingsOn(ch); len(d) != 0 {
			t.Errorf("a viewer %s hears nothing: %+v", name, d)
		}
	}

	// A start that is no longer the newest keeps its place: the newer one's caption stands, and nothing is sent.
	c.add(time.Second)
	n2 := a.observe(preCap("gd-a", "other", "t2", "Show the plan", c.t), c.t)
	flushAll()
	a.lateCaption("gd-a", n1, "Something stale", "t1", c.t)
	if d := doingsOn(all); len(d) != 0 {
		t.Errorf("a stale late caption is sent: %+v", d)
	}
	a.mu.Lock()
	cp := a.agents["gd-a"].cap
	a.mu.Unlock()
	if cp != "Show the plan" || n2 == n1 {
		t.Errorf("a stale late caption overwrote the newest start's: %q", cp)
	}

	// A late caption for a start still folding, behind a finish folded after it: the fold, whose newest event is
	// the finish, carries the start's late caption when it goes out.
	c.add(time.Second)
	a.observe(pre("gd-a", "other", "t3", c.t), c.t) // goes at once
	c.add(10 * time.Millisecond)
	n4 := a.observe(pre("gd-a", "other", "t4", c.t), c.t)
	if n4 == 0 || n4 == n2 {
		t.Fatalf("a start is numbered afresh: %d after %d", n4, n2)
	}
	flushAll()
	a.lateCaption("gd-a", n4, "Tail the build log", "t4", c.t)
	if d := doingsOn(all); len(d) != 1 || d[0].Cap != "Tail the build log" {
		t.Errorf("a late caption for a folding start is still heard as a doing event: %+v", d)
	}
	c.add(10 * time.Millisecond)
	a.observe(post("gd-a", "other", "t3", c.t), c.t)
	a.flush(c.add(activityMinGap))
	if got := drain(all); len(got) != 1 || got[0].Dur == 0 || got[0].N != 1 || got[0].Cap != "Tail the build log" {
		t.Errorf("the fold carries its start's late caption: %+v", got)
	}
	a.mu.Lock()
	open := a.agents["gd-a"].open["t4"]
	a.mu.Unlock()
	if open.cap != "Tail the build log" {
		t.Errorf("the open call keeps its late caption: %+v", open)
	}

	// A late caption for an agent that is not a gardener is kept but sent to nobody.
	c.add(time.Second)
	n5 := a.observe(pre("gd-z", "other", "z1", c.t), c.t)
	flushAll()
	a.lateCaption("gd-z", n5, "Lint the chart", "z1", c.t)
	if d := doingsOn(all); len(d) != 0 {
		t.Errorf("a late caption for a non-gardener is sent: %+v", d)
	}
}
