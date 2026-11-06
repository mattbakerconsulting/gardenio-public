package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestFmtDurationMs(t *testing.T) {
	for _, c := range []struct {
		ms   int64
		want string
	}{
		{840, "840ms"}, {12_300, "12.3s"}, {245_000, "4m 05s"}, {3_720_000, "1h 02m"},
	} {
		if got := fmtDurationMs(c.ms); got != c.want {
			t.Errorf("fmtDurationMs(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestGroupSlowToolCalls_GroupsByClassSlowestFirst(t *testing.T) {
	now := time.Now()
	rows := []*beadsapi.SlowToolCallRow{
		{ToolName: "Bash", CmdClass: "go-test", DurationMs: 90_000, CommandRedacted: "go test ./..."},
		{ToolName: "Bash", CmdClass: "git", DurationMs: 40_000, CommandRedacted: "git fetch"},
		{ToolName: "Bash", CmdClass: "go-test", DurationMs: 30_000, CommandRedacted: "go test ./a"},
		{ToolName: "WebFetch", DurationMs: 20_000},
	}
	groups := groupSlowToolCalls(rows, now)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	if groups[0].Class != "go-test" || groups[0].Count != 2 || groups[0].MaxDisplay != "1m 30s" || groups[0].TotalDisplay != "2m 00s" {
		t.Errorf("first group = %+v", groups[0])
	}
	if groups[1].Class != "git" || groups[2].Class != "(tool)" {
		t.Errorf("order = %s, %s, %s", groups[0].Class, groups[1].Class, groups[2].Class)
	}
	if groups[2].Rows[0].Summary != "WebFetch" {
		t.Errorf("non-Bash summary = %q, want tool name", groups[2].Rows[0].Summary)
	}
}

func TestToolLatencySummary_TruncatesAndTakesFirstLine(t *testing.T) {
	long := &beadsapi.SlowToolCallRow{CommandRedacted: strings.Repeat("x", 300)}
	if got := toolLatencySummary(long); len([]rune(got)) != 121 {
		t.Errorf("long summary runes = %d, want 121", len([]rune(got)))
	}
	multi := &beadsapi.SlowToolCallRow{CommandRedacted: "echo a\necho b"}
	if got := toolLatencySummary(multi); got != "echo a …" {
		t.Errorf("multiline summary = %q", got)
	}
}

func TestHandleToolLatency_RendersGroupsAndFullArgv(t *testing.T) {
	exit := 1
	daemon := newFake()
	daemon.slowToolCallsResp = []*beadsapi.SlowToolCallRow{{
		TS: time.Now().Add(-5 * time.Minute), ToolName: "Bash", CmdClass: "go-test", DurationMs: 95_000,
		CommandRedacted: "cd gardenio && go test -run TestFoo ./...\nsecond line",
		AgentBeadID:     "gd-agent1", SessionID: "sess-123", Project: "gardenio", Role: "crew",
		Source: "claude", ExitStatus: &exit,
	}}
	srv := newTestServerForTaints(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/tool-latency?window=6h&class=go-test", nil)
	rec := httptest.NewRecorder()
	srv.handleToolLatency(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"go-test", "1m 35s", "second line", "gd-agent1", "sess-123", "/b/gd-agent1"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	f := daemon.slowToolCallsLastFilter
	if f.CmdClass != "go-test" || f.Limit != toolLatencyLimit {
		t.Errorf("filter = %+v", f)
	}
	if d := time.Since(f.Since); d < 5*time.Hour+55*time.Minute || d > 6*time.Hour+5*time.Minute {
		t.Errorf("since offset = %v, want ~6h", d)
	}
}

func TestHandleToolLatency_UnknownWindowDefaults(t *testing.T) {
	daemon := newFake()
	srv := newTestServerForTaints(t, daemon)
	rec := httptest.NewRecorder()
	srv.handleToolLatency(rec, httptest.NewRequest(http.MethodGet, "/tool-latency?window=bogus", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if d := time.Since(daemon.slowToolCallsLastFilter.Since); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("default window offset = %v, want ~24h", d)
	}
	if !strings.Contains(rec.Body.String(), "No slow tool calls recorded in the last 24h") {
		t.Errorf("expected empty-state message")
	}
}

func TestHandleToolLatency_DaemonErrorShowsBanner(t *testing.T) {
	daemon := newFake()
	daemon.slowToolCallsErr = errors.New("boom")
	srv := newTestServerForTaints(t, daemon)
	rec := httptest.NewRecorder()
	srv.handleToolLatency(rec, httptest.NewRequest(http.MethodGet, "/tool-latency", nil))
	if !strings.Contains(rec.Body.String(), "Failed to load the slow tool-call ledger.") {
		t.Errorf("expected error banner")
	}
}
