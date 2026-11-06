package gardenhub

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/gardenhub/captioner"
)

// The captions' switch and bounds from the environment (gd-FOzMlrXga6, step 10 of plan gd-jMl4oF2cJO; spec
// 2026-10-28-bird-captions-design.md sections 8 and 14): FARM_CAPTIONS=off restores the wire before captions,
// FARM_CAPTION_WIRE_CAP shortens cap on the wire only, and FARM_TRANSCRIPT_ROWS sizes each gardener's ring.

func TestCaptionSettingsFromEnv(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	if got := CaptionSettingsFromEnv(env(nil)); got != DefaultCaptionSettings() || got.Off || got.WireCap != 80 || got.TranscriptRows != 200 {
		t.Fatalf("unset: %+v, want captions on, 80 runes on the wire, 200 rows", got)
	}
	got := CaptionSettingsFromEnv(env(map[string]string{"FARM_CAPTIONS": "off", "FARM_CAPTION_WIRE_CAP": "48", "FARM_TRANSCRIPT_ROWS": "500"}))
	if !got.Off || got.WireCap != 48 || got.TranscriptRows != 500 {
		t.Fatalf("set: %+v", got)
	}
	for _, v := range []string{"on", "true", "", "maybe"} {
		if CaptionSettingsFromEnv(env(map[string]string{"FARM_CAPTIONS": v})).Off {
			t.Errorf("FARM_CAPTIONS=%q turned captions off", v)
		}
	}
	// Out of range or not a number keeps the default: the wire never carries more than the spec's 80 runes, and a
	// ring is never unbounded.
	for _, kv := range []map[string]string{
		{"FARM_CAPTION_WIRE_CAP": "0", "FARM_TRANSCRIPT_ROWS": "0"},
		{"FARM_CAPTION_WIRE_CAP": "81", "FARM_TRANSCRIPT_ROWS": "1001"},
		{"FARM_CAPTION_WIRE_CAP": "lots", "FARM_TRANSCRIPT_ROWS": "-3"},
	} {
		if got := CaptionSettingsFromEnv(env(kv)); got.WireCap != 80 || got.TranscriptRows != 200 {
			t.Errorf("%v: %+v, want the defaults", kv, got)
		}
	}
}

func TestCaptionsOffRestoresTheWire(t *testing.T) {
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
	f.SetCaptionSettings(CaptionSettings{Off: true, WireCap: 80, TranscriptRows: 200})
	f.act.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	_, ch, cancel := f.SubscribeActivity(nil)
	defer cancel()

	f.ObserveHook(hookPre("gd-a", "t1", `{"command":"go test ./x","description":"Run the hub tests"}`))
	m := nextMsg(t, ch)
	if m.Event != ActivityEvent {
		t.Fatalf("want the start's activity event: %s %s", m.Event, m.Data)
	}
	var raw map[string]any
	if err := json.Unmarshal(m.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["cap"]; ok {
		t.Fatalf("captions off: the start carries no cap key at all: %s", m.Data)
	}
	c.add(time.Second)
	f.ObserveHook(hookPre("gd-a", "t2", `{"command":"docker build -t viewer ."}`))
	c.add(time.Second)
	f.ObserveHook(hookPost("gd-a", "t2"))

	if got := llm.requests(); len(got) != 0 {
		t.Fatalf("captions off: Haiku is never asked: %q", got)
	}
	if got := captionCounts(t, k); got[captioner.SrcDescription] != 0 || got[captioner.SrcNone] != 0 {
		t.Errorf("captions off: nothing is counted as a caption: %v", got)
	}
	f.act.mu.Lock()
	doing, _ := f.act.doingLocked("gd-a", c.t)
	f.act.mu.Unlock()
	if doing.Cap != "" {
		t.Errorf("captions off: gardeners[].doing has no cap: %+v", doing)
	}
	for _, r := range rowsOf(t, f.act, "gd-a") {
		if r.Cap != "" {
			t.Errorf("captions off: the transcript keeps no caption: %+v", r)
		}
	}
}

func TestCaptionWireCapAndTranscriptRows(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	f.SetCaptionSettings(CaptionSettings{WireCap: 48, TranscriptRows: 30})
	f.act.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	_, ch, cancel := f.SubscribeActivity(nil)
	defer cancel()

	long := "Trial-merge main into the plot transcript branch in a detached worktree and rerun"
	f.ObserveHook(hookPre("gd-a", "t0", `{"command":"git merge main","description":"`+long+`"}`))
	m := nextMsg(t, ch)
	var e FarmActivity
	if err := json.Unmarshal(m.Data, &e); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(e.Cap); n > 48 || n < 30 {
		t.Fatalf("the wire carries at most 48 runes of the caption: %d %q", n, e.Cap)
	}
	c.add(time.Second)
	f.ObserveHook(hookPost("gd-a", "t0"))
	if rows := rowsOf(t, f.act, "gd-a"); len(rows) != 1 || utf8.RuneCountInString(rows[0].Cap) <= 48 {
		t.Fatalf("the transcript keeps the full caption, not the wire's: %+v", rows)
	}

	for i := 1; i <= 50; i++ {
		c.add(time.Second)
		id := fmt.Sprintf("t%d", i)
		f.ObserveHook(hookPre("gd-a", id, `{"command":"ls","description":"List call `+fmt.Sprint(i)+`"}`))
		c.add(time.Second)
		f.ObserveHook(hookPost("gd-a", id))
	}
	rows := rowsOf(t, f.act, "gd-a")
	if len(rows) != 30 || rows[0].Cap != "List call 50" || rows[29].Cap != "List call 21" {
		t.Fatalf("a 30-row ring keeps the newest 30: %d rows, %q ... %q", len(rows), rows[0].Cap, rows[len(rows)-1].Cap)
	}

	// Out of range keeps what is set.
	f.SetCaptionSettings(CaptionSettings{WireCap: 500, TranscriptRows: 0})
	if f.act.wireCap != 48 || f.act.ringRows != 30 {
		t.Errorf("out-of-range settings changed the bounds: wire %d rows %d", f.act.wireCap, f.act.ringRows)
	}
}
