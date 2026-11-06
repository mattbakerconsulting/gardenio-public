package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookLine builds one hook_events.jsonl line the way coop's event_log.rs does.
func hookLine(t *testing.T, tsMs int64, event string, data map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_seq":     1,
		"json":         map[string]any{"event": event, "data": data},
		"timestamp_ms": tsMs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSlowCollector_TopNSlowestFirst(t *testing.T) {
	var lines []string
	for i := 1; i <= 25; i++ {
		lines = append(lines, hookLine(t, int64(1000+i), "post_tool_use", map[string]any{
			"tool_name":   "Bash",
			"tool_use_id": fmt.Sprintf("t%d", i),
			"tool_input":  map[string]any{"command": fmt.Sprintf("sleep %d", i)},
			"duration_ms": i * 100,
		}))
	}
	col := newSlowCollector(10)
	col.scan(strings.NewReader(strings.Join(lines, "\n")+"\n"), time.Time{})
	got := col.result()
	if len(got) != 10 {
		t.Fatalf("len = %d, want 10", len(got))
	}
	for i, c := range got {
		if want := int64((25 - i) * 100); c.DurationMs != want {
			t.Errorf("got[%d].DurationMs = %d, want %d", i, c.DurationMs, want)
		}
		if c.Source != "claude" || c.Tool != "Bash" {
			t.Errorf("got[%d] = %+v", i, c)
		}
	}
	if got[0].Args != "sleep 25" {
		t.Errorf("Args = %q, want the full command line", got[0].Args)
	}
}

func TestSlowCollector_RedactsSecretsAndClassifies(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	line := hookLine(t, 5000, "post_tool_use", map[string]any{
		"tool_name":   "Bash",
		"tool_use_id": "t1",
		"tool_input":  map[string]any{"command": "git push https://x:" + secret + "@github.com/o/r.git main"},
		"duration_ms": 9000,
	})
	col := newSlowCollector(10)
	col.scan(strings.NewReader(line+"\n"), time.Time{})
	got := col.result()
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if strings.Contains(got[0].Args, secret) {
		t.Fatalf("secret survived redaction: %q", got[0].Args)
	}
	if !strings.Contains(got[0].Args, "git push") {
		t.Errorf("command line mangled: %q", got[0].Args)
	}
	if got[0].CmdClass == "" {
		t.Error("cmd_class not set for Bash")
	}
}

func TestSlowCollector_DerivedFromPrePostPairing(t *testing.T) {
	lines := []string{
		hookLine(t, 10_000, "pre_tool_use", map[string]any{"tool_name": "Bash", "tool_use_id": "a", "tool_input": map[string]any{"command": "make"}}),
		hookLine(t, 17_500, "post_tool_use", map[string]any{"tool_name": "Bash", "tool_use_id": "a", "tool_input": map[string]any{"command": "make"}}),
		// Native duration wins even when a pair exists.
		hookLine(t, 20_000, "pre_tool_use", map[string]any{"tool_name": "Read", "tool_use_id": "b"}),
		hookLine(t, 21_000, "post_tool_use", map[string]any{"tool_name": "Read", "tool_use_id": "b", "tool_input": map[string]any{"file_path": "/x"}, "duration_ms": 300}),
		// No duration and no Pre: nothing to attribute.
		hookLine(t, 30_000, "post_tool_use", map[string]any{"tool_name": "Edit", "tool_use_id": "c"}),
	}
	col := newSlowCollector(10)
	col.scan(strings.NewReader(strings.Join(lines, "\n")+"\n"), time.Time{})
	got := col.result()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].DurationMs != 7500 || got[0].Source != "derived" {
		t.Errorf("got[0] = %+v, want derived 7500", got[0])
	}
	if got[1].DurationMs != 300 || got[1].Source != "claude" {
		t.Errorf("got[1] = %+v, want claude 300", got[1])
	}
	// Non-Bash tools keep their redacted tool_input as compact JSON.
	if !strings.Contains(got[1].Args, "/x") {
		t.Errorf("non-Bash args = %q", got[1].Args)
	}
}

func TestSlowCollector_SinceFiltersOtherSessions(t *testing.T) {
	old := hookLine(t, 1000, "post_tool_use", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "old"}, "duration_ms": 99_000})
	cur := hookLine(t, 9000, "post_tool_use", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "new"}, "duration_ms": 5})
	col := newSlowCollector(10)
	col.scan(strings.NewReader(old+"\n"+cur+"\n"), time.UnixMilli(5000))
	got := col.result()
	if len(got) != 1 || got[0].Args != "new" {
		t.Fatalf("got = %+v, want only the in-session call", got)
	}
}

func TestSlowCollector_TruncatedTrailingLineKeepsEarlier(t *testing.T) {
	good := hookLine(t, 2000, "post_tool_use", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "ok"}, "duration_ms": 4000})
	col := newSlowCollector(10)
	col.scan(strings.NewReader(good+"\n"+`{"hook_seq":2,"json":{"event":"post_tool_`), time.Time{})
	if got := col.result(); len(got) != 1 {
		t.Fatalf("got = %+v, want the complete line kept", got)
	}
}

func TestBuildSlowCall_TruncatesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", sessionSlowArgsMaxBytes) // 2 bytes each
	in, _ := json.Marshal(map[string]any{"command": long})
	c := buildSlowCall("Bash", "t", in, 1000, "claude", 1)
	if !c.TruncatedArgs || len(c.Args) > sessionSlowArgsMaxBytes {
		t.Fatalf("truncated=%v len=%d", c.TruncatedArgs, len(c.Args))
	}
	if strings.ContainsRune(c.Args, '�') {
		t.Error("cut in the middle of a rune")
	}
}

func TestCollectSessionSlowestCalls_MergesFilesAndSkipsStale(t *testing.T) {
	dir := t.TempDir()
	write := func(sess string, mtime time.Time, lines ...string) {
		d := filepath.Join(dir, "sessions", sess)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "hook_events.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now().Add(-time.Hour)
	startMs := start.UnixMilli()
	write("aaa", time.Now(), hookLine(t, startMs+1000, "post_tool_use", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "a"}, "duration_ms": 3000}))
	write("bbb", time.Now(), hookLine(t, startMs+2000, "post_tool_use", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "b"}, "duration_ms": 8000}))
	// A previous pod-life's session: file untouched since before this session began.
	write("old", start.Add(-time.Hour), hookLine(t, startMs-5000, "post_tool_use", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "stale"}, "duration_ms": 99_000}))

	got := collectSessionSlowestCalls(dir, start)
	if len(got) != 2 || got[0].Args != "b" || got[1].Args != "a" {
		t.Fatalf("got = %+v", got)
	}
	if calls := collectSessionSlowestCalls(filepath.Join(dir, "missing"), start); len(calls) != 0 {
		t.Errorf("missing dir -> %+v", calls)
	}
}
